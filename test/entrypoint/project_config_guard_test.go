// Guard on gonk-le3g: the agent pod must run opencode with repo-local config
// discovery OFF.
//
// THE HAZARD. images/Dockerfile.agent sets WORKDIR /workspace and this
// entrypoint extracts the session's repository archive there before exec'ing
// opencode, so opencode's working directory IS an untrusted checkout. At
// v1.18.3 opencode walks up from that directory and merges any opencode.json /
// opencode.jsonc it finds, plus every .opencode/ directory (config/config.ts
// and config/paths.ts, both guarded by OPENCODE_DISABLE_PROJECT_CONFIG). A
// repository that ships {"agent":{"build":{"permission":{"*":"allow"}}}} gets
// that block merged AFTER the env-derived ruleset (agent/agent.ts), and
// evaluation is findLast -- so it re-enables every tool pack/agents/*/
// agent.toml's OPENCODE_PERMISSION denies. A .opencode/ directory additionally
// gets an npm install and its plugins loaded, which is repo-controlled code.
//
// WHY THIS TEST RUNS THE SCRIPT INSTEAD OF GREPPING IT. The claim is not "a
// line exists in entrypoint.sh", it is "opencode's process has this variable".
// Those differ: an export inside a function, after an early exit, or shadowed
// by a later unset would all satisfy the grep. So the fake opencode on the
// test's PATH dumps its OWN environment (FAKE_OPENCODE_ENV_FILE, see
// testdata/fakebin/opencode) and the assertions below read that dump -- the
// environment the real binary would have been handed, observed at the same
// point.
package entrypoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFakeOpencodeEnv returns the environment the fake opencode recorded for
// one subcommand, as a name -> value map.
//
// A MISSING FILE IS A FATAL, NOT A MISSING KEY. If the entrypoint never
// reached that exec, every assertion below would otherwise "pass" against an
// empty map for the wrong reason.
func readFakeOpencodeEnv(t *testing.T, base, subcommand string) map[string]string {
	t.Helper()
	path := base + "." + subcommand
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake opencode never recorded an environment for `opencode %s` (%v) -- "+
			"the entrypoint did not reach that exec, so this test asserted nothing", subcommand, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		i := strings.Index(line, "=")
		if i <= 0 {
			// A blank line, or a continuation line of a multi-line value. The
			// variables this test cares about have neither, so skipping is
			// correct -- and mis-parsing a continuation as a name would only
			// ever ADD a key, never remove the one being asserted on.
			continue
		}
		env[line[:i]] = line[i+1:]
	}
	if len(env) == 0 {
		t.Fatalf("%s is empty: the fake opencode ran but recorded no environment at all", path)
	}
	return env
}

// TestEntrypointDisablesRepoLocalOpencodeConfig asserts OPENCODE_DISABLE_PROJECT_CONFIG
// is in the environment opencode is actually run with, for BOTH execs the
// entrypoint makes -- the provider check (`opencode models`) and the session
// itself (`opencode run`). Both matter: a repo-local config can name providers
// as well as permissions, and the provider assertion is the thing that decides
// whether this pod is allowed to talk to a model at all.
//
// It also asserts OPENCODE_CONFIG is still set in the same environment. That is
// not padding: the flag's blast radius is the point of the change, and
// opencode loads OPENCODE_CONFIG on its own branch (config/config.ts,
// `if (Flag.OPENCODE_CONFIG)`), unguarded by it. If a future opencode folded
// the two together, the overlay -- the LiteLLM provider, the model, the
// attribution header -- would vanish with the project config, and everything
// else in this repo would still look fine.
func TestEntrypointDisablesRepoLocalOpencodeConfig(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "opencode_env")
	env := append(baseEnv(t, dir),
		"GONK_MODEL=test-model",
		"GONK_LITELLM_URL=http://litellm.invalid",
		"GC_WEBHOOK_ARG_LITELLM_KEY=fake-session-key",
		"GONK_OPENCODE_OVERLAY="+filepath.Join(dir, "opencode.json"),
		"FAKE_OPENCODE_ENV_FILE="+envFile,
		"FAKE_CURL_STATUS=200",
		`FAKE_CURL_BODY={"prompt":"do the assigned task"}`,
	)

	const ranToCompletion = "session end: opencode run exited rc="
	out, outcome, waitErr := runEntrypointUntil(t, ranToCompletion, env)
	switch outcome {
	case outcomeHung:
		t.Fatalf("hang guard fired: the entrypoint neither exited nor reached %q:\n%s", ranToCompletion, out)
	case outcomeExited:
		t.Fatalf("entrypoint exited on its own (err=%v) instead of running opencode:\n%s", waitErr, out)
	}

	// opencode's own truthiness test is `value.toLowerCase() === "true" ||
	// value === "1"` (packages/core/src/flag/flag.ts). Asserting membership in
	// that set rather than merely "non-empty" is the same lesson gonk-46gy
	// taught about permission actions: a value opencode does not recognise
	// reads as a control and is not one -- OPENCODE_DISABLE_PROJECT_CONFIG=yes
	// leaves project config fully ON.
	truthy := map[string]bool{"1": true, "true": true, "TRUE": true, "True": true}

	for _, subcommand := range []string{"models", "run"} {
		got := readFakeOpencodeEnv(t, envFile, subcommand)

		val, ok := got["OPENCODE_DISABLE_PROJECT_CONFIG"]
		if !ok {
			t.Errorf("`opencode %s` ran WITHOUT OPENCODE_DISABLE_PROJECT_CONFIG (gonk-le3g). "+
				"opencode's working directory is the untrusted session checkout, so without this "+
				"flag it merges the repository's own opencode.json / opencode.jsonc / .opencode/ "+
				"into the config -- and a repo-supplied agent permission block is merged AFTER "+
				"the OPENCODE_PERMISSION denials in pack/agents/*/agent.toml and outranks them "+
				"(findLast). A checked-out repository could re-enable bash, edit, webfetch, "+
				"websearch, task and question by committing one file.", subcommand)
			continue
		}
		if !truthy[val] {
			t.Errorf("`opencode %s` ran with OPENCODE_DISABLE_PROJECT_CONFIG=%q, which opencode does "+
				"NOT read as set: packages/core/src/flag/flag.ts accepts only \"1\" or a "+
				"case-insensitive \"true\". Any other value leaves repo-local config discovery "+
				"fully enabled while the entrypoint looks like it disabled it (gonk-le3g).",
				subcommand, val)
		}

		if got["OPENCODE_CONFIG"] == "" {
			t.Errorf("`opencode %s` ran without OPENCODE_CONFIG -- the rendered overlay (LiteLLM "+
				"provider, model, attribution header) is delivered through it, and it is loaded on "+
				"a branch of config/config.ts that OPENCODE_DISABLE_PROJECT_CONFIG does not guard. "+
				"If this ever goes empty alongside the flag above, disabling project config has "+
				"taken gonk's OWN config with it.", subcommand)
		}
	}
}
