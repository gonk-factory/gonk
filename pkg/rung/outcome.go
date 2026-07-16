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
// instant it ends. It walks forward from local midnight rather than doing
// arithmetic on wall-clock durations, so a DST transition inside the window
// cannot produce an end time that is in the past.
func (q *QuietHours) EndAfter(now time.Time) (time.Time, bool) {
	local := now.In(q.Loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc)
	since := local.Sub(midnight)

	inWindow := false
	if q.Start < q.End {
		inWindow = since >= q.Start && since < q.End
	} else { // wraps midnight, e.g. 22:00-07:00
		inWindow = since >= q.Start || since < q.End
	}
	if !inWindow {
		return time.Time{}, false
	}
	end := midnight.Add(q.End)
	if !end.After(now) { // we are in the pre-midnight half of a wrapping window
		end = midnight.AddDate(0, 0, 1).Add(q.End)
	}
	return end.UTC(), true
}
