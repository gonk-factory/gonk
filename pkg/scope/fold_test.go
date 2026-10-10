package scope

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func p64(v int64) *int64 { return &v }

var fixRow = Row{
	Classes:  []Class{ClassSource, ClassTest, ClassDocs},
	Liftable: []Class{ClassCI},
	Caps:     Caps{MaxFiles: 3, MaxLines: 40, MaxDeletes: 0, MaxFileBytes: 256 << 10, MaxPatchBytes: 16 << 10},
}

func classSet(cs []Class) map[Class]bool {
	m := map[Class]bool{}
	for _, c := range cs {
		m[c] = true
	}
	return m
}

// The named break: "a project config widens past the pack row". Each row asks
// for MORE than the pack row and must get no more than the row.
func TestProjectConfigCannotWidenPastThePackRow(t *testing.T) {
	cases := []struct {
		name    string
		project Policy
		check   func(t *testing.T, e Effective)
	}{
		{"classes outside the row are dropped",
			Policy{Classes: []Class{ClassSource, ClassDeploy, ClassDeps, ClassOther}},
			func(t *testing.T, e Effective) {
				if !reflect.DeepEqual(e.Classes, []Class{ClassSource}) {
					t.Errorf("Classes = %v, want [source]", e.Classes)
				}
				for _, c := range []Class{ClassDeploy, ClassDeps, ClassOther} {
					if e.Allows(c, Lift{}) {
						t.Errorf("project config made %s writable past the pack row", c)
					}
				}
			}},
		{"caps above the row are clamped to the row",
			Policy{Caps: CapsPolicy{MaxFiles: p64(100), MaxLines: p64(5000), MaxDeletes: p64(9), MaxFileBytes: p64(1 << 30), MaxPatchBytes: p64(1 << 30)}},
			func(t *testing.T, e Effective) {
				if e.Caps != fixRow.Caps {
					t.Errorf("Caps = %+v, want the pack row's %+v", e.Caps, fixRow.Caps)
				}
			}},
		{"caps below the row tighten",
			Policy{Caps: CapsPolicy{MaxFiles: p64(1), MaxLines: p64(10)}},
			func(t *testing.T, e Effective) {
				want := fixRow.Caps
				want.MaxFiles, want.MaxLines = 1, 10
				if e.Caps != want {
					t.Errorf("Caps = %+v, want %+v", e.Caps, want)
				}
			}},
		{"silent project inherits the row exactly",
			Policy{},
			func(t *testing.T, e Effective) {
				if !reflect.DeepEqual(e.Classes, []Class{ClassTest, ClassDocs, ClassSource}) || e.Caps != fixRow.Caps {
					t.Errorf("got %v %+v, want the row", e.Classes, e.Caps)
				}
			}},
		{"an explicit empty class list allows nothing",
			Policy{Classes: []Class{}},
			func(t *testing.T, e Effective) {
				if len(e.Classes) != 0 || e.Allows(ClassSource, Lift{}) {
					t.Errorf("Classes = %v, want none", e.Classes)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustFold(t, Input{Row: fixRow, Project: c.project, Enabled: true})
			c.check(t, e)
		})
	}
}

func TestFoldRules(t *testing.T) {
	t.Run("enablement is AND, and a disabled workflow allows nothing", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: false, LiftOptIn: []Class{ClassCI}})
		if e.Enabled || e.Allows(ClassSource, Lift{}) {
			t.Error("a disabled workflow allows a class")
		}
		if _, err := e.GrantLift(Grant{Trigger: "pipeline-fix", Workflow: "configuration", Classes: []Class{ClassCI}}); err == nil {
			t.Error("a disabled workflow granted a lift")
		}
	})
	t.Run("classes intersect across all three layers", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: true,
			Instance: Policy{Classes: []Class{ClassSource, ClassDocs}},
			Group:    Policy{Classes: []Class{ClassDocs, ClassTest}},
			Project:  Policy{Classes: []Class{ClassDocs, ClassSource}}})
		if !reflect.DeepEqual(e.Classes, []Class{ClassDocs}) {
			t.Errorf("Classes = %v, want [docs]", e.Classes)
		}
	})
	t.Run("caps are the minimum across layers", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: true,
			Instance: Policy{Caps: CapsPolicy{MaxLines: p64(30)}},
			Group:    Policy{Caps: CapsPolicy{MaxLines: p64(20), MaxFiles: p64(2)}},
			Project:  Policy{Caps: CapsPolicy{MaxLines: p64(25)}}})
		if e.Caps.MaxLines != 20 || e.Caps.MaxFiles != 2 {
			t.Errorf("Caps = %+v, want lines 20 files 2", e.Caps)
		}
	})
	t.Run("protected patterns are the union", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: true,
			Instance: Policy{ProtectedPaths: []string{"a/**"}},
			Group:    Policy{ProtectedPaths: []string{"b/**"}},
			Project:  Policy{ProtectedPaths: []string{"c/**"}}})
		c := quietClassifier(e, BaseFacts{})
		for _, p := range []string{"a/x.go", "b/x.go", "c/x.go"} {
			if got := c.Classify(p).Class; got != ClassControl {
				t.Errorf("Classify(%q) = %s, want control", p, got)
			}
		}
	})
	t.Run("bead scope narrows by intersection and never widens", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: true})
		if !e.InBead("anything/at/all.go") {
			t.Error("no bead scope declared, yet a path is outside it")
		}
		n, err := e.WithBead([]string{"pkg/parse/**", "docs/**"})
		if err != nil {
			t.Fatal(err)
		}
		n, err = n.WithBead([]string{"pkg/**"})
		if err != nil {
			t.Fatal(err)
		}
		for p, want := range map[string]bool{"pkg/parse/a.go": true, "pkg/auth/a.go": false, "docs/x.md": false} {
			if got := n.InBead(p); got != want {
				t.Errorf("InBead(%q) = %v, want %v", p, got, want)
			}
		}
		if _, err := e.WithBead([]string{"../x"}); err == nil {
			t.Error("a bad bead glob was accepted")
		}
		if e.InBead("pkg/auth/a.go") != true {
			t.Error("WithBead mutated its receiver")
		}
	})
	t.Run("Narrow selects an envelope inside the fold, never outside it", func(t *testing.T) {
		e := mustFold(t, Input{Row: fixRow, Enabled: true, Project: Policy{Classes: []Class{ClassSource, ClassDocs}}})
		n := e.Narrow(ClassSource, ClassTest, ClassDeploy)
		if !reflect.DeepEqual(n.Classes, []Class{ClassSource}) {
			t.Errorf("Narrow = %v, want [source]", n.Classes)
		}
		if !reflect.DeepEqual(e.Classes, []Class{ClassDocs, ClassSource}) {
			t.Errorf("Narrow mutated its receiver: %v", e.Classes)
		}
	})
}

func TestFoldRefusesWhatItCannotMakeSenseOf(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"no base SHA", Input{Row: fixRow, BaseSHA: "main"}, "base SHA"},
		{"short SHA", Input{Row: fixRow, BaseSHA: "abc123"}, "base SHA"},
		{"pack row lists a protected class", Input{Row: Row{Classes: []Class{ClassSource, ClassCI}}}, "protected"},
		{"pack row lifts the floor", Input{Row: Row{Liftable: []Class{ClassControl}}}, "not liftable"},
		{"pack row lifts secret", Input{Row: Row{Liftable: []Class{ClassSecret}}}, "not liftable"},
		{"pack row unknown class", Input{Row: Row{Classes: []Class{"banana"}}}, "unknown class"},
		{"pack row negative cap", Input{Row: Row{Caps: Caps{MaxFiles: -1}}}, "negative"},
		{"project declares source_paths (Q3: operator-only)", Input{Row: fixRow, Project: Policy{SourcePaths: []string{"**/*.py"}}}, "source_paths"},
		{"project pattern aimed at a locked class", Input{Row: fixRow, Project: Policy{DocsPaths: []string{"deploy/**"}}}, "deploy"},
		{"operator pattern aimed at a locked class", Input{Row: fixRow, Instance: Policy{TestPaths: []string{"chart/**"}}}, "deploy"},
		{"group bad glob", Input{Row: fixRow, Group: Policy{ProtectedPaths: []string{"/abs"}}}, "pattern"},
		{"project lists a protected class", Input{Row: fixRow, Project: Policy{Classes: []Class{ClassCI}}}, "protected"},
		{"project negative cap", Input{Row: fixRow, Project: Policy{Caps: CapsPolicy{MaxLines: p64(-5)}}}, "negative"},
		{"lift opt-in to the floor", Input{Row: fixRow, LiftOptIn: []Class{ClassControl}}, "not liftable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.in.BaseSHA == "" {
				c.in.BaseSHA = testSHA
			}
			c.in.Enabled = true
			_, err := Fold(c.in)
			if err == nil {
				t.Fatal("Fold accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// Dispatch-time validation: the same checks, reachable without a pack row, so
// gonkcfg.Load and opercfg.Load refuse a bad policy before any spend.
func TestPolicyValidate(t *testing.T) {
	ok := Policy{
		Classes: []Class{ClassSource, ClassTest}, Caps: CapsPolicy{MaxFiles: p64(2)},
		ProtectedPaths: []string{"internal/auth/**"}, CIPaths: []string{"ci/**"},
		TestPaths: []string{"internal/testutil/**"}, DocsPaths: []string{"notes/**"},
	}
	if err := ok.Validate(LayerProject); err != nil {
		t.Fatalf("valid project policy refused: %v", err)
	}
	op := ok
	op.SourcePaths = []string{"**/*.py"}
	op.DenyLift = []Class{ClassCI}
	if err := op.Validate(LayerOperator); err != nil {
		t.Fatalf("valid operator policy refused: %v", err)
	}
	if err := op.Validate(LayerProject); err == nil {
		t.Error("a project policy with source_paths was accepted")
	}
	bad := map[string]Policy{
		"deploy -> docs":          {DocsPaths: []string{"deploy/**"}},
		"Dockerfile -> test":      {TestPaths: []string{"**/Dockerfile*"}},
		"go.mod -> docs":          {DocsPaths: []string{"go.mod"}},
		".agent -> test":          {TestPaths: []string{".agent/**"}},
		"chart -> ci":             {CIPaths: []string{"chart/ci/**"}},
		"secret -> ci":            {CIPaths: []string{"**/*.pem"}},
		"unknown class":           {Classes: []Class{"banana"}},
		"deny_lift of non-lift":   {DenyLift: []Class{ClassSource}},
		"too many patterns":       {ProtectedPaths: make([]string, MaxPatterns+1)},
		"bad pattern in ci_paths": {CIPaths: []string{"a[b]"}},
	}
	for name, p := range bad {
		if err := p.Validate(LayerOperator); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !(Policy{}).IsZero() || ok.IsZero() {
		t.Error("IsZero is wrong")
	}
}

// ---- the lift ----

func TestGrantLift(t *testing.T) {
	grant := Grant{Trigger: "pipeline-fix", Workflow: "configuration", Classes: []Class{ClassCI}}
	t.Run("needs pack AND project opt-in AND no operator veto", func(t *testing.T) {
		cases := []struct {
			name string
			in   Input
			ok   bool
		}{
			{"all three", Input{Row: fixRow, LiftOptIn: []Class{ClassCI}}, true},
			{"no project opt-in", Input{Row: fixRow}, false},
			{"pack row does not list it", Input{Row: Row{Classes: fixRow.Classes, Caps: fixRow.Caps}, LiftOptIn: []Class{ClassCI}}, false},
			{"instance vetoes", Input{Row: fixRow, LiftOptIn: []Class{ClassCI}, Instance: Policy{DenyLift: []Class{ClassCI}}}, false},
			{"group vetoes", Input{Row: fixRow, LiftOptIn: []Class{ClassCI}, Group: Policy{DenyLift: []Class{ClassCI}}}, false},
		}
		for _, c := range cases {
			c.in.Enabled = true
			e := mustFold(t, c.in)
			l, err := e.GrantLift(grant)
			if (err == nil) != c.ok {
				t.Errorf("%s: GrantLift err = %v, want ok=%v", c.name, err, c.ok)
			}
			if l.Has(ClassCI) != c.ok || e.Allows(ClassCI, l) != c.ok {
				t.Errorf("%s: Has/Allows(ci) != %v", c.name, c.ok)
			}
		}
	})
	e := mustFold(t, Input{Row: fixRow, Enabled: true, LiftOptIn: []Class{ClassCI}})
	t.Run("the floor is never granted", func(t *testing.T) {
		for _, c := range []Class{ClassControl, ClassSecret, ClassDeploy, ClassSource, "banana"} {
			l, err := e.GrantLift(Grant{Trigger: "t", Workflow: "w", Classes: []Class{ClassCI, c}})
			if err == nil {
				t.Errorf("GrantLift granted %s", c)
			}
			if !l.Empty() {
				t.Errorf("a refused grant returned a non-empty lift (%s)", l)
			}
		}
	})
	t.Run("a grant names who asked", func(t *testing.T) {
		for _, g := range []Grant{{Workflow: "w", Classes: []Class{ClassCI}}, {Trigger: "t", Classes: []Class{ClassCI}}, {Trigger: "t", Workflow: "w"}} {
			if _, err := e.GrantLift(g); err == nil {
				t.Errorf("GrantLift(%+v) accepted an unnamed or empty grant", g)
			}
		}
		l, err := e.GrantLift(grant)
		if err != nil {
			t.Fatal(err)
		}
		if s := l.String(); !strings.Contains(s, "ci") || !strings.Contains(s, "pipeline-fix") || !strings.Contains(s, "configuration") {
			t.Errorf("Lift.String() = %q, want the class, trigger and workflow", s)
		}
		if got := l.Classes(); len(got) != 1 || got[0] != ClassCI {
			t.Errorf("Classes() = %v", got)
		}
	})
	t.Run("the zero lift lifts nothing", func(t *testing.T) {
		var z Lift
		if !z.Empty() || z.Has(ClassCI) || e.Allows(ClassCI, z) || z.String() != "none" {
			t.Error("the zero Lift lifts something")
		}
	})
	t.Run("a lift is honoured only inside the fold that could grant it", func(t *testing.T) {
		l, err := e.GrantLift(grant)
		if err != nil {
			t.Fatal(err)
		}
		other := mustFold(t, Input{Row: fixRow, Enabled: true}) // no opt-in
		if other.Allows(ClassCI, l) {
			t.Error("a lift granted under one project's fold was honoured by another's")
		}
	})
	t.Run("a lift widens nothing but its own class", func(t *testing.T) {
		l, _ := e.GrantLift(grant)
		for _, c := range Classes() {
			want := classSet(e.Classes)[c] || c == ClassCI
			if got := e.Allows(c, l); got != want {
				t.Errorf("Allows(%s, lift ci) = %v, want %v", c, got, want)
			}
		}
	})
	t.Run("Narrow keeps the lift's reach but not the classes", func(t *testing.T) {
		l, _ := e.GrantLift(grant)
		n := e.Narrow() // the `configuration` envelope: no ordinary class at all
		if n.Allows(ClassSource, l) || !n.Allows(ClassCI, l) {
			t.Error("Narrow() did not leave exactly the lifted class")
		}
	})
}

// A Lift cannot be built from bytes: not from a batch, not from a file, not
// from issue text that reached a decoder. Every decoder REFUSES, and nothing
// is left behind in the value.
func TestLiftCannotBeDecoded(t *testing.T) {
	type carrier struct {
		Lift Lift `json:"lift" yaml:"lift" toml:"lift"`
	}
	decoders := map[string]func(doc string, c *carrier) error{
		"json": func(doc string, c *carrier) error { return json.Unmarshal([]byte(doc), c) },
		"yaml": func(doc string, c *carrier) error { return yaml.Unmarshal([]byte(doc), c) },
		"toml": func(doc string, c *carrier) error { _, err := toml.Decode(doc, c); return err },
	}
	docs := map[string][]string{
		"json": {`{"lift":{"classes":"ci"}}`, `{"lift":"ci"}`, `{"lift":["ci"]}`, `{"lift":{"Classes":["ci"],"ci":true}}`},
		"yaml": {"lift: ci\n", "lift: [ci]\n", "lift: {classes: ci}\n"},
		"toml": {"lift = \"ci\"\n"},
	}
	for name, dec := range decoders {
		for _, doc := range docs[name] {
			var c carrier
			err := dec(doc, &c)
			if err == nil {
				t.Errorf("%s %q decoded into a Lift without an error", name, doc)
			}
			if !c.Lift.Empty() || c.Lift.Has(ClassCI) {
				t.Errorf("%s %q produced a lift: %s", name, doc, c.Lift)
			}
		}
	}
	// Directly, too.
	var l Lift
	if err := json.Unmarshal([]byte(`{"classes":"ci"}`), &l); err == nil || !l.Empty() {
		t.Errorf("json.Unmarshal into a Lift: err=%v lift=%s", err, l)
	}
	rt := reflect.TypeOf(Lift{})
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).IsExported() {
			t.Errorf("Lift.%s is exported; a lift must only come from Effective.GrantLift", rt.Field(i).Name)
		}
	}
}

// ---- the property: no fold is wider than the pack row ----

var locked = []Class{ClassControl, ClassSecret, ClassCI, ClassDeploy, ClassTestConfig, ClassDeps, ClassAgentContext, ClassGenerated}

func randSubset(r *rand.Rand, from []Class) []Class {
	var out []Class
	for _, c := range from {
		if r.Intn(2) == 0 {
			out = append(out, c)
		}
	}
	return out
}

func randCaps(r *rand.Rand) CapsPolicy {
	pick := func() *int64 {
		if r.Intn(2) == 0 {
			return nil
		}
		return p64(int64(r.Intn(1 << 20)))
	}
	return CapsPolicy{MaxFiles: pick(), MaxLines: pick(), MaxDeletes: pick(), MaxFileBytes: pick(), MaxPatchBytes: pick()}
}

var patternPool = []string{"**", "**/*", "**/*.go", "**/*.py", "pkg/**", "internal/testutil/**", "tools/**", "**/*.yaml",
	"**/*.yml", "notes/**", "ci/**", "scripts/*", "**/*_helper.go", "a/**/z", "**/*.txt", "x"}

func randPatterns(r *rand.Rand) []string {
	var out []string
	for i := r.Intn(4); i > 0; i-- {
		out = append(out, patternPool[r.Intn(len(patternPool))])
	}
	return out
}

func randPolicy(r *rand.Rand, layer Layer) Policy {
	p := Policy{Caps: randCaps(r), ProtectedPaths: randPatterns(r), CIPaths: randPatterns(r),
		TestPaths: randPatterns(r), DocsPaths: randPatterns(r)}
	if r.Intn(2) == 0 {
		p.Classes = randSubset(r, []Class{ClassDeploy, ClassTestConfig, ClassDeps, ClassAgentContext, ClassGenerated, ClassTest, ClassDocs, ClassSource, ClassOther})
		if p.Classes == nil {
			p.Classes = []Class{}
		}
	}
	if layer == LayerOperator {
		p.SourcePaths = randPatterns(r)
		if r.Intn(4) == 0 {
			p.DenyLift = []Class{ClassCI}
		}
	}
	return p
}

var pathPool = []string{
	".gonk.yml", "CODEOWNERS", ".git/config", "sub/CODEOWNERS", ".gitlab-ci.yml", ".gitlab/ci/a.yml", ".github/workflows/a.yml",
	"certs/a.pem", ".env", "pkg/secrets/a.go", ".gitlab/a.key", "chart/values.yaml", "deploy/a.yaml", "k8s/a.yaml", "Dockerfile",
	"x/kustomization.yaml", "Makefile", "a.mk", ".golangci.yml", "go.work", "go.mod", "go.sum", "vendor/a/b.go", ".agent/a.md",
	"pkg/gen/zz.go", "pkg/a_test.go", "pkg/testdata/a.json", "README.md", "docs/a.txt", "CHANGELOG", "main.go", "pkg/a.go",
	"internal/testutil/a.go", "tools/a.py", "notes/a.txt", "ci/a.yml", "scripts/a", "a/b/z", "x", "pkg/x_helper.go", "LICENSE",
	"../escape.go", "/abs.go", "",
	// gonk-wj14: what the non-Go profiles have an opinion on.
	"svc/app.py", "svc/test_app.py", "tests/a.json", "conftest.py", "setup.py", "requirements.txt", "pyproject.toml", "api/a_pb2.py",
	"web/index.ts", "web/index.test.ts", "web/__tests__/a.js", "package.json", "web/yarn.lock", "tsconfig.json", "jest.config.js",
	"node_modules/a/b.js", "web/a.min.js", "guide/a.rst", "guide/a.adoc", "notes/b.txt", "CMakeLists.txt", "docs/conf.py",
	".gitlab/package.json", "secrets/a.py", "deploy/a.py", ".agent/a.py",
}

// randProfiles gives the layers of in a random profile configuration that
// Fold accepts: operator layers restrict the menu at random, and the project
// selects at random from what is left (or says nothing).
func randProfiles(r *rand.Rand, in *Input) {
	menu := ProfileNames()
	pick := func(from []string) []string {
		out := []string{}
		for _, n := range from {
			if r.Intn(2) == 0 {
				out = append(out, n)
			}
		}
		return out
	}
	for _, op := range []*Policy{&in.Instance, &in.Group} {
		if r.Intn(3) == 0 {
			op.AllowedProfiles = pick(ProfileNames())
			menu = foldAllowedProfiles(menu, op.AllowedProfiles)
		}
	}
	if r.Intn(4) != 0 {
		in.Project.Profiles = pick(menu)
	}
}

// TestNoFoldIsWiderThanThePackRow is 066 invariant 2. Over random pack rows,
// operator layers, project layers, lift opt-ins, controller narrowings and
// bead scopes it asserts, for every fold that loads:
//
//	classes  subset of the row's          caps  <= the row's, each one
//	liftable subset of the row's          a row that is disabled stays disabled
//
// and, per path, the statement of the lattice: a path the fold lets a dispatch
// write has a BUILT-IN class the row itself permits (directly, or as a class
// the row lets a dispatch lift), or it is one of the four unlocked classes a
// layer may reclassify (source, other, test, docs) -- and then only INTO a
// class the row permits. Nothing whose built-in class is locked and outside
// the row is ever writable, whatever the layers say.
func TestNoFoldIsWiderThanThePackRow(t *testing.T) {
	r := rand.New(rand.NewSource(20261005))
	folds, writable, sensitive, permissive, selfRefused := 0, 0, 0, 0, 0
	selections := map[string]bool{}
	// What each path is with the whole menu shipped and NOTHING selected: the
	// class a sensitive row gives it, or `other`.
	facts := BaseFacts{Generated: map[string]bool{"pkg/gen/zz.go": true}, CILocalIncludes: []string{"ci/a.yml"}}
	nothingSelected := buildTable(shipped, nil)
	alwaysClass := map[string]Class{}
	for _, p := range pathPool {
		alwaysClass[p], _ = nothingSelected.classify(p, facts)
	}
	for i := 0; i < 4000; i++ {
		row := Row{
			Classes: randSubset(r, []Class{ClassDeploy, ClassTestConfig, ClassDeps, ClassAgentContext, ClassGenerated, ClassTest, ClassDocs, ClassSource, ClassOther}),
			Caps: Caps{MaxFiles: int64(r.Intn(50)), MaxLines: int64(r.Intn(3000)), MaxDeletes: int64(r.Intn(3)),
				MaxFileBytes: int64(r.Intn(1 << 19)), MaxPatchBytes: int64(r.Intn(1 << 19))},
		}
		if r.Intn(2) == 0 {
			row.Liftable = []Class{ClassCI}
		}
		in := Input{Row: row, Instance: randPolicy(r, LayerOperator), Group: randPolicy(r, LayerOperator),
			Project: randPolicy(r, LayerProject), Enabled: r.Intn(8) != 0, BaseSHA: testSHA}
		if r.Intn(2) == 0 {
			in.LiftOptIn = []Class{ClassCI}
		}
		randProfiles(r, &in)
		eff, err := Fold(in)
		if err != nil && in.Project.Profiles != nil && strings.Contains(err.Error(), "may not reclassify docs -> test") {
			// The one way the generator contradicts itself: a project that
			// selected docs-txt and also aims test_paths at *.txt. Refusing
			// that is correct (and counted); the property is about folds
			// that load, so drop the pattern and go on.
			selfRefused++
			in.Project.TestPaths = nil
			eff, err = Fold(in)
		}
		if err != nil {
			t.Fatalf("iteration %d: Fold refused a generated input: %v", i, err)
		}
		folds++
		selections[strings.Join(eff.Profiles(), ",")] = true
		if r.Intn(3) == 0 {
			eff = eff.Narrow(randSubset(r, Classes())...)
		}
		if r.Intn(4) == 0 {
			if eff, err = eff.WithBead(randPatterns(r)); err != nil {
				t.Fatal(err)
			}
		}

		rowClasses, rowLift := classSet(row.Classes), classSet(row.Liftable)
		for _, c := range eff.Classes {
			if !rowClasses[c] {
				t.Fatalf("iteration %d: fold allows class %s, the row %v does not\ninput: %+v", i, c, row.Classes, in)
			}
		}
		for _, c := range eff.Liftable() {
			if !rowLift[c] {
				t.Fatalf("iteration %d: fold lets a dispatch lift %s, the row does not", i, c)
			}
		}
		if eff.Caps.MaxFiles > row.Caps.MaxFiles || eff.Caps.MaxLines > row.Caps.MaxLines || eff.Caps.MaxDeletes > row.Caps.MaxDeletes ||
			eff.Caps.MaxFileBytes > row.Caps.MaxFileBytes || eff.Caps.MaxPatchBytes > row.Caps.MaxPatchBytes {
			t.Fatalf("iteration %d: fold caps %+v exceed the row's %+v", i, eff.Caps, row.Caps)
		}
		if !in.Enabled && eff.Enabled {
			t.Fatalf("iteration %d: a disabled workflow came out enabled", i)
		}

		// The widest lift this fold will grant.
		lift, _ := eff.GrantLift(Grant{Trigger: "t", Workflow: "w", Classes: eff.Liftable()})
		cl := quietClassifier(eff, facts)
		for _, p := range pathPool {
			d := cl.Classify(p)
			// Profiles (gonk-wj14): a path a sensitive row of ANY shipped profile
			// names keeps that class under every selection and every layer --
			// it leaves it only toward control. Selecting is never how a
			// sensitive path becomes writable.
			if always := alwaysClass[p]; always != ClassOther {
				sensitive++
				if d.Builtin != always {
					t.Fatalf("iteration %d: %q is %s with profiles %v selected; a sensitive row makes it %s whatever is selected", i, p, d.Builtin, eff.Profiles(), always)
				}
				if d.Class != always && d.Class != ClassControl {
					t.Fatalf("iteration %d: %q left %s for %s", i, p, always, d.Class)
				}
			} else if d.Builtin.Sensitivity() == Normal {
				// ... and a permissive class comes only from a SELECTED profile
				// the operator layers allow.
				if d.Profile == "" || !containsString(eff.Profiles(), d.Profile) {
					t.Fatalf("iteration %d: %q is %s by profile %q, which is not selected (%v)", i, p, d.Builtin, d.Profile, eff.Profiles())
				}
				for _, op := range []Policy{in.Instance, in.Group} {
					if op.AllowedProfiles != nil && !containsString(op.AllowedProfiles, d.Profile) {
						t.Fatalf("iteration %d: %q is %s by profile %q, which an operator layer does not allow (%v)", i, p, d.Builtin, d.Profile, op.AllowedProfiles)
					}
				}
				permissive++
			}
			if !eff.Allows(d.Class, lift) || !eff.InBead(p) {
				continue
			}
			writable++
			if !in.Enabled {
				t.Fatalf("iteration %d: %q writable under a disabled workflow", i, p)
			}
			if !rowClasses[d.Class] && !rowLift[d.Class] {
				t.Fatalf("iteration %d: %q is writable as %s, which the row neither allows nor lets a dispatch lift", i, p, d.Class)
			}
			if d.Class == ClassControl || d.Class == ClassSecret {
				t.Fatalf("iteration %d: floor path %q (%s) is writable", i, p, d.Class)
			}
			if d.Class != d.Builtin {
				unlocked := d.Builtin == ClassSource || d.Builtin == ClassOther || d.Builtin == ClassTest || d.Builtin == ClassDocs
				if !unlocked {
					t.Fatalf("iteration %d: %q moved out of locked class %s into %s and became writable", i, p, d.Builtin, d.Class)
				}
			}
			for _, lc := range locked {
				if d.Builtin == lc && !rowClasses[lc] && !rowLift[lc] {
					t.Fatalf("iteration %d: %q has locked built-in class %s outside the row, yet is writable as %s", i, p, lc, d.Class)
				}
			}
		}
	}
	// Not vacuous: the generator must produce folds that allow writes.
	if folds < 4000 || writable < 1000 {
		t.Fatalf("property ran over %d folds and %d writable (path, fold) pairs; too few to mean anything", folds, writable)
	}
	if selfRefused == 0 || selfRefused > folds/10 {
		t.Fatalf("%d of %d generated inputs aimed test_paths at their own docs; expected a few", selfRefused, folds)
	}
	if len(selections) < 40 || sensitive < 50000 || permissive < 10000 {
		t.Fatalf("property saw %d distinct profile selections, %d sensitive and %d permissive (path, fold) pairs; too few to mean anything", len(selections), sensitive, permissive)
	}
}

func containsString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// FoldPolicies (gonk gonk-a82p) merges two operator layers of one kind into
// one. Its contract is that doing so is indistinguishable from handing Fold the
// two layers separately: same classes, caps, lifts, and the same class for
// every path -- so folding groups can never widen what Fold would have allowed.
func TestFoldPoliciesEqualsFoldingTheLayersSeparately(t *testing.T) {
	r := rand.New(rand.NewSource(20261006))
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 400; i++ {
		row := Row{
			Classes:  randSubset(r, []Class{ClassTest, ClassDocs, ClassSource, ClassOther}),
			Liftable: []Class{ClassCI},
			Caps:     Caps{MaxFiles: 1 << 19, MaxLines: 1 << 19, MaxDeletes: 1 << 19, MaxFileBytes: 1 << 19, MaxPatchBytes: 1 << 19},
		}
		base := Input{Row: row, Enabled: true, BaseSHA: testSHA, LiftOptIn: []Class{ClassCI}}
		base.Instance, base.Group = randPolicy(r, LayerOperator), randPolicy(r, LayerOperator)
		randProfiles(r, &base)
		a, b := base.Instance, base.Group
		sep, merged := base, base
		merged.Instance, merged.Group = Policy{}, FoldPolicies(a, b)
		es, err := Fold(sep)
		if err != nil {
			t.Fatal(err)
		}
		em, err := Fold(merged)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(es.Profiles(), em.Profiles()) {
			t.Fatalf("case %d: profiles %v folded separately, %v merged (a=%+v b=%+v)", i, es.Profiles(), em.Profiles(), a, b)
		}
		if !reflect.DeepEqual(es.Classes, em.Classes) || es.Caps != em.Caps || !reflect.DeepEqual(es.Liftable(), em.Liftable()) {
			t.Fatalf("case %d: separate %v %+v %v != merged %v %+v %v (a=%+v b=%+v)", i,
				es.Classes, es.Caps, es.Liftable(), em.Classes, em.Caps, em.Liftable(), a, b)
		}
		cs, cm := es.Classifier(BaseFacts{}, quiet), em.Classifier(BaseFacts{}, quiet)
		for _, p := range pathPool {
			if cs.Classify(p).Class != cm.Classify(p).Class {
				t.Fatalf("case %d: path %q is %s folded separately and %s merged (a=%+v b=%+v)", i, p, cs.Classify(p).Class, cm.Classify(p).Class, a, b)
			}
		}
	}
}

func TestFoldPoliciesRules(t *testing.T) {
	a := Policy{Classes: []Class{ClassSource, ClassTest}, Caps: CapsPolicy{MaxLines: p64(10), MaxFiles: p64(5)},
		ProtectedPaths: []string{"a/**"}, DenyLift: []Class{ClassCI}}
	b := Policy{Classes: []Class{ClassTest, ClassDocs}, Caps: CapsPolicy{MaxLines: p64(99), MaxDeletes: p64(1)},
		ProtectedPaths: []string{"b/**", "a/**"}}
	got := FoldPolicies(a, b)
	if !reflect.DeepEqual(got.Classes, []Class{ClassTest}) {
		t.Errorf("classes = %v", got.Classes)
	}
	if *got.Caps.MaxLines != 10 || *got.Caps.MaxFiles != 5 || *got.Caps.MaxDeletes != 1 || got.Caps.MaxFileBytes != nil {
		t.Errorf("caps = %+v", got.Caps)
	}
	if !reflect.DeepEqual(got.ProtectedPaths, []string{"a/**", "b/**"}) || !reflect.DeepEqual(got.DenyLift, []Class{ClassCI}) {
		t.Errorf("patterns %v deny %v", got.ProtectedPaths, got.DenyLift)
	}
	if !FoldPolicies(Policy{}, Policy{}).IsZero() {
		t.Error("two silent layers did not fold to a silent layer")
	}
	// Inputs are not aliased into the result.
	*got.Caps.MaxLines = 1
	if *a.Caps.MaxLines != 10 {
		t.Error("FoldPolicies aliased a cap pointer from its input")
	}
}
