package intake

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// Ecosystem profiles at onboarding (gonk-wj14; 066 section 11 item 5):
// the generated .gonk.yml pre-fills scope.profiles from marker files at the
// root of the DEFAULT branch, and the merge request says which were found and
// why. Onboarding proposes; a human merges or edits. Nothing detects at
// dispatch time.

// onboardedProfiles runs Onboard against a project holding files (path ->
// branch; "" is the default branch) and returns the scope.profiles the
// committed .gonk.yml carries and the merge request description.
func onboardedProfiles(t *testing.T, files map[string]string) ([]string, string) {
	t.Helper()
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	for path, branch := range files {
		if branch == "" {
			p.PutFile(path, []byte("x"))
		} else {
			p.PutFileOn(branch, path, []byte("x"))
		}
	}
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}
	raw, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536)
	if err != nil {
		t.Fatalf("no %s on the onboarding branch: %v", ConfigPath, err)
	}
	// The file gonk asks the maintainer to merge must be one gonk accepts.
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		t.Fatalf("the committed %s does not load: %v\n%s", ConfigPath, err, raw)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("%d merge requests, want 1", len(mrs))
	}
	// The body embeds the exact file it commits.
	if !strings.Contains(mrs[0].Description, string(raw)) {
		t.Fatalf("the merge request does not embed the committed %s byte for byte", ConfigPath)
	}
	return cfg.Scope.Profiles, mrs[0].Description
}

func TestOnboardPrefillsProfilesFromMarkerFiles(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    []string
		yes     []string // marker rows that must read "yes"
		because []string // "<profile> ... because <marker>"
	}{
		{"no marker: the default, said to be the default", map[string]string{"README.md": ""}, []string{"go", "docs-md"}, nil, nil},
		{"go.mod", map[string]string{"go.mod": ""}, []string{"go", "docs-md"}, []string{"go.mod"}, []string{"go/go.mod"}},
		{"pyproject.toml", map[string]string{"pyproject.toml": ""}, []string{"python", "docs-md"}, []string{"pyproject.toml"}, []string{"python/pyproject.toml"}},
		{"setup.py", map[string]string{"setup.py": ""}, []string{"python", "docs-md"}, []string{"setup.py"}, []string{"python/setup.py"}},
		{"requirements.txt", map[string]string{"requirements.txt": ""}, []string{"python", "docs-md"}, []string{"requirements.txt"}, []string{"python/requirements.txt"}},
		{"package.json", map[string]string{"package.json": ""}, []string{"node", "docs-md"}, []string{"package.json"}, []string{"node/package.json"}},
		{"a Go service with a web front end", map[string]string{"go.mod": "", "package.json": ""}, []string{"go", "node", "docs-md"},
			[]string{"go.mod", "package.json"}, []string{"go/go.mod", "node/package.json"}},
		{"all three", map[string]string{"go.mod": "", "setup.py": "", "requirements.txt": "", "package.json": ""}, []string{"go", "python", "node", "docs-md"},
			[]string{"go.mod", "setup.py", "requirements.txt", "package.json"}, []string{"go/go.mod", "python/setup.py", "node/package.json"}},

		// What the probe CANNOT see, each stated in the merge request text.
		{"a marker in a subdirectory is not seen", map[string]string{"web/package.json": "", "svc/go.mod": ""}, []string{"go", "docs-md"}, nil, nil},
		{"requirements-dev.txt is not requirements.txt", map[string]string{"requirements-dev.txt": ""}, []string{"go", "docs-md"}, nil, nil},
		// The DEFAULT branch is what is read: not a feature branch, and not
		// gonk's own onboarding branch left by a crashed attempt.
		{"a marker on another branch", map[string]string{"package.json": "feature/web"}, []string{"go", "docs-md"}, nil, nil},
		{"a marker on the onboarding branch only", map[string]string{"package.json": OnboardBranch}, []string{"go", "docs-md"}, nil, nil},
	}
	for _, c := range cases {
		got, body := onboardedProfiles(t, c.files)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: scope.profiles = %v, want %v", c.name, got, c.want)
		}
		if !strings.Contains(body, "pre-filled with `["+strings.Join(c.want, ", ")+"]`") {
			t.Errorf("%s: the merge request does not state the pre-filled value %v", c.name, c.want)
		}
		if !strings.Contains(body, "| `scope.profiles` | `["+strings.Join(c.want, ", ")+"]` |") {
			t.Errorf("%s: the key table does not show scope.profiles = %v", c.name, c.want)
		}
		// Every marker is listed as looked-for, with what was found.
		yes := map[string]bool{}
		for _, m := range c.yes {
			yes[m] = true
		}
		for _, pr := range scope.Profiles() {
			for _, m := range pr.Markers {
				row := fmt.Sprintf("| `%s` | no | `%s` |", m, pr.Name)
				if yes[m] {
					row = fmt.Sprintf("| `%s` | **yes** | `%s` |", m, pr.Name)
				}
				if !strings.Contains(body, row) {
					t.Errorf("%s: the merge request lacks the row %q", c.name, row)
				}
			}
		}
		for _, b := range c.because {
			profile, marker, _ := strings.Cut(b, "/")
			if want := fmt.Sprintf("- `%s` is proposed because `%s` exists.", profile, marker); !strings.Contains(body, want) {
				t.Errorf("%s: the merge request does not say %q", c.name, want)
			}
		}
		if n := strings.Count(body, " is proposed because "); n != len(c.because) {
			t.Errorf("%s: %d \"proposed because\" lines, want %d", c.name, n, len(c.because))
		}
		if none := strings.Contains(body, "None of them was found"); none != (len(c.because) == 0) {
			t.Errorf("%s: \"None of them was found\" present = %v with %d markers found", c.name, none, len(c.because))
		}
		for _, must := range []string{
			"at the root of\n`main`",                  // which branch was read
			"What this check cannot see",              // the limits
			"in a subdirectory",                       // ... named
			"`requirements-dev.txt`",                  // ... named
			"This is a proposal.",                     // a human disposes
			"gonk does not detect it again",           // nothing at dispatch time
			"the documentation style is not detected", // only when something was found; checked below
		} {
			if must == "the documentation style is not detected" && len(c.because) == 0 {
				continue
			}
			if !strings.Contains(body, must) {
				t.Errorf("%s: the merge request does not say %q", c.name, must)
			}
		}
	}
}

// A marker file larger than the probe's cap still exists: the over-cap answer
// is "present", not "absent".
func TestOnboardCountsAnOversizeMarkerAsPresent(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	p.PutFile("package.json", bytes.Repeat([]byte("x"), int(MaxOnboardFileBytes)+1))
	o := newOnboarder(gl)
	d, err := o.detectProfiles(context.Background(), p.Project, "main")
	if err != nil {
		t.Fatalf("detectProfiles = %v", err)
	}
	if !reflect.DeepEqual(d.Found, []string{"package.json"}) {
		t.Fatalf("Found = %v, want [package.json]", d.Found)
	}
	if got, _ := d.Selection(); !reflect.DeepEqual(got, []string{"node", "docs-md"}) {
		t.Fatalf("Selection = %v", got)
	}
	if !reflect.DeepEqual(d.Probed, scope.ProfileMarkers()) || d.Ref != "main" {
		t.Fatalf("detection = %+v", d)
	}
}

// A probe that cannot be ANSWERED is not "absent". Onboarding stops before it
// writes anything -- the merge request is opened exactly once, so it must not
// be opened on a guess -- and the next pass asks again.
func TestOnboardDoesNotGuessWhenAMarkerProbeFails(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	p.PutFile("pyproject.toml", []byte("x"))
	o := newOnboarder(gl)
	ctx := context.Background()

	gl.FailNext("GET", fmt.Sprintf("/api/v4/projects/%d/repository/files/pyproject.toml/raw", p.ID), 403, 1)
	err := o.Onboard(ctx, p.Project)
	if err == nil || !strings.Contains(err.Error(), "pyproject.toml") {
		t.Fatalf("Onboard = %v; want an error naming the marker it could not look for", err)
	}
	for _, r := range gl.Requests() {
		if strings.HasPrefix(r, "POST ") {
			t.Errorf("onboarding wrote (%s) after a marker probe failed", r)
		}
	}
	if mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"}); len(mrs) != 0 {
		t.Fatalf("a merge request was opened on a failed probe")
	}

	// The next pass: the probe answers, and the proposal is the right one.
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("second Onboard = %v", err)
	}
	raw, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Scope.Profiles, []string{"python", "docs-md"}) {
		t.Fatalf("scope.profiles = %v, want [python docs-md]", cfg.Scope.Profiles)
	}
}

// The render with no detection says so, and every selection onboarding can
// propose renders a config gonk accepts.
func TestRenderConfigAndTheUndetectedRender(t *testing.T) {
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "gonk did not read this repository's files") || strings.Contains(body, "| File | Found |") {
		t.Error("a render with no detection must say nothing was read, and show no marker table")
	}
	if !strings.Contains(body, "pre-filled with `[go, docs-md]`") {
		t.Error("a render with no detection must pre-fill the default")
	}

	def, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := RenderConfig([]string{"qwen-local"}, scope.DefaultProfiles())
	if err != nil || !bytes.Equal(def, same) {
		t.Fatalf("RenderDefaultConfig is not RenderConfig with the default selection (%v)", err)
	}
	for _, found := range [][]string{nil, {"go.mod"}, {"setup.py"}, {"package.json"}, {"go.mod", "pyproject.toml", "package.json"}} {
		sel, _ := scope.ProposeProfiles(found)
		raw, err := RenderConfig([]string{"qwen-local"}, sel)
		if err != nil {
			t.Fatalf("RenderConfig(%v): %v", sel, err)
		}
		cfg, err := gonkcfg.Load(raw)
		if err != nil {
			t.Fatalf("the config rendered for %v does not load: %v", sel, err)
		}
		if !reflect.DeepEqual(cfg.Scope.Profiles, sel) {
			t.Errorf("rendered scope.profiles = %v, want %v", cfg.Scope.Profiles, sel)
		}
		// The seed and the body are rendered from the same detection.
		c := OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"},
			Detection: ProfileDetection{Ref: "main", Probed: scope.ProfileMarkers(), Found: found}}
		b, err := RenderOnboardingMR(c)
		if err != nil || !strings.Contains(b, string(raw)) {
			t.Errorf("the merge request for %v does not embed the config rendered for it (%v)", found, err)
		}
	}
	for _, bad := range [][]string{{"rust"}, {"go", "go"}, {"**/*.py"}} {
		if _, err := RenderConfig([]string{"qwen-local"}, bad); err == nil {
			t.Errorf("RenderConfig rendered a .gonk.yml with profiles %v, which gonk would reject", bad)
		}
	}
	if _, err := RenderConfig(nil, scope.DefaultProfiles()); err == nil {
		t.Error("RenderConfig rendered an empty ladder")
	}
}

// "Nothing is detected at dispatch time: repository content never chooses its
// own scope." The marker files are read in exactly one place, onboarding, and
// the proposal is used in exactly two files of this package. Anything else in
// the repository that starts calling them is detection somewhere it must not
// be.
func TestProfilesAreDetectedOnlyAtOnboarding(t *testing.T) {
	allowed := map[string]bool{
		"pkg/intake/onboard.go ProfileMarkers": true, // detectProfiles: what to look for
		"pkg/intake/render.go ProposeProfiles": true, // ProfileDetection.Selection: what to pre-fill
		"pkg/intake/onboard.go detectProfiles": true, // its one call, in Onboard
	}
	seen := map[string]bool{}
	files := 0
	for _, root := range []string{"../../cmd", "../../internal", "../../pkg"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if strings.HasPrefix(rel, "pkg/scope/") {
				return nil // where they are defined
			}
			files++
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "ProposeProfiles", "ProfileMarkers", "detectProfiles":
					key := rel + " " + sel.Sel.Name
					seen[key] = true
					if !allowed[key] {
						t.Errorf("%s calls %s: ecosystem profiles are proposed ONCE, by onboarding, and never detected again (066 section 11 item 5)", rel, sel.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("expected %q; onboarding changed shape and this guard no longer describes it", key)
		}
	}
	if files < 100 {
		t.Fatalf("walked %d source files; the guard is looking in the wrong place", files)
	}
}
