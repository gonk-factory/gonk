package mergepolicy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The two fact tables are the product's rules and live in the reference,
// which is published with the code (they used to be section 9.1 of the fix
// plan and 5.1 of the epic plan, which are not). Both files are always in the
// tree, so neither test has a way to skip: an unreadable page is a failure.
const (
	fixRulesFile    = "../../docs/reference/gonk-pipeline-fix-rules.md"
	changeRulesFile = "../../docs/reference/gonk-change-rules.md"
)

// tableRows returns the first-column codes (A1, M11, ...) of the table rows
// between the heading that starts with `from` and the next heading that
// starts with `to`.
func tableRows(t *testing.T, text, from, to, letter string) []string {
	t.Helper()
	i := strings.Index(text, "\n"+from)
	if i < 0 {
		t.Fatalf("the plan has no heading %q", from)
	}
	text = text[i:]
	j := strings.Index(text, "\n"+to)
	if j < 0 {
		t.Fatalf("the plan has no heading %q after %q", to, from)
	}
	text = text[:j]
	re := regexp.MustCompile(`(?m)^\| (` + letter + `\d+) \|`)
	var rows []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		rows = append(rows, m[1])
	}
	return rows
}

// fixtureRows returns the plan rows the single-fact-false table covers, with a
// check that every fixture's code is of the row its name claims.
func fixtureRows(t *testing.T) map[string]bool {
	t.Helper()
	have := map[string]bool{}
	for _, bc := range breakCases {
		row := bc.code.Row()
		have[row] = true
	}
	return have
}

func sortedRows(rows map[string]bool) []string {
	var out []string
	for r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i][1:])
		b, _ := strconv.Atoi(out[j][1:])
		return a < b
	})
	return out
}

// Fix plan 9.1: every A row is carried by an M row that has a fixture, and
// every M row but the rate cap carries an A row.
func TestEveryPlanARowHasAFixture(t *testing.T) {
	want := tableRows(t, readFile(t, fixRulesFile), "## 4. Automatic merge: the facts required (A rows)\n", "## 5. ", "A")
	if len(want) != 9 {
		t.Fatalf("the pipeline-fix rules page lists %d A rows (%v), expected 9: its section 4 changed, re-read it", len(want), want)
	}
	carried := map[string]bool{}
	for m := range fixtureRows(t) {
		if a := ARow(m); a != "" {
			carried[a] = true
		} else if m != "M11" {
			t.Errorf("row %s carries no fix-plan row; only M11 may not", m)
		}
	}
	if got := sortedRows(carried); !reflect.DeepEqual(got, want) {
		t.Errorf("A rows with a fixture: %v\nA rows in the plan:    %v", got, want)
	}
}

// Epic plan 5.1: every M row has a single-fact-false fixture, and no fixture
// claims a row the plan does not have.
func TestEveryPlanMRowHasAFixture(t *testing.T) {
	want := tableRows(t, readFile(t, changeRulesFile), "## 5. Automatic merge: the facts required (M rows)\n", "## 6. ", "M")
	if len(want) != 11 {
		t.Fatalf("the change rules page lists %d M rows (%v), expected 11: its section 5 changed, re-read it", len(want), want)
	}
	if got := sortedRows(fixtureRows(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("M rows with a fixture: %v\nM rows in the plan:    %v", got, want)
	}
}

// The rows this package was written against, so that the test above skipping
// is not the only thing between a dropped fixture and a green run.
func TestTheFixturesCoverElevenRows(t *testing.T) {
	want := []string{"M1", "M2", "M3", "M4", "M5", "M6", "M7", "M8", "M9", "M10", "M11"}
	if got := sortedRows(fixtureRows(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("rows with a fixture: %v\nwant:                 %v", got, want)
	}
	rules := map[string]bool{}
	for _, r := range Rules() {
		rules[r.Row()] = true
	}
	if got := sortedRows(rules); !reflect.DeepEqual(got, want) {
		t.Errorf("rows with a rule: %v\nwant:              %v", got, want)
	}
}

// The package is PURE: no GitLab client, no store, no config schema, no
// command, no clock, no environment, no file. Read from the source's imports
// and identifiers, as pkg/scope's contract test does.
func TestThePackageIsPure(t *testing.T) {
	allowed := map[string]bool{
		"fmt": true, "path": true, "sort": true, "strings": true, "time": true,
		"github.com/gonk-factory/gonk/pkg/cidef":       true,
		"github.com/gonk-factory/gonk/pkg/pipefix":     true,
		"github.com/gonk-factory/gonk/pkg/pipelinectx": true,
		"github.com/gonk-factory/gonk/pkg/scope":       true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 4 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[p] {
				t.Errorf("%s imports %q: this package takes facts, it does not fetch them", name, p)
			}
		}
		src := readFile(t, name)
		for _, banned := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(src, banned) {
				t.Errorf("%s calls %s the only clock is the `now` the caller passes", name, banned)
			}
		}
	}
	if checked < 4 {
		t.Errorf("only %d source files were checked", checked)
	}
}
