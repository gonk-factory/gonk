package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/internal/meter/keysink"
	"github.com/gonk-factory/gonk/internal/meter/litellm"
	"github.com/gonk-factory/gonk/internal/meter/service"
	meterstore "github.com/gonk-factory/gonk/internal/meter/store"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/rung"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// IMAGE SKEW (gonk-t55 plan 3.3, "handshake" item 3): a session the agent's
// entrypoint refused for a session-contract mismatch is logged at ERROR with
// both versions, NOT reported to meter, NOT an attempt, and the bead is parked
// to run again. See sweep_skew.go.

// skewOutput is the container log of a refused session: the entrypoint's
// identity line, its refusal naming both versions, and its closing line --
// exactly the lines images/agent/entrypoint.sh writes (test/entrypoint pins
// the script side).
func skewOutput(sent string) string {
	return "gonk-agent-entrypoint: session start: runtime=jobs session=" + testSession + " agent=triage attempt=1\n" +
		"gonk-agent-entrypoint: FATAL: session contract mismatch: sent=" + sent + " supported=[1] -- the controller " +
		"that built this Job and this agent image disagree (image skew)\n" +
		"gonk-agent-entrypoint: " + skewRefusalLine + "\n"
}

// skewSession admits testSession as a session that exited with the
// entrypoint's contract-refusal code and output.
func skewSession(t *testing.T, rt *testRuntime, output string) runtime.Handle {
	t.Helper()
	h := rt.startNamed(t, testSession)
	rt.SetOutput(h, []byte(output))
	code := runtime.ExitSessionContractMismatch
	now := time.Now()
	rt.SetStatus(h, runtime.Status{
		Phase: runtime.PhaseFailed, Reason: "BackoffLimitExceeded",
		StartedAt: now.Add(-time.Second), FinishedAt: now, ExitCode: &code,
	})
	return h
}

func TestAnImageSkewedSessionIsParkedNotChargedAndNotAnAttempt(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	skewSession(t, rt, skewOutput("2"))
	now := time.Now()
	f.deps.Now = func() time.Time { return now }

	f.sweep(t)

	// NOT CHARGED: no outcome of any kind reached meter.
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v; a skewed session must report NOTHING to meter (that is what would record an attempt)", reqs)
	}
	// Parked, with a retry-after, the same attempt, and no session on it.
	got, ok, err := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if got.State != beadstore.StateParked {
		t.Fatalf("state = %s, want parked; log:\n%s", got.State, f.log.String())
	}
	if want := now.Add(skewRetryAfter); !got.RetryAfter.Equal(want) {
		t.Errorf("retry_after = %v, want %v", got.RetryAfter, want)
	}
	if got.SessionID != "" {
		t.Errorf("parked record still names session %q: the retry (same attempt) would be refused by the dedupe guard", got.SessionID)
	}
	if got.Attempt != f.rec.Attempt || got.ReservationID != f.rec.ReservationID {
		t.Errorf("attempt/reservation = %d/%s, want %d/%s unchanged", got.Attempt, got.ReservationID, f.rec.Attempt, f.rec.ReservationID)
	}
	if !got.ReslingFireBy.IsZero() || !got.ReslungAt.IsZero() {
		t.Errorf("parked record carries a re-sling stamp (%v/%v): the unpark's dispatch would refuse itself as late", got.ReslungAt, got.ReslingFireBy)
	}
	// The refused session is removed, so the retry creates a fresh Job.
	if live := rt.live(t); len(live) != 0 {
		t.Errorf("live = %v, want the refused session stopped", live)
	}
	// Nothing applied, nothing re-slung THIS tick (the park waits).
	if len(f.applier.notes) != 0 || len(f.orders.orders()) != 0 {
		t.Errorf("notes=%d orders=%d; a skewed session applies nothing and fires nothing now", len(f.applier.notes), len(f.orders.orders()))
	}
	// LOUD: one ERROR line naming both versions and this controller's own.
	out := f.log.String()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "IMAGE SKEW") {
			line = l
		}
	}
	for _, want := range []string{
		"level=ERROR", "job_session_contract=2", `agent_supported_session_contracts=1`,
		"controller_session_contract=" + strconv.Itoa(runtime.SessionContract), "session=" + testSession,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the skew line lacks %q:\n%s\nfull log:\n%s", want, line, out)
		}
	}
	if strings.Contains(out, "classified infra-failed") {
		t.Errorf("a skewed session was classified infra-failed:\n%s", out)
	}
}

// The unpark pass re-dispatches the skew-parked bead once its RetryAfter has
// passed -- the existing defer/park path, not a new one -- and with no fire_by,
// so the dispatch does not refuse itself as a late re-sling.
func TestASkewParkedBeadIsRedispatchedAfterItsRetryAfter(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	skewSession(t, rt, skewOutput("unset"))
	now := time.Now()
	f.deps.Now = func() time.Time { return now }
	// This attempt was itself a re-sling, long enough ago that its window has
	// ended: its stamps must not ride into the park.
	rec, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	rec.ReslungAt = now.Add(-time.Hour)
	rec.ReslingFireBy = reslingFireBy(rec.ReslungAt)
	if err := f.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	f.sweep(t)

	now = now.Add(skewRetryAfter - time.Second)
	f.sweep(t)
	if n := len(f.orders.orders()); n != 0 {
		t.Fatalf("%d dispatches fired before the retry-after", n)
	}
	now = now.Add(2 * time.Second)
	f.sweep(t)
	fired := f.orders.orders()
	if len(fired) != 1 || fired[0].Order != dispatchOrderName || fired[0].Vars["bead_anchor"] != f.rec.BeadAnchor {
		t.Fatalf("orders = %+v, want one gonk-dispatch for %s", fired, f.rec.BeadAnchor)
	}
	if v, ok := fired[0].Vars[fireByVar]; ok {
		t.Fatalf("the unpark carried %s=%s; a stale fire-by would make the dispatch refuse itself", fireByVar, v)
	}
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v across the park and the unpark, want none", reqs)
	}
}

// THE EXIT CODE ALONE IS NOT PROOF: jobs mode passes opencode's rc through,
// and an opencode that exits 8 is an ordinary failure, judged on its batch.
// Only the entrypoint's refusal line makes it skew.
func TestAnExit8WithoutTheContractRefusalIsJudgedAsUsual(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "done"})
	skewSession(t, rt, rejectedToolThenBatch)

	f.sweep(t)
	if got := f.oneOutcome(t); got != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %q, want success (judged on the batch); log:\n%s", got, f.log.String())
	}
	if strings.Contains(f.log.String(), "IMAGE SKEW") {
		t.Fatalf("an opencode exit 8 was taken for image skew:\n%s", f.log.String())
	}
}

// A skew exit whose output cannot be read, INSIDE the reservation, is neither
// judged nor parked: next tick. (Judging it would charge a skewed session;
// parking it without the proof would hide a real failure.)
func TestASkewExitWithUnreadableOutputWaitsForTheNextTick(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	h := skewSession(t, rt, skewOutput("2"))
	rt.SetOutputErr(h, errors.New("apiserver: log stream reset"))

	f.sweep(t)
	if reqs := f.fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none", reqs)
	}
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateRunning || got.SessionID != testSession {
		t.Fatalf("record = %s/%q, want it left running on its session", got.State, got.SessionID)
	}
	if live := rt.live(t); len(live) != 1 {
		t.Fatalf("live = %v, want the session left alone", live)
	}
}

// A park that loses its compare-and-swap leaves the newer record -- and its
// session -- alone.
func TestASkewParkThatLosesItsRaceStopsNothing(t *testing.T) {
	rt := newTestRuntime()
	f := newJobsSweepFixture(t, rt, &fakeOutcomeMeter{outcomeNext: "retry"})
	skewSession(t, rt, skewOutput("2"))
	// Another writer lands between this pass's read and its park.
	store := &raceOnceStore{Store: f.store, onPutIf: func() {
		cur, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
		cur.Rung = "moved-by-someone-else"
		_ = f.store.Put(context.Background(), cur)
	}}
	f.deps.Store = store

	f.sweep(t)
	got, _, _ := f.store.Get(context.Background(), f.rec.BeadAnchor)
	if got.State != beadstore.StateRunning || got.Rung != "moved-by-someone-else" {
		t.Fatalf("record = %+v, want the newer writer's record untouched", got)
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("%d Stops after a lost park; the session belongs to the newer record", n)
	}
}

// raceOnceStore runs onPutIf once, just before the first PutIf.
type raceOnceStore struct {
	beadstore.Store
	onPutIf func()
}

func (s *raceOnceStore) PutIf(ctx context.Context, rec beadstore.Record, ifRevision string) (string, error) {
	if s.onPutIf != nil {
		hook := s.onPutIf
		s.onPutIf = nil
		hook()
	}
	return s.Store.PutIf(ctx, rec, ifRevision)
}

// ---- THE ATTEMPT PROOF, against the REAL meter --------------------------------
//
// The attempt number is meter's (rung.Decide: len(prior attempts)+1), so the
// only honest proof that skew is "not an attempt" runs the real meter service
// (internal/meter/service over its in-memory store, behind its real HTTP mux)
// through a skew, the park, the unpark's re-dispatch and a successful run --
// and then reads meter's own attempt history.

const skewMeterOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local]
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
meter: { max_spend_staleness: 5m, reservation_ttl: 60m }
`

type realMeter struct {
	api   *meterAPI
	store *meterstore.Memory
	key   meterapi.KeyRef
}

func newRealMeter(t *testing.T) *realMeter {
	t.Helper()
	cfg, err := opercfg.Load([]byte(skewMeterOperatorYAML))
	if err != nil {
		t.Fatalf("operator config: %v", err)
	}
	st := meterstore.NewMemory()
	admin := litellm.NewFake()
	admin.Now = time.Now()
	svc := service.New(cfg, st, admin, admin, keysink.NewMemory(), time.Now)
	ctx := context.Background()
	resp, status, err := svc.Register(ctx, meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 42, Rig: "group-repo",
		GonkYML: "version: 1\nenabled: true\nactions: { triage: true }\nladder: [qwen-local]\nbudget: { monthly_cost_usd: 0 }\n",
	})
	if err != nil || status != 200 || resp.State != meterapi.StateActive {
		t.Fatalf("register: status=%d err=%v resp=%+v", status, err, resp)
	}
	if err := svc.SyncSpend(ctx); err != nil {
		t.Fatalf("spend sync: %v", err)
	}
	mux, err := service.NewMux(svc, "tok", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &realMeter{api: newMeterAPI(srv.URL, "tok"), store: st, key: resp.KeyRef}
}

func TestASkewFailureThenASuccessIsOneAttemptOnTheRealMeter(t *testing.T) {
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
			Now: func() time.Time { return now }, SpendPollInterval: time.Millisecond, SpendDeadline: 10 * time.Millisecond,
		}); code != 0 {
			t.Fatalf("sweep exit = %d; log:\n%s", code, log.String())
		}
	}

	// 1. First dispatch: meter decides attempt 1; the session is refused for skew.
	dispatch()
	if starts := rt.Starts(); len(starts) != 1 || starts[0].Name != name || starts[0].Env[envAttempt] != "1" {
		t.Fatalf("first start = %+v, want %s at attempt 1", starts, name)
	}
	code := runtime.ExitSessionContractMismatch
	rt.SetOutput(runtime.Handle(name), []byte(skewOutput("2")))
	rt.SetStatus(runtime.Handle(name), runtime.Status{Phase: runtime.PhaseFailed, Reason: "BackoffLimitExceeded",
		StartedAt: now, FinishedAt: now, ExitCode: &code})

	// 2. The sweep parks it. Meter has recorded NO attempt.
	sweep()
	if prior, _ := m.store.Attempts(ctx, "group/repo", args.BeadAnchor); len(prior) != 0 {
		t.Fatalf("meter recorded %+v after a skew failure; skew must not be an attempt", prior)
	}

	// 3. Past the retry-after, the unpark fires gonk-dispatch; run it.
	now = now.Add(skewRetryAfter + time.Second)
	sweep()
	if fired := orders.orders(); len(fired) != 1 {
		t.Fatalf("orders = %+v, want the unpark's one dispatch; log:\n%s", fired, log.String())
	}
	dispatch()

	// 4. The retry is attempt 1 AGAIN, under the same name, in a fresh session.
	starts := rt.Starts()
	if len(starts) != 2 || starts[1].Name != name || starts[1].Env[envAttempt] != "1" {
		t.Fatalf("second start = %+v, want %s at attempt 1 again", starts, name)
	}
	if st, _ := rt.Status(ctx, runtime.Handle(name)); st.ExitCode != nil || st.Phase.Finished() {
		t.Fatalf("the retry adopted the refused session (status %+v); it must be a fresh one", st)
	}

	// 5. This time the images agree and the agent answers.
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
		t.Fatalf("meter attempts = %+v, want exactly [attempt 1: success] -- skew then success is attempt 1 -> attempt 1, not 2", prior)
	}
	rec, _, _ := store.Get(ctx, args.BeadAnchor)
	if rec.State != beadstore.StateDone || rec.Attempt != 1 {
		t.Fatalf("record = %s attempt %d, want done at attempt 1", rec.State, rec.Attempt)
	}
}
