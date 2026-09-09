//go:build live

// Package live holds checks that run against a REAL DEPLOYMENT rather than a
// hermetic fixture.
//
// WHY THIS EXISTS SEPARATELY FROM test/component (gonk-q9e).
// test/component already has TestSyntheticPricesAgreeBetweenTheRungCatalogAndLiteLLM,
// and it is a good test: it proves the agreement RULE is enforceable and that the
// code reads both sides correctly. It could not have caught gonk-1zm, because
// BOTH SIDES ARE FIXTURES -- newWorld boots its own LiteLLM and its own catalog.
// The real deployment had no prices at all in LiteLLM for months while the real
// operator config declared them, and every hermetic test stayed green.
//
// A drift check has to read what is DEPLOYED. Both sides live in the cluster --
// the operator config in a ConfigMap, the prices in the running proxy -- so the
// cluster, not the gitops repo, is the source of truth for "what is actually
// running".
//
// Run:
//
//	GONK_LIVE_LITELLM_URL=https://litellm.orac.local \
//	GONK_LIVE_LITELLM_KEY=<admin key> \
//	go test -tags live ./test/live/ -v
//
// Skips (does not fail) when those are unset, so it is safe in any pipeline.
package live

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/test/harness"
	"gopkg.in/yaml.v3"
)

const (
	envURL       = "GONK_LIVE_LITELLM_URL"
	envKey       = "GONK_LIVE_LITELLM_KEY"
	envNamespace = "GONK_LIVE_NAMESPACE"
)

func mustEnv(t *testing.T) (url, key, ns string) {
	t.Helper()
	url, key = os.Getenv(envURL), os.Getenv(envKey)
	harness.RequireInfra(t, fmt.Sprintf("%s and %s (set both to run the live drift checks)", envURL, envKey), url != "" && key != "")
	ns = os.Getenv(envNamespace)
	if ns == "" {
		ns = "gonk"
	}
	return url, key, ns
}

// deployedCatalog reads the operator config out of the RUNNING deployment's
// ConfigMap, not out of the repo. What a file in git says is not evidence about
// what is serving traffic, and the whole point of this check is the gap between
// the two.
func deployedCatalog(t *testing.T, ns string) []opercfg.RungSpec {
	t.Helper()
	out, err := exec.Command("kubectl", "get", "cm", "gonk-operator-config",
		"-n", ns, "-o", "jsonpath={.data.operator-config\\.yaml}").Output()
	harness.RequireInfra(t, fmt.Sprintf("kubectl access to the %s operator config (%v)", ns, err), err == nil)
	var doc struct {
		Rungs []opercfg.RungSpec `yaml:"rungs"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode deployed operator config: %v", err)
	}
	if len(doc.Rungs) == 0 {
		t.Fatal("the deployed operator config declares no rungs at all")
	}
	return doc.Rungs
}

type modelPrice struct{ in, out float64 }

// livePrices reads GET /model/info from the RUNNING proxy.
func livePrices(t *testing.T, url, key string) map[string]modelPrice {
	t.Helper()
	c := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // private CA
	}
	req, err := http.NewRequest(http.MethodGet, url+"/model/info", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET /model/info: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /model/info: status %d: %s", resp.StatusCode, body)
	}
	var parsed struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				In  float64 `json:"input_cost_per_token"`
				Out float64 `json:"output_cost_per_token"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode /model/info: %v", err)
	}
	out := map[string]modelPrice{}
	for _, m := range parsed.Data {
		out[m.ModelName] = modelPrice{in: m.ModelInfo.In, out: m.ModelInfo.Out}
	}
	return out
}

// approx compares prices that have travelled through JSON floats.
func approx(a, b float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= math.Max(1e-12, math.Abs(b)*1e-9)
}

// THE CHECK THAT WOULD HAVE CAUGHT gonk-1zm. For months LiteLLM carried no cost
// fields at all for the local models, so every local rung billed $0, the virtual
// key's USD counter never moved, and a project's monthly_tokens ceiling had no
// hard enforcement anywhere -- while the operator config declared prices and
// every hermetic test passed.
func TestDeployedPricesMatchTheDeployedRungCatalog(t *testing.T) {
	url, key, ns := mustEnv(t)
	prices := livePrices(t, url, key)

	for _, r := range deployedCatalog(t, ns) {
		if r.Kind != opercfg.KindLocal {
			continue
		}
		want := r.SyntheticUSDPer1MTokens / 1e6
		got, ok := prices[r.Model]
		if !ok {
			t.Errorf("rung %q: model %q is NOT IN the running proxy's /model/info. "+
				"A rung nobody can route to, or a price nobody applies.", r.Name, r.Model)
			continue
		}
		if got.in == 0 && got.out == 0 {
			t.Errorf("rung %q model %q: the running proxy prices it at ZERO. This is the gonk-1zm "+
				"failure exactly: the USD door cannot close on a free model, so monthly_tokens is "+
				"enforced nowhere. Catalog wants $%g/token.", r.Name, r.Model, want)
			continue
		}
		if !approx(got.in, want) {
			t.Errorf("rung %q model %q: catalog says $%g/token, the running proxy says $%g/token. "+
				"These live in two different repos and NOTHING ELSE COMPARES THEM.",
				r.Name, r.Model, want, got.in)
		}
		// A SPLIT RATE IS ALSO DRIFT, and a subtler one. opercfg.PricePerToken()
		// models ONE price per token, and the meter reconciles against LiteLLM's
		// spend, so an input/output split silently desynchronises the two
		// counters -- the numbers stay plausible and stop agreeing.
		if !approx(got.out, got.in) {
			t.Errorf("rung %q model %q: input $%g != output $%g. gonk models ONE price per token; "+
				"a split makes LiteLLM's spend disagree with the meter's math.",
				r.Name, r.Model, got.in, got.out)
		}
	}
}

// opercfg FORBIDS a synthetic price on a cloud rung: its price is real and lives
// in LiteLLM's own config, so declaring one here would be a lie that the budget
// math would then believe.
func TestDeployedCloudRungsDeclareNoSyntheticPrice(t *testing.T) {
	_, _, ns := mustEnv(t)
	for _, r := range deployedCatalog(t, ns) {
		if r.Kind == opercfg.KindCloud && r.SyntheticUSDPer1MTokens != 0 {
			t.Errorf("cloud rung %q declares synthetic_usd_per_1m_tokens=%g. A cloud rung's price is "+
				"real; a synthetic one would make the budget math believe a fiction.",
				r.Name, r.SyntheticUSDPer1MTokens)
		}
	}
}

// THE CHECK THAT WOULD HAVE CAUGHT gonk-8gb. Pricing being correct is worth
// nothing if the key an agent presents is not the METERED key. Agents
// authenticated with the proxy MASTER key for months: it has no alias and no
// budget, so the per-project ceiling was never consulted and every session
// looked perfectly healthy.
func TestEveryProjectKeyIsScopedAndBudgeted(t *testing.T) {
	url, key, _ := mustEnv(t)

	// Asserted on the PROXY side rather than by enumerating secrets: what
	// matters is the ceiling the door will actually enforce, and that lives on
	// the key, not in the Secret that carries a copy of it.
	keys := listKeys(t, url, key)
	if len(keys) == 0 {
		t.Skip("no gonk- aliased virtual keys on the proxy yet; nothing to assert")
	}
	t.Logf("checking %d gonk- aliased virtual keys", len(keys))
	for alias, budget := range keys {
		if budget == nil {
			t.Errorf("virtual key %q has NO max_budget. A key with no ceiling cannot refuse anything, "+
				"so the hard door is open for that project.", alias)
			continue
		}
		if *budget <= 0 {
			t.Errorf("virtual key %q has max_budget=%g. A non-positive ceiling is not a ceiling.", alias, *budget)
		}
	}
}

func listKeys(t *testing.T, url, key string) map[string]*float64 {
	t.Helper()
	c := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // private CA
	}
	// size=100 is LiteLLM's documented maximum -- asking for more is a 422, and
	// a 422 decodes to an empty key list, which reads as "no keys exist". That
	// is how this check silently passed while asserting nothing.
	req, _ := http.NewRequest(http.MethodGet, url+"/key/list?return_full_object=true&size=100", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET /key/list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// FAIL on a non-200 rather than decoding it. An error body has no "keys"
	// field, so tolerating it turns "the proxy refused us" into "there is
	// nothing to check" -- the failure mode this whole file exists to prevent.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /key/list: status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed struct {
		Keys []struct {
			Alias     *string  `json:"key_alias"`
			MaxBudget *float64 `json:"max_budget"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode /key/list: %v (%s)", err, truncate(string(body), 200))
	}
	out := map[string]*float64{}
	for _, k := range parsed.Keys {
		if k.Alias == nil || len(*k.Alias) < 5 || (*k.Alias)[:5] != "gonk-" {
			continue
		}
		out[*k.Alias] = k.MaxBudget
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d bytes)", len(s))
}
