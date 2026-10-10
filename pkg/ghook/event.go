package ghook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event is the subset of a GitLab webhook payload gonk consumes. It is
// deliberately small: every field here is a field the golden fixtures must keep
// carrying across GitLab upgrades (spec 12.6). Fields consumed:
//
//	object_kind
//	project.id, project.path_with_namespace, project.default_branch, project.web_url
//	user.id, user.username
//	object_attributes.iid / .action / .title / .description        (issue)
//	object_attributes.id / .note / .noteable_type / .discussion_id (note)
//	issue.iid                                                       (note on an issue)
//	object_attributes.iid / .source_branch / .target_branch
//	  / .state / .action                                            (merge_request)
//	object_attributes.id / .iid / .ref / .tag / .sha / .source
//	  / .status                                                     (pipeline)
//	merge_request.iid / .source_project_id                          (pipeline of an MR)
type Event struct {
	Kind         Kind
	Project      Project
	User         User
	Issue        *Issue
	Note         *Note
	MergeRequest *MergeRequest
	// Pipeline is set for KindPipeline only (gonk-tios D1). Issue, Note and
	// MergeRequest are NIL on a pipeline event: anything that reads them must
	// check (the Handle nil-deref this plan found, F4).
	Pipeline *Pipeline

	// DeliveryID correlates the receiver's record with the dispatcher's, and
	// with GitLab's own hook delivery log. It is GitLab's X-Gitlab-Event-UUID
	// when present and DedupeKey's derived hash otherwise (see Handler), so it
	// is always non-empty for an event that reached the sink.
	//
	// IT IS A CORRELATOR, NEVER A DECISION INPUT. Nothing may branch on it: it
	// is caller-supplied on the header path, so a rule that read it would be a
	// rule an unauthenticated caller could steer. gonk deliberately does NOT
	// carry a W3C traceparent here -- docs/plans/dev-observability.md argues
	// why a distributed trace cannot survive the Gas City exec-order fork the
	// interesting hop goes through, and why the deterministic BeadAnchor is the
	// correlator that can.
	DeliveryID string
}

type Kind string

const (
	KindIssue        Kind = "issue"
	KindNote         Kind = "note"
	KindMergeRequest Kind = "merge_request"
	KindPipeline     Kind = "pipeline"
)

type Project struct {
	ID                int64
	PathWithNamespace string
	DefaultBranch     string
	WebURL            string
}

type User struct {
	ID       int64
	Username string
}

type Issue struct {
	IID         int64
	Action      string // open | reopen | update | close
	Title       string
	Description string
}

type Note struct {
	ID           int64
	Body         string
	NoteableType string // Issue | MergeRequest
	DiscussionID string
}

type MergeRequest struct {
	IID          int64
	Action       string // open | merge | close | update
	State        string
	SourceBranch string
	TargetBranch string
}

// Pipeline is the part of a GitLab Pipeline Hook gonk reads (gonk-tios D1).
// MRIID and MRSourceProjectID come from the payload's top-level
// `merge_request` object, present only for a merge-request pipeline
// (Source == "merge_request_event"); both are 0 otherwise.
type Pipeline struct {
	ID                int64
	IID               int64
	Ref               string
	Tag               bool
	SHA               string
	Source            string // push | merge_request_event | parent_pipeline | schedule | web | api | ...
	Status            string // failed | success | running | canceled | skipped | manual | created | ...
	MRIID             int64
	MRSourceProjectID int64
}

// headerKind maps X-Gitlab-Event to the object_kind we require in the body.
var headerKind = map[string]Kind{
	"Issue Hook":         KindIssue,
	"Note Hook":          KindNote,
	"Merge Request Hook": KindMergeRequest,
	"Pipeline Hook":      KindPipeline,
}

type rawEvent struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	User struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Attrs struct {
		ID           int64  `json:"id"`
		IID          int64  `json:"iid"`
		Action       string `json:"action"`
		State        string `json:"state"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		DiscussionID string `json:"discussion_id"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		// pipeline
		Ref    string `json:"ref"`
		Tag    bool   `json:"tag"`
		SHA    string `json:"sha"`
		Source string `json:"source"`
		Status string `json:"status"`
	} `json:"object_attributes"`
	Issue struct {
		IID int64 `json:"iid"`
	} `json:"issue"`
	// MergeRequest is the top-level `merge_request` object of a Pipeline Hook
	// (null unless the pipeline belongs to an MR).
	MergeRequest *struct {
		IID             int64 `json:"iid"`
		SourceProjectID int64 `json:"source_project_id"`
	} `json:"merge_request"`
}

// ParseEvent decodes and validates one delivery. The X-Gitlab-Event header and
// the body's object_kind must agree: a mismatch means either a GitLab payload
// change (which must not be silently absorbed — spec 12.6) or a caller confusion,
// and both deserve a loud failure rather than a guess.
func ParseEvent(header string, body []byte) (*Event, error) {
	want, ok := headerKind[header]
	if !ok {
		return nil, fmt.Errorf("ghook: unhandled event header %q", header)
	}
	var raw rawEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("ghook: malformed payload: %w", err)
	}
	if Kind(raw.ObjectKind) != want {
		return nil, fmt.Errorf("ghook: header %q but object_kind %q", header, raw.ObjectKind)
	}
	if raw.Project.ID <= 0 {
		return nil, fmt.Errorf("ghook: payload has no project.id")
	}
	if err := attributionSafe(raw.Project.PathWithNamespace); err != nil {
		return nil, err
	}

	ev := &Event{
		Kind: want,
		Project: Project{
			ID:                raw.Project.ID,
			PathWithNamespace: raw.Project.PathWithNamespace,
			DefaultBranch:     raw.Project.DefaultBranch,
			WebURL:            raw.Project.WebURL,
		},
		User: User{ID: raw.User.ID, Username: raw.User.Username},
	}

	switch want {
	case KindIssue:
		if raw.Attrs.IID <= 0 {
			return nil, fmt.Errorf("ghook: issue event has no iid")
		}
		ev.Issue = &Issue{
			IID: raw.Attrs.IID, Action: raw.Attrs.Action,
			Title: raw.Attrs.Title, Description: raw.Attrs.Description,
		}
	case KindNote:
		switch raw.Attrs.NoteableType {
		case "Issue":
			if raw.Issue.IID <= 0 {
				return nil, fmt.Errorf("ghook: note on an issue with no issue.iid")
			}
			ev.Issue = &Issue{IID: raw.Issue.IID}
		case "MergeRequest":
			// carried for v2 (pipeline/MR conversations); v1 dispatch ignores it.
		default:
			return nil, fmt.Errorf("ghook: note on unsupported noteable_type %q", raw.Attrs.NoteableType)
		}
		ev.Note = &Note{
			ID: raw.Attrs.ID, Body: raw.Attrs.Note,
			NoteableType: raw.Attrs.NoteableType, DiscussionID: raw.Attrs.DiscussionID,
		}
	case KindMergeRequest:
		if raw.Attrs.IID <= 0 {
			return nil, fmt.Errorf("ghook: merge_request event has no iid")
		}
		ev.MergeRequest = &MergeRequest{
			IID: raw.Attrs.IID, Action: raw.Attrs.Action, State: raw.Attrs.State,
			SourceBranch: raw.Attrs.SourceBranch, TargetBranch: raw.Attrs.TargetBranch,
		}
	case KindPipeline:
		if raw.Attrs.ID <= 0 {
			return nil, fmt.Errorf("ghook: pipeline event has no id")
		}
		if raw.Attrs.Ref == "" || raw.Attrs.Status == "" {
			return nil, fmt.Errorf("ghook: pipeline event has no ref or status")
		}
		pl := &Pipeline{
			ID: raw.Attrs.ID, IID: raw.Attrs.IID, Ref: raw.Attrs.Ref, Tag: raw.Attrs.Tag,
			SHA: raw.Attrs.SHA, Source: raw.Attrs.Source, Status: raw.Attrs.Status,
		}
		if raw.MergeRequest != nil {
			pl.MRIID, pl.MRSourceProjectID = raw.MergeRequest.IID, raw.MergeRequest.SourceProjectID
		}
		ev.Pipeline = pl
	}
	return ev, nil
}

// attributionSafe rejects values that would corrupt a downstream ledger row.
// pkg/atags accepts any string by contract (PLAN.md carry-forward); the boundary
// is where it gets enforced.
func attributionSafe(path string) error {
	if path == "" {
		return fmt.Errorf("ghook: payload has no project.path_with_namespace")
	}
	if strings.ContainsAny(path, "\n\r,") {
		return fmt.Errorf("ghook: project path contains attribution-unsafe characters")
	}
	return nil
}
