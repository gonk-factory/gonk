package beadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// fakeBd records every invocation's argv and answers from a queue of
// canned responses, in call order -- enough to pin BdCLI's exact command
// shapes without a real `bd` binary. (Task 6 Step 5's real end-to-end
// round trip, against the actual bd 1.0.3 binary, was run by hand during
// that task -- see images/Dockerfile.controller / test/images -- and is
// what this test's fixed argv shapes were transcribed from.)
type fakeBd struct {
	t         *testing.T
	calls     [][]string
	responses [][]byte
	errs      []error
	i         int
}

func (f *fakeBd) run(_ context.Context, _, _ string, args ...string) ([]byte, error) {
	f.t.Helper()
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.i >= len(f.responses) {
		f.t.Fatalf("unexpected extra bd invocation #%d: %v", f.i, args)
	}
	out, err := f.responses[f.i], f.errs[f.i]
	f.i++
	return out, err
}

func (f *fakeBd) push(out []byte, err error) {
	f.responses = append(f.responses, out)
	f.errs = append(f.errs, err)
}

// TestBdCLIInvokesTheRealSubcommandShapes pins the exact argv shapes Put,
// Get and List send to `bd`, confirmed against the real bd 1.0.3 binary
// (Task 6 Step 5). Three of these shapes replace an earlier, WRONG guess
// that silently no-oped or mis-decoded against the real CLI (see bd.go's
// package doc): `label remove/add` takes the verb immediately after
// `label` (not after the id), every JSON-producing call uses the bare
// `--json` flag (not `--format json`), and comment bodies decode from the
// "text" field (not "body").
func TestBdCLIInvokesTheRealSubcommandShapes(t *testing.T) {
	f := &fakeBd{t: t}
	// 1: findBeadID's `list --label <anchor-label> --json` -- not found.
	f.push([]byte(`[]`), nil)
	// 2: create --title <anchor> --labels <anchor-label> --json
	f.push([]byte(`{"id":"bd-1"}`), nil)
	// 3: label list <id> --json -- no gonk:: label yet.
	f.push([]byte(`["gonk-anchor:gonk:1:issue:1"]`), nil)
	// 4: label add <id> gonk::running -- BEFORE the record (see Put's doc:
	//    at every kill point the bead carries its current record's label).
	f.push([]byte(``), nil)
	// 5: comment <id> <marker>
	f.push([]byte(``), nil)
	// 6: settleLabels' `comments <id> --json` (answered empty here, so it
	//    stops: this test is about argv shapes, not resolution).
	f.push([]byte(`[]`), nil)

	b := &BdCLI{Run: f.run}
	rec := Record{BeadAnchor: "gonk:1:issue:1", State: StateRunning, Attempt: 1}
	if err := b.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put = %v", err)
	}

	want := map[int][]string{
		0: {"list", "--label", "gonk-anchor:gonk:1:issue:1", "--json"},
		1: {"create", "--title", "gonk:1:issue:1", "--labels", "gonk-anchor:gonk:1:issue:1", "--json"},
		2: {"label", "list", "bd-1", "--json"},
		3: {"label", "add", "bd-1", "gonk::running"},
		5: {"comments", "bd-1", "--json"},
	}
	if len(f.calls) != 6 {
		t.Fatalf("got %d bd invocations, want 6: %v", len(f.calls), f.calls)
	}
	for i, w := range want {
		if !reflect.DeepEqual(f.calls[i], w) {
			t.Errorf("call %d = %v, want %v", i, f.calls[i], w)
		}
	}
	// call 4 is `comment <id> <marker>` -- the marker is a JSON blob whose
	// exact bytes (timestamps included) aren't the point of this test; only
	// the command shape (verb + id, no --format/--json flag) is.
	if len(f.calls[4]) != 3 || f.calls[4][0] != "comment" || f.calls[4][1] != "bd-1" {
		t.Errorf("call 4 = %v, want [comment bd-1 <marker>]", f.calls[4])
	}
}

// TestBdCLIRemovesEveryExistingGonkStateLabelNotJustAGlob is the regression
// test for the glob bug: `bd label remove <id> "gonk::*"` does not
// glob-expand on the real binary, so Put must enumerate the bead's actual
// labels and remove each `gonk::`-prefixed one by its literal name.
func TestBdCLIRemovesEveryExistingGonkStateLabelNotJustAGlob(t *testing.T) {
	f := &fakeBd{t: t}
	f.push([]byte(`[{"id":"bd-2"}]`), nil) // findBeadID: found
	// Two prior gonk:: labels somehow present (e.g. a re-sling): both must
	// be removed individually, by their real names, never by a glob.
	f.push([]byte(`["gonk-anchor:x","gonk::running","gonk::parked"]`), nil) // label list
	f.push([]byte(``), nil)                                                 // label add gonk::done
	f.push([]byte(``), nil)                                                 // comment
	f.push([]byte(``), nil)                                                 // label remove gonk::running
	f.push([]byte(``), nil)                                                 // label remove gonk::parked
	f.push([]byte(`[]`), nil)                                               // settleLabels: comments

	b := &BdCLI{Run: f.run}
	rec := Record{BeadAnchor: "gonk:2:issue:9", State: StateDone}
	if err := b.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put = %v", err)
	}

	removed := map[string]bool{}
	for _, c := range f.calls {
		if len(c) == 4 && c[0] == "label" && c[1] == "remove" {
			removed[c[3]] = true
		}
	}
	if !removed["gonk::running"] || !removed["gonk::parked"] {
		t.Fatalf("expected both prior gonk:: labels removed by literal name, got calls: %v", f.calls)
	}
	if removed["gonk::*"] {
		t.Fatal("must never send a glob pattern to `bd label remove` -- the real binary does not expand it")
	}
	if !reflect.DeepEqual(f.calls[2], []string{"label", "add", "bd-2", "gonk::done"}) {
		t.Fatalf("call 2 = %v, want label add bd-2 gonk::done before the comment", f.calls[2])
	}
}

// TestBdCLIGetDecodesTheTextFieldNotBody is the regression test for the
// field-name bug: `bd comments <id> --json` names the comment content
// "text", and an earlier draft of getByID decoded a field called "body"
// that never exists on the real binary's output, silently returning "not
// found" no matter what Put had written.
func TestBdCLIGetDecodesTheTextFieldNotBody(t *testing.T) {
	rec := Record{BeadAnchor: "gonk:3:issue:1", State: StateParked, Attempt: 2}
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	marker := gonkStateMarkerPrefix + string(payload) + gonkStateMarkerSuffix
	commentsJSON, err := json.Marshal([]map[string]string{{"text": marker}})
	if err != nil {
		t.Fatalf("marshal comments: %v", err)
	}

	f := &fakeBd{t: t}
	f.push([]byte(`[{"id":"bd-3"}]`), nil) // findBeadID
	f.push(commentsJSON, nil)              // comments <id> --json

	b := &BdCLI{Run: f.run}
	got, ok, err := b.Get(context.Background(), "gonk:3:issue:1")
	if err != nil {
		t.Fatalf("Get = %v", err)
	}
	if !ok {
		t.Fatal("Get reported not-found against a comment that IS present under \"text\" -- " +
			"the body/text field-name regression is back")
	}
	if got.State != StateParked || got.Attempt != 2 {
		t.Fatalf("got %+v, want state=parked attempt=2", got)
	}
}

// TestBdCLISessionIDSurvivesTheCommentRoundTrip proves the correlation field
// crosses the bd boundary: Put JSON-encodes the whole Record into the
// gonk-state comment, and getByID must decode SessionID back out. This is the
// BdCLI mirror of the Memory round-trip -- the field must survive the one hop
// that actually leaves this process.
func TestBdCLISessionIDSurvivesTheCommentRoundTrip(t *testing.T) {
	rec := Record{BeadAnchor: "gonk:5:issue:2", State: StateRunning, SessionID: "gcs-abc123"}
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	marker := gonkStateMarkerPrefix + string(payload) + gonkStateMarkerSuffix
	commentsJSON, err := json.Marshal([]map[string]string{{"text": marker}})
	if err != nil {
		t.Fatalf("marshal comments: %v", err)
	}

	f := &fakeBd{t: t}
	f.push([]byte(`[{"id":"bd-5"}]`), nil) // findBeadID
	f.push(commentsJSON, nil)              // comments <id> --json

	b := &BdCLI{Run: f.run}
	got, ok, err := b.Get(context.Background(), "gonk:5:issue:2")
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if got.SessionID != "gcs-abc123" {
		t.Fatalf("SessionID = %q, want %q -- dispatch<->sweep correlation lost across the bd comment", got.SessionID, "gcs-abc123")
	}
}

// TestBdCLIGetLoadsPreBrokerCommentWithoutSessionID is the backward-compat
// guarantee: a gonk-state comment written before SessionID existed has no such
// JSON key, and must still decode -- with an empty SessionID, not an error.
func TestBdCLIGetLoadsPreBrokerCommentWithoutSessionID(t *testing.T) {
	// A pre-broker marker: whole-struct JSON that predates the field entirely.
	marker := gonkStateMarkerPrefix + `{"BeadAnchor":"gonk:6:issue:1","State":"done","Attempt":1}` + gonkStateMarkerSuffix
	commentsJSON, err := json.Marshal([]map[string]string{{"text": marker}})
	if err != nil {
		t.Fatalf("marshal comments: %v", err)
	}

	f := &fakeBd{t: t}
	f.push([]byte(`[{"id":"bd-6"}]`), nil) // findBeadID
	f.push(commentsJSON, nil)              // comments <id> --json

	b := &BdCLI{Run: f.run}
	got, ok, err := b.Get(context.Background(), "gonk:6:issue:1")
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v -- a pre-broker comment must still load", got, ok, err)
	}
	if got.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty on a pre-broker record", got.SessionID)
	}
	if got.State != StateDone || got.Attempt != 1 {
		t.Fatalf("got %+v, want state=done attempt=1", got)
	}
}

// TestBdCLIListUsesTheJSONFlagNotFormat pins List's argv shape too, so a
// future edit cannot quietly reintroduce `--format json`.
func TestBdCLIListUsesTheJSONFlagNotFormat(t *testing.T) {
	f := &fakeBd{t: t}
	f.push([]byte(`[]`), nil) // list --label gonk::running --json
	b := &BdCLI{Run: f.run}
	if _, err := b.List(context.Background(), StateRunning); err != nil {
		t.Fatalf("List = %v", err)
	}
	want := []string{"list", "--label", "gonk::running", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("List's bd invocation = %v, want %v", f.calls[0], want)
	}
}

// The bd import's enumeration (gonk-t41): by the ANCHOR label, so a bead whose
// state label was torn away is still found; --limit 0, so not only bd's default
// 50 (gonk-kx3); and NO --all, so exactly the beads findBeadID's default status
// filter showed the bd-backed binary. Flags checked against the real bd 1.1.0.
func TestBdCLIAnchorBeadsArgvAndGrouping(t *testing.T) {
	f := &fakeBd{t: t}
	f.push([]byte(`[
		{"id":"go-1","labels":["gonk-anchor:gonk:1:issue:1","gonk::running"]},
		{"id":"go-2","labels":["gonk::"]},
		{"id":"go-3","labels":["gonk-anchor:gonk:1:issue:1"]},
		{"id":"go-4","labels":["gonk-anchor:gonk:2:issue:9"]},
		{"id":"go-5","labels":["gonk-anchor:"]}
	]`), nil)
	b := &BdCLI{Run: f.run}
	got, labels, err := b.anchorBeads(context.Background())
	if err != nil {
		t.Fatalf("anchorBeads = %v", err)
	}
	wantArgv := []string{"list", "--label-pattern", "gonk-anchor:*", "--limit", "0", "--json"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], wantArgv) {
		t.Fatalf("argv = %v, want %v", f.calls, wantArgv)
	}
	want := map[string][]string{"gonk:1:issue:1": {"go-1", "go-3"}, "gonk:2:issue:9": {"go-4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("anchors = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(labels["go-1"], []string{"gonk-anchor:gonk:1:issue:1", "gonk::running"}) || len(labels) != 5 {
		t.Fatalf("labels = %v", labels)
	}
}

// A3 (fresh install): bd's message for a Dolt server that has no bd database --
// verbatim from the real bd 1.1.0 -- is ErrBdDatabaseMissing. An UNREACHABLE
// server is not: that is an outage, and the import must not treat it as "nothing
// to import".
func TestBdCLIRecognisesAMissingDatabaseButNotAnOutage(t *testing.T) {
	for name, c := range map[string]struct {
		stderr  string
		missing bool
	}{
		"fresh dolt": {`Error: failed to open database: database "bd_gonk" not found on Dolt server at 127.0.0.1:13317

Common causes:
  - Switched git branches (the Dolt database is runtime state, not in git)`, true},
		"dolt down": {`Error: failed to open database: dial tcp 127.0.0.1:3307: connect: connection refused`, false},
		"auth":      {`Error: failed to open database: Access denied for user 'root'`, false},
	} {
		b := &BdCLI{Run: func(context.Context, string, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("beadstore: bd list: exit status 1: %s", c.stderr)
		}}
		_, _, err := b.anchorBeads(context.Background())
		if err == nil || errors.Is(err, ErrBdDatabaseMissing) != c.missing {
			t.Errorf("%s: err = %v, ErrBdDatabaseMissing = %v, want %v", name, err, errors.Is(err, ErrBdDatabaseMissing), c.missing)
		}
	}
}
