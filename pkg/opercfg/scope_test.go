package opercfg

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The operator layer's `scope:` (gonk-066 S3), on `instance` and on each
// group. Documented in docs/reference/gonk-scope.md.

func withScope(instanceScope, groupScope string) string {
	doc := validOperatorYAML
	if instanceScope != "" {
		doc = strings.Replace(doc, "instance:\n  enabled: true\n", "instance:\n  enabled: true\n  scope:\n"+instanceScope, 1)
	}
	if groupScope != "" {
		doc = strings.Replace(doc, "  agentic:\n", "  agentic:\n    scope:\n"+groupScope, 1)
	}
	return doc
}

func TestLoadDecodesOperatorScope(t *testing.T) {
	oc, err := Load([]byte(withScope(
		"    classes: [source, test, docs]\n    caps: { max_files: 2 }\n    source_paths: [\"**/*.py\"]\n    protected_paths: [\"release/**\"]\n    deny_lift: [ci]\n",
		"      caps: { max_lines: 10 }\n      ci_paths: [\"ci/**\"]\n      test_paths: [\"internal/testutil/**\"]\n      docs_paths: [\"notes/**\"]\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in := oc.Instance.Scope
	if len(in.Classes) != 3 || in.Caps.MaxFiles == nil || *in.Caps.MaxFiles != 2 || len(in.SourcePaths) != 1 ||
		in.SourcePaths[0] != "**/*.py" || len(in.ProtectedPaths) != 1 || len(in.DenyLift) != 1 || in.DenyLift[0] != scope.ClassCI {
		t.Errorf("instance scope = %+v", in)
	}
	g := oc.Groups["agentic"].Scope
	if g.Caps.MaxLines == nil || *g.Caps.MaxLines != 10 || len(g.CIPaths) != 1 || len(g.TestPaths) != 1 || len(g.DocsPaths) != 1 {
		t.Errorf("group scope = %+v", g)
	}
	// And it folds: the operator's values reach an Effective scope.
	eff, err := scope.Fold(scope.Input{
		Row: scope.Row{Classes: []scope.Class{scope.ClassSource, scope.ClassOther}, Liftable: []scope.Class{scope.ClassCI},
			Caps: scope.Caps{MaxFiles: 9, MaxLines: 99}},
		Instance: in, Group: g, Enabled: true, LiftOptIn: []scope.Class{scope.ClassCI},
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Classes) != 1 || eff.Classes[0] != scope.ClassSource || eff.Caps.MaxFiles != 2 || eff.Caps.MaxLines != 10 || len(eff.Liftable()) != 0 {
		t.Errorf("folded = classes %v caps %+v liftable %v", eff.Classes, eff.Caps, eff.Liftable())
	}
}

func TestLoadWithoutScopeIsTheZeroPolicy(t *testing.T) {
	oc, err := Load([]byte(validOperatorYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !oc.Instance.Scope.IsZero() || !oc.Groups["agentic"].Scope.IsZero() {
		t.Error("an operator config with no scope section has a non-zero scope policy")
	}
}

// The meter refuses to START on an operator scope it cannot make sense of, at
// every operator layer.
func TestLoadRejectsInvalidOperatorScope(t *testing.T) {
	cases := map[string]struct{ instance, group, want string }{
		"instance unknown key":    {"    banana: 1\n", "", "scope"},
		"instance lattice":        {"    docs_paths: [\"deploy/**\"]\n", "", "instance"},
		"group lattice":           {"", "      test_paths: [\"chart/**\"]\n", "group agentic"},
		"group unknown key":       {"", "      banana: 1\n", "scope"},
		"instance protected":      {"    classes: [ci]\n", "", "scope"},
		"instance deny non-lift":  {"    deny_lift: [source]\n", "", "scope"},
		"group negative cap":      {"", "      caps: { max_lines: -1 }\n", "scope"},
		"instance source of test": {"    source_paths: [\"**/*_test.go\"]\n", "", "lattice"},
		"group secret to ci":      {"", "      ci_paths: [\"**/*.key\"]\n", "lattice"},
		// profiles (gonk-wj14)
		"instance unknown profile":  {"    allowed_profiles: [go, rust]\n", "", "scope"},
		"group unknown profile":     {"", "      allowed_profiles: [cobol]\n", "scope"},
		"instance selects profiles": {"    profiles: [go]\n", "", "scope"},
		"group selects profiles":    {"", "      profiles: [python]\n", "scope"},
		"profile listed twice":      {"    allowed_profiles: [go, go]\n", "", "scope"},
		"allowed_profiles a string": {"    allowed_profiles: go\n", "", "scope"},
	}
	for name, c := range cases {
		_, err := Load([]byte(withScope(c.instance, c.group)))
		if err == nil {
			t.Errorf("%s: Load accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "operator config: ") {
			t.Errorf("%s: err = %v, want an operator config error mentioning %q", name, err, c.want)
		}
	}
}

// GroupFor is what scope.Input.Group is documented as being. Reading
// oc.Groups[...] directly (as TestLoadDecodesOperatorScope does) never touches
// the fold, which is how gonk-a82p hid: foldPolicy dropped Policy.Scope, so a
// group's deny_lift, protected_paths and caps validated and were never applied.
func TestGroupForCarriesTheGroupScope(t *testing.T) {
	oc, err := Load([]byte(withScope("", "      caps: { max_lines: 10, max_files: 4 }\n      protected_paths: [\"release/**\"]\n      deny_lift: [ci]\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := oc.GroupFor("agentic/repo").Scope
	if got.Caps.MaxLines == nil || *got.Caps.MaxLines != 10 || got.Caps.MaxFiles == nil || *got.Caps.MaxFiles != 4 {
		t.Errorf("caps not carried: %+v", got.Caps)
	}
	if len(got.ProtectedPaths) != 1 || got.ProtectedPaths[0] != "release/**" {
		t.Errorf("protected_paths not carried: %v", got.ProtectedPaths)
	}
	if len(got.DenyLift) != 1 || got.DenyLift[0] != scope.ClassCI {
		t.Errorf("deny_lift not carried: %v", got.DenyLift)
	}
	// End to end: it must reach an Effective scope through GroupFor.
	eff, err := scope.Fold(scope.Input{
		Row: scope.Row{Classes: []scope.Class{scope.ClassSource}, Liftable: []scope.Class{scope.ClassCI},
			Caps: scope.Caps{MaxFiles: 9, MaxLines: 99}},
		Group: got, Enabled: true, LiftOptIn: []scope.Class{scope.ClassCI},
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Caps.MaxLines != 10 || len(eff.Liftable()) != 0 {
		t.Errorf("group scope did not reach the effective scope: caps %+v liftable %v", eff.Caps, eff.Liftable())
	}
}

// Nested groups fold in the tightening direction only: caps minimised, classes
// intersected, patterns and deny_lift unioned -- whichever group says what.
func TestGroupForNestedScopeOnlyTightens(t *testing.T) {
	cases := map[string]struct{ parent, child string }{
		"child looser cap":  {"      caps: { max_lines: 10 }\n", "      caps: { max_lines: 500 }\n"},
		"child tighter cap": {"      caps: { max_lines: 500 }\n", "      caps: { max_lines: 10 }\n"},
	}
	for name, c := range cases {
		oc := nestedConfig(t, c.parent, c.child)
		got := oc.GroupFor("agentic/sub/repo").Scope
		if got.Caps.MaxLines == nil || *got.Caps.MaxLines != 10 {
			t.Errorf("%s: max_lines = %v, want 10 (the tighter of the two)", name, got.Caps.MaxLines)
		}
	}
	oc := nestedConfig(t,
		"      classes: [source, test, docs]\n      protected_paths: [\"release/**\"]\n      deny_lift: [ci]\n",
		"      classes: [source, other]\n      protected_paths: [\"infra/**\"]\n")
	got := oc.GroupFor("agentic/sub/repo").Scope
	if len(got.Classes) != 1 || got.Classes[0] != scope.ClassSource {
		t.Errorf("classes = %v, want the intersection [source]", got.Classes)
	}
	if len(got.ProtectedPaths) != 2 || got.ProtectedPaths[0] != "release/**" || got.ProtectedPaths[1] != "infra/**" {
		t.Errorf("protected_paths = %v, want both, parent first", got.ProtectedPaths)
	}
	if len(got.DenyLift) != 1 || got.DenyLift[0] != scope.ClassCI {
		t.Errorf("a child that says nothing about deny_lift dropped the parent's veto: %v", got.DenyLift)
	}
	// A group that sets classes under a silent parent keeps them; an empty
	// (allow nothing) list stays non-nil through the fold.
	oc = nestedConfig(t, "", "      classes: []\n")
	if c := oc.GroupFor("agentic/sub/repo").Scope.Classes; c == nil || len(c) != 0 {
		t.Errorf("classes: [] = %#v, want non-nil empty (allow no class)", c)
	}
	oc = nestedConfig(t, "      classes: [source]\n", "      classes: [test]\n")
	if c := oc.GroupFor("agentic/sub/repo").Scope.Classes; c == nil || len(c) != 0 {
		t.Errorf("disjoint classes = %#v, want non-nil empty", c)
	}
}

// nestedConfig is the valid operator config with groups "agentic" (scope
// `parent`) and "agentic/sub" (scope `child`), each a pre-indented scope body.
func nestedConfig(t *testing.T, parent, child string) *OperatorConfig {
	t.Helper()
	doc := withScope("", parent)
	body := ""
	if child != "" {
		body = "    scope:\n" + child
	}
	doc = strings.Replace(doc, "  agentic/experiments:\n", "  agentic/sub:\n"+body+"  agentic/experiments:\n", 1)
	oc, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return oc
}

// allowed_profiles (gonk-wj14): an operator layer restricts the menu of
// ecosystem profiles a project may select. Each layer can only shrink it.
func TestAllowedProfilesFoldAcrossOperatorLayers(t *testing.T) {
	oc, err := Load([]byte(withScope("    allowed_profiles: [go, python, docs-md]\n", "      allowed_profiles: [python, node, docs-md]\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := oc.Instance.Scope.AllowedProfiles; len(got) != 3 || got[0] != "go" {
		t.Errorf("instance allowed_profiles = %v", got)
	}
	group := oc.GroupFor("agentic/repo").Scope
	if got := group.AllowedProfiles; len(got) != 3 || got[1] != "node" {
		t.Errorf("GroupFor did not carry the group's allowed_profiles: %v", got)
	}
	sel := func(profiles ...string) ([]string, error) {
		return scope.SelectProfiles(scope.Policy{Profiles: profiles}, oc.Instance.Scope, group)
	}
	if got, err := sel("python", "docs-md"); err != nil || len(got) != 2 {
		t.Errorf("python + docs-md is in both menus, got %v, %v", got, err)
	}
	for _, p := range []string{"go", "node", "docs-rst"} {
		if _, err := sel(p); err == nil || !strings.Contains(err.Error(), "allowed_profiles") {
			t.Errorf("a project selecting %q, which one operator layer does not allow, was accepted (err %v)", p, err)
		}
	}
	// A project in no group is bound by the instance menu alone.
	if _, err := scope.SelectProfiles(scope.Policy{Profiles: []string{"go"}}, oc.Instance.Scope, oc.GroupFor("elsewhere/repo").Scope); err != nil {
		t.Errorf("go is on the instance menu: %v", err)
	}

	// Nested groups: the intersection, whichever group is the narrower.
	for name, c := range map[string]struct{ parent, child string }{
		"child narrower":  {"      allowed_profiles: [go, python, node]\n", "      allowed_profiles: [python]\n"},
		"parent narrower": {"      allowed_profiles: [python]\n", "      allowed_profiles: [go, python, node]\n"},
		"overlap":         {"      allowed_profiles: [go, python]\n", "      allowed_profiles: [python, node]\n"},
	} {
		got := nestedConfig(t, c.parent, c.child).GroupFor("agentic/sub/repo").Scope.AllowedProfiles
		if len(got) != 1 || got[0] != "python" {
			t.Errorf("%s: nested allowed_profiles = %v, want [python]", name, got)
		}
	}
	// A nested group cannot re-open what its parent closed, and an empty
	// menu stays an empty menu (non-nil) through the fold.
	if got := nestedConfig(t, "      allowed_profiles: [go]\n", "      allowed_profiles: [node]\n").GroupFor("agentic/sub/repo").Scope.AllowedProfiles; got == nil || len(got) != 0 {
		t.Errorf("disjoint menus = %#v, want non-nil empty", got)
	}
	if got := nestedConfig(t, "      allowed_profiles: []\n", "      caps: { max_lines: 5 }\n").GroupFor("agentic/sub/repo").Scope.AllowedProfiles; got == nil || len(got) != 0 {
		t.Errorf("allowed_profiles: [] under a child that says nothing about profiles = %#v, want non-nil empty", got)
	}
	if got := nestedConfig(t, "      caps: { max_lines: 5 }\n", "      caps: { max_lines: 5 }\n").GroupFor("agentic/sub/repo").Scope.AllowedProfiles; got != nil {
		t.Errorf("no layer set allowed_profiles, got %#v", got)
	}
}

// The schema's menu is the code's menu: a profile added to pkg/scope and not
// to the operator schema (or the reverse) fails here.
func TestOperatorSchemaListsTheShippedProfiles(t *testing.T) {
	var doc struct {
		Defs struct {
			Policy struct {
				Properties struct {
					Scope struct {
						Properties map[string]struct {
							Items struct {
								Enum []string `json:"enum"`
							} `json:"items"`
						} `json:"properties"`
					} `json:"scope"`
				} `json:"properties"`
			} `json:"policy"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc.Defs.Policy.Properties.Scope.Properties
	if got, want := props["allowed_profiles"].Items.Enum, scope.ProfileNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("operator schema scope.allowed_profiles enum = %v, pkg/scope ships %v", got, want)
	}
	if _, ok := props["profiles"]; ok {
		t.Error("the operator schema offers scope.profiles; it is project-only")
	}
}
