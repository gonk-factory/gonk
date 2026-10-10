// Package beadstore is gonk-gate's per-bead work record: the small amount of
// state the gate needs between a dispatch and its outcome.
//
// It lives BEHIND AN INTERFACE with two implementations (AD-2):
//
//	Memory -- in-memory. EVERY test in cmd/gonk-gate runs against this. No Dolt,
//	          no containers, no `bd` binary.
//	BdCLI  -- shells the MIT `bd` binary against the shared Dolt bead store
//	          (spec 4.2). Its exact subcommands are CONFIRMED AGAINST THE REAL
//	          `bd` in Task 6's container smoke test, not guessed at here.
//
// That split is deliberate: it confines the one thing this plan cannot verify
// offline (bd's CLI surface) to a single file, and leaves the gate's entire
// decision logic testable with `go test`.
package beadstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

type State string

const (
	// StateRunning: a broker agent session was created; we are waiting for it
	// to finish.
	StateRunning State = "running"
	// StateParked: meter said `defer` (budget exhausted, quiet hours, capacity).
	// RetryAfter says when to ask again. THIS IS A NORMAL STATE, NOT AN ERROR.
	StateParked State = "parked"
	// StateNeedsHuman: meter said `deny` (ladder exhausted, action not allowed,
	// project not registered). We stop. The sweeper must NEVER pick these up
	// again -- re-sweeping a denied bead is how you spend money on work you
	// already decided not to do.
	StateNeedsHuman State = "needs-human"
	// StateDone: the gate passed.
	StateDone State = "done"
	// StatePendingPrompt: the v2 broker has RESERVED a session alias for this
	// bead+attempt but has not yet confirmed the session running (gonk-6n8,
	// T-13). It exists to close the ≤240s gap between minting the alias and
	// the broker's final StateRunning write, during which a re-dispatch for
	// the same attempt used to see no record at all and mint a second session
	// under the same alias -- see cmd/gonk-gate/broker_inject.go's
	// runBrokerDispatch for the full history (gonk-u6p).
	//
	// INTERIM ONLY, but NOT unreclaimable (T-19). Two things keep a record
	// from wedging here: runBrokerDispatch restores the PRIOR record -- state
	// and session id as they stood before the reservation write -- on every
	// path that does not reach StateRunning, and gonk-sweep Lists this state
	// and promotes anything older than pendingPromptGrace to StateRunning so
	// the normal reservation-deadline machinery settles it (see runSweep and
	// reclaimPendingPrompt in cmd/gonk-gate/sweep.go). The second exists
	// because the first cannot cover a dispatch process killed outright, which
	// runs no defer. T-55/T-58 replace this whole mechanism with a
	// deterministic Job name and a 409 at the API server; T-58 deletes this
	// state once that lands.
	StatePendingPrompt State = "pending-prompt"
)

// Record is one work item. BeadAnchor is the IDEMPOTENCY KEY (Plan 02): firing
// the same order twice must not create a second bead, because a second bead is
// a second session and a second session is duplicate spend.
type Record struct {
	BeadAnchor string // "gonk:{project_id}:issue:{iid}" -- the key
	BeadID     string
	Project    string
	ProjectID  int64
	Rig        string
	SessionKey string
	Trigger    string
	IssueIID   int64
	ConfigHash string

	State   State
	Rung    string
	Model   string
	Attempt int
	// ReservationID and ReservationExpiresAt come from METER's DecideResponse,
	// verbatim, on the pour that put this record into StateRunning. The
	// sweeper needs both: ReservationID to bind an OutcomeRequest to the
	// reservation meter actually minted (an unbound outcome is a forgery
	// vector), and ReservationExpiresAt to compute gate.Signals.ReservationExpired
	// without asking meter again.
	ReservationID        string
	ReservationExpiresAt time.Time
	RetryAfter           time.Time
	// SessionEndedAt is stamped by whatever observes the session finish (Gas
	// City's tracking-id lifecycle, out of this plan's available facts -- see
	// ADR/TODO in cmd/gonk-gate/sweep.go). The sweeper only classifies a
	// `running` record once this is non-zero: a zero value means "still
	// running, nothing to do this tick", not "ended at the epoch".
	SessionEndedAt time.Time
	// SessionID is the Gas City session id (from `gc session new`) the broker
	// dispatched for this bead; stamped at inject, read by sweep to locate the
	// returned effects batch. Empty on pre-broker records.
	SessionID string
	// CheckoutSHA is the full commit id the session's checkout was pinned to at
	// grant time (gonk-qdr4): the exact tree the agent was shown. Stamped on the
	// `running` record; empty when no checkout was granted (the shape needs none,
	// the rig is unconfigured, or the ref could not be resolved) and on records
	// written before the field existed. Additive JSON field -- no migration.
	CheckoutSHA string
	UpdatedAt   time.Time

	// ---- pipeline-diagnosis records only (gonk-tios D12). All zero on every
	// other record; additive JSON fields, no migration. Documented in
	// docs/reference/gonk-state-record.md.
	//
	// The run identity, from the order (intake computed it, D2), and reused by
	// every attempt and re-sling of the anchor: PipelineID is the pipeline the
	// session diagnoses (the newest failed one of the run at first dispatch);
	// PipelineTarget is "mr:<iid>" or "ref:<name>"; PipelineFingerprint and
	// PipelineLastSuccess are what BeadAnchor was built from (0 = no green
	// pipeline on the target).
	PipelineID          int64
	PipelineTarget      string
	PipelineFingerprint string
	PipelineLastSuccess int64
	// PipelineFixEpoch is the fifth part of the run identity (gonk-2usw 3.3):
	// the iid of the merged gonk pipeline-fix MR this run FOLLOWS, i.e. the
	// ":f<iid>" suffix of BeadAnchor. 0 -- the run follows no merged fix --
	// is omitted from the stored JSON, so such a record is byte-identical to
	// one written before the field existed.
	PipelineFixEpoch int64 `json:"PipelineFixEpoch,omitempty"`
	// PipelineSHA is the diagnosed pipeline's commit. The checkout is pinned to
	// it (CheckoutSHA, when a checkout was granted), and the sweep checks that
	// every file a diagnosis cites exists AT it -- which it must do whether or
	// not a checkout was granted, so it is recorded separately.
	PipelineSHA string
	// PipelineProjectURL is the project's WEB url (no trailing slash), e.g.
	// "https://gitlab.example/group/project" (gonk-385g). It exists so that
	// every controller-written run note can LINK the pipelines and jobs it
	// names: "<PipelineProjectURL>/-/pipelines/<id>", "/-/jobs/<id>". It is
	// derived by the controller from the web_url GitLab's own pipeline API
	// object carries (never from the order, never from model output), and
	// only stored when it is a plain http(s) URL that is safe inside a
	// markdown link. "" on a record written before the field existed, and on
	// a park/deny record written before any pipeline was read: a note then
	// names the pipeline in plain text. Run maintenance fills an empty one
	// from the pipelines it observes.
	PipelineProjectURL string
	// PipelineJobs is every failed job of the diagnosed pipeline as the session
	// was shown it, with the sha256 of each excerpt (D8 step 5 re-derives and
	// compares it). PipelineExcerptVersion is pipelinectx.ExcerptVersion and
	// PipelineExcerptMaxBytes the ONE per-job byte cap every excerpt of the
	// dispatch was built with (P6: recorded, because the 28 KiB fit makes it
	// irreproducible from the job list).
	PipelineJobs            []PipelineJob
	PipelineExcerptVersion  int
	PipelineExcerptMaxBytes int
	// PipelineFirstFailed / PipelineLatestFailed: the run's first failing
	// pipeline (as the dispatch saw it; 0 = unknown) and the newest failing
	// pipeline of the run seen so far (the sweep advances it while the run
	// stays red, D9 "still failing").
	PipelineFirstFailed  int64
	PipelineLatestFailed int64
	// PipelineRunOpen: the run has not ended. Set by dispatch on the running,
	// parked and needs-human writes; cleared by the sweep's run maintenance
	// when the target goes green (resolved), a different failure set begins
	// (superseded), or the MR target is no longer open. PipelineCheckedAt is
	// when that maintenance last looked (it looks at most every 5 minutes).
	PipelineRunOpen   bool
	PipelineCheckedAt time.Time
	// PipelineRunFixMR and PipelineRunFixState are, ON THE DIAGNOSED RUN'S
	// RECORD, the fix merge request gonk opened for this run and the fix
	// state last written to the run note's fix line (gonk-2usw plan 5.4
	// step 4, 8.1). 0 / "" until a fix merge request exists. Written by the
	// exit check (cmd/gonk-gate sweep_pipefix_verify.go); the run note's
	// status line is rendered from them.
	PipelineRunFixMR    int64  `json:",omitempty"`
	PipelineRunFixState string `json:",omitempty"`
	// PipelineDiagnosis is the diagnosis the sweep LANDED for this run
	// (gonk-2usw): the validated `diagnosis` effect's class, cited files,
	// evidence jobs, summary and remedy, set in the same pass that wrote the
	// run note in state diagnosed and persisted by that pass's record write.
	// The zero value (Recorded() false) means none was landed -- or the record
	// was written before the field existed. See PipelineDiagnosis.
	PipelineDiagnosis PipelineDiagnosis

	// ---- pipeline-fix records only (gonk-2usw). The zero value is omitted
	// from the stored JSON, so every other record is byte-identical to one
	// written before the field existed. Documented in
	// docs/reference/gonk-state-record.md, "Pipeline fix".
	//
	// A fix record is keyed by the FIX anchor (pipelinectx.FixAnchor of the
	// diagnosed run's anchor), has Trigger "pipeline-fix", and carries the
	// diagnosed run's identity in the Pipeline* fields above (PipelineID is
	// the run's newest failing pipeline when the fix was dispatched).
	PipelineFix PipelineFix `json:"PipelineFix,omitzero"`

	// Revision identifies ONE WRITE of this record (gonk-p8w8). The store
	// assigns a fresh random value on every successful Put and PutIf; callers
	// never set it, and a value a caller passes in is overwritten. Its only use
	// is as PutIf's precondition: "write this only if the record is still the
	// one I read". It is an opaque token, not a counter -- nothing may compare
	// two revisions for order.
	//
	// Empty on a record written before the field existed. PutIf treats that
	// empty revision as a real value, so a pass that read a legacy record can
	// still write it conditionally.
	Revision string

	// ReslungAt is when gonk-sweep reported this record's outcome and fired the
	// re-sling (escalate/retry). Zero on the records dispatch builds (fresh from
	// the webhook args) -- but NOT on every record dispatch writes: a failed
	// dispatch's release puts back the record it found verbatim, ReslungAt
	// included, and the sweep's retry of that failed re-sling depends on it.
	//
	// It is what stops a broker record from being judged twice (gonk-p8w8). A
	// broker record's SessionEndedAt is not a usable "already judged" marker --
	// nothing stamps it, so sweepRunning ignores it (gonk-u1p.5) -- and the
	// re-sling writes the record back in StateRunning with its old alias, which
	// looks exactly like a session still waiting to be judged. See
	// sweepRunning's use of it for why it is a grace window and not a skip.
	ReslungAt time.Time

	// ReslingFireBy is the deadline gonk-sweep passed with that re-sling's
	// order (the gonk-dispatch `fire_by` var): a gonk-dispatch process that
	// starts after it exits without writing anything (gonk-p8w8). It is what
	// makes the re-sling window ENFORCED rather than assumed. Gas City's 300s
	// clock for a dispatch starts before the process does, so any dispatch this
	// re-sling fired that was allowed to run started its clock no later than
	// ReslingFireBy, and was killed by ReslingFireBy + the dispatch timeout +
	// gc's 2s SIGTERM-to-SIGKILL grace -- however late gc actually fired it (its
	// order-run handler is detached from the client and writes a tracking bead
	// first) and however many times gcapi retried the POST.
	//
	// Stored rather than recomputed from ReslungAt so a later change to the
	// allowance cannot move the window of a record already fired. Carried back
	// verbatim by a failed dispatch's release, like ReslungAt.
	ReslingFireBy time.Time

	// RetryFeedback is what the controller tells the NEXT attempt about the
	// answer a static gate refused (gonk-nf9i, gonk-zxsx): which attempt, the
	// refusal's code, and one controller-built sentence each for what was
	// wrong and what is allowed. Written by gonk-sweep in the write that
	// records the re-sling, carried onto every record gonk-dispatch writes
	// for the same anchor, and cleared when an answer is applied. It is on
	// the record -- not in a process -- so the re-dispatch after a controller
	// restart renders the same prompt. See RetryFeedback and
	// docs/reference/gonk-state-record.md, "Retry feedback".
	RetryFeedback RetryFeedback `json:",omitzero"`
}

// RetryFeedback is a refused attempt's feedback as recorded. EVERY string in
// it was built by the controller (pkg/effects Explain*): fixed sentences,
// closed-set values, counts, and at most one short filtered token of the
// refused answer. It never holds a free-text field of that answer. The
// prompt renderer filters and caps each field again all the same.
type RetryFeedback struct {
	// Attempt is the attempt whose answer was refused, from 1. 0 = none.
	Attempt int `json:",omitempty"`
	// Code is the refusal's stable code ("parse", "no-batch", "FX-SHAPE(kind)",
	// "diagnosis_invalid"), "" when it has none.
	Code string `json:",omitempty"`
	// What says what was wrong; Allowed states the exact allowed set, "" when
	// the refusal is not about a set.
	What    string `json:",omitempty"`
	Allowed string `json:",omitempty"`
}

// Recorded reports whether there is feedback to give.
func (f RetryFeedback) Recorded() bool { return f.Attempt > 0 && f.What != "" }

// PipelineJob is one failed job of a diagnosed pipeline, as recorded at
// dispatch (Record.PipelineJobs).
type PipelineJob struct {
	ID   int64
	Name string
	// Excerpted: an excerpt of this job's log was built and shown. False for
	// jobs beyond the first four, and for a job whose log was unreadable at
	// dispatch: such a job has no excerpt, so no `line` evidence can name it.
	Excerpted bool
	// ExcerptSHA256 is the sha256 of the excerpt text ("" unless Excerpted).
	ExcerptSHA256 string
	// Groundable: the excerpt holds at least one quotable log line.
	Groundable bool

	// THE JOB'S FAILURE FACTS (gonk-2usw), recorded at dispatch from GitLab's
	// job object and the excerpt -- never from the session's answer. They are
	// what a pipeline-fix is later judged against, per job: the job failure
	// key is (Stage, Name, FailureReason, ErrorSignature)
	// (pipelinectx.JobFailureKey).
	//
	// SignatureVersion is pipelinectx.SignatureVersion at dispatch and doubles
	// as "these facts were recorded": it is 0 on a job recorded before the
	// fields existed, whose Stage and FailureReason are then "" because they
	// were never written, not because GitLab said so (FailureFactsRecorded).
	Stage         string
	FailureReason string // GitLab's job failure_reason, e.g. "script_failure"
	// AllowFailure is the job's allow_failure at dispatch. Always false today:
	// PipelineJobs lists only the jobs that failed the pipeline. Recorded so
	// "this job was required" is a stored fact the exit check compares
	// against, not an inference from the list's membership rule.
	AllowFailure bool
	// ErrorSignature is pipelinectx.ErrorSignature of the job's first error
	// line ("es<SignatureVersion>-<16 hex>"). "" when the job has no excerpt
	// (not Excerpted) or its log has no error line.
	ErrorSignature string
	// ErrorGeneric: the first error line is the runner's own job-failed
	// summary, or there is none (pipelinectx.GenericFirstError). Recorded
	// because it cannot be recovered from ErrorSignature, which is a hash.
	// Read it through GenericFailure, which also covers records without it.
	ErrorGeneric     bool
	SignatureVersion int
}

// FailureFactsRecorded reports whether dispatch recorded this job's failure
// facts (Stage ... SignatureVersion). False on a job recorded before
// gonk-2usw: such a job has no failure key to judge a fix against.
func (j PipelineJob) FailureFactsRecorded() bool { return j.SignatureVersion != 0 }

// GenericFailure is the pipeline-fix plan's "generic signature" (3.2): the
// job's signature cannot tell this failure from another one of the same job,
// because there is none or because it was built from the runner's summary.
// True for a job with no signature whatever ErrorGeneric says, so a record
// written before the flag existed reads as generic (the conservative side).
func (j PipelineJob) GenericFailure() bool { return j.ErrorSignature == "" || j.ErrorGeneric }

// PipelineDiagnosis is the landed diagnosis of a pipeline run
// (Record.PipelineDiagnosis): what the validated `diagnosis` effect said,
// kept as controller-held facts so a later dispatch need not parse the note.
//
// Everything here passed the sweep's whole validation chain before it was
// stored (shape, grounding, cited files exist at PipelineSHA, no secret in the
// rendered note). It is still MODEL-AUTHORED text and a model's choice of
// class, files and jobs: a reader may use it to NARROW what it does, never to
// widen it.
type PipelineDiagnosis struct {
	// Class is the effects.Classification, e.g. "code". "" = nothing recorded.
	Class string
	// Files are the cited repository-relative paths, in the diagnosis's order
	// (at most 10). Each existed at Record.PipelineSHA when the diagnosis
	// landed.
	Files []string
	// EvidenceJobs are the distinct job names the evidence items name, in
	// first-cited order. Each is the Name of an entry of Record.PipelineJobs.
	// A pipeline-level evidence item (no job) contributes nothing, so this is
	// empty for a diagnosis resting on pipeline_error alone.
	EvidenceJobs []string
	// Summary and Remedy are the diagnosis's prose (at most 1500 characters
	// each), with NUL characters removed (JSONB cannot hold them).
	Summary string
	Remedy  string
}

// Recorded reports whether a landed diagnosis is on the record.
func (d PipelineDiagnosis) Recorded() bool { return d.Class != "" }

// PipelineFix is what the CONTROLLER decided about one pipeline fix when it
// dispatched it (gonk-2usw plan 3.2, 4.2): which diagnosed run it is for, the
// jobs it will be judged on, the tree it is written against and the scope
// envelope it may write inside. gonk-sweep's eligibility pass writes it, in
// the same write that makes the fix exist; nothing in it comes from a
// session, an order var or a batch, and a later part may NARROW by it and
// never widen.
type PipelineFix struct {
	// RunAnchor is the BeadAnchor of the diagnosed run this fix is for. ""
	// means the record is not a fix record.
	RunAnchor string
	// State is the fix state of plan 7.1 (pkg/pipefix State), set ONLY by the
	// part that decides it: `needs-scope` by the dispatch-time scope check,
	// and everything from `awaiting-run` on by parts APPLY and VERIFY. It is
	// "" while the fix is being proposed -- the record's own State (running,
	// parked, pending-prompt, needs-human) then says where it is; see
	// cmd/gonk-gate fixStateOf for the mapping.
	State string `json:",omitempty"`
	// CauseKey is pipelinectx.CauseKey over Targets (16 hex): the identity of
	// the diagnosed root cause, and what the fix MR's marker carries.
	CauseKey string
	// Class is the diagnosis class the envelope was chosen for (code, test,
	// configuration).
	Class string
	// BaseSHA is the commit the fix is written against: the head of the
	// target, which was also the commit of the run's newest failing pipeline
	// (BasePipelineID), when the fix was dispatched (plan 3.2 E7).
	BaseSHA        string
	BasePipelineID int64
	// TargetBranch is the branch the fix merge request targets: the ref of a
	// ref target, the SOURCE branch of an MR target.
	TargetBranch string
	// ConfigSHA is the default-branch commit the project's `scope:` section
	// was read at for the fold below (066 invariant 3: never the fix branch).
	ConfigSHA string
	// Envelope is the set of path classes this fix may write (pkg/scope
	// Class values): plan 4.2's row for Class, inside the pack / operator /
	// project fold, lifted classes included. Lift is the subset of Envelope
	// that is a PROTECTED class lifted for this dispatch -- "ci" or nothing --
	// granted from the project's `pipeline_fix.allow_ci` by the controller.
	// A scope.Lift cannot be stored; the part that validates a batch mints
	// it again from this list through scope.Effective.GrantLift, which
	// re-checks the pack row, the opt-in and the operator veto.
	Envelope []string
	Lift     []string `json:",omitempty"`
	// Files are the diagnosis's cited files that are inside
	// `pipeline_fix.paths` (all of them when Paths is empty): the ONLY paths
	// an edit may touch. Paths is the project's narrowing globs as folded.
	Files []string
	Paths []string `json:",omitempty"`
	// The size caps of the fold (scope.Caps after the layers and
	// pipeline_fix.max_files / max_changed_lines).
	MaxFiles      int64
	MaxLines      int64
	MaxFileBytes  int64
	MaxPatchBytes int64
	// CIExecChanges is `pipeline_fix.ci_exec_changes` as folded at dispatch
	// (deny | manual-run | auto). MaxAttempts and VerifyTimeoutSeconds are
	// `pipeline_fix.max_attempts` and `verify_timeout` likewise.
	CIExecChanges        string
	MaxAttempts          int
	VerifyTimeoutSeconds int64
	// Targets is the target set T (plan 3.2): the failed, required jobs the
	// diagnosis's evidence names, with the job failure key each was recorded
	// with. The exit check judges the fix on exactly these. AlsoFailing are
	// the run's other recorded failed jobs, by name: "also failing, not
	// addressed by this merge request".
	Targets     []PipelineFixTarget
	AlsoFailing []string `json:",omitempty"`
	// BaseJobs is EVERY job of the base pipeline (BasePipelineID), latest
	// attempts, as GitLab listed them when the fix was decided: the canonical
	// job name, the status and allow_failure. The exit check needs it to tell
	// a job the fix broke (it passed here) from one that was already failing.
	BaseJobs []PipelineFixBaseJob
	// ExcerptVersion and SignatureVersion are pipelinectx's versions under
	// which Targets' signatures were recorded (the run record's): a later
	// failure can only be compared with them by a build of the same versions.
	ExcerptVersion   int
	SignatureVersion int
	// FiredAt is when gonk-sweep fired the pipeline-fix order for this record.
	FiredAt time.Time
	// ContextSHA256 is the hash of the context the fix session was shown, as
	// its preparer reported it at dispatch (plan 5.2; the apply's context
	// integrity check, 5.3 A9, compares against it). "" until a session was
	// started.
	ContextSHA256 string `json:",omitempty"`
	// Context is what the fix session was shown (FixContext, fixcontext.go):
	// the commit, each excerpted job's log hash, the diagnosis's hash, every
	// cited file's blob hash and the limits the prompt stated. gonk-dispatch
	// writes it in the same write that makes the record `running`;
	// ContextSHA256 is the sha256 of its JSON encoding. The apply's gate A9
	// (verifyPipefixContext) re-derives every hash in it before any write.
	Context FixContext `json:",omitzero"`

	// ---- written by the APPLY (gonk-2usw plan 5.3, 5.4), never before ----

	// AppliedAttempt is the meter attempt whose batch passed the static gates
	// and was applied. 0 until then.
	AppliedAttempt int `json:",omitempty"`
	// FixCommitSHA is the commit gonk made on the fix branch for
	// AppliedAttempt: the ONLY head the exit check judges (plan 6.3 J0).
	FixCommitSHA string `json:",omitempty"`
	// MRIID is the fix merge request's iid (source branch
	// pipelinectx.FixBranch of this record's anchor). 0 until it is open.
	MRIID int64 `json:",omitempty"`
	// ChangedFiles are the files the applied commit changed, with the path
	// class each was judged under and the broker-computed line counts.
	ChangedFiles []PipelineFixFile `json:",omitempty"`
	// Weakening is the test-weakening fact of the applied change (066
	// SC-WEAKEN; plan 6.5 U4): "decided" -- every changed source or test path
	// belongs to a profile with a deterministic predicate (Go), and the
	// predicate passed; "not-decidable" -- some changed path has no predicate,
	// so NOTHING says its tests were not weakened and the fix must never be
	// merged automatically. "" until a fix was applied.
	Weakening string `json:",omitempty"`
	// WeakeningUndecided are the changed paths that made Weakening
	// "not-decidable", sorted.
	WeakeningUndecided []string `json:",omitempty"`
	// CIDelta is the definition-integrity result of the applied commit
	// against the base (plan 6.2, pkg/cidef Kind): "none", "preserving" or
	// "exec-changing". "" until the post-commit check ran. A "violation" or
	// "unresolvable" result is never recorded here: the first is a refusal
	// (state not-fixed), the second leaves the fix unapplied.
	CIDelta string `json:",omitempty"`
	// VerifyDeadline is when the exit check stops waiting for the merge
	// request's pipeline (the apply's clock + VerifyTimeoutSeconds).
	VerifyDeadline time.Time `json:",omitzero"`
	// Refusal is the rule code of the static gate that refused the LAST
	// judged batch of this fix (e.g. "SC-WEAKEN(removed-test)", "A7(allow_failure)"),
	// "" when the last batch was accepted or none was judged. For the next
	// attempt's feedback and for a person reading the record.
	Refusal string `json:",omitempty"`

	// ---- written by the EXIT CHECK (gonk-2usw plan 6, 7; part VERIFY) ----

	// JudgedAt is when the exit check last looked at this fix's merge
	// request. The check looks at most once per two minutes (plan 6.3 J1),
	// and this is what it measures from.
	JudgedAt time.Time `json:",omitzero"`
	// Verdict is the exit check's answer for VerdictAttempt (a pkg/pipefix
	// Verdict: FORGED, NOT_FIXED, FLAKY, UNVERIFIABLE, REGRESSED,
	// FIXED_NEW_FAILURE, FIXED), "" until one was given or when the fix
	// ended without one (the merge request was merged, closed or taken
	// over). VerdictRule is the rule code that decided it: a row of plan 6.3
	// ("J3.RECURS") or the check's own bound ("VF.BOUND(...)").
	// VerdictDetail says it in the controller's words.
	Verdict        string `json:",omitempty"`
	VerdictRule    string `json:",omitempty"`
	VerdictDetail  string `json:",omitempty"`
	VerdictAttempt int    `json:",omitempty"`
	// JudgedPipelines are the merge request pipelines the verdict was given
	// on (plan 6.3's C), ascending.
	JudgedPipelines []int64 `json:",omitempty"`
	// Findings are the per-job findings of that verdict, controller-rendered
	// one per entry as "<rule> <job>" (J3 for the diagnosed jobs, J4 for the
	// others): what the notes and the next attempt's feedback are built from.
	Findings []string `json:",omitempty"`
	// Outcome is the meter outcome reported for OutcomeAttempt (plan 7.3):
	// reported once per attempt, by the exit check. "" until then.
	Outcome        string `json:",omitempty"`
	OutcomeAttempt int    `json:",omitempty"`
	// NoAutoMerge lists why this fix must never be merged automatically,
	// whatever a later policy says (plan 6.5 U2, U4; 9.1 A3, A4), sorted:
	// "weakening-not-decidable", "ci-exec-changing", "verdict-<state>" for a
	// verdict other than `fixed`. Empty only for a `fixed` fix with decided
	// weakening and no execution-changing definition delta -- and an empty
	// list GRANTS nothing: merging is a separate policy (plan 9).
	NoAutoMerge []string `json:",omitempty"`
	// Feedback is the controller-written sentence the NEXT attempt's prompt
	// is given about the attempt just judged (plan 5.2), built from the
	// verdict's rule codes. Set with `not-fixed`; cleared by nothing (the
	// attempt after next overwrites it).
	Feedback string `json:",omitempty"`
	// MergedAt is the fix merge request's merged_at, and MergedSHAs the
	// commits by which it can be on the target (merge commit, squash commit,
	// head), once it was seen merged (plan 7.4). ConfirmPipelineID is the
	// target pipeline that decided `confirmed` or `recurred`.
	MergedAt          time.Time `json:",omitzero"`
	MergedSHAs        []string  `json:",omitempty"`
	ConfirmPipelineID int64     `json:",omitempty"`
}

// PipelineFixFile is one file an applied fix changed.
type PipelineFixFile struct {
	Path         string
	Class        string
	LinesAdded   int
	LinesRemoved int
}

// Values of PipelineFix.Weakening.
const (
	FixWeakeningDecided      = "decided"
	FixWeakeningNotDecidable = "not-decidable"
)

// PipelineFixTarget is one job of a fix's target set, as recorded.
type PipelineFixTarget struct {
	Stage          string
	Name           string
	FailureReason  string
	ErrorSignature string
	// Generic: the signature cannot tell this failure from another failure
	// of the same job (PipelineJob.GenericFailure at the diagnosis).
	Generic bool `json:",omitempty"`
}

// PipelineFixBaseJob is one job of a fix's base pipeline, as recorded.
type PipelineFixBaseJob struct {
	Name         string
	Status       string
	AllowFailure bool `json:",omitempty"`
}

// Recorded reports whether the record is a pipeline-fix record.
func (f PipelineFix) Recorded() bool { return f.RunAnchor != "" }

// IsZero is what `omitzero` asks: a record that is not a fix record stores no
// PipelineFix key at all.
func (f PipelineFix) IsZero() bool {
	return f.RunAnchor == "" && f.State == "" && f.CauseKey == "" && f.Class == "" && f.BaseSHA == "" &&
		f.BasePipelineID == 0 && f.TargetBranch == "" && f.ConfigSHA == "" && len(f.Envelope) == 0 &&
		len(f.Lift) == 0 && len(f.Files) == 0 && len(f.Paths) == 0 && f.MaxFiles == 0 && f.MaxLines == 0 &&
		f.MaxFileBytes == 0 && f.MaxPatchBytes == 0 && f.CIExecChanges == "" && f.MaxAttempts == 0 &&
		f.VerifyTimeoutSeconds == 0 && len(f.Targets) == 0 && len(f.AlsoFailing) == 0 && f.FiredAt.IsZero() &&
		f.ContextSHA256 == "" && len(f.BaseJobs) == 0 && f.ExcerptVersion == 0 && f.SignatureVersion == 0 &&
		f.Context.IsZero() && f.AppliedAttempt == 0 && f.FixCommitSHA == "" && f.MRIID == 0 &&
		len(f.ChangedFiles) == 0 && f.Weakening == "" && len(f.WeakeningUndecided) == 0 && f.CIDelta == "" &&
		f.VerifyDeadline.IsZero() && f.Refusal == "" &&
		f.JudgedAt.IsZero() && f.Verdict == "" && f.VerdictRule == "" && f.VerdictDetail == "" && f.VerdictAttempt == 0 &&
		len(f.JudgedPipelines) == 0 && len(f.Findings) == 0 && f.Outcome == "" && f.OutcomeAttempt == 0 &&
		len(f.NoAutoMerge) == 0 && f.Feedback == "" && f.MergedAt.IsZero() && len(f.MergedSHAs) == 0 &&
		f.ConfirmPipelineID == 0
}

// ErrStale is what PutIf returns (wrapped) when the stored record is no longer
// the one the caller read. Test for it with errors.Is. Nothing was written.
var ErrStale = errors.New("beadstore: the record changed after it was read; nothing written")

type Store interface {
	// Put upserts on BeadAnchor and STAMPS UpdatedAt and Revision -- see
	// stampWriteTime for the UpdatedAt rule and why it belongs to the
	// interface rather than to each caller. Callers set neither field.
	//
	// Put is UNCONDITIONAL: it replaces whatever is stored. It is right only for
	// a writer that is the authority for the record at the moment it writes, and
	// wrong for one writing back a copy it read earlier -- see PutIf. Since
	// gonk-b2jk the only Put caller outside tests and the bd import/export is
	// dispatch's running-record write; its pending-prompt reservation, its
	// release and its park/deny writes are PutIf (reasons in
	// docs/plans/2026-09-13-y4m7-dispatch-conditional-writes.md s3).
	Put(ctx context.Context, r Record) error

	// PutIf is Put with a precondition: it writes r only if the stored
	// record's Revision is still ifRevision, and returns the Revision it wrote.
	// ifRevision "" matches an absent record or one written before revisions
	// existed. If the precondition does not hold it writes nothing and returns
	// an error wrapping ErrStale.
	//
	// It exists because an unconditional write-back of a record read minutes
	// earlier is how gonk-sweep erased a dispatch's pending-prompt reservation
	// and let a second session through for one bead+attempt (gonk-p8w8).
	PutIf(ctx context.Context, r Record, ifRevision string) (string, error)

	Get(ctx context.Context, beadAnchor string) (Record, bool, error)
	List(ctx context.Context, state State) ([]Record, error)

	// ListOpenPipelineRuns returns every record whose PipelineRunOpen is true,
	// in ANY state, in byte order of BeadAnchor (List's order).
	//
	// It exists so gonk-sweep's pipeline run maintenance reads only the runs
	// it can act on (verify report R6). Listing every done / needs-human /
	// parked record every 30 s tick and filtering in Go is a full scan plus a
	// JSON decode of a set that only grows; a CLOSED run is never needed
	// again, so a store must not hand it back. Postgres answers from a partial
	// index on the open runs alone (work_items_pipeline_run_open_idx).
	ListOpenPipelineRuns(ctx context.Context) ([]Record, error)
}

// AllStates is every State a record can be in.
var AllStates = []State{StateRunning, StateParked, StateNeedsHuman, StateDone, StatePendingPrompt}

// newRevision returns a fresh write token: 128 random bits, hex. Random rather
// than a counter because the two stores' writers do not coordinate -- a
// counter would need the very read-modify-write this field exists to guard.
func newRevision() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on a supported platform; if it ever does,
		// a predictable token would silently weaken every precondition.
		panic("beadstore: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// stampWrite applies every store-owned field for one write: UpdatedAt by
// stampWriteTime's rule, and a fresh Revision ALWAYS -- including for a
// pending-prompt record that keeps its UpdatedAt, because keeping the write
// TIME is about the reservation's age while the revision is about identity:
// a re-write is a different write.
func stampWrite(r Record, now time.Time) Record {
	r = stampWriteTime(r, now)
	r.Revision = newRevision()
	return r
}

// stampWriteTime is THE UpdatedAt RULE, and it lives here -- at the Store
// interface, not at each call site -- because the alternative was tried and
// failed. Only Memory.Put used to stamp; BdCLI.Put marshalled the record
// verbatim. That made UpdatedAt a per-caller convention: whichever call site
// remembered to set it got a working timestamp, and against the real store
// every one that forgot wrote a zero time. gonk-sweep's pending-prompt
// reclaim ages records by exactly this field, so a forgotten stamp does not
// fail a test -- it makes the sweep silently inert in production while every
// Memory-backed test stays green. Both Put implementations call this, so a
// stamp cannot be forgotten by a future call site, and a third implementation
// inherits the check through storetest's conformance suite.
//
// THE ONE EXCEPTION, and it is load-bearing: a record written back in
// StatePendingPrompt that ALREADY carries a write time keeps it. A
// reservation's age is measured from when it was RESERVED, and re-writing the
// same reservation is not re-reserving it.
//
// Without the exception, runBrokerDispatch's release path defeats the sweep it
// depends on. That path restores the record as it stood BEFORE the reservation
// write (see broker_inject.go); if what it restores is itself a stranded
// pending-prompt record, an unconditional stamp would reset that record's
// reclaim clock on every failed dispatch -- deferring the reclaim by a full
// grace window each time, for a record whose whole problem is that nobody is
// coming back for it. The reservation write itself is unaffected: it is built
// from a record runDispatch rebuilds fresh from the webhook args, so its
// UpdatedAt is zero and gets stamped here.
//
// Every other write restamps, including the reclaim's own promotion out of
// StatePendingPrompt into StateRunning: that write DOES change the record, so
// its write time moves.
func stampWriteTime(r Record, now time.Time) Record {
	if r.State == StatePendingPrompt && !r.UpdatedAt.IsZero() {
		return r
	}
	r.UpdatedAt = now
	return r
}
