package glab

import "time"

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Project struct {
	ID                int64       `json:"id"`
	PathWithNamespace string      `json:"path_with_namespace"`
	DefaultBranch     string      `json:"default_branch"`
	WebURL            string      `json:"web_url"`
	Archived          bool        `json:"archived"`
	Permissions       Permissions `json:"permissions"`
}

type Permissions struct {
	ProjectAccess *Access `json:"project_access"`
	GroupAccess   *Access `json:"group_access"`
}

type Access struct {
	AccessLevel int `json:"access_level"`
}

// GitLab access levels.
const (
	AccessGuest      = 10
	AccessReporter   = 20
	AccessDeveloper  = 30
	AccessMaintainer = 40
	AccessOwner      = 50
)

// EffectiveAccess is the higher of the bot's direct project access and its
// inherited group access. A bot invited at the group level has no
// project_access at all, so reading only project_access would misreport it as
// having no rights and open a spurious "I need Developer" issue.
func (p Project) EffectiveAccess() int {
	lvl := 0
	if p.Permissions.ProjectAccess != nil {
		lvl = p.Permissions.ProjectAccess.AccessLevel
	}
	if g := p.Permissions.GroupAccess; g != nil && g.AccessLevel > lvl {
		lvl = g.AccessLevel
	}
	return lvl
}

type Hook struct {
	ID                    int64  `json:"id"`
	URL                   string `json:"url"`
	IssuesEvents          bool   `json:"issues_events"`
	NoteEvents            bool   `json:"note_events"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	PushEvents            bool   `json:"push_events"`
	PipelineEvents        bool   `json:"pipeline_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

// HookOptions is the create/edit payload. Token is write-only: GitLab never
// returns it, which is why hook token freshness is tracked by a generation
// marker in the URL instead (see ADR-003).
type HookOptions struct {
	URL                   string `json:"url"`
	Token                 string `json:"token,omitempty"`
	IssuesEvents          bool   `json:"issues_events"`
	NoteEvents            bool   `json:"note_events"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	PushEvents            bool   `json:"push_events"`
	PipelineEvents        bool   `json:"pipeline_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

type Member struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	AccessLevel int        `json:"access_level"`
	CreatedAt   *time.Time `json:"created_at"`
}

type MergeRequest struct {
	IID          int64      `json:"iid"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	State        string     `json:"state"` // opened | closed | merged | locked
	SourceBranch string     `json:"source_branch"`
	TargetBranch string     `json:"target_branch"`
	WebURL       string     `json:"web_url"`
	UpdatedAt    *time.Time `json:"updated_at"`
	MergedAt     *time.Time `json:"merged_at"`
	ClosedAt     *time.Time `json:"closed_at"`
	// The fields below serve pipeline triage (gonk-tios): which project the MR's
	// source branch lives in (a fork MR has SourceProjectID != the target
	// project), its labels, and the head commit.
	ProjectID       int64    `json:"project_id"`
	SourceProjectID int64    `json:"source_project_id"`
	TargetProjectID int64    `json:"target_project_id"`
	Labels          []string `json:"labels"`
	SHA             string   `json:"sha"`
	Author          User     `json:"author"`
	// The fields below serve the pipeline-fix MR (gonk-2usw): the merge policy
	// reads them, and every one fails SAFE when GitLab omits it (the zero value
	// is "not mergeable / not resolved / still a draft-or-unknown", never the
	// permissive reading). Observed on the synthetic project 75 (2026-10-05):
	// draft=false, detailed_merge_status="not_open" on a closed MR,
	// blocking_discussions_resolved=true.
	//
	// DetailedMergeStatus is GitLab's single verdict on whether a merge would be
	// accepted ("mergeable", "not_approved", "ci_still_running",
	// "discussions_not_resolved", "draft_status", "not_open", ...). Only the
	// literal "mergeable" is a yes.
	DetailedMergeStatus string `json:"detailed_merge_status"`
	// Draft is true for a draft MR. GitLab refuses to merge a draft.
	Draft bool `json:"draft"`
	// BlockingDiscussionsResolved is true when no unresolved thread blocks the
	// merge (the "threads resolved" fact). Absent decodes to false: unresolved.
	BlockingDiscussionsResolved bool `json:"blocking_discussions_resolved"`
	// MergeCommitSHA is empty until merged (and after a fast-forward merge).
	MergeCommitSHA string `json:"merge_commit_sha"`
	// SquashCommitSHA is the commit a squash merge put on the target; empty
	// when the MR was not squashed. With MergeCommitSHA and SHA it is how a
	// pipeline is recognised as running ON a merged fix (gonk-2usw 3.3,
	// pipelinectx.FixEpoch).
	SquashCommitSHA string `json:"squash_commit_sha"`
}

type MRListOptions struct {
	SourceBranch string
	State        string // opened | closed | merged | all
	// UpdatedAfter bounds the listing server-side (GitLab's `updated_after`).
	// Zero means unbounded. Callers that rely on it re-check UpdatedAt.
	UpdatedAfter time.Time
	// TargetBranch and AuthorID bound the listing server-side (GitLab's
	// `target_branch`, `author_id`). Empty / zero means unbounded. Callers
	// that rely on them re-check the returned objects: a filter that only
	// works when the server cooperates is not a guard.
	TargetBranch string
	AuthorID     int64
}

type MROptions struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	AssigneeID         int64  `json:"assignee_id,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
	// Labels are applied at creation, sent as a JSON ARRAY so one label is one
	// label (see AddIssueLabel for why a comma-separated string is unsafe).
	// CreateMergeRequest verifies the reply carries every one.
	Labels []string `json:"labels,omitempty"`
}

// MRUpdate is a partial MR edit (PUT .../merge_requests/:iid). A nil
// Description leaves it alone; a non-nil pointer to "" CLEARS it (same
// convention as IssueUpdate). It carries no assignee or reviewer field on
// purpose: gonk never assigns, reviews or approves (066 item d).
type MRUpdate struct {
	Description  *string
	AddLabels    []string
	RemoveLabels []string
}

// MRApprovals is GET .../merge_requests/:iid/approvals. The reply carries no
// iid on the instance observed (project 75, 2026-10-05: keys approved,
// approved_by, user_can_approve, user_has_approved), so GetMRApprovals
// validates by the presence of `approved` and `approved_by` instead.
type MRApprovals struct {
	Approved   bool
	ApprovedBy []MRApprover
}

// MRApprover is one entry of approved_by: the approving user.
type MRApprover struct {
	User User `json:"user"`
}

// MRDiff is one file of an MR's current diff (GET .../merge_requests/:iid/diffs).
type MRDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	// Diff is the unified diff text. TooLarge means GitLab withheld it, so the
	// caller must treat the file as unreadable rather than as unchanged.
	Diff      string `json:"diff"`
	TooLarge  bool   `json:"too_large"`
	Collapsed bool   `json:"collapsed"`
}

type Issue struct {
	IID int64 `json:"iid"`
	// UpdatedAt backs the sweep's recency bound. It is also checked
	// client-side, because `updated_after` being silently ignored by a proxy or
	// an older GitLab would turn the bound off without any error.
	UpdatedAt time.Time `json:"updated_at"`
	// Author exists for the reconciler's issue sweep. The webhook path gets its
	// loop guard from the event payload's user (ghook.Handler.BotUserID); a swept
	// issue has no event, so without this the sweep would happily triage an issue
	// the bot itself opened -- the same infinite loop, arriving by the other door.
	Author      User     `json:"author"`
	Title       string   `json:"title"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	WebURL      string   `json:"web_url"`
	Description string   `json:"description"` // the issue body; the broker splices it into the triage prompt
}

type IssueOptions struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Labels      string `json:"labels,omitempty"` // comma-separated, per the API
}

type IssueListOptions struct {
	State  string
	Labels string
	// AuthorID filters to issues authored by this user (GitLab's `author_id`).
	// Zero means no filter. Triage uses it to find the bot's own tracking issue
	// without trusting a title or a label a human could also write.
	AuthorID int64
	// UpdatedAfter bounds the listing server-side (GitLab's `updated_after`).
	// Zero means unbounded.
	//
	// The reconciler's issue sweep depends on this, and not merely for
	// efficiency: unbounded, its FIRST pass against a project with history
	// treats every stale open issue as a lost event. Measured on project 75,
	// 2026-09-07 -- 68 open issues, 57 of them untriaged and non-bot, dating
	// back to July. Bounded to 24h: 8.
	UpdatedAfter time.Time
}

type CommitAction struct {
	Action   string `json:"action"` // create | update
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type CommitOptions struct {
	Branch        string         `json:"branch"`
	StartBranch   string         `json:"start_branch,omitempty"`
	CommitMessage string         `json:"commit_message"`
	Actions       []CommitAction `json:"actions"`
}

// Commit is a repository commit. CreateCommit and ResolveRef read only ID; the
// remaining fields are populated by GetCommit (GET .../repository/commits/:sha).
type Commit struct {
	ID           string     `json:"id"`
	ShortID      string     `json:"short_id"`
	Title        string     `json:"title"`
	Message      string     `json:"message"`
	AuthorName   string     `json:"author_name"`
	AuthorEmail  string     `json:"author_email"`
	AuthoredDate *time.Time `json:"authored_date"`
	WebURL       string     `json:"web_url"`
	// ParentIDs are the commit's parents (GetCommit). One entry for an
	// ordinary commit; a caller that must know a commit sits DIRECTLY on a
	// base it pinned compares this, never a branch name.
	ParentIDs []string `json:"parent_ids"`
}

// IssueUpdate is a partial issue edit (PUT .../issues/:iid). A nil Title or
// Description leaves that field alone; a non-nil pointer to "" CLEARS it. That
// distinction is why these are pointers: with plain strings, "unchanged" and
// "empty it" would be the same value.
type IssueUpdate struct {
	Title        *string
	Description  *string
	AddLabels    []string
	RemoveLabels []string
}

// Pipeline is a CI pipeline. The list endpoints fill only the first block
// (id, iid, project_id, status, source, ref, sha, web_url, created_at,
// updated_at); GET .../pipelines/:id also fills the second.
type Pipeline struct {
	ID        int64     `json:"id"`
	IID       int64     `json:"iid"`
	ProjectID int64     `json:"project_id"`
	Status    string    `json:"status"`
	Source    string    `json:"source"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Detail-only fields.
	Tag        bool       `json:"tag"`
	BeforeSHA  string     `json:"before_sha"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Duration   *float64   `json:"duration"`
	User       User       `json:"user"`
	// YamlErrors is GitLab's pipeline-level config error text; a pipeline that
	// failed to even create its jobs carries it. FailureReason is the
	// pipeline-level failure reason where the instance reports one
	// (e.g. "config_error"); both are empty otherwise.
	YamlErrors    string `json:"yaml_errors"`
	FailureReason string `json:"failure_reason"`
}

// Runner is the runner object GitLab embeds in a Job. It is the job's view of
// the runner, so it needs no admin runner API.
type Runner struct {
	ID          int64  `json:"id"`
	Description string `json:"description"`
	Name        string `json:"name"`
	RunnerType  string `json:"runner_type"` // instance_type | group_type | project_type
	IsShared    bool   `json:"is_shared"`
	Active      bool   `json:"active"`
	Paused      bool   `json:"paused"`
	Online      bool   `json:"online"`
	Status      string `json:"status"` // online | offline | stale | never_contacted
}

// RunnerManager is the runner-process identity GitLab (17.x+) embeds in a Job
// next to Runner: one runner can run on several hosts, and this says which one
// ran the job. Nil when the instance does not send it.
type RunnerManager struct {
	ID           int64  `json:"id"`
	SystemID     string `json:"system_id"`
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
}

// Job is one CI job (GET .../pipelines/:id/jobs).
type Job struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Stage         string     `json:"stage"`
	Status        string     `json:"status"`
	FailureReason string     `json:"failure_reason"`
	AllowFailure  bool       `json:"allow_failure"`
	Ref           string     `json:"ref"`
	Tag           bool       `json:"tag"`
	WebURL        string     `json:"web_url"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Duration      *float64   `json:"duration"`
	TagList       []string   `json:"tag_list"`
	// Runner is nil for a job that never reached a runner (GitLab sends null).
	Runner        *Runner        `json:"runner"`
	RunnerManager *RunnerManager `json:"runner_manager"`
	Pipeline      struct {
		ID  int64  `json:"id"`
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"pipeline"`
}

type Branch struct {
	Name string `json:"name"`
}
