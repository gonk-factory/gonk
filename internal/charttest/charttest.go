//go:build chart

// Package charttest renders chart/gonk with the real `helm` binary and lets Go
// tests assert on the result. It replaces `helm unittest` (spec 10.3): no plugin
// to install on an offline runner, and -- the reason that actually matters -- it
// can assert that `helm template` FAILS with a specific message, which is what
// every fail-closed guard in this chart is.
//
// Requires `helm` (>= 3.14) and, for kubeconform tests, `kubeconform` on PATH.
// Build tag `chart` keeps them out of the default `go test ./...` gate, because
// not every machine has helm. The standing gate is:
//
//	go test -tags chart ./internal/charttest/... -count=1
package charttest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ChartDir is chart/gonk, resolved from this file's own location so the tests do
// not care what directory `go test` was invoked from.
func ChartDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "chart", "gonk")
}

// Minimum is the smallest set of --set flags that satisfies values.schema.json.
// Every required value with no safe default is here; if this list grows, that is
// a NEW required value and it belongs in the chart README too.
func Minimum() []string {
	return []string{
		// Every bundled component's image tag: the DEFAULT render is the WHOLE
		// factory (umbrella model, owner decision 2026-07-13). The controller and
		// Dolt tags are provisional pins; the controller's port/delivery/workload
		// are smoke-gated (Task 0.5).
		"--set", "intake.image.tag=v0.1.0",
		"--set", "meter.image.tag=v0.1.0",
		"--set", "gascity.image.tag=v0.1.0",
		"--set", "dolt.image.tag=v1.43.0",
		"--set", "intake.webhookPublicURL=https://gonk.example.test/hook/gitlab",
		"--set", "litellm.externalURL=http://litellm.litellm.svc:4000",
		// gascity.supervisorURL is DERIVED when the controller is bundled (the
		// default). It is NOT set here, precisely so the default render exercises
		// the bundled path; the BYO path (gascity.enabled=false) supplies it.
		// The chart ships NO rung catalog and NO instance ladder (site-local; a
		// default naming a phantom model 404s at first token -- Plan 04 AD-1). So
		// a MINIMUM render must supply them, exactly as a real operator does via
		// HelmRelease values. `qwen-local` here is a TEST operator's choice, not a
		// chart default.
		"--set", "operatorConfig.instance.ladder={qwen-local}",
		"--set", "operatorConfig.rungs[0].name=qwen-local",
		"--set", "operatorConfig.rungs[0].kind=local",
		"--set", "operatorConfig.rungs[0].model=stub-local",
		"--set", "operatorConfig.rungs[0].est_cost_usd=0",
		"--set", "operatorConfig.rungs[0].est_tokens=200K",
		// helm's plain --set never infers float64 (only int64/bool/string), so a
		// decimal value here would arrive at values.schema.json as a STRING and
		// fail its "type": "number" check. --set-json parses the literal as JSON,
		// which is the only way to hand helm template a real float from the CLI.
		"--set-json", "operatorConfig.rungs[0].synthetic_usd_per_1m_tokens=0.20",
		"--set", "onboarding.defaultRung=qwen-local",
	}
}

func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is not on PATH; the chart gate needs it: %v", err)
	}
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

// Render runs `helm template` and fails the test if it errors.
func Render(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := RenderErr(t, extra...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// RenderErr runs `helm template` and RETURNS the error. This is the guard tests'
// entry point: a guard is proven by a render that fails with the right message.
func RenderErr(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"template", "gonk", ChartDir(), "--namespace", "gonk"}, extra...)
	return helm(t, args...)
}

// Lint runs `helm lint --strict`.
func Lint(t *testing.T, extra ...string) {
	t.Helper()
	args := append([]string{"lint", "--strict", ChartDir()}, extra...)
	if out, err := helm(t, args...); err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
}

// Profile returns the --values flag for a ci/ profile.
func Profile(name string) []string {
	return []string{"--values", filepath.Join(ChartDir(), "ci", name+".yaml")}
}

// Object is one rendered Kubernetes object, parsed just enough to assert on.
type Object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Raw map[string]any `yaml:"-"`
	Doc string         `yaml:"-"` // the original YAML text
}

func (o Object) Data(t *testing.T) map[string]string {
	t.Helper()
	d, _ := o.Raw["data"].(map[string]any)
	out := map[string]string{}
	for k, v := range d {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

// Objects splits a multi-document render into parsed objects, dropping empties.
func Objects(t *testing.T, out string) []Object {
	t.Helper()
	var objs []Object
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var raw map[string]any
		err := dec.Decode(&raw)
		if err != nil {
			break
		}
		if len(raw) == 0 {
			continue
		}
		b, _ := yaml.Marshal(raw)
		var o Object
		if err := yaml.Unmarshal(b, &o); err != nil {
			t.Fatalf("parse rendered object: %v\n%s", err, string(b))
		}
		o.Raw, o.Doc = raw, string(b)
		objs = append(objs, o)
	}
	return objs
}

// MustObject finds exactly one object by kind+name, or fails with the list of
// what WAS rendered (which is what you actually want when it is missing).
func MustObject(t *testing.T, out, kind, name string) Object {
	t.Helper()
	var have []string
	for _, o := range Objects(t, out) {
		if o.Kind == kind && o.Metadata.Name == name {
			return o
		}
		have = append(have, o.Kind+"/"+o.Metadata.Name)
	}
	t.Fatalf("no %s/%s in the render; got:\n  %s", kind, name, strings.Join(have, "\n  "))
	return Object{}
}

// NoObject asserts a kind+name is absent.
func NoObject(t *testing.T, out, kind, name string) {
	t.Helper()
	for _, o := range Objects(t, out) {
		if o.Kind == kind && o.Metadata.Name == name {
			t.Fatalf("%s/%s was rendered and must not be", kind, name)
		}
	}
}

// Kubeconform validates a render against the Kubernetes API schemas. CRDs
// (ServiceMonitor, PrometheusRule, CNPG Cluster) have no upstream schema, so
// they are validated against vendored schemas in chart/gonk/tests/crd-schemas/
// -- NOT with -ignore-missing-schemas, which would silently skip them.
func Kubeconform(t *testing.T, render string, crdSchemaDir string) {
	t.Helper()
	if _, err := exec.LookPath("kubeconform"); err != nil {
		t.Fatalf("kubeconform is not on PATH; the chart gate needs it: %v", err)
	}
	f := filepath.Join(t.TempDir(), "render.yaml")
	if err := os.WriteFile(f, []byte(render), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-strict", "-summary"}
	if crdSchemaDir != "" {
		args = append(args, "-schema-location", "default",
			"-schema-location", filepath.Join(crdSchemaDir, "{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"))
	}
	cmd := exec.Command("kubeconform", append(args, f)...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubeconform failed: %v\n%s", err, b.String())
	}
}
