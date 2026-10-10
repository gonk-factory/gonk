// The release-order gate (release-unit v2, docs/plans/2026-10-02-release-unit-v2.md).
//
// gitops follows ONE ImagePolicy, on gonk-controller, and the tag it selects
// sets every gonk image (chart `releaseTag`). That is only safe if, whenever a
// controller tag exists, the agent, intake and meter tags of the SAME name
// already exist. Two things in .gitlab-ci.yml make that true, and this file
// pins both:
//
//  1. controller-index `needs:` the other three index jobs (and its own two
//     arch legs), so the controller index is published LAST in a build
//     pipeline, and not at all if any of the others fails. Before this, the
//     order was the resource_group's: pipeline IID 499 published the
//     controller 8.5 minutes before the meter (verifier R1).
//  2. deployable-image-guard runs after controller-index on every
//     default-branch pipeline that builds, asserts BY FIELD that all four
//     images exist at the pipeline's tag, is not allowed to fail, and carries
//     the `cluster` runner tag (a second, untagged runner cannot resolve the
//     in-cluster registry Service -- 171ada4, verifier R2).
//
// The guard's shell is EXECUTED here against a fake crane rather than restated
// in Go, for the reason ci_tag_shape_test.go gives: a Go copy of the logic
// would pass even if the shipped shell were inverted.
package buildgate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// releaseImages is THE RELEASE: everything the chart deploys or hands to a
// pod. Not the e2e-only testclock meter.
var releaseImages = []string{"gonk-agent", "gonk-controller", "gonk-intake", "gonk-meter"}

// releaseIndexJobs are the index jobs the controller index must wait for.
var releaseIndexJobs = []string{"agent-index", "intake-index", "meter-index"}

const (
	controllerIndexJob = "controller-index"
	guardJob           = "deployable-image-guard"
)

// jobNeeds returns a job's `needs:` as job names, accepting both the string
// form and the `{job: name}` map form GitLab allows.
func jobNeeds(job map[string]any) []string {
	raw, _ := job["needs"].([]any)
	var out []string
	for _, n := range raw {
		switch v := n.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			out = append(out, fmt.Sprint(v["job"]))
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// extendsList returns a job's `extends:` as a list.
func extendsList(job map[string]any) []string {
	switch v := job["extends"].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// releaseOrderProblems reports every way the controller index could be
// published before (or without) the other three release images. Pure, so the
// negative control can prove it fails.
func releaseOrderProblems(doc map[string]any) []string {
	var problems []string
	ctrl, ok := doc[controllerIndexJob].(map[string]any)
	if !ok {
		return []string{"no `controller-index` job in .gitlab-ci.yml -- renamed? this gate (and the gitops ImagePolicy it protects) must follow it"}
	}
	needs := jobNeeds(ctrl)
	for _, want := range append([]string{"controller-image-amd64", "controller-image-arm64"}, releaseIndexJobs...) {
		if !contains(needs, want) {
			problems = append(problems, fmt.Sprintf(
				"controller-index does not `needs: %s` (needs: %v). The controller tag is what gitops' ONE "+
					"ImagePolicy follows, so it must be published LAST: without this it can exist before the "+
					"other images at the same tag and Flux deploys a tag they lack (ImagePullBackOff, the "+
					"2026-09-07 shape; pipeline IID 499 measured the controller 8.5 min before the meter)", want, needs))
		}
	}
	for _, n := range needs {
		if strings.Contains(n, "testclock") {
			problems = append(problems, fmt.Sprintf(
				"controller-index needs %q. The testclock meter is e2e-only and never deployed; putting it on "+
					"the release path makes a testclock failure withhold a release it is not part of", n))
		}
	}
	// A `needs:` across a rules boundary is how a job silently never runs (or
	// a pipeline fails to create). Every job on the path must take its rules
	// from .gonk-index and must not override them.
	for _, name := range append([]string{controllerIndexJob}, releaseIndexJobs...) {
		job, ok := doc[name].(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("no %q job -- controller-index depends on it", name))
			continue
		}
		if !contains(extendsList(job), ".gonk-index") {
			problems = append(problems, fmt.Sprintf("%q does not extend .gonk-index, so it may not share the build rules controller-index's `needs:` assumes", name))
		}
		if _, own := job["rules"]; own {
			problems = append(problems, fmt.Sprintf("%q overrides `rules:`; every index job on the release path must share *gonk_image_rules or the `needs:` crosses a rules boundary", name))
		}
	}
	return problems
}

// guardJobProblems reports every way deployable-image-guard could fail to
// guard a default-branch build.
func guardJobProblems(doc map[string]any) []string {
	job, ok := doc[guardJob].(map[string]any)
	if !ok {
		return []string{"no `deployable-image-guard` job in .gitlab-ci.yml"}
	}
	var problems []string

	if !contains(jobNeeds(job), controllerIndexJob) {
		problems = append(problems, fmt.Sprintf(
			"deployable-image-guard does not `needs: controller-index` (needs: %v), so it can run before "+
				"the last tag is published and pass or fail on a race", jobNeeds(job)))
	}
	for _, n := range jobNeeds(job) {
		if strings.Contains(n, "testclock") {
			problems = append(problems, fmt.Sprintf("deployable-image-guard needs %q; the testclock meter is not part of the release", n))
		}
	}

	if v, ok := job["allow_failure"]; ok && v != false {
		problems = append(problems, fmt.Sprintf(
			"deployable-image-guard sets `allow_failure: %v`. A guard that cannot fail main passes every other "+
				"check here and guards nothing (verifier C4)", v))
	}
	tags, _ := job["tags"].([]any)
	hasCluster := false
	for _, tg := range tags {
		if tg == "cluster" {
			hasCluster = true
		}
	}
	if !hasCluster {
		problems = append(problems, fmt.Sprintf(
			"deployable-image-guard has `tags: %v`, not `[cluster]`. It dials the in-cluster registry Service, "+
				"which the untagged runner (tak-docker-desktop) cannot resolve -- main would go red at random (171ada4, verifier R2)", tags))
	}

	rules, _ := job["rules"].([]any)
	if len(rules) == 0 {
		problems = append(problems, "deployable-image-guard has no `rules:`; it must be scoped to default-branch builds")
	}
	sawBranchExclusion, sawInputs := false, false
	for i, r := range rules {
		rule, _ := r.(map[string]any)
		if v, ok := rule["allow_failure"]; ok && v != false {
			problems = append(problems, fmt.Sprintf("deployable-image-guard rule %d sets `allow_failure: %v`", i, v))
		}
		if w := rule["when"]; w == "manual" {
			problems = append(problems, fmt.Sprintf("deployable-image-guard rule %d is `when: manual`; an unplayed guard guards nothing", i))
		}
		if rule["if"] == "$CI_COMMIT_BRANCH != $CI_DEFAULT_BRANCH" && rule["when"] == "never" {
			if i != 0 {
				problems = append(problems, "the non-default-branch exclusion is not deployable-image-guard's FIRST rule, so an earlier rule can run it on a branch")
			}
			sawBranchExclusion = true
		}
		if ch, ok := rule["changes"]; ok && rule["when"] != "never" {
			sawInputs = true
			if want := imageChangePathsAny(doc); !reflect.DeepEqual(ch, want) {
				problems = append(problems, fmt.Sprintf(
					"deployable-image-guard's `changes:` (%v) differs from the image inputs (%v); it must run on "+
						"exactly the pipelines that build, so share the &gonk_image_inputs anchor", ch, want))
			}
		}
	}
	if !sawBranchExclusion {
		problems = append(problems, "deployable-image-guard does not exclude non-default branches first (`if: '$CI_COMMIT_BRANCH != $CI_DEFAULT_BRANCH'` + `when: never`)")
	}
	if !sawInputs {
		problems = append(problems, "deployable-image-guard has no rule keyed on the image inputs, so it does not run exactly when main builds")
	}

	vars, _ := job["variables"].(map[string]any)
	got := strings.Fields(fmt.Sprint(vars["GUARD_IMAGES"]))
	sort.Strings(got)
	if !reflect.DeepEqual(got, releaseImages) {
		problems = append(problems, fmt.Sprintf("deployable-image-guard checks GUARD_IMAGES=%v; the release is exactly %v", got, releaseImages))
	}
	return problems
}

// imageChangePathsAny is the raw `changes:` list from .gonk-kaniko's first
// rule, in the parsed form (for an exact comparison).
func imageChangePathsAny(doc map[string]any) any {
	job, _ := doc[".gonk-kaniko"].(map[string]any)
	rules, _ := job["rules"].([]any)
	for _, r := range rules {
		if rule, ok := r.(map[string]any); ok {
			if ch, ok := rule["changes"]; ok {
				return ch
			}
		}
	}
	return nil
}

func TestControllerIndexIsPublishedLast(t *testing.T) {
	for _, p := range releaseOrderProblems(ciConfig(t)) {
		t.Error(p)
	}
}

func TestDeployableImageGuardGuardsEveryMainBuild(t *testing.T) {
	for _, p := range guardJobProblems(ciConfig(t)) {
		t.Error(p)
	}
}

// The testclock meter was never on the release path. Pin that it stays off
// it, and that its own index still depends only on its own legs.
func TestTestclockMeterIsNotOnTheReleasePath(t *testing.T) {
	doc := ciConfig(t)
	tc, ok := doc["meter-testclock-index"].(map[string]any)
	if !ok {
		t.Fatal("no meter-testclock-index job -- renamed? update this gate")
	}
	want := []string{"meter-testclock-image-amd64", "meter-testclock-image-arm64"}
	if got := jobNeeds(tc); !reflect.DeepEqual(got, want) {
		t.Errorf("meter-testclock-index needs %v, want exactly %v", got, want)
	}
	for _, name := range []string{controllerIndexJob, guardJob} {
		job, _ := doc[name].(map[string]any)
		for _, n := range jobNeeds(job) {
			if strings.Contains(n, "testclock") {
				t.Errorf("%s needs %q; the testclock meter is not part of the release", name, n)
			}
		}
	}
	job, _ := doc[guardJob].(map[string]any)
	vars, _ := job["variables"].(map[string]any)
	if strings.Contains(fmt.Sprint(vars["GUARD_IMAGES"]), "testclock") {
		t.Errorf("deployable-image-guard checks a testclock image: %v", vars["GUARD_IMAGES"])
	}
}

func TestTheReleaseOrderGateActuallyFails(t *testing.T) {
	mutate := map[string]func(doc map[string]any){
		"controller-index needs only its legs": func(doc map[string]any) {
			doc[controllerIndexJob].(map[string]any)["needs"] = []any{"controller-image-amd64", "controller-image-arm64"}
		},
		"controller-index drops meter-index": func(doc map[string]any) {
			doc[controllerIndexJob].(map[string]any)["needs"] = []any{"controller-image-amd64", "controller-image-arm64", "agent-index", "intake-index"}
		},
		"controller-index needs testclock": func(doc map[string]any) {
			j := doc[controllerIndexJob].(map[string]any)
			j["needs"] = append(j["needs"].([]any), "meter-testclock-index")
		},
		"intake-index overrides rules": func(doc map[string]any) {
			doc["intake-index"].(map[string]any)["rules"] = []any{map[string]any{"when": "always"}}
		},
		"no controller-index": func(doc map[string]any) { delete(doc, controllerIndexJob) },
	}
	for name, m := range mutate {
		doc := ciConfig(t)
		m(doc)
		if len(releaseOrderProblems(doc)) == 0 {
			t.Errorf("%s: releaseOrderProblems accepted it", name)
		}
	}
}

func TestTheGuardJobGateActuallyFails(t *testing.T) {
	mutate := map[string]func(job map[string]any){
		"allow_failure true":   func(j map[string]any) { j["allow_failure"] = true },
		"no cluster tag":       func(j map[string]any) { delete(j, "tags") },
		"needs nothing":        func(j map[string]any) { j["needs"] = []any{} },
		"needs testclock too":  func(j map[string]any) { j["needs"] = []any{"controller-index", "meter-testclock-index"} },
		"manual":               func(j map[string]any) { j["rules"] = []any{map[string]any{"when": "manual"}} },
		"runs on every branch": func(j map[string]any) { j["rules"] = []any{map[string]any{"changes": []any{"go.mod"}}} },
		"drops an image": func(j map[string]any) {
			j["variables"].(map[string]any)["GUARD_IMAGES"] = "gonk-agent gonk-controller gonk-intake"
		},
		"rule allow_failure":  func(j map[string]any) { j["rules"].([]any)[1].(map[string]any)["allow_failure"] = true },
		"inputs list drifted": func(j map[string]any) { j["rules"].([]any)[1].(map[string]any)["changes"] = []any{"go.mod"} },
		"no guard job":        nil,
	}
	for name, m := range mutate {
		doc := ciConfig(t)
		if m == nil {
			delete(doc, guardJob)
		} else {
			m(doc[guardJob].(map[string]any))
		}
		if len(guardJobProblems(doc)) == 0 {
			t.Errorf("%s: guardJobProblems accepted it", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The guard's shell, executed against a fake crane.
// ---------------------------------------------------------------------------

// fakeCrane answers `crane ls --insecure <repo>/<image>` from <dir>/<image>.tags,
// failing first <dir>/<image>.failures times, and logs every call.
const fakeCrane = `#!/bin/sh
echo "$*" >> "$FAKE_CRANE_DIR/calls"
[ "$1" = ls ] || { echo "fake crane: unexpected subcommand $1" >&2; exit 99; }
img=${3##*/}
f="$FAKE_CRANE_DIR/$img.failures"
if [ -f "$f" ]; then
  n=$(cat "$f")
  if [ "$n" -gt 0 ]; then
    echo $((n - 1)) > "$f"
    echo "fake crane: listing $img failed" >&2
    exit 1
  fi
fi
[ -f "$FAKE_CRANE_DIR/$img.tags" ] || { echo "fake crane: no such repo $img" >&2; exit 1; }
cat "$FAKE_CRANE_DIR/$img.tags"
`

type guardCase struct {
	tags     map[string]string // image -> listing
	failures map[string]int    // image -> failed listings before success
	gonkTag  string
}

func guardScript(t *testing.T) (script string, vars map[string]any) {
	t.Helper()
	job, ok := ciConfig(t)[guardJob].(map[string]any)
	if !ok {
		t.Fatal("no deployable-image-guard job")
	}
	steps, _ := job["script"].([]any)
	if len(steps) != 1 {
		t.Fatalf("deployable-image-guard has %d script steps; this test runs exactly one", len(steps))
	}
	vars, _ = job["variables"].(map[string]any)
	return fmt.Sprint(steps[0]), vars
}

func runGuard(t *testing.T, c guardCase) (exit int, out string, calls []string) {
	t.Helper()
	script, vars := guardScript(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crane"), []byte(fakeCrane), 0o755); err != nil {
		t.Fatal(err)
	}
	for img, l := range c.tags {
		if err := os.WriteFile(filepath.Join(dir, img+".tags"), []byte(l), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for img, n := range c.failures {
		if err := os.WriteFile(filepath.Join(dir, img+".failures"), []byte(fmt.Sprint(n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"FAKE_CRANE_DIR=" + dir,
		"TMPDIR=" + dir,
		"INTERNAL_REGISTRY_REPO=gitlab-registry.example:5000/agentic/gonk-project",
		"GONK_TAG=" + c.gonkTag,
		"GUARD_IMAGES=" + fmt.Sprint(vars["GUARD_IMAGES"]),
		"GUARD_LIST_ATTEMPTS=" + fmt.Sprint(vars["GUARD_LIST_ATTEMPTS"]),
		"GUARD_RETRY_SLEEP=0", // the only override: the shipped backoff, at zero cost
	}
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the guard: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "calls")); err == nil {
		calls = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return exit, string(b), calls
}

const relTag = "v0.1.0-510.0123456789ab"

// decoys: the per-arch legs, the testclock meter and a neighbour build sit
// next to the real tag in every real repository.
func listing(withTag bool) string {
	l := []string{relTag + "-amd64", relTag + "-arm64", relTag + "-testclock", "v0.1.0-509.ba9876543210", "v0.1.0-51.0123456789ab"}
	if withTag {
		l = append(l, relTag)
	}
	return strings.Join(l, "\n") + "\n"
}

func allFour(with bool) map[string]string {
	m := map[string]string{}
	for _, img := range releaseImages {
		m[img] = listing(with)
	}
	return m
}

func TestGuardPassesOnACompleteRelease(t *testing.T) {
	exit, out, calls := runGuard(t, guardCase{tags: allFour(true), gonkTag: relTag})
	if exit != 0 {
		t.Fatalf("complete release: exit %d, want 0\n%s", exit, out)
	}
	for _, img := range releaseImages {
		if !strings.Contains(out, "PRESENT "+img+":"+relTag) {
			t.Errorf("no PRESENT line for %s\n%s", img, out)
		}
	}
	if len(calls) != len(releaseImages) {
		t.Errorf("listed %d times, want once per image (%d): %v", len(calls), len(releaseImages), calls)
	}
}

func TestGuardFailsWhenOneImageLacksTheTag(t *testing.T) {
	for _, img := range releaseImages {
		tags := allFour(true)
		tags[img] = listing(false) // only the -amd64/-arm64/-testclock decoys
		exit, out, _ := runGuard(t, guardCase{tags: tags, gonkTag: relTag})
		if exit != 1 {
			t.Errorf("%s lacks %s (decoys only): exit %d, want 1\n%s", img, relTag, exit, out)
		}
		if !strings.Contains(out, "ABSENT  "+img+":"+relTag) {
			t.Errorf("%s: no ABSENT line\n%s", img, out)
		}
	}
}

// A tag one character short is a PREFIX of every real tag line: only an exact
// whole-line match may count.
func TestGuardMatchesTheWholeTagOnly(t *testing.T) {
	exit, out, _ := runGuard(t, guardCase{tags: allFour(true), gonkTag: relTag[:len(relTag)-1]})
	if exit != 1 {
		t.Errorf("tag one char short: exit %d, want 1\n%s", exit, out)
	}
	exit, out, _ = runGuard(t, guardCase{tags: allFour(true), gonkTag: "v0.1.0-510.0123456789a."})
	if exit != 1 {
		t.Errorf("tag with a regex metachar: exit %d, want 1 (fixed-string match)\n%s", exit, out)
	}
}

// Verifier R10: one failed listing must not red main.
func TestGuardRetriesAFailedListing(t *testing.T) {
	exit, out, calls := runGuard(t, guardCase{
		tags:     allFour(true),
		failures: map[string]int{"gonk-meter": 4},
		gonkTag:  relTag,
	})
	if exit != 0 {
		t.Fatalf("meter listing failed 4 times then succeeded: exit %d, want 0\n%s", exit, out)
	}
	if n := countCalls(calls, "gonk-meter"); n != 5 {
		t.Errorf("meter listed %d times, want 5", n)
	}
}

func TestGuardGivesUpAfterBoundedAttempts(t *testing.T) {
	_, vars := guardScript(t)
	attempts := fmt.Sprint(vars["GUARD_LIST_ATTEMPTS"])
	exit, out, calls := runGuard(t, guardCase{
		tags:     allFour(true),
		failures: map[string]int{"gonk-intake": 1000},
		gonkTag:  relTag,
	})
	if exit != 2 {
		t.Fatalf("intake never lists: exit %d, want 2\n%s", exit, out)
	}
	if got := fmt.Sprint(countCalls(calls, "gonk-intake")); got != attempts {
		t.Errorf("intake listed %s times, want GUARD_LIST_ATTEMPTS=%s", got, attempts)
	}
}

func TestGuardRefusesAnEmptyListing(t *testing.T) {
	tags := allFour(true)
	tags["gonk-agent"] = ""
	exit, out, _ := runGuard(t, guardCase{tags: tags, gonkTag: relTag})
	if exit != 2 {
		t.Errorf("empty agent listing: exit %d, want 2 (unreadable, not 'absent' and never 'present')\n%s", exit, out)
	}
}

func TestGuardRefusesAnEmptyTag(t *testing.T) {
	exit, out, calls := runGuard(t, guardCase{tags: allFour(true), gonkTag: ""})
	// Refused up front, before any listing: an empty tag is a broken tag
	// computation, not an absent image, and must say so.
	if exit != 1 || !strings.Contains(out, "GONK_TAG is empty") || len(calls) != 0 {
		t.Errorf("empty GONK_TAG: exit %d, %d listing(s); want exit 1, no listing, 'GONK_TAG is empty'\n%s", exit, len(calls), out)
	}
}

func countCalls(calls []string, img string) int {
	n := 0
	for _, c := range calls {
		if strings.HasSuffix(c, "/"+img) {
			n++
		}
	}
	return n
}
