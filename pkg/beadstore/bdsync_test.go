//go:build integration

// The cutover from bd (gonk-t41, docs/plans/2026-09-14-t41-postgres-workstore.md
// section 6), end to end: BdCLI over the in-process fake bd (the same one
// storetest.Run holds BdCLI to), into and out of a REAL Postgres work store.
package beadstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

func syncLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustBdGet(t *testing.T, bd *beadstore.BdCLI, anchor string) beadstore.Record {
	t.Helper()
	r, ok, err := bd.Get(context.Background(), anchor)
	if err != nil || !ok {
		t.Fatalf("bd Get(%s) = %+v, %v, %v", anchor, r, ok, err)
	}
	return r
}

func mustPgGet(t *testing.T, s *beadstore.Postgres, anchor string) beadstore.Record {
	t.Helper()
	r, ok, err := s.Get(context.Background(), anchor)
	if err != nil || !ok {
		t.Fatalf("Postgres Get(%s) = %+v, %v, %v", anchor, r, ok, err)
	}
	return r
}

// bdComment appends a raw gonk-state comment to a new bead carrying anchor's
// label -- how a record from an older binary (no Revision), a torn write, or a
// corrupt one is put in front of the import.
func bdComment(t *testing.T, fake *fakeBd, anchor, body string) {
	t.Helper()
	ctx := context.Background()
	out, err := fake.run(ctx, "", "", "create", "--title", anchor, "--labels", "gonk-anchor:"+anchor, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string }
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.run(ctx, "", "", "comment", created.ID, "<!-- gonk-state "+body+" -->"); err != nil {
		t.Fatal(err)
	}
	// Label it with its state, as every gonk-gate that wrote bd did, so the
	// bd-backed List could see it (a test that wants it hidden removes the label).
	var st struct{ State string }
	if json.Unmarshal([]byte(body), &st) == nil && st.State != "" {
		if _, err := fake.run(ctx, "", "", "label", "add", created.ID, "gonk::"+st.State); err != nil {
			t.Fatal(err)
		}
	}
}

// importOK runs an import that must have COMPLETED. gonk-p44x (e): it used to
// assert only err == nil, and an import returns nil with anchors in Failed --
// that is the whole point of the report. Every caller of this helper means "the
// import ran and decided every anchor", so a Failed anchor is a test failure
// here, not something a later assertion may or may not notice.
func importOK(t *testing.T, s *beadstore.Postgres, bd *beadstore.BdCLI) beadstore.BdSyncReport {
	t.Helper()
	rep, err := s.ImportFromBd(context.Background(), bd, syncLog(), beadstore.ImportOptions{})
	if err != nil {
		t.Fatalf("ImportFromBd = %v (report %+v)", err, rep)
	}
	if !rep.Complete() {
		t.Fatalf("the import did not complete: %d anchor(s) could not be read or written: %v (report %+v)",
			len(rep.Failed), rep.Failed, rep)
	}
	return rep
}

// THE CUTOVER. Every record the bd-backed gonk-gate could Get arrives in
// Postgres exactly as Get returned it -- revision and write time included, so a
// reservation keeps its age and a reader's revision stays valid -- across every
// state, past bd's 50-row default, including the shapes that were stranded in
// bd: a record with no state (go-ffoe), a bead with no record, a record from
// before revisions existed. A record that does not decode is reported and left.
func TestImportCopiesWhatTheBdBackedBinarySaw(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()

	states := []beadstore.State{beadstore.StateRunning, beadstore.StateParked, beadstore.StatePendingPrompt,
		beadstore.StateNeedsHuman, beadstore.StateDone}
	var anchors []string
	reserved := time.Date(2026, 9, 13, 15, 49, 24, 123456789, time.UTC)
	for i := 0; i < 60; i++ {
		a := fmt.Sprintf("gonk:75:issue:%d", 100+i)
		anchors = append(anchors, a)
		r := beadstore.Record{BeadAnchor: a, State: states[i%len(states)], Attempt: 1 + i%3,
			SessionID: fmt.Sprintf("gonk.triage.p75.i%d.a1", 100+i), ReservationID: fmt.Sprintf("rsv-%d", i),
			ReslungAt: reserved.Add(time.Duration(i) * time.Second)}
		if r.State == beadstore.StatePendingPrompt {
			r.UpdatedAt = reserved // a reservation keeps its age across writes
		}
		if err := bd.Put(ctx, r); err != nil {
			t.Fatalf("bd Put: %v", err)
		}
	}
	// go-ffoe: dispatch's release with no prior record writes State "".
	if err := bd.Put(ctx, beadstore.Record{BeadAnchor: "gonk:75:issue:69", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	// A record from before revisions existed.
	bdComment(t, fake, "gonk:75:issue:1", `{"BeadAnchor":"gonk:75:issue:1","State":"parked","Attempt":1}`)
	// A bead with the anchor label and no gonk-state record at all.
	if _, err := fake.run(ctx, "", "", "create", "--title", "x", "--labels", "gonk-anchor:gonk:75:issue:2", "--json"); err != nil {
		t.Fatal(err)
	}
	// A corrupt record.
	bdComment(t, fake, "gonk:75:issue:3", `{"BeadAnchor": not json`)

	rep := importOK(t, s, bd)
	if rep.Seen != 64 || rep.Written != 62 || rep.NoRecord != 1 || !slices.Equal(rep.Undecodable, []string{"gonk:75:issue:3"}) ||
		len(rep.Conflicts) != 0 || !rep.Complete() {
		t.Fatalf("report = %+v, want seen 64, written 62, no_record 1, undecodable [gonk:75:issue:3], complete", rep)
	}
	if done, err := s.BdImportCompleted(ctx); err != nil || !done {
		t.Fatalf("BdImportCompleted = %v, %v after a complete import", done, err)
	}

	for _, a := range append(anchors, "gonk:75:issue:69", "gonk:75:issue:1") {
		want, got := mustBdGet(t, bd, a), mustPgGet(t, s, a)
		if want.Revision == "" {
			// A record written before revisions existed gets one at import (A1).
			if got.Revision == "" {
				t.Fatalf("%s: imported with an empty revision", a)
			}
			want.Revision = got.Revision
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: Postgres has\n%+v\nbd's Get returned\n%+v", a, got, want)
		}
	}
	for _, st := range states {
		bdList, err := bd.List(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		pgList, err := s.List(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		// bd's own List is capped at 50 rows by bd (gonk-kx3); no state holds more
		// than 13 here, every record is labelled and fresh, so nothing is
		// quarantined and the lists match exactly.
		want := len(bdList)
		if len(pgList) != want || len(pgList) == 0 {
			t.Fatalf("List(%s): Postgres %d records, want %d (bd's List: %d)", st, len(pgList), want, len(bdList))
		}
	}
	if got := mustPgGet(t, s, "gonk:75:issue:69"); got.State != "" {
		t.Fatalf("go-ffoe's record state = %q, want it imported as it was (empty)", got.State)
	}
	if _, ok, _ := s.Get(ctx, "gonk:75:issue:3"); ok {
		t.Fatal("the undecodable record was imported")
	}
	// "" means "no record" in this store: it does NOT match the imported
	// pre-revision record (review defect A1).
	if _, err := s.PutIf(ctx, beadstore.Record{BeadAnchor: "gonk:75:issue:1", State: beadstore.StateRunning}, ""); !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf(\"\") over the imported pre-revision record = %v, want ErrStale", err)
	}
}

// Every controller pod start re-runs the import. With nothing changed it
// writes nothing.
func TestImportIsIdempotent(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	for i := 0; i < 5; i++ {
		if err := bd.Put(context.Background(), beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:1:issue:%d", i), State: beadstore.StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	importOK(t, s, bd)
	rep := importOK(t, s, bd)
	if rep.Written != 0 || rep.InSync != 5 || len(rep.Conflicts) != 0 {
		t.Fatalf("second import = %+v, want nothing written and 5 in sync", rep)
	}
}

// A rollback to the bd-backed binary happened WITHOUT an export, then the
// Postgres-backed binary came back. For each anchor the merge base decides:
// bd moved and Postgres did not -> bd's record is taken; both moved -> CONFLICT,
// neither touched; Postgres moved and bd did not -> Postgres kept.
func TestImportMergesAgainstTheLastSync(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const onlyBd, both, onlyPg = "gonk:1:issue:1", "gonk:1:issue:2", "gonk:1:issue:3"
	for _, a := range []string{onlyBd, both, onlyPg} {
		if err := bd.Put(ctx, beadstore.Record{BeadAnchor: a, State: beadstore.StateRunning, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	importOK(t, s, bd)

	// The Postgres-backed binary moves two records on.
	for _, a := range []string{both, onlyPg} {
		cur := mustPgGet(t, s, a)
		cur.Attempt, cur.SessionID = 2, "pg-writer"
		if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
			t.Fatal(err)
		}
	}
	// The rolled-back bd-backed binary moves two records on.
	for _, a := range []string{onlyBd, both} {
		cur := mustBdGet(t, bd, a)
		cur.Attempt, cur.SessionID = 3, "bd-writer"
		if err := bd.Put(ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	pgBoth := mustPgGet(t, s, both)

	rep := importOK(t, s, bd)
	if rep.Written != 1 || rep.InSync != 1 || !slices.Equal(rep.Conflicts, []string{both}) || !rep.Complete() {
		t.Fatalf("report = %+v, want 1 written (%s), 1 in sync (%s), conflict [%s]", rep, onlyBd, onlyPg, both)
	}
	if got := mustPgGet(t, s, onlyBd); got.SessionID != "bd-writer" || !reflect.DeepEqual(got, mustBdGet(t, bd, onlyBd)) {
		t.Fatalf("%s: Postgres %+v, want bd's newer record", onlyBd, got)
	}
	if got := mustPgGet(t, s, both); !reflect.DeepEqual(got, pgBoth) {
		t.Fatalf("%s (conflict): Postgres was changed to %+v", both, got)
	}
	if got := mustPgGet(t, s, onlyPg); got.SessionID != "pg-writer" {
		t.Fatalf("%s: Postgres %+v, want its own newer record kept", onlyPg, got)
	}
	// A conflict is reported on every run until a human resolves it.
	if again := importOK(t, s, bd); !slices.Equal(again.Conflicts, []string{both}) {
		t.Fatalf("second import conflicts = %v, want [%s] again", again.Conflicts, both)
	}
}

// A record the Postgres-backed binary created for an anchor bd also has, with
// no sync between them, is a conflict: neither is known to be newer.
func TestImportNeverSyncedDifferentRecordIsAConflict(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StatePendingPrompt, SessionID: "pg"}); err != nil {
		t.Fatal(err)
	}
	if err := bd.Put(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateRunning, SessionID: "bd"}); err != nil {
		t.Fatal(err)
	}
	rep := importOK(t, s, bd)
	if !slices.Equal(rep.Conflicts, []string{"gonk:1:issue:1"}) || rep.Written != 0 {
		t.Fatalf("report = %+v, want a conflict and nothing written", rep)
	}
	if got := mustPgGet(t, s, "gonk:1:issue:1"); got.SessionID != "pg" {
		t.Fatalf("Postgres record overwritten: %+v", got)
	}
}

// An anchor that cannot be read leaves the import INCOMPLETE: it is not marked
// imported, so gonk-gate keeps refusing to dispatch on the store, and the
// initContainer exits non-zero and retries.
func TestImportThatCannotReadAnAnchorIsNotMarkedComplete(t *testing.T) {
	s, _ := newWorkStore(t)
	defer beadstore.SetBdReadBackoff(time.Millisecond)()
	fake := newFakeBd()
	ctx := context.Background()
	good := &beadstore.BdCLI{Run: fake.run}
	for i := 0; i < 3; i++ {
		if err := good.Put(ctx, beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:1:issue:%d", i), State: beadstore.StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	failing := &beadstore.BdCLI{Run: func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "comments" && args[1] == "bd-2" {
			return nil, errors.New("dolt: connection reset")
		}
		return fake.run(ctx, dir, bin, args...)
	}}
	rep, err := s.ImportFromBd(ctx, failing, syncLog(), beadstore.ImportOptions{})
	if err != nil {
		t.Fatalf("ImportFromBd = %v", err)
	}
	if rep.Complete() || len(rep.Failed) != 1 || rep.Written != 2 {
		t.Fatalf("report = %+v, want 1 failed and 2 written", rep)
	}
	if done, _ := s.BdImportCompleted(ctx); done {
		t.Fatal("an incomplete import marked the store imported")
	}
	if rep := importOK(t, s, good); !rep.Complete() || rep.Written != 1 || rep.InSync != 2 {
		t.Fatalf("retry = %+v, want the missing anchor written and the rest in sync", rep)
	}
	if done, _ := s.BdImportCompleted(ctx); !done {
		t.Fatal("a complete retry did not mark the store imported")
	}
}

// Two beads for one anchor (BdCLI's first-write race, bd_notfinal_test.go): the
// import takes the one the bd-backed binary's Get read, and says so.
func TestImportOfDuplicateBeadsTakesTheOneGetRead(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	bdComment(t, fake, "gonk:1:issue:1", `{"BeadAnchor":"gonk:1:issue:1","State":"running","SessionID":"first","Revision":"r1"}`)
	bdComment(t, fake, "gonk:1:issue:1", `{"BeadAnchor":"gonk:1:issue:1","State":"running","SessionID":"second","Revision":"r2"}`)
	rep := importOK(t, s, bd)
	if !slices.Equal(rep.Duplicates, []string{"gonk:1:issue:1"}) {
		t.Fatalf("duplicates = %v", rep.Duplicates)
	}
	if got, want := mustPgGet(t, s, "gonk:1:issue:1"), mustBdGet(t, bd, "gonk:1:issue:1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("imported %+v, bd's Get read %+v", got, want)
	}
}

// THE ROLLBACK. After the Postgres-backed binary has run, workstore-export
// brings bd up to date, so the bd-backed binary starts from the current records;
// and when the Postgres-backed binary comes back, the import sees bd unchanged
// where nothing ran, and bd newer where the rolled-back binary wrote.
func TestExportMakesBdCurrentForARollbackAndTheNextImportAgrees(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const moved, untouched, created = "gonk:1:issue:1", "gonk:1:issue:2", "gonk:1:issue:3"
	for _, a := range []string{moved, untouched} {
		if err := bd.Put(ctx, beadstore.Record{BeadAnchor: a, State: beadstore.StateRunning, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	importOK(t, s, bd)

	cur := mustPgGet(t, s, moved)
	cur.State, cur.Attempt, cur.SessionID = beadstore.StatePendingPrompt, 2, "gonk.triage.p1.i1.a2"
	if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, beadstore.Record{BeadAnchor: created, State: beadstore.StateParked, Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ExportToBd(ctx, bd, syncLog())
	if err != nil || !rep.Complete() || rep.Written != 2 || rep.InSync != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("export = %+v, %v; want 2 written (%s, %s), 1 in sync", rep, err, moved, created)
	}
	for _, a := range []string{moved, created} {
		pg, b := mustPgGet(t, s, a), mustBdGet(t, bd, a)
		if b.State != pg.State || b.Attempt != pg.Attempt || b.SessionID != pg.SessionID {
			t.Fatalf("%s: bd has %s a%d %q after export, Postgres %s a%d %q", a, b.State, b.Attempt, b.SessionID, pg.State, pg.Attempt, pg.SessionID)
		}
	}
	// The rolled-back binary's List sees the exported records.
	if recs, err := bd.List(ctx, beadstore.StatePendingPrompt); err != nil || len(recs) != 1 || recs[0].BeadAnchor != moved {
		t.Fatalf("bd List(pending-prompt) = %+v, %v", recs, err)
	}

	// Rolled back: the bd-backed binary writes one record.
	b := mustBdGet(t, bd, created)
	b.State = beadstore.StateRunning
	if err := bd.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	// Rolled forward: the import takes exactly that write.
	rep = importOK(t, s, bd)
	if rep.Written != 1 || rep.InSync != 2 || len(rep.Conflicts) != 0 {
		t.Fatalf("import after the rollback = %+v, want 1 written (%s), 2 in sync", rep, created)
	}
	if got := mustPgGet(t, s, created); got.State != beadstore.StateRunning {
		t.Fatalf("%s: Postgres %s, want the rolled-back binary's running record", created, got.State)
	}
	if again, err := s.ExportToBd(ctx, bd, syncLog()); err != nil || again.Written != 0 || len(again.Conflicts) != 0 {
		t.Fatalf("export with nothing changed = %+v, %v", again, err)
	}
}

// An export must not overwrite a bd record that ALSO moved: that record is a
// conflict, and bd keeps it.
func TestExportDoesNotOverwriteABdRecordThatAlsoMoved(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const a = "gonk:1:issue:1"
	if err := bd.Put(ctx, beadstore.Record{BeadAnchor: a, State: beadstore.StateRunning}); err != nil {
		t.Fatal(err)
	}
	importOK(t, s, bd)
	cur := mustPgGet(t, s, a)
	cur.SessionID = "pg"
	if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
		t.Fatal(err)
	}
	b := mustBdGet(t, bd, a)
	b.SessionID = "bd"
	if err := bd.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	rep, err := s.ExportToBd(ctx, bd, syncLog())
	if err != nil || !slices.Equal(rep.Conflicts, []string{a}) || rep.Written != 0 {
		t.Fatalf("export = %+v, %v; want a conflict and nothing written", rep, err)
	}
	if got := mustBdGet(t, bd, a); got.SessionID != "bd" {
		t.Fatalf("bd record overwritten: %+v", got)
	}
}

// THE ROLLBACK THAT WILL ACTUALLY HAPPEN. The gonk-gate deployed before this
// stack writes records with NO Revision (the field arrived with gonk-p8w8, which
// ships with the work store). Every record on the cluster is such a record, and
// a rollback returns to that binary. Its writes leave the bd revision "" -- so a
// merge that asks "did bd's revision change?" never sees them. It must notice
// the RECORD changed.
func TestImportSeesWritesByABinaryThatHasNoRevisions(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const onlyBd, both, untouched = "gonk:75:issue:1", "gonk:75:issue:2", "gonk:75:issue:3"
	ids := map[string]string{}
	for _, a := range []string{onlyBd, both, untouched} {
		bdComment(t, fake, a, fmt.Sprintf(`{"BeadAnchor":%q,"State":"running","Attempt":1,"SessionID":"old-a1"}`, a))
	}
	rows, err := fake.run(ctx, "", "", "list", "--label-pattern", "gonk-anchor:*", "--limit", "0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(rows, &listed); err != nil {
		t.Fatal(err)
	}
	for _, r := range listed {
		ids[r.Labels[0][len("gonk-anchor:"):]] = r.ID
	}
	if rep := importOK(t, s, bd); rep.Written != 3 {
		t.Fatalf("first import = %+v", rep)
	}

	// The Postgres-backed binary moves one record on.
	cur := mustPgGet(t, s, both)
	cur.Attempt, cur.SessionID = 2, "pg-a2"
	if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
		t.Fatal(err)
	}
	// Rolled back: the revision-less binary appends revision-less records.
	for _, a := range []string{onlyBd, both} {
		if _, err := fake.run(ctx, "", "", "comment", ids[a],
			fmt.Sprintf(`<!-- gonk-state {"BeadAnchor":%q,"State":"done","Attempt":1,"SessionID":"old-a1"} -->`, a)); err != nil {
			t.Fatal(err)
		}
	}

	rep := importOK(t, s, bd)
	if rep.Written != 1 || rep.InSync != 1 || !slices.Equal(rep.Conflicts, []string{both}) {
		t.Fatalf("import after a rollback to a revision-less binary = %+v; want %s written, %s in sync, %s a conflict",
			rep, onlyBd, untouched, both)
	}
	if got := mustPgGet(t, s, onlyBd); got.State != beadstore.StateDone {
		t.Fatalf("%s: Postgres %s, want the rolled-back binary's done record", onlyBd, got.State)
	}
	if got := mustPgGet(t, s, both); got.SessionID != "pg-a2" {
		t.Fatalf("%s (conflict): Postgres changed to %+v", both, got)
	}
}

// The export's side of the same hole: after a revision-less binary wrote a
// record that Postgres ALSO moved, the export must report a conflict, not write
// over the rolled-back binary's write because both revisions read "".
func TestExportSeesWritesByABinaryThatHasNoRevisions(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const a = "gonk:75:issue:1"
	bdComment(t, fake, a, `{"BeadAnchor":"gonk:75:issue:1","State":"running","Attempt":1}`)
	importOK(t, s, bd)
	cur := mustPgGet(t, s, a)
	cur.SessionID = "pg"
	if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
		t.Fatal(err)
	}
	// bdComment made bead bd-1; the revision-less binary appends to it.
	if _, err := fake.run(ctx, "", "", "comment", "bd-1", `<!-- gonk-state {"BeadAnchor":"gonk:75:issue:1","State":"done","Attempt":1} -->`); err != nil {
		t.Fatal(err)
	}
	rep, err := s.ExportToBd(ctx, bd, syncLog())
	if err != nil || !slices.Equal(rep.Conflicts, []string{a}) || rep.Written != 0 {
		t.Fatalf("export = %+v, %v; want a conflict and nothing written", rep, err)
	}
	if got := mustBdGet(t, bd, a); got.State != beadstore.StateDone {
		t.Fatalf("bd record overwritten: %+v", got)
	}
}

// REVIEW DEFECT A1 (reproduced by the independent review of 5499ee0). The
// gonk-gate on the reference cluster writes no Revision, so every record the import copies has
// revision "". The dispatch path that conditions on "" while a record exists is
// reservePendingPrompt's unreadable-store branch (broker_inject.go): Get fails,
// it reserves with PutIf(pending, ""), the dispatch fails, and the release puts
// back `nothingDispatched` -- State "" and SessionID "" -- over its own
// reservation. If PutIf("") matches the imported record, the live session's
// running record is destroyed: invisible to List, its agent orphaned.
//
// With the fix the reservation is refused (ErrStale: a record exists), nothing
// is written, and the running record is intact.
func TestImportedRevisionlessRecordCannotBeDestroyedByPutIfEmpty(t *testing.T) {
	s, _ := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const a = "gonk:75:issue:73"
	bdComment(t, fake, a, `{"BeadAnchor":"gonk:75:issue:73","State":"running","Attempt":2,"SessionID":"gonk.triage.p75.i73.a2.FYKO","ReservationID":"rsv-9"}`)
	importOK(t, s, bd)

	imported := mustPgGet(t, s, a)
	if imported.Revision == "" {
		t.Fatal("the import stored a record with an empty revision: PutIf(\"\") can match it")
	}

	// The dispatch: store read failed, so it reserves conditioned on "".
	pending := beadstore.Record{BeadAnchor: a, State: beadstore.StatePendingPrompt, Attempt: 2,
		SessionID: "gonk.triage.p75.i73.a2.ZZZZ"}
	rev, err := s.PutIf(ctx, pending, "")
	if err == nil {
		// Without the fix: the dispatch then fails and releases.
		nothingDispatched := beadstore.Record{BeadAnchor: a, Attempt: 2}
		if _, rerr := s.PutIf(ctx, nothingDispatched, rev); rerr != nil {
			t.Fatalf("release = %v", rerr)
		}
		got := mustPgGet(t, s, a)
		t.Fatalf("PutIf(\"\") overwrote the imported running record; after the release it is State=%q SessionID=%q "+
			"-- the live session %s is orphaned and invisible to List", got.State, got.SessionID, imported.SessionID)
	}
	if !errors.Is(err, beadstore.ErrStale) {
		t.Fatalf("PutIf(\"\") over an imported record = %v, want ErrStale", err)
	}
	if got := mustPgGet(t, s, a); !reflect.DeepEqual(got, imported) {
		t.Fatalf("record changed by a refused write: %+v", got)
	}
	if recs, err := s.List(ctx, beadstore.StateRunning); err != nil || len(recs) != 1 {
		t.Fatalf("List(running) = %d records, %v; want the imported session", len(recs), err)
	}
}

// REVIEW DEFECT A2. The sync base's bd side must be a function of what bd
// STORES -- the gonk-state comment's text -- and of nothing in this binary. The
// first digest was sha256 of the decoded Record's JSON, so adding one field to
// Record changed every digest, every anchor Postgres had moved became a
// permanent conflict, and workstore-export exited 1 on all of them (the review
// simulated it by rewriting bd_digest). Pinned here by computing the digest
// independently of package beadstore: sha256 of the comment text exactly as
// `bd comments --json` returns it.
//
// The record deliberately carries a key Record does not have ("FutureField"),
// as a comment from a newer binary would, and lacks keys Record does have, as
// every comment from the maintainers' binary does: neither may matter.
func TestSyncBaseDigestsBdsRawCommentText(t *testing.T) {
	s, dsn := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const a = "gonk:75:issue:7"
	body := `{"BeadAnchor":"gonk:75:issue:7","State":"parked","Attempt":1,"FutureField":{"x":1}}`
	bdComment(t, fake, a, body)
	importOK(t, s, bd)

	sum := sha256.Sum256([]byte("<!-- gonk-state " + body + " -->"))
	var stored string
	if err := rawConn(t, dsn).QueryRow(ctx, `SELECT bd_digest FROM work_items_bd_sync WHERE anchor = $1`, a).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hex.EncodeToString(sum[:]) {
		t.Fatalf("bd_digest = %s, want sha256 of the raw comment text %s: the sync base depends on something other "+
			"than what bd stores (a Record field added later would turn every moved anchor into a conflict)",
			stored, hex.EncodeToString(sum[:]))
	}

	// And behaviourally: Postgres moves; bd does not; nothing conflicts either way.
	cur := mustPgGet(t, s, a)
	cur.State = beadstore.StateRunning
	if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
		t.Fatal(err)
	}
	if rep := importOK(t, s, bd); rep.InSync != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("re-import = %+v, want in sync", rep)
	}
	rep, err := s.ExportToBd(ctx, bd, syncLog())
	if err != nil || rep.Written != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("export = %+v, %v; want Postgres's move written to bd, no conflict", rep, err)
	}
}

// missingBdDatabase is a bd that answers every call with bd 1.1.0's exact
// message for a Dolt server without bd's database.
func missingBdDatabase() *beadstore.BdCLI {
	return &beadstore.BdCLI{Run: func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New(`beadstore: bd list --label-pattern gonk-anchor:* --limit 0 --json: exit status 1: ` +
			`Error: failed to open database: database "bd_gonk" not found on Dolt server at gonk-dolt:3307`)
	}}
}

// A3a (the review ran this for real): on a fresh Dolt the import runs before gc
// start creates bd's database, and bd says the database does not exist. That is
// an EMPTY, COMPLETE import -- the store is marked imported and the pod can
// start -- not a failure that crash-loops the initContainer forever.
func TestImportOnAFreshInstallWithNoBdDatabaseIsEmptyAndComplete(t *testing.T) {
	s, _ := newWorkStore(t)
	rep, err := s.ImportFromBd(context.Background(), missingBdDatabase(), syncLog(), beadstore.ImportOptions{FreshInstall: true})
	if rep.BdDatabase != "bd_gonk" {
		t.Errorf("report bd_database = %q, want the name bd looked for (a misconfigured name reports the same error)", rep.BdDatabase)
	}
	if err != nil || !rep.Complete() || !rep.BdDatabaseMissing || rep.Seen != 0 {
		t.Fatalf("import on a fresh install = %+v, %v; want complete, bd_database_missing, nothing seen", rep, err)
	}
	if done, err := s.BdImportCompleted(context.Background()); err != nil || !done {
		t.Fatalf("BdImportCompleted = %v, %v; a fresh install must be able to start", done, err)
	}
}

// DEFECT 1 (verification of the rework): on an EXISTING install a wrong
// dolt_database gives bd's "database not found" -- the same message a fresh
// install gives. Without a positive signal that this IS a fresh install, the
// import must not read it as an empty, complete import: that would mark the
// store imported and start the controller without bd's in-flight records.
func TestImportWithBdDatabaseMissingAndNoFreshInstallSignalFailsAndMarksNothing(t *testing.T) {
	s, _ := newWorkStore(t)
	rep, err := s.ImportFromBd(context.Background(), missingBdDatabase(), syncLog(), beadstore.ImportOptions{})
	if done, derr := s.BdImportCompleted(context.Background()); derr != nil || done {
		t.Errorf("BdImportCompleted = %v, %v; an existing install with a wrong database name must NOT be marked imported", done, derr)
	}
	if err == nil || !errors.Is(err, beadstore.ErrBdDatabaseMissing) {
		t.Fatalf("import with bd's database missing and no fresh-install signal = %+v, %v; want ErrBdDatabaseMissing", rep, err)
	}
	if !strings.Contains(err.Error(), "bd_gonk") {
		t.Errorf("error %q does not name the database bd looked for", err)
	}
}

// The fresh-install signal is not trusted over the store itself: a work store
// that already holds work records has been running, so bd's database being
// missing is a wrong name (or a lost Dolt), not a fresh install. The import
// refuses rather than re-stamping the pod as imported.
func TestImportWithBdDatabaseMissingOnAStoreHoldingRecordsFailsEvenWhenToldFresh(t *testing.T) {
	s, _ := newWorkStore(t)
	ctx := context.Background()
	if _, err := s.PutIf(ctx, beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateRunning, SessionID: "live"}, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.ImportFromBd(ctx, missingBdDatabase(), syncLog(), beadstore.ImportOptions{FreshInstall: true})
	if !errors.Is(err, beadstore.ErrNotAFreshInstall) || !errors.Is(err, beadstore.ErrBdDatabaseMissing) {
		t.Fatalf("import = %v; want ErrNotAFreshInstall: the store holds records, so this is not a fresh install", err)
	}
	if done, derr := s.BdImportCompleted(ctx); derr != nil || done {
		t.Fatalf("BdImportCompleted = %v, %v; want unmarked", done, derr)
	}
}

// The survey's import plan follows the same rule: without the signal a missing
// database is an error, not a plan that says "nothing to do".
func TestPlanImportWithBdDatabaseMissingAndNoFreshInstallSignalFails(t *testing.T) {
	s, _ := newWorkStore(t)
	if _, err := s.PlanImportFromBd(context.Background(), missingBdDatabase(), syncLog(), beadstore.ImportOptions{}); !errors.Is(err, beadstore.ErrNotAFreshInstall) {
		t.Fatalf("plan = %v; want ErrNotAFreshInstall", err)
	}
}

// An OUTAGE is not a fresh install: the import fails and marks nothing.
func TestImportWithBdUnreachableFailsAndMarksNothing(t *testing.T) {
	s, _ := newWorkStore(t)
	defer beadstore.SetBdReadBackoff(time.Millisecond)()
	calls := 0
	bd := &beadstore.BdCLI{Run: func(context.Context, string, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("Error: failed to open database: dial tcp 192.0.2.1:3307: connect: connection refused")
	}}
	if _, err := s.ImportFromBd(context.Background(), bd, syncLog(), beadstore.ImportOptions{}); err == nil {
		t.Fatal("import with bd unreachable succeeded")
	}
	if calls != 3 {
		t.Fatalf("bd enumeration tried %d times, want 3 (retried)", calls)
	}
	if done, _ := s.BdImportCompleted(context.Background()); done {
		t.Fatal("a failed import marked the store imported")
	}
}

// A3b: one anchor whose read fails once is retried, and the import completes.
func TestImportRetriesAnAnchorReadThatFailsOnce(t *testing.T) {
	s, _ := newWorkStore(t)
	defer beadstore.SetBdReadBackoff(time.Millisecond)()
	fake := newFakeBd()
	ctx := context.Background()
	good := &beadstore.BdCLI{Run: fake.run}
	for i := 0; i < 3; i++ {
		if err := good.Put(ctx, beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:1:issue:%d", i), State: beadstore.StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	failedOnce := false
	flaky := &beadstore.BdCLI{Run: func(ctx context.Context, dir, bin string, args ...string) ([]byte, error) {
		if !failedOnce && len(args) > 1 && args[0] == "comments" && args[1] == "bd-2" {
			failedOnce = true
			return nil, errors.New("dolt: connection reset by peer")
		}
		return fake.run(ctx, dir, bin, args...)
	}}
	rep := importOK(t, s, flaky)
	if !rep.Complete() || rep.Written != 3 || !failedOnce {
		t.Fatalf("import = %+v (failed once: %v); want all 3 written after a retry", rep, failedOnce)
	}
}

// A bd call against a stalled Dolt hangs (bd has no timeout). Each try is
// bounded, so a stalled bd fails the import in bounded time instead of hanging
// the controller's initContainer forever.
func TestImportAgainstAHungBdFailsInBoundedTime(t *testing.T) {
	s, _ := newWorkStore(t)
	defer beadstore.SetBdReadBackoff(time.Millisecond)()
	defer beadstore.SetBdReadTimeout(100 * time.Millisecond)()
	bd := &beadstore.BdCLI{Run: func(ctx context.Context, _, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done() // exec.CommandContext kills the process when ctx ends
		return nil, ctx.Err()
	}}
	start := time.Now()
	_, err := s.ImportFromBd(context.Background(), bd, syncLog(), beadstore.ImportOptions{})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("import against a hung bd = %v after %v; want a failure within a few tries' timeouts", err, time.Since(start))
	}
}

// OWNER DECISION B5: QUARANTINE THE BACKLOG. Records the bd-backed sweep could
// not see (a lost state label; past bd's 50-row default) and records nothing has
// written for over 24h are imported as needs-human -- which no sweep List reads
// -- with the original kept in work_items_quarantine. Everything else imports as
// it is. Without this the first Postgres-backed sweep acts on the whole backlog.
func TestImportQuarantinesTheBacklog(t *testing.T) {
	s, dsn := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	now := time.Now().UTC()
	put := func(r beadstore.Record, at time.Time) {
		t.Helper()
		fake.setNow(at)
		if err := bd.Put(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	fresh := now.Add(-10 * time.Minute)
	// 55 running records, all fresh: the bd-backed List saw the first 50 in
	// bd's order; the last 5 no sweep ever judged.
	for i := 0; i < 55; i++ {
		put(beadstore.Record{BeadAnchor: fmt.Sprintf("gonk:9:issue:%03d", i), State: beadstore.StateRunning, SessionID: "s"}, fresh)
	}
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:1", State: beadstore.StateParked}, fresh)                         // normal
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:2", State: beadstore.StateParked}, now.Add(-48*time.Hour))        // stale
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:3", State: beadstore.StatePendingPrompt}, fresh)                  // torn label below
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:4", State: beadstore.StateDone}, now.Add(-90*24*time.Hour))       // not swept: normal
	put(beadstore.Record{BeadAnchor: "gonk:1:issue:5", State: beadstore.StateNeedsHuman}, now.Add(-90*24*time.Hour)) // not swept
	torn, _ := fake.run(ctx, "", "", "list", "--label", "gonk-anchor:gonk:1:issue:3", "--json")
	var tornRows []struct{ ID string }
	if err := json.Unmarshal(torn, &tornRows); err != nil || len(tornRows) != 1 {
		t.Fatalf("find torn bead: %v %s", err, torn)
	}
	if _, err := fake.run(ctx, "", "", "label", "remove", tornRows[0].ID, "gonk::pending-prompt"); err != nil {
		t.Fatal(err)
	}

	rep := importOK(t, s, bd)
	wantReasons := map[string]int{beadstore.QuarantineHidden: 6, beadstore.QuarantineStale: 1}
	if !rep.Complete() || rep.Written != 60 || rep.Quarantined != 7 || !reflect.DeepEqual(rep.QuarantineByReason, wantReasons) {
		t.Fatalf("report = %+v; want 60 written, 7 quarantined %v", rep, wantReasons)
	}

	running, err := s.List(ctx, beadstore.StateRunning)
	if err != nil || len(running) != 50 {
		t.Fatalf("List(running) = %d, %v; want exactly the 50 the bd-backed sweep could see", len(running), err)
	}
	for _, a := range []string{"gonk:1:issue:2", "gonk:1:issue:3"} {
		if got := mustPgGet(t, s, a); got.State != beadstore.StateNeedsHuman {
			t.Errorf("%s: state %s, want needs-human (quarantined)", a, got.State)
		}
	}
	for a, st := range map[string]beadstore.State{"gonk:1:issue:1": beadstore.StateParked, "gonk:1:issue:4": beadstore.StateDone} {
		if got := mustPgGet(t, s, a); got.State != st {
			t.Errorf("%s: state %s, want %s (not quarantined)", a, got.State, st)
		}
	}
	for _, st := range []beadstore.State{beadstore.StateParked, beadstore.StatePendingPrompt} {
		recs, _ := s.List(ctx, st)
		for _, r := range recs {
			if r.BeadAnchor == "gonk:1:issue:2" || r.BeadAnchor == "gonk:1:issue:3" {
				t.Errorf("List(%s) returned quarantined %s", st, r.BeadAnchor)
			}
		}
	}
	var n int
	var reason, orig string
	c := rawConn(t, dsn)
	if err := c.QueryRow(ctx, `SELECT count(*) FROM work_items_quarantine`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("work_items_quarantine rows = %d, %v; want 7", n, err)
	}
	if err := c.QueryRow(ctx, `SELECT reason, original_state FROM work_items_quarantine WHERE anchor = 'gonk:1:issue:2'`).Scan(&reason, &orig); err != nil ||
		reason != beadstore.QuarantineStale || orig != "parked" {
		t.Fatalf("quarantine row = %q %q %v", reason, orig, err)
	}

	// The survey predicts exactly this, with ages.
	sv, err := beadstore.SurveyBd(ctx, bd, syncLog())
	if err != nil || sv.QuarantineTotal != 7 || !reflect.DeepEqual(sv.Quarantine, wantReasons) {
		t.Fatalf("survey quarantine = %d %v, %v; want the import's 7 %v", sv.QuarantineTotal, sv.Quarantine, err, wantReasons)
	}
	if sv.AgeBuckets["parked"][">30d"] != 0 || sv.AgeBuckets["parked"]["1d-7d"] != 1 || sv.AgeBuckets["done"][">30d"] != 1 {
		t.Fatalf("age buckets = %v", sv.AgeBuckets)
	}
	ages := 0
	for _, r := range sv.Records {
		if r.AgeHours != nil {
			ages++
		}
	}
	if len(sv.Records) != 60 || ages != 60 {
		t.Fatalf("survey records = %d, with ages %d; want 60 and 60", len(sv.Records), ages)
	}

	// Re-import: nothing new is quarantined (the records are in sync).
	if again := importOK(t, s, bd); again.Quarantined != 0 || again.InSync != 60 {
		t.Fatalf("re-import = %+v", again)
	}
}

// Review follow-up: a conflict used to be re-reported on every pod start with no
// way to see why without reading logs, and no way to pick a side. The plan
// (survey) names it with its detail and writes nothing; ResolveConflict makes a
// human's choice stick in BOTH directions.
func TestConflictIsPlannedThenResolvedEitherWay(t *testing.T) {
	for _, take := range []string{beadstore.TakeBd, beadstore.TakePostgres} {
		t.Run(take, func(t *testing.T) {
			s, dsn := newWorkStore(t)
			fake := newFakeBd()
			bd := &beadstore.BdCLI{Run: fake.run}
			ctx := context.Background()
			const a = "gonk:1:issue:1"
			if err := bd.Put(ctx, beadstore.Record{BeadAnchor: a, State: beadstore.StateRunning, SessionID: "orig"}); err != nil {
				t.Fatal(err)
			}
			importOK(t, s, bd)
			cur := mustPgGet(t, s, a)
			cur.SessionID = "pg"
			if _, err := s.PutIf(ctx, cur, cur.Revision); err != nil {
				t.Fatal(err)
			}
			b := mustBdGet(t, bd, a)
			b.SessionID = "bd"
			if err := bd.Put(ctx, b); err != nil {
				t.Fatal(err)
			}

			var syncBefore string
			c := rawConn(t, dsn)
			if err := c.QueryRow(ctx, `SELECT bd_digest || pg_revision FROM work_items_bd_sync WHERE anchor = $1`, a).Scan(&syncBefore); err != nil {
				t.Fatal(err)
			}
			plan, err := s.PlanImportFromBd(ctx, bd, syncLog(), beadstore.ImportOptions{})
			if err != nil || !slices.Equal(plan.Conflicts, []string{a}) || plan.ConflictDetails[a] == "" || plan.Direction != "import-plan" {
				t.Fatalf("plan = %+v, %v; want the conflict with its detail", plan, err)
			}
			var syncAfter string
			if err := c.QueryRow(ctx, `SELECT bd_digest || pg_revision FROM work_items_bd_sync WHERE anchor = $1`, a).Scan(&syncAfter); err != nil || syncAfter != syncBefore {
				t.Fatalf("the plan wrote the sync base: %q -> %q (%v)", syncBefore, syncAfter, err)
			}
			if got := mustPgGet(t, s, a); got.SessionID != "pg" {
				t.Fatalf("the plan wrote work_items: %+v", got)
			}

			if err := s.ResolveConflict(ctx, bd, a, take, syncLog()); err != nil {
				t.Fatalf("ResolveConflict(%s) = %v", take, err)
			}
			want := map[string]string{beadstore.TakeBd: "bd", beadstore.TakePostgres: "pg"}[take]
			if rep := importOK(t, s, bd); len(rep.Conflicts) != 0 || mustPgGet(t, s, a).SessionID != want {
				t.Fatalf("import after resolving to %s = %+v, Postgres session %q; want no conflict and %q",
					take, rep, mustPgGet(t, s, a).SessionID, want)
			}
			rep, err := s.ExportToBd(ctx, bd, syncLog())
			if err != nil || len(rep.Conflicts) != 0 || mustBdGet(t, bd, a).SessionID != want {
				t.Fatalf("export after resolving to %s = %+v, %v, bd session %q; want no conflict and %q",
					take, rep, err, mustBdGet(t, bd, a).SessionID, want)
			}
			if again := importOK(t, s, bd); len(again.Conflicts) != 0 || again.InSync != 1 {
				t.Fatalf("import after the export = %+v; want in sync", again)
			}
		})
	}
}

// gonk-p44x (b): QuarantineAgeUnknown had no test. A bd record whose current
// comment carries no usable created_at AND whose Record has no UpdatedAt has an
// UNKNOWN age: nothing can say whether a sweep looked at it this hour or last
// month, so the import must quarantine it rather than let the first Postgres
// sweep act on it.
func TestImportQuarantinesARecordOfUnknownAge(t *testing.T) {
	s, dsn := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()

	// Built by hand: a bead with the anchor and state labels, and a gonk-state
	// comment with NO UpdatedAt, stamped with a zero created_at (bd comments
	// predating created_at, and the go-ffoe shape, both look like this).
	const anchor = "gonk:7:issue:7"
	fake.setNow(time.Time{})
	created, err := fake.run(ctx, "", "", "create", "--title", anchor, "--labels", "gonk-anchor:"+anchor, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(created, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.run(ctx, "", "", "label", "add", out.ID, "gonk::running"); err != nil {
		t.Fatal(err)
	}
	payload := `{"BeadAnchor":"` + anchor + `","State":"running","SessionID":"live","Revision":"r1"}`
	if _, err := fake.run(ctx, "", "", "comment", out.ID, "<!-- gonk-state "+payload+" -->"); err != nil {
		t.Fatal(err)
	}

	// Precondition: the bd-backed List DOES see it, so "hidden" cannot be the
	// reason and only the unknown age can quarantine it.
	sv, err := beadstore.SurveyBd(ctx, bd, syncLog())
	if err != nil || len(sv.Records) != 1 || !sv.Records[0].VisibleToBdList || sv.Records[0].AgeHours != nil {
		t.Fatalf("survey = %+v, %v; want one record the bd List sees, with no age", sv.Records, err)
	}
	if sv.Records[0].Quarantine != beadstore.QuarantineAgeUnknown {
		t.Errorf("survey quarantine reason = %q, want %q", sv.Records[0].Quarantine, beadstore.QuarantineAgeUnknown)
	}

	rep := importOK(t, s, bd)
	if rep.Quarantined != 1 || rep.QuarantineByReason[beadstore.QuarantineAgeUnknown] != 1 {
		t.Fatalf("import = %+v; want 1 quarantined for %s", rep, beadstore.QuarantineAgeUnknown)
	}
	if got := mustPgGet(t, s, anchor); got.State != beadstore.StateNeedsHuman {
		t.Errorf("state = %s, want needs-human", got.State)
	}
	var reason, orig string
	if err := rawConn(t, dsn).QueryRow(ctx,
		`SELECT reason, original_state FROM work_items_quarantine WHERE anchor = $1`, anchor).Scan(&reason, &orig); err != nil ||
		reason != beadstore.QuarantineAgeUnknown || orig != "running" {
		t.Fatalf("quarantine row = %q %q, %v", reason, orig, err)
	}
}

// gonk-p44x (c): "the never-synced same-record comparison ignores Revision" had
// no test. The import gives a revision-less bd record a fresh revision, so the
// two sides differ in Revision ALONE. If the sync base is then lost -- the row
// is deleted, or a restore predates it -- a re-import sees "never synced" and
// must recognise the same write rather than report a conflict on a difference
// it created itself.
func TestNeverSyncedSameRecordIsNotAConflictOverTheImportsOwnRevision(t *testing.T) {
	s, dsn := newWorkStore(t)
	fake := newFakeBd()
	bd := &beadstore.BdCLI{Run: fake.run}
	ctx := context.Background()
	const anchor = "gonk:8:issue:8"
	// A revision-less record, the shape the maintainers' binary wrote.
	created, err := fake.run(ctx, "", "", "create", "--title", anchor, "--labels", "gonk-anchor:"+anchor, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(created, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.run(ctx, "", "", "label", "add", out.ID, "gonk::parked"); err != nil {
		t.Fatal(err)
	}
	payload := `{"BeadAnchor":"` + anchor + `","State":"parked","SessionID":"s1","UpdatedAt":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if _, err := fake.run(ctx, "", "", "comment", out.ID, "<!-- gonk-state "+payload+" -->"); err != nil {
		t.Fatal(err)
	}

	if rep := importOK(t, s, bd); rep.Written != 1 {
		t.Fatalf("first import = %+v; want the record written", rep)
	}
	if got := mustPgGet(t, s, anchor); got.Revision == "" {
		t.Fatal("the imported record kept an empty revision")
	}
	// Lose the sync base, keeping both sides exactly as they are.
	if _, err := rawConn(t, dsn).Exec(ctx, `DELETE FROM work_items_bd_sync WHERE anchor = $1`, anchor); err != nil {
		t.Fatal(err)
	}

	rep := importOK(t, s, bd)
	if len(rep.Conflicts) != 0 || rep.InSync != 1 || rep.Written != 0 {
		t.Fatalf("re-import with no sync base = %+v; want in sync, no conflict (only the revision the import itself assigned differs): %v",
			rep, rep.ConflictDetails)
	}
}
