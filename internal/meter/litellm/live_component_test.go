//go:build component

// Package litellm's live component test drives the REAL HTTPAdmin and
// HTTPSpendSource against a REAL LiteLLM (verified against
// ghcr.io/berriai/litellm-database:v1.92.0). It is the gate the standing unit
// mocks exist to approximate -- and the one that caught gonk-huy, because the
// original unit mocks lied about the wire shapes.
//
// It is NOT part of the standing gate (build tag `component`, heavy, needs a
// live proxy). Run it against a running harness:
//
//	GONK_LITELLM_URL=http://127.0.0.1:4000 \
//	GONK_LITELLM_MASTER_KEY=sk-master-... \
//	go test -tags=component -run TestLive ./internal/meter/litellm/ -v
//
// It mints its own proxy_admin key from the master key (admin routes need the
// proxy_admin role; a plain key is 401 -- verified), provisions a throwaway
// alias, and cleans the alias up on exit.
package litellm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

func liveConfig(t *testing.T) (base, master string) {
	t.Helper()
	base = strings.TrimRight(os.Getenv("GONK_LITELLM_URL"), "/")
	master = os.Getenv("GONK_LITELLM_MASTER_KEY")
	if base == "" || master == "" {
		t.Skip("set GONK_LITELLM_URL and GONK_LITELLM_MASTER_KEY to run the live component test")
	}
	return base, master
}

// mintAdminKey issues a proxy_admin-role key from the master key. Admin routes
// (/key/*, /spend/logs/v2) require proxy_admin; the adapter just carries it.
func mintAdminKey(t *testing.T, base, master string) string {
	t.Helper()
	body := doJSON(t, http.MethodPost, base+"/user/new", master, map[string]any{"user_role": "proxy_admin"})
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Key == "" {
		t.Fatalf("mint admin key: %v (body=%s)", err, body)
	}
	return out.Key
}

func doJSON(t *testing.T, method, url, bearer string, payload any) []byte {
	t.Helper()
	var r io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, b)
	}
	return b
}

// completion drives one chat completion as the project's key, carrying gonk
// attribution in the x-litellm-spend-logs-metadata header (the header LiteLLM
// persists at metadata.spend_logs_metadata -- the only attribution surface,
// since metadata is not forwarded upstream).
func completion(t *testing.T, base, projectToken string, tags map[string]string) int {
	t.Helper()
	tagJSON, _ := json.Marshal(tags)
	b, _ := json.Marshal(map[string]any{
		"model":    "glm",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+projectToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-litellm-spend-logs-metadata", string(tagJSON))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// TestLiveAdmin_EnsureKeyTwiceUpdatesBudget is Bug B's live gate: EnsureKey the
// same alias twice (create, then raise max_budget) and the SECOND call must
// succeed via the /key/list -> /key/update path, actually changing the budget,
// while the plaintext token minted at create keeps working.
func TestLiveAdmin_EnsureKeyTwiceUpdatesBudget(t *testing.T) {
	base, master := liveConfig(t)
	adminKey := mintAdminKey(t, base, master)
	admin := litellm.NewHTTPAdmin(base, adminKey, http.DefaultClient)
	ctx := context.Background()

	alias := fmt.Sprintf("gonk-live-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = admin.DeleteKey(context.Background(), alias) })

	b1 := 5.0
	info1, err := admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: &b1, BudgetDuration: "1mo", Models: []string{"glm"},
	})
	if err != nil {
		t.Fatalf("first EnsureKey (create): %v", err)
	}
	if !strings.HasPrefix(info1.Token, "sk-") {
		t.Fatalf("create did not return a usable plaintext token: %q", info1.Token)
	}
	t.Logf("create: alias=%s token=%s... budget=%v", alias, info1.Token[:10], b1)

	b2 := 9.0
	info2, err := admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: &b2, BudgetDuration: "1mo", Models: []string{"glm"},
	})
	if err != nil {
		t.Fatalf("second EnsureKey (update-by-alias) FAILED -- this is the gonk-huy Bug B regression: %v", err)
	}
	if info2.Token != "" {
		t.Fatalf("update path returned a token %q; it must be empty (plaintext is unrecoverable)", info2.Token)
	}
	t.Logf("update: second EnsureKey succeeded, empty token as designed")

	// The budget actually changed to 9.0.
	body := doJSON(t, http.MethodGet, base+"/key/info?key="+info1.Token, adminKey, nil)
	var ki struct {
		Info struct {
			MaxBudget float64 `json:"max_budget"`
			KeyAlias  string  `json:"key_alias"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &ki); err != nil {
		t.Fatalf("decode key/info: %v (%s)", err, body)
	}
	if ki.Info.MaxBudget != b2 {
		t.Fatalf("max_budget after update = %v, want %v -- the update did not take effect", ki.Info.MaxBudget, b2)
	}
	if ki.Info.KeyAlias != alias {
		t.Fatalf("key_alias = %q, want %q", ki.Info.KeyAlias, alias)
	}

	// The original plaintext token still authenticates (update, not rotate).
	if code := completion(t, base, info1.Token, map[string]string{"gonk_project": "group/repo"}); code != http.StatusOK {
		t.Fatalf("completion with the create-time token after update = HTTP %d, want 200", code)
	}
	t.Logf("verified: budget now %v, original token still authenticates", ki.Info.MaxBudget)
}

// TestLiveSpend_ReadsRealV2Rows is Bug A's live gate: the REAL HTTPSpendSource
// must read real /spend/logs/v2 rows (object envelope, YYYY-MM-DD dates, body
// pagination) -- the three things the original adapter got wrong.
func TestLiveSpend_ReadsRealV2Rows(t *testing.T) {
	base, master := liveConfig(t)
	adminKey := mintAdminKey(t, base, master)
	admin := litellm.NewHTTPAdmin(base, adminKey, http.DefaultClient)
	ctx := context.Background()

	alias := fmt.Sprintf("gonk-live-spend-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = admin.DeleteKey(context.Background(), alias) })

	b := 100.0
	info, err := admin.EnsureKey(ctx, litellm.KeySpec{
		Alias: alias, MaxBudgetUSD: &b, BudgetDuration: "1mo", Models: []string{"glm"},
	})
	if err != nil {
		t.Fatalf("provision project key: %v", err)
	}

	tags := map[string]string{
		"gonk_project":     "group/repo",
		"gonk_rig":         "repo",
		"gonk_bead_id":     "gk-live",
		"gonk_session_key": "s-live",
		"gonk_rung":        "glm",
		"gonk_attempt":     "1",
		"gonk_trigger":     "issue-triage",
	}
	for i := 0; i < 4; i++ {
		if code := completion(t, base, info.Token, tags); code != http.StatusOK {
			t.Fatalf("completion %d = HTTP %d, want 200", i, code)
		}
	}

	cat := map[string]opercfg.RungSpec{"glm": {Name: "glm", Kind: opercfg.KindCloud}}
	src := litellm.NewHTTPSpendSource(base, adminKey, http.DefaultClient, litellm.WithRungCatalog(cat))

	// Bug A's real gate: the adapter must READ and PARSE real /spend/logs/v2 rows
	// (object envelope + YYYY-MM-DD dates + body pagination). "Read and parsed"
	// means either attributed rows (Since returns them) OR rows it saw but could
	// not attribute (Unattributed++) -- BOTH prove the envelope/date/pagination
	// contract is correct against the real proxy. We deliberately do NOT require
	// attributed rows: whether spend_logs_metadata is populated, and the write
	// lag of the detailed log, are the P3-3 concerns tracked separately -- not
	// this adapter's job, which is to query v2 correctly and decode what it gets.
	//
	// The detailed spend log is a best-effort background batch write
	// (docs/spikes/litellm-verified.md, P3-3), so poll for rows to land.
	start := time.Now().UTC().Truncate(24 * time.Hour)
	deadline := time.Now().Add(120 * time.Second)
	var attributed, seen int
	var clock time.Time
	for time.Now().Before(deadline) {
		got, srcClock, err := src.Since(ctx, start)
		if err != nil {
			t.Fatalf("HTTPSpendSource.Since against REAL v2 FAILED -- gonk-huy Bug A regression: %v", err)
		}
		clock = srcClock
		attributed = len(got)
		seen = attributed + src.Unattributed()
		if seen > 0 {
			if attributed > 0 {
				t.Logf("read %d real v2 rows; first: cost=%v prompt_tokens=%v project=%q",
					attributed, got[0].CostUSD, got[0].PromptTokens, got[0].Tags.Project)
			} else {
				t.Logf("read %d real v2 rows (unattributed: spend_logs_metadata absent -- P3-3, not Bug A)", src.Unattributed())
			}
			break
		}
		time.Sleep(5 * time.Second)
	}
	if seen == 0 {
		t.Fatalf("adapter read no v2 rows within the poll window -- either it cannot read v2 (Bug A) or the spend-log lag exceeds the window (P3-3)")
	}
	if clock.IsZero() {
		t.Fatalf("source clock is zero -- the Date header (month-rollover guard) was not read")
	}
}
