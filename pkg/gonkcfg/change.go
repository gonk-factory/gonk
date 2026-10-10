package gonkcfg

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The issue -> merge request workflow's configuration (gonk-cuwn / gonk-3so,
// plan docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md sections 2.1, 2.7 and
// the owner decisions of 8.1): the `change:` section. WHETHER the workflow may
// run at all is the existing `actions.features`; this section says HOW.
// Everything is optional, and the ceilings below are the most any layer may
// ask for: a document above one is refused, never clamped.
//
// NOTHING DISPATCHES THIS WORKFLOW YET. This file is configuration only.
//
// Reference: docs/reference/gonk-change.md. The two JSON Schemas carry the
// same rules for a document; Validate repeats them for a Policy built in Go.

// ChangeKey is the configuration key the section sits under.
const ChangeKey = "change"

// The three forms of an explicit request to implement an issue (owner decision
// Q2, 2026-10-07). Each counts only when the person who made it is a project
// member with at least ChangeMinAccessLevel; that check is the dispatcher's.
// There is no fourth form: a triage verdict never starts this workflow in v1,
// and `triage-verdict` is not a value any layer may write.
const (
	// ChangeTriggerLabel: the implement label was applied to the issue.
	ChangeTriggerLabel = "label"
	// ChangeTriggerCommand: a slash-style command in an issue note.
	ChangeTriggerCommand = "command"
	// ChangeTriggerMention: an at-mention of the bot asking for implementation.
	ChangeTriggerMention = "mention"
)

// ChangeMinAccessLevel is the lowest GitLab access level whose holder may ask
// for an issue to be implemented: 40, Maintainer (50 is Owner). It is NOT
// configurable in v1 (owner decision Q2: owner or maintainer, tighter than the
// Developer level the plan first proposed).
const ChangeMinAccessLevel = 40

// When a change must come with documentation (plan 2.4). The order
// never < when-labelled < always is "loosest first"; a fold keeps the
// strictest.
const (
	RequireDocsNever        = "never"
	RequireDocsWhenLabelled = "when-labelled"
	RequireDocsAlways       = "always"
)

// Path classes a change may CREATE a file in (plan 2.3). These are pkg/scope
// path classes.
const (
	CreateClassSource = string(scope.ClassSource)
	CreateClassTest   = string(scope.ClassTest)
	CreateClassDocs   = string(scope.ClassDocs)
)

// Defaults: what a project with `actions.features: true` and no `change:`
// section gets.
const (
	DefaultChangeMaxAttempts     = 2
	DefaultChangeMaxOpen         = 1
	DefaultChangeVerifyTimeout   = 2 * time.Hour
	DefaultChangeMaxFiles        = 6
	DefaultChangeMaxChangedLines = 200
)

// Ceilings (plan 2.2 and 2.7).
const (
	ChangeMaxAttemptsCeiling     = 3
	ChangeMaxOpenCeiling         = 3
	ChangeVerifyTimeoutCeiling   = 6 * time.Hour
	ChangeMaxFilesCeiling        = 8
	ChangeMaxChangedLinesCeiling = 300
)

// Bounds of `docs_labels`: how many labels, and how long one may be (GitLab's
// own limit on a label title).
const (
	MaxDocsLabels   = 16
	MaxDocsLabelLen = 255
)

// `auto_merge.max_per_day` (plan 5.1 M11): automatic merges per project per
// 24 hours. Shared by `pipeline_fix.auto_merge` and `change.auto_merge`.
const (
	DefaultAutoMergeMaxPerDay = 3
	AutoMergeMaxPerDayCeiling = 3
)

var (
	changeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}
	createClassOrder      = []string{CreateClassSource, CreateClassTest, CreateClassDocs}
	changeMergeClassOrder = []string{AutoMergeClassDocs}
	requireDocsRank       = map[string]int{RequireDocsNever: 0, RequireDocsWhenLabelled: 1, RequireDocsAlways: 2}

	defaultChangeTriggers    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}
	defaultCreateClasses     = []string{CreateClassTest, CreateClassDocs}
	defaultDocsLabels        = []string{"feature", "enhancement", "type::feature"}
	defaultChangeMergeClass  = []string{AutoMergeClassDocs}
	docsLabelRe              = regexp.MustCompile(`^\S(.*\S)?$`)
	reviewRungRe             = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	defaultReviewTargetsList = []string{ReviewTargetGonk}
)

// ChangePolicy is ONE layer's `change:` section. nil means "this layer says
// nothing"; for the lists, a non-nil EMPTY list means "none".
type ChangePolicy struct {
	// Triggers: which request forms start the workflow.
	Triggers []string `yaml:"triggers"`
	// CreateClasses: path classes a change may create a NEW file in.
	CreateClasses []string `yaml:"create_classes"`
	// RequireDocs: never | when-labelled | always.
	RequireDocs *string `yaml:"require_docs"`
	// DocsLabels: issue labels that make documentation required under
	// `when-labelled`.
	DocsLabels []string `yaml:"docs_labels"`
	// RequireTestReport: refuse to start, and refuse to call a change
	// verified, without a JUnit test report on the pipeline.
	RequireTestReport *bool `yaml:"require_test_report"`
	// MaxAttempts caps attempts per issue; MaxOpen caps open change merge
	// requests per project.
	MaxAttempts *int64 `yaml:"max_attempts"`
	MaxOpen     *int64 `yaml:"max_open"`
	// VerifyTimeout: how long the exit check waits for the MR pipeline.
	VerifyTimeout *Duration `yaml:"verify_timeout"`
	// MaxFiles and MaxChangedLines are the size caps of one change.
	MaxFiles        *int64 `yaml:"max_files"`
	MaxChangedLines *int64 `yaml:"max_changed_lines"`
	// Paths narrows what a change may touch. PROJECT ONLY.
	Paths []string `yaml:"paths"`
	// AutoMerge is plan section 5's policy for this workflow; absent means off.
	AutoMerge AutoMergePolicy `yaml:"auto_merge"`
}

// IsZero reports whether the layer says nothing at all.
func (p ChangePolicy) IsZero() bool {
	return p.Triggers == nil && p.CreateClasses == nil && p.RequireDocs == nil && p.DocsLabels == nil &&
		p.RequireTestReport == nil && p.MaxAttempts == nil && p.MaxOpen == nil && p.VerifyTimeout == nil &&
		p.MaxFiles == nil && p.MaxChangedLines == nil && len(p.Paths) == 0 &&
		p.AutoMerge.Mode == nil && p.AutoMerge.Classes == nil && p.AutoMerge.MaxPerDay == nil
}

type intBound struct {
	key     string
	v       *int64
	ceiling int64
}

func (p ChangePolicy) ints() []intBound {
	return []intBound{
		{"max_attempts", p.MaxAttempts, ChangeMaxAttemptsCeiling},
		{"max_open", p.MaxOpen, ChangeMaxOpenCeiling},
		{"max_files", p.MaxFiles, ChangeMaxFilesCeiling},
		{"max_changed_lines", p.MaxChangedLines, ChangeMaxChangedLinesCeiling},
		{"auto_merge.max_per_day", p.AutoMerge.MaxPerDay, AutoMergeMaxPerDayCeiling},
	}
}

// validDocsLabel: non-empty, no leading or trailing white space, one line, at
// most MaxDocsLabelLen characters. The schema's pattern and maxLength say the
// same.
func validDocsLabel(l string) bool {
	return docsLabelRe.MatchString(l) && utf8.RuneCountInString(l) <= MaxDocsLabelLen
}

// Validate checks one layer. It refuses an unknown trigger form (so
// `triage-verdict`), create class, require_docs value, merge mode or merge
// class; a malformed, repeated or surplus docs label; a number below 1 or
// above its ceiling; a verify_timeout that is not positive or exceeds its
// ceiling; a malformed path pattern; and `paths` from an operator layer.
func (p ChangePolicy) Validate(layer scope.Layer) error {
	if layer != scope.LayerOperator && layer != scope.LayerProject {
		return fmt.Errorf("change: unknown layer %q", layer)
	}
	for _, l := range []struct {
		key     string
		got     []string
		allowed []string
	}{
		{"triggers", p.Triggers, changeTriggerOrder},
		{"create_classes", p.CreateClasses, createClassOrder},
		{"auto_merge.classes", p.AutoMerge.Classes, changeMergeClassOrder},
	} {
		for _, v := range l.got {
			if !contains(l.allowed, v) {
				return fmt.Errorf("change: %s: %q is not one of %v", l.key, v, l.allowed)
			}
		}
	}
	if v := p.RequireDocs; v != nil {
		if _, ok := requireDocsRank[*v]; !ok {
			return fmt.Errorf("change: require_docs: %q is not one of [%s %s %s]", *v, RequireDocsNever, RequireDocsWhenLabelled, RequireDocsAlways)
		}
	}
	if len(p.DocsLabels) > MaxDocsLabels {
		return fmt.Errorf("change: docs_labels: %d labels, the limit is %d", len(p.DocsLabels), MaxDocsLabels)
	}
	for i, l := range p.DocsLabels {
		if !validDocsLabel(l) {
			return fmt.Errorf("change: docs_labels: %q is not a label name (1 to %d characters, one line, no leading or trailing space)", l, MaxDocsLabelLen)
		}
		if contains(p.DocsLabels[:i], l) {
			return fmt.Errorf("change: docs_labels: %q is listed twice", l)
		}
	}
	if v := p.AutoMerge.Mode; v != nil && !autoMergeModes[*v] {
		return fmt.Errorf("change: auto_merge.mode: %q is not one of [%s %s %s]", *v, AutoMergeOff, AutoMergeAfterHumanApproval, AutoMergeAfterAgenticReview)
	}
	for _, n := range p.ints() {
		if n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {
			return fmt.Errorf("change: %s is %d; it must be between 1 and %d (the ceiling can only be tightened)", n.key, *n.v, n.ceiling)
		}
	}
	if v := p.VerifyTimeout; v != nil && (*v <= 0 || time.Duration(*v) > ChangeVerifyTimeoutCeiling) {
		return fmt.Errorf("change: verify_timeout is %s; it must be more than 0 and at most %s", time.Duration(*v), ChangeVerifyTimeoutCeiling)
	}
	if len(p.Paths) > 0 && layer == scope.LayerOperator {
		return fmt.Errorf("change: paths is project-only; an operator narrows paths with scope.protected_paths")
	}
	if len(p.Paths) > scope.MaxPatterns {
		return fmt.Errorf("change: paths: %d patterns, the limit is %d", len(p.Paths), scope.MaxPatterns)
	}
	for _, pat := range p.Paths {
		if err := scope.ValidatePattern(pat); err != nil {
			return fmt.Errorf("change: paths: %w", err)
		}
	}
	return nil
}

// CheckChange is the project-layer check Load runs after the schema: the
// `change:` section validates, and its automatic merge is not switched on
// without what it depends on.
//
//   - `change.auto_merge.mode` other than off requires `actions.features: true`
//     in the same file: without the workflow there is no merge request to
//     merge.
//   - `change.auto_merge.mode: after-agentic-review` additionally requires
//     `actions.reviews: true`: that mode merges on a review's finding, and
//     with reviews off there is never one (plan 5.1 M9).
//
// The rest of the section may be written while `actions.features` is false: it
// is validated and otherwise unused.
func CheckChange(p Policy) error {
	if err := p.Change.Validate(scope.LayerProject); err != nil {
		return err
	}
	if m := p.Change.AutoMerge.Mode; m != nil && *m != AutoMergeOff {
		if !on(p.Actions.Features) {
			return fmt.Errorf("change: auto_merge.mode: %s requires actions.features: true (with the workflow off there is no merge request to merge)", *m)
		}
		if *m == AutoMergeAfterAgenticReview && !on(p.Actions.Reviews) {
			return fmt.Errorf("change: auto_merge.mode: %s requires actions.reviews: true (that mode merges on a review's finding; with reviews off there is never one)", *m)
		}
	}
	return nil
}

// EffectiveChange is the folded `change:` section: defaults applied, every
// layer's tightening folded in. It says HOW a change may be made; WHETHER one
// may be made at all is Effective.Actions.Features, and a consumer must check
// that first -- these values are populated (with the defaults) for a project
// that never opted in.
type EffectiveChange struct {
	Triggers          []string // in the order label, command, mention; never nil
	CreateClasses     []string // in the order source, test, docs; never nil
	RequireDocs       string
	DocsLabels        []string // never nil
	RequireTestReport bool
	MaxAttempts       int
	MaxOpen           int
	VerifyTimeout     time.Duration
	MaxFiles          int
	MaxChangedLines   int
	Paths             []string // the project's narrowing globs; never nil
	AutoMerge         EffectiveAutoMerge
}

// AllowsTrigger reports whether a request of this form (ChangeTriggerLabel,
// ChangeTriggerCommand, ChangeTriggerMention) may start the workflow. It says
// nothing about WHO asked: the dispatcher checks ChangeMinAccessLevel.
func (e EffectiveChange) AllowsTrigger(form string) bool { return contains(e.Triggers, form) }

// AllowsCreateClass reports whether a change may create a new file whose path
// is of this pkg/scope class.
func (e EffectiveChange) AllowsCreateClass(class string) bool {
	return contains(e.CreateClasses, class)
}

// DocsRequired reports whether documentation is required of a change to source
// code for an issue carrying these labels: never under `never`, always under
// `always`, and under `when-labelled` exactly when one of the labels is in
// DocsLabels. A label can only ADD the requirement. Anything else reads as
// `always`. (A change that touches no source needs no documentation under any
// value; that half of the rule is the predicate's, plan 2.4.)
func (e EffectiveChange) DocsRequired(issueLabels []string) bool {
	switch e.RequireDocs {
	case RequireDocsNever:
		return false
	case RequireDocsWhenLabelled:
		for _, l := range issueLabels {
			if contains(e.DocsLabels, l) {
				return true
			}
		}
		return false
	}
	return true
}

// TightenScope returns the project's `scope:` layer with its file and line caps
// lowered to max_files / max_changed_lines, exactly as
// EffectivePipelineFix.TightenScope does for a fix.
func (e EffectiveChange) TightenScope(project scope.Policy) scope.Policy {
	return tightenScope(project, e.MaxFiles, e.MaxChangedLines)
}

func tightenScope(project scope.Policy, maxFiles, maxLines int) scope.Policy {
	lower := func(have *int64, limit int) *int64 {
		v := int64(limit)
		if have != nil && *have < v {
			v = *have
		}
		return &v
	}
	project.Caps.MaxFiles = lower(project.Caps.MaxFiles, maxFiles)
	project.Caps.MaxLines = lower(project.Caps.MaxLines, maxLines)
	return project
}

// FoldChange folds `next` onto `base`, two OPERATOR layers. The result is at
// least as tight as either: lists intersect, numbers and the timeout take the
// minimum, the stricter require_docs wins, docs_labels are unioned (a label
// can only add a requirement), `require_test_report: true` sticks, and two
// layers naming different auto-merge modes fold to off. `paths` is project-only
// and is not carried.
func FoldChange(base, next ChangePolicy) ChangePolicy {
	return ChangePolicy{
		Triggers:          intersectLists(base.Triggers, next.Triggers),
		CreateClasses:     intersectLists(base.CreateClasses, next.CreateClasses),
		RequireDocs:       stricterDocs(base.RequireDocs, next.RequireDocs),
		DocsLabels:        unionLists(base.DocsLabels, next.DocsLabels),
		RequireTestReport: stickyTrue(base.RequireTestReport, next.RequireTestReport),
		MaxAttempts:       minInt(base.MaxAttempts, next.MaxAttempts),
		MaxOpen:           minInt(base.MaxOpen, next.MaxOpen),
		VerifyTimeout:     minDuration(base.VerifyTimeout, next.VerifyTimeout),
		MaxFiles:          minInt(base.MaxFiles, next.MaxFiles),
		MaxChangedLines:   minInt(base.MaxChangedLines, next.MaxChangedLines),
		AutoMerge: AutoMergePolicy{
			Mode:      foldMode(base.AutoMerge.Mode, next.AutoMerge.Mode),
			Classes:   intersectLists(base.AutoMerge.Classes, next.AutoMerge.Classes),
			MaxPerDay: minInt(base.AutoMerge.MaxPerDay, next.AutoMerge.MaxPerDay),
		},
	}
}

// ResolveChange folds the operator layers and the project's section into the
// effective one. The PROJECT'S value (or the default, when the project is
// silent) is the base; an operator layer can only tighten it.
//
// Total over any input, including Policies that never saw a schema: unknown
// list members and malformed labels are dropped, an unknown require_docs reads
// as always, an unknown mode as off, and numbers are clamped to [0, ceiling].
func ResolveChange(instance, group, project ChangePolicy) EffectiveChange {
	op := FoldChange(instance, group)
	eff := EffectiveChange{
		Triggers:          resolveList(changeTriggerOrder, defaultChangeTriggers, project.Triggers, op.Triggers),
		CreateClasses:     resolveList(createClassOrder, defaultCreateClasses, project.CreateClasses, op.CreateClasses),
		RequireTestReport: on(project.RequireTestReport) || on(op.RequireTestReport),
		MaxAttempts:       resolveNum(DefaultChangeMaxAttempts, ChangeMaxAttemptsCeiling, project.MaxAttempts, op.MaxAttempts),
		MaxOpen:           resolveNum(DefaultChangeMaxOpen, ChangeMaxOpenCeiling, project.MaxOpen, op.MaxOpen),
		VerifyTimeout:     resolveDuration(DefaultChangeVerifyTimeout, ChangeVerifyTimeoutCeiling, project.VerifyTimeout, op.VerifyTimeout),
		MaxFiles:          resolveNum(DefaultChangeMaxFiles, ChangeMaxFilesCeiling, project.MaxFiles, op.MaxFiles),
		MaxChangedLines:   resolveNum(DefaultChangeMaxChangedLines, ChangeMaxChangedLinesCeiling, project.MaxChangedLines, op.MaxChangedLines),
		Paths:             append([]string{}, project.Paths...),
		AutoMerge: EffectiveAutoMerge{
			Mode:      resolveMode(project.AutoMerge.Mode, op.AutoMerge.Mode),
			Classes:   resolveList(changeMergeClassOrder, defaultChangeMergeClass, project.AutoMerge.Classes, op.AutoMerge.Classes),
			MaxPerDay: resolveNum(DefaultAutoMergeMaxPerDay, AutoMergeMaxPerDayCeiling, project.AutoMerge.MaxPerDay, op.AutoMerge.MaxPerDay),
		},
	}

	docs := RequireDocsWhenLabelled
	if project.RequireDocs != nil {
		docs = *project.RequireDocs
	}
	if s := stricterDocs(&docs, op.RequireDocs); s != nil {
		docs = *s
	}
	if _, ok := requireDocsRank[docs]; !ok {
		docs = RequireDocsAlways
	}
	eff.RequireDocs = docs

	labels := defaultDocsLabels
	if project.DocsLabels != nil {
		labels = project.DocsLabels
	}
	eff.DocsLabels = []string{}
	for _, l := range unionLists(labels, op.DocsLabels) {
		if validDocsLabel(l) && !contains(eff.DocsLabels, l) {
			eff.DocsLabels = append(eff.DocsLabels, l)
		}
	}
	return eff
}

// resolveList: the project's list (or the default) intersected with the
// operator's, in canonical order, unknown members dropped; never nil.
func resolveList(order, def, proj, oper []string) []string {
	base := def
	if proj != nil {
		base = proj
	}
	if oper != nil {
		base = intersectLists(base, oper)
	}
	out := []string{}
	for _, v := range order {
		if contains(base, v) {
			out = append(out, v)
		}
	}
	return out
}

// resolveNum: the project's number (or the default), lowered by the
// operator's, clamped to [0, ceiling].
func resolveNum(def int, ceiling int64, proj, oper *int64) int {
	v := int64(def)
	if proj != nil {
		v = *proj
	}
	if oper != nil && *oper < v {
		v = *oper
	}
	return int(min(max(v, 0), ceiling))
}

func resolveDuration(def, ceiling time.Duration, proj, oper *Duration) time.Duration {
	v := def
	if proj != nil {
		v = time.Duration(*proj)
	}
	if oper != nil && time.Duration(*oper) < v {
		v = time.Duration(*oper)
	}
	return min(max(v, 0), ceiling)
}

// resolveMode: off unless the project names a mode that every operator layer
// naming one agrees with; an unknown mode is off.
func resolveMode(proj, oper *string) string {
	mode := AutoMergeOff
	if proj != nil {
		mode = *proj
	}
	if m := foldMode(&mode, oper); m != nil {
		mode = *m
	}
	if !autoMergeModes[mode] {
		mode = AutoMergeOff
	}
	return mode
}

// stricterDocs returns the stricter of two require_docs values. A value it
// does not know is read as the strictest (always).
func stricterDocs(a, b *string) *string {
	rank := func(s *string) int {
		if r, ok := requireDocsRank[*s]; ok {
			return r
		}
		return 99
	}
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case rank(b) > rank(a):
		return b
	}
	return a
}

// unionLists: nil is "silent"; otherwise a's members, then b's that a lacks.
func unionLists(a, b []string) []string {
	if a == nil && b == nil {
		return nil
	}
	out := append([]string{}, a...)
	for _, v := range b {
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// stickyTrue: nil is silent; an explicit true anywhere sticks.
func stickyTrue(a, b *bool) *bool {
	if a != nil && *a {
		return a
	}
	if b != nil {
		return b
	}
	return a
}

func minDuration(a, b *Duration) *Duration {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	}
	return a
}
