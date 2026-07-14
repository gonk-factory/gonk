// Package glabtest is an in-memory GitLab REST API: enough of it to drive
// gonk-intake's tests with no network, no containers, and no live GitLab.
// Fidelity notes that matter:
//   - the private token is required on every request (401 otherwise);
//   - hook tokens are stored but never returned (GitLab does not return them);
//   - files are per-branch, so an onboarding MR does not make main look onboarded;
//   - CreateBranch on an existing branch is a 400, like the real API.
package glabtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// failure is one entry in the FailNext queue: the next `times` requests whose
// method and path match are answered with `status` instead of being routed.
// always makes the failure permanent (FailAlways), ignoring times.
type failure struct {
	method     string
	pathPrefix string
	status     int
	times      int
	always     bool
}

// Server is an in-memory GitLab. Zero value is not usable; construct with New.
type Server struct {
	Me glab.User

	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	token    string
	projects map[int64]*Project
	nextID   int64
	fails    []failure // FailNext queue
	requests []string  // "METHOD /path" log, in request order
}

// Project is a fake GitLab project plus every sub-resource pkg/glab touches.
type Project struct {
	glab.Project
	Files    map[string]map[string][]byte // branch -> path -> content
	Branches map[string]bool
	Hooks    []glab.Hook
	MRs      []glab.MergeRequest
	Issues   []glab.Issue
	Members  []glab.Member

	hookTokens map[int64]string
	srv        *Server
	// nextMRIID/nextIssueIID: MR and Issue "IID" (internal ID) is scoped to the
	// PROJECT in real GitLab, unlike every other ID here (project, commit, hook),
	// which is instance-global. Numbering these from the shared s.nextID would
	// make the first MR/issue in a project land on whatever id the counter had
	// reached -- not 1 -- which would not match real GitLab and would make
	// glabtest a bad double for tests that assert on a specific IID.
	nextMRIID    int64
	nextIssueIID int64
}

// New starts an httptest.Server backing a fresh, empty fake GitLab.
// t.Cleanup closes it.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		t:        t,
		token:    "glabtest-token",
		projects: map[int64]*Project{},
		nextID:   1,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the fake's base URL, suitable for glab.New.
func (s *Server) URL() string { return s.srv.URL }

// Client returns a real glab.Client pointed at the fake, with no retry
// backoff (tests should not sleep) and the token the fake accepts.
func (s *Server) Client() *glab.Client {
	c := glab.New(s.URL(), s.token)
	c.RetryBackoff = func(int) time.Duration { return 0 }
	return c
}

// RemoveProject deletes a project from the fake, as if the bot had been
// removed from it (or the project deleted). The next ListMemberProjects call
// will no longer return it, which is exactly the de-onboarding signal the
// reconciler watches for.
func (s *Server) RemoveProject(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projects, id)
}

// AddProject registers a project with default_branch "main" and a bot member
// (s.Me) at access. access of 0 means no membership at all (used to test the
// "bot invited but no access" path).
func (s *Server) AddProject(path string, access int) *Project {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := s.nextID
	s.nextID++
	p := &Project{
		Project: glab.Project{
			ID:                id,
			PathWithNamespace: path,
			DefaultBranch:     "main",
			WebURL:            fmt.Sprintf("%s/%s", s.srv.URL, path),
		},
		Files:      map[string]map[string][]byte{"main": {}},
		Branches:   map[string]bool{"main": true},
		hookTokens: map[int64]string{},
		srv:        s,
	}
	if access > 0 {
		p.Permissions.ProjectAccess = &glab.Access{AccessLevel: access}
		p.Members = append(p.Members, glab.Member{ID: s.Me.ID, Username: s.Me.Username, AccessLevel: access})
	}
	s.projects[id] = p
	return p
}

// PutFile seeds a file on the project's default branch.
func (p *Project) PutFile(path string, content []byte) {
	p.PutFileOn(p.DefaultBranch, path, content)
}

// PutFileOn seeds a file on the given branch, creating the branch if absent.
func (p *Project) PutFileOn(branch, path string, content []byte) {
	p.srv.mu.Lock()
	defer p.srv.mu.Unlock()
	if p.Files[branch] == nil {
		p.Files[branch] = map[string][]byte{}
	}
	p.Files[branch][path] = content
	p.Branches[branch] = true
}

// HookToken is test-only introspection: GitLab never returns a hook's token
// over the API, so the only way to assert one was stored is to ask the fake
// directly.
func (s *Server) HookToken(projectID, hookID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok {
		s.t.Fatalf("glabtest: HookToken: unknown project %d", projectID)
		return ""
	}
	return p.hookTokens[hookID]
}

// FailNext queues `times` responses of `status` for the next requests whose
// method and path (query string excluded) start with pathPrefix.
func (s *Server) FailNext(method, pathPrefix string, status, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, failure{method: method, pathPrefix: pathPrefix, status: status, times: times})
}

// FailAlways queues a permanent failure of `status` for every future request
// whose method and path (query string excluded) start with pathPrefix. Unlike
// FailNext it never expires -- for a project that is durably broken (e.g. a
// GitLab-side permission error) across an entire test.
func (s *Server) FailAlways(method, pathPrefix string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, failure{method: method, pathPrefix: pathPrefix, status: status, always: true})
}

// AddMember appends a member to a project's member list (GET
// /members/all), independent of the bot's own access set by AddProject.
func (s *Server) AddMember(projectID int64, m glab.Member) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok {
		s.t.Fatalf("glabtest: AddMember: unknown project %d", projectID)
		return
	}
	p.Members = append(p.Members, m)
}

// SetMRState moves a merge request to "merged" or "closed" at the given time,
// for reconciler tests that need to observe an MR's terminal state.
func (s *Server) SetMRState(projectID, iid int64, state string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[projectID]
	if !ok {
		s.t.Fatalf("glabtest: SetMRState: unknown project %d", projectID)
		return
	}
	for i := range p.MRs {
		if p.MRs[i].IID != iid {
			continue
		}
		atCopy := at
		p.MRs[i].State = state
		p.MRs[i].UpdatedAt = &atCopy
		switch state {
		case "merged":
			p.MRs[i].MergedAt = &atCopy
		case "closed":
			p.MRs[i].ClosedAt = &atCopy
		}
		return
	}
	s.t.Fatalf("glabtest: SetMRState: no MR iid=%d in project %d", iid, projectID)
}

// Requests is the "METHOD /path" log in request order, for asserting
// idempotency: several later tasks run reconcile twice and require zero
// writes on the second pass.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.requests))
	copy(out, s.requests)
	return out
}

// handle is the single entry point: (1) check PRIVATE-TOKEN, (2) consult the
// FailNext queue, (3) match METHOD /api/v4/... by splitting the escaped
// path, (4) write JSON. EscapedPath, not Path, so a %2F-encoded nested file
// path (see GetRawFile) survives as one segment instead of being split by
// its decoded literal slash.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()

	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+path)
	s.mu.Unlock()

	if r.Header.Get("PRIVATE-TOKEN") != s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "401 Unauthorized"})
		return
	}
	if status, ok := s.consumeFailure(r.Method, path); ok {
		writeJSON(w, status, map[string]string{"message": "glabtest: injected failure"})
		return
	}

	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 3 || segs[0] != "api" || segs[1] != "v4" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "glabtest: no route"})
		return
	}

	switch {
	case r.Method == http.MethodGet && len(segs) == 3 && segs[2] == "user":
		writeJSON(w, http.StatusOK, s.currentUser())
	case r.Method == http.MethodGet && len(segs) == 3 && segs[2] == "projects":
		s.handleListProjects(w, r)
	case r.Method == http.MethodGet && len(segs) == 8 && segs[2] == "projects" &&
		segs[4] == "repository" && segs[5] == "files" && segs[7] == "raw":
		s.handleGetRawFile(w, r, segs[3], segs[6])
	case r.Method == http.MethodGet && len(segs) == 6 && segs[2] == "projects" &&
		segs[4] == "repository" && segs[5] == "tree":
		s.handleDirExists(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 6 && segs[2] == "projects" &&
		segs[4] == "members" && segs[5] == "all":
		s.handleListMembers(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 5 && segs[2] == "projects" && segs[4] == "hooks":
		s.handleListHooks(w, r, segs[3])
	case r.Method == http.MethodPost && len(segs) == 5 && segs[2] == "projects" && segs[4] == "hooks":
		s.handleCreateHook(w, r, segs[3])
	case r.Method == http.MethodPut && len(segs) == 6 && segs[2] == "projects" && segs[4] == "hooks":
		s.handleEditHook(w, r, segs[3], segs[5])
	case r.Method == http.MethodPost && len(segs) == 6 && segs[2] == "projects" &&
		segs[4] == "repository" && segs[5] == "branches":
		s.handleCreateBranch(w, r, segs[3])
	case r.Method == http.MethodPost && len(segs) == 6 && segs[2] == "projects" &&
		segs[4] == "repository" && segs[5] == "commits":
		s.handleCreateCommit(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 5 && segs[2] == "projects" && segs[4] == "merge_requests":
		s.handleListMRs(w, r, segs[3])
	case r.Method == http.MethodPost && len(segs) == 5 && segs[2] == "projects" && segs[4] == "merge_requests":
		s.handleCreateMR(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 5 && segs[2] == "projects" && segs[4] == "issues":
		s.handleListIssues(w, r, segs[3])
	case r.Method == http.MethodPost && len(segs) == 5 && segs[2] == "projects" && segs[4] == "issues":
		s.handleCreateIssue(w, r, segs[3])
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "glabtest: no route"})
	}
}

func (s *Server) currentUser() glab.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Me
}

func (s *Server) consumeFailure(method, path string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.fails {
		f := &s.fails[i]
		if f.method != method || !strings.HasPrefix(path, f.pathPrefix) {
			continue
		}
		if f.always {
			return f.status, true
		}
		if f.times > 0 {
			f.times--
			return f.status, true
		}
	}
	return 0, false
}

// project looks up a project by its string ID from the URL, locking for the
// duration of the call. Callers that need to mutate the project take s.mu
// themselves afterward.
func (s *Server) project(idStr string) (*Project, bool) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	return p, ok
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ids := make([]int64, 0, len(s.projects))
	for id := range s.projects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	ps := make([]glab.Project, 0, len(ids))
	for _, id := range ids {
		ps = append(ps, s.projects[id].Project)
	}
	s.mu.Unlock()
	paginateWrite(w, r, ps)
}

func (s *Server) handleGetRawFile(w http.ResponseWriter, r *http.Request, idStr, encPath string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	filePath, err := url.PathUnescape(encPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad path encoding"})
		return
	}
	ref := r.URL.Query().Get("ref")

	s.mu.Lock()
	content, ok := p.Files[ref][filePath]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 File Not Found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) handleDirExists(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	dirPath := r.URL.Query().Get("path")
	ref := r.URL.Query().Get("ref")

	s.mu.Lock()
	var found bool
	for name := range p.Files[ref] {
		if strings.HasPrefix(name, dirPath+"/") {
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Tree Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, []map[string]string{{"name": "entry"}})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	s.mu.Lock()
	members := append([]glab.Member{}, p.Members...)
	s.mu.Unlock()
	paginateWrite(w, r, members)
}

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	s.mu.Lock()
	hooks := append([]glab.Hook{}, p.Hooks...)
	s.mu.Unlock()
	paginateWrite(w, r, hooks)
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	var opts glab.HookOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}

	s.mu.Lock()
	id := s.nextID
	s.nextID++
	h := glab.Hook{
		ID: id, URL: opts.URL, IssuesEvents: opts.IssuesEvents, NoteEvents: opts.NoteEvents,
		MergeRequestsEvents: opts.MergeRequestsEvents, PushEvents: opts.PushEvents,
		EnableSSLVerification: opts.EnableSSLVerification,
	}
	p.Hooks = append(p.Hooks, h)
	p.hookTokens[id] = opts.Token
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleEditHook(w http.ResponseWriter, r *http.Request, idStr, hookIDStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	hookID, err := strconv.ParseInt(hookIDStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Hook Not Found"})
		return
	}
	var opts glab.HookOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}

	s.mu.Lock()
	var updated *glab.Hook
	for i := range p.Hooks {
		if p.Hooks[i].ID != hookID {
			continue
		}
		p.Hooks[i] = glab.Hook{
			ID: hookID, URL: opts.URL, IssuesEvents: opts.IssuesEvents, NoteEvents: opts.NoteEvents,
			MergeRequestsEvents: opts.MergeRequestsEvents, PushEvents: opts.PushEvents,
			EnableSSLVerification: opts.EnableSSLVerification,
		}
		p.hookTokens[hookID] = opts.Token
		updated = &p.Hooks[i]
		break
	}
	s.mu.Unlock()
	if updated == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Hook Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, *updated)
}

func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	branch := r.URL.Query().Get("branch")
	ref := r.URL.Query().Get("ref")

	s.mu.Lock()
	if p.Branches[branch] {
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Branch already exists"})
		return
	}
	p.Branches[branch] = true
	if p.Files[branch] == nil {
		p.Files[branch] = map[string][]byte{}
	}
	for path, content := range p.Files[ref] {
		p.Files[branch][path] = content
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, glab.Branch{Name: branch})
}

func (s *Server) handleCreateCommit(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	var opts glab.CommitOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}

	s.mu.Lock()
	if p.Files[opts.Branch] == nil {
		p.Files[opts.Branch] = map[string][]byte{}
	}
	p.Branches[opts.Branch] = true
	for _, act := range opts.Actions {
		p.Files[opts.Branch][act.FilePath] = []byte(act.Content)
	}
	id := s.nextID
	s.nextID++
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, glab.Commit{ID: fmt.Sprintf("%040x", id)})
}

func (s *Server) handleListMRs(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	q := r.URL.Query()
	srcFilter := q.Get("source_branch")
	stateFilter := q.Get("state")

	s.mu.Lock()
	out := make([]glab.MergeRequest, 0, len(p.MRs))
	for _, mr := range p.MRs {
		if srcFilter != "" && mr.SourceBranch != srcFilter {
			continue
		}
		if stateFilter != "" && stateFilter != "all" && mr.State != stateFilter {
			continue
		}
		out = append(out, mr)
	}
	s.mu.Unlock()
	paginateWrite(w, r, out)
}

func (s *Server) handleCreateMR(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	var opts glab.MROptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}

	s.mu.Lock()
	p.nextMRIID++
	iid := p.nextMRIID
	mr := glab.MergeRequest{
		IID: iid, Title: opts.Title, Description: opts.Description, State: "opened",
		SourceBranch: opts.SourceBranch, TargetBranch: opts.TargetBranch,
		WebURL: fmt.Sprintf("%s/-/merge_requests/%d", p.WebURL, iid),
	}
	p.MRs = append(p.MRs, mr)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, mr)
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	q := r.URL.Query()
	stateFilter := q.Get("state")
	labelsFilter := q.Get("labels")

	s.mu.Lock()
	out := make([]glab.Issue, 0, len(p.Issues))
	for _, is := range p.Issues {
		if stateFilter != "" && stateFilter != "all" && is.State != stateFilter {
			continue
		}
		if labelsFilter != "" && !hasAllLabels(is.Labels, strings.Split(labelsFilter, ",")) {
			continue
		}
		out = append(out, is)
	}
	s.mu.Unlock()
	paginateWrite(w, r, out)
}

func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		var found bool
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request, idStr string) {
	p, ok := s.project(idStr)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
		return
	}
	var opts glab.IssueOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad request body"})
		return
	}

	s.mu.Lock()
	p.nextIssueIID++
	iid := p.nextIssueIID
	var labels []string
	if opts.Labels != "" {
		labels = strings.Split(opts.Labels, ",")
	}
	is := glab.Issue{
		IID: iid, Title: opts.Title, State: "opened", Labels: labels,
		WebURL: fmt.Sprintf("%s/-/issues/%d", p.WebURL, iid),
	}
	p.Issues = append(p.Issues, is)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, is)
}

// paginateWrite slices items per the page/per_page query params (default
// page 1, per_page 100, matching glab's paginate) and sets X-Next-Page when
// more remain.
func paginateWrite[T any](w http.ResponseWriter, r *http.Request, items []T) {
	page := queryIntDefault(r, "page", 1)
	perPage := queryIntDefault(r, "per_page", 100)
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 100
	}

	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	if end < len(items) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}

	out := make([]T, end-start)
	copy(out, items[start:end])
	writeJSON(w, http.StatusOK, out)
}

func queryIntDefault(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
