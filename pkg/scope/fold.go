package scope

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Row is one agent's pack row: the workflow's MAXIMUM (066 section 3.1). It
// comes from the baked pack (LoadRow) and from nowhere else. The zero Row
// allows nothing, which is what an agent with no [scope] section has.
type Row struct {
	// Classes are the non-protected classes the workflow may ever write.
	Classes []Class
	// Liftable are the protected classes a DISPATCH of this workflow may be
	// granted (only `ci` can appear; the floor cannot).
	Liftable []Class
	// Caps are the workflow's size ceilings.
	Caps Caps
}

func (r Row) validate() error {
	for _, c := range r.Classes {
		if !c.Valid() {
			return fmt.Errorf("scope: pack row: unknown class %q", c)
		}
		if c.Sensitivity() == Protected {
			return fmt.Errorf("scope: pack row: classes lists %q, a protected class; protected classes are never in an allowed set (ci is reached through liftable)", c)
		}
	}
	for _, c := range r.Liftable {
		if !Liftable(c) {
			return fmt.Errorf("scope: pack row: %q is not liftable; only ci is, and control and secret are the floor", c)
		}
	}
	for _, c := range []struct {
		key string
		v   int64
	}{{"max_files", r.Caps.MaxFiles}, {"max_lines", r.Caps.MaxLines}, {"max_deletes", r.Caps.MaxDeletes},
		{"max_file_bytes", r.Caps.MaxFileBytes}, {"max_patch_bytes", r.Caps.MaxPatchBytes}} {
		if c.v < 0 {
			return fmt.Errorf("scope: pack row: %s is negative (%d)", c.key, c.v)
		}
	}
	return nil
}

// Input is everything a fold reads. Each field names where it must come from;
// none of them is the batch, the session's tree, `.agent/` or issue text.
type Input struct {
	// Row: the baked pack (LoadRow).
	Row Row
	// Instance and Group: the operator config (opercfg.OperatorConfig.Instance
	// and GroupFor(project)), validated at meter start.
	Instance, Group Policy
	// Project: the `scope:` section of the project's .gonk.yml, as read by the
	// controller at BaseSHA (gonkcfg.ProjectConfig.Scope).
	Project Policy
	// BaseSHA is the commit the controller pinned and read Project at. It must
	// be a full commit SHA: a branch name is not a pin (066 finding F2).
	BaseSHA string
	// Enabled is the workflow's enablement, already AND-ed across the layers by
	// gonkcfg.Resolve (Effective.Actions): project opts in, operator may veto.
	Enabled bool
	// LiftOptIn names the liftable classes the PROJECT has opted in to having
	// lifted. It is set by the controller from project config (for the
	// pipeline-fix workflow, `pipeline_fix.allow_ci`); absent means none.
	LiftOptIn []Class
}

var shaRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// layerPattern is one configured pattern with where it came from.
type layerPattern struct {
	pattern string
	layer   Layer
	name    string // "instance", "group", "project"
	key     string
	to      Class
}

// Effective is a folded scope: what one workflow may touch in one project. It
// is produced only by Fold and is never wider than the pack row it was folded
// from (TestNoFoldIsWiderThanThePackRow).
type Effective struct {
	// Enabled is false when any layer switched the workflow off. A disabled
	// scope allows no class and grants no lift.
	Enabled bool
	// Classes is the allowed set, in class order: the row's, intersected with
	// every layer's (and with any Narrow).
	Classes []Class
	// Caps are the row's, minimised against every layer's.
	Caps Caps
	// BaseSHA is the pinned commit the project layer was read at.
	BaseSHA string

	liftable map[Class]bool
	patterns []layerPattern
	bead     [][]string // every declared bead scope; a path must be inside each
	profiles []string   // the selected profiles, in menu order (SelectProfiles)
	tbl      *table     // the built-in table for that selection
}

// Profiles returns the profiles whose source, test and docs rows apply under
// this scope, in menu order: the project's selection out of the menu its
// operator layers allow (SelectProfiles). The sensitive rows of every shipped
// profile apply whatever this says.
func (e Effective) Profiles() []string { return append([]string{}, e.profiles...) }

// table is the built-in table to classify with. An Effective that Fold did not
// produce (the zero value) has the default selection.
func (e Effective) table() *table {
	if e.tbl == nil {
		return defaultTable
	}
	return e.tbl
}

// Fold combines the pack row with the operator and project layers (066 section
// 3.2): enablement AND-ed, classes intersected, caps minimised, protected
// patterns unioned, lifts allowed only where the row lists them AND the
// project opted in AND no operator layer vetoes; and the project's profile
// selection checked against the operator layers' allowed menu. Any layer that does not
// validate is an error -- our misconfiguration, never the agent's failure
// (invariant 5): the caller refuses the dispatch, or reports ArtifactUnknown.
func Fold(in Input) (Effective, error) {
	if !shaRe.MatchString(in.BaseSHA) {
		return Effective{}, fmt.Errorf("scope: base SHA %q is not a full commit SHA; scope policy is read at a commit the controller pinned, never at a branch name", in.BaseSHA)
	}
	if err := in.Row.validate(); err != nil {
		return Effective{}, err
	}
	layers := []struct {
		name  string
		layer Layer
		p     Policy
	}{{"instance", LayerOperator, in.Instance}, {"group", LayerOperator, in.Group}, {"project", LayerProject, in.Project}}
	for _, l := range layers {
		if err := l.p.Validate(l.layer); err != nil {
			return Effective{}, fmt.Errorf("%s layer: %w", l.name, err)
		}
	}
	for _, c := range in.LiftOptIn {
		if !Liftable(c) {
			return Effective{}, fmt.Errorf("scope: lift opt-in: %q is not liftable; only ci is, and control and secret are the floor", c)
		}
	}

	// Which profiles' permissive rows apply. A selection outside the operator's
	// menu is refused here as it is at registration -- ours to fix, never the
	// agent's failure.
	profiles, err := SelectProfiles(in.Project, in.Instance, in.Group)
	if err != nil {
		return Effective{}, fmt.Errorf("project layer: %w", err)
	}

	eff := Effective{Enabled: in.Enabled, Caps: in.Row.Caps, BaseSHA: in.BaseSHA, liftable: map[Class]bool{},
		profiles: profiles, tbl: buildTable(shipped, profiles)}

	allowed := map[Class]bool{}
	for _, c := range in.Row.Classes {
		allowed[c] = true
	}
	optIn := map[Class]bool{}
	for _, c := range in.LiftOptIn {
		optIn[c] = true
	}
	for _, c := range in.Row.Liftable {
		eff.liftable[c] = optIn[c]
	}
	for _, l := range layers {
		if l.p.Classes != nil {
			allowed = intersectSet(allowed, l.p.Classes)
		}
		for _, c := range []struct {
			dst *int64
			v   *int64
		}{{&eff.Caps.MaxFiles, l.p.Caps.MaxFiles}, {&eff.Caps.MaxLines, l.p.Caps.MaxLines}, {&eff.Caps.MaxDeletes, l.p.Caps.MaxDeletes},
			{&eff.Caps.MaxFileBytes, l.p.Caps.MaxFileBytes}, {&eff.Caps.MaxPatchBytes, l.p.Caps.MaxPatchBytes}} {
			if c.v != nil && *c.v < *c.dst {
				*c.dst = *c.v
			}
		}
		for _, c := range l.p.DenyLift {
			eff.liftable[c] = false
		}
		for _, k := range l.p.patternKeys() {
			for _, pat := range k.patterns {
				eff.patterns = append(eff.patterns, layerPattern{pattern: pat, layer: l.layer, name: l.name, key: k.key, to: k.to})
			}
		}
	}
	eff.Classes = orderedClasses(allowed)
	return eff, nil
}

func intersectSet(have map[Class]bool, with []Class) map[Class]bool {
	out := map[Class]bool{}
	for _, c := range with {
		if have[c] {
			out[c] = true
		}
	}
	return out
}

func orderedClasses(set map[Class]bool) []Class {
	out := []Class{}
	for _, c := range classOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

// Allows reports whether a path of class c may be written under this scope by
// a dispatch holding lift. A protected class is allowed only through a lift,
// and only one this scope could itself have granted.
func (e Effective) Allows(c Class, lift Lift) bool {
	if !e.Enabled || !c.Valid() {
		return false
	}
	if c.Sensitivity() == Protected {
		return Liftable(c) && lift.Has(c) && e.liftable[c]
	}
	for _, k := range e.Classes {
		if k == c {
			return true
		}
	}
	return false
}

// Liftable returns the classes GrantLift will grant, in class order.
func (e Effective) Liftable() []Class {
	set := map[Class]bool{}
	if e.Enabled {
		for c, ok := range e.liftable {
			if ok && Liftable(c) {
				set[c] = true
			}
		}
	}
	return orderedClasses(set)
}

// Narrow returns a copy whose allowed classes are intersected with the given
// ones. It is how the CONTROLLER selects a narrower envelope inside the fold
// (the pipeline-fix plan's per-diagnosis-class rows, 4.2); it cannot add a
// class, and it does not touch what may be lifted. Narrow() allows no ordinary
// class at all.
func (e Effective) Narrow(classes ...Class) Effective {
	have := map[Class]bool{}
	for _, c := range e.Classes {
		have[c] = true
	}
	e.Classes = orderedClasses(intersectSet(have, classes))
	return e
}

// WithBead returns a copy further narrowed to paths matching one of globs (the
// bead's declared scope, or a diagnosis's cited files). Repeated calls
// intersect: a path must be inside every declared scope. An empty list
// declares nothing.
func (e Effective) WithBead(globs []string) (Effective, error) {
	if len(globs) == 0 {
		return e, nil
	}
	for _, g := range globs {
		if err := ValidatePattern(g); err != nil {
			return Effective{}, fmt.Errorf("scope: bead scope: %w", err)
		}
	}
	bead := make([][]string, 0, len(e.bead)+1)
	bead = append(bead, e.bead...)
	e.bead = append(bead, append([]string(nil), globs...))
	return e, nil
}

// InBead reports whether p is inside every declared bead scope (true when none
// is declared).
func (e Effective) InBead(p string) bool {
	for _, globs := range e.bead {
		if matchAny(globs, p) == "" {
			return false
		}
	}
	return true
}

// Grant is a controller's request to lift protected classes for ONE dispatch.
// Trigger and Workflow say which dispatch; they are what the lift is logged
// under (066 section 9a: "each lift is named, per class, and logged with the
// session").
type Grant struct {
	Trigger  string  // e.g. "pipeline-fix"
	Workflow string  // e.g. the diagnosis class the envelope was chosen for
	Classes  []Class // the protected classes to lift
}

// Lift is a per-dispatch permission to write a protected class.
//
// IT COMES FROM THE CONTROLLER AND FROM NOWHERE ELSE. Its fields are
// unexported and its decoders refuse (UnmarshalJSON, UnmarshalText,
// UnmarshalYAML all return an error), so no batch, no file under `.agent/`
// and no issue text can be turned into one: the only way to hold a non-empty
// Lift is Effective.GrantLift, called by controller code with a Grant it
// built from the dispatch. The zero Lift lifts nothing, and is what every
// existing agent runs with.
type Lift struct {
	classes  string // class names in class order, comma-joined; "" lifts nothing
	trigger  string
	workflow string
}

// GrantLift mints the lift for one dispatch. It refuses -- returning the zero
// Lift -- unless every requested class is liftable (the floor never is),
// listed in the pack row, opted in to by the project and not vetoed by an
// operator layer; and unless the grant names its trigger and workflow.
func (e Effective) GrantLift(g Grant) (Lift, error) {
	if strings.TrimSpace(g.Trigger) == "" || strings.TrimSpace(g.Workflow) == "" {
		return Lift{}, fmt.Errorf("scope: a lift must name the trigger and workflow of the dispatch that carries it")
	}
	if len(g.Classes) == 0 {
		return Lift{}, fmt.Errorf("scope: a lift must name at least one class")
	}
	if !e.Enabled {
		return Lift{}, fmt.Errorf("scope: the workflow is disabled; nothing can be lifted")
	}
	set := map[Class]bool{}
	for _, c := range g.Classes {
		switch {
		case !c.Valid():
			return Lift{}, fmt.Errorf("scope: lift: unknown class %q", c)
		case !Liftable(c):
			return Lift{}, fmt.Errorf("scope: lift: %q cannot be lifted by any dispatch (only ci can; control and secret are the floor)", c)
		case !e.liftable[c]:
			return Lift{}, fmt.Errorf("scope: lift: %q is not liftable for this workflow in this project (needs the pack row, the project's opt-in, and no operator veto)", c)
		}
		set[c] = true
	}
	names := make([]string, 0, len(set))
	for _, c := range orderedClasses(set) {
		names = append(names, string(c))
	}
	return Lift{classes: strings.Join(names, ","), trigger: g.Trigger, workflow: g.Workflow}, nil
}

var errLiftDecode = errors.New("scope: a lift cannot be decoded from data; it is granted to a dispatch by the controller (Effective.GrantLift)")

// UnmarshalJSON, UnmarshalText and UnmarshalYAML refuse. A struct that embeds
// a Lift and is decoded from JSON, TOML or YAML therefore fails loudly instead
// of quietly carrying the zero lift past a reader who expected otherwise.
func (l *Lift) UnmarshalJSON([]byte) error { return errLiftDecode }

// UnmarshalText refuses; see UnmarshalJSON.
func (l *Lift) UnmarshalText([]byte) error { return errLiftDecode }

// UnmarshalYAML refuses; see UnmarshalJSON.
func (l *Lift) UnmarshalYAML(*yaml.Node) error { return errLiftDecode }

// Empty reports whether the lift lifts nothing.
func (l Lift) Empty() bool { return l.classes == "" }

// Has reports whether the lift covers c. Never true for a floor class.
func (l Lift) Has(c Class) bool {
	if !Liftable(c) {
		return false
	}
	for _, k := range strings.Split(l.classes, ",") {
		if k == string(c) {
			return true
		}
	}
	return false
}

// Classes returns the lifted classes, in class order.
func (l Lift) Classes() []Class {
	var out []Class
	for _, c := range classOrder {
		if l.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

// Trigger and Workflow name the dispatch the lift was granted to.
func (l Lift) Trigger() string  { return l.trigger }
func (l Lift) Workflow() string { return l.workflow }

// String is the form to log with the session: "none", or the classes and the
// dispatch that carries them.
func (l Lift) String() string {
	if l.Empty() {
		return "none"
	}
	return fmt.Sprintf("%s (trigger=%s workflow=%s)", l.classes, l.trigger, l.workflow)
}
