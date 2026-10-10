package beadstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is the test implementation and the reference semantics. EVERY test
// in cmd/gonk-gate runs against this -- no Dolt, no containers, no `bd`
// binary.
type Memory struct {
	mu   sync.Mutex
	recs map[string]Record
}

func NewMemory() *Memory { return &Memory{recs: map[string]Record{}} }

// Put upserts on r.BeadAnchor and stamps UpdatedAt (stampWriteTime -- the same
// rule BdCLI.Put applies, so the two stores cannot drift apart on it). The
// upsert is the whole of the idempotency contract: firing the same order twice
// must land on the same record, not create a second one (a second bead is a
// second session is duplicate spend).
func (m *Memory) Put(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[r.BeadAnchor] = stampWrite(r, time.Now().UTC())
	return nil
}

// PutIf checks the precondition and writes under one lock, so for Memory the
// compare-and-swap is exact. BdCLI cannot hold a lock across its bd
// invocations and gets the same outcome a different way (see BdCLI.PutIf).
func (m *Memory) PutIf(_ context.Context, r Record, ifRevision string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.recs[r.BeadAnchor].Revision; cur != ifRevision {
		return "", fmt.Errorf("%w: %s is at revision %q, the write expected %q",
			ErrStale, r.BeadAnchor, cur, ifRevision)
	}
	w := stampWrite(r, time.Now().UTC())
	m.recs[r.BeadAnchor] = w
	return w.Revision, nil
}

func (m *Memory) Get(_ context.Context, k string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[k]
	return r, ok, nil
}

// List filters by state, sorted by BeadAnchor for determinism: a sweeper
// walking an unordered map would process beads in a different order every
// run, which makes a flaky test look like a flaky sweeper.
func (m *Memory) List(_ context.Context, st State) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		if r.State == st {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadAnchor < out[j].BeadAnchor })
	return out, nil
}

// ListOpenPipelineRuns is Store.ListOpenPipelineRuns: every record with
// PipelineRunOpen, any state, sorted by BeadAnchor.
func (m *Memory) ListOpenPipelineRuns(_ context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.recs {
		if r.PipelineRunOpen {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadAnchor < out[j].BeadAnchor })
	return out, nil
}
