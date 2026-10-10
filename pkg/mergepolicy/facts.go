package mergepolicy

import (
	"time"

	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Mode is `auto_merge.mode` (plan 5.3). The zero value is not a mode and is
// treated exactly as ModeOff.
type Mode string

const (
	// ModeOff: a human merges; gonk only watches. The default.
	ModeOff Mode = "off"
	// ModeAfterHumanApproval: gonk presses merge on the judged SHA after a
	// human who may approve did (M8). Ships first.
	ModeAfterHumanApproval Mode = "after-human-approval"
	// ModeAfterAgenticReview: gonk merges after an independent review by a
	// model DISTINCT from the author's found nothing blocking (M9).
	ModeAfterAgenticReview Mode = "after-agentic-review"
)

// Modes returns the closed set, in the plan's order.
func Modes() []Mode { return []Mode{ModeOff, ModeAfterHumanApproval, ModeAfterAgenticReview} }

// Kind is the workflow that authored the merge request.
type Kind string

const (
	// KindPipefix: a pipeline-fix MR (gonk-2usw). Its best verdict is FIXED.
	KindPipefix Kind = "pipefix"
	// KindChange: an issue -> MR change (gonk-3so). Its best verdict is GREEN.
	KindChange Kind = "change"
)

// ChangeBranchPrefix is the source-branch prefix of a KindChange MR (epic
// plan 3.1, "gonk/change-*"). The pipefix prefix is pipelinectx.FixBranchPrefix.
// Part CH-APPLY, which will create these branches, does not exist yet; a test
// there must pin its prefix to this constant.
const ChangeBranchPrefix = "gonk/change-"

// VerdictGreen is the best verdict of a KindChange MR (epic plan 2.5, K5).
// pkg/changejudge (part CH-JUDGE) does not exist yet; a test there must pin
// its constant to this one. The pipefix verdicts are pkg/pipefix's.
const VerdictGreen = "GREEN"

// The review verdicts and the independence facts of epic plan 3.2 / 3.3.
const (
	ReviewNoBlockingFindings = "no-blocking-findings"
	ReviewBlocking           = "blocking"

	IndependenceDistinctModel = "distinct-model"
	IndependenceSameModel     = "same-model"
	// IndependenceUnknown: one of the two model names is missing. It is not
	// distinct.
	IndependenceUnknown = "unknown"
)

const (
	// HoldLabel is the label a HUMAN sets to stop gonk (epic plan section 4).
	HoldLabel = "gonk::hold"
	// StatusMergeable is the only detailed_merge_status that is a yes (M10).
	StatusMergeable = "mergeable"
	// PipelineSuccess is the only pipeline status that is green (M6).
	PipelineSuccess = "success"
	// AccessDeveloper is GitLab's Developer access level (M8); a test pins it
	// to pkg/glab's.
	AccessDeveloper = 30
	// MaxFactAge is how old a fact may be when the merge is decided (plan
	// 5.1: "no more than 60 s before the merge call").
	MaxFactAge = 60 * time.Second
	// RateWindow is M11's window.
	RateWindow = 24 * time.Hour
	// DefaultMaxPerDay is `auto_merge.max_per_day`'s default (M11). Decide
	// does NOT apply it: a Config whose MaxPerDay is 0 merges nothing. It is
	// here for X-CONFIG to fill Config with.
	DefaultMaxPerDay = 3
)

// EligibleClasses is the CLOSED set of plan 5.2: the only observed-diff
// classes that are ever merged automatically. No Config adds to it.
func EligibleClasses() []scope.Class { return []scope.Class{scope.ClassDocs, scope.ClassCI} }

// Config is the EFFECTIVE auto-merge configuration for this merge request's
// kind, after the operator / project fold. X-CONFIG fills it from the schema
// (`pipeline_fix.auto_merge` for KindPipefix, `change.auto_merge` for
// KindChange); until then it is filled by hand in tests only.
//
// The zero value merges nothing: Mode "" is off, Classes is empty, MaxPerDay
// is 0.
type Config struct {
	// Mode: `auto_merge.mode`.
	Mode Mode
	// Classes: `auto_merge.classes`. Only members of EligibleClasses have any
	// effect; anything else listed here is ignored, not honoured.
	Classes []scope.Class
	// MaxPerDay: `auto_merge.max_per_day`. Automatic merges on this project in
	// the last RateWindow must be FEWER than this.
	MaxPerDay int
	// RequiredApprovals: how many distinct qualifying human approvals
	// ModeAfterHumanApproval needs. Anything below 1 means 1: there is no
	// setting under which zero humans approve.
	RequiredApprovals int
}

// Facts is everything Decide reads. Each group is one row of plan 5.1, is
// filled by the controller from one source inside the pass that merges, and
// carries ReadAt; the zero value of a group is "not read".
type Facts struct {
	// Kind, JudgedSHA and BotUserID are the controller's own: which workflow
	// wrote the MR, the commit that workflow judged (the record's fix / change
	// commit), and the bot's GitLab user id. An unknown Kind, a JudgedSHA that
	// is not a full 40-hex SHA and a zero BotUserID each fail M2.
	Kind      Kind
	JudgedSHA string
	BotUserID int64

	Config    ConfigFacts   // M1
	MR        MRFacts       // M2
	Verdict   VerdictFacts  // M3
	CI        CIFacts       // M4
	Diff      DiffFacts     // M5
	Evidence  EvidenceFacts // M6
	Target    TargetFacts   // M7
	Approvals ApprovalFacts // M8 (ModeAfterHumanApproval only)
	Review    ReviewFacts   // M9 (the veto: every mode)
	Status    StatusFacts   // M10
	Rate      RateFacts     // M11
}

// ConfigFacts is how Config was obtained (M1).
type ConfigFacts struct {
	ReadAt time.Time
	// AtSHA: the commit `.gonk.yml` was fetched at. It must be the target
	// branch head (TargetFacts.HeadSHA), not the reconcile cache's.
	AtSHA string
	// OperatorVeto: an instance or group layer forbids automatic merges.
	OperatorVeto bool
	// LayerModes: the mode each layer that states one states. At least one
	// layer must state it (the project opts in), and every one must equal
	// Config.Mode.
	LayerModes []LayerMode
}

// LayerMode is one configuration layer's `auto_merge.mode`.
type LayerMode struct {
	Layer string // "instance", "group", "project"
	Mode  Mode
}

// MRFacts is the merge request as GitLab shows it now, next to what the
// record says about it (M2).
type MRFacts struct {
	ReadAt time.Time
	// AuthorID: the MR author's user id.
	AuthorID int64
	// SourceBranch, and the branch the record says gonk pushed.
	SourceBranch string
	RecordBranch string
	// MetadataAnchor: the anchor in the MR description's metadata block.
	// RecordAnchor: the record's.
	MetadataAnchor string
	RecordAnchor   string
	// HeadSHA: the MR's `sha` now.
	HeadSHA string
	Draft   bool
	Labels  []string
	// BlockingDiscussionsResolved: GitLab's `blocking_discussions_resolved`.
	BlockingDiscussionsResolved bool
}

// VerdictFacts is the workflow's exit verdict RE-COMPUTED from live pipelines
// and jobs in this pass (M3). ReadAt is when it was recomputed; a verdict
// copied from the record is not this fact.
type VerdictFacts struct {
	ReadAt time.Time
	// SHA: the commit the verdict is about.
	SHA string
	// Verdict: a pipefix.Verdict for KindPipefix, VerdictGreen or another
	// changejudge verdict for KindChange.
	Verdict string
	// TargetJobs (KindPipefix): every job of the target set T.
	TargetJobs []TargetJob
}

// TargetJob is one diagnosed job as the fix MR's pipelines show it.
type TargetJob struct {
	Name string
	// Passed: every attempt in every candidate pipeline succeeded.
	Passed bool
	// Attempts: how many attempts the job has, retries included, across the
	// candidate pipelines. It must be exactly 1.
	Attempts int
}

// CIFacts is the CI definition delta, re-computed (M4).
type CIFacts struct {
	ReadAt time.Time
	Delta  cidef.Kind
}

// DiffFacts is the MR's CURRENT diff, re-listed and re-classified (M5).
type DiffFacts struct {
	ReadAt time.Time
	// Incomplete: GitLab withheld part of the diff (too_large, collapsed,
	// overflow) or the listing was paged short. An incomplete diff is not a
	// diff.
	Incomplete bool
	// Profiles: the project's SELECTED scope profiles (scope.Effective's).
	// MP-DOCSPURE allows only the prose extensions of the selected
	// documentation profiles.
	Profiles []string
	Paths    []ChangedPath
	// StaticGatesPassed: the workflow's static gates, re-run on this diff,
	// all passed. StaticGateCodes names the ones that did not.
	StaticGatesPassed bool
	StaticGateCodes   []string
}

// Git file modes as `git diff` and GitLab's a_mode / b_mode print them.
const (
	FileModeRegular    = "100644"
	FileModeExecutable = "100755"
	FileModeSymlink    = "120000"
	FileModeSubmodule  = "160000"
)

// ChangedPath is one file of the diff.
type ChangedPath struct {
	// Decision: the scope classifier's decision for the file's path (the NEW
	// path of a rename; the path of a deleted file).
	Decision scope.Decision
	// Created, Deleted, Renamed: GitLab's new_file, deleted_file,
	// renamed_file.
	Created bool
	Deleted bool
	Renamed bool
	// OldPath, OldClass: a renamed file's previous path and that path's class.
	OldPath  string
	OldClass scope.Class
	// OldMode, NewMode: the file's mode before and after ("100644", ...). The
	// side that does not exist (before a creation, after a deletion) is "0" or
	// "000000". "" means the mode was not read.
	OldMode string
	NewMode string
}

// EvidenceFacts is the class evidence (M6): the candidate pipelines of the
// judged commit, and how often the diagnosed failure was seen.
type EvidenceFacts struct {
	ReadAt    time.Time
	Pipelines []PipelineFact
	// DiagnosedObservations (KindPipefix, fix plan A6): how many times the
	// diagnosed failure was observed with the same job failure key -- two
	// pipelines of the run, or two attempts of the base job.
	DiagnosedObservations int
}

// PipelineFact is one candidate pipeline of the MR.
type PipelineFact struct {
	ID     int64
	SHA    string
	Status string
	// FailedAttempts: job attempts in this pipeline that did not succeed,
	// retried ones included, allow_failure ones included.
	FailedAttempts int
}

// TargetFacts is the target branch now (M7).
type TargetFacts struct {
	ReadAt time.Time
	// HeadSHA: the target branch's head.
	HeadSHA string
	// JudgedBaseSHA: the target commit the judged pipeline was built on.
	JudgedBaseSHA string
}

// ApprovalFacts is M8's input.
type ApprovalFacts struct {
	ReadAt    time.Time
	Approvals []Approval
	// AgeObservable (epic plan H7): this GitLab lets the controller date an
	// approval, and it did. When false, only an MR with exactly one commit is
	// eligible, and ApprovedAt is not consulted.
	AgeObservable bool
	// HeadCommittedAt: when the MR's head commit was created.
	HeadCommittedAt time.Time
	// CommitCount: the number of commits on the MR.
	CommitCount int
}

// Approval is one entry of the MR's approved_by, joined with the project's
// members.
type Approval struct {
	UserID int64
	// Member, AccessLevel: the approver's project membership (inherited
	// included) and access level.
	Member      bool
	AccessLevel int
	// ApprovedAt: when the approval was given (the approval's system note).
	ApprovedAt time.Time
}

// ReviewFacts is M9's input: the review RECORDS of this merge request. A
// label is not a review fact and no field here is one.
type ReviewFacts struct {
	ReadAt time.Time
	// ReviewsEnabled: `actions.reviews` is true for the project.
	ReviewsEnabled bool
	Reviews        []Review
}

// Review is one review fact: one independent review session's recorded
// result.
type Review struct {
	// HeadSHA: the MR head the review ran on.
	HeadSHA string
	// Verdict: ReviewBlocking or ReviewNoBlockingFindings.
	Verdict string
	// AuthorModel, ReviewerModel: Record.Model of the author's session and of
	// the reviewer's. Independence is computed from these two; nothing is
	// taken on a recorded "distinct-model" string.
	AuthorModel   string
	ReviewerModel string
}

// StatusFacts is M10's input.
type StatusFacts struct {
	ReadAt time.Time
	// DetailedMergeStatus: GitLab's `detailed_merge_status`.
	DetailedMergeStatus string
}

// RateFacts is M11's input.
type RateFacts struct {
	ReadAt time.Time
	// RecentMerges: when each of this project's automatic merges happened. The
	// caller may pass more than the window; MergesInWindow counts.
	RecentMerges []time.Time
}
