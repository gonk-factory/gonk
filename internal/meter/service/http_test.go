package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/metrics"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// httpFixture wraps a fixture with a real httptest.Server driving the mux
// built by NewMux, so tests exercise the real request/response shapes rather
// than calling Service methods directly.
type httpFixture struct {
	*fixture
	srv     *httptest.Server
	token   string
	reg     *prometheus.Registry
	metrics *metrics.Metrics
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	f := newTestService(t)
	reg := prometheus.NewRegistry()
	mtr := metrics.New(reg)
	f.svc.SetMetrics(mtr)
	mux, err := NewMux(f.svc, "test-token", "prev-token", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &httpFixture{fixture: f, srv: srv, token: "test-token", reg: reg, metrics: mtr}
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
// Task 9: /metrics and /admin/spend/sync
// ==================================================================

// TestHTTPMetricsIsUnauthenticatedAndServesPrometheusText confirms /metrics
// joins /healthz and /readyz as the unauthenticated exceptions (meterapi.go's
// own endpoint doc comment), and that the registry mounted is PRIVATE: no
// default go_*/process_* collectors, matching cmd/gonk-intake's house style.
func TestHTTPMetricsIsUnauthenticatedAndServesPrometheusText(t *testing.T) {
	h := newHTTPFixture(t)
	resp, body := h.do(http.MethodGet, meterapi.MetricsPath, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics without a token: status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("gonk_meter_")) {
		t.Fatalf("/metrics body has no gonk_meter_ series: %s", body)
	}
	if bytes.Contains(body, []byte("go_goroutines")) {
		t.Fatalf("/metrics exposes the default go_* collector -- the registry must be private")
	}
}

// TestHTTPAdminSpendSyncIsAuthenticatedAndRejectsWrongMethod: unlike
// /healthz, /readyz, and /metrics, /admin/spend/sync sits behind the same
// bearer token as every other route (Task 9 Step 3b), and a non-POST request
// gets Go 1.22+ ServeMux's automatic 405 rather than the handler's own 200.
func TestHTTPAdminSpendSyncIsAuthenticatedAndRejectsWrongMethod(t *testing.T) {
	h := newHTTPFixture(t)
	resp, _ := h.do(http.MethodPost, meterapi.AdminSpendSyncPath, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer token: status = %d, want 401", resp.StatusCode)
	}
	resp, _ = h.do(http.MethodGet, meterapi.AdminSpendSyncPath, h.token, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/spend/sync: status = %d, want 405", resp.StatusCode)
	}
}

// TestHTTPAdminSpendSyncReportsHonestSpendAsOf covers the two response
// shapes in Task 9 Step 3b's table: a successful pass carries rows_ingested
// and unattributed; a failed one carries synced:false and error, omits
// rows_ingested/unattributed, and STILL reports the last GOOD spend_as_of --
// never zero -- with HTTP 200 either way (the endpoint itself did its job).
func TestHTTPAdminSpendSyncReportsHonestSpendAsOf(t *testing.T) {
	h := newHTTPFixture(t)

	resp, body := h.do(http.MethodPost, meterapi.AdminSpendSyncPath, h.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin spend sync: status = %d, body=%s", resp.StatusCode, body)
	}
	var sr meterapi.SpendSyncResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	if !sr.Synced || sr.RowsIngested == nil || sr.SpendAsOf.IsZero() {
		t.Fatalf("admin spend sync response = %+v, want synced with a non-zero spend_as_of and rows_ingested set", sr)
	}

	h.admin.SpendErr = errors.New("litellm unreachable")
	resp, body = h.do(http.MethodPost, meterapi.AdminSpendSyncPath, h.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed sync: status = %d, want 200 (the endpoint itself did its job)", resp.StatusCode)
	}
	var sr2 meterapi.SpendSyncResponse
	if err := json.Unmarshal(body, &sr2); err != nil {
		t.Fatal(err)
	}
	if sr2.Synced || sr2.Error == "" {
		t.Fatalf("failed sync response = %+v, want synced=false with an error", sr2)
	}
	if sr2.RowsIngested != nil || sr2.Unattributed != nil {
		t.Fatalf("failed sync response = %+v, want rows_ingested/unattributed OMITTED (we do not know), not present as zero", sr2)
	}
	if !sr2.SpendAsOf.Equal(sr.SpendAsOf) {
		t.Fatalf("failed sync spend_as_of = %v, want the last GOOD sync's %v (never zero just because this call failed)", sr2.SpendAsOf, sr.SpendAsOf)
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

// --- prompt-by-reference (gonk-mzd) -----------------------------------------

const testNonceAlias = "gonk.triage.p42.i3.a1.ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// THE TEST THAT MATTERS MOST IN THIS CHANGE.
//
// Adding an unauthenticated route to a service that otherwise guards project
// registration and budget decisions is exactly the kind of change that goes
// wrong quietly. This is an exhaustive table over every route and method: the
// prompt GET is the ONLY entry allowed to answer without a bearer, and every
// other combination must 401. A future route added without thought fails here
// by default, which is the point.
func TestOnlyThePromptGetIsUnauthenticated(t *testing.T) {
	h := newHTTPFixture(t)

	cases := []struct {
		method, path string
		exempt       bool
	}{
		{"PUT", "/v1/projects/group%2Frepo", false},
		{"GET", "/v1/projects/group%2Frepo", false},
		{"DELETE", "/v1/projects/group%2Frepo", false},
		{"POST", "/v1/policy/decide", false},
		{"POST", "/v1/policy/outcome", false},
		{"GET", "/v1/cost/bead/b1", false},
		{"GET", "/v1/cost/session/s1", false},
		{"GET", "/v1/cost/project/group%2Frepo", false},
		{"GET", "/v1/cost/instance", false},
		{"POST", "/admin/spend/sync", false},

		// The one exemption -- and only for GET.
		{"GET", "/v1/prompt/" + testNonceAlias, true},
		// Write access must NOT come with it: an unauthenticated PUT here would
		// let anyone replace the prompt a session is about to run.
		{"PUT", "/v1/prompt/" + testNonceAlias, false},
		{"DELETE", "/v1/prompt/" + testNonceAlias, false},
		// Status reveals whether a prompt was fetched; that is operator data.
		{"GET", "/v1/prompt/" + testNonceAlias + "/status", false},

		// TRAJECTORY INGEST MUST NEVER JOIN THE EXEMPTION (gonk-p8j). The agent
		// pod can already reach this meter -- that is how it fetches its prompt
		// -- so an unauthenticated trace endpoint would let the SUBJECT of the
		// evidence WRITE the evidence, which is exactly why the design note
		// rejects the pod-local transcript. It would be worse here, because it
		// would look like a control.
		{"POST", "/v1/trace", false},
		{"GET", "/v1/trace", false},
		{"PUT", "/v1/trace", false},
	}

	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, _ := h.do(c.method, c.path, "", nil) // no bearer
			defer func() { _ = resp.Body.Close() }()
			got401 := resp.StatusCode == http.StatusUnauthorized
			if c.exempt && got401 {
				t.Fatalf("%s %s = 401 without a bearer, want the exemption to apply",
					c.method, c.path)
			}
			if !c.exempt && !got401 {
				t.Fatalf("%s %s = %d without a bearer, want 401 -- only GET on the "+
					"prompt path may answer unauthenticated", c.method, c.path, resp.StatusCode)
			}
		})
	}
}

// The alias IS the capability, so an alias without a nonce is not one. Shape is
// checked before the store is touched, which also stops this route becoming a
// free database probe on the listener that serves budget decisions.
func TestNonceFreeAliasIsRefusedUnauthenticated(t *testing.T) {
	h := newHTTPFixture(t)
	for _, alias := range []string{
		"gonk.triage.p42.i3.a1",                            // the old deterministic form: guessable
		"gonk.triage.p42.i3.a1.TOOSHORT",                   // wrong length
		"gonk.triage.p42.i3.a1.abcdefghijklmnopqrstuvwxyz", // lower case is not base32
		"gonk.triage.p42.i3.a1.ABCDEFGHIJKLMNOPQRSTUVWXY1", // 1 is not in the alphabet
		"nodotsatall",
	} {
		resp, _ := h.do("GET", "/v1/prompt/"+alias, "", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET with alias %q = %d, want 401 -- a nonce-free alias is "+
				"guessable and must not read as a capability", alias, resp.StatusCode)
		}
	}
}

// One-shot is the only thing standing between "unauthenticated read" and
// "replayable unauthenticated read". 404 and 410 are deliberately different
// answers: never-delivered versus already-consumed are opposite diagnoses.
func TestPromptIsOneShotAndDistinguishes404From410(t *testing.T) {
	h := newHTTPFixture(t)

	resp, _ := h.do("GET", "/v1/prompt/"+testNonceAlias, "", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET before any PUT = %d, want 404", resp.StatusCode)
	}

	resp, _ = h.do("PUT", "/v1/prompt/"+testNonceAlias, h.token,
		meterapi.PromptRequest{Prompt: "triage this", Model: "qwen3-14b", Metadata: `{"a":"b"}`})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT with bearer = %d, want 204", resp.StatusCode)
	}

	resp, body := h.do("GET", "/v1/prompt/"+testNonceAlias, "", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET = %d, want 200", resp.StatusCode)
	}
	var got meterapi.PromptResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Prompt != "triage this" || got.Model != "qwen3-14b" || got.Metadata != `{"a":"b"}` {
		t.Fatalf("payload = %+v -- model and metadata must travel with the prompt "+
			"so the pod can render its overlay per session (gonk-m6t)", got)
	}

	resp, _ = h.do("GET", "/v1/prompt/"+testNonceAlias, "", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("second GET = %d, want 410 -- a replayable unauthenticated read "+
			"is the whole risk of exempting this route", resp.StatusCode)
	}
}

// Dispatch confirms delivery by asking whether the pod fetched, rather than by
// trusting its own submit -- which is the claim the keystroke path made even
// when the composer was empty.
func TestPromptStatusReportsTheFetch(t *testing.T) {
	h := newHTTPFixture(t)
	resp, _ := h.do("PUT", "/v1/prompt/"+testNonceAlias, h.token,
		meterapi.PromptRequest{Prompt: "p", Model: "m"})
	_ = resp.Body.Close()

	resp, body := h.do("GET", "/v1/prompt/"+testNonceAlias+"/status", h.token, nil)
	_ = resp.Body.Close()
	var st meterapi.PromptStatusResponse
	_ = json.Unmarshal(body, &st)
	if st.Fetched {
		t.Fatal("status reported fetched before any GET")
	}

	resp, _ = h.do("GET", "/v1/prompt/"+testNonceAlias, "", nil)
	_ = resp.Body.Close()

	resp, body = h.do("GET", "/v1/prompt/"+testNonceAlias+"/status", h.token, nil)
	_ = resp.Body.Close()
	_ = json.Unmarshal(body, &st)
	if !st.Fetched || st.FetchedAt.IsZero() {
		t.Fatalf("status after fetch = %+v, want fetched with a timestamp", st)
	}
}
