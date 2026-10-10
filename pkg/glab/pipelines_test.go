package glab

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// recorder answers every request with a canned status/body and remembers the
// requests, so a test can assert method + path + query + headers + body.
type recorder struct {
	mu     sync.Mutex
	status int
	body   string
	header map[string]string
	reqs   []recorded
}

type recorded struct {
	method, path string
	query        url.Values
	header       http.Header
	body         string
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.Query(), r.Header.Clone(), string(b)})
	st, body, hdr := rc.status, rc.body, rc.header
	rc.mu.Unlock()
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	if st == 0 {
		st = 200
	}
	w.WriteHeader(st)
	_, _ = io.WriteString(w, body)
}

func (rc *recorder) only(t *testing.T) recorded {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) != 1 {
		t.Fatalf("requests = %d, want exactly 1: %+v", len(rc.reqs), rc.reqs)
	}
	return rc.reqs[0]
}

func (rc *recorder) none(t *testing.T) {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) != 0 {
		t.Fatalf("want no request, got %+v", rc.reqs)
	}
}

func setup(t *testing.T, status int, body string) (*Client, *recorder) {
	t.Helper()
	rc := &recorder{status: status, body: body}
	return newTestClient(t, rc), rc
}

func wantReq(t *testing.T, got recorded, method, path string, q map[string]string) {
	t.Helper()
	if got.method != method || got.path != path {
		t.Fatalf("request = %s %s, want %s %s", got.method, got.path, method, path)
	}
	for k, v := range q {
		if g := got.query.Get(k); g != v {
			t.Errorf("query %s = %q, want %q (all: %v)", k, g, v, got.query)
		}
	}
}

const notFoundBody = `{"message":"404 Not found"}`

// Synthetic, written from GitLab's documented pipeline schema.
const pipelineJSON = `{"id":501,"iid":12,"project_id":9,"status":"failed","source":"merge_request_event",
"ref":"feat/x","sha":"` + sha1a + `","before_sha":"` + sha1b + `","tag":false,"yaml_errors":"jobs:build config contains unknown keys",
"failure_reason":"config_error","web_url":"https://gl.example/g/p/-/pipelines/501",
"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:05:05Z",
"started_at":"2026-01-02T03:04:10Z","finished_at":"2026-01-02T03:05:00Z","duration":50.5,
"user":{"id":3,"username":"dev"}}`

const (
	sha1a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha1b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestGetPipeline(t *testing.T) {
	c, rc := setup(t, 200, pipelineJSON)
	p, err := c.GetPipeline(context.Background(), 9, 501)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/pipelines/501", nil)
	if p.ID != 501 || p.IID != 12 || p.ProjectID != 9 || p.Status != "failed" || p.Source != "merge_request_event" ||
		p.Ref != "feat/x" || p.SHA != sha1a || p.BeforeSHA != sha1b || p.Tag ||
		p.YamlErrors == "" || p.FailureReason != "config_error" || p.WebURL == "" ||
		p.StartedAt == nil || p.FinishedAt == nil || p.Duration == nil || *p.Duration != 50.5 ||
		p.User.Username != "dev" || p.CreatedAt.IsZero() {
		t.Fatalf("decoded pipeline = %+v", p)
	}
}

func TestGetPipelineNotFoundBodyIsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	p, err := c.GetPipeline(context.Background(), 9, 501)
	if !IsNotFound(err) || p != nil {
		t.Fatalf("got %+v, %v; want nil + IsNotFound", p, err)
	}
}

func TestGetPipelineWrongIDIsAnError(t *testing.T) {
	// A 200 whose body is a different object (or an error shape that parses).
	c, _ := setup(t, 200, `{"message":"404 Not found"}`)
	if p, err := c.GetPipeline(context.Background(), 9, 501); err == nil {
		t.Fatalf("got %+v, nil; want an error for a reply with no matching id", p)
	}
}

func TestListPipelines(t *testing.T) {
	c, rc := setup(t, 200, `[{"id":7,"status":"success","ref":"main","sha":"`+sha1a+`","source":"push"}]`)
	ps, err := c.ListPipelines(context.Background(), 9, "main", "success", 1)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/pipelines",
		map[string]string{"ref": "main", "status": "success", "per_page": "1", "order_by": "id", "sort": "desc"})
	if len(ps) != 1 || ps[0].ID != 7 || ps[0].SHA != sha1a || ps[0].Source != "push" {
		t.Fatalf("pipelines = %+v", ps)
	}
}

func TestListPipelinesPerPageClamped(t *testing.T) {
	for in, want := range map[int]string{0: "20", -3: "20", 500: "100", 100: "100", 5: "5"} {
		c, rc := setup(t, 200, `[]`)
		if _, err := c.ListPipelines(context.Background(), 9, "main", "", in); err != nil {
			t.Fatal(err)
		}
		if got := rc.only(t).query.Get("per_page"); got != want {
			t.Errorf("perPage %d -> per_page=%q, want %q", in, got, want)
		}
	}
}

func TestListPipelinesIgnoredFilterIsAnError(t *testing.T) {
	// A server that ignores ?status=success would make "the newest success" a lie.
	c, _ := setup(t, 200, `[{"id":8,"status":"failed","ref":"main"}]`)
	if ps, err := c.ListPipelines(context.Background(), 9, "main", "success", 1); err == nil {
		t.Fatalf("got %+v, nil; want an error when the status filter was ignored", ps)
	}
	c, _ = setup(t, 200, `[{"id":8,"status":"success","ref":"other"}]`)
	if ps, err := c.ListPipelines(context.Background(), 9, "main", "success", 1); err == nil {
		t.Fatalf("got %+v, nil; want an error when the ref filter was ignored", ps)
	}
}

func TestListPipelines404IsAnErrorNotEmpty(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	ps, err := c.ListPipelines(context.Background(), 9, "main", "", 5)
	if !IsNotFound(err) || ps != nil {
		t.Fatalf("got %+v, %v", ps, err)
	}
}

func TestListMRPipelines(t *testing.T) {
	c, rc := setup(t, 200, `[{"id":31,"status":"failed","ref":"refs/merge-requests/4/head","source":"merge_request_event"}]`)
	ps, err := c.ListMRPipelines(context.Background(), 9, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/merge_requests/4/pipelines", map[string]string{"per_page": "100"})
	if len(ps) != 1 || ps[0].ID != 31 || ps[0].Status != "failed" {
		t.Fatalf("pipelines = %+v", ps)
	}
}

// Synthetic job, from GitLab's documented job schema (runner object included).
const jobsJSON = `[{"id":900,"name":"unit","stage":"test","status":"failed","failure_reason":"script_failure",
"allow_failure":false,"ref":"main","tag":false,"web_url":"https://gl.example/j/900",
"created_at":"2026-01-02T03:00:00Z","started_at":"2026-01-02T03:01:00Z","finished_at":"2026-01-02T03:02:00Z","duration":60.25,
"tag_list":["docker","gpu"],
"runner":{"id":17,"description":"shared-runner-a","name":"gitlab-runner","runner_type":"instance_type","is_shared":true,"active":true,"paused":false,"online":true,"status":"online"},
"runner_manager":{"id":88,"system_id":"r_abc123","version":"18.10.0","platform":"linux","architecture":"amd64"},
"pipeline":{"id":501,"ref":"main","sha":"` + sha1a + `"}},
{"id":901,"name":"lint","stage":"test","status":"failed","failure_reason":"stuck_or_timeout_failure","allow_failure":true,"runner":null,"tag_list":[],"pipeline":{"id":501}}]`

func TestListPipelineJobsQueryAndFields(t *testing.T) {
	c, rc := setup(t, 200, jobsJSON)
	jobs, err := c.ListPipelineJobs(context.Background(), 9, 501, "failed", false)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/pipelines/501/jobs",
		map[string]string{"scope[]": "failed", "include_retried": "false"})
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	j := jobs[0]
	if j.ID != 900 || j.Name != "unit" || j.Stage != "test" || j.Status != "failed" ||
		j.FailureReason != "script_failure" || j.AllowFailure || j.Ref != "main" || j.WebURL == "" ||
		j.StartedAt == nil || j.FinishedAt == nil || j.Duration == nil || *j.Duration != 60.25 ||
		j.Pipeline.ID != 501 || j.Pipeline.SHA != sha1a {
		t.Fatalf("job = %+v", j)
	}
	if len(j.TagList) != 2 || j.TagList[0] != "docker" || j.TagList[1] != "gpu" {
		t.Fatalf("tag_list = %v", j.TagList)
	}
	r := j.Runner
	if r == nil || r.ID != 17 || r.Description != "shared-runner-a" || r.Name != "gitlab-runner" ||
		r.RunnerType != "instance_type" || !r.IsShared || !r.Active || r.Paused || !r.Online || r.Status != "online" {
		t.Fatalf("runner = %+v", r)
	}
	m := j.RunnerManager
	if m == nil || m.ID != 88 || m.SystemID != "r_abc123" || m.Version != "18.10.0" || m.Platform != "linux" || m.Architecture != "amd64" {
		t.Fatalf("runner_manager = %+v", m)
	}
	// A job that never reached a runner: null runner decodes to nil, not a zero struct.
	if jobs[1].Runner != nil || jobs[1].RunnerManager != nil || !jobs[1].AllowFailure || jobs[1].FailureReason != "stuck_or_timeout_failure" {
		t.Fatalf("job[1] = %+v", jobs[1])
	}
}

func TestListPipelineJobsIncludeRetriedAndNoScope(t *testing.T) {
	c, rc := setup(t, 200, `[]`)
	if _, err := c.ListPipelineJobs(context.Background(), 9, 501, "", true); err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "GET", "/api/v4/projects/9/pipelines/501/jobs", map[string]string{"include_retried": "true"})
	if _, has := r.query["scope[]"]; has {
		t.Errorf("scope[] sent for an empty scope: %v", r.query)
	}
}

func TestListPipelineJobs404IsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	if jobs, err := c.ListPipelineJobs(context.Background(), 9, 501, "failed", false); !IsNotFound(err) || jobs != nil {
		t.Fatalf("got %+v, %v", jobs, err)
	}
}

func TestListPipelineJobsPaginates(t *testing.T) {
	page := 0
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			w.Header().Set("X-Next-Page", "2")
			_, _ = fmt.Fprint(w, `[{"id":1,"name":"a"}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"id":2,"name":"b"}]`)
	}))
	jobs, err := c.ListPipelineJobs(context.Background(), 9, 501, "failed", false)
	if err != nil || len(jobs) != 2 || jobs[1].Name != "b" {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
}

// ---- trace tail ----

func traceServer(t *testing.T, trace string, honourRange bool) (*Client, *recorder) {
	t.Helper()
	rc := &recorder{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, recorded{r.Method, r.URL.EscapedPath(), r.URL.Query(), r.Header.Clone(), ""})
		rc.mu.Unlock()
		var n int
		if honourRange && r.Header.Get("Range") != "" {
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=-%d", &n); err == nil && n < len(trace) {
				start := len(trace) - n
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(trace)-1, len(trace)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, trace[start:])
				return
			}
		}
		_, _ = io.WriteString(w, trace)
	}))
	return c, rc
}

func TestTraceTailSendsSuffixRange(t *testing.T) {
	c, rc := traceServer(t, "abc", true)
	if _, err := c.GetJobTraceTail(context.Background(), 9, 900, 1000, 10); err != nil {
		t.Fatal(err)
	}
	r := rc.only(t)
	wantReq(t, r, "GET", "/api/v4/projects/9/jobs/900/trace", nil)
	if got := r.header.Get("Range"); got != "bytes=-1000" {
		t.Fatalf("Range = %q, want bytes=-1000", got)
	}
}

func TestTraceTailSmallTraceIsWhole(t *testing.T) {
	c, _ := traceServer(t, "line1\nline2\n", false)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(tt.Data) != "line1\nline2\n" || tt.HeadTruncated || tt.TailTruncated || tt.BytesRead != 12 {
		t.Fatalf("tail = %+v (%q)", tt, tt.Data)
	}
}

func TestTraceTailKeepsLastBytesWhenRangeIgnored(t *testing.T) {
	// Server ignores Range; trace is shorter than maxRead but longer than tail.
	trace := strings.Repeat("x", 5000) + "THE-END"
	c, _ := traceServer(t, trace, false)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tt.Data) != 100 || !strings.HasSuffix(string(tt.Data), "THE-END") || !tt.HeadTruncated || tt.TailTruncated {
		t.Fatalf("len=%d suffix=%q head=%v tail=%v", len(tt.Data), tt.Data[len(tt.Data)-7:], tt.HeadTruncated, tt.TailTruncated)
	}
}

func TestTraceTailStopsAtMaxReadWhenRangeIgnored(t *testing.T) {
	// 9 MiB-shaped case, scaled: 10 000 bytes against maxRead 4 000. The read
	// must stop at maxRead, and say that Data is NOT the end of the trace.
	trace := strings.Repeat("a", 4000) + strings.Repeat("b", 6000)
	c, _ := traceServer(t, trace, false)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 4000, 50)
	if err != nil {
		t.Fatal(err)
	}
	if tt.BytesRead != 4000 || !tt.TailTruncated || !tt.HeadTruncated || len(tt.Data) != 50 ||
		strings.Contains(string(tt.Data), "b") {
		t.Fatalf("tail = bytesRead %d tailTrunc %v headTrunc %v len %d data %q", tt.BytesRead, tt.TailTruncated, tt.HeadTruncated, len(tt.Data), tt.Data)
	}
}

func TestTraceTailHonouredRangeReturnsRealEnd(t *testing.T) {
	// Server honours Range: 10 000-byte trace, maxRead 4 000 -> the LAST 4 000,
	// so the end is real (TailTruncated false) and the head is cut.
	trace := strings.Repeat("a", 6000) + strings.Repeat("b", 3990) + "THE-END!!!"
	c, _ := traceServer(t, trace, true)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 4000, 20)
	if err != nil {
		t.Fatal(err)
	}
	if string(tt.Data) != trace[len(trace)-20:] || tt.TailTruncated || !tt.HeadTruncated || tt.BytesRead != 4000 {
		t.Fatalf("tail = %+v data=%q", tt, tt.Data)
	}
}

func TestTraceTailPartialContentFromStartIsNotHeadTruncated(t *testing.T) {
	rc := &recorder{status: 206, body: "whole", header: map[string]string{"Content-Range": "bytes 0-4/5"}}
	c := newTestClient(t, rc)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 100, 100)
	if err != nil || tt.HeadTruncated || string(tt.Data) != "whole" {
		t.Fatalf("tail = %+v, %v", tt, err)
	}
}

func TestTraceTailPartialContentFromMidTraceIsHeadTruncated(t *testing.T) {
	// The whole 206 body fits in `tail`, so only Content-Range says bytes precede it.
	rc := &recorder{status: 206, body: "tail!", header: map[string]string{"Content-Range": "bytes 500-504/505"}}
	c := newTestClient(t, rc)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 100, 100)
	if err != nil || !tt.HeadTruncated || string(tt.Data) != "tail!" || tt.TailTruncated {
		t.Fatalf("tail = %+v, %v", tt, err)
	}
}

func TestTraceTail404IsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 100, 10)
	if !IsNotFound(err) || tt != nil {
		t.Fatalf("got %+v, %v; the 404 JSON must not become a trace", tt, err)
	}
}

func TestTraceTail416IsAnEmptyTrace(t *testing.T) {
	c, _ := setup(t, 416, "")
	tt, err := c.GetJobTraceTail(context.Background(), 9, 900, 100, 10)
	if err != nil || tt == nil || len(tt.Data) != 0 || tt.HeadTruncated {
		t.Fatalf("got %+v, %v", tt, err)
	}
}

func TestTraceTailRejectsNonPositiveBounds(t *testing.T) {
	c, rc := setup(t, 200, "x")
	for _, a := range [][2]int64{{0, 10}, {10, 0}, {-1, 5}} {
		if _, err := c.GetJobTraceTail(context.Background(), 9, 900, a[0], a[1]); err == nil {
			t.Errorf("maxRead=%d tail=%d: want error", a[0], a[1])
		}
	}
	rc.none(t)
}

func TestTailBufferBoundedAndKeepsLast(t *testing.T) {
	tb := newTailBuffer(10)
	max := 0
	for i := 0; i < 1000; i++ {
		_, _ = fmt.Fprintf(tb, "%03d", i%1000)
		if len(tb.buf) > max {
			max = len(tb.buf)
		}
	}
	if max > 2*10+3 {
		t.Fatalf("buffer grew to %d, want <= %d", max, 2*10+3)
	}
	if got := string(tb.Bytes()); got != "6997998999" {
		t.Fatalf("tail = %q, want the last 10 bytes written", got)
	}
	if tb.dropped != 1000*3-10 {
		t.Fatalf("dropped = %d", tb.dropped)
	}
}

// ---- commits ----

// The CI lint's tests are in pipefix_reads_test.go (CILintStatic): there is one
// lint call, and it is not a dry run (gonk-z6gb).

func TestListMRsForCommit(t *testing.T) {
	c, rc := setup(t, 200, `[{"iid":4,"state":"opened","source_branch":"feat/x","source_project_id":9,"target_project_id":9,"project_id":9,"labels":["a"],"sha":"`+sha1a+`"}]`)
	mrs, err := c.ListMRsForCommit(context.Background(), 9, sha1a)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/repository/commits/"+sha1a+"/merge_requests", nil)
	m := mrs[0]
	if len(mrs) != 1 || m.IID != 4 || m.State != "opened" || m.SourceBranch != "feat/x" || m.SourceProjectID != 9 ||
		m.TargetProjectID != 9 || m.ProjectID != 9 || len(m.Labels) != 1 || m.SHA != sha1a {
		t.Fatalf("mrs = %+v", mrs)
	}
}

func TestListMRsForCommit404AndNonSHA(t *testing.T) {
	c, rc := setup(t, 404, notFoundBody)
	if mrs, err := c.ListMRsForCommit(context.Background(), 9, sha1a); !IsNotFound(err) || mrs != nil {
		t.Fatalf("got %+v, %v", mrs, err)
	}
	if _, err := c.ListMRsForCommit(context.Background(), 9, "feat/x"); err == nil {
		t.Fatal("want error for a branch name")
	}
	if n := len(rc.reqs); n != 1 {
		t.Fatalf("requests = %d, want 1 (the non-SHA must not be sent)", n)
	}
}

func TestGetCommit(t *testing.T) {
	c, rc := setup(t, 200, `{"id":"`+sha1a+`","short_id":"aaaaaaaa","title":"fix thing","message":"fix thing\n\nbody","author_name":"A Dev","author_email":"a@example.com","authored_date":"2026-01-02T03:04:05Z","web_url":"https://gl.example/c"}`)
	cm, err := c.GetCommit(context.Background(), 9, sha1a)
	if err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/repository/commits/"+sha1a, nil)
	if cm.ID != sha1a || cm.Title != "fix thing" || cm.Message != "fix thing\n\nbody" || cm.AuthorName != "A Dev" ||
		cm.AuthorEmail != "a@example.com" || cm.AuthoredDate == nil || cm.ShortID != "aaaaaaaa" {
		t.Fatalf("commit = %+v", cm)
	}
}

func TestGetCommitWrongIDAnd404(t *testing.T) {
	c, _ := setup(t, 200, `{"id":"`+sha1b+`","title":"other"}`)
	if cm, err := c.GetCommit(context.Background(), 9, sha1a); err == nil {
		t.Fatalf("got %+v, nil; want error for a reply with another id", cm)
	}
	c, _ = setup(t, 404, notFoundBody)
	if cm, err := c.GetCommit(context.Background(), 9, sha1a); !IsNotFound(err) || cm != nil {
		t.Fatalf("got %+v, %v", cm, err)
	}
}

func TestGetCommitRejectsNonSHAWithoutARequest(t *testing.T) {
	c, rc := setup(t, 200, `{}`)
	if _, err := c.GetCommit(context.Background(), 9, "abc123"); err == nil {
		t.Fatal("want error for an abbreviated sha")
	}
	rc.none(t)
}
