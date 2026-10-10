package jobs

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/gonk-factory/gonk/internal/keydomain"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// Labels (D6, and ADR-008 section 9).
const (
	// appLabel/appValue: KEPT DELIBERATELY from Gas City's agent pods. The
	// agent NetworkPolicy's podSelector and the helm-test netpol probe both
	// select app=gc-agent; a Job pod without it renders fine and is enforced
	// by nothing (ADR-008 sections 9 and 12).
	appLabel = "app"
	appValue = "gc-agent"
	// ComponentLabel=ComponentValue marks every object this runtime owns;
	// List selects by it.
	ComponentLabel = keydomain.Prefix + "component"
	ComponentValue = "agent-session"
	// SessionLabel carries the session name, so a session's pods can be found
	// without trusting the Job controller's own label names.
	SessionLabel = keydomain.Prefix + "session"
)

// The session's mounted delivery (D3). The entrypoint (images/agent/
// entrypoint.sh, jobs mode) reads these files from SessionDir: the first three
// always (a missing one is its exit 6), and KeyRigURL when present -- it is
// written only when Spec.RigURL is set, and its absence means "no checkout".
const (
	SessionDir     = "/run/gonk/session"
	KeyPrompt      = "prompt"
	KeyLiteLLMKey  = "litellm-key"
	KeyMetadata    = "metadata.json"
	KeyRigURL      = "rig-url"
	containerName  = "agent"
	sessionVolume  = "session"
	agentUID       = int64(65532)
	secretFileMode = int32(0o440) // root:65532 via fsGroup; the agent reads by group
	homeDir        = "/tmp/gonk-home"
	runtimeJobsVal = "jobs"
)

// Env names the runtime itself sets on the agent container. These are AGENT
// env (read by images/agent/entrypoint.sh), not controller env, so they do
// not belong in internal/envname.
const (
	envSessionRuntime = "GONK_SESSION_RUNTIME"
	envSessionDir     = "GONK_SESSION_DIR"
	envSessionName    = "GONK_SESSION_NAME"
	envAgent          = "GONK_AGENT"
)

// forbiddenEnv matches env names that must never reach a Jobs pod (D5,
// gonk-426v): a live Gas City agent pod carries Dolt credentials, the beads
// store, the instance token and the city write key. A session under Jobs has
// no use for any of them, and carrying none is an exit criterion.
var forbiddenEnv = regexp.MustCompile(`^(GC_DOLT|BEADS_|GC_INSTANCE|GC_CITY_WRITE)`)

// fixedEnv is the env the runtime always sets, from the Spec -- including the
// session contract version (runtime.SessionContract), which the entrypoint
// checks before anything else (plan 3.3, handshake). A Spec.Env
// entry may repeat one of these with the SAME value (dispatch builds D5's env
// and may include them) but never contradict it: a caller that could turn
// GONK_SESSION_RUNTIME off would get the Gas City fetch loop in a Job.
func fixedEnv(s runtime.Spec) map[string]string {
	return map[string]string{
		envSessionRuntime: runtimeJobsVal,
		envSessionDir:     SessionDir,
		envSessionName:    s.Name,
		envAgent:          s.Agent,
		// The contract version is this BINARY's, never the Spec's: it states
		// what the controller that built this Job speaks.
		runtime.EnvSessionContract: strconv.Itoa(runtime.SessionContract),
	}
}

// defaultEnv is set only when Spec.Env does not set the name. The pack's
// [env] block (D5) is the source of truth for these and does set them; the
// defaults exist so a Spec built without the pack still gets a writable HOME
// (the image's own HOME=/workspace makes opencode scan $HOME/.opencode).
var defaultEnv = map[string]string{
	"HOME":            homeDir,
	"XDG_CONFIG_HOME": homeDir + "/.config",
	"XDG_DATA_HOME":   homeDir + "/.local/share",
	"XDG_CACHE_HOME":  homeDir + "/.cache",
	"XDG_STATE_HOME":  homeDir + "/.local/state",
}

// reservedLabels may not be set through Spec.Labels.
var reservedLabels = []string{appLabel, ComponentLabel, SessionLabel}

// validateSpec refuses a Spec the runtime must not start. now is the time
// Start runs at; the deadline must be at least a second after it.
func validateSpec(s runtime.Spec, cfg Config, now time.Time) error {
	var errs []error
	if err := runtime.ValidName(s.Name); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(s.Agent) == "" {
		errs = append(errs, errors.New("spec.Agent is empty"))
	}
	if s.Image == "" && cfg.Image == "" {
		errs = append(errs, errors.New("spec.Image is empty and the runtime has no default agent image"))
	}
	// Empty mounted files are refused here rather than by the entrypoint:
	// the entrypoint would exit non-zero, but only after scheduling a pod
	// and burning a session attempt on a failure dispatch could see now.
	if len(s.Prompt) == 0 {
		errs = append(errs, errors.New("spec.Prompt is empty"))
	}
	if len(s.LiteLLMKey) == 0 {
		errs = append(errs, errors.New("spec.LiteLLMKey is empty (the key is resolved fail-closed; never start a session without one)"))
	}
	if len(s.Metadata) == 0 {
		errs = append(errs, errors.New("spec.Metadata is empty"))
	}
	if s.Deadline.IsZero() {
		errs = append(errs, errors.New("spec.Deadline is zero; a session must have a deadline (activeDeadlineSeconds)"))
	} else if s.Deadline.Sub(now) < time.Second {
		errs = append(errs, fmt.Errorf("spec.Deadline %s is not at least 1s after now %s", s.Deadline.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)))
	}
	fixed := fixedEnv(s)
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		v := s.Env[k]
		if forbiddenEnv.MatchString(k) {
			errs = append(errs, fmt.Errorf("spec.Env carries %s, which must never reach a session pod (D5, gonk-426v)", k))
		}
		if want, ok := fixed[k]; ok && v != want {
			errs = append(errs, fmt.Errorf("spec.Env sets %s=%q, but the runtime sets it to %q", k, v, want))
		}
		if len(s.LiteLLMKey) > 0 && strings.Contains(v, string(s.LiteLLMKey)) {
			errs = append(errs, fmt.Errorf("spec.Env %s carries the LiteLLM key; the key is mounted, never env", k))
		}
		// The rig URL embeds the checkout grant's key (pkg/rig serves the
		// tree to anyone presenting it until the grant's TTL): mounted, never
		// env, exactly like the LiteLLM key.
		if s.RigURL != "" && strings.Contains(v, s.RigURL) {
			errs = append(errs, fmt.Errorf("spec.Env %s carries the rig checkout URL; it is mounted as %s, never env", k, KeyRigURL))
		}
	}
	if s.RigURL != "" && strings.ContainsAny(s.RigURL, "\n\r") {
		errs = append(errs, errors.New("spec.RigURL contains a line break"))
	}
	for _, k := range reservedLabels {
		if _, ok := s.Labels[k]; ok {
			errs = append(errs, fmt.Errorf("spec.Labels sets %s, which the runtime owns", k))
		}
	}
	return errors.Join(errs...)
}

// jobSpec builds the session's Job and its Secret (D3, D6). It is pure: the
// same Spec, Config and now give the same objects. It does not validate --
// Start runs validateSpec first -- and it leaves the Secret's ownerReference
// to Start, because the Job's UID exists only once the API server has
// created it.
//
// now is the time activeDeadlineSeconds is counted from: the seconds from
// now until Spec.Deadline, rounded UP, so the Job is never killed before the
// deadline the meter reservation promised.
func jobSpec(s runtime.Spec, cfg Config, now time.Time) (*batchv1.Job, *corev1.Secret) {
	image := s.Image
	if image == "" {
		image = cfg.Image
	}

	objLabels := map[string]string{}
	maps.Copy(objLabels, s.Labels)
	objLabels[ComponentLabel] = ComponentValue
	objLabels[SessionLabel] = s.Name

	podLabels := maps.Clone(objLabels)
	podLabels[appLabel] = appValue

	deadline := int64(s.Deadline.Sub(now) / time.Second)
	if s.Deadline.Sub(now)%time.Second != 0 {
		deadline++
	}

	var pullSecrets []corev1.LocalObjectReference
	if cfg.PullSecret != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: cfg.PullSecret}}
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: cfg.Namespace,
			Labels:    objLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(deadline),
			TTLSecondsAfterFinished: ptr.To(cfg.TTLSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           cfg.ServiceAccount,
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             pullSecrets,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(agentUID),
						RunAsGroup:   ptr.To(agentUID),
						FSGroup:      ptr.To(agentUID),
					},
					Containers: []corev1.Container{{
						Name:  containerName,
						Image: image,
						// No Command/Args: the image's ENTRYPOINT is the
						// session (D6: no tmux; Gas City overrode it).
						Env: podEnv(s),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    cfg.CPURequest,
								corev1.ResourceMemory: cfg.MemoryRequest,
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    cfg.CPULimit,
								corev1.ResourceMemory: cfg.MemoryLimit,
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      sessionVolume,
							MountPath: SessionDir,
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: sessionVolume,
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName:  s.Name,
							DefaultMode: ptr.To(secretFileMode),
							// NOT optional: kubelet holds the pod until the
							// Secret exists, which is what lets Start create
							// it after the Job (D3).
							Optional: ptr.To(false),
						}},
					}},
				},
			},
		},
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name,
			Namespace: cfg.Namespace,
			Labels:    maps.Clone(objLabels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			KeyPrompt:     slices.Clone(s.Prompt),
			KeyLiteLLMKey: slices.Clone(s.LiteLLMKey),
			KeyMetadata:   slices.Clone(s.Metadata),
		},
	}
	if s.RigURL != "" {
		secret.Data[KeyRigURL] = []byte(s.RigURL)
	}
	return job, secret
}

// podEnv is the agent container's env, sorted by name: defaults, then
// Spec.Env over them, then the fixed runtime env over that.
func podEnv(s runtime.Spec) []corev1.EnvVar {
	env := maps.Clone(defaultEnv)
	maps.Copy(env, s.Env)
	maps.Copy(env, fixedEnv(s))
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}
