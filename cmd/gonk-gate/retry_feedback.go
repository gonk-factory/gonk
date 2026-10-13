package main

// RETRY FEEDBACK (gonk-nf9i, gonk-zxsx): a refused answer leaves a word for
// the next attempt.
//
// Before this, an answer refused by a static gate -- unparseable, out of
// shape, any rule code -- was retried with a byte-identical prompt. On dev
// the fix agent misspelled one JSON key in 5 of 8 sessions and the retry was
// told nothing; 2 of 5 runs lost a correct diagnosis to it.
//
// THE REFUSED ANSWER IS UNTRUSTED. It is model output, and what the model
// read is project text. So nothing of it is replayed:
//
//  1. the statement is BUILT by pkg/effects (Explain*) and by the fixed
//     sentences of this file -- never copied from the answer, a Refusal's
//     Detail, or an error's text. The one name a refusal is about (a
//     misspelled key) comes back as effects.Token: 32 bytes of
//     [A-Za-z0-9_.-], quoted;
//  2. it is RECORDED (beadstore.Record.RetryFeedback), so the re-dispatch
//     after a controller restart renders the same block;
//  3. it is RENDERED through feedbackText, which filters every field again
//     to printable ASCII without the characters a fence or a marker is made
//     of, and caps it. The store is ours, and this does not rely on that.
//
// Nothing here decides anything: no gate reads the feedback, and no answer
// is repaired. Wired for the `pipefix` and `pipeline` agents, which have a
// validation chain of their own, and (gonk-hpgy) for `triage` and `scaffold`,
// which share the issue-shaped chain of applyBrokerBatch: its gates' errors
// quote the batch (a label, a path, a comment line), so each gate has ONE
// fixed sentence here (issueGateFeedback) and its error text is never read.
// The `mention` agent is not dispatched and is not wired.

import (
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
)

// Caps on the rendered block, in bytes. Together with the frame they bound
// the block at retryFeedbackMaxBytes, which the prompt caps are budgeted for.
//
// The diagnosis block also states the two closed sets an evidence item
// chooses from (diagnosisFeedbackSets): one line of failed job names, at most
// retryFeedbackCapJobs bytes, and the fact-key line, a constant of pkg/effects.
// retryFeedbackDiagMaxBytes bounds that block; the diagnosis prompt is fitted
// with it in place (fitPipelinePrompt), and
// TestPipelinePromptFitsWithFourMaximalJobs holds a maximal one.
const (
	retryFeedbackCapCode    = 64
	retryFeedbackCapWhat    = 200
	retryFeedbackCapAllowed = 200
	retryFeedbackMaxBytes   = 768
	retryFeedbackCapJobs    = 320
)

// retryFeedbackDiagMaxBytes is the most a diagnosis prompt's block can be:
// the common block, the job-name line and the fact-key line, each with its
// line break.
var retryFeedbackDiagMaxBytes = retryFeedbackMaxBytes + retryFeedbackCapJobs + 1 + len(effects.FactsText()) + 1

// retryFeedbackHeading opens the block. The component stub keys on it.
const retryFeedbackHeading = "== Your previous answer was refused =="

// retryFeedbackKinds are the effect kinds of an agent's task, for a parse
// refusal's feedback; nil for an agent that gets no feedback.
func retryFeedbackKinds(agent string) []effects.Kind {
	switch agent {
	case pipefixAgent:
		return effects.FixKinds()
	case pipelineAgent:
		return []effects.Kind{effects.KindDiagnosis}
	case triageAgent: // TestIssueAgentFeedbackKindsAreTheirShapes ties both to the shape files
		return []effects.Kind{effects.KindComment, effects.KindLabel}
	case scaffoldAgent:
		return []effects.Kind{effects.KindFile}
	}
	return nil
}

// The issue-shaped agents (agentForTrigger's values for issue-triage and
// scaffold).
const (
	triageAgent   = "triage"
	scaffoldAgent = "scaffold"
)

// ownChainAgent reports whether agent has a validation chain of its own
// (applyPipelineBatch, applyPipefixBatch) rather than the issue-shaped one.
func ownChainAgent(agent string) bool { return agent == pipelineAgent || agent == pipefixAgent }

// The gates of the issue-shaped chain, as applyBrokerBatch prefixes a
// violation and as the feedback's code spells them.
const (
	issueGateShape      = "shape"
	issueGateTarget     = "target"
	issueGatePath       = "path"
	issueGateComment    = "comment"
	issueGateTrajectory = "trajectory"
	issueGateLabel      = "label"
)

// issueGateFeedback is one fixed sentence per gate of the issue-shaped chain
// whose refusal pkg/effects has no Explain* for. Each says what the gate
// refuses and nothing of what the batch held: the gate's own error quotes the
// label, the path or the comment line, and an issue is text anyone may write.
var issueGateFeedback = map[string]string{
	issueGateTarget:     "an effect names a target that is not the issue of this task; leave target_iid and target_type out",
	issueGatePath:       "a file effect names a path this task may not write, or holds more than a file may",
	issueGateComment:    "the comment holds a line GitLab would act on, or is too long",
	issueGateTrajectory: feedbackDiagTrajectory,
	issueGateLabel:      "a label is empty, too long, holds a comma, or is one only gonk sets",
}

// issueWording fits a sentence of pkg/effects to the issue-shaped prompts,
// which show the fence lines without the OUTPUT heading the two pipeline
// prompts put above them.
func issueWording(what string) string {
	return strings.ReplaceAll(what, " shown under OUTPUT", " this message shows") // feedback:issue-wording
}

// retryFeedbackSuffix is what an issue-shaped prompt ends with for rec's
// attempt: a blank line and the block, or nothing.
func retryFeedbackSuffix(rec beadstore.Record) string {
	if fb := renderRetryFeedback(rec.RetryFeedback, rec.Attempt); fb != "" { // prompt:issue-refused
		return "\n\n" + fb
	}
	return ""
}

// scaffoldPromptFor is the scaffold prompt built from fetched material, with
// the feedback block -- except when there is no material: that prompt tells
// the session to emit NO batch, and "answer again in full" would contradict it.
func scaffoldPromptFor(rec beadstore.Record, project, repoContext string) string {
	p := renderScaffoldPrompt(project, repoContext)
	if strings.TrimSpace(repoContext) == "" { // prompt:scaffold-no-material
		return p
	}
	return p + retryFeedbackSuffix(rec)
}

// carryRetryFeedback puts, on the record an issue-shaped dispatch writes,
// what the sweep recorded about a refused earlier answer of the same bead --
// as setPipelineIdentity and setPipelineFixIdentity do for their agents. The
// renderer shows it only to a later attempt than the one it is about.
func carryRetryFeedback(base *beadstore.Record, decided decidedOnRecord) {
	if prior := decided.rec; decided.err == nil && decided.found && prior.BeadAnchor == base.BeadAnchor {
		base.RetryFeedback = prior.RetryFeedback // dispatch:carry-issue-feedback
	}
}

// recordRetryFeedback puts fb on the record as the feedback of the attempt
// the record is running. The caller's record write persists it.
func recordRetryFeedback(recp *beadstore.Record, fb effects.Feedback) {
	if fb.What == "" { // feedback:record-empty
		return
	}
	recp.RetryFeedback = beadstore.RetryFeedback{Attempt: recp.Attempt, Code: fb.Code, What: fb.What, Allowed: fb.Allowed}
}

// feedbackText makes one recorded field safe to print on one line of a
// prompt: every byte that is not printable ASCII, and each of ` < > = which
// fences, markers, code spans and this prompt's own headings are made of,
// becomes '?'; then it is cut to max bytes. A field built by pkg/effects passes through unchanged.
func feedbackText(s string, max int) string {
	b := []byte(s)
	if len(b) > max { // feedback:render-cap
		b = b[:max]
	}
	for i, c := range b {
		if c < 0x20 || c > 0x7e || c == '`' || c == '<' || c == '>' || c == '=' { // feedback:render-filter
			b[i] = '?'
		}
	}
	return string(b)
}

// renderRetryFeedback is the block a prompt carries about a refused earlier
// attempt, "" when there is none to carry: nothing recorded, or feedback that
// is not about an EARLIER attempt than the one being prompted.
//
// The format is fixed:
//
//	== Your previous answer was refused ==
//	gonk checked the answer of attempt <n> of this task and refused it; nothing of it was used.
//	refused by: <code>                      (only when the refusal has a code)
//	what was wrong: <what>
//	what is allowed: <allowed>              (only when the refusal is about a set)
//	<one line per closed set of the task>   (sets; the diagnosis agent's only)
//	gonk wrote the lines above from its own checks. Answer again in full, with this corrected.
//
// sets are whole lines the CALLER built from the gate's own closed sets, each
// already bounded and without a line break (diagnosisFeedbackSets). Nothing
// of the record goes into them.
func renderRetryFeedback(fb beadstore.RetryFeedback, attempt int, sets ...string) string {
	if !fb.Recorded() || fb.Attempt >= attempt { // feedback:render-guard
		return ""
	}
	var b strings.Builder
	b.WriteString(retryFeedbackHeading + "\n")
	fmt.Fprintf(&b, "gonk checked the answer of attempt %d of this task and refused it; nothing of it was used.\n", fb.Attempt)
	if fb.Code != "" {
		fmt.Fprintf(&b, "refused by: %s\n", feedbackText(fb.Code, retryFeedbackCapCode))
	}
	fmt.Fprintf(&b, "what was wrong: %s\n", feedbackText(fb.What, retryFeedbackCapWhat))
	if fb.Allowed != "" {
		fmt.Fprintf(&b, "what is allowed: %s\n", feedbackText(fb.Allowed, retryFeedbackCapAllowed))
	}
	for _, line := range sets {
		b.WriteString(line + "\n")
	}
	b.WriteString("gonk wrote the lines above from its own checks. Answer again in full, with this corrected.\n")
	return b.String()
}

// ---- the diagnosis agent's closed sets (gonk-zxsx, gonk-37kg.1) -------------------
//
// On dev a diagnosis named a failed job with two digits of its name swapped,
// was told "copied exactly from its heading", and swapped two other digits on
// the retry. A refused diagnosis is therefore told the two closed sets an
// evidence item chooses from, whatever it was refused for: the names "job"
// may hold and the keys "fact" may hold.
//
// BOTH COME FROM WHAT THE GATE READS. The names are
// effects.DiagnosisContext.JobNames() of the context of THIS dispatch -- the
// same enumeration effects.ValidateDiagnosis builds rule:evidence-job's set
// from, over the jobs this dispatch records for the sweep (JobRecords). The
// keys are effects.FactsText(), built from the list the rule's set is built
// from. TestDiagnosisFeedbackSetsAreTheGates fails if either drifts.
//
// A JOB NAME IS PROJECT TEXT, not model output: whoever can push wrote it,
// and the prompt already shows it, in this canonical form, in the job's
// heading. Here it is shown WHOLE AND UNCHANGED OR NOT AT ALL -- a name that
// is not one token and one that does not fit the line are counted, never cut
// or repaired: a near-copy of a name is the very error this line is for.
//
// ONE TOKEN, because this line stands above "gonk wrote the lines above from
// its own checks": a job named like a sentence would be read as the
// controller's. A name is shown only when every byte is an ASCII letter, a
// digit or one of - _ . : / (feedbackJobToken). No space, so no sentence; no
// quote, backslash or control byte, so nothing to escape; and nothing the
// block's own filter (feedbackText) would alter. A name with a space or a
// matrix suffix (`test: [go, 1.22]`) is therefore only counted, and the line
// says where it is: in the headings under Failed jobs. What a shown token
// SPELLS is still the project's choice; the line's intro says they are names.

const (
	feedbackJobsIntro = "the failed jobs are, each name between double quotes: "
	feedbackJobsNone  = "the failed jobs are: none; the only evidence item this pipeline takes is the fact pipeline_error, without \"job\""
	// feedbackJobsMore closes a line that could not show every name.
	feedbackJobsMore = "%d more, named in the headings under Failed jobs"
)

// feedbackJobToken reports whether c may stand in a job name the block shows.
func feedbackJobToken(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.:/", c) >= 0 // feedback:job-token
}

// feedbackJobListable reports whether a job name is shown in the block: it is
// not empty and every byte of it is a feedbackJobToken byte.
func feedbackJobListable(name string) bool {
	for i := 0; i < len(name); i++ {
		if !feedbackJobToken(name[i]) { // feedback:job-listable
			return false
		}
	}
	return name != "" // feedback:job-nonempty
}

// feedbackJobsLine is the job-name line: every listable name that fits, in
// the context's order, and a count of the rest. At most retryFeedbackCapJobs
// bytes, without a line break.
func feedbackJobsLine(names []string) string {
	if len(names) == 0 {
		return feedbackJobsNone
	}
	// Room is kept for the longest closing the line can need.
	room := retryFeedbackCapJobs - len(feedbackJobsIntro) - len("; ") - len(fmt.Sprintf(feedbackJobsMore, len(names)))
	var shown []string
	used, more := 0, 0
	for _, n := range names {
		q := `"` + n + `"`
		if !feedbackJobListable(n) || used+len(q)+len(", ") > room { // feedback:job-fits
			more++
			continue
		}
		shown = append(shown, q)
		used += len(q) + len(", ")
	}
	line := feedbackJobsIntro + strings.Join(shown, ", ")
	if more > 0 { // feedback:job-more
		if len(shown) > 0 {
			line += "; "
		}
		line += fmt.Sprintf(feedbackJobsMore, more)
	}
	return line
}

// diagnosisFeedbackSets are the closed-set lines of a diagnosis prompt's
// block: the job names of dc, then the fact keys -- left out when the
// refusal's own "what is allowed" already is that line.
func diagnosisFeedbackSets(fb beadstore.RetryFeedback, dc effects.DiagnosisContext) []string {
	sets := []string{feedbackJobsLine(dc.JobNames())}
	if facts := effects.FactsText(); fb.Allowed != facts { // feedback:facts-once
		sets = append(sets, facts)
	}
	return sets
}

// pipefixRuleFeedback is one fixed sentence per rule code of
// broker_pipefix_gates.go, as pkg/effects has for its own
// (TestPipefixEveryRuleHasFeedback). Each names what the rule refuses and
// nothing about any failure or how to mend one.
var pipefixRuleFeedback = map[effects.Rule]string{
	ruleFixPackShape: "the batch does not hold the effects this task takes: a kind is missing, repeated too often, or not one of them",
	ruleFixTarget:    "an effect names a target; a fix batch names none (no target_iid, no target_type)",
	ruleFixTraject:   "the session's recorded steps do not support its answer",
	ruleShown:        "an edit names a file that is not in the list of files you may change",
	ruleWeaken:       "the change removes, skips or builds out a test, which the rules of this task forbid",
	ruleCIPre:        "the change would let a job fail without failing the pipeline, or stop a job from running, which the rules of this task forbid",
	ruleTextMarker:   "the text you wrote for people reads as one of gonk's own markers",
	ruleTextSecret:   "the text you wrote for people holds what looks like a credential",
	ruleTextComment:  "the text you wrote for people holds a line GitLab would act on, or is too long",
	ruleCIDef:        "the change alters how the pipeline decides what runs or what may fail, which the rules of this task forbid",
}

// fixRefusalFeedback is the feedback for a fix batch a gate refused: the
// sentence of pkg/effects for its rules, this file's for the apply's own.
// Like effects.ExplainFix it reads the refusal's codes and effect index only.
func fixRefusalFeedback(r *effects.Refusal, batch effects.Batch) effects.Feedback {
	fb := effects.ExplainFix(r, batch)
	if s := pipefixRuleFeedback[r.Rule]; s != "" { // feedback:own-rule
		fb.What = s
	}
	return fb
}

// Fixed sentences for the refusals of the diagnosis apply that are not
// effects.ValidateDiagnosis's.
const (
	feedbackDiagTarget     = "an effect names a target that is not this pipeline; leave target_iid and target_type out"
	feedbackDiagMarker     = "the diagnosis holds text that reads as one of gonk's own markers"
	feedbackDiagSecret     = "the diagnosis holds what looks like a credential"
	feedbackDiagQuick      = "the diagnosis holds a line GitLab would act on, or is too long to post"
	feedbackDiagTrajectory = "the session's recorded steps do not support its answer"
)
