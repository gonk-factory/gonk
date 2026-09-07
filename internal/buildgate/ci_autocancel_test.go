package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// reservedTopLevel are the .gitlab-ci.yml keys that configure the pipeline
// itself. Every OTHER top-level mapping key declares a job -- including the
// hidden `.templates`, which matter here because a template that opted out of
// interruptibility would take every job extending it out with it.
var reservedTopLevel = map[string]bool{
	"default":       true,
	"include":       true,
	"stages":        true,
	"variables":     true,
	"workflow":      true,
	"spec":          true,
	"image":         true,
	"services":      true,
	"before_script": true,
	"after_script":  true,
	"cache":         true,
}

func ciConfig(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse .gitlab-ci.yml: %v", err)
	}
	return doc
}

func jobNames(doc map[string]any) []string {
	var names []string
	for k, v := range doc {
		if reservedTopLevel[k] {
			continue
		}
		if _, ok := v.(map[string]any); !ok {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// autoCancelProblems reports why a parsed pipeline would NOT cancel its own
// superseded pipelines. Pure, so the negative control below can prove it fails.
func autoCancelProblems(doc map[string]any) []string {
	var problems []string

	wf, ok := doc["workflow"].(map[string]any)
	if !ok {
		return []string{"no `workflow:` block, so auto-cancel is left to a project setting nobody can review in a diff"}
	}
	ac, ok := wf["auto_cancel"].(map[string]any)
	if !ok {
		return []string{"`workflow:` has no `auto_cancel:` block"}
	}
	if got := ac["on_new_commit"]; got != "conservative" && got != "interruptible" {
		problems = append(problems, fmt.Sprintf(
			"workflow.auto_cancel.on_new_commit is %v; it must be `conservative` or `interruptible`, "+
				"or a superseded pipeline keeps its place in the gonk-images-$BUILD_ARCH queue (gonk-7n2)", got))
	}
	if got := ac["on_job_failure"]; got != "all" {
		problems = append(problems, fmt.Sprintf(
			"workflow.auto_cancel.on_job_failure is %v, not `all`; one failed kaniko build already makes the "+
				"commit's tag set undeployable, so the remaining builds are an hour of johnny and bailey "+
				"spent on images nothing can pull", got))
	}
	return problems
}

// interruptibleProblems reports every way a job could still protect a
// superseded pipeline from cancellation.
func interruptibleProblems(doc map[string]any) []string {
	var problems []string

	def, _ := doc["default"].(map[string]any)
	if v, ok := def["interruptible"]; !ok || v != true {
		problems = append(problems, "`default:` does not set `interruptible: true`, so any job that "+
			"does not say so itself defaults to interruptible: false and re-protects the pipeline it is in")
	}

	for _, name := range jobNames(doc) {
		job := doc[name].(map[string]any)
		v, ok := job["interruptible"]
		if !ok {
			continue // inherits the default, which is asserted above
		}
		if v != true {
			problems = append(problems, fmt.Sprintf(
				"job %q sets `interruptible: %v`. Under workflow.auto_cancel.on_new_commit=conservative one "+
					"such job, once STARTED, protects the whole superseded pipeline -- and it keeps holding "+
					"resource_group gonk-images-$BUILD_ARCH while it does (gonk-7n2)", name, v))
		}
	}
	return problems
}

// A burst of commits used to cost an hour: image jobs serialise on
// resource_group gonk-images-$BUILD_ARCH ACROSS pipelines, so three superseded
// pipelines had to be cancelled by hand before the wanted build could start
// (gonk-7n2, measured 2026-09-07). These two gates keep that automatic.
func TestSupersededPipelinesCancelThemselves(t *testing.T) {
	for _, p := range autoCancelProblems(ciConfig(t)) {
		t.Error(p)
	}
}

func TestEveryJobIsInterruptible(t *testing.T) {
	doc := ciConfig(t)

	// Guard the guard: if the job-detection heuristic ever stops finding jobs,
	// the loop above passes vacuously.
	if names := jobNames(doc); len(names) < 10 {
		t.Fatalf("only found %d jobs in .gitlab-ci.yml (%v) -- the reserved-key list is probably stale "+
			"and this gate is passing vacuously", len(names), names)
	}

	for _, p := range interruptibleProblems(doc) {
		t.Error(p)
	}
}

// Negative controls. A gate that cannot fail is not a gate; both of the above
// are greps over YAML, and the failure mode of a grep is silently matching
// nothing forever.
func TestTheAutoCancelGateActuallyFails(t *testing.T) {
	cases := map[string]string{
		"no workflow block": "stages: [lint]\nlint:\n  script: [true]\n",
		"auto-cancel off":   "workflow:\n  auto_cancel:\n    on_new_commit: none\n    on_job_failure: all\n",
		"failure not fatal": "workflow:\n  auto_cancel:\n    on_new_commit: conservative\n    on_job_failure: none\n",
	}
	for name, src := range cases {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
			t.Fatalf("%s: fixture does not parse: %v", name, err)
		}
		if problems := autoCancelProblems(doc); len(problems) == 0 {
			t.Errorf("%s: autoCancelProblems accepted a pipeline that would not auto-cancel:\n%s", name, src)
		}
	}
}

func TestTheInterruptibleGateActuallyFails(t *testing.T) {
	src := strings.Join([]string{
		"default:",
		"  interruptible: true",
		"lint:",
		"  interruptible: false",
		"  script: [true]",
	}, "\n") + "\n"

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	problems := interruptibleProblems(doc)
	if len(problems) == 0 {
		t.Fatal("interruptibleProblems accepted a job with `interruptible: false`")
	}
	if !strings.Contains(strings.Join(problems, "\n"), `job "lint"`) {
		t.Errorf("the opted-out job was not named in the failure: %v", problems)
	}
}
