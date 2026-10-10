package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-nrbx: each ROUND (a user interaction that starts new work on an issue
// already holding a FINAL answer) gets a FRESH status note. The earlier answer
// keeps its text and loses only its status marker, so the newest round's note
// is the single LIVE status note. Within one round gonk-l900's edit-in-place is
// unchanged.
//
// THE ROUND RULE: a note is a finished round exactly when it carries the answer
// marker (<!-- gonk:bead:ID -->). A lifecycle write that finds the live note
// (status marker) finished retires it and starts a new note. The rule is read
// from the thread, so it survives restarts, and a re-run of the same round
// finds a non-final note and edits.

const roundBatch1 = "GONK_BATCH_START\n" +
	`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Round one answer."}]}` + "\nGONK_BATCH_END\n"
const roundBatch2 = "GONK_BATCH_START\n" +
	`{"verdict":"reply-only","effects":[{"kind":"comment","body":"Round two answer."}]}` + "\nGONK_BATCH_END\n"

func statusHolders(t *testing.T, forge *noticeForge) []glab.Note {
	t.Helper()
	_, _, _, th := forge.snapshot()
	var out []glab.Note
	for _, n := range th {
		if strings.Contains(n.Body, statusMarker("gk-1a2b")) {
			out = append(out, n)
		}
	}
	return out
}

// A new round's dispatch carries a later attempt number: meter counts attempts
// per task, so round 2 starts at attempt 2, not 1. The round rule must not
// depend on the attempt number.

// round1 runs a whole first round to its answer on forge/store and returns the
// answer note.
func round1(t *testing.T, store beadstore.Store, forge *noticeForge, rt *testRuntime, log *bytes.Buffer) glab.Note {
	t.Helper()
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, log)); code != 0 {
		t.Fatalf("round 1 dispatch exit = %d\n%s", code, log.String())
	}
	finish(rt, 1, roundBatch1)
	lifecycleSweep(t, rt, store, forge, &fakeOutcomeMeter{outcomeNext: "done"}, &fakeOrders{}, log)
	n := onlyNote(t, forge)
	mustContain(t, n.Body, "Round one answer.", answerMarker("gk-1a2b"), statusMarker("gk-1a2b"))
	return n
}

func TestNewRoundStartsAFreshNoteAndRetiresTheAnswersStatusMarker(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	ans1 := round1(t, store, forge, rt, &log)
	text1 := strings.Replace(ans1.Body, "\n\n"+statusMarker("gk-1a2b"), "", 1)

	// Round 2: a mention re-dispatches the same issue.
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(2, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("round 2 dispatch exit = %d\n%s", code, log.String())
	}
	creates, _, _, th := forge.snapshot()
	if creates != 2 || len(th) != 2 {
		t.Fatalf("creates=%d notes=%d, want a SECOND note for round 2", creates, len(th))
	}
	if th[0].ID != ans1.ID || th[0].Body != text1 {
		t.Fatalf("round 1 answer must be unchanged apart from the status marker:\n got: %q\nwant: %q", th[0].Body, text1)
	}
	live := statusHolders(t, forge)
	if len(live) != 1 || live[0].ID != th[1].ID {
		t.Fatalf("want exactly one LIVE status note, the new one: %+v", live)
	}
	mustContain(t, live[0].Body, "I am triaging this issue now (attempt 2")
}

func TestRoundTwoAnswerReplacesRoundTwoNoteOnlyAndReapplyDoesNotCreate(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	ans1 := round1(t, store, forge, rt, &log)
	rt2 := rt
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt2, runMeterAt(2, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("round 2 dispatch exit = %d\n%s", code, log.String())
	}
	_, _, _, th := forge.snapshot()
	note2 := th[1]

	sd := sweepDeps{Runtime: rt2, GL: forge, Apply: forge, BotUsername: "gonk-bot", PackDir: repoPackDir, Log: testLogger(&log)}
	h := rt2.finishSession(t, testSession, roundBatch2)
	rec := baseRunningRecord()
	rec.SessionID = string(h)
	st, _ := rt2.Status(context.Background(), h)
	for pass := 1; pass <= 2; pass++ { // the SAME round's batch, applied twice
		if applied, v, err := applyBrokerBatch(context.Background(), sd, "triage", &rec, st); !applied || v != "" || err != nil {
			t.Fatalf("pass %d: applied=%v v=%q err=%v", pass, applied, v, err)
		}
	}
	creates, _, _, th := forge.snapshot()
	if creates != 2 || len(th) != 2 {
		t.Fatalf("creates=%d notes=%d: re-applying round 2 must not create a note", creates, len(th))
	}
	if th[1].ID != note2.ID {
		t.Fatal("round 2's answer did not replace round 2's note")
	}
	mustContain(t, th[1].Body, "Round two answer.", statusMarker("gk-1a2b"))
	if strings.Contains(th[0].Body, "Round two") || !strings.Contains(th[0].Body, "Round one answer.") || th[0].ID != ans1.ID {
		t.Fatalf("round 1 answer was touched:\n%s", th[0].Body)
	}
	if strings.Contains(th[0].Body, statusMarker("gk-1a2b")) {
		t.Fatal("round 1 answer still carries the status marker")
	}
}

func TestDeferAndDenyInRoundTwoEditRoundTwoNoteOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		fm   *fakeMeter
		want string
	}{
		{"defer", deferMeter(), "cannot answer yet"},
		{"deny", denyMeter(meterapi.ReasonLadderExhausted, "x"), "need a human"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
			var log bytes.Buffer
			ans1 := round1(t, store, forge, rt, &log)
			// A new round that is deferred/denied at dispatch: starts a fresh note.
			if code := runDispatch(context.Background(), noticeDeps(t, tc.fm, store, forge, &log)); code != 0 {
				t.Fatalf("exit = %d\n%s", code, log.String())
			}
			creates, edits, _, th := forge.snapshot()
			if creates != 2 || len(th) != 2 {
				t.Fatalf("creates=%d notes=%d, want the round's first note created fresh", creates, len(th))
			}
			mustContain(t, th[1].Body, tc.want)
			if th[0].ID != ans1.ID || !strings.Contains(th[0].Body, "Round one answer.") || strings.Contains(th[0].Body, statusMarker("gk-1a2b")) {
				t.Fatalf("round 1 answer wrong:\n%s", th[0].Body)
			}
			editsBefore := edits
			// The same round again (a later defer/deny pass): edit, never create.
			if code := runDispatch(context.Background(), noticeDeps(t, tc.fm, store, forge, &log)); code != 0 {
				t.Fatalf("repeat exit = %d\n%s", code, log.String())
			}
			creates2, _, _, th2 := forge.snapshot()
			if creates2 != 2 || len(th2) != 2 || th2[0].Body != th[0].Body {
				t.Fatalf("repeat created or touched notes: creates=%d notes=%d", creates2, len(th2))
			}
			_ = editsBefore
		})
	}
}

// A restart mid-round (a brand new process: new runtime, same thread and store)
// still finds round 2's note, and the sweep's re-sling edits it.
func TestRestartMidRoundTwoStillFindsRoundTwoNote(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	round1(t, store, forge, rt, &log)
	rt2 := rt
	if code := runDispatch(context.Background(), runNoticeDeps(t, rt2, runMeterAt(2, "cheap"), store, forge, &log)); code != 0 {
		t.Fatalf("round 2 exit = %d\n%s", code, log.String())
	}
	_, _, _, th := forge.snapshot()
	note2 := th[1]
	// "Restart": fresh deps; the only state is the thread and the store. Edit via
	// the same lifecycle writers a restarted process would run.
	sd := sweepDeps{GL: forge, Apply: forge, BotUsername: "gonk-bot", Log: testLogger(&log)}
	upsertStatusNote(context.Background(), sd, 42, 3, "gk-1a2b", runningBody(3, "big", "", "gonk-bot"))
	creates, _, _, th := forge.snapshot()
	if creates != 2 || len(th) != 2 || th[1].ID != note2.ID {
		t.Fatalf("creates=%d notes=%d: restarted process must edit round 2's note", creates, len(th))
	}
	mustContain(t, th[1].Body, "attempt 3")
	if strings.Contains(th[0].Body, "attempt 3") {
		t.Fatal("round 1's answer was edited")
	}
}

// Round 2's placeholder never landed (its create failed): round 2's answer
// must be posted as a NEW note, never written over round 1's answer.
func TestRoundTwoAnswerWithNoPlaceholderDoesNotOverwriteRoundOne(t *testing.T) {
	store, forge, rt := beadstore.NewMemory(), newNoticeForge(), newTestRuntime()
	var log bytes.Buffer
	ans1 := round1(t, store, forge, rt, &log)
	sd := sweepDeps{Runtime: rt, GL: forge, Apply: forge, BotUsername: "gonk-bot", PackDir: repoPackDir, Log: testLogger(&log)}
	h := rt.finishSession(t, testSession, roundBatch2)
	rec := baseRunningRecord()
	rec.SessionID = string(h)
	st, _ := rt.Status(context.Background(), h)
	if applied, v, err := applyBrokerBatch(context.Background(), sd, "triage", &rec, st); !applied || v != "" || err != nil {
		t.Fatalf("applied=%v v=%q err=%v", applied, v, err)
	}
	_, _, _, th := forge.snapshot()
	if len(th) != 2 {
		t.Fatalf("notes = %d, want round 2's answer as a second note", len(th))
	}
	if th[0].ID != ans1.ID || !strings.Contains(th[0].Body, "Round one answer.") || strings.Contains(th[0].Body, statusMarker("gk-1a2b")) {
		t.Fatalf("round 1 answer overwritten or still live:\n%s", th[0].Body)
	}
	mustContain(t, th[1].Body, "Round two answer.", statusMarker("gk-1a2b"))
}
