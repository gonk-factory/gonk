package beadstore

import (
	"context"
	"encoding/json"
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
	// 3: comment <id> <marker>
	f.push([]byte(``), nil)
	// 4: label list <id> --json -- no gonk:: label yet.
	f.push([]byte(`["gonk-anchor:gonk:1:issue:1"]`), nil)
	// 5: label add <id> gonk::running
	f.push([]byte(``), nil)

	b := &BdCLI{Run: f.run}
	rec := Record{BeadAnchor: "gonk:1:issue:1", State: StateRunning, Attempt: 1}
	if err := b.Put(context.Background(), rec); err != nil {
		t.Fatalf("Put = %v", err)
	}

	want := map[int][]string{
		0: {"list", "--label", "gonk-anchor:gonk:1:issue:1", "--json"},
		1: {"create", "--title", "gonk:1:issue:1", "--labels", "gonk-anchor:gonk:1:issue:1", "--json"},
		3: {"label", "list", "bd-1", "--json"},
		4: {"label", "add", "bd-1", "gonk::running"},
	}
	if len(f.calls) != 5 {
		t.Fatalf("got %d bd invocations, want 5: %v", len(f.calls), f.calls)
	}
	for i, w := range want {
		if !reflect.DeepEqual(f.calls[i], w) {
			t.Errorf("call %d = %v, want %v", i, f.calls[i], w)
		}
	}
	// call 2 is `comment <id> <marker>` -- the marker is a JSON blob whose
	// exact bytes (timestamps included) aren't the point of this test; only
	// the command shape (verb + id, no --format/--json flag) is.
	if len(f.calls[2]) != 3 || f.calls[2][0] != "comment" || f.calls[2][1] != "bd-1" {
		t.Errorf("call 2 = %v, want [comment bd-1 <marker>]", f.calls[2])
	}
}

// TestBdCLIRemovesEveryExistingGonkStateLabelNotJustAGlob is the regression
// test for the glob bug: `bd label remove <id> "gonk::*"` does not
// glob-expand on the real binary, so Put must enumerate the bead's actual
// labels and remove each `gonk::`-prefixed one by its literal name.
func TestBdCLIRemovesEveryExistingGonkStateLabelNotJustAGlob(t *testing.T) {
	f := &fakeBd{t: t}
	f.push([]byte(`[{"id":"bd-2"}]`), nil) // findBeadID: found
	f.push([]byte(``), nil)                // comment
	// Two prior gonk:: labels somehow present (e.g. a re-sling): both must
	// be removed individually, by their real names, never by a glob.
	f.push([]byte(`["gonk-anchor:x","gonk::running","gonk::parked"]`), nil) // label list
	f.push([]byte(``), nil)                                                 // label remove gonk::running
	f.push([]byte(``), nil)                                                 // label remove gonk::parked
	f.push([]byte(``), nil)                                                 // label add gonk::done

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
	last := f.calls[len(f.calls)-1]
	if !reflect.DeepEqual(last, []string{"label", "add", "bd-2", "gonk::done"}) {
		t.Fatalf("last call = %v, want label add bd-2 gonk::done", last)
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
