package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-l900: ONE gonk status note per issue, posted the moment gonk decides to
// work on it and edited through the lifecycle -- triaging now, retrying after a
// failed attempt, deferred, denied -- ending REPLACED by the validated triage
// answer. noticeForge (dispatch_deny_notice_test.go) is a stateful thread that
// counts creates and edits separately, so "edited the same note" and "posted a
// second one" cannot be confused.

// GetIssue lets the sweep read the issue's state off the same thread: always
// open here.
func (f *noticeForge) GetIssue(_ context.Context, _, iid int64) (*glab.Issue, error) {
	return &glab.Issue{IID: iid, State: "opened"}, nil
}

func (f *noticeForge) GetRawFile(context.Context, int64, string, string, int64) ([]byte, error) {
	return nil, errors.New("not found")
}

// runNoticeDeps is a dispatch that can succeed and writes to forge's thread.
func runNoticeDeps(t *testing.T, rt *testRuntime, fm *fakeMeter, store beadstore.Store, forge *noticeForge, log *bytes.Buffer) dispatchDeps {
	d := liveDispatchDeps(t, rt, fm, store, log)
	d.Apply, d.GL, d.BotUsername = forge, forge, "gonk-bot"
	return d
}

func runMeterAt(attempt int, rung string) *fakeMeter {
	fm := attemptMeter(attempt)
	fm.resp.Rung = rung
	return fm
}

// lifecycleSweep runs one sweep pass over store against forge's thread.
func lifecycleSweep(t *testing.T, rt *testRuntime, store beadstore.Store, forge *noticeForge, fm *fakeOutcomeMeter, orders *fakeOrders, log *bytes.Buffer) {
	t.Helper()
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: forge, Apply: forge,
		Store: store, BotUsername: "gonk-bot", PackDir: repoPackDir, Log: testLogger(log),
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("sweep exit = %d\n%s", code, log.String())
	}
}

// finish marks the session dispatch started for attempt as finished, with out.
func finish(rt *testRuntime, attempt int, out string) {
	h := runtime.Handle(runtime.Name("triage", 42, runtime.KindIssue, 3, attempt))
	rt.SetOutput(h, []byte(out))
	rt.SetPhase(h, runtime.PhaseSucceeded, "")
}

func onlyNote(t *testing.T, forge *noticeForge) glab.Note {
	t.Helper()
	_, _, _, thread := forge.snapshot()
	if len(thread) != 1 {
		t.Fatalf("thread has %d notes, want exactly one gonk note: %+v", len(thread), thread)
	}
	return thread[0]
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Fatalf("note lacks %q:\n%s", w, body)
		}
	}
}

// Item 1. The note exists BEFORE the session starts: rom#1 showed an empty
// thread for 5+ minutes while two attempts ran.
func TestRunPostsOneTriagingNoteBeforeTheSessionStarts(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	var atStart []glab.Note
	rt.onStart = func(runtime.Spec) error {
		_, _, _, atStart = forge.snapshot()
		return nil
	}
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "qwen3-14b"), store, forge, &log)); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	if len(atStart) != 1 {
		t.Fatalf("when the session started the thread had %d notes, want the triaging note already there", len(atStart))
	}
	creates, edits, _, _ := forge.snapshot()
	if creates != 1 || edits != 0 {
		t.Fatalf("creates=%d edits=%d, want one note created", creates, edits)
	}
	mustContain(t, onlyNote(t, forge).Body,
		"I am triaging this issue now (attempt 1, using model `qwen3-14b`).", statusMarker("gk-1a2b"))
	if len(rt.Starts()) != 1 {
		t.Fatalf("starts = %d, want the session started", len(rt.Starts()))
	}
}

// Ordering: the note reports a record write, so it is never posted before one.
// A reservation whose write errored (unconfirmed) does not count; the note
// then waits for the running record, and with no record write at all there is
// no note.
func TestRunNoteWaitsForARecordWrite(t *testing.T) {
	t.Run("reservation unconfirmed: note after the running record", func(t *testing.T) {
		forge, rt := newNoticeForge(), newTestRuntime()
		store := &hookStore{Memory: beadstore.NewMemory()}
		store.putIf = func(int, beadstore.Record, string) (string, error) { return "", errors.New("store hiccup") }
		var log bytes.Buffer
		var atStart int
		rt.onStart = func(runtime.Spec) error {
			_, _, _, th := forge.snapshot()
			atStart = len(th)
			return nil
		}
		if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log.String())
		}
		if atStart != 0 {
			t.Fatal("note posted before any record write had been confirmed")
		}
		mustContain(t, onlyNote(t, forge).Body, "I am triaging this issue now (attempt 1")
	})
	t.Run("no record write at all: no note", func(t *testing.T) {
		forge, rt := newNoticeForge(), newTestRuntime()
		var log bytes.Buffer
		d := runNoticeDeps(t, rt, runMeterAt(1, "cheap"), denyWriteFailStore{beadstore.NewMemory()}, forge, &log)
		if code := runDispatch(context.Background(), d); code == 0 {
			t.Fatalf("exit = 0 with every record write failing\n%s", log.String())
		}
		if creates, edits, _, _ := forge.snapshot(); creates+edits != 0 {
			t.Fatalf("creates=%d edits=%d: a note reported a record that was never written", creates, edits)
		}
	})
}

// Items 1, 2 and 4 end to end, on ONE thread: attempt 1 runs and gate-fails
// (no batch fence -- rom#1's first attempt), the sweep re-slings with an
// escalation, attempt 2 runs and its batch is applied. The thread holds one
// gonk note throughout, and it ends as the answer.
func TestLifecycleRunGateFailedRetryThenAnswerLeavesOneNote(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	orders := &fakeOrders{}
	var log bytes.Buffer

	// Attempt 1: RUN.
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "qwen3-14b"), store, forge, &log)); code != 0 {
		t.Fatalf("dispatch 1 exit = %d\n%s", code, log.String())
	}
	first := onlyNote(t, forge)

	// Attempt 1 finishes with no fence; tokens were spent => gate-failed.
	finish(rt, 1, "I thought about it but forgot the batch.")
	gateFailed := &fakeOutcomeMeter{
		outcomeNext: "escalate", nextRung: "qwen3-6-35b", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 900, AsOf: time.Now().Add(time.Hour)},
	}
	lifecycleSweep(t, rt, store, forge, gateFailed, orders, &log)
	if reqs := gateFailed.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want gate-failed", reqs)
	}
	if len(orders.names()) != 1 {
		t.Fatalf("re-sling not fired: %v", orders.names())
	}
	resling := onlyNote(t, forge)
	if resling.ID != first.ID {
		t.Fatal("the re-sling posted a new note instead of editing the triaging one")
	}
	mustContain(t, resling.Body,
		"Attempt 1 (model `qwen3-14b`) did not produce a valid answer: its output did not contain a result in the format I require.",
		"I am retrying now with a stronger model (attempt 2, using model `qwen3-6-35b`).",
		statusMarker("gk-1a2b"))
	if strings.Contains(resling.Body, "GONK_BATCH") {
		t.Fatalf("the plain-words reason leaked the machine violation:\n%s", resling.Body)
	}

	// Attempt 2: RUN again, on the stronger rung, carrying attempt 1's account.
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(2, "qwen3-6-35b"), store, forge, &log)); code != 0 {
		t.Fatalf("dispatch 2 exit = %d\n%s", code, log.String())
	}
	running2 := onlyNote(t, forge)
	if running2.ID != first.ID {
		t.Fatal("attempt 2 posted a new note")
	}
	mustContain(t, running2.Body,
		"Attempt 1 (model `qwen3-14b`) did not produce a valid answer",
		"I am triaging this issue now (attempt 2, using model `qwen3-6-35b`).")
	if strings.Contains(running2.Body, "I am retrying now") {
		t.Fatalf("the stale retrying line survived into the running note:\n%s", running2.Body)
	}

	// Attempt 2 produces a valid batch; the sweep applies it.
	finish(rt, 2, "GONK_BATCH_START\n"+
		`{"verdict":"reply-only","effects":[{"kind":"comment","body":"The export endpoint has no timeout handling."},{"kind":"label","add":["gonk::bug"]}]}`+
		"\nGONK_BATCH_END\n")
	done := &fakeOutcomeMeter{outcomeNext: "done"}
	lifecycleSweep(t, rt, store, forge, done, orders, &log)
	if reqs := done.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want success\n%s", reqs, log.String())
	}
	answer := onlyNote(t, forge)
	creates, edits, labels, _ := forge.snapshot()
	if answer.ID != first.ID || creates != 1 || edits != 3 {
		t.Fatalf("creates=%d edits=%d id=%d (first %d): want ONE note, created once and edited at re-sling, attempt 2 and the answer",
			creates, edits, answer.ID, first.ID)
	}
	mustContain(t, answer.Body, "The export endpoint has no timeout handling.",
		"<!-- gonk:bead:gk-1a2b -->", statusMarker("gk-1a2b"))
	for _, stale := range []string{"I am triaging", "did not produce a valid answer"} {
		if strings.Contains(answer.Body, stale) {
			t.Fatalf("the answer still carries status text %q:\n%s", stale, answer.Body)
		}
	}
	if !contains(labels, "gonk::bug") || !contains(labels, "gonk::verdict-reply-only") {
		t.Fatalf("labels = %v, want the batch's and the verdict's as today", labels)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Item 2, infra-failed: the plain sentence says it was not a bad answer, and
// that the retry stays on the same model.
func TestInfraFailedReslingEditsTheNoteAndSaysSameModel(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	h := runtime.Handle(runtime.Name("triage", 42, runtime.KindIssue, 3, 1))
	rt.SetPhase(h, runtime.PhaseFailed, reasonDeadlineExceeded)
	lifecycleSweep(t, rt, store, forge, &fakeOutcomeMeter{outcomeNext: "retry"}, &fakeOrders{}, &log)
	creates, edits, _, _ := forge.snapshot()
	if creates != 1 || edits != 1 {
		t.Fatalf("creates=%d edits=%d, want the one note edited", creates, edits)
	}
	mustContain(t, onlyNote(t, forge).Body,
		"Attempt 1 (model `cheap`) did not finish because of an infrastructure problem, not a bad answer: it ran out of the time allowed for one attempt.",
		"I am retrying now with the same model (attempt 2).")
}

// A gate failure that meter answers with `retry` is a FLOOR RETRY (gonk-tios
// R2): the next attempt runs the same rung again. The note must say it was a
// bad answer (not infrastructure) AND that the model is the same one -- never
// "a stronger model", which would be a false statement to the reader.
func TestGateFailedFloorRetryReslingSaysSameModelNotStronger(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	orders := &fakeOrders{}
	var log bytes.Buffer
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "qwen3-6-35b"), store, forge, &log)); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	finish(rt, 1, "I thought about it but forgot the batch.")
	floorRetry := &fakeOutcomeMeter{
		outcomeNext: "retry", nextRung: "qwen3-6-35b", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 900, AsOf: time.Now().Add(time.Hour)},
	}
	lifecycleSweep(t, rt, store, forge, floorRetry, orders, &log)
	if reqs := floorRetry.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want gate-failed", reqs)
	}
	if len(orders.names()) != 1 {
		t.Fatalf("a floor retry must re-sling exactly once: %v", orders.names())
	}
	body := onlyNote(t, forge).Body
	mustContain(t, body,
		"Attempt 1 (model `qwen3-6-35b`) did not produce a valid answer: its output did not contain a result in the format I require.",
		"I am retrying now with the same model (attempt 2).")
	for _, wrong := range []string{"stronger model", "infrastructure problem"} {
		if strings.Contains(body, wrong) {
			t.Fatalf("a floor retry's note says %q:\n%s", wrong, body)
		}
	}
}

// Item 4, idempotency: a sweep that applies the same batch again (its outcome
// POST failed, its record write was stale) edits the answer to the same text
// -- or writes nothing -- and never posts a second copy. This needs the
// answer to KEEP the status marker.
func TestReapplyingTheBatchDoesNotPostASecondAnswer(t *testing.T) {
	forge, rt := newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	sd := sweepDeps{Runtime: rt, GL: forge, Apply: forge, BotUsername: "gonk-bot", PackDir: repoPackDir, Log: testLogger(&log)}
	upsertStatusNote(context.Background(), sd, 42, 3, "gk-1a2b", runningBody(1, "cheap", "", "gonk-bot"))
	h := rt.finishSession(t, testSession, "GONK_BATCH_START\n"+
		`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Answer."}]}`+"\nGONK_BATCH_END\n")
	rec := baseRunningRecord()
	rec.SessionID = string(h)
	st, _ := rt.Status(context.Background(), h)
	for pass := 1; pass <= 2; pass++ {
		applied, violation, err := applyBrokerBatch(context.Background(), sd, "triage", &rec, st)
		if !applied || violation != "" || err != nil {
			t.Fatalf("pass %d: applied=%v violation=%q err=%v", pass, applied, violation, err)
		}
	}
	creates, _, _, _ := forge.snapshot()
	if creates != 1 {
		t.Fatalf("creates = %d, want only the original status note -- the answer must REPLACE it, twice over", creates)
	}
	mustContain(t, onlyNote(t, forge).Body, "Answer.", statusMarker("gk-1a2b"))
}

// Item 3: the defer note says why in plain words and when, as an absolute UTC
// time.
func TestDeferNoteNamesTheReasonAndTheAbsoluteRevisitTime(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	var log bytes.Buffer
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonMonthlyTokensExhausted, Attempt: 1,
		Detail: "rung \"cheap\" needs ~12000 tokens, 500 remain this window", RetryAfter: reset,
	}}
	if code := runDispatch(context.Background(), noticeDeps(t, fm, store, forge, &log)); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	mustContain(t, onlyNote(t, forge).Body,
		"This project has used its token allowance for the current budget period.",
		"I will look at this again after 2026-11-01 00:00:00 UTC.")
}

// Every defer reason pkg/rung.Decide can return gets a plain sentence. Before
// gonk-l900 three of the five (the money defers) got none.
func TestEveryDeferReasonMeterEmitsIsExplained(t *testing.T) {
	for _, r := range []string{
		meterapi.ReasonKeyMissing, meterapi.ReasonQuietHours, meterapi.ReasonSpendStale,
		meterapi.ReasonMonthlyTokensExhausted, meterapi.ReasonMonthlyCostExhausted,
	} {
		if deferReasonText(r) == "" {
			t.Errorf("defer reason %q has no plain-words explanation", r)
		}
	}
}

// Item 5: a deny after retries edits the same note.
func TestDenyAfterARunEditsTheSameNote(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("run exit = %d\n%s", code, log.String())
	}
	first := onlyNote(t, forge)
	if code := runDispatch(context.Background(), noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "2 rungs, 2 gate failures"), store, forge, &log)); code != 0 {
		t.Fatalf("deny exit = %d\n%s", code, log.String())
	}
	creates, edits, _, _ := forge.snapshot()
	n := onlyNote(t, forge)
	if creates != 1 || edits != 1 || n.ID != first.ID {
		t.Fatalf("creates=%d edits=%d: want the triaging note edited into the deny", creates, edits)
	}
	mustContain(t, n.Body, "need a human", "Every model rung")
}

// GitLab failing at each step is a WARN and changes no outcome.
func TestGitLabFailureAtEachStepWarnsAndChangesNoOutcome(t *testing.T) {
	gitlabDown := errors.New("gitlab 503")

	t.Run("run: create fails", func(t *testing.T) {
		store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
		forge.failAll = gitlabDown
		var log bytes.Buffer
		if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, log.String())
		}
		if rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3"); rec.State != beadstore.StateRunning || len(rt.Starts()) != 1 {
			t.Fatalf("state=%q starts=%d, want the session started and recorded", rec.State, len(rt.Starts()))
		}
		mustContain(t, log.String(), "level=WARN", "status note create failed", "gitlab 503")
	})

	t.Run("run: the thread cannot be read -- nothing posted blind", func(t *testing.T) {
		store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
		upsertStatusNote(context.Background(), sweepDeps{Apply: forge, GL: forge, BotUsername: "gonk-bot", Log: testLogger(&bytes.Buffer{})},
			42, 3, "gk-1a2b", "an earlier status")
		forge.failList = gitlabDown
		var log bytes.Buffer
		if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, log.String())
		}
		if creates, edits, _, _ := forge.snapshot(); creates != 1 || edits != 0 {
			t.Fatalf("creates=%d edits=%d: an unreadable thread must not get a second note", creates, edits)
		}
		mustContain(t, log.String(), "level=WARN", "status note not written", "gitlab 503")
	})

	t.Run("re-sling: edit fails", func(t *testing.T) {
		store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
		var log bytes.Buffer
		if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log.String())
		}
		forge.failUpdate = gitlabDown
		finish(rt, 1, "no batch")
		orders := &fakeOrders{}
		fm := &fakeOutcomeMeter{outcomeNext: "escalate", spendSynced: true,
			sessionCost: meterapi.SessionCostResponse{TotalTokens: 5, AsOf: time.Now().Add(time.Hour)}}
		lifecycleSweep(t, rt, store, forge, fm, orders, &log)
		if len(orders.names()) != 1 {
			t.Fatalf("the re-sling must fire whatever GitLab did: %v", orders.names())
		}
		mustContain(t, log.String(), "level=WARN", "status note update failed", "gitlab 503")
	})

	t.Run("answer: edit fails -- the answer still lands, outcome success", func(t *testing.T) {
		store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
		var log bytes.Buffer
		if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log.String())
		}
		forge.failUpdate = gitlabDown
		finish(rt, 1, "GONK_BATCH_START\n"+`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Answer."}]}`+"\nGONK_BATCH_END\n")
		fm := &fakeOutcomeMeter{outcomeNext: "done"}
		lifecycleSweep(t, rt, store, forge, fm, &fakeOrders{}, &log)
		if reqs := fm.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
			t.Fatalf("outcome = %+v, want success", reqs)
		}
		mustContain(t, log.String(), "level=WARN", "could not edit gonk's status note into the answer")
	})

	t.Run("defer: create fails", func(t *testing.T) {
		store, forge := beadstore.NewMemory(), newNoticeForge()
		forge.failAll = gitlabDown
		var log bytes.Buffer
		if code := runDispatch(context.Background(), noticeDeps(t, deferMeter(), store, forge, &log)); code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, log.String())
		}
		if rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3"); rec.State != beadstore.StateParked {
			t.Fatalf("state = %q, want parked", rec.State)
		}
		mustContain(t, log.String(), "level=WARN", "status note create failed")
	})
}

// A repeated RUN for the same attempt (a re-dispatch after a failed one) is
// a no-op on the thread, not a second note and not a pointless edit.
func TestRepeatedRunNoteIsANoOp(t *testing.T) {
	forge := newNoticeForge()
	sd := sweepDeps{Apply: forge, GL: forge, BotUsername: "gonk-bot", Log: testLogger(&bytes.Buffer{})}
	for range 3 {
		upsertStatusNote(context.Background(), sd, 42, 3, "gk-1a2b", runningBody(1, "cheap", "", "gonk-bot"))
	}
	if creates, edits, _, _ := forge.snapshot(); creates != 1 || edits != 0 {
		t.Fatalf("creates=%d edits=%d, want one create and nothing after", creates, edits)
	}
}

// The plain-words mapping never echoes the machine violation.
func TestAttemptFailureTextIsPlain(t *testing.T) {
	for _, tc := range []struct{ outcome, violation, cause, want string }{
		{meterapi.OutcomeGateFailed, "no GONK_BATCH_START/END fence in session output", "", "did not contain a result in the format I require"},
		{meterapi.OutcomeGateFailed, "unparseable batch: invalid character '}' at offset 17", "", "malformed"},
		{meterapi.OutcomeGateFailed, "shape: comment effects: want 1, got 2", "", "required parts"},
		{meterapi.OutcomeGateFailed, "comment: line 2 is a quick action", "", "posting rule"},
		{meterapi.OutcomeInfraFailed, "", "session_deadline_exceeded", "ran out of the time"},
		{meterapi.OutcomeInfraFailed, "", "no_model_tokens: no artifact and meter recorded no model spend", "never answered"},
		{meterapi.OutcomeInfraFailed, "", "artifact_unknown: issue read failed: 500", "could not read its result"},
	} {
		got := attemptFailureText(tc.outcome, tc.violation, tc.cause)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%q/%q: got %q, want it to contain %q", tc.violation, tc.cause, got, tc.want)
		}
		for _, leak := range []string{"offset", "GONK_BATCH", "no_model_tokens", "500", "want 1"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q/%q: plain text leaks %q: %q", tc.violation, tc.cause, leak, got)
			}
		}
	}
}

// Ordering, sweep side: the re-sling note reports the re-sling RECORD. When
// that write is refused (the record moved while the pass judged it), there is
// no re-sling, and the note must not claim one.
func TestReslingNoteNotPostedWhenTheReslingWriteIsRefused(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	finish(rt, 1, "no batch")
	fm := &fakeOutcomeMeter{outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 5, AsOf: time.Now().Add(time.Hour)}}
	// Another writer moves the record while the sweep is inside /outcome.
	fm.onOutcome = func() {
		rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
		_ = store.Put(context.Background(), rec)
	}
	orders := &fakeOrders{}
	lifecycleSweep(t, rt, store, forge, fm, orders, &log)
	if len(orders.names()) != 0 {
		t.Fatalf("precondition: a refused re-sling write must not fire: %v", orders.names())
	}
	if creates, edits, _, _ := forge.snapshot(); creates != 1 || edits != 0 {
		t.Fatalf("creates=%d edits=%d: the note claimed a re-sling that was never recorded", creates, edits)
	}
}

// Item 4's backstop: an applied batch with no comment for the issue turns the
// status note into the verdict in plain words. (A valid triage batch always
// carries exactly one comment for its own issue -- the shape and target gates
// enforce it -- so this is asserted on the body, not through a sweep.)
func TestResultSummaryBodySaysTheVerdictPlainly(t *testing.T) {
	for v, want := range map[string]string{
		"reply-only": "no code change is needed", "code-change": "needs a code change", "close": "can be closed",
	} {
		got := resultSummaryBody(effectsVerdict(v), "gonk-bot")
		if !strings.Contains(got, "I have finished triaging this issue.") || !strings.Contains(got, want) {
			t.Errorf("verdict %q: body = %q, want it to contain %q", v, got, want)
		}
	}
}

func effectsVerdict(v string) effects.Verdict { return effects.Verdict(v) }
