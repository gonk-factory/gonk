package scope

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// One row per class BOUNDARY: a path that is in the class, next to the nearest
// path that is not. First match in the documented order wins, so several rows
// exist to pin the ORDER (a path that two classes' patterns both match).
func TestBuiltinClassBoundaries(t *testing.T) {
	cases := []struct {
		path string
		want Class
	}{
		// control
		{".git/config", ClassControl},
		{"CODEOWNERS", ClassControl},
		{"docs/CODEOWNERS", ClassControl},
		{".gitlab/CODEOWNERS", ClassControl}, // control outranks ci
		{".gonk.yml", ClassControl},
		{"sub/.gonk.yml", ClassControl}, // basename, as pkg/effects/protected.go
		{".agent/gonk.yml.example", ClassAgentContext},
		{"internal/codeowners/parse.go", ClassSource},
		{".gitignore", ClassOther}, // .git/ is the directory
		// ci
		{".gitlab-ci.yml", ClassCI},
		{".gitlab-ci.yaml", ClassCI},
		{".gitlab/ci/build.yml", ClassCI},
		{".github/workflows/deploy.yml", ClassCI},
		{"sub/.gitlab-ci.yml", ClassOther}, // only the root file is the definition
		{"src/github/client.go", ClassSource},
		{"docs/gitlab-ci-explained.md", ClassDocs},
		// secret -- and it outranks ci (the floor wins over a liftable class)
		{"certs/server.pem", ClassSecret},
		{"deploy/tls.key", ClassSecret},
		{".env", ClassSecret},
		{"svc/.env.local", ClassSecret},
		{"home/id_rsa.pub", ClassSecret},
		{"x/bundle.p12", ClassSecret},
		{"pkg/secrets/store.go", ClassSecret},
		{".gitlab/deploy.key", ClassSecret},
		{".github/secrets/token.yml", ClassSecret},
		{"pkg/secret/store.go", ClassSource}, // singular: not the pattern
		{"pkg/keys/key.go", ClassSource},
		// deploy
		{"chart/values.yaml", ClassDeploy},
		{"charts/x/Chart.yaml", ClassDeploy},
		{"deploy/prod.yaml", ClassDeploy},
		{"k8s/deployment-web.yaml", ClassDeploy},
		{"Dockerfile", ClassDeploy},
		{"images/Dockerfile.agent", ClassDeploy},
		{"overlays/dev/kustomization.yaml", ClassDeploy},
		{"chart/README.md", ClassDeploy}, // deploy outranks docs
		{"pkg/chart/render.go", ClassSource},
		// test-config
		{"Makefile", ClassTestConfig},
		{"build/rules.mk", ClassTestConfig},
		{".golangci.yml", ClassTestConfig},
		{"pkg/x/testmain_test.go", ClassTestConfig}, // outranks test
		{"go.work", ClassTestConfig},
		{"sub/Makefile", ClassOther},
		// deps
		{"go.mod", ClassDeps},
		{"go.sum", ClassDeps},
		{"vendor/github.com/x/y/z.go", ClassDeps},
		{"vendor/github.com/x/y/z_test.go", ClassDeps}, // outranks test
		{"sub/go.mod", ClassOther},
		// agent-context
		{".agent/README.md", ClassAgentContext}, // outranks docs
		{".agentx/README.md", ClassDocs},
		// test
		{"pkg/x/x_test.go", ClassTest},
		{"pkg/x/testdata/in.json", ClassTest},
		{"pkg/x/testdata/README.md", ClassTest}, // outranks docs
		{"pkg/x/test.go", ClassSource},
		// docs
		{"README.md", ClassDocs},
		{"docs/design.txt", ClassDocs},
		{"docs/tool/main.go", ClassDocs}, // outranks source
		{"CHANGELOG", ClassDocs},
		{"CHANGELOG.rst", ClassDocs},
		{"sub/CHANGELOG.rst", ClassOther},
		// source
		{"main.go", ClassSource},
		{"internal/a/b/c.go", ClassSource},
		// other
		{"script.py", ClassOther},
		{"assets/logo.png", ClassOther},
		{"LICENSE", ClassOther},
	}
	for _, c := range cases {
		if got := Builtin(c.path); got != c.want {
			t.Errorf("Builtin(%q) = %s, want %s", c.path, got, c.want)
		}
	}
}

// A path that is not a clean relative path has no honest class. It is given
// the one class nothing can ever write.
func TestUnclassifiablePathsAreControl(t *testing.T) {
	for _, p := range []string{"", "/etc/passwd", "../x.go", "a/../../x.go", "a//b.go", "./a.go", "a/./b.go", "a/b/", "a\x00.go", "."} {
		if got := Builtin(p); got != ClassControl {
			t.Errorf("Builtin(%q) = %s, want control (fail closed)", p, got)
		}
	}
}

func TestEveryClassHasASensitivityAndIsListedOnce(t *testing.T) {
	want := map[Class]Sensitivity{
		ClassControl: Protected, ClassCI: Protected, ClassSecret: Protected,
		ClassDeploy: Sensitive, ClassTestConfig: Sensitive, ClassDeps: Sensitive,
		ClassAgentContext: Sensitive, ClassGenerated: Sensitive, ClassOther: Sensitive,
		ClassTest: Normal, ClassDocs: Normal, ClassSource: Normal,
	}
	if len(Classes()) != len(want) {
		t.Fatalf("Classes() has %d entries, want %d", len(Classes()), len(want))
	}
	seen := map[Class]bool{}
	for _, c := range Classes() {
		if seen[c] {
			t.Errorf("class %s listed twice", c)
		}
		seen[c] = true
		if got := c.Sensitivity(); got != want[c] {
			t.Errorf("%s.Sensitivity() = %s, want %s", c, got, want[c])
		}
		if !c.Valid() {
			t.Errorf("%s.Valid() = false", c)
		}
	}
	if Class("banana").Valid() {
		t.Error("an unknown class is Valid")
	}
}

// The floor: the classes nothing lifts. Only `ci` is liftable.
func TestOnlyCIIsLiftable(t *testing.T) {
	for _, c := range Classes() {
		if got, want := Liftable(c), c == ClassCI; got != want {
			t.Errorf("Liftable(%s) = %v, want %v", c, got, want)
		}
	}
	floor := Floor()
	if len(floor) != 2 || floor[0] != ClassControl || floor[1] != ClassSecret {
		t.Errorf("Floor() = %v, want [control secret]", floor)
	}
}

// The reclassification lattice, 066 section 3.2 and owner decision Q3.
func TestLatticeMoves(t *testing.T) {
	cases := []struct {
		from, to Class
		layer    Layer
		ok       bool
	}{
		// The named refusal.
		{ClassDeploy, ClassDocs, LayerProject, false},
		{ClassDeploy, ClassDocs, LayerOperator, false},
		// The sanctioned downward moves (Q3).
		{ClassSource, ClassTest, LayerProject, true},
		{ClassSource, ClassDocs, LayerProject, true},
		{ClassOther, ClassTest, LayerProject, true},
		{ClassOther, ClassDocs, LayerProject, true},
		{ClassSource, ClassTest, LayerOperator, true},
		// other -> source is operator-only (Q3: "everything else operator-only").
		{ClassOther, ClassSource, LayerOperator, true},
		{ClassOther, ClassSource, LayerProject, false},
		// Sideways between normal classes is not a listed move.
		{ClassTest, ClassDocs, LayerProject, false},
		{ClassDocs, ClassTest, LayerProject, false},
		{ClassTest, ClassSource, LayerOperator, false},
		// Toward control: always.
		{ClassDeploy, ClassControl, LayerProject, true},
		{ClassSource, ClassControl, LayerProject, true},
		{ClassCI, ClassControl, LayerProject, true},
		{ClassSecret, ClassControl, LayerProject, true},
		// Toward ci: only from the four unlocked classes, because ci is the one
		// protected class a dispatch can lift.
		{ClassSource, ClassCI, LayerProject, true},
		{ClassOther, ClassCI, LayerProject, true},
		{ClassTest, ClassCI, LayerProject, true},
		{ClassDocs, ClassCI, LayerProject, true},
		{ClassDeploy, ClassCI, LayerProject, false},
		{ClassSecret, ClassCI, LayerProject, false},
		{ClassControl, ClassCI, LayerProject, false},
		{ClassTestConfig, ClassCI, LayerOperator, false},
		// No config key targets these.
		{ClassSource, ClassDeps, LayerProject, false},
		{ClassSource, ClassSecret, LayerOperator, false},
		// Identity is not a move.
		{ClassTest, ClassTest, LayerProject, true},
	}
	for _, c := range cases {
		err := CheckMove(c.from, c.to, c.layer)
		if (err == nil) != c.ok {
			t.Errorf("CheckMove(%s -> %s, %s) = %v, want ok=%v", c.from, c.to, c.layer, err, c.ok)
		}
	}
	// Exhaustive: nothing ever leaves a locked class except toward control.
	for _, from := range []Class{ClassControl, ClassCI, ClassSecret, ClassDeploy, ClassTestConfig, ClassDeps, ClassAgentContext, ClassGenerated} {
		for _, to := range Classes() {
			if to == from || to == ClassControl {
				continue
			}
			for _, l := range []Layer{LayerProject, LayerOperator} {
				if err := CheckMove(from, to, l); err == nil {
					t.Errorf("CheckMove(%s -> %s, %s) allowed; %s is locked", from, to, l, from)
				}
			}
		}
	}
}

func mustFold(t *testing.T, in Input) Effective {
	t.Helper()
	if in.BaseSHA == "" {
		in.BaseSHA = testSHA
	}
	eff, err := Fold(in)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	return eff
}

func quietClassifier(eff Effective, base BaseFacts) *Classifier {
	// Discarded BEFORE formatting: the property tests classify millions of paths.
	return eff.Classifier(base, slog.New(slog.NewTextHandler(new(bytes.Buffer), &slog.HandlerOptions{Level: slog.LevelError + 1})))
}

func TestClassifyAppliesLayerPatternsUnderTheLattice(t *testing.T) {
	eff := mustFold(t, Input{
		Row: Row{Classes: []Class{ClassSource, ClassTest, ClassDocs}},
		Instance: Policy{
			SourcePaths:    []string{"**/*.py"},
			ProtectedPaths: []string{"release/**"},
		},
		// Every pattern here passes the load-time probe (none is AIMED at a locked
		// class), yet several also match locked paths. Those are refused per path.
		Project: Policy{
			TestPaths:      []string{"internal/testutil/**", "**/*.yaml", "**/*.py"},
			DocsPaths:      []string{"notes/**", "**/prod.yaml", "pkg/x/x_*"},
			CIPaths:        []string{"ci/**", "**/ci.yaml", "certs/**"},
			ProtectedPaths: []string{"internal/auth/**"},
		},
		Enabled: true,
	})
	c := quietClassifier(eff, BaseFacts{})
	cases := []struct {
		path    string
		want    Class
		builtin Class
		refused int
	}{
		{"internal/testutil/fake.go", ClassTest, ClassSource, 0},
		{"internal/testutil/data.bin", ClassTest, ClassOther, 0},
		{"notes/plan.txt", ClassDocs, ClassOther, 0},
		{"ci/build.sh", ClassCI, ClassOther, 0},
		{"internal/auth/token.go", ClassControl, ClassSource, 0},
		{"release/notes.md", ClassControl, ClassDocs, 0},
		// operator source_paths moves other -> source; the project's test_paths
		// also matches *.py and, test being checked first, wins.
		{"tools/gen.py", ClassTest, ClassOther, 0},
		// The lattice refuses, per path, and the built-in class stands.
		{"chart/values.yaml", ClassDeploy, ClassDeploy, 1}, // test_paths **/*.yaml
		{"deploy/prod.yaml", ClassDeploy, ClassDeploy, 2},  // test_paths and docs_paths
		{"k8s/ci.yaml", ClassDeploy, ClassDeploy, 2},       // ci_paths and test_paths
		{"certs/server.pem", ClassSecret, ClassSecret, 1},  // ci_paths certs/**
		{"pkg/x/x_test.go", ClassTest, ClassTest, 1},       // docs_paths: test -> docs is not a move
		{"pkg/x/x.go", ClassSource, ClassSource, 0},        // untouched
		{"certs/readme.txt", ClassCI, ClassOther, 0},       // ci_paths certs/**, an unlocked path
		{"internal/auth/CODEOWNERS", ClassControl, ClassControl, 0},
	}
	for _, tc := range cases {
		d := c.Classify(tc.path)
		if d.Class != tc.want || d.Builtin != tc.builtin || len(d.Refused) != tc.refused {
			t.Errorf("Classify(%q) = class %s builtin %s refused %d (%v), want %s / %s / %d",
				tc.path, d.Class, d.Builtin, len(d.Refused), d.Refused, tc.want, tc.builtin, tc.refused)
		}
		if d.Reclassified() != (tc.want != tc.builtin) {
			t.Errorf("Classify(%q).Reclassified() = %v", tc.path, d.Reclassified())
		}
	}
}

func TestBaseFactsClassifyCIIncludesAndGeneratedFiles(t *testing.T) {
	eff := mustFold(t, Input{Row: Row{Classes: []Class{ClassSource}}, Enabled: true,
		Project: Policy{TestPaths: []string{"pkg/gen/**"}}})
	c := quietClassifier(eff, BaseFacts{
		CILocalIncludes: []string{"ci/lint.yml", "templates/*.yml", "deploy/ci.yml", "keys/ci.pem", "odd/[x].yml"},
		Generated:       map[string]bool{"pkg/gen/zz_generated.go": true, "pkg/gen/api_test.go": true},
	})
	cases := []struct {
		path string
		want Class
	}{
		{"ci/lint.yml", ClassCI},
		{"templates/go.yml", ClassCI},
		{"templates/sub/go.yml", ClassOther},        // * does not cross a slash
		{"deploy/ci.yml", ClassCI},                  // an included file IS the CI definition
		{"keys/ci.pem", ClassSecret},                // but never out of the floor
		{"odd/[x].yml", ClassCI},                    // not a valid pattern: still CI, by its literal name
		{"pkg/gen/zz_generated.go", ClassGenerated}, // and test_paths cannot unlock it
		{"pkg/gen/api_test.go", ClassGenerated},     // generated outranks test
		{"pkg/gen/hand.go", ClassTest},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.path).Class; got != tc.want {
			t.Errorf("Classify(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

// Q3: "log every reclassification so the 'how it plays out' review has data".
// The classifier logs by construction; a caller cannot forget to.
func TestEveryReclassificationIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	eff := mustFold(t, Input{Row: Row{Classes: []Class{ClassSource, ClassTest}}, Enabled: true,
		Project: Policy{TestPaths: []string{"internal/testutil/**", "**/*.yaml"}}})
	c := eff.Classifier(BaseFacts{}, log)

	c.Classify("pkg/a.go") // not reclassified: silent
	if buf.Len() != 0 {
		t.Fatalf("an ordinary classification logged: %s", buf.String())
	}
	c.Classify("internal/testutil/fake.go")
	out := buf.String()
	for _, want := range []string{"scope: path reclassified", "path=internal/testutil/fake.go", "from=source", "to=test",
		"layer=project", "key=test_paths", "pattern=internal/testutil/**", "base_sha=" + testSHA} {
		if !strings.Contains(out, want) {
			t.Errorf("reclassification log lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	c.Classify("chart/values.yaml")
	out = buf.String()
	for _, want := range []string{"scope: reclassification refused", "path=chart/values.yaml", "from=deploy", "to=test", "key=test_paths"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal log lacks %q:\n%s", want, out)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "a/b/c.got", false},
		{"chart/**", "chart/a", true},
		{"chart/**", "chart/a/b", true},
		{"chart/**", "chart", false},
		{"chart/**", "charts/a", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c/zz", false},
		{"**", "a/b", true},
		{"*.md", "a.md", true},
		{"*.md", "a/b.md", false},
		{"**/Dockerfile*", "Dockerfile.agent", true},
		{"**/.env*", "x/.envrc", true},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"**/secrets/**", "x/secrets/y", true},
		{"**/secrets/**", "secrets", false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestValidatePattern(t *testing.T) {
	for _, ok := range []string{"a/**", "**/*.go", "internal/testutil/**", "a?b", "x"} {
		if err := ValidatePattern(ok); err != nil {
			t.Errorf("ValidatePattern(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs/**", "../x", "a/../b", "a//b", "a/", "./a", "a[bc]", `a\b`, "a\x00", "a/***/b", "a**", strings.Repeat("a", 300)} {
		if err := ValidatePattern(bad); err == nil {
			t.Errorf("ValidatePattern(%q) = nil, want an error", bad)
		}
	}
}

func TestCILocalIncludes(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
		err  bool
	}{
		{"none", "test:\n  script: [true]\n", nil, false},
		{"string", "include: /ci/a.yml\n", []string{"ci/a.yml"}, false},
		{"remote string", "include: https://example.invalid/a.yml\n", nil, false},
		{"list", "include:\n  - ci/a.yml\n  - local: /ci/b.yml\n  - project: g/p\n    file: /x.yml\n  - remote: https://example.invalid/c.yml\n  - template: Go.gitlab-ci.yml\n  - local: 'ci/*.yml'\n",
			[]string{"ci/a.yml", "ci/b.yml", "ci/*.yml"}, false},
		{"map", "include:\n  local: ci/a.yml\n", []string{"ci/a.yml"}, false},
		{"not yaml", "{{{{", nil, true},
		{"bad local", "include:\n  - local: ../../etc/x\n", nil, true},
		{"empty doc", "", nil, false},
	}
	for _, c := range cases {
		got, err := CILocalIncludes([]byte(c.yaml))
		if (err != nil) != c.err {
			t.Errorf("%s: err = %v, want err=%v", c.name, err, c.err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsGenerated(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{"// Code generated by protoc-gen-go. DO NOT EDIT.\npackage x\n", true},
		{"// Code generated by x. DO NOT EDIT.", true},
		{"package x\n// Code generated by x. DO NOT EDIT.\n", false}, // line 1 only
		{"// Code generated by x.\n", false},
		{"// code generated by x. DO NOT EDIT.\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsGenerated([]byte(c.content)); got != c.want {
			t.Errorf("IsGenerated(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}
