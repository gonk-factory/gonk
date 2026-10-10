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
	"encoding/json"
	"fmt"
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
		"issue-triage":       "triage",
		"scaffold":           "scaffold",
		"pipeline-diagnosis": "pipeline", // gonk-tios P7
		"pipeline-fix":       "pipefix",  // gonk-2usw
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

// ---------------------------------------------------------------------------
// OPENCODE_PERMISSION (gonk-7b79)
// ---------------------------------------------------------------------------

// deniedPermissions are the opencode permission keys EVERY agent in this pack
// must set to "deny".
//
// The list is not a taste preference. Each entry is a capability the agent's
// own prompt already forbids in prose (cmd/gonk-gate/broker_inject.go:
// renderTriagePrompt, renderScaffoldPrompt, renderScaffoldCheckoutPrompt all
// say some version of "do NOT run git, glab, bd, or any external API" and "do
// NOT fetch anything yourself"), and the point of this file is that the harness
// enforces it instead of the prompt asking nicely. The untrusted half is real:
// broker_inject.go splices the issue body into the prompt through capBody,
// whose own doc comment calls it "truncates an UNTRUSTED issue body".
//
// The key names are opencode's permission keys, NOT its tool ids, and the two
// differ in exactly one place: edit/write/apply_patch ALL map to the "edit"
// permission (packages/opencode/src/permission/index.ts, `disabled()`), so the
// single "edit" entry removes all three tools. Everything else maps to its own
// name, including the shell tool, whose id is "bash"
// (packages/opencode/src/tool/shell/id.ts).
var deniedPermissions = []string{"bash", "edit", "webfetch", "websearch", "task", "question"}

// TestAgentPermissionsAreNarrowed is the guard on gonk-7b79's fix.
//
// IT EXISTS BECAUSE THE OBVIOUS EDIT IS THE WRONG ONE. Every one of these
// files used to read OPENCODE_PERMISSION = '{"*":"allow"}', and reverting to
// that is a one-token change that makes every symptom go away, produces no
// error, and silently hands a model reading untrusted issue text a shell, a
// file writer and a URL fetcher in a pod whose NetworkPolicy is inert on this
// cluster (flannel implements none -- gonk-dku). Nothing else in the repo would
// notice: the pack loads, the session starts, the agent works. This test is the
// only thing that notices.
//
// The three properties asserted are the three that can each independently
// break the fix:
//
//  1. THE CATCH-ALL IS FIRST AND IS "allow". opencode evaluates permissions
//     with `findLast` over the rules in config key order
//     (packages/opencode/src/permission/index.ts `evaluate` + `fromConfig`), so
//     a "*" written AFTER the denials re-allows everything and the file still
//     looks careful. "allow" rather than an allow-list because an unmatched
//     permission falls through to opencode's `{action: "ask"}` fallback, and an
//     "ask" in a pod with no operator blocks until the reservation TTL reaps
//     the session -- which is the failure the packs' own comments were written
//     about.
//
//  2. EVERY KEY IN deniedPermissions IS PRESENT AND IS "deny". Present matters
//     as much as the value: a missing key is not a denial, it is the catch-all
//     allowing the tool.
//
//  3. EVERY VALUE IS "allow" OR "deny". This is the hang guard, and it is
//     deliberately stated as a WHITELIST rather than as "nothing is 'ask'"
//     (gonk-46gy). opencode never validates this string -- OPENCODE_PERMISSION
//     is merged into the config AFTER the schema decode (config/config.ts) --
//     and permission/index.ts `ask` treats every action that is not exactly
//     "deny" or "allow" as an ask, awaiting a Deferred with no timeout, while
//     `disabled()` (which tests for "deny") leaves the tool in the model's
//     list. So `"read":"alow"` both hangs the session and un-narrows the
//     agent, and a test that matched the literal "ask" passed it.
//
//  4. NO KEY AFTER THE CATCH-ALL CARRIES A WILDCARD (gonk-46gy). Property 1
//     says the catch-all leads; this says nothing later acts as one. Keys are
//     matched with Wildcard.match, so a trailing `"bas*":"allow"` becomes the
//     `findLast` for permission "bash" and re-allows it over the explicit
//     `"bash":"deny"` above it.
func TestAgentPermissionsAreNarrowed(t *testing.T) {
	agentsDir := filepath.Join(packRoot(t), "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		seen++
		cfg := decodeTOMLFile(t, filepath.Join(agentsDir, name, "agent.toml"))
		env, _ := cfg["env"].(map[string]any)
		raw, _ := env["OPENCODE_PERMISSION"].(string)
		if strings.TrimSpace(raw) == "" {
			// TestEveryModelAgentCanActuallyStart already reports the absence
			// with the reasoning; do not duplicate its message here.
			continue
		}

		entries, err := orderedJSONObject(raw)
		if err != nil {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION is not a JSON object: %v\n"+
				"opencode parses this value with JSON.parse and, on failure, logs "+
				"\"OPENCODE_PERMISSION contains invalid JSON, skipping\" and CARRIES ON with "+
				"its own defaults (packages/opencode/src/config/config.ts) -- so a typo here "+
				"does not fail the pod, it silently restores an unnarrowed agent.", name, err)
			continue
		}

		// Flatten for the checks below. A key whose value was a per-pattern
		// object has no scalar action, so it is deliberately ABSENT from
		// values -- TestExternalDirectoryIsRestoredAfterTheCatchAll owns those,
		// and a silent zero value here would let a nested map pass the
		// allow/deny check by looking like an empty string.
		keys := make([]string, 0, len(entries))
		values := map[string]string{}
		nested := map[string][]permEntry{}
		for _, e := range entries {
			keys = append(keys, e.key)
			if e.nestedValue() {
				nested[e.key] = e.nested
				continue
			}
			values[e.key] = e.action
		}

		if len(keys) == 0 || keys[0] != "*" {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION must list \"*\" FIRST, got %v -- "+
				"opencode takes the LAST matching rule in key order (permission/index.ts "+
				"`evaluate`), so a trailing \"*\" re-allows every tool the denials below it "+
				"were supposed to remove, in a file that still reads as though it denied them.",
				name, keys)
		} else if values["*"] != "allow" {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION has \"*\":%q, want \"allow\" -- "+
				"the catch-all is the NO-HANG property. Anything not named explicitly must "+
				"resolve to allow, or an unlisted permission falls through to opencode's "+
				"`{action: \"ask\"}` fallback and blocks a session nobody can answer.",
				name, values["*"])
		}

		for _, key := range deniedPermissions {
			if _, isNested := nested[key]; isNested {
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION gives %q a per-pattern object "+
					"(gonk-7b79) -- these six must be a FLAT \"deny\". Only a rule whose pattern "+
					"is exactly \"*\" and whose action is \"deny\" removes the tool from the list "+
					"the model is sent (permission/index.ts `disabled()` tests "+
					"`rule.pattern === \"*\" && rule.action === \"deny\"`), so a pattern map leaves "+
					"the tool armed and merely refuses some calls.", name, key)
				continue
			}
			got, ok := values[key]
			if !ok {
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION does not mention %q (gonk-7b79) -- "+
					"an ABSENT key is not a denial, it is the \"*\":\"allow\" catch-all handing the "+
					"agent that tool. See agents/triage/agent.toml for why each of these is denied.",
					name, key)
				continue
			}
			if got != "deny" {
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION has %q:%q, want \"deny\" (gonk-7b79) -- "+
					"untrusted issue text reaches this model (cmd/gonk-gate/broker_inject.go splices "+
					"iss.Description through capBody, \"truncates an UNTRUSTED issue body\"), and "+
					"\"deny\" is what removes the tool from the list the model is even offered "+
					"(permission/index.ts `disabled()` via session/llm/request.ts `resolveTools`).",
					name, key, got)
			}
		}

		// EVERY VALUE IS "allow" OR "deny". This is NOT the same check as
		// "nothing is spelled \"ask\"", which is what this loop used to do, and
		// the difference is a hang (gonk-46gy).
		//
		// NOTHING VALIDATES THIS STRING BUT THIS TEST. opencode's config
		// schema does have an Action literal ("ask"|"allow"|"deny",
		// packages/core/src/v1/config/permission.ts), but OPENCODE_PERMISSION
		// never meets it: the env var is JSON.parse'd and mergeDeep'd into the
		// ALREADY-DECODED config (packages/opencode/src/config/config.ts,
		// `if (Flag.OPENCODE_PERMISSION) { result.permission = mergeDeep(...) }`),
		// so an unknown action reaches `evaluate` intact.
		//
		// AND AN UNKNOWN ACTION IS AN "ask" THAT ALSO LEAVES THE TOOL ARMED.
		// permission/index.ts `ask` branches with no default case --
		//     if (rule.action === "deny") return DeniedError
		//     if (rule.action === "allow") continue
		//     needsAsk = true
		// -- so "alow" awaits a Deferred with NO TIMEOUT, while `disabled()`
		// (which tests `rule.action === "deny"`) keeps the tool in the list the
		// model is sent. One transposed letter therefore hangs the session AND
		// un-narrows the agent. Matching the literal "ask" caught the spelling
		// of the mistake and not the property.
		for _, e := range entries {
			if e.nestedValue() {
				// A per-pattern map is NOT exempt from this property, it is
				// checked by a different test that also knows what the
				// patterns are supposed to be
				// (TestExternalDirectoryIsRestoredAfterTheCatchAll). What is
				// enforced here is that only the ONE key that test knows about
				// may take this shape at all, so a second nested map cannot
				// arrive unexamined.
				if e.key != "external_directory" {
					t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION gives %q a per-pattern object. "+
						"Only \"external_directory\" may (gonk-blxu), because that is the only one "+
						"with a test that knows which patterns belong in it. A pattern map nobody "+
						"checks is the failure this whole file exists for: it renders as a control "+
						"and enforces whatever the patterns happen to match.", name, e.key)
				}
				continue
			}
			key := e.key
			switch got := e.action; got {
			case "allow", "deny":
				// The only two actions a TOP-LEVEL key in this pack may use.
			case "ask":
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION sets %q:\"ask\" -- NEVER. "+
					"There is no operator attached to a gonk agent pod, so an \"ask\" blocks the "+
					"session until the reservation TTL reaps it, which burns budget and reads as "+
					"an infra failure rather than a config choice. Use \"deny\": it removes the "+
					"tool outright and cannot hang.", name, key)
			default:
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION sets %q:%q, which is not an "+
					"opencode action -- the only values allowed here are \"allow\" and \"deny\" "+
					"(gonk-46gy). This is NOT a pedantic spelling check. opencode does not "+
					"validate this value (the env var is merged into the config AFTER the schema "+
					"decode, config/config.ts), and permission/index.ts `ask` treats ANYTHING "+
					"that is neither \"deny\" nor \"allow\" as an ask -- it awaits a Deferred "+
					"with no timeout, in a pod with no operator -- while `disabled()` tests for "+
					"\"deny\" exactly and so leaves the tool in the list the model is sent. A "+
					"typo here hangs the session and un-narrows the agent at the same time.",
					name, key, got)
			}
		}

		// NO WILDCARD KEY AFTER THE CATCH-ALL (gonk-46gy). The keys[0] == "*"
		// check above establishes that the catch-all leads; it does not
		// establish that nothing LATER acts as one. Permission keys are matched
		// with Wildcard.match (packages/core/src/util/wildcard.ts), which turns
		// "*" into ".*" and "?" into "." -- so `{"*":"allow", ...,
		// "bash":"deny", "bas*":"allow"}` makes that trailing rule the
		// `findLast` for permission "bash" and silently re-allows the tool the
		// line above it denied, in a file that still reads as though bash were
		// denied. Only the leading catch-all may carry a metacharacter; every
		// denial after it must name exactly one permission.
		for i, key := range keys {
			if i == 0 {
				continue
			}
			if strings.ContainsAny(key, "*?") {
				t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION key %q (position %d) contains a "+
					"wildcard metacharacter (gonk-46gy) -- only the leading \"*\" catch-all may. "+
					"opencode matches permission keys with Wildcard.match (\"*\" -> \".*\", "+
					"\"?\" -> \".\") and takes the LAST matching rule, so a pattern key here "+
					"outranks every explicit denial before it: %q:%q re-allows anything it "+
					"matches. Name each denied permission exactly.",
					name, key, i, key, values[key])
			}
		}
	}
	if seen == 0 {
		t.Fatal("no agent directories found under pack/agents -- this test asserted nothing")
	}
}

// TestExternalDirectoryIsRestoredAfterTheCatchAll guards gonk-blxu's fix, and
// what it guards is not a style rule -- it is a DEMONSTRATED credential
// exposure. With the old value, a live agent under a Kubernetes Job read
// /tmp/gonk/llkey and the LiteLLM virtual key's PLAINTEXT came back to the
// model in the tool result (docs/spikes/adr-008-t55-gate/runs/blxu-evidence.txt
// and runC-pod.log). With opencode's own defaults -- i.e. with no
// OPENCODE_PERMISSION at all -- the identical run was blocked:
//
//	! permission requested: external_directory (/tmp/gonk/*); auto-rejecting
//	x Read /tmp/gonk/llkey failed
//
// So the control already existed upstream and gonk's trailing "*":"allow" was
// switching it off, because OPENCODE_PERMISSION merges LAST and `evaluate`
// takes the LAST matching rule.
//
// THIS TEST ASSERTS BEHAVIOUR, NOT TEXT, and that is the whole point of it.
// The bead it closes rejected an earlier candidate fix -- a `read` pattern like
// "/tmp/gonk/*" -- precisely because `read` matches worktree-RELATIVE paths, so
// the rule would have rendered as a control and matched nothing. A whitelist
// entry with a typo in it would be the same defect wearing a different hat. So
// this test reimplements opencode's own two functions --
// `fromConfig` (flatten in key order) and `evaluate` (findLast + Wildcard.match,
// packages/opencode/src/permission/index.ts) -- and asks the resulting ruleset
// the two questions that matter, using the EXACT resource strings opencode
// builds (packages/core/src/location-mutation.ts: `<realpath dir>/*`):
//
//  1. external_directory for "/tmp/gonk/*" must NOT resolve to "allow".
//     That is the credential directory, spelled the way the observed run
//     spelled it.
//
//  2. external_directory for the truncated-tool-output directory MUST resolve
//     to "allow". opencode INVITES the model to open that path when a tool's
//     output is truncated ("Full output saved to: ...; Use Grep to search the
//     full content or Read with offset/limit" -- packages/opencode/src/tool/
//     truncate.ts), and upstream whitelists it for exactly that reason
//     (agent/agent.ts: `whitelistedDirs = [Truncate.GLOB, ...]`).
//
//     STATED AT ITS REAL STRENGTH: this assertion pins gonk's DECLARED intent,
//     not the mechanism that makes truncation work. opencode re-appends
//     `external_directory: {Truncate.GLOB: "allow"}` as the last rule of every
//     agent unless the user config carries an explicit DENY on exactly that
//     pattern (agent/agent.ts:296-311, and upstream's own tests
//     test/agent/agent.test.ts:527/555). That was confirmed in the Job harness:
//     with this whitelist entry deleted, and again with the whole map replaced
//     by {"*":"deny"}, the truncated-output read still succeeded, while a
//     sibling directory one level up was auto-rejected in the same run. So the
//     check below cannot go red for the reason a reader first assumes -- what
//     it DOES catch is a "deny" written here, which is the one edit that
//     switches upstream's protection off, and a whitelist pattern that has
//     drifted from XDG_DATA_HOME.
//
// AND THE TRUNCATE PATH IS RECOMPUTED FROM THIS SAME FILE'S XDG_DATA_HOME
// rather than pasted, because that coupling is the part most likely to rot:
//
//	tool/truncate.ts:18       GLOB = path.join(TRUNCATION_DIR, "*")
//	tool/truncation-dir.ts:4  TRUNCATION_DIR = path.join(Global.Path.data, "tool-output")
//	core/src/global.ts:11     data = path.join(xdgData, "opencode")
//
// and xdgData is XDG_DATA_HOME, which each agent.toml sets in the same [env]
// block. Change XDG_DATA_HOME without changing the whitelist and the whitelist
// entry stops matching anything -- silently, since a dead ALLOW entry produces
// no error, just a truncated output the agent can no longer open. This test is
// what notices.
func TestExternalDirectoryIsRestoredAfterTheCatchAll(t *testing.T) {
	agentsDir := filepath.Join(packRoot(t), "agents")
	dirEntries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("reading agents/: %v", err)
	}
	seen := 0
	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		cfg := decodeTOMLFile(t, filepath.Join(agentsDir, name, "agent.toml"))
		env, _ := cfg["env"].(map[string]any)
		raw, _ := env["OPENCODE_PERMISSION"].(string)
		if strings.TrimSpace(raw) == "" {
			// TestEveryModelAgentCanActuallyStart reports the absence itself.
			continue
		}
		seen++

		entries, err := orderedJSONObject(raw)
		if err != nil {
			// TestAgentPermissionsAreNarrowed reports the parse failure with
			// the reasoning; do not duplicate it.
			continue
		}

		idx := -1
		for i, e := range entries {
			if e.key == "external_directory" {
				idx = i
			}
		}
		if idx < 0 {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION does not mention "+
				"\"external_directory\" (gonk-blxu) -- WITHOUT IT THE LITELLM VIRTUAL KEY AT "+
				"/tmp/gonk/llkey IS READABLE BY THE AGENT AND WAS OBSERVED COMING BACK IN A "+
				"TOOL RESULT. opencode ships this control on by default; the leading "+
				"\"*\":\"allow\" in this very line is what switches it off, because "+
				"OPENCODE_PERMISSION is merged LAST and `evaluate` takes the LAST matching "+
				"rule. Restoring it means naming it again, after the catch-all.", name)
			continue
		}
		if idx == 0 {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION lists \"external_directory\" "+
				"FIRST (gonk-blxu) -- it must come AFTER the \"*\":\"allow\" catch-all. "+
				"`evaluate` is findLast over the rules in key order, and the catch-all matches "+
				"permission \"external_directory\" with pattern \"*\" just like every other "+
				"permission, so a catch-all placed after this map overrides every rule in it "+
				"and the map enforces nothing.", name)
		}

		ed := entries[idx]
		if !ed.nestedValue() {
			t.Errorf("agents/%s/agent.toml OPENCODE_PERMISSION sets \"external_directory\":%q, "+
				"a bare action (gonk-blxu). It must be a PER-PATTERN MAP, because a bare action "+
				"cannot both block /tmp/gonk and keep opencode's truncated-tool-output directory "+
				"readable -- and the model is explicitly told to open that directory when a "+
				"tool's output is truncated (packages/opencode/src/tool/truncate.ts).",
				name, ed.action)
			continue
		}

		if len(ed.nested) == 0 || ed.nested[0].key != "*" {
			got := "(empty)"
			if len(ed.nested) > 0 {
				got = ed.nested[0].key
			}
			t.Errorf("agents/%s/agent.toml external_directory map starts with %q, want \"*\" "+
				"(gonk-blxu) -- the map is evaluated findLast in key order exactly like the "+
				"outer object, so the blocking rule must come FIRST and the whitelisted "+
				"directories after it. Reversed, \"*\" is the last match for every path and the "+
				"whitelist is what enforces nothing.", name, got)
		} else if ed.nested[0].action != "ask" {
			t.Errorf("agents/%s/agent.toml external_directory[\"*\"] is %q, want \"ask\" "+
				"(gonk-blxu) -- this mirrors opencode's own default "+
				"(packages/opencode/src/agent/agent.ts, `external_directory: {\"*\": \"ask\", "+
				"...whitelist}`). It does NOT hang a gonk pod: gonk only ever runs "+
				"`opencode run`, and that command's non-interactive loop answers every "+
				"permission.asked itself with a reject, printing \"auto-rejecting\" "+
				"(packages/opencode/src/cli/cmd/run.ts). This is the one place in this pack "+
				"where \"ask\" is allowed, and it is allowed because it was OBSERVED, in a "+
				"pod, completing the session in about eight seconds.", name, ed.nested[0].action)
		}

		for _, rule := range ed.nested {
			switch rule.action {
			case "allow", "deny", "ask":
			default:
				t.Errorf("agents/%s/agent.toml external_directory[%q] is %q, which is not an "+
					"opencode action (gonk-46gy) -- nothing validates this string but this test "+
					"(OPENCODE_PERMISSION is merged in AFTER the config schema decode), and "+
					"permission/index.ts treats anything that is neither \"deny\" nor \"allow\" "+
					"as an ask.", name, rule.key, rule.action)
			}
		}

		// THE RULESET, BUILT THE WAY opencode BUILDS IT: every key flattened in
		// order, the catch-all included, because the catch-all is a rule for
		// this permission too.
		rules := flattenPermissionConfig(entries)

		// The credential directory. "/tmp/gonk/*" is not a guess: it is the
		// string opencode printed in the run that proved the hole, because
		// location-mutation.ts asks with `<directory>/*`, not with the file.
		if got := evaluatePermissionRules(rules, "external_directory", "/tmp/gonk/*"); got == "allow" {
			t.Errorf("agents/%s/agent.toml resolves external_directory for \"/tmp/gonk/*\" to "+
				"\"allow\" (gonk-blxu) -- THAT IS THE DIRECTORY HOLDING THE LITELLM VIRTUAL KEY "+
				"(images/agent/entrypoint.sh materializes /tmp/gonk/llkey, mode 0600, owned by "+
				"65532 -- the same uid the agent runs as, so file permissions stop nothing). "+
				"An agent reading untrusted issue text has been observed reading it and handing "+
				"the plaintext back to the model. Whatever rule allows this, it is the wrong "+
				"one.", name)
		}

		dataHome, _ := env["XDG_DATA_HOME"].(string)
		if strings.TrimSpace(dataHome) == "" {
			t.Errorf("agents/%s/agent.toml sets OPENCODE_PERMISSION but no XDG_DATA_HOME "+
				"(gonk-blxu) -- opencode's truncated-tool-output directory is derived from it "+
				"(Global.Path.data = <xdgData>/opencode), so without it there is no way to "+
				"tell whether the external_directory whitelist names the right path, and a "+
				"whitelist naming the wrong path is a dead rule.", name)
			continue
		}
		wantTruncate := strings.TrimSuffix(dataHome, "/") + "/opencode/tool-output/*"
		if got := evaluatePermissionRules(rules, "external_directory", wantTruncate); got != "allow" {
			t.Errorf("agents/%s/agent.toml resolves external_directory for %q to %q, want "+
				"\"allow\" (gonk-blxu). That path is opencode's TRUNCATED-TOOL-OUTPUT directory, "+
				"recomputed here from this file's own XDG_DATA_HOME (%q) + \"/opencode/"+
				"tool-output/*\" -- Truncate.GLOB in packages/opencode/src/tool/truncate.ts. "+
				"When a tool's output exceeds the line/byte limit, opencode writes the full "+
				"text there and TELLS THE MODEL to open it, so this pack declares it allowed. "+
				"Either the whitelist entry is missing, or it no longer matches because "+
				"XDG_DATA_HOME moved and the whitelist did not follow -- and a whitelist entry "+
				"that matches nothing is the dead-rule defect gonk-blxu rejected once already. "+
				"NOTE, so this is not over-read: upstream re-allows Truncate.GLOB by itself "+
				"unless the config DENIES that exact pattern (agent/agent.ts:296-311), which "+
				"was confirmed in the Job harness, so what actually breaks truncated reads is "+
				"writing \"deny\" here, not losing the \"allow\".",
				name, wantTruncate, got, dataHome)
		}

		// The normal path must still be normal: reading INSIDE the checkout is
		// what triage does all day, and "fix the leak by denying read" would
		// pass every assertion above.
		if got := evaluatePermissionRules(rules, "read", "docs/anything.md"); got != "allow" {
			t.Errorf("agents/%s/agent.toml resolves read for a worktree-relative path to %q, "+
				"want \"allow\" -- external_directory is a gate on paths OUTSIDE the worktree; "+
				"closing it must not cost the agent the ability to read the checkout it was "+
				"given.", name, got)
		}
	}
	if seen == 0 {
		t.Fatal("no agent declared OPENCODE_PERMISSION -- this test asserted nothing")
	}
}

// permRule is one flattened rule, the shape opencode's `evaluate` consumes
// (packages/opencode/src/permission/index.ts, PermissionV1.Rule).
type permRule struct {
	permission string
	pattern    string
	action     string
}

// flattenPermissionConfig is `Permission.fromConfig`: a bare action becomes one
// rule with pattern "*", a per-pattern map becomes one rule per entry, and KEY
// ORDER IS PRESERVED THROUGHOUT because evaluation is findLast.
func flattenPermissionConfig(entries []permEntry) []permRule {
	var out []permRule
	for _, e := range entries {
		if !e.nestedValue() {
			out = append(out, permRule{permission: e.key, pattern: "*", action: e.action})
			continue
		}
		for _, r := range e.nested {
			out = append(out, permRule{permission: e.key, pattern: r.key, action: r.action})
		}
	}
	return out
}

// evaluatePermissionRules is `Permission.evaluate`: the LAST rule whose
// permission matches AND whose pattern matches wins, and nothing matching falls
// back to "ask".
func evaluatePermissionRules(rules []permRule, permission, pattern string) string {
	action := "ask"
	found := false
	for _, r := range rules {
		if wildcardMatch(permission, r.permission) && wildcardMatch(pattern, r.pattern) {
			action = r.action
			found = true
		}
	}
	if !found {
		return "ask"
	}
	return action
}

var wildcardMeta = regexp.MustCompile(`[.+^${}()|\[\]\\]`)

// wildcardMatch is packages/core/src/util/wildcard.ts, transcribed. It is
// TRANSCRIBED RATHER THAN APPROXIMATED on purpose: "does this whitelist entry
// match the path opencode will ask about" is the entire question this file's
// external_directory test exists to answer, and a looser matcher here would
// pass a pattern the real one rejects -- which is the dead-rule failure mode
// the fix was written to avoid.
func wildcardMatch(input, pattern string) bool {
	normalized := strings.ReplaceAll(input, `\`, "/")
	escaped := strings.ReplaceAll(pattern, `\`, "/")
	escaped = wildcardMeta.ReplaceAllString(escaped, `\${0}`)
	escaped = strings.ReplaceAll(escaped, "*", ".*")
	escaped = strings.ReplaceAll(escaped, "?", ".")
	if strings.HasSuffix(escaped, " .*") {
		escaped = strings.TrimSuffix(escaped, " .*") + "( .*)?"
	}
	re, err := regexp.Compile(`(?s)\A` + escaped + `\z`)
	if err != nil {
		return false
	}
	return re.MatchString(normalized)
}

// permEntry is one key/value pair of OPENCODE_PERMISSION, IN FILE ORDER.
//
// opencode's config accepts two value shapes per permission key
// (packages/core/src/v1/config/permission.ts `Rule = Union([Action, Object])`)
// and `fromConfig` (packages/opencode/src/permission/index.ts) flattens them
// into the SAME rule list:
//
//	"bash": "deny"                -> {permission:"bash", pattern:"*",   action:"deny"}
//	"external_directory": {...}   -> one rule per pattern, in object order
//
// so both shapes have to be readable here, and both have to keep their order:
// `evaluate` takes the LAST matching rule.
type permEntry struct {
	key    string      // the permission name
	action string      // the action, when the value was a JSON string
	nested []permEntry // the per-pattern rules, when the value was a JSON object
}

// nestedValue reports whether this entry carried a per-pattern object rather
// than a bare action. It is not `len(nested) > 0`: an EMPTY object is a real
// (and useless) config, and calling it a scalar would hide it.
func (e permEntry) nestedValue() bool { return e.nested != nil }

// orderedJSONObject decodes OPENCODE_PERMISSION PRESERVING KEY ORDER, which
// encoding/json's map decoding throws away and which opencode's evaluation
// order depends on (permission/index.ts `fromConfig` iterates
// Object.entries(), and opencode's config parser keeps the user's original key
// order on purpose -- see the comment in
// packages/core/src/v1/config/permission.ts).
//
// It reads exactly two levels, because opencode accepts exactly two: a
// per-pattern object may contain actions only, never another object.
func orderedJSONObject(raw string) ([]permEntry, error) {
	return decodePermObject(json.NewDecoder(strings.NewReader(raw)))
}

func decodePermObject(dec *json.Decoder) ([]permEntry, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("want a JSON object, got %v", tok)
	}
	var out []permEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("non-string object key %v", keyTok)
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch v := valTok.(type) {
		case string:
			out = append(out, permEntry{key: key, action: v})
		case json.Delim:
			if v != '{' {
				return nil, fmt.Errorf("value for %q is %v, want an action string or a per-pattern object", key, v)
			}
			// json.Decoder has already consumed the '{'; hand the rest to a
			// decoder positioned inside the object.
			nested, err := decodePermObjectBody(dec, key)
			if err != nil {
				return nil, err
			}
			if nested == nil {
				nested = []permEntry{}
			}
			out = append(out, permEntry{key: key, nested: nested})
		default:
			return nil, fmt.Errorf("value for %q is %v, want an action string "+
				"(\"allow\"/\"deny\"/\"ask\") or a per-pattern object", key, valTok)
		}
	}
	// consume the closing '}'
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// decodePermObjectBody reads the body of an object whose '{' the caller has
// already consumed.
func decodePermObjectBody(dec *json.Decoder, parent string) ([]permEntry, error) {
	var out []permEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("non-string object key %v inside %q", keyTok, parent)
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		val, ok := valTok.(string)
		if !ok {
			return nil, fmt.Errorf("value for %q inside %q is %v, want an action string; "+
				"opencode's permission config is two levels deep at most", key, parent, valTok)
		}
		out = append(out, permEntry{key: key, action: val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}
