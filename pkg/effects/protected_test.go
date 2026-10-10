package effects

import (
	"reflect"
	"strings"
	"testing"
)

// The denylist exists for the agent that does NOT exist yet.
//
// Scaffold is safe today because its allowlist is a single prefix (.agent/),
// which happens to exclude everything dangerous. The first agent that
// legitimately edits source code needs a much wider allowlist, and on that day
// the incidental protection disappears silently. These paths must be refused on
// their own account, whatever an agent's shape says.

func fileEffect(p string) Batch {
	return Batch{Effects: []Effect{{Kind: KindFile, Path: p, Content: "x"}}}
}

func TestProtectedPathsAreRefusedOnTheirOwnAccount(t *testing.T) {
	for _, p := range []string{
		// CI definitions: arbitrary code execution wearing a commit, and the
		// specific way an agent could make a red pipeline green without fixing
		// anything.
		".gitlab-ci.yml",
		".gitlab/agents/thing.yml",
		".github/workflows/deploy.yml",
		// gonk's own config: a run that edits this rewrites its ladder, its
		// budget and its own permission to act.
		".gonk.yml",
		// Review controls: editing these changes who has to approve the change.
		"CODEOWNERS",
		".gitlab/CODEOWNERS",
		"docs/CODEOWNERS",
		// The repository's own machinery.
		".git/config",
	} {
		if err := ValidateProtectedPaths(fileEffect(p)); err == nil {
			t.Errorf("ValidateProtectedPaths(%q) = nil, want a refusal", p)
		}
	}
}

// It must not over-reach: ordinary content that merely resembles a protected
// path is fine, or the gate becomes something people route around.
func TestProtectedPathsAllowOrdinaryContent(t *testing.T) {
	for _, p := range []string{
		".agent/README.md",
		".agent/ci-notes.md",           // ABOUT ci, not a CI definition
		"docs/gitlab-ci-explained.md",  // ditto
		"src/github/client.go",         // .github is the directory, github is not
		"internal/codeowners/parse.go", // lowercase, a source file
		".agent/gonk.yml.example",      // not .gonk.yml
	} {
		if err := ValidateProtectedPaths(fileEffect(p)); err != nil {
			t.Errorf("ValidateProtectedPaths(%q) = %v, want nil", p, err)
		}
	}
}

// The denylist is checked independently of any allowlist, so a future agent
// whose shape permits wide paths still cannot reach these.
func TestProtectedPathsApplyRegardlessOfAllowlist(t *testing.T) {
	// A batch that would pass a hypothetical "anything under the repo" allowlist.
	b := Batch{Effects: []Effect{
		{Kind: KindFile, Path: "src/main.go", Content: "package main"},
		{Kind: KindFile, Path: ".gitlab-ci.yml", Content: "test: {script: true}"},
	}}
	if err := ValidateProtectedPaths(b); err == nil {
		t.Fatal("a protected path passed because the rest of the batch was ordinary")
	}
}

// ValidatePaths is the gate the broker actually calls; the denylist must be
// inside it, or adding it changes nothing.
func TestValidatePathsEnforcesTheDenylist(t *testing.T) {
	// Reachable only if some future allowlist admits it -- assert the denylist
	// fires from ValidatePaths itself rather than trusting the prefix check.
	if err := ValidatePaths(fileEffect(".gitlab-ci.yml")); err == nil {
		t.Fatal("ValidatePaths let a protected path through")
	}
}

func TestProtectedPathsIgnoreNonFileEffects(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "mentions .gitlab-ci.yml"}}}
	if err := ValidateProtectedPaths(b); err != nil {
		t.Fatalf("a comment mentioning a protected path = %v, want nil", err)
	}
}

// A non-file effect that names a path must not escape the denylist.
func TestProtectedPathsCoverNonFileKinds(t *testing.T) {
	for k := range knownKinds {
		b := Batch{Effects: []Effect{{Kind: k, Path: ".gitlab-ci.yml"}}}
		if err := ValidateProtectedPaths(b); err == nil {
			t.Errorf("kind %q carrying .gitlab-ci.yml was not refused", k)
		}
	}
	// A synthetic kind the package has never heard of is covered too.
	b := Batch{Effects: []Effect{{Kind: "patch", Path: ".gitlab/ci/x.yml"}}}
	if err := ValidateProtectedPaths(b); err == nil {
		t.Error("synthetic path-carrying kind bypassed the denylist")
	}
	if err := ValidatePaths(Batch{Effects: []Effect{{Kind: KindComment, Body: "x", Path: ".gonk.yml"}}}); err == nil {
		t.Error("ValidatePaths must refuse a comment effect naming .gonk.yml")
	}
}

// Fails when a path-carrying field is added to Effect without being EITHER
// enumerated by effectPaths (so the denylist checks it) OR listed, with a
// reason, in citationOnlyPathFields (a path gonk only cites and never writes).
// Every string / []string field whose name contains "Path" or "File" is set to
// a protected path in isolation: an unlisted one must be refused. "File"
// because a list of paths is naturally called Files and a "path"-only match
// would wave it through silently.
func TestEveryPathFieldIsProtected(t *testing.T) {
	rt := reflect.TypeOf(Effect{})
	found := 0
	fields := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		fields[f.Name] = true
		name := strings.ToLower(f.Name)
		if !strings.Contains(name, "path") && !strings.Contains(name, "file") {
			continue
		}
		found++
		if reason, ok := citationOnlyPathFields[f.Name]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("Effect.%s is citation-only with no reason; say why no apply path writes it", f.Name)
			}
			continue
		}
		var e Effect
		fv := reflect.ValueOf(&e).Elem().Field(i)
		switch f.Type.Kind() {
		case reflect.String:
			fv.SetString(".gitlab-ci.yml")
		case reflect.Slice:
			if f.Type.Elem().Kind() != reflect.String {
				t.Fatalf("field %s: unsupported path field type %s; extend this test", f.Name, f.Type)
			}
			fv.Set(reflect.ValueOf([]string{".gitlab-ci.yml"}))
		default:
			t.Fatalf("field %s: unsupported path field type %s; extend this test", f.Name, f.Type)
		}
		if err := ValidateProtectedPaths(Batch{Effects: []Effect{e}}); err == nil {
			t.Errorf("Effect.%s names a path but ValidateProtectedPaths does not check it -- add it to effectPaths, or to citationOnlyPathFields with the reason no apply path writes it", f.Name)
		}
	}
	if found == 0 {
		t.Fatal("no path-named field found on Effect; this guard is vacuous")
	}
	// A stale allowlist entry is a hole waiting for a field of that name.
	for name := range citationOnlyPathFields {
		if !fields[name] {
			t.Errorf("citationOnlyPathFields names %q, which is not a field of Effect", name)
		}
	}
}
