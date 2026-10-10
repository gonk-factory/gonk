// Package runtime is the SessionRuntime seam: the one interface gonk-gate's
// dispatch and sweep use to start an agent session, watch it, read what it
// produced, and stop it (gonk-t55, plan docs/plans/2026-09-25-t55-session-runtime.md
// section 3.0, D1 and D2).
//
// WHY A SEAM. Dispatch and sweep used to talk to Gas City's supervisor API
// directly (pkg/gcapi), through eight separate calls spread over the inject
// and apply paths. The session is the thing gonk actually needs -- a pod that
// runs one prompt and leaves one log -- and that is small enough to state as
// five methods. The production implementation is Kubernetes Jobs
// (pkg/runtime/jobs); tests run against pkg/runtime/fake.
//
// WHAT IS DELIBERATELY NOT HERE. ADR-008 section 7 sketched Await and Batch.
// The sweep is a periodic process, not a waiter, so it needs a non-blocking
// Status instead of Await; and batch extraction stays in gonk-gate over
// Output, so the effects-fence parser has exactly one owner.
//
// THE NAME IS THE HANDLE. A session's name is deterministic in (agent,
// project, issue/MR, attempt) -- see Name -- and is the idempotency key:
// starting the same name twice yields the SAME session, not a second one and
// not an error. That is what makes a re-dispatch of the same bead and attempt
// safe, and it is why the handle is nothing more than that name: the sweep
// stores it as beadstore.Record.SessionID and can rebuild a Handle from the
// record alone.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when the runtime has no session under a handle.
//
// It is NOT "destroyed". Stop on a handle the runtime does not know returns
// ErrNotFound rather than nil, because the caller cannot tell from a nil
// whether it stopped something or was talking about a session that never
// existed (a wrong handle, a Job already reaped by its TTL). Callers that only
// want "it is gone" test errors.Is(err, ErrNotFound) and carry on.
var ErrNotFound = errors.New("runtime: session not found")

// Handle identifies one session. It is the session's Spec.Name, verbatim; see
// the package doc for why it is not an opaque token.
type Handle string

// String returns the handle as the session name.
func (h Handle) String() string { return string(h) }

// Spec is everything needed to start one session.
//
// Secrets never go in Env: Prompt and LiteLLMKey (and Metadata, which is
// spend attribution) are MOUNTED into the session, so they do not appear in a
// pod spec, a `describe`, or a process listing.
type Spec struct {
	// Name is the session's deterministic name and its idempotency key. Build
	// it with Name(...); it must satisfy ValidName.
	Name string
	// Agent is the pack agent the session runs: triage | mention | scaffold.
	Agent string
	// Image is the agent container image reference.
	Image string
	// Env is the session's NON-secret environment only.
	Env map[string]string
	// Prompt is mounted into the session, never put in the environment.
	Prompt []byte
	// LiteLLMKey is mounted into the session, never put in the environment.
	LiteLLMKey []byte
	// Metadata is the spend-attribution JSON, mounted alongside the prompt.
	Metadata []byte
	// Deadline is when the session must be finished by; the Jobs
	// implementation turns it into activeDeadlineSeconds.
	Deadline time.Time
	// Labels are extra labels for the session's objects.
	Labels map[string]string
	// RigURL is the full fetch URL of this session's checkout grant, or ""
	// when no checkout was granted. It is MOUNTED (the Jobs runtime writes it
	// to the session Secret as `rig-url`), never env: the URL embeds the
	// grant's key, and pkg/rig serves that tree to anyone presenting it for
	// the grant's TTL, so an env value would hand it to anyone who can `get`
	// the Job (plan 3.3 handshake item 4).
	RigURL string
}

// THE SESSION CONTRACT (plan 3.3, "handshake"). The controller that builds a
// session and the agent image that runs it are deployed as separate images,
// and dev has observed them running different versions for ~20 minutes at a
// time. Everything the two must agree on -- which env names are set, which
// files the session Secret holds and what they mean, what the exit codes say
// -- is versioned by ONE integer, SessionContract.
//
// The runtime puts it on every session as EnvSessionContract; the agent's
// entrypoint (images/agent/entrypoint.sh, GONK_SUPPORTED_SESSION_CONTRACTS)
// refuses to run a contract it does not know and exits
// ExitSessionContractMismatch BEFORE reading its inputs or spending anything.
// The sweep recognises that exit code as image skew: not the agent's fault,
// not an attempt, parked and retried once the images agree.
//
// BUMP IT on any change to that interface that an older or newer peer would
// misread, and add the new value to the entrypoint's supported set in the
// same commit. test/entrypoint pins that the entrypoint supports this value,
// so the two cannot drift silently.
const (
	// SessionContract is the contract version this controller speaks.
	SessionContract = 1
	// EnvSessionContract is the agent env name the runtime sets it in. It is
	// runtime-owned: a Spec may not set it.
	EnvSessionContract = "GONK_SESSION_CONTRACT"
	// ExitSessionContractMismatch is the entrypoint's DEDICATED exit code for
	// an unset or unsupported contract (the next free code after the
	// entrypoint's own 1-7).
	ExitSessionContractMismatch = 8
	// ExitLoopWatchdog is the entrypoint's DEDICATED exit code for "the loop
	// watchdog stopped opencode" (gonk-un7z; the next free code after 8): the
	// same tool call FAILED GONK_LOOP_WATCHDOG_THRESHOLD times (default 2), so
	// the session was killed before it spent more tokens re-sending its
	// context for nothing. The entrypoint logs the repeated call on a
	// "loop watchdog: stopped opencode" line, which the sweep quotes as the
	// cause. It is the MODEL'S behaviour, not infra: such a session is judged
	// like any other (no batch, model spend -> gate-failed), so the ladder
	// escalates. Adding it is not a contract change -- an older controller
	// already judges that Failed, batch-less Job gate-failed.
	ExitLoopWatchdog = 9
)

// Phase is where a session is in its life.
//
// The zero value is PhaseUnknown, not PhasePending, on purpose: an
// implementation that forgets to set Phase must not read as "still starting,
// wait", which the sweep would do quietly until the reservation expired.
type Phase int

const (
	// PhaseUnknown is the zero value. No implementation returns it with a nil
	// error; seeing it means a Status was never filled in.
	PhaseUnknown Phase = iota
	// PhasePending: admitted, not yet running (scheduling, image pull, waiting
	// for its mounted secret).
	PhasePending
	// PhaseRunning: the session's container is running.
	PhaseRunning
	// PhaseSucceeded: the session exited 0. That is NOT the outcome: opencode
	// exits 0 after a rejected tool call, so the sweep judges the batch in
	// Output, never the exit code.
	PhaseSucceeded
	// PhaseFailed: the session exited non-zero or was killed; Status.Reason
	// says why (e.g. DeadlineExceeded).
	PhaseFailed
	// PhaseNotFound: the runtime has no such session. Status reports this as a
	// phase with a nil error, because "absent" is an answer the sweep acts on
	// (its released-by-absent path), not a failure to get an answer.
	PhaseNotFound
)

// String names the phase for logs.
func (p Phase) String() string {
	switch p {
	case PhaseUnknown:
		return "Unknown"
	case PhasePending:
		return "Pending"
	case PhaseRunning:
		return "Running"
	case PhaseSucceeded:
		return "Succeeded"
	case PhaseFailed:
		return "Failed"
	case PhaseNotFound:
		return "NotFound"
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

// Finished reports whether the phase is terminal: the session ran and will
// not run again, so its Output is complete.
func (p Phase) Finished() bool { return p == PhaseSucceeded || p == PhaseFailed }

// Status is a point-in-time view of one session.
type Status struct {
	Phase Phase
	// Reason is the runtime's own word for why the session is where it is
	// (DeadlineExceeded, BackoffLimitExceeded, ImagePullBackOff, ...). Empty
	// when there is nothing to say.
	Reason string
	// StartedAt is when the session started running; zero before that.
	StartedAt time.Time
	// FinishedAt is when the session finished; zero until it has.
	FinishedAt time.Time
	// ExitCode is the session container's exit code, when the runtime knows
	// it: set for a finished session whose container terminated, nil
	// otherwise (still running, never started, or its pod is gone). The sweep
	// reads it for exactly two things -- ExitSessionContractMismatch (image
	// skew: not judged) and ExitLoopWatchdog (the cause it logs) -- and never
	// as the outcome (see PhaseSucceeded).
	ExitCode *int
}

// SessionRuntime runs agent sessions.
type SessionRuntime interface {
	// Start admits a session. It returns only once the runtime has ADMITTED
	// it (for Jobs: the API server accepted the create), not once it runs.
	// If a session named spec.Name already exists, Start returns that
	// session's handle and a nil error, and changes nothing about it.
	//
	// created reports which of the two happened: true when THIS call made the
	// session, false when it handed back one that already existed (adopted).
	// It is meaningful only with a nil error. It exists because the name is
	// deterministic (D2): a caller that adopted a session did not start it,
	// and must not tear it down as its own -- the dispatch whose record names
	// it is its owner (plan 3.4 C2). Under Gas City's nonced aliases two
	// dispatches could never reach one session, so the question never arose.
	Start(ctx context.Context, spec Spec) (h Handle, created bool, err error)
	// Status reports where the session is. It does not block waiting for a
	// change: the sweep polls. An unknown handle is Phase PhaseNotFound with a
	// nil error.
	Status(ctx context.Context, h Handle) (Status, error)
	// Output returns the session's full container log, with no size envelope.
	// An unknown handle returns ErrNotFound.
	Output(ctx context.Context, h Handle) ([]byte, error)
	// Stop ends the session and removes it. It is idempotent in effect; an
	// unknown handle (including one already stopped) returns ErrNotFound.
	Stop(ctx context.Context, h Handle) error
	// List returns every session this runtime owns.
	List(ctx context.Context) ([]Handle, error)
}
