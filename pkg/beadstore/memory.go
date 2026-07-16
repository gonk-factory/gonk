package beadstore

import (
	"context"
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

// Put upserts on r.BeadAnchor and stamps UpdatedAt. This is the whole of the
// idempotency contract: firing the same order twice must land on the same
// record, not create a second one (a second bead is a second session is
// duplicate spend).
func (m *Memory) Put(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.UpdatedAt = time.Now().UTC()
	m.recs[r.BeadAnchor] = r
	return nil
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
