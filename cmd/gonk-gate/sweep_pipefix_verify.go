package main

// THE EXIT CHECK OF A PIPELINE FIX, PHASE B (gonk-2usw plan 6, 7.1-7.3, 8.1;
// part VERIFY).
//
// The apply (broker_pipefix_apply.go) stops at "merge request open": the fix
// record stays `running`, holds its meter reservation, carries
// PipelineFix.State `verifying` or `awaiting-run`, and NO outcome has been
// reported for the attempt. This file takes it from there. For every such
// record, on a sweep tick (sweepRunning -> sweepRunningFix -> judgeOpenFix),
// at most once per pipefixJudgeEvery:
//
//  1. FACTS. The recorded half of pipefix.JudgeFacts comes from the record
//     (fixFactsOf); the judge-time half from GitLab through the six GETs of
//     pipefixReader (readPipefixJudgeFacts). Nothing comes from a session.
//  2. DECISION. pipefix.Judge, pure. This file adds no rule to it, with one
//     exception that is not a judgement of the fix: ITS OWN BOUND. Judge
//     turns "still waiting" into UNVERIFIABLE at the verify deadline, but
//     "cannot judge" (a recorded fact missing, the merge request unreadable,
//     a malformed reply) has no deadline there -- and a record that cannot be
//     judged must still be able to END. Past the verify deadline plus
//     pipefixJudgeGrace, a fix that still has no verdict is settled
//     `unverifiable` by rule VF.BOUND(<what>).
//  3. ACTION, by the Decision:
//
//	wait, cannot-judge   nothing but a stamp (JudgedAt) so the next look is
//	                     two minutes away. `awaiting-run` becomes `verifying`
//	                     once a pipeline that runs something exists.
//	verdict / stopped    settleFix: the GitLab writes, THEN the meter outcome,
//	                     THEN the record -- see below.
//
// THE STATE MACHINE (plan 7.1) AND THE METER OUTCOME (7.3), as built:
//
//	decision                     fix state          meter outcome   record
//	FIXED                        fixed              success         done
//	FIXED_NEW_FAILURE            fixed-new-failure  success         done
//	FLAKY                        flaky              success         done
//	UNVERIFIABLE (J1/J2/J3, or   unverifiable       success         done
//	  VF.BOUND)
//	REGRESSED                    regressed          success         done
//	FORGED, NOT_FIXED            not-fixed, then:   gate-failed *
//	  attempts left, run fresh     (state cleared)                  running, re-slung
//	  otherwise                    denied                           needs-human
//	stopped: merged              merged             success         done
//	stopped: closed              closed-unmerged    success         done
//	stopped: somebody pushed     taken-over         success         done
//
//	* through gate.Classify: an escalation must be PAID for, so a not-fixed
//	  attempt whose tokens meter cannot show is infra-failed (a retry of the
//	  same rung), never gate-failed.
//
// "success" means "do not spend another rung on this", not "the pipeline is
// fixed" (plan 7.3): the fix STATE says what happened, and it is logged with
// the outcome on one line per settled attempt ("sweep: pipeline fix
// settled") so the two are never conflated.
//
// ORDER OF A SETTLEMENT, AND WHY. GitLab first (the fix note, the merge
// request's status block and labels, the run note's fix line), then meter,
// then the record. Each GitLab write is idempotent and each later step is
// skipped when an earlier one failed, so a failure leaves a record that is
// still `verifying` and the next look does the whole settlement again. The
// meter's outcome is idempotent per reservation (a second report of a
// reported attempt is a no-op there), so "again" cannot book a second
// outcome; the record write is what ends the repetition, and it is a PutIf
// over the revision this look read. Past the bound the GitLab writes are no
// longer a precondition: a GitLab that stays down must not hold a
// reservation for ever.
//
// THE NEXT ATTEMPT. After not-fixed with attempts left, the record goes back
// to "being dispatched": PipelineFix.State is CLEARED (gonk-dispatch refuses a
// fix record that carries one) while FixCommitSHA, MRIID and ChangedFiles are
// KEPT, Feedback is set from the verdict's rule codes, and the record is
// re-slung exactly as sweepRunning re-slings (ReslungAt, ReslingFireBy, the
// gonk-dispatch order). The next session is shown the feedback and the
// previous change; its apply commits on top of FixCommitSHA and adopts the
// open merge request.
//
// THIS FILE NEVER MERGES. pipefixVerifyForge has no accept call, and
// TestTheExitCheckCannotMerge pins its method set.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/gate"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const (
	// pipefixJudgeEvery bounds how often one open fix is looked at (plan 6.3
	// J1: "re-look at most every 2 min; zero tokens").
	pipefixJudgeEvery = 2 * time.Minute
	// pipefixJudgeGrace is how long past its verify deadline a fix may stay
	// without a verdict before the exit check settles it itself (VF.BOUND).
	// Judge already answers UNVERIFIABLE at the deadline for everything it
	// can time out; the grace is for what it cannot (see the file comment),
	// and for a GitLab outage that straddles the deadline.
	pipefixJudgeGrace = 30 * time.Minute
)

// Rule codes of the exit check's own bound. They are not rows of plan 6.3:
// Judge gave no verdict, and the check stopped waiting for one.
const (
	ruleVerifyBoundCannotJudge = "VF.BOUND(cannot-judge)"
	ruleVerifyBoundWait        = "VF.BOUND(wait)"
)

// Reasons a fix must never be merged automatically (PipelineFix.NoAutoMerge).
const (
	noAutoMergeWeakening = "weakening-not-decidable"
	noAutoMergeExec      = "ci-exec-changing"
	noAutoMergeVerdict   = "verdict-" // + the fix state
)

// pipefixVerifyForge is what the exit check reads and writes on GitLab.
// Satisfied by *glab.Client. THE WRITES ARE EXACTLY: UpdateMergeRequest (the
// description's status block and the labels, in ONE request), and the note
// and label writes of pipelineForge. There is no method here that merges,
// approves, assigns, pushes or retries a job.
type pipefixVerifyForge interface {
	pipelineForge
	pipefixReader
	UpdateMergeRequest(ctx context.Context, projectID, mrIID int64, u glab.MRUpdate) (*glab.MergeRequest, error)
}

var _ pipefixVerifyForge = (*glab.Client)(nil)

// fixFactsOf is the RECORDED half of pipefix.JudgeFacts (plan 6.1), copied
// from the fix record. Recorded is true only for a fix record the apply
// finished with: Judge names every other missing field itself.
func fixFactsOf(rec beadstore.Record) pipefix.FixFacts {
	pf := rec.PipelineFix
	targets := make([]pipefix.Target, 0, len(pf.Targets))
	for _, t := range pf.Targets {
		targets = append(targets, pipefix.Target{
			Key: pipelinectx.JobFailureKey{Stage: t.Stage, Name: t.Name, FailureReason: t.FailureReason, ErrorSignature: t.ErrorSignature},
			// The same rule TargetSet applied at dispatch: no signature is a
			// generic one.
			Generic: t.Generic || t.ErrorSignature == "",
		})
	}
	base := make([]pipefix.BaseJob, 0, len(pf.BaseJobs))
	for _, j := range pf.BaseJobs {
		base = append(base, pipefix.BaseJob{Name: j.Name, Status: j.Status, AllowFailure: j.AllowFailure})
	}
	return pipefix.FixFacts{
		Recorded:         pf.Recorded() && rec.Trigger == atags.TriggerPipelineFix && pf.AppliedAttempt > 0,
		Targets:          targets,
		BasePipelineID:   pf.BasePipelineID,
		BaseJobs:         base,
		FixBaseSHA:       pf.BaseSHA,
		FixCommitSHA:     pf.FixCommitSHA,
		MRIID:            pf.MRIID,
		ExcerptVersion:   pf.ExcerptVersion,
		SignatureVersion: pf.SignatureVersion,
		Lift:             pf.Lift,
		CIExecChanges:    pf.CIExecChanges,
		VerifyDeadline:   pf.VerifyDeadline,
	}
}

// fixVerifyBound is when the exit check stops waiting for a verdict: the
// verify deadline plus the grace. A record with no verify deadline (the apply
// always writes one) is bounded by the reservation's deadline instead.
func fixVerifyBound(rec beadstore.Record) time.Time {
	deadline := rec.PipelineFix.VerifyDeadline
	if deadline.IsZero() {
		deadline = runningDeadline(rec) // verify:bound-fallback
	}
	return deadline.Add(pipefixJudgeGrace)
}

// fixReservationSlack is what runningDeadline's FALLBACK adds for a
// pipeline-fix record: the reservation meter mints for the trigger is
// reservation_ttl + verify_timeout (rung.ReservationTTL), so a record that
// lost its deadline is bounded by the default TTL plus the verify timeout it
// recorded -- clamped as meter clamps it.
func fixReservationSlack(rec beadstore.Record) time.Duration {
	if rec.Trigger != atags.TriggerPipelineFix {
		return 0
	}
	slack := time.Duration(rec.PipelineFix.VerifyTimeoutSeconds) * time.Second
	return min(max(slack, 0), gonkcfg.FixVerifyTimeoutCeiling)
}

// staleFixSessionAfter is staleSessionAfter for a pipefix session: its Job's
// own deadline is the fix reservation's, which can be the verify-timeout
// ceiling longer than any other session's.
const staleFixSessionAfter = opercfg.DefaultReservationTTL + gonkcfg.FixVerifyTimeoutCeiling + time.Hour

// fixSettlement is what the exit check decided for one attempt.
type fixSettlement struct {
	state   pipefix.State
	verdict pipefix.Verdict // "" when the fix ended without one (stopped)
	rule    string
	detail  string
	res     pipefix.Result
	// bound: this is the check's own bound, not Judge's answer.
	bound bool
}

// judgeOpenFix is pipefixJudge: one look at one fix whose merge request is
// open.
func judgeOpenFix(ctx context.Context, d sweepDeps, rec beadstore.Record) {
	pf := rec.PipelineFix
	if !fixOpenStates[pf.State] {
		// A running record in any other fix state is not something this
		// build writes; touching it would be guessing.
		d.Log.Warn("sweep: a running pipeline-fix record carries a fix state the exit check does not own; leaving it",
			"bead", rec.BeadAnchor, "fix_state", pf.State)
		return
	}
	now := d.Now()
	if !pf.JudgedAt.IsZero() && now.Sub(pf.JudgedAt) < pipefixJudgeEvery { // verify:throttle
		return
	}
	forge, ok := d.Pipelines.(pipefixVerifyForge)
	if d.Pipelines == nil || !ok {
		d.Log.Warn("sweep: a pipeline fix is waiting for its exit check and no forge that can run it is configured",
			"bead", rec.BeadAnchor, "mr", pf.MRIID)
		return
	}
	facts, ev := readPipefixJudgeFacts(ctx, forge, rec.ProjectID, fixFactsOf(rec))
	res := pipefix.Judge(facts, now)
	v := &fixVerify{d: d, f: forge, rec: rec, readRevision: rec.Revision, now: now}

	// THE ONE LINE PER LOOK.
	d.Log.Info("sweep: pipeline fix exit check", "bead", rec.BeadAnchor, "mr", pf.MRIID, "commit", pf.FixCommitSHA,
		"fix_state", pf.State, "decision", string(res.Decision), "verdict", string(res.Verdict), "rule", string(res.Rule),
		"state", string(res.State), "candidates", fmt.Sprint(res.Candidates), "definition", string(res.Definition),
		"unread", unreadReads(ev), "verify_deadline", pf.VerifyDeadline, "detail", res.Detail)

	switch res.Decision {
	case pipefix.DecisionVerdict:
		v.settle(ctx, fixSettlement{state: res.State, verdict: res.Verdict, rule: string(res.Rule), detail: res.Detail, res: res})
	case pipefix.DecisionStopped:
		v.settle(ctx, fixSettlement{state: res.State, rule: string(res.Rule), detail: res.Detail, res: res})
	default:
		// wait, cannot-judge: no verdict. The check's own bound.
		if !now.Before(fixVerifyBound(rec)) { // verify:bound
			rule := ruleVerifyBoundWait
			if res.Decision == pipefix.DecisionCannotJudge {
				rule = ruleVerifyBoundCannotJudge
			}
			v.settle(ctx, fixSettlement{state: pipefix.StateUnverifiable, verdict: pipefix.VerdictUnverifiable, rule: rule,
				detail: "no verdict could be given before the verify deadline and its grace had passed: " + res.Detail,
				res:    res, bound: true})
			return
		}
		v.keepWaiting(ctx, res)
	}
}

// unreadReads names the reads that failed, for the look's log line.
func unreadReads(ev pipefixEvidence) string {
	var out []string
	for _, r := range ev.Reads {
		if !r.OK {
			out = append(out, r.What+" "+r.Ref)
		}
	}
	return strings.Join(out, ", ")
}

// fixVerify is one look that has a decision to act on.
type fixVerify struct {
	d            sweepDeps
	f            pipefixVerifyForge
	rec          beadstore.Record // the fix record as this look read it
	readRevision string
	now          time.Time
}

func (v *fixVerify) put(ctx context.Context, rec beadstore.Record, why string) bool {
	if _, err := v.d.Store.PutIf(ctx, rec, v.readRevision); errors.Is(err, beadstore.ErrStale) {
		v.d.Log.Warn("sweep: pipeline fix: the record changed after this look read it; leaving the newer record in place",
			"bead", rec.BeadAnchor, "why", why)
		return false
	} else if err != nil {
		v.d.Log.Error("sweep: pipeline fix: the record could not be written; the next look repeats this",
			"bead", rec.BeadAnchor, "why", why, "err", err)
		return false
	}
	return true
}

// keepWaiting records that the fix was looked at and has no verdict yet.
func (v *fixVerify) keepWaiting(ctx context.Context, res pipefix.Result) {
	rec := v.rec
	rec.PipelineFix.JudgedAt = v.now
	state := pipefix.State(rec.PipelineFix.State)
	// awaiting-run -> verifying: a pipeline that runs something exists on
	// the fix commit (a maintainer started it; plan 5.5, 7.1).
	if state == pipefix.StateAwaitingRun && res.Decision == pipefix.DecisionWait && len(res.Candidates) > 0 { // verify:started
		state = pipefix.StateVerifying
		rec.PipelineFix.State = string(state)
	}
	// GitLab says what the record says -- written only when it does not yet:
	// a waiting look that changes nothing reads the merge request's pipeline
	// and NOTHING of the landing (plan 6.3 J1: the re-look is cheap).
	if v.landingBehind(ctx, rec, state) { // verify:landing-behind
		v.show(ctx, rec, state, fixOpenedText(rec, rec.PipelineFix.MRIID, rec.PipelineFix.FixCommitSHA, state),
			fixStatusLine(state, rec.PipelineFix.FixCommitSHA), true)
	}
	v.put(ctx, rec, "waiting")
}

// landingBehind reports whether GitLab has not been told this fix state yet,
// from the work store alone: the diagnosed run's record carries the merge
// request and the state last SHOWN (writeRunFixLine writes them last, after
// the fix note and the merge request). A run that cannot be read is not
// behind: show could not write to its landing either.
func (v *fixVerify) landingBehind(ctx context.Context, rec beadstore.Record, state pipefix.State) bool {
	run, found, err := v.d.Store.Get(ctx, rec.PipelineFix.RunAnchor)
	if err != nil || !found {
		return false
	}
	return run.PipelineRunFixMR != rec.PipelineFix.MRIID || run.PipelineRunFixState != string(state)
}

// show writes one fix state to GitLab: the fix note on the diagnosis landing,
// the merge request's status block and state label, the run note's fix line.
// It reports whether ALL of them now say it. open marks the two states the
// apply also writes (verifying, awaiting-run), whose note is the apply's.
func (v *fixVerify) show(ctx context.Context, rec beadstore.Record, state pipefix.State, noteText, statusText string, open bool) bool {
	pf := rec.PipelineFix
	run, found, err := v.d.Store.Get(ctx, pf.RunAnchor)
	if err != nil || !found {
		v.d.Log.Warn("sweep: pipeline fix: the diagnosed run's record could not be read; its landing is not updated",
			"bead", rec.BeadAnchor, "run", pf.RunAnchor, "found", found, "err", err)
		return false
	}
	lander, err := newPipelineLander(ctx, v.f, v.d.Log)
	if err != nil {
		v.d.Log.Warn("sweep: pipeline fix: GitLab could not be asked who the bot is; nothing is shown", "bead", rec.BeadAnchor, "err", err)
		return false
	}
	ok := true
	// ---- the fix note. Never backwards (fixNoteMayReplace), never a
	// person's note (found among the bot's notes only).
	note := fixNote{State: string(state), Text: noteText, Apply: open, Verify: !open}
	changed, ld, nerr := lander.writeFixNote(ctx, run, rec.BeadAnchor, note) // verify:note
	switch {
	case errors.Is(nerr, errFixNoteAdvanced):
		// The note already says something later; that is not a failure of
		// this write.
		v.d.Log.Info("sweep: pipeline fix: the fix note already carries a later state; not rewriting it",
			"bead", rec.BeadAnchor, "state", string(state))
	case nerr != nil:
		v.d.Log.Warn("sweep: pipeline fix: the fix note could not be written", "bead", rec.BeadAnchor, "state", string(state), "err", nerr)
		ok = false
	case changed && fixStateNeedsHuman(state): // verify:needs-human
		lander.addLandingLabel(ctx, run, ld, labelNeedsHuman)
	}
	// ---- the merge request: status block and state label, ONE request.
	if merr := updateFixMR(ctx, v.f, lander.me, rec, state, statusText); merr != nil { // verify:mr
		v.d.Log.Warn("sweep: pipeline fix: the merge request's status could not be updated", "bead", rec.BeadAnchor,
			"mr", pf.MRIID, "state", string(state), "err", merr)
		ok = false
	}
	// ---- the run note's fix line (plan 5.4 step 4).
	if _, lerr := writeRunFixLine(ctx, v.d, lander, run, pf.MRIID, state); lerr != nil { // verify:run-line
		v.d.Log.Warn("sweep: pipeline fix: the run note's fix line could not be written", "bead", rec.BeadAnchor,
			"run", run.BeadAnchor, "err", lerr)
		ok = false
	}
	return ok
}

// fixStateNeedsHuman: the states that end with a person (plan 8.1's
// `gonk::needs-human` rows).
func fixStateNeedsHuman(s pipefix.State) bool {
	switch s {
	case pipefix.StateFlaky, pipefix.StateUnverifiable, pipefix.StateRegressed, pipefix.StateDenied, pipefix.StateRecurred:
		return true
	}
	return false
}

// settle ends the attempt: GitLab, then meter, then the record.
func (v *fixVerify) settle(ctx context.Context, s fixSettlement) {
	d, rec := v.d, v.rec
	pf := &rec.PipelineFix
	attempt := max(rec.Attempt, 1)

	// What the verdict leads to. `not-fixed` is not terminal (plan 7.1): the
	// next attempt, or `denied`.
	final, retry, whyNot := s.state, false, ""
	if s.state == pipefix.StateNotFixed {
		retry, whyNot = v.mayRetry(ctx)
		if !retry {
			final = pipefix.StateDenied
		}
	}

	// ---- the result, on the record (written last).
	pf.JudgedAt = v.now
	pf.Verdict, pf.VerdictRule, pf.VerdictDetail, pf.VerdictAttempt = string(s.verdict), s.rule, oneLine(s.detail, 600), attempt
	pf.JudgedPipelines = append([]int64(nil), s.res.Candidates...)
	pf.Findings = renderFindings(s.res)
	pf.NoAutoMerge = noAutoMergeReasons(rec, s, final)
	if def := s.res.Definition; def != "" && s.verdict != "" {
		pf.CIDelta = string(def) // the delta the verdict was given on (plan 9.1 A4)
	}
	if s.state == pipefix.StateNotFixed {
		pf.Feedback = fixFeedback(s)
	}

	// ---- 1. GitLab.
	noteText, statusText := fixVerdictTexts(rec, s, final, retry, whyNot)
	shown := v.show(ctx, rec, noteStateFor(final, retry), noteText, statusText, false)
	if !shown && v.now.Before(fixVerifyBound(rec)) { // verify:gitlab-first
		// Not settled: the record is still `verifying`, and the next look
		// does this again. (Past the bound GitLab is no longer waited for.)
		d.Log.Warn("sweep: pipeline fix: the verdict could not be shown on GitLab; not reporting it yet",
			"bead", rec.BeadAnchor, "state", string(final), "rule", s.rule)
		return
	}

	// ---- 2. the meter outcome, once for the attempt (plan 7.3).
	present, reported := pipefix.ArtifactPresent(s.state) // on the VERDICT's state: denied reports what not-fixed does
	if !reported {
		d.Log.Error("sweep: pipeline fix: the exit check reached a state that reports no outcome; leaving the record",
			"bead", rec.BeadAnchor, "state", string(s.state))
		return
	}
	signals := gate.Signals{ArtifactPresent: present}
	if !present {
		// An escalation must be paid for (pkg/gate): prove the tokens.
		tokens, stale := gatherSpend(ctx, d, rec)
		signals.ModelTokens, signals.SpendStale = tokens, stale
	}
	outcome := gate.Classify(signals) // verify:outcome
	resp, err := d.Meter.Outcome(ctx, meterapi.OutcomeRequest{
		Project: rec.Project, BeadID: rec.BeadAnchor, SessionKey: rec.SessionKey,
		Attempt: rec.Attempt, Rung: rec.Rung, ReservationID: rec.ReservationID, Outcome: outcome,
		Trigger: rec.Trigger,
	})
	if err != nil {
		d.Log.Error("sweep: POST /v1/policy/outcome failed for a judged pipeline fix; the next look repeats the settlement",
			"err", err, "bead", rec.BeadAnchor, "outcome", outcome)
		return
	}
	pf.Outcome, pf.OutcomeAttempt = outcome, attempt

	// THE ONE LINE PER SETTLED ATTEMPT: the fix state and the meter outcome
	// side by side (plan 7.3: the two are never conflated).
	d.Log.Info("sweep: pipeline fix settled", "bead", rec.BeadAnchor, "mr", pf.MRIID, "attempt", attempt, "rung", rec.Rung,
		"fix_state", string(final), "verdict", string(s.verdict), "rule", s.rule, "outcome", outcome,
		"next", resp.Next, "retry", retry, "no_retry_because", whyNot, "no_auto_merge", strings.Join(pf.NoAutoMerge, ","),
		"bound", s.bound)

	// ---- 3. the record.
	switch {
	case retry:
		// Back to "being dispatched" (see the file comment), stamped and
		// fired exactly as sweepRunning re-slings.
		pf.State = "" // verify:clear-state
		// The next attempt's first look is the tick after its apply, not
		// two minutes after this one.
		pf.JudgedAt = time.Time{}
		rec.SessionEndedAt = time.Time{}
		rec.ReslungAt = d.Now()
		rec.ReslingFireBy = reslingFireBy(rec.ReslungAt)
		if !v.put(ctx, rec, "re-sling") {
			return // no fire after a lost or failed write (gonk-p8w8, gonk-aavs)
		}
		if refire(ctx, d, rec) {
			d.Log.Info("sweep: pipeline fix re-sling fired", "bead", rec.BeadAnchor, "attempt_judged", attempt,
				"outcome", outcome, "fire_by", rec.ReslingFireBy)
		}
	case final == pipefix.StateDenied:
		pf.State = string(final)
		rec.State = beadstore.StateNeedsHuman
		v.put(ctx, rec, "denied")
	default:
		pf.State = string(final)
		rec.State = beadstore.StateDone
		v.put(ctx, rec, string(final))
	}
}

// mayRetry reports whether a further attempt may follow a not-fixed verdict,
// and why not. Two bounds, both the controller's:
//
//   - pipeline_fix.max_attempts, counted in meter's attempt numbers exactly
//     as gonk-dispatch counts it (capFixAttempts): the attempt just judged is
//     rec.Attempt, so the next one is refused when rec.Attempt has reached it;
//   - the diagnosed run is still the one the fix was decided against
//     (fixRunStale): gonk-dispatch refuses a fix whose run has closed or
//     moved, and a re-sling it refuses would be re-fired for ever.
func (v *fixVerify) mayRetry(ctx context.Context) (bool, string) {
	rec, pf := v.rec, v.rec.PipelineFix
	if pf.MaxAttempts > 0 && rec.Attempt >= pf.MaxAttempts { // verify:max-attempts
		return false, fmt.Sprintf("this project allows at most %d attempt(s) per fix (`pipeline_fix.max_attempts`)", pf.MaxAttempts)
	}
	run, found, err := v.d.Store.Get(ctx, pf.RunAnchor)
	if err != nil {
		return false, "the diagnosed run's record could not be read"
	}
	if stale := fixRunStale(run, found, rec); stale != "" { // verify:run-stale
		return false, stale
	}
	return true, ""
}

// noteStateFor is the fix-note state of a settlement: `not-fixed` while a
// further attempt follows (plan 8.1: "attempt n did not fix it; trying
// again"), the final state otherwise.
func noteStateFor(final pipefix.State, retry bool) pipefix.State {
	if retry {
		return pipefix.StateNotFixed
	}
	return final
}

// noAutoMergeReasons is PipelineFix.NoAutoMerge for a settled fix: every
// reason this fix must never be merged automatically, sorted.
func noAutoMergeReasons(rec beadstore.Record, s fixSettlement, final pipefix.State) []string {
	var out []string
	if rec.PipelineFix.Weakening != beadstore.FixWeakeningDecided { // automerge:weakening
		out = append(out, noAutoMergeWeakening)
	}
	if rec.PipelineFix.CIDelta == string(cidef.KindExecChanging) || s.res.Definition == cidef.KindExecChanging { // automerge:exec
		out = append(out, noAutoMergeExec)
	}
	if final != pipefix.StateFixed { // automerge:verdict
		out = append(out, noAutoMergeVerdict+string(final))
	}
	sort.Strings(out)
	return out
}

// renderFindings is PipelineFix.Findings: "<rule> <job>" for each diagnosed
// job (J3) and each other failing job (J4), job names through jobNameText.
func renderFindings(res pipefix.Result) []string {
	var out []string
	for _, j := range res.Jobs {
		out = append(out, string(j.Rule)+" "+jobNameText(j.Job))
	}
	for _, o := range res.Others {
		out = append(out, string(o.Rule)+" "+jobNameText(o.Job))
	}
	return out
}

// fixFeedback is the sentence the next attempt's prompt is given (plan 5.2):
// the verdict, the rule code that decided it, and the diagnosed jobs' findings.
// Controller-written; the only project-chosen text in it is job names, which
// go through jobNameText here and through oneLine where it is rendered.
func fixFeedback(s fixSettlement) string {
	var why string
	switch {
	case strings.HasPrefix(s.rule, "J2."):
		why = "the change altered the pipeline definition it is judged by; a fix must leave when a job runs, what may fail and (unless allowed) what it executes as they were"
	case s.rule == string(pipefix.RuleJ3Weakened):
		why = "a diagnosed job was allowed to fail in the fix's pipeline"
	default:
		why = "a diagnosed job still failed the way it was diagnosed in the fix's pipeline"
	}
	var jobs []string
	for _, j := range s.res.Jobs {
		jobs = append(jobs, fmt.Sprintf("%q %s", jobNameText(j.Job), j.Rule))
	}
	out := fmt.Sprintf("%s by rule %s: %s", s.verdict, s.rule, why)
	if len(jobs) > 0 {
		out += " (" + strings.Join(jobs, "; ") + ")"
	}
	return out
}

// ---- the merge request ------------------------------------------------------------

// fixStateLabels are the labels of which a fix merge request carries EXACTLY
// ONE at a time. gonk::fix-new-failure and gonk::ci-exec-changed qualify a
// state and are not in this set.
var fixStateLabels = []string{labelFixVerifying, labelFixAwaitingRun, labelFixVerified, labelFixInconclusive, labelFixFailed}

// fixStateLabel is the one state label of a fix state (plan 8.1). "" means
// the state changes no label: the merge request is merged, and what it
// carried when it merged is left as its history.
func fixStateLabel(s pipefix.State) string {
	switch s {
	case pipefix.StateVerifying, pipefix.StateNotFixed: // not-fixed here is "trying again"
		return labelFixVerifying
	case pipefix.StateAwaitingRun:
		return labelFixAwaitingRun
	case pipefix.StateFixed, pipefix.StateFixedNewFailure:
		return labelFixVerified
	case pipefix.StateFlaky, pipefix.StateUnverifiable, pipefix.StateRegressed, pipefix.StateTakenOver, pipefix.StateClosedUnmerged:
		return labelFixInconclusive
	case pipefix.StateDenied:
		return labelFixFailed
	}
	return ""
}

// setStatusBlock replaces the text between the status markers of a fix merge
// request's description. A description without the block (somebody removed
// it) gets it back at the top; everything else is left byte for byte.
func setStatusBlock(description, status string) string {
	block := fixStatusOpen + "\n" + strings.TrimSpace(status) + "\n" + fixStatusClose
	open := strings.Index(description, fixStatusOpen)
	if open < 0 {
		return block + "\n\n" + description
	}
	rest := description[open+len(fixStatusOpen):]
	end := strings.Index(rest, fixStatusClose)
	if end < 0 { // an opening marker with no closing one: keep what follows it
		return description[:open] + block + "\n\n" + strings.TrimLeft(rest, "\n")
	}
	return description[:open] + block + rest[end+len(fixStatusClose):]
}

// updateFixMR makes the fix merge request say `state`: its status block, its
// one state label, and gonk::fix-new-failure exactly when the state is
// fixed-new-failure. ONE UpdateMergeRequest request carries the description
// and both label lists, so no reader ever sees two state labels or none.
// Nothing is sent when the merge request already says it.
//
// It writes only to a merge request that is still gonk's: the bot authored
// it, both ends are this project, its source branch is this fix's branch. A
// merged merge request is not edited at all.
func updateFixMR(ctx context.Context, f pipefixVerifyForge, me glab.User, rec beadstore.Record, state pipefix.State, status string) error {
	pf := rec.PipelineFix
	branch, err := pipelinectx.FixBranch(rec.BeadAnchor)
	if err != nil {
		return err
	}
	mr, err := f.GetMergeRequest(ctx, rec.ProjectID, pf.MRIID)
	if err != nil {
		return fmt.Errorf("read merge request !%d: %w", pf.MRIID, err)
	}
	if mr == nil || mr.IID != pf.MRIID {
		return fmt.Errorf("merge request !%d: the reply is not that merge request", pf.MRIID)
	}
	if mr.Author.ID != me.ID || mr.SourceBranch != branch || mr.SourceProjectID != rec.ProjectID || mr.TargetProjectID != rec.ProjectID { // mr:ours
		return fmt.Errorf("merge request !%d is not this fix's (author %d, source %q); not editing it", mr.IID, mr.Author.ID, mr.SourceBranch)
	}
	if mr.State == pipefix.MRStateMerged || state == pipefix.StateMerged { // mr:merged
		return nil
	}
	var u glab.MRUpdate
	if want := fixStateLabel(state); want != "" {
		if !slices.Contains(mr.Labels, want) {
			u.AddLabels = append(u.AddLabels, want)
		}
		for _, l := range fixStateLabels {
			if l != want && slices.Contains(mr.Labels, l) { // mr:one-label
				u.RemoveLabels = append(u.RemoveLabels, l)
			}
		}
	}
	switch has := slices.Contains(mr.Labels, labelFixNewFailure); {
	case state == pipefix.StateFixedNewFailure && !has:
		u.AddLabels = append(u.AddLabels, labelFixNewFailure)
	case state != pipefix.StateFixedNewFailure && has:
		u.RemoveLabels = append(u.RemoveLabels, labelFixNewFailure)
	}
	if desc := setStatusBlock(mr.Description, status); desc != mr.Description {
		if m := closingPattern.FindString(desc); m != "" { // mr:closing
			return fmt.Errorf("the fix description would carry a closing pattern (%q); not writing it", m)
		}
		u.Description = &desc
	}
	if u.Description == nil && len(u.AddLabels) == 0 && len(u.RemoveLabels) == 0 {
		return nil
	}
	got, err := f.UpdateMergeRequest(ctx, rec.ProjectID, mr.IID, u)
	if err != nil {
		return fmt.Errorf("update merge request !%d: %w", mr.IID, err)
	}
	if got == nil || got.IID != mr.IID {
		return fmt.Errorf("update merge request !%d: the reply is not that merge request", mr.IID)
	}
	return nil
}

// ---- the run note's fix line -------------------------------------------------------

// writeRunFixLine puts "which fix merge request, in which state" on the
// DIAGNOSED RUN: on its record (PipelineRunFixMR, PipelineRunFixState) and,
// while the run's note is still `diagnosed`, on that note's status (plan 5.4
// step 4: "one line on the run note"). GitLab first, then the record, as run
// maintenance does. It returns the run record as it left it.
func writeRunFixLine(ctx context.Context, d sweepDeps, l *pipelineLander, run beadstore.Record, mrIID int64, state pipefix.State) (beadstore.Record, error) {
	if run.PipelineRunFixMR == mrIID && run.PipelineRunFixState == string(state) {
		return run, nil
	}
	readRevision := run.Revision
	run.PipelineRunFixMR, run.PipelineRunFixState = mrIID, string(state)
	t, err := pipelineRunOf(run)
	if err != nil {
		return run, err
	}
	ld, err := l.findRunLanding(ctx, run, t)
	if err != nil {
		return run, err
	}
	// Only a DIAGNOSED note has the line; a closed run's note is history.
	if ld.note != nil && ld.state == pipelinectx.RunStateDiagnosed && run.PipelineRunOpen {
		if _, err := l.writeRunNote(ctx, run, runNote{State: pipelinectx.RunStateDiagnosed, Status: diagnosedStatus(run),
			Body: preservedBody(ld.note.Body)}); err != nil {
			return run, err
		}
	}
	rev, err := d.Store.PutIf(ctx, run, readRevision)
	if errors.Is(err, beadstore.ErrStale) {
		// Run maintenance wrote the run meanwhile: the next look repeats this.
		return run, nil
	}
	if err != nil {
		return run, err
	}
	run.Revision = rev
	return run, nil
}

// fixLineText is the run note's fix line.
func fixLineText(mrIID int64, state string) string {
	var words string
	switch pipefix.State(state) {
	case pipefix.StateVerifying:
		words = "gonk is waiting for its pipeline"
	case pipefix.StateAwaitingRun:
		words = "a maintainer must review it and start its pipeline"
	case pipefix.StateFixed:
		words = "the diagnosed failure no longer occurs in its pipeline"
	case pipefix.StateFixedNewFailure:
		words = "the diagnosed failure no longer occurs in its pipeline; a different failure does"
	case pipefix.StateNotFixed:
		words = "an attempt did not fix the failure; gonk is trying again"
	case pipefix.StateFlaky, pipefix.StateUnverifiable, pipefix.StateRegressed:
		words = "gonk could not establish that it works (" + state + "); a person is needed"
	case pipefix.StateDenied:
		words = "it did not fix the failure and gonk has stopped; a person is needed"
	case pipefix.StateTakenOver:
		words = "somebody else pushed to it; gonk has stopped judging it"
	case pipefix.StateClosedUnmerged:
		words = "closed without being merged"
	case pipefix.StateMerged:
		words = "merged; gonk is watching the next pipeline"
	case pipefix.StateConfirmed:
		words = "merged, and the diagnosed failure is gone"
	case pipefix.StateRecurred:
		words = "merged, and the diagnosed failure occurred again; a person is needed"
	default:
		words = "state " + codeSpan(state)
	}
	return fmt.Sprintf("- Fix: merge request !%d -- %s\n", mrIID, words)
}
