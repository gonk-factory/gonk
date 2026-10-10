package effects

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// The per-dispatch lift (gonk-066 section 9 a/b; gonk-2usw plan 4.2).
//
// The denylist is a DEFAULT. A dispatch the controller chose -- the owner's
// example is a pipeline-fix session permitted to change a pipeline definition
// -- may carry a lift of the `ci` class. Nothing lifts the floor.

// ciLift builds a lift the only way one can be built: a controller folds the
// pack row with the layers and grants it.
func ciLift(t *testing.T) scope.Lift {
	t.Helper()
	eff, err := scope.Fold(scope.Input{
		Row:       scope.Row{Liftable: []scope.Class{scope.ClassCI}},
		BaseSHA:   "0123456789abcdef0123456789abcdef01234567",
		Enabled:   true,
		LiftOptIn: []scope.Class{scope.ClassCI},
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := eff.GrantLift(scope.Grant{Trigger: "pipeline-fix", Workflow: "configuration", Classes: []scope.Class{scope.ClassCI}})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func pathBatch(p string) Batch {
	// A synthetic path-carrying kind: the denylist is kind-agnostic, and the
	// kind that will carry a lift (`edit`) is another part's.
	return Batch{Effects: []Effect{{Kind: "edit", Path: p}}}
}

func TestProtectedPathsWithLift(t *testing.T) {
	lift := ciLift(t)
	cases := []struct {
		path       string
		noLift     bool // passes with no lift
		withCILift bool // passes with the ci lift
	}{
		// ci: refused by default, passes ONLY with the lift.
		{".gitlab-ci.yml", false, true},
		{".gitlab-ci.yaml", false, true},
		{".gitlab/ci/build.yml", false, true},
		{".github/workflows/deploy.yml", false, true},
		// The floor: refused with or without it.
		{".gonk.yml", false, false},
		{"sub/.gonk.yml", false, false},
		{"CODEOWNERS", false, false},
		{"docs/CODEOWNERS", false, false},
		{".gitlab/CODEOWNERS", false, false}, // under a ci directory, still control
		{".github/CODEOWNERS", false, false},
		{".gitlab/.gonk.yml", false, false},
		{".git/config", false, false},
		{".git/hooks/pre-commit", false, false},
		// Secret-class files under a ci directory: the lift does not reach them.
		{".gitlab/deploy.key", false, false},
		{".github/secrets/token.yml", false, false},
		{".gitlab/ci/.env", false, false},
		// A lift is for the path as written, not for what it cleans to.
		{"./.gitlab-ci.yml", false, false},
		{"a/../.gitlab-ci.yml", false, false},
		{".gitlab//ci.yml", false, false},
		// Ordinary content: never this gate's business.
		{"src/main.go", true, true},
		{".agent/README.md", true, true},
		{"docs/gitlab-ci-explained.md", true, true},
		{"sub/.gitlab-ci.yml", true, true},
	}
	for _, c := range cases {
		if err := ValidateProtectedPathsWithLift(pathBatch(c.path), scope.Lift{}); (err == nil) != c.noLift {
			t.Errorf("no lift: %q -> %v, want pass=%v", c.path, err, c.noLift)
		}
		if err := ValidateProtectedPaths(pathBatch(c.path)); (err == nil) != c.noLift {
			t.Errorf("ValidateProtectedPaths: %q -> %v, want pass=%v", c.path, err, c.noLift)
		}
		if err := ValidateProtectedPathsWithLift(pathBatch(c.path), lift); (err == nil) != c.withCILift {
			t.Errorf("ci lift: %q -> %v, want pass=%v", c.path, err, c.withCILift)
		}
	}
}

// One floor path anywhere in a lifted batch refuses the whole batch.
func TestLiftDoesNotCarryAFloorPathInTheSameBatch(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: "edit", Path: ".gitlab-ci.yml"}, {Kind: "edit", Path: ".gonk.yml"}}}
	err := ValidateProtectedPathsWithLift(b, ciLift(t))
	if err == nil || !strings.Contains(err.Error(), "effect 1") || !strings.Contains(err.Error(), ".gonk.yml") {
		t.Fatalf("err = %v, want effect 1 (.gonk.yml) refused", err)
	}
}

// With no lift, the refusal text is exactly what it was before a lift existed.
// These strings are the pre-change output, copied; a change to them is a
// change to what triage, scaffold and pipeline sessions are told.
func TestNoLiftRefusalsAreByteIdentical(t *testing.T) {
	want := map[string]string{
		".gitlab-ci.yml":     `effects: effect 0: path ".gitlab-ci.yml" is a CI definition and is protected -- no agent may propose changes to it`,
		".gonk.yml":          `effects: effect 0: path ".gonk.yml" is a repository control file and is protected -- no agent may propose changes to it`,
		"docs/CODEOWNERS":    `effects: effect 0: path "docs/CODEOWNERS" is a repository control file and is protected -- no agent may propose changes to it`,
		".gitlab/CODEOWNERS": `effects: effect 0: path ".gitlab/CODEOWNERS" is a repository control file and is protected -- no agent may propose changes to it`,
		".gitlab/x.yml":      `effects: effect 0: path ".gitlab/x.yml" is a protected directory and is protected -- no agent may propose changes to it`,
		".github/x.yml":      `effects: effect 0: path ".github/x.yml" is a protected directory and is protected -- no agent may propose changes to it`,
		".git/config":        `effects: effect 0: path ".git/config" is a protected directory and is protected -- no agent may propose changes to it`,
		"./.gitlab-ci.yml":   `effects: effect 0: path "./.gitlab-ci.yml" is a CI definition and is protected -- no agent may propose changes to it`,
	}
	for p, msg := range want {
		for name, err := range map[string]error{
			"ValidateProtectedPaths":         ValidateProtectedPaths(fileEffect(p)),
			"ValidateProtectedPathsWithLift": ValidateProtectedPathsWithLift(fileEffect(p), scope.Lift{}),
			"ValidatePaths":                  ValidatePaths(fileEffect(p)),
		} {
			if err == nil || err.Error() != msg {
				t.Errorf("%s(%q) = %v\nwant %s", name, p, err, msg)
			}
		}
	}
}

// ValidatePaths -- the gate the broker calls for the agents that exist -- takes
// no lift and has no way to be given one.
func TestValidatePathsTakesNoLift(t *testing.T) {
	ft := reflect.TypeOf(ValidatePaths)
	if ft.NumIn() != 1 || ft.In(0) != reflect.TypeOf(Batch{}) {
		t.Fatalf("ValidatePaths has signature %s; it must take the batch and nothing else", ft)
	}
	ft = reflect.TypeOf(ValidateProtectedPaths)
	if ft.NumIn() != 1 || ft.In(0) != reflect.TypeOf(Batch{}) {
		t.Fatalf("ValidateProtectedPaths has signature %s; it must take the batch and nothing else", ft)
	}
}

// The lift covers every path-carrying field the denylist covers, and the floor
// holds on every one of them.
func TestLiftAppliesToEveryWritePathField(t *testing.T) {
	lift := ciLift(t)
	rt := reflect.TypeOf(Effect{})
	found := 0
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := strings.ToLower(f.Name)
		if !strings.Contains(name, "path") && !strings.Contains(name, "file") {
			continue
		}
		if _, cited := citationOnlyPathFields[f.Name]; cited {
			continue
		}
		found++
		set := func(p string) Batch {
			var e Effect
			fv := reflect.ValueOf(&e).Elem().Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				fv.SetString(p)
			case reflect.Slice:
				fv.Set(reflect.ValueOf([]string{p}))
			default:
				t.Fatalf("field %s: unsupported path field type %s; extend this test", f.Name, f.Type)
			}
			return Batch{Effects: []Effect{e}}
		}
		if err := ValidateProtectedPathsWithLift(set(".gonk.yml"), lift); err == nil {
			t.Errorf("Effect.%s: .gonk.yml passed WITH a lift", f.Name)
		}
		if err := ValidateProtectedPathsWithLift(set(".gitlab-ci.yml"), scope.Lift{}); err == nil {
			t.Errorf("Effect.%s: a ci path passed WITHOUT a lift", f.Name)
		}
		if err := ValidateProtectedPathsWithLift(set(".gitlab-ci.yml"), lift); err != nil {
			t.Errorf("Effect.%s: a ci path was refused with the ci lift: %v", f.Name, err)
		}
	}
	if found == 0 {
		t.Fatal("no write-path field found on Effect; this guard is vacuous")
	}
}

// pkg/scope's `control` and `ci` rows and this package's floor constants are
// two statements of one list. They must agree on every path, or a lift could
// be judged against a different floor than the one that is enforced.
func TestFloorAgreesWithScopeClassifier(t *testing.T) {
	segs := []string{".git", ".gitlab", ".github", ".gonk.yml", "CODEOWNERS", ".gitlab-ci.yml", ".gitlab-ci.yaml",
		"src", "docs", "ci", "a.go", "x.yml", ".gitignore", ".agent", "codeowners", ".gonk.yaml", "deploy.key", "secrets", ".env"}
	paths := []string{".gitlab-ci.yml", ".gitlab-ci.yaml", ".gonk.yml", "CODEOWNERS", ".git/config", ".gitlab/x", ".github/x",
		"src/github/client.go", ".gitignore", ".gitlab", ".github", ".git", "x/.git/config", "x/.gitlab/ci.yml"}
	r := rand.New(rand.NewSource(66))
	for i := 0; i < 5000; i++ {
		n := 1 + r.Intn(4)
		parts := make([]string, n)
		for j := range parts {
			parts[j] = segs[r.Intn(len(segs))]
		}
		paths = append(paths, strings.Join(parts, "/"))
	}
	hits := map[scope.Class]int{}
	for _, p := range paths {
		floor := floorClass(p)
		built := scope.Builtin(p)
		hits[floor]++
		var want scope.Class // what the floor should say, given scope's class
		switch built {
		case scope.ClassControl:
			want = scope.ClassControl
		case scope.ClassCI:
			want = scope.ClassCI
		case scope.ClassSecret:
			// scope ranks secret above ci; the floor constants know nothing of
			// secret, so a secret-class path is either unprotected here or sits
			// under a ci directory. Either way a lift must not open it.
			if floor != "" && floor != scope.ClassCI {
				t.Errorf("%q: floor says %s, scope says secret", p, floor)
			}
			if err := ValidateProtectedPathsWithLift(pathBatch(p), ciLift(t)); floor != "" && err == nil {
				t.Errorf("%q: secret-class path under the ci floor passed with a ci lift", p)
			}
			continue
		}
		if floor != want {
			t.Errorf("%q: pkg/effects floor says %q, pkg/scope says %s", p, floor, built)
		}
	}
	if hits[scope.ClassControl] < 100 || hits[scope.ClassCI] < 100 || hits[""] < 100 {
		t.Fatalf("generator did not exercise all three outcomes: %v", hits)
	}
}

// ---- the contract: a lift never comes from the batch ----

// A batch has no field that could carry a lift or any other scope policy, at
// any depth, and ParseBatch refuses a key that tries.
func TestBatchCannotCarryALiftOrScopePolicy(t *testing.T) {
	liftType := reflect.TypeOf(scope.Lift{})
	var walk func(rt reflect.Type, where string, seen map[reflect.Type]bool)
	fields := 0
	walk = func(rt reflect.Type, where string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.PkgPath() == liftType.PkgPath() {
			t.Errorf("%s has type %s: a batch must carry nothing from pkg/scope", where, rt)
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			fields++
			name := strings.ToLower(f.Name + " " + f.Tag.Get("json"))
			for _, banned := range []string{"lift", "scope", "protected", "denylist", "allow"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s (json %q) looks like scope policy; policy never rides in a batch", where, f.Name, f.Tag.Get("json"))
				}
			}
			walk(f.Type, where+"."+f.Name, seen)
		}
	}
	walk(reflect.TypeOf(Batch{}), "Batch", map[reflect.Type]bool{})
	if fields < 10 {
		t.Fatalf("walked %d fields of Batch; the guard is vacuous", fields)
	}

	for _, key := range []string{"lift", "scope", "protected_paths", "lifted", "allow_ci", "classes"} {
		for _, doc := range []string{
			fmt.Sprintf(`{"effects":[{"kind":"comment","body":"x"}],"verdict":"reply-only",%q:["ci"]}`, key),
			fmt.Sprintf(`{"effects":[{"kind":"comment","body":"x",%q:["ci"]}],"verdict":"reply-only"}`, key),
		} {
			if _, err := ParseBatch([]byte(doc)); err == nil {
				t.Errorf("ParseBatch accepted a batch carrying %q: %s", key, doc)
			}
		}
	}
	// Not vacuous: the same batch without the key parses.
	if _, err := ParseBatch([]byte(`{"effects":[{"kind":"comment","body":"x"}],"verdict":"reply-only"}`)); err != nil {
		t.Fatalf("the control batch does not parse: %v", err)
	}
}
