// The chart-to-image contract (gonk-xsk3, shrunk per
// docs/reviews/2026-09-17-chart-complexity-review.md).
//
// chart/IMAGE-CONTRACT lists every invocation of a first-party gonk binary the
// chart makes, as rendered in the chart goldens. Two rules:
//
//  1. The file equals what the goldens invoke. A chart edit that starts (or
//     stops) running a subcommand or flag cannot land without the file saying so.
//  2. When an image's lines change against origin/main, that image's Chart.yaml
//     floor must go UP, and no floor may ever go DOWN. That is the mistake
//     behind the 2026-09-12 outage: the chart started invoking `gonk-gate
//     sweep-health` and nothing raised what image it needed.
//  3. The base IS origin/main. Comparing against HEAD would only see
//     uncommitted edits, so a missing origin/main is a failure with the fetch
//     that fixes it, not a silent fallback.
//
// What this does NOT prove: that the floor's build really implements the
// invocation (`hack/require_image_tags.py` at authoring time and a kind install
// do that, not this), anything a golden profile does not render, env vars, HTTP
// paths, or a binary reached through a shell variable. See the plan document.
package buildgate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/internal/keydomain"
)

const (
	contractFile = "chart/IMAGE-CONTRACT"
	goldenDir    = "internal/charttest/testdata/golden"
	guardsFile   = "chart/gonk/templates/_guards.tpl"
	floorName    = "image-floor."
	floorPrefix  = keydomain.Prefix + floorName
)

// gonkBinary matches a bare binary name or a PATH to one. `=` is excluded on
// purpose: `GATE=/usr/local/bin/gonk-gate` is an assignment, not a call (D6).
var gonkBinary = regexp.MustCompile(`^(?:[\w.@/+-]*/)?gonk-(?:gate|intake|meter)$`)

// shellTrim is what surrounds a command word in a script: quotes, `$(`, a
// backtick, and the `;` or `)` that can end it (D5).
const shellTrim = "\"'`$();"

// invocationLine normalises one argv starting at a gonk binary: component,
// argv0 as written, the subcommand if the next word is not a flag, then flag
// NAMES (spelling kept: -allow and --allow are different contracts), sorted.
func invocationLine(component string, words []string) string {
	out := []string{component, strings.Trim(words[0], `"'`)}
	if len(words) > 1 && !strings.HasPrefix(words[1], "-") {
		out = append(out, strings.Trim(words[1], `"'`))
	}
	flags := map[string]bool{}
	for _, w := range words[1:] {
		if w = strings.Trim(w, `"'`); strings.HasPrefix(w, "-") {
			flags[strings.SplitN(w, "=", 2)[0]] = true
		}
	}
	return strings.Join(append(out, slices.Sorted(maps.Keys(flags))...), " ")
}

// scanArgv records the gonk invocations in one argv. A word that is a gonk
// binary starts an invocation that runs to the end of the argv. A multi-word
// element (an `sh -c` script) is scanned line by line, skipping comment lines,
// and an invocation there runs to the end of its shell command.
// proseMode says whether quoted prose is scanned. Threading it through instead
// of hiding it in a package variable is what lets the allowlist test below scan
// the same goldens BOTH ways and compare (F1).
type proseMode bool

const (
	dropProse proseMode = false
	keepProse proseMode = true
)

func scanArgv(component string, argv []string, found map[string]bool, mode proseMode) {
	for i, a := range argv {
		if gonkBinary.MatchString(a) {
			found[invocationLine(component, argv[i:])] = true
			return
		}
		if !strings.ContainsAny(a, " \n") {
			continue
		}
		// A `\`-continued command is ONE command: joining first is what stops a
		// flag on a continuation line from being invisible here (D4).
		joined := strings.ReplaceAll(a, "\\\n", " ")
		for _, line := range strings.Split(joined, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			text := line
			if mode == dropProse {
				text = dropQuotedProse(line)
			}
			fields := strings.Fields(text)
			for j, f := range fields {
				if !gonkBinary.MatchString(strings.Trim(f, shellTrim)) {
					continue
				}
				words := []string{strings.Trim(f, shellTrim)}
				for _, w := range fields[j+1:] {
					cut := strings.IndexAny(w, ";|&)`")
					if cut < 0 {
						words = append(words, w)
						continue
					}
					if cut > 0 {
						words = append(words, strings.TrimRight(w[:cut], `"'`))
					}
					break
				}
				found[invocationLine(component, words)] = true
			}
		}
	}
}

// dropQuotedProse removes quoted spans that are TEXT rather than a command, so
// that `echo "bootstrap-city: this gonk-gate has no Postgres work store"` is not
// read as an invocation of `gonk-gate has` (E3). It was, and writing that line
// into chart/IMAGE-CONTRACT would have made the file state something the chart
// does not do -- the one thing that file must never do.
//
// A quoted span is KEPT when it can still hold a command:
//   - it contains `$(` or a backtick (a substitution: `"$(gonk-gate dispatch)"`);
//   - its first word is a gonk binary (a quoted command: `su -c "gonk-gate sweep"`).
//
// Anything else is prose and is replaced by a space, so a gonk binary anywhere
// but the first word of such a span is invisible. That covers EVERY quoted
// compound written inside a script -- `sh -c "set -e; gonk-gate sweep"`, its
// single-quoted twin, `su -c "env X=1 gonk-gate sweep"` -- not just the
// env-prefix case (F1).
//
// It does NOT cover a container whose `command:` is
// ["/bin/sh","-c","set -e; gonk-gate sweep"]: YAML eats those quotes, so the
// reader sees a plain script and finds the call. Both shapes were measured
// against the real goldens on 2026-09-20.
//
// TestTheProseRuleHidesOnlyWhatThisFileDeclares holds the blind spot to a
// DECLARED list, so it cannot widen unnoticed.
func dropQuotedProse(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); i++ {
		q := line[i]
		if q != '\'' && q != '"' {
			out.WriteByte(q)
			continue
		}
		end := strings.IndexByte(line[i+1:], q)
		if end < 0 { // unterminated: leave the rest as it is
			out.WriteString(line[i:])
			break
		}
		span := line[i+1 : i+1+end]
		keep := strings.Contains(span, "$(") || strings.Contains(span, "`")
		if f := strings.Fields(span); !keep && len(f) > 0 && gonkBinary.MatchString(strings.Trim(f[0], shellTrim)) {
			keep = true
		}
		if keep {
			out.WriteByte(q)
			out.WriteString(span)
			out.WriteByte(q)
		} else {
			out.WriteByte(' ')
		}
		i += end + 1
	}
	return out.String()
}

// goldenInvocations walks every container, initContainer, exec probe and
// lifecycle exec in the rendered manifests. A container with args but no
// command runs its image ENTRYPOINT, read from images/Dockerfile.<component>.
func goldenInvocations(docs [][]byte, entrypoint func(component string) []string, mode proseMode) (map[string]bool, error) {
	found := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case []any:
			for _, e := range n {
				walk(e)
			}
		case map[string]any:
			for k, e := range n {
				if k != "containers" && k != "initContainers" && k != "ephemeralContainers" {
					walk(e)
					continue
				}
				list, _ := e.([]any)
				for _, c := range list {
					container, _ := c.(map[string]any)
					image, _ := container["image"].(string)
					repo := filepath.Base(strings.SplitN(image, ":", 2)[0])
					component := strings.TrimPrefix(repo, "gonk-")
					argv := strs(container["command"])
					if len(argv) == 0 {
						argv = entrypoint(component)
					}
					scanArgv(component, append(argv, strs(container["args"])...), found, mode)
					for _, p := range []string{"readinessProbe", "livenessProbe", "startupProbe"} {
						scanArgv(component, strs(dig(container, p, "exec", "command")), found, mode)
					}
					for _, h := range []string{"postStart", "preStop"} {
						scanArgv(component, strs(dig(container, "lifecycle", h, "exec", "command")), found, mode)
					}
				}
			}
		}
	}
	for _, d := range docs {
		dec := yaml.NewDecoder(strings.NewReader(string(d)))
		for {
			var v any
			if err := dec.Decode(&v); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, err
			}
			walk(v)
		}
	}
	return found, nil
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

// parseContract returns the file's lines grouped by component.
func parseContract(data string) map[string][]string {
	out := map[string][]string{}
	for _, l := range strings.Split(data, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			c := strings.Fields(l)[0]
			out[c] = append(out[c], l)
		}
	}
	for c := range out {
		sort.Strings(out[c])
	}
	return out
}

// parseFloors returns component -> floor tag from Chart.yaml's annotations:
// the keys under gonk's own key domain (internal/keydomain).
func parseFloors(chartYAML string) map[string]string {
	return floorsWhere(chartYAML, func(key string) (string, bool) { return strings.CutPrefix(key, floorPrefix) })
}

// parseBaseFloors reads the BASE chart's floors by NAME, under whatever domain
// that chart spelled them. The key domain is itself something a commit can
// change, and in the commit that changes it the base chart has no key under
// the new domain at all: read with parseFloors the base would have NO floors,
// and "a floor was lowered" would compare against nothing and pass. So the base
// side keys on the part after the slash.
func parseBaseFloors(chartYAML string) map[string]string {
	return floorsWhere(chartYAML, func(key string) (string, bool) {
		i := strings.LastIndex(key, "/")
		if i < 0 {
			return "", false
		}
		return strings.CutPrefix(key[i+1:], floorName)
	})
}

func floorsWhere(chartYAML string, name func(key string) (string, bool)) map[string]string {
	var c struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	_ = yaml.Unmarshal([]byte(chartYAML), &c)
	out := map[string]string{}
	for k, v := range c.Annotations {
		if n, ok := name(k); ok {
			out[n] = v
		}
	}
	return out
}

func floorBuild(tag string) int {
	m := fluxFilterTags.FindStringSubmatch(tag)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[fluxFilterTags.SubexpIndex("build")])
	return n
}

// contractProblems is the whole gate, pure so the negative controls can drive it.
func contractProblems(rendered map[string]bool, contract, baseContract string, floors, baseFloors map[string]string) []string {
	var problems []string
	want := parseContract(contract)
	have := slices.Sorted(maps.Keys(rendered))
	if got := parseContract(strings.Join(have, "\n")); fmt.Sprint(got) != fmt.Sprint(want) {
		problems = append(problems, fmt.Sprintf("%s does not match what the chart goldens invoke.\n"+
			"  file says:\n    %s\n  goldens invoke:\n    %s\nIf the chart now invokes something new, write it into "+
			"the file AND raise that image's floor in Chart.yaml to a build that has it (hack/require_image_tags.py).",
			contractFile, strings.Join(flatten(want), "\n    "), strings.Join(have, "\n    ")))
	}
	base := parseContract(baseContract)
	for c := range want {
		if floorBuild(floors[c]) < 0 {
			problems = append(problems, fmt.Sprintf("the chart invokes a binary on the %s image but Chart.yaml has no "+
				"rankable %s%s annotation, so G23 guards nothing for it", c, floorPrefix, c))
		}
	}
	// A LOWERED floor is the one edit that re-arms the outage on its own: the
	// chart keeps invoking what it invokes and G23 stops refusing the image that
	// cannot serve it. It is never right, with or without a contract change (D2).
	for c, bf := range baseFloors {
		if b := floorBuild(bf); b >= 0 && floorBuild(floors[c]) < b {
			problems = append(problems, fmt.Sprintf("the %s image floor was LOWERED (%q -> %q). Floors only ever "+
				"climb: lowering one lets a render that G23 refuses today go through tomorrow, which is how "+
				"2026-09-12 happened. If the floor is genuinely too high, say why in %s and raise it elsewhere.",
				c, bf, floors[c], contractFile))
		}
	}
	for _, c := range unionKeys(want, base) {
		if fmt.Sprint(want[c]) == fmt.Sprint(base[c]) {
			continue
		}
		if floorBuild(floors[c]) <= floorBuild(baseFloors[c]) {
			problems = append(problems, fmt.Sprintf("what the chart invokes on the %s image changed against the base "+
				"(%v -> %v), but its floor did not go up (%q -> %q). A changed invocation deployed against the old "+
				"floor is the 2026-09-12 outage. Raise %s%s.", c, base[c], want[c], baseFloors[c], floors[c], floorPrefix, c))
		}
	}
	return problems
}

func flatten(m map[string][]string) []string {
	var out []string
	for _, c := range unionKeys(m, nil) {
		out = append(out, m[c]...)
	}
	return out
}

func unionKeys(a, b map[string][]string) []string {
	keys := append(slices.Collect(maps.Keys(a)), slices.Collect(maps.Keys(b))...)
	slices.Sort(keys)
	return slices.Compact(keys)
}

// baseRef is origin/main, the last released state, falling back to HEAD (a
// single-branch clone that never fetched main). "" means no git at all.
//
// THE FALLBACK IS WEAK AND MUST NOT BE SILENT (D1). Against HEAD the floor
// rules only see UNCOMMITTED edits, so a branch that commits a contract change
// with no floor raise passes. The caller says which base it used, every run,
// and refuses the fallback in CI, where the fix is one fetch.
func baseRef(root string) string {
	for _, ref := range []string{"origin/main", "HEAD"} {
		if exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil {
			return ref
		}
	}
	return ""
}

// baseProblem rejects any base but origin/main -- RED, not a warning, wherever
// it happens. Against HEAD the floor rules see only UNCOMMITTED edits, so a
// branch that commits a contract change with no floor raise sails through and
// the gate is decoration. The fix is one fetch, so there is no honest reason to
// tolerate it.
func baseProblem(ref string) string {
	if ref == "origin/main" {
		return ""
	}
	return fmt.Sprintf("%s was compared against %s, not origin/main. The floor rules then only see UNCOMMITTED "+
		"edits, which is not what this gate claims to check. Fetch the released state:\n%s",
		contractFile, ref, fetchMainHint)
}

const fetchMainHint = "git fetch --depth=1 origin +refs/heads/main:refs/remotes/origin/main\n" +
	"(`git fetch --depth=1 origin main` is NOT enough: it moves FETCH_HEAD only and creates no origin/main)"

// gitShow returns a file at ref, or "" when the file does not exist there (a
// base with no contract is an empty contract and no floors).
func gitShow(root, ref, path string) string {
	out, err := exec.Command("git", "-C", root, "show", ref+":"+path).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// scanRepoGoldens reads every golden profile in the checkout at root.
func scanRepoGoldens(root string, mode proseMode) (map[string]bool, error) {
	files, _ := filepath.Glob(filepath.Join(root, goldenDir, "*.yaml"))
	if len(files) == 0 {
		return nil, fmt.Errorf("no chart goldens under %s; the contract would be checked against nothing", goldenDir)
	}
	var docs [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		docs = append(docs, b)
	}
	var epErr error
	rendered, err := goldenInvocations(docs, func(component string) []string {
		b, err := os.ReadFile(filepath.Join(root, "images", "Dockerfile."+component))
		if err != nil {
			return nil // not a first-party image: no ENTRYPOINT we can know
		}
		var ep []string
		for _, l := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "ENTRYPOINT "); ok {
				ep = nil
				if json.Unmarshal([]byte(rest), &ep) != nil {
					epErr = fmt.Errorf("images/Dockerfile.%s ENTRYPOINT is not exec form: %s", component, l)
				}
			}
		}
		return ep
	}, mode)
	return rendered, errors.Join(err, epErr)
}

// checkContractRepo runs the gate over a checkout at root.
func checkContractRepo(root string) (problems []string, ref string, err error) {
	rendered, err := scanRepoGoldens(root, dropProse)
	if err != nil {
		return nil, "", err
	}
	contract, err := os.ReadFile(filepath.Join(root, contractFile))
	if err != nil {
		return nil, "", err
	}
	chart, err := os.ReadFile(filepath.Join(root, sealChartDir, "Chart.yaml"))
	if err != nil {
		return nil, "", err
	}
	if ref = baseRef(root); ref == "" {
		return nil, "", fmt.Errorf("no git history to compare %s against (neither origin/main nor HEAD resolves)", contractFile)
	}
	return contractProblems(rendered, string(contract), gitShow(root, ref, contractFile),
		parseFloors(string(chart)), parseBaseFloors(gitShow(root, ref, sealChartDir+"/Chart.yaml"))), ref, nil
}

func TestTheChartImageContractMatchesTheGoldensAndTheFloors(t *testing.T) {
	problems, ref, err := checkContractRepo(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// Printed, not t.Logf: a plain `go test ./...` never shows a passing test's
	// log, and which base was used decides how much this gate actually proves.
	fmt.Printf("%s: compared against %s\n", contractFile, ref)
	t.Logf("%s: compared against %s", contractFile, ref)
	if p := baseProblem(ref); p != "" {
		t.Error(p)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// G23 ranks tags with the gitops ImagePolicy's own filterTags pattern, verbatim.
func TestTheImageFloorGuardRanksWithTheImagePolicyPattern(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), guardsFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := "$rankable := `" + fluxFilterTags.String() + "`"; !strings.Contains(string(b), want) {
		t.Fatalf("%s does not declare %s verbatim", guardsFile, want)
	}
}

// ---------------------------------------------------------------------------
// Negative controls
// ---------------------------------------------------------------------------

const (
	ctlBefore = "controller /usr/local/bin/gonk-gate sweep\n"
	ctlAfter  = "controller /usr/local/bin/gonk-gate sweep\ncontroller /usr/local/bin/gonk-gate sweep-health --max-age\n"
	floor341  = "v0.1.0-341.6e9441a7e028" // the outage image
	floor343  = "v0.1.0-343.a434b6fb3cac" // the first build with sweep-health
)

func set(lines string) map[string]bool {
	return parseSet(parseContract(lines))
}

func parseSet(m map[string][]string) map[string]bool {
	out := map[string]bool{}
	for _, l := range flatten(m) {
		out[l] = true
	}
	return out
}

// Every rule in contractProblems, proven to fail -- with the 2026-09-12 outage
// as the central case: the chart starts running `gonk-gate sweep-health` and
// the controller floor stays where it was.
func TestTheChartImageContractGateActuallyFails(t *testing.T) {
	floors := func(c, i, m string) map[string]string {
		return map[string]string{"controller": c, "intake": i, "meter": m}
	}
	cases := []struct {
		name                   string
		rendered               map[string]bool
		contract, baseContract string
		floors, baseFloors     map[string]string
		wantSaid               string
	}{
		{"chart invokes a subcommand the file does not list", set(ctlAfter), ctlBefore, ctlBefore,
			floors(floor341, floor341, floor341), floors(floor341, floor341, floor341), "does not match"},
		{"file lists an invocation the chart no longer makes", set(ctlBefore), ctlAfter, ctlAfter,
			floors(floor343, floor341, floor341), floors(floor343, floor341, floor341), "does not match"},
		{"flag spelled differently from the file", set("intake /usr/local/bin/gonk-intake netpol-probe --allow\n"),
			"intake /usr/local/bin/gonk-intake netpol-probe -allow\n", "intake /usr/local/bin/gonk-intake netpol-probe -allow\n",
			floors(floor341, floor341, floor341), floors(floor341, floor341, floor341), "does not match"},
		{"THE OUTAGE: contract changed, floor did not move", set(ctlAfter), ctlAfter, ctlBefore,
			floors(floor341, floor341, floor341), floors(floor341, floor341, floor341), "did not go up"},
		{"contract changed on controller, only the meter floor raised", set(ctlAfter), ctlAfter, ctlBefore,
			floors(floor341, floor341, floor343), floors(floor341, floor341, floor341), "the controller image changed"},
		{"contract changed and the floor went DOWN", set(ctlAfter), ctlAfter, ctlBefore,
			floors(floor341, floor341, floor341), floors(floor343, floor341, floor341), "did not go up"},
		{"contract changed and the floor is not rankable", set(ctlAfter), ctlAfter, ctlBefore,
			floors("v0.1.0-a434b6fb3cac", floor341, floor341), floors(floor341, floor341, floor341), "no rankable"},
		{"image invoked with no floor at all", set(ctlBefore), ctlBefore, ctlBefore,
			map[string]string{}, map[string]string{}, "no rankable"},
		{"A FLOOR LOWERED WITH NO CONTRACT CHANGE (D2)", set(ctlBefore), ctlBefore, ctlBefore,
			floors(floor341, floor343, floor343), floors(floor343, floor343, floor343), "was LOWERED"},
		{"a floor lowered on an image the chart does not invoke", set(ctlBefore), ctlBefore, ctlBefore,
			floors(floor343, floor341, floor343), floors(floor343, floor343, floor343), "was LOWERED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := strings.Join(contractProblems(tc.rendered, tc.contract, tc.baseContract, tc.floors, tc.baseFloors), "\n")
			if p == "" {
				t.Fatalf("contractProblems accepted %q", tc.name)
			}
			if !strings.Contains(p, tc.wantSaid) {
				t.Errorf("wanted %q in:\n%s", tc.wantSaid, p)
			}
		})
	}

	// The passing cases, so the above is not vacuously true of every input.
	if p := contractProblems(set(ctlAfter), ctlAfter, ctlBefore, floors(floor343, floor341, floor341),
		floors(floor341, floor341, floor341)); len(p) != 0 {
		t.Errorf("rejected the outage's correct fix (contract changed, controller floor raised): %v", p)
	}
	if p := contractProblems(set(ctlBefore), ctlBefore, ctlBefore, floors(floor341, floor341, floor341),
		floors(floor341, floor341, floor341)); len(p) != 0 {
		t.Errorf("rejected an unchanged contract: %v", p)
	}
}

// The reader must find each shape the chart can use to run a binary, and must
// not mistake a Service name, a URL or a shell comment for one. A reader that
// finds nothing makes every rule above vacuous.
func TestTheChartImageContractReaderFindsEveryForm(t *testing.T) {
	manifest := `
kind: Deployment
spec:
  template:
    spec:
      initContainers:
        - name: import
          image: registry.example/gonk-controller:v1
          command: ["/usr/local/bin/gonk-gate", "workstore-import", "--dsn-file=/x"]
      containers:
        - name: meter
          image: registry.example/gonk-meter:v1
          args: ["--listen=:8080"]
          readinessProbe:
            exec:
              command: [/usr/local/bin/gonk-gate, sweep-health, --max-age=15m]
          lifecycle:
            preStop:
              exec:
                command: [gonk-gate, warm-cache]
        - name: script
          image: registry.example/gonk-controller:v1
          command: ["/bin/sh", "-c"]
          args:
            - |
              # a comment naming gonk-gate sweep --nope is not a call
              curl http://gonk-meter.gonk.svc:8080 -H "gonk-intake:9090"
              "$(gonk-gate dispatch --order=x)" && echo ok; gonk-gate sweep -v | cat
              gonk-gate reconcile \
                --city /city \
                --since=1h
              (gonk-gate probe) ; gonk-intake netpol-probe -allow x;
              gonk-gate;
              (gonk-meter)
              # PROSE, not calls (E3). The echo below is the real line from the
              # T-41 bootstrap script that produced a fabricated
              # "gonk-gate has" entry in chart/IMAGE-CONTRACT.
              echo "bootstrap-city: this gonk-gate has no Postgres work store; not importing bd records"
              printf '%s\n' 'run gonk-gate sweep by hand if this fails'
              su -c "gonk-gate rescue --now"
              echo "captured $(gonk-gate audit --json) and moved on"
              echo don't panic; gonk-gate ping
              GATE=/usr/local/bin/gonk-gate
              $GATE noop
---
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: dolt
          image: docker.io/dolthub/dolt-sql-server:2.1.7
          args: ["gonk-meter:8080"]
`
	got, err := goldenInvocations([][]byte{[]byte(manifest)}, func(c string) []string {
		if c == "meter" {
			return []string{"/usr/local/bin/gonk-meter"}
		}
		return nil
	}, dropProse)
	if err != nil {
		t.Fatal(err)
	}
	// Note what is NOT here: `GATE=/usr/local/bin/gonk-gate` is an assignment
	// (D6), and `$GATE noop` is a call through a variable the reader cannot
	// follow -- a documented blind spot, not a silent one.
	want := set(`controller /usr/local/bin/gonk-gate workstore-import --dsn-file
meter /usr/local/bin/gonk-meter --listen
meter /usr/local/bin/gonk-gate sweep-health --max-age
meter gonk-gate warm-cache
controller gonk-gate dispatch --order
controller gonk-gate sweep -v
controller gonk-gate reconcile --city --since
controller gonk-gate probe
controller gonk-intake netpol-probe -allow
controller gonk-gate
controller gonk-meter
controller gonk-gate rescue --now
controller gonk-gate audit --json
controller gonk-gate ping
`)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reader found:\n  %v\nwant:\n  %v", got, want)
	}
}

// The base is origin/main, not HEAD: COMMITTING a contract change must not hide
// a missing floor raise (the gonk-jbwk hole in the seal). And with no
// origin/main the gate still compares against HEAD rather than nothing.
func TestTheChartImageContractComparesAgainstOriginMain(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t",
			"-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := func(sub string) string {
		return "kind: Pod\nspec:\n  containers:\n    - image: r/gonk-controller:v1\n      command: [gonk-gate, " + sub + "]\n"
	}
	chart := func(floor string) string { return "annotations:\n  " + floorPrefix + "controller: " + floor + "\n" }
	expect := func(what, wantRef, wantSaid string) {
		t.Helper()
		problems, ref, err := checkContractRepo(root)
		if err != nil {
			t.Fatal(err)
		}
		p := strings.Join(problems, "\n")
		if ref != wantRef || (wantSaid == "") != (p == "") || !strings.Contains(p, wantSaid) {
			t.Fatalf("%s: base %s (want %s), problems:\n%s\nwanted %q", what, ref, wantRef, p, wantSaid)
		}
	}

	git("init", "-q")
	write(goldenDir+"/p.yaml", golden("sweep"))
	write(contractFile, "controller gonk-gate sweep\n")
	write(sealChartDir+"/Chart.yaml", chart(floor341))
	git("add", "-A")
	git("commit", "-qm", "released")
	expect("released state, no origin/main", "HEAD", "")

	// THE OUTAGE, uncommitted: caught against HEAD even with no origin/main.
	write(goldenDir+"/p.yaml", golden("sweep-health"))
	write(contractFile, "controller gonk-gate sweep-health\n")
	expect("uncommitted change, no origin/main", "HEAD", "did not go up")

	// Committed, with origin/main at the released state: still caught.
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	git("add", "-A")
	git("commit", "-qm", "chart starts invoking sweep-health")
	expect("committed change, origin/main at the release", "origin/main", "did not go up")

	// And the fix passes.
	write(sealChartDir+"/Chart.yaml", chart(floor343))
	expect("floor raised", "origin/main", "")
}

// The base must be origin/main. Falling back to HEAD is caught, not tolerated,
// and the message says exactly which fetch fixes it (D1).
func TestTheContractBaseMustBeOriginMain(t *testing.T) {
	if p := baseProblem("origin/main"); p != "" {
		t.Errorf("origin/main was rejected as a base: %s", p)
	}
	for _, ref := range []string{"HEAD", "", "origin/master"} {
		p := baseProblem(ref)
		if p == "" {
			t.Fatalf("base %q was accepted; the floor rules would see only uncommitted edits", ref)
		}
		if !strings.Contains(p, "+refs/heads/main:refs/remotes/origin/main") {
			t.Errorf("the refusal for %q does not give the fetch that fixes it:\n%s", ref, p)
		}
	}
}

// ---------------------------------------------------------------------------
// The declared blind spot
// ---------------------------------------------------------------------------

// hiddenByProseRule is every invocation-shaped line the reader DELIBERATELY
// does not report, exactly as it appears in the goldens (F1).
//
// dropQuotedProse buys a truthful contract at a price: a gonk binary inside a
// quoted span is treated as text unless the span holds a substitution or STARTS
// with the binary. So any single-line quoted compound hides its calls --
// `sh -c "set -e; gonk-gate workstore-import --dsn-file /x"`, its single-quoted
// twin, and an env-prefixed one -- which is an ordinary way to write an
// initContainer. Nothing in the chart does that today, and this list says so
// out loud: it is the difference between scanning with the rule and without it,
// and it must stay exactly this, or the rule has started hiding something real.
var hiddenByProseRule = map[string]bool{
	// echo "bootstrap-city: this gonk-gate has no Postgres work store; not
	// importing bd records" -- workload-gonk-controller.yaml, the T-41
	// bootstrap script. Not a call, and never written into chart/IMAGE-CONTRACT.
	"controller gonk-gate has": true,
}

func TestTheProseRuleHidesOnlyWhatThisFileDeclares(t *testing.T) {
	root := repoRoot(t)
	strict, err := scanRepoGoldens(root, dropProse)
	if err != nil {
		t.Fatal(err)
	}
	everything, err := scanRepoGoldens(root, keepProse)
	if err != nil {
		t.Fatal(err)
	}

	hidden := map[string]bool{}
	for line := range everything {
		if !strict[line] {
			hidden[line] = true
		}
	}
	for line := range hidden {
		if !hiddenByProseRule[line] {
			t.Errorf("the prose rule is hiding %q, which is NOT declared in hiddenByProseRule.\n"+
				"Either the chart has started invoking a gonk binary inside a quoted compound -- in which case "+
				"the reader must be taught that shape, because %s would be silently incomplete -- or the line is "+
				"prose and belongs in the list with a comment quoting it.", line, contractFile)
		}
	}
	for line := range hiddenByProseRule {
		if !hidden[line] {
			t.Errorf("hiddenByProseRule declares %q, but the goldens no longer produce it. Delete the entry: a "+
				"stale allowlist can hide a real invocation that happens to match it.", line)
		}
	}
	// Dropping prose must only ever REMOVE lines. If it added one, the rule
	// would be rewriting what the chart invokes rather than filtering it.
	for line := range strict {
		if !everything[line] {
			t.Errorf("dropping quoted prose ADDED %q, which the unfiltered scan does not see", line)
		}
	}
	t.Logf("prose rule hides %d line(s) across the goldens: %v", len(hidden), slices.Sorted(maps.Keys(hidden)))
}
