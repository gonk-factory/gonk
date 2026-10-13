package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/gcapi/gcapitest"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/rung"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// LEGACY GAS CITY SESSIONS AT THE CUTOVER (gonk-t55 plan 3.4 C1). A running or
// pending-prompt record the old controller wrote names a Gas City alias, which
// the Jobs runtime has never heard of. It must not be judged absent (charged,
// escalated): it is parked, its Gas City pod deleted by label, and the bead
// re-dispatched as a Job. See sweep_legacy.go.

// legacyParkLine is the EXACT text of the legacy-park log line. It is spelled
// out here, not taken from legacyParkMessage, because the dev smoke test keys
// on this text: changing the constant must turn this red.
const legacyParkLine = "sweep: legacy Gas City session at cutover; not judged, not charged: " +
	"parking the bead to re-dispatch as a Job"

// legacyFixture is the jobs sweep fixture with its running record rewritten
// to name a Gas City alias, and a fake apiserver holding that session's pod
// plus three bystanders the legacy pod pass must leave alone.
func legacyFixture(t *testing.T) (*jobsSweepFixture, *fakeAPIServer, string) {
	t.Helper()
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	alias := brokerSessionAlias("triage", f.rec.ProjectID, f.rec.IssueIID, f.rec.Attempt)
	f.rec.SessionID = alias
	if err := f.store.Put(context.Background(), f.rec); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPIServer(t)
	api.AddPod("s-go-legacy", alias, time.Now().Add(-10*time.Minute))
	// Another legacy session a pending-prompt record still CLAIMS: kept.
	claimedAlias := brokerSessionAlias("triage", f.rec.ProjectID, 99, 1)
	claimed := f.rec
	claimed.BeadAnchor, claimed.IssueIID, claimed.SessionID = "gonk:42:issue:99", 99, claimedAlias
	claimed.State = beadstore.StatePendingPrompt
	if err := f.store.Put(context.Background(), claimed); err != nil {
		t.Fatal(err)
	}
	api.AddPod("s-go-claimed", claimedAlias, time.Now().Add(-10*time.Minute))
	// A Gas City session that is not gonk's (its alias is not gonk's shape).
	api.AddPod("s-go-control", "control-dispatcher", time.Now().Add(-10*time.Minute))
	// A pod already on its way out.
	api.AddPod("s-go-going", brokerSessionAlias("triage", f.rec.ProjectID, 98, 1), time.Now().Add(-10*time.Minute)).
		DeletionTimestamp = ptrTime(metav1.Now())
	f.deps.LegacyPods = api.Reaper(t)
	f.deps.LegacyCloser = newFakeGasCity(api, alias, claimedAlias)
	return f, api, alias
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

// bystanders is what legacyFixture's apiserver must still hold after a pass
// that deleted the legacy session's pod and nothing else.
var bystanders = []string{"s-go-claimed", "s-go-control", "s-go-going"}

func samePods(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestALegacyGasCitySessionIsParkedNotChargedAndItsPodDeleted(t *testing.T) {
	f, api, alias := legacyFixture(t)
	now := time.Now()
	f.deps.Now = func() time.Time { return now }

	f.sweep(t)

	// NOT JUDGED, NOT CHARGED: nothing reached meter, nothing was applied.
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v; a legacy Gas City session must report NOTHING to meter; log:\n%s", reqs, f.log.String())
	}
	if len(f.applier.notes) != 0 {
		t.Fatalf("notes = %+v; a legacy session is not judged", f.applier.notes)
	}
	out := f.log.String()
	for _, bad := range []string{"released_by=absent", "classified infra-failed", "no session exists under this name"} {
		if strings.Contains(out, bad) {
			t.Errorf("the legacy session went down the absent/judged path (%q):\n%s", bad, out)
		}
	}
	// Its Gas City pod is gone, in the same pass; the bystanders are not.
	if got := api.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left = %v, want %v (only the legacy session's pod deleted, by its gc-agent label)", got, bystanders)
	}
	// Parked, same attempt and reservation, no session, no re-sling stamps.
	got, ok, err := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if got.State != beadstore.StateParked || got.SessionID != "" {
		t.Fatalf("record = %s/%q, want parked with no session; log:\n%s", got.State, got.SessionID, out)
	}
	if !got.RetryAfter.Equal(now.Add(legacyRetryAfter)) {
		t.Errorf("retry_after = %v, want %v", got.RetryAfter, now.Add(legacyRetryAfter))
	}
	if got.Attempt != f.rec.Attempt || got.ReservationID != f.rec.ReservationID || !got.ReservationExpiresAt.Equal(f.rec.ReservationExpiresAt) {
		t.Errorf("attempt/reservation changed: %+v", got)
	}
	if !got.ReslungAt.IsZero() || !got.ReslingFireBy.IsZero() {
		t.Errorf("parked record carries re-sling stamps %v/%v", got.ReslungAt, got.ReslingFireBy)
	}
	// RE-DISPATCHED in the same pass: the unpark fired gonk-dispatch, with no
	// fire_by (it is not a re-sling).
	fired := f.orders.orders()
	if len(fired) != 1 || fired[0].Order != dispatchOrderName || fired[0].Vars["bead_anchor"] != f.rec.BeadAnchor {
		t.Fatalf("orders = %+v, want one gonk-dispatch for %s", fired, f.rec.BeadAnchor)
	}
	if _, has := fired[0].Vars[fireByVar]; has {
		t.Errorf("the unpark carried %s", fireByVar)
	}
	// ONE stable WARN line, with its exact text, naming the session.
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, legacyParkLine) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "session="+alias) {
		t.Fatalf("legacy-park lines = %q, want exactly one WARN naming %s; log:\n%s", lines, alias, out)
	}
	if !strings.Contains(out, "pod=s-go-legacy") {
		t.Fatalf("the pod deletion is not logged by name:\n%s", out)
	}
}

// A pending-prompt reservation the old controller wrote goes through the
// sweep's reclaim (after pendingPromptGrace) and is then parked, never judged.
func TestALegacyPendingPromptReservationIsReclaimedThenParkedNotJudged(t *testing.T) {
	f, api, _ := legacyFixture(t)
	rec, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	rec.State = beadstore.StatePendingPrompt
	if err := f.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(pendingPromptGrace + time.Minute)
	f.deps.Now = func() time.Time { return now }

	f.sweep(t) // reclaim: pending-prompt -> running
	f.sweep(t) // the running pass: legacy -> parked

	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none; log:\n%s", reqs, f.log.String())
	}
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateParked || got.SessionID != "" {
		t.Fatalf("record = %s/%q, want parked; log:\n%s", got.State, got.SessionID, f.log.String())
	}
	// Both legacy reservations -- this one and the fixture's issue-99 one --
	// were past the grace, so both were reclaimed, parked, and their pods
	// deleted; only the non-gonk and the terminating pods remain.
	if names := api.Names(); !samePods(names, []string{"s-go-control", "s-go-going"}) {
		t.Fatalf("pods left = %v, want [s-go-control s-go-going]", names)
	}
	if !strings.Contains(f.log.String(), legacyParkLine) {
		t.Fatalf("no legacy-park line:\n%s", f.log.String())
	}
}

// A pod whose delete fails does not hold the bead: it is parked, and the pod
// is deleted by a later pass. And GAS CITY RESPAWNS an open session's pod
// (its reconciler wakes an active session whose runtime is missing), so a pod
// that comes back under the same alias is deleted again on the next pass.
func TestALegacyPodIsDeletedByLaterPassesWhenItsDeleteFailsOrItComesBack(t *testing.T) {
	f, api, alias := legacyFixture(t)
	api.DeleteFails["s-go-legacy"] = true

	f.sweep(t)
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateParked {
		t.Fatalf("record = %s, want parked even though the pod delete failed; log:\n%s", got.State, f.log.String())
	}
	if !strings.Contains(f.log.String(), "IT IS STILL RUNNING") {
		t.Fatalf("the failed delete was not logged at ERROR:\n%s", f.log.String())
	}
	if names := api.Names(); len(names) != len(bystanders)+1 {
		t.Fatalf("pods left = %v, want the undeletable legacy pod still there", names)
	}

	delete(api.DeleteFails, "s-go-legacy")
	f.sweep(t)
	if names := api.Names(); !samePods(names, bystanders) {
		t.Fatalf("pods left = %v, want the legacy pod deleted by the next pass", names)
	}

	api.AddPod("s-go-respawn", alias, time.Now()) // Gas City woke it again
	f.sweep(t)
	if names := api.Names(); !samePods(names, bystanders) {
		t.Fatalf("pods left = %v, want the respawned legacy pod deleted too", names)
	}
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none", reqs)
	}
}

// Past its reservation a legacy session is still parked, not judged, and the
// janitor charge that is now unavoidable is said.
func TestALegacySessionPastItsReservationIsParkedAndTheJanitorChargeIsSaid(t *testing.T) {
	f, _, _ := legacyFixture(t)
	now := f.rec.ReservationExpiresAt.Add(time.Minute)
	f.deps.Now = func() time.Time { return now }
	f.sweep(t)
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateParked {
		t.Fatalf("record = %s, want parked; log:\n%s", got.State, f.log.String())
	}
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none even past the reservation", reqs)
	}
	if !strings.Contains(f.log.String(), "janitor records that attempt infra-failed") {
		t.Fatalf("the janitor charge is not said:\n%s", f.log.String())
	}
}

// THE INTERCEPT RUNS AFTER THE RE-SLING WINDOW CHECK, AND THE ORDER MATTERS
// (plan 3.5 R2; verifier V19). A legacy record carrying ReslungAt is one the
// OLD controller already judged: it reported the outcome to meter (the attempt
// IS charged) and fired a re-sling dispatch that owns the bead until the
// window ends. Inside that window the sweep must wait, legacy alias or not.
// Were the intercept moved above the window check, this pass would park the
// record -- erasing the ReslungAt marker, logging "not judged, not charged"
// for an attempt that was both, and firing a SECOND gonk-dispatch while the
// re-sling's own may still be in flight (the gonk-p8w8 class the window
// exists to prevent). Past the window, it is parked as any legacy record is.
func TestALegacyRecordInsideItsReslingWindowWaitsAndIsNotParked(t *testing.T) {
	f, api, alias := legacyFixture(t)
	rec, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	reslung := time.Now().Add(-time.Minute)
	rec.ReslungAt, rec.ReslingFireBy = reslung, reslingFireBy(reslung)
	if err := f.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	end, ok := reslingWindowEnd(rec)
	if !ok {
		t.Fatal("fixture: the record carries no re-sling window")
	}
	now := reslung.Add(time.Minute)
	f.deps.Now = func() time.Time { return now }

	f.sweep(t)

	out := f.log.String()
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateRunning || got.SessionID != alias || !got.ReslungAt.Equal(reslung) {
		t.Fatalf("record = %s/%q reslung_at=%v, want it untouched (running, %s, %v) inside the re-sling window "+
			"-- the legacy intercept ran before the window check; log:\n%s", got.State, got.SessionID, got.ReslungAt, alias, reslung, out)
	}
	if fired := f.orders.orders(); len(fired) != 0 {
		t.Fatalf("orders = %+v, want none: the re-sling's own dispatch owns the bead until %v", fired, end)
	}
	if strings.Contains(out, legacyParkLine) {
		t.Fatalf("a record the old controller already judged and charged was logged as not judged, not charged:\n%s", out)
	}
	if !strings.Contains(out, "waiting for its dispatch") {
		t.Fatalf("the window wait is not logged:\n%s", out)
	}
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none: it was judged already", reqs)
	}
	// The running record still claims its alias, so the per-pass pod pass keeps the pod.
	if names := api.Names(); !slices.Contains(names, "s-go-legacy") {
		t.Fatalf("pods = %v: the legacy pod of a record inside its window was deleted", names)
	}

	// Past the window, the re-sling did not replace the record: parked, re-dispatched.
	now = end.Add(time.Second)
	f.sweep(t)
	got, _, _ = f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateParked || !strings.Contains(f.log.String(), legacyParkLine) {
		t.Fatalf("record = %s past the window, want parked with the legacy line; log:\n%s", got.State, f.log.String())
	}
	if fired := f.orders.orders(); len(fired) != 1 {
		t.Fatalf("orders = %+v past the window, want exactly one re-dispatch", fired)
	}
}

// An unreadable store deletes no legacy pod: every pod would look unclaimed.
func TestTheLegacyPodPassDeletesNothingWhenTheStoreCannotBeRead(t *testing.T) {
	f, api, _ := legacyFixture(t)
	f.deps.LegacyPods = api.Reaper(t)
	// runSweep lists running, parked and pending-prompt (3 reads), then the
	// stale-session pass reads running and pending-prompt (2); every read after
	// those five is the legacy pod pass's.
	f.deps.Store = &failListAfterStore{Store: f.store, after: 5}
	// A legacy pod no record names: only the per-pass pass could delete it.
	// (The fixture's own legacy pod is deleted by the park, which reads no
	// list -- it closes and deletes the one alias its record named.)
	api.AddPod("s-go-orphan", brokerSessionAlias("triage", f.rec.ProjectID, 97, 1), time.Now().Add(-time.Hour))
	f.sweep(t)
	if names := api.Names(); !slices.Contains(names, "s-go-orphan") {
		t.Fatalf("pods left = %v, want the unclaimed legacy pod kept on an unreadable store; log:\n%s", names, f.log.String())
	}
	if !strings.Contains(f.log.String(), "deleting NO legacy Gas City pods") {
		t.Fatalf("not said:\n%s", f.log.String())
	}
}

// failListAfterStore fails every List after the first `after`.
type failListAfterStore struct {
	beadstore.Store
	after, n int
}

func (s *failListAfterStore) List(ctx context.Context, st beadstore.State) ([]beadstore.Record, error) {
	s.n++
	if s.n > s.after {
		return nil, errStoreDown
	}
	return s.Store.List(ctx, st)
}

// With no pod cleanup configured the bead is still parked, and that the pod
// was not looked for is said.
func TestALegacySessionWithNoPodCleanupConfiguredIsStillParked(t *testing.T) {
	f, _, _ := legacyFixture(t)
	f.deps.LegacyPods = nil
	f.sweep(t)
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateParked {
		t.Fatalf("record = %s, want parked; log:\n%s", got.State, f.log.String())
	}
	if !strings.Contains(f.log.String(), "no pod cleanup configured") {
		t.Fatalf("the unconfigured cleanup was not said:\n%s", f.log.String())
	}
}

// The shape test: every alias brokerSessionAlias mints (issue- and
// project-scoped) and the pre-nonce form are legacy; no runtime.Name is.
func TestLegacyGasCityAliasShape(t *testing.T) {
	for _, id := range []string{
		brokerSessionAlias("triage", 75, 83, 1),
		brokerSessionAlias("scaffold", 42, 0, 2),
		"gonk.triage.p75.i24.a1", // pre-nonce (before gonk-mzd)
		"gonk.triage.p75.i83.a1.OX3K" + testNonce[:22],
	} {
		if !isLegacyGasCitySession(id) {
			t.Errorf("%q not recognised as a legacy Gas City alias", id)
		}
	}
	for _, id := range []string{
		runtime.Name("triage", 75, runtime.KindIssue, 83, 1),
		runtime.Name("scaffold", 42, runtime.KindIssue, 0, 1),
		runtime.Name("a-very-long-agent-name-that-forces-the-hash-path", 123456789, runtime.KindIssue, 987654321, 12),
		"", "gonkish.triage.p1.a1", "s-go-whg1",
	} {
		if isLegacyGasCitySession(id) {
			t.Errorf("%q taken for a legacy Gas City alias", id)
		}
	}
}

// THE ATTEMPT PROOF, on the REAL meter (the handshake's harness): a legacy
// session parked at the cutover, then the Job retry succeeding, is ONE attempt
// -- attempt 1, success -- in meter's own history.
func TestALegacyParkThenASuccessIsOneAttemptOnTheRealMeter(t *testing.T) {
	ctx := context.Background()
	m := newRealMeter(t)
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	store := beadstore.NewMemory()
	orders := &fakeOrders{}
	applier := &recordingApplier{}
	log := &syncBuilder{}
	args := baseDispatchArgs()
	args.ProjectID = p.ID
	name := runtime.Name("triage", p.ID, runtime.KindIssue, 3, 1)
	api := newFakeAPIServer(t)

	dispatch := func() {
		t.Helper()
		if code := runDispatch(ctx, dispatchDeps{
			Keys:  readerWith(secret(m.key.SecretName, m.key.SecretKey, "sk-project-real")),
			Meter: m.api, Runtime: rt, Store: store, Forge: promptTestForge(),
			Args: args, Clock: newFakeClock(), Log: testLogger(log),
		}); code != 0 {
			t.Fatalf("dispatch exit = %d; log:\n%s", code, log.String())
		}
	}
	now := time.Now()
	sweep := func() {
		t.Helper()
		if code := runSweep(ctx, sweepDeps{
			Meter: m.api, Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
			Store: store, BotUsername: "gonk", PackDir: repoPackDir, Log: testLogger(log),
			LegacyPods: api.Reaper(t),
			Now:        func() time.Time { return now }, SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		}); code != 0 {
			t.Fatalf("sweep exit = %d; log:\n%s", code, log.String())
		}
	}

	// 1. THE OLD CONTROLLER'S STATE: meter decided attempt 1 and the record
	// is running under a Gas City alias, its pod up. (Built by a real dispatch
	// against the real meter, then rewritten to the alias the old controller
	// recorded; the Job the new dispatch made is removed -- there was none.)
	dispatch()
	rec, _, _ := store.Get(ctx, args.BeadAnchor)
	if rec.Attempt != 1 || rec.ReservationID == "" {
		t.Fatalf("first decision = attempt %d reservation %q, want attempt 1 with a reservation", rec.Attempt, rec.ReservationID)
	}
	alias := brokerSessionAlias("triage", p.ID, 3, 1)
	rec.SessionID = alias
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := rt.Runtime.Stop(ctx, runtime.Handle(name)); err != nil {
		t.Fatal(err)
	}
	api.AddPod("s-go-cutover", alias, now.Add(-5*time.Minute))

	// 2. The first sweep under the new controller: parked, pod deleted, NO
	// attempt recorded, and the unpark fires the re-dispatch in the same pass.
	sweep()
	if prior, _ := m.store.Attempts(ctx, "group/repo", args.BeadAnchor); len(prior) != 0 {
		t.Fatalf("meter recorded %+v after a legacy park; it must not be an attempt; log:\n%s", prior, log.String())
	}
	if names := api.Names(); len(names) != 0 {
		t.Fatalf("pods left = %v, want the legacy pod deleted; log:\n%s", names, log.String())
	}
	if fired := orders.orders(); len(fired) != 1 {
		t.Fatalf("orders = %+v, want the unpark's one dispatch; log:\n%s", fired, log.String())
	}
	dispatch()

	// 3. The retry is attempt 1 AGAIN, as a Job under the D2 name.
	starts := rt.Starts()
	if len(starts) != 2 || starts[1].Name != name || starts[1].Env[envAttempt] != "1" {
		t.Fatalf("starts = %+v, want the retry %s at attempt 1", starts, name)
	}

	// 4. It answers.
	rt.SetOutput(runtime.Handle(name), []byte("GONK_BATCH_START\n"+
		`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Which export size fails?"}]}`+"\nGONK_BATCH_END\n"))
	rt.SetPhase(runtime.Handle(name), runtime.PhaseSucceeded, "")
	sweep()
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want the batch applied; log:\n%s", applier.notes, log.String())
	}

	// METER'S OWN HISTORY: exactly one attempt, attempt 1, a success.
	prior, err := m.store.Attempts(ctx, "group/repo", args.BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(prior) != 1 || prior[0].Attempt != 1 || prior[0].Outcome != rung.OutcomeSuccess {
		t.Fatalf("meter attempts = %+v, want exactly [attempt 1: success]", prior)
	}
	final, _, _ := store.Get(ctx, args.BeadAnchor)
	if final.State != beadstore.StateDone || final.Attempt != 1 {
		t.Fatalf("record = %s attempt %d, want done at attempt 1", final.State, final.Attempt)
	}
}

// fakeGasCity models the two things of Gas City's behaviour the legacy park
// depends on, as read from its source (a checkout of gascity at 4fda5a28445f,
// which IS the pinned GASCITY_REF in images/versions.env; read, NOT observed
// on a cluster):
//
//   - An OPEN session whose pod is gone gets a new pod on the reconciler's next
//     cycle. The k8s provider finds a session's pod by label and reports it
//     not running when there is none (internal/runtime/k8s/provider.go:721-730,
//     findRunningPod); an open bead in state "active" is wanted awake
//     (cmd/gc/session_reconcile.go:111, WakeSession); and a wanted, not-alive
//     session is started (cmd/gc/session_reconciler.go:3317, `if shouldWake &&
//     !target.alive`). Crash-loop quarantine is not modelled.
//   - A CLOSED session is not in the reconciler's set at all
//     (cmd/gc/session_bead_snapshot.go:14-18: the per-cycle snapshot holds only
//     active session beads; closed history is not loaded), so nothing
//     restarts its pod.
//
// Close of an alias with no open session answers 404, which the production
// adapter (main.go gcLegacyCloser) maps to errLegacySessionGone.
type fakeGasCity struct {
	api    *fakeAPIServer
	open   map[string]bool
	closed []string
	// closeErr fails every close (after recording nothing).
	closeErr error
	// onClose runs at the start of every close.
	onClose func(alias string)
	n       int
}

func newFakeGasCity(api *fakeAPIServer, openAliases ...string) *fakeGasCity {
	g := &fakeGasCity{api: api, open: map[string]bool{}}
	for _, a := range openAliases {
		g.open[a] = true
	}
	return g
}

func (g *fakeGasCity) CloseLegacySession(_ context.Context, alias string) error {
	if g.onClose != nil {
		g.onClose(alias)
	}
	if g.closeErr != nil {
		return g.closeErr
	}
	if !g.open[alias] {
		return fmt.Errorf("%w: %s: gcapi: 404 session not found", errLegacySessionGone, alias)
	}
	g.open[alias] = false
	g.closed = append(g.closed, alias)
	return nil
}

// reconcile is one Gas City reconciler cycle: every OPEN session with no live
// pod gets a fresh one under its alias.
func (g *fakeGasCity) reconcile() {
	have := map[string]bool{}
	g.api.mu.Lock()
	for _, p := range g.api.pods {
		if p.DeletionTimestamp == nil {
			have[p.Labels[agentAliasLabel]] = true
		}
	}
	g.api.mu.Unlock()
	for alias, open := range g.open {
		if open && !have[alias] {
			g.n++
			g.api.AddPod(fmt.Sprintf("s-go-respawn-%d", g.n), alias, time.Now())
		}
	}
}

// The park CLOSES the Gas City session and only THEN deletes its pod.
func TestTheLegacyParkClosesTheGasCitySessionBeforeDeletingItsPod(t *testing.T) {
	f, api, alias := legacyFixture(t)
	gc := f.deps.LegacyCloser.(*fakeGasCity)
	var podsAtClose []string
	gc.onClose = func(a string) {
		if a == alias {
			podsAtClose = api.Names()
		}
	}
	f.sweep(t)
	if len(gc.closed) != 1 || gc.closed[0] != alias {
		t.Fatalf("closed = %v, want exactly [%s] (only the alias the record named; claimed aliases untouched)", gc.closed, alias)
	}
	if !slices.Contains(podsAtClose, "s-go-legacy") {
		t.Fatalf("pods at close time = %v: the pod was deleted BEFORE the session was closed", podsAtClose)
	}
	if got := api.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left = %v, want %v", got, bystanders)
	}
	if !strings.Contains(f.log.String(), "closed the legacy Gas City session") {
		t.Fatalf("the close is not logged:\n%s", f.log.String())
	}
}

// ONCE THE SESSION IS CLOSED, THE PER-PASS POD PASS HAS NOTHING TO DELETE FOR
// IT: Gas City does not restart a closed session's pod. And the contrast: when
// the close FAILS, Gas City restarts the pod and the pass deletes it.
func TestTheLegacyPodPassHasNothingToDeleteOnceTheSessionIsClosed(t *testing.T) {
	f, api, alias := legacyFixture(t)
	gc := f.deps.LegacyCloser.(*fakeGasCity)

	f.sweep(t) // park: close, delete the pod
	gc.reconcile()
	before := strings.Count(f.log.String(), legacyPodDeletedMessage)
	f.sweep(t)
	for _, p := range api.Names() {
		if strings.HasPrefix(p, "s-go-respawn") {
			t.Fatalf("Gas City restarted %s after the session was closed; pods = %v", p, api.Names())
		}
	}
	if after := strings.Count(f.log.String(), legacyPodDeletedMessage); after != before {
		t.Fatalf("the pod pass deleted %d pod(s) after the session was closed; want none:\n%s", after-before, f.log.String())
	}
	if got := api.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left = %v, want %v", got, bystanders)
	}

	// Contrast, same model: a session the park could NOT close is restarted by
	// Gas City, and the per-pass pass is what removes the restart.
	f2, api2, alias2 := legacyFixture(t)
	gc2 := f2.deps.LegacyCloser.(*fakeGasCity)
	gc2.closeErr = errors.New("gcapi: 503 supervisor unavailable")
	f2.sweep(t)
	if !strings.Contains(f2.log.String(), "could not close the legacy Gas City session; deleting its pod anyway") {
		t.Fatalf("the failed close is not a WARN:\n%s", f2.log.String())
	}
	if got := api2.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left after a failed close = %v; the pod delete must still run", got)
	}
	gc2.reconcile()
	if got := api2.Names(); samePods(got, bystanders) {
		t.Fatalf("the model did not restart the still-open session's pod; the contrast proves nothing")
	}
	f2.sweep(t)
	if got := api2.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left = %v; the per-pass pass must delete the pod Gas City restarted for %s", got, alias2)
	}
	if gc.open[alias] {
		t.Fatalf("the park did not close %s", alias)
	}
}

// A close answering 404 (the bead is already closed) is INFO, not a warning,
// and the pod is still deleted.
func TestALegacyCloseThatFindsTheSessionAlreadyClosedIsInfo(t *testing.T) {
	f, api, alias := legacyFixture(t)
	f.deps.LegacyCloser.(*fakeGasCity).open[alias] = false
	f.sweep(t)
	out := f.log.String()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "legacy Gas City session was already closed") {
			line = l
		}
	}
	if !strings.Contains(line, "level=INFO") {
		t.Fatalf("already-closed line = %q, want INFO; log:\n%s", line, out)
	}
	if strings.Contains(out, "could not close the legacy") {
		t.Fatalf("a 404 close was treated as a failure:\n%s", out)
	}
	if got := api.Names(); !samePods(got, bystanders) {
		t.Fatalf("pods left = %v, want the pod deleted", got)
	}
}

// The production adapter maps Gas City's 404, and only it, onto
// errLegacySessionGone.
func TestGCLegacyCloserMapsNotFoundToGone(t *testing.T) {
	gc := gcapitest.New(t)
	alias := "gonk.triage.p42.i3.a1." + testNonce
	c := gcLegacyCloser{c: gc.Client("gonk-city")}
	if err := c.CloseLegacySession(context.Background(), alias); !errors.Is(err, errLegacySessionGone) {
		t.Fatalf("close of an unknown alias = %v, want errLegacySessionGone", err)
	}
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, time.Now().Format(time.RFC3339))
	if err := c.CloseLegacySession(context.Background(), alias); err != nil {
		t.Fatalf("close of an open session = %v, want nil", err)
	}
	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("live sessions after close = %v", live)
	}
}

// THE PRODUCTION WIRING. Every test above hands the sweep its closer and pod
// lister directly, so a main.go that stopped setting them would leave all of
// them green while dev closed nothing and deleted nothing. This reads main.go's
// syntax tree: the sweepDeps it builds must set LegacyCloser to the gcapi
// adapter and LegacyPods to the pod reaper.
func TestMainWiresTheLegacyCloserAndPodLister(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"LegacyCloser": "gcLegacyCloser", "LegacyPods": "podReaperOrNil"}
	got := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "sweepDeps" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, _ := kv.Key.(*ast.Ident)
			if key == nil {
				continue
			}
			switch v := kv.Value.(type) {
			case *ast.CompositeLit:
				if id, ok := v.Type.(*ast.Ident); ok {
					got[key.Name] = id.Name
				}
			case *ast.CallExpr:
				if id, ok := v.Fun.(*ast.Ident); ok {
					got[key.Name] = id.Name
				}
			}
		}
		return true
	})
	for k, v := range want {
		if got[k] != v {
			t.Errorf("main.go's sweepDeps sets %s = %q, want %s(...)", k, got[k], v)
		}
	}
}
