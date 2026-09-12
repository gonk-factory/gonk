package intake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// recordLog captures intake's real production log shape: slog JSON on stderr
// (cmd/gonk-intake/main.go). Asserting on the parsed JSON, not on a substring,
// is what makes these tests about the RECORD rather than about a message.
type recordLog struct{ b strings.Builder }

func (l *recordLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *recordLog) text() string { return l.b.String() }

// decisions returns only the `intake decision` records, ignoring the extra
// diagnostic lines Handle also writes on some paths.
func (l *recordLog) decisions(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if m["msg"] == "intake decision" {
			out = append(out, m)
		}
	}
	return out
}

// one asserts the single decision record and returns it.
func (l *recordLog) one(t *testing.T) map[string]any {
	t.Helper()
	recs := l.decisions(t)
	if len(recs) != 1 {
		t.Fatalf("got %d `intake decision` records, want exactly 1:\n%s", len(recs), l.text())
	}
	return recs[0]
}

// entryWith builds a cache entry from a mutated meter project response, so a
// test can put a project into a state that is NOT dispatchable.
func entryWith(mutObs func(*Observation), mutMeter func(*meterapi.ProjectResponse)) Entry {
	return Entry{
		Project:        glab.Project{ID: 42, PathWithNamespace: "group/repo"},
		Classification: Classify(obs(mutObs), active(mutMeter)),
		LastReconcile:  time.Now(),
	}
}

func dispatchWith(t *testing.T, e Entry, m DecideClient, disp Dispatcher, lr *recordLog) *Dispatch {
	t.Helper()
	cache := NewCache()
	cache.Put(42, e)
	return &Dispatch{
		Dispatcher: disp, Meter: m, Labeler: &recordingLabeler{}, Cache: cache,
		BotUsername: "gonk", Obs: NopObserver{}, Log: lr.logger(),
		KickReconcile: func() {},
	}
}

func withDelivery(ev *ghook.Event, id string) *ghook.Event {
	ev.DeliveryID = id
	return ev
}

// failingDispatcher makes the fire_error branch reachable.
type failingDispatcher struct{}

func (failingDispatcher) FireOrder(context.Context, OrderRequest) error {
	return errors.New("gas city said no")
}

// THE PROPERTY. Every path through Handle produces EXACTLY ONE decision record,
// and every non-dispatch carries a reason. An HTTP 200 with no explanation is
// the bug this exists to kill: on 2026-09-12 a note event for project 75 issue
// !71 was accepted and answered 200, and intake wrote nothing at all.
//
// The table is deliberately exhaustive over Handle's return paths, INCLUDING
// the success path -- which used to be the most silent of all (a Prometheus
// counter and nothing else), so `kubectl logs` could not tell "intake dropped
// it" from "intake fired and the next hop ate it".
func TestEveryHandlePathProducesExactlyOneDecisionRecord(t *testing.T) {
	run := func(resp *meterapi.DecideResponse, err error) *fakeDecide {
		return &fakeDecide{resp: resp, err: err}
	}

	cases := []struct {
		name         string
		entry        *Entry // nil -> do not populate the cache (unknown_project)
		meter        *fakeDecide
		dispatcher   Dispatcher
		event        *ghook.Event
		wantDecision string
		wantReason   string
	}{
		{
			name: "dispatched (issue triage)", entry: ptr(validEntry()),
			meter: run(&meterapi.DecideResponse{Decision: "run", Rung: "qwen-local", Model: "qwen3", ReservationID: "r-1"}, nil),
			event: issueEvent("open"), wantDecision: DecisionDispatched,
		},
		{
			name: "dispatched (mention reply)", entry: ptr(validEntry()),
			meter: run(&meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}, nil),
			event: noteEvent("@gonk please look again"), wantDecision: DecisionDispatched,
		},
		{
			name: "merge request events are reconcile signals", entry: ptr(validEntry()),
			meter: run(nil, nil), event: mrEvent(),
			wantDecision: DecisionIgnored, wantReason: "mr_event",
		},
		{
			name: "unknown project", entry: nil,
			meter: run(nil, nil), event: issueEvent("open"),
			wantDecision: DecisionIgnored, wantReason: "unknown_project",
		},
		{
			name: "stale cache entry", entry: func() *Entry {
				e := validEntry()
				e.LastReconcile = time.Now().Add(-90 * time.Minute)
				return &e
			}(),
			meter: run(nil, nil), event: issueEvent("open"),
			wantDecision: DecisionIgnored, wantReason: "stale-config",
		},
		{
			name: "issue update is not a trigger", entry: ptr(validEntry()),
			meter: run(nil, nil), event: issueEvent("update"),
			wantDecision: DecisionIgnored, wantReason: "not_a_trigger",
		},
		{
			name: "a comment that does not mention the bot", entry: ptr(validEntry()),
			meter: run(nil, nil), event: noteEvent("just chatting"),
			wantDecision: DecisionIgnored, wantReason: "no_mention",
		},
		{
			name: "mentions disabled in .gonk.yml", entry: ptr(entryWith(nil, func(r *meterapi.ProjectResponse) {
				r.Effective.Triage.RespondToMentions = false
			})),
			meter: run(nil, nil), event: noteEvent("@gonk have another go"),
			wantDecision: DecisionIgnored, wantReason: "mentions_disabled",
		},
		{
			name: "blocked project", entry: func() *Entry {
				e := validEntry()
				e.Blocked = true
				return &e
			}(),
			meter: run(nil, nil), event: issueEvent("open"),
			wantDecision: DecisionIgnored, wantReason: "blocked_project",
		},
		{
			name: "meter unreachable", entry: ptr(validEntry()),
			meter: run(nil, errors.New("connection refused")), event: issueEvent("open"),
			wantDecision: DecisionError, wantReason: "decide_error",
		},
		{
			name: "meter deferred", entry: ptr(validEntry()),
			meter: run(&meterapi.DecideResponse{Decision: "defer", Reason: "quiet_hours"}, nil),
			event: issueEvent("open"), wantDecision: DecisionDeferred, wantReason: "quiet_hours",
		},
		{
			name: "meter denied", entry: ptr(validEntry()),
			meter: run(&meterapi.DecideResponse{Decision: "deny", Reason: "ladder_exhausted"}, nil),
			event: issueEvent("open"), wantDecision: DecisionDenied, wantReason: "ladder_exhausted",
		},
		{
			name: "meter answered something this binary does not know", entry: ptr(validEntry()),
			meter: run(&meterapi.DecideResponse{Decision: "maybe"}, nil),
			event: issueEvent("open"), wantDecision: DecisionError, wantReason: "decide_unknown",
		},
		{
			name: "firing the order failed", entry: ptr(validEntry()),
			meter:      run(&meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}, nil),
			dispatcher: failingDispatcher{}, event: issueEvent("open"),
			wantDecision: DecisionError, wantReason: "fire_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := &recordLog{}
			var disp Dispatcher = &recordingDispatcher{}
			if tc.dispatcher != nil {
				disp = tc.dispatcher
			}
			e := validEntry()
			if tc.entry != nil {
				e = *tc.entry
			}
			d := dispatchWith(t, e, tc.meter, disp, lr)
			if tc.entry == nil {
				d.Cache = NewCache() // the project is genuinely unknown
			}

			d.Handle(context.Background(), withDelivery(tc.event, "delivery-"+tc.name))

			rec := lr.one(t)
			if rec["decision"] != tc.wantDecision {
				t.Errorf("decision = %v, want %q\n%s", rec["decision"], tc.wantDecision, lr.text())
			}
			if tc.wantReason != "" && rec["reason"] != tc.wantReason {
				t.Errorf("reason = %v, want %q\n%s", rec["reason"], tc.wantReason, lr.text())
			}
			// THE INVARIANT, asserted on every row: anything that is not a
			// dispatch must say why, and must never fall back to the bug marker.
			if tc.wantDecision != DecisionDispatched {
				r, _ := rec["reason"].(string)
				if r == "" {
					t.Errorf("a %s record carries no reason at all\n%s", tc.wantDecision, lr.text())
				}
				if r == noReason {
					t.Errorf("a %s record reached emit() with no reason set (%s)\n%s", tc.wantDecision, noReason, lr.text())
				}
			}
			// Always correlatable: the delivery id and the ids that identify
			// the artifact must be on every record, whatever was decided.
			if rec["delivery"] != "delivery-"+tc.name {
				t.Errorf("delivery = %v, want the id the receiver assigned\n%s", rec["delivery"], lr.text())
			}
			if rec["project_id"] == nil {
				t.Errorf("record carries no project_id\n%s", lr.text())
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A dispatch record must name the bead anchor -- that anchor is what gonk-gate,
// gonk-sweep and gonk-meter all key on, so it is the one string that joins the
// four processes a single work item passes through.
func TestDispatchRecordCarriesTheBeadAnchorAndTheNextHopsInputs(t *testing.T) {
	lr := &recordLog{}
	d := dispatchWith(t, validEntry(),
		&fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "sonnet", Model: "claude", ReservationID: "res-9"}},
		&recordingDispatcher{}, lr)

	d.Handle(context.Background(), withDelivery(noteEvent("@gonk go on then"), "d-1"))

	rec := lr.one(t)
	for k, want := range map[string]any{
		"decision":    DecisionDispatched,
		"bead":        BeadAnchor(42, 3),
		"session_key": SessionKey(42, 3),
		"trigger":     "mention-reply",
		"rung":        "sonnet",
		"model":       "claude",
		"reservation": "res-9",
		"note_id":     float64(5),
	} {
		if rec[k] != want {
			t.Errorf("record[%q] = %v, want %v\n%s", k, rec[k], want, lr.text())
		}
	}
}

// Comment and issue text is untrusted user input. It must never reach the log.
func TestDecisionRecordNeverCarriesUntrustedText(t *testing.T) {
	const secretish = "ATTACKER-CONTROLLED-BODY-TEXT"
	for _, ev := range []*ghook.Event{
		noteEvent("@gonk " + secretish),
		noteEvent(secretish), // the no_mention path
		func() *ghook.Event {
			e := issueEvent("open")
			e.Issue.Title = secretish
			e.Issue.Description = secretish
			return e
		}(),
	} {
		lr := &recordLog{}
		d := dispatchWith(t, validEntry(),
			&fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}},
			&recordingDispatcher{}, lr)
		d.Handle(context.Background(), withDelivery(ev, "d-x"))
		if strings.Contains(lr.text(), secretish) {
			t.Errorf("untrusted event text reached the log:\n%s", lr.text())
		}
	}
}

// emit must not be able to write a silent ignore. If a future branch forgets to
// call ignore(), the record says so in a string an operator can grep for --
// rather than emitting reason="" which reads like a legitimate answer.
func TestAnUnsetDecisionRendersAsABugNotAsACleanIgnore(t *testing.T) {
	lr := &recordLog{}
	r := newDecisionRecord(noteEvent("hi"))
	r.emit(lr.logger())

	rec := lr.one(t)
	if rec["decision"] != DecisionError || rec["reason"] != noReason {
		t.Errorf("an un-set record emitted decision=%v reason=%v, want %q/%q\n%s",
			rec["decision"], rec["reason"], DecisionError, noReason, lr.text())
	}
}

// FireScaffold is the OTHER order-firing path, and it was silent on every
// branch too -- which is how a project sits at `pending` forever with nothing
// saying why (gonk-bgx). It is not webhook-driven, so it has no delivery id;
// the bead anchor is its join key.
func TestFireScaffoldProducesExactlyOneDecisionRecord(t *testing.T) {
	cases := []struct {
		name         string
		entry        Entry
		meter        *fakeDecide
		dispatcher   Dispatcher
		wantDecision string
		wantReason   string
	}{
		{
			name: "dispatched", entry: validEntry(),
			meter:        &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}},
			wantDecision: DecisionDispatched,
		},
		{
			name: "stale entry", entry: func() Entry {
				e := validEntry()
				e.LastReconcile = time.Now().Add(-90 * time.Minute)
				return e
			}(),
			meter:        &fakeDecide{},
			wantDecision: DecisionIgnored, wantReason: "stale-config",
		},
		{
			name: "meter deferred", entry: validEntry(),
			meter:        &fakeDecide{resp: &meterapi.DecideResponse{Decision: "defer", Reason: "quiet_hours"}},
			wantDecision: DecisionDeferred, wantReason: "quiet_hours",
		},
		{
			name: "meter denied", entry: validEntry(),
			meter:        &fakeDecide{resp: &meterapi.DecideResponse{Decision: "deny", Reason: "scaffold_disabled"}},
			wantDecision: DecisionDenied, wantReason: "scaffold_disabled",
		},
		{
			name: "meter unreachable", entry: validEntry(),
			meter:        &fakeDecide{err: errors.New("connection refused")},
			wantDecision: DecisionError, wantReason: "decide_error",
		},
		{
			name: "firing the order failed", entry: validEntry(),
			meter:        &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}},
			dispatcher:   failingDispatcher{},
			wantDecision: DecisionError, wantReason: "fire_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := &recordLog{}
			var disp Dispatcher = &recordingDispatcher{}
			if tc.dispatcher != nil {
				disp = tc.dispatcher
			}
			d := dispatchWith(t, tc.entry, tc.meter, disp, lr)

			_ = d.FireScaffold(context.Background(), tc.entry)

			rec := lr.one(t)
			if rec["decision"] != tc.wantDecision {
				t.Errorf("decision = %v, want %q\n%s", rec["decision"], tc.wantDecision, lr.text())
			}
			if tc.wantReason != "" && rec["reason"] != tc.wantReason {
				t.Errorf("reason = %v, want %q\n%s", rec["reason"], tc.wantReason, lr.text())
			}
			if rec["bead"] != "gonk:42:scaffold" {
				t.Errorf("bead = %v, want gonk:42:scaffold (the join key)\n%s", rec["bead"], lr.text())
			}
			if tc.wantDecision != DecisionDispatched {
				if r, _ := rec["reason"].(string); r == "" || r == noReason {
					t.Errorf("a %s record carries reason %q\n%s", tc.wantDecision, r, lr.text())
				}
			}
		})
	}
}
