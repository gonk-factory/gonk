package beadstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore/storetest"
)

// EVERY Store implementation runs the SAME suite. Adding a third store means
// adding a Test here; a store that is not wired in is a store nothing holds to
// the contract, which is precisely how BdCLI came to be the one implementation
// that did not stamp UpdatedAt.

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func() beadstore.Store { return beadstore.NewMemory() })
}

func TestBdCLIConformance(t *testing.T) {
	storetest.Run(t, func() beadstore.Store {
		return &beadstore.BdCLI{Run: newFakeBd().run}
	})
}

// fakeBd is an in-process model of the `bd` binary's storage, answering the
// exact argv shapes BdCLI sends (those shapes are pinned separately, against
// the real bd 1.0.3, by TestBdCLIInvokesTheRealSubcommandShapes in bd_test.go).
// It stores labels and comments and nothing else, because that is all bd is to
// this package: BdCLI keeps a Record as one `<!-- gonk-state {json} -->`
// comment plus a `gonk::<state>` label.
//
// It is deliberately dumb about the RECORD -- it never parses the JSON. That
// is what makes it a fair test subject for a suite about what BdCLI writes
// into that JSON: the fake cannot accidentally supply a field BdCLI failed to.
type fakeBd struct {
	mu       sync.Mutex
	next     int
	labels   map[string][]string
	comments map[string][]string
}

func newFakeBd() *fakeBd {
	return &fakeBd{labels: map[string][]string{}, comments: map[string][]string{}}
}

func (f *fakeBd) run(_ context.Context, _, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(args) == 4 && args[0] == "list" && args[1] == "--label" && args[3] == "--json":
		rows := []map[string]string{}
		for _, id := range f.ids() {
			if hasLabel(f.labels[id], args[2]) {
				rows = append(rows, map[string]string{"id": id})
			}
		}
		return json.Marshal(rows)

	case len(args) == 6 && args[0] == "create" && args[1] == "--title" && args[3] == "--labels":
		f.next++
		id := fmt.Sprintf("bd-%d", f.next)
		f.labels[id] = []string{args[4]}
		return json.Marshal(map[string]string{"id": id})

	case len(args) == 3 && args[0] == "comment":
		f.comments[args[1]] = append(f.comments[args[1]], args[2])
		return nil, nil

	case len(args) == 3 && args[0] == "comments" && args[2] == "--json":
		rows := []map[string]string{}
		for _, c := range f.comments[args[1]] {
			rows = append(rows, map[string]string{"text": c})
		}
		return json.Marshal(rows)

	case len(args) == 4 && args[0] == "label" && args[1] == "list" && args[3] == "--json":
		ls := f.labels[args[2]]
		if ls == nil {
			ls = []string{}
		}
		return json.Marshal(ls)

	case len(args) == 4 && args[0] == "label" && args[1] == "add":
		if !hasLabel(f.labels[args[2]], args[3]) {
			f.labels[args[2]] = append(f.labels[args[2]], args[3])
		}
		return nil, nil

	case len(args) == 4 && args[0] == "label" && args[1] == "remove":
		kept := f.labels[args[2]][:0]
		for _, l := range f.labels[args[2]] {
			if l != args[3] {
				kept = append(kept, l)
			}
		}
		f.labels[args[2]] = kept
		return nil, nil
	}
	return nil, fmt.Errorf("fake bd: unhandled invocation %v", args)
}

// ids returns bead ids in a stable order so List does not depend on map
// iteration -- a sweeper walking beads in a different order every run turns a
// flaky test into a flaky-looking sweeper.
func (f *fakeBd) ids() []string {
	out := make([]string, 0, len(f.labels))
	for id := range f.labels {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func hasLabel(ls []string, want string) bool {
	for _, l := range ls {
		if l == want {
			return true
		}
	}
	return false
}
