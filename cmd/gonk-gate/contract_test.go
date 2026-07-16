package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gate"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/intake"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// The owner's requirement, made executable: intake (Gate 1) and gonk-gate (Gate 2)
// must agree EXACTLY on what run/defer/deny mean. They already share the TYPES
// (both import pkg/meterapi, a compile-time contract). This asserts they share the
// SEMANTICS.
//
// The table is the single source of truth for both call sites. If someone changes
// one side's behaviour, this test fails and they have to come and change the
// meaning here, in the open, where a reviewer sees it.
func TestGateSemanticsAreSharedWithIntake(t *testing.T) {
	for _, tc := range []struct {
		decision string
		mayPour  bool // Gate 2: pour a formula?
		mayFire  bool // Gate 1: fire the order?
		terminal bool // does the bead stop here until a human or a clock intervenes?
	}{
		{meterapi.DecisionRun, true, true, false},
		{meterapi.DecisionDefer, false, false, false}, // a clock un-parks it
		{meterapi.DecisionDeny, false, false, true},   // only a human revives it
	} {
		if gotPour := gate.MayPour(tc.decision); gotPour != tc.mayPour {
			t.Fatalf("MayPour(%q) = %v, want %v", tc.decision, gotPour, tc.mayPour)
		}
		if gotFire := intake.MayFire(tc.decision); gotFire != tc.mayFire {
			t.Fatalf("MayFire(%q) = %v, want %v -- GATE 1 AND GATE 2 HAVE DRIFTED.\n"+
				"Read 'The two call sites MUST agree' in Plan 04 before you 'fix' this test.", tc.decision, gotFire, tc.mayFire)
		}
	}
	// An unknown decision must be refused by BOTH. A new decision kind added to
	// meter must not be silently treated as `run` by an old binary.
	if gate.MayPour("something-new") || intake.MayFire("something-new") {
		t.Fatal("an unknown decision kind must fail closed on both sides")
	}
}

// Gate 1 (intake) and Gate 2 (gonk-dispatch) MUST send meter the SAME bead_id for
// one work item, or meter mints TWO reservations for one attempt -- double budget
// headroom, and Gate 1's reservation leaks until its TTL. Both sides pin bead_id
// to the deterministic BeadAnchor, NOT the Gas City bead id. Here we drive Gate 2
// with a BeadAnchor equal to intake's own BeadAnchor(42, 3) AND a *different* Gas
// City bead id, and assert the /decide body carries the BeadAnchor -- so meter
// reuses the one open reservation Gate 1 already opened.
func TestBothGatesSendTheSameBeadID(t *testing.T) {
	anchor := intake.BeadAnchor(42, 3) // the exact value Gate 1 sends
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Attempt: 1, RetryAfter: time.Now().Add(time.Hour),
	}}
	args := baseDispatchArgs()
	args.BeadAnchor = anchor
	args.BeadID = "gk-1a2b" // Gas City id differs ON PURPOSE

	_ = runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gcapitest.New(t).Client("gonk-city"),
		Store: beadstore.NewMemory(), Args: args,
	})

	var got map[string]any
	if err := json.Unmarshal(fm.lastBody, &got); err != nil {
		t.Fatalf("decode /decide body: %v", err)
	}
	if got["bead_id"] != anchor {
		t.Fatalf("Gate 2 sent bead_id=%v, want the BeadAnchor %q that Gate 1 sends -- "+
			"a mismatch mints a SECOND reservation for one attempt", got["bead_id"], anchor)
	}
	if got["bead_id"] == "gk-1a2b" {
		t.Fatal("Gate 2 leaked the Gas City bead id onto the wire; it must send the BeadAnchor")
	}
}
