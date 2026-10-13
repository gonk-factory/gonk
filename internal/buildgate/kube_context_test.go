package buildgate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The kubectl current context is SHARED STATE on a development machine: every
// shell, every tool and every other work session on it reads the same file.
//
// On 2026-10-11 it was found that creating this project's throwaway k3d test
// cluster had, weeks earlier, switched the machine's default context to it.
// Sessions on unrelated work then ran bare `kubectl` against the wrong cluster
// and got confident, wrong answers. Putting the default back exposed the mirror
// image of the same fault: hack/netpol_enforcement_e2e.sh ran bare `kubectl`,
// so with the default pointing at a real cluster it would have created and
// deleted workloads there.
//
// Two rules follow, and these tests hold them:
//   - nothing this repo runs on a developer's machine may CHANGE the default
//     context (TestNoScriptChangesTheDefaultKubeContext);
//   - a script that writes to a cluster names its context on every call and
//     refuses one that is not a throwaway cluster
//     (TestTheNetpolScriptRefusesAClusterThatIsNotThrowaway).
//
// .github/workflows is out of scope on purpose: a hosted runner is a fresh
// machine per job and its default context is nobody else's.

// fakeKubectl writes a kubectl stand-in that answers `config current-context`
// with current and appends every invocation to a log.
func fakeKubectl(t *testing.T, current string) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "calls.log")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"if [ \"$1\" = config ] && [ \"$2\" = current-context ]; then printf '%s\\n' '" + current + "'; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binDir, logPath
}

func runNetpolScript(t *testing.T, binDir string, env ...string) (exit int, stderr string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(repoRoot(t), "hack", "netpol_enforcement_e2e.sh"))
	cmd.Env = append([]string{"PATH=" + binDir + ":" + os.Getenv("PATH"), "IMAGE=example.invalid/x:y", "HOME=" + t.TempDir()}, env...)
	var eb bytes.Buffer
	cmd.Stderr = &eb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), eb.String()
	}
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return 0, eb.String()
}

func TestTheNetpolScriptRefusesAClusterThatIsNotThrowaway(t *testing.T) {
	for _, tc := range []struct {
		name, current string
		env           []string
	}{
		{"the default context is a real cluster", "admin@prod", nil},
		{"no current context at all", "", nil},
		{"KUBE_CONTEXT names a real cluster although the default is throwaway", "k3d-gonk", []string{"KUBE_CONTEXT=admin@prod"}},
		{"a name that only contains k3d", "prod-k3d-east", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, log := fakeKubectl(t, tc.current)
			exit, stderr := runNetpolScript(t, bin, tc.env...)
			if exit != 2 || !strings.Contains(stderr, "refusing to run") {
				t.Fatalf("exit = %d, stderr = %q; want exit 2 and a refusal", exit, stderr)
			}
			calls, _ := os.ReadFile(log)
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line != "" && line != "config current-context" {
					t.Errorf("the script called kubectl %q before refusing; only the context read is allowed", line)
				}
			}
		})
	}
}

// With a throwaway context the script proceeds, and EVERY call it makes names
// that context: the fake fails each call, so the script stops at the first one,
// which is enough to see the flag. The static half below covers the rest.
func TestTheNetpolScriptNamesItsContextOnEveryCall(t *testing.T) {
	for _, tc := range []struct {
		name, current, want string
		env                 []string
	}{
		{"from the current context", "k3d-gonk", "k3d-gonk", nil},
		{"KUBE_CONTEXT wins over the current context", "admin@prod", "kind-scratch", []string{"KUBE_CONTEXT=kind-scratch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, log := fakeKubectl(t, tc.current)
			exit, stderr := runNetpolScript(t, bin, tc.env...)
			if exit == 2 && strings.Contains(stderr, "refusing to run") {
				t.Fatalf("a throwaway context was refused: %s", stderr)
			}
			calls, _ := os.ReadFile(log)
			var acted int
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line == "" || line == "config current-context" {
					continue
				}
				acted++
				if !strings.HasPrefix(line, "--context "+tc.want+" ") {
					t.Errorf("kubectl %q does not name the context %q", line, tc.want)
				}
			}
			if acted == 0 {
				t.Fatal("the script made no cluster call; this test saw nothing")
			}
		})
	}

	// Static half: the only kubectl the script may run unwrapped is the one
	// inside the wrapper function and the context read that feeds the guard.
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "hack", "netpol_enforcement_e2e.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `kubectl() { command kubectl --context "$KUBE_CONTEXT" "$@"; }`) {
		t.Error("the kubectl wrapper that pins --context is gone")
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "command kubectl") &&
			!strings.Contains(line, `--context "$KUBE_CONTEXT"`) &&
			!strings.Contains(line, "config current-context") {
			t.Errorf("line %d bypasses the wrapper: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// changesDefaultContext matches a command that rewrites the shared kubeconfig's
// current context, or creates a cluster in a way that does.
//
// k3d and kind both merge the new cluster into the default kubeconfig AND
// switch to it unless told not to, so a create is flagged unless the same line
// carries the flag that stops it.
var changesDefaultContext = []struct {
	re     *regexp.Regexp
	unless string
	why    string
}{
	{regexp.MustCompile(`\bkubectl\b.*\bconfig\s+(use-context|set-context\s+--current|use\b)`), "", "switches the default context"},
	{regexp.MustCompile(`\bkubectx\b`), "", "switches the default context"},
	{regexp.MustCompile(`\bk3d\s+cluster\s+create\b`), "--kubeconfig-switch-context=false", "k3d switches the default context to the new cluster"},
	{regexp.MustCompile(`\bk3d\s+kubeconfig\s+merge\b`), "--kubeconfig-switch-context=false", "k3d switches the default context to the merged cluster"},
	{regexp.MustCompile(`\bkind\s+create\s+cluster\b`), "--kubeconfig", "kind switches the default context unless given its own kubeconfig file"},
}

func contextChangeIn(line string) string {
	code := line
	if i := strings.Index(code, "#"); i >= 0 {
		code = code[:i]
	}
	for _, c := range changesDefaultContext {
		if c.re.MatchString(code) && (c.unless == "" || !strings.Contains(code, c.unless)) {
			return c.why
		}
	}
	return ""
}

func TestContextChangeScannerSeesEachForm(t *testing.T) {
	for line, want := range map[string]bool{
		"kubectl config use-context admin@prod":                 true,
		"\tkubectl --kubeconfig x config use-context k3d-gonk":  true,
		"kubectl config set-context --current --namespace=gonk": true,
		"kubectx k3d-gonk":                                     true,
		"k3d cluster create gonk --agents 0 --wait":            true,
		"k3d kubeconfig merge gonk --kubeconfig-merge-default": true,
		"kind create cluster --name gonk":                      true,
		"k3d cluster create gonk --kubeconfig-update-default=false --kubeconfig-switch-context=false": false,
		"kind create cluster --name gonk --kubeconfig \"$tmp/kubeconfig\"":                            false,
		"kubectl config current-context":                                                              false,
		"kubectl --context \"$KUBE_CONTEXT\" get pods":                                                false,
		"kubectl config set-context dev --namespace=gonk":                                             false,
		"# run: kubectl config use-context k3d-gonk":                                                  false,
		"k3d cluster delete gonk":                                                                     false,
		"k3d image import gonk-intake:ci -c gonk":                                                     false,
	} {
		if got := contextChangeIn(line) != ""; got != want {
			t.Errorf("contextChangeIn(%q) flagged = %v, want %v", line, got, want)
		}
	}
}

func TestNoScriptChangesTheDefaultKubeContext(t *testing.T) {
	root := repoRoot(t)
	var files []string
	for _, dir := range []string{"hack", "pack/scripts", "images", "test"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			switch filepath.Ext(p) {
			case ".sh", ".py", ".mk":
				files = append(files, p)
			}
			return nil
		})
	}
	files = append(files, filepath.Join(root, "Makefile"))
	if len(files) < 10 {
		t.Fatalf("only %d files scanned; the walk is not seeing the tree", len(files))
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		for i, line := range strings.Split(string(b), "\n") {
			if why := contextChangeIn(line); why != "" {
				t.Errorf("%s:%d %s:\n    %s\n  pass --context (or a private KUBECONFIG file) instead; the default context is shared by every session on the machine",
					rel, i+1, why, strings.TrimSpace(line))
			}
		}
	}
}
