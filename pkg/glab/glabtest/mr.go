package glabtest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
)

// MR merge, approvals, diffs and branch-delete fakes for gonk-2usw (part GLAB).

// SetMRApprovals records who approved an MR (GET .../approvals).
func (s *Server) SetMRApprovals(projectID, mrIID int64, approvers ...glab.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetMRApprovals", projectID)
	if p.MRApprovals == nil {
		p.MRApprovals = map[int64][]glab.User{}
	}
	p.MRApprovals[mrIID] = approvers
}

// SetMRDiffs records an MR's current diff (GET .../diffs).
func (s *Server) SetMRDiffs(projectID, mrIID int64, diffs ...glab.MRDiff) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetMRDiffs", projectID)
	if p.MRDiffs == nil {
		p.MRDiffs = map[int64][]glab.MRDiff{}
	}
	p.MRDiffs[mrIID] = diffs
}

// SetMRHead moves an MR's head sha, as a push to its source branch would.
func (s *Server) SetMRHead(projectID, mrIID int64, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetMRHead", projectID)
	for i := range p.MRs {
		if p.MRs[i].IID == mrIID {
			p.MRs[i].SHA = sha
			return
		}
	}
	s.t.Fatalf("glabtest: SetMRHead: no MR iid=%d in project %d", mrIID, projectID)
}

// handleMergeMR is PUT .../merge. Like GitLab it refuses (405) a merge of an
// MR that is not open, is a draft, or whose detailed_merge_status is set and is
// not "mergeable"; and refuses (409) when the pinned sha is not the head.
func (s *Server) handleMergeMR(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mr, ok := s.findMR(p, iidStr)
	if !ok {
		notFound(w, "Merge Request")
		return
	}
	switch {
	case mr.State != "opened", mr.Draft, mr.DetailedMergeStatus != "" && mr.DetailedMergeStatus != "mergeable":
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "405 Method Not Allowed"})
		return
	case body.SHA == "" || body.SHA != mr.SHA:
		writeJSON(w, http.StatusConflict, map[string]string{"message": "SHA does not match HEAD of source branch"})
		return
	}
	now := time.Now().UTC()
	mr.State = "merged"
	mr.MergedAt = &now
	mr.UpdatedAt = &now
	writeJSON(w, http.StatusOK, *mr)
}

func (s *Server) handleMRApprovals(w http.ResponseWriter, p *Project, iidStr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mr, ok := s.findMR(p, iidStr)
	if !ok {
		notFound(w, "Merge Request")
		return
	}
	by := []glab.MRApprover{}
	for _, u := range p.MRApprovals[mr.IID] {
		by = append(by, glab.MRApprover{User: u})
	}
	// Same keys as the instance observed; no iid, like the real reply.
	writeJSON(w, http.StatusOK, map[string]any{
		"approved": len(by) > 0, "approved_by": by,
		"user_has_approved": false, "user_can_approve": true,
	})
}

func (s *Server) handleMRDiffs(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	s.mu.Lock()
	mr, ok := s.findMR(p, iidStr)
	var out []glab.MRDiff
	if ok {
		out = append(out, p.MRDiffs[mr.IID]...)
	}
	s.mu.Unlock()
	if !ok {
		notFound(w, "Merge Request")
		return
	}
	paginateWrite(w, r, out)
}

func (s *Server) handleDeleteBranch(w http.ResponseWriter, idStr, escName string) {
	p, ok := s.project(idStr)
	if !ok {
		notFound(w, "Project")
		return
	}
	name, err := url.PathUnescape(escName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad branch name"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.Branches[name] {
		notFound(w, "Branch")
		return
	}
	delete(p.Branches, name)
	delete(p.Files, name)
	delete(p.BranchHeads, name)
	delete(p.Modes, name)
	w.WriteHeader(http.StatusNoContent)
}
