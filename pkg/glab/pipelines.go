package glab

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Pipeline, job, trace, CI-lint and commit reads for pipeline triage
// (gonk-tios). Every call here is a GET. Each asserts on a field it depends on
// (the id it asked for, the SHA it asked for) rather than on the reply merely
// parsing: GitLab answers errors as JSON too, and a proxy can answer 200 with
// the wrong object.

const maxListPerPage = 100

// GetPipeline fetches one pipeline, including the detail-only fields
// (started/finished, user, yaml_errors). The returned id must equal the one
// asked for.
func (c *Client) GetPipeline(ctx context.Context, projectID, pipelineID int64) (*Pipeline, error) {
	var p Pipeline
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/pipelines/%d", projectID, pipelineID),
	}, &p); err != nil {
		return nil, err
	}
	if p.ID != pipelineID {
		return nil, fmt.Errorf("gitlab: GET pipeline %d: reply carries id %d", pipelineID, p.ID)
	}
	return &p, nil
}

// ListPipelines returns ONE page (newest first: order_by=id, sort=desc) of at
// most perPage pipelines (1..100) on ref, optionally filtered by status. It is
// deliberately not paginated: callers want "the newest N", and walking a long
// history to find them is the unbounded read this avoids.
//
// ref and status are re-checked on every returned pipeline. If a proxy or an
// older GitLab ignored the filter, "the newest success" would silently be the
// newest anything, so a mismatch is an error rather than a wrong answer.
func (c *Client) ListPipelines(ctx context.Context, projectID int64, ref, status string, perPage int) ([]Pipeline, error) {
	q := map[string]string{
		"order_by": "id",
		"sort":     "desc",
		"per_page": strconv.Itoa(clampPerPage(perPage)),
	}
	if ref != "" {
		q["ref"] = ref
	}
	if status != "" {
		q["status"] = status
	}
	var ps []Pipeline
	rq := request{method: "GET", path: fmt.Sprintf("/api/v4/projects/%d/pipelines", projectID), query: q}
	if err := c.getJSON(ctx, rq, &ps); err != nil {
		return nil, err
	}
	for _, p := range ps {
		if (ref != "" && p.Ref != ref) || (status != "" && p.Status != status) {
			return nil, fmt.Errorf("gitlab: GET %s: filter ref=%q status=%q ignored: got pipeline %d ref=%q status=%q",
				rq.path, ref, status, p.ID, p.Ref, p.Status)
		}
	}
	return ps, nil
}

// ListMRPipelines returns ONE page (up to 100, newest first) of the pipelines
// of a merge request. Same single-page rationale as ListPipelines.
func (c *Client) ListMRPipelines(ctx context.Context, projectID, mrIID int64) ([]Pipeline, error) {
	var ps []Pipeline
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/merge_requests/%d/pipelines", projectID, mrIID),
		query:  map[string]string{"per_page": strconv.Itoa(maxListPerPage)},
	}, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

// ListPipelineJobs returns every job of a pipeline. scope is a single GitLab
// job status ("failed", "success", ...; empty means all). includeRetried
// false returns only the LATEST attempt of each job, which is what a failure
// fingerprint wants; true also returns superseded attempts.
func (c *Client) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64, scope string, includeRetried bool) ([]Job, error) {
	q := map[string]string{"include_retried": strconv.FormatBool(includeRetried)}
	if scope != "" {
		q["scope[]"] = scope
	}
	return paginate[Job](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/pipelines/%d/jobs", projectID, pipelineID),
		query:  q,
	})
}

// TraceTail is the bounded result of GetJobTraceTail.
type TraceTail struct {
	// Data is at most `tail` bytes: the last bytes that were read.
	Data []byte
	// BytesRead is how many body bytes were consumed (not the trace's size).
	BytesRead int64
	// HeadTruncated: bytes precede Data in the trace.
	HeadTruncated bool
	// TailTruncated: the trace continues PAST what was read, so Data is NOT the
	// end of the trace. Only possible when the server ignored the Range request
	// and the trace is longer than maxRead.
	TailTruncated bool
}

// GetJobTraceTail reads the END of a job's log without ever holding or reading
// more than maxRead bytes, and returns at most the last `tail` of them.
//
// The bound is two layers. (1) The request carries `Range: bytes=-<maxRead>`
// (a suffix range), so a server that honours it (206) sends only the last
// maxRead bytes. (2) Whatever the server does, the body is read through a
// capped reader that stops at maxRead and feeds a tail buffer, so a server
// that ignores Range (plain 200) cannot make this read more than maxRead
// bytes. In that fallback a trace longer than maxRead yields the first maxRead
// bytes' tail with TailTruncated=true, and the caller must treat it as not
// the end of the log.
//
// NOTE: whether GitLab's /jobs/:id/trace honours Range is NOT verified against
// a live instance (no credentials when this was written); layer (2) is what
// the guarantee rests on.
func (c *Client) GetJobTraceTail(ctx context.Context, projectID, jobID, maxRead, tail int64) (*TraceTail, error) {
	if maxRead <= 0 || tail <= 0 {
		return nil, fmt.Errorf("gitlab: job trace: maxRead (%d) and tail (%d) must be positive", maxRead, tail)
	}
	if tail > maxRead {
		tail = maxRead
	}
	out := &TraceTail{}
	_, _, err := c.do(ctx, request{
		method:  "GET",
		path:    fmt.Sprintf("/api/v4/projects/%d/jobs/%d/trace", projectID, jobID),
		headers: map[string]string{"Range": "bytes=-" + strconv.FormatInt(maxRead, 10)},
		consume: func(resp *http.Response) error {
			tb := newTailBuffer(tail)
			n, err := io.Copy(tb, io.LimitReader(resp.Body, maxRead))
			if err != nil {
				return err
			}
			out.Data, out.BytesRead = tb.Bytes(), n
			out.HeadTruncated = tb.dropped > 0
			if resp.StatusCode == http.StatusPartialContent {
				// A suffix range starts mid-trace unless the whole trace fit.
				if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start > 0 {
					out.HeadTruncated = true
				}
			}
			if n == maxRead {
				var probe [1]byte
				if m, _ := resp.Body.Read(probe[:]); m > 0 {
					out.TailTruncated = true
				}
			}
			return nil
		},
	})
	if err != nil {
		if ae := asAPIError(err); ae != nil && ae.Status == http.StatusRequestedRangeNotSatisfiable {
			// A suffix range on an empty trace.
			return &TraceTail{}, nil
		}
		return nil, err
	}
	return out, nil
}

// contentRangeStart parses the first byte offset from "bytes a-b/total".
func contentRangeStart(h string) (int64, bool) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "bytes ")
	i := strings.IndexByte(h, '-')
	if i <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(h[:i], 10, 64)
	return n, err == nil && n >= 0
}

// tailBuffer keeps the last `cap` bytes written to it. Memory is bounded by
// 2*cap plus one write: it compacts back to `cap` whenever it reaches 2*cap.
type tailBuffer struct {
	cap     int64
	buf     []byte
	dropped int64 // bytes discarded from the front
}

func newTailBuffer(cap int64) *tailBuffer { return &tailBuffer{cap: cap} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if int64(len(t.buf)) >= 2*t.cap {
		t.compact()
	}
	return len(p), nil
}

func (t *tailBuffer) compact() {
	if over := int64(len(t.buf)) - t.cap; over > 0 {
		t.dropped += over
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
}

// Bytes returns the last cap bytes (fewer if fewer were written).
func (t *tailBuffer) Bytes() []byte {
	t.compact()
	return t.buf
}

// CILint is the reply of the CI lint endpoint, as CILintStatic
// (pipefix_reads.go) returns it. There is ONE lint call in this package and it
// is not a dry run: see CILintStatic for why (gonk-z6gb).
type CILint struct {
	Valid      bool     `json:"valid"`
	Errors     []string `json:"errors"`
	Warnings   []string `json:"warnings"`
	MergedYAML string   `json:"merged_yaml"`
	// Includes: every file GitLab pulled in through `include:`, with the
	// commit it read it at.
	Includes []CILintInclude `json:"includes"`
}

// ListMRsForCommit lists the merge requests that contain a commit (GET
// .../repository/commits/:sha/merge_requests), every state; callers filter on
// State, SourceBranch and SourceProjectID.
func (c *Client) ListMRsForCommit(ctx context.Context, projectID int64, sha string) ([]MergeRequest, error) {
	if !IsCommitSHA(sha) {
		return nil, fmt.Errorf("gitlab: merge requests for commit: %q is not a full commit SHA", sha)
	}
	return paginate[MergeRequest](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/commits/%s/merge_requests", projectID, url.PathEscape(sha)),
	})
}

// GetCommit fetches one commit by FULL sha. The reply's id must equal sha.
func (c *Client) GetCommit(ctx context.Context, projectID int64, sha string) (*Commit, error) {
	if !IsCommitSHA(sha) {
		return nil, fmt.Errorf("gitlab: get commit: %q is not a full commit SHA", sha)
	}
	var cm Commit
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/commits/%s", projectID, url.PathEscape(sha)),
	}, &cm); err != nil {
		return nil, err
	}
	if cm.ID != sha {
		return nil, fmt.Errorf("gitlab: get commit %s: reply carries id %q", sha, cm.ID)
	}
	return &cm, nil
}

func clampPerPage(n int) int {
	if n < 1 {
		return 20
	}
	if n > maxListPerPage {
		return maxListPerPage
	}
	return n
}
