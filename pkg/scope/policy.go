package scope

import (
	"fmt"
)

// PolicyKey is the configuration key (`scope:`) this package's Policy sits
// under, in both the operator config's policies and a project's .gonk.yml.
const PolicyKey = "scope"

// Caps are a workflow's size limits. In a pack Row they are the MAXIMUM; a
// layer may only lower them.
type Caps struct {
	MaxFiles      int64 // files changed
	MaxLines      int64 // added + removed lines of the broker-computed diff
	MaxDeletes    int64 // files deleted
	MaxFileBytes  int64 // size of one file that may be changed
	MaxPatchBytes int64 // bytes of proposed content across the batch
}

// CapsPolicy is one layer's caps. nil means "this layer says nothing".
type CapsPolicy struct {
	MaxFiles      *int64 `yaml:"max_files"`
	MaxLines      *int64 `yaml:"max_lines"`
	MaxDeletes    *int64 `yaml:"max_deletes"`
	MaxFileBytes  *int64 `yaml:"max_file_bytes"`
	MaxPatchBytes *int64 `yaml:"max_patch_bytes"`
}

func (c CapsPolicy) each() []struct {
	key string
	v   *int64
} {
	return []struct {
		key string
		v   *int64
	}{
		{"max_files", c.MaxFiles}, {"max_lines", c.MaxLines}, {"max_deletes", c.MaxDeletes},
		{"max_file_bytes", c.MaxFileBytes}, {"max_patch_bytes", c.MaxPatchBytes},
	}
}

// Policy is ONE layer's `scope:` section: an operator policy (instance or
// group, pkg/opercfg) or a project's .gonk.yml (pkg/gonkcfg). Every key can
// only tighten -- the two reclassification keys excepted, and those are bound
// by the lattice (CheckMove).
//
// The JSON Schemas (docs/schemas/) list which keys each layer may write;
// Validate repeats every rule that matters, because a Policy can be built in
// Go without passing a schema. Documented in docs/reference/gonk-scope.md.
type Policy struct {
	// Classes, when set, is INTERSECTED with the pack row's classes. nil says
	// nothing; an empty list allows no class at all.
	Classes []Class `yaml:"classes"`
	// Caps are minimised against the pack row's.
	Caps CapsPolicy `yaml:"caps"`
	// ProtectedPaths become class `control`: never writable, never liftable.
	ProtectedPaths []string `yaml:"protected_paths"`
	// CIPaths become class `ci` (from source, other, test or docs only).
	CIPaths []string `yaml:"ci_paths"`
	// TestPaths and DocsPaths are the Q3 downward move: source/other -> test/docs.
	TestPaths []string `yaml:"test_paths"`
	DocsPaths []string `yaml:"docs_paths"`
	// SourcePaths moves other -> source. OPERATOR ONLY (Q3).
	SourcePaths []string `yaml:"source_paths"`
	// DenyLift vetoes a per-dispatch lift of the named classes. The operator
	// schema exposes it; honoured from any layer, since it only tightens.
	DenyLift []Class `yaml:"deny_lift"`
	// Profiles selects ecosystem profiles from the shipped menu (profile.go):
	// the profiles whose source, test and docs rows apply to this project.
	// PROJECT ONLY. nil is the default selection (go, docs-md); an empty list
	// selects none. Choosing among rows the operator shipped is not the
	// free-form widening Q3 forbids -- source_paths stays operator-only.
	Profiles []string `yaml:"profiles"`
	// AllowedProfiles restricts the menu projects under this layer may select
	// from. OPERATOR ONLY. nil says nothing; an empty list allows no profile.
	// INTERSECTED across operator layers, so it can only shrink the menu.
	AllowedProfiles []string `yaml:"allowed_profiles"`
}

// IsZero reports whether the layer says nothing at all.
func (p Policy) IsZero() bool {
	if p.Classes != nil || p.Profiles != nil || p.AllowedProfiles != nil || len(p.ProtectedPaths)+len(p.CIPaths)+len(p.TestPaths)+len(p.DocsPaths)+len(p.SourcePaths)+len(p.DenyLift) > 0 {
		return false
	}
	for _, c := range p.Caps.each() {
		if c.v != nil {
			return false
		}
	}
	return true
}

// patternKeys lists the reclassifying keys with the class each moves a path
// into, in the order the classifier consults them.
func (p Policy) patternKeys() []struct {
	key      string
	to       Class
	patterns []string
} {
	return []struct {
		key      string
		to       Class
		patterns []string
	}{
		{"protected_paths", ClassControl, p.ProtectedPaths},
		{"ci_paths", ClassCI, p.CIPaths},
		{"test_paths", ClassTest, p.TestPaths},
		{"docs_paths", ClassDocs, p.DocsPaths},
		{"source_paths", ClassSource, p.SourcePaths},
	}
}

// Validate is the dispatch-time check of one layer (066 section 3.2: "checked
// when the policy is loaded, so a bad .gonk.yml fails at dispatch, not at judge
// time"). pkg/gonkcfg.Load and pkg/opercfg.Load call it, so an invalid scope
// makes the project invalid -- or stops the meter starting -- before any spend.
//
// What it refuses: an unknown class; a protected class in `classes` (protected
// classes are never in an allowed set -- `ci` is reached by a lift); a negative
// cap; a malformed pattern or too many of them; `source_paths` from a project;
// a `deny_lift` of a class that is not liftable; a profile that is not on the
// shipped menu; `profiles` from an operator layer or `allowed_profiles` from a
// project; and a pattern AIMED at a class the lattice does not let that layer
// move (`docs_paths: [deploy/**]`).
//
// Whether a project's selection is inside its operator's allowed_profiles
// takes both layers, so it is not checked here: SelectProfiles does, for Fold
// and for the meter.
//
// "Aimed at" is decided by classifying sample paths built from the pattern
// (probes), under this layer's own profile selection when it has one. That catches the plain mistakes early and loudly; it is not the
// guarantee. The guarantee is Classifier.Classify, which applies the lattice
// to every path individually, so `test_paths: ["**"]` loads and still moves
// nothing out of a locked class.
func (p Policy) Validate(layer Layer) error {
	if layer != LayerOperator && layer != LayerProject {
		return fmt.Errorf("scope: unknown layer %q", layer)
	}
	for _, c := range p.Classes {
		if !c.Valid() {
			return fmt.Errorf("scope: classes: unknown class %q", c)
		}
		if c.Sensitivity() == Protected {
			return fmt.Errorf("scope: classes: %q is a protected class and is never in an allowed set", c)
		}
	}
	for _, c := range p.Caps.each() {
		if c.v != nil && *c.v < 0 {
			return fmt.Errorf("scope: caps.%s is negative (%d)", c.key, *c.v)
		}
	}
	for _, c := range p.DenyLift {
		if !Liftable(c) {
			return fmt.Errorf("scope: deny_lift: %q is not a liftable class (only ci is)", c)
		}
	}
	if layer == LayerProject && len(p.SourcePaths) > 0 {
		return fmt.Errorf("scope: source_paths is operator-only: a project may reclassify source/other into test or docs, and nothing else (gonk-066 Q3)")
	}
	if layer == LayerOperator && p.Profiles != nil {
		return fmt.Errorf("scope: profiles is project-only: an operator layer restricts the menu with allowed_profiles, and the project selects from it")
	}
	if layer == LayerProject && p.AllowedProfiles != nil {
		return fmt.Errorf("scope: allowed_profiles is operator-only: a project selects profiles with `profiles`")
	}
	if err := checkProfileNames("profiles", p.Profiles); err != nil {
		return err
	}
	if err := checkProfileNames("allowed_profiles", p.AllowedProfiles); err != nil {
		return err
	}
	// The probes below ask what a pattern is aimed at. A project that selected
	// profiles is asking about ITS table, not the default one.
	tbl := defaultTable
	if p.Profiles != nil {
		tbl = buildTable(shipped, p.Profiles)
	}
	for _, k := range p.patternKeys() {
		if len(k.patterns) > MaxPatterns {
			return fmt.Errorf("scope: %s: %d patterns, the limit is %d", k.key, len(k.patterns), MaxPatterns)
		}
		for _, pat := range k.patterns {
			if err := ValidatePattern(pat); err != nil {
				return fmt.Errorf("scope: %s: %w", k.key, err)
			}
			for _, probe := range probes(pat) {
				from, _ := tbl.classify(probe, BaseFacts{})
				if err := CheckMove(from, k.to, layer); err != nil {
					return fmt.Errorf("scope: %s: pattern %q matches %s-class paths such as %q: %w", k.key, pat, from, probe, err)
				}
			}
		}
	}
	return nil
}

// FoldPolicies folds two layers of the SAME kind (a nested operator group onto
// its ancestor group) into one Policy that Fold then treats as a single layer.
// It applies Fold's own per-layer rules and nothing looser: classes are
// INTERSECTED (nil is silent; an empty or disjoint result stays non-nil, which
// means "allow no class"), allowed_profiles likewise, every cap takes the
// MINIMUM of those set, the pattern lists and deny_lift are UNIONED (base first, so the coarser layer's
// patterns are consulted first, as Fold orders separate layers). Nothing in
// either input can be dropped or loosened by the other: the result is never
// less restrictive than either side.
//
// `profiles` is NOT carried: it is project-only, Validate refuses it in an
// operator layer, and these are operator layers.
func FoldPolicies(base, next Policy) Policy {
	out := Policy{
		Classes: foldClasses(base.Classes, next.Classes),
		Caps: CapsPolicy{
			MaxFiles:      minCap(base.Caps.MaxFiles, next.Caps.MaxFiles),
			MaxLines:      minCap(base.Caps.MaxLines, next.Caps.MaxLines),
			MaxDeletes:    minCap(base.Caps.MaxDeletes, next.Caps.MaxDeletes),
			MaxFileBytes:  minCap(base.Caps.MaxFileBytes, next.Caps.MaxFileBytes),
			MaxPatchBytes: minCap(base.Caps.MaxPatchBytes, next.Caps.MaxPatchBytes),
		},
		ProtectedPaths: unionStrings(base.ProtectedPaths, next.ProtectedPaths),
		CIPaths:        unionStrings(base.CIPaths, next.CIPaths),
		TestPaths:      unionStrings(base.TestPaths, next.TestPaths),
		DocsPaths:      unionStrings(base.DocsPaths, next.DocsPaths),
		SourcePaths:    unionStrings(base.SourcePaths, next.SourcePaths),

		AllowedProfiles: foldAllowedProfiles(base.AllowedProfiles, next.AllowedProfiles),
	}
	for _, c := range append(append([]Class(nil), base.DenyLift...), next.DenyLift...) {
		if !containsClass(out.DenyLift, c) {
			out.DenyLift = append(out.DenyLift, c)
		}
	}
	return out
}

func foldClasses(a, b []Class) []Class {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return append([]Class{}, b...)
	case b == nil:
		return append([]Class{}, a...)
	}
	out := []Class{}
	for _, c := range a {
		if containsClass(b, c) && !containsClass(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func containsClass(l []Class, c Class) bool {
	for _, x := range l {
		if x == c {
			return true
		}
	}
	return false
}

func minCap(a, b *int64) *int64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil || *a <= *b:
		v := *a
		return &v
	}
	v := *b
	return &v
}

// unionStrings appends b's entries not already in a, a first. nil + nil is nil.
func unionStrings(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range [][]string{a, b} {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// foldAllowedProfiles intersects two menus, in a's order. nil is silent; an
// empty or disjoint result stays non-nil ("allow no profile").
func foldAllowedProfiles(a, b []string) []string {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return append([]string{}, b...)
	case b == nil:
		return append([]string{}, a...)
	}
	in := map[string]bool{}
	for _, n := range b {
		in[n] = true
	}
	out := []string{}
	for _, n := range a {
		if in[n] {
			out = append(out, n)
			in[n] = false
		}
	}
	return out
}
