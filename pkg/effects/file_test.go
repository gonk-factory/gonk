package effects

import (
	"strings"
	"testing"
)

// The `file` effect is how the scaffold agent proposes repository content
// without holding forge credentials: it returns paths and bytes, and the broker
// commits them. That makes ValidatePaths a SECURITY boundary, not a tidy-up --
// the content and the paths are model output derived from an untrusted
// repository, and the broker writes them into a real branch.

func fileBatch(paths ...string) Batch {
	b := Batch{}
	for _, p := range paths {
		b.Effects = append(b.Effects, Effect{Kind: KindFile, Path: p, Content: "x"})
	}
	return b
}

func TestFileEffectParses(t *testing.T) {
	b, err := ParseBatch([]byte(`{"effects":[{"kind":"file","path":".agent/README.md","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("ParseBatch = %v", err)
	}
	if len(b.Effects) != 1 || b.Effects[0].Kind != KindFile {
		t.Fatalf("effects = %+v", b.Effects)
	}
	if b.Effects[0].Path != ".agent/README.md" || b.Effects[0].Content != "hello" {
		t.Fatalf("path/content not decoded: %+v", b.Effects[0])
	}
}

// Every path must land under .agent/. The scaffold agent's job is that
// directory and nothing else; a batch that writes elsewhere is not a scaffold,
// it is an arbitrary commit into someone's repository.
func TestValidatePathsRejectsAnythingOutsideAgentDir(t *testing.T) {
	for _, p := range []string{
		"README.md",                    // repo root
		".gonk.yml",                    // gonk's OWN config -- would let a run rewrite its budget
		".gitlab-ci.yml",               // CI -- would let a run execute arbitrary code
		".agent/../.gitlab-ci.yml",     // traversal out and back
		"../outside.md",                // traversal above the repo
		"/etc/passwd",                  // absolute
		".agentx/thing.md",             // prefix confusion: .agent is not a prefix match
		".github/workflows/deploy.yml", // another CI surface
	} {
		if err := ValidatePaths(fileBatch(p)); err == nil {
			t.Errorf("ValidatePaths(%q) = nil, want an error", p)
		}
	}
}

func TestValidatePathsAcceptsAgentDirContent(t *testing.T) {
	ok := fileBatch(".agent/README.md", ".agent/build/testing.md", ".agent/conventions.md")
	if err := ValidatePaths(ok); err != nil {
		t.Fatalf("ValidatePaths = %v, want nil", err)
	}
}

// A duplicate path means the batch disagrees with itself about a file's
// content. Applying it would silently pick one, so refuse the whole batch.
func TestValidatePathsRejectsDuplicates(t *testing.T) {
	if err := ValidatePaths(fileBatch(".agent/a.md", ".agent/a.md")); err == nil {
		t.Fatal("duplicate path: err = nil, want an error")
	}
}

func TestValidatePathsRejectsEmptyPath(t *testing.T) {
	if err := ValidatePaths(Batch{Effects: []Effect{{Kind: KindFile, Content: "x"}}}); err == nil {
		t.Fatal("empty path: err = nil, want an error")
	}
}

// Size is capped for the same reason the injected issue body is: this is
// unbounded model output, and it is going into a commit.
func TestValidatePathsCapsContentSize(t *testing.T) {
	big := Batch{Effects: []Effect{{
		Kind: KindFile, Path: ".agent/big.md", Content: strings.Repeat("a", MaxFileBytes+1),
	}}}
	if err := ValidatePaths(big); err == nil {
		t.Fatalf("oversize content: err = nil, want an error")
	}
	atCap := Batch{Effects: []Effect{{
		Kind: KindFile, Path: ".agent/big.md", Content: strings.Repeat("a", MaxFileBytes),
	}}}
	if err := ValidatePaths(atCap); err != nil {
		t.Fatalf("content exactly at the cap = %v, want nil", err)
	}
}

// Total size is capped too: many small files are the same problem as one big
// one, and the per-file cap alone does not bound a batch.
func TestValidatePathsCapsTotalSize(t *testing.T) {
	var b Batch
	for i := 0; i < (MaxBatchBytes/MaxFileBytes)+2; i++ {
		b.Effects = append(b.Effects, Effect{
			Kind:    KindFile,
			Path:    ".agent/f" + string(rune('a'+i)) + ".md",
			Content: strings.Repeat("a", MaxFileBytes),
		})
	}
	if err := ValidatePaths(b); err == nil {
		t.Fatal("oversize batch: err = nil, want an error")
	}
}

// ValidatePaths only speaks about file effects; a triage batch passes through
// it untouched so the broker can call it unconditionally.
func TestValidatePathsIgnoresNonFileEffects(t *testing.T) {
	b := Batch{Effects: []Effect{
		{Kind: KindComment, Body: "hi"},
		{Kind: KindLabel, Add: []string{"gonk::bug"}},
	}}
	if err := ValidatePaths(b); err != nil {
		t.Fatalf("ValidatePaths on a triage batch = %v, want nil", err)
	}
}
