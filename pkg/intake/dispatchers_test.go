package intake

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogDispatcherNeverErrorsAndLogsTheTrigger(t *testing.T) {
	var buf logCapture
	log := slog.New(slog.NewTextHandler(&buf, nil))
	d := NewLogDispatcher(log)

	o := OrderRequest{Trigger: "issue-triage", Project: "group/repo", Rig: "group-repo", BeadAnchor: "gonk:1:issue:2"}
	if err := d.FireOrder(context.Background(), o); err != nil {
		t.Fatalf("FireOrder = %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "issue-triage") || !strings.Contains(got, "group/repo") {
		t.Errorf("log line = %q, want it to name the trigger and project", got)
	}
}

// A nil *slog.Logger must not panic (every existing Dispatch/Reconciler test
// leaves Log unset; LogDispatcher should tolerate the same).
func TestLogDispatcherToleratesNilLogger(t *testing.T) {
	d := &LogDispatcher{}
	if err := d.FireOrder(context.Background(), OrderRequest{Trigger: "t", Project: "p", Rig: "r"}); err != nil {
		t.Fatalf("FireOrder = %v", err)
	}
}

// OD-A (resolved by Plan 04 Task 2): POST /v0/city/{cityName}/order/gonk-dispatch/run,
// body {"vars": {...}}, no per-route auth.
func TestHTTPDispatcherPostsOrderAsVars(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewHTTPDispatcher(srv.URL, nil)
	o := OrderRequest{
		Trigger: "issue-triage", Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		IssueIID: 5, SessionKey: "gonk-1-issue-5", BeadAnchor: "gonk:1:issue:5",
	}
	if err := d.FireOrder(context.Background(), o); err != nil {
		t.Fatalf("FireOrder = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v0/city/gonk/order/gonk-dispatch/run" {
		t.Errorf("path = %q", gotPath)
	}
	vars, ok := gotBody["vars"].(map[string]any)
	if !ok {
		t.Fatalf("body = %+v, want a top-level \"vars\" object", gotBody)
	}
	if vars["bead_anchor"] != "gonk:1:issue:5" || vars["trigger"] != "issue-triage" {
		t.Errorf("vars = %+v, want the OrderRequest fields", vars)
	}
}

func TestHTTPDispatcherErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewHTTPDispatcher(srv.URL, nil)
	err := d.FireOrder(context.Background(), OrderRequest{Trigger: "t", Project: "p", Rig: "r"})
	if err == nil {
		t.Fatal("FireOrder = nil, want an error on a 500")
	}
}

// recordingDenyGL captures AddIssueLabel calls without a real GitLab.
type recordingDenyGL struct {
	calls []struct {
		projectID, issueIID int64
		label               string
	}
}

func (g *recordingDenyGL) AddIssueLabel(_ context.Context, projectID, issueIID int64, label string) error {
	g.calls = append(g.calls, struct {
		projectID, issueIID int64
		label               string
	}{projectID, issueIID, label})
	return nil
}

func TestGitLabDenyLabelerAppliesDefaultLabel(t *testing.T) {
	gl := &recordingDenyGL{}
	l := NewDenyLabeler(gl)
	if err := l.ApplyDenyLabel(context.Background(), 42, 7, "ladder-exhausted"); err != nil {
		t.Fatalf("ApplyDenyLabel = %v", err)
	}
	if len(gl.calls) != 1 || gl.calls[0].label != DefaultDenyLabel {
		t.Fatalf("calls = %+v, want one call with label %q", gl.calls, DefaultDenyLabel)
	}
	if gl.calls[0].projectID != 42 || gl.calls[0].issueIID != 7 {
		t.Fatalf("calls = %+v, want project 42 issue 7", gl.calls)
	}
}

func TestGitLabDenyLabelerHonoursOverrideLabel(t *testing.T) {
	gl := &recordingDenyGL{}
	l := &GitLabDenyLabeler{GL: gl, Label: "team::denied"}
	if err := l.ApplyDenyLabel(context.Background(), 1, 1, "reason"); err != nil {
		t.Fatal(err)
	}
	if gl.calls[0].label != "team::denied" {
		t.Errorf("label = %q, want the override", gl.calls[0].label)
	}
}

// logCapture is a minimal io.Writer so the LogDispatcher test can assert on
// slog output without depending on slog's internal format.
type logCapture struct{ b []byte }

func (c *logCapture) Write(p []byte) (int, error) {
	c.b = append(c.b, p...)
	return len(p), nil
}
func (c *logCapture) String() string { return string(c.b) }
