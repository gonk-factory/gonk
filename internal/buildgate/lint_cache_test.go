// `make lint` gives golangci-lint a cache that belongs to the checkout it runs
// in (gonk-nn71).
//
// The defect: golangci-lint's default cache is per USER and keyed by package
// content, and a cached issue carries the absolute path it was found at. A
// commit gated in a fresh worktree, then gated again in another fresh worktree
// after the first was removed, went red in lint with three forbidigo findings
// at paths inside the first, deleted, directory -- lines that carry
// `//nolint:forbidigo`, which the linter could no longer read. Nothing in the
// tree was wrong.
//
// Two things are pinned, because either can be lost on its own:
//   - hack/lint_cache_dir.sh answers with a different directory for every
//     checkout path (its behaviour, by running it);
//   - the lint recipe actually hands that answer to every golangci-lint run
//     it makes on the host (its text -- running the recipe would run the
//     linter).
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

// lintCacheDir runs hack/lint_cache_dir.sh from dir with exactly env (plus a
// PATH), and returns what it printed and whether it exited 0.
func lintCacheDir(t *testing.T, dir string, env ...string) (out string, ok bool) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "hack", "lint_cache_dir.sh")
	cmd := exec.Command("sh", script)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if _, isExit := err.(*exec.ExitError); !isExit {
			t.Fatalf("could not run %s: %v", script, err)
		}
		return strings.TrimSpace(stdout.String() + stderr.String()), false
	}
	return strings.TrimSpace(stdout.String()), true
}

// realDir is a new directory, with symlinks resolved: the script answers with
// `pwd -P`, and a temp dir is itself behind a symlink on some systems.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLintCacheDirIsDifferentForEveryCheckout(t *testing.T) {
	home, a, b := realDir(t), realDir(t), realDir(t)

	gotA, ok := lintCacheDir(t, a, "HOME="+home)
	if !ok {
		t.Fatalf("lint_cache_dir.sh failed in %s: %s", a, gotA)
	}
	gotB, ok := lintCacheDir(t, b, "HOME="+home)
	if !ok {
		t.Fatalf("lint_cache_dir.sh failed in %s: %s", b, gotB)
	}
	if gotA == gotB {
		t.Fatalf("two checkouts (%s, %s) were given the SAME lint cache %s: the second replays the first one's issues at the first one's paths (gonk-nn71)", a, b, gotA)
	}
	// Not merely different: each is named after its own checkout, so no third
	// path can be given either of them.
	root := filepath.Join(home, ".cache", "golangci-lint-by-checkout")
	if gotA != root+a || gotB != root+b {
		t.Fatalf("caches are %q and %q; want %q and %q (the cache root, then the checkout's own absolute path)", gotA, gotB, root+a, root+b)
	}

	// The same checkout always gets the same one, by whatever name it is
	// reached: the key is the physical path.
	link := filepath.Join(realDir(t), "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	if got, _ := lintCacheDir(t, link, "HOME="+home); got != gotA {
		t.Errorf("reached through a symlink, checkout %s was given %q, not its own cache %q", a, got, gotA)
	}

	// XDG_CACHE_HOME moves the root and nothing else.
	xdg := realDir(t)
	if got, _ := lintCacheDir(t, a, "HOME="+home, "XDG_CACHE_HOME="+xdg); got != filepath.Join(xdg, "golangci-lint-by-checkout")+a {
		t.Errorf("with XDG_CACHE_HOME=%s the cache is %q, want it under that directory and still named after the checkout", xdg, got)
	}
}

// A caller that names a cache names the ROOT: the checkout's path is appended
// to that too. So a run that asks for an empty cache gets one, and two
// checkouts handed the same value -- an export in a shell profile -- still do
// not share entries. (Used as the directory itself, the caller's value brought
// the defect back: the same three stale findings, measured.) And with nowhere
// to put a cache the script says so rather than printing a path under nothing.
func TestLintCacheDirIsPerCheckoutUnderACallersCacheToo(t *testing.T) {
	a, b, chosen := realDir(t), realDir(t), realDir(t)
	gotA, okA := lintCacheDir(t, a, "HOME="+realDir(t), "GOLANGCI_LINT_CACHE="+chosen)
	gotB, okB := lintCacheDir(t, b, "HOME="+realDir(t), "GOLANGCI_LINT_CACHE="+chosen+"/")
	if !okA || !okB {
		t.Fatalf("the script failed with GOLANGCI_LINT_CACHE set: %q, %q", gotA, gotB)
	}
	if gotA != chosen+a || gotB != chosen+b {
		t.Errorf("with GOLANGCI_LINT_CACHE=%s the caches are %q and %q; want %q and %q: under the caller's directory, and still one per checkout", chosen, gotA, gotB, chosen+a, chosen+b)
	}
	if gotA == gotB || gotA == chosen {
		t.Errorf("two checkouts given one GOLANGCI_LINT_CACHE share a cache (%q, %q): that is gonk-nn71 again, by environment variable", gotA, gotB)
	}

	got, ok := lintCacheDir(t, a)
	if ok {
		t.Fatalf("with none of GOLANGCI_LINT_CACHE, XDG_CACHE_HOME and HOME the script printed %q and exited 0; a cache path built on an empty root would be shared by everyone", got)
	}
	if !strings.Contains(got, "GOLANGCI_LINT_CACHE") {
		t.Errorf("the refusal %q does not say what to set", got)
	}
}

// hostLintRunRe is a golangci-lint run made by the host binary in the lint
// recipe (`$$bin run ...`); the container fallback runs in a fresh container
// with no cache mounted, so it has nothing to share.
var hostLintRunRe = regexp.MustCompile(`(\S*)\s*\$\$bin run\b`)

func TestMakeLintHandsEveryHostRunThePerCheckoutCache(t *testing.T) {
	_, recipes := makeTargets(t)
	recipe := recipes["lint"]
	if recipe == "" {
		t.Fatal("the Makefile has no `lint` recipe -- has it been renamed? this guard must move with it")
	}
	if problem := lintRecipeCacheProblem(recipe); problem != "" {
		t.Fatal(problem)
	}
}

// lintRecipeCacheProblem says what is wrong with a lint recipe's use of the
// cache, or "" when every host run is given the per-checkout one.
func lintRecipeCacheProblem(recipe string) string {
	if !strings.Contains(recipe, `cache="$$(hack/lint_cache_dir.sh)" || exit 1`) {
		return "the lint recipe does not take its cache directory from hack/lint_cache_dir.sh (and stop if that fails), so golangci-lint falls back to one cache per user, shared by every checkout (gonk-nn71)"
	}
	runs := hostLintRunRe.FindAllStringSubmatch(recipe, -1)
	if len(runs) == 0 {
		return "found no `$$bin run` in the lint recipe -- the recipe changed shape and this guard is checking nothing"
	}
	for _, m := range runs {
		if m[1] != `GOLANGCI_LINT_CACHE="$$cache"` {
			return "the lint recipe runs `$$bin run` without GOLANGCI_LINT_CACHE=\"$$cache\" in front of it (found " + m[1] + "): that run uses the per-user cache every checkout shares (gonk-nn71)"
		}
	}
	return ""
}

func TestTheLintCacheGuardActuallyRejects(t *testing.T) {
	const good = "\tcache=\"$$(hack/lint_cache_dir.sh)\" || exit 1; \\\n\tGOLANGCI_LINT_CACHE=\"$$cache\" $$bin run --allow-serial-runners ./...; \\\n"
	if p := lintRecipeCacheProblem(good); p != "" {
		t.Fatalf("a recipe that passes the per-checkout cache was rejected: %s", p)
	}
	bad := map[string]string{
		"the recipe as it was before gonk-nn71": "\t$$bin run --allow-serial-runners ./...; \\\n",
		"computed, never passed":                "\tcache=\"$$(hack/lint_cache_dir.sh)\" || exit 1; \\\n\t$$bin run ./...; \\\n",
		"passed to one run of two":              "\tcache=\"$$(hack/lint_cache_dir.sh)\" || exit 1; \\\n\tGOLANGCI_LINT_CACHE=\"$$cache\" $$bin run ./...; \\\n\ttime $$bin run ./pkg/...; \\\n",
		"a failing script is ignored":           "\tcache=\"$$(hack/lint_cache_dir.sh)\"; \\\n\tGOLANGCI_LINT_CACHE=\"$$cache\" $$bin run ./...; \\\n",
		"no host run at all":                    "\tcache=\"$$(hack/lint_cache_dir.sh)\" || exit 1; \\\n\techo nothing; \\\n",
	}
	for name, recipe := range bad {
		if lintRecipeCacheProblem(recipe) == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
