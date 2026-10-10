package scope

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writePack(t *testing.T, agent, body string) string {
	t.Helper()
	dir := t.TempDir()
	adir := filepath.Join(dir, "agents", agent)
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adir, "effect-shape.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const fullRow = `
[edit]
min = 1
max = 10

[scope]
classes = ["source", "test", "docs"]
liftable = ["ci"]
max_files = 3
max_lines = 40
max_deletes = 0
max_file_bytes = 262144
max_patch_bytes = 16384

[trajectory]
read_tools = ["read"]
`

func TestLoadRow(t *testing.T) {
	row, err := LoadRow(writePack(t, "coder", fullRow), "coder")
	if err != nil {
		t.Fatal(err)
	}
	want := Row{Classes: []Class{ClassSource, ClassTest, ClassDocs}, Liftable: []Class{ClassCI},
		Caps: Caps{MaxFiles: 3, MaxLines: 40, MaxDeletes: 0, MaxFileBytes: 262144, MaxPatchBytes: 16384}}
	if !reflect.DeepEqual(row, want) {
		t.Errorf("row = %+v, want %+v", row, want)
	}
}

// No row is the row that allows nothing: every agent that exists today.
func TestLoadRowAbsentIsTheEmptyRow(t *testing.T) {
	for name, dir := range map[string]string{
		"no [scope] table": writePack(t, "triage", "[comment]\nmin = 1\nmax = 1\n"),
		"no file":          t.TempDir(),
		"empty table":      writePack(t, "triage", "[scope]\n"),
	} {
		row, err := LoadRow(dir, "triage")
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(row, Row{}) {
			t.Errorf("%s: row = %+v, want the zero Row", name, row)
		}
		eff := mustFold(t, Input{Row: row, Enabled: true, LiftOptIn: []Class{ClassCI}})
		for _, c := range Classes() {
			if eff.Allows(c, Lift{}) {
				t.Errorf("%s: the empty row allows %s", name, c)
			}
		}
		if len(eff.Liftable()) != 0 {
			t.Errorf("%s: the empty row lets a dispatch lift %v", name, eff.Liftable())
		}
	}
}

// rowAgents are the agents ACKNOWLEDGED to have a pack row, with the row each
// must have. pipefix (gonk-2usw part AGENT, plan 4.2 / 4.3) is the first and
// only one: the workflow that proposes repository edits.
var rowAgents = map[string]Row{
	"pipefix": {
		Classes:  []Class{ClassSource, ClassDocs, ClassTest},
		Liftable: []Class{ClassCI},
		Caps:     Caps{MaxFiles: 3, MaxLines: 40, MaxDeletes: 0, MaxFileBytes: 256 << 10, MaxPatchBytes: 16 << 10},
	},
}

// The real baked pack: no agent has a row except the acknowledged ones, so
// none of the others can be given a class or a lift by this package.
func TestRealPackAgentsHaveNoRow(t *testing.T) {
	packDir := filepath.Join("..", "..", "pack")
	entries, err := os.ReadDir(filepath.Join(packDir, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	seen, acknowledged := 0, 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		row, err := LoadRow(packDir, e.Name())
		if err != nil {
			t.Errorf("LoadRow(%s): %v", e.Name(), err)
			continue
		}
		// An agent that gains a [scope] row is a deliberate change; this list is
		// where it is acknowledged.
		if want, ok := rowAgents[e.Name()]; ok {
			acknowledged++
			if !reflect.DeepEqual(row, want) {
				t.Errorf("agent %s has pack scope row %+v, acknowledged as %+v", e.Name(), row, want)
			}
			continue
		}
		if !reflect.DeepEqual(row, Row{}) {
			t.Errorf("agent %s now has a pack scope row %+v; acknowledge it here", e.Name(), row)
		}
	}
	if acknowledged != len(rowAgents) {
		t.Errorf("%d of the %d acknowledged row agents exist in the pack; an acknowledgement outlived its agent", acknowledged, len(rowAgents))
	}
	if seen < 3 {
		t.Fatalf("found %d pack agents; this guard is looking in the wrong place", seen)
	}
}

func TestLoadRowRefuses(t *testing.T) {
	caps := "max_files = 1\nmax_lines = 1\nmax_deletes = 0\nmax_file_bytes = 1\nmax_patch_bytes = 1\n"
	cases := map[string]struct{ body, want string }{
		"unknown key":             {"[scope]\nclasses = [\"source\"]\nmax_file = 3\n" + caps, "unknown key"},
		"unknown nested key":      {"[scope]\nclasses = [\"source\"]\n" + caps + "[scope.caps]\nmax_files = 9\n", "unknown key"},
		"unknown class":           {"[scope]\nclasses = [\"sauce\"]\n" + caps, "unknown class"},
		"protected class":         {"[scope]\nclasses = [\"ci\"]\n" + caps, "protected"},
		"control in classes":      {"[scope]\nclasses = [\"control\"]\n" + caps, "protected"},
		"floor in liftable":       {"[scope]\nliftable = [\"control\"]\n" + caps, "not liftable"},
		"secret in liftable":      {"[scope]\nliftable = [\"secret\"]\n" + caps, "not liftable"},
		"source in liftable":      {"[scope]\nliftable = [\"source\"]\n" + caps, "not liftable"},
		"a cap missing":           {"[scope]\nclasses = [\"source\"]\nmax_files = 1\nmax_lines = 1\nmax_file_bytes = 1\nmax_patch_bytes = 1\n", "max_deletes"},
		"a cap missing with lift": {"[scope]\nliftable = [\"ci\"]\nmax_files = 1\n", "max_lines"},
		"negative cap":            {"[scope]\nclasses = [\"source\"]\nmax_files = -1\nmax_lines = 1\nmax_deletes = 0\nmax_file_bytes = 1\nmax_patch_bytes = 1\n", "negative"},
		"not toml":                {"[scope\n", "load pack row"},
		"wrong type":              {"[scope]\nclasses = \"source\"\n" + caps, "load pack row"},
	}
	for name, c := range cases {
		_, err := LoadRow(writePack(t, "coder", c.body), "coder")
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}
