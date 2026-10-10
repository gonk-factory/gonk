package changejudge

import (
	"fmt"
	"strings"
)

// Form is the syntactic form of a request to implement an issue. The closed
// set is the owner's decision 8.1 Q2. The caller matches the form; this
// package never looks at text.
type Form string

const (
	// FormLabel: the implement label on the issue. The Actor is the user of
	// the NEWEST `add` event of that label (resource label events), never
	// the issue's author and never "whoever": the label alone, with no event
	// read, is not a request.
	FormLabel Form = "label"
	// FormCommand: a slash-style command in a note on the issue. The Actor
	// is the note's author.
	FormCommand Form = "command"
	// FormMention: an at-mention of the bot asking for implementation, in a
	// note on the issue. The Actor is the note's author.
	FormMention Form = "mention"
)

// Forms returns the closed set of accepted request forms. The triage-verdict
// trigger of plan 2.1 is not one (8.1 Q2: "stays out of v1"), and neither is
// anything a configuration could name.
func Forms() []Form { return []Form{FormLabel, FormCommand, FormMention} }

// MinRequesterAccess is the lowest GitLab access level whose holder may
// request a change: Maintainer (owner decision 8.1 Q2, "ONLY from a project
// owner/maintainer"; tighter than the Developer the plan's 2.1 table says).
// It equals glab.AccessMaintainer; a test pins it.
const MinRequesterAccess = 40

// IssueStateOpened is GitLab's state of an open issue.
const IssueStateOpened = "opened"

// EligibleFacts is everything plan 2.1's predicate reads. Each group is
// filled by the controller from one source and carries a marker saying it was
// read; the zero value of a group is "not read".
type EligibleFacts struct {
	Config  ConfigFacts
	Trigger TriggerFacts
	Issue   IssueFacts
	Base    BaseFacts
	Dedup   DedupFacts
	Serial  SerialFacts
	Pinned  PinnedFacts
	// BaseReport is I8's input: the test report of the base pipeline. It is
	// read only when Config.RequireTestReport; Judge needs it either way.
	BaseReport Report
}

// ConfigFacts is the EFFECTIVE configuration after the instance / group /
// project fold.
type ConfigFacts struct {
	Loaded bool
	// Features: `actions.features` (I1).
	Features bool
	// MaxOpen: `change.max_open` (I6). Below 1 nothing is eligible.
	MaxOpen int
	// RequireTestReport: `change.require_test_report` (I8).
	RequireTestReport bool
}

// TriggerFacts is I2's input: EVERY request to implement this issue the
// controller found, in the caller's order (newest first is the convention;
// the order only chooses which refusal is reported, never eligibility).
type TriggerFacts struct {
	// Read: the issue's label events and notes were read, completely.
	Read     bool
	Requests []Request
}

// Request is one request, in one form, by one actor.
type Request struct {
	Form Form
	// LabelOnIssue: FormLabel only -- the issue carries the implement label
	// NOW. An add event for a label that was since removed is not a request.
	LabelOnIssue bool
	Actor        Actor
}

// Actor is who made a request, as GitLab says -- never as the request says.
type Actor struct {
	// Identified: the label event or the note was read and names its user.
	Identified bool
	UserID     int64
	// IsBot: the actor is gonk's own account.
	IsBot bool
	// AccessRead: the project's members were read and looked up for UserID.
	AccessRead bool
	// AccessLevel: the actor's access level on the project (glab.Access*);
	// 0 when the actor is not a member.
	AccessLevel int
}

// IssueFacts is I3's input.
type IssueFacts struct {
	Read bool
	// State: GitLab's issue state (`opened`, `closed`).
	State        string
	Confidential bool
	AuthorIsBot  bool
}

// BaseFacts is I4's input ("step 0"): the default branch head and EVERY
// pipeline listed for it. Eligible selects the pipelines of the head SHA
// itself; the caller does not filter.
type BaseFacts struct {
	Read bool
	// SHA: the default branch head now, which is the SHA the change would be
	// pinned to.
	SHA       string
	Pipelines []BasePipeline
}

// BasePipeline is one pipeline of the default branch.
type BasePipeline struct {
	ID     int64
	SHA    string
	Source string
	Status string
}

// DedupFacts is I5's input.
type DedupFacts struct {
	Read bool
	// ChangeState: the state of the change record for this anchor; "" when
	// there is none.
	ChangeState State
	// OpenAnchorMRs, ClosedUnmergedAnchorMRs, MergedAnchorMRs: bot-authored
	// merge requests whose metadata block carries this anchor.
	OpenAnchorMRs           int
	ClosedUnmergedAnchorMRs int
	MergedAnchorMRs         int
}

// SerialFacts is I6's input.
type SerialFacts struct {
	Read bool
	// OpenChangeMRs: open bot-authored change MRs on this project, any issue.
	OpenChangeMRs int
}

// PinnedFacts is I7's input: `.gonk.yml` as read at the PINNED base SHA.
type PinnedFacts struct {
	// Read: the file was fetched at Base.SHA (or found absent there). A fetch
	// error is not Read.
	Read bool
	// ConfigValid: it parses and validates.
	ConfigValid bool
	// ScopeFolded: the scope fold over it succeeded.
	ScopeFolded bool
}

// ReasonCode names the one predicate that refused a dispatch: "<row>.<what>".
type ReasonCode string

// Reason codes, in evaluation order. The part before the dot is the plan's
// row. A code ending in ".unread" means the row's facts were not read.
const (
	I1Unread             ReasonCode = "I1.unread"
	I1FeaturesOff        ReasonCode = "I1.features-off"
	I2Unread             ReasonCode = "I2.unread"
	I2NoRequest          ReasonCode = "I2.no-request"
	I2FormNotAccepted    ReasonCode = "I2.form-not-accepted"
	I2LabelNotOnIssue    ReasonCode = "I2.label-not-on-issue"
	I2ActorIsBot         ReasonCode = "I2.actor-is-bot"
	I2ActorCannotRequest ReasonCode = "I2.actor-cannot-request"
	I3Unread             ReasonCode = "I3.unread"
	I3NotOpen            ReasonCode = "I3.issue-not-open"
	I3Confidential       ReasonCode = "I3.issue-confidential"
	I3BotAuthored        ReasonCode = "I3.issue-bot-authored"
	I4Unread             ReasonCode = "I4.unread"
	I4NoPipeline         ReasonCode = "I4.base-no-pipeline"
	I4NotSettled         ReasonCode = "I4.base-not-settled"
	I4NotGreen           ReasonCode = "I4.base-not-green"
	I5Unread             ReasonCode = "I5.unread"
	I5ClosedUnmerged     ReasonCode = "I5.closed-unmerged"
	I5ChangeTerminal     ReasonCode = "I5.change-terminal"
	I5ChangeInFlight     ReasonCode = "I5.change-in-flight"
	I5AnchorMROpen       ReasonCode = "I5.anchor-mr-open"
	I5AnchorMRMerged     ReasonCode = "I5.anchor-mr-merged"
	I6Unread             ReasonCode = "I6.unread"
	I6MaxOpen            ReasonCode = "I6.max-open"
	I7Unread             ReasonCode = "I7.unread"
	I7ConfigInvalid      ReasonCode = "I7.config-invalid"
	I7ScopeFoldFailed    ReasonCode = "I7.scope-fold-failed"
	I8Unread             ReasonCode = "I8.unread"
	I8NoTestReport       ReasonCode = "I8.no-test-report"
)

// Reason is why a dispatch was refused. The zero value means "eligible".
type Reason struct {
	Code ReasonCode
	// Text is the sentence for the log and, when Note is set, for the note.
	Text string
	// Note: the plan marks this row N -- the reason is shown on the issue.
	// ("Once" is the caller's: one note per reason, by its marker.) False
	// for the silent rows.
	Note bool
	// NoVerifierPath: I4 with no pipeline at all, and I8 -- the project gives
	// gonk nothing to verify a change with. Decided for zero tokens.
	NoVerifierPath bool
	// Ours: I7 -- the fault is in configuration, not in the issue. No
	// dispatch and NO note blaming the issue.
	Ours bool
	// Unknown: the row could not be evaluated because its facts were not
	// read. Nothing is decided and nothing is noted; look again.
	Unknown bool
}

// Row is the plan's row of the reason: "I1" .. "I8"; "" when eligible.
func (r Reason) Row() string {
	row, _, _ := strings.Cut(string(r.Code), ".")
	return row
}

// Eligible is plan 2.1's predicate: may a `change` session be dispatched for
// this issue? All rows must hold, evaluated in the plan's order I1..I8; the
// FIRST one that fails is the reason, and a row whose facts were not read
// fails as Unknown at its place in that order (so "not opted in" is still
// answered silently when later facts are missing, and a missing fact never
// reads as eligibility).
//
// I2 is the owner's decision 8.1 Q2: a request in one of three forms, made by
// a project member with access >= Maintainer who is not the bot. ONE request
// that passes every check is enough, whatever the others are; when none
// passes, an unread request outranks a refused one (nothing is noted while a
// valid request may still be there), and otherwise the first refused request
// in the caller's order is the reason.
//
// true comes with the zero Reason.
func Eligible(f EligibleFacts) (bool, Reason) {
	silent := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text}
	}
	noted := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text, Note: true}
	}
	unread := func(code ReasonCode, text string) (bool, Reason) {
		return false, Reason{Code: code, Text: text, Unknown: true}
	}

	// I1: opted in.
	if !f.Config.Loaded {
		return unread(I1Unread, "the effective configuration was not loaded")
	}
	if !f.Config.Features {
		return silent(I1FeaturesOff, "actions.features is not enabled")
	}

	// I2: a request by somebody who may make it.
	if !f.Trigger.Read {
		return unread(I2Unread, "the issue's label events and notes were not read")
	}
	if len(f.Trigger.Requests) == 0 {
		return silent(I2NoRequest, "nobody asked for this issue to be implemented")
	}
	var refused, unknown *Reason
	requested := false
	for _, rq := range f.Trigger.Requests {
		why := requestReason(rq)
		switch {
		case why.Code == "":
			requested = true
		case why.Unknown && unknown == nil:
			unknown = &why
		case !why.Unknown && refused == nil:
			refused = &why
		}
	}
	if !requested && unknown != nil {
		return false, *unknown
	}
	if !requested {
		return false, *refused
	}

	// I3: an issue an MR may be opened for.
	if !f.Issue.Read || f.Issue.State == "" {
		return unread(I3Unread, "the issue was not read")
	}
	if f.Issue.State != IssueStateOpened {
		return noted(I3NotOpen, fmt.Sprintf("no change proposed: the issue is `%s`, not open", f.Issue.State))
	}
	if f.Issue.Confidential {
		return noted(I3Confidential, "no change proposed: the issue is confidential, and a merge request would publish it")
	}
	if f.Issue.AuthorIsBot {
		return noted(I3BotAuthored, "no change proposed: gonk does not implement an issue it filed itself")
	}

	// I4: step 0 -- the base is green.
	if !f.Base.Read || f.Base.SHA == "" {
		return unread(I4Unread, "the default branch head and its pipelines were not read")
	}
	var base []BasePipeline
	for _, p := range f.Base.Pipelines {
		if p.ID == 0 || p.SHA == "" || p.Source == "" || p.Status == "" {
			return unread(I4Unread, "a pipeline of the default branch without id, sha, source or status")
		}
		if p.SHA != f.Base.SHA {
			continue
		}
		if p.Source == SourceParentPipeline {
			continue
		}
		base = append(base, p)
	}
	if len(base) == 0 {
		return false, Reason{
			Code: I4NoPipeline, Note: true, NoVerifierPath: true,
			Text: "no change proposed: the default branch has no pipeline; gonk does not build on a base it cannot judge",
		}
	}
	for _, p := range base {
		if !settledStatus(p.Status) {
			return silent(I4NotSettled, fmt.Sprintf("pipeline %d of the default branch head has not finished (`%s`); waiting for it", p.ID, p.Status))
		}
	}
	for _, p := range base {
		if p.Status != StatusSuccess {
			return noted(I4NotGreen, fmt.Sprintf(
				"no change proposed: the default branch is red (pipeline %d is `%s`); gonk does not build on a base it cannot judge", p.ID, p.Status))
		}
	}

	// I5: dedup.
	if !f.Dedup.Read || (f.Dedup.ChangeState != "" && !KnownState(f.Dedup.ChangeState)) {
		return unread(I5Unread, "the change record or the merge requests of this issue were not read")
	}
	closedText := "no change proposed: a change merge request for this issue was closed without being merged, and gonk does not propose it again"
	if f.Dedup.ChangeState == StateClosedUnmerged {
		return noted(I5ClosedUnmerged, closedText)
	}
	if f.Dedup.ChangeState != "" && f.Dedup.ChangeState.Terminal() {
		return silent(I5ChangeTerminal, fmt.Sprintf("a change for this issue already ended in state %q", f.Dedup.ChangeState))
	}
	if f.Dedup.ChangeState != "" {
		return silent(I5ChangeInFlight, fmt.Sprintf("a change for this issue is in flight (state %q)", f.Dedup.ChangeState))
	}
	if f.Dedup.ClosedUnmergedAnchorMRs != 0 {
		return noted(I5ClosedUnmerged, closedText)
	}
	if f.Dedup.OpenAnchorMRs != 0 {
		return silent(I5AnchorMROpen, "a change merge request for this issue is already open")
	}
	if f.Dedup.MergedAnchorMRs != 0 {
		return silent(I5AnchorMRMerged, "a change merge request for this issue was already merged")
	}

	// I6: serial.
	if !f.Serial.Read {
		return unread(I6Unread, "the project's open change merge requests were not read")
	}
	if f.Serial.OpenChangeMRs >= f.Config.MaxOpen {
		return silent(I6MaxOpen, fmt.Sprintf("%d change merge requests are open on this project (max_open %d)",
			f.Serial.OpenChangeMRs, f.Config.MaxOpen))
	}

	// I7: the configuration at the pinned base. OURS.
	if !f.Pinned.Read {
		return unread(I7Unread, ".gonk.yml at the pinned base SHA was not read")
	}
	if !f.Pinned.ConfigValid {
		return false, Reason{Code: I7ConfigInvalid, Ours: true, Text: ".gonk.yml at the pinned base SHA is not valid"}
	}
	if !f.Pinned.ScopeFolded {
		return false, Reason{Code: I7ScopeFoldFailed, Ours: true, Text: "the scope fold over .gonk.yml at the pinned base SHA failed"}
	}

	// I8: a test report, where the project requires one.
	if f.Config.RequireTestReport {
		if !f.BaseReport.wellFormed() {
			return unread(I8Unread, "the base pipeline's test report was not read")
		}
		if f.BaseReport.Total == 0 {
			return false, Reason{
				Code: I8NoTestReport, Note: true, NoVerifierPath: true,
				Text: "no change proposed: change.require_test_report is set and the default branch's pipeline publishes no test report",
			}
		}
	}
	return true, Reason{}
}

// requestReason is I2 for one request: the zero Reason when it is a request
// by somebody who may make it.
func requestReason(rq Request) Reason {
	if !knownForm(rq.Form) {
		return Reason{Code: I2FormNotAccepted, Text: fmt.Sprintf("a request of form %q is not one gonk accepts", rq.Form)}
	}
	if rq.Form == FormLabel && !rq.LabelOnIssue {
		return Reason{Code: I2LabelNotOnIssue, Text: "the implement label is no longer on the issue"}
	}
	if !rq.Actor.Identified || rq.Actor.UserID == 0 {
		return Reason{Code: I2Unread, Unknown: true, Text: fmt.Sprintf("who made the `%s` request was not read", rq.Form)}
	}
	if rq.Actor.IsBot {
		return Reason{Code: I2ActorIsBot, Text: fmt.Sprintf("the `%s` request was made by gonk itself", rq.Form)}
	}
	if !rq.Actor.AccessRead {
		return Reason{Code: I2Unread, Unknown: true, Text: fmt.Sprintf("the access level of who made the `%s` request was not read", rq.Form)}
	}
	if rq.Actor.AccessLevel < MinRequesterAccess {
		return Reason{Code: I2ActorCannotRequest, Note: true,
			Text: fmt.Sprintf("no change proposed: the `%s` request was made by someone who cannot request this (a project maintainer or owner can)", rq.Form)}
	}
	return Reason{}
}

func knownForm(f Form) bool {
	for _, k := range Forms() {
		if k == f {
			return true
		}
	}
	return false
}
