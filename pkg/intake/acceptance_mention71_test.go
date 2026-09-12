package intake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// THE ACCEPTANCE TEST for gonk-pop3.
//
// On 2026-09-12 02:40:32 UTC the owner replied on project 75 issue !71:
//
//	@gonk you are absolutely right choomy, that sort function SHOULD honor the
//	desc parameter. you have permission to fix it.
//
// GitLab delivered it (projects/75/hooks/4/events: note_hooks -> HTTP 200, and
// the hook has note_events: True). Intake logged NOTHING -- its last line for
// project 75 was a checkout grant eight minutes earlier -- so answering "why
// did that mention do nothing?" required querying GitLab's own hook delivery
// log, then reading dispatch.go, and the answer was still only a HYPOTHESIS
// (gonk-ecn) because no log line anywhere confirmed it.
//
// This test replays that exact delivery through the REAL receiver, the REAL
// bounded queue and the REAL dispatcher, and asserts the question is now
// answerable from the records alone. It PRINTS them (go test -v) because the
// point of the bead is the observed output, not the assertion.
//
// It covers BOTH candidate answers, because before this change a reader could
// not tell them apart:
//
//	(a) intake dropped it   -> the record names the reason;
//	(b) intake dispatched it -> the record names the trigger, the bead anchor
//	    and the reservation, which hands the investigation to the next hop by
//	    name. (gonk-gate's half of that handoff is asserted in
//	    cmd/gonk-gate: TestMentionReplyRefusalIsLoggedWithTheTrigger.)
func TestAcceptanceMentionOn75Issue71IsFullyAccountedForInLogs(t *testing.T) {
	body := readFile(t, "testdata/note-75-71-mention.json")

	cases := []struct {
		name string
		// entry is project 75's cache entry at the moment of delivery.
		entry Entry
		meter *fakeDecide
		// what a human reading `kubectl logs deploy/gonk-intake` learns.
		wantDecision string
		wantReason   string
	}{
		{
			name:  "(b) intake DID dispatch: the drop is downstream of intake",
			entry: entry75(nil),
			meter: &fakeDecide{resp: &meterapi.DecideResponse{
				Decision: "run", Rung: "qwen-local", Model: "qwen3-coder", ReservationID: "res-71a",
			}},
			wantDecision: DecisionDispatched,
		},
		{
			name: "(a) intake dropped it: respond_to_mentions is off for this project",
			entry: entry75(func(r *meterapi.ProjectResponse) {
				r.Effective.Triage.RespondToMentions = false
			}),
			meter:        &fakeDecide{},
			wantDecision: DecisionIgnored,
			wantReason:   "mentions_disabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := &recordLog{}
			log := lr.logger()

			// --- the real pipeline, wired the way cmd/gonk-intake wires it ---
			cache := NewCache()
			cache.Put(75, tc.entry)
			disp := &recordingDispatcher{}
			d := &Dispatch{
				Dispatcher: disp, Meter: tc.meter, Labeler: &recordingLabeler{},
				Cache: cache, BotUsername: "gonk", Obs: NopObserver{}, Log: log,
				KickReconcile: func() {},
			}

			events := make(chan *ghook.Event, 256)
			v, err := ghook.NewVerifier(strings.Repeat("s", 48))
			if err != nil {
				t.Fatal(err)
			}
			hook, err := ghook.NewHandler(v, ghook.NewDeduper(time.Hour, 1024), 4242,
				func(e *ghook.Event) bool {
					select {
					case events <- e:
						return true
					default:
						return false
					}
				}, ghook.NopObserver{})
			if err != nil {
				t.Fatal(err)
			}
			hook.Log = log

			// --- the delivery GitLab actually made ---------------------------
			r := httptest.NewRequest(http.MethodPost, "/hook/gitlab", strings.NewReader(body))
			r.Header.Set("X-Gitlab-Event", "Note Hook")
			r.Header.Set("X-Gitlab-Token", strings.Repeat("s", 48))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Gitlab-Event-UUID", "b5f0c3d1-2026-09-12-024032")
			w := httptest.NewRecorder()
			hook.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("receiver answered %d, want the 200 GitLab recorded", w.Code)
			}

			// --- the queue consumer run()' goroutine runs in production ------
			select {
			case ev := <-events:
				d.Handle(context.Background(), ev)
			default:
				t.Fatal("the event never reached the dispatch queue")
			}

			t.Logf("\n--- what `kubectl logs deploy/gonk-intake` now shows for this delivery ---\n%s", lr.text())

			// 1. The delivery is accounted for at the receiver.
			delivery := recordsWithMsg(t, lr, "webhook delivery")
			if len(delivery) != 1 {
				t.Fatalf("got %d receiver records, want exactly 1:\n%s", len(delivery), lr.text())
			}
			if delivery[0]["outcome"] != string(ghook.OutcomeAccepted) {
				t.Errorf("receiver outcome = %v, want accepted", delivery[0]["outcome"])
			}
			for k, want := range map[string]any{"project_id": float64(75), "issue_iid": float64(71), "note_id": float64(4471)} {
				if delivery[0][k] != want {
					t.Errorf("receiver record %q = %v, want %v", k, delivery[0][k], want)
				}
			}

			// 2. The dispatch decision is accounted for, with a reason.
			dec := lr.one(t)
			if dec["decision"] != tc.wantDecision {
				t.Errorf("decision = %v, want %q\n%s", dec["decision"], tc.wantDecision, lr.text())
			}
			if tc.wantReason != "" && dec["reason"] != tc.wantReason {
				t.Errorf("reason = %v, want %q\n%s", dec["reason"], tc.wantReason, lr.text())
			}

			// 3. Both records name the SAME delivery, so they join by grep.
			if delivery[0]["delivery"] != dec["delivery"] {
				t.Errorf("the two records disagree about the delivery id: %v vs %v",
					delivery[0]["delivery"], dec["delivery"])
			}

			// 4. On the dispatched path the record must hand the investigation
			//    on BY NAME: which trigger, which bead, which reservation.
			if tc.wantDecision == DecisionDispatched {
				if dec["trigger"] != "mention-reply" {
					t.Errorf("trigger = %v, want mention-reply\n%s", dec["trigger"], lr.text())
				}
				if dec["bead"] != "gonk:75:issue:71" {
					t.Errorf("bead = %v, want gonk:75:issue:71\n%s", dec["bead"], lr.text())
				}
				if len(disp.orders) != 1 {
					t.Fatalf("fired %d orders, want 1", len(disp.orders))
				}
			}

			// 5. And the comment text itself never reaches the log.
			if strings.Contains(lr.text(), "choomy") {
				t.Errorf("the note body reached the log:\n%s", lr.text())
			}
		})
	}
}

func entry75(mut func(*meterapi.ProjectResponse)) Entry {
	return Entry{
		Project:        glab.Project{ID: 75, PathWithNamespace: "group/sortlib"},
		Classification: Classify(obs(nil), active(mut)),
		LastReconcile:  time.Now(),
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func recordsWithMsg(t *testing.T, lr *recordLog, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(lr.text()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}
