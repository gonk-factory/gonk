// Package jobs is the Kubernetes Jobs implementation of runtime.SessionRuntime
// (gonk-t55, plan docs/plans/2026-09-25-t55-session-runtime.md, part "jobs";
// ADR-008 section 14).
//
// One session is one batch/v1 Job plus one Secret:
//
//   - The Job runs the agent image's own entrypoint once (backoffLimit 0),
//     is killed by activeDeadlineSeconds at the meter reservation's expiry, and
//     is garbage-collected ttlSecondsAfterFinished after it ends -- orphans
//     clean themselves up with no gonk code running.
//   - The Secret holds the prompt, the LiteLLM key and the spend metadata,
//     mounted read-only at /run/gonk/session. It is created AFTER the Job with
//     an ownerReference to it, so it dies with the Job; the pod's volume is not
//     optional, so kubelet waits for it. If the Secret cannot be created, Start
//     deletes the Job: there is never a half-session.
//
// The session name is the Job name, the Secret name and the handle. A second
// Start of the same name gets AlreadyExists from the API server and returns
// the EXISTING session, creating nothing -- in particular no second Secret.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// cleanupTimeout bounds the Job delete Start issues after a failed Secret
// create. That delete runs on a context detached from the caller's, because
// the commonest reason for the Secret create to fail is the caller's context
// ending -- and a cleanup that dies with it leaves exactly the half-session
// the delete exists to prevent.
const cleanupTimeout = 30 * time.Second

// Runtime is the Jobs SessionRuntime. Build it with New.
type Runtime struct {
	client kubernetes.Interface
	cfg    Config
	now    func() time.Time
	// readLog streams one container's log. It is the clientset's GetLogs in
	// production; a test replaces it, because the fake clientset answers
	// every pod's log with the same fixed body and so cannot show WHICH pod
	// was read.
	readLog func(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

var _ runtime.SessionRuntime = (*Runtime)(nil)

// New returns the Jobs runtime over client, refusing a Config that fails
// Validate (an empty namespace above all: client-go would make every call
// cluster-scoped).
func New(client kubernetes.Interface, cfg Config) (runtime.SessionRuntime, error) {
	return newRuntime(client, cfg)
}

func newRuntime(client kubernetes.Interface, cfg Config) (*Runtime, error) {
	if client == nil {
		return nil, errors.New("jobs: nil Kubernetes client")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Runtime{client: client, cfg: cfg, now: time.Now}
	r.readLog = func(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
		return client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).Stream(ctx)
	}
	return r, nil
}

// Start creates the session's Job, then its Secret. See the package doc.
func (r *Runtime) Start(ctx context.Context, spec runtime.Spec) (runtime.Handle, bool, error) {
	now := r.now()
	if err := validateSpec(spec, r.cfg, now); err != nil {
		return "", false, fmt.Errorf("jobs: refusing to start %q: %w", spec.Name, err)
	}
	job, secret := jobSpec(spec, r.cfg, now)
	h := runtime.Handle(spec.Name)

	jobs := r.client.BatchV1().Jobs(r.cfg.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Idempotent Start: the session exists. Return it and touch nothing
		// -- not the Job, and not its Secret, which its own Start created.
		existing, gerr := jobs.Get(ctx, spec.Name, metav1.GetOptions{})
		if gerr != nil {
			return "", false, fmt.Errorf("jobs: %s already exists but cannot be read: %w", spec.Name, gerr)
		}
		if existing.Labels[ComponentLabel] != ComponentValue || existing.Labels[SessionLabel] != spec.Name {
			return "", false, fmt.Errorf("jobs: a Job named %s exists in %s but is not a gonk agent session (labels %v); refusing to adopt it",
				spec.Name, r.cfg.Namespace, existing.Labels)
		}
		// A Job that is BEING DELETED is not a session to hand back: it is
		// the one the sweep just stopped -- a session refused for image skew
		// is stopped and re-dispatched under the SAME name, because the
		// attempt does not advance (plan 3.3, handshake item 3). Adopting it
		// would record a session that is about to vanish, which the sweep
		// then judges as absent and CHARGES as an attempt. Refuse instead:
		// dispatch puts the parked record back and the next unpark retries.
		if existing.DeletionTimestamp != nil {
			return "", false, fmt.Errorf("jobs: %s is still being deleted (since %s); not adopting it -- retry once it is gone",
				spec.Name, existing.DeletionTimestamp.UTC().Format(time.RFC3339))
		}
		return h, false, nil // adopted: this call created nothing
	}
	if err != nil {
		return "", false, fmt.Errorf("jobs: create Job %s: %w", spec.Name, err)
	}

	secret.OwnerReferences = []metav1.OwnerReference{ownerRef(created)}
	if _, err := r.client.CoreV1().Secrets(r.cfg.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		derr := jobs.Delete(cctx, spec.Name, metav1.DeleteOptions{
			PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
			Preconditions:     &metav1.Preconditions{UID: ptr.To(created.UID)},
		})
		if derr != nil && !apierrors.IsNotFound(derr) {
			return "", false, fmt.Errorf("jobs: create Secret %s failed (%w) AND deleting its Job failed: %w -- the Job will wait for its Secret until activeDeadlineSeconds", spec.Name, err, derr)
		}
		return "", false, fmt.Errorf("jobs: create Secret %s (its Job was deleted): %w", spec.Name, err)
	}
	return h, true, nil
}

// ownerRef makes job the Secret's controller owner. blockOwnerDeletion is
// left unset on purpose: setting it needs `update` on jobs/finalizers where
// the OwnerReferencesPermissionEnforcement admission plugin runs, which Role
// gonk-gate-jobs does not grant; garbage collection works without it.
func ownerRef(job *batchv1.Job) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
		Controller: ptr.To(true),
	}
}

// Status maps the Job and its pod onto a runtime.Status.
//
// Terminal phases come from the Job's own conditions and nothing else:
// Complete -> Succeeded; Failed -> Failed with the condition's reason
// (DeadlineExceeded, BackoffLimitExceeded, ...). A FailureTarget or
// SuccessCriteriaMet condition is NOT terminal -- the pod may still be
// terminating and its log incomplete -- so it reads as Running, with a
// FailureTarget's reason in Reason.
//
// Before that, the pod decides: no pod yet or a container Waiting is Pending
// (with the waiting reason -- ImagePullBackOff, CreateContainerConfigError,
// ContainerCreating -- or an unschedulable pod's reason); a container Running
// or already Terminated (the Job controller has not caught up) is Running.
func (r *Runtime) Status(ctx context.Context, h runtime.Handle) (runtime.Status, error) {
	job, err := r.client.BatchV1().Jobs(r.cfg.Namespace).Get(ctx, string(h), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return runtime.Status{Phase: runtime.PhaseNotFound}, nil
	}
	if err != nil {
		return runtime.Status{}, fmt.Errorf("jobs: status %s: %w", h, err)
	}

	var started time.Time
	if job.Status.StartTime != nil {
		started = job.Status.StartTime.Time
	}
	if c := condition(job, batchv1.JobComplete); c != nil {
		fin := c.LastTransitionTime.Time
		if job.Status.CompletionTime != nil {
			fin = job.Status.CompletionTime.Time
		}
		code, err := r.exitCode(ctx, job)
		if err != nil {
			return runtime.Status{}, fmt.Errorf("jobs: status %s: %w", h, err)
		}
		return runtime.Status{Phase: runtime.PhaseSucceeded, Reason: c.Reason, StartedAt: started, FinishedAt: fin, ExitCode: code}, nil
	}
	if c := condition(job, batchv1.JobFailed); c != nil {
		// The exit code is read for a FAILED session because one exit code
		// means something the Job condition cannot say: the entrypoint's
		// runtime.ExitSessionContractMismatch, image skew (plan 3.3). A pod
		// list that fails is an unreadable status, not a status without a
		// code -- the sweep retries rather than charge a skewed session.
		code, err := r.exitCode(ctx, job)
		if err != nil {
			return runtime.Status{}, fmt.Errorf("jobs: status %s: %w", h, err)
		}
		return runtime.Status{Phase: runtime.PhaseFailed, Reason: c.Reason, StartedAt: started, FinishedAt: c.LastTransitionTime.Time, ExitCode: code}, nil
	}
	var target string
	if c := condition(job, batchv1.JobFailureTarget); c != nil {
		target = c.Reason
	}

	pod, err := r.sessionPod(ctx, job)
	if err != nil {
		return runtime.Status{}, fmt.Errorf("jobs: status %s: %w", h, err)
	}
	if pod == nil {
		if target != "" || condition(job, batchv1.JobSuccessCriteriaMet) != nil {
			return runtime.Status{Phase: runtime.PhaseRunning, Reason: target, StartedAt: started}, nil
		}
		return runtime.Status{Phase: runtime.PhasePending, Reason: target}, nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		switch {
		case cs.State.Running != nil:
			return runtime.Status{Phase: runtime.PhaseRunning, Reason: target, StartedAt: cs.State.Running.StartedAt.Time}, nil
		case cs.State.Terminated != nil:
			return runtime.Status{Phase: runtime.PhaseRunning, Reason: target, StartedAt: cs.State.Terminated.StartedAt.Time}, nil
		case cs.State.Waiting != nil:
			return runtime.Status{Phase: runtime.PhasePending, Reason: firstNonEmpty(target, cs.State.Waiting.Reason)}, nil
		}
	}
	// No container status yet: scheduling, or waiting on the volume.
	reason := target
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			reason = firstNonEmpty(reason, c.Reason)
		}
	}
	return runtime.Status{Phase: runtime.PhasePending, Reason: reason}, nil
}

// exitCode is the agent container's terminated exit code in the session's
// newest pod, or nil when there is no pod or its container has not
// terminated (a Job killed at its deadline before scheduling has neither).
func (r *Runtime) exitCode(ctx context.Context, job *batchv1.Job) (*int, error) {
	pod, err := r.sessionPod(ctx, job)
	if err != nil || pod == nil {
		return nil, err
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName && cs.State.Terminated != nil {
			return ptr.To(int(cs.State.Terminated.ExitCode)), nil
		}
	}
	return nil, nil
}

// Output returns the full log of the session's container. A session whose
// Job exists but has no pod (not yet scheduled, or its pod was removed by
// the deadline) has produced no output: that is an empty result and a nil
// error, not a failure to read.
func (r *Runtime) Output(ctx context.Context, h runtime.Handle) ([]byte, error) {
	job, err := r.client.BatchV1().Jobs(r.cfg.Namespace).Get(ctx, string(h), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("jobs: output %s: %w", h, runtime.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: output %s: %w", h, err)
	}
	pod, err := r.sessionPod(ctx, job)
	if err != nil {
		return nil, fmt.Errorf("jobs: output %s: %w", h, err)
	}
	if pod == nil {
		return []byte{}, nil
	}
	rc, err := r.readLog(ctx, r.cfg.Namespace, pod.Name, containerName)
	if err != nil {
		return nil, fmt.Errorf("jobs: output %s: log of pod %s: %w", h, pod.Name, err)
	}
	defer func() { _ = rc.Close() }()
	out, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("jobs: output %s: reading log of pod %s: %w", h, pod.Name, err)
	}
	return out, nil
}

// Stop deletes the session's Job with Foreground propagation (its pod and
// its owned Secret go first), and deletes the Secret directly as well when
// it is owned by that Job, so the prompt and key do not outlive a Stop by
// however long the garbage collector takes. An unknown session is
// runtime.ErrNotFound -- never "destroyed" (ADR-008 section 7, gonk-wmb7).
func (r *Runtime) Stop(ctx context.Context, h runtime.Handle) error {
	jobs := r.client.BatchV1().Jobs(r.cfg.Namespace)
	job, err := jobs.Get(ctx, string(h), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("jobs: stop %s: %w", h, runtime.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("jobs: stop %s: %w", h, err)
	}
	err = jobs.Delete(ctx, string(h), metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
		Preconditions:     &metav1.Preconditions{UID: ptr.To(job.UID)},
	})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("jobs: stop %s: %w", h, runtime.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("jobs: stop %s: delete Job: %w", h, err)
	}

	secrets := r.client.CoreV1().Secrets(r.cfg.Namespace)
	sec, err := secrets.Get(ctx, string(h), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("jobs: stop %s: Job deleted; reading its Secret: %w", h, err)
	case !ownedBy(sec.OwnerReferences, job.UID):
		return nil // not this Job's; the garbage collector owns the question
	}
	err = secrets.Delete(ctx, string(h), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(sec.UID)}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("jobs: stop %s: Job deleted; deleting its Secret: %w", h, err)
	}
	return nil
}

// List returns every session Job in the namespace, by the component label,
// sorted by name.
func (r *Runtime) List(ctx context.Context) ([]runtime.Handle, error) {
	sel := labels.SelectorFromSet(labels.Set{ComponentLabel: ComponentValue}).String()
	list, err := r.client.BatchV1().Jobs(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("jobs: list: %w", err)
	}
	out := make([]runtime.Handle, 0, len(list.Items))
	for _, j := range list.Items {
		out = append(out, runtime.Handle(j.Name))
	}
	sort.Slice(out, func(i, k int) bool { return out[i] < out[k] })
	return out, nil
}

// sessionPod returns the newest pod of job, or nil if it has none. Pods are
// found by the session label the pod template carries and kept only if the
// Job is their controller -- by UID, so a pod left from an earlier Job of the
// same name is never mistaken for this one's.
func (r *Runtime) sessionPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	sel := labels.SelectorFromSet(labels.Set{SessionLabel: job.Name}).String()
	pods, err := r.client.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if !controlledBy(p.OwnerReferences, job.UID) {
			continue
		}
		if best == nil || best.CreationTimestamp.Before(&p.CreationTimestamp) ||
			(best.CreationTimestamp.Equal(&p.CreationTimestamp) && best.Name < p.Name) {
			best = p
		}
	}
	return best, nil
}

func condition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func controlledBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, o := range refs {
		if o.UID == uid && o.Kind == "Job" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, o := range refs {
		if o.UID == uid && o.Kind == "Job" {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
