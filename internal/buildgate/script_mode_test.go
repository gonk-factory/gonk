package buildgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Scripts meant to be RUN BY PATH must be executable IN GIT, which is not the
// same as being executable on the machine that wrote them.
//
// This repository is checked out on a /mnt/c path, where `chmod +x` does not
// reliably reach git's index. On 2026-09-07 two new scripts reached main as mode
// 100644 despite a local chmod, and the GitHub workflow running one of them died
// with "Permission denied" (exit 126) AFTER successfully standing up two
// Kubernetes clusters -- the expensive part done, then defeated by a file mode.
// A contributor on Windows would hit exactly the same thing.
//
// SCOPE, deliberately narrow, because a gate that fires on files which are fine
// by design is a gate people learn to ignore:
//   - hack/ and pack/scripts/ only -- the scripts CI and humans invoke by path.
//   - Anything with a #! shebang. A .py without one is a module, not a command.
//   - NOT test_*.py: those are pytest inputs, never executed directly.
//   - NOT images/**: images/agent/entrypoint.sh is 100644 on purpose and
//     Dockerfile.agent:122 chmods it during the build, so its repo mode is
//     irrelevant.
//   - NOT .beads/hooks/**: bd installs those into .git/hooks itself.
//
// git ls-files is the authority, not os.Stat: the question is what was COMMITTED.
func TestScriptsRunByPathAreExecutableInGit(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-s", "hack", "pack/scripts").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}

	var checked int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mode, path := fields[0], fields[len(fields)-1]
		if strings.HasPrefix(filepath.Base(path), "test_") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(root, path))
		if rerr != nil || !strings.HasPrefix(string(body), "#!") {
			continue // not a command
		}
		checked++
		if mode != "100755" {
			t.Errorf("%s has a shebang but is committed as %s, so running it by path fails with "+
				"\"Permission denied\". Fix with `git update-index --chmod=+x %s` -- a plain chmod "+
				"does not always reach git's index on this filesystem.", path, mode, path)
		}
	}
	if checked == 0 {
		t.Fatal("found no shebang scripts under hack/ or pack/scripts -- the path list is stale " +
			"and this gate is passing vacuously")
	}
	t.Logf("checked %d scripts", checked)
}
