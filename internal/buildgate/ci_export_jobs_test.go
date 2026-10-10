package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard/gitexport"
)

// The export jobs of .gitlab-ci.yml (gonk-cuwn, part ENFORCE): the export's
// own check -- deny-pattern guard AND the pinned secret scanner -- on every
// pipeline, and the gate run inside the public tree on the default branch.
// A job existing is not evidence it gates anything, so what is pinned here is
// each way such a job goes quiet: a rule that stops it running, a failure
// that is allowed, a scanner that is skipped, a pin that drifted.

var (
	reSecretsVersion = regexp.MustCompile(`(?m)^\s*GITLEAKS_VERSION:\s*([0-9.]+)\s*$`)
	reSecretsSHA     = regexp.MustCompile(`(?m)^\s*GITLEAKS_SHA256:\s*([0-9a-f]{64})\s*$`)
)

// secretsWorkflowPin returns the scanner version and archive digest the
// public `secrets` workflow enforces.
func secretsWorkflowPin(t *testing.T) (version, sha string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "secrets.yml"))
	if err != nil {
		t.Fatal(err)
	}
	v, s := reSecretsVersion.FindSubmatch(data), reSecretsSHA.FindSubmatch(data)
	if v == nil || s == nil {
		t.Fatal("secrets.yml no longer carries GITLEAKS_VERSION / GITLEAKS_SHA256 in the shape this test reads")
	}
	return string(v[1]), string(s[1])
}

func jobScript(job map[string]any, key string) string {
	return strings.Join(flattenScript(job[key]), "\n")
}

// exportCheckProblems reports every way the export-check job could stop
// being "guard + pinned scanner on every pipeline, blocking".
func exportCheckProblems(doc map[string]any, version, sha string) []string {
	job, ok := doc["export-check"].(map[string]any)
	if !ok {
		return []string{"no `export-check` job in .gitlab-ci.yml"}
	}
	var problems []string
	if job["stage"] != "test" {
		problems = append(problems, fmt.Sprintf("export-check is in stage %v, want test", job["stage"]))
	}
	// Every pipeline, and blocking: nothing that can make it not run, or run
	// and not count.
	for _, key := range []string{"rules", "only", "except", "when", "allow_failure", "needs", "extends", "retry"} {
		if _, has := job[key]; has {
			problems = append(problems, fmt.Sprintf("export-check has `%s:`; it runs on every pipeline, unconditionally, and its failure fails the pipeline", key))
		}
	}
	vars, _ := job["variables"].(map[string]any)
	url := fmt.Sprint(vars["GITLEAKS_ARCHIVE_URL"])
	wantURL := "https://github.com/gitleaks/gitleaks/releases/download/v" + version + "/gitleaks_" + version + "_linux_x64.tar.gz"
	if url != wantURL {
		problems = append(problems, fmt.Sprintf("export-check GITLEAKS_ARCHIVE_URL is %s, want the release the secrets workflow pins: %s", url, wantURL))
	}
	if got := fmt.Sprint(vars["GITLEAKS_ARCHIVE_SHA256"]); got != sha {
		problems = append(problems, fmt.Sprintf("export-check GITLEAKS_ARCHIVE_SHA256 is %s; the secrets workflow pins %s", got, sha))
	}
	if version != gitexport.GitleaksVersion {
		problems = append(problems, fmt.Sprintf("the secrets workflow pins gitleaks %s; the export tool accepts only %s", version, gitexport.GitleaksVersion))
	}
	script := jobScript(job, "script")
	for _, want := range []string{
		`curl -fsSL -o /tmp/gitleaks.tar.gz "$GITLEAKS_ARCHIVE_URL"`,
		`echo "$GITLEAKS_ARCHIVE_SHA256  /tmp/gitleaks.tar.gz" | sha256sum -c -`,
		`go run ./hack/relexport check `,
		`--src "$CI_COMMIT_SHA"`,
		`--gitleaks /tmp/gitleaks`,
	} {
		if !strings.Contains(script, want) {
			problems = append(problems, fmt.Sprintf("export-check script does not contain: %s", want))
		}
	}
	all := jobScript(job, "before_script") + "\n" + script + "\n" + jobScript(job, "after_script")
	for _, bad := range []string{"--guard-only", "|| true", "|| :", "set +e", "--quiet", "exit 0"} {
		if strings.Contains(all, bad) {
			problems = append(problems, fmt.Sprintf("export-check contains %q: the check may not be narrowed, softened or silenced", bad))
		}
	}
	if i, j := strings.Index(script, "sha256sum -c"), strings.Index(script, "tar -xzf"); i < 0 || j < 0 || j < i {
		problems = append(problems, "export-check unpacks the scanner archive before (or without) checking its digest")
	}
	return problems
}

// exportGateProblems reports every way export-gate could stop running the
// gate inside the public tree on the default branch.
func exportGateProblems(doc map[string]any) []string {
	job, ok := doc["export-gate"].(map[string]any)
	if !ok {
		return []string{"no `export-gate` job in .gitlab-ci.yml"}
	}
	var problems []string
	if job["stage"] != "test" {
		problems = append(problems, fmt.Sprintf("export-gate is in stage %v, want test", job["stage"]))
	}
	rules, _ := job["rules"].([]any)
	onDefault := false
	for i, r := range rules {
		rule, _ := r.(map[string]any)
		if rule["if"] == "$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH" {
			_, hasWhen := rule["when"]
			_, hasAllow := rule["allow_failure"]
			onDefault = i == 0 && !hasWhen && !hasAllow
		}
	}
	if !onDefault {
		problems = append(problems, "export-gate's first rule must run it on the default branch, automatically and blocking (`if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH`, no `when`, no `allow_failure`)")
	}
	if _, has := job["allow_failure"]; has {
		problems = append(problems, "export-gate has a job-level `allow_failure:`")
	}
	script := jobScript(job, "script")
	order := []string{
		`go run ./hack/relexport write `,
		`--src "$CI_COMMIT_SHA"`,
		`cd /tmp/public-tree`,
		`git init `,
		`git add --all --force`,
		`git update-ref refs/remotes/origin/main HEAD`,
		`make toolchain vet guard test`,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(script[at:], want)
		if i < 0 {
			problems = append(problems, fmt.Sprintf("export-gate script does not contain, in order: %s", want))
			continue
		}
		at += i
	}
	for _, bad := range []string{"|| true", "|| :", "set +e", "exit 0", "-skip"} {
		if strings.Contains(script, bad) {
			problems = append(problems, fmt.Sprintf("export-gate contains %q", bad))
		}
	}
	return problems
}

func TestExportCheckRunsGuardAndPinnedScannerOnEveryPipeline(t *testing.T) {
	version, sha := secretsWorkflowPin(t)
	for _, p := range exportCheckProblems(ciConfig(t), version, sha) {
		t.Error(p)
	}
}

func TestExportGateRunsTheGateInsideThePublicTree(t *testing.T) {
	for _, p := range exportGateProblems(ciConfig(t)) {
		t.Error(p)
	}
}

func TestTheExportJobGatesActuallyFail(t *testing.T) {
	version, sha := secretsWorkflowPin(t)
	job := func(doc map[string]any, name string) map[string]any { return doc[name].(map[string]any) }
	setScript := func(j map[string]any, old, new string) {
		j["script"] = []any{strings.Replace(jobScript(j, "script"), old, new, 1)}
	}
	for name, mutate := range map[string]func(doc map[string]any){
		"job removed":            func(doc map[string]any) { delete(doc, "export-check") },
		"moved to another stage": func(doc map[string]any) { job(doc, "export-check")["stage"] = "images" },
		"only on main": func(doc map[string]any) {
			job(doc, "export-check")["rules"] = []any{map[string]any{"if": "$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH"}}
		},
		"manual":          func(doc map[string]any) { job(doc, "export-check")["when"] = "manual" },
		"allowed to fail": func(doc map[string]any) { job(doc, "export-check")["allow_failure"] = true },
		"another digest": func(doc map[string]any) {
			job(doc, "export-check")["variables"].(map[string]any)["GITLEAKS_ARCHIVE_SHA256"] = strings.Repeat("0", 64)
		},
		"another release": func(doc map[string]any) {
			job(doc, "export-check")["variables"].(map[string]any)["GITLEAKS_ARCHIVE_URL"] = "https://example.invalid/gitleaks.tar.gz"
		},
		"guard only": func(doc map[string]any) {
			setScript(job(doc, "export-check"), "--gitleaks /tmp/gitleaks", "--guard-only")
		},
		"softened": func(doc map[string]any) {
			setScript(job(doc, "export-check"), "--gitleaks /tmp/gitleaks", "--gitleaks /tmp/gitleaks || true")
		},
		"digest not checked": func(doc map[string]any) { setScript(job(doc, "export-check"), "| sha256sum -c -", "") },
		"another commit": func(doc map[string]any) {
			setScript(job(doc, "export-check"), `--src "$CI_COMMIT_SHA"`, `--src "$CI_COMMIT_BEFORE_SHA"`)
		},
		"checked after unpacking": func(doc map[string]any) {
			j := job(doc, "export-check")
			s := jobScript(j, "script")
			sum := `echo "$GITLEAKS_ARCHIVE_SHA256  /tmp/gitleaks.tar.gz" | sha256sum -c -`
			tar := `tar -xzf /tmp/gitleaks.tar.gz -C /tmp gitleaks`
			s = strings.Replace(s, sum, "@@", 1)
			s = strings.Replace(s, tar, sum, 1)
			j["script"] = []any{strings.Replace(s, "@@", tar, 1)}
		},
	} {
		doc := ciConfig(t)
		mutate(doc)
		if len(exportCheckProblems(doc, version, sha)) == 0 {
			t.Errorf("export-check, %s: the gate did not notice", name)
		}
	}
	if len(exportCheckProblems(ciConfig(t), "9.9.9", sha)) == 0 || len(exportCheckProblems(ciConfig(t), version, strings.Repeat("1", 64))) == 0 {
		t.Error("export-check: a pin that differs from the secrets workflow's was not noticed")
	}

	for name, mutate := range map[string]func(doc map[string]any){
		"job removed": func(doc map[string]any) { delete(doc, "export-gate") },
		"manual on main": func(doc map[string]any) {
			job(doc, "export-gate")["rules"].([]any)[0].(map[string]any)["when"] = "manual"
		},
		"allowed to fail": func(doc map[string]any) {
			job(doc, "export-gate")["rules"].([]any)[0].(map[string]any)["allow_failure"] = true
		},
		"job-level allow": func(doc map[string]any) { job(doc, "export-gate")["allow_failure"] = true },
		"no rule for main": func(doc map[string]any) {
			job(doc, "export-gate")["rules"] = job(doc, "export-gate")["rules"].([]any)[1:]
		},
		"tests not run": func(doc map[string]any) {
			setScript(job(doc, "export-gate"), "make toolchain vet guard test", "make toolchain vet")
		},
		"gate run in the wrong": func(doc map[string]any) { setScript(job(doc, "export-gate"), "cd /tmp/public-tree", "true") },
		"no released state": func(doc map[string]any) {
			setScript(job(doc, "export-gate"), "git update-ref refs/remotes/origin/main HEAD", "true")
		},
		"softened": func(doc map[string]any) {
			setScript(job(doc, "export-gate"), "make toolchain vet guard test", "make toolchain vet guard test || true")
		},
	} {
		doc := ciConfig(t)
		mutate(doc)
		if len(exportGateProblems(doc)) == 0 {
			t.Errorf("export-gate, %s: the gate did not notice", name)
		}
	}
}
