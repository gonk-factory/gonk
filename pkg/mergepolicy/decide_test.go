package mergepolicy

import (
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Every fixture in this package is INVENTED (plan section 0): a project that
// counts crates, with a job `unit-tests`, a bot with user id 900, and two
// people, 41 (a maintainer) and 42 (a developer).

const (
	shaJudged = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	shaBase   = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"
	shaOther  = "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d"

	botID       = int64(900)
	maintainer  = int64(41)
	developer   = int64(42)
	reporterID  = int64(43)
	fixBranch   = "gonk/pipefix-0123456789ab"
	chgBranch   = "gonk/change-0123456789ab"
	fixAnchor   = "gonk:75:pipeline:ref:main:aaaaaaaaaaaa"
	modelAuthor = "crate-coder-30b"
	modelReview = "crate-reader-70b"
)

var (
	now  = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	read = now.Add(-5 * time.Second)
)

var classifier = scope.Effective{}.Classifier(scope.BaseFacts{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

// edited is an existing regular file, modified, classified by the real
// classifier under the default profiles (go, docs-md).
func edited(p string) ChangedPath {
	return ChangedPath{Decision: classifier.Classify(p), OldMode: FileModeRegular, NewMode: FileModeRegular}
}

// created is a new regular file.
func created(p string) ChangedPath {
	return ChangedPath{Decision: classifier.Classify(p), Created: true, OldMode: "0", NewMode: FileModeRegular}
}

// scenario is one (mode, kind, class) combination that CAN merge.
type scenario struct {
	mode  Mode
	kind  Kind
	class scope.Class
}

func (s scenario) String() string {
	return fmt.Sprintf("%s/%s/%s", s.mode, s.kind, s.class)
}

// scenarios is every combination plan 5.2 x 5.3 makes eligible: the `ci` row
// applies to pipefix MRs only.
var scenarios = []scenario{
	{ModeAfterHumanApproval, KindPipefix, scope.ClassCI},
	{ModeAfterHumanApproval, KindPipefix, scope.ClassDocs},
	{ModeAfterHumanApproval, KindChange, scope.ClassDocs},
	{ModeAfterAgenticReview, KindPipefix, scope.ClassCI},
	{ModeAfterAgenticReview, KindPipefix, scope.ClassDocs},
	{ModeAfterAgenticReview, KindChange, scope.ClassDocs},
}

func human(s scenario) bool   { return s.mode == ModeAfterHumanApproval }
func agentic(s scenario) bool { return s.mode == ModeAfterAgenticReview }
func isPipefix(s scenario) bool {
	return s.kind == KindPipefix
}
func isChange(s scenario) bool { return s.kind == KindChange }
func isDocs(s scenario) bool   { return s.class == scope.ClassDocs }
func isCI(s scenario) bool     { return s.class == scope.ClassCI }

// baseline returns facts and a config under which EVERY row holds.
//
// Only what the mode needs is filled: under after-agentic-review the approval
// facts are the zero value (not read), and under after-human-approval there
// is no review at all -- so a baseline that merges also shows that neither
// mode asks for the other's fact.
func baseline(s scenario) (Facts, Config) {
	f := Facts{
		Kind: s.kind, JudgedSHA: shaJudged, BotUserID: botID,
		Config: ConfigFacts{
			ReadAt: read, AtSHA: shaBase,
			LayerModes: []LayerMode{{Layer: "project", Mode: s.mode}},
		},
		MR: MRFacts{
			ReadAt: read, AuthorID: botID,
			SourceBranch: fixBranch, RecordBranch: fixBranch,
			MetadataAnchor: fixAnchor, RecordAnchor: fixAnchor,
			HeadSHA: shaJudged, Labels: []string{"gonk::pipeline-fix"},
			BlockingDiscussionsResolved: true,
		},
		Verdict: VerdictFacts{ReadAt: read, SHA: shaJudged},
		CI:      CIFacts{ReadAt: read},
		Diff: DiffFacts{
			ReadAt: read, Profiles: scope.DefaultProfiles(), StaticGatesPassed: true,
		},
		Evidence: EvidenceFacts{
			ReadAt:    read,
			Pipelines: []PipelineFact{{ID: 7001, SHA: shaJudged, Status: PipelineSuccess}},
		},
		Target: TargetFacts{ReadAt: read, HeadSHA: shaBase, JudgedBaseSHA: shaBase},
		Review: ReviewFacts{ReadAt: read},
		Status: StatusFacts{ReadAt: read, DetailedMergeStatus: StatusMergeable},
		Rate: RateFacts{
			ReadAt:       read,
			RecentMerges: []time.Time{now.Add(-2 * time.Hour), now.Add(-23 * time.Hour), now.Add(-30 * time.Hour)},
		},
	}
	cfg := Config{Mode: s.mode, Classes: []scope.Class{scope.ClassDocs, scope.ClassCI}, MaxPerDay: 3, RequiredApprovals: 1}

	switch s.kind {
	case KindPipefix:
		f.Verdict.Verdict = string(pipefix.VerdictFixed)
		f.Verdict.TargetJobs = []TargetJob{{Name: "unit-tests", Passed: true, Attempts: 1}}
		f.Evidence.DiagnosedObservations = 2
	case KindChange:
		f.MR.SourceBranch, f.MR.RecordBranch = chgBranch, chgBranch
		f.Verdict.Verdict = VerdictGreen
	}
	switch s.class {
	case scope.ClassCI:
		f.CI.Delta = cidef.KindPreserving
		f.Diff.Paths = []ChangedPath{edited(".gitlab-ci.yml")}
	case scope.ClassDocs:
		f.CI.Delta = cidef.KindNone
		f.Diff.Paths = []ChangedPath{edited("docs/guide.md"), created("docs/tally/usage.md")}
	}
	switch s.mode {
	case ModeAfterHumanApproval:
		f.Approvals = ApprovalFacts{
			ReadAt: read, AgeObservable: true, HeadCommittedAt: now.Add(-time.Hour), CommitCount: 1,
			Approvals: []Approval{{UserID: developer, Member: true, AccessLevel: 30, ApprovedAt: now.Add(-10 * time.Minute)}},
		}
	case ModeAfterAgenticReview:
		f.Review.ReviewsEnabled = true
		f.Review.Reviews = []Review{{
			HeadSHA: shaJudged, Verdict: ReviewNoBlockingFindings, AuthorModel: modelAuthor, ReviewerModel: modelReview,
		}}
	}
	return f, cfg
}

func TestTheBaselineMerges(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.String(), func(t *testing.T) {
			f, cfg := baseline(s)
			d := Decide(f, cfg, now)
			want := Decision{Merge: true, Rule: RuleMerge, Mode: s.mode, SHA: shaJudged, Class: s.class}
			d.Text = ""
			if !reflect.DeepEqual(d, want) {
				t.Fatalf("Decide(baseline) = %+v\nwant %+v", d, want)
			}
			if d.Rule.Row() != "" {
				t.Errorf("Row() of %q = %q, want \"\"", d.Rule, d.Rule.Row())
			}
		})
	}
}

// The fixtures must be what they say they are, by the REAL classifier.
func TestTheFixturePathsAreClassifiedAsTheTestsAssume(t *testing.T) {
	for p, want := range map[string]scope.Class{
		".gitlab-ci.yml":      scope.ClassCI,
		"docs/guide.md":       scope.ClassDocs,
		"docs/tally/usage.md": scope.ClassDocs,
		"docs/tool.py":        scope.ClassDocs, // S7: everything under docs/ is `docs`
		"docs/build.sh":       scope.ClassDocs,
		"CHANGELOG":           scope.ClassDocs,
		"tally/count.go":      scope.ClassSource,
		"tally/count_test.go": scope.ClassTest,
		"go.mod":              scope.ClassDeps,
		"chart/values.yaml":   scope.ClassDeploy,
		".gonk.yml":           scope.ClassControl,
	} {
		if got := classifier.Classify(p).Class; got != want {
			t.Errorf("Classify(%q) = %s, the tests assume %s", p, got, want)
		}
	}
}

// ---- the single-fact-false table ----

// breakCase makes exactly ONE fact false. `code` must then be the deciding
// rule AND the only failure, unless `also` lists facts that cannot be told
// apart from it (each such row says why).
type breakCase struct {
	name string
	code RuleCode
	also []RuleCode
	// when: the scenarios the break applies to; nil means all.
	when func(scenario) bool
	do   func(f *Facts, c *Config)
}

// stale is 61 s before now: literal, so that a longer MaxFactAge is noticed.
var stale = now.Add(-61 * time.Second)

// pyTest is a test file no shipped profile with a weakening predicate vouches
// for: its test-weakening is not decidable.
func pyTest() ChangedPath {
	return ChangedPath{
		Decision: scope.Decision{Path: "tests/test_tally.py", Class: scope.ClassTest, Builtin: scope.ClassTest, Profile: "python"},
		OldMode:  FileModeRegular, NewMode: FileModeRegular,
	}
}

var breakCases = []breakCase{
	// ---- M1 (fix plan A1)
	{name: "M1 the config was not read", code: M1Unread, do: func(f *Facts, c *Config) { f.Config.ReadAt = time.Time{} }},
	{name: "M1 the config was read 61 s ago", code: M1Stale, do: func(f *Facts, c *Config) { f.Config.ReadAt = stale }},
	{name: "M1 the mode is off", code: M1ModeOff, also: []RuleCode{M1LayersDisagree}, // the project layer still says the old mode
		do: func(f *Facts, c *Config) { c.Mode = ModeOff }},
	{name: "M1 the mode is off in every layer", code: M1ModeOff, do: func(f *Facts, c *Config) {
		c.Mode = ModeOff
		f.Config.LayerModes = []LayerMode{{Layer: "project", Mode: ModeOff}}
	}},
	{name: "M1 the mode is the zero value", code: M1ModeOff, do: func(f *Facts, c *Config) {
		c.Mode = ""
		f.Config.LayerModes = []LayerMode{{Layer: "project", Mode: ""}}
	}},
	{name: "M1 the mode is one nobody defined", code: M1ModeUnknown, do: func(f *Facts, c *Config) {
		c.Mode = "after-lunch"
		f.Config.LayerModes = []LayerMode{{Layer: "project", Mode: "after-lunch"}}
	}},
	{name: "M1 an operator layer vetoes", code: M1OperatorVeto, do: func(f *Facts, c *Config) { f.Config.OperatorVeto = true }},
	{name: "M1 the group layer says off", code: M1LayersDisagree, do: func(f *Facts, c *Config) {
		f.Config.LayerModes = append(f.Config.LayerModes, LayerMode{Layer: "group", Mode: ModeOff})
	}},
	{name: "M1 no layer states a mode", code: M1LayersDisagree, do: func(f *Facts, c *Config) { f.Config.LayerModes = nil }},
	{name: "M1 the config was read at another commit", code: M1ConfigNotAtTip, do: func(f *Facts, c *Config) { f.Config.AtSHA = shaOther }},
	{name: "M1 the config's commit is unknown", code: M1ConfigNotAtTip, do: func(f *Facts, c *Config) { f.Config.AtSHA = "" }},

	// ---- M2 (fix plan A2)
	{name: "M2 the MR was not read", code: M2Unread, do: func(f *Facts, c *Config) { f.MR.ReadAt = time.Time{} }},
	{name: "M2 the MR was read 61 s ago", code: M2Stale, do: func(f *Facts, c *Config) { f.MR.ReadAt = stale }},
	{name: "M2 the workflow kind is unknown", code: M2KindUnknown, do: func(f *Facts, c *Config) { f.Kind = "" }},
	{name: "M2 the judged SHA is abbreviated", code: M2JudgedSHAInvalid, when: human,
		// every comparison against it then fails too: nothing equals a bad SHA
		also: []RuleCode{M2HeadNotJudged, M3VerdictWrongSHA, M6PipelineWrongSHA},
		do:   func(f *Facts, c *Config) { f.JudgedSHA = shaJudged[:12] }},
	{name: "M2 the judged SHA is abbreviated, so no review is of it either", code: M2JudgedSHAInvalid, when: agentic,
		also: []RuleCode{M2HeadNotJudged, M3VerdictWrongSHA, M6PipelineWrongSHA, M9NoReviewFact},
		do:   func(f *Facts, c *Config) { f.JudgedSHA = shaJudged[:12] }},
	{name: "M2 the bot's user id is unknown", code: M2BotUnknown, when: agentic,
		do: func(f *Facts, c *Config) { f.BotUserID = 0 }},
	{name: "M2 the bot's user id is unknown, so no approver is provably not the bot", code: M2BotUnknown,
		also: []RuleCode{M8ApprovalsInsufficient}, when: human,
		do: func(f *Facts, c *Config) { f.BotUserID = 0 }},
	{name: "M2 a person authored it", code: M2NotBotAuthored, do: func(f *Facts, c *Config) { f.MR.AuthorID = maintainer }},
	{name: "M2 the source branch has no workflow prefix", code: M2BranchNotWorkflow, do: func(f *Facts, c *Config) {
		f.MR.SourceBranch, f.MR.RecordBranch = "fix/tally", "fix/tally"
	}},
	{name: "M2 a pipefix MR on a change branch", code: M2BranchNotWorkflow, when: isPipefix, do: func(f *Facts, c *Config) {
		f.MR.SourceBranch, f.MR.RecordBranch = chgBranch, chgBranch
	}},
	{name: "M2 a change MR on a pipefix branch", code: M2BranchNotWorkflow, when: isChange, do: func(f *Facts, c *Config) {
		f.MR.SourceBranch, f.MR.RecordBranch = fixBranch, fixBranch
	}},
	{name: "M2 the source branch is not the recorded one", code: M2BranchNotRecorded, do: func(f *Facts, c *Config) {
		f.MR.RecordBranch = f.MR.SourceBranch[:len(f.MR.SourceBranch)-1] + "c"
	}},
	{name: "M2 the record holds no branch", code: M2BranchNotRecorded, do: func(f *Facts, c *Config) { f.MR.RecordBranch = "" }},
	{name: "M2 the metadata anchor is another run's", code: M2AnchorMismatch, do: func(f *Facts, c *Config) {
		f.MR.MetadataAnchor = "gonk:75:pipeline:ref:main:bbbbbbbbbbbb"
	}},
	{name: "M2 no anchor on either side", code: M2AnchorMismatch, do: func(f *Facts, c *Config) {
		f.MR.MetadataAnchor, f.MR.RecordAnchor = "", ""
	}},
	{name: "M2 somebody pushed: the head is not the judged commit", code: M2HeadNotJudged, do: func(f *Facts, c *Config) { f.MR.HeadSHA = shaOther }},
	{name: "M2 the MR is a draft", code: M2Draft, do: func(f *Facts, c *Config) { f.MR.Draft = true }},
	{name: "M2 a human set gonk::hold", code: M2Hold, do: func(f *Facts, c *Config) { f.MR.Labels = append(f.MR.Labels, "gonk::hold") }},
	{name: "M2 gonk::hold in another case", code: M2Hold, do: func(f *Facts, c *Config) { f.MR.Labels = []string{" Gonk::Hold"} }},
	{name: "M2 a thread is unresolved", code: M2ThreadsUnresolved, do: func(f *Facts, c *Config) { f.MR.BlockingDiscussionsResolved = false }},

	// ---- M3 (fix plan A3)
	{name: "M3 the verdict was not recomputed", code: M3Unread, do: func(f *Facts, c *Config) { f.Verdict.ReadAt = time.Time{} }},
	{name: "M3 the verdict was recomputed 61 s ago", code: M3Stale, do: func(f *Facts, c *Config) { f.Verdict.ReadAt = stale }},
	{name: "M3 the verdict is about another commit", code: M3VerdictWrongSHA, do: func(f *Facts, c *Config) { f.Verdict.SHA = shaOther }},
	{name: "M3 FIXED_NEW_FAILURE", code: M3FixedNewFailure, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Verdict.Verdict = string(pipefix.VerdictFixedNewFailure)
	}},
	{name: "M3 UNVERIFIABLE", code: M3NotBestVerdict, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Verdict.Verdict = string(pipefix.VerdictUnverifiable)
	}},
	{name: "M3 a pipefix MR with a change verdict", code: M3NotBestVerdict, when: isPipefix, do: func(f *Facts, c *Config) { f.Verdict.Verdict = VerdictGreen }},
	{name: "M3 a change MR with a pipefix verdict", code: M3NotBestVerdict, when: isChange, do: func(f *Facts, c *Config) {
		f.Verdict.Verdict = string(pipefix.VerdictFixed)
	}},
	{name: "M3 verified-unobserved", code: M3NotBestVerdict, when: isChange, do: func(f *Facts, c *Config) { f.Verdict.Verdict = "TESTS_NOT_RUN" }},
	{name: "M3 no verdict", code: M3NotBestVerdict, do: func(f *Facts, c *Config) { f.Verdict.Verdict = "" }},
	{name: "M3 FIXED with no target job", code: M3NoTargetJobs, when: isPipefix, do: func(f *Facts, c *Config) { f.Verdict.TargetJobs = nil }},
	{name: "M3 a target job did not pass", code: M3TargetNotPassed, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Verdict.TargetJobs = append(f.Verdict.TargetJobs, TargetJob{Name: "lint", Passed: false, Attempts: 1})
	}},
	{name: "M3 a target job passed on its second attempt", code: M3TargetRetried, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Verdict.TargetJobs[0].Attempts = 2
	}},
	{name: "M3 a target job with no attempt", code: M3TargetRetried, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Verdict.TargetJobs[0].Attempts = 0
	}},

	// ---- M4 (fix plan A4)
	{name: "M4 the CI delta was not computed", code: M4Unread, do: func(f *Facts, c *Config) { f.CI.ReadAt = time.Time{} }},
	{name: "M4 the CI delta was computed 61 s ago", code: M4Stale, do: func(f *Facts, c *Config) { f.CI.ReadAt = stale }},
	{name: "M4 exec-changing", code: M4ExecChanging, do: func(f *Facts, c *Config) { f.CI.Delta = cidef.KindExecChanging }},
	{name: "M4 violation", code: M4DeltaNotEligible, do: func(f *Facts, c *Config) { f.CI.Delta = cidef.KindViolation }},
	{name: "M4 unresolvable", code: M4DeltaNotEligible, do: func(f *Facts, c *Config) { f.CI.Delta = cidef.KindUnresolvable }},
	{name: "M4 the zero Kind", code: M4DeltaNotEligible, do: func(f *Facts, c *Config) { f.CI.Delta = "" }},
	{name: "M4 a docs-only diff with a preserving CI delta", code: M4DeltaNotForClass, when: isDocs, do: func(f *Facts, c *Config) {
		f.CI.Delta = cidef.KindPreserving
	}},
	{name: "M4 a ci-only diff that changed nothing effective", code: M4DeltaNotForClass, when: isCI, do: func(f *Facts, c *Config) {
		f.CI.Delta = cidef.KindNone
	}},

	// ---- M5 (fix plan A5)
	{name: "M5 the diff was not listed", code: M5Unread, do: func(f *Facts, c *Config) { f.Diff.ReadAt = time.Time{} }},
	{name: "M5 the diff was listed 61 s ago", code: M5Stale, do: func(f *Facts, c *Config) { f.Diff.ReadAt = stale }},
	{name: "M5 GitLab withheld part of the diff", code: M5DiffIncomplete, do: func(f *Facts, c *Config) { f.Diff.Incomplete = true }},
	{name: "M5 the diff is empty", code: M5EmptyDiff, do: func(f *Facts, c *Config) { f.Diff.Paths = nil }},
	{name: "M5 a path was not classified", code: M5Unclassified, do: func(f *Facts, c *Config) {
		f.Diff.Paths = append(f.Diff.Paths, ChangedPath{Decision: scope.Decision{Path: "docs/more.md"}, OldMode: FileModeRegular, NewMode: FileModeRegular})
	}},
	{name: "M5 a path has a class nobody defined", code: M5Unclassified, do: func(f *Facts, c *Config) {
		f.Diff.Paths[0].Decision.Class = "prose"
	}},
	{name: "M5 a path with a class and no name", code: M5Unclassified, do: func(f *Facts, c *Config) {
		f.Diff.Paths = append(f.Diff.Paths, ChangedPath{
			Decision: scope.Decision{Class: f.Diff.Paths[0].Decision.Class}, OldMode: FileModeRegular, NewMode: FileModeRegular,
		})
	}},
	{name: "M5 a static gate failed on the current diff", code: M5StaticGates, do: func(f *Facts, c *Config) {
		f.Diff.StaticGatesPassed = false
		f.Diff.StaticGateCodes = []string{"SC-SIZE"}
	}},
	{name: "M5 a non-Go test: weakening is not decidable", code: M5WeakeningUndecidable, also: []RuleCode{M5ClassNeverEligible},
		do: func(f *Facts, c *Config) { f.Diff.Paths = []ChangedPath{pyTest()} }},
	{name: "M5 a Go test only", code: M5ClassNeverEligible, do: func(f *Facts, c *Config) {
		f.Diff.Paths = []ChangedPath{edited("tally/count_test.go")}
	}},
	{name: "M5 a source file beside the eligible ones", code: M5ClassNeverEligible, do: func(f *Facts, c *Config) {
		f.Diff.Paths = append(f.Diff.Paths, edited("tally/count.go"))
	}},
	{name: "M5 a source file the project listed in auto_merge.classes", code: M5ClassNeverEligible, do: func(f *Facts, c *Config) {
		c.Classes = scope.Classes()
		f.Diff.Paths = append(f.Diff.Paths, edited("tally/count.go"))
	}},
	{name: "M5 the project did not enable this class", code: M5ClassNotEnabled, do: func(f *Facts, c *Config) {
		for _, k := range EligibleClasses() {
			if k != f.Diff.Paths[0].Decision.Class {
				c.Classes = []scope.Class{k}
			}
		}
	}},
	{name: "M5 the project enabled no class", code: M5ClassNotEnabled, do: func(f *Facts, c *Config) { c.Classes = nil }},
	{name: "M5 docs and ci together", code: M5MixedClasses, when: isPipefix, do: func(f *Facts, c *Config) {
		f.Diff.Paths = []ChangedPath{edited("docs/guide.md"), edited(".gitlab-ci.yml")}
	}},
	{name: "M5 a ci-only change MR", code: M5CINotForKind, when: isChange, do: func(f *Facts, c *Config) {
		f.Diff.Paths = []ChangedPath{edited(".gitlab-ci.yml")}
		f.CI.Delta = cidef.KindPreserving
	}},
	{name: "M5 docs/tool.py is not prose", code: M5DocsPure, when: isDocs, do: func(f *Facts, c *Config) {
		f.Diff.Paths = append(f.Diff.Paths, edited("docs/tool.py"))
	}},
	{name: "M5 a new executable Markdown file", code: M5DocsPure, when: isDocs, do: func(f *Facts, c *Config) {
		p := created("docs/render.md")
		p.NewMode = FileModeExecutable
		f.Diff.Paths = append(f.Diff.Paths, p)
	}},
	{name: "M5 no docs profile selected", code: M5DocsPure, when: isDocs, do: func(f *Facts, c *Config) { f.Diff.Profiles = []string{"go"} }},

	// ---- M6 (fix plan A3's "every candidate pipeline success", and A6)
	{name: "M6 the pipelines were not read", code: M6Unread, do: func(f *Facts, c *Config) { f.Evidence.ReadAt = time.Time{} }},
	{name: "M6 the pipelines were read 61 s ago", code: M6Stale, do: func(f *Facts, c *Config) { f.Evidence.ReadAt = stale }},
	{name: "M6 no candidate pipeline", code: M6NoPipeline, do: func(f *Facts, c *Config) { f.Evidence.Pipelines = nil }},
	{name: "M6 a pipeline of another commit", code: M6PipelineWrongSHA, do: func(f *Facts, c *Config) { f.Evidence.Pipelines[0].SHA = shaOther }},
	{name: "M6 a second candidate pipeline failed", code: M6PipelineNotSuccess, do: func(f *Facts, c *Config) {
		f.Evidence.Pipelines = append(f.Evidence.Pipelines, PipelineFact{ID: 7002, SHA: shaJudged, Status: "failed"})
	}},
	{name: "M6 the pipeline is still running", code: M6PipelineNotSuccess, do: func(f *Facts, c *Config) { f.Evidence.Pipelines[0].Status = "running" }},
	{name: "M6 a green pipeline with a failed attempt in it", code: M6FailedAttempt, do: func(f *Facts, c *Config) { f.Evidence.Pipelines[0].FailedAttempts = 1 }},
	{name: "M6 the diagnosed failure was seen once", code: M6ObservedOnce, when: isPipefix, do: func(f *Facts, c *Config) { f.Evidence.DiagnosedObservations = 1 }},

	// ---- M7 (fix plan A7)
	{name: "M7 the target head was not read", code: M7Unread, do: func(f *Facts, c *Config) { f.Target.ReadAt = time.Time{} }},
	{name: "M7 the target head was read 61 s ago", code: M7Stale, do: func(f *Facts, c *Config) { f.Target.ReadAt = stale }},
	{name: "M7 the target moved", code: M7TargetMoved, do: func(f *Facts, c *Config) {
		f.Target.HeadSHA = shaOther
		f.Config.AtSHA = shaOther // the config WAS read at the new head
	}},
	{name: "M7 the judged base is unknown", code: M7TargetMoved, do: func(f *Facts, c *Config) { f.Target.JudgedBaseSHA = "" }},
	{name: "M7 both SHAs abbreviated alike", code: M7TargetMoved, do: func(f *Facts, c *Config) {
		f.Target.HeadSHA, f.Target.JudgedBaseSHA, f.Config.AtSHA = shaBase[:12], shaBase[:12], shaBase[:12]
	}},

	// ---- M8 (fix plan A8, mode after-human-approval)
	{name: "M8 the approvals were not read", code: M8Unread, when: human, do: func(f *Facts, c *Config) { f.Approvals.ReadAt = time.Time{} }},
	{name: "M8 the approvals were read 61 s ago", code: M8Stale, when: human, do: func(f *Facts, c *Config) { f.Approvals.ReadAt = stale }},
	{name: "M8 nobody approved", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) { f.Approvals.Approvals = nil }},
	{name: "M8 only the bot approved", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.Approvals = []Approval{{UserID: botID, Member: true, AccessLevel: 40, ApprovedAt: now.Add(-time.Minute)}}
	}},
	{name: "M8 the approver is not a member", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) { f.Approvals.Approvals[0].Member = false }},
	{name: "M8 the approver is a reporter", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.Approvals = []Approval{{UserID: reporterID, Member: true, AccessLevel: 20, ApprovedAt: now.Add(-time.Minute)}}
	}},
	{name: "M8 the approval is older than the head commit", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.Approvals[0].ApprovedAt = f.Approvals.HeadCommittedAt.Add(-time.Second)
	}},
	{name: "M8 the approval is as old as the head commit", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.Approvals[0].ApprovedAt = f.Approvals.HeadCommittedAt
	}},
	{name: "M8 the head commit's time is unknown", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.HeadCommittedAt = time.Time{}
	}},
	{name: "M8 an approver with no user id", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) { f.Approvals.Approvals[0].UserID = 0 }},
	{name: "M8 two approvals required, one person approved twice", code: M8ApprovalsInsufficient, when: human, do: func(f *Facts, c *Config) {
		c.RequiredApprovals = 2
		f.Approvals.Approvals = append(f.Approvals.Approvals, f.Approvals.Approvals[0])
	}},
	{name: "M8 approval age not observable, two commits", code: M8AgeUnobservable, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.AgeObservable = false
		f.Approvals.CommitCount = 2
	}},
	{name: "M8 approval age not observable, commit count unknown", code: M8AgeUnobservable, when: human, do: func(f *Facts, c *Config) {
		f.Approvals.AgeObservable = false
		f.Approvals.CommitCount = 0
	}},

	// ---- M9 (fix plan A8, mode after-agentic-review; the veto under every mode)
	{name: "M9 the review records were not read", code: M9Unread, do: func(f *Facts, c *Config) { f.Review.ReadAt = time.Time{} }},
	{name: "M9 the review records were read 61 s ago", code: M9Stale, do: func(f *Facts, c *Config) { f.Review.ReadAt = stale }},
	{name: "M9 a blocking review of this head", code: M9Blocking, do: func(f *Facts, c *Config) {
		f.Review.Reviews = append(f.Review.Reviews, Review{HeadSHA: shaJudged, Verdict: ReviewBlocking, AuthorModel: modelAuthor, ReviewerModel: modelReview})
	}},
	{name: "M9 a blocking review by the author's own model still vetoes", code: M9Blocking, do: func(f *Facts, c *Config) {
		f.Review.Reviews = append(f.Review.Reviews, Review{HeadSHA: shaJudged, Verdict: ReviewBlocking, AuthorModel: modelAuthor, ReviewerModel: modelAuthor})
	}},
	{name: "M9 a review of this head with a verdict nobody defined", code: M9VerdictUnknown, do: func(f *Facts, c *Config) {
		f.Review.Reviews = append(f.Review.Reviews, Review{HeadSHA: shaJudged, Verdict: "looks-fine", AuthorModel: modelAuthor, ReviewerModel: modelReview})
	}},
	{name: "M9 actions.reviews is off", code: M9ReviewsNotEnabled, when: agentic, do: func(f *Facts, c *Config) { f.Review.ReviewsEnabled = false }},
	{name: "M9 no review fact", code: M9NoReviewFact, when: agentic, do: func(f *Facts, c *Config) { f.Review.Reviews = nil }},
	{name: "M9 a label and no review fact", code: M9NoReviewFact, when: agentic, do: func(f *Facts, c *Config) {
		f.Review.Reviews = nil
		f.MR.Labels = append(f.MR.Labels, "gonk::review-no-blocking")
	}},
	{name: "M9 the review is of an older head", code: M9NoReviewFact, when: agentic, do: func(f *Facts, c *Config) { f.Review.Reviews[0].HeadSHA = shaOther }},
	{name: "M9 the reviewer ran on the author's model", code: M9SameModel, when: agentic, do: func(f *Facts, c *Config) {
		f.Review.Reviews[0].ReviewerModel = modelAuthor
	}},
	{name: "M9 the same model, spelled in another case", code: M9SameModel, when: agentic, do: func(f *Facts, c *Config) {
		f.Review.Reviews[0].ReviewerModel = " " + strings.ToUpper(modelAuthor)
	}},
	{name: "M9 the reviewer's model was not recorded", code: M9ModelUnknown, when: agentic, do: func(f *Facts, c *Config) { f.Review.Reviews[0].ReviewerModel = "" }},
	{name: "M9 the author's model was not recorded", code: M9ModelUnknown, when: agentic, do: func(f *Facts, c *Config) { f.Review.Reviews[0].AuthorModel = "" }},

	// ---- M10 (fix plan A9)
	{name: "M10 the merge status was not read", code: M10Unread, do: func(f *Facts, c *Config) { f.Status.ReadAt = time.Time{} }},
	{name: "M10 the merge status was read 61 s ago", code: M10Stale, do: func(f *Facts, c *Config) { f.Status.ReadAt = stale }},
	{name: "M10 not_approved", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "not_approved" }},
	{name: "M10 no status", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "" }},
	{name: "M10 the old merge_status spelling", code: M10NotMergeable, do: func(f *Facts, c *Config) { f.Status.DetailedMergeStatus = "can_be_merged" }},

	// ---- M11 (no fix-plan row)
	{name: "M11 the merge ledger was not read", code: M11Unread, do: func(f *Facts, c *Config) { f.Rate.ReadAt = time.Time{} }},
	{name: "M11 the merge ledger was read 61 s ago", code: M11Stale, do: func(f *Facts, c *Config) { f.Rate.ReadAt = stale }},
	{name: "M11 a third merge in the last 24 h", code: M11RateCapReached, do: func(f *Facts, c *Config) {
		f.Rate.RecentMerges = append(f.Rate.RecentMerges, now.Add(-time.Minute))
	}},
	{name: "M11 max_per_day is 2", code: M11RateCapReached, do: func(f *Facts, c *Config) { c.MaxPerDay = 2 }},
	{name: "M11 max_per_day is 0", code: M11RateCapReached, do: func(f *Facts, c *Config) { c.MaxPerDay = 0 }},
}

// checkRefusal asserts the shape every refusal has.
func checkRefusal(t *testing.T, d Decision, want []RuleCode) {
	t.Helper()
	if d.Merge {
		t.Fatalf("MERGED; want a refusal by %v. Decision: %+v", want, d)
	}
	if got := d.FailedRules(); !reflect.DeepEqual(got, want) {
		t.Errorf("failed facts = %v\nwant           %v", got, want)
	}
	if d.Rule != want[0] {
		t.Errorf("deciding rule = %q, want %q", d.Rule, want[0])
	}
	if d.SHA != "" || d.Class != "" {
		t.Errorf("a refusal carries SHA %q and class %q; both must be empty", d.SHA, d.Class)
	}
	if len(d.Failed) > 0 && (d.Text == "" || d.Text != d.Failed[0].Text) {
		t.Errorf("Text = %q, want the deciding failure's sentence %q", d.Text, d.Failed[0].Text)
	}
	for _, f := range d.Failed {
		if f.Text == "" {
			t.Errorf("%s has no sentence", f.Rule)
		}
		unread := strings.HasSuffix(string(f.Rule), ".unread") || strings.HasSuffix(string(f.Rule), ".stale")
		if f.Unread != unread {
			t.Errorf("%s: Unread = %v, want %v", f.Rule, f.Unread, unread)
		}
	}
}

// TestEachFactFalseAloneBlocksTheMerge is plan 6.2's "each of M1-M11 false
// ALONE blocks", by rule code, in every scenario the fact applies to.
func TestEachFactFalseAloneBlocksTheMerge(t *testing.T) {
	for _, s := range scenarios {
		for _, bc := range breakCases {
			if bc.when != nil && !bc.when(s) {
				continue
			}
			t.Run(s.String()+"/"+bc.name, func(t *testing.T) {
				f, cfg := baseline(s)
				bc.do(&f, &cfg)
				checkRefusal(t, Decide(f, cfg, now), append([]RuleCode{bc.code}, bc.also...))
			})
		}
	}
}

func TestEveryRuleCodeHasAFixtureInSomeScenario(t *testing.T) {
	have := map[RuleCode]bool{}
	for _, bc := range breakCases {
		applies := false
		for _, s := range scenarios {
			if bc.when == nil || bc.when(s) {
				applies = true
			}
		}
		if !applies {
			t.Errorf("fixture %q applies to no scenario", bc.name)
		}
		have[bc.code] = true
		if !strings.HasPrefix(bc.name, bc.code.Row()+" ") {
			t.Errorf("fixture %q: code %q is not of the row its name starts with", bc.name, bc.code)
		}
	}
	for _, r := range Rules() {
		if !have[r] {
			t.Errorf("rule %q has no single-fact-false fixture", r)
		}
	}
	if len(have) != len(Rules()) {
		t.Errorf("%d codes have fixtures, Rules() lists %d", len(have), len(Rules()))
	}
}

// Rules() is the evaluation order, has no duplicate, and names every M* constant
// of rules.go (read from the source, so a constant nobody listed is caught).
func TestRulesListsEveryCodeOnce(t *testing.T) {
	seen := map[RuleCode]bool{}
	lastRow := 0
	for _, r := range Rules() {
		if seen[r] {
			t.Errorf("%q is listed twice", r)
		}
		seen[r] = true
		var n int
		if _, err := fmt.Sscanf(r.Row(), "M%d", &n); err != nil {
			t.Errorf("%q: row %q is not M<n>", r, r.Row())
		}
		if n < lastRow {
			t.Errorf("%q is listed after a rule of M%d: Rules() is not in the plan's order", r, lastRow)
		}
		lastRow = n
	}
	src := readFile(t, "rules.go")
	for _, m := range regexp.MustCompile(`RuleCode = "(M\d+\.[^"]+)"`).FindAllStringSubmatch(src, -1) {
		if !seen[RuleCode(m[1])] {
			t.Errorf("the constant %q is not in Rules()", m[1])
		}
		delete(seen, RuleCode(m[1]))
	}
	for r := range seen {
		t.Errorf("Rules() lists %q, which is not a constant of rules.go", r)
	}
}

// ---- the tests plan 6.2 names for MP-CORE ----

// groups is every fact group and how to reach its ReadAt.
var groups = []struct {
	row    string
	when   func(scenario) bool
	readAt func(*Facts) *time.Time
	// clear zeroes the whole group, leaving ReadAt for the caller to set.
	clear func(*Facts)
	// emptyIsAFact: a group that was read and holds nothing is a real answer
	// ("no review exists", "nothing was merged today") that blocks nothing.
	emptyIsAFact func(scenario) bool
}{
	{"M1", nil, func(f *Facts) *time.Time { return &f.Config.ReadAt }, func(f *Facts) { f.Config = ConfigFacts{} }, nil},
	{"M2", nil, func(f *Facts) *time.Time { return &f.MR.ReadAt }, func(f *Facts) { f.MR = MRFacts{} }, nil},
	{"M3", nil, func(f *Facts) *time.Time { return &f.Verdict.ReadAt }, func(f *Facts) { f.Verdict = VerdictFacts{} }, nil},
	{"M4", nil, func(f *Facts) *time.Time { return &f.CI.ReadAt }, func(f *Facts) { f.CI = CIFacts{} }, nil},
	{"M5", nil, func(f *Facts) *time.Time { return &f.Diff.ReadAt }, func(f *Facts) { f.Diff = DiffFacts{} }, nil},
	{"M6", nil, func(f *Facts) *time.Time { return &f.Evidence.ReadAt }, func(f *Facts) { f.Evidence = EvidenceFacts{} }, nil},
	{"M7", nil, func(f *Facts) *time.Time { return &f.Target.ReadAt }, func(f *Facts) { f.Target = TargetFacts{} }, nil},
	{"M8", human, func(f *Facts) *time.Time { return &f.Approvals.ReadAt }, func(f *Facts) { f.Approvals = ApprovalFacts{} }, nil},
	{"M9", nil, func(f *Facts) *time.Time { return &f.Review.ReadAt }, func(f *Facts) { f.Review = ReviewFacts{} }, human},
	{"M10", nil, func(f *Facts) *time.Time { return &f.Status.ReadAt }, func(f *Facts) { f.Status = StatusFacts{} }, nil},
	{"M11", nil, func(f *Facts) *time.Time { return &f.Rate.ReadAt }, func(f *Facts) { f.Rate = RateFacts{} }, func(scenario) bool { return true }},
}

// "an unreadable fact merges" (plan 6.2; fix plan 9.1 "any unreadable fact
// means do not merge").
func TestAnUnreadFactNeverMerges(t *testing.T) {
	t.Run("the zero value of everything", func(t *testing.T) {
		for _, m := range Modes() {
			d := Decide(Facts{}, Config{Mode: m, Classes: scope.Classes(), MaxPerDay: 99}, now)
			if d.Merge || len(d.Failed) == 0 {
				t.Fatalf("mode %q: Decide(zero Facts) = %+v; want a refusal", m, d)
			}
		}
		if d := Decide(Facts{}, Config{}, time.Time{}); d.Merge || d.Rule != M1Unread {
			t.Errorf("Decide(zero, zero, zero) = merge %v by %q; want a refusal by %q", d.Merge, d.Rule, M1Unread)
		}
	})
	for _, s := range scenarios {
		for _, g := range groups {
			if g.when != nil && !g.when(s) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s was not read", s, g.row), func(t *testing.T) {
				f, cfg := baseline(s)
				*g.readAt(&f) = time.Time{}
				d := Decide(f, cfg, now)
				checkRefusal(t, d, []RuleCode{RuleCode(g.row + ".unread")})
			})
			// The marker alone is not the fact: a group that says it was read
			// and holds nothing must not merge either.
			t.Run(fmt.Sprintf("%s/%s is marked read and is empty", s, g.row), func(t *testing.T) {
				f, cfg := baseline(s)
				g.clear(&f)
				*g.readAt(&f) = read
				d := Decide(f, cfg, now)
				if g.emptyIsAFact != nil && g.emptyIsAFact(s) {
					if !d.Merge {
						t.Fatalf("an empty %s group is a fact here; refused by %v", g.row, d.FailedRules())
					}
					return
				}
				if d.Merge {
					t.Fatalf("MERGED on an empty %s group: %+v", g.row, d)
				}
				if d.Rule.Row() != g.row {
					t.Errorf("deciding rule %q is not of row %s (failed: %v)", d.Rule, g.row, d.FailedRules())
				}
			})
		}
	}
}

// Plan 5.1: "no more than 60 s before the merge call; nothing is carried over
// from the pass that judged".
func TestAStaleFactNeverMerges(t *testing.T) {
	for _, s := range scenarios {
		for _, g := range groups {
			if g.when != nil && !g.when(s) {
				continue
			}
			for _, tc := range []struct {
				name  string
				at    time.Time
				fresh bool
			}{
				{"read exactly 60 s ago", now.Add(-60 * time.Second), true},
				{"read at now", now, true},
				{"read 60 s and 1 ns ago", now.Add(-60*time.Second - time.Nanosecond), false},
				{"read by the pass that judged, an hour ago", now.Add(-time.Hour), false},
				{"read 1 ns in the future", now.Add(time.Nanosecond), false},
			} {
				t.Run(fmt.Sprintf("%s/%s %s", s, g.row, tc.name), func(t *testing.T) {
					f, cfg := baseline(s)
					*g.readAt(&f) = tc.at
					d := Decide(f, cfg, now)
					if tc.fresh {
						if !d.Merge {
							t.Fatalf("refused by %v; a fact this fresh is a fact", d.FailedRules())
						}
						return
					}
					checkRefusal(t, d, []RuleCode{RuleCode(g.row + ".stale")})
				})
			}
		}
	}
	t.Run("a zero now makes every fact a fact from the future", func(t *testing.T) {
		f, cfg := baseline(scenarios[0])
		if d := Decide(f, cfg, time.Time{}); d.Merge || d.Rule != M1Stale {
			t.Fatalf("Decide(baseline, zero now) = merge %v by %q; want a refusal by %q", d.Merge, d.Rule, M1Stale)
		}
	})
}

// "the bot's own approval counts".
func TestTheBotsOwnApprovalNeverCounts(t *testing.T) {
	for _, s := range scenarios {
		if !human(s) {
			continue
		}
		t.Run(s.String(), func(t *testing.T) {
			f, cfg := baseline(s)
			bot := Approval{UserID: botID, Member: true, AccessLevel: 50, ApprovedAt: now.Add(-time.Minute)}
			f.Approvals.Approvals = []Approval{bot}
			checkRefusal(t, Decide(f, cfg, now), []RuleCode{M8ApprovalsInsufficient})

			// Nor does it count toward a higher requirement: one person and the bot are one approval.
			f.Approvals.Approvals = []Approval{bot, {UserID: developer, Member: true, AccessLevel: 30, ApprovedAt: now.Add(-time.Minute)}}
			cfg.RequiredApprovals = 2
			checkRefusal(t, Decide(f, cfg, now), []RuleCode{M8ApprovalsInsufficient})

			f.Approvals.Approvals = append(f.Approvals.Approvals, Approval{UserID: maintainer, Member: true, AccessLevel: 40, ApprovedAt: now.Add(-time.Minute)})
			if d := Decide(f, cfg, now); !d.Merge {
				t.Fatalf("two people approved and two are required; refused by %v", d.FailedRules())
			}
		})
	}
}

// No setting lets zero humans approve under after-human-approval.
func TestRequiredApprovalsIsNeverBelowOne(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		f, cfg := baseline(scenarios[0])
		cfg.RequiredApprovals = n
		if d := Decide(f, cfg, now); !d.Merge {
			t.Fatalf("RequiredApprovals %d with one approval: refused by %v", n, d.FailedRules())
		}
		f.Approvals.Approvals = nil
		checkRefusal(t, Decide(f, cfg, now), []RuleCode{M8ApprovalsInsufficient})
	}
}

// H7 not observable: only an MR with exactly one commit is eligible, and then
// the approval's date is not consulted.
func TestApprovalAgeNotObservable(t *testing.T) {
	f, cfg := baseline(scenarios[0])
	f.Approvals.AgeObservable = false
	f.Approvals.HeadCommittedAt = time.Time{}
	f.Approvals.Approvals[0].ApprovedAt = time.Time{}
	f.Approvals.CommitCount = 1
	if d := Decide(f, cfg, now); !d.Merge {
		t.Fatalf("one commit, one approval, age not observable: refused by %v", d.FailedRules())
	}
	f.Approvals.CommitCount = 2
	checkRefusal(t, Decide(f, cfg, now), []RuleCode{M8AgeUnobservable})
}

// "`docs/tool.py` counts as docs-only" (owner decision 8.1 Q4 i).
func TestAScriptUnderDocsIsNotDocsOnly(t *testing.T) {
	for _, s := range scenarios {
		if !isDocs(s) {
			continue
		}
		for _, p := range []string{"docs/tool.py", "docs/build.sh", "docs/Makefile", "docs/conf.yaml", "CHANGELOG"} {
			t.Run(s.String()+"/"+p, func(t *testing.T) {
				f, cfg := baseline(s)
				cp := edited(p)
				if cp.Decision.Class != scope.ClassDocs {
					t.Fatalf("fixture: %q is class %s, not docs", p, cp.Decision.Class)
				}
				f.Diff.Paths = []ChangedPath{cp}
				d := Decide(f, cfg, now)
				checkRefusal(t, d, []RuleCode{M5DocsPure})
				if !strings.Contains(d.Text, p) {
					t.Errorf("the refusal does not name the path: %q", d.Text)
				}
			})
		}
	}
}

// "`same-model` merges" (owner decision 8.1 Q4 ii): the mode is UNAVAILABLE,
// a named reason, and no Config changes that.
func TestTheSameModelNeverSatisfiesAgenticReview(t *testing.T) {
	for _, s := range scenarios {
		if !agentic(s) {
			continue
		}
		t.Run(s.String(), func(t *testing.T) {
			f, cfg := baseline(s)
			f.Review.Reviews[0].ReviewerModel = modelAuthor
			for _, c := range []Config{
				cfg,
				{Mode: cfg.Mode, Classes: scope.Classes(), MaxPerDay: 1000, RequiredApprovals: 0},
				{Mode: cfg.Mode, Classes: cfg.Classes, MaxPerDay: 3, RequiredApprovals: -1},
			} {
				d := Decide(f, c, now)
				checkRefusal(t, d, []RuleCode{M9SameModel})
				if !strings.Contains(d.Text, "UNAVAILABLE") || !strings.Contains(d.Text, modelAuthor) {
					t.Errorf("the reason does not say the mode is UNAVAILABLE and which model: %q", d.Text)
				}
			}
			// A human's approval does not stand in for the review this mode asks for.
			h, _ := baseline(scenario{ModeAfterHumanApproval, s.kind, s.class})
			f.Approvals = h.Approvals
			checkRefusal(t, Decide(f, cfg, now), []RuleCode{M9SameModel})

			// One distinct-model review of this head is enough, whatever else reviewed it.
			f.Review.Reviews = append(f.Review.Reviews, Review{HeadSHA: shaJudged, Verdict: ReviewNoBlockingFindings, AuthorModel: modelAuthor, ReviewerModel: modelReview})
			if d := Decide(f, cfg, now); !d.Merge {
				t.Fatalf("a distinct-model review exists; refused by %v", d.FailedRules())
			}
		})
	}
}

// "a `blocking` fact merges under human-approval mode": the review is a veto
// under EVERY mode.
func TestABlockingReviewVetoesUnderEveryMode(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.String(), func(t *testing.T) {
			f, cfg := baseline(s)
			f.Review.Reviews = append(f.Review.Reviews, Review{HeadSHA: shaJudged, Verdict: ReviewBlocking, AuthorModel: modelAuthor, ReviewerModel: modelReview})
			checkRefusal(t, Decide(f, cfg, now), []RuleCode{M9Blocking})

			// A blocking review of an OLDER head is not about this commit.
			f.Review.Reviews[len(f.Review.Reviews)-1].HeadSHA = shaOther
			if d := Decide(f, cfg, now); !d.Merge {
				t.Fatalf("the blocking review is of another head; refused by %v", d.FailedRules())
			}
		})
	}
}

// "a label without a review fact merges": no policy reads a label.
func TestALabelIsNotAReviewFact(t *testing.T) {
	for _, s := range scenarios {
		if !agentic(s) {
			continue
		}
		t.Run(s.String(), func(t *testing.T) {
			f, cfg := baseline(s)
			f.Review.Reviews = nil
			f.MR.Labels = append(f.MR.Labels, "gonk::review-no-blocking", "no-blocking-findings", "distinct-model")
			checkRefusal(t, Decide(f, cfg, now), []RuleCode{M9NoReviewFact})
		})
	}
	// And the reverse: a blocking LABEL is display only, it vetoes nothing.
	f, cfg := baseline(scenarios[0])
	f.MR.Labels = append(f.MR.Labels, "gonk::review-blocking")
	if d := Decide(f, cfg, now); !d.Merge {
		t.Fatalf("a label vetoed the merge: %v", d.FailedRules())
	}
}

// Review is necessary and never sufficient: with a clean distinct-model review
// in hand, every other row still has to hold. (The table above runs every
// break under after-agentic-review; this says it in one place.)
func TestAReviewIsNeverSufficient(t *testing.T) {
	for _, s := range scenarios {
		if !agentic(s) {
			continue
		}
		n := 0
		for _, bc := range breakCases {
			if bc.code.Row() == "M9" || bc.code.Row() == "M8" || (bc.when != nil && !bc.when(s)) {
				continue
			}
			f, cfg := baseline(s)
			bc.do(&f, &cfg)
			if d := Decide(f, cfg, now); d.Merge {
				t.Errorf("%s/%s: a review alone merged it", s, bc.name)
			}
			n++
		}
		if n < 60 {
			t.Errorf("%s: only %d non-review breaks were tried", s, n)
		}
	}
}

// Off is the default: the zero Config merges nothing, whatever the facts.
func TestOffIsTheDefault(t *testing.T) {
	for _, s := range scenarios {
		f, _ := baseline(s)
		f.Config.LayerModes = []LayerMode{{Layer: "project", Mode: ""}}
		d := Decide(f, Config{}, now)
		if d.Merge || d.Rule != M1ModeOff {
			t.Errorf("%s: Decide(good facts, zero Config) = merge %v by %q; want a refusal by %q", s, d.Merge, d.Rule, M1ModeOff)
		}
	}
	if got := Modes(); !reflect.DeepEqual(got, []Mode{"off", "after-human-approval", "after-agentic-review"}) {
		t.Errorf("Modes() = %v", got)
	}
}

// Plan 5.2: "Never eligible, under any config".
func TestNoConfigMakesAnotherClassEligible(t *testing.T) {
	if got := EligibleClasses(); !reflect.DeepEqual(got, []scope.Class{scope.ClassDocs, scope.ClassCI}) {
		t.Fatalf("EligibleClasses() = %v, want [docs ci]", got)
	}
	n := 0
	for _, class := range scope.Classes() {
		if class == scope.ClassDocs || class == scope.ClassCI {
			continue
		}
		n++
		for _, s := range scenarios {
			t.Run(fmt.Sprintf("%s/%s", s, class), func(t *testing.T) {
				f, cfg := baseline(s)
				cfg.Classes = scope.Classes()
				// profile "go": the one profile whose tests ARE decidable, so the
				// class itself is what refuses
				f.Diff.Paths = []ChangedPath{{
					Decision: scope.Decision{Path: "tally/thing", Class: class, Builtin: class, Profile: "go"},
					OldMode:  FileModeRegular, NewMode: FileModeRegular,
				}}
				checkRefusal(t, Decide(f, cfg, now), []RuleCode{M5ClassNeverEligible})
			})
		}
	}
	if n != 10 {
		t.Errorf("%d classes besides docs and ci were tried, expected 10: pkg/scope's class list changed, re-read plan 5.2", n)
	}
}

// Owner decisions (fix plan 14.1 Q3, Q6): a fixed-new-failure MR and an
// execution-changing CI fix never merge, under any setting.
func TestWhatNeverMergesUnderAnySetting(t *testing.T) {
	configs := func(base Config) []Config {
		return []Config{
			base,
			{Mode: base.Mode, Classes: scope.Classes(), MaxPerDay: 1 << 30, RequiredApprovals: 0},
			{Mode: base.Mode, Classes: scope.Classes(), MaxPerDay: 1 << 30, RequiredApprovals: -5},
		}
	}
	for _, s := range scenarios {
		f, cfg := baseline(s)
		for i, c := range configs(cfg) {
			g := f
			g.CI.Delta = cidef.KindExecChanging
			if d := Decide(g, c, now); d.Merge || d.Rule != M4ExecChanging {
				t.Errorf("%s config %d: exec-changing: merge %v by %q, want a refusal by %q", s, i, d.Merge, d.Rule, M4ExecChanging)
			}
			if !isPipefix(s) {
				continue
			}
			g = f
			g.Verdict.Verdict = string(pipefix.VerdictFixedNewFailure)
			if d := Decide(g, c, now); d.Merge || d.Rule != M3FixedNewFailure {
				t.Errorf("%s config %d: FIXED_NEW_FAILURE: merge %v by %q, want a refusal by %q", s, i, d.Merge, d.Rule, M3FixedNewFailure)
			}
		}
	}
	// Every pipefix verdict but FIXED is refused at M3.
	for _, v := range pipefix.Verdicts() {
		f, cfg := baseline(scenarios[0])
		f.Verdict.Verdict = string(v)
		d := Decide(f, cfg, now)
		if (v == pipefix.VerdictFixed) != d.Merge {
			t.Errorf("verdict %s: merge = %v", v, d.Merge)
		}
		if !d.Merge && d.Rule.Row() != "M3" {
			t.Errorf("verdict %s refused by %q, not by M3", v, d.Rule)
		}
	}
}

// The merge call is pinned to the judged SHA: Decision.SHA is that commit, in
// full, and only on a merge.
func TestTheMergeIsPinnedToTheJudgedSHA(t *testing.T) {
	for _, s := range scenarios {
		f, cfg := baseline(s)
		if d := Decide(f, cfg, now); d.SHA != shaJudged {
			t.Errorf("%s: Decision.SHA = %q, want the judged commit %q", s, d.SHA, shaJudged)
		}
		// Every SHA in the facts agrees with a judged SHA that is not a full
		// one: nothing to pin a merge call to.
		for _, bad := range []string{"", shaJudged[:12], shaJudged[:39], shaJudged + "0", strings.ToUpper(shaJudged), "g" + shaJudged[1:], " " + shaJudged[1:]} {
			g, _ := baseline(s)
			g.JudgedSHA, g.MR.HeadSHA, g.Verdict.SHA = bad, bad, bad
			for i := range g.Evidence.Pipelines {
				g.Evidence.Pipelines[i].SHA = bad
			}
			for i := range g.Review.Reviews {
				g.Review.Reviews[i].HeadSHA = bad
			}
			d := Decide(g, cfg, now)
			if d.Merge || d.Rule != M2JudgedSHAInvalid || d.SHA != "" {
				t.Errorf("%s: judged SHA %q: merge %v by %q with SHA %q; want a refusal by %q and no SHA", s, bad, d.Merge, d.Rule, d.SHA, M2JudgedSHAInvalid)
			}
		}
	}
}

// Decide lists EVERY failed fact, in the plan's order, and the first is the
// deciding rule -- whichever two facts are false.
func TestEveryFailedFactIsListedInPlanOrder(t *testing.T) {
	rank := map[RuleCode]int{}
	for i, r := range Rules() {
		rank[r] = i
	}
	for _, s := range scenarios {
		var cases []breakCase
		for _, bc := range breakCases {
			if bc.when == nil || bc.when(s) {
				cases = append(cases, bc)
			}
		}
		pairs := 0
		for _, a := range cases {
			for _, b := range cases {
				f, cfg := baseline(s)
				if !apply(a, &f, &cfg) || !apply(b, &f, &cfg) {
					continue // b edits something a removed
				}
				d := Decide(f, cfg, now)
				pairs++
				if d.Merge || len(d.Failed) == 0 {
					t.Fatalf("%s: %q then %q MERGED", s, a.name, b.name)
				}
				if d.Rule != d.Failed[0].Rule {
					t.Fatalf("%s: %q then %q: Rule %q is not Failed[0] %q", s, a.name, b.name, d.Rule, d.Failed[0].Rule)
				}
				for i := 1; i < len(d.Failed); i++ {
					if rank[d.Failed[i-1].Rule] >= rank[d.Failed[i].Rule] {
						t.Fatalf("%s: %q then %q: failed facts out of order or repeated: %v", s, a.name, b.name, d.FailedRules())
					}
				}
				// no break hides the other entirely: one of the two is named
				named := false
				for _, x := range d.Failed {
					named = named || x.Rule == a.code || x.Rule == b.code
				}
				if !named {
					t.Fatalf("%s: %q then %q: failed %v names neither %q nor %q", s, a.name, b.name, d.FailedRules(), a.code, b.code)
				}
			}
		}
		if pairs < 5000 {
			t.Errorf("%s: only %d pairs tried", s, pairs)
		}
	}
	// One spelled out: three rows false at once.
	f, cfg := baseline(scenarios[0])
	f.Status.DetailedMergeStatus = "ci_still_running"
	f.MR.Draft = true
	f.Target.HeadSHA, f.Config.AtSHA = shaOther, shaOther
	checkRefusal(t, Decide(f, cfg, now), []RuleCode{M2Draft, M7TargetMoved, M10NotMergeable})
}

// apply runs a break; false when it indexes something an earlier break removed.
func apply(bc breakCase, f *Facts, c *Config) (ok bool) {
	defer func() { ok = recover() == nil }()
	bc.do(f, c)
	return true
}

// ---- M11, the rate cap, as a pure function of supplied timestamps ----

func TestMergesInWindow(t *testing.T) {
	at := func(d time.Duration) time.Time { return now.Add(d) }
	for _, tc := range []struct {
		name   string
		merges []time.Time
		want   int
	}{
		{"none", nil, 0},
		{"one, a minute ago", []time.Time{at(-time.Minute)}, 1},
		{"exactly 24 h ago is inside", []time.Time{at(-24 * time.Hour)}, 1},
		{"24 h and 1 ns ago is outside", []time.Time{at(-24*time.Hour - time.Nanosecond)}, 0},
		{"at now", []time.Time{now}, 1},
		{"a merge dated in the future counts (clock skew must not open the cap)", []time.Time{at(time.Hour)}, 1},
		{"a zero timestamp counts (an undated merge is not an old one)", []time.Time{{}}, 1},
		{"unsorted, mixed", []time.Time{at(-30 * time.Hour), at(-time.Hour), at(-25 * time.Hour), at(-23 * time.Hour)}, 2},
		{"the same instant twice is two merges", []time.Time{at(-time.Hour), at(-time.Hour)}, 2},
		{"another zone, the same instant", []time.Time{at(-time.Hour).In(time.FixedZone("x", 5*3600))}, 1},
	} {
		if got := MergesInWindow(tc.merges, now); got != tc.want {
			t.Errorf("%s: MergesInWindow = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestTheRateCapIsStrictlyFewerThanMaxPerDay(t *testing.T) {
	for _, tc := range []struct {
		inWindow, max int
		merge         bool
	}{
		{0, 3, true}, {2, 3, true}, {3, 3, false}, {4, 3, false},
		{0, 1, true}, {1, 1, false}, {0, 0, false}, {0, -1, false},
	} {
		f, cfg := baseline(scenarios[0])
		cfg.MaxPerDay = tc.max
		f.Rate.RecentMerges = []time.Time{now.Add(-48 * time.Hour)} // outside the window
		for i := 0; i < tc.inWindow; i++ {
			f.Rate.RecentMerges = append(f.Rate.RecentMerges, now.Add(-time.Duration(i+1)*time.Hour))
		}
		d := Decide(f, cfg, now)
		if d.Merge != tc.merge {
			t.Errorf("%d merges in the window, max_per_day %d: merge = %v, want %v", tc.inWindow, tc.max, d.Merge, tc.merge)
		}
		if !tc.merge {
			checkRefusal(t, d, []RuleCode{M11RateCapReached})
		}
	}
	if DefaultMaxPerDay != 3 || RateWindow != 24*time.Hour || MaxFactAge != 60*time.Second {
		t.Errorf("plan 5.1 says max_per_day 3, 24 h, 60 s; have %d, %s, %s", DefaultMaxPerDay, RateWindow, MaxFactAge)
	}
}

// ---- independence ----

func TestIndependence(t *testing.T) {
	for _, tc := range []struct{ author, reviewer, want string }{
		{modelAuthor, modelReview, IndependenceDistinctModel},
		{modelAuthor, modelAuthor, IndependenceSameModel},
		{modelAuthor, "  " + modelAuthor + "\n", IndependenceSameModel},
		{modelAuthor, strings.ToUpper(modelAuthor), IndependenceSameModel},
		{"", modelReview, IndependenceUnknown},
		{modelAuthor, "", IndependenceUnknown},
		{"", "", IndependenceUnknown},
		{" ", "\t", IndependenceUnknown},
	} {
		if got := Independence(tc.author, tc.reviewer); got != tc.want {
			t.Errorf("Independence(%q, %q) = %q, want %q", tc.author, tc.reviewer, got, tc.want)
		}
	}
}

// ---- constants other packages own ----

func TestConstantsAgreeWithTheirOwners(t *testing.T) {
	if AccessDeveloper != glab.AccessDeveloper {
		t.Errorf("AccessDeveloper = %d, pkg/glab says %d", AccessDeveloper, glab.AccessDeveloper)
	}
	if !strings.HasPrefix(fixBranch, pipelinectx.FixBranchPrefix) || !pipelinectx.IsFixBranch(fixBranch) {
		t.Errorf("the fixture branch %q is not a pipefix branch", fixBranch)
	}
	if strings.HasPrefix(ChangeBranchPrefix, pipelinectx.FixBranchPrefix) || strings.HasPrefix(pipelinectx.FixBranchPrefix, ChangeBranchPrefix) {
		t.Errorf("the two workflow prefixes %q and %q overlap", ChangeBranchPrefix, pipelinectx.FixBranchPrefix)
	}
	for m, a := range map[string]string{"M1": "A1", "M2": "A2", "M3": "A3", "M4": "A4", "M5": "A5", "M6": "A6", "M7": "A7", "M8": "A8", "M9": "A8", "M10": "A9", "M11": "", "M12": "", "": ""} {
		if got := ARow(m); got != a {
			t.Errorf("ARow(%q) = %q, want %q", m, got, a)
		}
	}
}
