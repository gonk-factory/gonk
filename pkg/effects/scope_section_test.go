package effects

import "testing"

// [scope] is the second non-kind section (gonk-066 S3): the agent's pack row,
// read by pkg/scope. LoadShape skips it by name, exactly as it skips
// [trajectory], and still fails loud on any OTHER unknown table.
func TestLoadShapeSkipsTheScopeSection(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "agents/coder/effect-shape.toml", `
[comment]
min = 1
max = 1
[scope]
classes = ["source"]
max_files = 3
max_lines = 40
max_deletes = 0
max_file_bytes = 1024
max_patch_bytes = 1024
`)
	s, err := LoadShape(dir, "coder")
	if err != nil {
		t.Fatalf("LoadShape with a [scope] section: %v", err)
	}
	if len(s.Kinds) != 1 || s.Kinds[KindComment] != (Card{1, 1}) {
		t.Fatalf("kinds = %+v, want only comment {1,1}", s.Kinds)
	}
	if _, isKind := s.Kinds[Kind(ScopeSection)]; isKind {
		t.Fatal("[scope] was loaded as an effect kind")
	}
	writeFixture(t, dir, "agents/typo/effect-shape.toml", "[comment]\nmin = 1\nmax = 1\n[scopes]\nclasses = []\n")
	if _, err := LoadShape(dir, "typo"); err == nil {
		t.Fatal("a typo'd [scopes] section was skipped; only [scope] and [trajectory] are")
	}
}
