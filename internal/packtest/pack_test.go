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
//   - docs/reference/specs/pack-spec.md   -- agent directory layout
//
// This is not a substitute for Task 6's real-loader-in-a-container check --
// it is the fast gate that catches the mistakes we already know we can
// make: an unknown pack.toml key, an exec order with a pool, a missing
// agent directory, an exec script that does not exist on disk.
//
// ADR-007 §3 deleted gonk's whole formula layer (formulas/, [steps.check],
// and the control-dispatcher agent that routed their workflow-control
// beads), so this pack currently declares no formula order at all. The
// formula-xor-exec/no-pool-on-exec rule above still applies to any order
// this pack ships (including the two plain exec orders it has now); the
// formulas v2 reserved-var and [steps.check]/formula_compiler rules this
// package used to also enforce had nothing left to check once formulas/
// was deleted, and were deleted with it.
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

// The exec target of every exec order must exist on disk and be a file (not
// a directory). A pack file referencing a script that does not exist is a
// silent no-op at dispatch time in production, on the money path.
//
// This used to also walk formulas/*.toml's [steps.check].check.path: ADR-007
// §3 deleted the whole formula layer -- formulas, [steps.check], and the
// `gonk-gate check` subcommand that was its body -- so there is no longer a
// formulas/ directory, and no check.path left to validate.
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
// There used to be a deterministicAgents exemption here for control-dispatcher,
// Gas City's compiler-v2 workflow-control worker -- needed only because
// graph-v2 formulas ([steps.check]) routed their workflow-control beads
// through it. ADR-007 §3 deleted the whole formula layer, control-dispatcher
// with it (pack/agents/control-dispatcher/), so every agent this pack ships
// now is a model agent and none is exempt.
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
		if !e.IsDir() {
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

// The DIRECTORY NAME is the agent name. agent.toml exists for every agent
// this pack ships, and a stray `name` field inside it is IGNORED by the
// loader -- which means someone will one day set it, believe it, and be
// wrong.
//
// This used to also require a prompt.template.md alongside agent.toml.
// ADR-007 §3 deleted every one: the broker path
// (cmd/gonk-gate/broker_inject.go's renderTriagePrompt/renderScaffoldPrompt)
// renders each agent's prompt in Go, splicing in controller-fetched
// issue/repo context that a static template file could never hold, so no
// agent this pack ships carries one any more.
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

// Every trigger in cmd/gonk-gate's agentForTrigger map (broker_inject.go)
// names a real agent directory under agents/. That map is unexported
// (package main), so this is a hand-kept literal copy -- cmd/gonk-gate's own
// tests (TestDispatchCreatesTriageSessionOnRun et al.) pin the same names
// from the other side. A typo here is a session-create 404 at dispatch time,
// in production, on the money path.
//
// There used to be a THIRD entry, orderForTrigger, mapping trigger to a
// formula-order name (gonk-triage/gonk-scaffold/gonk-mention). ADR-007 §3
// deleted it along with the rest of the formula layer: a ported trigger no
// longer pours an order at all, it goes straight through agentForTrigger to
// a broker session.
//
// mention-reply is DELIBERATELY ABSENT from agentForTrigger, and so from the
// table below too. It used to pour a formula (gonk-mention) that provably
// could not deliver its prompt (upstream Gas City drops caller vars,
// gonk-6gs / #4668) -- ADR-007 §3 chose to fail it loudly instead of leaving
// it to idle, and it is not yet ported onto the broker either, so
// runDispatch refuses it outright
// (cmd/gonk-gate: TestDispatchRefusesMentionReplyUntilItIsPortedToTheBroker).
// T-24 ports it for real; add it here when it does.
func TestEveryTriggerRoutesToARealAgent(t *testing.T) {
	triggerAgent := map[string]string{
		"issue-triage": "triage",
		"scaffold":     "scaffold",
	}
	agentsDir := filepath.Join(packRoot(t), "agents")
	for trigger, agent := range triggerAgent {
		t.Run(trigger, func(t *testing.T) {
			dir := filepath.Join(agentsDir, agent)
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				t.Errorf("trigger %q maps to agent %q, but agents/%s does not exist", trigger, agent, agent)
			}
		})
	}
	t.Run("mention-reply", func(t *testing.T) {
		t.Skip("mention-reply is not yet on agentForTrigger -- T-24 ports it onto the broker; " +
			"until then runDispatch refuses it (see cmd/gonk-gate's " +
			"TestDispatchRefusesMentionReplyUntilItIsPortedToTheBroker)")
	})
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
