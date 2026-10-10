package ghook

import (
	"sync"
	"testing"
	"time"
)

func TestDeduperSuppressesRepeats(t *testing.T) {
	d := NewDeduper(time.Hour, 10)
	if d.Seen("k") {
		t.Fatal("first sighting must be new")
	}
	if !d.Seen("k") {
		t.Fatal("second sighting must be a duplicate")
	}
	if d.Seen("other") {
		t.Fatal("a different key must be new")
	}
}

func TestDeduperExpires(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDeduper(time.Minute, 10)
	d.now = func() time.Time { return now }
	d.Seen("k")
	now = now.Add(2 * time.Minute)
	if d.Seen("k") {
		t.Fatal("entry should have expired")
	}
}

// Unbounded memory here is a remote DoS: every distinct event id would be
// retained forever.
func TestDeduperIsBounded(t *testing.T) {
	d := NewDeduper(time.Hour, 4)
	for i := range 100 {
		d.Seen(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if got := d.Len(); got > 4 {
		t.Fatalf("cache holds %d entries, cap is 4", got)
	}
}

func TestDeduperIsConcurrencySafe(t *testing.T) {
	d := NewDeduper(time.Hour, 128)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Seen("key")
			d.Seen(string(rune(i)))
		}()
	}
	wg.Wait()
}

func TestDedupeKeyPrefersEventUUID(t *testing.T) {
	ev, err := ParseEvent("Issue Hook", fixture(t, "issue-open"))
	if err != nil {
		t.Fatal(err)
	}
	if got := DedupeKey("abc-123", ev); got != "uuid:abc-123" {
		t.Fatalf("key = %q", got)
	}
	// No UUID header (older GitLab, or a replay stripped of it): derive one.
	k1 := DedupeKey("", ev)
	k2 := DedupeKey("", ev)
	if k1 == "" || k1 != k2 {
		t.Fatalf("derived key must be stable and non-empty: %q %q", k1, k2)
	}
}
