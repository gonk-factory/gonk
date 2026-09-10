package rung

import (
	"fmt"
	"time"
)

// Outcome is how one session attempt ended. The classification is STRICT and
// it is the entire escalation trigger (spec 6.3): only a completed-but-failed
// gate escalates. Infrastructure failures must never cost a project a rung --
// a flaky pod is not evidence that a bigger model is needed.
type Outcome string

const (
	// OutcomeSuccess: the session completed and passed its gate.
	OutcomeSuccess Outcome = "success"
	// OutcomeGateFailed: the session COMPLETED and FAILED its objective gate
	// (v1 triage: did not post the required artifacts within the turn/token
	// caps). This is the ONLY outcome that escalates.
	OutcomeGateFailed Outcome = "gate-failed"
	// OutcomeInfraFailed: connection error, LiteLLM 5xx, pod eviction, expired
	// reservation. Retry the SAME rung.
	OutcomeInfraFailed Outcome = "infra-failed"
	// OutcomeAborted: cancelled by a human or by a config change. Neither
	// escalates nor retries.
	OutcomeAborted Outcome = "aborted"
)

func (o Outcome) Valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeGateFailed, OutcomeInfraFailed, OutcomeAborted:
		return true
	}
	return false
}

// Escalates reports whether this outcome advances the ladder.
func (o Outcome) Escalates() bool { return o == OutcomeGateFailed }

// Attempt is one recorded attempt on a bead. Meter owns this history (ADR-004):
// a caller-supplied attempt count would let anyone skip straight to the most
// expensive rung.
type Attempt struct {
	Attempt int     `json:"attempt"` // 1-based
	Rung    string  `json:"rung"`
	Outcome Outcome `json:"outcome"`
}

// Escalations counts the gate failures in an attempt history. This IS the rung
// index: rung N is earned by failing the gate N times.
func Escalations(prior []Attempt) int {
	n := 0
	for _, a := range prior {
		if a.Outcome.Escalates() {
			n++
		}
	}
	return n
}

// ConsecutiveInfraFailures counts infra failures at the END of the history at
// the given rung. A rung that keeps dying on infrastructure must eventually
// stop rather than retry forever.
func ConsecutiveInfraFailures(prior []Attempt, at string) int {
	n := 0
	for i := len(prior) - 1; i >= 0; i-- {
		a := prior[i]
		if a.Outcome == OutcomeInfraFailed && a.Rung == at {
			n++
			continue
		}
		break
	}
	return n
}

// QuietHours is a resolved schedule.quiet_hours window (spec 5.4). It is
// resolved ONCE, at project registration, so that a bad timezone is a 400 at
// onboarding rather than a surprise at 3am -- and so that Decide stays pure.
type QuietHours struct {
	Start time.Duration // since local midnight
	End   time.Duration
	Loc   *time.Location
}

// ParseQuietHours parses "22:00-07:00" in the named IANA timezone. A window
// whose end is before its start wraps midnight.
//
// An EMPTY timezone is an error, not a default. The .gonk.yml schema does not
// require `timezone` when `quiet_hours` is set, and time.LoadLocation("")
// returns UTC with a nil error -- so a project writing quiet hours and no
// timezone would silently get a UTC window, quiet at the wrong nine hours of the
// day, with nothing to tell them. Make it a 400 at registration instead.
func ParseQuietHours(window, timezone string) (*QuietHours, error) {
	if window == "" {
		return nil, nil
	}
	if timezone == "" {
		return nil, fmt.Errorf("quiet hours %q: schedule.timezone is required when quiet_hours is set (an empty timezone would silently mean UTC)", window)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("quiet hours: timezone %q: %w", timezone, err)
	}
	var sh, sm, eh, em int
	if _, err := fmt.Sscanf(window, "%2d:%2d-%2d:%2d", &sh, &sm, &eh, &em); err != nil {
		return nil, fmt.Errorf("quiet hours %q: want HH:MM-HH:MM: %w", window, err)
	}
	q := &QuietHours{
		Start: time.Duration(sh)*time.Hour + time.Duration(sm)*time.Minute,
		End:   time.Duration(eh)*time.Hour + time.Duration(em)*time.Minute,
		Loc:   loc,
	}
	if q.Start == q.End {
		return nil, fmt.Errorf("quiet hours %q: a zero-length window", window)
	}
	return q, nil
}

// EndAfter reports whether now is inside the quiet window and, if so, the next
// instant it ends.
//
// R-21: an earlier version compared time.Time.Sub(midnight) -- the ELAPSED
// REAL DURATION since local midnight -- against Start/End, which are WALL-CLOCK
// offsets. Those two only agree on a day with no DST transition. On the US
// fall-back morning (e.g. America/New_York, 2026-11-01), midnight to 06:30
// wall-clock spans 7h30m of REAL time (the repeated hour adds an extra 60
// real minutes), so the elapsed-duration comparison judged 06:30 to be past
// the window's wall-clock 07:00 end and reported "not quiet" -- wrong; the
// wall clock had only reached 06:30. Comparing on local.Hour()/Minute()/etc
// (a pure wall-clock read of `now`, never an elapsed duration) makes today's
// DST offset irrelevant to the inWindow test, which is what "uses wall-clock
// in the configured location" means.
//
// The end instant has the same trap in reverse: naively adding q.End to
// midnight (or constructing the end via time.Date for TODAY without checking
// the result) can either replay the elapsed-duration bug, or -- if q.End
// itself names a wall-clock reading that does not exist that day (the
// spring-forward gap, e.g. 02:30 on a US spring-forward date) -- silently
// hand back time.Date's own normalization, which reinterprets the missing
// hour using the PRE-transition offset and reads EARLIER than intended
// (sometimes earlier than `now`). wallClockOn detects that mismatch and
// shifts forward by exactly the gap, so EndAfter never reports a window
// ending at a nonexistent -- or past -- local time.
func (q *QuietHours) EndAfter(now time.Time) (time.Time, bool) {
	local := now.In(q.Loc)
	since := wallClockOffset(local)

	inWindow := false
	if q.Start < q.End {
		inWindow = since >= q.Start && since < q.End
	} else { // wraps midnight, e.g. 22:00-07:00
		inWindow = since >= q.Start || since < q.End
	}
	if !inWindow {
		return time.Time{}, false
	}
	// The end wall-clock reading belongs to TODAY unless we are in the
	// pre-midnight half of a wrapping window (since >= q.End can only happen
	// there -- a non-wrapping window always has since < q.End by the inWindow
	// check above), in which case it belongs to TOMORROW.
	days := 0
	if since >= q.End {
		days = 1
	}
	return wallClockOn(local, days, q.End, q.Loc).UTC(), true
}

// wallClockOffset is now's reading on ITS OWN location's wall clock -- hours,
// minutes, seconds, nanoseconds since local midnight, exactly as a person
// looking at a clock in that timezone would read it. Unlike now.Sub(midnight)
// this is immune to a DST transition earlier in the same day: Hour/Minute/etc
// are wall-clock FIELDS of the already-resolved local time, not an elapsed
// duration computed from two instants.
func wallClockOffset(t time.Time) time.Duration {
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
}

// wallClockOn constructs the instant when loc's wall clock reads `since`
// (hours/minutes/seconds/nanoseconds past midnight) on base's calendar date
// plus `days`.
//
// time.Date normalizes a wall-clock reading that falls in a spring-forward
// gap (it does not error -- there is no error return) by reapplying the
// PRE-transition offset, which lands EARLIER than the requested reading, by
// exactly the gap's width. Detect that via the round-trip (want.Clock() no
// longer equals what was asked for) and correct it by shifting forward by
// the same width, landing on the wall-clock instant on the FAR side of the
// gap that corresponds to the requested one -- e.g. a nonexistent 02:30
// during a 1-hour spring-forward gap resolves to 03:30, never to the
// pre-transition 01:30 time.Date would otherwise hand back silently.
func wallClockOn(base time.Time, days int, since time.Duration, loc *time.Location) time.Time {
	y, mo, day := base.AddDate(0, 0, days).Date()
	h := int(since / time.Hour)
	m := int((since % time.Hour) / time.Minute)
	s := int((since % time.Minute) / time.Second)
	ns := int(since % time.Second)
	want := time.Date(y, mo, day, h, m, s, ns, loc)
	if gh, gm, gs := want.Clock(); gh != h || gm != m || gs != s {
		wanted := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
		got := time.Duration(gh)*time.Hour + time.Duration(gm)*time.Minute + time.Duration(gs)*time.Second
		want = want.Add(wanted - got)
	}
	return want
}
