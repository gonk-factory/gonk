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

// imageChangePaths returns the `changes:` list that decides whether a commit
// rebuilds images (.gonk-kaniko's rules, shared with .gonk-index by anchor).
func imageChangePaths(t *testing.T, doc map[string]any) []string {
	t.Helper()
	job, ok := doc[".gonk-kaniko"].(map[string]any)
	if !ok {
		t.Fatal("no `.gonk-kaniko` template in .gitlab-ci.yml -- has it been renamed? this guard must be updated with it")
	}
	rules, ok := job["rules"].([]any)
	if !ok || len(rules) == 0 {
		t.Fatal("`.gonk-kaniko` has no `rules:`")
	}
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := rule["changes"].([]any)
		if !ok {
			continue
		}
		var paths []string
		for _, p := range raw {
			paths = append(paths, fmt.Sprint(p))
		}
		return paths
	}
	t.Fatal("no rule in `.gonk-kaniko` carries a `changes:` list")
	return nil
}

// copySource matches the build-context side of a Dockerfile COPY/ADD. Lines
// with --from copy between stages, not from the repo, so they are skipped.
var copySource = regexp.MustCompile(`(?m)^\s*(?:COPY|ADD)\s+(.*)$`)

// dockerfileCopySources returns every repo path the four image Dockerfiles
// copy out of the build context.
func dockerfileCopySources(t *testing.T) map[string][]string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "images")
	files, err := filepath.Glob(filepath.Join(dir, "Dockerfile.*"))
	if err != nil {
		t.Fatalf("glob Dockerfiles: %v", err)
	}
	if len(files) < 4 {
		t.Fatalf("found %d Dockerfiles under images/, expected at least 4 (agent, controller, intake, meter)", len(files))
	}

	out := map[string][]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range copySource.FindAllStringSubmatch(string(data), -1) {
			args := strings.Fields(m[1])
			var srcs []string
			skip := false
			for _, a := range args {
				if strings.HasPrefix(a, "--from") {
					skip = true
					break
				}
				if strings.HasPrefix(a, "--") {
					continue
				}
				srcs = append(srcs, a)
			}
			if skip || len(srcs) < 2 {
				continue // inter-stage copy, or COPY with no destination
			}
			out[filepath.Base(f)] = append(out[filepath.Base(f)], srcs[:len(srcs)-1]...)
		}
	}
	return out
}

// covers reports whether any entry in the rules' changes list would match a
// commit touching src. Both sides are compared on their first path segment,
// which is all the globs in that list ever discriminate on (`cmd/**/*`,
// `go.mod`, `images/**/*`).
func covers(changes []string, src string) bool {
	seg := strings.SplitN(strings.TrimPrefix(filepath.ToSlash(src), "./"), "/", 2)[0]
	for _, c := range changes {
		cs := strings.SplitN(filepath.ToSlash(c), "/", 2)[0]
		if cs == seg {
			return true
		}
	}
	return false
}

// The image rules skip the build entirely for a commit that touches nothing
// shipping in an image (gonk-1t4: 70% of commits, and the cache layers they
// pushed filled the registry volume, gonk-mzm). That is only safe while the
// `changes:` list is a SUPERSET of what the Dockerfiles actually copy. A COPY
// added for a path missing from the list would make a commit editing that path
// deploy the PREVIOUS image under the new commit's tag -- green CI, stale
// binary, which is this project's signature failure shape (gonk-n50).
func TestImageRulesCoverEveryDockerfileCopySource(t *testing.T) {
	changes := imageChangePaths(t, ciConfig(t))
	if len(changes) < 5 {
		t.Fatalf("the image `changes:` list has only %d entries (%v) -- suspiciously short; this gate would pass vacuously", len(changes), changes)
	}

	sources := dockerfileCopySources(t)
	if len(sources) == 0 {
		t.Fatal("parsed no COPY sources out of any Dockerfile -- the regex is stale and this gate is vacuous")
	}

	var files []string
	for f := range sources {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, f := range files {
		for _, src := range sources[f] {
			if !covers(changes, src) {
				t.Errorf("%s copies %q, but no entry in .gonk-kaniko's `changes:` list matches it (%v).\n"+
					"A commit touching that path would build no image, so v0.1.0-<sha12> for it would either "+
					"not exist or -- worse -- name a stale build.", f, src, changes)
			}
		}
	}
}

func TestTheCopyCoverageGateActuallyFails(t *testing.T) {
	if covers([]string{"cmd/**/*", "go.mod"}, "pack/") {
		t.Error("covers() accepted `pack/` against a changes list that does not mention it")
	}
	if !covers([]string{"images/**/*"}, "images/agent/entrypoint.sh") {
		t.Error("covers() rejected a path the changes list plainly matches")
	}
}

// Belt and braces on the anchor the CI file relies on: the index jobs must use
// the SAME rules as the build jobs, or a commit can publish per-arch tags with
// no index over them (gonk-n50 took gonk down for six days in exactly that shape).
func TestIndexJobsShareTheBuildRules(t *testing.T) {
	doc := ciConfig(t)
	build, ok := doc[".gonk-kaniko"].(map[string]any)
	if !ok {
		t.Fatal("no `.gonk-kaniko` template")
	}
	index, ok := doc[".gonk-index"].(map[string]any)
	if !ok {
		t.Fatal("no `.gonk-index` template")
	}
	got, err := yaml.Marshal(index["rules"])
	if err != nil {
		t.Fatalf("marshal index rules: %v", err)
	}
	want, err := yaml.Marshal(build["rules"])
	if err != nil {
		t.Fatalf("marshal build rules: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("`.gonk-index` rules have drifted from `.gonk-kaniko` rules.\nbuild:\n%s\nindex:\n%s", want, got)
	}
}
