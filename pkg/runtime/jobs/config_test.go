package jobs

import (
	"strconv"
	"testing"

	"github.com/gonk-factory/gonk/internal/envname"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := configFrom(lookupFrom(map[string]string{
		envname.SessionNamespace: "gonk",
		envname.AgentImage:       "registry.example/agent:v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "gonk" || cfg.Image != "registry.example/agent:v1" ||
		cfg.ServiceAccount != "gc-agent" || cfg.PullSecret != "gitlab-registry-pull-creds" ||
		cfg.TTLSecondsAfterFinished != 3600 ||
		cfg.CPURequest.String() != "500m" || cfg.MemoryRequest.String() != "1Gi" ||
		cfg.CPULimit.String() != "2" || cfg.MemoryLimit.String() != "4Gi" {
		t.Errorf("defaults = %+v", cfg)
	}
}

// TestConfigFromEnvReadsEveryChartName: every name the chart renders
// (envname.SessionRuntime) changes the Config. A name the runtime stopped
// reading would leave the chart setting a variable nothing consumes.
func TestConfigFromEnvReadsEveryChartName(t *testing.T) {
	env := map[string]string{
		envname.SessionNamespace:      "ns-x",
		envname.AgentImage:            "img-x",
		envname.SessionServiceAccount: "sa-x",
		envname.SessionTTLSeconds:     "777",
		envname.SessionCPURequest:     "100m",
		envname.SessionMemoryRequest:  "100Mi",
		envname.SessionCPULimit:       "300m",
		envname.SessionMemoryLimit:    "300Mi",
	}
	for _, n := range envname.SessionRuntime {
		if _, ok := env[n]; !ok {
			t.Fatalf("envname.SessionRuntime has %s, which this test does not set -- extend it and ConfigFromEnv", n)
		}
	}
	cfg, err := configFrom(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{
		envname.SessionNamespace:      cfg.Namespace,
		envname.AgentImage:            cfg.Image,
		envname.SessionServiceAccount: cfg.ServiceAccount,
		envname.SessionTTLSeconds:     strconv.Itoa(int(cfg.TTLSecondsAfterFinished)),
		envname.SessionCPURequest:     cfg.CPURequest.String(),
		envname.SessionMemoryRequest:  cfg.MemoryRequest.String(),
		envname.SessionCPULimit:       cfg.CPULimit.String(),
		envname.SessionMemoryLimit:    cfg.MemoryLimit.String(),
	}
	for n, v := range env {
		if got[n] != v {
			t.Errorf("%s=%q reached the Config as %q", n, v, got[n])
		}
	}
}

func TestConfigFromEnvRefuses(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{envname.SessionNamespace: "gonk", envname.AgentImage: "img"}
	}
	cases := map[string]func(map[string]string){
		"namespace unset":       func(m map[string]string) { delete(m, envname.SessionNamespace) },
		"namespace blank":       func(m map[string]string) { m[envname.SessionNamespace] = "  " },
		"image unset":           func(m map[string]string) { delete(m, envname.AgentImage) },
		"service account blank": func(m map[string]string) { m[envname.SessionServiceAccount] = "" },
		"ttl not a number":      func(m map[string]string) { m[envname.SessionTTLSeconds] = "1h" },
		"ttl zero":              func(m map[string]string) { m[envname.SessionTTLSeconds] = "0" },
		"ttl negative":          func(m map[string]string) { m[envname.SessionTTLSeconds] = "-5" },
		"cpu request malformed": func(m map[string]string) { m[envname.SessionCPURequest] = "lots" },
		"memory limit blank":    func(m map[string]string) { m[envname.SessionMemoryLimit] = "" },
		"request above limit":   func(m map[string]string) { m[envname.SessionCPURequest] = "3" },
		"memory limit zero":     func(m map[string]string) { m[envname.SessionMemoryLimit] = "0" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := base()
			mut(m)
			if cfg, err := configFrom(lookupFrom(m)); err == nil {
				t.Fatalf("accepted: %+v", cfg)
			}
		})
	}
}

// TestConfigFromEnvUsesTheProcessEnv: the exported entry point reads the
// real environment.
func TestConfigFromEnvUsesTheProcessEnv(t *testing.T) {
	t.Setenv(envname.SessionNamespace, "from-env")
	t.Setenv(envname.AgentImage, "img-from-env")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "from-env" || cfg.Image != "img-from-env" {
		t.Errorf("cfg = %+v", cfg)
	}
}
