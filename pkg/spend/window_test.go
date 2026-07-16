package spend

import (
	"testing"
	"time"

	_ "time/tzdata" // or LoadLocation returns nil and time.Date PANICS
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMonthWindow(t *testing.T) {
	w := MonthWindow(at("2026-07-13T10:30:00Z"))
	if !w.Start.Equal(at("2026-07-01T00:00:00Z")) || !w.End.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("July window = %v", w)
	}
	// A non-UTC input still yields the UTC calendar month it falls in.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("tzdata unavailable: %v", err) // never a nil *Location into time.Date
	}
	w = MonthWindow(time.Date(2026, 7, 31, 21, 0, 0, 0, ny)) // = 2026-08-01T01:00Z
	if !w.Start.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("a local-July instant that is already August in UTC must land in August: %v", w)
	}
	// December rolls the year.
	w = MonthWindow(at("2026-12-31T23:59:59Z"))
	if !w.End.Equal(at("2027-01-01T00:00:00Z")) {
		t.Fatalf("December window = %v", w)
	}
}

func TestAdvanceIsMonotone(t *testing.T) {
	july := MonthWindow(at("2026-07-13T00:00:00Z"))

	if w, rolled := Advance(july, at("2026-07-31T23:59:59Z")); rolled || w != july {
		t.Fatal("rolled over before the window ended")
	}
	if w, rolled := Advance(july, at("2026-08-01T00:00:00Z")); !rolled || !w.Start.Equal(at("2026-08-01T00:00:00Z")) {
		t.Fatalf("failed to roll exactly at the boundary: %v %v", w, rolled)
	}
	// THE money case: the clock jumps backwards into last month. If we rolled
	// back, July's spend would vanish and the project would get a free budget.
	if w, rolled := Advance(july, at("2026-06-15T00:00:00Z")); rolled || w != july {
		t.Fatalf("a backwards clock jump reset the budget window: %v", w)
	}
	// A long outage: skipping a month is fine, we land on the current one.
	if w, rolled := Advance(july, at("2026-10-05T00:00:00Z")); !rolled || !w.Start.Equal(at("2026-10-01T00:00:00Z")) {
		t.Fatalf("failed to catch up after an outage: %v", w)
	}
	// A zero window (cold start) always initializes.
	if w, rolled := Advance(Window{}, at("2026-07-13T00:00:00Z")); !rolled || w != july {
		t.Fatalf("cold start = %v %v", w, rolled)
	}
}

func TestSkewOK(t *testing.T) {
	local := at("2026-07-13T10:00:00Z")
	max := 5 * time.Minute
	for _, c := range []struct {
		remote time.Time
		want   bool
	}{
		{at("2026-07-13T10:04:59Z"), true},
		{at("2026-07-13T09:55:01Z"), true},
		{at("2026-07-13T10:05:01Z"), false}, // we are behind
		{at("2026-07-13T09:54:59Z"), false}, // we are ahead -- the rollover risk
		{time.Time{}, true},                 // no Date header: cannot judge, do not block
	} {
		if got := SkewOK(local, c.remote, max); got != c.want {
			t.Errorf("SkewOK(local=%v, remote=%v) = %v, want %v", local, c.remote, got, c.want)
		}
	}
}
