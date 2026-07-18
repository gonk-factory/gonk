package intake

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
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
// body {"vars": {...}}. With no Signer configured, NO grant headers are sent --
// the legacy loopback / network-position path, unchanged.
func TestHTTPDispatcherPostsOrderAsVars(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	var gotCSRF, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotCSRF = r.Header.Get("X-GC-Request")
		gotGrant = r.Header.Get("X-GC-City-Write")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	defer srv.Close()

	d := NewHTTPDispatcher(srv.URL, nil, nil) // nil signer
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
	if gotCSRF != "" || gotGrant != "" {
		t.Errorf("nil-Signer request sent grant headers X-GC-Request=%q X-GC-City-Write=%q; want none", gotCSRF, gotGrant)
	}
	vars, ok := gotBody["vars"].(map[string]any)
	if !ok {
		t.Fatalf("body = %+v, want a top-level \"vars\" object", gotBody)
	}
	if vars["bead_anchor"] != "gonk:1:issue:5" || vars["trigger"] != "issue-triage" {
		t.Errorf("vars = %+v, want the OrderRequest fields", vars)
	}
	// Order-run vars are STRING-typed on the wire (Gas City namespaces each into
	// the exec's GC_WEBHOOK_ARG_* env, which is string-typed): project_id is the
	// string "1", not the JSON number 1.
	if pid, ok := vars["project_id"].(string); !ok || pid != "1" {
		t.Errorf("project_id var = %#v, want string \"1\"", vars["project_id"])
	}
}

// TestHTTPDispatcherSignsOrderRunWithVerifiableGrant is the money-path test: an
// intake HTTPDispatcher wired with a Signer must send BOTH the X-GC-Request CSRF
// header and an X-GC-City-Write grant that an independent reimplementation of
// the gascity write-auth server verifies with the matching public key, exactly
// as pkg/gcapi's own round-trip test does -- proving intake's dispatch is now
// authenticated because signing lives in the one gcapi client.
func TestHTTPDispatcherSignsOrderRunWithVerifiableGrant(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gcapi.NewSigner(priv, "gonk-write-1")
	if err != nil {
		t.Fatal(err)
	}

	var verified bool
	var gotVars map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		verifyGrantAsServer(t, pub, "gonk", r, body)
		verified = true
		var outer struct {
			Vars map[string]any `json:"vars"`
		}
		_ = json.Unmarshal(body, &outer)
		gotVars = outer.Vars
		_, _ = io.WriteString(w, `{"status":"queued","tracking_id":"t"}`)
	}))
	defer srv.Close()

	d := NewHTTPDispatcher(srv.URL, signer, nil)
	o := OrderRequest{
		Trigger: "issue-triage", Project: "group/repo", ProjectID: 7, Rig: "group-repo",
		IssueIID: 5, SessionKey: "gonk-7-issue-5", BeadAnchor: "gonk:7:issue:5",
	}
	if err := d.FireOrder(context.Background(), o); err != nil {
		t.Fatalf("FireOrder = %v", err)
	}
	if !verified {
		t.Fatal("server handler never verified a grant")
	}
	if gotVars["bead_anchor"] != "gonk:7:issue:5" {
		t.Errorf("bead_anchor var = %v", gotVars["bead_anchor"])
	}
}

func TestHTTPDispatcherErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewHTTPDispatcher(srv.URL, nil, nil)
	d.GC.MaxRetries = 0 // this test asserts the error, not the retry policy
	err := d.FireOrder(context.Background(), OrderRequest{Trigger: "t", Project: "p", Rig: "r"})
	if err == nil {
		t.Fatal("FireOrder = nil, want an error on a 500")
	}
}

// verifyGrantAsServer replays exactly what a gascity write-auth server does with
// the X-GC-City-Write header: CSRF gate, split+decode, ed25519.Verify over the
// raw payload bytes on the wire, then recompute the req digest from the wire
// method/path/query/body and compare. It fails the test on any check the server
// would reject. This mirrors pkg/gcapi's own server reimplementation (that one
// is in an internal test file, so it is transcribed here rather than imported).
func verifyGrantAsServer(t *testing.T, pub ed25519.PublicKey, city string, r *http.Request, body []byte) {
	t.Helper()
	if got := r.Header.Get("X-GC-Request"); got != "true" {
		t.Fatalf("X-GC-Request = %q, want \"true\" (CSRF gate always required on a signed mutation)", got)
	}
	token := r.Header.Get("X-GC-City-Write")
	if token == "" {
		t.Fatal("X-GC-City-Write header is empty (dispatch did not sign)")
	}
	seg := strings.Split(token, ".")
	if len(seg) != 2 || seg[0] == "" || seg[1] == "" {
		t.Fatalf("token is not two non-empty dot-separated segments: %q", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(seg[0])
	if err != nil {
		t.Fatalf("decode payload segment: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(seg[1])
	if err != nil {
		t.Fatalf("decode sig segment: %v", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("ed25519.Verify failed: signature does not match payload bytes on the wire")
	}
	var g struct {
		Aud  string `json:"aud"`
		City string `json:"city"`
		Jti  string `json:"jti"`
		Iat  int64  `json:"iat"`
		Exp  int64  `json:"exp"`
		Req  string `json:"req"`
	}
	if err := json.Unmarshal(payload, &g); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if g.Aud != "gc-city-write.v2" {
		t.Errorf("aud = %q, want gc-city-write.v2", g.Aud)
	}
	if g.City != city {
		t.Errorf("city = %q, want %q (must equal the {cityName} path segment)", g.City, city)
	}
	if g.Jti == "" {
		t.Error("jti is empty")
	}
	if g.Exp <= g.Iat {
		t.Errorf("exp (%d) must be > iat (%d)", g.Exp, g.Iat)
	}
	if want := serverReqDigest(r.Method, r.URL.Path, r.URL.RawQuery, body); g.Req != want {
		t.Fatalf("req digest mismatch:\n  grant.req = %s\n  server    = %s", g.Req, want)
	}
}

// serverReqDigest mirrors the gascity server's request binding:
// hex(sha256(method "\n" path [ "\n" canonicalQuery ] "\n" hex(sha256(body)))).
func serverReqDigest(method, path, rawQuery string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	preimage := method + "\n" + path
	if rawQuery != "" {
		if vals, err := url.ParseQuery(rawQuery); err == nil {
			if cq := vals.Encode(); cq != "" {
				preimage += "\n" + cq
			}
		} else {
			preimage += "\n" + rawQuery
		}
	}
	preimage += "\n" + hex.EncodeToString(bodyHash[:])
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
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
