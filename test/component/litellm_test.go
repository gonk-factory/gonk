//go:build component

package component_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestLiteLLMAdminAPIShapes (P3-1) drives the REAL internal/meter/litellm client
// against the REAL proxy: create, update (raise + lower max_budget), delete. A
// deleted key must stop working IMMEDIATELY, not eventually. This is the first
// contact the httptest mocks could never make -- and the fix (gonk-huy) is what
// lets it pass at all.
func TestLiteLLMAdminAPIShapes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	alias := "gonk-l2-admin-" + harnessShortID()

	// /key/generate: a usable key with a USD max_budget and a calendar-month duration.
	info, err := w.LiteLLM.Admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: ptr(3.00), BudgetDuration: "1mo", Models: []string{"stub-glm"},
	})
	if err != nil {
		t.Fatalf("EnsureKey(create): %v", err)
	}
	if info.Token == "" {
		t.Fatal("EnsureKey(create) returned no token")
	}
	if got := w.LiteLLM.KeyMaxBudget(t, info.Token); !ledger.ApproxUSD(got, 3.00) {
		t.Fatalf("max_budget after create = %v, want 3.00", got)
	}
	if ki := w.LiteLLM.KeyInfo(t, info.Token); ki.BudgetDuration != "1mo" {
		t.Fatalf("budget_duration = %q, want 1mo", ki.BudgetDuration)
	}

	// /key/update: raising max_budget takes effect (EnsureKey is idempotent by
	// alias -> the fixed updateByAlias path).
	if _, err := w.LiteLLM.Admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: ptr(9.00), BudgetDuration: "1mo", Models: []string{"stub-glm"},
	}); err != nil {
		t.Fatalf("EnsureKey(raise): %v", err)
	}
	if got := w.LiteLLM.KeyMaxBudget(t, info.Token); !ledger.ApproxUSD(got, 9.00) {
		t.Fatalf("max_budget after raise = %v, want 9.00", got)
	}
	// ...and lowering it too.
	if _, err := w.LiteLLM.Admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: ptr(1.00), BudgetDuration: "1mo", Models: []string{"stub-glm"},
	}); err != nil {
		t.Fatalf("EnsureKey(lower): %v", err)
	}
	if got := w.LiteLLM.KeyMaxBudget(t, info.Token); !ledger.ApproxUSD(got, 1.00) {
		t.Fatalf("max_budget after lower = %v, want 1.00", got)
	}

	// The key works before delete.
	w.Stub.SetScript(alwaysAnswer(10, 0))
	if code := w.LiteLLM.CallDirect(t, info.Token, "stub-glm", nil); code != 200 {
		t.Fatalf("live key call before delete = %d, want 200", code)
	}
	// /key/delete: the key stops working IMMEDIATELY.
	w.LiteLLM.DeleteKey(t, alias)
	if code := w.LiteLLM.CallDirect(t, info.Token, "stub-glm", nil); code != 401 {
		t.Fatalf("deleted key call = %d, want 401 (a deleted key must die immediately)", code)
	}
}

// TestMeterSpendPollerAlwaysCarriesADateBound is a HARD SAFETY test, not a
// convenience one: an unbounded GET /spend/logs OOM-killed the live LiteLLM the
// whole cluster shares (docs/environment.md; gonk-wgq). Meter polls this endpoint
// on a timer. So drive the REAL HTTPSpendSource through a recording proxy and
// require EVERY captured request to carry a start_date -- never a naked query.
func TestMeterSpendPollerAlwaysCarriesADateBound(t *testing.T) {
	w := newWorld(t)
	proxyURL, captured, stop := w.LiteLLM.RecordingProxy(t, "/spend/logs")
	defer stop()

	// A real Plan 03 spend source pointed at the proxy (which forwards to real
	// LiteLLM). Poll several times, over several windows.
	src := litellm.NewHTTPSpendSource(proxyURL, w.LiteLLM.AdminKey, &http.Client{Timeout: 30 * time.Second})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, _, err := src.Since(ctx, time.Now().Add(-time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatalf("spend poll %d: %v", i, err)
		}
	}

	caps := captured()
	if len(caps) == 0 {
		t.Fatal("recording proxy captured no /spend/logs request -- the poller never queried")
	}
	for i, q := range caps {
		if q.Get("start_date") == "" {
			t.Fatalf("captured /spend/logs request %d has NO start_date (%v) -- an unbounded "+
				"query OOM-kills the shared LiteLLM (gonk-wgq)", i, q.Encode())
		}
	}
	t.Logf("every one of %d captured /spend/logs/v2 requests carried a start_date", len(caps))
}

// TestDedicatedAdminKeyCanPerformEveryAdminCall (P3-6 / OD-A). The shared
// LiteLLM handle authenticates Admin/Spend with a DEDICATED proxy_admin key
// (minted via /user/new user_role=proxy_admin), NOT the master key. If any admin
// call demands the master key, THAT is the finding -- record it, do not silently
// switch to master. Here every call must succeed on the dedicated key.
func TestDedicatedAdminKeyCanPerformEveryAdminCall(t *testing.T) {
	w := newWorld(t)
	if w.LiteLLM.AdminKey == w.LiteLLM.MasterKey {
		t.Fatal("the harness admin key IS the master key -- the dedicated-key path is not being exercised")
	}
	ctx := context.Background()
	alias := "gonk-l2-dedicated-" + harnessShortID()

	// /key/generate + /key/update via the adapter (both on the dedicated key).
	info, err := w.LiteLLM.Admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: ptr(2.00), BudgetDuration: "1mo", Models: []string{"stub-glm"},
	})
	if err != nil {
		t.Fatalf("dedicated key /key/generate: %v", err)
	}
	if _, err := w.LiteLLM.Admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: ptr(4.00), BudgetDuration: "1mo", Models: []string{"stub-glm"},
	}); err != nil {
		t.Fatalf("dedicated key /key/update: %v", err)
	}
	// /spend/logs/v2 via the adapter.
	if _, _, err := w.LiteLLM.Spend.Since(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("dedicated key /spend/logs/v2: %v", err)
	}
	// /model/info (harness call on the dedicated key).
	_ = w.LiteLLM.ModelInfo(t)
	// /key/delete via the adapter.
	if err := w.LiteLLM.Admin.DeleteKey(ctx, alias); err != nil {
		t.Fatalf("dedicated key /key/delete: %v", err)
	}
	_ = info
	t.Log("a dedicated proxy_admin key performed generate/update/spend-logs/model-info/delete -- master key NOT required")
}

// TestLiteLLMHasNoRealUpstream is the determinism guard: LiteLLM must see exactly
// one upstream and it must be the stub. A test that could reach a cloud provider
// is not deterministic (and spends real money).
func TestLiteLLMHasNoRealUpstream(t *testing.T) {
	w := newWorld(t)
	if len(w.LiteLLM.Upstreams) == 0 {
		t.Fatal("no upstreams configured")
	}
	want := strings.TrimRight(w.LiteLLM.StubURL, "/") + "/v1"
	for _, base := range w.LiteLLM.Upstreams {
		if base != want {
			t.Fatalf("upstream %q is not the stub (%q) -- a non-stub upstream can reach a real model", base, want)
		}
	}
	// The stub is a loopback address, never a cloud host.
	if !strings.Contains(want, "127.0.0.1") {
		t.Fatalf("stub upstream %q is not loopback", want)
	}
}

// TestWhetherLiteLLMForwardsMetadataUpstream discovers, empirically, whether
// LiteLLM forwards `metadata` to the upstream model. EITHER answer is fine -- but
// it decides whether the stub log can check ATTRIBUTION (it can always check
// VOLUME). The answer is WRITTEN DOWN in docs/spikes/litellm-verified.md.
func TestWhetherLiteLLMForwardsMetadataUpstream(t *testing.T) {
	w := newWorld(t)
	w.Stub.SetScript(alwaysAnswer(10, 0))
	key := w.LiteLLM.ProvisionKey(t, "gonk-l2-md-"+harnessShortID(), nil, []string{"stub-glm"})

	before := len(w.Stub.Log().Calls())
	if code := w.LiteLLM.CallDirect(t, key, "stub-glm", map[string]string{"gonk_project": "acme/md"}); code != 200 {
		t.Fatalf("tagged call = %d, want 200", code)
	}
	calls := w.Stub.Log().Calls()
	if len(calls) <= before {
		t.Fatal("the stub never saw the call")
	}
	last := calls[len(calls)-1]
	forwarded := len(last.Metadata) > 0
	t.Logf("RESULT (record in litellm-verified.md): LiteLLM forwards metadata upstream = %v (stub saw metadata=%v)",
		forwarded, last.Metadata)
	// No assertion on the outcome -- only that we observed it. The finding is the value.
}

// TestCatalogMatchesModelInfo (P3-8) is the drift check meter's
// gonk_meter_catalog_drift_total exists for: every LOCAL rung's synthetic price
// in the operator catalog must equal LiteLLM's real /model/info price for that
// model. These live in two different files and NOTHING ELSE reads both.
func TestCatalogMatchesModelInfo(t *testing.T) {
	w := newWorld(t)
	info := w.LiteLLM.ModelInfo(t)
	drift := 0
	for _, r := range w.Catalog {
		if r.Kind != opercfg.KindLocal {
			continue
		}
		price, ok := info[r.Model]
		if !ok {
			t.Fatalf("rung %q model %q missing from /model/info", r.Name, r.Model)
		}
		want := r.SyntheticUSDPer1MTokens / 1e6
		if !ledger.ApproxUSD(price.InputPerToken, want) {
			drift++
			t.Errorf("rung %q: catalog says $%g/token, LiteLLM says $%g/token", r.Name, want, price.InputPerToken)
		}
	}
	if drift != 0 {
		t.Fatalf("catalog drift = %d, want 0 (the hard door would be set to a different ceiling than meter thinks)", drift)
	}
}
