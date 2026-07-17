package beadstore

import (
	"context"
	"testing"
	"time"
)

func TestMemoryRoundTrip(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	rec := Record{
		BeadAnchor:    "gonk:42:issue:3",
		BeadID:        "gk-1a2b",
		Project:       "group/repo",
		Rig:           "group-repo",
		SessionKey:    "gonk-42-issue-3",
		Trigger:       "issue-triage",
		IssueIID:      3,
		ProjectID:     42,
		State:         StateRunning,
		Rung:          "qwen-local",
		Attempt:       1,
		ReservationID: "rsv-1",
	}
	if err := s.Put(ctx, rec); err != nil {
		t.Fatalf("Put = %v", err)
	}
	got, ok, err := s.Get(ctx, "gonk:42:issue:3")
	if err != nil || !ok || got.BeadID != "gk-1a2b" || got.State != StateRunning {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
}

// The sweeper's whole job is "find the beads that need me". Two queries, and they
// must not overlap: a running bead is not a parked bead.
func TestListByState(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	now := time.Now()
	_ = s.Put(ctx, Record{BeadAnchor: "a", State: StateRunning})
	_ = s.Put(ctx, Record{BeadAnchor: "b", State: StateParked, RetryAfter: now.Add(-time.Minute)})
	_ = s.Put(ctx, Record{BeadAnchor: "c", State: StateParked, RetryAfter: now.Add(time.Hour)})
	_ = s.Put(ctx, Record{BeadAnchor: "d", State: StateNeedsHuman})
	_ = s.Put(ctx, Record{BeadAnchor: "e", State: StateDone})

	running, err := s.List(ctx, StateRunning)
	if err != nil || len(running) != 1 || running[0].BeadAnchor != "a" {
		t.Fatalf("running = %+v, %v", running, err)
	}
	parked, err := s.List(ctx, StateParked)
	if err != nil || len(parked) != 2 {
		t.Fatalf("parked = %+v, %v", parked, err)
	}
	// A DONE or NEEDS-HUMAN bead must never come back to the sweeper: needs-human
	// means a human, and re-sweeping it would spend money on a bead we already
	// gave up on (meterapi: `deny` / ladder-exhausted).
	for _, st := range []State{StateDone, StateNeedsHuman} {
		got, _ := s.List(ctx, StateRunning)
		for _, r := range got {
			if r.State == st {
				t.Fatalf("%s bead came back as running", st)
			}
		}
	}
}

// Put is an UPSERT keyed on BeadAnchor. Firing the same order twice (a duplicate
// webhook delivery, an intake restart mid-flight) must not create a second work
// record -- that is Plan 02's BeadAnchor idempotency carried into the pack, and
// Plan 06's K13/K18 rest on it.
func TestPutIsIdempotentOnBeadAnchor(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	_ = s.Put(ctx, Record{BeadAnchor: "gonk:1:issue:1", Attempt: 1, State: StateRunning})
	_ = s.Put(ctx, Record{BeadAnchor: "gonk:1:issue:1", Attempt: 2, State: StateRunning})
	all, _ := s.List(ctx, StateRunning)
	if len(all) != 1 {
		t.Fatalf("got %d records, want 1 -- BeadAnchor is the idempotency key and a duplicate bead is DUPLICATE SPEND", len(all))
	}
	if all[0].Attempt != 2 {
		t.Fatalf("attempt = %d, want the upsert to win", all[0].Attempt)
	}
}
