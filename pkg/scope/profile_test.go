package scope

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Ecosystem profiles: gonk-wj14, design 066 section 11.

func foldWithProfiles(t *testing.T, profiles []string, operator ...Policy) Effective {
	t.Helper()
	in := Input{Row: Row{Classes: []Class{ClassSource, ClassTest, ClassDocs}}, Project: Policy{Profiles: profiles}, Enabled: true}
	if len(operator) > 0 {
		in.Instance = operator[0]
	}
	if len(operator) > 1 {
		in.Group = operator[1]
	}
	return mustFold(t, in)
}

func classUnder(t *testing.T, profiles []string, p string) Decision {
	t.Helper()
	return quietClassifier(foldWithProfiles(t, profiles), BaseFacts{}).Classify(p)
}

func TestShippedProfilesAreValid(t *testing.T) {
	if err := validateProfiles(Profiles()); err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "python", "node", "docs-md", "docs-rst", "docs-txt", "docs-adoc"}
	if got := ProfileNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("menu = %v, want %v", got, want)
	}
	if got := DefaultProfiles(); !reflect.DeepEqual(got, []string{"go", "docs-md"}) {
		t.Fatalf("default selection = %v, want [go docs-md]", got)
	}
	// Every ecosystem has an opinion on source, test, test-config and deps;
	// every documentation style on docs.
	for _, pr := range Profiles() {
		has := map[Class]bool{}
		for _, row := range pr.Rows {
			has[row.Class] = true
		}
		need := []Class{ClassDocs}
		if pr.Kind == ProfileEcosystem {
			need = []Class{ClassSource, ClassTest, ClassTestConfig, ClassDeps}
		}
		for _, c := range need {
			if !has[c] {
				t.Errorf("profile %s has no %s row", pr.Name, c)
			}
		}
	}
	// The accessors hand out copies: a caller cannot edit the menu.
	ps := Profiles()
	ps[0].Rows[0].Patterns[0] = "**"
	ps[0].Markers[0] = "x"
	if again := Profiles(); again[0].Rows[0].Patterns[0] == "**" || again[0].Markers[0] == "x" {
		t.Fatal("Profiles() returned the menu itself, not a copy")
	}
	lp, _ := LookupProfile("python")
	lp.Rows[0].Patterns[0] = "**"
	if again, _ := LookupProfile("python"); again.Rows[0].Patterns[0] == "**" {
		t.Fatal("LookupProfile returned the menu itself, not a copy")
	}
	d := DefaultProfiles()
	d[0] = "python"
	if DefaultProfiles()[0] != "go" {
		t.Fatal("DefaultProfiles returned the default itself, not a copy")
	}
}

// The menu is data, and a menu that says something a profile may not say is
// refused -- at package initialisation for the shipped one.
func TestValidateProfilesRefuses(t *testing.T) {
	good := func() Profile {
		return Profile{Name: "x", Kind: ProfileEcosystem, Weakening: WeakeningNone, Markers: []string{"x.lock"},
			Rows: []ProfileRow{{ClassSource, []string{"**/*.x"}}}}
	}
	if err := validateProfiles([]Profile{good()}); err != nil {
		t.Fatalf("a valid profile was refused: %v", err)
	}
	cases := map[string]func(*Profile){
		"a ci row (the one liftable class)": func(p *Profile) { p.Rows = []ProfileRow{{ClassCI, []string{"ci/**"}}} },
		"a control row":                     func(p *Profile) { p.Rows = []ProfileRow{{ClassControl, []string{"x"}}} },
		"a secret row":                      func(p *Profile) { p.Rows = []ProfileRow{{ClassSecret, []string{"x"}}} },
		"an agent-context row":              func(p *Profile) { p.Rows = []ProfileRow{{ClassAgentContext, []string{"x"}}} },
		"an other row":                      func(p *Profile) { p.Rows = []ProfileRow{{ClassOther, []string{"x"}}} },
		"an unknown class":                  func(p *Profile) { p.Rows = []ProfileRow{{"banana", []string{"x"}}} },
		"no rows":                           func(p *Profile) { p.Rows = nil },
		"a row with no patterns":            func(p *Profile) { p.Rows = []ProfileRow{{ClassSource, nil}} },
		"a class listed twice":              func(p *Profile) { p.Rows = append(p.Rows, ProfileRow{ClassSource, []string{"y"}}) },
		"a malformed pattern":               func(p *Profile) { p.Rows = []ProfileRow{{ClassSource, []string{"a[b]"}}} },
		"an ecosystem silent on weakening":  func(p *Profile) { p.Weakening = "" },
		"an unknown weakening value":        func(p *Profile) { p.Weakening = "probably" },
		"an unknown kind":                   func(p *Profile) { p.Kind = "build" },
		"a bad name":                        func(p *Profile) { p.Name = "Py thon" },
		"a marker in a subdirectory":        func(p *Profile) { p.Markers = []string{"sub/x.lock"} },
		"a marker that is a pattern":        func(p *Profile) { p.Markers = []string{"x*.lock"} },
		"a docs profile with source paths":  func(p *Profile) { p.Kind, p.Weakening, p.Markers = ProfileDocs, "", nil },
		"a docs profile claiming a predicate": func(p *Profile) {
			p.Kind, p.Markers, p.Weakening, p.Rows = ProfileDocs, nil, WeakeningDeterministic, []ProfileRow{{ClassDocs, []string{"x"}}}
		},
		"a docs profile with markers": func(p *Profile) {
			p.Kind, p.Weakening, p.Rows = ProfileDocs, "", []ProfileRow{{ClassDocs, []string{"x"}}}
		},
	}
	for name, mutate := range cases {
		p := good()
		mutate(&p)
		if err := validateProfiles([]Profile{p}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateProfiles([]Profile{good(), good()}); err == nil {
		t.Error("a name listed twice: accepted")
	}
}

// The rules of 066 section 11, one row each. `sel` nil is a project with no
// scope.profiles key.
func TestProfileRules(t *testing.T) {
	goOnly, py, node := []string{"go"}, []string{"python"}, []string{"node"}
	cases := []struct {
		name string
		sel  []string
		path string
		want Class
		prof string
	}{
		// A permissive row applies only when its profile is selected.
		{"a .py file is other without the python profile", nil, "svc/app.py", ClassOther, ""},
		{"... and with only go selected", goOnly, "svc/app.py", ClassOther, ""},
		{"... and source with it", py, "svc/app.py", ClassSource, "python"},
		{"a python test is other without python", nil, "svc/test_app.py", ClassOther, ""},
		{"... and test with it", py, "svc/test_app.py", ClassTest, "python"},
		{"tests/ is test with python", py, "tests/data/in.json", ClassTest, "python"},
		{"a nested tests/ too", py, "svc/tests/helpers.py", ClassTest, "python"},
		{"a .ts file is other without node", goOnly, "web/index.ts", ClassOther, ""},
		{"... and source with it", node, "web/index.ts", ClassSource, "node"},
		{"every node source extension: js", node, "a.js", ClassSource, "node"},
		{"jsx", node, "a/b.jsx", ClassSource, "node"},
		{"tsx", node, "a/b.tsx", ClassSource, "node"},
		{"mjs", node, "a/b.mjs", ClassSource, "node"},
		{"cjs", node, "a/b.cjs", ClassSource, "node"},
		{"a node test", node, "web/index.test.ts", ClassTest, "node"},
		{"a node spec", node, "web/index.spec.jsx", ClassTest, "node"},
		{"__tests__", node, "web/__tests__/fixtures/a.json", ClassTest, "node"},
		{"a file that merely has .spec. in its name is not a test", node, "api/openapi.spec.yaml", ClassOther, ""},
		{"a Go file is other when go is not selected", py, "main.go", ClassOther, ""},
		{"a Go test too", py, "pkg/x/x_test.go", ClassOther, ""},
		{"testdata too", py, "pkg/x/testdata/in.json", ClassOther, ""},
		{"markdown is other when docs-md is not selected", py, "README.md", ClassOther, ""},
		{"docs/ is other when docs-md is not selected", []string{"python", "docs-rst"}, "docs/index.html", ClassOther, ""},
		{"rst is other by default", nil, "guide/index.rst", ClassOther, ""},
		{"rst is docs with docs-rst", []string{"docs-rst"}, "guide/index.rst", ClassDocs, "docs-rst"},
		{"txt is other by default", nil, "notes/plan.txt", ClassOther, ""},
		{"txt is docs with docs-txt", []string{"docs-txt"}, "notes/plan.txt", ClassDocs, "docs-txt"},
		{"adoc is docs with docs-adoc", []string{"docs-adoc"}, "guide/index.adoc", ClassDocs, "docs-adoc"},
		{"asciidoc too", []string{"docs-adoc"}, "guide/index.asciidoc", ClassDocs, "docs-adoc"},
		{"the changelog follows any docs style", []string{"docs-rst"}, "CHANGELOG.rst", ClassDocs, "docs-rst"},
		{"an empty selection selects nothing", []string{}, "main.go", ClassOther, ""},
		{"... not even docs", []string{}, "README.md", ClassOther, ""},

		// A sensitive row of EVERY shipped profile applies, selected or not.
		{"package.json is deps on a go-only project", goOnly, "package.json", ClassDeps, "node"},
		{"nested too", goOnly, "web/app/package.json", ClassDeps, "node"},
		{"conftest.py is test-config on a go-only project", goOnly, "conftest.py", ClassTestConfig, "python"},
		{"nested too", goOnly, "svc/tests/conftest.py", ClassTestConfig, "python"},
		{"requirements.txt is deps by default", nil, "requirements.txt", ClassDeps, "python"},
		{"requirements-dev.txt", nil, "requirements-dev.txt", ClassDeps, "python"},
		{"pyproject.toml", nil, "pyproject.toml", ClassDeps, "python"},
		{"Pipfile.lock", nil, "Pipfile.lock", ClassDeps, "python"},
		{"poetry.lock", nil, "svc/poetry.lock", ClassDeps, "python"},
		{"uv.lock", nil, "uv.lock", ClassDeps, "python"},
		{"pytest.ini", nil, "pytest.ini", ClassTestConfig, "python"},
		{"tox.ini", nil, "tox.ini", ClassTestConfig, "python"},
		{"noxfile.py", nil, "noxfile.py", ClassTestConfig, "python"},
		{"setup.cfg", nil, "setup.cfg", ClassTestConfig, "python"},
		{".coveragerc", nil, ".coveragerc", ClassTestConfig, "python"},
		{"jest config", nil, "jest.config.js", ClassTestConfig, "node"},
		{"vitest config", nil, "web/vitest.config.ts", ClassTestConfig, "node"},
		{"playwright config", nil, "playwright.config.ts", ClassTestConfig, "node"},
		{"tsconfig", nil, "tsconfig.json", ClassTestConfig, "node"},
		{"tsconfig variants", nil, "web/tsconfig.build.json", ClassTestConfig, "node"},
		{"eslintrc", nil, ".eslintrc.json", ClassTestConfig, "node"},
		{"eslint flat config", nil, "eslint.config.mjs", ClassTestConfig, "node"},
		{"package-lock.json", nil, "package-lock.json", ClassDeps, "node"},
		{"yarn.lock", nil, "yarn.lock", ClassDeps, "node"},
		{"pnpm-lock.yaml", nil, "pnpm-lock.yaml", ClassDeps, "node"},
		{"node_modules", nil, "node_modules/left-pad/index.js", ClassDeps, "node"},
		{"a nested node_modules", nil, "web/node_modules/left-pad/index.js", ClassDeps, "node"},
		{"generated protobuf python", nil, "api/v1/api_pb2.py", ClassGenerated, "python"},
		{"a minified bundle", nil, "web/dist/app.min.js", ClassGenerated, "node"},
		{"CMakeLists.txt is build configuration, not prose", []string{"docs-txt"}, "CMakeLists.txt", ClassTestConfig, "docs-txt"},
		{"Sphinx conf.py is executed by the docs build", []string{"python", "docs-md", "docs-rst"}, "docs/conf.py", ClassTestConfig, "docs-rst"},
		{"... in a nested source dir too", nil, "docs/source/conf.py", ClassTestConfig, "docs-rst"},
		{"the go profile's own sensitive rows hold when go is not selected", py, "go.mod", ClassDeps, "go"},
		{"Makefile", py, "Makefile", ClassTestConfig, "go"},
		{"vendor", node, "vendor/x/y.go", ClassDeps, "go"},

		// A sensitive match beats a selected permissive match.
		{"setup.py with python selected is deps, not source", py, "setup.py", ClassDeps, "python"},
		{"conftest.py under tests/ is test-config, not test", py, "tests/conftest.py", ClassTestConfig, "python"},
		{"noxfile.py is test-config, not source", py, "noxfile.py", ClassTestConfig, "python"},
		{"requirements.txt with docs-txt selected is deps, not docs", []string{"docs-txt"}, "requirements.txt", ClassDeps, "python"},
		{"docs/requirements.txt with docs-md is deps, not docs", nil, "docs/requirements.txt", ClassDeps, "python"},
		{"jest.config.js with node selected is test-config, not source", node, "jest.config.js", ClassTestConfig, "node"},
		{"eslint.config.mjs with node selected", node, "eslint.config.mjs", ClassTestConfig, "node"},
		{"a node_modules test file is deps, not test", node, "node_modules/x/a.test.js", ClassDeps, "node"},
		{"a generated python module is generated, not source", py, "api/api_pb2.py", ClassGenerated, "python"},
		{"a minified file is generated, not source", node, "web/app.min.js", ClassGenerated, "node"},
		{"package.json under __tests__ is deps, not test", node, "web/__tests__/package.json", ClassDeps, "node"},

		// Profiles move nothing out of the protected classes or deploy.
		{".gitlab/package.json stays ci", node, ".gitlab/package.json", ClassCI, ""},
		{".gitlab-ci.yml stays ci", []string{"docs-txt"}, ".gitlab-ci.yml", ClassCI, ""},
		{".github/scripts/build.py stays ci", py, ".github/scripts/build.py", ClassCI, ""},
		{"a key under tests/ stays secret", py, "tests/fixtures/tls.key", ClassSecret, ""},
		{"secrets/ stays secret", node, "secrets/index.js", ClassSecret, ""},
		{"CODEOWNERS under docs stays control", nil, "docs/CODEOWNERS", ClassControl, ""},
		{".gonk.yml stays control", py, "svc/.gonk.yml", ClassControl, ""},
		{"deploy/ stays deploy", py, "deploy/render.py", ClassDeploy, ""},
		{"chart/ stays deploy", node, "chart/package.json", ClassDeploy, ""},
		{".agent/ stays agent-context", py, ".agent/notes.py", ClassAgentContext, ""},
		{"an unclean path stays control", py, "../app.py", ClassControl, ""},
	}
	for _, c := range cases {
		d := classUnder(t, c.sel, c.path)
		if d.Class != c.want || d.Profile != c.prof {
			t.Errorf("%s: %q under %v = %s (profile %q), want %s (profile %q)", c.name, c.path, c.sel, d.Class, d.Profile, c.want, c.prof)
		}
	}
}

// Builtin is the default selection, and an Effective that Fold did not make
// classifies the same way.
func TestBuiltinIsTheDefaultSelection(t *testing.T) {
	def := quietClassifier(foldWithProfiles(t, nil), BaseFacts{})
	explicit := quietClassifier(foldWithProfiles(t, []string{"docs-md", "go"}), BaseFacts{})
	zero := quietClassifier(Effective{}, BaseFacts{})
	for _, p := range goldenPaths(t) {
		want := Builtin(p)
		if got := def.Classify(p).Class; got != want {
			t.Fatalf("%q: no profiles key classifies as %s, Builtin as %s", p, got, want)
		}
		if got := explicit.Classify(p).Class; got != want {
			t.Fatalf("%q: profiles [docs-md, go] classifies as %s, Builtin as %s", p, got, want)
		}
		if got := zero.Classify(p).Class; got != want {
			t.Fatalf("%q: the zero Effective classifies as %s, Builtin as %s", p, got, want)
		}
	}
	if got := foldWithProfiles(t, nil).Profiles(); !reflect.DeepEqual(got, []string{"go", "docs-md"}) {
		t.Errorf("Profiles() with no key = %v", got)
	}
	// Menu order, whatever order the project wrote.
	if got := foldWithProfiles(t, []string{"docs-rst", "node", "python"}).Profiles(); !reflect.DeepEqual(got, []string{"python", "node", "docs-rst"}) {
		t.Errorf("Profiles() = %v, want menu order", got)
	}
	if got := foldWithProfiles(t, []string{}).Profiles(); len(got) != 0 {
		t.Errorf("Profiles() of an empty selection = %v", got)
	}
}

func TestUnknownProfileIsRefused(t *testing.T) {
	for _, names := range [][]string{{"rust"}, {"go", "Python"}, {""}, {"go", "go"}, {"docs"}} {
		p := Policy{Profiles: names}
		err := p.Validate(LayerProject)
		if err == nil || !strings.Contains(err.Error(), "profile") {
			t.Errorf("Validate(project) accepted profiles %q (err %v)", names, err)
		}
		if _, err := Fold(Input{Project: p, BaseSHA: testSHA}); err == nil {
			t.Errorf("Fold accepted profiles %q", names)
		}
		if _, err := SelectProfiles(p); err == nil {
			t.Errorf("SelectProfiles accepted profiles %q", names)
		}
		op := Policy{AllowedProfiles: names}
		if err := op.Validate(LayerOperator); err == nil {
			t.Errorf("Validate(operator) accepted allowed_profiles %q", names)
		}
		if _, err := SelectProfiles(Policy{}, op); err == nil {
			t.Errorf("SelectProfiles accepted allowed_profiles %q", names)
		}
	}
	for _, names := range [][]string{{}, {"go"}, ProfileNames()} {
		if err := (Policy{Profiles: names}).Validate(LayerProject); err != nil {
			t.Errorf("profiles %q refused: %v", names, err)
		}
		if err := (Policy{AllowedProfiles: names}).Validate(LayerOperator); err != nil {
			t.Errorf("allowed_profiles %q refused: %v", names, err)
		}
	}
}

// Who may write which key: a project selects, an operator restricts the menu.
func TestProfileKeysAreLayerBound(t *testing.T) {
	if err := (Policy{Profiles: []string{"go"}}).Validate(LayerOperator); err == nil || !strings.Contains(err.Error(), "project-only") {
		t.Errorf("an operator layer with `profiles` was accepted (err %v)", err)
	}
	if err := (Policy{Profiles: []string{}}).Validate(LayerOperator); err == nil {
		t.Error("an operator layer with an empty `profiles` was accepted")
	}
	if err := (Policy{AllowedProfiles: []string{"go"}}).Validate(LayerProject); err == nil || !strings.Contains(err.Error(), "operator-only") {
		t.Errorf("a project with `allowed_profiles` was accepted (err %v)", err)
	}
	if err := (Policy{AllowedProfiles: []string{}}).Validate(LayerProject); err == nil {
		t.Error("a project with an empty `allowed_profiles` was accepted")
	}
	// Free-form source_paths stays operator-only, with a profile selected too.
	if err := (Policy{Profiles: []string{"python"}, SourcePaths: []string{"**/*.rb"}}).Validate(LayerProject); err == nil {
		t.Error("a project that selected a profile was allowed source_paths")
	}
	if (Policy{Profiles: []string{}}).IsZero() || (Policy{AllowedProfiles: []string{}}).IsZero() {
		t.Error("a layer that sets profiles or allowed_profiles to an empty list is not silent")
	}
}

func TestAllowedProfilesRestrictTheMenu(t *testing.T) {
	allow := func(names ...string) Policy {
		if names == nil {
			names = []string{}
		}
		return Policy{AllowedProfiles: names}
	}
	cases := []struct {
		name     string
		project  []string // nil: no key
		operator []Policy
		want     []string // nil: refused
	}{
		{"no restriction, no key: the default", nil, nil, []string{"go", "docs-md"}},
		{"no restriction: what the project selected", []string{"python", "docs-rst"}, nil, []string{"python", "docs-rst"}},
		{"a selection inside the menu", []string{"python"}, []Policy{allow("python", "docs-md")}, []string{"python"}},
		{"a selection outside the menu is refused", []string{"python"}, []Policy{allow("go", "docs-md")}, nil},
		{"one disallowed name among allowed ones is refused", []string{"go", "node"}, []Policy{allow("go", "docs-md")}, nil},
		{"the menu is the INTERSECTION of the operator layers", []string{"python"}, []Policy{allow("go", "python"), allow("python", "node")}, []string{"python"}},
		{"... allowed by the instance but not the group: refused", []string{"go"}, []Policy{allow("go", "python"), allow("python", "node")}, nil},
		{"... allowed by the group but not the instance: refused", []string{"node"}, []Policy{allow("go", "python"), allow("python", "node")}, nil},
		{"a silent layer does not restrict", []string{"node"}, []Policy{{}, allow("node")}, []string{"node"}},
		{"an empty menu allows no profile", []string{"go"}, []Policy{allow()}, nil},
		{"an empty selection is always inside the menu", []string{}, []Policy{allow()}, []string{}},
		{"no key under a restricted menu: the default, intersected", nil, []Policy{allow("go", "python")}, []string{"go"}},
		{"no key under a menu without the defaults: nothing", nil, []Policy{allow("python")}, []string{}},
		{"no key under an empty menu: nothing", nil, []Policy{allow()}, []string{}},
	}
	for _, c := range cases {
		got, err := SelectProfiles(Policy{Profiles: c.project}, c.operator...)
		in := Input{Row: Row{Classes: []Class{ClassSource}}, Project: Policy{Profiles: c.project}, Enabled: true, BaseSHA: testSHA}
		if len(c.operator) > 0 {
			in.Instance = c.operator[0]
		}
		if len(c.operator) > 1 {
			in.Group = c.operator[1]
		}
		eff, ferr := Fold(in)
		if c.want == nil {
			if err == nil || !strings.Contains(err.Error(), "allowed_profiles") {
				t.Errorf("%s: SelectProfiles = %v, %v; want a refusal naming allowed_profiles", c.name, got, err)
			}
			if ferr == nil || !strings.Contains(ferr.Error(), "project layer") {
				t.Errorf("%s: Fold = %v; want it refused as the project layer's error", c.name, ferr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: SelectProfiles = %v, %v; want %v", c.name, got, err, c.want)
		}
		if ferr != nil || !reflect.DeepEqual(eff.Profiles(), c.want) {
			t.Errorf("%s: Fold profiles = %v, %v; want %v", c.name, eff.Profiles(), ferr, c.want)
		}
	}

	// And the restriction is what classifies: under a menu without `go`, a
	// project with no key no longer has Go source.
	eff := mustFold(t, Input{Row: Row{Classes: []Class{ClassSource}}, Group: allow("python", "docs-md"), Enabled: true})
	if d := quietClassifier(eff, BaseFacts{}).Classify("main.go"); d.Class != ClassOther {
		t.Errorf("main.go under a menu without go = %s, want other", d.Class)
	}
	if d := quietClassifier(eff, BaseFacts{}).Classify("go.mod"); d.Class != ClassDeps {
		t.Errorf("go.mod under a menu without go = %s; the sensitive rows are not on the menu to remove", d.Class)
	}
}

func TestFoldPoliciesIntersectsAllowedProfiles(t *testing.T) {
	a, b := Policy{AllowedProfiles: []string{"go", "python", "docs-md"}}, Policy{AllowedProfiles: []string{"node", "python", "go"}}
	if got := FoldPolicies(a, b).AllowedProfiles; !reflect.DeepEqual(got, []string{"go", "python"}) {
		t.Errorf("FoldPolicies allowed_profiles = %v, want the intersection [go python]", got)
	}
	if got := FoldPolicies(a, Policy{}).AllowedProfiles; !reflect.DeepEqual(got, a.AllowedProfiles) {
		t.Errorf("a silent nested layer changed the menu: %v", got)
	}
	if got := FoldPolicies(Policy{}, b).AllowedProfiles; !reflect.DeepEqual(got, b.AllowedProfiles) {
		t.Errorf("a silent parent layer changed the menu: %v", got)
	}
	if got := FoldPolicies(Policy{AllowedProfiles: []string{"go"}}, Policy{AllowedProfiles: []string{"node"}}).AllowedProfiles; got == nil || len(got) != 0 {
		t.Errorf("disjoint menus folded to %#v; want a non-nil empty menu (allow none), not nil (silent)", got)
	}
	if got := FoldPolicies(Policy{}, Policy{}).AllowedProfiles; got != nil {
		t.Errorf("two silent layers folded to %#v", got)
	}
	// Folding two groups is the same as handing Fold the two layers.
	for _, sel := range [][]string{nil, {"go"}, {"python"}, {"node"}, {}} {
		sep, e1 := SelectProfiles(Policy{Profiles: sel}, a, b)
		merged, e2 := SelectProfiles(Policy{Profiles: sel}, FoldPolicies(a, b))
		if (e1 == nil) != (e2 == nil) || !reflect.DeepEqual(sep, merged) {
			t.Errorf("selection %v: separately %v/%v, merged %v/%v", sel, sep, e1, merged, e2)
		}
	}
	// `profiles` is project-only and is not carried by a fold of operator layers.
	if got := FoldPolicies(Policy{Profiles: []string{"go"}}, Policy{Profiles: []string{"python"}}).Profiles; got != nil {
		t.Errorf("FoldPolicies carried profiles %v", got)
	}
}

// subsetsOfMenu returns every selection: all 2^n subsets of the menu.
func subsetsOfMenu() [][]string {
	names := ProfileNames()
	var out [][]string
	for mask := 0; mask < 1<<len(names); mask++ {
		sel := []string{}
		for i, n := range names {
			if mask&(1<<i) != 0 {
				sel = append(sel, n)
			}
		}
		out = append(out, sel)
	}
	return out
}

// TestSelectingProfilesOnlyMovesOther is the bound on what a selection can do,
// over EVERY selection (all 128) and every path of the golden list:
//
//   - a path that is control, secret or ci with no profile shipped at all has
//     that class under every selection: profiles move nothing out of the
//     protected classes;
//   - a path that some sensitive row names (its class with NOTHING selected is
//     not `other`) has that same class under every selection: selecting can
//     not turn a sensitive row off;
//   - every other path is `other` with nothing selected, and a selection can
//     make it test, docs or source -- and nothing else.
func TestSelectingProfilesOnlyMovesOther(t *testing.T) {
	paths := goldenPaths(t)
	base := BaseFacts{Generated: map[string]bool{"pkg/x/main.go": true, "src/index.ts": true}, CILocalIncludes: []string{"scripts/script.sh", "src/app.py"}}
	noProfiles := buildTable(nil, nil)  // the core rows alone
	nothing := buildTable(shipped, nil) // the whole menu shipped, nothing selected
	floors, always := make([]Class, len(paths)), make([]Class, len(paths))
	for i, p := range paths {
		floors[i], _ = noProfiles.classify(p, base)
		always[i], _ = nothing.classify(p, base)
	}
	protected, sensitive, moved := 0, 0, 0
	for _, sel := range subsetsOfMenu() {
		tbl := buildTable(shipped, sel)
		for i, p := range paths {
			got, _ := tbl.classify(p, base)
			if floors[i].Sensitivity() == Protected {
				protected++
				if got != floors[i] {
					t.Fatalf("selection %v moved %q out of %s into %s", sel, p, floors[i], got)
				}
				continue
			}
			if always[i] != ClassOther {
				sensitive++
				if got != always[i] {
					t.Fatalf("selection %v: %q is %s; a sensitive row makes it %s whatever is selected", sel, p, got, always[i])
				}
				continue
			}
			switch got {
			case ClassOther:
			case ClassTest, ClassDocs, ClassSource:
				moved++
			default:
				t.Fatalf("selection %v: %q is %s; a selection may only turn `other` into test, docs or source", sel, p, got)
			}
		}
	}
	if protected < 10000 || sensitive < 10000 || moved < 10000 {
		t.Fatalf("checked %d protected, %d sensitive and %d moved (selection, path) pairs; too few to mean anything", protected, sensitive, moved)
	}
}

// The testing methodology is a recorded property of each ecosystem profile
// (066 section 11 item 6). No predicate is built here; later parts read this
// to keep a fix they cannot check for test weakening out of auto-merge.
func TestWeakeningPredicateIsRecordedPerProfile(t *testing.T) {
	want := map[string]bool{"go": true, "python": false, "node": false, "docs-md": false, "docs-rst": false, "docs-txt": false, "docs-adoc": false}
	for _, pr := range Profiles() {
		w, ok := want[pr.Name]
		if !ok {
			t.Errorf("profile %s has no expectation here; say whether a weakening predicate exists for it", pr.Name)
			continue
		}
		if got := WeakeningPredicate(pr.Name); got != w {
			t.Errorf("WeakeningPredicate(%q) = %v, want %v", pr.Name, got, w)
		}
		if pr.Kind == ProfileEcosystem && pr.Weakening == "" {
			t.Errorf("ecosystem profile %s is silent on test weakening", pr.Name)
		}
	}
	for _, n := range []string{"", "rust", "GO"} {
		if WeakeningPredicate(n) {
			t.Errorf("WeakeningPredicate(%q) = true for a profile that is not shipped", n)
		}
	}

	eff := mustFold(t, Input{
		Row:      Row{Classes: []Class{ClassSource, ClassTest, ClassDocs}},
		Instance: Policy{SourcePaths: []string{"**/*.rb"}},
		Project:  Policy{Profiles: []string{"go", "python", "node", "docs-md"}, TestPaths: []string{"internal/testutil/**", "spec/**"}},
		Enabled:  true,
	})
	c := quietClassifier(eff, BaseFacts{})
	cases := []struct {
		path  string
		class Class
		want  bool
	}{
		{"pkg/x/x.go", ClassSource, true},
		{"pkg/x/x_test.go", ClassTest, true},
		{"internal/testutil/fake.go", ClassTest, true}, // a Go file moved into test is still Go
		{"svc/app.py", ClassSource, false},
		{"svc/test_app.py", ClassTest, false},
		{"web/index.ts", ClassSource, false},
		{"web/index.test.ts", ClassTest, false},
		{"lib/thing.rb", ClassSource, false},     // source by source_paths: no profile vouches for it
		{"spec/thing_spec.rb", ClassTest, false}, // test by test_paths alone
		{"README.md", ClassDocs, true},           // nothing to weaken
		{"package.json", ClassDeps, true},
		{"LICENSE", ClassOther, true},
	}
	for _, tc := range cases {
		d := c.Classify(tc.path)
		if d.Class != tc.class || d.WeakeningDecidable() != tc.want {
			t.Errorf("%q: class %s (profile %q) decidable %v; want class %s decidable %v", tc.path, d.Class, d.Profile, d.WeakeningDecidable(), tc.class, tc.want)
		}
	}
}

// Onboarding proposes a selection from marker files (066 section 11 item 5).
func TestProposeProfiles(t *testing.T) {
	cases := []struct {
		found []string
		want  []string
		why   []ProfileEvidence
	}{
		{nil, []string{"go", "docs-md"}, nil},
		{[]string{"README.md", "LICENSE"}, []string{"go", "docs-md"}, nil},
		{[]string{"go.mod"}, []string{"go", "docs-md"}, []ProfileEvidence{{"go", "go.mod"}}},
		{[]string{"pyproject.toml"}, []string{"python", "docs-md"}, []ProfileEvidence{{"python", "pyproject.toml"}}},
		{[]string{"setup.py"}, []string{"python", "docs-md"}, []ProfileEvidence{{"python", "setup.py"}}},
		{[]string{"requirements.txt"}, []string{"python", "docs-md"}, []ProfileEvidence{{"python", "requirements.txt"}}},
		// One reason per profile: the first marker in the profile's own order.
		{[]string{"requirements.txt", "setup.py", "pyproject.toml"}, []string{"python", "docs-md"}, []ProfileEvidence{{"python", "pyproject.toml"}}},
		{[]string{"package.json"}, []string{"node", "docs-md"}, []ProfileEvidence{{"node", "package.json"}}},
		{[]string{"package.json", "go.mod", "setup.py"}, []string{"go", "python", "node", "docs-md"},
			[]ProfileEvidence{{"go", "go.mod"}, {"python", "setup.py"}, {"node", "package.json"}}},
	}
	for _, c := range cases {
		got, why := ProposeProfiles(c.found)
		if !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(why, c.why) {
			t.Errorf("ProposeProfiles(%v) = %v, %v; want %v, %v", c.found, got, why, c.want, c.why)
		}
		// Whatever it proposes is a selection a project may write.
		if err := (Policy{Profiles: got}).Validate(LayerProject); err != nil {
			t.Errorf("ProposeProfiles(%v) proposed an invalid selection: %v", c.found, err)
		}
	}
	if got, want := ProfileMarkers(), []string{"go.mod", "pyproject.toml", "setup.py", "requirements.txt", "package.json"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProfileMarkers() = %v, want %v", got, want)
	}
}

// A project's own patterns are probed against ITS table: a pattern that would
// be a refused move under the default selection is fine when the project did
// not select the profile that makes it one, and the reverse.
func TestValidateProbesUnderTheLayersOwnSelection(t *testing.T) {
	// *_test.go is `test` by default, so docs_paths aimed at it is test -> docs.
	if err := (Policy{DocsPaths: []string{"examples/*_test.go"}}).Validate(LayerProject); err == nil {
		t.Error("docs_paths aimed at Go tests accepted under the default selection")
	}
	// A project that did not select go has no Go tests: the same path is other.
	if err := (Policy{Profiles: []string{"python"}, DocsPaths: []string{"examples/*_test.go"}}).Validate(LayerProject); err != nil {
		t.Errorf("docs_paths over what is `other` for this project refused: %v", err)
	}
	// And a project that selected docs-txt cannot aim test_paths at its docs.
	if err := (Policy{Profiles: []string{"docs-txt"}, TestPaths: []string{"notes/*.txt"}}).Validate(LayerProject); err == nil {
		t.Error("test_paths aimed at this project's docs accepted")
	}
	// A sensitive row is never a legal target, selected or not.
	for _, sel := range [][]string{nil, {"go"}, {"node"}, {}} {
		if err := (Policy{Profiles: sel, DocsPaths: []string{"**/package.json"}}).Validate(LayerProject); err == nil {
			t.Errorf("docs_paths aimed at package.json accepted under selection %v", sel)
		}
		if err := (Policy{Profiles: sel, TestPaths: []string{"**/conftest.py"}}).Validate(LayerProject); err == nil {
			t.Errorf("test_paths aimed at conftest.py accepted under selection %v", sel)
		}
	}
}

// No layer's pattern moves a path out of a sensitive profile row, selected or
// not: the per-path lattice, with profiles in the table.
func TestLayerPatternsCannotMoveAProfilesSensitivePath(t *testing.T) {
	eff := mustFold(t, Input{
		Row:      Row{Classes: []Class{ClassSource, ClassTest, ClassDocs}},
		Instance: Policy{SourcePaths: []string{"**"}},
		Project:  Policy{Profiles: []string{"python", "node", "docs-md"}, TestPaths: []string{"**"}, DocsPaths: []string{"**"}},
		Enabled:  true,
	})
	c := quietClassifier(eff, BaseFacts{})
	for p, want := range map[string]Class{
		"package.json": ClassDeps, "web/yarn.lock": ClassDeps, "requirements.txt": ClassDeps, "setup.py": ClassDeps,
		"conftest.py": ClassTestConfig, "tsconfig.json": ClassTestConfig, "pytest.ini": ClassTestConfig,
		"api/api_pb2.py": ClassGenerated, "web/app.min.js": ClassGenerated, "go.mod": ClassDeps, "Makefile": ClassTestConfig,
	} {
		if d := c.Classify(p); d.Class != want || len(d.Refused) == 0 {
			t.Errorf("%q = %s with %d refusals; want %s, every `**` pattern refused", p, d.Class, len(d.Refused), want)
		}
	}
}

// The reference documents every profile and every pattern it ships: the table
// in docs/reference/gonk-scope.md is checked against the data, pattern by
// pattern, so the two cannot drift.
func TestReferenceDocumentsEveryProfilePattern(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/gonk-scope.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	n := 0
	for _, pr := range Profiles() {
		for _, row := range pr.Rows {
			var cells []string
			for _, pat := range row.Patterns {
				cells = append(cells, "`"+pat+"`")
			}
			line := "| `" + pr.Name + "` | `" + string(row.Class) + "` | " + strings.Join(cells, ", ") + " |"
			n++
			if !strings.Contains(doc, line) {
				t.Errorf("docs/reference/gonk-scope.md lacks the row\n%s", line)
			}
		}
	}
	if n < 20 {
		t.Fatalf("checked %d rows; the guard is vacuous", n)
	}
}

// The table's file-name shortcut (`**/<name>` matched against the last path
// segment) is an optimisation and nothing else: for every pattern of every
// table and every path of the golden list it answers what Match answers.
func TestTablePatternShortcutAgreesWithMatch(t *testing.T) {
	paths := goldenPaths(t)
	tbl := buildTable(shipped, ProfileNames())
	shortcuts, checked := 0, 0
	for _, row := range tbl.rows {
		for _, tp := range row.pats {
			if tp.base != "" {
				shortcuts++
			}
			for _, p := range paths {
				if !cleanRel(p) {
					continue
				}
				checked++
				if got, want := tp.match(strings.Split(p, "/")), Match(tp.pattern, p); got != want {
					t.Fatalf("pattern %q, path %q: the table says %v, Match says %v", tp.pattern, p, got, want)
				}
			}
		}
	}
	if shortcuts < 50 || checked < 100000 {
		t.Fatalf("%d shortcut patterns, %d comparisons; the guard is vacuous", shortcuts, checked)
	}
}
