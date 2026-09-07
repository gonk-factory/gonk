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
}

type MRListOptions struct {
	SourceBranch string
	State        string // opened | closed | merged | all
}

type MROptions struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	AssigneeID         int64  `json:"assignee_id,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
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

type Commit struct {
	ID string `json:"id"`
}

type Branch struct {
	Name string `json:"name"`
}
