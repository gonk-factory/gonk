package trace

import (
	"path/filepath"
	"runtime"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
)

func packDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "pack")
}

// The shipped triage shape must actually declare the predicate this slice
// exists for -- otherwise the gate is wired to nothing and every batch passes.
func TestShippedTriagePolicyRequiresAReadForArtifactlessVerdicts(t *testing.T) {
	p, err := LoadPolicy(packDir(t), "triage")
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Empty() {
		t.Fatal("triage declares no trajectory predicates; the gate would be wired to nothing")
	}
	for _, v := range []string{"reply-only", "close"} {
		if !contains(p.RequireTargetReadFor, v) {
			t.Fatalf("verdict %q is not gated on a read; it produces no artifact and nothing else can catch it", v)
		}
	}
	// code-change must NOT be here: it produces a diff the verify pipeline can
	// judge on outcome evidence, which is the stronger instrument.
	if contains(p.RequireTargetReadFor, "code-change") {
		t.Fatal("code-change should not be gated on process evidence; verify judges it on outcome evidence")
	}
	if len(p.ReadTools) == 0 {
		t.Fatal("no read tools declared, so no call could ever satisfy the predicate")
	}
}

// The two loaders must coexist on one file: adding [trajectory] must not make
// the shape loader treat it as an unknown effect kind, and the shape must still
// parse exactly as before.
func TestShapeAndPolicyLoadFromTheSameFile(t *testing.T) {
	dir := packDir(t)
	shape, err := effects.LoadShape(dir, "triage")
	if err != nil {
		t.Fatalf("LoadShape rejected the file after [trajectory] was added: %v", err)
	}
	if len(shape.Kinds) == 0 {
		t.Fatal("shape lost its kinds")
	}
	if _, ok := shape.Kinds[effects.KindComment]; !ok {
		t.Fatal("comment cardinality disappeared from the shape")
	}
	if _, err := LoadPolicy(dir, "triage"); err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
}

// A typo'd effect kind must STILL be rejected. Carving out [trajectory] must
// not have loosened the check that catches a misspelled kind.
func TestATypoedKindIsStillRejected(t *testing.T) {
	dir := t.TempDir()
	agent := filepath.Join(dir, "agents", "typo")
	if err := mkdirAll(agent); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agent, "effect-shape.toml"), "[commnet]\nmin = 1\nmax = 1\n")
	if _, err := effects.LoadShape(dir, "typo"); err == nil {
		t.Fatal("a misspelled kind was accepted; the carve-out loosened the wrong thing")
	}
}

// An agent with no trajectory section declares no requirements, which must be
// no-policy rather than an error -- otherwise adding trajectory to one agent
// breaks every other.
func TestAnAgentWithNoTrajectorySectionHasAnEmptyPolicy(t *testing.T) {
	p, err := LoadPolicy(packDir(t), "scaffold")
	if err != nil {
		t.Fatalf("LoadPolicy on an agent without [trajectory]: %v", err)
	}
	if !p.Empty() {
		t.Fatalf("expected an empty policy, got %+v", p)
	}
}

// A missing pack directory must not error either: it means no requirements.
func TestAMissingShapeFileIsNotAnError(t *testing.T) {
	p, err := LoadPolicy(t.TempDir(), "nope")
	if err != nil {
		t.Fatalf("a missing file must mean 'no requirements', got %v", err)
	}
	if !p.Empty() {
		t.Fatal("expected an empty policy")
	}
}
