package jobs

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/gonk-factory/gonk/internal/envname"
)

// Defaults for the optional settings. Each is the value D6 read off the live
// Gas City agent pod (s-go-do58, 2026-09-25) or decided in the plan, and each
// is the chart's default too (chart/gonk/values.yaml sessionRuntime), so an
// unset variable and a default chart agree.
const (
	// DefaultServiceAccount is the agent ServiceAccount. The pod does not
	// mount its token (D6): an agent needs no API access.
	DefaultServiceAccount = "gc-agent"
	// DefaultPullSecret is the registry pull secret the live agent pod names.
	// There is no env for it: a name carrying SECRET would be stripped from
	// the exec orders' environment (see envname), and the chart already
	// attaches it to ServiceAccount gc-agent, so this is belt-and-braces.
	DefaultPullSecret = "gitlab-registry-pull-creds"
	// DefaultTTLSecondsAfterFinished keeps a finished Job (and so its pod
	// log) long enough for the ~60s sweep to read it (D6).
	DefaultTTLSecondsAfterFinished = 3600
	// DefaultCPURequest .. DefaultMemoryLimit are the live pod's resources.
	DefaultCPURequest    = "500m"
	DefaultMemoryRequest = "1Gi"
	DefaultCPULimit      = "2"
	DefaultMemoryLimit   = "4Gi"
)

// Config is what the Jobs runtime needs that a Spec does not carry: where the
// sessions run and what shape their pods have.
type Config struct {
	// Namespace the Jobs and Secrets are created in, and listed from.
	// REQUIRED: client-go reads an empty namespace as cluster scope, which is
	// the gonk-8gb shape (a namespaced call silently made cluster-wide).
	Namespace string
	// Image is the agent image used when a Spec does not name one. REQUIRED:
	// there is no sensible default image, and a wrong one is the xsk3
	// outage class.
	Image string
	// ServiceAccount the session pod runs as.
	ServiceAccount string
	// PullSecret is the imagePullSecrets entry on the pod; empty for none.
	PullSecret string
	// TTLSecondsAfterFinished is the Job's ttlSecondsAfterFinished.
	TTLSecondsAfterFinished int32
	// Resources for the one agent container.
	CPURequest, MemoryRequest, CPULimit, MemoryLimit resource.Quantity
}

// ConfigFromEnv reads the Config from the controller's environment, through
// the names in internal/envname (the chart renders them from the same
// constants, so neither side can be renamed alone):
//
//	GONK_SESSION_NAMESPACE        required
//	GONK_AGENT_IMAGE              required
//	GONK_SESSION_SERVICE_ACCOUNT  default gc-agent
//	GONK_SESSION_TTL_SECONDS      default 3600, must be > 0
//	GONK_SESSION_CPU_REQUEST      default 500m
//	GONK_SESSION_MEMORY_REQUEST   default 1Gi
//	GONK_SESSION_CPU_LIMIT        default 2
//	GONK_SESSION_MEMORY_LIMIT     default 4Gi
//
// A variable that is SET but empty or malformed is an error, not a default:
// a chart that renders a blank value meant something, and guessing is how a
// misconfiguration ships quietly.
func ConfigFromEnv() (Config, error) {
	return configFrom(os.LookupEnv)
}

// configFrom is ConfigFromEnv over an injectable lookup, for tests.
func configFrom(lookup func(string) (string, bool)) (Config, error) {
	var errs []error
	required := func(name string) string {
		v, ok := lookup(name)
		v = strings.TrimSpace(v)
		if !ok || v == "" {
			errs = append(errs, fmt.Errorf("%s is not set; the Jobs session runtime cannot run without it", name))
		}
		return v
	}
	optional := func(name, def string) string {
		v, ok := lookup(name)
		if !ok {
			return def
		}
		v = strings.TrimSpace(v)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is set but empty; unset it to take the default %q", name, def))
		}
		return v
	}
	quantity := func(name, def string) resource.Quantity {
		v := optional(name, def)
		if v == "" {
			return resource.Quantity{}
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a Kubernetes quantity: %w", name, v, err))
		}
		return q
	}

	cfg := Config{
		Namespace:      required(envname.SessionNamespace),
		Image:          required(envname.AgentImage),
		ServiceAccount: optional(envname.SessionServiceAccount, DefaultServiceAccount),
		PullSecret:     DefaultPullSecret,
		CPURequest:     quantity(envname.SessionCPURequest, DefaultCPURequest),
		MemoryRequest:  quantity(envname.SessionMemoryRequest, DefaultMemoryRequest),
		CPULimit:       quantity(envname.SessionCPULimit, DefaultCPULimit),
		MemoryLimit:    quantity(envname.SessionMemoryLimit, DefaultMemoryLimit),
	}
	if ttl := optional(envname.SessionTTLSeconds, strconv.Itoa(DefaultTTLSecondsAfterFinished)); ttl != "" {
		n, err := strconv.ParseInt(ttl, 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not an integer: %w", envname.SessionTTLSeconds, ttl, err))
		}
		cfg.TTLSecondsAfterFinished = int32(n)
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate refuses a Config the runtime must not run with.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Namespace) == "" {
		errs = append(errs, errors.New("jobs: Config.Namespace is empty (an empty namespace is cluster scope to client-go)"))
	}
	if strings.TrimSpace(c.Image) == "" {
		errs = append(errs, errors.New("jobs: Config.Image is empty"))
	}
	if strings.TrimSpace(c.ServiceAccount) == "" {
		errs = append(errs, errors.New("jobs: Config.ServiceAccount is empty"))
	}
	if c.TTLSecondsAfterFinished <= 0 {
		errs = append(errs, fmt.Errorf("jobs: Config.TTLSecondsAfterFinished is %d; it must be > 0 or a finished Job is reaped before the sweep can read its log", c.TTLSecondsAfterFinished))
	}
	for _, r := range []struct {
		name       string
		req, limit resource.Quantity
	}{
		{"cpu", c.CPURequest, c.CPULimit},
		{"memory", c.MemoryRequest, c.MemoryLimit},
	} {
		if r.req.Sign() <= 0 || r.limit.Sign() <= 0 {
			errs = append(errs, fmt.Errorf("jobs: %s request %s / limit %s must both be > 0", r.name, r.req.String(), r.limit.String()))
			continue
		}
		if r.req.Cmp(r.limit) > 0 {
			errs = append(errs, fmt.Errorf("jobs: %s request %s exceeds its limit %s (the API server would refuse every Job)", r.name, r.req.String(), r.limit.String()))
		}
	}
	return errors.Join(errs...)
}
