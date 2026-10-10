package buildgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/internal/keydomain"
)

// THE KEY DOMAIN IS ONE STRING, IN GO AND IN THE CHART.
//
// internal/keydomain says why: a label key is a wire contract between a writer
// and a selector. Two things would let it split, and neither is caught by a
// compiler:
//
//   - a Go file re-inlines the domain as a literal, so that changing the
//     constant moves one side and not the other;
//   - the chart (which cannot import Go) spells its annotations and labels under
//     a different domain from the binaries it deploys.
//
// These tests hold both. They are deliberately about the DOMAIN only: which
// keys exist is asserted where each key is used (pkg/runtime/jobs,
// internal/meter/keysink, internal/charttest).

// keyDomainLiteral is the domain spelled out, so that this test fails when
// keydomain.Domain changes and someone has to look at the wire change that is.
const keyDomainLiteral = "gonk-factory.github.io"

func TestKeyDomainIsTheConfirmedOne(t *testing.T) {
	if keydomain.Domain != keyDomainLiteral {
		t.Fatalf("keydomain.Domain = %q, want %q.\nChanging it is a WIRE CHANGE: session Jobs labelled under "+
			"the old domain are invisible to the new gate. Drain first (chart/gonk/README.md, \"Upgrading\"), "+
			"then change this literal too.", keydomain.Domain, keyDomainLiteral)
	}
	if keydomain.Prefix != keyDomainLiteral+"/" {
		t.Fatalf("keydomain.Prefix = %q, want %q", keydomain.Prefix, keyDomainLiteral+"/")
	}
}

// goSourceFiles returns the tracked, non-vendored, non-test .go files.
func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z", "--", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" || strings.HasPrefix(f, "vendor/") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		files = append(files, f)
	}
	return files
}

const keyDomainHome = "internal/keydomain/keydomain.go"

// domainRepeats returns the files (other than the constant's home) whose text
// contains the domain.
func domainRepeats(files map[string]string) []string {
	var bad []string
	for name, body := range files {
		if name != keyDomainHome && strings.Contains(body, keydomain.Domain) {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	return bad
}

func TestKeyDomainIsSpelledOnceInGo(t *testing.T) {
	root := repoRoot(t)
	names := goSourceFiles(t, root)
	// Not vacuous: the scan must have covered the files that USE the constant.
	files := map[string]string{}
	for _, f := range names {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	for _, must := range []string{keyDomainHome, "pkg/runtime/jobs/spec.go", "internal/meter/keysink/k8s.go"} {
		body, ok := files[must]
		if !ok {
			t.Fatalf("%s was not scanned (%d files were): the walk is wrong, and this test would pass on nothing", must, len(files))
		}
		if must != keyDomainHome && !strings.Contains(body, "keydomain.Prefix") {
			t.Errorf("%s does not build its keys from keydomain.Prefix", must)
		}
	}
	if !strings.Contains(files[keyDomainHome], `"`+keydomain.Domain+`"`) {
		t.Fatalf("%s does not hold the domain literal", keyDomainHome)
	}
	if bad := domainRepeats(files); len(bad) > 0 {
		t.Fatalf("the key domain %q is spelled as a literal outside %s:\n  %s\n"+
			"Build the key from keydomain.Prefix. A second copy is how a writer and a selector end up "+
			"on different keys, which no compiler and no deploy reports.",
			keydomain.Domain, keyDomainHome, strings.Join(bad, "\n  "))
	}
}

// The detector, against a planted repeat (the scan above is only as good as this).
func TestKeyDomainRepeatIsDetected(t *testing.T) {
	got := domainRepeats(map[string]string{
		keyDomainHome:   `const Domain = "` + keydomain.Domain + `"`,
		"pkg/a/a.go":    `const k = "` + keydomain.Prefix + `session"`,
		"pkg/b/b.go":    `const k = keydomain.Prefix + "session"`,
		"cmd/c/main.go": "// " + keydomain.Domain + " in a comment counts too: it rots the same way",
	})
	if want := "cmd/c/main.go pkg/a/a.go"; strings.Join(got, " ") != want {
		t.Fatalf("domainRepeats = %v, want [%s]", got, want)
	}
}

// chartKeyNames are the names of the keys the CHART writes under gonk's
// domain. A name here must appear under keydomain.Domain and under no other.
var chartKeyNames = []string{"exposure", "image-floor.", "byo", "network-policy-enforcement"}

var chartKeyUse = regexp.MustCompile(`([A-Za-z0-9][A-Za-z0-9.-]*)/(exposure\b|image-floor\.|byo\b|network-policy-enforcement\b)`)

// chartKeyProblems returns one line per use of a chart key name under a
// domain other than want, and the count of uses seen per name.
func chartKeyProblems(files map[string]string, want string) (problems []string, seen map[string]int) {
	seen = map[string]int{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for i, line := range strings.Split(files[n], "\n") {
			for _, m := range chartKeyUse.FindAllStringSubmatch(line, -1) {
				seen[m[2]]++
				if m[1] != want {
					problems = append(problems, n+":"+strconv.Itoa(i+1)+": "+m[0]+" (domain "+m[1]+", want "+want+")")
				}
			}
		}
	}
	return problems, seen
}

func TestChartKeysUseTheGoKeyDomain(t *testing.T) {
	root := repoRoot(t)
	files := map[string]string{}
	add := func(rel string) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(b)
	}
	for _, rel := range []string{sealChartDir + "/Chart.yaml", sealChartDir + "/values.schema.json", sealChartDir + "/README.md", contractFile} {
		add(rel)
	}
	tpls, err := filepath.Glob(filepath.Join(root, sealChartDir, "templates", "*"))
	if err != nil || len(tpls) < 10 {
		t.Fatalf("chart templates: %d found, err %v", len(tpls), err)
	}
	for _, p := range tpls {
		rel, _ := filepath.Rel(root, p)
		add(filepath.ToSlash(rel))
	}
	problems, seen := chartKeyProblems(files, keydomain.Domain)
	for _, name := range chartKeyNames {
		if seen[name] == 0 {
			t.Errorf("no key named <domain>/%s anywhere in the chart: this test asserts nothing about it (seen: %v)", name, seen)
		}
	}
	if len(problems) > 0 {
		t.Fatalf("chart keys under a domain other than keydomain.Domain (%s):\n  %s", keydomain.Domain, strings.Join(problems, "\n  "))
	}

	// Chart.yaml's annotations are DATA the guard G23 and this package read by
	// key: every gonk annotation must be under the domain, parsed, not grepped.
	var c struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	if err := yaml.Unmarshal([]byte(files[sealChartDir+"/Chart.yaml"]), &c); err != nil {
		t.Fatal(err)
	}
	var floors, byo int
	for k := range c.Annotations {
		name, ok := strings.CutPrefix(k, keydomain.Prefix)
		switch {
		case ok && strings.HasPrefix(name, floorName):
			floors++
		case ok && name == "byo":
			byo++
		}
	}
	if floors != 3 || byo != 1 {
		t.Fatalf("Chart.yaml annotations under %s: %d image floors (want 3) and %d byo (want 1); annotations: %v",
			keydomain.Prefix, floors, byo, c.Annotations)
	}
}

func TestChartKeyUnderAnotherDomainIsDetected(t *testing.T) {
	problems, seen := chartKeyProblems(map[string]string{
		"a.yaml": "    " + keydomain.Prefix + "exposure: internal\n    app.kubernetes.io/component: intake\n",
		"b.yaml": "  keys.example.test/image-floor.controller: v0.1.0-1.0123456789ab\n",
		// The guard builds the key with printf: the name is followed by a verb,
		// not by a word character.
		"c.tpl": `{{- $floor := index $a (printf "keys.example.test/image-floor.%s" .name) -}}` + "\n",
	}, keydomain.Domain)
	if len(problems) != 2 || !strings.HasPrefix(problems[0], "b.yaml:1: keys.example.test/image-floor.") ||
		!strings.HasPrefix(problems[1], "c.tpl:1: keys.example.test/image-floor.") {
		t.Fatalf("problems = %q, want exactly the b.yaml floor and the c.tpl printf", problems)
	}
	if seen["exposure"] != 1 || seen["image-floor."] != 2 {
		t.Fatalf("seen = %v", seen)
	}
}

// THE BASE CHART'S FLOORS ARE READ BY NAME (parseBaseFloors). In the commit
// that changes the key domain the base has no key under the new one; read by
// the new prefix it would have no floors, and a floor lowered in that same
// commit would be compared against nothing.
func TestALoweredFloorIsCaughtAcrossAKeyDomainChange(t *testing.T) {
	base := "annotations:\n  keys.example.test/" + floorName + "controller: v0.1.0-412.0123456789ab\n" +
		"  keys.example.test/byo: gitlab\n  unrelated: x\n"
	baseFloors := parseBaseFloors(base)
	if len(baseFloors) != 1 || baseFloors["controller"] != "v0.1.0-412.0123456789ab" {
		t.Fatalf("parseBaseFloors = %v, want the controller floor read by name under any domain", baseFloors)
	}
	if got := parseFloors(base); len(got) != 0 {
		t.Fatalf("parseFloors read %v from a chart under another domain; the HEAD side must be strict", got)
	}
	head := "annotations:\n  " + floorPrefix + "controller: v0.1.0-343.0123456789ab\n"
	contract := "controller gonk-gate sweep\n"
	problems := contractProblems(map[string]bool{"controller gonk-gate sweep": true}, contract, contract, parseFloors(head), baseFloors)
	if len(problems) != 1 || !strings.Contains(problems[0], "LOWERED") {
		t.Fatalf("a floor lowered in the commit that renames the key domain was not caught; problems: %q", problems)
	}
}
