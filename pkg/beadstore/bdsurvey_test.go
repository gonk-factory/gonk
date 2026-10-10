package beadstore_test

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// workstore-survey sizes a cutover before it happens (gonk-t41 verification,
// gap 1): how many records in each state the Postgres-backed sweep will see on
// its first pass, and which of them the bd-backed sweep could NOT see because
// their bead lost its state label. It writes nothing.
func TestSurveyBdCountsStatesAndFindsRecordsHiddenFromBdList(t *testing.T) {
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	put := func(r beadstore.Record) {
		t.Helper()
		if err := bd.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateRunning})
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:2", State: beadstore.StateRunning})
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:3", State: beadstore.StateParked})
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:4"}) // go-ffoe: no state
	// A torn label: the running record's bead lost gonk::running.
	if _, err := fake.run(ctx, "", "", "label", "remove", "bd-2", "gonk::running"); err != nil {
		t.Fatal(err)
	}
	before := len(fake.comments["bd-1"])

	sv, err := beadstore.SurveyBd(ctx, bd, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("SurveyBd = %v", err)
	}
	if sv.Anchors != 4 || !reflect.DeepEqual(sv.ByState, map[string]int{"running": 2, "parked": 1, "": 1}) ||
		!reflect.DeepEqual(sv.HiddenFromBdList, []string{"gonk:1:issue:2"}) || len(sv.Failed) != 0 {
		t.Fatalf("survey = %+v", sv)
	}
	if recs, _ := bd.List(ctx, beadstore.StateRunning); len(recs) != 1 {
		t.Fatalf("precondition: bd's List(running) returned %d, want 1 (the torn bead hidden)", len(recs))
	}
	if len(fake.comments["bd-1"]) != before {
		t.Fatal("the survey wrote to bd")
	}
}
