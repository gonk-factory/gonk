package gonkcfg

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The pipeline-fix opt-in (gonk-2usw, plan section 3.1): `actions.pipeline_fixes`
// and the `pipeline_fix:` section. Everything is OFF unless a project writes it,
// and every key can only TIGHTEN what the workflow is given: the ceilings below
// are the pack's maximum and no layer raises them.
//
// Reference: docs/reference/gonk-pipeline-fix.md. The two JSON Schemas carry
// the same rules for a document; Validate repeats them for a Policy built in Go.

// PipelineFixKey is the configuration key the section sits under.
const PipelineFixKey = "pipeline_fix"

// Diagnosis classes a fix may follow. `flaky`, `infrastructure` and `unknown`
// are deliberately not here: no configuration can make them eligible (plan 3.2
// row E4).
const (
	FixClassCode          = "code"
	FixClassTest          = "test"
	FixClassConfiguration = "configuration"
)

// What a failed pipeline ran on: the default branch (`ref`) or a merge request
// (`mr`).
const (
	FixTargetRef = "ref"
	FixTargetMR  = "mr"
)

// How an execution-changing CI fix is handled (plan 6.2, owner decision Q6).
// The order deny < manual-run < auto is "strictest first"; a fold keeps the
// strictest.
const (
	CIExecDeny      = "deny"
	CIExecManualRun = "manual-run"
	CIExecAuto      = "auto"
)

// Auto-merge modes (plan section 9). Off unless a project names another.
const (
	AutoMergeOff                = "off"
	AutoMergeAfterHumanApproval = "after-human-approval"
	AutoMergeAfterAgenticReview = "after-agentic-review"
)

// Observed diff classes an automatic merge may be allowed for: the pack
// ceiling of plan 9.2. These are pkg/scope path classes, not diagnosis classes.
const (
	AutoMergeClassDocs = string(scope.ClassDocs)
	AutoMergeClassCI   = string(scope.ClassCI)
)

// Defaults: what a project that opts in with `actions.pipeline_fixes: true`
// and writes nothing else gets.
const (
	DefaultFixMaxAttempts     = 2
	DefaultFixMaxChain        = 5
	DefaultFixVerifyTimeout   = 2 * time.Hour
	DefaultFixMaxFiles        = 3
	DefaultFixMaxChangedLines = 40
)

// Ceilings: the most any layer may ask for. A document above one is refused
// (schema `maximum`, and Validate); a Policy built in Go that slipped past both
// is clamped by ResolvePipelineFix.
const (
	FixMaxAttemptsCeiling     = 3
	FixMaxChainCeiling        = 10
	FixVerifyTimeoutCeiling   = 6 * time.Hour
	FixMaxFilesCeiling        = 3
	FixMaxChangedLinesCeiling = 40
)

var (
	fixClassOrder       = []string{FixClassCode, FixClassTest, FixClassConfiguration}
	fixTargetOrder      = []string{FixTargetRef, FixTargetMR}
	autoMergeClassOrder = []string{AutoMergeClassDocs, AutoMergeClassCI}
	ciExecRank          = map[string]int{CIExecDeny: 0, CIExecManualRun: 1, CIExecAuto: 2}
	autoMergeModes      = map[string]bool{AutoMergeOff: true, AutoMergeAfterHumanApproval: true, AutoMergeAfterAgenticReview: true}

	defaultFixClasses       = []string{FixClassCode, FixClassTest}
	defaultFixTargets       = []string{FixTargetRef}
	defaultAutoMergeClasses = []string{AutoMergeClassDocs}
)

// Duration is a YAML duration scalar ("2h", "90m", "45s").
type Duration time.Duration

// UnmarshalYAML accepts a string time.ParseDuration understands. A bare number
// is refused: it has no unit, and guessing one is how a 2h timeout becomes 2ns.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line %d: a duration must be a string with a unit, e.g. \"2h\"", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration: %w", n.Line, n.Value, err)
	}
	*d = Duration(v)
	return nil
}

// PipelineFixPolicy is ONE layer's `pipeline_fix:` section. nil means "this
// layer says nothing"; for the two lists, a non-nil EMPTY list means "none".
type PipelineFixPolicy struct {
	// Classes: diagnosis classes that may be followed by a fix.
	Classes []string `yaml:"classes"`
	// Targets: which failed pipelines may be fixed (`ref`, `mr`).
	Targets []string `yaml:"targets"`
	// AllowCI is the project's opt-in to the per-dispatch `ci` lift for
	// `configuration` fixes. An operator layer may only veto it.
	AllowCI *bool `yaml:"allow_ci"`
	// CIExecChanges: deny | manual-run | auto.
	CIExecChanges *string `yaml:"ci_exec_changes"`
	// MaxAttempts caps attempts per fix; MaxChain caps fix MRs per red streak.
	MaxAttempts *int64 `yaml:"max_attempts"`
	MaxChain    *int64 `yaml:"max_chain"`
	// VerifyTimeout: how long the exit check waits for the MR pipeline.
	VerifyTimeout *Duration `yaml:"verify_timeout"`
	// MaxFiles and MaxChangedLines are the size caps of one fix.
	MaxFiles        *int64 `yaml:"max_files"`
	MaxChangedLines *int64 `yaml:"max_changed_lines"`
	// Paths narrows what a fix may edit (intersection with everything else).
	// PROJECT ONLY: an operator narrows paths with `scope.protected_paths`.
	Paths []string `yaml:"paths"`
	// AutoMerge is section 9's policy; absent means off.
	AutoMerge AutoMergePolicy `yaml:"auto_merge"`
}

// AutoMergePolicy is one layer's `pipeline_fix.auto_merge:`.
type AutoMergePolicy struct {
	Mode    *string  `yaml:"mode"`
	Classes []string `yaml:"classes"`
	// MaxPerDay caps automatic merges on the project per 24 hours (gonk-cuwn
	// plan 5.1 M11). Default and ceiling: DefaultAutoMergeMaxPerDay,
	// AutoMergeMaxPerDayCeiling.
	MaxPerDay *int64 `yaml:"max_per_day"`
}

// IsZero reports whether the layer says nothing at all.
func (p PipelineFixPolicy) IsZero() bool {
	return p.Classes == nil && p.Targets == nil && p.AllowCI == nil && p.CIExecChanges == nil &&
		p.MaxAttempts == nil && p.MaxChain == nil && p.VerifyTimeout == nil && p.MaxFiles == nil &&
		p.MaxChangedLines == nil && len(p.Paths) == 0 && p.AutoMerge.Mode == nil && p.AutoMerge.Classes == nil &&
		p.AutoMerge.MaxPerDay == nil
}

func (p PipelineFixPolicy) ints() []struct {
	key     string
	v       *int64
	ceiling int64
} {
	return []struct {
		key     string
		v       *int64
		ceiling int64
	}{
		{"max_attempts", p.MaxAttempts, FixMaxAttemptsCeiling},
		{"max_chain", p.MaxChain, FixMaxChainCeiling},
		{"max_files", p.MaxFiles, FixMaxFilesCeiling},
		{"max_changed_lines", p.MaxChangedLines, FixMaxChangedLinesCeiling},
		{"auto_merge.max_per_day", p.AutoMerge.MaxPerDay, AutoMergeMaxPerDayCeiling},
	}
}

// Validate checks one layer. It refuses an unknown class, target, mode or
// ci_exec_changes value; a number below 1 or above its ceiling; a
// verify_timeout that is not positive or exceeds its ceiling; a malformed path
// pattern; and `paths` from an operator layer.
//
// pkg/gonkcfg.Load and pkg/opercfg.Load call it, so a bad section makes the
// project invalid -- or stops the meter starting -- before any spend.
func (p PipelineFixPolicy) Validate(layer scope.Layer) error {
	if layer != scope.LayerOperator && layer != scope.LayerProject {
		return fmt.Errorf("pipeline_fix: unknown layer %q", layer)
	}
	for _, l := range []struct {
		key     string
		got     []string
		allowed []string
	}{
		{"classes", p.Classes, fixClassOrder},
		{"targets", p.Targets, fixTargetOrder},
		{"auto_merge.classes", p.AutoMerge.Classes, autoMergeClassOrder},
	} {
		for _, v := range l.got {
			if !contains(l.allowed, v) {
				return fmt.Errorf("pipeline_fix: %s: %q is not one of %v", l.key, v, l.allowed)
			}
		}
	}
	if v := p.CIExecChanges; v != nil {
		if _, ok := ciExecRank[*v]; !ok {
			return fmt.Errorf("pipeline_fix: ci_exec_changes: %q is not one of [%s %s %s]", *v, CIExecDeny, CIExecManualRun, CIExecAuto)
		}
	}
	if v := p.AutoMerge.Mode; v != nil && !autoMergeModes[*v] {
		return fmt.Errorf("pipeline_fix: auto_merge.mode: %q is not one of [%s %s %s]", *v, AutoMergeOff, AutoMergeAfterHumanApproval, AutoMergeAfterAgenticReview)
	}
	for _, n := range p.ints() {
		if n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {
			return fmt.Errorf("pipeline_fix: %s is %d; it must be between 1 and %d (the ceiling can only be tightened)", n.key, *n.v, n.ceiling)
		}
	}
	if v := p.VerifyTimeout; v != nil && (*v <= 0 || time.Duration(*v) > FixVerifyTimeoutCeiling) {
		return fmt.Errorf("pipeline_fix: verify_timeout is %s; it must be more than 0 and at most %s", time.Duration(*v), FixVerifyTimeoutCeiling)
	}
	if len(p.Paths) > 0 && layer == scope.LayerOperator {
		return fmt.Errorf("pipeline_fix: paths is project-only; an operator narrows paths with scope.protected_paths")
	}
	if len(p.Paths) > scope.MaxPatterns {
		return fmt.Errorf("pipeline_fix: paths: %d patterns, the limit is %d", len(p.Paths), scope.MaxPatterns)
	}
	for _, pat := range p.Paths {
		if err := scope.ValidatePattern(pat); err != nil {
			return fmt.Errorf("pipeline_fix: paths: %w", err)
		}
	}
	return nil
}

// CheckPipelineFix is the project-layer check Load runs after the schema: the
// `pipeline_fix:` section validates, and `actions.pipeline_fixes: true` is
// written together with `actions.pipelines: true`.
//
// The second rule is plan 3.1's: a fix follows a diagnosis, so fixes without
// diagnoses is a configuration that can never do anything -- refused at
// registration rather than left to look like "gonk is ignoring my failures".
func CheckPipelineFix(p Policy) error {
	if on(p.Actions.PipelineFixes) && !on(p.Actions.Pipelines) {
		return fmt.Errorf("actions.pipeline_fixes: true requires actions.pipelines: true (a fix follows a diagnosis; with no diagnosis there is nothing to fix)")
	}
	return p.PipelineFix.Validate(scope.LayerProject)
}

func on(b *bool) bool { return b != nil && *b }

// EffectivePipelineFix is the folded `pipeline_fix:` section: defaults applied,
// every layer's tightening folded in. It says HOW a fix may be made; WHETHER
// one may be made at all is Effective.Actions.PipelineFixes, and a consumer
// must check that first -- these values are populated (with the defaults) for a
// project that never opted in.
type EffectivePipelineFix struct {
	Classes         []string // in the order code, test, configuration; never nil
	Targets         []string // in the order ref, mr; never nil
	AllowCI         bool
	CIExecChanges   string
	MaxAttempts     int
	MaxChain        int
	VerifyTimeout   time.Duration
	MaxFiles        int
	MaxChangedLines int
	Paths           []string // the project's narrowing globs; never nil
	AutoMerge       EffectiveAutoMerge
}

// EffectiveAutoMerge is the folded `pipeline_fix.auto_merge:`.
type EffectiveAutoMerge struct {
	Mode    string   // AutoMergeOff unless the project named a mode no operator layer contradicts
	Classes []string // in the order docs, ci; never nil
	// MaxPerDay: automatic merges allowed on the project per 24 hours.
	MaxPerDay int
}

// AllowsClass reports whether a diagnosis of this class may be followed by a fix.
func (e EffectivePipelineFix) AllowsClass(class string) bool { return contains(e.Classes, class) }

// AllowsTarget reports whether a failed pipeline on this kind of target
// (FixTargetRef, FixTargetMR) may be fixed.
func (e EffectivePipelineFix) AllowsTarget(target string) bool { return contains(e.Targets, target) }

// LiftOptIn is the project's lift opt-in in the form pkg/scope wants it:
// scope.Input.LiftOptIn. `allow_ci: true` opts in to the `ci` lift and to
// nothing else; without it the answer is nil and Fold grants no lift. The
// pack row's `liftable` and an operator's `scope.deny_lift` still apply --
// this only supplies the PROJECT'S third of that conjunction.
func (e EffectivePipelineFix) LiftOptIn() []scope.Class {
	if !e.AllowCI {
		return nil
	}
	return []scope.Class{scope.ClassCI}
}

// TightenScope returns the project's `scope:` layer with its file and line caps
// lowered to max_files / max_changed_lines. Pass the result as
// scope.Input.Project: Fold minimises caps across layers, so this can only
// tighten, and a project `scope.caps` already below these values stands.
func (e EffectivePipelineFix) TightenScope(project scope.Policy) scope.Policy {
	lower := func(have *int64, cap int) *int64 {
		v := int64(cap)
		if have != nil && *have < v {
			v = *have
		}
		return &v
	}
	project.Caps.MaxFiles = lower(project.Caps.MaxFiles, e.MaxFiles)
	project.Caps.MaxLines = lower(project.Caps.MaxLines, e.MaxChangedLines)
	return project
}

// FoldPipelineFix folds `next` onto `base`, two OPERATOR layers (the instance
// and a group, or a group and its nested group). The result is at least as
// tight as either: lists intersect, numbers and the timeout take the minimum,
// `allow_ci: false` sticks, the stricter ci_exec_changes wins, and two layers
// naming different auto-merge modes fold to off.
func FoldPipelineFix(base, next PipelineFixPolicy) PipelineFixPolicy {
	out := PipelineFixPolicy{
		Classes:         intersectLists(base.Classes, next.Classes),
		Targets:         intersectLists(base.Targets, next.Targets),
		AllowCI:         vetoFold(base.AllowCI, next.AllowCI),
		CIExecChanges:   stricterExec(base.CIExecChanges, next.CIExecChanges),
		MaxAttempts:     minInt(base.MaxAttempts, next.MaxAttempts),
		MaxChain:        minInt(base.MaxChain, next.MaxChain),
		MaxFiles:        minInt(base.MaxFiles, next.MaxFiles),
		MaxChangedLines: minInt(base.MaxChangedLines, next.MaxChangedLines),
		AutoMerge: AutoMergePolicy{
			Mode:    foldMode(base.AutoMerge.Mode, next.AutoMerge.Mode),
			Classes: intersectLists(base.AutoMerge.Classes, next.AutoMerge.Classes),
		},
	}
	out.AutoMerge.MaxPerDay = minInt(base.AutoMerge.MaxPerDay, next.AutoMerge.MaxPerDay)
	switch {
	case base.VerifyTimeout == nil:
		out.VerifyTimeout = next.VerifyTimeout
	case next.VerifyTimeout == nil || *base.VerifyTimeout <= *next.VerifyTimeout:
		out.VerifyTimeout = base.VerifyTimeout
	default:
		out.VerifyTimeout = next.VerifyTimeout
	}
	return out
}

// ResolvePipelineFix folds the operator layers and the project's section into
// the effective one. The PROJECT'S value (or the default, when the project is
// silent) is the base; an operator layer can only tighten it -- an operator
// value wider than the base has no effect, so an operator can never opt a
// project in to a class, a target, the CI lift, `auto`, or a merge mode.
//
// Total over any input, including Policies that never saw a schema: unknown
// list members are dropped, an unknown ci_exec_changes reads as deny, an
// unknown mode as off, and numbers are clamped to [0, ceiling].
func ResolvePipelineFix(instance, group, project PipelineFixPolicy) EffectivePipelineFix {
	op := FoldPipelineFix(instance, group)

	list := func(order, def, proj, oper []string) []string {
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
	num := func(def int, ceiling int64, proj, oper *int64) int {
		v := int64(def)
		if proj != nil {
			v = *proj
		}
		if oper != nil && *oper < v {
			v = *oper
		}
		return int(min(max(v, 0), ceiling))
	}

	eff := EffectivePipelineFix{
		Classes:         list(fixClassOrder, defaultFixClasses, project.Classes, op.Classes),
		Targets:         list(fixTargetOrder, defaultFixTargets, project.Targets, op.Targets),
		AllowCI:         andAction(project.AllowCI, op.AllowCI),
		MaxAttempts:     num(DefaultFixMaxAttempts, FixMaxAttemptsCeiling, project.MaxAttempts, op.MaxAttempts),
		MaxChain:        num(DefaultFixMaxChain, FixMaxChainCeiling, project.MaxChain, op.MaxChain),
		MaxFiles:        num(DefaultFixMaxFiles, FixMaxFilesCeiling, project.MaxFiles, op.MaxFiles),
		MaxChangedLines: num(DefaultFixMaxChangedLines, FixMaxChangedLinesCeiling, project.MaxChangedLines, op.MaxChangedLines),
		Paths:           append([]string{}, project.Paths...),
		AutoMerge: EffectiveAutoMerge{
			Classes: list(autoMergeClassOrder, defaultAutoMergeClasses, project.AutoMerge.Classes, op.AutoMerge.Classes),
		},
	}

	exec := CIExecManualRun
	if project.CIExecChanges != nil {
		exec = *project.CIExecChanges
	}
	if s := stricterExec(&exec, op.CIExecChanges); s != nil {
		exec = *s
	}
	if _, ok := ciExecRank[exec]; !ok {
		exec = CIExecDeny
	}
	eff.CIExecChanges = exec

	timeout := DefaultFixVerifyTimeout
	if project.VerifyTimeout != nil {
		timeout = time.Duration(*project.VerifyTimeout)
	}
	if op.VerifyTimeout != nil && time.Duration(*op.VerifyTimeout) < timeout {
		timeout = time.Duration(*op.VerifyTimeout)
	}
	eff.VerifyTimeout = min(max(timeout, 0), FixVerifyTimeoutCeiling)

	mode := AutoMergeOff
	if project.AutoMerge.Mode != nil {
		mode = *project.AutoMerge.Mode
	}
	if m := foldMode(&mode, op.AutoMerge.Mode); m != nil {
		mode = *m
	}
	if !autoMergeModes[mode] {
		mode = AutoMergeOff
	}
	eff.AutoMerge.Mode = mode
	eff.AutoMerge.MaxPerDay = num(DefaultAutoMergeMaxPerDay, AutoMergeMaxPerDayCeiling, project.AutoMerge.MaxPerDay, op.AutoMerge.MaxPerDay)
	return eff
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// intersectLists: nil is "silent". Two set lists give a NON-NIL intersection,
// so "these layers allow nothing in common" cannot be mistaken for silence.
func intersectLists(a, b []string) []string {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return append([]string{}, b...)
	case b == nil:
		return append([]string{}, a...)
	}
	out := []string{}
	for _, v := range a {
		if contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// vetoFold: nil is silent; an explicit false anywhere sticks.
func vetoFold(a, b *bool) *bool {
	if a != nil && !*a {
		return a
	}
	if b != nil {
		return b
	}
	return a
}

func minInt(a, b *int64) *int64 {
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

// stricterExec returns the stricter of two ci_exec_changes values. A value it
// does not know is read as the strictest (deny).
func stricterExec(a, b *string) *string {
	rank := func(s *string) int {
		if r, ok := ciExecRank[*s]; ok {
			return r
		}
		return -1
	}
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case rank(b) < rank(a):
		return b
	}
	return a
}

// foldMode: two layers naming the same mode keep it; naming different modes
// folds to off. A mode is never upgraded by a fold.
func foldMode(a, b *string) *string {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a == *b:
		return a
	}
	off := AutoMergeOff
	return &off
}
