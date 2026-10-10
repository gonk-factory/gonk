package main

// gonk-2usw part DISPATCH: the dispatch half. Every test starts from an order
// the SWEEP fired for an eligible run (pfWorld), reads its vars exactly as
// main.go does, and runs runDispatch against a fake meter, the fake session
// runtime and a FAKE of the seam to part AGENT (the fix session's preparer).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// fakePreparer is the test double of the seam: it records what it was asked to
// prepare and answers with a fixed session.
type fakePreparer struct {
	calls []pipefixPrepareInput
	err   error
}

const (
	pfPrompt     = "FIX PROMPT (a fake of part AGENT's)"
	pfContextSHA = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
)

func (f *fakePreparer) prepare(_ context.Context, _ dispatchDeps, in pipefixPrepareInput) (pipefixSession, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return pipefixSession{}, f.err
	}
	return pipefixSession{Prompt: pfPrompt, CheckoutURL: "http://rig.invalid/fetch/" + in.Session,
		CheckoutSHA: in.Fix.PipelineFix.BaseSHA, ContextSHA256: pfContextSHA}, nil
}

// pfDispatch is one gonk-dispatch run for a fix order.
type pfDispatch struct {
	code int
	rt   *testRuntime
	fm   *fakeMeter
	prep *fakePreparer
	logs *bytes.Buffer
}

func pfRunDecision() meterapi.DecideResponse {
	d := runDecision("m")
	d.Rung, d.ReservationID = "r2", "rsv-fix-1"
	return d
}

// dispatchFix runs gonk-dispatch for vars. prep nil means "no preparer wired"
// (production today).
func (w *pfWorld) dispatchFix(vars map[string]string, dec meterapi.DecideResponse, prep *fakePreparer) *pfDispatch {
	w.t.Helper()
	pd := &pfDispatch{rt: newTestRuntime(), fm: &fakeMeter{resp: dec}, prep: prep, logs: &bytes.Buffer{}}
	deps := dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(pd.fm.server(w.t)), Runtime: pd.rt, Store: w.store,
		Forge: promptTestForge(), Pipelines: w.r.gl.Client(), BotUsername: "gonk", PackDir: w.pack,
		Args:  dispatchArgsFrom(func(k string) string { return vars[k] }),
		Clock: &fakeClock{now: w.now.Add(time.Second)}, Log: testLogger(pd.logs),
	}
	if prep != nil {
		deps.PrepareFix = prep.prepare
	}
	pd.code = runDispatch(context.Background(), deps)
	return pd
}

// firedFix runs the look and returns the one order it fired.
func (w *pfWorld) firedFix() map[string]string {
	w.t.Helper()
	log := w.look()
	fired := w.orders.orders()
	if len(fired) != 1 {
		w.t.Fatalf("the look fired %d order(s), want 1:\n%s", len(fired), log)
	}
	return fired[0].Vars
}

// THE RUN PATH: one session, named for the fix's base pipeline, started with
// what the preparer returned; the preparer called ONCE, with the envelope the
// sweep decided; the record running with those decisions intact; and only
// then the `proposing` note on the diagnosis landing, linked to the pipeline.
func TestFixDispatchStartsOneSessionThroughThePreparer(t *testing.T) {
	for _, mr := range []bool{false, true} {
		name := "default-branch"
		if mr {
			name = "merge-request"
		}
		t.Run(name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{mr: mr})
			vars := w.firedFix()
			decided, _ := w.fixRecord()
			prep := &fakePreparer{}
			pd := w.dispatchFix(vars, pfRunDecision(), prep)
			if pd.code != 0 {
				t.Fatalf("exit %d\n%s", pd.code, pd.logs)
			}

			// Meter was asked, for the FIX anchor and the fix trigger.
			if pd.fm.calls != 1 {
				t.Fatalf("meter /decide calls = %d, want 1", pd.fm.calls)
			}
			for _, want := range []string{`"trigger":"pipeline-fix"`, `"bead_id":"` + w.fixAnchor() + `"`, `"session_key":"` + decided.SessionKey + `"`} {
				if !strings.Contains(string(pd.fm.lastBody), want) {
					t.Errorf("the decide request lacks %s: %s", want, pd.fm.lastBody)
				}
			}

			// The preparer: once, with the controller's decisions.
			if len(prep.calls) != 1 {
				t.Fatalf("the preparer was called %d times, want exactly once", len(prep.calls))
			}
			in := prep.calls[0]
			wantSession := runtime.Name(pipefixAgent, w.r.pid, runtime.KindPipeline, 900, 1)
			if in.Session != wantSession || in.Rung != "r2" || in.Fix.Attempt != 1 {
				t.Errorf("prepare input: session %q rung %q attempt %d, want %q r2 1", in.Session, in.Rung, in.Fix.Attempt, wantSession)
			}
			if !reflect.DeepEqual(in.Fix.PipelineFix, decided.PipelineFix) {
				t.Errorf("the preparer was handed another fix than the sweep decided:\n got %+v\nwant %+v", in.Fix.PipelineFix, decided.PipelineFix)
			}
			if !reflect.DeepEqual(in.Fix.PipelineFix.Envelope, []string{"docs", "source"}) || len(in.Fix.PipelineFix.Lift) != 0 ||
				!reflect.DeepEqual(in.Fix.PipelineFix.Files, []string{"calc/sum.py"}) || in.Fix.PipelineFix.BaseSHA != plSHA {
				t.Errorf("envelope %v lift %v files %v base %s", in.Fix.PipelineFix.Envelope, in.Fix.PipelineFix.Lift,
					in.Fix.PipelineFix.Files, in.Fix.PipelineFix.BaseSHA)
			}
			if in.Run.BeadAnchor != w.r.rec.BeadAnchor || in.Run.PipelineDiagnosis.Class != "code" ||
				!reflect.DeepEqual(in.Run.PipelineDiagnosis.Files, []string{"calc/sum.py"}) || len(in.Run.PipelineJobs) == 0 {
				t.Errorf("the preparer was not handed the diagnosed run: %+v", in.Run.PipelineDiagnosis)
			}

			// The session: exactly one, the pipefix agent, what the preparer returned.
			starts := pd.rt.Starts()
			if len(starts) != 1 {
				t.Fatalf("%d sessions started, want 1", len(starts))
			}
			s := starts[0]
			if s.Name != wantSession || s.Agent != pipefixAgent || string(s.Prompt) != pfPrompt ||
				s.RigURL != "http://rig.invalid/fetch/"+wantSession {
				t.Errorf("session %q agent %q prompt %q rig %q", s.Name, s.Agent, s.Prompt, s.RigURL)
			}
			if s.Name == runtime.Name(pipelineAgent, w.r.pid, runtime.KindPipeline, 900, 1) {
				t.Error("the fix session has the diagnosis session's name")
			}

			// The record: running, the decisions intact, the context hash kept.
			rec, _ := w.fixRecord()
			want := decided.PipelineFix
			want.ContextSHA256 = pfContextSHA
			if rec.State != beadstore.StateRunning || rec.SessionID != wantSession || rec.CheckoutSHA != plSHA ||
				rec.Rung != "r2" || rec.ReservationID != "rsv-fix-1" || rec.Trigger != atags.TriggerPipelineFix {
				t.Fatalf("running record: %+v", rec)
			}
			if !reflect.DeepEqual(rec.PipelineFix, want) {
				t.Fatalf("the running record's fix:\n got %+v\nwant %+v", rec.PipelineFix, want)
			}
			if rec.PipelineID != 900 || rec.PipelineSHA != plSHA || rec.PipelineTarget != w.r.target.String() ||
				rec.PipelineFingerprint != w.r.rec.PipelineFingerprint || rec.PipelineLastSuccess != 880 ||
				rec.PipelineProjectURL != decided.PipelineProjectURL {
				t.Fatalf("the running record lost the run identity: %+v", rec)
			}
			if rec.PipelineRunOpen || !rec.ReslingFireBy.IsZero() || !rec.ReslungAt.IsZero() {
				t.Fatalf("the running fix record is an open run, or still carries the fire window: %+v", rec)
			}

			// The note: proposing, on the diagnosis landing, beside the run note.
			st, body, ok := w.fixNote()
			if !ok || st != string(pipefix.StateProposing) {
				t.Fatalf("fix note state %q ok=%v\n%s", st, ok, pd.logs)
			}
			for _, want := range []string{"preparing a fix merge request", "attempt 1", "model `r2`",
				"[pipeline 900](" + w.r.proj.WebURL + "/-/pipelines/900)", "`" + plSHA[:12] + "`"} {
				if !strings.Contains(body, want) {
					t.Errorf("the proposing note lacks %q:\n%s", want, body)
				}
			}
			if runState := w.runNoteState(); runState != pipelinectx.RunStateDiagnosed {
				t.Errorf("the run note moved to %q; the fix note must not touch it", runState)
			}
			if hasString(w.landingLabels(), labelNeedsHuman) {
				t.Error("a proposing fix put gonk::needs-human on the landing")
			}

			// The sweep's next look: in flight. No second order.
			requireReason(t, w.look(), pipefix.E9FixInFlight)
			if n := len(w.orders.orders()); n != 1 {
				t.Fatalf("%d orders after the session started, want 1", n)
			}
		})
	}
}

// runNoteState is the state of the run's own note: the bot note carrying a RUN
// marker (the fix note is a second note on the same landing).
func (w *pfWorld) runNoteState() string {
	for _, n := range w.landingNotes() {
		if st := pipelinectx.RunMarkerStates(n.Body, w.r.rec.BeadAnchor); len(st) > 0 {
			return st[len(st)-1]
		}
	}
	return ""
}

// EVERYTHING BELOW IS REFUSED BEFORE METER IS ASKED: no reservation, no
// session, no preparer call, no GitLab write.
func TestFixDispatchRefusalsHappenBeforeMeter(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange may change the order's vars, the store, and whether a
		// preparer is wired.
		arrange func(w *pfWorld, vars map[string]string) (prep *fakePreparer)
		code    int
		log     string
		// released: the fix record must be back to "not dispatched" (no state,
		// no fire window), so the sweep's next look decides again.
		released bool
	}{
		{name: "no preparer is wired (production until part AGENT)", code: 2, log: "routes to no agent",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer { return nil }},
		{name: "the anchor is not the fix anchor of the order's run identity", code: 2, log: "run identity does not match",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				vars["pipeline_fingerprint"] = "ffffffffffffffff"
				return &fakePreparer{}
			}},
		{name: "a RUN anchor under the pipeline-fix trigger", code: 2, log: "run identity does not match",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				vars["bead_anchor"] = w.r.rec.BeadAnchor
				return &fakePreparer{}
			}},
		{name: "another fix's session key", code: 2, log: "run identity does not match",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				vars["session_key"] = "gonk-1-pf-000000000000"
				return &fakePreparer{}
			}},
		{name: "no fix record: an order nobody decided", code: 2, log: "recorded no fix",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				w.store = beadstore.NewMemory()
				if err := w.store.Put(context.Background(), w.r.rec); err != nil {
					w.t.Fatal(err)
				}
				return &fakePreparer{}
			}},
		{name: "a record under the fix anchor that is not a fix record", code: 2, log: "recorded no fix",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				if err := w.store.Put(context.Background(), beadstore.Record{BeadAnchor: w.fixAnchor(), Trigger: atags.TriggerPipelineFix}); err != nil {
					w.t.Fatal(err)
				}
				return &fakePreparer{}
			}},
		{name: "the pack has no fix agent", code: 2, log: "the pack has no usable fix agent",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				w.pack = t.TempDir()
				return &fakePreparer{}
			}},
		{name: "the fix already has a recorded state", code: 0, log: "already has a recorded state",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				rec, _ := w.fixRecord()
				rec.PipelineFix.State = "needs-scope"
				if err := w.store.Put(context.Background(), rec); err != nil {
					w.t.Fatal(err)
				}
				return &fakePreparer{}
			}},
		{name: "the order names another pipeline than the fix was decided against", code: 0, log: "names another pipeline",
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				rec, _ := w.fixRecord()
				rec.PipelineFix.BasePipelineID = 901
				if err := w.store.Put(context.Background(), rec); err != nil {
					w.t.Fatal(err)
				}
				return &fakePreparer{}
			}},
		{name: "the run has closed since", code: 0, log: "the diagnosed run is closed", released: true,
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				w.setRun(func(r *beadstore.Record) { r.PipelineRunOpen = false })
				return &fakePreparer{}
			}},
		{name: "the run has a newer failing pipeline since", code: 0, log: "a newer failing pipeline", released: true,
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				w.setRun(func(r *beadstore.Record) { r.PipelineLatestFailed = 901 })
				return &fakePreparer{}
			}},
		{name: "the run's record is gone", code: 0, log: "has no record", released: true,
			arrange: func(w *pfWorld, vars map[string]string) *fakePreparer {
				fix, _ := w.fixRecord()
				w.store = beadstore.NewMemory()
				if err := w.store.Put(context.Background(), fix); err != nil {
					w.t.Fatal(err)
				}
				return &fakePreparer{}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			vars := w.firedFix()
			prep := tc.arrange(w, vars)
			writesBefore := len(writes(w.r.gl))
			pd := w.dispatchFix(vars, pfRunDecision(), prep)
			if pd.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", pd.code, tc.code, pd.logs)
			}
			if !strings.Contains(pd.logs.String(), tc.log) {
				t.Fatalf("the refusal does not say %q:\n%s", tc.log, pd.logs)
			}
			if pd.fm.calls != 0 {
				t.Fatalf("meter was asked %d time(s) for an order that must be refused first", pd.fm.calls)
			}
			if n := len(pd.rt.Starts()); n != 0 {
				t.Fatalf("%d session(s) started", n)
			}
			if prep != nil && len(prep.calls) != 0 {
				t.Fatalf("the preparer was called %d time(s)", len(prep.calls))
			}
			if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
				t.Fatalf("a refused fix order wrote to GitLab: %v", writes(w.r.gl)[writesBefore:])
			}
			if tc.released {
				rec, ok := w.fixRecord()
				if !ok || rec.State != "" || !rec.ReslingFireBy.IsZero() || !rec.ReslungAt.IsZero() || !rec.PipelineFix.Recorded() {
					t.Fatalf("the fix was not released: ok=%v %+v", ok, rec)
				}
				if st, stale, known := fixStateOf(rec, w.now); st != "" || !stale || !known {
					t.Fatalf("a released fix reads as (%q, stale=%v, known=%v); the next look would not decide again", st, stale, known)
				}
			}
		})
	}
}

// A release never takes a record a dispatch is acting on.
func TestAStaleFixDispatchDoesNotReleaseARunningRecord(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	if pd := w.dispatchFix(vars, pfRunDecision(), &fakePreparer{}); pd.code != 0 {
		t.Fatalf("setup: exit %d\n%s", pd.code, pd.logs)
	}
	running, _ := w.fixRecord()
	w.setRun(func(r *beadstore.Record) { r.PipelineRunOpen = false })
	pd := w.dispatchFix(vars, pfRunDecision(), &fakePreparer{})
	if pd.code != 0 || pd.fm.calls != 0 {
		t.Fatalf("exit %d, meter calls %d\n%s", pd.code, pd.fm.calls, pd.logs)
	}
	after, _ := w.fixRecord()
	if after.Revision != running.Revision || after.State != beadstore.StateRunning {
		t.Fatalf("a late duplicate dispatch rewrote a running fix record: %+v", after)
	}
}

// DENY: the fix record is needs-human (fix state `denied`), the landing says
// so and asks for a person. No session, no preparer.
func TestAFixMeterDeniesIsNotedOnTheLanding(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	prep := &fakePreparer{}
	pd := w.dispatchFix(vars, meterapi.DecideResponse{Decision: meterapi.DecisionDeny, Attempt: 3,
		Reason: meterapi.ReasonLadderExhausted, Detail: "no rung left"}, prep)
	if pd.code != 0 || len(pd.rt.Starts()) != 0 || len(prep.calls) != 0 {
		t.Fatalf("exit %d starts %d prepares %d\n%s", pd.code, len(pd.rt.Starts()), len(prep.calls), pd.logs)
	}
	rec, _ := w.fixRecord()
	if rec.State != beadstore.StateNeedsHuman || !rec.PipelineFix.Recorded() {
		t.Fatalf("record %+v", rec)
	}
	if st, _, known := fixStateOf(rec, w.now); st != pipefix.StateDenied || !known {
		t.Fatalf("a denied fix reads as %q", st)
	}
	st, body, ok := w.fixNote()
	if !ok || st != string(pipefix.StateDenied) || !strings.Contains(body, "A human is needed") || !strings.Contains(body, "no rung left") {
		t.Fatalf("fix note %q ok=%v:\n%s", st, ok, body)
	}
	if !hasString(w.landingLabels(), labelNeedsHuman) {
		t.Fatal("no gonk::needs-human on the landing of a denied fix")
	}
	if w.runNoteState() != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the RUN note moved to %q: a denied fix must not rewrite the diagnosis's note", w.runNoteState())
	}
	// Terminal: never proposed again.
	requireReason(t, w.look(), pipefix.E9FixTerminal)
	if n := len(w.orders.orders()); n != 1 {
		t.Fatalf("%d orders after a deny, want 1", n)
	}
}

// DEFER: parked with the decisions intact, nothing written to GitLab; the
// sweep's unpark re-fires the same identity, and the look meanwhile says in
// flight.
func TestAFixMeterDefersIsParkedAndUnparkedWithItsIdentity(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	decided, _ := w.fixRecord()
	writesBefore := len(writes(w.r.gl))
	retry := w.now.Add(30 * time.Minute)
	pd := w.dispatchFix(vars, meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Attempt: 1,
		Reason: "quiet_hours", RetryAfter: retry}, &fakePreparer{})
	if pd.code != 0 || len(pd.rt.Starts()) != 0 {
		t.Fatalf("exit %d\n%s", pd.code, pd.logs)
	}
	rec, _ := w.fixRecord()
	if rec.State != beadstore.StateParked || !rec.RetryAfter.Equal(retry) || !reflect.DeepEqual(rec.PipelineFix, decided.PipelineFix) {
		t.Fatalf("parked record %+v", rec)
	}
	if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
		t.Fatalf("a deferred fix wrote to GitLab: %v", writes(w.r.gl)[writesBefore:])
	}
	requireReason(t, w.look(), pipefix.E9FixInFlight)

	// The unpark is the sweep's ordinary refire of a parked record.
	orders := &fakeOrders{}
	if !refire(context.Background(), sweepDeps{Orders: orders, Log: testLogger(&bytes.Buffer{})}, rec) {
		t.Fatal("refire refused")
	}
	got := orders.orders()[0].Vars
	want := map[string]string{}
	for k, v := range vars {
		want[k] = v
	}
	delete(want, fireByVar) // an unpark carries no start deadline
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the unpark's order:\n got %v\nwant %v", got, want)
	}
	if err := checkPipelineFixIdentity(dispatchArgsFrom(func(k string) string { return got[k] })); err != nil {
		t.Fatalf("the unpark's order does not carry the fix identity: %v", err)
	}
}

// A PREPARER THAT FAILS starts no session and writes no note; the record goes
// back to what the sweep wrote, so the fire window decides the retry.
func TestAFailedPrepareStartsNothingAndRestoresTheRecord(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	decided, _ := w.fixRecord()
	writesBefore := len(writes(w.r.gl))
	prep := &fakePreparer{err: errors.New("no checkout could be granted")}
	pd := w.dispatchFix(vars, pfRunDecision(), prep)
	if pd.code != 1 || len(pd.rt.Starts()) != 0 || len(prep.calls) != 1 {
		t.Fatalf("exit %d starts %d prepares %d\n%s", pd.code, len(pd.rt.Starts()), len(prep.calls), pd.logs)
	}
	rec, _ := w.fixRecord()
	if rec.State != "" || rec.SessionID != "" || !reflect.DeepEqual(rec.PipelineFix, decided.PipelineFix) ||
		!rec.ReslingFireBy.Equal(decided.ReslingFireBy) {
		t.Fatalf("the record was not restored: %+v", rec)
	}
	if n := len(writes(w.r.gl)) - writesBefore; n != 0 {
		t.Fatalf("a dispatch that started nothing wrote to GitLab: %v", writes(w.r.gl)[writesBefore:])
	}
	if _, _, ok := w.fixNote(); ok {
		t.Fatal("a proposing note was written for a session that never started")
	}
}

// A dispatch that starts after its fire_by deadline does nothing at all.
func TestALateFixDispatchDoesNothing(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	before, _ := w.fixRecord()
	w.now = before.ReslingFireBy.Add(time.Minute)
	prep := &fakePreparer{}
	pd := w.dispatchFix(vars, pfRunDecision(), prep)
	after, _ := w.fixRecord()
	if pd.code != 0 || pd.fm.calls != 0 || len(prep.calls) != 0 || after.Revision != before.Revision {
		t.Fatalf("exit %d meter %d prepares %d\n%s", pd.code, pd.fm.calls, len(prep.calls), pd.logs)
	}
}

// THE FIX NOTE NEVER MOVES BACKWARDS, never touches a human's note, and is
// never invented without a diagnosis note to sit under.
func TestTheFixNoteIsEditedInPlaceAndNeverBackwards(t *testing.T) {
	ctx := context.Background()
	w := newPFWorld(t, plOpts{})
	l, err := newPipelineLander(ctx, w.r.gl.Client(), testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	fix := w.fixAnchor()
	write := func(state, text string) (bool, error) {
		changed, _, err := l.writeFixNote(ctx, w.r.rec, fix, fixNote{State: state, Text: text})
		return changed, err
	}
	// A human QUOTES a fix marker: their note is never the fix note.
	marker, _ := pipelinectx.FixMarker(fix, "fixed")
	w.r.gl.AddNote(w.r.pid, 1, glab.User{ID: rtHumanID, Username: "human"}, "look at this\n\n"+marker, false)

	if changed, err := write(pipelinectx.FixNoteNotEligible, "no fix: reasons"); err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	if changed, err := write(pipelinectx.FixNoteNotEligible, "no fix: reasons"); err != nil || changed {
		t.Fatalf("an identical write wrote again: changed=%v err=%v", changed, err)
	}
	if changed, err := write("proposing", "preparing"); err != nil || !changed {
		t.Fatalf("not-eligible -> proposing: changed=%v err=%v", changed, err)
	}
	st, body, _ := w.fixNote()
	if st != "proposing" || !strings.Contains(body, "preparing") {
		t.Fatalf("the note was not edited in place: %q\n%s", st, body)
	}
	bot := 0
	for _, n := range w.landingNotes() {
		if n.Author.ID == plBotID {
			bot++
		} else if !strings.Contains(n.Body, "look at this") {
			t.Fatalf("the human's note was edited: %s", n.Body)
		}
	}
	if bot != 2 { // the run note and ONE fix note
		t.Fatalf("%d bot notes on the landing, want 2", bot)
	}
	// A dispatch-time writer cannot claim a later state for itself...
	if changed, err := write("verifying", "!7 opened"); !errors.Is(err, errFixNoteAdvanced) || changed {
		t.Fatalf("proposing -> verifying by a dispatch-time write: changed=%v err=%v, want errFixNoteAdvanced", changed, err)
	}
	// ...the apply moves it on; a dispatch-time write must not move it back.
	if changed, _, err := l.writeFixNote(ctx, w.r.rec, fix, fixNote{State: "verifying", Text: "!7 opened", Apply: true}); err != nil || !changed {
		t.Fatalf("proposing -> verifying: changed=%v err=%v", changed, err)
	}
	for _, back := range []string{"proposing", pipelinectx.FixNoteNotEligible, "denied", "needs-scope"} {
		if changed, err := write(back, "late"); !errors.Is(err, errFixNoteAdvanced) || changed {
			t.Fatalf("verifying -> %s: changed=%v err=%v, want errFixNoteAdvanced", back, changed, err)
		}
	}
	if st, _, _ := w.fixNote(); st != "verifying" {
		t.Fatalf("the note went backwards to %q", st)
	}
	// Marker-shaped text is refused, whoever built it.
	for _, text := range []string{"x <!-- gonk:pipeline-run:a state=resolved -->", "see gonk:pipeline-fix:whatever", "an html <!-- comment"} {
		if _, err := write("fixed", text); !errors.Is(err, errFixNoteForges) {
			t.Fatalf("text %q: err=%v, want errFixNoteForges", text, err)
		}
	}
	// No diagnosis note, no fix note.
	bare := newPLRun(t, plOpts{})
	lb, err := newPipelineLander(ctx, bare.gl.Client(), testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	bfix, _ := pipelinectx.FixAnchor(bare.rec.BeadAnchor)
	if changed, _, err := lb.writeFixNote(ctx, bare.rec, bfix, fixNote{State: "proposing", Text: "x"}); !errors.Is(err, errFixNoteNoLanding) || changed {
		t.Fatalf("a fix note for a run with no diagnosis note: changed=%v err=%v", changed, err)
	}
	if n := len(writes(bare.gl)); n != 0 {
		t.Fatalf("a fix note with no landing wrote to GitLab %d time(s)", n)
	}
	// The same for an MR target, where there is no tracking issue to be missing:
	// the run's own note is what must exist.
	bareMR := newPLRun(t, plOpts{mr: true})
	lm, err := newPipelineLander(ctx, bareMR.gl.Client(), testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	mfix, _ := pipelinectx.FixAnchor(bareMR.rec.BeadAnchor)
	if changed, _, err := lm.writeFixNote(ctx, bareMR.rec, mfix, fixNote{State: "proposing", Text: "x"}); !errors.Is(err, errFixNoteNoLanding) || changed {
		t.Fatalf("a fix note on an MR with no diagnosis note: changed=%v err=%v", changed, err)
	}
	if n := len(writes(bareMR.gl)); n != 0 {
		t.Fatalf("a fix note with no landing wrote to the MR %d time(s)", n)
	}
}

// A FIX WHOSE MERGE REQUEST IS OPEN IS LEFT ALONE BY A SWEEP THAT HAS NO EXIT
// CHECK: no outcome is reported, no session stopped, nothing re-slung -- and
// past its reservation it says so. It is never judged AS A SESSION, with or
// without the exit check: that routing (sweepRunning -> sweepRunningFix) is
// what this test pins, by running the sweep as a build without part VERIFY's
// judge. (Before part APPLY this guard covered every running fix record; a
// fix SESSION is now judged by the apply, and what is left is the merge
// request it opened.)
func TestTheSweepDoesNotJudgeARunningFixWithoutAJudge(t *testing.T) {
	judge := pipefixJudge
	pipefixJudge = nil
	t.Cleanup(func() { pipefixJudge = judge })
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	prep := &fakePreparer{}
	pd := w.dispatchFix(vars, pfRunDecision(), prep)
	if pd.code != 0 {
		t.Fatalf("setup: exit %d\n%s", pd.code, pd.logs)
	}
	session := pd.rt.Starts()[0].Name
	pd.rt.SetOutput(runtime.Handle(session), []byte("```gonk-effects\n{\"verdict\":\"fix\",\"effects\":[]}\n```\n"))
	pd.rt.SetPhase(runtime.Handle(session), runtime.PhaseSucceeded, "")
	// As the apply leaves it: still running, its merge request open.
	opened, _ := w.fixRecord()
	opened.PipelineFix.State, opened.PipelineFix.MRIID = string(pipefix.StateVerifying), 7
	if err := w.store.Put(context.Background(), opened); err != nil {
		t.Fatal(err)
	}
	before, _ := w.fixRecord()
	om := &fakeOutcomeMeter{outcomeNext: "done"}
	var log bytes.Buffer
	// The store stamps the record's write time from the real clock, and with
	// no reservation deadline on the decision the record is bounded by that
	// plus the default TTL: one tick inside it, one well past it.
	for _, at := range []time.Time{time.Now().Add(time.Minute), time.Now().Add(48 * time.Hour)} {
		now := at
		if code := runSweep(context.Background(), sweepDeps{
			Meter: meterClient(om.server(t)), Runtime: pd.rt, Orders: w.orders, Store: w.store,
			GL: w.r.gl.Client(), Pipelines: w.r.gl.Client(), BotUsername: "gonk", PackDir: w.pack,
			Log: testLogger(&log), Now: func() time.Time { return now },
			SpendPollInterval: time.Millisecond, SpendDeadline: time.Second,
		}); code != 0 {
			t.Fatalf("sweep exit %d\n%s", code, log.String())
		}
	}
	after, _ := w.fixRecord()
	if after.Revision != before.Revision {
		t.Fatalf("the sweep rewrote a running fix record: %+v", after)
	}
	if n := len(om.requests()); n != 0 {
		t.Fatalf("the sweep reported %d outcome(s) for a fix session it cannot judge", n)
	}
	if n := pd.rt.stopCount(); n != 0 {
		t.Fatalf("the sweep stopped the fix session %d time(s)", n)
	}
	if n := len(w.orders.orders()); n != 1 {
		t.Fatalf("%d orders, want only the original fix order: a fix session must not be re-slung by the triage path", n)
	}
	if !strings.Contains(log.String(), "open past its reservation and nothing in this build judges it") {
		t.Fatalf("the unjudged fix session was not reported:\n%s", log.String())
	}
}

// pfMeter answers /decide with one fixed decision and records every /outcome.
type pfMeter struct {
	dec      meterapi.DecideResponse
	mu       sync.Mutex
	outcomes []meterapi.OutcomeRequest
}

func (m *pfMeter) server(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(meterapi.DecidePath, func(w http.ResponseWriter, r *http.Request) { writeJSONTest(w, m.dec) })
	mux.HandleFunc(meterapi.OutcomePath, func(w http.ResponseWriter, r *http.Request) {
		var req meterapi.OutcomeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.outcomes = append(m.outcomes, req)
		m.mu.Unlock()
		writeJSONTest(w, meterapi.OutcomeResponse{OK: true, Next: "done"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func (m *pfMeter) requests() []meterapi.OutcomeRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]meterapi.OutcomeRequest(nil), m.outcomes...)
}

// dispatchFixRecordingOutcomes is dispatchFix against a meter that also
// records the outcomes it is told.
func (w *pfWorld) dispatchFixRecordingOutcomes(vars map[string]string, dec meterapi.DecideResponse, prep *fakePreparer) (*pfDispatch, *pfMeter) {
	w.t.Helper()
	om := &pfMeter{dec: dec}
	pd := &pfDispatch{rt: newTestRuntime(), prep: prep, logs: &bytes.Buffer{}}
	pd.code = runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(om.server(w.t)), Runtime: pd.rt, Store: w.store,
		Forge: promptTestForge(), Pipelines: w.r.gl.Client(), BotUsername: "gonk", PackDir: w.pack,
		Args:  dispatchArgsFrom(func(k string) string { return vars[k] }),
		Clock: &fakeClock{now: w.now.Add(time.Second)}, Log: testLogger(pd.logs), PrepareFix: prep.prepare,
	})
	return pd, om
}

// pipeline_fix.max_attempts (plan 3.4): meter's ladder would run attempt 3,
// the project allows 2. No session; the record is needs-human; the reservation
// meter minted for the attempt is settled `aborted`, bound to its id; the
// landing asks for a person. At the cap itself (attempt 2 of 2) the fix runs.
func TestAFixPastTheProjectsMaxAttemptsStartsNoSession(t *testing.T) {
	t.Run("attempt 3 of 2: stopped", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		vars := w.firedFix()
		dec := pfRunDecision()
		dec.Attempt = 3
		prep := &fakePreparer{}
		pd, om := w.dispatchFixRecordingOutcomes(vars, dec, prep)
		if pd.code != 0 || len(pd.rt.Starts()) != 0 || len(prep.calls) != 0 {
			t.Fatalf("exit %d starts %d prepares %d\n%s", pd.code, len(pd.rt.Starts()), len(prep.calls), pd.logs)
		}
		rec, _ := w.fixRecord()
		if rec.State != beadstore.StateNeedsHuman || rec.Attempt != 3 {
			t.Fatalf("record %+v", rec)
		}
		got := om.requests()
		if len(got) != 1 || got[0].Outcome != meterapi.OutcomeAborted || got[0].ReservationID != "rsv-fix-1" ||
			got[0].Attempt != 3 || got[0].BeadID != w.fixAnchor() || got[0].Trigger != atags.TriggerPipelineFix {
			t.Fatalf("outcomes reported: %+v, want one `aborted` bound to rsv-fix-1 attempt 3", got)
		}
		st, body, ok := w.fixNote()
		if !ok || st != string(pipefix.StateDenied) || !strings.Contains(body, "pipeline_fix.max_attempts") ||
			!strings.Contains(body, "at most 2 attempt(s)") {
			t.Fatalf("fix note %q ok=%v:\n%s", st, ok, body)
		}
		if !hasString(w.landingLabels(), labelNeedsHuman) {
			t.Fatal("no gonk::needs-human on the landing")
		}
	})
	t.Run("attempt 2 of 2: runs", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		vars := w.firedFix()
		dec := pfRunDecision()
		dec.Attempt = 2
		prep := &fakePreparer{}
		pd, om := w.dispatchFixRecordingOutcomes(vars, dec, prep)
		if pd.code != 0 || len(pd.rt.Starts()) != 1 || len(prep.calls) != 1 || len(om.requests()) != 0 {
			t.Fatalf("exit %d starts %d prepares %d outcomes %d\n%s", pd.code, len(pd.rt.Starts()), len(prep.calls), len(om.requests()), pd.logs)
		}
	})
}

// checkPipelineFixIdentity, row by row: the order of an eligible run passes,
// and each var that does not belong to it is refused.
func TestFixOrderIdentity(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	good := w.firedFix()
	args := func(mutate func(v map[string]string)) dispatchArgs {
		v := map[string]string{}
		for k, x := range good {
			v[k] = x
		}
		mutate(v)
		return dispatchArgsFrom(func(k string) string { return v[k] })
	}
	if err := checkPipelineFixIdentity(args(func(map[string]string) {})); err != nil {
		t.Fatalf("the sweep's own order is refused: %v", err)
	}
	for name, mutate := range map[string]func(v map[string]string){
		"no pipeline id":          func(v map[string]string) { delete(v, "pipeline_id") },
		"pipeline id zero":        func(v map[string]string) { v["pipeline_id"] = "0" },
		"a malformed target":      func(v map[string]string) { v["pipeline_target"] = "branch:main" },
		"another target":          func(v map[string]string) { v["pipeline_target"] = "mr:5" },
		"another last success":    func(v map[string]string) { v["pipeline_last_success"] = "881" },
		"a fix epoch not its own": func(v map[string]string) { v["pipeline_fix_epoch"] = "12" },
		"another project":         func(v map[string]string) { v["project_id"] = "999" },
		"the run's anchor":        func(v map[string]string) { v["bead_anchor"] = w.r.rec.BeadAnchor },
		"the run's session key":   func(v map[string]string) { v["session_key"] = w.r.rec.SessionKey },
	} {
		if err := checkPipelineFixIdentity(args(mutate)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- the seam, wired (gonk-2usw integration of parts AGENT and DISPATCH) --------

// dispatchFixReal is dispatchFix with the PRODUCTION preparer (pipefixPrepare:
// part AGENT's functions behind the seam) and the world's configuration
// source standing in for gonk-meter's project read.
func (w *pfWorld) dispatchFixReal(vars map[string]string, dec meterapi.DecideResponse) *pfDispatch {
	w.t.Helper()
	pd := &pfDispatch{rt: newTestRuntime(), fm: &fakeMeter{resp: dec}, logs: &bytes.Buffer{}}
	deps := dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(pd.fm.server(w.t)), Runtime: pd.rt, Store: w.store,
		Forge: promptTestForge(), Pipelines: w.r.gl.Client(), BotUsername: "gonk", PackDir: w.pack,
		Args:  dispatchArgsFrom(func(k string) string { return vars[k] }),
		Clock: &fakeClock{now: w.now.Add(time.Second)}, Log: testLogger(pd.logs),
		PrepareFix: pipefixPrepare,
		FixConfig: func(context.Context, string) (*meterapi.ProjectResponse, error) {
			w.cfgCalls++
			return w.cfg, w.cfgErr
		},
	}
	pd.code = runDispatch(context.Background(), deps)
	return pd
}

// The production preparer is wired: main.go's seam value is part AGENT's
// implementation, not nil.
func TestTheProductionPreparerIsWired(t *testing.T) {
	if pipefixPrepare == nil {
		t.Fatal("pipefixPrepare is nil: main.go would refuse every pipeline-fix order and run no fix pass")
	}
	if agentForTrigger[atags.TriggerPipelineFix] != pipefixAgent {
		t.Fatalf("agentForTrigger[%q] = %q, want %q", atags.TriggerPipelineFix, agentForTrigger[atags.TriggerPipelineFix], pipefixAgent)
	}
	if artifactKindForTrigger[atags.TriggerPipelineFix] == "" || artifactKindForTrigger[atags.TriggerPipelineFix] == "comment" {
		t.Fatalf("artifactKindForTrigger[%q] = %q: a fix has no issue for the sweep to read or close",
			atags.TriggerPipelineFix, artifactKindForTrigger[atags.TriggerPipelineFix])
	}
	if !needsCheckout[pipefixAgent] {
		t.Fatal("the fix agent gets no checkout")
	}
}

// THE SEAM, END TO END AT DISPATCH: the order the sweep fired, through the
// real preparer. The session is started with part AGENT's prompt; the record
// carries the context the session was shown and its hash; and gate A9, run at
// once with the envelope folded again from that record, accepts it and hands
// back exactly the editable file's blob.
func TestFixDispatchThroughTheRealPreparer(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	vars := w.firedFix()
	pd := w.dispatchFixReal(vars, pfRunDecision())
	if pd.code != 0 {
		t.Fatalf("exit %d\n%s", pd.code, pd.logs)
	}
	starts := pd.rt.Starts()
	if len(starts) != 1 || starts[0].Agent != pipefixAgent {
		t.Fatalf("sessions started: %+v", starts)
	}
	prompt := string(starts[0].Prompt)
	for _, want := range []string{"Propose a fix for a diagnosed GitLab pipeline failure", "== Files you may change ==", `"calc/sum.py"`, plSHA} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the session's prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, pfPrompt) {
		t.Fatal("the session got the test double's prompt")
	}
	rec, _ := w.fixRecord()
	fc := rec.PipelineFix.Context
	if rec.State != beadstore.StateRunning || !fc.Recorded() {
		t.Fatalf("state %q, context recorded %v\n%s", rec.State, fc.Recorded(), pd.logs)
	}
	if rec.PipelineFix.ContextSHA256 == "" || rec.PipelineFix.ContextSHA256 != pipefixContextHash(fc) {
		t.Fatalf("ContextSHA256 %q is not the hash of the recorded context (%q)", rec.PipelineFix.ContextSHA256, pipefixContextHash(fc))
	}
	if fc.BaseSHA != rec.PipelineFix.BaseSHA || fc.PipelineID != rec.PipelineFix.BasePipelineID {
		t.Fatalf("context base %s pipeline %d, fix base %s pipeline %d", fc.BaseSHA, fc.PipelineID, rec.PipelineFix.BaseSHA, rec.PipelineFix.BasePipelineID)
	}
	if len(fc.Files) != 1 || fc.Files[0].Path != "calc/sum.py" || !fc.Files[0].Editable || fc.Files[0].SHA256 == "" {
		t.Fatalf("context files %+v", fc.Files)
	}
	sum := sha256.Sum256([]byte(prompt))
	if fc.PromptSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("the recorded prompt hash is not the hash of the prompt the session was started with")
	}
	// Gate A9, as the apply will run it.
	env, err := refoldPipefixEnvelope(context.Background(), w.r.gl.Client(), w.cfg, w.pack, rec, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("refold: %v", err)
	}
	run, _, _ := w.store.Get(context.Background(), rec.PipelineFix.RunAnchor)
	maxEdits, err := pipefixMaxEdits(w.pack)
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := verifyPipefixContext(context.Background(), w.r.gl.Client(), rec.ProjectID, fc, run.PipelineDiagnosis, env, maxEdits)
	if err != nil {
		t.Fatalf("gate A9 refuses the context dispatch just recorded: %v", err)
	}
	if len(blobs) != 1 || len(blobs["calc/sum.py"]) == 0 {
		t.Fatalf("A9 returned blobs for %d file(s)", len(blobs))
	}
}

// A `pipeline-fix` ORDER FOR A PROJECT THAT HAS NOT OPTED IN IS REFUSED, at
// each layer on its own:
//
//	no fix record      the sweep writes one only for an opted-in project, so
//	                   an order without one is refused BEFORE meter is asked
//	a record exists    (the project opted out after the look, or the record
//	                   was forged) and meter is stale enough to say run: the
//	                   preparer reads the configuration itself and starts
//	                   nothing
func TestAPipelineFixOrderForAnOptedOutProjectIsRefused(t *testing.T) {
	t.Run("no fix record", func(t *testing.T) {
		w := newPFWorld(t, plOpts{})
		w.cfg.Effective.Actions.PipelineFixes = false
		log := w.look()
		requireReason(t, log, pipefix.E1FixesOff)
		if len(w.orders.orders()) != 0 {
			t.Fatal("the sweep fired an order for an opted-out project")
		}
		// The order a different (or older, or hostile) sweep would have sent.
		on := newPFWorld(t, plOpts{})
		vars := on.firedFix()
		before := len(writes(w.r.gl))
		pd := w.dispatchFixReal(vars, pfRunDecision())
		if pd.code != 2 || pd.fm.calls != 0 || len(pd.rt.Starts()) != 0 {
			t.Fatalf("exit %d, %d meter call(s), %d session(s); want a refusal before meter\n%s", pd.code, pd.fm.calls, len(pd.rt.Starts()), pd.logs)
		}
		if n := len(writes(w.r.gl)) - before; n != 0 {
			t.Fatalf("%d GitLab write(s) for a refused order", n)
		}
	})
	for _, tc := range []struct {
		name string
		off  func(c *meterapi.ProjectResponse)
	}{
		{"pipeline_fixes off", func(c *meterapi.ProjectResponse) { c.Effective.Actions.PipelineFixes = false }},
		{"pipelines off", func(c *meterapi.ProjectResponse) { c.Effective.Actions.Pipelines = false }},
		{"no effective configuration", func(c *meterapi.ProjectResponse) { c.Effective = nil }},
	} {
		t.Run("a record exists, "+tc.name, func(t *testing.T) {
			w := newPFWorld(t, plOpts{})
			vars := w.firedFix() // decided while opted in
			tc.off(w.cfg)
			before := len(writes(w.r.gl))
			pd := w.dispatchFixReal(vars, pfRunDecision())
			if pd.code == 0 || len(pd.rt.Starts()) != 0 {
				t.Fatalf("exit %d, %d session(s) started for an opted-out project\n%s", pd.code, len(pd.rt.Starts()), pd.logs)
			}
			if !strings.Contains(pd.logs.String(), errPipefixNotOptedIn.Error()) {
				t.Fatalf("the refusal does not say why:\n%s", pd.logs)
			}
			if n := len(writes(w.r.gl)) - before; n != 0 {
				t.Fatalf("%d GitLab write(s) for an opted-out project", n)
			}
			rec, _ := w.fixRecord()
			if rec.State == beadstore.StateRunning || rec.PipelineFix.Context.Recorded() {
				t.Fatalf("the fix record moved on: state %q", rec.State)
			}
		})
	}
}

// The envelope is folded AGAIN at dispatch and must agree with what the sweep
// recorded: a record whose envelope, lift, caps or files say more than the
// fold gives is not dispatched from.
func TestTheEnvelopeIsFoldedAgainAndMustAgreeWithTheRecord(t *testing.T) {
	w := newPFWorld(t, plOpts{})
	w.firedFix()
	rec, _ := w.fixRecord()
	fold := func(r beadstore.Record) error {
		_, err := refoldPipefixEnvelope(context.Background(), w.r.gl.Client(), w.cfg, w.pack, r, testLogger(&bytes.Buffer{}))
		return err
	}
	if err := fold(rec); err != nil {
		t.Fatalf("the record the sweep just wrote does not fold: %v", err)
	}
	for name, forge := range map[string]func(r *beadstore.Record){
		"a class added to the envelope": func(r *beadstore.Record) { r.PipelineFix.Envelope = []string{"ci", "docs", "source"} },
		"a lift on the record":          func(r *beadstore.Record) { r.PipelineFix.Lift = []string{"ci"} },
		"a larger line cap":             func(r *beadstore.Record) { r.PipelineFix.MaxLines = 4000 },
		"a larger file cap":             func(r *beadstore.Record) { r.PipelineFix.MaxFiles = 30 },
	} {
		r := rec
		forge(&r)
		if err := fold(r); !errors.Is(err, errPipefixEnvelopeChanged) {
			t.Errorf("%s: err = %v, want errPipefixEnvelopeChanged", name, err)
		}
	}
	// The lift is minted from the project's allow_ci by GrantLift, never from
	// the record: a `configuration` record naming a ci lift folds only when
	// the project (as meter holds it NOW) allows it.
	cfgRec := rec
	cfgRec.PipelineFix.Class, cfgRec.PipelineFix.Envelope, cfgRec.PipelineFix.Lift = "configuration", []string{"ci"}, []string{"ci"}
	if err := fold(cfgRec); !errors.Is(err, errPipefixEnvelopeChanged) {
		t.Errorf("a recorded ci lift without allow_ci: err = %v, want errPipefixEnvelopeChanged", err)
	}
	w.cfg.Effective.PipelineFix.AllowCI = true
	env, err := refoldPipefixEnvelope(context.Background(), w.r.gl.Client(), w.cfg, w.pack, cfgRec, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("with allow_ci the configuration row does not fold: %v", err)
	}
	if !env.Lift.Has(scope.ClassCI) {
		t.Fatal("the refold granted no ci lift although the project allows it")
	}
	// A path that pipeline_fix.paths no longer covers.
	w.cfg.Effective.PipelineFix.AllowCI = false
	w.cfg.Effective.PipelineFix.Paths = []string{"docs/**"}
	if err := fold(rec); !errors.Is(err, errPipefixEnvelopeChanged) {
		t.Errorf("a recorded file outside pipeline_fix.paths: err = %v, want errPipefixEnvelopeChanged", err)
	}
}
