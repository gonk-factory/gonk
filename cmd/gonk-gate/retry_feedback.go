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
// is repaired. Wired for the `pipefix` and `pipeline` agents. Triage and
// scaffold share the sweep's re-sling but neither the record carry-over at
// dispatch nor a prompt that has a place for it; they are not wired.

import (
	"fmt"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
)

// Caps on the rendered block, in bytes. Together with the frame they bound
// the block at retryFeedbackMaxBytes, which the prompt caps are budgeted for.
const (
	retryFeedbackCapCode    = 64
	retryFeedbackCapWhat    = 200
	retryFeedbackCapAllowed = 200
	retryFeedbackMaxBytes   = 768
)

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
	}
	return nil
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
//	gonk wrote the lines above from its own checks. Answer again in full, with this corrected.
func renderRetryFeedback(fb beadstore.RetryFeedback, attempt int) string {
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
	b.WriteString("gonk wrote the lines above from its own checks. Answer again in full, with this corrected.\n")
	return b.String()
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
