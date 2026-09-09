package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// fakeMeter is the SERVER side of pkg/meterapi. Plan 03 implements the real one
// against the same types, so this fake and that server cannot drift apart on the
// wire shape -- only on behaviour, which is what Plan 06 is for.
type fakeMeter struct {
	srv     *httptest.Server
	puts    []meterapi.ProjectRequest
	deletes []string
	fail    bool // 500 on everything
	// failFirst models THE BOOT RACE: meter is not listening yet when intake
	// comes up, so the first N registration attempts fail outright. Observed
	// live twice on 2026-07-31 and again on the 2026-08-04 deploy
	// ("connect: connection refused"). It decrements per non-health request, so
	// a caller with a retry ladder gets through and a caller without one does not.
	failFirst int
	// failNow is `fail`, but flippable from a test goroutine WHILE the server is
	// serving -- which is what modelling a mid-life meter outage requires. It is
	// atomic because the handler runs on httptest's goroutine and the plain
	// bools here are not guarded.
	failNow  atomic.Bool
	invalid  bool // 422: the project's yaml will not load
	state    meterapi.State
	authSeen string
}

func newFakeMeter(t *testing.T) *fakeMeter {
	m := &fakeMeter{state: meterapi.StateActive}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.authSeen = r.Header.Get("Authorization")
		if r.URL.Path == meterapi.HealthzPath {
			w.WriteHeader(200)
			return
		}
		if m.fail || m.failNow.Load() {
			http.Error(w, `{"error":"nope"}`, 500)
			return
		}
		if m.failFirst > 0 {
			m.failFirst--
			http.Error(w, `{"error":"not listening yet"}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodDelete {
			// EscapedPath, not Path: net/url decodes %2F back to a literal "/" in
			// .Path, which would hide the very escaping this test verifies.
			m.deletes = append(m.deletes, r.URL.EscapedPath())
			w.WriteHeader(204)
			return
		}
		var req meterapi.ProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad"}`, 400)
			return
		}
		m.puts = append(m.puts, req)

		resp := meterapi.ProjectResponse{
			Project: req.Project, Rig: req.Rig, State: m.state,
			ConfigHash: "sha256:fake",
			Effective: &meterapi.Effective{
				Enabled: true,
				Actions: meterapi.Actions{Triage: true},
				Ladder:  []string{"qwen-local"},
				Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
			},
			KeyRef: meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		}
		code := 200
		if m.invalid {
			code = 422
			resp.State = meterapi.StateInvalid
			resp.Effective = nil
			resp.Error = ".gonk.yml: unknown field 'banana'"
			resp.KeyRef = meterapi.KeyRef{}
			resp.Budget = meterapi.ZeroBudget()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func newReconciler(t *testing.T, gl *glabtest.Server, m *fakeMeter) *Reconciler {
	t.Helper()
	return &Reconciler{
		GL:        gl.Client(),
		Meter:     NewMeterClient(m.srv.URL, "meter-token", nil),
		Cache:     NewCache(),
		BotUserID: 7,
		HookURL:   "https://gonk.orac.local/hook/gitlab",
		HookToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TokenGen:  "1",
		Obs:       NopObserver{},
	}
}

// The registration must carry the RAW yaml, byte for byte. This is Conflict A on
// the wire: if intake ever starts sending a resolved policy instead, meter has
// lost its single-resolver property and two components can disagree about a
// budget ceiling.
func TestReconcileRegistersRAWConfigWithMeter(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	raw := "version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte(raw))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)

	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if sum.Projects != 1 || sum.Errors != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(m.puts) != 1 {
		t.Fatalf("meter puts = %d, want 1", len(m.puts))
	}
	got := m.puts[0]
	if got.GonkYML != raw {
		t.Fatalf("meter got %q, want the RAW bytes %q -- intake must not transform the config", got.GonkYML, raw)
	}
	if got.Project != "group/repo" || got.ProjectID != p.ID || got.Rig != "group-repo" || got.DefaultBranch != "main" {
		t.Fatalf("GitLab metadata missing from the registration: %+v", got)
	}
	if m.authSeen != "Bearer meter-token" {
		t.Fatalf("auth = %q; the meter API is not unauthenticated", m.authSeen)
	}
	e, ok := r.Cache.Get(p.ID)
	if !ok || e.State() != StateValid || !e.Dispatchable() {
		t.Fatalf("cache entry = %+v ok=%v", e, ok)
	}
	// The Effective came off the wire, not out of a local resolver.
	if e.Classification.Meter == nil || !e.Classification.MayTriage() {
		t.Fatalf("effective policy must come from meter's response: %+v", e.Classification)
	}

	hooks, _ := gl.Client().ListHooks(context.Background(), p.ID)
	if len(hooks) != 1 || !hooks[0].IssuesEvents || !hooks[0].NoteEvents || !hooks[0].MergeRequestsEvents || hooks[0].PushEvents {
		t.Fatalf("hook = %+v", hooks)
	}
}

// *** THE REGRESSION GUARD FOR CONFLICT A. ***
// Intake must not import a resolver, and must not ship one.
func TestIntakeDoesNotResolve(t *testing.T) {
	// A compile-time-ish assertion, enforced by the DoD grep as well:
	//   grep -rn "gonkcfg.Resolve" pkg/intake/  -> MUST BE EMPTY
	// This test documents the rule where an implementer will actually read it.
	// If you are here because you "just need the ladder", the ladder is in
	// Classification.Meter.Effective.Ladder. Use it.
	t.Log("intake resolves nothing; see the DoD grep")
}

// Idempotency is the whole game: reconcile runs every 10 minutes forever.
func TestReconcileIsIdempotent(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(gl.Requests())
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, req := range gl.Requests()[before:] {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "PUT") {
			t.Errorf("second pass wrote to GitLab: %s", req)
		}
	}
	if len(m.puts) != 1 {
		t.Errorf("meter registered %d times for an unchanged config hash, want 1", len(m.puts))
	}
}

// MeterResyncInterval must force a re-PUT even when the config hash has not
// changed: meter's answer can change WITHOUT the project's .gonk.yml moving
// (an operator flipping the instance kill switch, or tightening a group
// ceiling), and the config-hash short-circuit above must not pin intake's
// copy of the policy forever.
func TestReconcileResyncsPeriodicallyEvenWhenConfigUnchanged(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	r.MeterResyncInterval = time.Hour

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.puts) != 1 {
		t.Fatalf("meter puts after pass 1 = %d, want 1", len(m.puts))
	}

	// A second immediate pass must NOT re-register: we are well inside the
	// resync interval and the config hash has not changed.
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.puts) != 1 {
		t.Fatalf("meter puts after pass 2 (immediate) = %d, want 1 (still within MeterResyncInterval)", len(m.puts))
	}

	// Backdate the cached entry's LastMeterSync past the resync interval,
	// simulating that an hour has elapsed since the last registration, and
	// reconcile again: even though the config hash is unchanged, intake must
	// re-PUT to pick up any answer meter would now give differently (e.g. an
	// operator's kill switch).
	e, ok := r.Cache.Get(p.ID)
	if !ok {
		t.Fatal("missing cache entry")
	}
	e.LastMeterSync = time.Now().Add(-2 * time.Hour)
	r.Cache.Put(p.ID, e)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.puts) != 2 {
		t.Fatalf("meter puts after pass 3 (overdue) = %d, want 2 (periodic resync must fire)", len(m.puts))
	}
}

// A hook provisioned with an old secret generation must be repaired, or a secret
// rotation silently deafens gonk (GitLab never returns hook tokens, so the
// generation marker in the URL is how staleness is detected -- ADR-003).
func TestReconcileRepairsStaleHook(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	_, _ = gl.Client().CreateHook(context.Background(), p.ID, glab.HookOptions{
		URL: "https://gonk.orac.local/hook/gitlab?gen=0", Token: "old", IssuesEvents: true,
	})
	r := newReconciler(t, gl, newFakeMeter(t))
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooks, _ := gl.Client().ListHooks(context.Background(), p.ID)
	if len(hooks) != 1 {
		t.Fatalf("hook was duplicated instead of repaired: %+v", hooks)
	}
	if !strings.Contains(hooks[0].URL, "gen=1") || !hooks[0].NoteEvents {
		t.Fatalf("hook not repaired: %+v", hooks[0])
	}
	if got := gl.HookToken(p.ID, hooks[0].ID); got == "old" {
		t.Fatal("hook token was not re-set on rotation")
	}
}

// A 422 is the PROJECT's fault, not ours. Meter has already deleted the key.
// Intake records `invalid`, dispatches nothing, and keeps the error text so the
// onboarding/comment flow can show it to a human.
func TestReconcileInvalidConfigIsRecordedNotFatal(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nbanana: true\n"))
	m := newFakeMeter(t)
	m.invalid = true
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("a 422 must not fail the pass: %v", err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateInvalid {
		t.Fatalf("state = %q, want invalid", e.State())
	}
	if e.Dispatchable() {
		t.Fatal("an invalid project must not be dispatchable")
	}
	if !strings.Contains(e.Classification.Reason, "banana") {
		t.Fatalf("meter's error text must survive for the human: %q", e.Classification.Reason)
	}
}

// No unmetered work: if meter is unreachable, we do not know the project's
// policy, so nothing runs. We must NOT fall back to a local resolve.
func TestReconcileMeterFailureBlocksDispatch(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.fail = true
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("a meter failure must not abort the whole pass: %v", err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateUnsynced {
		t.Fatalf("state = %q, want unsynced", e.State())
	}
	if e.Dispatchable() {
		t.Fatal("a project whose policy we could not resolve must not be dispatchable")
	}
}

// key-missing: meter resolved the config but LiteLLM was down, so there is no
// virtual key. Meter would defer anyway; do not churn a bead for it.
func TestReconcileKeyMissingIsNotDispatchable(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.state = meterapi.StateKeyMissing
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateKeyMissing || e.Dispatchable() {
		t.Fatalf("entry = %+v", e)
	}
}

// De-onboarding: the bot is removed from the project (spec 5.1). Intake DELETEs
// the registration; meter disables the project and deletes the key.
func TestReconcileDeletesProjectsTheBotLeft(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	gl.RemoveProject(p.ID) // no longer a membership
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("de-onboarded project must leave the cache")
	}
	if len(m.deletes) != 1 || !strings.Contains(m.deletes[0], "group%2Frepo") {
		t.Fatalf("de-onboarding must DELETE the meter registration (URL-escaped): %v", m.deletes)
	}
}

// Archiving a project is not a de-onboard the vanished-membership sweep can
// see: the bot is still a member, so ListMemberProjects still returns it and
// `seen[p.ID]` is set before reconcileProject ever runs. If reconcileProject
// only dropped the cache entry without deregistering, the sweep would never
// notice and the LiteLLM key would stay live for a project nobody can push
// to anymore. Regression guard for R-15.
func TestReconcileDeregistersArchivedProject(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Cache.Get(p.ID); !ok {
		t.Fatal("setup: project must be cached before archiving it")
	}

	gl.SetArchived(p.ID, true) // still a membership -- just archived
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(m.deletes) != 1 || !strings.Contains(m.deletes[0], "group%2Frepo") {
		t.Fatalf("archiving must DELETE the meter registration exactly once (URL-escaped): %v", m.deletes)
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("archived project must leave the cache")
	}
}

// One bad project must not stop the others.
func TestReconcileContinuesPastOneProjectError(t *testing.T) {
	gl := glabtest.New(t)
	p1 := gl.AddProject("group/a", glab.AccessMaintainer)
	p2 := gl.AddProject("group/b", glab.AccessMaintainer)
	p2.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/repository/files", p1.ID), 500)
	r := newReconciler(t, gl, newFakeMeter(t))
	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if sum.Errors != 1 || sum.Projects != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if _, ok := r.Cache.Get(p2.ID); !ok {
		t.Fatal("the healthy project must still have been reconciled")
	}
}
