package mergepolicy

import (
	"strings"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// RuleCode names one predicate: "<plan row>.<what>". RuleMerge is the one
// code that is not a refusal.
type RuleCode string

// RuleMerge is Decision.Rule when every row held.
const RuleMerge RuleCode = "merge"

// Rule codes, in evaluation order. The part before the dot is the row of
// epic plan 5.1. "<row>.unread": the row's facts were not read. "<row>.stale":
// they were read more than MaxFactAge ago, or after `now`.
const (
	M1Unread         RuleCode = "M1.unread"
	M1Stale          RuleCode = "M1.stale"
	M1ModeOff        RuleCode = "M1.mode-off"
	M1ModeUnknown    RuleCode = "M1.mode-unknown"
	M1OperatorVeto   RuleCode = "M1.operator-veto"
	M1LayersDisagree RuleCode = "M1.layers-disagree"
	M1ConfigNotAtTip RuleCode = "M1.config-not-at-target-head"

	M2Unread            RuleCode = "M2.unread"
	M2Stale             RuleCode = "M2.stale"
	M2KindUnknown       RuleCode = "M2.kind-unknown"
	M2JudgedSHAInvalid  RuleCode = "M2.judged-sha-invalid"
	M2BotUnknown        RuleCode = "M2.bot-identity-unknown"
	M2NotBotAuthored    RuleCode = "M2.not-bot-authored"
	M2BranchNotWorkflow RuleCode = "M2.branch-not-workflow"
	M2BranchNotRecorded RuleCode = "M2.branch-not-recorded"
	M2AnchorMismatch    RuleCode = "M2.anchor-mismatch"
	M2HeadNotJudged     RuleCode = "M2.head-not-judged"
	M2Draft             RuleCode = "M2.draft"
	M2Hold              RuleCode = "M2.hold"
	M2ThreadsUnresolved RuleCode = "M2.threads-unresolved"

	M3Unread          RuleCode = "M3.unread"
	M3Stale           RuleCode = "M3.stale"
	M3VerdictWrongSHA RuleCode = "M3.verdict-wrong-sha"
	M3FixedNewFailure RuleCode = "M3.fixed-new-failure"
	M3NotBestVerdict  RuleCode = "M3.not-best-verdict"
	M3NoTargetJobs    RuleCode = "M3.no-target-jobs"
	M3TargetNotPassed RuleCode = "M3.target-not-passed"
	M3TargetRetried   RuleCode = "M3.target-retried"

	M4Unread           RuleCode = "M4.unread"
	M4Stale            RuleCode = "M4.stale"
	M4ExecChanging     RuleCode = "M4.exec-changing"
	M4DeltaNotEligible RuleCode = "M4.delta-not-eligible"
	M4DeltaNotForClass RuleCode = "M4.delta-not-for-class"

	M5Unread               RuleCode = "M5.unread"
	M5Stale                RuleCode = "M5.stale"
	M5DiffIncomplete       RuleCode = "M5.diff-incomplete"
	M5EmptyDiff            RuleCode = "M5.empty-diff"
	M5Unclassified         RuleCode = "M5.unclassified"
	M5StaticGates          RuleCode = "M5.static-gates-failed"
	M5WeakeningUndecidable RuleCode = "M5.weakening-undecidable"
	M5ClassNeverEligible   RuleCode = "M5.class-never-eligible"
	M5ClassNotEnabled      RuleCode = "M5.class-not-enabled"
	M5MixedClasses         RuleCode = "M5.mixed-classes"
	M5CINotForKind         RuleCode = "M5.ci-not-for-kind"
	M5DocsPure             RuleCode = "M5.MP-DOCSPURE"

	M6Unread             RuleCode = "M6.unread"
	M6Stale              RuleCode = "M6.stale"
	M6NoPipeline         RuleCode = "M6.no-candidate-pipeline"
	M6PipelineWrongSHA   RuleCode = "M6.pipeline-wrong-sha"
	M6PipelineNotSuccess RuleCode = "M6.pipeline-not-success"
	M6FailedAttempt      RuleCode = "M6.failed-attempt"
	M6ObservedOnce       RuleCode = "M6.observed-once"

	M7Unread      RuleCode = "M7.unread"
	M7Stale       RuleCode = "M7.stale"
	M7TargetMoved RuleCode = "M7.target-moved"

	M8Unread                RuleCode = "M8.unread"
	M8Stale                 RuleCode = "M8.stale"
	M8AgeUnobservable       RuleCode = "M8.approval-age-unobservable"
	M8ApprovalsInsufficient RuleCode = "M8.approvals-insufficient"

	M9Unread            RuleCode = "M9.unread"
	M9Stale             RuleCode = "M9.stale"
	M9Blocking          RuleCode = "M9.blocking"
	M9VerdictUnknown    RuleCode = "M9.verdict-unknown"
	M9ReviewsNotEnabled RuleCode = "M9.reviews-not-enabled"
	M9NoReviewFact      RuleCode = "M9.no-review-fact"
	M9SameModel         RuleCode = "M9.same-model"
	M9ModelUnknown      RuleCode = "M9.model-unknown"

	M10Unread       RuleCode = "M10.unread"
	M10Stale        RuleCode = "M10.stale"
	M10NotMergeable RuleCode = "M10.not-mergeable"

	M11Unread         RuleCode = "M11.unread"
	M11Stale          RuleCode = "M11.stale"
	M11RateCapReached RuleCode = "M11.rate-cap-reached"
)

var ruleOrder = []RuleCode{
	M1Unread, M1Stale, M1ModeOff, M1ModeUnknown, M1OperatorVeto, M1LayersDisagree, M1ConfigNotAtTip,
	M2Unread, M2Stale, M2KindUnknown, M2JudgedSHAInvalid, M2BotUnknown, M2NotBotAuthored,
	M2BranchNotWorkflow, M2BranchNotRecorded, M2AnchorMismatch, M2HeadNotJudged, M2Draft, M2Hold,
	M2ThreadsUnresolved,
	M3Unread, M3Stale, M3VerdictWrongSHA, M3FixedNewFailure, M3NotBestVerdict, M3NoTargetJobs,
	M3TargetNotPassed, M3TargetRetried,
	M4Unread, M4Stale, M4ExecChanging, M4DeltaNotEligible, M4DeltaNotForClass,
	M5Unread, M5Stale, M5DiffIncomplete, M5EmptyDiff, M5Unclassified, M5StaticGates,
	M5WeakeningUndecidable, M5ClassNeverEligible, M5ClassNotEnabled, M5MixedClasses, M5CINotForKind,
	M5DocsPure,
	M6Unread, M6Stale, M6NoPipeline, M6PipelineWrongSHA, M6PipelineNotSuccess, M6FailedAttempt,
	M6ObservedOnce,
	M7Unread, M7Stale, M7TargetMoved,
	M8Unread, M8Stale, M8AgeUnobservable, M8ApprovalsInsufficient,
	M9Unread, M9Stale, M9Blocking, M9VerdictUnknown, M9ReviewsNotEnabled, M9NoReviewFact,
	M9SameModel, M9ModelUnknown,
	M10Unread, M10Stale, M10NotMergeable,
	M11Unread, M11Stale, M11RateCapReached,
}

// Rules returns every refusal code, in evaluation order.
func Rules() []RuleCode { return append([]RuleCode(nil), ruleOrder...) }

// Row is the epic plan's row of the code: "M1" .. "M11"; "" for RuleMerge.
func (r RuleCode) Row() string {
	row, _, found := strings.Cut(string(r), ".")
	if !found {
		return ""
	}
	return row
}

// ARow maps a row of epic plan 5.1 to the row of fix plan 9.1 it carries
// ("M10" -> "A9"). M8 and M9 are both A8, the mode's human or review fact.
// M11, the rate cap, has no fix-plan row: "".
func ARow(mRow string) string {
	switch mRow {
	case "M1":
		return "A1"
	case "M2":
		return "A2"
	case "M3":
		return "A3"
	case "M4":
		return "A4"
	case "M5":
		return "A5"
	case "M6":
		return "A6"
	case "M7":
		return "A7"
	case "M8", "M9":
		return "A8"
	case "M10":
		return "A9"
	}
	return ""
}

// Failure is one fact that did not hold.
type Failure struct {
	Rule RuleCode
	// Text is the sentence for the log.
	Text string
	// Unread: the row's facts were not read, or were stale. Nothing was
	// decided about the merge request itself; look again.
	Unread bool
}

// Decision is Decide's answer.
type Decision struct {
	// Merge: every row held. False for the zero value.
	Merge bool
	// Rule is the ONE deciding code: RuleMerge when Merge, otherwise the
	// first failed row in the plan's order (Failed[0].Rule).
	Rule RuleCode
	// Text is the deciding rule's sentence.
	Text string
	// Failed lists EVERY fact that did not hold, in evaluation order. Empty
	// exactly when Merge.
	Failed []Failure
	// Mode is the mode the decision was made under.
	Mode Mode
	// SHA is the judged commit the merge call must be pinned to. Set only
	// when Merge; always a full 40-hex SHA then.
	SHA string
	// Class is the eligible class the diff matched. Set only when Merge.
	Class scope.Class
}

// FailedRules returns the codes of Failed, in order.
func (d Decision) FailedRules() []RuleCode {
	out := make([]RuleCode, 0, len(d.Failed))
	for _, f := range d.Failed {
		out = append(out, f.Rule)
	}
	return out
}
