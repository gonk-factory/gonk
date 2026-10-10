package beadstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/beadstore/storetest"
)

// EVERY Store implementation runs the SAME suite. Adding a third store means
// adding a Test here; a store that is not wired in is a store nothing holds to
// the contract, which is precisely how BdCLI came to be the one implementation
// that did not stamp UpdatedAt.

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) beadstore.Store { return beadstore.NewMemory() })
	storetest.RunCompareAndSwap(t, func(t *testing.T) beadstore.Store { return beadstore.NewMemory() })
}

// BdCLI runs Run but NOT RunCompareAndSwap: it does not pass it, and
// bd_notfinal_test.go pins how (gonk-vred, gonk-t41). gonk-gate no longer
// selects BdCLI for its state; it survives only to read (and, on a rollback,
// write back) the records the bd-backed binaries left.
func TestBdCLIConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) beadstore.Store {
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
	// stamps[id][i] is comments[id][i]'s bd created_at, taken from now() when
	// the comment is written (whole seconds, as bd 1.1.0 stores it).
	stamps map[string][]time.Time
	now    func() time.Time
}

func newFakeBd() *fakeBd {
	return &fakeBd{labels: map[string][]string{}, comments: map[string][]string{},
		stamps: map[string][]time.Time{}, now: time.Now}
}

// setNow makes later comments carry the given created_at.
func (f *fakeBd) setNow(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = func() time.Time { return t }
}

func (f *fakeBd) run(_ context.Context, _, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(args) == 4 && args[0] == "list" && args[1] == "--label" && args[3] == "--json":
		// Rows carry their labels, as the real bd 1.1.0 `list --json` does
		// (observed live on go-3tbq, 2026-09-13); BdCLI.List reads them.
		type row struct {
			ID     string   `json:"id"`
			Labels []string `json:"labels"`
		}
		rows := []row{}
		for _, id := range f.ids() {
			if hasLabel(f.labels[id], args[2]) {
				rows = append(rows, row{ID: id, Labels: append([]string(nil), f.labels[id]...)})
			}
		}
		return json.Marshal(rows)

	case len(args) == 6 && args[0] == "list" && args[1] == "--label" && args[3] == "--limit" && args[5] == "--json":
		// What the bd-backed List saw: its call, with bd's default limit written
		// out (gonk-t41 quarantine). The fake applies the limit to its own order.
		limit, err := strconv.Atoi(args[4])
		if err != nil {
			return nil, err
		}
		type row struct {
			ID     string   `json:"id"`
			Labels []string `json:"labels"`
		}
		rows := []row{}
		for _, id := range f.ids() {
			if hasLabel(f.labels[id], args[2]) && (limit == 0 || len(rows) < limit) {
				rows = append(rows, row{ID: id, Labels: append([]string(nil), f.labels[id]...)})
			}
		}
		return json.Marshal(rows)

	case len(args) == 6 && args[0] == "list" && args[1] == "--label-pattern" && args[3] == "--limit" && args[4] == "0" && args[5] == "--json":
		// The bd import's enumeration (gonk-t41). Only a trailing-* prefix glob is
		// modelled, which is the only pattern BdCLI sends.
		prefix, ok := strings.CutSuffix(args[2], "*")
		if !ok || strings.ContainsAny(prefix, "*?[") {
			return nil, fmt.Errorf("fake bd: unmodelled label pattern %q", args[2])
		}
		type row struct {
			ID     string   `json:"id"`
			Labels []string `json:"labels"`
		}
		rows := []row{}
		for _, id := range f.ids() {
			for _, l := range f.labels[id] {
				if strings.HasPrefix(l, prefix) {
					rows = append(rows, row{ID: id, Labels: append([]string(nil), f.labels[id]...)})
					break
				}
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
		f.stamps[args[1]] = append(f.stamps[args[1]], f.now().UTC().Truncate(time.Second))
		return nil, nil

	case len(args) == 3 && args[0] == "comments" && args[2] == "--json":
		rows := []map[string]string{}
		for i, c := range f.comments[args[1]] {
			rows = append(rows, map[string]string{"text": c, "created_at": f.stamps[args[1]][i].Format(time.RFC3339)})
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
