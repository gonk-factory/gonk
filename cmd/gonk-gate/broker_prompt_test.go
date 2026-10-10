package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// Under the SessionRuntime (gonk-t55) the prompt is not stored for the pod to
// fetch, and not typed into a TUI: it is MOUNTED into the session, next to the
// project's LiteLLM key and the attribution metadata (plan D3). These tests pin
// what dispatch hands the runtime, because the Spec is the whole of what the
// session receives.

func runDecision(model string) meterapi.DecideResponse {
	return meterapi.DecideResponse{
		Decision: meterapi.DecisionRun,
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Rung:     "cheap", Model: model, Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
	}
}

// THE SECRETS ARE MOUNTED, NEVER ENV. The prompt, the project's own key
// (gonk-8gb) and the attribution metadata (gonk-m6t) ride the Spec's mounted
// fields; not one of them -- nor the key's value under any name -- may appear
// in the environment, which is visible in a pod spec, a `describe` and a
// process listing.
func TestDispatchMountsThePromptKeyAndMetadataAndPutsNoneOfThemInTheEnv(t *testing.T) {
	rt := newTestRuntime()
	fm := &fakeMeter{resp: runDecision("qwen3-6-35b")}
	if code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
		Forge: promptTestForge(), Args: baseDispatchArgs(),
		Clock: newFakeClock(),
	}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	spec, ok := rt.Spec(runtime.Handle(testSession))
	if !ok {
		t.Fatalf("no session %s; starts = %+v", testSession, rt.Starts())
	}
	if string(spec.LiteLLMKey) != "sk-project-abc" {
		t.Fatalf("mounted key = %q, want THIS project's key (gonk-8gb)", spec.LiteLLMKey)
	}
	if !strings.Contains(string(spec.Prompt), "Export times out") || !strings.Contains(string(spec.Prompt), "GONK_BATCH_START") {
		t.Fatalf("mounted prompt is not the rendered triage prompt:\n%s", spec.Prompt)
	}
	if !strings.Contains(string(spec.Metadata), `"gonk_project":"group/repo"`) {
		t.Fatalf("mounted metadata = %s, want meter's attribution payload (gonk-m6t)", spec.Metadata)
	}
	if spec.Image != "" || spec.Agent != "triage" {
		t.Fatalf("spec image/agent = %q/%q, want \"\" (the runtime's configured image)/triage", spec.Image, spec.Agent)
	}
	if spec.Env[envModel] != "qwen3-6-35b" {
		t.Fatalf("GONK_MODEL = %q, want the rung's model from meter's live answer", spec.Env[envModel])
	}
	for k, v := range spec.Env {
		if strings.Contains(v, "sk-project-abc") {
			t.Fatalf("env %s carries the project key -- a key in env is a key in every pod spec", k)
		}
		if strings.Contains(v, "Export times out") || strings.Contains(v, "GONK_BATCH_START") {
			t.Fatalf("env %s carries prompt text; the prompt is mounted, never env", k)
		}
	}
}

// THE NAME IS DETERMINISTIC (plan D2): the same bead and attempt dispatched by
// two unrelated processes reach the SAME name, which is what makes the second
// Start land on the first session instead of a new pod. A new attempt gets a
// new name, so a re-sling never collides with the session it replaces.
func TestSessionNameIsTheSameForTheSameAttemptAndNewForTheNext(t *testing.T) {
	names := map[int][]string{}
	for _, attempt := range []int{1, 1, 2} {
		rt := newTestRuntime()
		dec := runDecision("m")
		dec.Attempt = attempt
		fm := &fakeMeter{resp: dec}
		if code := runDispatch(context.Background(), dispatchDeps{
			Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
			Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
			Forge: promptTestForge(), Args: baseDispatchArgs(), Clock: newFakeClock(),
		}); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		starts := rt.Starts()
		if len(starts) != 1 {
			t.Fatalf("starts = %d, want 1", len(starts))
		}
		names[attempt] = append(names[attempt], starts[0].Name)
	}
	if names[1][0] != names[1][1] || names[1][0] != "gonk-triage-p42-i3-a1" {
		t.Fatalf("attempt 1 names = %v, want both gonk-triage-p42-i3-a1", names[1])
	}
	if names[2][0] != "gonk-triage-p42-i3-a2" {
		t.Fatalf("attempt 2 name = %q, want gonk-triage-p42-i3-a2", names[2][0])
	}
}

// A real issue, because the triage prompt renders one; an empty stubForge
// returns a nil issue and the renderer dereferences it.
func promptTestForge() stubForge {
	return stubForge{iss: &glab.Issue{
		IID: 3, Title: "Export times out", State: "opened",
		Labels: []string{"needs-triage"}, Description: "It hangs then 504s.",
	}}
}

const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// keyLooksNonced reports whether key ends in a 26-character base32 segment:
// rigGrantKey's 128 bits of crypto/rand.
func keyLooksNonced(key string) bool {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return false
	}
	n := key[i+1:]
	if len(n) != 26 {
		return false
	}
	for _, c := range n {
		if !strings.ContainsRune(base32Alphabet, c) {
			return false
		}
	}
	return true
}

// THE CHECKOUT GRANT IS NOT KEYED BY THE SESSION NAME. pkg/rig hands a
// granted tree to anyone who presents its key on gonk-intake's private
// listener, which every agent pod can reach. The Gas City alias carried 128
// bits of nonce and was the key; the session name is deterministic
// (gonk-triage-p42-i3-a1 is derivable from a project and an issue), so a grant
// keyed by it would let any agent pod fetch any project's checkout. The key is
// the name plus a fresh nonce, new on every grant, and the URL the session is
// handed carries it.
func TestTheCheckoutGrantKeyIsNotTheGuessableSessionName(t *testing.T) {
	var mu sync.Mutex
	var registered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		registered = append(registered, strings.TrimPrefix(r.URL.Path, "/rig/"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	var urls []string
	for i := 0; i < 2; i++ {
		rt := newTestRuntime()
		fm := &fakeMeter{resp: runDecision("m")}
		if code := runDispatch(context.Background(), dispatchDeps{
			Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
			Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
			Forge: promptTestForge(), Args: baseDispatchArgs(), Clock: newFakeClock(),
			Rig: &rig.Client{BaseURL: srv.URL}, RigBaseURL: "http://intake.private:8081",
		}); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		spec, _ := rt.Spec(runtime.Handle(testSession))
		if spec.RigURL == "" {
			t.Fatalf("Spec.RigURL is empty with a checkout granted")
		}
		// PLAN 3.3 HANDSHAKE ITEM 4: the URL embeds the grant's key, so it is
		// MOUNTED (Spec.RigURL -> the session Secret's rig-url), and no env
		// value may carry it -- anyone who can `get` the Job reads its env.
		for k, v := range spec.Env {
			if strings.Contains(v, spec.RigURL) || k == "GONK_RIG_URL" {
				t.Fatalf("Spec.Env %s=%q carries the checkout URL; it is mounted, never env", k, v)
			}
		}
		urls = append(urls, spec.RigURL)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(registered) != 2 {
		t.Fatalf("grants registered = %v, want 2", registered)
	}
	for i, key := range registered {
		if key == testSession || !strings.HasPrefix(key, testSession+".") || !keyLooksNonced(key) {
			t.Fatalf("grant key %q, want %s.<26-char base32 nonce> -- never the bare, guessable name", key, testSession)
		}
		if want := rig.FetchURL("http://intake.private:8081", key); urls[i] != want {
			t.Fatalf("Spec.RigURL = %q, want %q (the session must be handed the nonced key)", urls[i], want)
		}
	}
	if registered[0] == registered[1] {
		t.Fatalf("the same grant key %q was minted twice", registered[0])
	}
}
