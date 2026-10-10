package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/BurntSushi/toml"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// THE SESSION'S ENVIRONMENT IS BUILT FROM THE PACK, NOT FROM GAS CITY (gonk-t55
// plan D5, gonk-426v).
//
// A Gas City agent pod carried whatever Gas City put in it: the agent.toml
// [env] block, plus the city's own plumbing -- GC_DOLT_*, BEADS_*,
// GC_INSTANCE_TOKEN, GC_CITY_WRITE_PUBKEY. None of that plumbing is the
// agent's business, and some of it is a credential to the city's own store
// (gonk-426v). A Jobs session gets EXACTLY two things:
//
//  1. the [env] block of <packDir>/agents/<agent>/agent.toml -- the file
//     internal/packtest's permission assertions already hold to account
//     (OPENCODE_PERMISSION, gonk-blxu), so the permissions a test proves are
//     the permissions a pod runs with; and
//  2. the named GONK_* variables below; and
//  3. the five the RUNTIME sets on every session itself -- GONK_SESSION_RUNTIME,
//     GONK_SESSION_DIR, GONK_SESSION_NAME, GONK_AGENT and the session
//     contract version GONK_SESSION_CONTRACT (pkg/runtime/jobs; plan 3.3).
//     Those are the runtime's to set, and it refuses a Spec whose Env
//     contradicts them, so neither dispatch nor the pack may set them.
//
// Nothing else. Secrets are not env at all: the prompt, the project's LiteLLM
// key, the attribution metadata and the checkout URL are MOUNTED
// (runtime.Spec.Prompt / LiteLLMKey / Metadata / RigURL), so they appear in no
// pod spec, no `describe` and no process listing. The checkout URL is on that
// list because it embeds the rig grant's key, which pkg/rig serves the tree
// for until the grant's TTL (plan 3.3, handshake item 4).

// The names dispatch sets, all read by the entrypoint in jobs mode
// (images/agent/entrypoint.sh, plan D10). Named once so a test and the builder
// cannot disagree.
const (
	envLiteLLMURL = "GONK_LITELLM_URL"
	envRigBaseURL = "GONK_RIG_BASE_URL"
	envModel      = "GONK_MODEL"
	envAttempt    = "GONK_ATTEMPT"
)

// runtimeOwnedEnv are the names the SessionRuntime sets on every session
// itself (pkg/runtime/jobs) and refuses to see in a Spec's Env: the jobs-mode
// switch, where the Secret is mounted, the session's identity, and the
// session contract version (plan 3.3).
var runtimeOwnedEnv = []string{"GONK_SESSION_RUNTIME", "GONK_SESSION_DIR", "GONK_SESSION_NAME", "GONK_AGENT", runtime.EnvSessionContract}

// forbiddenSessionEnv matches the Gas City plumbing a session must never
// carry, whatever the pack says (D5; gonk-426v). It is a REFUSAL, not a
// filter: a pack that puts one of these in an agent's [env] is misconfigured,
// and silently dropping it would hide that.
var forbiddenSessionEnv = regexp.MustCompile(`^(GC_|BEADS_|DOLT)`)

// agentPackFile is the parsed shape of an agent.toml, as far as the session
// env cares: its [env] table. Every other key belongs to Gas City's loader.
type agentPackFile struct {
	Env map[string]string `toml:"env"`
}

// packAgentEnv reads the [env] table of <packDir>/agents/<agent>/agent.toml.
//
// A missing or unparseable file is an ERROR, never an empty env: the [env]
// block is where OPENCODE_PERMISSION lives, and an agent started without it
// runs with opencode's default permissions -- every tool allowed (gonk-blxu).
// Failing the dispatch is the only safe answer.
func packAgentEnv(packDir, agent string) (map[string]string, error) {
	if packDir == "" {
		return nil, fmt.Errorf("no pack directory configured")
	}
	path := filepath.Join(packDir, "agents", agent, "agent.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent pack file: %w", err)
	}
	var f agentPackFile
	if _, err := toml.Decode(string(raw), &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(f.Env) == 0 {
		return nil, fmt.Errorf("%s has no [env] table; refusing to start an agent without its permission config", path)
	}
	for k := range f.Env {
		if forbiddenSessionEnv.MatchString(k) {
			return nil, fmt.Errorf("%s [env] sets %s, which a session must never carry (gonk-426v)", path, k)
		}
		if slices.Contains(runtimeOwnedEnv, k) {
			return nil, fmt.Errorf("%s [env] sets %s, which only the session runtime sets", path, k)
		}
	}
	return f.Env, nil
}

// sessionEnvInputs are the per-dispatch values of the named GONK_* variables.
// The session's name and agent are NOT here: the runtime sets those from the
// Spec itself (runtimeOwnedEnv).
type sessionEnvInputs struct {
	Attempt    int
	Model      string
	LiteLLMURL string
	RigBaseURL string
	// The full checkout URL is deliberately NOT here: it goes in
	// runtime.Spec.RigURL, which the runtime mounts (plan 3.3, item 4).
}

// buildSessionEnv is the whole of a session's environment: the pack's [env]
// with the named GONK_* variables laid over it. An empty optional value is
// left out rather than set to "", so the entrypoint's ${VAR:-} defaults see
// it as unset.
func buildSessionEnv(pack map[string]string, in sessionEnvInputs) map[string]string {
	env := make(map[string]string, len(pack)+4)
	for k, v := range pack {
		env[k] = v
	}
	env[envAttempt] = strconv.Itoa(in.Attempt)
	set := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	set(envModel, in.Model)
	set(envLiteLLMURL, in.LiteLLMURL)
	set(envRigBaseURL, in.RigBaseURL)
	return env
}
