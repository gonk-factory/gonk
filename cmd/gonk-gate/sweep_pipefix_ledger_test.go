package main

// gonk-2usw part VERIFY, L1 evidence: THE METER'S OWN LEDGER PER FIX STATE.
//
// fakeOutcomeMeter records what gonk-sweep SENT. This file runs the REAL
// meter service (internal/meter/service over its in-memory store, behind its
// real HTTP mux, as sweep_skew_test.go does) and reads what meter BOOKED: one
// attempt row per fix attempt, with the outcome plan 7.3 names for the state
// the exit check reached -- and never two rows for one attempt.
//
// The reservation is minted by meter's own /decide for trigger pipeline-fix
// (so its rung is the fix trigger's ladder floor), and the session's tokens
// are rows in the fake LiteLLM spend log, attributed by session key.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/internal/meter/keysink"
	"github.com/gonk-factory/gonk/internal/meter/litellm"
	"github.com/gonk-factory/gonk/internal/meter/service"
	meterstore "github.com/gonk-factory/gonk/internal/meter/store"
	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/rung"
	"github.com/gonk-factory/gonk/pkg/spend"
)

const fixMeterOperatorYAML = `
version: 1
instance:
  enabled: true
  ladder: [local-1, local-2, local-3]
rungs:
  - { name: local-1, kind: local, model: small, est_cost_usd: 0, est_tokens: "16K", synthetic_usd_per_1m_tokens: 1.0 }
  - { name: local-2, kind: local, model: medium, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
  - { name: local-3, kind: local, model: large, est_cost_usd: 0, est_tokens: "50K", synthetic_usd_per_1m_tokens: 1.0 }
meter: { max_spend_staleness: 5m, reservation_ttl: 60m }
`

const fixMeterGonkYML = "version: 1\nenabled: true\nactions: { pipelines: true, pipeline_fixes: true }\n" +
	"ladder: [local-1, local-2, local-3]\nbudget: { monthly_cost_usd: 0 }\npipeline_fix: { max_attempts: 3 }\n"

type fixMeter struct {
	api   *meterAPI
	store *meterstore.Memory
	admin *litellm.Fake
}

func newFixMeter(t *testing.T, projectID int64) *fixMeter {
	t.Helper()
	cfg, err := opercfg.Load([]byte(fixMeterOperatorYAML))
	if err != nil {
		t.Fatalf("operator config: %v", err)
	}
	st := meterstore.NewMemory()
	admin := litellm.NewFake()
	admin.Now = time.Now()
	svc := service.New(cfg, st, admin, admin, keysink.NewMemory(), time.Now)
	resp, status, err := svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "example/calc", ProjectID: projectID, Rig: "example-calc", GonkYML: fixMeterGonkYML,
	})
	if err != nil || status != 200 || resp.State != meterapi.StateActive {
		t.Fatalf("register: status=%d err=%v resp=%+v", status, err, resp)
	}
	if err := svc.SyncSpend(context.Background()); err != nil {
		t.Fatalf("spend sync: %v", err)
	}
	mux, err := service.NewMux(svc, "tok", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixMeter{api: newMeterAPI(srv.URL, "tok"), store: st, admin: admin}
}

// reserve asks the real meter to decide the fix's next attempt, binds the
// stored fix record to the reservation it minted, and books the session's
// model spend (so a not-fixed attempt is PROVABLY paid for).
func (m *fixMeter) reserve(t *testing.T, w *fvWorld) meterapi.DecideResponse {
	t.Helper()
	rec := w.rec()
	dec, err := m.api.Decide(context.Background(), meterapi.DecideRequest{
		Project: "example/calc", Rig: "example-calc", BeadID: rec.BeadAnchor, SessionKey: rec.SessionKey, Trigger: atags.TriggerPipelineFix,
	})
	if err != nil || dec.Decision != meterapi.DecisionRun || dec.ReservationID == "" {
		t.Fatalf("meter did not decide run for the fix: %+v %v", dec, err)
	}
	rec.ReservationID, rec.Attempt, rec.Rung, rec.ReservationExpiresAt = dec.ReservationID, dec.Attempt, dec.Rung, dec.ReservationExpiresAt
	rec.PipelineFix.AppliedAttempt = dec.Attempt
	// The meter's clock is the wall clock; the world's sweep clock runs a
	// minute ahead of it. The session ended before meter's spend view.
	rec.SessionEndedAt = time.Now().Add(-time.Minute)
	if err := w.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	m.admin.AddRows(spend.Row{
		CallID: fmt.Sprintf("call-%s-%d", rec.SessionKey, dec.Attempt), PromptTokens: 900, CompletionTokens: 300, At: time.Now(),
		Tags: atags.Tags{Project: "example/calc", Rig: "example-calc", BeadID: rec.BeadAnchor, SessionKey: rec.SessionKey,
			Rung: dec.Rung, Attempt: dec.Attempt, Trigger: atags.TriggerPipelineFix},
	})
	return *dec
}

// look is one exit-check look against the real meter.
func (m *fixMeter) look(t *testing.T, w *fvWorld) string {
	t.Helper()
	w.clock = w.clock.Add(pipefixJudgeEvery + time.Second)
	at := w.log.Len()
	d := w.deps()
	d.Meter = m.api
	m.admin.Now = time.Now() // LiteLLM's clock agrees with meter's
	if code := runSweep(context.Background(), d); code != 0 {
		t.Fatalf("sweep exit %d\n%s", code, w.log.String())
	}
	return w.log.String()[at:]
}

func (m *fixMeter) ledger(t *testing.T, w *fvWorld) []rung.Attempt {
	t.Helper()
	rows, err := m.store.Attempts(context.Background(), "example/calc", w.fixAnchor())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func ledgerText(rows []rung.Attempt) string {
	var parts []string
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("{attempt %d rung %s outcome %s}", r.Attempt, r.Rung, r.Outcome))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ONE LEDGER ROW PER ATTEMPT, WITH PLAN 7.3'S OUTCOME FOR THE STATE REACHED.
func TestTheRealMetersLedgerHoldsOneRowPerAttemptWithThePlansOutcome(t *testing.T) {
	for _, c := range []struct {
		name     string
		arrange  func(w *fvWorld)
		fixState pipefix.State
		outcome  rung.Outcome
	}{
		{"fixed", func(w *fvWorld) { w.mrPipeline("success", fvPass("build"), fvPass("unit-tests")) },
			pipefix.StateFixed, rung.OutcomeSuccess},
		{"fixed-new-failure", func(w *fvWorld) { w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", fvOtherLog)) },
			pipefix.StateFixedNewFailure, rung.OutcomeSuccess},
		{"flaky", func(w *fvWorld) {
			w.mrPipeline("success", fvPass("build"), fvFail("unit-tests", plTestLog), fvPass("unit-tests"))
		}, pipefix.StateFlaky, rung.OutcomeSuccess},
		{"unverifiable", func(w *fvWorld) { w.mrPipeline("success", fvPass("build")) },
			pipefix.StateUnverifiable, rung.OutcomeSuccess},
		{"regressed", func(w *fvWorld) { w.mrPipeline("failed", fvFail("build", plLintLog), fvPass("unit-tests")) },
			pipefix.StateRegressed, rung.OutcomeSuccess},
		{"taken-over", func(w *fvWorld) {
			w.r.gl.SetMRHead(w.r.pid, w.rec().PipelineFix.MRIID, "9999999999999999999999999999999999999999")
		}, pipefix.StateTakenOver, rung.OutcomeSuccess},
		{"closed-unmerged", func(w *fvWorld) { w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "closed", w.clock) },
			pipefix.StateClosedUnmerged, rung.OutcomeSuccess},
		{"merged", func(w *fvWorld) { w.r.gl.SetMRState(w.r.pid, w.rec().PipelineFix.MRIID, "merged", w.clock) },
			pipefix.StateMerged, rung.OutcomeSuccess},
		{"denied (not-fixed at max_attempts)", func(w *fvWorld) {
			rec := w.rec()
			rec.PipelineFix.MaxAttempts = 1
			_ = w.store.Put(context.Background(), rec)
			w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
		}, pipefix.StateDenied, rung.OutcomeGateFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newFVWorld(t, faOpts{})
			m := newFixMeter(t, w.r.pid)
			dec := m.reserve(t, w)
			if dec.Attempt != 1 || dec.Rung != "local-2" {
				t.Fatalf("meter's first fix attempt: %d on %q, want 1 on the fix trigger's floor rung local-2", dec.Attempt, dec.Rung)
			}
			c.arrange(w)
			log := m.look(t, w)
			rows := m.ledger(t, w)
			rec := w.rec()
			t.Logf("LEDGER (real meter) fix_state=%-18s rows=%s", rec.PipelineFix.State, ledgerText(rows))
			if rec.PipelineFix.State != string(c.fixState) {
				t.Fatalf("fix state %q, want %q\n%s", rec.PipelineFix.State, c.fixState, log)
			}
			if len(rows) != 1 || rows[0].Attempt != 1 || rows[0].Rung != "local-2" || rows[0].Outcome != c.outcome {
				t.Fatalf("meter's ledger %s, want one row: attempt 1, local-2, %s", ledgerText(rows), c.outcome)
			}
			// More ticks: still one row.
			m.look(t, w)
			m.look(t, w)
			if rows := m.ledger(t, w); len(rows) != 1 {
				t.Fatalf("the ledger grew to %s", ledgerText(rows))
			}
		})
	}
}

// TWO ATTEMPTS ON THE REAL METER: not-fixed is a paid gate failure, the next
// attempt is meter's attempt 2 on the same floor rung (the fix trigger's one
// floor retry), its fix is fixed, and the ledger is exactly those two rows.
func TestTwoFixAttemptsOnTheRealMeterAreTwoLedgerRows(t *testing.T) {
	w := newFVWorld(t, faOpts{cfg: func(c *meterapi.ProjectResponse) { c.Effective.PipelineFix.MaxAttempts = 3 }})
	m := newFixMeter(t, w.r.pid)
	m.reserve(t, w)
	w.mrPipeline("failed", fvPass("build"), fvFail("unit-tests", plTestLog))
	log := m.look(t, w)
	if rows := m.ledger(t, w); len(rows) != 1 || rows[0].Outcome != rung.OutcomeGateFailed {
		t.Fatalf("after not-fixed the ledger is %s, want one gate-failed row\n%s", ledgerText(rows), log)
	}
	rec := w.rec()
	if rec.PipelineFix.State != "" || rec.ReslungAt.IsZero() || rec.PipelineFix.Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("after not-fixed: fix state %q reslung %v outcome %q", rec.PipelineFix.State, rec.ReslungAt, rec.PipelineFix.Outcome)
	}

	// The second attempt: meter decides it (attempt 2), and it is applied on
	// top of the first. Stand in for the dispatch + session + apply by moving
	// the record as they would, with a real second commit on the branch.
	dec := m.reserve(t, w)
	if dec.Attempt != 2 || dec.Rung != "local-2" {
		t.Fatalf("meter's second fix attempt: %d on %q, want 2 on local-2 (the floor retry)", dec.Attempt, dec.Rung)
	}
	c2, err := w.r.gl.Client().CreateCommit(context.Background(), w.r.pid, glabCommitOn(w.branch(), "second attempt", pfSource, "package calc\n"))
	if err != nil {
		t.Fatal(err)
	}
	rec = w.rec()
	rec.State, rec.ReslungAt, rec.ReslingFireBy = beadstore.StateRunning, time.Time{}, time.Time{}
	rec.PipelineFix.State, rec.PipelineFix.FixCommitSHA = string(pipefix.StateVerifying), c2.ID
	rec.PipelineFix.VerifyDeadline = w.clock.Add(2 * time.Hour)
	if err := w.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	w.mrPipeline("success", fvPass("build"), fvPass("unit-tests"))
	log = m.look(t, w)
	rows := m.ledger(t, w)
	t.Logf("LEDGER (real meter) two attempts: rows=%s", ledgerText(rows))
	if len(rows) != 2 || rows[0].Outcome != rung.OutcomeGateFailed || rows[1].Attempt != 2 || rows[1].Outcome != rung.OutcomeSuccess {
		t.Fatalf("ledger %s, want [gate-failed, success]\n%s", ledgerText(rows), log)
	}
	if got := w.rec(); got.PipelineFix.State != string(pipefix.StateFixed) || got.PipelineFix.OutcomeAttempt != 2 {
		t.Fatalf("fix state %q outcome attempt %d", got.PipelineFix.State, got.PipelineFix.OutcomeAttempt)
	}
}

func glabCommitOn(branch, message, path, content string) glab.CommitOptions {
	return glab.CommitOptions{Branch: branch, CommitMessage: message,
		Actions: []glab.CommitAction{{Action: "update", FilePath: path, Content: content}}}
}
