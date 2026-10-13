// The image-entrypoint gate (gonk-pk1z).
//
// Some images CI uses have an ENTRYPOINT that is not a shell: alpine/helm's is
// `helm`, crane's is `crane`, kaniko's is `/kaniko/executor`. GitLab runs a
// job's script by starting `sh -c <script>` in the image, so unless the job
// sets `image: {entrypoint: [""]}` the container runs `helm sh -c ...` and
// dies with 'unknown command "sh" for "helm"'.
//
// WHY THIS IS LATENT. The docker executor on runner tak-docker-desktop does
// NOT override the entrypoint; the kubernetes executor (the in-cluster runner)
// does. chart-lint always landed on the cluster runner, so it looked fine for
// months; the first time it landed on tak-docker-desktop (pipeline 510,
// 92a7a7c) it failed, on_job_failure cancelled every image build, and nothing
// had ever said the job depended on which runner it got. Setting the override
// is harmless on the kubernetes runner and required on the docker one, so
// every such job must set it.
//
// WHAT IT SEES: jobs in this file, with YAML anchors/merge keys resolved by
// the parser and `extends:` followed (a job inherits `image` from its
// extends chain unless it sets its own; `image` is replaced wholesale, not
// merged). WHAT IT CANNOT SEE: jobs that come from `include:`d files
// (the shared templates project, e.g. .manifest-merge) -- they are not in this
// repo's file -- and `default: image:`. A job whose image is a variable
// ($GONK_CHART_TOOLS_IMAGE) is not matched by name and so is not checked.
package buildgate

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// nonShellEntrypointImages are image repositories (name without tag/digest)
// known to have a non-shell ENTRYPOINT.
var nonShellEntrypointImages = []string{
	"alpine/helm",
	"gcr.io/go-containerregistry/crane",
	"gcr.io/kaniko-project/executor",
}

// imageRepo strips tag and digest: "a/b:tag@sha256:x" -> "a/b".
func imageRepo(name string) string {
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	// a ':' after the last '/' is a tag (a ':' before it is a registry port)
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// effectiveImage resolves a job's image through its extends chain.
func effectiveImage(doc map[string]any, job map[string]any, depth int) (any, bool) {
	if img, ok := job["image"]; ok {
		return img, true
	}
	if depth > 10 {
		return nil, false
	}
	for _, parent := range extendsList(job) {
		if pj, ok := doc[parent].(map[string]any); ok {
			if img, ok := effectiveImage(doc, pj, depth+1); ok {
				return img, true
			}
		}
	}
	return nil, false
}

// entrypointProblems reports every job (including hidden .templates) whose
// effective image is a non-shell-entrypoint image without entrypoint [""].
func entrypointProblems(doc map[string]any) []string {
	var problems []string
	names := make([]string, 0, len(doc))
	for k := range doc {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		job, ok := doc[name].(map[string]any)
		if !ok {
			continue
		}
		img, ok := effectiveImage(doc, job, 0)
		if !ok {
			continue
		}
		var imgName string
		var ep any
		var epSet bool
		switch v := img.(type) {
		case string:
			imgName = v
		case map[string]any:
			imgName = fmt.Sprint(v["name"])
			ep, epSet = v["entrypoint"]
		default:
			continue
		}
		matched := false
		for _, want := range nonShellEntrypointImages {
			if imageRepo(imgName) == want {
				matched = true
			}
		}
		if !matched {
			continue
		}
		list, isList := ep.([]any)
		if !epSet || !isList || len(list) != 1 || fmt.Sprint(list[0]) != "" {
			problems = append(problems, fmt.Sprintf(
				"job %q uses %s without `entrypoint: [\"\"]` (got %v): the docker executor will run the image's own entrypoint with `sh -c ...` as arguments",
				name, imgName, ep))
		}
	}
	return problems
}

func TestNonShellEntrypointImagesOverrideTheEntrypoint(t *testing.T) {
	for _, p := range entrypointProblems(ciConfig(t)) {
		t.Error(p)
	}
}

// The gate must see the jobs it is about: if chart-lint stops being matched
// (renamed image, restructured file) the test above would pass vacuously.
func TestEntrypointGateActuallyFails(t *testing.T) {
	doc := ciConfig(t)
	lint, ok := doc["chart-lint"].(map[string]any)
	if !ok {
		t.Fatal("no chart-lint job -- renamed? update this gate")
	}
	for name, img := range map[string]any{
		"string form":         "alpine/helm:3.19.0",
		"map without entrypt": map[string]any{"name": "alpine/helm:3.19.0"},
		"entrypoint helm":     map[string]any{"name": "alpine/helm:3.19.0", "entrypoint": []any{"helm"}},
	} {
		lint["image"] = img
		if len(entrypointProblems(doc)) == 0 {
			t.Errorf("chart-lint %s: entrypointProblems accepted it", name)
		}
	}
	// inherited image: a job extending .gonk-kaniko loses the override if the
	// template loses it.
	doc = ciConfig(t)
	doc[".gonk-kaniko"].(map[string]any)["image"] = map[string]any{"name": "gcr.io/kaniko-project/executor:v1.24.0-debug"}
	if len(entrypointProblems(doc)) < 2 {
		t.Errorf("kaniko template without entrypoint: want the template and its heirs flagged, got %v", entrypointProblems(doc))
	}
	// and it must be looking at the real jobs
	doc = ciConfig(t)
	n := 0
	for _, name := range []string{"chart-lint", "chart-publish", ".gonk-kaniko", "agent-image-amd64"} {
		job, _ := doc[name].(map[string]any)
		img, _ := effectiveImage(doc, job, 0)
		for _, want := range nonShellEntrypointImages {
			if m, ok := img.(map[string]any); ok && imageRepo(fmt.Sprint(m["name"])) == want {
				n++
			}
		}
	}
	if n < 4 {
		t.Errorf("gate matched only %d of the 4 known jobs", n)
	}
}
