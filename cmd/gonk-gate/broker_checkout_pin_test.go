package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-qdr4: the checkout grant pins a COMMIT, not a branch name.

const (
	pinSHAAtGrant = "1111111111111111111111111111111111111111"
	pinSHAAfter   = "2222222222222222222222222222222222222222"
)

// branchForge is a forge whose default-branch head can be moved by a "push".
type branchForge struct {
	mu   sync.Mutex
	head string
}

func (b *branchForge) push(sha string) { b.mu.Lock(); b.head = sha; b.mu.Unlock() }

func (b *branchForge) resolve(ref string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ref != "main" {
		return "", fmt.Errorf("404 Commit Not Found: %s", ref)
	}
	return b.head, nil
}

// treeFetcher is intake's ArchiveFetcher over a forge that has two commits. Like
// GitLab's archive endpoint it accepts a sha OR a branch name; a name resolves
// to whatever the head is at the moment of the call.
type treeFetcher struct {
	forge *branchForge
	asked []string
	mu    sync.Mutex
}

func (f *treeFetcher) RepoArchive(_ context.Context, _ int64, ref string, _ int64) ([]byte, error) {
	f.mu.Lock()
	f.asked = append(f.asked, ref)
	f.mu.Unlock()
	if ref == "main" {
		ref, _ = f.forge.resolve("main")
	}
	switch ref {
	case pinSHAAtGrant:
		return []byte("tree-at-grant"), nil
	case pinSHAAfter:
		return []byte("tree-after-push"), nil
	}
	return nil, fmt.Errorf("no such commit %q", ref)
}

func pinDispatch(t *testing.T, forge stubForge, rigURL string, logs io.Writer, store beadstore.Store) (runtime.Spec, int) {
	t.Helper()
	rt := newTestRuntime()
	fm := &fakeMeter{resp: runDecision("m")}
	code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Forge: forge, Args: baseDispatchArgs(), Clock: newFakeClock(), Log: testLogger(logs),
		Rig: &rig.Client{BaseURL: rigURL}, RigBaseURL: rigURL,
	})
	spec, _ := rt.Spec(runtime.Handle(testSession))
	return spec, code
}

// The grant on the wire carries the commit SHA the default branch resolved to --
// never the branch name -- and the same SHA reaches the bead record, the INFO log
// and the agent's prompt.
func TestTheCheckoutGrantCarriesTheCommitNotTheBranchName(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	bf := &branchForge{head: pinSHAAtGrant}
	forge := promptTestForge()
	forge.resolve = bf.resolve
	store := beadstore.NewMemory()
	var logs bytes.Buffer
	spec, code := pinDispatch(t, forge, srv.URL, &logs, store)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("grants registered = %v, want 1", bodies)
	}
	var got struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.SHA != pinSHAAtGrant {
		t.Errorf("grant body %s: sha = %q, want the resolved commit %s", bodies[0], got.SHA, pinSHAAtGrant)
	}
	if strings.Contains(bodies[0], `"main"`) {
		t.Errorf("grant body %s names the branch; it must carry only the commit", bodies[0])
	}
	if !rig.ValidSHA(got.SHA) {
		t.Errorf("granted %q is not a full commit SHA", got.SHA)
	}

	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.CheckoutSHA != pinSHAAtGrant {
		t.Errorf("bead record CheckoutSHA = %q, want %s", rec.CheckoutSHA, pinSHAAtGrant)
	}
	if !strings.Contains(string(spec.Prompt), pinSHAAtGrant) {
		t.Errorf("the prompt does not name the commit the agent is looking at:\n%s", spec.Prompt)
	}
	var infoLine string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "checkout granted for this session") {
			infoLine = l
		}
	}
	if !strings.Contains(infoLine, "level=INFO") || !strings.Contains(infoLine, "sha="+pinSHAAtGrant) {
		t.Errorf("no INFO grant line carrying the sha; got %q in\n%s", infoLine, logs.String())
	}
}

// END TO END against the real rig handler: dispatch grants at commit A, a push
// moves main to B, and the pod's fetch of the URL it was handed still returns
// A's tree, asked of the forge BY SHA.
func TestAPushAfterDispatchDoesNotChangeTheTreeThePodFetches(t *testing.T) {
	bf := &branchForge{head: pinSHAAtGrant}
	tf := &treeFetcher{forge: bf}
	mux := http.NewServeMux()
	(&rig.Handler{Store: rig.NewStore(nil), Fetch: tf}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	forge := promptTestForge()
	forge.resolve = bf.resolve
	spec, code := pinDispatch(t, forge, srv.URL, io.Discard, beadstore.NewMemory())
	if code != 0 || spec.RigURL == "" {
		t.Fatalf("exit = %d, RigURL = %q; want a granted checkout", code, spec.RigURL)
	}

	bf.push(pinSHAAfter) // the push between grant and fetch

	resp, err := http.Get(spec.RigURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "tree-at-grant" {
		t.Fatalf("fetch = %d %q after a push, want 200 tree-at-grant", resp.StatusCode, body)
	}
	tf.mu.Lock()
	defer tf.mu.Unlock()
	if len(tf.asked) != 1 || tf.asked[0] != pinSHAAtGrant {
		t.Errorf("forge was asked for %v, want exactly [%s]", tf.asked, pinSHAAtGrant)
	}
}

// A ref that cannot be resolved fails the grant: nothing is registered, the
// session gets no checkout and a prompt that does not claim one, the reason is
// logged with the ref and the forge's words, and the dispatch still proceeds.
func TestAnUnresolvableRefGrantsNothingAndSaysWhy(t *testing.T) {
	var mu sync.Mutex
	grants := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		grants++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	forge := promptTestForge()
	forge.resolve = func(string) (string, error) { return "", fmt.Errorf("404 Commit Not Found") }
	store := beadstore.NewMemory()
	var logs bytes.Buffer
	spec, code := pinDispatch(t, forge, srv.URL, &logs, store)
	if code != 0 {
		t.Fatalf("exit = %d: an unresolvable ref must degrade to no checkout, not fail the dispatch", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if grants != 0 {
		t.Errorf("%d grant(s) registered for an unresolvable ref, want none", grants)
	}
	if spec.RigURL != "" {
		t.Errorf("RigURL = %q, want none", spec.RigURL)
	}
	if strings.Contains(string(spec.Prompt), "checked out in your working directory") {
		t.Errorf("the prompt claims a checkout that was not granted:\n%s", spec.Prompt)
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.CheckoutSHA != "" {
		t.Errorf("CheckoutSHA = %q with no checkout", rec.CheckoutSHA)
	}
	want := []string{"could not grant a checkout for triage", `cannot resolve ref \"main\"`, "404 Commit Not Found", "level=WARN"}
	for _, w := range want {
		if !strings.Contains(logs.String(), w) {
			t.Errorf("log lacks %q:\n%s", w, logs.String())
		}
	}
}
