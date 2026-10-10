package cidef

import (
	"fmt"
	"reflect"
	"sort"
)

// Kind is the outcome of a base-vs-head comparison. The zero value is not a
// Kind: a Delta nobody computed can never read as "none".
type Kind string

const (
	// KindNone: no effective difference. (An edit to a template no job uses
	// lands here: nothing a job runs under has changed.)
	KindNone Kind = "none"
	// KindPreserving: something changed, and every GATING and EXEC key of
	// every base job is equal.
	KindPreserving Kind = "preserving"
	// KindExecChanging: an EXEC key changed, on diagnosed jobs only. Whether
	// that is PERMITTED (the `ci` lift, `ci_exec_changes`) is the caller's
	// policy, not this package's.
	KindExecChanging Kind = "exec-changing"
	// KindViolation: D1, D2 or D3 is broken. J2 answers FORGED.
	KindViolation Kind = "violation"
	// KindUnresolvable: the comparison could not be made. J2 answers
	// UNVERIFIABLE(definition).
	KindUnresolvable Kind = "unresolvable"
)

// Rule codes of section 6.2.
const (
	RuleD1 = "D1" // every base job exists at head with the same stage
	RuleD2 = "D2" // every GATING key is equal at head
	RuleD3 = "D3" // an EXEC key changed on a job that was not diagnosed
)

// Side says which document a finding is about.
type Side string

const (
	SideBase Side = "base"
	SideHead Side = "head"
	SideBoth Side = "both"
)

// Violation is one broken rule. Job is "" for a document-level key.
type Violation struct {
	Rule   string // RuleD1, RuleD2, RuleD3
	Job    string
	Key    string // the job key, or a Global* name; "" for a removed job
	Detail string
}

// Unresolvable is one thing that could not be compared. Job and Key are ""
// for a document-level finding. Side matters to the caller: an unresolvable
// construct that only the HEAD has was introduced by the change being judged.
type Unresolvable struct {
	Side   Side
	Job    string
	Key    string
	Reason string
}

// KeyChange is one key of one job whose effective value differs. Values are
// deliberately not carried: CI variables are not something to copy into a
// note.
type KeyChange struct {
	Key   string
	Class Class
	// BasePresent / HeadPresent: the key exists in the effective definition.
	BasePresent bool
	HeadPresent bool
}

// JobDelta lists the changed keys of one job that exists in both documents.
// Jobs without a change are not listed.
type JobDelta struct {
	Job       string // the definition's job name
	Diagnosed bool   // in the diagnosed set T
	Changes   []KeyChange
}

// GlobalChange is a changed document-level key.
type GlobalChange struct {
	Key   string // GlobalWorkflow, GlobalStages, GlobalDefaultRetry, GlobalVariables
	Class Class  // ClassFree only for stages appended
	// Appended holds the new stages when Key is GlobalStages and the base
	// order is a prefix of the head order (implicit stages aside).
	Appended []string
}

// Rename is a HINT: a removed job and an added job whose effective
// definitions are identical. The job is still listed in Removed and Added and
// is still a D1 violation; this only lets a note say what happened.
type Rename struct {
	From string
	To   string
}

// Delta is the base-vs-head comparison.
type Delta struct {
	Kind Kind

	Jobs    []JobDelta
	Removed []string // base jobs absent at head (each a D1 violation)
	Added   []string // head jobs absent at base (free)
	Renamed []Rename
	Globals []GlobalChange

	Violations   []Violation
	Unresolvable []Unresolvable
}

// Compare computes the delta between two effective definitions. diagnosed is
// the set T: PIPELINE job names of the diagnosed failure, mapped to
// definition names with DefinitionName against base.
//
// The rules are section 6.2's and apply to EVERY job of the base definition,
// not only to the diagnosed ones. Kind is the first of: violation,
// unresolvable, exec-changing, preserving, none -- the order of J5, where a
// forged definition outranks one that could not be verified. Both lists are
// always complete, so a caller that wants another order has what it needs.
func Compare(base, head *Definition, diagnosed []string) Delta {
	var d Delta
	unresolvable := func(side Side, job, key, reason string) {
		d.Unresolvable = append(d.Unresolvable, Unresolvable{Side: side, Job: job, Key: key, Reason: reason})
	}
	violation := func(rule, job, key, detail string) {
		d.Violations = append(d.Violations, Violation{Rule: rule, Job: job, Key: key, Detail: detail})
	}

	// A document that could not be read has no jobs. Nothing below may run
	// on it: "every job was removed" would be a verdict about the agent
	// drawn from our own failure to read.
	if bp, hp := documentProblems(base), documentProblems(head); len(bp)+len(hp) > 0 {
		for _, p := range sided(bp, hp) {
			unresolvable(p.side, "", "", p.reason)
		}
		d.Kind = KindUnresolvable
		return d
	}

	inT := map[string]bool{}
	for _, name := range diagnosed {
		defName, ok := DefinitionName(base, name)
		if !ok {
			// Without the mapping, an EXEC change to the diagnosed job would
			// be reported as a change to a job outside T.
			unresolvable(SideBase, name, "", fmt.Sprintf("the diagnosed job %q has no definition in the base document", name))
			continue
		}
		inT[defName] = true
	}
	baseNames := sortedKeys(base.Jobs)
	outsideT := false
	for _, name := range baseNames {
		outsideT = outsideT || !inT[name]
	}
	execChanged, otherChanged := false, false

	// ---- document-level keys ----
	global := func(key string, bv any, bHas bool, hv any, hHas bool) (changed bool) {
		if side, reason, un := unresolvedIn(bv, bHas, hv, hHas); un {
			unresolvable(side, "", key, reason)
			return false
		}
		return bHas != hHas || !reflect.DeepEqual(bv, hv)
	}
	if global(GlobalWorkflow, base.Workflow, base.HasWorkflow, head.Workflow, head.HasWorkflow) {
		d.Globals = append(d.Globals, GlobalChange{Key: GlobalWorkflow, Class: ClassGating})
		violation(RuleD2, "", GlobalWorkflow, "`workflow` differs from the base")
	}
	if appended, ok := stagesAppended(listedStages(base.Stages), listedStages(head.Stages)); !ok {
		d.Globals = append(d.Globals, GlobalChange{Key: GlobalStages, Class: ClassGating})
		violation(RuleD2, "", GlobalStages, "the base stages are not the head's first stages, in order")
	} else if len(appended) > 0 {
		d.Globals = append(d.Globals, GlobalChange{Key: GlobalStages, Class: ClassFree, Appended: appended})
		otherChanged = true
	}
	if global(GlobalDefaultRetry, base.DefaultRetry, base.HasDefaultRetry, head.DefaultRetry, head.HasDefaultRetry) {
		d.Globals = append(d.Globals, GlobalChange{Key: GlobalDefaultRetry, Class: ClassGating})
		violation(RuleD2, "", GlobalDefaultRetry, "`default.retry` differs from the base")
	}
	if global(GlobalVariables, base.Variables, base.HasVariables, head.Variables, head.HasVariables) {
		// A global variable is an EXEC key of every job: it also feeds
		// `rules` and `workflow:rules`, whatever a job's `inherit` says.
		d.Globals = append(d.Globals, GlobalChange{Key: GlobalVariables, Class: ClassExec})
		execChanged = true
		if outsideT {
			violation(RuleD3, "", GlobalVariables, "the global `variables` changed and not every base job was diagnosed")
		}
	}

	// ---- every base job ----
	for _, name := range baseNames {
		bj, hj := base.Jobs[name], head.Jobs[name]
		if hj == nil {
			d.Removed = append(d.Removed, name)
			violation(RuleD1, name, "", "the job is not defined at head")
			continue
		}
		if len(bj.Problems)+len(hj.Problems) > 0 {
			for _, p := range sided(bj.Problems, hj.Problems) {
				unresolvable(p.side, name, "", p.reason)
			}
			continue
		}
		jd := JobDelta{Job: name, Diagnosed: inT[name]}
		for _, key := range unionKeys(bj.Keys, hj.Keys) {
			class := Classify(key)
			bv, bHas := bj.Keys[key]
			hv, hHas := hj.Keys[key]
			change := KeyChange{Key: key, Class: class, BasePresent: bHas, HeadPresent: hHas}
			if side, reason, un := unresolvedIn(bv, bHas, hv, hHas); un {
				if class == ClassFree {
					// A free key may change anyway: reported as a change,
					// which is the most it can be, and nothing is refused.
					jd.Changes = append(jd.Changes, change)
					otherChanged = true
					continue
				}
				unresolvable(side, name, key, reason)
				continue
			}
			if bHas == hHas && reflect.DeepEqual(bv, hv) {
				continue
			}
			jd.Changes = append(jd.Changes, change)
			switch class {
			case ClassExistence:
				violation(RuleD1, name, key, "the job's stage differs from the base")
			case ClassGating:
				violation(RuleD2, name, key, "a GATING key differs from the base")
			case ClassExec:
				execChanged = true
				if !inT[name] {
					violation(RuleD3, name, key, "an EXEC key differs from the base on a job that was not diagnosed")
				}
			case ClassFree:
				otherChanged = true
			default:
				unresolvable(SideBoth, name, key,
					fmt.Sprintf("the key %q changed and section 6.2's table has no rule for it", key))
			}
		}
		if len(jd.Changes) > 0 {
			d.Jobs = append(d.Jobs, jd)
		}
	}

	for _, name := range sortedKeys(head.Jobs) {
		if base.Jobs[name] == nil {
			d.Added = append(d.Added, name)
			otherChanged = true
		}
	}
	d.Renamed = renames(base, head, d.Removed, d.Added)

	switch {
	case len(d.Violations) > 0:
		d.Kind = KindViolation
	case len(d.Unresolvable) > 0:
		d.Kind = KindUnresolvable
	case execChanged:
		d.Kind = KindExecChanging
	case otherChanged:
		d.Kind = KindPreserving
	default:
		d.Kind = KindNone
	}
	return d
}

// Diff is Compare over two merged CI YAML documents.
func Diff(baseMergedYAML, headMergedYAML []byte, diagnosed []string) Delta {
	return Compare(Effective(baseMergedYAML), Effective(headMergedYAML), diagnosed)
}

func documentProblems(def *Definition) []string {
	if def == nil {
		return []string{"no definition"}
	}
	return def.Problems
}

type sidedReason struct {
	side   Side
	reason string
}

// sided labels each reason with the side it was found on, SideBoth when the
// same reason was found on both. Sorted by reason.
func sided(base, head []string) []sidedReason {
	side := map[string]Side{}
	for _, r := range base {
		side[r] = SideBase
	}
	for _, r := range head {
		if s, seen := side[r]; seen && s != SideHead {
			side[r] = SideBoth
		} else {
			side[r] = SideHead
		}
	}
	out := make([]sidedReason, 0, len(side))
	for _, r := range sortedKeys(side) {
		out = append(out, sidedReason{side: side[r], reason: r})
	}
	return out
}

// unresolvedIn reports whether either value holds an Unresolved marker. A
// value that does is never compared: not even to itself.
func unresolvedIn(bv any, bHas bool, hv any, hHas bool) (Side, string, bool) {
	br, bun := "", false
	if bHas {
		br, bun = firstUnresolved(bv)
	}
	hr, hun := "", false
	if hHas {
		hr, hun = firstUnresolved(hv)
	}
	switch {
	case bun && hun:
		return SideBoth, br, true
	case bun:
		return SideBase, br, true
	case hun:
		return SideHead, hr, true
	}
	return "", "", false
}

// stagesAppended: the base stages must be the head's FIRST stages, in order.
// What follows them is new and free. Both lists are given WITHOUT GitLab's
// implicit stages (listedStages): `.pre` and `.post` bracket every list and
// would make "appended" unreachable on the merged YAML GitLab returns. A stage inserted before an existing one
// keeps the base order as a subsequence and is still refused: section 6.2's
// free row says "appended".
func stagesAppended(base, head []string) (appended []string, ok bool) {
	if len(head) < len(base) {
		return nil, false
	}
	for i, s := range base {
		if head[i] != s {
			return nil, false
		}
	}
	return append([]string(nil), head[len(base):]...), true
}

// listedStages is a stage list without GitLab's implicit stages
// (ImplicitStages), wherever the document names them.
func listedStages(stages []string) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		implicit := false
		for _, i := range ImplicitStages {
			implicit = implicit || s == i
		}
		if !implicit {
			out = append(out, s)
		}
	}
	return out
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renames pairs a removed job with an added one when their effective
// definitions are identical and the pairing is unambiguous both ways.
func renames(base, head *Definition, removed, added []string) []Rename {
	same := func(from, to string) bool {
		bj, hj := base.Jobs[from], head.Jobs[to]
		if len(bj.Problems)+len(hj.Problems) > 0 {
			return false
		}
		if _, un := firstUnresolved(bj.Keys); un {
			return false
		}
		return reflect.DeepEqual(bj.Keys, hj.Keys)
	}
	matchesOf := func(from string) (to []string) {
		for _, a := range added {
			if same(from, a) {
				to = append(to, a)
			}
		}
		return to
	}
	claimed := map[string]int{}
	candidate := map[string]string{}
	for _, r := range removed {
		if to := matchesOf(r); len(to) == 1 {
			candidate[r] = to[0]
			claimed[to[0]]++
		}
	}
	var out []Rename
	for _, r := range removed {
		if to, ok := candidate[r]; ok && claimed[to] == 1 {
			out = append(out, Rename{From: r, To: to})
		}
	}
	return out
}
