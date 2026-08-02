// Package packtest validates pack/'s declarative TOML files against the
// rules Gas City's real loader enforces -- OFFLINE, with no Gas City binary,
// no container, and no network. Every rule asserted here is transcribed from
// the MIT gascity source at the pin recorded in images/versions.env (Task 5),
// specifically:
//
//   - internal/config/pack.go       -- PackConfig's top-level table set
//   - internal/config/webhook.go    -- the 5-scheme verify registry
//   - internal/orders/order.go      -- Order/OrderParam field shapes and the
//     formula-xor-exec / no-pool-on-exec rules
//   - internal/graphv2/invocation.go -- the v2 reserved variable names
//   - docs/reference/specs/pack-spec.md   -- agent directory layout
//   - docs/reference/specs/formula-spec-v2.md -- [[steps]], [steps.check],
//     and the formula_compiler >=2.0.0 declaration [steps.check] requires
//
// This is not a substitute for Task 6's real-loader-in-a-container check --
// it is the fast gate that catches the mistakes we already know we can
// make: an unknown pack.toml key, a formula var name the v2 compiler
// reserves, an exec order with a pool, a missing agent directory, a check
// script that does not exist on disk.
package packtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// packRoot locates the pack/ directory relative to this test file, so the
// test works regardless of the caller's working directory.
func packRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "pack")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("pack directory not found at %s: %v", root, err)
	}
	return root
}

func decodeTOMLFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return decodeTOMLString(t, string(data))
}

func decodeTOMLString(t *testing.T, data string) map[string]any {
	t.Helper()
	var out map[string]any
	if _, err := toml.Decode(data, &out); err != nil {
		t.Fatalf("decoding TOML: %v", err)
	}
	return out
}

func globTOML(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(matches)
	return matches
}

// asMapSlice normalizes an array-of-tables value (e.g. [[steps]], [[webhook]])
// decoded into an interface{} field -- BurntSushi's generic decode produces
// either []map[string]any or []any depending on nesting, so callers get a
// single shape regardless.
func asMapSlice(v any) []map[string]any {
	switch vv := v.(type) {
	case []map[string]any:
		return vv
	case []any:
		out := make([]map[string]any, 0, len(vv))
		for _, item := range vv {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// The 15 top-level tables internal/config/pack.go's PackConfig struct
// actually decodes. The loader REJECTS UNKNOWN KEYS -- an invented field is
// a hard load failure, not a warning.
var knownPackTopLevelTables = map[string]bool{
	"pack": true, "imports": true, "agent_defaults": true, "agent": true,
	"named_session": true, "service": true, "webhook": true, "providers": true,
	"upstreams": true, "runtimes": true, "patches": true, "doctor": true,
	"commands": true, "global": true, "pricing": true,
}

func TestPackTOMLUsesOnlyKnownTopLevelTables(t *testing.T) {
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "pack.toml"))
	for key := range cfg {
		if !knownPackTopLevelTables[key] {
			t.Errorf("pack.toml has top-level key %q, which is not one of the 15 tables "+
				"internal/config/pack.go's PackConfig decodes -- the real loader rejects "+
				"unknown keys. (Note: `schema` belongs inside [pack], not at the top level --"+
				" see PackMeta.Schema.)", key)
		}
	}
	if _, ok := cfg["pack"]; !ok {
		t.Error("pack.toml has no [pack] table")
	}
}

// Proves the mechanism above actually catches something, using the EXACT
// mistake an early draft of this pack made: a bare top-level `schema = 2`
// (PackConfig has no such field -- schema lives at [pack].schema). This is a
// permanent regression test, not a one-off manual check.
func TestPackTOMLUsesOnlyKnownTopLevelTablesCatchesABadKey(t *testing.T) {
	bad := decodeTOMLString(t, "schema = 2\n\n[pack]\nname = \"x\"\nschema = 2\n")
	foundBad := false
	for key := range bad {
		if !knownPackTopLevelTables[key] {
			foundBad = true
		}
	}
	if !foundBad {
		t.Fatal("a bare top-level `schema` key was not flagged as unknown -- " +
			"the known-table check is not doing its job")
	}
}

// There is NO [[order]] and NO [[formula]] in pack.toml. Orders are
// orders/<name>.toml and formulas are formulas/*.toml, discovered by
// well-known directory (pack-spec.md 1.2.8, 1.2.14).
func TestPackTOMLHasNoInlineOrdersOrFormulas(t *testing.T) {
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "pack.toml"))
	for _, key := range []string{"order", "orders", "formula", "formulas"} {
		if v, ok := cfg[key]; ok {
			t.Errorf("pack.toml has a top-level %q key (%+v) -- orders/formulas are discovered "+
				"from orders/*.toml and formulas/*.toml, never declared inline", key, v)
		}
	}
}

// Owner decision: the two Go servers (gonk-intake, gonk-meter) are chart
// Deployments, not pack [[service]] proxy_process entries -- a proxy_process
// couples the BUDGET ENFORCER's availability to the Gas City controller's
// lifecycle.
func TestPackTOMLHasNoService(t *testing.T) {
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "pack.toml"))
	if v, ok := cfg["service"]; ok {
		t.Errorf("pack.toml declares [[service]] = %+v -- see pack.toml's own comment on why "+
			"there is none (owner decision, 2026-07-13)", v)
	}
}

// The pack ships no webhook (see pack.toml's comment: GitLab's X-Gitlab-Token
// is not in the closed 5-scheme verify registry). If one is ever added, its
// verify scheme must be one of the five the loader knows.
func TestNoWebhookOrOnlyAKnownVerifyScheme(t *testing.T) {
	schemes := map[string]bool{
		"github-hmac-sha256": true, "hmac-sha256": true, "slack-v0": true,
		"discord-ed25519": true, "jwt-jwks": true,
	}
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "pack.toml"))
	raw, ok := cfg["webhook"]
	if !ok {
		return // gonk ships none -- this is the expected, documented case.
	}
	for _, hook := range asMapSlice(raw) {
		verify, _ := hook["verify"].(map[string]any)
		scheme, _ := verify["scheme"].(string)
		if !schemes[scheme] {
			t.Errorf("webhook %v declares verify.scheme %q, not one of the 5 known schemes -- "+
				"X-Gitlab-Token is NOT among them (hmac-sha256 is an HMAC over the body and "+
				"will not substitute); adding a GitLab webhook here is a bug, not a feature",
				hook["name"], scheme)
		}
	}
}

// formula XOR exec, never both. Exec orders may not have a pool (no agent
// pipeline to route to). Formula orders need a pool or their work is never
// claimed by any agent.
func TestOrdersAreFormulaXorExecAndExecOrdersHaveNoPool(t *testing.T) {
	for _, path := range globTOML(t, filepath.Join(packRoot(t), "orders")) {
		cfg := decodeTOMLFile(t, path)
		order, ok := cfg["order"].(map[string]any)
		if !ok {
			t.Errorf("%s has no [order] table", path)
			continue
		}
		_, hasFormula := order["formula"]
		_, hasExec := order["exec"]
		if hasFormula == hasExec {
			t.Errorf("%s: order must declare exactly one of formula/exec (xor), got formula=%v exec=%v",
				path, hasFormula, hasExec)
		}
		_, hasPool := order["pool"]
		if hasExec && hasPool {
			t.Errorf("%s: exec orders may not have a pool (internal/orders/order.go's Validate rejects it)", path)
		}
		if hasFormula && !hasPool {
			t.Errorf("%s: formula order declares no pool -- its work will never be routed to an agent", path)
		}
	}
}

// internal/orders/order.go's OrderParam has exactly one field: `required`
// (bool). There is no `description` field on the real struct.
func TestOrderParamsHaveOnlyKnownFields(t *testing.T) {
	for _, path := range globTOML(t, filepath.Join(packRoot(t), "orders")) {
		cfg := decodeTOMLFile(t, path)
		order, _ := cfg["order"].(map[string]any)
		params, _ := order["params"].(map[string]any)
		for name, raw := range params {
			p, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s: order.params.%s is not a table", path, name)
				continue
			}
			for field := range p {
				if field != "required" {
					t.Errorf("%s: order.params.%s has field %q -- OrderParam has exactly one "+
						"field, `required`; there is no `description` on the real struct "+
						"(internal/orders/order.go)", path, name, field)
				}
			}
		}
	}
}

// The exec target of every exec order, and the check.path of every
// [steps.check], must exist on disk and be a file (not a directory). A pack
// file referencing a script that does not exist is a silent no-op at
// dispatch time in production, on the money path.
func TestExecAndCheckScriptsExistOnDisk(t *testing.T) {
	root := packRoot(t)
	for _, path := range globTOML(t, filepath.Join(root, "orders")) {
		cfg := decodeTOMLFile(t, path)
		order, _ := cfg["order"].(map[string]any)
		exec, _ := order["exec"].(string)
		if exec == "" {
			continue
		}
		full := filepath.Join(root, exec)
		if fi, err := os.Stat(full); err != nil || fi.IsDir() {
			t.Errorf("%s: exec = %q does not exist on disk at %s", path, exec, full)
		}
	}
	for _, path := range globTOML(t, filepath.Join(root, "formulas")) {
		cfg := decodeTOMLFile(t, path)
		for _, step := range asMapSlice(cfg["steps"]) {
			check, ok := step["check"].(map[string]any)
			if !ok {
				continue
			}
			inner, ok := check["check"].(map[string]any)
			if !ok {
				t.Errorf("%s: [steps.check] has no nested [steps.check.check] table (mode/path/timeout)", path)
				continue
			}
			if mode, _ := inner["mode"].(string); mode != "exec" {
				t.Errorf("%s: [steps.check.check].mode = %q, want \"exec\" (the only supported checker)", path, mode)
			}
			p, _ := inner["path"].(string)
			if p == "" {
				t.Errorf("%s: [steps.check.check] has no path", path)
				continue
			}
			full := filepath.Join(root, p)
			if fi, err := os.Stat(full); err != nil || fi.IsDir() {
				t.Errorf("%s: check.path = %q does not exist on disk at %s", path, p, full)
			}
		}
	}
}

// Doctor checks are directories under doctor/, each with a runnable run.sh
// (pack-spec.md 1.2.10).
func TestDoctorChecksHaveRunnableScripts(t *testing.T) {
	doctorDir := filepath.Join(packRoot(t), "doctor")
	entries, err := os.ReadDir(doctorDir)
	if err != nil {
		t.Fatalf("reading doctor/: %v", err)
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found++
		runPath := filepath.Join(doctorDir, e.Name(), "run.sh")
		fi, err := os.Stat(runPath)
		if err != nil {
			t.Errorf("doctor/%s has no run.sh: %v", e.Name(), err)
			continue
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("doctor/%s/run.sh is not executable", e.Name())
		}
	}
	if found == 0 {
		t.Error("no doctor checks defined under doctor/")
	}
}

// deterministicAgents are the agents that are NOT model agents: no provider, no
// prompt, no comment, no tokens. They are exempt from the prompt.template.md and
// bead-marker rules below, which encode "an agent is a thing that talks to a
// model and posts a marked comment" -- true for triage/scaffold/mention, false
// for Gas City's control lane.
//
// control-dispatcher is the deterministic compiler-v2 workflow control worker
// (prompt_mode = "none", start_command runs `gc convoy control --serve`). Giving
// it a prompt template would be inventing a prompt for something that never
// receives one.
var deterministicAgents = map[string]bool{
	"control-dispatcher": true,
}

// EVERY MODEL AGENT MUST BE ABLE TO START ON THIS IMAGE.
//
// This is the test that was missing, and its absence cost a live debugging
// session on 2026-08-02: scaffold was ported to the broker, dispatched
// correctly, reserved budget -- and ran nothing, because its agent.toml
// carried only a description and min_active_sessions. With no start_command
// the loader falls through to the catalog's default provider, whose binary
// this image does not ship, and the controller skipped the pool with
//
//	pool "scaffold": agent "scaffold": provider not found in PATH ... (skipping)
//
// mention had the identical gap. Nothing failed: the order fired, the meter
// reserved, and no session was ever created. Every OTHER packtest passed,
// because they check the pack's CONTENT and this is about whether an agent can
// RUN.
//
// The deterministic agents are exempt by the same rule the chart's
// bootstrap-city uses when it injects LiteLLM config: they run no model, so
// they need no harness and must not be given a credential.
func TestEveryModelAgentCanActuallyStart(t *testing.T) {
	agentsDir := filepath.Join(packRoot(t), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	// HOME and the XDG vars are not decoration. Gas City's session passthrough
	// copies the CONTROLLER's HOME into every session pod, where that path does
	// not exist and is not writable; the session dies on mkdir and the
	// reconciler respawns it in a loop. Overriding HOME alone is NOT enough --
	// opencode is XDG-aware and follows XDG_CONFIG_HOME straight back to the
	// same unwritable path. All five, or none of it works.
	requiredEnv := []string{
		"HOME",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		// Without this a non-interactive run blocks forever on its first
		// tool-permission prompt, which reads as a hung agent, not a config bug.
		"OPENCODE_PERMISSION",
	}

	for _, e := range entries {
		if !e.IsDir() || deterministicAgents[e.Name()] {
			continue
		}
		name := e.Name()
		cfg := decodeTOMLFile(t, filepath.Join(agentsDir, name, "agent.toml"))

		if s, _ := cfg["start_command"].(string); strings.TrimSpace(s) == "" {
			t.Errorf("agents/%s/agent.toml has no start_command -- the loader will resolve the "+
				"catalog default provider, whose binary this image does not ship, and the pool "+
				"is skipped with 'provider not found in PATH'. No session is ever created and "+
				"NOTHING reports an error.", name)
		}
		env, _ := cfg["env"].(map[string]any)
		for _, key := range requiredEnv {
			if v, _ := env[key].(string); strings.TrimSpace(v) == "" {
				t.Errorf("agents/%s/agent.toml [env] is missing %s -- see the comments in "+
					"agents/triage/agent.toml for why each of these is load-bearing", name, key)
			}
		}
	}
}

// The DIRECTORY NAME is the agent name. Both files exist for every agent
// this pack ships, and a stray `name` field inside agent.toml is IGNORED by
// the loader -- which means someone will one day set it, believe it, and be
// wrong.
func TestEveryAgentDirHasBothFilesAndNoNameField(t *testing.T) {
	agentsDir := filepath.Join(packRoot(t), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found++
		name := e.Name()
		dir := filepath.Join(agentsDir, name)
		if _, err := os.Stat(filepath.Join(dir, "prompt.template.md")); err != nil && !deterministicAgents[name] {
			t.Errorf("agents/%s has no prompt.template.md", name)
		}
		agentTOML := filepath.Join(dir, "agent.toml")
		if _, err := os.Stat(agentTOML); err != nil {
			t.Errorf("agents/%s has no agent.toml", name)
			continue
		}
		cfg := decodeTOMLFile(t, agentTOML)
		if _, ok := cfg["name"]; ok {
			t.Errorf("agents/%s/agent.toml declares a `name` field -- the loader ignores it "+
				"for identity and uses the directory name (pack-spec.md 1.2.4); do not rely on it", name)
		}
	}
	if found == 0 {
		t.Error("no agents defined under agents/")
	}
}

// Every formula order's pool names a real agent directory. Gas City routes a
// pool's ready work to ANY agent whose work query matches that pool label
// (docs/tutorials/06-beads.md, "How agents find work") -- an unknown pool
// name means the work is produced but never claimed by anyone.
func TestFormulaOrdersRouteToRealAgents(t *testing.T) {
	agentNames := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(packRoot(t), "agents"))
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			agentNames[e.Name()] = true
		}
	}
	for _, path := range globTOML(t, filepath.Join(packRoot(t), "orders")) {
		cfg := decodeTOMLFile(t, path)
		order, _ := cfg["order"].(map[string]any)
		if _, hasFormula := order["formula"]; !hasFormula {
			continue
		}
		pool, _ := order["pool"].(string)
		if pool == "" {
			continue // flagged separately by TestOrdersAreFormulaXorExecAndExecOrdersHaveNoPool
		}
		if !agentNames[pool] {
			t.Errorf("%s: pool %q names no agent directory under agents/", path, pool)
		}
	}
}

// Every order named in cmd/gonk-gate's orderForTrigger map exists as an order
// file that declares a formula. That map is unexported (package main), so
// this is a hand-kept literal copy -- cmd/gonk-gate's own tests
// (TestDispatchPoursOnRun et al.) pin the same three names from the other
// side. A typo here is a 404 at dispatch time, in production, on the money
// path.
func TestEveryTriggerOrderExists(t *testing.T) {
	triggerOrder := map[string]string{
		"issue-triage":  "gonk-triage",
		"scaffold":      "gonk-scaffold",
		"mention-reply": "gonk-mention",
	}
	for trigger, order := range triggerOrder {
		path := filepath.Join(packRoot(t), "orders", order+".toml")
		cfg, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("trigger %q maps to order %q, but %s does not exist: %v", trigger, order, path, err)
			continue
		}
		parsed := decodeTOMLString(t, string(cfg))
		if o, _ := parsed["order"].(map[string]any); o == nil || o["formula"] == nil {
			t.Errorf("%s exists but declares no formula", path)
		}
	}
}

// formulas v2 forbids declaring vars named convoy_id, bead_id, or the
// deprecated issue alias (internal/graphv2/invocation.go: "vars.<name>:
// formulas v2 reserved variable cannot be declared"). More importantly, any
// CALLER-supplied vars map (i.e. what a formula order is fired with) that
// contains one of these keys is rejected outright, regardless of whether the
// formula declares it -- see cmd/gonk-gate/dispatch_test.go's
// TestDispatchNeverSendsAReservedFormulaVarName for the other half of this
// contract.
func TestFormulaVarsNeverDeclareAReservedFormulasV2Name(t *testing.T) {
	reserved := map[string]bool{"convoy_id": true, "bead_id": true, "issue": true}
	for _, path := range globTOML(t, filepath.Join(packRoot(t), "formulas")) {
		cfg := decodeTOMLFile(t, path)
		vars, _ := cfg["vars"].(map[string]any)
		for name := range vars {
			if reserved[name] {
				t.Errorf("%s declares reserved var %q -- formulas v2 forbids declaring "+
					"convoy_id/bead_id/issue", path, name)
			}
		}
	}
}

// [steps.check] is graph-only (formula-spec-v2.md section 5): "a formula
// that uses them without [the v2 declaration] must fail to compile." Every
// formula using [steps.check] here must declare
// [requires] formula_compiler = ">=2.0.0" (or the equivalent legacy
// contract = "graph.v2" alias).
func TestFormulasUsingCheckDeclareGraphV2(t *testing.T) {
	for _, path := range globTOML(t, filepath.Join(packRoot(t), "formulas")) {
		cfg := decodeTOMLFile(t, path)
		usesCheck := false
		for _, step := range asMapSlice(cfg["steps"]) {
			if _, ok := step["check"]; ok {
				usesCheck = true
			}
		}
		if !usesCheck {
			continue
		}
		requires, _ := cfg["requires"].(map[string]any)
		_, hasCompilerReq := requires["formula_compiler"]
		hasContract := cfg["contract"] == "graph.v2"
		if !hasCompilerReq && !hasContract {
			t.Errorf("%s uses [steps.check] (graph-only) without [requires] "+
				"formula_compiler = \">=2.0.0\" -- it must fail to compile under the real loader", path)
		}
	}
}

// *** THE MARKER IS LOAD-BEARING. *** gonk-gate check and gonk-gate sweep
// grep for it. If a prompt or formula-step rewrite drops it, the gate goes
// blind: every successful session classifies as gate-failed, every project
// climbs its ladder to the most expensive rung, and the bill arrives before
// the bug report. The instruction lives in two layers by design (see
// agents/*/prompt.template.md's own comment: the agent prompt is a static,
// per-session role description; the actual bead_id is only known at the
// per-dispatch formula-step level) -- so this checks BOTH.
func TestEveryAgentAndFormulaStepEmitsTheBeadMarker(t *testing.T) {
	const marker = "<!-- gonk:bead:"
	agentsDir := filepath.Join(packRoot(t), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(agentsDir, e.Name(), "prompt.template.md")
		data, err := os.ReadFile(p)
		if err != nil {
			continue // flagged separately by TestEveryAgentDirHasBothFilesAndNoNameField
		}
		if !strings.Contains(string(data), marker) {
			t.Errorf("%s does not instruct the agent about the bead marker", p)
		}
	}
	formulaPaths := globTOML(t, filepath.Join(packRoot(t), "formulas"))
	if len(formulaPaths) == 0 {
		t.Fatal("no formulas defined")
	}
	for _, path := range formulaPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(data), marker) {
			t.Errorf("%s: no step description carries the bead marker instruction -- without "+
				"it the deterministic gate cannot tell success from failure, and every project "+
				"escalates to its most expensive rung", path)
		}
	}
}

// AD-1: THE PACK NAMES NO MODEL. Rungs map to models in the OPERATOR's
// catalog (pkg/opercfg), never here. A model name in an open-source pack
// hardcodes one site's inference topology and silently disagrees with the
// rung the budget was checked against. This is the Go-test twin of Step 7's
// `make lint-pack` grep, so `go test ./...` alone catches a regression.
func TestNoModelNameAppearsAnywhereInThePack(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)qwen|llama|gpt-|claude|sonnet\b|opus|haiku|glm|mistral|gemini|ollama|vllm`)
	root := packRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if loc := pattern.FindStringIndex(string(data)); loc != nil {
			t.Errorf("%s names a model (matched %q) -- the pack must be model-agnostic; "+
				"rungs map to models in the operator's catalog (pkg/opercfg), never here",
				path, string(data)[loc[0]:loc[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// The pack holds no credential and no bare secret value.
func TestNoCredentialShapedStringInThePack(t *testing.T) {
	pattern := regexp.MustCompile(`(?i)sk-[a-z0-9]|glpat-[a-z0-9]`)
	root := packRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if pattern.Match(data) {
			t.Errorf("%s contains a credential-shaped string", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
