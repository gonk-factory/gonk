package changejudge

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Every fixture in this package is INVENTED (plan section 0): a project that
// counts crates, with jobs compile / unit-tests / lint, and an issue asking
// for a wrong tally to be corrected.

const (
	shaBase   = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	shaChange = "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4"
	shaOther  = "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d"

	userMaintainer = int64(4101)
	userDeveloper  = int64(4102)
	userStranger   = int64(4103)
	userBot        = int64(4199)
)

// by is an identified actor at an access level.
func by(user int64, level int) Actor {
	return Actor{Identified: true, UserID: user, AccessRead: true, AccessLevel: level}
}

func labelBy(a Actor) Request { return Request{Form: FormLabel, LabelOnIssue: true, Actor: a} }

// baseCases: the report the default branch's pipeline published.
func baseCases() []Case {
	return []Case{
		{Suite: "unit", Classname: "crates.test_tally", Name: "test_empty", Status: CaseSuccess},
		{Suite: "unit", Classname: "crates.test_tally", Name: "test_one_crate", Status: CaseSuccess},
		{Suite: "unit", Classname: "crates.test_label", Name: "test_label_width", Status: CaseSuccess},
	}
}

func report(cases ...Case) Report {
	return Report{Read: true, Total: len(cases), Cases: cases}
}

// eligibleFacts is an issue for which every row of 2.1 holds: a maintainer
// added the implement label.
func eligibleFacts() EligibleFacts {
	return EligibleFacts{
		Config:  ConfigFacts{Loaded: true, Features: true, MaxOpen: 2, RequireTestReport: true},
		Trigger: TriggerFacts{Read: true, Requests: []Request{labelBy(by(userMaintainer, glab.AccessMaintainer))}},
		Issue:   IssueFacts{Read: true, State: IssueStateOpened},
		Base: BaseFacts{Read: true, SHA: shaBase, Pipelines: []BasePipeline{
			{ID: 300, SHA: shaBase, Source: "push", Status: StatusSuccess},
			{ID: 290, SHA: shaOther, Source: "push", Status: StatusFailed}, // an older commit: not the base
		}},
		Dedup:      DedupFacts{Read: true},
		Serial:     SerialFacts{Read: true, OpenChangeMRs: 1},
		Pinned:     PinnedFacts{Read: true, ConfigValid: true, ScopeFolded: true},
		BaseReport: report(baseCases()...),
	}
}

func TestTheBaselineIssueIsEligible(t *testing.T) {
	ok, why := Eligible(eligibleFacts())
	if !ok || !reflect.DeepEqual(why, Reason{}) {
		t.Fatalf("Eligible(baseline) = %v, %+v; want true and the zero Reason", ok, why)
	}
	if why.Row() != "" {
		t.Errorf("Row() of the zero Reason = %q, want \"\"", why.Row())
	}
}

type eligibleCase struct {
	name string
	row  string
	code ReasonCode
	mut  func(*EligibleFacts)
	// what the plan's "On failure" column says
	note, noVerifier, ours bool
	text                   string // a substring, when the plan gives the words
}

func oneRequest(rq Request) func(*EligibleFacts) {
	return func(f *EligibleFacts) { f.Trigger.Requests = []Request{rq} }
}

// eligibleRows: one fixture per predicate of every row I1-I8. Each is the
// eligible baseline with ONE fact changed, so it fails exactly one predicate,
// and the test asserts WHICH by code.
var eligibleRows = []eligibleCase{
	{name: "I1 actions.features is off", row: "I1", code: I1FeaturesOff,
		mut: func(f *EligibleFacts) { f.Config.Features = false }},

	{name: "I2 nobody asked", row: "I2", code: I2NoRequest,
		mut: func(f *EligibleFacts) { f.Trigger.Requests = nil }},
	{name: "I2 the triage verdict is not a request form", row: "I2", code: I2FormNotAccepted,
		mut: oneRequest(Request{Form: "triage-verdict", Actor: by(userMaintainer, glab.AccessMaintainer)})},
	{name: "I2 a request with no form", row: "I2", code: I2FormNotAccepted,
		mut: oneRequest(Request{Actor: by(userMaintainer, glab.AccessOwner)})},
	{name: "I2 the label was added by a maintainer and has since been removed", row: "I2", code: I2LabelNotOnIssue,
		mut: oneRequest(Request{Form: FormLabel, LabelOnIssue: false, Actor: by(userMaintainer, glab.AccessMaintainer)})},
	{name: "I2 the label was added by the bot", row: "I2", code: I2ActorIsBot,
		mut: oneRequest(labelBy(Actor{Identified: true, UserID: userBot, IsBot: true, AccessRead: true, AccessLevel: glab.AccessMaintainer}))},
	{name: "I2 the label was added by a developer", row: "I2", code: I2ActorCannotRequest, note: true,
		text: "someone who cannot request this",
		mut:  oneRequest(labelBy(by(userDeveloper, glab.AccessDeveloper)))},
	{name: "I2 the label was added by a reporter", row: "I2", code: I2ActorCannotRequest, note: true,
		mut: oneRequest(labelBy(by(userDeveloper, glab.AccessReporter)))},
	{name: "I2 the label was added by somebody who is not a member", row: "I2", code: I2ActorCannotRequest, note: true,
		mut: oneRequest(labelBy(by(userStranger, 0)))},
	{name: "I2 a slash command by a developer", row: "I2", code: I2ActorCannotRequest, note: true,
		text: "`command` request",
		mut:  oneRequest(Request{Form: FormCommand, Actor: by(userDeveloper, glab.AccessDeveloper)})},
	{name: "I2 an at-mention by somebody who is not a member", row: "I2", code: I2ActorCannotRequest, note: true,
		text: "`mention` request",
		mut:  oneRequest(Request{Form: FormMention, Actor: by(userStranger, 0)})},
	{name: "I2 a slash command by the bot", row: "I2", code: I2ActorIsBot,
		mut: oneRequest(Request{Form: FormCommand, Actor: Actor{Identified: true, UserID: userBot, IsBot: true, AccessRead: true, AccessLevel: glab.AccessOwner}})},

	{name: "I3 the issue is closed", row: "I3", code: I3NotOpen, note: true,
		mut: func(f *EligibleFacts) { f.Issue.State = "closed" }},
	{name: "I3 the issue is confidential", row: "I3", code: I3Confidential, note: true,
		mut: func(f *EligibleFacts) { f.Issue.Confidential = true }},
	{name: "I3 the bot filed the issue", row: "I3", code: I3BotAuthored, note: true,
		mut: func(f *EligibleFacts) { f.Issue.AuthorIsBot = true }},

	{name: "I4 the default branch has no pipeline at all", row: "I4", code: I4NoPipeline, note: true, noVerifier: true,
		text: "gonk does not build on a base it cannot judge",
		mut:  func(f *EligibleFacts) { f.Base.Pipelines = nil }},
	{name: "I4 the only pipelines are of older commits", row: "I4", code: I4NoPipeline, note: true, noVerifier: true,
		mut: func(f *EligibleFacts) { f.Base.Pipelines = f.Base.Pipelines[1:] }},
	{name: "I4 the head's only pipeline is a child pipeline", row: "I4", code: I4NoPipeline, note: true, noVerifier: true,
		mut: func(f *EligibleFacts) { f.Base.Pipelines[0].Source = SourceParentPipeline }},
	{name: "I4 the base pipeline is still running", row: "I4", code: I4NotSettled,
		mut: func(f *EligibleFacts) { f.Base.Pipelines[0].Status = "running" }},
	{name: "I4 the base pipeline failed", row: "I4", code: I4NotGreen, note: true,
		text: "the default branch is red",
		mut:  func(f *EligibleFacts) { f.Base.Pipelines[0].Status = StatusFailed }},
	{name: "I4 the base pipeline was canceled", row: "I4", code: I4NotGreen, note: true,
		mut: func(f *EligibleFacts) { f.Base.Pipelines[0].Status = StatusCanceled }},
	{name: "I4 the base pipeline is blocked on a manual job", row: "I4", code: I4NotGreen, note: true,
		mut: func(f *EligibleFacts) { f.Base.Pipelines[0].Status = StatusManual }},
	{name: "I4 one of two pipelines of the head failed", row: "I4", code: I4NotGreen, note: true,
		mut: func(f *EligibleFacts) {
			f.Base.Pipelines = append(f.Base.Pipelines, BasePipeline{ID: 301, SHA: shaBase, Source: "schedule", Status: StatusFailed})
		}},

	{name: "I5 a change for this issue is in flight", row: "I5", code: I5ChangeInFlight,
		mut: func(f *EligibleFacts) { f.Dedup.ChangeState = StateVerifying }},
	{name: "I5 a change for this issue was not verified and waits for its next attempt", row: "I5", code: I5ChangeInFlight,
		mut: func(f *EligibleFacts) { f.Dedup.ChangeState = StateNotVerified }},
	{name: "I5 a change for this issue ended verified", row: "I5", code: I5ChangeTerminal,
		mut: func(f *EligibleFacts) { f.Dedup.ChangeState = StateVerified }},
	{name: "I5 a change for this issue ended inconclusive", row: "I5", code: I5ChangeTerminal,
		mut: func(f *EligibleFacts) { f.Dedup.ChangeState = StateInconclusive }},
	{name: "I5 the record says a maintainer closed the change MR", row: "I5", code: I5ClosedUnmerged, note: true,
		text: "closed without being merged",
		mut:  func(f *EligibleFacts) { f.Dedup.ChangeState = StateClosedUnmerged }},
	{name: "I5 a closed, unmerged MR carries this anchor", row: "I5", code: I5ClosedUnmerged, note: true,
		mut: func(f *EligibleFacts) { f.Dedup.ClosedUnmergedAnchorMRs = 1 }},
	{name: "I5 an open MR carries this anchor", row: "I5", code: I5AnchorMROpen,
		mut: func(f *EligibleFacts) { f.Dedup.OpenAnchorMRs = 1 }},
	{name: "I5 a merged MR carries this anchor", row: "I5", code: I5AnchorMRMerged,
		mut: func(f *EligibleFacts) { f.Dedup.MergedAnchorMRs = 1 }},

	{name: "I6 max_open change MRs are open", row: "I6", code: I6MaxOpen,
		mut: func(f *EligibleFacts) { f.Serial.OpenChangeMRs = 2 }},
	{name: "I6 max_open is zero", row: "I6", code: I6MaxOpen,
		mut: func(f *EligibleFacts) { f.Config.MaxOpen = 0; f.Serial.OpenChangeMRs = 0 }},

	{name: "I7 the config at the pinned base is not valid", row: "I7", code: I7ConfigInvalid, ours: true,
		mut: func(f *EligibleFacts) { f.Pinned.ConfigValid = false }},
	{name: "I7 the scope fold failed", row: "I7", code: I7ScopeFoldFailed, ours: true,
		mut: func(f *EligibleFacts) { f.Pinned.ScopeFolded = false }},

	{name: "I8 a test report is required and the base published none", row: "I8", code: I8NoTestReport, note: true, noVerifier: true,
		mut: func(f *EligibleFacts) { f.BaseReport = report() }},
}

func TestEligibleRows(t *testing.T) {
	for _, c := range eligibleRows {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			c.mut(&f)
			ok, why := Eligible(f)
			if ok {
				t.Fatalf("eligible; want refused by %s", c.code)
			}
			if why.Code != c.code {
				t.Fatalf("refused by %q (%s); want %q", why.Code, why.Text, c.code)
			}
			if why.Row() != c.row {
				t.Errorf("Row() = %q, want %q", why.Row(), c.row)
			}
			if why.Unknown {
				t.Errorf("Unknown is set: a row that was EVALUATED is a decision, not a missing fact")
			}
			if why.Note != c.note || why.NoVerifierPath != c.noVerifier || why.Ours != c.ours {
				t.Errorf("note=%v no-verifier-path=%v ours=%v; want %v %v %v",
					why.Note, why.NoVerifierPath, why.Ours, c.note, c.noVerifier, c.ours)
			}
			if why.Text == "" {
				t.Errorf("no reason text")
			}
			if c.text != "" && !strings.Contains(why.Text, c.text) {
				t.Errorf("text = %q, want it to say %q", why.Text, c.text)
			}
		})
	}
}

// Owner decision 8.1 Q2: each of the three forms is a request when a project
// maintainer or owner made it -- and only then.
func TestTheThreeTriggerFormsFromAMaintainerOrOwner(t *testing.T) {
	levels := []struct {
		name  string
		level int
		want  bool
	}{
		{"not a member", 0, false},
		{"guest", glab.AccessGuest, false},
		{"reporter", glab.AccessReporter, false},
		{"developer", glab.AccessDeveloper, false},
		{"one below maintainer", glab.AccessMaintainer - 1, false},
		{"maintainer", glab.AccessMaintainer, true},
		{"owner", glab.AccessOwner, true},
	}
	if !reflect.DeepEqual(Forms(), []Form{FormLabel, FormCommand, FormMention}) {
		t.Fatalf("Forms() = %v; the owner decided three: label, command, mention", Forms())
	}
	for _, form := range Forms() {
		for _, l := range levels {
			t.Run(string(form)+"/"+l.name, func(t *testing.T) {
				f := eligibleFacts()
				f.Trigger.Requests = []Request{{Form: form, LabelOnIssue: form == FormLabel, Actor: by(userDeveloper, l.level)}}
				ok, why := Eligible(f)
				if ok != l.want {
					t.Fatalf("eligible = %v (%+v), want %v", ok, why, l.want)
				}
				if !ok && why.Code != I2ActorCannotRequest {
					t.Errorf("refused by %q, want %q", why.Code, I2ActorCannotRequest)
				}
			})
		}
	}
}

func TestTheRequesterFloorIsGitLabsMaintainer(t *testing.T) {
	if MinRequesterAccess != glab.AccessMaintainer {
		t.Fatalf("MinRequesterAccess = %d, glab.AccessMaintainer = %d", MinRequesterAccess, glab.AccessMaintainer)
	}
}

// One valid request is enough; the others, whatever they are, neither help
// nor hurt. And when none is valid, an unread one means "look again" rather
// than a note.
func TestSeveralRequests(t *testing.T) {
	good := Request{Form: FormCommand, Actor: by(userMaintainer, glab.AccessMaintainer)}
	stranger := labelBy(by(userStranger, 0))
	bot := labelBy(Actor{Identified: true, UserID: userBot, IsBot: true, AccessRead: true, AccessLevel: glab.AccessOwner})
	unreadActor := Request{Form: FormMention, Actor: Actor{}}
	unreadAccess := Request{Form: FormMention, Actor: Actor{Identified: true, UserID: userDeveloper}}
	cases := []struct {
		name     string
		requests []Request
		ok       bool
		code     ReasonCode
		unknown  bool
	}{
		{"a stranger's label, then a maintainer's command", []Request{stranger, good}, true, "", false},
		{"a maintainer's command, then a stranger's label", []Request{good, stranger}, true, "", false},
		{"an unread request beside a valid one", []Request{unreadActor, good}, true, "", false},
		{"the bot's label beside a valid command", []Request{bot, good}, true, "", false},
		{"a stranger's label and the bot's: the first is the reason", []Request{stranger, bot}, false, I2ActorCannotRequest, false},
		{"the bot's label and a stranger's: the first is the reason", []Request{bot, stranger}, false, I2ActorIsBot, false},
		{"a stranger's label and an unread mention: nothing is decided yet", []Request{stranger, unreadActor}, false, I2Unread, true},
		{"an unread mention and a stranger's label: nothing is decided yet", []Request{unreadActor, stranger}, false, I2Unread, true},
		{"a stranger's label and a mention whose author's access is unread", []Request{stranger, unreadAccess}, false, I2Unread, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			f.Trigger.Requests = c.requests
			ok, why := Eligible(f)
			if ok != c.ok || why.Code != c.code || why.Unknown != c.unknown {
				t.Fatalf("Eligible = %v, code %q, unknown %v; want %v, %q, %v", ok, why.Code, why.Unknown, c.ok, c.code, c.unknown)
			}
			if c.unknown && why.Note {
				t.Errorf("an unread request wrote a note")
			}
		})
	}
}

// The plan's test for this part: "a red base dispatches" must be seen red.
// Step 0 is not optional: nothing is built on a base gonk cannot judge.
func TestARedBaseIsNotDispatched(t *testing.T) {
	for _, status := range []string{StatusFailed, StatusCanceled, StatusSkipped, StatusManual} {
		t.Run(status, func(t *testing.T) {
			f := eligibleFacts()
			f.Base.Pipelines[0].Status = status
			ok, why := Eligible(f)
			if ok {
				t.Fatalf("a base whose pipeline is %q was dispatched", status)
			}
			if why.Code != I4NotGreen || !why.Note || why.Unknown {
				t.Fatalf("refused by %+v; want %s, noted", why, I4NotGreen)
			}
		})
	}
	t.Run("the newest pipeline of the head is green and an older one of the same commit is red", func(t *testing.T) {
		f := eligibleFacts()
		f.Base.Pipelines = []BasePipeline{
			{ID: 301, SHA: shaBase, Source: "push", Status: StatusSuccess},
			{ID: 300, SHA: shaBase, Source: "push", Status: StatusFailed},
		}
		if ok, why := Eligible(f); ok || why.Code != I4NotGreen {
			t.Fatalf("Eligible = %v, %+v; a commit that was red once is not a base gonk can judge by one sample", ok, why)
		}
	})
	t.Run("a green pipeline of an OLDER commit is not the head's", func(t *testing.T) {
		f := eligibleFacts()
		f.Base.Pipelines = []BasePipeline{{ID: 290, SHA: shaOther, Source: "push", Status: StatusSuccess}}
		if ok, why := Eligible(f); ok || why.Code != I4NoPipeline {
			t.Fatalf("Eligible = %v, %+v; want %s", ok, why, I4NoPipeline)
		}
	})
	t.Run("a red pipeline of an older commit does not hold a green head", func(t *testing.T) {
		if ok, why := Eligible(eligibleFacts()); !ok {
			t.Fatalf("refused by %+v", why)
		}
	})
}

// A row whose facts were not read refuses as Unknown: nothing decided,
// nothing noted, and never eligible.
func TestAnUnreadFactIsNeverEligibility(t *testing.T) {
	cases := []struct {
		name string
		code ReasonCode
		mut  func(*EligibleFacts)
	}{
		{"the config was not loaded", I1Unread, func(f *EligibleFacts) { f.Config.Loaded = false }},
		{"the label events and notes were not read", I2Unread, func(f *EligibleFacts) { f.Trigger.Read = false }},
		{"the label is on the issue and no event was read (the label alone)", I2Unread,
			oneRequest(Request{Form: FormLabel, LabelOnIssue: true})},
		{"no event was read, though a user and an access level are filled in", I2Unread,
			oneRequest(labelBy(Actor{UserID: userMaintainer, AccessRead: true, AccessLevel: glab.AccessOwner}))},
		{"the event names no user", I2Unread,
			oneRequest(labelBy(Actor{Identified: true, AccessRead: true, AccessLevel: glab.AccessOwner}))},
		{"the actor's access was not read", I2Unread,
			oneRequest(labelBy(Actor{Identified: true, UserID: userMaintainer, AccessLevel: glab.AccessOwner}))},
		{"the issue was not read", I3Unread, func(f *EligibleFacts) { f.Issue.Read = false }},
		{"the issue has no state", I3Unread, func(f *EligibleFacts) { f.Issue.State = "" }},
		{"the base was not read", I4Unread, func(f *EligibleFacts) { f.Base.Read = false }},
		{"the base SHA is empty", I4Unread, func(f *EligibleFacts) { f.Base.SHA = "" }},
		{"the base SHA is empty and so is a pipeline's (equal, and not a fact)", I4Unread, func(f *EligibleFacts) {
			f.Base.SHA = ""
			f.Base.Pipelines = []BasePipeline{{ID: 300, Source: "push", Status: StatusSuccess}}
		}},
		{"a base pipeline has no status", I4Unread, func(f *EligibleFacts) { f.Base.Pipelines[0].Status = "" }},
		{"a base pipeline has no id", I4Unread, func(f *EligibleFacts) { f.Base.Pipelines[1].ID = 0 }},
		{"a base pipeline has no SHA", I4Unread, func(f *EligibleFacts) { f.Base.Pipelines[0].SHA = "" }},
		{"a base pipeline has no source", I4Unread, func(f *EligibleFacts) { f.Base.Pipelines[0].Source = "" }},
		{"the change record and MRs were not read", I5Unread, func(f *EligibleFacts) { f.Dedup.Read = false }},
		{"the change record holds a state nobody defined", I5Unread, func(f *EligibleFacts) { f.Dedup.ChangeState = "half-done" }},
		{"the open change MRs were not read", I6Unread, func(f *EligibleFacts) { f.Serial.Read = false }},
		{"the pinned config was not read", I7Unread, func(f *EligibleFacts) { f.Pinned.Read = false }},
		{"a report is required and the base report was not read", I8Unread, func(f *EligibleFacts) { f.BaseReport = Report{} }},
		{"a report is required and its cases do not add up to its total", I8Unread, func(f *EligibleFacts) { f.BaseReport.Total = 7 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			c.mut(&f)
			ok, why := Eligible(f)
			if ok {
				t.Fatalf("eligible with a fact unread")
			}
			if why.Code != c.code || !why.Unknown {
				t.Fatalf("got %q unknown=%v, want %q unknown=true", why.Code, why.Unknown, c.code)
			}
			if why.Note || why.NoVerifierPath || why.Ours {
				t.Errorf("an unread fact wrote something: %+v", why)
			}
		})
	}
	t.Run("the zero value", func(t *testing.T) {
		ok, why := Eligible(EligibleFacts{})
		if ok || !why.Unknown || why.Code != I1Unread {
			t.Fatalf("Eligible(zero) = %v, %+v; want refused, unknown, %s", ok, why, I1Unread)
		}
	})
}

// The plan's order IS the contract ("first failure wins"): with every row
// broken at once, the reason is I1's; mend the rows one at a time and the
// reason walks down the table.
func TestTheFirstFailingRowIsTheReason(t *testing.T) {
	breaks := []struct {
		code ReasonCode
		mut  func(*EligibleFacts)
	}{
		{I1FeaturesOff, func(f *EligibleFacts) { f.Config.Features = false }},
		{I2ActorCannotRequest, oneRequest(labelBy(by(userStranger, 0)))},
		{I3Confidential, func(f *EligibleFacts) { f.Issue.Confidential = true }},
		{I4NotGreen, func(f *EligibleFacts) { f.Base.Pipelines[0].Status = StatusFailed }},
		{I5AnchorMROpen, func(f *EligibleFacts) { f.Dedup.OpenAnchorMRs = 1 }},
		{I6MaxOpen, func(f *EligibleFacts) { f.Serial.OpenChangeMRs = 9 }},
		{I7ConfigInvalid, func(f *EligibleFacts) { f.Pinned.ConfigValid = false }},
		{I8NoTestReport, func(f *EligibleFacts) { f.BaseReport = report() }},
	}
	for from := range breaks {
		f := eligibleFacts()
		for _, b := range breaks[from:] {
			b.mut(&f)
		}
		ok, why := Eligible(f)
		if ok || why.Code != breaks[from].code {
			t.Errorf("rows from %s on broken: Eligible = %v, %q; want %q", breaks[from].code, ok, why.Code, breaks[from].code)
		}
	}
	// And inside a row, the order of its predicates.
	inside := []struct {
		name string
		code ReasonCode
		mut  func(*EligibleFacts)
	}{
		{"I3: closed outranks confidential", I3NotOpen, func(f *EligibleFacts) { f.Issue.State = "closed"; f.Issue.Confidential = true }},
		{"I3: confidential outranks bot-authored", I3Confidential, func(f *EligibleFacts) { f.Issue.Confidential = true; f.Issue.AuthorIsBot = true }},
		{"I4: a running pipeline outranks a red one (the red is not noted until the base is whole)", I4NotSettled, func(f *EligibleFacts) {
			f.Base.Pipelines = []BasePipeline{
				{ID: 300, SHA: shaBase, Source: "push", Status: StatusFailed},
				{ID: 301, SHA: shaBase, Source: "push", Status: "running"},
			}
		}},
		{"I5: a closed-unmerged record outranks an open MR", I5ClosedUnmerged, func(f *EligibleFacts) {
			f.Dedup.ChangeState = StateClosedUnmerged
			f.Dedup.OpenAnchorMRs = 1
		}},
		{"I5: a record outranks the MRs", I5ChangeInFlight, func(f *EligibleFacts) {
			f.Dedup.ChangeState = StateProposing
			f.Dedup.ClosedUnmergedAnchorMRs = 1
		}},
		{"I5: closed-unmerged outranks open", I5ClosedUnmerged, func(f *EligibleFacts) {
			f.Dedup.ClosedUnmergedAnchorMRs = 1
			f.Dedup.OpenAnchorMRs = 1
		}},
		{"I5: open outranks merged", I5AnchorMROpen, func(f *EligibleFacts) { f.Dedup.OpenAnchorMRs = 1; f.Dedup.MergedAnchorMRs = 1 }},
		{"I7: an invalid config outranks a failed fold", I7ConfigInvalid, func(f *EligibleFacts) { f.Pinned.ConfigValid = false; f.Pinned.ScopeFolded = false }},
		{"I2: the bot is refused before its access is asked for", I2ActorIsBot,
			oneRequest(labelBy(Actor{Identified: true, UserID: userBot, IsBot: true}))},
		{"I2: a removed label is refused before its actor is asked for", I2LabelNotOnIssue,
			oneRequest(Request{Form: FormLabel})},
	}
	for _, c := range inside {
		t.Run(c.name, func(t *testing.T) {
			f := eligibleFacts()
			c.mut(&f)
			if ok, why := Eligible(f); ok || why.Code != c.code {
				t.Fatalf("Eligible = %v, %q; want %q", ok, why.Code, c.code)
			}
		})
	}
}

// An unread fact of a LATER row does not hide an earlier decision: a project
// that has not opted in is answered silently even when nothing else was read.
func TestAnUnreadFactDoesNotHideAnEarlierDecision(t *testing.T) {
	f := EligibleFacts{Config: ConfigFacts{Loaded: true}}
	ok, why := Eligible(f)
	if ok || why.Code != I1FeaturesOff || why.Unknown {
		t.Fatalf("Eligible = %v, %+v; want %s, decided", ok, why, I1FeaturesOff)
	}
}

// I8 reads the report only when the project requires one.
func TestTheBaseReportIsOnlyRequiredWhenConfigured(t *testing.T) {
	for _, r := range []Report{{}, report(), {Read: true, Total: 3}} {
		f := eligibleFacts()
		f.Config.RequireTestReport = false
		f.BaseReport = r
		if ok, why := Eligible(f); !ok {
			t.Errorf("require_test_report off, base report %+v: refused by %+v", r, why)
		}
	}
}

func TestMaxOpenIsAStrictBound(t *testing.T) {
	for open := 0; open <= 3; open++ {
		f := eligibleFacts()
		f.Config.MaxOpen = 2
		f.Serial.OpenChangeMRs = open
		ok, why := Eligible(f)
		if want := open < 2; ok != want {
			t.Errorf("%d open, max_open 2: eligible = %v (%q), want %v", open, ok, why.Code, want)
		}
	}
}

func TestEligibleDoesNotModifyItsFacts(t *testing.T) {
	f, g := eligibleFacts(), eligibleFacts()
	Eligible(f)
	if !reflect.DeepEqual(f, g) {
		t.Fatalf("Eligible modified its facts")
	}
}

// The path classes Judge names are scope's.
func TestTheClassesAreScopes(t *testing.T) {
	if ClassSource != string(scope.ClassSource) || ClassTest != string(scope.ClassTest) || ClassDocs != string(scope.ClassDocs) {
		t.Fatalf("classes %q %q %q are not scope's %q %q %q", ClassSource, ClassTest, ClassDocs,
			scope.ClassSource, scope.ClassTest, scope.ClassDocs)
	}
}
