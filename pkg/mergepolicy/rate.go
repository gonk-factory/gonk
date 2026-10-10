package mergepolicy

import (
	"strings"
	"time"
)

// MergesInWindow is M11's count: how many of the supplied automatic-merge
// timestamps fall in the RateWindow ending at now. It is a pure function of
// its arguments; the caller supplies the project's merges from the store.
//
// Every doubt counts AGAINST merging: the window is closed at both ends (a
// merge exactly 24 h ago still counts), a timestamp after `now` counts (clock
// skew must not open the cap), and a zero timestamp counts (an undated merge
// is not an old one).
func MergesInWindow(merges []time.Time, now time.Time) int {
	start := now.Add(-RateWindow)
	n := 0
	for _, m := range merges {
		if m.IsZero() || !m.Before(start) {
			n++
		}
	}
	return n
}

// Independence is epic plan 3.2 N5's comparison: the reviewer's recorded
// model against the author's. Names are compared trimmed and without regard
// to case; a missing name on either side is IndependenceUnknown, never
// distinct.
//
// It compares NAMES. One model served under two names reads as distinct; which
// names are distinct adequate models is the operator's `review.rung`, not
// something this function can know.
func Independence(authorModel, reviewerModel string) string {
	a, r := strings.TrimSpace(authorModel), strings.TrimSpace(reviewerModel)
	if a == "" || r == "" {
		return IndependenceUnknown
	}
	if strings.EqualFold(a, r) {
		return IndependenceSameModel
	}
	return IndependenceDistinctModel
}
