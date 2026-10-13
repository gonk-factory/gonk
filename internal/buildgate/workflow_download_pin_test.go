// Everything a workflow under .github/workflows/ fetches and then RUNS must be
// checked against a sha256 the step asserts, and every service image must be
// named by digest.
//
// WHY THIS IS A TEST (gonk-5at0, gonk-jvuo). ci.yml's header has said since
// T-18 that "every downloaded tarball is checked against a sha256 the step
// ASSERTS", and TestEveryActionIsPinnedByCommitSHA (action_pin_test.go) holds
// the `uses:` half of that promise. Nothing held the other half, and it was
// false in four places by the time a reviewer read the tree: release.yml
// downloaded cosign with no checksum and SIGNED THE PUBLISHED IMAGES with it;
// netpol.yml piped k3d's install.sh into bash, applied Calico's manifest from
// a URL by tag, and built kind with `go install`; ci.yml ran `postgres:16`.
// A sentence in a header is a snapshot. This is the property.
//
// WHAT IT CHECKS, per `run:` script:
//
//  1. a download (curl, wget) is never piped into a shell;
//  2. a download is written to a file, and that file is named on a later
//     `sha256sum -c` line of the same script;
//  3. the value that line asserts is a 64-hex literal: written out, or a
//     ${VAR} the step's (or job's, or workflow's) env: sets to one. A check
//     against a sum computed from the download itself asserts nothing;
//  4. kubectl is never handed a URL (`-f https://...`): fetch, check, apply;
//  5. a tool is never built from a module version (`go install x@v`,
//     `go run x@v`): one rule for every tool, and it is the release binary
//     with its checksum.
//
// and per job: every `services.<name>.image` and `container` image carries
// `@sha256:<64 hex>`.
//
// THE EXCEPTION. A request whose response is DATA -- an API answer that is
// parsed, never executed -- has nothing to pin. It says so on a comment line
// directly above the command:
//
//	# unchecked-download-ok: <why this response is not code>
//
// The reason is required. release.yml has one (the "is this tag already
// released" query).
//
// WHAT IT DOES NOT SEE, so nobody reads more into a green run than is there:
//   - whether a pinned sha256 is the RIGHT one. This suite is hermetic; the
//     value is compared with upstream's published checksum by a person, at
//     pin time (ci.yml's kubeconform comment records what an unasserted,
//     invented value cost).
//   - a download made by something other than curl or wget (an action, a
//     `docker pull` inside a script, a package manager, a tool that fetches
//     its own images at run time -- k3d, kind and Calico's manifest all do);
//   - a script outside the workflow file that a step calls.
package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const uncheckedDownloadMarker = "unchecked-download-ok:"

var (
	downloadCmdRe = regexp.MustCompile(`(^|[\s;&|(` + "`" + `$])(curl|wget)(\s|$)`)
	// A download whose output reaches a shell: `curl ... | bash`, with any
	// number of VAR=value words or a sudo in between.
	pipedToShellRe = regexp.MustCompile(`\|\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+)?(?:/\S*/)?(?:ba|z|da|k)?sh(\s|$)`)
	// curl's and wget's "write it here": -o FILE, -oFILE, --output FILE,
	// --output=FILE, -O FILE (wget).
	outputFileRe  = regexp.MustCompile(`(?:\s-o\s*|\s--output(?:=|\s+)|\s--output-document(?:=|\s+)|\s-O\s+)("[^"]+"|'[^']+'|[^\s-]\S*)`)
	sha256CheckRe = regexp.MustCompile(`(^|[\s|;&])sha256sum\s+(?:\S+\s+)*?(-c|--check)(\s|$)`)
	hex64Re       = regexp.MustCompile(`(^|[^0-9a-f])([0-9a-f]{64})([^0-9a-f]|$)`)
	shellVarRe    = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	kubectlURLRe  = regexp.MustCompile(`(^|[\s;&|(])kubectl\s[^#]*https?://`)
	goModuleRunRe = regexp.MustCompile(`(^|[\s;&|(])go\s+(install|run)\s+(?:-\S+\s+)*\S+@\S+`)
	imageDigestRe = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	full64HexRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// logicalLine is one shell command of a run: script, with backslash
// continuations joined, and whether the comment line directly above it
// carries the exception marker with a reason.
type logicalLine struct {
	text     string
	excepted bool
}

// scriptLines splits a run: script into commands. Comment lines are not
// commands; a marker on one applies to the next command only.
func scriptLines(script string) []logicalLine {
	var out []logicalLine
	var cur strings.Builder
	excepted := false
	for _, raw := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(raw)
		if cur.Len() == 0 {
			if trimmed == "" {
				excepted = false
				continue
			}
			if strings.HasPrefix(trimmed, "#") {
				if i := strings.Index(trimmed, uncheckedDownloadMarker); i >= 0 {
					excepted = strings.TrimSpace(trimmed[i+len(uncheckedDownloadMarker):]) != ""
				}
				continue
			}
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, logicalLine{text: cur.String(), excepted: excepted})
		cur.Reset()
		excepted = false
	}
	if cur.Len() > 0 {
		out = append(out, logicalLine{text: cur.String(), excepted: excepted})
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// assertsAPinnedSum reports whether a `sha256sum -c` line compares against a
// 64-hex value that is written down: in the line, or in env through a
// variable the line names.
func assertsAPinnedSum(line string, env map[string]string) bool {
	if hex64Re.MatchString(line) {
		return true
	}
	for _, m := range shellVarRe.FindAllStringSubmatch(line, -1) {
		if full64HexRe.MatchString(env[m[1]]) {
			return true
		}
	}
	return false
}

// workflowScriptProblems returns what is wrong with one run: script, as
// sentences; nil when nothing is. env is every env: in scope for the step.
func workflowScriptProblems(script string, env map[string]string) []string {
	var problems []string
	lines := scriptLines(script)
	for i, l := range lines {
		if goModuleRunRe.MatchString(l.text) && !l.excepted {
			problems = append(problems, fmt.Sprintf("`%s` builds a tool from a module version; fetch the release binary and assert its sha256 like every other tool", clip(l.text)))
		}
		if kubectlURLRe.MatchString(l.text) && !l.excepted {
			problems = append(problems, fmt.Sprintf("`%s` hands kubectl a URL; download the manifest to a file, assert its sha256, then apply the file", clip(l.text)))
		}
		if !downloadCmdRe.MatchString(l.text) || l.excepted {
			continue
		}
		if pipedToShellRe.MatchString(l.text) {
			problems = append(problems, fmt.Sprintf("`%s` pipes a download into a shell; nothing can be asserted about a script that is already running", clip(l.text)))
			continue
		}
		m := outputFileRe.FindStringSubmatch(" " + l.text)
		if m == nil {
			problems = append(problems, fmt.Sprintf("`%s` downloads without writing a file (-o), so there is nothing a sha256 can be asserted on", clip(l.text)))
			continue
		}
		file := unquote(m[1])
		checked := false
		for _, later := range lines[i+1:] {
			if sha256CheckRe.MatchString(later.text) && strings.Contains(later.text, file) && assertsAPinnedSum(later.text, env) {
				checked = true
				break
			}
		}
		if !checked {
			problems = append(problems, fmt.Sprintf("`%s` downloads %s and no later `sha256sum -c` line of the step asserts a pinned 64-hex sha256 for that file", clip(l.text), file))
		}
	}
	return problems
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}

// stringMap reads a YAML mapping of scalars (an env: block) into dst.
func stringMap(dst map[string]string, v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, val := range m {
		dst[k] = fmt.Sprint(val)
	}
}

// workflowFinding is one problem, with where it is.
type workflowFinding struct{ where, what string }

// workflowDownloadFindings applies every check in this file to one parsed
// workflow. It also returns how many run: scripts and images it looked at, so
// the caller can tell a clean tree from a scan that saw nothing.
func workflowDownloadFindings(name string, doc map[string]any) (findings []workflowFinding, scripts, images int) {
	jobs, _ := doc["jobs"].(map[string]any)
	jobNames := make([]string, 0, len(jobs))
	for j := range jobs {
		jobNames = append(jobNames, j)
	}
	sort.Strings(jobNames)
	for _, jn := range jobNames {
		job, _ := jobs[jn].(map[string]any)
		where := name + " job " + jn

		checkImage := func(kind string, v any) {
			img := ""
			switch x := v.(type) {
			case string:
				img = x
			case map[string]any:
				img, _ = x["image"].(string)
			}
			if img == "" {
				return
			}
			images++
			if !imageDigestRe.MatchString(img) {
				findings = append(findings, workflowFinding{where, fmt.Sprintf("%s image %q is not pinned by digest; a tag is the registry's promise not to move it. Write name:tag@sha256:<64 hex>", kind, img)})
			}
		}
		if c, ok := job["container"]; ok {
			checkImage("container", c)
		}
		if svcs, ok := job["services"].(map[string]any); ok {
			svcNames := make([]string, 0, len(svcs))
			for s := range svcs {
				svcNames = append(svcNames, s)
			}
			sort.Strings(svcNames)
			for _, s := range svcNames {
				checkImage("service "+s, svcs[s])
			}
		}

		steps, _ := job["steps"].([]any)
		for i, st := range steps {
			step, _ := st.(map[string]any)
			script, ok := step["run"].(string)
			if !ok {
				continue
			}
			scripts++
			env := map[string]string{}
			stringMap(env, doc["env"])
			stringMap(env, job["env"])
			stringMap(env, step["env"])
			label := fmt.Sprintf("%s step %d", where, i+1)
			if n, ok := step["name"].(string); ok {
				label = fmt.Sprintf("%s step %q", where, n)
			}
			for _, p := range workflowScriptProblems(script, env) {
				findings = append(findings, workflowFinding{label, p})
			}
		}
	}
	return findings, scripts, images
}

// TestEveryWorkflowDownloadIsCheckedAgainstAPinnedSha256 is the property: the
// workflows as they are in the tree have no finding.
func TestEveryWorkflowDownloadIsCheckedAgainstAPinnedSha256(t *testing.T) {
	root := repoRoot(t)
	scripts, images, downloads := 0, 0, 0
	for _, path := range workflowFiles(t, root) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		findings, s, i := workflowDownloadFindings(filepath.Base(path), doc)
		scripts += s
		images += i
		for _, f := range findings {
			t.Errorf("%s: %s", f.where, f.what)
		}
		for _, l := range scriptLines(string(b)) {
			if downloadCmdRe.MatchString(l.text) {
				downloads++
			}
		}
	}
	// A scan that saw no script, no image or no download would call any tree
	// clean. All three exist today; if one really goes to zero, say so here.
	if scripts == 0 || images == 0 || downloads == 0 {
		t.Fatalf("scanned %d run: scripts, %d service/container images and %d download commands -- one of them is zero, so the scan is broken (or the workflows changed shape and this test must change with them)", scripts, images, downloads)
	}
	t.Logf("run: scripts checked: %d; service/container images: %d; download commands: %d", scripts, images, downloads)
}

// TestTheWorkflowDownloadGateActuallyRejects proves each rule fires on the
// thing it is for, and on the four lines that were in the tree when gonk-5at0
// and gonk-jvuo were filed -- verbatim -- and stays quiet on the shapes the
// workflows use today.
func TestTheWorkflowDownloadGateActuallyRejects(t *testing.T) {
	const sum = "95f14e87aa28c09d5941f11bd024c1d02fdc0303ccaa23f61cef67bc92619d73"
	pinned := map[string]string{"TOOL_SHA256": sum, "TOOL_VERSION": "v1.2.3"}

	good := []struct{ name, script string }{
		{"download, assert through an env var, install", `set -euo pipefail
url="https://example.test/releases/${TOOL_VERSION}/tool.tar.gz"
curl -fsSL "$url" -o tool.tar.gz
echo "${TOOL_SHA256}  tool.tar.gz" | sha256sum -c -
tar xzf tool.tar.gz tool`},
		{"a quoted path under RUNNER_TEMP", `curl -fsSL "$url" -o "${RUNNER_TEMP}/tool"
echo "${TOOL_SHA256}  ${RUNNER_TEMP}/tool" | sha256sum -c -`},
		{"a literal sum and --check", `wget -q -O tool https://example.test/tool
echo "` + sum + `  tool" | sha256sum --check -`},
		{"a data request that says why it is not code", `# unchecked-download-ok: an API answer, parsed as JSON and never executed
http=$(curl -sS -o /tmp/answer.json -w '%{http_code}' \
  -H "Accept: application/json" \
  "https://api.example.test/thing")`},
		{"no download at all", `go test ./...
echo "curl is only a word in this sentence: libcurl"`},
		{"a manifest fetched, checked, then applied", `curl -fsSL "$url" -o calico.yaml
echo "${TOOL_SHA256}  calico.yaml" | sha256sum -c -
kubectl apply -f calico.yaml`},
	}
	for _, c := range good {
		if p := workflowScriptProblems(c.script, pinned); len(p) != 0 {
			t.Errorf("%s: a well-formed script was rejected: %q", c.name, p)
		}
	}

	bad := []struct{ name, script, want string }{
		{"gonk-5at0 verbatim: cosign with no checksum", `cosign version >/dev/null 2>&1 || {
  curl -fsSL -o /tmp/cosign https://github.com/sigstore/cosign/releases/download/v2.4.1/cosign-linux-amd64
  sudo install -m 0755 /tmp/cosign /usr/local/bin/cosign
}`, "no later `sha256sum -c` line"},
		{"gonk-jvuo verbatim: k3d install.sh piped to bash", `curl -fsSL https://raw.githubusercontent.com/k3d-io/k3d/v5.7.4/install.sh | TAG=v5.7.4 bash`, "pipes a download into a shell"},
		{"gonk-jvuo verbatim: kind by go install", `go install sigs.k8s.io/kind@v0.24.0`, "builds a tool from a module version"},
		{"gonk-jvuo verbatim: Calico applied from a URL", `kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml`, "hands kubectl a URL"},
		{"piped to sudo sh", `wget -qO- https://example.test/install | sudo sh -s -- -b /usr/local/bin`, "pipes a download into a shell"},
		{"piped to tar: no file to check", `curl -fsSL https://example.test/tool.tar.gz | tar xz`, "without writing a file"},
		{"the sum is printed, not asserted", `curl -fsSL "$url" -o tool.tar.gz
sha256sum tool.tar.gz`, "no later `sha256sum -c` line"},
		{"another file is checked", `curl -fsSL "$url" -o tool.tar.gz
echo "${TOOL_SHA256}  other.tar.gz" | sha256sum -c -`, "no later `sha256sum -c` line"},
		{"checked against itself", `curl -fsSL "$url" -o tool.tar.gz
echo "$(sha256sum tool.tar.gz | cut -d' ' -f1)  tool.tar.gz" | sha256sum -c -`, "no later `sha256sum -c` line"},
		{"checked against a variable nothing pins", `curl -fsSL "$url" -o tool.tar.gz
echo "${UNSET_SHA256}  tool.tar.gz" | sha256sum -c -`, "no later `sha256sum -c` line"},
		{"the check comes BEFORE the download", `echo "${TOOL_SHA256}  tool.tar.gz" | sha256sum -c -
curl -fsSL "$url" -o tool.tar.gz`, "no later `sha256sum -c` line"},
		{"a marker with no reason excuses nothing", `# unchecked-download-ok:
curl -fsSL -o /tmp/tool https://example.test/tool`, "no later `sha256sum -c` line"},
		{"a marker does not reach past the next command", `# unchecked-download-ok: the first one is data
curl -sS -o /tmp/a.json https://api.example.test/a
curl -fsSL -o /tmp/tool https://example.test/tool`, "downloads /tmp/tool"},
	}
	for _, c := range bad {
		p := workflowScriptProblems(c.script, pinned)
		if len(p) != 1 || !strings.Contains(p[0], c.want) {
			t.Errorf("%s: got %q, want exactly one problem containing %q", c.name, p, c.want)
		}
	}

	// The image rule, through the same entry point the real test uses.
	doc := func(src string) map[string]any {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(src), &d); err != nil {
			t.Fatalf("fixture does not parse: %v", err)
		}
		return d
	}
	const digest = "@sha256:ca0bd484cb98bf4b24eb1010e73fb3fcbd6714d240fbc1a10eea5b7dbecb641d"
	findings, _, images := workflowDownloadFindings("fixture.yml", doc(`
jobs:
  a:
    container: golang:1.26
    services:
      postgres:
        image: postgres:16
      cache:
        image: redis:7`+digest+`
    steps:
      - uses: example/action@v1
        with:
          image: not-a-service-image:latest
  b:
    container:
      image: debian:trixie-slim`+digest+`
`))
	if images != 4 {
		t.Errorf("looked at %d images, want 4 (two containers, two services; a `with: image:` is an action input, not an image this pulls)", images)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.what)
	}
	if len(got) != 2 || !strings.Contains(got[0], `"golang:1.26"`) || !strings.Contains(got[1], `"postgres:16"`) {
		t.Errorf("image findings = %q; want exactly the tag-only container golang:1.26 and the tag-only service postgres:16 (gonk-jvuo verbatim)", got)
	}

	// env: is merged workflow, then job, then step, so a sum pinned at any
	// level counts and an inner level wins.
	findings, scripts, _ := workflowDownloadFindings("fixture.yml", doc(`
env:
  A_SHA256: `+sum+`
jobs:
  a:
    env:
      B_SHA256: `+sum+`
    steps:
      - run: |
          curl -fsSL -o a https://example.test/a
          echo "${A_SHA256}  a" | sha256sum -c -
          curl -fsSL -o b https://example.test/b
          echo "${B_SHA256}  b" | sha256sum -c -
      - name: the step unpins what the workflow pinned
        env:
          A_SHA256: not-a-sum
        run: |
          curl -fsSL -o a https://example.test/a
          echo "${A_SHA256}  a" | sha256sum -c -
`))
	if scripts != 2 || len(findings) != 1 || !strings.Contains(findings[0].where, "the step unpins") {
		t.Errorf("scripts=%d findings=%+v; want 2 scripts and one finding, in the step whose env overrides the pinned sum", scripts, findings)
	}
}
