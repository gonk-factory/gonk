//go:build chart

package charttest

// G23, the image floor (gonk-xsk3): the render refuses an image older than the
// chart needs. internal/buildgate/image_contract_test.go holds the floors to
// chart/IMAGE-CONTRACT; these prove the render actually enforces them.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/internal/keydomain"
)

// The controller image dev actually ran chart 0.1.10 against on 2026-09-12,
// built before `gonk-gate sweep-health` existed. It is below every floor this
// chart has ever carried, which is the only property these tests need of it.
const outageControllerTag = "v0.1.0-341.6e9441a7e028"

// THE FLOORS COME FROM Chart.yaml, NOT FROM CONSTANTS HERE (gonk-jth4).
//
// They used to be copied into this file, so moving a floor -- the ordinary way
// this chart changes -- made these tests fail for a reason that was not a fault:
// the copy was stale. That happened when the controller floor went 343 -> 412.
// Reading the chart means a floor change is exercised by these tests instead of
// breaking them, and it is why `make gate` can now run them (Makefile: goldens).
type floorSpec struct {
	name, value, tag string
	build            int
}

var floorTag = regexp.MustCompile(`^v(\d+\.\d+\.\d+)-(\d+)\.([0-9a-f]{12})$`)

// chartFloorSpecs returns the floor of every image the chart runs a binary on,
// with the values path the guard names for it.
func chartFloorSpecs(t *testing.T) []floorSpec {
	t.Helper()
	valuePath := map[string]string{
		"controller": "gascity.image.tag",
		"intake":     "intake.image.tag",
		"meter":      "meter.image.tag",
	}
	floors := chartFloors(t)
	var out []floorSpec
	for _, name := range []string{"controller", "intake", "meter"} {
		tag := floors[name]
		if tag == "" {
			t.Fatalf("Chart.yaml has no image floor for %s; internal/buildgate's image contract check requires one", name)
		}
		m := floorTag.FindStringSubmatch(tag)
		if m == nil {
			t.Fatalf("the %s floor %q is not a default-branch tag, so G23 cannot rank anything against it", name, tag)
		}
		build, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, floorSpec{name: name, value: valuePath[name], tag: tag, build: build})
	}
	return out
}

// withBuild rewrites a floor tag to another build number, keeping the shape the
// ImagePolicy selects. The sha is not compared by anything in the render.
func (f floorSpec) withBuild(build int) string {
	m := floorTag.FindStringSubmatch(f.tag)
	return fmt.Sprintf("v%s-%d.%s", m[1], build, m[3])
}

// atFloors renders with every image exactly at its own floor, except the ones
// overridden by name.
func atFloors(t *testing.T, override map[string]string, extra ...string) []string {
	t.Helper()
	tags := map[string]string{}
	for _, f := range chartFloorSpecs(t) {
		if tag, ok := override[f.name]; ok {
			tags[f.name] = tag
		} else {
			tags[f.name] = f.tag
		}
	}
	return ranked(tags["controller"], tags["intake"], tags["meter"], extra...)
}

// ranked is Minimum() with every gonk image on a rankable tag and the unranked
// opt-in turned back OFF, which is how a Flux-fed cluster renders.
func ranked(controller, intake, meter string, extra ...string) []string {
	return append(append(Minimum(),
		"--set", "imageFloor.acceptUnrankedTags=false",
		"--set", "gascity.image.tag="+controller,
		"--set", "intake.image.tag="+intake,
		"--set", "meter.image.tag="+meter,
	), extra...)
}

func chartFloors(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(SourceChartDir(), "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Annotations map[string]string `yaml:"annotations"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	floors := map[string]string{}
	for k, v := range c.Annotations {
		if name, ok := strings.CutPrefix(k, keydomain.Prefix+"image-floor."); ok {
			floors[name] = v
		}
	}
	return floors
}

// THE OUTAGE, REPLAYED: this chart (whose controller readinessProbe runs
// gonk-gate sweep-health, as 0.1.10's did) against the image dev ran it on.
// The render must refuse -- before anything is applied -- and must say which
// value, which build, and that the running release is untouched.
func TestImageFloorRefusesTheSweepHealthOutageCombination(t *testing.T) {
	controller := chartFloorSpecs(t)[0]
	out, err := RenderErr(t, atFloors(t, map[string]string{"controller": outageControllerTag})...)
	if err == nil {
		t.Fatalf("the chart RENDERED against controller %s, the image that has no sweep-health. This is the 2026-09-12 outage.", outageControllerTag)
	}
	for _, want := range []string{
		"IMAGE FLOOR (gonk-xsk3)",
		"gascity.image.tag is " + outageControllerTag + " (build 341)",
		fmt.Sprintf("needs the controller image from build %d", controller.build),
		"the running release is untouched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refused, but the message does not say %q:\n%s", want, out)
		}
	}
}

func TestImageFloorAdmitsTheFloorAndAnythingNewer(t *testing.T) {
	specs := chartFloorSpecs(t)
	// Each image exactly at ITS OWN floor. They are not all the same build.
	Render(t, atFloors(t, nil)...)
	// Each image one build past its floor.
	next := map[string]string{}
	for _, f := range specs {
		next[f.name] = f.withBuild(f.build + 1)
	}
	Render(t, atFloors(t, next)...)
	// One image far ahead neither excuses nor holds back another.
	Render(t, atFloors(t, map[string]string{"controller": specs[0].withBuild(specs[0].build + 9000)})...)
}

// Every floor is enforced on its OWN image. One image lagging behind its floor
// is refused even when the others are current -- and ONLY that image is named.
func TestImageFloorIsEnforcedPerImage(t *testing.T) {
	for _, f := range chartFloorSpecs(t) {
		t.Run(f.name, func(t *testing.T) {
			// ONE build below ITS OWN floor, everything else exactly at its own.
			// With floors that differ per image, a shared "old tag" would not
			// prove this: it would be below all three at once.
			lagging := f.withBuild(f.build - 1)
			out, err := RenderErr(t, atFloors(t, map[string]string{f.name: lagging})...)
			if err == nil {
				t.Fatalf("%s at build %d rendered against its floor of %d", f.name, f.build-1, f.build)
			}
			if !strings.Contains(out, f.value+" is "+lagging) {
				t.Errorf("refused for the wrong image; wanted %s named:\n%s", f.value, out)
			}
			for _, other := range chartFloorSpecs(t) {
				if other.name != f.name && strings.Contains(out, other.value+" is ") {
					t.Errorf("the refusal also names %s, which is at its floor:\n%s", other.name, out)
				}
			}
		})
	}
}

// The comparison is NUMERIC, and the tags are chosen so that a STRING
// comparison would get both cases backwards: a power of ten above the floor
// sorts before it ("1000" < "412"), and a run of nines below it sorts after
// ("99" > "412").
func TestImageFloorComparesBuildNumbersNumerically(t *testing.T) {
	controller := chartFloorSpecs(t)[0]
	above := 1
	for above <= controller.build {
		above *= 10
	}
	below, _ := strconv.Atoi(strings.Repeat("9", len(strconv.Itoa(controller.build))-1))
	if above <= controller.build || below >= controller.build {
		t.Fatalf("the probes are wrong for a floor of %d: above=%d below=%d", controller.build, above, below)
	}
	Render(t, atFloors(t, map[string]string{"controller": controller.withBuild(above)})...)
	if _, err := RenderErr(t, atFloors(t, map[string]string{"controller": controller.withBuild(below)})...); err == nil {
		t.Fatalf("build %d rendered against the controller floor of %d", below, controller.build)
	}
}

// A disabled component deploys no image, so its floor has nothing to guard.
func TestImageFloorIgnoresADisabledComponent(t *testing.T) {
	Render(t, atFloors(t, map[string]string{"controller": outageControllerTag},
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=http://gc.example.test:9443")...)
}

// A tag with no build number is refused unless the operator attests the images
// came from the chart's own commit.
func TestImageFloorRefusesUnrankedTagsUnlessAttested(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "e2e-run42", "v0.1.0-a434b6fb3cac", "v0.1.0-343.a434b6fb3cac-amd64"} {
		t.Run(tag, func(t *testing.T) {
			out, err := RenderErr(t, atFloors(t, map[string]string{"controller": tag})...)
			if err == nil {
				t.Fatalf("unranked tag %q rendered without imageFloor.acceptUnrankedTags", tag)
			}
			if !strings.Contains(out, "has no build number") || !strings.Contains(out, "imageFloor.acceptUnrankedTags=true") {
				t.Errorf("refused, but did not say why or how:\n%s", out)
			}
			Render(t, atFloors(t, map[string]string{"controller": tag}, "--set", "imageFloor.acceptUnrankedTags=true")...)
		})
	}
	// The attestation is about UNRANKED tags only. It must not excuse a ranked
	// tag below the floor -- or it would be a switch that turns the floor off.
	if _, err := RenderErr(t, atFloors(t, map[string]string{"controller": outageControllerTag}, "--set", "imageFloor.acceptUnrankedTags=true")...); err == nil {
		t.Fatal("imageFloor.acceptUnrankedTags=true let a RANKED tag below the floor render; it has become an off switch")
	}
}

// A floor the guard cannot parse must fail the render, never be skipped.
func TestImageFloorRefusesAMalformedFloor(t *testing.T) {
	dir := t.TempDir()
	src := ChartDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dir, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "Chart.yaml" {
			re := regexp.MustCompile(`(?m)^(\s*` + regexp.QuoteMeta(keydomain.Prefix+"image-floor.controller") + `:).*$`)
			if !re.Match(b) {
				t.Fatalf("Chart.yaml has no controller floor to corrupt")
			}
			b = re.ReplaceAll(b, []byte("${1} v0.1.0-a434b6fb3cac"))
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := helm(t, append([]string{"template", "gonk", dir, "--namespace", "gonk"}, atFloors(t, nil)...)...)
	if err == nil {
		t.Fatal("a chart whose controller floor is not a rankable tag rendered")
	}
	if !strings.Contains(out, "is not a default-branch image tag") {
		t.Errorf("wrong refusal:\n%s", out)
	}
}

// imageFloor absent entirely (a values file that never mentions it, or an
// operator who nulls the block). The guard must produce ITS message, not a
// template nil-pointer -- an error nobody can act on, and one that would be
// read as "the chart is broken" rather than "this tag cannot be ranked".
func TestImageFloorSurvivesTheWholeBlockBeingAbsent(t *testing.T) {
	out, err := RenderErr(t, append(Minimum(), "--set", "imageFloor=null")...)
	if err == nil {
		t.Fatal("an unranked tag rendered with imageFloor absent; the guard was skipped")
	}
	if strings.Contains(out, "nil pointer") {
		t.Fatalf("the guard died on a nil pointer instead of refusing with its message:\n%s", out)
	}
	if !strings.Contains(out, "has no build number") {
		t.Errorf("wrong refusal:\n%s", out)
	}
}
