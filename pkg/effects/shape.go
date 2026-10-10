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
	// The verdict/kind rule of the fix vocabulary (fix.go). It is HERE, in the
	// gate every agent's batch passes, and not only in ValidateFix: a triage,
	// scaffold or pipeline batch stating verdict `fix` would otherwise parse,
	// pass its shape, and reach a broker that has no meaning for it.
	if r := verdictKindRule(b); r != nil { // gate:verdict-kind
		return r
	}
	return nil
}

// TargetKind is the resource family a target iid belongs to. iids are only
// unique within a family inside a project: issue 5 and MR !5 coexist.
type TargetKind string

const (
	TargetIssue        TargetKind = "issue"
	TargetMergeRequest TargetKind = "merge_request"
	// TargetPipeline is a GitLab pipeline. Its TargetIID is the pipeline's
	// ID (instance-wide), not its per-project iid: that is the number every
	// pipeline API and the session's injected context use. There is NO
	// TargetCommit: nothing lands on a commit (gonk-tios D9), and a target kind
	// with no apply path is the half-support effects.go refuses.
	TargetPipeline TargetKind = "pipeline"
)

var knownTargetKinds = map[TargetKind]bool{TargetIssue: true, TargetMergeRequest: true, TargetPipeline: true}

// Targets is the set of resources a session was actually handed, keyed by
// (kind, iid). Build it only from what was injected into the session's context.
type Targets struct {
	Issues        map[int64]bool
	MergeRequests map[int64]bool
	// Pipelines is keyed on pipeline ID (see TargetPipeline).
	Pipelines map[int64]bool
}

func (t Targets) has(k TargetKind, iid int64) bool {
	switch k {
	case TargetIssue:
		return t.Issues[iid]
	case TargetMergeRequest:
		return t.MergeRequests[iid]
	case TargetPipeline:
		return t.Pipelines[iid]
	}
	return false
}

// effectTargetKinds declares which target kinds each effect kind may name; the
// FIRST entry is the default when the batch omits target_type. Comment and
// label apply through the issue endpoints (resolveTarget in cmd/gonk-gate), so
// naming an MR would silently write to the ISSUE with that number -- they stay
// issue-only until an MR apply path exists. A kind absent from this map may not
// name an explicit target at all (a file effect writes to the repo, not to an
// iid). A diagnosis may name only the pipeline it diagnoses (or nothing, the
// primary target): WHERE it lands -- an MR note or the ref's issue -- is chosen
// by the controller, never by the agent (gonk-tios D9).
var effectTargetKinds = map[Kind][]TargetKind{
	KindComment:   {TargetIssue},
	KindLabel:     {TargetIssue},
	KindNewIssue:  {TargetIssue},
	KindDiagnosis: {TargetPipeline},
}

// ValidateTargets is the target-binding gate: a run cannot propose an effect on
// a resource it was never handed. Every effect naming an existing resource
// (TargetIID != 0) must name a (kind, iid) in the session's injected context
// and the kind must be one its effect kind may name; a TargetIID of 0 means
// "the session's primary target" and is always allowed. (Create-type kinds like
// new_issue bind to a parent -- their parent-binding detail is deferred with
// new_issue's apply path, so a 0 target is accepted as the primary target.)
func ValidateTargets(b Batch, allowed Targets) error {
	for i, e := range b.Effects {
		if e.TargetType != "" && !knownTargetKinds[e.TargetType] {
			return fmt.Errorf("effects: effect %d (kind %q) has unknown target_type %q", i, e.Kind, e.TargetType)
		}
		if e.TargetIID == 0 {
			if e.TargetType != "" {
				return fmt.Errorf("effects: effect %d (kind %q) names target_type %q without a target_iid", i, e.Kind, e.TargetType)
			}
			continue
		}
		kinds := effectTargetKinds[e.Kind]
		if len(kinds) == 0 {
			return fmt.Errorf("effects: effect %d (kind %q) may not name a target iid", i, e.Kind)
		}
		tk := e.TargetType
		if tk == "" {
			tk = kinds[0]
		}
		ok := false
		for _, k := range kinds {
			ok = ok || k == tk
		}
		if !ok {
			return fmt.Errorf("effects: effect %d (kind %q) may not target a %s (allowed: %v)", i, e.Kind, tk, kinds)
		}
		if !allowed.has(tk, e.TargetIID) {
			return fmt.Errorf("effects: effect %d (kind %q) targets %s iid %d not in injected context", i, e.Kind, tk, e.TargetIID)
		}
	}
	return nil
}
