package effects

import "testing"

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
