package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-cuwn part X-CONFIG: "everything default OFF, so every existing
// .gonk.yml and operator config resolves exactly as today".
//
// testdata/golden/existing_configs_before.json was written by THIS test, with
// UPDATE_GOLDEN_BEFORE=1, at the commit that introduced the test and changed
// no production code (the parent of every X-CONFIG code commit). It is the
// project response -- state, effective config, budget -- that gonk-meter gave
// each configuration in the corpus below BEFORE the change/review surface
// existed. It must never be regenerated: a difference is a behaviour change
// for a project that asked for nothing.
//
// The comparison removes the keys X-CONFIG ADDS to the response
// (additiveKeys) and requires everything else to be byte-identical. The added
// keys' own values for these configurations are pinned separately, in
// testdata/golden/existing_configs_added.json (UPDATE_GOLDEN=1).

// additiveKeys are the response paths added by X-CONFIG. A path here is
// removed before the comparison with the "before" golden; nothing else is.
var additiveKeys = [][]string{
	{"effective", "actions", "reviews"},
	{"effective", "change"},
	{"effective", "review"},
	{"effective", "pipeline_fix", "auto_merge", "max_per_day"},
}

const existingOperatorTight = `
version: 1
instance:
  enabled: true
  ladder: [qwen-local, glm, sonnet]
  actions: { features: false }
  budget: { monthly_cost_usd: 200, monthly_tokens: "80M" }
  scope: { caps: { max_files: 2 }, deny_lift: [ci] }
  pipeline_fix: { classes: [code], max_attempts: 1, auto_merge: { mode: "off" } }
cloud_allowance:
  enabled: true
groups:
  kubernetes:
    budget: { monthly_cost_usd: 50 }
    actions: { pipeline_fixes: false }
  kubernetes/experiments:
    pipeline_fix: { max_chain: 2 }
  other:
    pipeline_fix: { verify_timeout: 30m, auto_merge: { mode: after-human-approval, classes: [docs] } }
rungs:
  - { name: qwen-local, kind: local, model: qwen3-coder-30b, est_cost_usd: 0,    est_tokens: "50K",  synthetic_usd_per_1m_tokens: 1.0 }
  - { name: glm,        kind: cloud, model: glm-5,           est_cost_usd: 0.40, est_tokens: "200K" }
  - { name: sonnet,     kind: cloud, model: claude-sonnet,   est_cost_usd: 2.00, est_tokens: "100K" }
meter:
  max_spend_staleness: 5m
  reservation_ttl: 60m
`

type existingCase struct {
	name, operator, project, yaml string
}

func existingCorpus(t *testing.T) []existingCase {
	t.Helper()
	onboarding, err := os.ReadFile(filepath.Join("testdata", "existing", "onboarding-at-8037e70.gonk.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const head = "version: 1\nenabled: true\nladder: [qwen-local, glm, sonnet]\nbudget: { monthly_cost_usd: 5 }\n"
	fullFix := head + "actions: { triage: true, pipelines: true, pipeline_fixes: true, features: true, scaffold: true }\n" +
		"pipeline_fix:\n  classes: [code, test, configuration]\n  targets: [ref, mr]\n  allow_ci: true\n  ci_exec_changes: auto\n" +
		"  max_attempts: 3\n  max_chain: 9\n  verify_timeout: 45m\n  max_files: 2\n  max_changed_lines: 12\n  paths: [\"internal/**\"]\n" +
		"  auto_merge: { mode: after-human-approval, classes: [docs, ci] }\n" +
		"scope: { classes: [source, test, docs], caps: { max_files: 3 }, protected_paths: [\"internal/auth/**\"], profiles: [go, python, docs-md] }\n" +
		"schedule: { quiet_hours: \"22:00-06:00\", timezone: UTC }\ncontinuity: fresh\n" +
		"triage: { label_prefix: \"bot::\", respond_to_mentions: false }\nprovenance: { commit_trailers: false, include_usage: true }\n"
	cases := []existingCase{}
	for _, op := range []struct{ name, yaml string }{{"plain-operator", testOperatorYAML}, {"tight-operator", existingOperatorTight}} {
		for _, p := range []struct{ name, project, yaml string }{
			{"onboarding-default", "solo/repo", string(onboarding)},
			{"triage-only", "solo/repo", simpleYAML("qwen-local, glm", 10)},
			{"nothing-opted-in", "solo/repo", "version: 1\nenabled: true\nladder: [qwen-local]\n"},
			{"disabled", "solo/repo", "version: 1\nenabled: false\nladder: [qwen-local]\nactions: { triage: true, pipelines: true }\n"},
			{"pipelines-only", "kubernetes/kubectl", head + "actions: { triage: true, pipelines: true }\n"},
			{"features-on", "solo/repo", head + "actions: { triage: true, features: true }\n"},
			{"fixes-defaults", "solo/repo", head + "actions: { pipelines: true, pipeline_fixes: true }\n"},
			{"fixes-everything-set", "solo/repo", fullFix},
			{"fixes-everything-set-under-a-vetoing-group", "kubernetes/kubectl", fullFix},
			{"fixes-under-a-nested-group", "kubernetes/experiments/kubectl", fullFix},
			{"fixes-under-a-group-naming-a-merge-mode", "other/repo", fullFix},
			{"fixes-agentic-review-mode", "other/repo", head + "actions: { pipelines: true, pipeline_fixes: true }\npipeline_fix: { auto_merge: { mode: after-agentic-review } }\n"},
			{"staged-fix-section", "solo/repo", head + "actions: { triage: true }\npipeline_fix: { classes: [], max_files: 1, auto_merge: { mode: after-human-approval } }\n"},
			{"unlimited-budget", "solo/repo", "version: 1\nenabled: true\nladder: [qwen-local]\nactions: { triage: true }\n"},
			{"empty-ladder-intersection", "solo/repo", "version: 1\nenabled: true\nladder: [nope]\nactions: { triage: true }\n"},
			{"invalid: fixes without pipelines", "solo/repo", head + "actions: { pipeline_fixes: true }\n"},
			{"invalid: unknown top-level key", "solo/repo", head + "banana: true\n"},
		} {
			cases = append(cases, existingCase{op.name + " / " + p.name, op.yaml, p.project, p.yaml})
		}
	}
	return cases
}

// existingResponses registers every configuration through the real service
// and returns name -> the response as generic JSON.
func existingResponses(t *testing.T) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, c := range existingCorpus(t) {
		f := newTestServiceWithConfig(t, c.operator)
		resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
			Project: c.project, ProjectID: 1, Rig: strings.ReplaceAll(c.project, "/", "-"), GonkYML: c.yaml,
		})
		if err != nil {
			t.Fatalf("%s: Register: %v", c.name, err)
		}
		raw, err := json.Marshal(struct {
			Status   int                      `json:"http_status"`
			Response meterapi.ProjectResponse `json:"response"`
		}{status, resp})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatal(err)
		}
		out[c.name] = generic
	}
	return out
}

func removePath(v any, path []string) (removed any, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, false
	}
	if len(path) == 1 {
		removed, ok = m[path[0]]
		delete(m, path[0])
		return removed, ok
	}
	return removePath(m[path[0]], path[1:])
}

func TestExistingConfigsResolveByteIdentically(t *testing.T) {
	got := existingResponses(t)
	if len(got) < 30 {
		t.Fatalf("the corpus has only %d configurations", len(got))
	}
	added := map[string]any{}
	valid := 0
	for name, resp := range got {
		r := resp.(map[string]any)["response"]
		perCase := map[string]any{}
		for _, p := range additiveKeys {
			if v, ok := removePath(r, p); ok {
				perCase[strings.Join(p, ".")] = v
			}
		}
		if r.(map[string]any)["effective"] != nil {
			valid++
			// Every resolved project states every added key: none is omitted.
			if len(perCase) != len(additiveKeys) && os.Getenv("UPDATE_GOLDEN_BEFORE") != "1" {
				t.Errorf("%s: the response states %d of the %d added keys: %v", name, len(perCase), len(additiveKeys), perCase)
			}
		}
		added[name] = perCase
	}
	if valid < 20 {
		t.Fatalf("only %d configurations in the corpus resolved", valid)
	}

	compare := func(file string, v any, env string) {
		t.Helper()
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, '\n')
		path := filepath.Join("testdata", "golden", file)
		if os.Getenv(env) == "1" {
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bytes.Equal(raw, want) {
			t.Errorf("%s differs.\n--- got ---\n%s\n--- want ---\n%s", file, firstDiff(raw, want), firstDiff(want, raw))
		}
	}
	// What existed before, byte for byte. NEVER regenerate this file.
	compare("existing_configs_before.json", got, "UPDATE_GOLDEN_BEFORE")
	// What X-CONFIG added, for the same configurations: all of it OFF.
	compare("existing_configs_added.json", added, "UPDATE_GOLDEN")
}

// firstDiff returns the lines of a around its first difference from b.
func firstDiff(a, b []byte) string {
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := range al {
		if i >= len(bl) || al[i] != bl[i] {
			return strings.Join(al[max(i-6, 0):min(i+6, len(al))], "\n")
		}
	}
	return "(a prefix of the other)"
}
