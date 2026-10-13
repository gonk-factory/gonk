package main

// WHAT A JUDGED FIX LOOKS LIKE ON GITLAB (gonk-2usw plan 8.1; part VERIFY):
// the fix note's text and the merge request's status block for every state
// the exit check and the post-merge confirmation write.
//
// EVERY WORD IS THE CONTROLLER'S. The only project-chosen text is job names:
// in a note they are code spans (codeSpan), in the merge request's
// description exact names GitLab can read nothing into (ident, gonk-htl3).
// pipefix.Result.Detail is NEVER rendered -- it quotes job names raw, and a note that held marker-shaped
// text would be refused by fixNote.render for ever. The texts are built from
// the rule codes and the findings instead.

import (
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/pipefix"
)

// jobFindingLines renders the per-job verdict table of plan 8.1: one line per
// diagnosed job and per other failing job. quote is codeSpan for a note and
// ident for a merge request description.
func jobFindingLines(res pipefix.Result, quote func(string) string) string {
	var b strings.Builder
	for _, j := range res.Jobs {
		fmt.Fprintf(&b, "- %s: %s", quote(j.Job), findingWords(j.Rule))
		if j.Attempts > 1 {
			fmt.Fprintf(&b, " (%d attempts: %d failed, %d passed)", j.Attempts, j.Failed, j.Succeeded)
		}
		b.WriteString("\n")
	}
	for _, o := range res.Others {
		fmt.Fprintf(&b, "- %s (not a diagnosed job): %s\n", quote(o.Job), findingWords(o.Rule))
	}
	return b.String()
}

// findingWords puts one J3 / J4 rule into words. A rule this build has no
// words for is shown by its code, never dropped.
func findingWords(r pipefix.Rule) string {
	switch r {
	case pipefix.RuleJ3Passed:
		return "ran and passed"
	case pipefix.RuleJ3Recurs:
		return "still fails the way it was diagnosed"
	case pipefix.RuleJ3Changed:
		return "no longer fails the way it was diagnosed; it fails differently"
	case pipefix.RuleJ3FlakySame, pipefix.RuleJ3FlakyChanged:
		return "both failed and passed across its attempts"
	case pipefix.RuleJ3Weakened:
		return "was allowed to fail"
	case pipefix.RuleJ3Absent:
		return "did not run at all"
	case pipefix.RuleJ3Status:
		return "did not finish as passed or failed (skipped, manual, canceled or never started)"
	case pipefix.RuleJ3Unread, pipefix.RuleJ3Unknown:
		return "could not be read"
	case pipefix.RuleJ3Versions:
		return "cannot be compared with the recorded failure by this version of gonk"
	case pipefix.RuleJ4Regressed:
		return "passed before this change and fails now"
	case pipefix.RuleJ4RegressedAllowed:
		return "passed before this change and fails now (it is allowed to fail)"
	case pipefix.RuleJ4Remaining:
		return "was already failing and still fails"
	case pipefix.RuleJ4Revealed:
		return "did not run before and fails now"
	}
	return "rule " + codeSpan(string(r))
}

// unverifiableWhy says, by rule family, why nothing could be established.
func unverifiableWhy(rule string) string {
	switch {
	case rule == ruleVerifyBoundCannotJudge || rule == ruleVerifyBoundWait:
		return "gonk could not read what it needed from GitLab, or the facts it recorded for this fix are incomplete, and it has stopped waiting"
	case rule == string(pipefix.RuleJ1MergedResult):
		return "this project runs merged results pipelines (or a merge train): the merge request's pipeline ran on a commit GitLab made by " +
			"merging the branch into its target, not on the fix's commit, and gonk cannot tell from GitLab which commit of the branch that was. " +
			"gonk can verify a fix only from a pipeline of the fix's own commit"
	case rule == string(pipefix.RuleJ1Timeout):
		return "no pipeline of the fix's commit finished before the verify deadline"
	case rule == string(pipefix.RuleJ1Unread) || rule == string(pipefix.RuleJ2Unread) || rule == string(pipefix.RuleJ3Unread):
		return "GitLab could not be read before the verify deadline"
	case strings.HasPrefix(rule, "J2."):
		return "the pipeline definition could not be compared with the one the failure was diagnosed under"
	case rule == string(pipefix.RuleJ5NotSuccess):
		return "the pipeline did not succeed and no failed job explains why"
	}
	return "the diagnosed job was not observed running in the fix's pipeline"
}

// fixVerdictTexts renders a settlement: the fix note (for the diagnosis
// landing) and the merge request's status block. final is the state the fix
// ends in; retry says a further attempt follows a not-fixed verdict, and
// whyNot why none does.
func fixVerdictTexts(rec beadstore.Record, s fixSettlement, final pipefix.State, retry bool, whyNot string) (note, status string) {
	pf := rec.PipelineFix
	mr := fmt.Sprintf("!%d", pf.MRIID)
	attempt := max(rec.Attempt, 1)
	pipes := pipelineList(rec, s.res.Candidates)
	ruleNote := fmt.Sprintf(" (rule %s)", codeSpan(s.rule))
	var n, st strings.Builder
	switch {
	case final == pipefix.StateFixed:
		fmt.Fprintf(&n, "**%s: the diagnosed failure no longer occurs.** Every diagnosed job ran and passed in %s, on every attempt.", mr, pipes)
		st.WriteString("**Verified.** Every diagnosed job ran and passed in this merge request's pipeline, on every attempt.")
	case final == pipefix.StateFixedNewFailure:
		fmt.Fprintf(&n, "**%s: the diagnosed failure no longer occurs, and a different failure does.** In %s the failure gonk diagnosed is gone; "+
			"what fails now is listed below and will be diagnosed after %s merges.", mr, pipes, mr)
		st.WriteString("**Verified, with a different failure.** The failure gonk diagnosed no longer occurs in this merge request's pipeline. " +
			"Something else fails there (listed below); gonk will diagnose it after this merges, not before.")
	case retry:
		fmt.Fprintf(&n, "**%s: attempt %d did not fix it; gonk is trying again.**%s", mr, attempt, ruleNote)
		fmt.Fprintf(&st, "**Not fixed yet.** Attempt %d did not meet the criterion%s; gonk is preparing another commit on this branch.", attempt, ruleNote)
	case final == pipefix.StateDenied:
		fmt.Fprintf(&n, "**%s: attempt %d did not fix it, and gonk has stopped.**%s No further attempt follows: %s.", mr, attempt, ruleNote, orReason(whyNot))
		fmt.Fprintf(&st, "**Not fixed.** Attempt %d did not meet the criterion%s and gonk has stopped: %s. "+
			"A person can take this branch over, or close this merge request.", attempt, ruleNote, orReason(whyNot))
	case final == pipefix.StateFlaky:
		fmt.Fprintf(&n, "**%s: inconclusive -- a diagnosed job both failed and passed.**%s gonk does not count a retry as a pass.", mr, ruleNote)
		st.WriteString("**Inconclusive: flaky.** A diagnosed job both failed and passed across its attempts in this merge request's " +
			"pipeline. gonk does not count a retry as a pass, and cannot tell whether this change is why it passed.")
	case final == pipefix.StateRegressed:
		fmt.Fprintf(&n, "**%s: inconclusive -- a job that passed before now fails.**%s", mr, ruleNote)
		st.WriteString("**Inconclusive: a job that passed before this change fails now.** It may be this change, or that job's own " +
			"instability; gonk cannot tell.")
	case final == pipefix.StateUnverifiable:
		fmt.Fprintf(&n, "**%s: inconclusive -- gonk could not establish whether it works.** %s%s.", mr, upperFirst(unverifiableWhy(s.rule)), ruleNote)
		fmt.Fprintf(&st, "**Inconclusive.** gonk could not establish whether this change works: %s%s.", unverifiableWhy(s.rule), ruleNote)
	case final == pipefix.StateTakenOver:
		fmt.Fprintf(&n, "**%s: somebody else pushed to the fix branch.** gonk has stopped judging it and will not touch the branch again.", mr)
		st.WriteString("**Taken over.** This branch's head is no longer the commit gonk made, so gonk has stopped judging this merge request.")
	case final == pipefix.StateClosedUnmerged:
		fmt.Fprintf(&n, "**%s was closed without being merged.** gonk will not propose this fix again.", mr)
		st.WriteString("**Closed.** This merge request was closed without being merged; gonk will not propose this fix again.")
	case final == pipefix.StateMerged:
		fmt.Fprintf(&n, "**%s merged.** gonk is watching the next pipeline on %s to see whether the diagnosed failure is gone.", mr, codeSpan(pf.TargetBranch))
		st.WriteString("**Merged.**")
	default:
		fmt.Fprintf(&n, "**%s: state %s.**%s", mr, codeSpan(string(final)), ruleNote)
		fmt.Fprintf(&st, "**State %s.**%s", codeSpan(string(final)), ruleNote)
	}
	if lines := jobFindingLines(s.res, codeSpan); lines != "" {
		n.WriteString("\n\n" + lines)
	}
	if lines := jobFindingLines(s.res, ident); lines != "" {
		st.WriteString("\n\n" + lines)
	}
	if facts := mergeCautions(pf, s); facts != "" {
		n.WriteString("\n" + facts)
		st.WriteString("\n" + facts)
	}
	if fixStateNeedsHuman(final) {
		n.WriteString("\nA person is needed: gonk will not retry this on its own.\n")
	}
	n.WriteString("\n" + fixNoteFooter)
	return strings.TrimSpace(n.String()), strings.TrimSpace(st.String())
}

// mergeCautions are the two recorded facts a person must see before merging
// (plan 6.5 U2, U4), stated on the result so nothing later merges past them.
func mergeCautions(pf beadstore.PipelineFix, s fixSettlement) string {
	var b strings.Builder
	if pf.Weakening == beadstore.FixWeakeningNotDecidable {
		b.WriteString("gonk cannot tell whether this change weakens a test, and never merges such a change on its own.\n")
	}
	if pf.CIDelta == string(cidef.KindExecChanging) || s.res.Definition == cidef.KindExecChanging {
		b.WriteString("This change alters what a diagnosed job EXECUTES, so its passing is a claim the change makes about itself; " +
			"gonk never merges such a change on its own.\n")
	}
	return b.String()
}

// pipelineList names the judged pipelines, linked: "pipeline #P" or
// "pipelines #P, #Q"; "its pipeline" when none was judged.
func pipelineList(rec beadstore.Record, ids []int64) string {
	if len(ids) == 0 {
		return "its pipeline"
	}
	refs := make([]string, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, pipelineRef(rec, id))
	}
	return strings.Join(refs, ", ")
}

func orReason(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no further attempt is allowed"
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- after the merge (plan 7.4) ----------------------------------------------

// fixMergedText is the fix note of `merged`: who watches what.
func fixMergedText(fix beadstore.Record, waitingOn string) string {
	pf := fix.PipelineFix
	text := fmt.Sprintf("**!%d merged.** gonk is watching the next pipeline on %s to see whether the diagnosed failure is gone.",
		pf.MRIID, codeSpan(pf.TargetBranch))
	if waitingOn != "" {
		text += "\n\n" + waitingOn
	}
	return text + "\n\n" + fixNoteFooter
}

// fixConfirmedText and fixRecurredText are the closing notes of plan 8.1.
func fixConfirmedText(fix beadstore.Record, pipelineID int64, green bool, res pipefix.ConfirmResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**!%d is confirmed on %s.** After it merged, the diagnosed failure did not occur there.", fix.PipelineFix.MRIID, pipelineRef(fix, pipelineID))
	if !green {
		b.WriteString(" That pipeline still failed, for a different reason: gonk treats that as a new failure and diagnoses it separately.")
	}
	b.WriteString("\n\n" + confirmFindingLines(res))
	b.WriteString("\n" + fixNoteFooter)
	return b.String()
}

func fixRecurredText(fix beadstore.Record, pipelineID int64, res pipefix.ConfirmResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**The failure recurred on %s, after !%d merged.** The fix did not remove the failure gonk diagnosed. "+
		"gonk proposes no revert and will not retry this on its own: a person is needed.", pipelineRef(fix, pipelineID), fix.PipelineFix.MRIID)
	b.WriteString("\n\n" + confirmFindingLines(res))
	b.WriteString("\n" + fixNoteFooter)
	return b.String()
}

func confirmFindingLines(res pipefix.ConfirmResult) string {
	var b strings.Builder
	for _, j := range res.Jobs {
		fmt.Fprintf(&b, "- %s: %s\n", codeSpan(j.Job), findingWords(j.Rule))
	}
	return b.String()
}

// fixEndedStatus is the run note's status when a merged, confirmed fix ended
// the run while the target is still red (plan 7.4 row 2; EPOCH's open gap).
// The note moves to `superseded`: the run is over, and whatever fails next on
// the target -- with the same failing jobs or others -- is a new run.
func fixEndedStatus(run beadstore.Record, mrIID, pipelineID int64) string {
	return fmt.Sprintf("Superseded: the fix merge request !%d merged and the failure diagnosed below did not occur in %s, "+
		"which failed for a different reason. gonk treats that as a new run. Anything below is about the earlier failure, %s.",
		mrIID, pipelineRef(run, pipelineID), pipelineRef(run, run.PipelineID))
}
