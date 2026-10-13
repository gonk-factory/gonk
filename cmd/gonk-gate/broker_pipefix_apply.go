package main

// THE APPLY OF A PIPELINE FIX (gonk-2usw plan 5.3, 5.4, 8.1; part APPLY).
//
// A finished pipefix session proposed `edit` effects as text. Nothing it said
// is trusted, and it holds no credential: THIS file decides whether the
// batch may become a commit, and if so makes the commit, opens the merge
// request and writes the notes, all under the controller's bot PAT with
// controller-written text.
//
// PHASE A -- STATIC GATES, ALL BEFORE THE FIRST WRITE. The whole batch is
// refused on the first failure, by rule code. "Ours" means the outcome is
// UNKNOWN (infra-failed, never an escalation); "agent" means gate-failed.
//
//	gate  what                                                  fault   code
//	A1    the pack's effect shape; effects.ValidateFix (the     agent   FX-PACK, FX-SHAPE,
//	      shape alone accepts an EMPTY batch)                           SC-FORM
//	A2    no effect names a target                              agent   FX-TARGET
//	--    the trajectory floor (only when enforcement is on)    agent   FX-TRAJ
//	A3    the project is still opted in; the envelope folds     OURS
//	      again and agrees with the record; the lift is
//	      MINTED AGAIN by GrantLift (refoldPipefixEnvelope)
//	A9    the recorded context hashes to ContextSHA256; the     OURS
//	      diagnosis, every excerpt and every editable blob
//	      still hash to it (verifyPipefixContext). Its blobs
//	      are the bytes the session was shown.
//	A4    each edit's path: one the session was told it may     agent   SC-SHOWN, SC-FORM,
//	      change; clean, cited, in the envelope, not                    SC-PROT, SC-CLASS,
//	      protected (effects.ValidateEditPaths)                         SC-CITED, SC-BEAD
//	A5    the base is read through pkg/basefacts: a path that   agent   SC-FORM(symlink|...)
//	      is not a REGULAR FILE is refused; the blob read       OURS    (a read error, or a
//	      there must be the one A9 proved                               blob that differs)
//	A5/6  effects.PlanFix: exactly-once edits; size, secrets,   agent   ED-*, SC-SIZE,
//	      generated files                                               SC-SECRET, SC-CLASS
//	A6    SC-WEAKEN for Go (pkg/weaken); undecidable paths      agent   SC-WEAKEN
//	      are RECORDED, not refused
//	A7    the CI pre-check, when a ci file changed              agent   CI-PRE
//	A8    the agent's prose                                     agent   TX-*
//
// A9 runs before A4 on purpose: its blobs are A5's input, and a batch is
// never judged against a context that has moved.
//
// THE APPLY (5.4) -- deterministic, and IDEMPOTENT: every step first looks
// for what a previous, interrupted run left, and adopts it.
//
//  1. branch pipelinectx.FixBranch(fix anchor), created at the pinned base
//     SHA (never a branch name);
//  2. ONE commit on it, by the bot, `update` actions only, with a
//     controller-written message;
//  3. the definition check AFTER the commit and BEFORE the merge request
//     (CI-DEF): a violation deletes the branch and writes nothing else;
//  4. ONE merge request, found again by its source branch; title,
//     description and labels are the controller's. It says `Related to`,
//     never a closing keyword. NO assignee, NO reviewer, NO approval: this
//     file never sets one and pkg/glab's MR update has no field for one;
//  5. the fix note on the diagnosis landing.
//
// It stops at "merge request open". Judging the merge request's pipeline is
// part VERIFY's; so is reporting the attempt's outcome to meter, which is why
// an accepted fix reports NONE here (plan 7.3: the attempt is judged in two
// phases).
//
// WHAT THE RECORD SAYS AFTERWARDS (PipelineFix.State, set explicitly):
//
//	verifying        merge request open; its pipeline runs on push
//	awaiting-run     merge request open, commit made with [skip ci]: a
//	                 person must start the pipeline (a ci file changed and
//	                 pipeline_fix.ci_exec_changes is not `auto`)
//	needs-scope      the session's verdict, or an execution-changing CI
//	                 edit under ci_exec_changes: deny
//	taken-over       the fix branch's head is not a commit gonk made
//	closed-unmerged  the fix merge request was closed
//	merged           the fix merge request was merged
//	""               a refused batch: the next attempt is dispatched from
//	                 this record, and PipelineFix.Refusal holds the code

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/basefacts"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// The labels of a fix merge request (plan 8.1). Controller-written and
// reserved (broker_label.go): no agent can propose one.
const (
	labelFixVerifying    = "gonk::fix-verifying"
	labelFixAwaitingRun  = "gonk::fix-awaiting-run"
	labelCIExecChanged   = "gonk::ci-exec-changed"
	labelFixVerified     = "gonk::fix-verified"     // part VERIFY writes it
	labelFixNewFailure   = "gonk::fix-new-failure"  // part VERIFY writes it
	labelFixInconclusive = "gonk::fix-inconclusive" // part VERIFY writes it
	labelFixFailed       = "gonk::fix-failed"       // part VERIFY writes it
)

// fixMRLabels is every label a fix merge request can carry besides
// gonk::pipeline-fix: the closed set broker_label.go reserves.
var fixMRLabels = []string{labelFixVerifying, labelFixAwaitingRun, labelCIExecChanged,
	labelFixVerified, labelFixNewFailure, labelFixInconclusive, labelFixFailed}

// The fix merge request's status block: the first lines of its description,
// between these two markers, which a later part edits in place.
const (
	fixStatusOpen  = "<!-- gonk:pipeline-fix-status -->"
	fixStatusClose = "<!-- /gonk:pipeline-fix-status -->"
)

// pipefixApplyForge is what the apply reads and writes on GitLab. Satisfied by
// *glab.Client. THE WRITES ARE EXACTLY: CreateBranch, DeleteBranch,
// CreateCommit, CreateMergeRequest, UpdateMRLabels, and the note and label
// writes of pipelineForge. There is no method here that assigns, requests a
// review, approves or merges.
type pipefixApplyForge interface {
	pipelineForge
	ListTree(ctx context.Context, projectID int64, ref, dir string) ([]glab.TreeEntry, error)
	ResolveRef(ctx context.Context, projectID int64, ref string) (string, error)
	GetCommit(ctx context.Context, projectID int64, sha string) (*glab.Commit, error)
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
	CreateBranch(ctx context.Context, projectID int64, branch, ref string) (*glab.Branch, error)
	DeleteBranch(ctx context.Context, projectID int64, branch string) error
	CreateCommit(ctx context.Context, projectID int64, opts glab.CommitOptions) (*glab.Commit, error)
	CreateMergeRequest(ctx context.Context, projectID int64, opts glab.MROptions) (*glab.MergeRequest, error)
}

var _ pipefixApplyForge = (*glab.Client)(nil)

// fixOpenStates are the fix states in which a merge request is open and
// nothing has judged it yet: the record stays `running`, no outcome is
// reported, and part VERIFY's judge takes it from there (sweepRunningFix).
var fixOpenStates = map[string]bool{
	string(pipefix.StateAwaitingRun): true,
	string(pipefix.StateVerifying):   true,
}

// fixConfig reads the project's effective configuration for the apply:
// d.FixConfig when set (tests), else gonk-meter.
func (d sweepDeps) fixConfig(ctx context.Context, project string) (*meterapi.ProjectResponse, error) {
	if d.FixConfig != nil {
		return d.FixConfig(ctx, project)
	}
	if d.Meter == nil {
		return nil, errors.New("no meter client configured")
	}
	return d.Meter.Project(ctx, project)
}

// applyPipefixBatch is phase A and the apply for one parsed batch of a
// finished pipefix session. Same return contract as applyBrokerBatch:
//
//	applied=true            something terminal-for-this-attempt is on the
//	                        record: a merge request is open (fixOpenStates),
//	                        or needs-scope landed, or the fix was found taken
//	                        over / closed / merged. recp.PipelineFix says which.
//	violation != ""         the batch was refused by a gate (the agent's
//	                        fault). NOTHING WAS WRITTEN TO GITLAB -- except
//	                        by CI-DEF, which by plan 5.4 step 2 runs after
//	                        the commit and removes the branch it made.
//	                        recp.PipelineFix.Refusal holds the rule code.
//	err != nil              ours: unknown. The next sweep runs this again and
//	                        converges on whatever was already written.
//
// Nothing here writes the store: the caller's record write persists *recp.
func applyPipefixBatch(ctx context.Context, d sweepDeps, recp *beadstore.Record, batch effects.Batch) (applied bool, violation string, err error) {
	rec := *recp
	pf := rec.PipelineFix
	refuse := func(gate string, r *effects.Refusal) (bool, string, error) {
		d.Log.Warn("sweep: pipeline fix refused, wrote nothing", "bead", rec.BeadAnchor, "gate", gate,
			"rule", r.Code(), "detail", r.Detail)
		recp.PipelineFix.Refusal = r.Code()
		// The next attempt's word about it (retry_feedback.go): built from
		// the rule's codes, never from r.Detail, which quotes the batch.
		recordRetryFeedback(recp, fixRefusalFeedback(r, batch))
		return false, "fix gate " + gate + ": " + r.Error(), nil
	}
	unknown := func(gate string, e error) (bool, string, error) {
		d.Log.Warn("sweep: pipeline fix could not be checked or applied; retrying, not escalating",
			"bead", rec.BeadAnchor, "gate", gate, "err", e)
		return false, "", fmt.Errorf("fix %s: %w", gate, e)
	}
	if !pf.Recorded() || rec.Trigger != "pipeline-fix" {
		return unknown("A0", errors.New("the record is not a pipeline-fix record"))
	}
	forge, ok := d.Pipelines.(pipefixApplyForge)
	if d.Pipelines == nil || !ok {
		return unknown("A0", errors.New("no pipeline forge that can apply a fix is configured"))
	}

	// ---- A1: shape. The pack's shape AND the fix rules: the shape alone
	// accepts an empty batch (every minimum is 0).
	shape, serr := effects.LoadShape(d.PackDir, pipefixAgent)
	if serr != nil {
		return false, "", serr // OUR misconfiguration: never apply unvalidated
	}
	if verr := effects.Validate(batch, shape); verr != nil { // gate:A1-shape
		if r := effects.AsRefusal(verr); r != nil {
			return refuse("A1", r)
		}
		applied, violation, err = refuse("A1", fixRefusal(ruleFixPackShape, "", "", "%v", verr))
		recp.RetryFeedback.Allowed = effects.ExplainShape(shape).Allowed // the counts the pack's shape takes
		return applied, violation, err
	}
	if verr := effects.ValidateFix(batch); verr != nil { // gate:A1-fix
		if r := effects.AsRefusal(verr); r != nil {
			return refuse("A1", r)
		}
		return refuse("A1", fixRefusal(effects.RuleFixShape, "", "", "%v", verr))
	}
	// ---- A2: no effect may name a target. Where a fix lands is never the
	// agent's choice.
	if terr := effects.ValidateTargets(batch, effects.Targets{}); terr != nil { // gate:A2
		return refuse("A2", fixRefusal(ruleFixTarget, "", "", "%v", terr))
	}
	if v := checkTrajectory(ctx, d, rec, pipefixAgent, batch); v != "" {
		return refuse("A1", fixRefusal(ruleFixTraject, "", "", "%s", v))
	}

	// ---- A3: our configuration. Still opted in; the envelope folded again;
	// the lift minted again.
	cfg, cerr := d.fixConfig(ctx, rec.Project)
	if cerr != nil {
		return unknown("A3", fmt.Errorf("read the project's effective configuration: %w", cerr))
	}
	env, eerr := refoldPipefixEnvelope(ctx, forge, cfg, d.PackDir, rec, d.Log) // gate:A3
	if eerr != nil {
		return unknown("A3", eerr)
	}
	maxEdits, merr := pipefixMaxEdits(d.PackDir)
	if merr != nil {
		return unknown("A3", merr)
	}

	// ---- A9: context integrity. The record's own hash first, then every
	// hash in it against GitLab as it is now.
	run, found, gerr := d.Store.Get(ctx, pf.RunAnchor)
	if gerr != nil || !found {
		return unknown("A9", fmt.Errorf("read the diagnosed run's record %q (found=%v): %w", pf.RunAnchor, found, gerr))
	}
	if !pf.Context.Recorded() || pipefixContextHash(pf.Context) != pf.ContextSHA256 { // gate:A9-record
		return unknown("A9", fmt.Errorf("%w: the recorded fix context does not hash to the recorded ContextSHA256", errPipefixContextChanged))
	}
	if pf.Context.BaseSHA != pf.BaseSHA || !glab.IsCommitSHA(pf.BaseSHA) { // gate:A9-base
		return unknown("A9", fmt.Errorf("%w: the session was shown commit %q, the fix is written against %q",
			errPipefixContextChanged, pf.Context.BaseSHA, pf.BaseSHA))
	}
	shown, verr := verifyPipefixContext(ctx, forge, rec.ProjectID, pf.Context, run.PipelineDiagnosis, env, maxEdits) // gate:A9
	if verr != nil {
		return unknown("A9", verr)
	}

	// ---- needs-scope: no edit, nothing to read. PlanFix validates the
	// request; A8 gates its text; then the note lands.
	if batch.EffectiveVerdict() == effects.VerdictNeedsScope {
		plan, perr := effects.PlanFix(batch, env, nil, nil)
		if perr != nil {
			if r := effects.AsRefusal(perr); r != nil {
				return refuse("A1", r)
			}
			return unknown("A1", perr)
		}
		if r := fixTextGates(plan); r != nil { // gate:A8-scope
			return refuse("A8", r)
		}
		return landFixNeedsScope(ctx, d, forge, recp, run, scopeRequestText(run, rec, plan.Request))
	}

	// ---- A4: paths. First against what the session was SHOWN, then the
	// envelope's own path predicates. No blob has been read for the batch yet.
	if r := shownRefusal(batch, pf.Context); r != nil { // gate:A4-shown
		return refuse("A4", r)
	}
	if perr := effects.ValidateEditPaths(batch, env); perr != nil { // gate:A4-paths
		if r := effects.AsRefusal(perr); r != nil {
			return refuse("A4", r)
		}
		return unknown("A4", perr)
	}

	// ---- A5: the base, through the mode-aware reader. A symlink, a
	// submodule or a directory at an edit path is refused; a read error is
	// ours; and the regular file read there must be the blob A9 proved.
	paths := effects.EditPaths(batch)
	facts, berr := basefacts.Read(ctx, forge, rec.ProjectID, pf.BaseSHA, paths, nil) // gate:A5-read
	if berr != nil {
		return unknown("A5", berr)
	}
	if r := modeRefusal(facts.Refusals); r != nil { // gate:A5-mode
		return refuse("A5", r)
	}
	base := effects.BaseBlobs{}
	for _, p := range paths {
		blob, regular := facts.Blobs[p]
		if !regular {
			continue // absent at the base: PlanFix answers ED-MISSING
		}
		if !bytes.Equal(blob, shown[p]) { // gate:A5-same
			return unknown("A5", fmt.Errorf("%w: %q at %s is not the content the session was shown", errPipefixContextChanged, p, pf.BaseSHA))
		}
		base[p] = blob
	}

	// ---- A5/A6: the gate of pkg/effects, with the final inputs.
	plan, perr := effects.PlanFix(batch, env, base, d.FixSecrets) // gate:A6-plan
	if perr != nil {
		if r := effects.AsRefusal(perr); r != nil {
			return refuse("A6", r)
		}
		return unknown("A6", perr)
	}
	// ---- A6: SC-WEAKEN.
	weak, wr := weakeningGate(plan, base, env.Scope.Classifier(env.Base, d.Log)) // gate:A6-weaken
	if wr != nil {
		return refuse("A6", wr)
	}
	// ---- A7: the CI pre-check.
	if r := ciPreCheck(plan); r != nil { // gate:A7
		return refuse("A7", r)
	}
	// ---- A8: the fix_meta reason.
	if r := fixTextGates(plan); r != nil { // gate:A8
		return refuse("A8", r)
	}

	// ---- every gate passed. From here on, GitLab is written. ----
	a := &fixApply{d: d, f: forge, rec: rec, run: run, plan: plan, base: base, weak: weak,
		branch: "", now: d.Now()}
	return a.apply(ctx, recp, refuse, unknown)
}

// ---- the apply -------------------------------------------------------------------

// fixApply is one accepted plan being applied.
type fixApply struct {
	d    sweepDeps
	f    pipefixApplyForge
	rec  beadstore.Record // the fix record
	run  beadstore.Record // the diagnosed run's record
	plan effects.FixPlan
	base effects.BaseBlobs
	weak weakeningFact
	now  time.Time

	branch string
	skip   bool // the commit carries [skip ci]
}

func (a *fixApply) apply(ctx context.Context, recp *beadstore.Record,
	refuse func(string, *effects.Refusal) (bool, string, error),
	unknown func(string, error) (bool, string, error)) (bool, string, error) {
	pf, pid := a.rec.PipelineFix, a.rec.ProjectID
	branch, berr := pipelinectx.FixBranch(a.rec.BeadAnchor)
	if berr != nil || !pipelinectx.IsFixBranch(branch) {
		return unknown("apply", fmt.Errorf("no fix branch for %q: %v", a.rec.BeadAnchor, berr))
	}
	a.branch = branch
	lander, err := newPipelineLander(ctx, a.f, a.d.Log)
	if err != nil {
		return unknown("apply", err)
	}
	t, err := pipelineRunOf(a.run)
	if err != nil {
		return unknown("apply", err)
	}
	ld, err := lander.findRunLanding(ctx, a.run, t)
	if err != nil {
		return unknown("apply", err)
	}
	if ld.note == nil || (t.Kind == pipelinectx.TargetKindRef && ld.issue == nil) {
		// The fix note and the `Related to` link both need the landing; a fix
		// with neither is not opened.
		return unknown("apply", errFixNoteNoLanding)
	}

	// THE MERGE REQUEST, IF ONE EXISTS -- read BEFORE any write, because what
	// it says decides whether anything may be written at all.
	mr, mrState, err := a.findMR(ctx, lander.me)
	if err != nil {
		return unknown("apply", err)
	}
	switch mrState {
	case "foreign":
		return a.terminal(recp, pipefix.StateTakenOver, "a merge request on the fix branch is not gonk's")
	case "closed":
		return a.terminal(recp, pipefix.StateClosedUnmerged, "the fix merge request was closed")
	case "merged":
		return a.terminal(recp, pipefix.StateMerged, "the fix merge request was merged")
	}

	// A CI change is committed with [skip ci] unless the project accepted
	// automatic runs: whether it changes what a job EXECUTES is only knowable
	// from the merged definition of the commit, which does not exist yet, and
	// an execution-changing edit must not run before a person saw it (5.5).
	a.skip = changesCI(a.plan) && pf.CIExecChanges != "auto" // apply:skip-ci

	// Before the first write: neither the title nor the commit message can
	// close an issue (gonk-vu0n). Ours if it fails; nothing is written.
	if err := a.titleInvariant(); err != nil { // apply:title-invariant
		return unknown("apply", err)
	}

	// ---- 1 + 2: the branch at the pinned base, and ONE commit on it.
	commit, takenOver, err := a.ensureCommit(ctx)
	if err != nil {
		return unknown("apply", err)
	}
	if takenOver != "" {
		return a.terminal(recp, pipefix.StateTakenOver, takenOver)
	}

	// ---- 3: the definition check, AFTER the commit and BEFORE the merge
	// request exists.
	delta, err := a.definitionDelta(ctx, commit)
	if err != nil {
		return unknown("apply", err)
	}
	switch {
	case delta.Kind == cidef.KindViolation: // apply:cidef-violation
		a.dropBranch(ctx, mr)
		return refuse("5.4", fixRefusal(ruleCIDef, "violation", "", "the change alters how the pipeline decides what runs or what may fail: %s", deltaText(delta)))
	case delta.Kind == cidef.KindUnresolvable:
		return unknown("apply", fmt.Errorf("the CI definition cannot be resolved at the base or at the commit: %s", deltaText(delta)))
	case delta.Kind == cidef.KindExecChanging && pf.CIExecChanges == "deny": // apply:cidef-deny
		a.dropBranch(ctx, mr)
		return landFixNeedsScope(ctx, a.d, a.f, recp, a.run, execDeniedText(a.run, a.rec))
	}

	// ---- 4: ONE merge request.
	state := pipefix.StateVerifying
	if a.skip {
		state = pipefix.StateAwaitingRun
	}
	labels := []string{verdictLabel(effects.VerdictFix), labelFixVerifying}
	if a.skip {
		labels = []string{verdictLabel(effects.VerdictFix), labelFixAwaitingRun}
	}
	if delta.Kind == cidef.KindExecChanging {
		labels = append(labels, labelCIExecChanged)
	}
	facts := fixFacts{commit: commit, state: state, delta: delta.Kind, weak: a.weak, landing: ld}
	if mr == nil {
		desc, derr := a.description(facts)
		if derr != nil {
			return unknown("apply", derr)
		}
		created, cerr := a.f.CreateMergeRequest(ctx, pid, glab.MROptions{ // apply:mr
			SourceBranch: a.branch, TargetBranch: pf.TargetBranch,
			Title: a.title(), Description: desc, Labels: labels,
			// NO AssigneeID. Not zero by accident: gonk assigns no one
			// (066 item d), and TestFixMRHasNoAssigneeReviewerOrApproval
			// reads the request the fake received.
		})
		if cerr != nil {
			return unknown("apply", fmt.Errorf("open the fix merge request: %w", cerr))
		}
		mr = created
	} else if missing := missingLabels(mr.Labels, labels); len(missing) > 0 {
		if _, lerr := a.f.UpdateMRLabels(ctx, pid, mr.IID, missing, nil); lerr != nil {
			a.d.Log.Warn("sweep: pipeline fix: could not complete the merge request's labels", "bead", a.rec.BeadAnchor,
				"mr", mr.IID, "err", lerr)
		}
	}
	if mr.IID < 1 {
		return unknown("apply", errors.New("the fix merge request carries no iid"))
	}

	// THE MERGE REQUEST IS OPEN: record it before the note, which cannot
	// un-open it.
	fix := &recp.PipelineFix
	fix.State = string(state)
	fix.AppliedAttempt = a.rec.Attempt
	fix.FixCommitSHA = commit
	fix.MRIID = mr.IID
	fix.ChangedFiles = changedFiles(a.plan)
	fix.Weakening, fix.WeakeningUndecided = a.weak.state(), a.weak.undecided
	fix.CIDelta = string(delta.Kind)
	fix.VerifyDeadline = a.now.Add(time.Duration(pf.VerifyTimeoutSeconds) * time.Second)
	fix.Refusal = ""

	// ---- 5: the fix note on the diagnosis landing. Best effort from here:
	// the artifact exists, and a note that did not land is written by the
	// next pass that looks at this fix.
	note := fixNote{State: string(state), Text: fixOpenedText(a.rec, mr.IID, commit, state), Apply: true}
	if _, _, nerr := lander.writeFixNote(ctx, a.run, a.rec.BeadAnchor, note); nerr != nil {
		a.d.Log.Warn("sweep: pipeline fix: the fix note could not be written (the merge request is open)",
			"bead", a.rec.BeadAnchor, "mr", mr.IID, "err", nerr)
	}
	a.d.Log.Info("sweep: pipeline fix applied", "bead", a.rec.BeadAnchor, "run", a.run.BeadAnchor, "mr", mr.IID,
		"branch", a.branch, "commit", commit, "state", string(state), "files", len(a.plan.Files),
		"lines_added", a.plan.Facts.LinesAdded, "lines_removed", a.plan.Facts.LinesRemoved,
		"weakening", a.weak.state(), "ci_delta", string(delta.Kind), "skip_ci", a.skip)
	return true, "", nil
}

// terminal records a fix that ended without a new write: the branch or the
// merge request is no longer gonk's to touch.
func (a *fixApply) terminal(recp *beadstore.Record, st pipefix.State, why string) (bool, string, error) {
	a.d.Log.Warn("sweep: pipeline fix not applied: "+why+"; nothing written", "bead", a.rec.BeadAnchor,
		"branch", a.branch, "state", string(st))
	recp.PipelineFix.State = string(st)
	recp.PipelineFix.Refusal = ""
	return true, "", nil
}

// findMR returns the fix's merge request and what it is:
//
//	nil, ""          none exists on the fix branch
//	mr,  "open"      gonk's own, open: adopted
//	_,   "closed"    gonk's own, closed without a merge (and none open)
//	_,   "merged"    gonk's own, merged (and none open)
//	_,   "foreign"   an OPEN merge request on the branch that the bot did not
//	                 author, or whose ends are not this project
func (a *fixApply) findMR(ctx context.Context, me glab.User) (*glab.MergeRequest, string, error) {
	pid := a.rec.ProjectID
	mrs, err := a.f.ListMergeRequests(ctx, pid, glab.MRListOptions{SourceBranch: a.branch, State: "all"})
	if err != nil {
		return nil, "", fmt.Errorf("list the merge requests of %s: %w", a.branch, err)
	}
	var open, closed, merged *glab.MergeRequest
	for i := range mrs {
		m := &mrs[i]
		if m.SourceBranch != a.branch { // the filter is re-checked, not trusted
			continue
		}
		ours := m.Author.ID == me.ID && m.SourceProjectID == pid && m.TargetProjectID == pid
		switch m.State {
		case "opened", "locked":
			if !ours { // apply:mr-foreign
				return nil, "foreign", nil
			}
			open = m
		case "merged":
			if ours {
				merged = m
			}
		case "closed":
			if ours {
				closed = m
			}
		}
	}
	switch {
	case open != nil:
		return open, "open", nil
	case merged != nil:
		return merged, "merged", nil
	case closed != nil: // apply:mr-closed
		return closed, "closed", nil
	}
	return nil, "", nil
}

// commitMessage is the controller's commit message. The job names are the
// project's own text, so they are reduced to characters that cannot form a
// reference or a closing keyword's target (refSafe): a job named
// "x closes #1" must not close issue 1 when the fix merges.
func (a *fixApply) commitMessage() string {
	msg := a.title() + "\n\n" + fmt.Sprintf("gonk-fix: %s attempt=%d", a.rec.BeadAnchor, max(a.rec.Attempt, 1))
	if a.skip {
		msg += "\n\n[skip ci] A maintainer must review this change and start its pipeline."
	}
	return msg
}

// title is the commit's first line and the merge request's title:
// "gonk: fix job <job> on <ref>".
func (a *fixApply) title() string {
	pf := a.rec.PipelineFix
	job := "of the diagnosed failure"
	if len(pf.Targets) > 0 {
		job = refSafe(pf.Targets[0].Name)
		if n := len(pf.Targets) - 1; n > 0 {
			job += fmt.Sprintf(" (+%d more)", n)
		}
	}
	// "fix job <name>", not "fix <name>": GitLab reads a closing verb directly
	// followed by a reference or a tracker key (ABC-12) as "close it on
	// merge", and a job may be NAMED like a tracker key.
	//
	// And a job may be named like a whole closing STATEMENT (gonk-vu0n):
	// "fixes ABC-12", "closes https://...". refSafe leaves the letters, the
	// hyphen and the colon such a name is spelled with, so the finished line
	// goes through defuseClosing; apply checks the result (titleInvariant).
	return defuseClosing(fmt.Sprintf("gonk: fix job %s on %s", job, refSafe(pf.TargetBranch)))
}

// refSafe keeps letters, digits and a few separators of project-chosen text
// that is rendered OUTSIDE a code span (a title, a commit message), and
// replaces everything else -- `#`, `!`, `@`, `[`, newlines -- so the text can
// never be read as a `#`/`!` reference, a mention or a second line. It keeps
// `-`, `:` and `/` (job names are full of them: `build:linux`,
// `unit-tests-3.12`), so by itself it does NOT stop a tracker key or a
// closing statement spelled in letters: title() adds defuseClosing.
func refSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '/', r == ':', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	// No URL: `://` is what GitLab links and cross-references from a title,
	// with or without a closing verb before it (inert's own rule).
	if out := strings.TrimSpace(strings.ReplaceAll(b.String(), "://", ": //")); out != "" { // refsafe:url
		return out
	}
	return "_"
}

// ensureCommit makes sure the fix branch exists at the pinned base and holds
// this attempt's ONE commit, and returns that commit's SHA.
//
// The branch's head decides what happens:
//
//	no branch            create it at the parent, then commit
//	head == parent       commit (the branch exists; a previous run stopped
//	                     between the two writes)
//	head is OUR commit   adopt it, write nothing (a previous run stopped
//	                     after the commit)
//	anything else        taken over: write nothing
//
// "parent" is the pinned base SHA for the first applied attempt, and the
// previous attempt's recorded commit for a later one (no force-push, plan
// 5.4). OUR commit is recognised by ALL of: the exact message this attempt
// would write, exactly that one parent, and every file this attempt changes
// holding exactly this attempt's content at that commit.
func (a *fixApply) ensureCommit(ctx context.Context) (sha, takenOver string, err error) {
	pf, pid := a.rec.PipelineFix, a.rec.ProjectID
	parent := pf.BaseSHA
	if pf.FixCommitSHA != "" { // a later attempt builds on the earlier commit
		parent = pf.FixCommitSHA
	}
	head, rerr := a.f.ResolveRef(ctx, pid, a.branch)
	switch {
	case rerr != nil && glab.IsNotFound(rerr):
		if parent != pf.BaseSHA {
			return "", "the fix branch is gone although an earlier attempt committed to it", nil
		}
		// FROM THE PINNED SHA, never from a branch name: the target may have
		// moved since the fix was decided.
		if _, cerr := a.f.CreateBranch(ctx, pid, a.branch, pf.BaseSHA); cerr != nil { // apply:branch
			return "", "", fmt.Errorf("create branch %s at %s: %w", a.branch, pf.BaseSHA, cerr)
		}
		head = pf.BaseSHA
	case rerr != nil:
		return "", "", fmt.Errorf("resolve branch %s: %w", a.branch, rerr)
	}
	if head != parent { // apply:adopt
		ours, oerr := a.isOurCommit(ctx, head, parent)
		if oerr != nil {
			return "", "", oerr
		}
		if !ours {
			return "", fmt.Sprintf("the fix branch's head %s is not a commit gonk made for this attempt", shortSHA(head)), nil
		}
		a.d.Log.Info("sweep: pipeline fix: the commit already exists; adopting it", "bead", a.rec.BeadAnchor,
			"branch", a.branch, "commit", head)
		return head, "", nil
	}
	actions := make([]glab.CommitAction, 0, len(a.plan.Files))
	changed := map[string]bool{}
	for _, f := range a.plan.Files {
		changed[f.Path] = true
		// `update` only: a fix never creates, deletes, moves or re-modes a file.
		actions = append(actions, glab.CommitAction{Action: "update", FilePath: f.Path, Content: string(f.Content)})
	}
	// A later attempt leaves the branch at base + THIS attempt's change: a
	// file only an earlier attempt changed goes back to its base content.
	var revert []string
	for _, f := range pf.ChangedFiles {
		if !changed[f.Path] {
			revert = append(revert, f.Path)
		}
	}
	if len(revert) > 0 {
		facts, berr := basefacts.Read(ctx, a.f, pid, pf.BaseSHA, revert, nil)
		if berr != nil {
			return "", "", berr
		}
		for _, p := range revert {
			blob, ok := facts.Blobs[p]
			if !ok {
				return "", "", fmt.Errorf("%q, changed by an earlier attempt, is not a regular file at the base", p)
			}
			actions = append(actions, glab.CommitAction{Action: "update", FilePath: p, Content: string(blob)})
		}
	}
	c, cerr := a.f.CreateCommit(ctx, pid, glab.CommitOptions{ // apply:commit
		Branch: a.branch, CommitMessage: a.commitMessage(), Actions: actions,
	})
	if cerr != nil {
		return "", "", fmt.Errorf("commit to %s: %w", a.branch, cerr)
	}
	if c == nil || !glab.IsCommitSHA(c.ID) {
		return "", "", errors.New("the commit reply carries no commit SHA")
	}
	return c.ID, "", nil
}

// isOurCommit reports whether head is the commit this attempt would make on
// top of parent.
func (a *fixApply) isOurCommit(ctx context.Context, head, parent string) (bool, error) {
	pid := a.rec.ProjectID
	c, err := a.f.GetCommit(ctx, pid, head)
	if err != nil {
		return false, fmt.Errorf("read the fix branch's head %s: %w", head, err)
	}
	if strings.TrimSpace(c.Message) != strings.TrimSpace(a.commitMessage()) { // adopt:message
		return false, nil
	}
	if len(c.ParentIDs) != 1 || c.ParentIDs[0] != parent { // adopt:parent
		return false, nil
	}
	for _, f := range a.plan.Files {
		got, rerr := a.f.GetRawFile(ctx, pid, f.Path, head, a.rec.PipelineFix.MaxFileBytes+int64(len(f.Content)))
		if rerr != nil {
			if glab.IsNotFound(rerr) {
				return false, nil
			}
			return false, fmt.Errorf("read %q at %s: %w", f.Path, head, rerr)
		}
		if !bytes.Equal(got, f.Content) { // adopt:content
			return false, nil
		}
	}
	return true, nil
}

// definitionDelta is plan 5.4 step 2 / 6.2 for the commit just made.
//
// When the change touches no ci-class path the shortcut applies (SC-CIDEF):
// the pipeline definition file and EVERY file it includes -- through the
// includes of included files, scope.ResolveCILocalIncludes -- must be
// byte-identical at the base and at the commit, and then the delta is `none`
// with no resolver involved. For a gonk-authored commit of `update` actions
// on other paths they cannot differ; if they do, that is reported as
// unresolvable (ours). Identical files include identical files, so comparing
// the set resolved at the base covers the commit's set too.
//
// The shortcut does NOT apply when any include is a wildcard: the files
// behind a pattern are not enumerated. Then, and when the change does touch
// a ci-class path, the MERGED definition at both commits is compared by
// pkg/cidef, with the fix's target set as the diagnosed jobs.
func (a *fixApply) definitionDelta(ctx context.Context, commit string) (cidef.Delta, error) {
	pf, pid := a.rec.PipelineFix, a.rec.ProjectID
	if !changesCI(a.plan) { // delta:shortcut
		inc, rerr, ierr := pipefixCIIncludes(ctx, a.f, pid, pf.BaseSHA, 1<<20)
		switch {
		case ierr != nil:
			return cidef.Delta{}, fmt.Errorf("read the pipeline definition's includes: %w", ierr)
		case rerr != nil:
			return cidef.Delta{}, fmt.Errorf("read %s at %s: %w", pipefixCIFile, pf.BaseSHA, rerr)
		}
		// The shortcut holds only when EVERY file of the definition can be
		// named: a wildcard include is not expanded, so the files behind it
		// (and what they include) could differ unseen. Then the merged
		// definitions are compared below, as for a ci change (gonk-9q0y).
		if inc.Complete { // delta:complete
			ciPaths := append([]string{pipefixCIFile}, inc.Paths...)
			for _, p := range ciPaths {
				b, berr := a.f.GetRawFile(ctx, pid, p, pf.BaseSHA, 1<<20)
				h, herr := a.f.GetRawFile(ctx, pid, p, commit, 1<<20)
				if (berr != nil && !glab.IsNotFound(berr)) || (herr != nil && !glab.IsNotFound(herr)) {
					return cidef.Delta{}, fmt.Errorf("compare %q at the base and at the commit: %w", p, errors.Join(berr, herr))
				}
				if (berr == nil) != (herr == nil) || !bytes.Equal(b, h) { // delta:same
					return cidef.Delta{Kind: cidef.KindUnresolvable}, nil
				}
			}
			return cidef.Delta{Kind: cidef.KindNone}, nil
		}
	}
	baseLint, err := a.f.CILintStatic(ctx, pid, pf.BaseSHA)
	if err != nil {
		return cidef.Delta{}, fmt.Errorf("lint the CI definition at the base %s: %w", pf.BaseSHA, err)
	}
	headLint, err := a.f.CILintStatic(ctx, pid, commit)
	if err != nil {
		return cidef.Delta{}, fmt.Errorf("lint the CI definition at the commit %s: %w", commit, err)
	}
	var diagnosed []string
	for _, t := range pf.Targets {
		diagnosed = append(diagnosed, t.Name)
	}
	if !changesCI(a.plan) { // delta:wildcard
		// Here only because an include is a wildcard. The change touches no
		// ci path, so ANY difference between the two merged definitions is
		// not the agent's doing and not something a gonk commit can cause:
		// unresolvable (ours), as in the shortcut -- never a refusal the
		// agent is told about, and never a delta other than `none`.
		if !headLint.Valid || cidef.Diff([]byte(baseLint.MergedYAML), []byte(headLint.MergedYAML), diagnosed).Kind != cidef.KindNone {
			return cidef.Delta{Kind: cidef.KindUnresolvable}, nil
		}
		return cidef.Delta{Kind: cidef.KindNone}, nil
	}
	if !headLint.Valid { // delta:invalid
		// A definition GitLab rejects runs nothing: the diagnosed job cannot
		// "still run and pass".
		return cidef.Delta{Kind: cidef.KindViolation}, nil
	}
	return cidef.Diff([]byte(baseLint.MergedYAML), []byte(headLint.MergedYAML), diagnosed), nil
}

// deltaText is a short controller-written description of a delta, for a
// refusal's detail. Job names go through oneLine.
func deltaText(d cidef.Delta) string {
	return oneLine(fmt.Sprintf("definition delta %s", d.Kind), 200)
}

// dropBranch deletes the fix branch after a post-commit refusal (plan 5.4
// step 2) -- but ONLY when no merge request stands on it: deleting the source
// branch of an open merge request would close it, and that merge request is
// an earlier attempt's, for a person to deal with.
func (a *fixApply) dropBranch(ctx context.Context, mr *glab.MergeRequest) {
	if mr != nil {
		a.d.Log.Warn("sweep: pipeline fix: the refused commit stays on the fix branch, which carries an open merge request",
			"bead", a.rec.BeadAnchor, "branch", a.branch, "mr", mr.IID)
		return
	}
	if err := a.f.DeleteBranch(ctx, a.rec.ProjectID, a.branch); err != nil && !glab.IsNotFound(err) {
		a.d.Log.Warn("sweep: pipeline fix: could not delete the fix branch after refusing its commit",
			"bead", a.rec.BeadAnchor, "branch", a.branch, "err", err)
	}
}

func missingLabels(have, want []string) []string {
	var out []string
	for _, l := range want {
		if !slices.Contains(have, l) {
			out = append(out, l)
		}
	}
	return out
}

func changedFiles(plan effects.FixPlan) []beadstore.PipelineFixFile {
	out := make([]beadstore.PipelineFixFile, 0, len(plan.Facts.Files))
	for _, f := range plan.Facts.Files {
		out = append(out, beadstore.PipelineFixFile{Path: f.Path, Class: string(f.Class), LinesAdded: f.Added, LinesRemoved: f.Removed})
	}
	return out
}

// ---- needs-scope ------------------------------------------------------------------

// landFixNeedsScope lands a needs-scope outcome: the fix note on the
// diagnosis landing (which MUST land -- it is this outcome's whole artifact),
// then `gonk::needs-human` on the landing. Success for the meter, terminal
// for the fix, for a person (plan 4.4, 7.3; owner decision 066 Q5).
func landFixNeedsScope(ctx context.Context, d sweepDeps, f pipefixApplyForge, recp *beadstore.Record, run beadstore.Record, text string) (bool, string, error) {
	lander, err := newPipelineLander(ctx, f, d.Log)
	if err != nil {
		return false, "", fmt.Errorf("fix needs-scope: %w", err)
	}
	note := fixNote{State: string(pipefix.StateNeedsScope), Text: text, Apply: true}
	changed, ld, err := lander.writeFixNote(ctx, run, recp.BeadAnchor, note)
	if err != nil {
		return false, "", fmt.Errorf("fix needs-scope: the note did not land: %w", err)
	}
	if changed {
		// The label of the needs-scope verdict (verdictLabels): a person is needed.
		lander.addLandingLabel(ctx, run, ld, verdictLabel(effects.VerdictNeedsScope))
	}
	recp.PipelineFix.State = string(pipefix.StateNeedsScope)
	recp.PipelineFix.Refusal = ""
	d.Log.Info("sweep: pipeline fix needs scope; said so on the landing", "bead", recp.BeadAnchor, "run", run.BeadAnchor)
	return true, "", nil
}
