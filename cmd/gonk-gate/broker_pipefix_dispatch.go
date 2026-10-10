package main

// The dispatch half of a pipeline fix (gonk-2usw plan 3.2, 5.2, 8.1): what a
// `pipeline-fix` order must carry, what gonk-dispatch checks BEFORE meter is
// asked, the seam through which the fix session's prompt and checkout are
// prepared (and its production implementation over part AGENT's functions), and the fix note's dispatch-time states.
//
// THE ORDER CARRIES AN IDENTITY, NOT A DECISION. A pipeline-fix order's vars
// are the diagnosed run's identity (pipeline_target, pipeline_fingerprint,
// pipeline_last_success, pipeline_fix_epoch), the pipeline the fix is written
// against (pipeline_id), the FIX anchor as bead_anchor and the fix session
// key. Everything the controller DECIDED about the fix -- the scope envelope,
// the lift, the cited files, the caps, the target set, the base commit -- is
// on the fix record gonk-sweep wrote before it fired the order, and is read
// from THERE. An order for which no such record exists dispatches nothing: no
// var can supply an envelope.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// ---- THE SEAM TO PART AGENT -------------------------------------------------

// pipefixPrepareInput is everything a fix session is prepared from. All of it
// is controller-held: nothing here was said by a session.
type pipefixPrepareInput struct {
	// Session is the deterministic session name (runtime.Name of the pipefix
	// agent, the project, the fix's base pipeline and the attempt). A checkout
	// is granted to this name.
	Session string
	// Fix is the fix record as dispatch is about to write it: Attempt is
	// meter's, and PipelineFix holds the envelope, lift, files, caps, target
	// set and base commit the sweep decided.
	Fix beadstore.Record
	// Run is the diagnosed run's record: the landed diagnosis
	// (PipelineDiagnosis), the recorded failed jobs (PipelineJobs) and the
	// run identity.
	Run beadstore.Record
	// Rung is the rung meter decided for this attempt.
	Rung string
}

// pipefixSession is what a prepared fix session is started with.
type pipefixSession struct {
	// Prompt is the whole prompt, within the session's prompt cap.
	Prompt string
	// CheckoutURL / CheckoutSHA: the rig grant for this session, pinned to
	// Fix.PipelineFix.BaseSHA (grantCheckoutAt). Both "" when no checkout
	// could be granted; whether a fix session may run without one is the
	// preparer's decision -- it returns an error if not.
	CheckoutURL string
	CheckoutSHA string
	// ContextSHA256 is the hash of the context the session was shown (plan
	// 5.2, 5.3 A9). Recorded on the fix record as PipelineFix.ContextSHA256.
	ContextSHA256 string
	// Context is that context itself: what the apply's gate A9 re-derives
	// (verifyPipefixContext). Recorded as PipelineFix.Context in the same
	// write; ContextSHA256 is pipefixContextHash of it.
	Context beadstore.FixContext
}

// pipefixPreparer prepares ONE fix session for a record: it builds the
// context and the prompt, and grants the checkout. An error means no session
// can be started (dispatch exits 1 and its reservation is released), exactly
// as preparePipelineDispatch's does.
//
// It is called AFTER meter said run and the pending-prompt reservation was
// taken, and BEFORE the session starts. It must not write to GitLab and must
// not write the record: dispatch records what it returns.
type pipefixPreparer func(ctx context.Context, d dispatchDeps, in pipefixPrepareInput) (pipefixSession, error)

// pipefixPrepare is the production preparer: part AGENT's functions behind
// part DISPATCH's seam (prepareFixSession). main.go passes it to dispatch and
// turns the sweep's fix pass on with it (sweepDeps.FixesLive).
//
// WHAT KEEPS THE PATH INERT NOW IS THE PROJECT'S OPT-IN, NOT A NIL: the pass
// reads one meter registration per project per look and stops at E1 for a
// project without actions.pipeline_fixes (TestAnOptedOutProjectIsInert); an
// order with no fix record is refused before meter is asked
// (pipefixPreflight); meter's own action gate denies the trigger; and the
// preparer below refuses a project that is not opted in even when a record
// says otherwise (refoldPipefixEnvelope, errPipefixNotOptedIn). A dispatchDeps
// built WITHOUT a preparer still refuses every pipeline-fix order (exit 2).
var pipefixPrepare pipefixPreparer = prepareFixSession

// pipefixContextHash is the hash recorded as PipelineFix.ContextSHA256: the
// sha256 of the fix context's JSON encoding (struct order, no map anywhere in
// it, so the encoding is stable).
func pipefixContextHash(fc beadstore.FixContext) string {
	raw, err := json.Marshal(fc)
	if err != nil {
		panic(err) // strings, numbers, bools and slices of them always marshal
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// fixConfig reads the project's effective configuration for a fix dispatch:
// d.FixConfig when set (tests), else gonk-meter.
func (d dispatchDeps) fixConfig(ctx context.Context, project string) (*meterapi.ProjectResponse, error) {
	if d.FixConfig != nil {
		return d.FixConfig(ctx, project)
	}
	if d.Meter == nil {
		return nil, errors.New("no meter client configured")
	}
	return d.Meter.Project(ctx, project)
}

// prepareFixSession is the seam's production implementation. It folds the
// fix's envelope again from its record (refoldPipefixEnvelope: still opted
// in, still the classes, lift and caps the sweep recorded, the lift minted
// here by GrantLift), builds the request from the DIAGNOSED run's record and
// the fix record's base, and calls part AGENT's preparePipefixDispatch.
//
// It writes nothing. An error means no session:
// errors.Is(err, errPipefixNothingEditable) is needs-scope at dispatch, which
// runBrokerDispatch lands (settleFixNeedsScope); anything else is exit 1.
func prepareFixSession(ctx context.Context, d dispatchDeps, in pipefixPrepareInput) (pipefixSession, error) {
	var none pipefixSession
	pf := in.Fix.PipelineFix
	if d.Pipelines == nil {
		return none, fmt.Errorf("%w: no pipeline forge configured", errPipefixContext)
	}
	cfg, err := d.fixConfig(ctx, in.Fix.Project)
	if err != nil {
		return none, fmt.Errorf("%w: read the project's effective configuration: %v", errPipefixEnvelope, err)
	}
	env, err := refoldPipefixEnvelope(ctx, d.Pipelines, cfg, d.packDir(), in.Fix, d.Log)
	if err != nil {
		return none, err
	}
	if in.Run.BeadAnchor != pf.RunAnchor {
		return none, fmt.Errorf("%w: the run record %q is not the run this fix is for (%q)", errPipefixContext, in.Run.BeadAnchor, pf.RunAnchor)
	}
	req, err := pipefixRequestFromRecord(in.Run, pf.BaseSHA, pf.BasePipelineID, env, in.Fix.Attempt)
	if err != nil {
		return none, err
	}
	// A LATER ATTEMPT is told what the exit check found and shown the change
	// that did not meet the criterion (plan 5.2). Both are the controller's:
	// the sentence was built from the verdict's rule codes (fixFeedback), and
	// the change is read back from the repository at the two recorded commits.
	if pf.Feedback != "" && pf.FixCommitSHA != "" { // dispatch:previous
		req.Previous = &pipefixPrevious{Result: pf.Feedback, Diff: previousFixChange(ctx, d.Pipelines, in.Fix, d.Log)}
	}
	// A THIRD SOURCE of the same feedback (gonk-nf9i): an answer phase A
	// refused never reached the exit check, so it left no Feedback above and
	// no commit -- and the retry used to be prompted byte for byte as before.
	req.Refused = in.Fix.RetryFeedback // dispatch:refused
	out, err := preparePipefixDispatch(ctx, d, in.Session, req)
	if err != nil {
		return none, err
	}
	d.Log.Info("pipeline fix session prepared", "bead", in.Fix.BeadAnchor, "session", in.Session,
		"base_sha", pf.BaseSHA, "base_pipeline", pf.BasePipelineID, "editable", len(out.editable),
		"checkout", out.checkoutSHA != "", "prompt_bytes", len(out.prompt),
		"refusal_feedback", renderRetryFeedback(req.Refused, req.Attempt) != "")
	return pipefixSession{Prompt: out.prompt, CheckoutURL: out.checkoutURL, CheckoutSHA: out.checkoutSHA,
		ContextSHA256: pipefixContextHash(out.context), Context: out.context}, nil
}

// previousFixChange renders what the previous attempt's commit changed: for
// each file it changed, the lines of the base commit it replaced and the
// lines it put there (the common head and tail of the file left out).
// Controller-computed from two reads per file; a file that cannot be read is
// named as unavailable, never guessed. The prompt renders it fenced and
// capped (pipefixCapPreviousDiff).
func previousFixChange(ctx context.Context, r pipefixContextReader, fix beadstore.Record, log *slog.Logger) string {
	pf := fix.PipelineFix
	var b strings.Builder
	for _, f := range pf.ChangedFiles {
		limit := max(pf.MaxFileBytes, 1) * 2
		base, berr := r.GetRawFile(ctx, fix.ProjectID, f.Path, pf.BaseSHA, limit)
		head, herr := r.GetRawFile(ctx, fix.ProjectID, f.Path, pf.FixCommitSHA, limit)
		fmt.Fprintf(&b, "--- %s\n", f.Path)
		if berr != nil || herr != nil {
			log.Warn("pipeline fix: the previous attempt's change to a file could not be read; the prompt says so",
				"bead", fix.BeadAnchor, "path", f.Path, "err", errors.Join(berr, herr))
			b.WriteString("(unavailable)\n")
			continue
		}
		removed, added := changedLines(string(base), string(head))
		for _, l := range removed {
			b.WriteString("-" + l + "\n")
		}
		for _, l := range added {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

// changedLines trims the lines two texts share at their head and at their
// tail and returns what is left of each: the lines `a` lost and the lines `b`
// has in their place.
func changedLines(a, b string) (removed, added []string) {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for len(al) > 0 && len(bl) > 0 && al[0] == bl[0] {
		al, bl = al[1:], bl[1:]
	}
	for len(al) > 0 && len(bl) > 0 && al[len(al)-1] == bl[len(bl)-1] {
		al, bl = al[:len(al)-1], bl[:len(bl)-1]
	}
	return al, bl
}

// settleFixNeedsScope lands "needs-scope, decided at dispatch" (plan 3.2 E6,
// 4.4 way 1) for a fix whose preparer found NO cited file editable at the
// base commit -- a file that is gone, empty, binary, generated or over the
// size cap, which the sweep's class check (E6) cannot see. Zero tokens: no
// session was started.
//
// Called by runBrokerDispatch AFTER its pending-prompt reservation was
// released (the record is back to what the sweep wrote). In order: the fix
// record becomes terminal (done, fix state needs-scope) with a PutIf over the
// record as it now stands; meter's reservation is settled `aborted`, as a
// capped attempt's is; the landing says a person is needed. A lost write
// leaves the record for the next look, which fires again and lands here again.
func settleFixNeedsScope(ctx context.Context, d dispatchDeps, base beadstore.Record, dec meterapi.DecideResponse) {
	a := d.Args
	cur, found, err := d.Store.Get(ctx, a.BeadAnchor)
	if err != nil || !found || cur.State != "" || cur.PipelineFix.State != "" {
		d.Log.Warn("pipeline fix: needs-scope at dispatch could not be recorded; the next look decides again",
			"bead", a.BeadAnchor, "found", found, "state", string(cur.State), "err", err)
		return
	}
	rec := cur
	rec.State = beadstore.StateDone
	rec.PipelineFix.State = string(pipefix.StateNeedsScope)
	rec.ReslungAt, rec.ReslingFireBy = time.Time{}, time.Time{}
	if _, err := d.Store.PutIf(ctx, rec, cur.Revision); err != nil {
		d.Log.Warn("pipeline fix: needs-scope at dispatch could not be recorded; the next look decides again",
			"bead", a.BeadAnchor, "err", err)
		return
	}
	d.Log.Warn("pipeline fix: no cited file may be edited at the base commit; needs-scope, no session started",
		"bead", a.BeadAnchor, "base_sha", rec.PipelineFix.BaseSHA)
	// `aborted`: no session ran; nothing to escalate, nothing to retry.
	if id := dec.ReservationID; id != "" {
		if _, err := d.Meter.Outcome(ctx, meterapi.OutcomeRequest{
			Project: a.Project, BeadID: a.BeadAnchor, SessionKey: a.SessionKey, Attempt: dec.Attempt, Rung: dec.Rung,
			ReservationID: id, Trigger: a.Trigger, Outcome: meterapi.OutcomeAborted,
		}); err != nil {
			d.Log.Warn("pipeline fix: could not settle the reservation of a fix that needs scope; it expires on its own",
				"bead", a.BeadAnchor, "reservation", dec.ReservationID, "err", err)
		}
	}
	d.postFixNote(ctx, base, d.fixRun, fixNote{State: string(pipefix.StateNeedsScope), Text: fixNothingEditableText(base)}, true)
}

// ---- before meter is asked ---------------------------------------------------

// checkPipelineFixIdentity refuses a pipeline-fix order whose vars do not
// rebuild its own anchor: bead_anchor must be exactly the FIX anchor of the
// run the vars identify, session_key the fix session key of that anchor, and
// the order must name the pipeline the fix is written against.
func checkPipelineFixIdentity(a dispatchArgs) error {
	if a.PipelineID < 1 {
		return fmt.Errorf("no pipeline id (pipeline_id %d)", a.PipelineID)
	}
	t, err := pipelinectx.ParseTarget(a.PipelineTarget)
	if err != nil {
		return err
	}
	run, err := pipelinectx.Anchor(a.ProjectID, t, a.PipelineFingerprint, a.PipelineLastSuccess, a.PipelineFixEpoch)
	if err != nil {
		return err
	}
	want, err := pipelinectx.FixAnchor(run)
	if err != nil {
		return err
	}
	if want != a.BeadAnchor {
		return fmt.Errorf("bead_anchor %q is not the fix anchor of this run identity (%q)", a.BeadAnchor, want)
	}
	key, err := pipelinectx.FixSessionKey(want)
	if err != nil {
		return err
	}
	if key != a.SessionKey {
		return fmt.Errorf("session_key %q is not the session key of this fix (%q)", a.SessionKey, key)
	}
	return nil
}

// pipefixPreflight is everything a pipeline-fix dispatch checks before meter
// is asked, from the work store alone (no GitLab request, no spend):
//
//   - the fix record the sweep wrote exists, is this fix's, and carries the
//     controller's decisions; an order without one is refused (exit 2);
//   - the fix has not already ended (a recorded fix state): nothing to do;
//   - the diagnosed run is still open, still diagnosed, and its newest failing
//     pipeline is still the one the fix was decided against. If not, the fix
//     is RELEASED: the record goes back to "not dispatched" so the sweep's
//     next look decides again on current facts, and nothing is dispatched.
//     This is what stops a fix that meter parked from being unparked, hours
//     later, against a tree that has moved.
//
// proceed=false means dispatch stops now and exits with code.
func pipefixPreflight(ctx context.Context, d dispatchDeps, decided decidedOnRecord) (run beadstore.Record, code int, proceed bool) {
	a := d.Args
	if decided.err != nil {
		d.Log.Error("pipeline fix: the fix record could not be read; dispatching nothing", "bead", a.BeadAnchor, "err", decided.err)
		return run, 1, false
	}
	fix := decided.rec
	if !decided.found || !fix.PipelineFix.Recorded() || fix.Trigger != atags.TriggerPipelineFix {
		d.Log.Error("pipeline fix: refusing an order for which gonk-sweep recorded no fix; dispatching nothing "+
			"(the scope envelope comes from that record, never from the order)", "bead", a.BeadAnchor)
		return run, 2, false
	}
	wantRun, err := pipelinectx.RunAnchorOfFix(a.BeadAnchor)
	if err != nil || fix.PipelineFix.RunAnchor != wantRun {
		d.Log.Error("pipeline fix: the fix record names another run than this order's anchor; dispatching nothing",
			"bead", a.BeadAnchor, "record_run", fix.PipelineFix.RunAnchor, "err", err)
		return run, 2, false
	}
	if fix.PipelineFix.State != "" {
		d.Log.Info("pipeline fix: this fix already has a recorded state; dispatching nothing",
			"bead", a.BeadAnchor, "fix_state", fix.PipelineFix.State)
		return run, 0, false
	}
	if fix.PipelineFix.BasePipelineID != a.PipelineID {
		d.Log.Warn("pipeline fix: the order names another pipeline than the fix was decided against; dispatching nothing "+
			"(a later look has re-decided this fix: its own order dispatches it)",
			"bead", a.BeadAnchor, "order_pipeline", a.PipelineID, "record_pipeline", fix.PipelineFix.BasePipelineID)
		return run, 0, false
	}
	run, found, err := d.Store.Get(ctx, wantRun)
	if err != nil {
		d.Log.Error("pipeline fix: the diagnosed run's record could not be read; dispatching nothing", "bead", a.BeadAnchor, "err", err)
		return run, 1, false
	}
	stale := fixRunStale(run, found, fix)
	if stale == "" {
		return run, 0, true
	}
	d.Log.Warn("pipeline fix: not dispatching: "+stale+"; releasing the fix so the next look decides again",
		"bead", a.BeadAnchor, "run", wantRun, "base_pipeline", fix.PipelineFix.BasePipelineID)
	if fix.State == "" || fix.State == beadstore.StateParked {
		// Only a record nobody is acting on. A reserved or running one is its
		// dispatch's, and is left alone. The release clears the fire window
		// and the park, so fixStateOf reads the record as "not dispatched" at
		// once and nothing unparks it.
		released := fix
		released.State = ""
		released.RetryAfter = time.Time{}
		released.ReslungAt, released.ReslingFireBy = time.Time{}, time.Time{}
		if _, err := d.Store.PutIf(ctx, released, fix.Revision); err != nil && !errors.Is(err, beadstore.ErrStale) {
			d.Log.Warn("pipeline fix: the release could not be written", "bead", a.BeadAnchor, "err", err)
		}
	}
	return run, 0, false
}

// fixRunStale says why a fix can no longer be dispatched against its diagnosed
// run, or "" when it can: the run must still exist, be open, carry its landed
// diagnosis, and its newest failing pipeline must still be the one the fix
// was decided against. gonk-dispatch asks it before meter (pipefixPreflight);
// the exit check asks it before it re-slings a not-fixed fix, so it never
// fires an order this would refuse.
func fixRunStale(run beadstore.Record, found bool, fix beadstore.Record) string {
	switch {
	case !found:
		return "the diagnosed run has no record"
	case !run.PipelineRunOpen:
		return "the diagnosed run is closed"
	case !run.PipelineDiagnosis.Recorded() || run.State != beadstore.StateDone:
		return "the run carries no landed diagnosis"
	case max(run.PipelineLatestFailed, run.PipelineID) != fix.PipelineFix.BasePipelineID:
		return "the run has a newer failing pipeline than the one this fix was decided against"
	}
	return ""
}

// setPipelineFixIdentity puts, on the record dispatch writes, the run identity
// and the controller's decisions about the fix -- copied from the STORED fix
// record, never from the order (pipefixPreflight has checked the two agree).
func setPipelineFixIdentity(base *beadstore.Record, stored beadstore.Record) {
	base.PipelineID = stored.PipelineID
	base.PipelineTarget = stored.PipelineTarget
	base.PipelineFingerprint = stored.PipelineFingerprint
	base.PipelineLastSuccess = stored.PipelineLastSuccess
	base.PipelineFixEpoch = stored.PipelineFixEpoch
	base.PipelineSHA = stored.PipelineSHA
	base.PipelineProjectURL = stored.PipelineProjectURL
	base.PipelineFix = stored.PipelineFix
	// What the sweep recorded about a refused earlier answer goes with the
	// fix to its next attempt (retry_feedback.go).
	base.RetryFeedback = stored.RetryFeedback // dispatch:carry-feedback
	// NOT PipelineRunOpen: a fix record is not a run, and run maintenance
	// must never be handed one.
	base.PipelineRunOpen = false
}

// capFixAttempts ends a fix whose next attempt would exceed the project's
// `pipeline_fix.max_attempts`. Meter said run and minted a reservation for
// the attempt; no session is started for it. In order:
//
//  1. the fix record is written needs-human (fix state `denied`), a PutIf over
//     the record meter decided on, exactly as a deny is;
//  2. the reservation is settled `aborted` -- "cancelled by a config change",
//     which neither escalates nor counts as an infrastructure failure -- so
//     the budget headroom it holds is returned now rather than at its TTL.
//     Best effort: if meter cannot be told, the reservation expires on its
//     own and is booked infra-failed, which does not escalate either;
//  3. the landing says a person is needed.
//
// THE COUNT IS METER'S ATTEMPT NUMBER, which includes attempts that ended as
// infrastructure failures. That is stricter than "attempts that opened a
// merge request", and deliberately the simple reading: the dispatch cannot
// see how earlier attempts ended, and a cap that errs low costs a person a
// look, while one that errs high costs the project pipelines.
func capFixAttempts(ctx context.Context, d dispatchDeps, base beadstore.Record, decided decidedOnRecord, dec meterapi.DecideResponse) int {
	a := d.Args
	base.State = beadstore.StateNeedsHuman
	if code, written := writeDecided(ctx, d, base, decided); !written {
		return code
	}
	d.Log.Warn("pipeline fix: the project's max_attempts is reached; starting no session",
		"bead", a.BeadAnchor, "attempt", dec.Attempt, "max_attempts", base.PipelineFix.MaxAttempts)
	if dec.ReservationID != "" {
		if _, err := d.Meter.Outcome(ctx, meterapi.OutcomeRequest{
			Project: a.Project, BeadID: a.BeadAnchor, SessionKey: a.SessionKey, Attempt: dec.Attempt, Rung: dec.Rung,
			ReservationID: dec.ReservationID, Outcome: meterapi.OutcomeAborted, Trigger: a.Trigger,
		}); err != nil {
			d.Log.Warn("pipeline fix: could not settle the capped attempt's reservation; it expires on its own",
				"bead", a.BeadAnchor, "reservation", dec.ReservationID, "err", err)
		}
	}
	detail := fmt.Sprintf("this project allows at most %d attempt(s) per fix (`pipeline_fix.max_attempts`), and attempt %d would be the next",
		base.PipelineFix.MaxAttempts, dec.Attempt)
	d.postFixNote(ctx, base, d.fixRun, fixNote{State: string(pipefix.StateDenied), Text: fixCappedText(base, detail)}, true)
	return 0
}

// ---- the fix note, at dispatch -------------------------------------------------

// postFixNote writes the fix note's dispatch-time state (proposing, denied)
// on the diagnosed run's landing. Called only AFTER the record write it
// reports has landed. BEST EFFORT, bounded by statusNoteTimeout: a failure is
// logged at WARN and never changes the exit code or the record.
func (d dispatchDeps) postFixNote(ctx context.Context, fix, run beadstore.Record, n fixNote, needsHuman bool) {
	if d.Pipelines == nil || terminated(ctx) {
		return
	}
	nctx, cancel := context.WithTimeout(ctx, statusNoteTimeout)
	defer cancel()
	l, err := newPipelineLander(nctx, d.Pipelines, d.Log)
	if err != nil {
		d.Log.Warn("pipeline fix note not written", "bead", fix.BeadAnchor, "state", n.State, "err", err)
		return
	}
	changed, ld, err := l.writeFixNote(nctx, run, fix.BeadAnchor, n)
	if err != nil {
		d.Log.Warn("pipeline fix note not written", "bead", fix.BeadAnchor, "state", n.State, "err", err)
		return
	}
	if changed && needsHuman {
		l.addLandingLabel(nctx, run, ld, labelNeedsHuman)
	}
	// A fix that is denied AFTER an attempt opened its merge request (the
	// attempt that was to follow a not-fixed verdict was refused by meter or
	// by max_attempts) leaves that merge request open. Its state label must
	// not go on saying "verifying": one request swaps it for
	// gonk::fix-failed. Best effort, like the landing label.
	if n.State == string(pipefix.StateDenied) && fix.PipelineFix.MRIID > 0 { // dispatch:denied-label
		if _, lerr := d.Pipelines.UpdateMRLabels(nctx, fix.ProjectID, fix.PipelineFix.MRIID,
			[]string{labelFixFailed}, []string{labelFixVerifying, labelFixAwaitingRun}); lerr != nil {
			d.Log.Warn("pipeline fix: the open merge request's state label could not be updated", "bead", fix.BeadAnchor,
				"mr", fix.PipelineFix.MRIID, "err", lerr)
		}
	}
	d.Log.Info("pipeline fix note written", "bead", fix.BeadAnchor, "state", n.State, "run", run.BeadAnchor,
		"target", run.PipelineTarget, "changed", changed)
}

// fixProposing and fixDenied are the two notes dispatch writes.
func fixProposing(fix beadstore.Record, attempt int, rung string) fixNote {
	return fixNote{State: string(pipefix.StateProposing), Text: proposingText(fix, attempt, rung)}
}

func fixDenied(fix beadstore.Record, reason, detail string) fixNote {
	return fixNote{State: string(pipefix.StateDenied), Text: fixDeniedText(fix, reason, detail)}
}
