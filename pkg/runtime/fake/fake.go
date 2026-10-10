// Package fake is an in-memory runtime.SessionRuntime: enough of a session
// runtime to drive gonk-gate's dispatch and sweep tests with no cluster.
//
// It keeps the contract the Jobs implementation keeps, because a fake that is
// kinder than production is how a test passes over a bug:
//
//   - Start is idempotent on Spec.Name: a second Start of a live name returns
//     the SAME handle, a nil error and created=false, and leaves the first
//     session's Spec and state untouched; the first returns created=true.
//   - Start refuses a Spec whose Name is not a runtime.ValidName.
//   - Stop removes the session; a second Stop returns runtime.ErrNotFound.
//   - Status of an unknown handle is PhaseNotFound with a nil error; Output of
//     one is runtime.ErrNotFound.
//
// A new session starts in PhasePending with empty output. Tests move it on
// with SetPhase / SetStatus / SetOutput, and inject failures with
// SetStartErr / SetOutputErr. Every method is safe for concurrent use.
package fake

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

type session struct {
	spec      runtime.Spec
	status    runtime.Status
	output    []byte
	outputErr error
}

// Runtime is the fake. Use New; the zero value is not ready.
type Runtime struct {
	// Now stamps StartedAt/FinishedAt in SetPhase. Defaults to time.Now; set
	// it before use for deterministic timestamps.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[runtime.Handle]*session
	startErr error
	starts   []runtime.Spec
}

var _ runtime.SessionRuntime = (*Runtime)(nil)

// New returns an empty fake runtime.
func New() *Runtime {
	return &Runtime{Now: time.Now, sessions: map[runtime.Handle]*session{}}
}

// Start admits spec, or returns the existing session's handle if one is
// already live under spec.Name (created=false, as the Jobs runtime reports an
// adopted Job). Every call is recorded (see Starts), including the idempotent
// ones and the ones that fail, so a test can assert how many times dispatch
// tried.
func (r *Runtime) Start(ctx context.Context, spec runtime.Spec) (runtime.Handle, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, cloneSpec(spec))
	if r.startErr != nil {
		return "", false, r.startErr
	}
	if err := runtime.ValidName(spec.Name); err != nil {
		return "", false, err
	}
	h := runtime.Handle(spec.Name)
	if _, ok := r.sessions[h]; ok {
		return h, false, nil
	}
	r.sessions[h] = &session{
		spec:   cloneSpec(spec),
		status: runtime.Status{Phase: runtime.PhasePending},
	}
	return h, true, nil
}

// Status reports the session's status, or PhaseNotFound for an unknown one.
func (r *Runtime) Status(ctx context.Context, h runtime.Handle) (runtime.Status, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Status{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[h]
	if !ok {
		return runtime.Status{Phase: runtime.PhaseNotFound}, nil
	}
	return s.status, nil
}

// Output returns a copy of the session's output, its injected error, or
// runtime.ErrNotFound for an unknown session.
func (r *Runtime) Output(ctx context.Context, h runtime.Handle) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[h]
	if !ok {
		return nil, fmt.Errorf("output %s: %w", h, runtime.ErrNotFound)
	}
	if s.outputErr != nil {
		return nil, s.outputErr
	}
	return bytes.Clone(s.output), nil
}

// Stop removes the session, or returns runtime.ErrNotFound if there is none.
func (r *Runtime) Stop(ctx context.Context, h runtime.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[h]; !ok {
		return fmt.Errorf("stop %s: %w", h, runtime.ErrNotFound)
	}
	delete(r.sessions, h)
	return nil
}

// List returns every live session's handle, sorted.
func (r *Runtime) List(ctx context.Context) ([]runtime.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]runtime.Handle, 0, len(r.sessions))
	for h := range r.sessions {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// --- test hooks ---

// SetStartErr makes every following Start fail with err (after recording the
// call) until it is set back to nil. The failed Start admits nothing.
func (r *Runtime) SetStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

// SetOutputErr makes Output of h fail with err until it is set back to nil.
func (r *Runtime) SetOutputErr(h runtime.Handle, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mustGet(h).outputErr = err
}

// SetOutput replaces the session's output (what Output returns).
func (r *Runtime) SetOutput(h runtime.Handle, out []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mustGet(h).output = bytes.Clone(out)
}

// SetPhase moves the session to phase with reason, stamping StartedAt on the
// first move to Running or a finished phase, and FinishedAt on the first move
// to a finished phase. PhaseNotFound removes the session, as if its Job had
// been deleted or reaped by its TTL.
func (r *Runtime) SetPhase(h runtime.Handle, phase runtime.Phase, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.mustGet(h)
	if phase == runtime.PhaseNotFound {
		delete(r.sessions, h)
		return
	}
	now := r.Now()
	st := s.status
	st.Phase, st.Reason = phase, reason
	if (phase == runtime.PhaseRunning || phase.Finished()) && st.StartedAt.IsZero() {
		st.StartedAt = now
	}
	if phase.Finished() && st.FinishedAt.IsZero() {
		st.FinishedAt = now
	}
	s.status = st
}

// SetStatus replaces the session's whole status, timestamps included.
func (r *Runtime) SetStatus(h runtime.Handle, st runtime.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mustGet(h).status = st
}

// Spec returns a copy of the Spec the live session h was started with.
func (r *Runtime) Spec(h runtime.Handle) (runtime.Spec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[h]
	if !ok {
		return runtime.Spec{}, false
	}
	return cloneSpec(s.spec), true
}

// Starts returns a copy of every Spec passed to Start, in call order,
// including idempotent repeats and failed calls.
func (r *Runtime) Starts() []runtime.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]runtime.Spec, len(r.starts))
	for i, s := range r.starts {
		out[i] = cloneSpec(s)
	}
	return out
}

// mustGet panics on an unknown handle: a test moving a session it never
// started is a bug in the test, and a silent no-op would hide it. Callers
// hold r.mu.
func (r *Runtime) mustGet(h runtime.Handle) *session {
	s, ok := r.sessions[h]
	if !ok {
		panic(fmt.Sprintf("fake runtime: no session %q (never started, or stopped)", h))
	}
	return s
}

// cloneSpec deep-copies the Spec's maps and byte slices, so neither the
// caller mutating its Spec after Start nor a test mutating a returned copy
// can change what the fake holds.
func cloneSpec(s runtime.Spec) runtime.Spec {
	s.Env = maps.Clone(s.Env)
	s.Labels = maps.Clone(s.Labels)
	s.Prompt = bytes.Clone(s.Prompt)
	s.LiteLLMKey = bytes.Clone(s.LiteLLMKey)
	s.Metadata = bytes.Clone(s.Metadata)
	return s
}
