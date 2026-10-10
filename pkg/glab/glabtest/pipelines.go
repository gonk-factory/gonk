package glabtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/glab"
)

// Pipeline, job, trace, commit, CI-lint and MR-note fakes for gonk-tios. Seed
// with the Add*/Set* helpers; read back through the real glab.Client.

// AddPipeline appends a pipeline to the project and returns it. A zero ID is
// assigned the next instance-global id; IID defaults to the project-scoped
// count; ProjectID, CreatedAt and WebURL are filled when empty.
func (s *Server) AddPipeline(projectID int64, pl glab.Pipeline) glab.Pipeline {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddPipeline", projectID)
	if pl.ID == 0 {
		pl.ID = s.nextID
		s.nextID++
	}
	if pl.IID == 0 {
		pl.IID = int64(len(p.Pipelines) + 1)
	}
	pl.ProjectID = projectID
	if pl.CreatedAt.IsZero() {
		pl.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(pl.ID) * time.Minute)
		pl.UpdatedAt = pl.CreatedAt
	}
	if pl.WebURL == "" {
		pl.WebURL = fmt.Sprintf("%s/-/pipelines/%d", p.WebURL, pl.ID)
	}
	p.Pipelines = append(p.Pipelines, pl)
	return pl
}

// SetPipelineStatus changes a pipeline's status in place.
func (s *Server) SetPipelineStatus(projectID, pipelineID int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetPipelineStatus", projectID)
	for i := range p.Pipelines {
		if p.Pipelines[i].ID == pipelineID {
			p.Pipelines[i].Status = status
			return
		}
	}
	s.t.Fatalf("glabtest: SetPipelineStatus: no pipeline %d in project %d", pipelineID, projectID)
}

// AddMRPipeline links a pipeline to an MR (GET .../merge_requests/:iid/pipelines).
func (s *Server) AddMRPipeline(projectID, mrIID, pipelineID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddMRPipeline", projectID)
	if p.MRPipelines == nil {
		p.MRPipelines = map[int64][]int64{}
	}
	p.MRPipelines[mrIID] = append(p.MRPipelines[mrIID], pipelineID)
}

// AddJob appends a job to a pipeline and returns it (ID assigned when zero).
// Pipeline.ID/Ref/SHA on the job are filled from the pipeline.
func (s *Server) AddJob(projectID, pipelineID int64, j glab.Job) glab.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddJob", projectID)
	if j.ID == 0 {
		j.ID = s.nextID
		s.nextID++
	}
	for _, pl := range p.Pipelines {
		if pl.ID == pipelineID {
			j.Pipeline.ID, j.Pipeline.Ref, j.Pipeline.SHA = pl.ID, pl.Ref, pl.SHA
			if j.Ref == "" {
				j.Ref = pl.Ref
			}
		}
	}
	if j.Pipeline.ID == 0 {
		s.t.Fatalf("glabtest: AddJob: no pipeline %d in project %d", pipelineID, projectID)
	}
	if p.Jobs == nil {
		p.Jobs = map[int64][]glab.Job{}
	}
	p.Jobs[pipelineID] = append(p.Jobs[pipelineID], j)
	return j
}

// SetTrace sets a job's raw log.
func (s *Server) SetTrace(projectID, jobID int64, trace []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetTrace", projectID)
	if p.Traces == nil {
		p.Traces = map[int64][]byte{}
	}
	p.Traces[jobID] = trace
}

// AddCommit registers a commit (keyed by c.ID, which must be a full SHA).
func (s *Server) AddCommit(projectID int64, c glab.Commit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddCommit", projectID)
	if !glab.IsCommitSHA(c.ID) {
		s.t.Fatalf("glabtest: AddCommit: %q is not a full SHA", c.ID)
	}
	if p.Commits == nil {
		p.Commits = map[string]glab.Commit{}
	}
	p.Commits[c.ID] = c
}

// SetBranchHead makes GET .../repository/commits/:branch answer with the commit
// sha (a full SHA), as GitLab resolves a branch name to its head. The commit
// need not have been added with AddCommit: only its id is served then.
func (s *Server) SetBranchHead(projectID int64, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetBranchHead", projectID)
	if !glab.IsCommitSHA(sha) {
		s.t.Fatalf("glabtest: SetBranchHead: %q is not a full SHA", sha)
	}
	if p.BranchHeads == nil {
		p.BranchHeads = map[string]string{}
	}
	p.BranchHeads[branch] = sha
}

// SetCommitMRs records which MRs (by IID) contain a commit.
func (s *Server) SetCommitMRs(projectID int64, sha string, mrIIDs ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetCommitMRs", projectID)
	if p.CommitMRs == nil {
		p.CommitMRs = map[string][]int64{}
	}
	p.CommitMRs[sha] = mrIIDs
}

// SetCILint records the lint reply for a content_ref SHA. A sha with no entry
// answers 404 like a ref GitLab cannot resolve.
func (s *Server) SetCILint(projectID int64, sha string, l glab.CILint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetCILint", projectID)
	if p.CILints == nil {
		p.CILints = map[string]glab.CILint{}
	}
	p.CILints[sha] = l
}

// CILintQueries returns the query of every ci/lint request the project got,
// in order: what was asked (content_ref) and HOW (dry_run or not) is the
// difference between a commit's own definition and the default branch's.
func (s *Server) CILintQueries(projectID int64) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("CILintQueries", projectID)
	return append([]url.Values(nil), p.CILintQueries...)
}

// SetCILintDryRun records what a lint of sha answers when it is a DRY RUN
// (dry_run=true), where that differs from the static reply: the real GitLab
// resolves `include: local:` on the default branch for a dry run, so the
// merged YAML can hold another commit's included file.
func (s *Server) SetCILintDryRun(projectID int64, sha string, l glab.CILint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("SetCILintDryRun", projectID)
	if p.CILintsDryRun == nil {
		p.CILintsDryRun = map[string]glab.CILint{}
	}
	p.CILintsDryRun[sha] = l
}

// AddMR seeds a merge request with every field the caller sets (IID, SHA,
// SourceBranch, SourceProjectID, Labels, State...). Empty State becomes
// "opened"; empty WebURL is derived. It keeps the project's MR IID counter
// ahead of the seeded IID so a later CreateMergeRequest cannot collide.
func (s *Server) AddMR(projectID int64, mr glab.MergeRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddMR", projectID)
	if mr.State == "" {
		mr.State = "opened"
	}
	if mr.WebURL == "" {
		mr.WebURL = fmt.Sprintf("%s/-/merge_requests/%d", p.WebURL, mr.IID)
	}
	if mr.ProjectID == 0 {
		mr.ProjectID = projectID
	}
	if mr.TargetProjectID == 0 {
		mr.TargetProjectID = projectID
	}
	if mr.SourceProjectID == 0 {
		mr.SourceProjectID = projectID
	}
	if mr.IID > p.nextMRIID {
		p.nextMRIID = mr.IID
	}
	p.MRs = append(p.MRs, mr)
}

// AddMRNote appends a note to an MR (the MR twin of AddNote).
func (s *Server) AddMRNote(projectID, mrIID int64, author glab.User, body string, system bool) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("AddMRNote", projectID)
	if p.MRNotes == nil {
		p.MRNotes = map[int64][]glab.Note{}
	}
	id := s.nextID
	s.nextID++
	p.MRNotes[mrIID] = append(p.MRNotes[mrIID], glab.Note{ID: id, Body: body, Author: author, System: system})
	return id
}

// MRNotesOf returns a copy of an MR's notes, for asserting what was written.
func (s *Server) MRNotesOf(projectID, mrIID int64) []glab.Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("MRNotesOf", projectID)
	return append([]glab.Note(nil), p.MRNotes[mrIID]...)
}

// mustProject requires s.mu held.
func (s *Server) mustProject(op string, id int64) *Project {
	p, ok := s.projects[id]
	if !ok {
		s.t.Fatalf("glabtest: %s: unknown project %d", op, id)
	}
	return p
}

func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 " + what + " Not Found"})
}

// handlePipelineRoutes serves the gonk-tios routes; false means no route matched.
func (s *Server) handlePipelineRoutes(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) < 5 || segs[2] != "projects" {
		return false
	}
	p, ok := s.project(segs[3])
	if !ok {
		notFound(w, "Project")
		return true
	}
	rest := segs[4:]
	get, put, post := r.Method == http.MethodGet, r.Method == http.MethodPut, r.Method == http.MethodPost
	switch {
	case get && len(rest) == 1 && rest[0] == "pipelines":
		s.handleListPipelines(w, r, p)
	case get && len(rest) == 2 && rest[0] == "pipelines":
		s.handleGetPipeline(w, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "pipelines" && rest[2] == "jobs":
		s.handlePipelineJobs(w, r, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "jobs" && rest[2] == "trace":
		s.handleTrace(w, r, p, rest[1])
	case get && len(rest) == 2 && rest[0] == "ci" && rest[1] == "lint":
		s.handleCILint(w, r, p)
	case get && len(rest) == 3 && rest[0] == "repository" && rest[1] == "commits":
		s.handleGetCommit(w, p, rest[2])
	case get && len(rest) == 4 && rest[0] == "repository" && rest[1] == "commits" && rest[3] == "merge_requests":
		s.handleCommitMRs(w, r, p, rest[2])
	case get && len(rest) == 2 && rest[0] == "merge_requests":
		s.handleGetMR(w, p, rest[1])
	case put && len(rest) == 2 && rest[0] == "merge_requests":
		s.handleUpdateMR(w, r, p, rest[1])
	case put && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "merge":
		s.handleMergeMR(w, r, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "approvals":
		s.handleMRApprovals(w, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "diffs":
		s.handleMRDiffs(w, r, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "pipelines":
		s.handleMRPipelines(w, r, p, rest[1])
	case get && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "notes":
		s.handleListMRNotes(w, r, p, rest[1])
	case post && len(rest) == 3 && rest[0] == "merge_requests" && rest[2] == "notes":
		s.handleCreateMRNote(w, r, p, rest[1])
	case put && len(rest) == 4 && rest[0] == "merge_requests" && rest[2] == "notes":
		s.handleUpdateMRNote(w, r, p, rest[1], rest[3])
	case put && len(rest) == 2 && rest[0] == "issues":
		s.handleUpdateIssue(w, r, p, rest[1])
	case post && len(rest) == 3 && rest[0] == "issues" && rest[2] == "notes":
		s.handleCreateIssueNote(w, r, p, rest[1])
	case put && len(rest) == 4 && rest[0] == "issues" && rest[2] == "notes":
		s.handleUpdateIssueNote(w, r, p, rest[1], rest[3])
	default:
		return false
	}
	return true
}

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request, p *Project) {
	q := r.URL.Query()
	s.mu.Lock()
	var out []glab.Pipeline
	for i := len(p.Pipelines) - 1; i >= 0; i-- { // newest first
		pl := p.Pipelines[i]
		if v := q.Get("ref"); v != "" && pl.Ref != v {
			continue
		}
		if v := q.Get("status"); v != "" && pl.Status != v {
			continue
		}
		out = append(out, listShape(pl))
	}
	s.mu.Unlock()
	paginateWrite(w, r, out)
}

// listShape strips the detail-only fields: GitLab's list endpoint does not
// return them, so a caller that needs them must call GetPipeline.
func listShape(pl glab.Pipeline) glab.Pipeline {
	return glab.Pipeline{ID: pl.ID, IID: pl.IID, ProjectID: pl.ProjectID, Status: pl.Status, Source: pl.Source,
		Ref: pl.Ref, SHA: pl.SHA, WebURL: pl.WebURL, CreatedAt: pl.CreatedAt, UpdatedAt: pl.UpdatedAt}
}

func (s *Server) handleGetPipeline(w http.ResponseWriter, p *Project, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pl := range p.Pipelines {
		if pl.ID == id {
			writeJSON(w, http.StatusOK, pl)
			return
		}
	}
	notFound(w, "Pipeline")
}

func (s *Server) handlePipelineJobs(w http.ResponseWriter, r *http.Request, p *Project, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	q := r.URL.Query()
	scopes := q["scope[]"]
	s.mu.Lock()
	known := false
	for _, pl := range p.Pipelines {
		known = known || pl.ID == id
	}
	var out []glab.Job
	for _, j := range p.Jobs[id] {
		if len(scopes) > 0 && !contains(scopes, j.Status) {
			continue
		}
		out = append(out, j)
	}
	s.mu.Unlock()
	if !known {
		notFound(w, "Pipeline")
		return
	}
	// Without include_retried=true GitLab returns only the LATEST attempt of
	// each job name (the highest id); with it, every attempt. Observed on the
	// synthetic project 75 (GitLab 19.4.1, gonk-2usw part PROBE): a job retried
	// once is two job objects of one name, and only the newer is listed by
	// default.
	if q.Get("include_retried") != "true" {
		out = latestAttempts(out)
	}
	paginateWrite(w, r, out)
}

// latestAttempts keeps, per job name, the job with the highest id, in the
// order given.
func latestAttempts(jobs []glab.Job) []glab.Job {
	newest := map[string]int64{}
	for _, j := range jobs {
		if j.ID > newest[j.Name] {
			newest[j.Name] = j.ID
		}
	}
	var out []glab.Job
	for _, j := range jobs {
		if j.ID == newest[j.Name] {
			out = append(out, j)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request, p *Project, idStr string) {
	id, _ := strconv.ParseInt(idStr, 10, 64)
	s.mu.Lock()
	trace, ok := p.Traces[id]
	ignore := s.TraceIgnoresRange
	s.mu.Unlock()
	if !ok {
		notFound(w, "Job")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	rng := r.Header.Get("Range")
	if rng != "" && !ignore && strings.HasPrefix(rng, "bytes=-") {
		n, err := strconv.ParseInt(strings.TrimPrefix(rng, "bytes=-"), 10, 64)
		if err == nil && n > 0 {
			if len(trace) == 0 {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start := int64(len(trace)) - n
			if start < 0 {
				start = 0
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(trace)-1, len(trace)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(trace[start:])
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(trace)
}

// SetCILintFromFile makes the lint of a ref that has NO SetCILint entry answer
// from the repository itself: valid, with the content of `path` at that ref
// as the merged YAML -- what GitLab returns for a definition with no include.
// A ref that does not carry the file still answers 404. It exists so the lint
// of a commit the code under test CREATES (whose SHA no test can know in
// advance) reflects what that commit changed.
func (s *Server) SetCILintFromFile(projectID int64, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mustProject("SetCILintFromFile", projectID).ciLintFile = path
}

func (s *Server) handleCILint(w http.ResponseWriter, r *http.Request, p *Project) {
	ref := r.URL.Query().Get("content_ref")
	s.mu.Lock()
	p.CILintQueries = append(p.CILintQueries, r.URL.Query())
	l, ok := p.CILints[ref]
	if !ok && p.ciLintFile != "" {
		if content, has := p.Files[ref][p.ciLintFile]; has {
			l, ok = glab.CILint{Valid: true, MergedYAML: string(content)}, true
		}
	}
	// A dry run resolves local includes on the default branch, so its reply
	// can differ from the static one for the same commit (SetCILintDryRun).
	if dry, has := p.CILintsDryRun[ref]; has && r.URL.Query().Get("dry_run") == "true" {
		l, ok = dry, true
	}
	s.mu.Unlock()
	if !ok {
		notFound(w, "Ref")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleGetCommit(w http.ResponseWriter, p *Project, sha string) {
	if name, err := url.PathUnescape(sha); err == nil {
		sha = name // a branch name may hold an escaped slash
	}
	s.mu.Lock()
	if head, isBranch := p.BranchHeads[sha]; isBranch {
		c, known := p.Commits[head]
		s.mu.Unlock()
		if !known {
			c = glab.Commit{ID: head}
		}
		writeJSON(w, http.StatusOK, c)
		return
	}
	c, ok := p.Commits[sha]
	s.mu.Unlock()
	if !ok {
		notFound(w, "Commit")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleCommitMRs(w http.ResponseWriter, r *http.Request, p *Project, sha string) {
	s.mu.Lock()
	_, known := p.Commits[sha]
	var out []glab.MergeRequest
	for _, iid := range p.CommitMRs[sha] {
		for _, mr := range p.MRs {
			if mr.IID == iid {
				out = append(out, mr)
			}
		}
	}
	s.mu.Unlock()
	if !known {
		notFound(w, "Commit")
		return
	}
	paginateWrite(w, r, out)
}

func (s *Server) findMR(p *Project, iidStr string) (*glab.MergeRequest, bool) {
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	for i := range p.MRs {
		if p.MRs[i].IID == iid {
			return &p.MRs[i], true
		}
	}
	return nil, false
}

func (s *Server) handleGetMR(w http.ResponseWriter, p *Project, iidStr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mr, ok := s.findMR(p, iidStr)
	if !ok {
		notFound(w, "Merge Request")
		return
	}
	writeJSON(w, http.StatusOK, *mr)
}

func (s *Server) handleUpdateMR(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	var body struct {
		Description  *string  `json:"description"`
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
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
	if body.Description != nil {
		mr.Description = *body.Description
	}
	mr.Labels = editLabels(mr.Labels, body.AddLabels, body.RemoveLabels)
	writeJSON(w, http.StatusOK, *mr)
}

func editLabels(have, add, remove []string) []string {
	out := []string{}
	for _, l := range have {
		if !contains(remove, l) {
			out = append(out, l)
		}
	}
	for _, l := range add {
		if !contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) handleMRPipelines(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	s.mu.Lock()
	if _, ok := s.findMR(p, iidStr); !ok {
		s.mu.Unlock()
		notFound(w, "Merge Request")
		return
	}
	ids := p.MRPipelines[iid]
	var out []glab.Pipeline
	for i := len(ids) - 1; i >= 0; i-- { // newest first
		for _, pl := range p.Pipelines {
			if pl.ID == ids[i] {
				out = append(out, listShape(pl))
			}
		}
	}
	s.mu.Unlock()
	paginateWrite(w, r, out)
}

func (s *Server) handleListMRNotes(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	s.mu.Lock()
	_, ok := s.findMR(p, iidStr)
	notes := append([]glab.Note{}, p.MRNotes[iid]...)
	s.mu.Unlock()
	if !ok {
		notFound(w, "Merge Request")
		return
	}
	paginateWrite(w, r, notes)
}

func (s *Server) handleCreateMRNote(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "400 Bad request - body is missing"})
		return
	}
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.findMR(p, iidStr); !ok {
		notFound(w, "Merge Request")
		return
	}
	if p.MRNotes == nil {
		p.MRNotes = map[int64][]glab.Note{}
	}
	n := glab.Note{ID: s.nextID, Body: body.Body, Author: s.Me}
	s.nextID++
	p.MRNotes[iid] = append(p.MRNotes[iid], n)
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) handleUpdateMRNote(w http.ResponseWriter, r *http.Request, p *Project, iidStr, noteStr string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "400 Bad request - body is missing"})
		return
	}
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	noteID, _ := strconv.ParseInt(noteStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range p.MRNotes[iid] {
		if p.MRNotes[iid][i].ID == noteID {
			p.MRNotes[iid][i].Body = body.Body
			writeJSON(w, http.StatusOK, p.MRNotes[iid][i])
			return
		}
	}
	notFound(w, "Note")
}

// handleUpdateIssue serves PUT .../issues/:iid for title, description,
// add_labels/remove_labels and state_event=close|reopen. Fields are read from
// the JSON body, and state_event also from the query string (CloseIssue sends
// it there).
func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	var body struct {
		Title        *string  `json:"title"`
		Description  *string  `json:"description"`
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
		StateEvent   string   `json:"state_event"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
			return
		}
	}
	if v := r.URL.Query().Get("state_event"); v != "" {
		body.StateEvent = v
	}
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range p.Issues {
		if p.Issues[i].IID != iid {
			continue
		}
		is := &p.Issues[i]
		if body.Title != nil {
			is.Title = *body.Title
		}
		if body.Description != nil {
			is.Description = *body.Description
		}
		is.Labels = editLabels(is.Labels, body.AddLabels, body.RemoveLabels)
		switch body.StateEvent {
		case "close":
			is.State = "closed"
		case "reopen":
			is.State = "opened"
		}
		writeJSON(w, http.StatusOK, *is)
		return
	}
	notFound(w, "Issue")
}

// handleCreateIssueNote serves POST .../issues/:iid/notes: the note is authored
// by the token's user (s.Me), as in GitLab.
func (s *Server) handleCreateIssueNote(w http.ResponseWriter, r *http.Request, p *Project, iidStr string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "400 Bad request - body is missing"})
		return
	}
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, is := range p.Issues {
		if is.IID == iid {
			found = true
		}
	}
	if !found {
		notFound(w, "Issue")
		return
	}
	if p.Notes == nil {
		p.Notes = map[int64][]glab.Note{}
	}
	n := glab.Note{ID: s.nextID, Body: body.Body, Author: s.Me}
	s.nextID++
	p.Notes[iid] = append(p.Notes[iid], n)
	writeJSON(w, http.StatusCreated, n)
}

// handleUpdateIssueNote serves PUT .../issues/:iid/notes/:id.
func (s *Server) handleUpdateIssueNote(w http.ResponseWriter, r *http.Request, p *Project, iidStr, noteStr string) {
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "400 Bad request - body is missing"})
		return
	}
	iid, _ := strconv.ParseInt(iidStr, 10, 64)
	noteID, _ := strconv.ParseInt(noteStr, 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range p.Notes[iid] {
		if p.Notes[iid][i].ID == noteID {
			p.Notes[iid][i].Body = body.Body
			writeJSON(w, http.StatusOK, p.Notes[iid][i])
			return
		}
	}
	notFound(w, "Note")
}

// IssueNotesOf returns a copy of an issue's notes, for asserting what was written.
func (s *Server) IssueNotesOf(projectID, issueIID int64) []glab.Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("IssueNotesOf", projectID)
	return append([]glab.Note(nil), p.Notes[issueIID]...)
}

// IssuesOf returns a copy of a project's issues, for asserting what was written.
func (s *Server) IssuesOf(projectID int64) []glab.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("IssuesOf", projectID)
	return append([]glab.Issue(nil), p.Issues...)
}

// MRsOf returns a copy of a project's merge requests, for asserting labels.
func (s *Server) MRsOf(projectID int64) []glab.MergeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.mustProject("MRsOf", projectID)
	return append([]glab.MergeRequest(nil), p.MRs...)
}
