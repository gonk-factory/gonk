// The tag-shape gate (gonk-7ywn). 0f13d63 made the DEFAULT BRANCH publish a
// RANKABLE image tag -- $GONK_VERSION-$CI_PIPELINE_IID.<sha12> -- while every
// other ref keeps the old, unrankable $GONK_VERSION-<sha12>. Flux image
// automation ranks on the pipeline IID, so the split is not cosmetic; it is
// the whole safety property:
//
//	A NON-DEFAULT-BRANCH BUILD CAN NEVER BE SELECTED BY THE IMAGEPOLICY.
//
// The kaniko rules fire on ANY branch. A feature branch that published a
// rankable tag would carry a HIGHER IID than main's most recent build (main's
// images are only rebuilt when main changes), and image automation would
// deploy branch code to the live cluster. The branch tag being UNRANKABLE is
// what makes that impossible: the ImagePolicy's filterTags pattern requires
// the <iid>.<sha> form and simply cannot match a sha-only tag.
//
// WHY THIS FILE EXECUTES SHELL INSTEAD OF READING YAML. Restating the `if`
// condition in Go would prove nothing -- it would pass just as happily if the
// shell logic were inverted, because the Go copy and the shell original are
// independent statements of the same intent and only one of them runs in CI.
// So the gate extracts the ACTUAL snippet out of .gitlab-ci.yml, runs it under
// `sh` with a matrix of CI environments, and asserts on the GONK_TAG it
// PRINTS. That is the measurement closest to the claim.
//
// It is an ordinary untagged test: `go test ./...` (make test, ci.yml's
// `go test -race` step, and .gitlab-ci.yml's test job) runs it. A gate behind
// a build tag no job names would not be a gate -- see ci_build_tags_test.go,
// which polices exactly that.
package buildgate

import (
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fluxFilterTags is DUPLICATED FROM THE GITOPS IMAGEPOLICY and must be changed
// with it. The policy (clusters/orac/apps/gonk, not in this repo) reads:
//
//	filterTags:
//	  pattern: '^v(?P<ver>\d+\.\d+\.\d+)-(?P<build>\d+)\.[0-9a-f]{12}$'
//	  extract: '$build'
//	policy:
//	  numerical: {order: asc}
//
// Flux's image-reflector uses Go's regexp -- the same engine as this test --
// and calls MatchString, which is UNANCHORED. The `^` and `$` in the pattern
// are therefore load-bearing: they are what rejects the per-arch legs
// ($GONK_TAG-amd64 / -arm64), the testclock meter ($GONK_TAG-testclock), and
// every legacy v0.1.0-<sha12> tag already in the registry. This test calls
// MatchString for the same reason -- using FindString or adding anchors of its
// own would test a pattern Flux does not run.
var fluxFilterTags = regexp.MustCompile(`^v(?P<ver>\d+\.\d+\.\d+)-(?P<build>\d+)\.[0-9a-f]{12}$`)

// tagScriptSites returns, per top-level .gitlab-ci.yml key, the first
// before_script entry that computes GONK_TAG.
//
// It asks WHAT THE SITE RESOLVES TO, not how it got there. Today all three
// call sites use a YAML anchor (&gonk_tag_script / *gonk_tag_script), and the
// parser expands aliases, so identical strings come back. That is deliberate:
// a future refactor to `!reference`, an include, or anything else that still
// yields ONE definition must keep passing, while a copy-paste second
// definition must fail. The property is single-sourcing; the anchor is merely
// today's mechanism.
func tagScriptSites(t *testing.T, doc map[string]any) map[string]string {
	t.Helper()
	sites := map[string]string{}
	for name, v := range doc {
		job, ok := v.(map[string]any)
		if !ok {
			continue
		}
		steps, ok := job["before_script"].([]any)
		if !ok || len(steps) == 0 {
			continue
		}
		first, ok := steps[0].(string)
		if !ok || !strings.Contains(first, "GONK_TAG=") {
			continue
		}
		sites[name] = first
	}
	return sites
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTagComputationHasExactlyOneDefinition is the single-source half of the
// property. Three jobs compute GONK_TAG (.gonk-tag, .gonk-kaniko, .gonk-index)
// and meter-testclock-index overrides before_script to re-run it; if any of
// them ever drifted, the branch-exclusion guarantee would hold for some image
// tags and not others -- and the ones it failed for would be the ones a
// feature branch published.
func TestTagComputationHasExactlyOneDefinition(t *testing.T) {
	sites := tagScriptSites(t, ciConfig(t))

	// Guard the guard. A scan that found nothing would make the identity
	// assertion below vacuously true.
	if len(sites) < 3 {
		t.Fatalf("found %d GONK_TAG computation site(s) in .gitlab-ci.yml (%v) -- expected at least 3 "+
			"(.gonk-tag, .gonk-kaniko, .gonk-index). Either the scan is broken or the tag computation "+
			"has been moved somewhere this gate cannot see it", len(sites), sortedKeys(sites))
	}

	names := sortedKeys(sites)
	first := sites[names[0]]
	for _, n := range names[1:] {
		if sites[n] != first {
			t.Fatalf("GONK_TAG is computed DIFFERENTLY in %q and %q -- there must be exactly one "+
				"definition, or the branch-exclusion property holds for some images and not others.\n"+
				"%s:\n%s\n%s:\n%s", names[0], n, names[0], first, n, sites[n])
		}
	}
	t.Logf("one shared GONK_TAG definition across %d sites: %s", len(names), strings.Join(names, ", "))
}

// runTagScript executes the extracted snippet under `sh` with exactly the
// environment given (nothing inherited but PATH, so a stray CI_* in the
// developer's shell cannot change the answer) and returns the GONK_TAG it
// printed.
func runTagScript(t *testing.T, script string, env map[string]string) string {
	t.Helper()
	root := repoRoot(t)

	// PATH only. The snippet needs printf/cut; it must not see anything else.
	full := []string{"PATH=/usr/bin:/bin:/usr/local/bin"}
	for k, v := range env {
		full = append(full, k+"="+v)
	}
	sort.Strings(full)

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = root // the snippet sources images/versions.env by relative path
	cmd.Env = full
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the extracted tag snippet failed: %v\nenv: %v\noutput:\n%s", err, full, out)
	}

	const prefix = "GONK_TAG="
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("the tag snippet printed no %s line.\nenv: %v\noutput:\n%s", prefix, full, out)
	return ""
}

// testSHA is a made-up 40-char commit sha. Only its first 12 characters reach
// the tag; they are hex and not all-digits, which is the ordinary case.
const testSHA = "c0fe5bcb2c0c9a8b7d6e5f4a3b2c1d0e9f8a7b6c"

const testSHA12 = "c0fe5bcb2c0c"

// TestNoNonDefaultBranchCanEmitARankableTag is the gate. It EXECUTES the
// snippet .gitlab-ci.yml actually ships, under every CI environment that can
// reach it, and asserts on the printed tag -- not on a Go restatement of the
// shell condition, which would pass even if that condition were inverted.
func TestNoNonDefaultBranchCanEmitARankableTag(t *testing.T) {
	sites := tagScriptSites(t, ciConfig(t))
	if len(sites) == 0 {
		t.Fatal("no GONK_TAG computation found in .gitlab-ci.yml -- the scan is broken, not the repo")
	}
	script := sites[sortedKeys(sites)[0]]

	cases := []struct {
		name string
		env  map[string]string
		// want is the exact tag the snippet must print.
		want string
		// rankable says whether the Flux ImagePolicy must be able to select
		// it. Exactly one scenario below may set this.
		rankable bool
	}{
		{
			name: "default branch, pipeline IID present",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "main",
				"CI_DEFAULT_BRANCH": "main", "CI_PIPELINE_IID": "337",
			},
			want: "v0.1.0-337." + testSHA12, rankable: true,
		},
		{
			name: "feature branch (the case this property exists for)",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "fix/tag-shape-collateral",
				"CI_DEFAULT_BRANCH": "main", "CI_PIPELINE_IID": "999",
			},
			want: "v0.1.0-" + testSHA12,
		},
		{
			name: "branch name that merely CONTAINS the default branch name",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "mainline",
				"CI_DEFAULT_BRANCH": "main", "CI_PIPELINE_IID": "999",
			},
			want: "v0.1.0-" + testSHA12,
		},
		{
			name: "CI_COMMIT_BRANCH unset -- a tag or merge-request pipeline",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_DEFAULT_BRANCH": "main",
				"CI_PIPELINE_IID": "999",
			},
			want: "v0.1.0-" + testSHA12,
		},
		{
			name: "CI_PIPELINE_IID unset, on the default branch",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "main",
				"CI_DEFAULT_BRANCH": "main",
			},
			want: "v0.1.0-" + testSHA12,
		},
		{
			name: "renamed default branch, building THAT branch",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "trunk",
				"CI_DEFAULT_BRANCH": "trunk", "CI_PIPELINE_IID": "42",
			},
			want: "v0.1.0-42." + testSHA12, rankable: true,
		},
		{
			name: "renamed default branch, building the now-ordinary `main`",
			env: map[string]string{
				"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "main",
				"CI_DEFAULT_BRANCH": "trunk", "CI_PIPELINE_IID": "42",
			},
			want: "v0.1.0-" + testSHA12,
		},
	}

	sawRankable := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runTagScript(t, script, tc.env)
			t.Logf("env %v -> GONK_TAG=%s", tc.env, got)
			if got != tc.want {
				t.Fatalf("GONK_TAG = %q, want %q", got, tc.want)
			}
			// The claim that matters is not the string but whether Flux can
			// SELECT it. Ask the pattern, the way Flux asks it.
			if fluxFilterTags.MatchString(got) != tc.rankable {
				t.Fatalf("the gitops ImagePolicy filterTags %s tag %q; this scenario requires it to %s it.\n"+
					"A non-default-branch build that the policy CAN select is branch code deploying to the "+
					"live cluster -- that is the whole reason the two tag shapes differ",
					map[bool]string{true: "MATCHES", false: "does NOT match"}[!tc.rankable],
					got, map[bool]string{true: "match", false: "reject"}[tc.rankable])
			}
		})
		if tc.rankable {
			sawRankable++
		}
	}

	// Guard the guard, the other way: if the pattern matched nothing at all
	// (a typo in fluxFilterTags, say), every `does not match` assertion above
	// would pass for the wrong reason.
	if sawRankable == 0 {
		t.Fatal("no scenario expected a rankable tag -- the positive control is missing and every negative assertion above is vacuous")
	}

	// And the derived tags CI publishes from GONK_TAG must not be selectable
	// either: the per-arch legs are single-architecture images and the
	// testclock meter must never reach production (its own source file says
	// so). These are the tags the `^...$` anchors exist to reject.
	ranked := "v0.1.0-337." + testSHA12
	for _, suffix := range []string{"-amd64", "-arm64", "-testclock", "-testclock-amd64"} {
		if fluxFilterTags.MatchString(ranked + suffix) {
			t.Errorf("the ImagePolicy pattern matches %q -- the anchors are not doing their job; "+
				"Flux could select a single-arch or testclock image", ranked+suffix)
		}
	}
	// A legacy sha-only tag must stay unselectable however old it is.
	if fluxFilterTags.MatchString("v0.1.0-95e4599f470d") {
		t.Error("the ImagePolicy pattern matches a legacy v0.1.0-<sha12> tag -- every branch build in the registry just became deployable")
	}
}

// TestTagScriptWithNoBranchVariablesAtAll records a DEGENERATE CASE honestly
// rather than leaving it undiscovered.
//
// The condition is `[ -n "$CI_PIPELINE_IID" ] && [ "$CI_COMMIT_BRANCH" = "$CI_DEFAULT_BRANCH" ]`.
// With BOTH branch variables unset, the string comparison is "" = "" -- TRUE --
// so an IID alone is enough to produce the rankable form. Measured, not
// reasoned: this test runs it.
//
// It is not reachable from GitLab CI, which sets CI_DEFAULT_BRANCH as a
// predefined variable in every pipeline; CI_COMMIT_BRANCH is the one that goes
// missing (tag and merge-request pipelines), and that case is covered above
// and is safe. So this is asserted as OBSERVED BEHAVIOUR, not as an approved
// design: if the snippet is ever run anywhere GitLab does not populate
// CI_DEFAULT_BRANCH, it emits a tag Flux can select. Filed as gonk-nx9z.
func TestTagScriptWithNoBranchVariablesAtAll(t *testing.T) {
	sites := tagScriptSites(t, ciConfig(t))
	if len(sites) == 0 {
		t.Fatal("no GONK_TAG computation found in .gitlab-ci.yml -- the scan is broken, not the repo")
	}
	script := sites[sortedKeys(sites)[0]]

	got := runTagScript(t, script, map[string]string{
		"CI_COMMIT_SHA": testSHA, "CI_PIPELINE_IID": "999",
	})
	t.Logf("both branch variables unset, IID set -> GONK_TAG=%s (rankable=%v)", got, fluxFilterTags.MatchString(got))

	if got != "v0.1.0-999."+testSHA12 {
		t.Fatalf("GONK_TAG = %q with both branch variables unset; this test pins the CURRENT behaviour so a "+
			"change to it is noticed. If the snippet was hardened to require a non-empty CI_COMMIT_BRANCH "+
			"(which would be an improvement), update this test and close gonk-nx9z", got)
	}
}

// TestTheTagShapeGateActuallyFails is the negative control, in the house style
// of TestTheAutoCancelGateActuallyFails. A gate that never goes red for a
// broken input is not a gate, and this one's whole claim is that it would
// catch an INVERTED condition -- the failure a Go restatement of the shell
// could not catch.
//
// It feeds runTagScript a deliberately wrong snippet (the real one with the
// branch test negated) and asserts that the feature-branch scenario then
// produces a tag the ImagePolicy CAN select, i.e. that the assertion in
// TestNoNonDefaultBranchCanEmitARankableTag would have fired.
func TestTheTagShapeGateActuallyFails(t *testing.T) {
	sites := tagScriptSites(t, ciConfig(t))
	if len(sites) == 0 {
		t.Fatal("no GONK_TAG computation found in .gitlab-ci.yml -- the scan is broken, not the repo")
	}
	real := sites[sortedKeys(sites)[0]]

	// Invert the branch test: `=` becomes `!=`. Derived from the shipped
	// snippet rather than hand-written, so this control tracks the real code.
	const cond = `[ "$CI_COMMIT_BRANCH" = "$CI_DEFAULT_BRANCH" ]`
	if !strings.Contains(real, cond) {
		t.Fatalf("the shipped snippet no longer contains %s, so this negative control cannot mutate it. "+
			"Rewrite the mutation to match the new condition -- do NOT delete this test, it is the only "+
			"thing proving the gate above has teeth.\nsnippet:\n%s", cond, real)
	}
	broken := strings.Replace(real, cond,
		`[ "$CI_COMMIT_BRANCH" != "$CI_DEFAULT_BRANCH" ]`, 1)

	got := runTagScript(t, broken, map[string]string{
		"CI_COMMIT_SHA": testSHA, "CI_COMMIT_BRANCH": "fix/whatever",
		"CI_DEFAULT_BRANCH": "main", "CI_PIPELINE_IID": "999",
	})
	t.Logf("inverted snippet, feature branch -> GONK_TAG=%s", got)
	if !fluxFilterTags.MatchString(got) {
		t.Fatalf("an INVERTED tag condition still produced an unrankable tag %q -- the gate above proves "+
			"nothing, because the thing it claims to detect does not change the observed output", got)
	}
}
