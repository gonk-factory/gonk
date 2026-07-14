package ghook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Deduper suppresses repeat deliveries. GitLab can deliver the same event twice
// (retries after a slow response, an operator hitting "Test"), and a duplicate
// triage order costs real tokens. Bounded in both time and size: an unbounded
// map keyed on attacker-influenced ids is a memory DoS.
type Deduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	max  int
	now  func() time.Time
}

func NewDeduper(ttl time.Duration, max int) *Deduper {
	if max < 1 {
		max = 1
	}
	return &Deduper{seen: make(map[string]time.Time), ttl: ttl, max: max, now: time.Now}
}

// Seen reports whether key was already recorded, and records it if not.
func (d *Deduper) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	if at, ok := d.seen[key]; ok {
		if now.Sub(at) < d.ttl {
			return true
		}
		delete(d.seen, key) // expired
	}
	d.evict(now)
	d.seen[key] = now
	return false
}

func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// evict drops expired entries, then, if still at capacity, the oldest ones.
func (d *Deduper) evict(now time.Time) {
	for k, at := range d.seen {
		if now.Sub(at) >= d.ttl {
			delete(d.seen, k)
		}
	}
	for len(d.seen) >= d.max {
		oldestKey, oldestAt := "", time.Time{}
		for k, at := range d.seen {
			if oldestAt.IsZero() || at.Before(oldestAt) {
				oldestKey, oldestAt = k, at
			}
		}
		delete(d.seen, oldestKey)
	}
}

// DedupeKey prefers GitLab's X-Gitlab-Event-UUID, which is exactly this. When
// absent, derive a stable key from the identifying fields of the event.
func DedupeKey(eventUUID string, ev *Event) string {
	if eventUUID != "" {
		return "uuid:" + eventUUID
	}
	var id string
	switch {
	case ev.Note != nil:
		id = fmt.Sprintf("note:%d", ev.Note.ID)
	case ev.MergeRequest != nil:
		id = fmt.Sprintf("mr:%d:%s", ev.MergeRequest.IID, ev.MergeRequest.Action)
	case ev.Issue != nil:
		id = fmt.Sprintf("issue:%d:%s", ev.Issue.IID, ev.Issue.Action)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", ev.Kind, ev.Project.ID, id)))
	return "derived:" + hex.EncodeToString(sum[:8])
}
