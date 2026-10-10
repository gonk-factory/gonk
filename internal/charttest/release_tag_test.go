//go:build chart

package charttest

// release-unit (docs/plans/2026-09-26-release-unit.md, item 2): EVERY gonk image
// deploys under ONE release tag. `releaseTag` drives the controller, intake,
// meter and the agent image the controller hands to session pods
// (GC_K8S_IMAGE, and GONK_AGENT_IMAGE for the Jobs runtime); a component's own
// image.tag overrides it for that component only; and a render where an enabled component resolves to NO tag fails (G24).
//
// WHY. Dev's agent and controller tags were bumped by four independent Flux
// ImagePolicies in separate commits -- 0 of 68 automation commits moved both --
// so for up to 20 minutes the controller ran against the previous agent image.
// One value cannot disagree with itself.
//
// These run in `make gate` (goldens -run '^TestReleaseTag'), pinned there by
// internal/buildgate TestGateRunsEveryReleaseTagChartTest.

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/envname"
)

// A rankable default-branch tag above every floor Chart.yaml carries, so the
// floor guard (G23) is live in these renders, not attested away.
const releaseUnitTag = "v0.1.0-99000.0123456789ab"

// agentImageEnv are the controller env vars that carry the agent image a
// session pod runs: GC_K8S_IMAGE (Gas City) and GONK_AGENT_IMAGE (the Jobs
// runtime, gonk-t55). Every one rendered is asserted; that BOTH are rendered is
// TestReleaseTagDrivesBothAgentImageEnvs.
var agentImageEnv = []string{"GC_K8S_IMAGE", envname.AgentImage}

// gonkImageRef recognises a gonk image BY ITS NAME -- the last path element,
// `gonk-<component>` -- and captures the registry path in front of it.
//
// It used to key on one registry's path element, so it described a single
// installation: under any other image.registry it matched nothing, every
// reference was "not a gonk image", and the tests said so by failing for a
// reason that was not a fault. The registry is a value; the image name is the
// contract. What registry a render uses is asserted on its own
// (TestDefaultRenderUsesThePublicRegistryAndNoPullSecret,
// TestPrivateRegistryAndPullSecretReachEveryPod), from imageUse.registry.
var gonkImageRef = regexp.MustCompile(`^(?:(.+)/)?(gonk-[a-z0-9-]+):([^@\s"/]*)$`)

// imageUse is one place a gonk image reference was rendered.
type imageUse struct {
	where, registry, image, tag string
}

// gonkImageUses returns every gonk image reference in a render: every
// container and initContainer of every pod-bearing object (Deployments,
// StatefulSets, the helm-test Pod), plus the agent image env values. Third-party
// images (dolt) are not gonk's and are not returned.
func gonkImageUses(t *testing.T, objs []Object) []imageUse {
	t.Helper()
	var out []imageUse
	for _, o := range objs {
		var pspec map[string]any
		switch o.Kind {
		case "Pod":
			pspec, _ = o.Raw["spec"].(map[string]any)
		case "Deployment", "StatefulSet", "Job", "DaemonSet":
			spec, _ := o.Raw["spec"].(map[string]any)
			tmpl, _ := spec["template"].(map[string]any)
			pspec, _ = tmpl["spec"].(map[string]any)
		default:
			continue
		}
		for _, group := range []string{"containers", "initContainers"} {
			list, _ := pspec[group].([]any)
			for _, c := range list {
				cm, _ := c.(map[string]any)
				cname, _ := cm["name"].(string)
				img, _ := cm["image"].(string)
				if m := gonkImageRef.FindStringSubmatch(img); m != nil {
					out = append(out, imageUse{o.Kind + "/" + o.Metadata.Name + "/" + cname, m[1], m[2], m[3]})
				}
				envs, _ := cm["env"].([]any)
				for _, e := range envs {
					em, _ := e.(map[string]any)
					name, _ := em["name"].(string)
					val, _ := em["value"].(string)
					for _, want := range agentImageEnv {
						if name != want {
							continue
						}
						m := gonkImageRef.FindStringSubmatch(val)
						if m == nil {
							t.Errorf("%s/%s env %s=%q is not a gonk image reference (<registry>/gonk-<component>:<tag>)", o.Metadata.Name, cname, name, val)
							continue
						}
						out = append(out, imageUse{o.Kind + "/" + o.Metadata.Name + "/" + cname + " env " + name, m[1], m[2], m[3]})
					}
				}
			}
		}
	}
	return out
}

func describeUses(uses []imageUse) string {
	var lines []string
	for _, u := range uses {
		lines = append(lines, u.where+" -> "+u.image+":"+u.tag)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n  ")
}

// requireOneTag asserts every use carries `want`, and that the render is not
// vacuous: each image in `mustSee` appears at least once.
func requireOneTag(t *testing.T, uses []imageUse, want string, mustSee ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, u := range uses {
		seen[u.image] = true
		if u.tag != want {
			t.Errorf("%s runs %s:%s, not the release tag %s", u.where, u.image, u.tag, want)
		}
	}
	for _, img := range mustSee {
		if !seen[img] {
			t.Errorf("no %s image in the render at all, so the one-tag assertion says nothing about it.\nrendered:\n  %s",
				img, describeUses(uses))
		}
	}
}

// The default render -- the minimum values, which set ONLY releaseTag -- runs
// every gonk image under that one tag, INCLUDING the agent image the controller
// hands to session pods.
func TestReleaseTagDrivesEveryGonkImageByDefault(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "imageFloor.acceptUnrankedTags=false",
		"--set", "releaseTag="+releaseUnitTag)...)
	uses := gonkImageUses(t, Objects(t, out))
	requireOneTag(t, uses, releaseUnitTag, "gonk-controller", "gonk-intake", "gonk-meter", "gonk-agent")

	var sawAgentEnv bool
	for _, u := range uses {
		if strings.Contains(u.where, " env ") {
			sawAgentEnv = true
		}
	}
	if !sawAgentEnv {
		t.Errorf("no agent image env (%v) rendered on the controller; the controller<->agent skew is exactly what this must cover.\n  %s",
			agentImageEnv, describeUses(uses))
	}
}

// BOTH AGENT IMAGE ENVS FOLLOW releaseTag (t55 onto release-unit, 2026-10-03).
// With the Jobs runtime the controller hands out the agent image twice:
// GC_K8S_IMAGE to Gas City and GONK_AGENT_IMAGE to gonk-gate's own Job
// dispatch. The test above is satisfied by EITHER one, so a GONK_AGENT_IMAGE
// built from componentImages.agent.tag alone -- ".../gonk-agent:" whenever only
// releaseTag is set, which is how dev is configured -- would pass it while every
// Jobs session failed InvalidImageName. This requires BOTH, each on the release
// tag, and a per-component agent override to move BOTH together.
func TestReleaseTagDrivesBothAgentImageEnvs(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		extra      []string
	}{
		{"releaseTag only", releaseUnitTag, nil},
		{"agent override", releaseUnitTag + "-pinned", []string{"--set", "componentImages.agent.tag=" + releaseUnitTag + "-pinned"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(Minimum(),
				"--set", "imageFloor.acceptUnrankedTags=true",
				"--set", "releaseTag="+releaseUnitTag)
			out := Render(t, append(args, tc.extra...)...)
			got := map[string]string{}
			for _, u := range gonkImageUses(t, Objects(t, out)) {
				for _, env := range agentImageEnv {
					if strings.HasSuffix(u.where, " env "+env) {
						if u.image != "gonk-agent" {
							t.Errorf("%s carries %s, not gonk-agent", u.where, u.image)
						}
						got[env] = u.tag
					}
				}
			}
			for _, env := range agentImageEnv {
				tag, ok := got[env]
				if !ok {
					t.Errorf("%s is not rendered on the controller; both agent image envs must be asserted", env)
					continue
				}
				if tag != tc.want {
					t.Errorf("%s renders gonk-agent:%q, want the tag %q -- one release must run ONE agent image", env, tag, tc.want)
				}
			}
		})
	}
}

// Every shipped profile is written the release-unit way, and renders one tag
// across every gonk image it deploys.
func TestReleaseTagOneTagInEveryProfile(t *testing.T) {
	for _, profile := range goldenProfiles {
		t.Run(profile, func(t *testing.T) {
			out := Render(t, "--values", filepath.Join(ChartDir(), "ci", profile+".yaml"))
			uses := gonkImageUses(t, Objects(t, out))
			if len(uses) == 0 {
				t.Fatal("rendered no gonk image at all")
			}
			requireOneTag(t, uses, uses[0].tag)
			if uses[0].tag == "" {
				t.Fatalf("the profile's gonk images render with an EMPTY tag:\n  %s", describeUses(uses))
			}
		})
	}
}

// A per-component tag still wins -- for that component ONLY. (e2e pins the
// testclock meter this way.) Everything else stays on the release tag.
func TestReleaseTagPerComponentOverrideMovesOnlyThatImage(t *testing.T) {
	override := releaseUnitTag + "-testclock"
	out := Render(t, append(Minimum(),
		"--set", "imageFloor.acceptUnrankedTags=true",
		"--set", "releaseTag="+releaseUnitTag,
		"--set", "meter.image.tag="+override)...)
	var meter, rest []imageUse
	for _, u := range gonkImageUses(t, Objects(t, out)) {
		if u.image == "gonk-meter" {
			meter = append(meter, u)
		} else {
			rest = append(rest, u)
		}
	}
	if len(meter) == 0 {
		t.Fatal("no gonk-meter image rendered")
	}
	requireOneTag(t, meter, override)
	requireOneTag(t, rest, releaseUnitTag, "gonk-controller", "gonk-intake", "gonk-agent")
}

// THE AGENT TAG CAN NO LONGER RENDER EMPTY. Before release-unit, a values file
// that did not set componentImages.agent.tag rendered
// GC_K8S_IMAGE=".../gonk-agent:" -- six of the eight shipped profiles did --
// and the kubelet refuses that as InvalidImageName only when a SESSION pod is
// created, long after the release reported success.
func TestReleaseTagEmptyAgentTagFailsTheRender(t *testing.T) {
	out, err := RenderErr(t, append(Minimum(),
		"--set", "releaseTag=",
		"--set", "gascity.image.tag=v0.1.0",
		"--set", "intake.image.tag=v0.1.0",
		"--set", "meter.image.tag=v0.1.0")...)
	if err == nil {
		t.Fatalf("rendered with no agent tag at all; GC_K8S_IMAGE would be `gonk-agent:`:\n%s", grepLines(out, "GC_K8S_IMAGE", 1))
	}
	for _, want := range []string{"componentImages.agent.tag is empty and so is releaseTag", `"gonk-agent:"`, "InvalidImageName"} {
		if !strings.Contains(out, want) {
			t.Errorf("refused, but the message does not say %q:\n%s", want, out)
		}
	}
}

// ...and the same holds for every other enabled gonk image: each resolved tag
// is checked, and the refusal names the one that is empty.
func TestReleaseTagEmptyResolvedTagFailsForEachImage(t *testing.T) {
	keys := []string{"gascity.image.tag", "intake.image.tag", "meter.image.tag", "componentImages.agent.tag"}
	for _, empty := range keys {
		t.Run(empty, func(t *testing.T) {
			args := append(Minimum(), "--set", "releaseTag=")
			for _, k := range keys {
				if k != empty {
					args = append(args, "--set", k+"=v0.1.0")
				}
			}
			out, err := RenderErr(t, args...)
			if err == nil {
				t.Fatalf("rendered with %s and releaseTag both empty", empty)
			}
			if !strings.Contains(out, empty+" is empty and so is releaseTag") {
				t.Errorf("refused, but not for %s:\n%s", empty, out)
			}
		})
	}
}

// With no controller bundled, nothing hands the agent image to anyone, so an
// unset agent tag is not an error (the byo-gascity profile).
func TestReleaseTagAgentNotRequiredWithoutABundledController(t *testing.T) {
	Render(t, append(Minimum(),
		"--set", "releaseTag=",
		"--set", "intake.image.tag=v0.1.0",
		"--set", "meter.image.tag=v0.1.0",
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.external.svc:9443")...)
}

// G11 holds for the release tag too: a floating tag cannot be rolled back or
// reasoned about.
func TestReleaseTagSchemaRejectsLatest(t *testing.T) {
	if _, err := RenderErr(t, append(Minimum(), "--set", "releaseTag=latest")...); err == nil {
		t.Fatal("releaseTag=latest was accepted")
	}
}

// The image floor (G23) compares the RESOLVED tag, and when that came from
// releaseTag the refusal says releaseTag is the value to change.
func TestReleaseTagIsWhatTheImageFloorCompares(t *testing.T) {
	lagging := "v0.1.0-1.0123456789ab"
	out, err := RenderErr(t, append(Minimum(),
		"--set", "imageFloor.acceptUnrankedTags=false",
		"--set", "releaseTag="+lagging)...)
	if err == nil {
		t.Fatalf("releaseTag at build 1 rendered against the image floors")
	}
	if !strings.Contains(out, "releaseTag (gascity.image.tag unset) is "+lagging) {
		t.Errorf("refused, but the message does not name releaseTag as the value to change:\n%s", out)
	}
}

// grepLines returns up to n lines of s containing sub, for failure messages.
func grepLines(s, sub string, n int) string {
	var hits []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.Contains(ln, sub) {
			hits = append(hits, ln)
			if len(hits) == n {
				break
			}
		}
	}
	return strings.Join(hits, "\n")
}
