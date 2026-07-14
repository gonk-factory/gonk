package intake

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

func newTestHook(t *testing.T) http.Handler {
	t.Helper()
	v, err := ghook.NewVerifier("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	h, err := ghook.NewHandler(v, ghook.NewDeduper(time.Hour, 8), 7, func(*ghook.Event) bool { return true }, ghook.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The security property to prove: the public listener serves exactly one path.
func TestPublicListenerExposesOnlyTheHook(t *testing.T) {
	srv := NewServer(ServerConfig{Hook: newTestHook(t)})
	for _, path := range []string{"/", "/metrics", "/healthz", "/admin/reconcile", "/debug/pprof/"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("public listener serves %s (code %d); it must serve /hook/gitlab and nothing else", path, w.Code)
		}
	}
}

// fakePass is the Pass seam under test: it records Kick() calls and lets the
// test control what WaitForNextPass returns, without a real Reconciler (and
// therefore without a real GitLab or meter behind it).
type fakePass struct {
	mu       sync.Mutex
	kicks    int
	waitResp ReconcileSummary
	waitErr  error
	waitedCh chan struct{} // closed the first time WaitForNextPass is called, if non-nil
}

func (f *fakePass) Kick() {
	f.mu.Lock()
	f.kicks++
	f.mu.Unlock()
}

func (f *fakePass) Kicks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kicks
}

func (f *fakePass) WaitForNextPass(ctx context.Context) (ReconcileSummary, error) {
	if f.waitedCh != nil {
		close(f.waitedCh)
	}
	return f.waitResp, f.waitErr
}

func TestPrivateListenerServesOpsEndpoints(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg) // register the standard series so /metrics has something to gather
	fp := &fakePass{waitResp: ReconcileSummary{Result: "ok"}}
	srv := NewServer(ServerConfig{Reg: reg, Reconcile: fp})

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Private().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
	}

	r := httptest.NewRequest("POST", "/admin/reconcile", nil)
	w := httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Errorf("POST /admin/reconcile = %d, want 202", w.Code)
	}
	var body map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body["kicked"] {
		t.Errorf("POST /admin/reconcile body = %s, want {\"kicked\":true}", w.Body.String())
	}
	if fp.Kicks() != 1 {
		t.Errorf("kicks = %d, want 1", fp.Kicks())
	}

	r = httptest.NewRequest("GET", "/admin/reconcile", nil)
	w = httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /admin/reconcile = %d, want 405", w.Code)
	}
}

// TestAdminReconcileWaitReturnsTheSummary exercises the ?wait=true path at the
// HTTP layer: it must return 200 with the JSON ReconcileSummary the Pass
// produced (not merely 202).
func TestAdminReconcileWaitReturnsTheSummary(t *testing.T) {
	fp := &fakePass{waitResp: ReconcileSummary{
		Projects: 3, States: map[string]int{"valid": 3}, Result: "ok",
	}}
	srv := NewServer(ServerConfig{Reconcile: fp})

	r := httptest.NewRequest("POST", "/admin/reconcile?wait=true", nil)
	w := httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/reconcile?wait=true = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got ReconcileSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Projects != 3 || got.Result != "ok" || got.States["valid"] != 3 {
		t.Errorf("summary = %+v, want the fake's ReconcileSummary echoed back", got)
	}
}

// A wait that times out (ctx exceeded) must not come back as a success.
func TestAdminReconcileWaitPropagatesTimeout(t *testing.T) {
	fp := &fakePass{waitErr: errors.New("context deadline exceeded")}
	srv := NewServer(ServerConfig{Reconcile: fp})

	r := httptest.NewRequest("POST", "/admin/reconcile?wait=true", nil)
	w := httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("wait error must not be reported as 200: %s", w.Body.String())
	}
}

func TestAdminReconcileWithoutReconcileWiredIs503(t *testing.T) {
	srv := NewServer(ServerConfig{})
	r := httptest.NewRequest("POST", "/admin/reconcile", nil)
	w := httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when no Pass is wired", w.Code)
	}
}

// GET /readyz must reflect a wired ReadyChecker's verdict, not just always 200.
func TestReadyzReflectsReadyChecker(t *testing.T) {
	rc := &fakeReady{err: errors.New("meter unreachable")}
	srv := NewServer(ServerConfig{Ready: rc})
	r := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when not ready", w.Code)
	}

	rc.err = nil
	w = httptest.NewRecorder()
	srv.Private().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 when ready", w.Code)
	}
}

type fakeReady struct{ err error }

func (f *fakeReady) Ready(context.Context) error { return f.err }
