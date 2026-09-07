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
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// GoldenChartVersion is the version every render is normalized to.
//
// WHY THE GOLDENS DO NOT SEE THE REAL VERSION (gonk-sjb): _helpers.tpl emits
// `helm.sh/chart: <name>-<version>` on every object -- 231 occurrences across
// the eight profiles -- and the controller's `checksum/operator-config`
// annotation hashes a ConfigMap that itself carries that label. So a plain
// 0.1.0 -> 0.1.1 bump moved 478 golden lines, none of them a real change.
//
// Under reconcileStrategy: ChartVersion every chart edit MUST bump the version,
// so that churn would land on top of every genuine chart diff forever -- in the
// one gate whose stated purpose is that a moved volumeMount must not look like
// a formatting change. Normalizing here makes a version bump a ZERO-line golden
// diff. The real version is not unchecked: internal/buildgate's chart seal binds
// it to the chart's content hash, which the goldens never did.
const GoldenChartVersion = "0.0.0-golden"

// SourceChartDir is the real chart/gonk in the working tree, resolved from this
// file's own location so the tests do not care where `go test` was invoked.
// Use it when you mean the chart AS COMMITTED; use ChartDir to render.
func SourceChartDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "chart", "gonk")
}

var (
	normOnce sync.Once
	normDir  string
	normErr  error
)

// ChartDir is a version-normalized COPY of chart/gonk, built once per test
// binary. Everything that renders or lints goes through it -- including
// Profile(), whose ci/ values files are copied alongside -- so no caller has to
// know normalization happened.
func ChartDir() string {
	normOnce.Do(func() { normDir, normErr = normalizeChart(SourceChartDir()) })
	if normErr != nil {
		panic("charttest: cannot build the version-normalized chart copy: " + normErr.Error())
	}
	return normDir
}

var chartVersionLine = regexp.MustCompile(`(?m)^version:\s*\S+\s*$`)

// normalizeChart copies the chart to a temp dir with Chart.yaml's version
// rewritten to GoldenChartVersion. Only that one line changes; everything else
// is byte-identical, so the goldens still assert the whole chart.
func normalizeChart(src string) (string, error) {
	dst, err := os.MkdirTemp("", "gonk-chart-normalized-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "Chart.yaml" {
			replaced := chartVersionLine.ReplaceAll(b, []byte("version: "+GoldenChartVersion))
			if bytes.Equal(replaced, b) {
				return fmt.Errorf("no top-level `version:` line in Chart.yaml to normalize")
			}
			b = replaced
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		os.RemoveAll(dst)
		return "", err
	}
	return dst, nil
}

// cleanupNormalizedChart is called from TestMain.
func cleanupNormalizedChart() {
	if normDir != "" {
		os.RemoveAll(normDir)
	}
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
		"--set", "dolt.image.tag=2.1.7",
		"--set", "intake.webhookPublicURL=https://gonk.example.test/hook/gitlab",
		"--set", "litellm.externalURL=http://litellm.litellm.svc:4000",
		// G22: the bundled controller is grant-gated (gc start --foreground, an
		// 0.0.0.0/allow_mutations [api] plane). A default render (gascity.enabled)
		// must supply the ed25519 PUBLIC verify key, exactly as a real operator
		// does; the value here is the repo golden vector's real pubkey. The private
		// signing-key Secret name defaults (secrets.gcWriteKey.existingSecret), so
		// only the pubkey is a required --set. See chart/gonk/smoke.
		"--set", "gascity.writeAuth.verifyKey=k1:1hcioE4eYD4PsM66wVJ8oBErEfCTyNPt9Q/+ZT0drmk=",
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
//
// The schema-location template below MUST match how this kubeconform binary
// actually substitutes it, which is NOT documented consistently across
// kubeconform versions: verified empirically (v0.6.7, see bead notes) by
// pointing -schema-location at a logging HTTP server and reading the requests
// it made. It lowercases {{.ResourceKind}} (Cluster -> cluster) and splits
// apiVersion on "/" into {{.Group}} (unset for core/v1) and
// {{.ResourceAPIVersion}} (just the version, e.g. "v1" -- NOT "group_v1"). A
// template of "{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" -- which matches
// neither the casing nor drops the group -- silently never resolves, and every
// CRD falls through to "could not find schema", which -strict without
// -ignore-missing-schemas turns into a hard failure. The vendored schema
// files live one directory per API group (chart/gonk/tests/crd-schemas/<group>/
// <lowercase-kind>_<version>.json) to keep the same Kind in two different
// groups from colliding.
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
			"-schema-location", filepath.Join(crdSchemaDir, "{{.Group}}", "{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"))
	}
	cmd := exec.Command("kubeconform", append(args, f)...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubeconform failed: %v\n%s", err, b.String())
	}
}
