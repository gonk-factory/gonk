package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// namedSessionEnv is every name buildSessionEnv may add to the pack's [env]
// (plan D5). A new name is a decision, made here, where a reviewer sees it.
// The runtime adds runtimeOwnedEnv itself.
// The checkout URL is NOT one of them: it is mounted (runtime.Spec.RigURL).
var namedSessionEnv = []string{envLiteLLMURL, envRigBaseURL, envModel, envAttempt}

// PLAN D5, AS DISPATCH HANDS IT TO THE RUNTIME: the Spec's env is the pack's
// agent.toml [env] -- byte for byte, so the OPENCODE_PERMISSION
// internal/packtest proves is the one the pod runs with (gonk-blxu) -- plus the
// named GONK_* variables, and NOTHING ELSE (the runtime adds its own five:
// runtimeOwnedEnv). In particular none of Gas City's
// plumbing: a live Gas City agent pod carried GC_DOLT_*, BEADS_*,
// GC_INSTANCE_TOKEN and GC_CITY_WRITE_PUBKEY (gonk-426v).
func TestDispatchedSessionEnvIsThePacksEnvPlusTheNamedVarsAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		trigger string
		agent   string
		iid     int64
	}{{"issue-triage", "triage", 3}, {"scaffold", "scaffold", 0}} {
		t.Run(tc.agent, func(t *testing.T) {
			rt := newTestRuntime()
			fm := &fakeMeter{resp: runDecision("m")}
			args := baseDispatchArgs()
			args.Trigger, args.IssueIID = tc.trigger, tc.iid
			if code := runDispatch(context.Background(), dispatchDeps{
				Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
				Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
				Forge: promptTestForge(), Args: args, Clock: newFakeClock(),
				LiteLLMURL: "http://litellm.local:4000", RigBaseURL: "http://intake.private:8081",
			}); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			starts := rt.Starts()
			if len(starts) != 1 {
				t.Fatalf("starts = %d, want 1", len(starts))
			}
			env := starts[0].Env

			pack, err := packAgentEnv(repoPackDir, tc.agent)
			if err != nil {
				t.Fatal(err)
			}
			if pack["OPENCODE_PERMISSION"] == "" {
				t.Fatalf("the %s pack env has no OPENCODE_PERMISSION; this test would prove nothing", tc.agent)
			}
			for k, v := range pack {
				if env[k] != v {
					t.Errorf("env %s = %q, want the pack's %q verbatim", k, env[k], v)
				}
			}
			allowed := map[string]bool{}
			for k := range pack {
				allowed[k] = true
			}
			for _, k := range namedSessionEnv {
				allowed[k] = true
			}
			var extra []string
			for k := range env {
				if !allowed[k] {
					extra = append(extra, k)
				}
				if forbiddenSessionEnv.MatchString(k) {
					t.Errorf("env carries %s: Gas City plumbing a session must never see (gonk-426v)", k)
				}
				if slices.Contains(runtimeOwnedEnv, k) {
					t.Errorf("env sets %s, which the runtime owns and refuses in a Spec", k)
				}
			}
			sort.Strings(extra)
			if len(extra) != 0 {
				t.Errorf("env carries names that are neither the pack's nor named in plan D5: %v", extra)
			}
			want := map[string]string{
				envAttempt: "1", envModel: "m",
				envLiteLLMURL: "http://litellm.local:4000", envRigBaseURL: "http://intake.private:8081",
			}
			for k, v := range want {
				if env[k] != v {
					t.Errorf("env %s = %q, want %q", k, env[k], v)
				}
			}
			if starts[0].RigURL != "" {
				t.Errorf("Spec.RigURL = %q with no checkout granted (no rig client)", starts[0].RigURL)
			}
		})
	}
}

// writePack writes one agent.toml under a fresh pack root.
func writePack(t *testing.T, agent, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "agents", agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// A pack that cannot give the session its permissions refuses the dispatch as
// MISCONFIGURATION (exit 2) before anything is reserved or started: an agent
// without its [env] runs with opencode's default, allow-everything
// permissions (gonk-blxu), and one with Gas City's plumbing in it is gonk-426v.
func TestDispatchRefusesAPackThatCannotGiveTheSessionItsEnv(t *testing.T) {
	for name, body := range map[string]string{
		"missing file":          "",
		"no env table":          "description = \"x\"\n",
		"forbidden GC_ name":    "[env]\nOPENCODE_PERMISSION = \"{}\"\nGC_DOLT_PASSWORD = \"x\"\n",
		"forbidden BEADS_ name": "[env]\nOPENCODE_PERMISSION = \"{}\"\nBEADS_DIR = \"/x\"\n",
		"forbidden DOLT name":   "[env]\nDOLT_ROOT = \"/x\"\n",
		"runtime-owned name":    "[env]\nOPENCODE_PERMISSION = \"{}\"\nGONK_SESSION_DIR = \"/elsewhere\"\n",
		"unparseable":           "[env\n",
	} {
		t.Run(name, func(t *testing.T) {
			pack := t.TempDir()
			if body != "" {
				pack = writePack(t, "triage", body)
			}
			rt := newTestRuntime()
			store := beadstore.NewMemory()
			var logged strings.Builder
			code := runDispatch(context.Background(), dispatchDeps{
				Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
				Meter: meterClient((&fakeMeter{resp: runDecision("m")}).server(t)), Runtime: rt, Store: store,
				Forge: promptTestForge(), Args: baseDispatchArgs(), Clock: newFakeClock(),
				PackDir: pack, Log: testLogger(&logged),
			})
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (misconfiguration); log:\n%s", code, logged.String())
			}
			if n := len(rt.Starts()); n != 0 {
				t.Fatalf("Start called %d times with an unusable pack", n)
			}
			if _, ok, _ := store.Get(context.Background(), baseDispatchArgs().BeadAnchor); ok {
				t.Fatal("a record was written for a dispatch that could never start its session")
			}
		})
	}
}

// The optional GONK_* values are LEFT OUT when empty rather than set to "",
// so the entrypoint's ${VAR:-} defaults read them as unset; the pack's own
// names are not overridden unless the named set claims them.
func TestBuildSessionEnvOmitsEmptyOptionalValues(t *testing.T) {
	env := buildSessionEnv(map[string]string{"HOME": "/tmp/gonk-home"}, sessionEnvInputs{Attempt: 3})
	for _, k := range []string{envModel, envLiteLLMURL, envRigBaseURL} {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q set with no value; want it absent", k, v)
		}
	}
	if len(env) != 2 || env["HOME"] != "/tmp/gonk-home" || env[envAttempt] != "3" {
		t.Fatalf("env = %v, want exactly HOME and GONK_ATTEMPT", env)
	}
}
