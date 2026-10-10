// Package spend is the ledger side of gonk-meter: the budget window that spend
// is measured over, and the spend-log rows joined to attribution tags.
package spend

import "time"

// Window is the half-open budget period [Start, End). It is the UTC calendar
// month -- not the project's local month, not a rolling 30 days. One global
// boundary means two projects in different timezones cannot disagree about
// which month a charge lands in, and it is the boundary LiteLLM's own key
// budget_duration must be configured to match (see AD-9).
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) Contains(t time.Time) bool {
	u := t.UTC()
	return !u.Before(w.Start) && u.Before(w.End)
}

func (w Window) Zero() bool { return w.Start.IsZero() }

// MonthWindow is the UTC calendar month containing now.
func MonthWindow(now time.Time) Window {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.AddDate(0, 1, 0)}
}

// Advance returns the window that should be in force at now, given the one
// currently in force. It is MONOTONE: it never returns a window that starts
// earlier than prev.
//
// This is a money invariant, not tidiness. A backwards clock jump -- an NTP
// correction, a restored VM snapshot, a node with a dead RTC -- would otherwise
// move the window back into a month whose spend has already been counted,
// discard it, and hand every project a second budget. Refusing to move
// backwards costs nothing (a genuinely-past window is simply the one we are
// already in) and closes the hole completely.
//
// Forwards skew is NOT solved here: no arithmetic can tell "it is really
// August" from "my clock is wrong". That is a health problem -- see SkewOK,
// which fails /readyz and defers every decision rather than guessing.
func Advance(prev Window, now time.Time) (Window, bool) {
	next := MonthWindow(now)
	if prev.Zero() {
		return next, true
	}
	if now.UTC().Before(prev.End) {
		return prev, false // still inside it, INCLUDING a jump backwards
	}
	if !next.Start.After(prev.Start) {
		return prev, false // belt and braces: never move backwards
	}
	return next, true
}

// SkewOK reports whether our clock agrees with the spend source's clock (its
// HTTP Date header) closely enough to be trusted with a rollover decision. A
// zero remote time means the source told us nothing, which is not evidence of
// skew -- do not block the factory on a missing header.
func SkewOK(local, remote time.Time, max time.Duration) bool {
	if remote.IsZero() {
		return true
	}
	d := local.Sub(remote)
	if d < 0 {
		d = -d
	}
	return d <= max
}
