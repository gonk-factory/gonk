package buildgate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The flake tooling (gonk-5pyx; docs/testing-time.md). Three properties are
// pinned here because each can be lost by an edit that looks harmless:
//
//   - `make hunt` is an INSTRUMENT. The owner approved it as something a
//     person runs, and explicitly not as a gate or a scheduled job. A hunt in
//     the gate would make the gate slow and load-dependent -- and red on
//     purpose-built load, which is not what a gate's red means.
//   - The hunt's report is arithmetic a person acts on (a failure RATE goes
//     into a P1 bead), so the arithmetic is tested, not trusted.
//   - The time.Sleep lint is configured. Deleting the forbidigo block would
//     leave every nolint annotation in place, reading as enforced, with
//     nothing enforcing it.

// makeTargets parses the Makefile into target -> prerequisites and
// target -> recipe text. Only plain `name: deps` rules are read; that is the
// only shape this Makefile uses for the gate's closure.
func makeTargets(t *testing.T) (deps map[string][]string, recipes map[string]string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return parseMakeTargets(string(data))
}

var makeRuleRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+):(?:[^=]|$)(.*)$`)

func parseMakeTargets(src string) (deps map[string][]string, recipes map[string]string) {
	deps, recipes = map[string][]string{}, map[string]string{}
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(src))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "\t") {
			if cur != "" {
				recipes[cur] += line + "\n"
			}
			continue
		}
		if m := makeRuleRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(m[1], ".") {
			cur = m[1]
			rest := line[len(m[1])+1:]
			deps[cur] = append(deps[cur], strings.Fields(rest)...)
			continue
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			cur = ""
		}
	}
	return deps, recipes
}

// gateClosureMentionsHunt returns the targets in `gate`'s transitive closure
// that ARE hunt or whose recipe runs the hunt script.
func gateClosureMentionsHunt(deps map[string][]string, recipes map[string]string) []string {
	var bad []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		if n == "hunt" || strings.Contains(recipes[n], "flake_hunt") || strings.Contains(recipes[n], "$(MAKE) hunt") {
			bad = append(bad, n)
		}
		for _, d := range deps[n] {
			walk(d)
		}
	}
	walk("gate")
	return bad
}

func TestHuntIsNeverPartOfTheGate(t *testing.T) {
	deps, recipes := makeTargets(t)
	if _, ok := deps["hunt"]; !ok {
		t.Fatal("the Makefile has no `hunt` target; hack/flake_hunt.sh's `make hunt` usage is stale")
	}
	if !strings.Contains(recipes["hunt"], "hack/flake_hunt.sh") {
		t.Fatalf("`make hunt` does not run hack/flake_hunt.sh; recipe:\n%s", recipes["hunt"])
	}
	if len(deps["gate"]) == 0 {
		t.Fatal("found no prerequisites for `gate` -- the Makefile parser is stale and this check would pass vacuously")
	}
	if bad := gateClosureMentionsHunt(deps, recipes); len(bad) != 0 {
		t.Fatalf("`make gate` reaches the flake hunt through %v. The hunt is an instrument a person runs "+
			"under deliberate load; it must never be a gate (gonk-5pyx, docs/testing-time.md).", bad)
	}
	// Nor is it scheduled or run by CI: the owner approved it explicitly NOT
	// as a nightly job.
	huntRe := regexp.MustCompile(`flake_hunt|make hunt`)
	ci := map[string][]byte{}
	if data, ok := maintainerCI(t); ok {
		ci[maintainerCIFile] = data
	}
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		ci[".github/workflows/"+e.Name()] = data
	}
	if len(entries) == 0 {
		t.Fatalf("no workflow files under %s -- the scan is broken, not the repo", dir)
	}
	for name, data := range ci {
		if huntRe.Match(data) {
			t.Errorf("CI runs the flake hunt (%s). It is not a CI job and not scheduled (gonk-5pyx).", name)
		}
	}
}

func TestTheHuntInTheGateCheckActuallyFails(t *testing.T) {
	deps, recipes := parseMakeTargets("gate: fmt test\n\techo gate\nfmt:\n\tgofmt -l .\ntest: hunt\n\tgo test ./...\nhunt:\n\thack/flake_hunt.sh\n")
	if bad := gateClosureMentionsHunt(deps, recipes); len(bad) != 1 || bad[0] != "hunt" {
		t.Fatalf("a gate that depends on hunt through `test` was not caught: %v", bad)
	}
	deps, recipes = parseMakeTargets("gate: test\ntest:\n\tgo test ./... && hack/flake_hunt.sh\n")
	if bad := gateClosureMentionsHunt(deps, recipes); len(bad) != 1 || bad[0] != "test" {
		t.Fatalf("a gate whose test recipe runs the hunt script was not caught: %v", bad)
	}
	deps, recipes = parseMakeTargets("gate: test\ntest:\n\tgo test ./...\nhunt:\n\thack/flake_hunt.sh\n")
	if bad := gateClosureMentionsHunt(deps, recipes); len(bad) != 0 {
		t.Fatalf("a hunt target that the gate does not reach was flagged: %v", bad)
	}
}

// runSummarize feeds the hunt script's own report code a records file.
func runSummarize(t *testing.T, records, crashes string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	rec := filepath.Join(dir, "records.tsv")
	cr := filepath.Join(dir, "crashes.tsv")
	if err := os.WriteFile(rec, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cr, []byte(crashes), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "hack", "flake_hunt.sh"), "--summarize", rec, cr)
	cmd.Env = append(os.Environ(), "TOP=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run summarize: %v", err)
	}
	return out.String(), code
}

func TestFlakeHuntReportsRatesAndPercentiles(t *testing.T) {
	// ./p TestFlaky: 4 runs, 1 FAIL, durations 0.1 0.2 0.3 4.0.
	// Nearest-rank: p50 = d[ceil(2)] = 0.2, p99 = d[ceil(3.96)] = d[4] = 4.0.
	// ./p TestSteady: 2 runs, 0 fails -- must not appear under FAILING.
	records := strings.Join([]string{
		"./p\tTestFlaky\tPASS\t0.30\tl\t",
		"./p\tTestFlaky\tFAIL\t4.00\tl\t",
		"./p\tTestFlaky\tPASS\t0.10\tl\t",
		"./p\tTestFlaky\tPASS\t0.20\tl\t",
		"./p\tTestSteady\tPASS\t0.01\tl\t",
		"./p\tTestSteady\tPASS\t0.02\tl\t",
	}, "\n") + "\n"
	out, code := runSummarize(t, records, "")
	if code != 1 {
		t.Errorf("exit = %d with a failure present, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "2 tests, 6 test runs, 1 failures") {
		t.Errorf("summary totals wrong:\n%s", out)
	}
	row := regexp.MustCompile(`(?m)^1\s+4\s+25\.0%\s+0\.20\s+4\.00\s+4\.00\s+\./p TestFlaky$`)
	if !row.MatchString(out) {
		t.Errorf("TestFlaky row should read 1 fail / 4 runs, 25.0%%, p50 0.20, p99 4.00, max 4.00:\n%s", out)
	}
	failing := out[strings.Index(out, "-- FAILING"):]
	if strings.Contains(failing, "TestSteady") {
		t.Errorf("a test with no failures is listed under FAILING:\n%s", out)
	}
}

func TestFlakeHuntCleanRunExitsZeroAndCrashesDoNot(t *testing.T) {
	records := "./p\tTestA\tPASS\t0.10\tl\t\n./p\tTestA\tPASS\t0.10\tl\t\n"
	out, code := runSummarize(t, records, "")
	if code != 0 || !strings.Contains(out, "1 tests, 2 test runs, 0 failures") || strings.Contains(out, "-- FAILING") {
		t.Fatalf("clean run: exit %d, want 0 and no FAILING section:\n%s", code, out)
	}
	// A binary that died without a FAIL line (a -test.timeout panic) is not a
	// clean run just because no test reported red.
	out, code = runSummarize(t, records, "./p\t3\t2\t/tmp/x.log\t12345\n")
	if code != 1 || !strings.Contains(out, "-- CRASHES: 1") || !strings.Contains(out, "shuffle=12345") {
		t.Fatalf("crash: exit %d, want 1 and a CRASHES section naming the seed:\n%s", code, out)
	}
}

// golangciConfig is the part of .golangci.yml the time.Sleep rule lives in.
// It is PARSED, not grepped: the first version of this check looked for the
// substring "    - forbidigo\n", which the exclusions block further down the
// same file also contains -- so deleting forbidigo from linters.enable left
// this test green while the lint was disarmed and golangci-lint reported 0
// issues on the very packages the annotations live in (gonk-5pyx review, D1).
type golangciConfig struct {
	Linters struct {
		Enable   []string `yaml:"enable"`
		Settings struct {
			Forbidigo struct {
				Forbid []struct {
					Pattern string `yaml:"pattern"`
				} `yaml:"forbid"`
				AnalyzeTypes bool `yaml:"analyze-types"`
			} `yaml:"forbidigo"`
			NoLintLint struct {
				RequireExplanation bool `yaml:"require-explanation"`
				RequireSpecific    bool `yaml:"require-specific"`
			} `yaml:"nolintlint"`
		} `yaml:"settings"`
		Exclusions struct {
			Rules []struct {
				Path       string   `yaml:"path"`
				PathExcept string   `yaml:"path-except"`
				Linters    []string `yaml:"linters"`
			} `yaml:"rules"`
		} `yaml:"exclusions"`
	} `yaml:"linters"`
}

// timeSleepLintProblems returns one message per property of the time.Sleep
// rule that the given .golangci.yml does not have. Empty means the rule is
// armed: forbidigo and nolintlint both ENABLED (not merely mentioned), the
// pattern present, matched by resolved type, scoped to _test.go files, and
// every escape hatch required to name its linter and give a reason.
func timeSleepLintProblems(data []byte) []string {
	var cfg golangciConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return []string{fmt.Sprintf("cannot parse .golangci.yml: %v", err)}
	}
	var bad []string
	enabled := map[string]bool{}
	for _, l := range cfg.Linters.Enable {
		enabled[l] = true
	}
	for _, l := range []string{"forbidigo", "nolintlint"} {
		if !enabled[l] {
			bad = append(bad, l+" is not in linters.enable")
		}
	}
	fb := cfg.Linters.Settings.Forbidigo
	var found bool
	for _, f := range fb.Forbid {
		if f.Pattern == `^time\.Sleep$` {
			found = true
		}
	}
	if !found {
		bad = append(bad, `linters.settings.forbidigo.forbid has no pattern '^time\.Sleep$'`)
	}
	if !fb.AnalyzeTypes {
		bad = append(bad, "forbidigo analyze-types is not true, so an aliased import of time slips past the pattern")
	}
	nl := cfg.Linters.Settings.NoLintLint
	if !nl.RequireExplanation {
		bad = append(bad, "nolintlint require-explanation is not true, so //nolint:forbidigo needs no reason")
	}
	if !nl.RequireSpecific {
		bad = append(bad, "nolintlint require-specific is not true, so a bare //nolint disables everything silently")
	}
	scoped := false
	for _, r := range cfg.Linters.Exclusions.Rules {
		for _, l := range r.Linters {
			if l == "forbidigo" && r.PathExcept == `_test\.go$` {
				scoped = true
			}
		}
	}
	if !scoped {
		bad = append(bad, `no exclusions rule limits forbidigo to _test.go (path-except '_test\.go$')`)
	}
	return bad
}

func TestTheTimeSleepLintIsConfigured(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".golangci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range timeSleepLintProblems(data) {
		t.Errorf(".golangci.yml: %s -- the time.Sleep-in-tests rule (gonk-5pyx, docs/testing-time.md) is not "+
			"fully armed, and every `//nolint:forbidigo // reason` in the tree reads as enforced when it is not", p)
	}
}

// The control D1 asked for: each property, removed or flipped one at a time,
// must be REPORTED. A checker that cannot fail is a checker that proves
// nothing -- which is exactly what the grep version turned out to be.
func TestTheTimeSleepLintCheckActuallyFails(t *testing.T) {
	root := repoRoot(t)
	good, err := os.ReadFile(filepath.Join(root, ".golangci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if p := timeSleepLintProblems(good); len(p) != 0 {
		t.Fatalf("the repo's own config is reported broken: %v", p)
	}
	cases := []struct {
		name, old, new, want string
	}{
		// THE EXACT EDIT THE GREP MISSED: forbidigo deleted from
		// linters.enable, while the identical word survives in exclusions.
		{"enable deleted", "\n    - forbidigo\n    - nolintlint\n", "\n    - nolintlint\n", "forbidigo is not in linters.enable"},
		{"nolintlint deleted", "\n    - nolintlint\n", "\n", "nolintlint is not in linters.enable"},
		{"pattern changed", `pattern: '^time\.Sleep$'`, `pattern: '^time\.Nope$'`, "no pattern"},
		{"analyze-types off", "analyze-types: true", "analyze-types: false", "analyze-types is not true"},
		{"require-explanation off", "require-explanation: true", "require-explanation: false", "require-explanation is not true"},
		{"require-specific off", "require-specific: true", "require-specific: false", "require-specific is not true"},
		{"scope widened", `path-except: '_test\.go$'`, `path-except: 'nothing$'`, "limits forbidigo to _test.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := strings.Replace(string(good), c.old, c.new, 1)
			if mutated == string(good) {
				t.Fatalf("fixture %q did not change the config -- this case is stale and tests nothing", c.old)
			}
			problems := timeSleepLintProblems([]byte(mutated))
			var hit bool
			for _, p := range problems {
				if strings.Contains(p, c.want) {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("mutation %q was not reported; got %v", c.name, problems)
			}
		})
	}
}

// The gate's untagged `go test ./...` shuffles, in all three runners (gonk-5pyx).
// Dropping -shuffle=on from one of them would let an order dependence be green
// there and red elsewhere -- the same drift that kept .gitlab-ci.yml red for
// hours while Hub was green (see the Makefile's `test` comment).
func TestTheGateShufflesEverywhere(t *testing.T) {
	root := repoRoot(t)
	lineRe := regexp.MustCompile(`(?m)^[\t -]*(?:run: )?(?:\$\(GO\)|go) test \./\.\.\. -race -count=1(.*)$`)
	files := map[string][]byte{}
	for _, f := range []string{"Makefile", ".github/workflows/ci.yml"} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = data
	}
	// The maintainers' pipeline is the third runner; an exported tree does
	// not carry it, and the two published ones are still checked there.
	if data, ok := maintainerCI(t); ok {
		files[maintainerCIFile] = data
	}
	for f, data := range files {
		ms := lineRe.FindAllStringSubmatch(string(data), -1)
		if len(ms) != 1 {
			t.Errorf("%s: want exactly one untagged `go test ./... -race -count=1` line, found %d", f, len(ms))
			continue
		}
		if strings.TrimSpace(ms[0][1]) != "-shuffle=on" {
			t.Errorf("%s: the gate's go test line ends in %q, want \"-shuffle=on\"", f, strings.TrimSpace(ms[0][1]))
		}
	}
}
