package effects

import (
	"fmt"
	"sort"
)

// N is the "unbounded" max in a Card (any count >= min is allowed).
const N = int(^uint(0) >> 1) // max int

// Card is a per-kind cardinality: how many of this kind a batch may contain.
type Card struct{ Min, Max int }

// Shape is a task profile's expected-effect-shape: a per-kind cardinality map.
// A kind absent from the map is forbidden ({0,0}).
type Shape struct{ Kinds map[Kind]Card }

// Validate is the deterministic alignment gate -- no model in the loop. It
// checks per-kind counts against the shape. The first violation is returned so
// the caller can record it (spec §6.3).
func Validate(b Batch, s Shape) error {
	counts := map[Kind]int{}
	for _, e := range b.Effects {
		counts[e.Kind]++
	}
	// Any kind present that the shape does not allow (default {0,0}).
	for k, n := range counts {
		card, ok := s.Kinds[k]
		if !ok {
			return fmt.Errorf("effects: kind %q is forbidden by this shape (got %d)", k, n)
		}
		if n > card.Max {
			return fmt.Errorf("effects: kind %q count %d exceeds max %d", k, n, card.Max)
		}
	}
	// Any required kind (min>=1) missing or under-count.
	kinds := make([]Kind, 0, len(s.Kinds))
	for k := range s.Kinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		if counts[k] < s.Kinds[k].Min {
			return fmt.Errorf("effects: kind %q count %d below min %d", k, counts[k], s.Kinds[k].Min)
		}
	}
	return nil
}

// ValidateTargets is the target-binding gate: a run cannot propose an effect on
// a resource it was never handed. Every effect naming an existing resource
// (TargetIID != 0) must name an iid in the session's injected context; a
// TargetIID of 0 means "the session's primary target" and is always allowed.
// (Create-type kinds like new_issue bind to a parent -- for this slice their
// parent-binding detail is deferred with new_issue's apply path, so a 0 target
// is accepted as the primary target.)
func ValidateTargets(b Batch, allowed map[int64]bool) error {
	for i, e := range b.Effects {
		if e.TargetIID != 0 && !allowed[e.TargetIID] {
			return fmt.Errorf("effects: effect %d (kind %q) targets iid %d not in injected context", i, e.Kind, e.TargetIID)
		}
	}
	return nil
}
