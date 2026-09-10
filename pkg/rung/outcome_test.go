package rung

import (
	"testing"
	"time"

	// Without tzdata, time.LoadLocation("America/New_York") fails in a scratch
	// container AND in any CI image with no zoneinfo -- and every DST case
	// below would fail for a reason that has nothing to do with EndAfter.
	_ "time/tzdata"
)

// TestEndAfterDST is R-21: QuietHours.EndAfter must read wall-clock time in
// the configured location, never elapsed real duration since midnight. The
// two are the same on an ordinary day and DIVERGE on a DST transition day,
// which is exactly the bug an earlier version had (see outcome.go's doc
// comment on EndAfter): on the US fall-back morning it judged 06:30 EST to be
// past a 07:00 quiet-hours end because 7h30m of REAL time had elapsed since
// midnight, even though the WALL CLOCK had only reached 06:30 -- 30 minutes
// still inside the window.
//
// The table below is the review's exact probe (fall-back, R-21) plus the
// spring-forward gap, an ambiguous fall-back reading taken on BOTH of its two
// occurrences, and a schedule whose END time itself falls inside a
// spring-forward gap -- the literal "quiet window ends at a nonexistent local
// time" defect the task calls out.
func TestEndAfterDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}

	wrap := &QuietHours{Start: 22 * time.Hour, End: 7 * time.Hour, Loc: ny}                 // 22:00-07:00
	preMidnight := &QuietHours{Start: 0, End: 2 * time.Hour, Loc: ny}                       // 00:00-02:00
	gapEnd := &QuietHours{Start: 1 * time.Hour, End: 2*time.Hour + 30*time.Minute, Loc: ny} // 01:00-02:30

	cases := []struct {
		name    string
		q       *QuietHours
		now     time.Time
		want    bool
		wantEnd time.Time // only checked when want is true
	}{
		{
			// R-21's exact probe: 2026-11-01 is the US fall-back date (clocks
			// repeat 01:00-01:59). 06:30 is UNAMBIGUOUS (well after the
			// repeated hour) and wall-clock-inside 22:00-07:00. The buggy
			// elapsed-duration version reported this as NOT quiet, because
			// midnight-to-06:30 spans 7h30m of real time (the repeated hour
			// adds 60 real minutes) -- past the 7h "End" threshold it was
			// comparing against. The right answer is quiet=true, ending at
			// wall-clock 07:00 EST the same morning.
			name:    "fall-back: unambiguous 06:30 EST is still before 07:00 wall-clock",
			q:       wrap,
			now:     time.Date(2026, 11, 1, 6, 30, 0, 0, ny),
			want:    true,
			wantEnd: time.Date(2026, 11, 1, 7, 0, 0, 0, ny),
		},
		{
			// Same fall-back date, but now sits ON the repeated wall-clock
			// hour -- 01:30 happens TWICE, first at -04:00 (EDT), then again
			// an hour of real time later at -05:00 (EST). Both instants read
			// the identical wall clock (01:30) and must be judged identically:
			// quiet, ending at the (unambiguous) wall-clock 02:00 that
			// morning. This is checked on the FIRST occurrence here.
			name: "fall-back: 01:30 EDT (first pass of the repeated hour)",
			q:    preMidnight,
			// 2026-11-01 05:30 UTC == 2026-11-01 01:30 EDT (before the fall-back).
			now:     time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC),
			want:    true,
			wantEnd: time.Date(2026, 11, 1, 2, 0, 0, 0, ny),
		},
		{
			// The SECOND occurrence of the same wall-clock 01:30, an hour of
			// real time later, at the other UTC offset. Must land on the
			// exact same answer as the first pass: the window does not care
			// which pass of the repeated hour produced this wall-clock
			// reading, only what the wall clock reads.
			name: "fall-back: 01:30 EST (second pass of the repeated hour)",
			q:    preMidnight,
			// 2026-11-01 06:30 UTC == 2026-11-01 01:30 EST (after the fall-back).
			now:     time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC),
			want:    true,
			wantEnd: time.Date(2026, 11, 1, 2, 0, 0, 0, ny),
		},
		{
			// 2026-03-08 is the US spring-forward date: wall-clock 02:00-02:59
			// DOES NOT EXIST (clocks jump straight from 01:59:59 to 03:00:00).
			// A caller (or, as here, a test) that builds `now` from that
			// nonexistent reading gets time.Date's silent normalization: Go
			// reapplies the PRE-transition (EST) offset and hands back
			// 01:30 EST instead of erroring. EndAfter must still behave
			// sanely on whatever real instant that normalization produced:
			// wall-clock 01:30 is inside 22:00-07:00, ending at the
			// (unambiguous, post-gap) wall-clock 07:00 EDT that morning.
			name:    "spring-forward: now built from the nonexistent 02:30 normalizes to 01:30 EST, still handled",
			q:       wrap,
			now:     time.Date(2026, 3, 8, 2, 30, 0, 0, ny),
			want:    true,
			wantEnd: time.Date(2026, 3, 8, 7, 0, 0, 0, ny),
		},
		{
			// The hard case: the window's OWN End (02:30) is the nonexistent
			// wall-clock reading, not just `now`. This is the literal R-21
			// defect the task names: "a quiet window that ends at a
			// nonexistent local time". time.Date(2026,3,8,2,30,...) alone
			// would normalize to 01:30 EST -- BEFORE `now` (01:15 EST) in
			// this case, i.e. an end already in the past. The right answer
			// shifts forward by exactly the gap's width (1h) and lands on
			// 03:30 EDT: the wall-clock instant on the far side of the gap
			// that corresponds to the requested (nonexistent) 02:30.
			name:    "spring-forward: the window's End (02:30) itself falls in the gap",
			q:       gapEnd,
			now:     time.Date(2026, 3, 8, 1, 15, 0, 0, ny),
			want:    true,
			wantEnd: time.Date(2026, 3, 8, 3, 30, 0, 0, ny),
		},
		{
			// Sanity check outside the window on the SAME DST-transition day:
			// must not spuriously report quiet.
			name: "fall-back: 08:00, after the window's 07:00 end, is not quiet",
			q:    wrap,
			now:  time.Date(2026, 11, 1, 8, 0, 0, 0, ny),
			want: false,
		},
		{
			// And a completely ordinary day, as a control: no DST anywhere
			// near it, so the naive elapsed-duration arithmetic and the
			// wall-clock arithmetic agree -- this case cannot distinguish the
			// fix from the bug, but it guards against breaking the common
			// case while fixing the rare one.
			name:    "ordinary day: no DST transition nearby",
			q:       wrap,
			now:     time.Date(2026, 6, 15, 23, 30, 0, 0, ny),
			want:    true,
			wantEnd: time.Date(2026, 6, 16, 7, 0, 0, 0, ny),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			end, quiet := c.q.EndAfter(c.now)
			if quiet != c.want {
				t.Fatalf("EndAfter(%s) quiet = %v, want %v (end=%s)",
					c.now.Format(time.RFC3339), quiet, c.want, end.Format(time.RFC3339))
			}
			if !c.want {
				return
			}
			if !end.Equal(c.wantEnd) {
				t.Fatalf("EndAfter(%s) end = %s, want %s",
					c.now.Format(time.RFC3339), end.In(ny).Format(time.RFC3339), c.wantEnd.Format(time.RFC3339))
			}
			// The end must always be strictly after now -- an end at or
			// before now is a window that is already over, which is the
			// general shape of the R-21 defect (a wrong, non-future end).
			if !end.After(c.now) {
				t.Fatalf("EndAfter(%s) returned an end (%s) that is not after now",
					c.now.Format(time.RFC3339), end.Format(time.RFC3339))
			}
		})
	}
}
