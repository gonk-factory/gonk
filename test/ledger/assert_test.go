package ledger_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

// An assertion library that cannot fail is worse than none. These tests feed
// Assert deliberately-corrupted Views and require each corruption to be caught;
// a *failure* of the assertion is the *pass* of the test. Same saboteur idiom as
// Plan 03's suites: without it, every scenario built on ledger.Assert could be
// quietly vacuous and green.

// recorder is a ledger.TB whose Fatalf/Errorf record the failure instead of
// aborting the test binary, and whose Fatalf calls runtime.Goexit to reproduce
// testing.TB's abort-this-goroutine semantics (so Assert stops at the first
// failing check, exactly as it would under a real *testing.T).
type recorder struct {
	failed bool
	msg    string
}

func (r *recorder) Helper()                   {}
func (r *recorder) Logf(string, ...any)       {}
func (r *recorder) Errorf(f string, a ...any) { r.record(f, a...) }
func (r *recorder) Fatalf(f string, a ...any) { r.record(f, a...); runtime.Goexit() }
func (r *recorder) record(f string, a ...any) {
	if !r.failed {
		r.failed, r.msg = true, fmt.Sprintf(f, a...)
	}
}

// mustFail runs fn against a recorder in its own goroutine (so Goexit unwinds
// only fn) and requires that fn reported a failure.
func mustFail(t *testing.T, name string, fn func(ledger.TB)) {
	t.Helper()
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	if !r.failed {
		t.Fatalf("%s: expected the assertion to FAIL, but it passed", name)
	}
	t.Logf("%s correctly caught: %s", name, r.msg)
}

// ---- the good scenario every corruption starts from ----

var at = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func goodTags() atags.Tags {
	return atags.Tags{
		Project:    "acme/widget",
		Rig:        "rig-1",
		BeadID:     "gonk:1:issue:7",
		SessionKey: "sess-7-1",
		Rung:       "qwen-local",
		Attempt:    1,
		Trigger:    atags.TriggerIssueTriage,
	}
}

func goodRows() []spend.Row {
	return []spend.Row{
		{CallID: "c1", Tags: goodTags(), CostUSD: 0.05, PromptTokens: 10, CompletionTokens: 20, Synthetic: true, At: at},
		{CallID: "c2", Tags: goodTags(), CostUSD: 0.05, PromptTokens: 10, CompletionTokens: 20, Synthetic: true, At: at},
	}
}

func goodStub() *fakeStub {
	call := stubmodel.Call{Model: "stub-local", Status: 200, Usage: stubmodel.Usage{PromptTokens: 10, CompletionTokens: 20}}
	return &fakeStub{calls: []stubmodel.Call{call, call}}
}

func catalog() map[string]string {
	return map[string]string{"qwen-local": ledger.KindLocal, "qwen-slow": ledger.KindLocal, "glm": ledger.KindCloud}
}

func goodWant() []ledger.Want {
	return []ledger.Want{{
		Project: "acme/widget", Rig: "rig-1", BeadID: "gonk:1:issue:7", SessionKey: "sess-7-1",
		Rung: "qwen-local", Attempt: 1, Trigger: atags.TriggerIssueTriage,
		Calls: 2, PromptTokens: 20, CompletionTokens: 40, SyntheticUSD: 0.10,
	}}
}

// viewsFrom builds Views whose meter (CostSource) is derived from the SAME rows
// as the spend source, so the meter-agreement leg never masks an earlier bug.
func viewsFrom(rows []spend.Row, stub *fakeStub) ledger.Views {
	return ledger.Views{
		Stub:    stub,
		Spend:   &fakeSpend{rows: rows},
		Cost:    &fakeCost{rows: rows},
		Catalog: catalog(),
	}
}

// ---- fakes ----

type fakeStub struct {
	calls     []stubmodel.Call
	exhausted bool
}

func (f *fakeStub) Calls() []stubmodel.Call { return f.calls }
func (f *fakeStub) ScriptExhausted() bool   { return f.exhausted }

type fakeSpend struct {
	rows []spend.Row
	err  error
}

func (f *fakeSpend) Rows(context.Context) ([]spend.Row, error) { return f.rows, f.err }

type fakeCost struct {
	rows      []spend.Row
	attempts  map[string][]meterapi.AttemptView
	budget    meterapi.Budget // optional; ProjectCost echoes it
	remaining meterapi.Budget // optional; ProjectCost echoes it
}

func (f *fakeCost) BeadCost(_ context.Context, bead string) (meterapi.BeadCostResponse, error) {
	var project string
	for _, r := range f.rows {
		if r.Tags.BeadID == bead {
			project = r.Tags.Project
			break
		}
	}
	tot := spend.BeadTotals(f.rows, project, bead)
	return meterapi.BeadCostResponse{
		BeadID: bead, Project: project,
		CostUSD: tot.CostUSD, SyntheticCostUSD: tot.SyntheticCostUSD,
		PromptTokens: tot.PromptTokens, CompletionTokens: tot.CompletionTokens,
		TotalTokens: tot.TotalTokens(), Attempts: f.attempts[bead],
	}, nil
}

func (f *fakeCost) ProjectCost(_ context.Context, project string) (meterapi.ProjectCostResponse, error) {
	tot := spend.Sum(f.rows, func(r spend.Row) bool { return r.Tags.Project == project })
	return meterapi.ProjectCostResponse{
		Project: project, CostUSD: tot.CostUSD, SyntheticCostUSD: tot.SyntheticCostUSD,
		PromptTokens: tot.PromptTokens, CompletionTokens: tot.CompletionTokens, TotalTokens: tot.TotalTokens(),
		Budget: f.budget, Remaining: f.remaining,
	}, nil
}

// ---- the positive control: a consistent ledger must PASS ----

func TestAssertPassesOnAConsistentLedger(t *testing.T) {
	// A real *testing.T: any failure here is a real failure, proving the
	// assertion is not vacuously green.
	ledger.Assert(t, viewsFrom(goodRows(), goodStub()), goodWant())
}

// ---- the saboteurs: each corruption must be CAUGHT ----

func TestAssertCatchesADoubleCharge(t *testing.T) {
	rows := goodRows()
	dup := rows[0]
	dup.CallID = "c1-dup" // a genuinely distinct row for the same work: a double charge
	rows = append(rows, dup)
	mustFail(t, "double-charge", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(rows, goodStub()), goodWant())
	})
}

func TestAssertCatchesALostRow(t *testing.T) {
	rows := goodRows()[:1] // one call happened at the stub, but only... none reached the ledger of the two
	mustFail(t, "lost-row", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(rows, goodStub()), goodWant())
	})
}

func TestAssertCatchesAnUnattributedRow(t *testing.T) {
	rows := goodRows()
	rows[1].Tags.BeadID = "" // strip gonk_bead_id: spend nobody is accountable for
	mustFail(t, "unattributed-row", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(rows, goodStub()), goodWant())
	})
}

func TestAssertCatchesMisattribution(t *testing.T) {
	rows := goodRows()
	rows[1].Tags.Rung = "qwen-slow" // swap gonk_rung to another (in-catalog) rung
	mustFail(t, "misattribution", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(rows, goodStub()), goodWant())
	})
}

func TestAssertCatchesCurrencyMixing(t *testing.T) {
	rows := goodRows()
	rows[1].Synthetic = false // real dollars on a local rung
	mustFail(t, "currency-mixing", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(rows, goodStub()), goodWant())
	})
}

func TestAssertCatchesAnExhaustedScript(t *testing.T) {
	stub := goodStub()
	stub.exhausted = true // the stub served a call the scenario did not script
	mustFail(t, "exhausted-script", func(tb ledger.TB) {
		ledger.Assert(tb, viewsFrom(goodRows(), stub), goodWant())
	})
}

func TestAssertNoSpendCatchesOneRow(t *testing.T) {
	v := viewsFrom(goodRows()[:1], goodStub())
	mustFail(t, "no-spend-one-row", func(tb ledger.TB) {
		ledger.AssertNoSpend(tb, v)
	})
}

// ---- AssertNoSpend / AssertAttempts positive controls ----

func TestAssertNoSpendPassesOnAnEmptyLedger(t *testing.T) {
	v := ledger.Views{
		Stub:    &fakeStub{}, // no calls
		Spend:   &fakeSpend{rows: nil},
		Catalog: catalog(),
	}
	ledger.AssertNoSpend(t, v)
}

func TestAssertAttemptsMatchesTheLadder(t *testing.T) {
	cost := &fakeCost{
		rows: goodRows(),
		attempts: map[string][]meterapi.AttemptView{
			"gonk:1:issue:7": {
				{Attempt: 1, Rung: "qwen-local", Outcome: meterapi.OutcomeGateFailed},
				{Attempt: 2, Rung: "glm", Outcome: meterapi.OutcomeSuccess},
			},
		},
	}
	v := ledger.Views{Stub: goodStub(), Spend: &fakeSpend{rows: goodRows()}, Cost: cost, Catalog: catalog()}
	ledger.AssertAttempts(t, v, "gonk:1:issue:7", []ledger.Attempt{
		{N: 1, Rung: "qwen-local", Outcome: meterapi.OutcomeGateFailed},
		{N: 2, Rung: "glm", Outcome: meterapi.OutcomeSuccess},
	})
	// And a mismatch must be caught.
	mustFail(t, "wrong-attempt-outcome", func(tb ledger.TB) {
		ledger.AssertAttempts(tb, v, "gonk:1:issue:7", []ledger.Attempt{
			{N: 1, Rung: "qwen-local", Outcome: meterapi.OutcomeSuccess}, // wrong outcome
			{N: 2, Rung: "glm", Outcome: meterapi.OutcomeSuccess},
		})
	})
}
