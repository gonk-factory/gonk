package buildgate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Where the arm64 image legs run is a fact about ONE cluster: a node name and
// a taint key. Both are internal detail, and this file is published, so
// neither may be written here (gonk-cuwn, release-export plan S6). The two
// values come from CI/CD variables defined on the project, outside the
// repository.
//
// What the runner does with them (gitlab-runner executors/kubernetes
// overwrites.go, read 2026-10-09):
//
//   - KUBERNETES_NODE_SELECTOR_* must be `key=value`. An EMPTY value is not
//     ignored: it fails the job before a pod exists ("provided value \"\" does
//     not match \"k=v\"").
//   - KUBERNETES_NODE_TOLERATIONS_* is `key[=value]:effect`. An EMPTY value is
//     accepted and becomes a toleration with no key and operator Exists --
//     the pod tolerates EVERY taint. That is the unsafe direction, so the
//     jobs themselves refuse to run with either variable empty.
var arm64Placement = map[string]string{
	"KUBERNETES_NODE_SELECTOR_HOSTNAME": "$GONK_ARM64_NODE_SELECTOR",
	"KUBERNETES_NODE_TOLERATIONS_GPU":   "$GONK_ARM64_TOLERATION",
}

var ciVariableRef = regexp.MustCompile(`^\$[A-Z][A-Z0-9_]*$`)

// arm64PlacementProblems reports every way the arm64 placement could be a
// literal again, be missing, or run unguarded. Pure, so the negative control
// can prove it fails.
func arm64PlacementProblems(doc map[string]any) []string {
	var problems []string
	tmpl, ok := doc[".gonk-arm64"].(map[string]any)
	if !ok {
		return []string{"no `.gonk-arm64` template in .gitlab-ci.yml"}
	}
	vars, _ := tmpl["variables"].(map[string]any)
	for key, want := range arm64Placement {
		raw, present := vars[key]
		got := fmt.Sprint(raw)
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf(".gonk-arm64 has no %s: the arm64 legs would inherit the amd64 placement of the shared template", key))
		case !ciVariableRef.MatchString(got):
			problems = append(problems, fmt.Sprintf(".gonk-arm64 %s is the literal %q; it must be a reference to a CI/CD variable defined outside the repository", key, got))
		case got != want:
			problems = append(problems, fmt.Sprintf(".gonk-arm64 %s is %s, want %s (the name the owner's project settings and the release procedure use)", key, got, want))
		}
	}

	// The guard: every image leg runs it, and it refuses an arm64 leg whose
	// placement variables are empty.
	kaniko, _ := doc[".gonk-kaniko"].(map[string]any)
	joined := strings.Join(flattenScript(kaniko["before_script"]), "\n")
	for _, want := range arm64Placement {
		name := strings.TrimPrefix(want, "$")
		if !strings.Contains(joined, `[ -z "$`+name+`" ]`) {
			problems = append(problems, fmt.Sprintf(".gonk-kaniko before_script does not refuse an empty %s", name))
		}
	}
	if !strings.Contains(joined, `[ "$BUILD_ARCH" = "arm64" ]`) {
		problems = append(problems, ".gonk-kaniko before_script has no arm64 placement guard")
	}
	return problems
}

// flattenScript flattens a script value (strings, and lists nested by YAML
// anchors) into its lines' sources.
func flattenScript(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, flattenScript(e)...)
		}
		return out
	}
	return nil
}

func TestArm64PlacementComesFromCIVariablesNotLiterals(t *testing.T) {
	for _, p := range arm64PlacementProblems(ciConfig(t)) {
		t.Error(p)
	}
}

func TestTheArm64PlacementGateActuallyFails(t *testing.T) {
	for key := range arm64Placement {
		for name, mutate := range map[string]func(vars map[string]any){
			"a literal":       func(vars map[string]any) { vars[key] = "example.test/key=value" },
			"missing":         func(vars map[string]any) { delete(vars, key) },
			"another name":    func(vars map[string]any) { vars[key] = "$SOME_OTHER_VARIABLE" },
			"partly literal":  func(vars map[string]any) { vars[key] = "example.test/key=$GONK_ARM64_NODE_SELECTOR" },
			"braced variable": func(vars map[string]any) { vars[key] = "prefix-${GONK_ARM64_TOLERATION}" },
		} {
			doc := ciConfig(t)
			mutate(doc[".gonk-arm64"].(map[string]any)["variables"].(map[string]any))
			if len(arm64PlacementProblems(doc)) == 0 {
				t.Errorf("%s made %s: the gate did not notice", key, name)
			}
		}
	}
	doc := ciConfig(t)
	doc[".gonk-kaniko"].(map[string]any)["before_script"] = []any{"true"}
	if got := arm64PlacementProblems(doc); len(got) != 3 {
		t.Errorf("without the before_script guard: %d problems, want 3: %v", len(got), got)
	}
}

// The guard is run, not read: with either variable empty an arm64 leg stops,
// an amd64 leg does not care, and with both set the leg goes on.
func TestTheArm64PlacementGuardRefusesEmptyVariables(t *testing.T) {
	kaniko, _ := ciConfig(t)[".gonk-kaniko"].(map[string]any)
	var guard string
	for _, s := range flattenScript(kaniko["before_script"]) {
		if strings.Contains(s, "GONK_ARM64_NODE_SELECTOR") {
			guard = s
		}
	}
	if guard == "" {
		t.Fatal("no arm64 placement guard in .gonk-kaniko before_script")
	}
	for _, c := range []struct {
		name           string
		arch, sel, tol string
		wantExit       int
		wantOut        string
	}{
		{"arm64, both set", "arm64", "example.test/k=v", "example.test/t=true:NoSchedule", 0, ""},
		{"arm64, selector empty", "arm64", "", "example.test/t=true:NoSchedule", 1, "GONK_ARM64_NODE_SELECTOR"},
		{"arm64, toleration empty", "arm64", "example.test/k=v", "", 1, "GONK_ARM64_TOLERATION"},
		{"arm64, both empty", "arm64", "", "", 1, "GONK_ARM64_NODE_SELECTOR"},
		{"amd64, both empty", "amd64", "", "", 0, ""},
	} {
		exit, out := runSh(t, guard+"\necho reached-the-build\n",
			"BUILD_ARCH="+c.arch, "GONK_ARM64_NODE_SELECTOR="+c.sel, "GONK_ARM64_TOLERATION="+c.tol)
		if exit != c.wantExit || !strings.Contains(out, c.wantOut) {
			t.Errorf("%s: exit %d, want %d; output %q must contain %q", c.name, exit, c.wantExit, out, c.wantOut)
		}
		if reached := strings.Contains(out, "reached-the-build"); reached != (c.wantExit == 0) {
			t.Errorf("%s: reached the build = %v", c.name, reached)
		}
	}
}

// runSh runs a script with ONLY the given variables set (plus PATH).
func runSh(t *testing.T, script string, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run sh: %v", err)
	return -1, ""
}
