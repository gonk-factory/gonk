package integration_test

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/corpus"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// TestHostileConfigsFailClosedThroughTheWholeStack pushes every hostile .gonk.yml
// through the real intake into the real meter and requires fail-CLOSED behaviour
// end to end. For the Reject corpus (the loader must refuse):
//   - meter answers 422 (the PROJECT's yaml is bad), never 400 (intake's bug),
//     never 500, never a panic;
//   - the project lands `invalid`; Effective is NULL (not a zero Effective);
//   - budget is ZERO, not null -- null means UNLIMITED and would be catastrophic;
//   - NO virtual key exists; NO session may run; NO spend;
//   - and BOTH PROCESSES ARE STILL UP afterwards.
//
// AcceptByLoader entries (whose property is not gonkcfg's to judge -- BOM/CRLF
// normalization, meter-side ladder-order/timezone enforcement) are held only to
// the liveness contract: no panic, no hang, both processes up. Asserting 422 on
// them would OVERCLAIM what schema validation covers (see test/corpus).
func TestHostileConfigsFailClosedThroughTheWholeStack(t *testing.T) {
	for _, c := range corpus.GonkYML(t) {
		t.Run(c.Name, func(t *testing.T) {
			w := NewWorld(t)
			project := "acme/" + c.Name
			p := w.GitLab.AddProject(project, glab.AccessMaintainer)
			p.PutFile(".gonk.yml", c.Bytes)

			w.Reconcile(t) // must not panic, must not hang

			if c.Disposition == corpus.Reject {
				// The real meter PUT answers 422 with a full ProjectResponse -- 400
				// would mean we blamed intake for the project's bad yaml.
				raw := w.Meter.registerRaw(t, project, p.ID, string(c.Bytes))
				if raw.Status != 422 {
					t.Fatalf("status = %d, want 422 (400 blames intake for the project's bad yaml)", raw.Status)
				}
				if raw.Body.Effective != nil {
					t.Fatal("an invalid config has NO Effective (ADR-002), not a zero one")
				}
				if raw.Body.Budget.MonthlyCostUSD == nil {
					t.Fatal("null budget means UNLIMITED; an invalid project must get ZERO")
				}
				if *raw.Body.Budget.MonthlyCostUSD != 0 {
					t.Fatalf("invalid project budget = %v, want 0", *raw.Body.Budget.MonthlyCostUSD)
				}
				requireIntakeState(t, w, project, "invalid")
				if w.LLM.HasKeyFor(project) {
					t.Fatal("an invalid project holds a live virtual key")
				}
				d := w.SessionFor(project, "gk-h", "sess-h", atags.TriggerIssueTriage).Decide(t)
				ledger.AssertDenied(t, d, meterapi.ReasonInvalidConfig)
				ledger.AssertNoSpend(t, w.Views)
			}

			requireAlive(t, w) // /healthz on both, after every single hostile input
		})
	}
}

// TestDecidedMetadataIsAlwaysAttributable states PLAN.md carry-forward 3 at the
// boundary: meter is the SINGLE charset boundary that mints attribution tags, so
// every `run` it hands out carries metadata that round-trips cleanly through
// pkg/atags -- no value that could column-shift a CSV or forge a log line ever
// leaves the building unvalidated.
func TestDecidedMetadataIsAlwaysAttributable(t *testing.T) {
	w := NewWorld(t)
	onboard(t, w, "acme/widget", withLadder("qwen-local"), withBudget(0))
	d := w.Session("gk-1", "s1", atags.TriggerScaffold).Decide(t)
	ledger.AssertRan(t, d)
	tags, err := atags.FromMetadata(d.Metadata)
	if err != nil {
		t.Fatalf("meter minted un-attributable metadata: %v", err)
	}
	if err := tags.Validate(); err != nil {
		t.Fatalf("minted tags do not validate: %v", err)
	}
}
