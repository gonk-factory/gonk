package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
)

// LiteLLM is a REAL LiteLLM proxy (the pinned OD-6 tag) running in a container,
// configured to see EXACTLY ONE upstream: the stub model. It is the first thing
// in gonk that lets internal/meter/litellm speak to a real proxy instead of an
// httptest mock (Plan 06 Task 5, P3-1). Everything a component test needs to
// drive or inspect the proxy hangs off this handle.
//
// The dedicated admin key (P3-6): StartLiteLLM boots with a master key, then
// mints a SEPARATE proxy_admin-role key (via /user/new + /key/generate) and
// points Admin/Spend at THAT, never the master key. The spike (docs/spikes/
// litellm-verified.md) proved a plain virtual key gets 401 on the admin routes
// but a proxy_admin-role key does every admin call -- so this is the real,
// least-privilege configuration meter should ship, exercised here.
type LiteLLM struct {
	// Image is the pinned tag actually run (OD-6). A version bump invalidates
	// docs/spikes/litellm-verified.md and re-runs this task.
	Image string
	// URL is the proxy base, http://127.0.0.1:<PortLiteLLM>.
	URL string
	// MasterKey is the runtime-generated master credential. Never committed.
	MasterKey string
	// AdminKey is the DEDICATED proxy_admin key (NOT the master key). This is
	// what Admin and Spend authenticate with, and what meter should be given.
	AdminKey string

	// Admin and Spend are the REAL Plan 03 adapters, pointed at this proxy with
	// the dedicated admin key. Driving THESE against THIS is the whole point.
	Admin *litellm.HTTPAdmin
	Spend *litellm.HTTPSpendSource

	StubURL   string          // the single upstream every model routes to
	Upstreams []string        // every api_base the harness wrote (all == StubURL/v1)
	Models    []ModelUpstream // the rendered model list

	rt        *Runtime
	client    *http.Client
	container string
}

// ModelPrice is a LiteLLM per-token price (USD/token), as /model/info reports it.
type ModelPrice struct {
	InputPerToken  float64
	OutputPerToken float64
}

// ModelUpstream is one entry of LiteLLM's model_list: the model_name clients
// request (== the rung catalog's Model), its price, and (implicitly) the single
// stub upstream. Local-rung prices MUST equal the catalog's synthetic price --
// that agreement is what TestSyntheticPricesAgree (P3-4) checks.
type ModelUpstream struct {
	ModelName string
	Price     ModelPrice
}

// litellmDBName is the database LiteLLM uses inside the shared Postgres. It is
// SEPARATE from every meter ledger DB (StartLedgerDB), so LiteLLM's spend log
// and meter's reservation store never share a database.
const litellmDBName = "litellm"

// StartLiteLLM renders a config with exactly `models` (all routed to stubURL),
// boots the pinned LiteLLM against pgDSN (which must reach a Postgres where the
// `litellm` database exists), waits for liveness, and mints a dedicated
// proxy_admin key. It returns the handle and a stop func the caller must invoke
// at teardown (TestMain owns the shared proxy; there is no t.Cleanup here so the
// same fixture serves package-level setup and a per-test proxy alike).
func StartLiteLLM(rt *Runtime, c *Creds, image, pgDSN, stubURL string, models []ModelUpstream) (_ *LiteLLM, stop func(), err error) {
	masterKey, err := RandomToken(24)
	if err != nil {
		return nil, nil, err
	}
	masterKey = "sk-" + masterKey

	cfgPath, upstreams, err := renderLiteLLMConfig(c, masterKey, stubURL, models)
	if err != nil {
		return nil, nil, err
	}

	name := "gonk-l2-litellm-" + shortID()
	args := []string{"run", "-d", "--name", name}
	if rt.HostNetwork {
		args = append(args, "--network=host")
	}
	args = append(args,
		"-e", "DATABASE_URL="+dsnWithDB(pgDSN, litellmDBName),
		"-e", "LITELLM_MASTER_KEY="+masterKey,
		"-e", "STORE_MODEL_IN_DB=True",
		"-v", cfgPath+":/app/config.yaml:ro,Z",
		image,
		"--config", "/app/config.yaml", "--port", itoa(PortLiteLLM),
	)
	if out, rerr := exec.Command(rt.Bin, args...).CombinedOutput(); rerr != nil {
		return nil, nil, fmt.Errorf("harness: start litellm: %v: %s", rerr, out)
	}
	stop = func() { _ = exec.Command(rt.Bin, "rm", "-f", name).Run() }
	defer func() {
		if err != nil {
			stop()
		}
	}()

	l := &LiteLLM{
		Image:     image,
		URL:       "http://127.0.0.1:" + itoa(PortLiteLLM),
		MasterKey: masterKey,
		StubURL:   stubURL,
		Upstreams: upstreams,
		Models:    models,
		rt:        rt,
		client:    &http.Client{Timeout: 30 * time.Second},
		container: name,
	}

	// Wait for the proxy to accept requests (boot is ~15-30s; docs/spikes).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if werr := WaitFor(ctx, "litellm /health/liveliness", 90*time.Second, func() (bool, error) {
		return l.alive(), nil
	}); werr != nil {
		out, _ := exec.Command(rt.Bin, "logs", "--tail", "30", name).CombinedOutput()
		return nil, nil, fmt.Errorf("%w (litellm logs:\n%s)", werr, out)
	}

	// Mint the DEDICATED proxy_admin key (P3-6): a proxy_admin user, then a key
	// under it. This is what Admin/Spend use -- never the master key.
	adminKey, err := l.mintProxyAdminKey()
	if err != nil {
		return nil, nil, err
	}
	l.AdminKey = adminKey
	l.Admin = litellm.NewHTTPAdmin(l.URL, adminKey, l.client)
	l.Spend = litellm.NewHTTPSpendSource(l.URL, adminKey, l.client)
	return l, stop, nil
}

// WithCatalog rebuilds Spend with a rung catalog so Row.Synthetic is set (the
// currency firewall). Returns the same handle for chaining.
func (l *LiteLLM) WithCatalog(catalog map[string]opercfg.RungSpec) *LiteLLM {
	l.Spend = litellm.NewHTTPSpendSource(l.URL, l.AdminKey, l.client, litellm.WithRungCatalog(catalog))
	return l
}

func (l *LiteLLM) alive() bool {
	resp, err := l.client.Get(l.URL + "/health/liveliness")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

// mintProxyAdminKey creates a proxy_admin user and issues a key for it, using
// the master key. The returned key is a least-privilege admin credential.
func (l *LiteLLM) mintProxyAdminKey() (string, error) {
	uid := "gonk-meter-admin-" + shortID()
	if _, _, err := l.masterCall("POST", "/user/new", map[string]any{
		"user_id": uid, "user_role": "proxy_admin",
	}); err != nil {
		return "", fmt.Errorf("harness: create proxy_admin user: %w", err)
	}
	body, status, err := l.masterCall("POST", "/key/generate", map[string]any{
		"user_id": uid, "key_alias": "gonk-admin-" + shortID(),
	})
	if err != nil || status >= 400 {
		return "", fmt.Errorf("harness: generate proxy_admin key: status %d: %s (%v)", status, body, err)
	}
	var kr struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &kr); err != nil || kr.Key == "" {
		return "", fmt.Errorf("harness: proxy_admin key response: %s", body)
	}
	return kr.Key, nil
}

// masterCall is a raw JSON call authenticated with the MASTER key. Used only for
// bootstrapping the dedicated admin key; every test-visible call goes through
// Admin/Spend (the real adapters) on the dedicated key.
func (l *LiteLLM) masterCall(method, path string, body any) ([]byte, int, error) {
	return l.rawCall(method, path, l.MasterKey, body)
}

func (l *LiteLLM) rawCall(method, path, key string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, l.URL+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

// ModelInfo reads GET /model/info and returns model_name -> price. Used by the
// synthetic-price-agreement (P3-4) and catalog-drift (P3-8) checks.
func (l *LiteLLM) ModelInfo(t testing.TB) map[string]ModelPrice {
	t.Helper()
	body, status, err := l.rawCall("GET", "/model/info", l.AdminKey, nil)
	if err != nil || status != 200 {
		t.Fatalf("harness: /model/info: status %d: %s (%v)", status, body, err)
	}
	var parsed struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				InputCostPerToken  float64 `json:"input_cost_per_token"`
				OutputCostPerToken float64 `json:"output_cost_per_token"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("harness: decode /model/info: %v (%s)", err, body)
	}
	out := map[string]ModelPrice{}
	for _, m := range parsed.Data {
		out[m.ModelName] = ModelPrice{
			InputPerToken:  m.ModelInfo.InputCostPerToken,
			OutputPerToken: m.ModelInfo.OutputCostPerToken,
		}
	}
	return out
}

// KeyInfoRaw is the subset of GET /key/info a component test asserts on. v1.92.0
// nests these fields under `info` (docs/spikes/litellm-verified.md).
type KeyInfoRaw struct {
	MaxBudget      float64
	Spend          float64
	BudgetDuration string
	BudgetResetAt  time.Time
}

// KeyInfo reads GET /key/info?key=<token>. v1.92.0 accepts ONLY ?key=<token>
// (NOT ?key_alias=), and returns {"key":..., "info":{...}} -- both facts are the
// gonk-huy findings this harness was built around.
func (l *LiteLLM) KeyInfo(t testing.TB, token string) KeyInfoRaw {
	t.Helper()
	body, status, err := l.rawCall("GET", "/key/info?key="+url.QueryEscape(token), l.AdminKey, nil)
	if err != nil || status != 200 {
		t.Fatalf("harness: /key/info: status %d: %s (%v)", status, body, err)
	}
	var parsed struct {
		Info struct {
			MaxBudget      float64 `json:"max_budget"`
			Spend          float64 `json:"spend"`
			BudgetDuration string  `json:"budget_duration"`
			BudgetResetAt  string  `json:"budget_reset_at"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("harness: decode /key/info: %v (%s)", err, body)
	}
	ki := KeyInfoRaw{
		MaxBudget:      parsed.Info.MaxBudget,
		Spend:          parsed.Info.Spend,
		BudgetDuration: parsed.Info.BudgetDuration,
	}
	if parsed.Info.BudgetResetAt != "" {
		if ts, perr := time.Parse(time.RFC3339, parsed.Info.BudgetResetAt); perr == nil {
			ki.BudgetResetAt = ts.UTC()
		} else {
			t.Fatalf("harness: parse budget_reset_at %q: %v", parsed.Info.BudgetResetAt, perr)
		}
	}
	return ki
}

// KeyMaxBudget is the USD ceiling LiteLLM enforces on a key.
func (l *LiteLLM) KeyMaxBudget(t testing.TB, token string) float64 {
	t.Helper()
	return l.KeyInfo(t, token).MaxBudget
}

// ProvisionKey creates (idempotent-by-alias) a virtual key through the REAL
// adapter and returns its usable token. maxBudgetUSD nil means unlimited (no
// hard door). budget_duration is a UTC calendar month (AD-9), which the spike
// confirmed "1mo" actually is.
func (l *LiteLLM) ProvisionKey(t testing.TB, alias string, maxBudgetUSD *float64, models []string) string {
	t.Helper()
	info, err := l.Admin.EnsureKey(context.Background(), litellm.KeySpec{
		Alias:          alias,
		MaxBudgetUSD:   maxBudgetUSD,
		BudgetDuration: "1mo",
		Models:         models,
		Metadata:       map[string]string{"gonk_alias": alias},
	})
	if err != nil {
		t.Fatalf("harness: ProvisionKey %q: %v", alias, err)
	}
	if info.Token == "" {
		t.Fatalf("harness: ProvisionKey %q returned an empty token", alias)
	}
	return info.Token
}

// DeleteKey removes a key by alias (for tests that assert immediate refusal).
func (l *LiteLLM) DeleteKey(t testing.TB, alias string) {
	t.Helper()
	if err := l.Admin.DeleteKey(context.Background(), alias); err != nil {
		t.Fatalf("harness: DeleteKey %q: %v", alias, err)
	}
}

// CallDirect makes ONE chat completion straight at LiteLLM with the given key --
// meter and any reservation completely bypassed. It is how the hard-door test
// proves LiteLLM refuses on its own. Returns the HTTP status.
func (l *LiteLLM) CallDirect(t testing.TB, key, model string, metadata map[string]string) int {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "direct"}},
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", l.URL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("harness: CallDirect: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if len(metadata) > 0 {
		md, _ := json.Marshal(metadata)
		req.Header.Set("x-litellm-spend-logs-metadata", string(md))
	}
	resp, err := l.client.Do(req)
	if err != nil {
		t.Fatalf("harness: CallDirect do: %v", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode
}

// ---------------------------------------------------------------- SkewProxy

// SkewProxy sits in front of LiteLLM and REWRITES the `Date` response header by
// +skew. Meter measures clock skew against that header (Decision 7), so this is
// how skew gets INJECTED deterministically instead of waited for. Point a
// meter's LITELLM_URL at the returned URL. The caller stops it via the returned
// func (or t.Cleanup).
func (l *LiteLLM) SkewProxy(t testing.TB, skew time.Duration) (proxyURL string, stop func()) {
	t.Helper()
	target, err := url.Parse(l.URL)
	if err != nil {
		t.Fatalf("harness: SkewProxy parse target: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = func(resp *http.Response) error {
		// Overwrite Date with a skewed instant. Because the header is already
		// present, net/http will not replace it with the proxy's own clock.
		resp.Header.Set("Date", time.Now().UTC().Add(skew).Format(http.TimeFormat))
		return nil
	}
	srv := httptest.NewServer(rp)
	return srv.URL, srv.Close
}

// RecordingProxy sits in front of LiteLLM and CAPTURES the query of every
// forwarded request whose path matches pathContains. It is how
// TestMeterSpendPollerAlwaysCarriesADateBound proves every /spend/logs/v2 the
// adapter issues carries a start_date -- the OOM safety guard, as a test.
func (l *LiteLLM) RecordingProxy(t testing.TB, pathContains string) (proxyURL string, captured func() []url.Values, stop func()) {
	t.Helper()
	target, err := url.Parse(l.URL)
	if err != nil {
		t.Fatalf("harness: RecordingProxy parse target: %v", err)
	}
	var mu sync.Mutex
	var caps []url.Values
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			if strings.Contains(pr.In.URL.Path, pathContains) {
				mu.Lock()
				caps = append(caps, cloneValues(pr.In.URL.Query()))
				mu.Unlock()
			}
			pr.SetURL(target)
		},
	}
	srv := httptest.NewServer(rp)
	return srv.URL, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		out := make([]url.Values, len(caps))
		copy(out, caps)
		return out
	}, srv.Close
}

// ---------------------------------------------------------------- config render

func renderLiteLLMConfig(c *Creds, masterKey, stubURL string, models []ModelUpstream) (path string, upstreams []string, err error) {
	base := strings.TrimRight(stubURL, "/") + "/v1"
	var b strings.Builder
	b.WriteString("model_list:\n")
	for _, m := range models {
		b.WriteString("  - model_name: " + m.ModelName + "\n")
		b.WriteString("    litellm_params:\n")
		b.WriteString("      model: openai/" + m.ModelName + "\n")
		b.WriteString("      api_base: " + base + "\n")
		b.WriteString("      api_key: sk-stub\n")
		fmt.Fprintf(&b, "      input_cost_per_token: %.12g\n", m.Price.InputPerToken)
		fmt.Fprintf(&b, "      output_cost_per_token: %.12g\n", m.Price.OutputPerToken)
		upstreams = append(upstreams, base)
	}
	b.WriteString("litellm_settings:\n  drop_params: true\n")
	b.WriteString("general_settings:\n")
	b.WriteString("  master_key: " + masterKey + "\n")
	// A small flush interval keeps the detailed spend-log lag as short as this
	// version allows -- the lag is measured honestly by TestMeasureLiteLLMSpendLogLag.
	b.WriteString("  proxy_batch_write_at: 1\n")

	path, err = c.WriteSecret("litellm-config.yaml", b.String())
	return path, upstreams, err
}

// dsnWithDB swaps the database name in a postgres DSN.
func dsnWithDB(dsn, db string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + db
	return u.String()
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

var shortIDMu sync.Mutex

func shortID() string {
	shortIDMu.Lock()
	defer shortIDMu.Unlock()
	tok, err := RandomToken(12)
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	clean := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(tok))
	if len(clean) > 8 {
		clean = clean[:8]
	}
	return clean
}

// ListenLoopback binds a TCP listener on 127.0.0.1:port (used to serve the stub
// on a fixed port under host networking, reachable by the LiteLLM container).
func ListenLoopback(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
}
