package mergepolicy

import (
	"fmt"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Decide is epic plan 5.1: may the controller merge this merge request now?
//
// Every row M1..M11 is evaluated, in that order (which is the order of
// Rules(), and so of Decision.Failed), against facts the caller read in THIS
// pass. Merge is true only when every row held; otherwise
// Decision.Failed lists every fact that did not, and Decision.Rule is the
// first. A row whose facts were not read, or are older than MaxFactAge at
// `now`, fails as "<row>.unread" / "<row>.stale".
//
// M8 is evaluated under ModeAfterHumanApproval only. M9 has two halves: the
// veto (no `blocking` review of the judged commit) under EVERY mode, and the
// passing distinct-model review under ModeAfterAgenticReview only.
func Decide(f Facts, cfg Config, now time.Time) Decision {
	e := &eval{f: f, cfg: cfg, now: now}
	e.m1()
	e.m2()
	e.m3()
	class, classKnown := e.m5class()
	e.m4(class, classKnown)
	e.m5()
	e.m6()
	e.m7()
	e.m8()
	e.m9()
	e.m10()
	e.m11()

	d := Decision{Mode: cfg.Mode}
	if len(e.failed) > 0 {
		d.Failed = e.failed
		d.Rule, d.Text = e.failed[0].Rule, e.failed[0].Text
		return d
	}
	d.Merge = true
	d.Rule = RuleMerge
	d.Text = fmt.Sprintf("every merge fact holds: a %s-only %s merge request, mode %s, pinned to %s", class, f.Kind, cfg.Mode, f.JudgedSHA)
	d.SHA = f.JudgedSHA
	d.Class = class
	return d
}

// eval collects the failures of one Decide call.
type eval struct {
	f      Facts
	cfg    Config
	now    time.Time
	failed []Failure
}

func (e *eval) fail(code RuleCode, format string, args ...any) {
	e.failed = append(e.failed, Failure{Rule: code, Text: fmt.Sprintf(format, args...)})
}

// wasRead reports whether a fact group read at `at` is a fact at e.now.
func wasRead(at, now time.Time) (read, fresh bool) {
	if at.IsZero() {
		return false, false
	}
	return true, !at.After(now) && now.Sub(at) <= MaxFactAge
}

// read is the gate of every row: false, with the row's failure recorded, when
// the group was not read or is not fresh.
func (e *eval) read(at time.Time, unread, stale RuleCode, what string) bool {
	read, fresh := wasRead(at, e.now)
	if !read {
		e.failed = append(e.failed, Failure{Rule: unread, Unread: true, Text: what + " was not read in this pass"})
		return false
	}
	if !fresh {
		e.failed = append(e.failed, Failure{Rule: stale, Unread: true, Text: fmt.Sprintf(
			"%s was read at %s, which is not within %s before the decision at %s",
			what, at.UTC().Format(time.RFC3339Nano), MaxFactAge, e.now.UTC().Format(time.RFC3339Nano))})
		return false
	}
	return true
}

// fullSHA reports whether s is a full, lower-case, 40-hex commit SHA -- the
// only thing a merge call may be pinned to.
func fullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// m1: opted in, at the target head, with no veto and no disagreement.
func (e *eval) m1() {
	if !e.read(e.f.Config.ReadAt, M1Unread, M1Stale, "the auto-merge configuration") {
		return
	}
	switch e.cfg.Mode {
	case ModeOff, "":
		e.fail(M1ModeOff, "auto_merge.mode is off for this merge request kind")
	case ModeAfterHumanApproval, ModeAfterAgenticReview:
	default:
		e.fail(M1ModeUnknown, "auto_merge.mode %q is not a mode", e.cfg.Mode)
	}
	if e.f.Config.OperatorVeto {
		e.fail(M1OperatorVeto, "an operator layer forbids automatic merges")
	}
	if len(e.f.Config.LayerModes) == 0 {
		e.fail(M1LayersDisagree, "no configuration layer states auto_merge.mode")
	} else {
		for _, l := range e.f.Config.LayerModes {
			if l.Mode != e.cfg.Mode {
				e.fail(M1LayersDisagree, "the %s layer says auto_merge.mode %q, the effective mode is %q", l.Layer, l.Mode, e.cfg.Mode)
				break
			}
		}
	}
	// Held to the target head whenever that is a SHA; when it is not, M7
	// refuses. An unknown AtSHA equals no head.
	if head := e.f.Target.HeadSHA; fullSHA(head) && e.f.Config.AtSHA != head {
		e.fail(M1ConfigNotAtTip, "the configuration was read at %q, the target branch head is %s", e.f.Config.AtSHA, head)
	}
}

// branchPrefix is the source-branch prefix of a workflow's merge requests.
func branchPrefix(k Kind) (string, bool) {
	switch k {
	case KindPipefix:
		return pipelinectx.FixBranchPrefix, true
	case KindChange:
		return ChangeBranchPrefix, true
	}
	return "", false
}

// m2: gonk authored it and nobody else touched it.
func (e *eval) m2() {
	if !e.read(e.f.MR.ReadAt, M2Unread, M2Stale, "the merge request") {
		return
	}
	mr := e.f.MR
	prefix, known := branchPrefix(e.f.Kind)
	if !known {
		e.fail(M2KindUnknown, "the workflow kind %q is not one that authors merge requests", e.f.Kind)
	}
	if !fullSHA(e.f.JudgedSHA) {
		e.fail(M2JudgedSHAInvalid, "the judged commit %q is not a full 40-hex SHA; a merge cannot be pinned to it", e.f.JudgedSHA)
	}
	switch {
	case e.f.BotUserID == 0:
		e.fail(M2BotUnknown, "the bot's own user id is unknown, so authorship cannot be checked")
	case mr.AuthorID != e.f.BotUserID:
		e.fail(M2NotBotAuthored, "the merge request's author (user %d) is not the bot (user %d)", mr.AuthorID, e.f.BotUserID)
	}
	if known && !strings.HasPrefix(mr.SourceBranch, prefix) {
		e.fail(M2BranchNotWorkflow, "the source branch %q does not start with %q", mr.SourceBranch, prefix)
	}
	if mr.SourceBranch != mr.RecordBranch {
		e.fail(M2BranchNotRecorded, "the source branch %q is not the branch on the record (%q)", mr.SourceBranch, mr.RecordBranch)
	}
	if mr.RecordAnchor == "" || mr.MetadataAnchor != mr.RecordAnchor {
		e.fail(M2AnchorMismatch, "the metadata anchor %q is not the record's anchor (%q)", mr.MetadataAnchor, mr.RecordAnchor)
	}
	if mr.HeadSHA != e.f.JudgedSHA {
		e.fail(M2HeadNotJudged, "the merge request head %q is not the judged commit %q", mr.HeadSHA, e.f.JudgedSHA)
	}
	if mr.Draft {
		e.fail(M2Draft, "the merge request is a draft")
	}
	for _, l := range mr.Labels {
		if strings.EqualFold(strings.TrimSpace(l), HoldLabel) {
			e.fail(M2Hold, "the merge request carries the label %s", HoldLabel)
			break
		}
	}
	if !mr.BlockingDiscussionsResolved {
		e.fail(M2ThreadsUnresolved, "the merge request has unresolved blocking threads")
	}
}

// m3: the workflow's exit verdict on the judged commit is its best one.
func (e *eval) m3() {
	if !e.read(e.f.Verdict.ReadAt, M3Unread, M3Stale, "the recomputed exit verdict") {
		return
	}
	v := e.f.Verdict
	if v.SHA != e.f.JudgedSHA {
		e.fail(M3VerdictWrongSHA, "the verdict is about %q, not the judged commit %q", v.SHA, e.f.JudgedSHA)
	}
	switch e.f.Kind {
	case KindPipefix:
		switch v.Verdict {
		case string(pipefix.VerdictFixed):
		case string(pipefix.VerdictFixedNewFailure):
			e.fail(M3FixedNewFailure, "the verdict is %s: a fix that revealed a new failure is never merged automatically", v.Verdict)
			return
		default:
			e.fail(M3NotBestVerdict, "the pipeline-fix verdict is %q, not %s", v.Verdict, pipefix.VerdictFixed)
			return
		}
		if len(v.TargetJobs) == 0 {
			e.fail(M3NoTargetJobs, "the verdict names no diagnosed job")
			return
		}
		var notPassed, retried []string
		for _, j := range v.TargetJobs {
			if !j.Passed {
				notPassed = append(notPassed, j.Name)
			}
			if j.Attempts != 1 {
				retried = append(retried, fmt.Sprintf("%s (%d attempts)", j.Name, j.Attempts))
			}
		}
		if len(notPassed) > 0 {
			e.fail(M3TargetNotPassed, "diagnosed jobs that did not pass: %s", strings.Join(notPassed, ", "))
		}
		if len(retried) > 0 {
			e.fail(M3TargetRetried, "diagnosed jobs that did not pass on their only attempt: %s", strings.Join(retried, ", "))
		}
	case KindChange:
		if v.Verdict != VerdictGreen {
			e.fail(M3NotBestVerdict, "the change verdict is %q, not %s", v.Verdict, VerdictGreen)
		}
	}
}

// m4: the CI definition delta. `exec-changing` is refused by its own code:
// no setting makes it eligible.
func (e *eval) m4(class scope.Class, classKnown bool) {
	if !e.read(e.f.CI.ReadAt, M4Unread, M4Stale, "the CI definition delta") {
		return
	}
	delta := e.f.CI.Delta
	switch delta {
	case cidef.KindNone, cidef.KindPreserving:
	case cidef.KindExecChanging:
		e.fail(M4ExecChanging, "the change alters what a CI job executes: never merged automatically, under any setting")
		return
	default:
		e.fail(M4DeltaNotEligible, "the CI definition delta is %q, not %s or %s", delta, cidef.KindNone, cidef.KindPreserving)
		return
	}
	if !classKnown {
		return // M5 has refused; there is no class row to hold the delta to
	}
	if want := classDelta(class); delta != want {
		e.fail(M4DeltaNotForClass, "a %s-only diff is eligible with CI delta %s; this one has %s", class, want, delta)
	}
}

// classDelta is plan 5.2's delta per class row: a docs-only diff changes no
// CI definition at all; a ci-only diff changes one and preserves every job's
// GATING and EXEC keys.
func classDelta(class scope.Class) cidef.Kind {
	if class == scope.ClassCI {
		return cidef.KindPreserving
	}
	return cidef.KindNone
}

// m5class says which ONE eligible class the listed paths are; known is false
// when they are not all of one class of EligibleClasses. It reports nothing
// and does not ask whether the diff was read: m5 does both. It exists so M4
// can hold the CI delta to the class row.
func (e *eval) m5class() (class scope.Class, known bool) {
	paths := e.f.Diff.Paths
	if len(paths) == 0 {
		return "", false
	}
	class = paths[0].Decision.Class
	for _, p := range paths {
		if p.Decision.Class != class {
			return "", false
		}
	}
	if !containsClass(EligibleClasses(), class) {
		return "", false
	}
	return class, true
}

// m5: scope holds on the CURRENT diff, and the diff is one eligible class.
func (e *eval) m5() {
	if !e.read(e.f.Diff.ReadAt, M5Unread, M5Stale, "the merge request's current diff") {
		return
	}
	d := e.f.Diff
	if d.Incomplete {
		e.fail(M5DiffIncomplete, "GitLab did not return the whole diff")
		return
	}
	if len(d.Paths) == 0 {
		e.fail(M5EmptyDiff, "the merge request changes no file")
		return
	}
	var unclassified, undecidable, never, disabled []string
	classes := map[scope.Class]bool{}
	for _, p := range d.Paths {
		c := p.Decision.Class
		name := fmt.Sprintf("`%s` (%s)", p.Decision.Path, c)
		switch {
		case p.Decision.Path == "" || !c.Valid():
			unclassified = append(unclassified, name)
			continue
		case !containsClass(EligibleClasses(), c):
			never = append(never, name)
		case !containsClass(e.cfg.Classes, c):
			disabled = append(disabled, name)
		}
		if !p.Decision.WeakeningDecidable() {
			undecidable = append(undecidable, name)
		}
		classes[c] = true
	}
	if len(unclassified) > 0 {
		e.fail(M5Unclassified, "changed paths with no class: %s", strings.Join(unclassified, ", "))
		return
	}
	if !d.StaticGatesPassed {
		e.fail(M5StaticGates, "the static gates do not pass on the current diff: %s", strings.Join(d.StaticGateCodes, ", "))
	}
	if len(undecidable) > 0 {
		e.fail(M5WeakeningUndecidable, "whether these weaken the tests is not decidable: %s", strings.Join(undecidable, ", "))
	}
	if len(never) > 0 {
		e.fail(M5ClassNeverEligible, "only docs-only and ci-only diffs are ever merged automatically; this one changes %s", strings.Join(never, ", "))
	}
	if len(disabled) > 0 {
		e.fail(M5ClassNotEnabled, "auto_merge.classes does not list the class of %s", strings.Join(disabled, ", "))
	}
	if len(never) == 0 && len(classes) > 1 {
		e.fail(M5MixedClasses, "the diff is neither docs-only nor ci-only: it changes both")
	}
	// The two class rows' own conditions are checked on whatever paths of the
	// class the diff holds, mixed or not, so a refusal names everything wrong.
	var docs []ChangedPath
	for _, p := range d.Paths {
		if p.Decision.Class == scope.ClassDocs {
			docs = append(docs, p)
		}
	}
	if classes[scope.ClassCI] && e.f.Kind == KindChange {
		e.fail(M5CINotForKind, "a ci-only diff is eligible on a pipeline-fix merge request only")
	}
	if len(docs) > 0 {
		if findings := DocsPure(docs, d.Profiles); len(findings) > 0 {
			e.fail(M5DocsPure, "MP-DOCSPURE: not prose files only: %s", docsPureText(findings))
		}
	}
}

func containsClass(list []scope.Class, c scope.Class) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

// m6: the class evidence. Both class rows need every candidate pipeline of
// the judged commit green with no failed attempt; a pipeline-fix needs its
// diagnosed failure to have been seen at least twice (fix plan A6).
func (e *eval) m6() {
	if !e.read(e.f.Evidence.ReadAt, M6Unread, M6Stale, "the candidate pipelines") {
		return
	}
	ev := e.f.Evidence
	if len(ev.Pipelines) == 0 {
		e.fail(M6NoPipeline, "the judged commit has no candidate pipeline")
	}
	var wrongSHA, notGreen, failedAttempts []string
	for _, p := range ev.Pipelines {
		id := fmt.Sprintf("#%d", p.ID)
		if p.SHA != e.f.JudgedSHA {
			wrongSHA = append(wrongSHA, fmt.Sprintf("%s (%q)", id, p.SHA))
		}
		if p.Status != PipelineSuccess {
			notGreen = append(notGreen, fmt.Sprintf("%s (%q)", id, p.Status))
		}
		if p.FailedAttempts != 0 {
			failedAttempts = append(failedAttempts, fmt.Sprintf("%s (%d)", id, p.FailedAttempts))
		}
	}
	if len(wrongSHA) > 0 {
		e.fail(M6PipelineWrongSHA, "candidate pipelines that are not of the judged commit %q: %s", e.f.JudgedSHA, strings.Join(wrongSHA, ", "))
	}
	if len(notGreen) > 0 {
		e.fail(M6PipelineNotSuccess, "candidate pipelines that are not %s: %s", PipelineSuccess, strings.Join(notGreen, ", "))
	}
	if len(failedAttempts) > 0 {
		e.fail(M6FailedAttempt, "candidate pipelines holding a failed job attempt: %s", strings.Join(failedAttempts, ", "))
	}
	if e.f.Kind == KindPipefix && ev.DiagnosedObservations < 2 {
		e.fail(M6ObservedOnce, "the diagnosed failure was observed %d time(s); two are required", ev.DiagnosedObservations)
	}
}

// m7: the judged pipeline is not stale against the target.
func (e *eval) m7() {
	if !e.read(e.f.Target.ReadAt, M7Unread, M7Stale, "the target branch head") {
		return
	}
	t := e.f.Target
	if !fullSHA(t.HeadSHA) || t.HeadSHA != t.JudgedBaseSHA {
		e.fail(M7TargetMoved, "the target branch head %q is not the commit the judged pipeline was built on (%q)", t.HeadSHA, t.JudgedBaseSHA)
	}
}

// m8: a human who may approve did, after the head commit. The bot's own
// approval never counts.
func (e *eval) m8() {
	if e.cfg.Mode != ModeAfterHumanApproval {
		return
	}
	if !e.read(e.f.Approvals.ReadAt, M8Unread, M8Stale, "the merge request's approvals") {
		return
	}
	a := e.f.Approvals
	if !a.AgeObservable && a.CommitCount != 1 {
		e.fail(M8AgeUnobservable, "an approval cannot be dated on this GitLab, and the merge request has %d commits: only one with exactly one commit is eligible", a.CommitCount)
		return
	}
	need := e.cfg.RequiredApprovals
	if need < 1 {
		need = 1
	}
	counted := map[int64]bool{}
	var excluded []string
	for _, ap := range a.Approvals {
		if why := e.approvalExcluded(ap); why != "" {
			excluded = append(excluded, fmt.Sprintf("user %d: %s", ap.UserID, why))
			continue
		}
		counted[ap.UserID] = true
	}
	if len(counted) < need {
		text := fmt.Sprintf("%d qualifying human approval(s), %d required", len(counted), need)
		if len(excluded) > 0 {
			text += "; not counted: " + strings.Join(excluded, "; ")
		}
		e.fail(M8ApprovalsInsufficient, "%s", text)
	}
}

// approvalExcluded says why an approval does not count; "" when it does.
func (e *eval) approvalExcluded(ap Approval) string {
	a, bot := e.f.Approvals, e.f.BotUserID
	switch {
	case ap.UserID == 0:
		return "the approver has no user id"
	case bot == 0:
		return "the bot's user id is unknown, so the approver is not provably someone else"
	case ap.UserID == bot:
		return "the bot's own approval never counts"
	case !ap.Member:
		return "not a project member"
	case ap.AccessLevel < AccessDeveloper:
		return fmt.Sprintf("access level %d is below Developer (%d)", ap.AccessLevel, AccessDeveloper)
	case !a.AgeObservable:
		return ""
	case a.HeadCommittedAt.IsZero():
		return "the head commit's time is unknown"
	case !ap.ApprovedAt.After(a.HeadCommittedAt):
		return "the approval is not newer than the head commit"
	}
	return ""
}

// m9: the review. A veto under every mode; under ModeAfterAgenticReview also
// the necessary fact, and only when a DISTINCT model produced it.
func (e *eval) m9() {
	if !e.read(e.f.Review.ReadAt, M9Unread, M9Stale, "the review records") {
		return
	}
	var clean []Review
	blocking, unknown := 0, ""
	for _, r := range e.f.Review.Reviews {
		if r.HeadSHA != e.f.JudgedSHA {
			continue // a review of another head says nothing about this commit
		}
		switch r.Verdict {
		case ReviewBlocking:
			blocking++
		case ReviewNoBlockingFindings:
			clean = append(clean, r)
		default:
			unknown = r.Verdict
		}
	}
	if blocking > 0 {
		e.fail(M9Blocking, "%d review(s) of the judged commit returned `%s`: a veto under every mode", blocking, ReviewBlocking)
	}
	if unknown != "" {
		e.fail(M9VerdictUnknown, "a review of the judged commit has the verdict %q, which is not a review verdict", unknown)
	}
	if e.cfg.Mode != ModeAfterAgenticReview {
		return
	}
	if !e.f.Review.ReviewsEnabled {
		e.fail(M9ReviewsNotEnabled, "after-agentic-review needs actions.reviews, which is not enabled")
	}
	if len(clean) == 0 {
		e.fail(M9NoReviewFact, "no review fact for the judged commit %q has the verdict `%s`", e.f.JudgedSHA, ReviewNoBlockingFindings)
		return
	}
	same := ""
	for _, r := range clean {
		switch Independence(r.AuthorModel, r.ReviewerModel) {
		case IndependenceDistinctModel:
			return
		case IndependenceSameModel:
			same = strings.TrimSpace(r.ReviewerModel)
		}
	}
	if same != "" {
		e.fail(M9SameModel, "after-agentic-review is UNAVAILABLE: the only review of the judged commit ran on the author's own model (%s); a distinct reviewer model is required", same)
		return
	}
	e.fail(M9ModelUnknown, "after-agentic-review is UNAVAILABLE: the author's or the reviewer's model was not recorded, so the review is not provably by a distinct model")
}

// m10: GitLab agrees.
func (e *eval) m10() {
	if !e.read(e.f.Status.ReadAt, M10Unread, M10Stale, "the merge request's merge status") {
		return
	}
	if s := e.f.Status.DetailedMergeStatus; s != StatusMergeable {
		e.fail(M10NotMergeable, "detailed_merge_status is %q, not %s", s, StatusMergeable)
	}
}

// m11: the per-project rate cap.
func (e *eval) m11() {
	if !e.read(e.f.Rate.ReadAt, M11Unread, M11Stale, "the project's automatic-merge ledger") {
		return
	}
	if n := MergesInWindow(e.f.Rate.RecentMerges, e.now); n >= e.cfg.MaxPerDay {
		e.fail(M11RateCapReached, "%d automatic merge(s) on this project in the last %s; auto_merge.max_per_day is %d", n, RateWindow, e.cfg.MaxPerDay)
	}
}
