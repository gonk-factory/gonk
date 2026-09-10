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
	// gets records every GET /v1/projects/{project} this fake served (the
	// same EscapedPath convention as deletes). Tests use it to assert that a
	// tombstoned, already-deregistered archived project causes NO further
	// meter traffic -- not even a read-only GET.
	gets []string
	// notRegistered makes GET answer 404 ("project not registered"), the way
	// the real meter would for a project it genuinely has no record of --
	// e.g. one a DIFFERENT, now-gone intake replica already deregistered.
	// Defaults false (GET reports the project found), matching this fake's
	// existing default-success posture for PUT and DELETE.
	notRegistered bool
	// bare404 makes GET answer a 404 with NO meter error body at all --
	// standing in for a 404 that meter's own handler never wrote: a BaseURL
	// with a wrong path prefix, an ingress or proxy in front of meter, a
	// route that stopped matching. Deliberately distinct from
	// notRegistered, which is meter's OWN 404 and carries its
	// {"error":"project not registered"} body.
	bare404 bool
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
	failNow atomic.Bool
	// failDeleteNow fails ONLY the DELETE, leaving GET healthy. `failNow`
	// cannot express that: it 500s everything, and the archived branch's GET
	// runs first, so a test using it never reaches the DELETE at all. This
	// knob is what exercises "meter says the project IS registered, and the
	// deregister then fails" -- the case that decides whether a tombstone
	// gets set for something this process did not actually delete. Atomic
	// for the same reason failNow is.
	failDeleteNow atomic.Bool
	invalid       bool // 422: the project's yaml will not load
	state         meterapi.State
	authSeen      string
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
			if m.failDeleteNow.Load() {
				http.Error(w, `{"error":"nope"}`, 500)
				return
			}
			// EscapedPath, not Path: net/url decodes %2F back to a literal "/" in
			// .Path, which would hide the very escaping this test verifies.
			m.deletes = append(m.deletes, r.URL.EscapedPath())
			w.WriteHeader(204)
			return
		}
		if r.Method == http.MethodGet {
			m.gets = append(m.gets, r.URL.EscapedPath())
			if m.bare404 {
				// No body at all -- nothing meter's writeError would ever
				// produce, which is the whole point.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if m.notRegistered {
				http.Error(w, `{"error":"project not registered"}`, http.StatusNotFound)
				return
			}
			resp := meterapi.ProjectResponse{
				Rig: "fake", State: m.state, ConfigHash: "sha256:fake",
				Effective: &meterapi.Effective{
					Enabled: true,
					Actions: meterapi.Actions{Triage: true},
					Ladder:  []string{"qwen-local"},
					Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
				},
				KeyRef: meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)
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

// The cache is DERIVED ONLY (cache.go): it rebuilds empty on every intake
// restart, reschedule, rollout or fresh replica. A project that is already
// archived the first time such a fresh reconciler ever observes it was never
// Cache.Put, so a deregister gated on a cache hit would skip it forever --
// exactly the hole a cache-gated fix would silently reintroduce. Deregister
// must fire off the live GitLab project, not the cache.
func TestReconcileDeregistersArchivedProjectOnColdCache(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	gl.SetArchived(p.ID, true) // already archived before the reconciler ever sees it
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m) // fresh Reconciler: empty cache, first pass ever

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(m.deletes) != 1 || !strings.Contains(m.deletes[0], "group%2Frepo") {
		t.Fatalf("a cold-cache first pass must still DELETE the meter registration for an already-archived project (URL-escaped): %v", m.deletes)
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("an already-archived project must never enter the cache")
	}
}

// T-12 made the archived branch deregister UNCONDITIONALLY (off `p`, not a
// cache hit) to close the cold-cache hole above. Left there, "unconditional"
// means every subsequent pass over the SAME archived project repeats the
// meter DELETE forever -- a meter call, an Obs.MeterPush("ok"), and (per
// meter's own handler) a Secret delete and a DB delete behind it, none of it
// visible because Deregister is idempotent. Once a deregistration has
// actually succeeded, a later pass over the same still-archived project must
// not touch meter again AT ALL.
func TestReconcileSkipsMeterCallOnSecondArchivedPass(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)

	// Pass 1: register while active.
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Pass 2: the project archives. This is the FIRST archived pass, so it
	// must still deregister.
	gl.SetArchived(p.ID, true)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletes) != 1 {
		t.Fatalf("pass 2 (first archived pass): deletes = %d, want 1", len(m.deletes))
	}
	getsAfterPass2, deletesAfterPass2 := len(m.gets), len(m.deletes)

	// Pass 3: same archived project, already successfully deregistered in
	// pass 2. This must issue NO meter call -- neither a GET nor a DELETE --
	// or the reconciler is still repeating an idempotent-but-not-free call
	// every pass forever, which is the defect this test exists to catch.
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.gets) != getsAfterPass2 || len(m.deletes) != deletesAfterPass2 {
		t.Fatalf("pass 3 touched meter for an already-deregistered archived project: gets %d->%d, deletes %d->%d",
			getsAfterPass2, len(m.gets), deletesAfterPass2, len(m.deletes))
	}
}

// A restart (or reschedule, rollout, fresh replica) empties the reconciler's
// in-memory tombstone the same way it empties the rest of the cache (cache.go
// is derived-only). Simulated here with a brand-new Reconciler (fresh Cache)
// against a meter that already has NO record of the project -- standing in
// for "a previous intake replica deregistered this project before this one
// ever started". The cold reconciler must ask meter (GET) rather than assume
// either answer, and since meter says gone, it must NOT re-issue a DELETE.
//
// What it must ALSO not do is tombstone. The tombstone records what THIS
// process DELETED, never what it merely observed: a negative GET means
// "nothing to do this pass", not "never ask again". Tombstoning an
// observation is how a 404 nobody looked at closely turns into a permanent,
// silent "already gone" for the life of the process -- and meter's state can
// move under us regardless (a re-registration, a restored backup, an
// operator's manual write). Re-asking costs one read-only GET per pass: no
// LiteLLM call, no Secret delete, no DB write. So the second pass here asks
// again rather than short-circuiting on a tombstone it never earned.
func TestReconcileArchivedProjectAlreadyGoneFromMeterKeepsCheckingEveryPass(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	gl.SetArchived(p.ID, true)
	m := newFakeMeter(t)
	m.notRegistered = true       // meter has no record of this project
	r := newReconciler(t, gl, m) // fresh Reconciler: cold tombstone, as after a restart

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletes) != 0 {
		t.Fatalf("meter already had no registration for the project; must not issue a DELETE: %v", m.deletes)
	}
	if len(m.gets) != 1 {
		t.Fatalf("a cold tombstone must ask meter once via GET before deciding: got %d gets", len(m.gets))
	}
	if r.Cache.WasDeregistered(p.ID) {
		t.Fatal("a negative GET must NOT tombstone: only a DELETE this process actually issued may do that")
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("an already-archived project must never enter the cache")
	}

	// Second pass: the check repeats, because nothing was ever deleted here
	// and therefore nothing was tombstoned.
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.gets) != 2 {
		t.Fatalf("gets = %d, want 2: an untombstoned archived project must be re-checked every pass", len(m.gets))
	}
	if len(m.deletes) != 0 {
		t.Fatalf("re-checking must stay read-only while meter reports the project gone: %v", m.deletes)
	}
}

// A 404 is not automatically "already deregistered". Meter's own 404 carries
// meter's {"error":"project not registered"} body (internal/meter/service's
// writeError); a BARE 404 is what an intermediary returns when the request
// never reached meter's handler at all -- a BaseURL with a wrong path prefix,
// an ingress or proxy in front of meter, a route that stopped matching.
//
// Decided on the status code alone, that misconfiguration reads as "nothing
// to deregister" and the pass reports success while the archived project's
// LiteLLM key stays live. Before the tombstone existed the same misroute made
// the DELETE fail loudly every pass; converting it into a silent success is
// the wrong direction for the one component whose job is making sure a
// credential does not outlive its project. So it must stay LOUD: an error,
// counted in the summary, no tombstone, retried next pass.
func TestReconcileArchivedProjectTreatsBare404AsErrorNotAsAlreadyGone(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	gl.SetArchived(p.ID, true)
	m := newFakeMeter(t)
	m.bare404 = true // a 404 meter's own handler did not write
	r := newReconciler(t, gl, m)

	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("a per-project meter failure must not abort the pass: %v", err)
	}
	if sum.Errors != 1 {
		t.Fatalf("sum.Errors = %d, want 1: a bare 404 is an unanswered question, not a successful pass (%+v)", sum.Errors, sum)
	}
	if r.Cache.WasDeregistered(p.ID) {
		t.Fatal("a bare 404 must never tombstone the project: nothing was confirmed and nothing was deleted")
	}
	if len(m.deletes) != 0 {
		t.Fatalf("a bare 404 must not be followed by a DELETE: %v", m.deletes)
	}

	// Retried, not silently accepted.
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(m.gets) != 2 {
		t.Fatalf("gets = %d, want 2: a bare 404 must be re-asked next pass", len(m.gets))
	}
}

// The load-bearing negative case for the tombstone: meter reports the project
// IS still registered, and the deregister DELETE then fails. Nothing was
// deleted, so nothing may be tombstoned -- a tombstone here would skip the
// meter call for the remaining lifetime of the process and leave a live
// LiteLLM key behind an archived project, which is precisely the failure the
// archived branch exists to prevent. The tombstone must stay COLD, and the
// next pass must retry the DELETE and only then tombstone.
func TestReconcileArchivedProjectFailedDeleteLeavesTombstoneCold(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// GET stays healthy (meter still has the registration); only the DELETE
	// fails. That is the ordering that matters: the check succeeds and says
	// "still registered", so the reconciler DOES attempt the deregistration
	// and DOES fail at it.
	gl.SetArchived(p.ID, true)
	m.failDeleteNow.Store(true)
	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("a per-project deregister failure must not abort the pass: %v", err)
	}
	if sum.Errors != 1 {
		t.Fatalf("sum.Errors = %d, want 1 (the failed deregister)", sum.Errors)
	}
	if len(m.gets) == 0 {
		t.Fatal("setup: the registration check must have run and reported the project still registered")
	}
	if len(m.deletes) != 0 {
		t.Fatalf("setup: the DELETE was supposed to fail, so none should have been recorded: %v", m.deletes)
	}
	if r.Cache.WasDeregistered(p.ID) {
		t.Fatal("a FAILED deregister must leave the tombstone cold: nothing was deleted, so the next pass must retry")
	}
	if _, ok := r.Cache.Get(p.ID); !ok {
		t.Fatal("a failed deregister must keep the cache entry so the next pass retries")
	}

	// Meter recovers: the retry runs, succeeds, and only NOW tombstones.
	m.failDeleteNow.Store(false)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletes) != 1 {
		t.Fatalf("deletes = %d, want 1 once the retry succeeds", len(m.deletes))
	}
	if !r.Cache.WasDeregistered(p.ID) {
		t.Fatal("a SUCCESSFUL deregister must tombstone, so later passes stop calling meter for this project")
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("archived project must leave the cache once deregister succeeds")
	}
}

// An archived project whose meter is UNREACHABLE must fail closed. The meter
// here is down for everything, so what fails is the read-only registration
// check (MeterClient.Get), which runs BEFORE any DELETE -- the DELETE is
// never attempted at all in the failing pass, and the tombstone is never set,
// because a process that could not reach meter has confirmed nothing and
// deleted nothing. (The sibling case -- the check succeeds and the DELETE
// itself fails -- is TestReconcileArchivedProjectFailedDeleteLeavesTombstoneCold.)
//
// Unable to conclude anything, reconcileProject keeps the cache entry so the
// next pass retries, mirroring the vanished-membership sweep's own failure
// handling instead of forgetting a still-live key. The cost: the retained
// entry keeps its pre-archival StateValid classification, so it stays
// Dispatchable() until a later pass's retry succeeds. Pinned here as the
// intended behaviour, not left as an unverified side effect: it is the same
// trade the sweep already makes (never forget a live key), and the window
// closes on the very next successful reconcile pass.
func TestReconcileArchivedProjectStaysDispatchableWhileMeterIsUnreachable(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	gl.SetArchived(p.ID, true)
	m.failNow.Store(true) // meter is down; the deregister DELETE will fail
	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("a per-project deregister failure must not abort the pass: %v", err)
	}
	if sum.Errors != 1 {
		t.Fatalf("sum.Errors = %d, want 1 (the failed deregister)", sum.Errors)
	}

	if r.Cache.WasDeregistered(p.ID) {
		t.Fatal("an unreachable meter must leave the tombstone cold: nothing was confirmed and nothing was deleted")
	}

	e, ok := r.Cache.Get(p.ID)
	if !ok {
		t.Fatal("a failed deregister must keep the cache entry so the next pass retries")
	}
	if !e.Dispatchable() {
		t.Fatalf("documenting current behaviour: the retained entry is still Dispatchable() during the retry window: %+v", e)
	}

	m.failNow.Store(false) // meter recovers
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.deletes) != 1 {
		t.Fatalf("deletes = %d, want 1 once the retry succeeds", len(m.deletes))
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("archived project must leave the cache once deregister succeeds")
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

// Summary.Dispatched must count every order actually FIRED in a pass, not just
// the scaffold path: reconcileProject only ever incremented it for
// out.scaffoldFired, so a triage order the issue sweep fired never reached it.
// An operator watching `?wait=true` during an incident where the webhook was
// down and the sweep was the only thing dispatching would have seen Dispatched
// == 0 and concluded nothing was firing, when it was -- exactly the invisible
// failure T-38's item 5 exists to close.
func TestReconcileSummaryCountsIssueSweepDispatches(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	gl.AddIssue(p.ID, 1, "opened") // no gonk:: label: not yet triaged
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	sw := &countingSweeper{fire: true} // simulate Gate 1 actually firing the order
	r.Issues = sw

	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if sum.IssuesSwept != 1 {
		t.Fatalf("IssuesSwept = %d, want 1 (the attempt)", sum.IssuesSwept)
	}
	if sum.Dispatched != 1 {
		t.Fatalf("Dispatched = %d, want 1: the sweep's fired order must be counted, same as scaffold's", sum.Dispatched)
	}
}

// A zero BotUserID makes the issue-sweep loop guard
// (`r.BotUserID != 0 && is.Author.ID == r.BotUserID`) silently no-op instead
// of disabling itself loudly: an issue authored by user id 0 would never
// occur, so the guard just never fires and gonk can react to its own swept
// issues. NewReconciler is the sanctioned constructor and must refuse to
// build one, mirroring ghook.NewHandler's refusal of the same field.
func TestNewReconcilerRefusesZeroBotUserID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := NewReconciler(&Reconciler{BotUserID: id}); err == nil {
			t.Errorf("NewReconciler(BotUserID: %d) = nil error, want a refusal", id)
		}
	}
}

func TestNewReconcilerAcceptsAPositiveBotUserID(t *testing.T) {
	r, err := NewReconciler(&Reconciler{BotUserID: 7})
	if err != nil {
		t.Fatalf("NewReconciler(BotUserID: 7) = %v, want no error", err)
	}
	if r.BotUserID != 7 {
		t.Fatalf("BotUserID = %d, want 7", r.BotUserID)
	}
}
