package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// httpFixture wraps a fixture with a real httptest.Server driving the mux
// built by NewMux, so tests exercise the real request/response shapes rather
// than calling Service methods directly.
type httpFixture struct {
	*fixture
	srv   *httptest.Server
	token string
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	f := newTestService(t)
	mux, err := NewMux(f.svc, "test-token", "prev-token")
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &httpFixture{fixture: f, srv: srv, token: "test-token"}
}

func (h *httpFixture) do(method, path, bearer string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		h.t.Fatal(err)
	}
	return resp, buf.Bytes()
}

// ==================================================================
// Auth
// ==================================================================

func TestHTTPRequiresBearerToken(t *testing.T) {
	h := newHTTPFixture(t)
	resp, _ := h.do(http.MethodGet, meterapi.ProjectPath("group/repo"), "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer token: status = %d, want 401", resp.StatusCode)
	}
}

func TestHTTPAcceptsEitherRotationSlot(t *testing.T) {
	h := newHTTPFixture(t)
	for _, tok := range []string{"test-token", "prev-token"} {
		resp, _ := h.do(http.MethodGet, meterapi.ProjectPath("never/registered"), tok, nil)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("bearer %q was rejected, want either slot accepted", tok)
		}
	}
	resp, _ := h.do(http.MethodGet, meterapi.ProjectPath("never/registered"), "wrong-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token got status %d, want 401", resp.StatusCode)
	}
}

func TestHTTPHealthAndReadyAreUnauthenticated(t *testing.T) {
	h := newHTTPFixture(t)
	resp, _ := h.do(http.MethodGet, meterapi.HealthzPath, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz without a token: status = %d, want 200", resp.StatusCode)
	}
	// /readyz is unauthenticated but reports 503 until the first sync.
	resp, _ = h.do(http.MethodGet, meterapi.ReadyzPath, "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before any sync = %d, want 503", resp.StatusCode)
	}
	h.syncOnce()
	resp, _ = h.do(http.MethodGet, meterapi.ReadyzPath, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz after a sync = %d, want 200", resp.StatusCode)
	}
}

// ==================================================================
// PUT/GET/DELETE /v1/projects/{project}
// ==================================================================

func TestHTTPRegisterCodesAndShapes(t *testing.T) {
	h := newHTTPFixture(t)

	// 200: a valid, resolved config.
	resp, body := h.do(http.MethodPut, meterapi.ProjectPath("group/repo"), h.token, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo",
		GonkYML: simpleYAML("qwen-local, glm", 5),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid register: status = %d, body = %s", resp.StatusCode, body)
	}
	var pr meterapi.ProjectResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if pr.State != meterapi.StateActive {
		t.Fatalf("state = %q, want active", pr.State)
	}
	assertNoLeakedToken(t, body)

	// 422: an invalid .gonk.yml.
	resp, body = h.do(http.MethodPut, meterapi.ProjectPath("group/bad"), h.token, meterapi.ProjectRequest{
		Project: "group/bad", ProjectID: 2, Rig: "group-bad", GonkYML: "not valid [",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid config: status = %d, want 422; body=%s", resp.StatusCode, body)
	}
	var pr2 meterapi.ProjectResponse
	if err := json.Unmarshal(body, &pr2); err != nil {
		t.Fatalf("decode 422 body: %v; body=%s", err, body)
	}
	if pr2.State != meterapi.StateInvalid || pr2.Error == "" {
		t.Fatalf("422 body = %+v, want state=invalid with a non-empty error", pr2)
	}

	// 400: a malformed REQUEST (project_id missing), nothing recorded.
	resp, body = h.do(http.MethodPut, meterapi.ProjectPath("group/new"), h.token, meterapi.ProjectRequest{
		Project: "group/new", Rig: "group-new", GonkYML: simpleYAML("qwen-local", 5),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing project_id: status = %d, want 400; body=%s", resp.StatusCode, body)
	}
	if _, ok, _ := h.store.GetRegistration(context.Background(), "group/new"); ok {
		t.Fatal("a 400 (malformed request) must record nothing")
	}
}

func TestHTTPUnlimitedBudgetRendersNull(t *testing.T) {
	h := newHTTPFixture(t)
	_, body := h.do(http.MethodPut, meterapi.ProjectPath("solo/repo"), h.token, meterapi.ProjectRequest{
		Project: "solo/repo", ProjectID: 1, Rig: "solo-repo",
		GonkYML: "version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\n",
	})
	if !bytes.Contains(body, []byte(`"monthly_cost_usd":null`)) {
		t.Fatalf("unlimited budget did not render null: %s", body)
	}
}

func TestHTTPGetAndDeleteProject(t *testing.T) {
	h := newHTTPFixture(t)
	resp, _ := h.do(http.MethodGet, meterapi.ProjectPath("never/registered"), h.token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET an unregistered project: status = %d, want 404", resp.StatusCode)
	}

	h.do(http.MethodPut, meterapi.ProjectPath("group/repo"), h.token, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("qwen-local", 5),
	})
	resp, body := h.do(http.MethodGet, meterapi.ProjectPath("group/repo"), h.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET a registered project: status = %d, body=%s", resp.StatusCode, body)
	}

	resp, _ = h.do(http.MethodDelete, meterapi.ProjectPath("group/repo"), h.token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d, want 204", resp.StatusCode)
	}
	// Idempotent: deleting an unknown/already-deleted project is STILL 204.
	resp, _ = h.do(http.MethodDelete, meterapi.ProjectPath("group/repo"), h.token, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second DELETE: status = %d, want 204 (idempotent)", resp.StatusCode)
	}
}

// ==================================================================
// /v1/policy/decide, /v1/policy/outcome
// ==================================================================

func TestHTTPDecideShapes(t *testing.T) {
	h := newHTTPFixture(t)
	h.do(http.MethodPut, meterapi.ProjectPath("group/repo"), h.token, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("qwen-local, glm", 5),
	})
	h.syncOnce()

	resp, body := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
		Project: "group/repo", Rig: "group-repo", BeadID: "gk-1", SessionKey: "sess-1", Trigger: trigger,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run decision: status = %d, want 200 (defer/deny are ALSO 200); body=%s", resp.StatusCode, body)
	}
	var dr meterapi.DecideResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if dr.Decision != meterapi.DecisionRun || dr.ReservationID == "" {
		t.Fatalf("decide response = %+v, want a run with a reservation_id", dr)
	}
	assertNoLeakedToken(t, body)

	// A deny is STILL HTTP 200.
	resp, body = h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
		Project: "never/registered", Rig: "x", BeadID: "gk-1", SessionKey: "sess-1", Trigger: trigger,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deny decision: status = %d, want 200", resp.StatusCode)
	}
	var dr2 meterapi.DecideResponse
	if err := json.Unmarshal(body, &dr2); err != nil {
		t.Fatal(err)
	}
	if dr2.Decision != meterapi.DecisionDeny || len(dr2.Metadata) != 0 || dr2.KeyRef.SecretName != "" || dr2.ReservationID != "" {
		t.Fatalf("deny body = %+v, want empty metadata/key_ref/reservation_id", dr2)
	}

	// A malformed request (missing bead_id) is 400, no side effect.
	resp, _ = h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
		Project: "group/repo", Rig: "group-repo", SessionKey: "sess-1", Trigger: trigger,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing bead_id: status = %d, want 400", resp.StatusCode)
	}

	// Garbage JSON: 400.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+meterapi.DecidePath, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	gresp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gresp.Body.Close() }()
	if gresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage JSON: status = %d, want 400", gresp.StatusCode)
	}
}

func TestHTTPOutcomeCodes(t *testing.T) {
	h := newHTTPFixture(t)
	h.do(http.MethodPut, meterapi.ProjectPath("group/repo"), h.token, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("qwen-local, glm", 5),
	})
	h.syncOnce()
	_, decideBody := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
		Project: "group/repo", Rig: "group-repo", BeadID: "gk-1", SessionKey: "sess-1", Trigger: trigger,
	})
	var dr meterapi.DecideResponse
	if err := json.Unmarshal(decideBody, &dr); err != nil {
		t.Fatal(err)
	}

	resp, body := h.do(http.MethodPost, meterapi.OutcomePath, h.token, meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: dr.Attempt,
		Rung: dr.Rung, ReservationID: dr.ReservationID, Outcome: meterapi.OutcomeSuccess,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid outcome: status = %d, body=%s", resp.StatusCode, body)
	}

	resp, _ = h.do(http.MethodPost, meterapi.OutcomePath, h.token, meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: 1,
		Rung: "qwen-local", ReservationID: "unknown", Outcome: meterapi.OutcomeSuccess,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown reservation_id: status = %d, want 400", resp.StatusCode)
	}
}

// TestHTTPCostBeadHasNoProjectInThePath confirms meterapi.CostBeadPath's
// actual shape (bead_id only, no project) is honored: the project is
// DISCOVERED server-side, not required as a query parameter.
func TestHTTPCostBeadHasNoProjectInThePath(t *testing.T) {
	h := newHTTPFixture(t)
	if got := meterapi.CostBeadPath("gk-1"); got != "/v1/cost/bead/gk-1" {
		t.Fatalf("meterapi.CostBeadPath = %q, want no project component", got)
	}

	resp, _ := h.do(http.MethodGet, meterapi.CostBeadPath("gk-1"), h.token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cost for a never-attempted bead: status = %d, want 404", resp.StatusCode)
	}

	h.do(http.MethodPut, meterapi.ProjectPath("group/repo"), h.token, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: simpleYAML("qwen-local, glm", 5),
	})
	h.syncOnce()
	_, decideBody := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
		Project: "group/repo", Rig: "group-repo", BeadID: "gk-1", SessionKey: "sess-1", Trigger: trigger,
	})
	var dr meterapi.DecideResponse
	if err := json.Unmarshal(decideBody, &dr); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodPost, meterapi.OutcomePath, h.token, meterapi.OutcomeRequest{
		Project: "group/repo", BeadID: "gk-1", SessionKey: "sess-1", Attempt: dr.Attempt,
		Rung: dr.Rung, ReservationID: dr.ReservationID, Outcome: meterapi.OutcomeSuccess,
	})

	resp, body := h.do(http.MethodGet, meterapi.CostBeadPath("gk-1"), h.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cost for an attempted bead: status = %d, body=%s", resp.StatusCode, body)
	}
	var bc meterapi.BeadCostResponse
	if err := json.Unmarshal(body, &bc); err != nil {
		t.Fatal(err)
	}
	if bc.Project != "group/repo" || len(bc.Attempts) != 1 {
		t.Fatalf("bead cost = %+v, want project discovered as group/repo with 1 attempt", bc)
	}
}

// ==================================================================
// No response anywhere leaks a token.
// ==================================================================

func assertNoLeakedToken(t *testing.T, body []byte) {
	t.Helper()
	if bytes.Contains(body, []byte("sk-")) {
		t.Fatalf("response body leaks a token (contains \"sk-\"): %s", body)
	}
}
