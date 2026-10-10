package effects

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixture(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func TestLoadShapeTriage(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "agents/triage/effect-shape.toml", `
[comment]
min = 1
max = 1
[label]
min = 0
max = -1   # -1 => unbounded (N)
`)
	s, err := LoadShape(dir, "triage")
	if err != nil {
		t.Fatal(err)
	}
	if s.Kinds[KindComment] != (Card{1, 1}) {
		t.Fatalf("comment card %+v", s.Kinds[KindComment])
	}
	if s.Kinds[KindLabel].Max != N {
		t.Fatalf("label max %d want N", s.Kinds[KindLabel].Max)
	}
}

func TestLoadShapeRejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "agents/triage/effect-shape.toml", `
[nuke]
min = 1
max = 1
`)
	if _, err := LoadShape(dir, "triage"); err == nil {
		t.Fatal("want error for unknown kind key")
	}
}

// The real baked triage shape must load and yield comment {1,1}, label {0,N}.
func TestLoadShapeRealTriagePack(t *testing.T) {
	packDir := filepath.Join("..", "..", "pack")
	s, err := LoadShape(packDir, "triage")
	if err != nil {
		t.Fatalf("LoadShape real pack: %v", err)
	}
	if s.Kinds[KindComment] != (Card{1, 1}) {
		t.Fatalf("comment card %+v want {1,1}", s.Kinds[KindComment])
	}
	if got := s.Kinds[KindLabel]; got.Min != 0 || got.Max != N {
		t.Fatalf("label card %+v want {0,N}", got)
	}
}
