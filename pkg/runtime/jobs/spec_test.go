package jobs

import (
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		Namespace:               "gonk",
		Image:                   "registry.example/gonk/agent:v1-7.abc",
		ServiceAccount:          "gc-agent",
		PullSecret:              "gitlab-registry-pull-creds",
		TTLSecondsAfterFinished: 3600,
		CPURequest:              resource.MustParse("500m"),
		MemoryRequest:           resource.MustParse("1Gi"),
		CPULimit:                resource.MustParse("2"),
		MemoryLimit:             resource.MustParse("4Gi"),
	}
}

const (
	testPrompt = "Triage issue 7: THE-PROMPT-TEXT"
	testKey    = "sk-THE-VIRTUAL-KEY-0123456789"
	testMeta   = `{"project":42,"issue":7}`
)

// testSpec is a Spec shaped like the one dispatch builds: the pack's [env]
// (HOME, XDG_*, OPENCODE_PERMISSION) plus D5's GONK_* names.
func testSpec() runtime.Spec {
	name := runtime.Name("triage", 42, runtime.KindIssue, 7, 1)
	return runtime.Spec{
		Name:  name,
		Agent: "triage",
		Env: map[string]string{
			"HOME":                 "/tmp/gonk-home",
			"XDG_CONFIG_HOME":      "/tmp/gonk-home/.config",
			"OPENCODE_PERMISSION":  `{"*":"allow","bash":"deny"}`,
			"GONK_LITELLM_URL":     "http://litellm:4000",
			"GONK_RIG_BASE_URL":    "http://gonk-intake:8080",
			"GONK_MODEL":           "gonk/triage",
			"GONK_ATTEMPT":         "1",
			"GONK_SESSION_RUNTIME": "jobs", // repeating a fixed value is allowed
		},
		Prompt:     []byte(testPrompt),
		LiteLLMKey: []byte(testKey),
		Metadata:   []byte(testMeta),
		Deadline:   t0.Add(20*time.Minute + 500*time.Millisecond),
		Labels:     map[string]string{"gonk-factory.github.io/project": "42"},
	}
}

func envMap(t *testing.T, c corev1.Container) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("env %s uses valueFrom; session env is literal values only", e.Name)
		}
		if _, dup := m[e.Name]; dup {
			t.Errorf("env %s appears twice", e.Name)
		}
		m[e.Name] = e.Value
	}
	return m
}

// TestJobSpecD6 asserts every field D6 and D3 decide, one by one, so a
// change to any of them is a named failure rather than a golden-file diff.
func TestJobSpecD6(t *testing.T) {
	s, cfg := testSpec(), testConfig()
	job, sec := jobSpec(s, cfg, t0)

	// Job metadata and lifecycle.
	if job.Name != s.Name || job.Namespace != "gonk" {
		t.Errorf("job %s/%s, want gonk/%s", job.Namespace, job.Name, s.Name)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" {
		t.Errorf("job typemeta %s %s", job.APIVersion, job.Kind)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0 (one attempt; a retry would re-run a model session)", job.Spec.BackoffLimit)
	}
	// 20m0.5s rounds UP to 1201: the Job must never die before the deadline.
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1201 {
		t.Errorf("activeDeadlineSeconds = %v, want 1201", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 3600", job.Spec.TTLSecondsAfterFinished)
	}
	for k, v := range map[string]string{ComponentLabel: ComponentValue, SessionLabel: s.Name, "gonk-factory.github.io/project": "42"} {
		if job.Labels[k] != v {
			t.Errorf("job label %s = %q, want %q", k, job.Labels[k], v)
		}
	}

	// Pod template.
	pt := job.Spec.Template
	for k, v := range map[string]string{
		"app":          "gc-agent", // NetworkPolicy + netpol probe (ADR-008 s9)
		ComponentLabel: ComponentValue,
		SessionLabel:   s.Name,
	} {
		if pt.Labels[k] != v {
			t.Errorf("pod label %s = %q, want %q", k, pt.Labels[k], v)
		}
	}
	ps := pt.Spec
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s, want Never", ps.RestartPolicy)
	}
	if ps.ServiceAccountName != "gc-agent" {
		t.Errorf("serviceAccountName = %q", ps.ServiceAccountName)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false", ps.AutomountServiceAccountToken)
	}
	if len(ps.ImagePullSecrets) != 1 || ps.ImagePullSecrets[0].Name != "gitlab-registry-pull-creds" {
		t.Errorf("imagePullSecrets = %v", ps.ImagePullSecrets)
	}
	psc := ps.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
		psc.RunAsUser == nil || *psc.RunAsUser != 65532 ||
		psc.RunAsGroup == nil || *psc.RunAsGroup != 65532 ||
		psc.FSGroup == nil || *psc.FSGroup != 65532 {
		t.Errorf("pod securityContext = %+v, want runAsNonRoot, uid/gid/fsGroup 65532", psc)
	}
	if len(ps.Containers) != 1 || len(ps.InitContainers) != 0 {
		t.Fatalf("containers = %d, init = %d; want exactly one agent container", len(ps.Containers), len(ps.InitContainers))
	}
	c := ps.Containers[0]
	if c.Name != "agent" || c.Image != cfg.Image {
		t.Errorf("container %s image %s", c.Name, c.Image)
	}
	if len(c.Command) != 0 || len(c.Args) != 0 {
		t.Errorf("command %v args %v; want the image's ENTRYPOINT (no tmux wrapper)", c.Command, c.Args)
	}
	csc := c.SecurityContext
	if csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation ||
		csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" || len(csc.Capabilities.Add) != 0 {
		t.Errorf("container securityContext = %+v, want no privilege escalation, drop ALL", csc)
	}
	for name, pair := range map[string][2]string{
		"cpu request":    {c.Resources.Requests.Cpu().String(), "500m"},
		"memory request": {c.Resources.Requests.Memory().String(), "1Gi"},
		"cpu limit":      {c.Resources.Limits.Cpu().String(), "2"},
		"memory limit":   {c.Resources.Limits.Memory().String(), "4Gi"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %s, want %s", name, pair[0], pair[1])
		}
	}

	// Delivery: the Secret, mounted read-only and non-optional.
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/run/gonk/session" || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("volumeMounts = %+v, want one read-only mount at /run/gonk/session", c.VolumeMounts)
	}
	if len(ps.Volumes) != 1 || ps.Volumes[0].Secret == nil || ps.Volumes[0].Name != c.VolumeMounts[0].Name {
		t.Fatalf("volumes = %+v, want one Secret volume backing the mount", ps.Volumes)
	}
	sv := ps.Volumes[0].Secret
	if sv.SecretName != s.Name {
		t.Errorf("volume secretName = %q, want %q", sv.SecretName, s.Name)
	}
	if sv.Optional == nil || *sv.Optional {
		t.Errorf("secret volume optional = %v, want false (kubelet must wait for the Secret)", sv.Optional)
	}
	if sv.DefaultMode == nil || *sv.DefaultMode != 0o440 {
		t.Errorf("secret volume defaultMode = %v, want 0440", sv.DefaultMode)
	}
	if sec.Name != s.Name || sec.Namespace != "gonk" || sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret %s/%s type %s", sec.Namespace, sec.Name, sec.Type)
	}
	if sec.Labels[ComponentLabel] != ComponentValue || sec.Labels[SessionLabel] != s.Name {
		t.Errorf("secret labels = %v", sec.Labels)
	}
	if len(sec.Data) != 3 || string(sec.Data["prompt"]) != testPrompt ||
		string(sec.Data["litellm-key"]) != testKey || string(sec.Data["metadata.json"]) != testMeta {
		t.Errorf("secret data keys/values wrong: %v", keys(sec.Data))
	}
	if len(sec.StringData) != 0 {
		t.Errorf("secret stringData set: %v", sec.StringData)
	}

	// Env: the fixed runtime names, the Spec's env, the D6 HOME.
	env := envMap(t, c)
	for k, v := range map[string]string{
		"GONK_SESSION_RUNTIME": "jobs",
		"GONK_SESSION_DIR":     "/run/gonk/session",
		"GONK_SESSION_NAME":    s.Name,
		"GONK_AGENT":           "triage",
		"HOME":                 "/tmp/gonk-home",
		"XDG_CONFIG_HOME":      "/tmp/gonk-home/.config",
		"XDG_DATA_HOME":        "/tmp/gonk-home/.local/share",
		"XDG_CACHE_HOME":       "/tmp/gonk-home/.cache",
		"XDG_STATE_HOME":       "/tmp/gonk-home/.local/state",
		"GONK_LITELLM_URL":     "http://litellm:4000",
		"GONK_ATTEMPT":         "1",
	} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// forbiddenEnvName is the plan's D5 pattern, spelled out again here rather
// than reusing forbiddenEnv, so that loosening the implementation's regexp
// cannot also loosen the test.
var forbiddenEnvName = regexp.MustCompile(`^(GC_DOLT|BEADS_|GC_INSTANCE|GC_CITY_WRITE)`)

// TestJobSpecEnvCarriesNoSecretsAndNoGasCityNames: the pod spec -- which
// `kubectl describe` prints and every reader of the Job can see -- holds
// neither the key nor the prompt, and none of the Gas City names a live
// agent pod carries (gonk-426v).
func TestJobSpecEnvCarriesNoSecretsAndNoGasCityNames(t *testing.T) {
	job, _ := jobSpec(testSpec(), testConfig(), t0)
	pt := job.Spec.Template
	for _, c := range append(append([]corev1.Container{}, pt.Spec.InitContainers...), pt.Spec.Containers...) {
		for _, e := range c.Env {
			if forbiddenEnvName.MatchString(e.Name) {
				t.Errorf("container %s env %s: forbidden in a Jobs pod (D5, gonk-426v)", c.Name, e.Name)
			}
			if strings.Contains(e.Value, testKey) {
				t.Errorf("container %s env %s carries the LiteLLM key", c.Name, e.Name)
			}
			if strings.Contains(e.Value, "THE-PROMPT-TEXT") {
				t.Errorf("container %s env %s carries the prompt", c.Name, e.Name)
			}
		}
		if len(c.EnvFrom) != 0 {
			t.Errorf("container %s uses envFrom %v; the Secret is mounted, never env", c.Name, c.EnvFrom)
		}
		for _, a := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(a, testKey) || strings.Contains(a, "THE-PROMPT-TEXT") {
				t.Errorf("container %s argv carries a secret", c.Name)
			}
		}
	}
	for k, v := range pt.Annotations {
		if strings.Contains(v, testKey) || strings.Contains(v, "THE-PROMPT-TEXT") {
			t.Errorf("pod annotation %s carries a secret", k)
		}
	}
}

// TestValidateSpecRefuses: every Spec Start must not act on.
func TestValidateSpecRefuses(t *testing.T) {
	cases := map[string]func(*runtime.Spec){
		"forbidden GC_DOLT env":       func(s *runtime.Spec) { s.Env["GC_DOLT_HOST"] = "dolt" },
		"forbidden BEADS_ env":        func(s *runtime.Spec) { s.Env["BEADS_DIR"] = "/x" },
		"forbidden GC_INSTANCE env":   func(s *runtime.Spec) { s.Env["GC_INSTANCE_TOKEN"] = "t" },
		"forbidden GC_CITY_WRITE env": func(s *runtime.Spec) { s.Env["GC_CITY_WRITE_PUBKEY"] = "k" },
		"runtime turned off":          func(s *runtime.Spec) { s.Env["GONK_SESSION_RUNTIME"] = "gascity" },
		"session dir moved":           func(s *runtime.Spec) { s.Env["GONK_SESSION_DIR"] = "/tmp" },
		"key in env":                  func(s *runtime.Spec) { s.Env["LITELLM_API_KEY"] = testKey },
		"invalid name":                func(s *runtime.Spec) { s.Name = "Not_A_Label" },
		"empty agent":                 func(s *runtime.Spec) { s.Agent = "" },
		"empty prompt":                func(s *runtime.Spec) { s.Prompt = nil },
		"empty key":                   func(s *runtime.Spec) { s.LiteLLMKey = nil },
		"empty metadata":              func(s *runtime.Spec) { s.Metadata = nil },
		"zero deadline":               func(s *runtime.Spec) { s.Deadline = time.Time{} },
		"past deadline":               func(s *runtime.Spec) { s.Deadline = t0.Add(-time.Second) },
		"sub-second deadline":         func(s *runtime.Spec) { s.Deadline = t0.Add(999 * time.Millisecond) },
		"reserved label app":          func(s *runtime.Spec) { s.Labels["app"] = "other" },
		"reserved label session":      func(s *runtime.Spec) { s.Labels[SessionLabel] = "x" },
	}
	if err := validateSpec(testSpec(), testConfig(), t0); err != nil {
		t.Fatalf("the base spec must be valid: %v", err)
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := testSpec()
			mut(&s)
			if err := validateSpec(s, testConfig(), t0); err == nil {
				t.Fatal("validateSpec accepted it")
			}
		})
	}
	t.Run("no image anywhere", func(t *testing.T) {
		cfg := testConfig()
		cfg.Image = ""
		if err := validateSpec(testSpec(), cfg, t0); err == nil {
			t.Fatal("validateSpec accepted a spec with no image")
		}
	})
}

// TestJobSpecImagePrecedence: a Spec's image wins over the default.
func TestJobSpecImagePrecedence(t *testing.T) {
	s := testSpec()
	s.Image = "registry.example/other:1"
	job, _ := jobSpec(s, testConfig(), t0)
	if got := job.Spec.Template.Spec.Containers[0].Image; got != s.Image {
		t.Errorf("image = %s, want the Spec's %s", got, s.Image)
	}
}
