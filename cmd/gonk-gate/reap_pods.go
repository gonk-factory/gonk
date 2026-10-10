package main

// runPodReap IS UNREFERENCED FROM DISPATCH AND SWEEP since gonk-t55 (plan D7):
// it is the Gas City reaper's pod half, and it asks Gas City (CloseSession)
// before every delete, and the sweep holds no Gas City client (only the two
// one-method interfaces main.go wires: orderRunner and legacySessionCloser),
// and gonk no longer creates Gas City sessions for it to find. It would NOT have touched Job pods: it acts only on pods
// whose gc-agent label is a gonk alias (gonkSessionAlias), and Job pods carry
// no gc-agent label. See reap.go. T-56 deletes it.
//
// k8sPodReaper -- the pod list and delete, and the namespace switch -- IS
// still used: the sweep's legacy Gas City pod pass (sweep_legacy.go, plan 3.4
// C1) deletes the pods of sessions in flight at the cutover with it.

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/gonk-factory/gonk/internal/envname"
	"github.com/gonk-factory/gonk/pkg/gcapi"
)

// POD-LEVEL RECOVERY FOR A LEAKED AGENT POD (gonk-mfsr).
//
// THE CLASS. gascity can roll a pending session create back -- closing the
// SESSION BEAD -- and then start the runtime anyway one second later. Observed
// 2026-09-21: pod s-go-whg1, bead closed at 06:32:34 with close_reason
// "session create failed: aborted before creation_complete", creation_complete_at
// 06:32:35, pod Running and holding 500m/1Gi half an hour later.
//
// Once that bead is closed, NO CALL GONK CAN MAKE REACHES THAT POD.
// ListSessions is the OPEN-bead list (humaHandleSessionList reads the session
// read model with IncludeClosed unset), so the pod is not a row; close, stop and
// kill all resolve with allowClosed=false, so they 404 forever. gonk-wmb7 made
// the leak audible -- a 404 from close is now an ERROR naming the alias -- but
// could not recover it. THE POD LIST IS THE ONLY SOURCE OF TRUTH FOR THIS CLASS,
// which is what this file adds.
//
// IT IS THE LAST RESORT, NOT THE FIRST. Every pod this deletes is one gascity
// has been asked to close first and has answered 404 to. A close that SUCCEEDS
// means gascity owns the session after all and will tear the pod down through
// its own provider, which reclaims the ConfigMaps and the session record as well
// as the pod; deleting the pod under it would leave those behind. So a
// successful close ENDS the handling of that pod here.
//
// THE BIAS IS THE SAME AS runReap'S, AND IT MATTERS MORE HERE. runReap closes a
// session, which gascity then tears down in order. This DELETES A POD. A leaked
// pod costs money; a wrongly deleted pod kills a live agent mid-turn, destroys
// work it has not written, and burns a ladder attempt. Every ambiguity below --
// an unreadable pod list, an unparseable creation timestamp, a close that failed
// for a reason that is not 404 -- resolves toward deleting NOTHING.

// podReapNamespaceEnv names the namespace whose agent pods gonk-gate may list
// and delete. IT IS ALSO THE ON SWITCH: empty means no pod reaper at all.
//
// The two are one variable on purpose. An empty namespace does not mean "the
// current one" to client-go -- it makes the request CLUSTER-SCOPED, which is
// the exact blast radius this feature must never have, and the exact shape of
// the gonk-8gb bug (envname.KeysinkNamespace unset turned a namespaced Secret
// read into a cluster-scoped one and every dispatch failed closed with what
// looked like an RBAC problem). Tying the switch to the namespace makes the
// dangerous value unreachable: there is no configuration in which this code
// runs without a namespace to run in.
//
// THE STRING ITSELF LIVES IN internal/envname, NOT HERE, and that is the whole
// point: nothing in Go joins this name to the chart template that sets it, so a
// one-sided rename disarms the reaper silently -- the binary reads a variable
// nobody sets, gets "", and builds no reaper, which is indistinguishable from a
// cluster with no leaks. Review proved that reachable: renaming only this
// constant left cmd/gonk-gate, internal/charttest and internal/buildgate all
// green (gonk-b6pb). internal/charttest now asserts the RENDERED manifest
// against envname.PodReapNamespace, so the two sides cannot move apart.
//
// The name carries no Gas City sensitivity marker (TOKEN/SECRET/KEY/PASSWORD),
// or internal/execenv.FilterInherited would strip it from the environment of
// the `gonk-sweep` EXEC ORDER and the reaper would silently never arm. See the
// GONK_*_FILE note in chart/gonk/templates/workload-gonk-controller.yaml.
const podReapNamespaceEnv = envname.PodReapNamespace

// agentPodSelector is the label every Gas City agent pod carries, and the only
// pods this code will even look at.
//
// Read off GASCITY_REF internal/runtime/k8s/pod.go buildPod, which is the one
// place agent pods are built:
//
//	Labels: {"app": "gc-agent", "gc-session": <label>, "gc-agent": <agentLabel>}
//
// It is also what the provider itself lists by, so a pod without it is not a
// pod gascity considers an agent session either.
const agentPodSelector = "app=gc-agent"

// agentAliasLabel holds the session's PUBLIC ALIAS -- the same string gonk sent
// as `alias` on POST /v0/city/{city}/sessions, and therefore the same string
// rec.SessionID holds, so claimedSessionIDs joins against it unchanged.
//
// VERIFIED, NOT ASSUMED, because the whole join rests on it. buildPod sets
// `gc-agent: SanitizeLabel(cfg.Env["GC_ALIAS"])`, and GC_ALIAS is the session
// alias (internal/session/lifecycle.go RuntimeEnvWithSessionContext, called from
// internal/session/manager.go's start path). Both of the ways that value can
// stop being our alias fail SAFE:
//
//   - SanitizeLabel truncates at 63 characters, which would cut the alias's
//     26-character nonce and make it fail gonkSessionAlias -- skipped. THE
//     BUDGET IS TIGHT: a minimal alias (`gonk.x.p1.i1.a1.` + a 26-character
//     nonce) is already 42 characters, leaving 22 to share between the agent
//     name and the extra digits of the three numbers. Today's agents -- triage
//     and scaffold -- fit with room; a 27-character agent name does not (79
//     characters, measured). TestBrokerAliasesSurviveKubernetesLabelSanitisationUnchanged
//     walks agentForTrigger and goes red when a new name does not fit, because
//     the failure mode otherwise is silent: the reaper simply never recovers a
//     leak for that agent, which is indistinguishable from there being none;
//   - a session created without an alias falls back to the agent NAME
//     ("triage"), which also fails gonkSessionAlias -- skipped.
//
// The `gc-session` label is NOT usable for this: it is SanitizeLabel of the
// RUNTIME session name (s-<beadID>, e.g. s-go-whg1), which gonk never sees.
const agentAliasLabel = "gc-agent"

// agentPod is the subset of a pod the deletion decision rests on. Deliberately
// small and deliberately NOT corev1.Pod: this is a hostile-ish read used to
// decide what to DESTROY, and it should take only the fields that decide.
type agentPod struct {
	Name string
	// Alias is metadata.labels["gc-agent"] -- the session alias, unvalidated.
	Alias string
	// CreatedAt is metadata.creationTimestamp. A zero value means UNKNOWN AGE
	// and is treated as too young, never as "epoch, therefore ancient".
	CreatedAt time.Time
	// Phase is status.phase. Succeeded/Failed hold no agent.
	Phase string
	// Terminating reports a non-nil metadata.deletionTimestamp: the pod is
	// already on its way out and a second delete would add nothing.
	Terminating bool
}

// podLister is the Kubernetes surface this needs, which is exactly two verbs.
// An interface so a test can drive the reaper without a cluster; the production
// implementation is k8sPodReaper below.
type podLister interface {
	// ListAgentPods returns every app=gc-agent pod in the namespace. An error
	// means the reaper deletes NOTHING this tick.
	ListAgentPods(ctx context.Context) ([]agentPod, error)
	// DeletePod deletes one pod by name.
	DeletePod(ctx context.Context, name string) error
}

// runPodReap deletes agent pods that leaked past every bead-level teardown.
//
// claimed is claimedSessionIDs' output -- the aliases a RUNNING record claims --
// and listed is every alias gascity's session list returned on this same tick.
// Both are passed in rather than re-read: they are the reads runReap already
// fails closed on, and re-reading them here would give this function a second,
// possibly disagreeing, view of what is live.
//
// A pod whose alias gascity still LISTS is not ours to delete: the session is
// open-bead, runReap's own loop has already judged it against the same grace and
// the same claim set, and a close in flight tears the pod down properly.
func runPodReap(ctx context.Context, dd reaperDeps, claimed, listed map[string]struct{}) {
	if dd.Pods == nil {
		return // not configured; see podReapNamespaceEnv
	}

	// FAIL CLOSED ON THE POD LIST, for the same reason runReap fails closed on
	// the bead store: a read that did not happen is not a read that found
	// nothing. There is no partial-list mode here -- one error, zero deletions.
	pods, err := dd.Pods.ListAgentPods(ctx)
	if err != nil {
		dd.Log.Error("reap: cannot list agent pods; deleting NOTHING this tick", "err", err)
		return
	}

	now := dd.Now()
	for _, p := range pods {
		if !gonkSessionAlias.MatchString(p.Alias) {
			continue // not a session gonk minted; not our business
		}
		if _, live := claimed[p.Alias]; live {
			continue // a running bead claims it: this is a working agent
		}
		if _, open := listed[p.Alias]; open {
			// gascity still lists it, so its bead is open and runReap's session
			// loop owns it -- including the decision not to touch it yet.
			continue
		}
		if p.Terminating {
			continue // already on its way out
		}
		if p.Phase == "Succeeded" || p.Phase == "Failed" {
			// No agent is running in a terminal pod, so it is not the leak this
			// recovers -- and its logs are the only surviving record of what the
			// session did. Reclaiming those is gascity's own teardown's job.
			continue
		}
		if p.CreatedAt.IsZero() {
			// UNKNOWN AGE IS TREATED AS TOO YOUNG, exactly as in runReap.
			dd.Log.Warn("reap: agent pod has no readable creation timestamp; leaving it alone",
				"pod", p.Name, "session", p.Alias)
			continue
		}
		if now.Sub(p.CreatedAt) < reapGrace {
			continue // a dispatch may still be setting it up
		}

		// LAST GATE, AND THE ONLY ONE THAT ASKS GASCITY. Everything above is
		// gonk's own view; this asks the owner of the pod whether it still wants
		// it. Only a 404 -- the allowClosed=false signature of a closed session
		// bead -- authorizes deletion.
		err := dd.GC.CloseSession(ctx, p.Alias)
		switch {
		case err == nil:
			// gascity owned it after all. Its provider stops the runtime and
			// reclaims the pod AND the per-session ConfigMaps; deleting the pod
			// out from under that would strand the rest.
			dd.Log.Warn("reap: closed an ORPHANED session found from its POD -- gascity still owned it",
				"pod", p.Name, "session", p.Alias, "age", now.Sub(p.CreatedAt).String())
		case gcapi.IsNotFound(err):
			// THE gonk-mfsr CLASS. No open session bead, so no gascity call will
			// ever reach this pod again. The pod list is the only thing that can
			// recover it, and this is that recovery.
			if derr := dd.Pods.DeletePod(ctx, p.Name); derr != nil {
				dd.Log.Error("reap: could not delete a LEAKED agent pod -- IT IS STILL RUNNING",
					"pod", p.Name, "session", p.Alias, "err", derr)
				continue
			}
			dd.Log.Warn("reap: DELETED a LEAKED agent pod -- its session bead was already closed, "+
				"so no gascity call could reach it",
				"pod", p.Name, "session", p.Alias, "age", now.Sub(p.CreatedAt).String())
		default:
			// Ambiguous: a timeout, a 5xx, a rejected grant. We do not know
			// whether gascity owns this pod, and "do not know" means do nothing.
			dd.Log.Error("reap: could not ask gascity about an unclaimed agent pod; NOT deleting it",
				"pod", p.Name, "session", p.Alias, "err", err)
		}
	}
}

// k8sPodReaper is podLister against a real cluster.
//
// ITS TWO CALLS ARE ITS WHOLE RBAC FOOTPRINT: Pods(ns).List and Pods(ns).Delete,
// which is why chart/gonk/templates/role-gonk-gate-podreap.yaml grants exactly
// `pods: [list, delete]` in one namespace and internal/charttest reads THIS FILE
// to check that it still does.
type k8sPodReaper struct {
	cs        kubernetes.Interface
	namespace string
}

// newPodReaper builds a reaper from the in-cluster service account, or returns
// nil when the feature is not configured or there is no cluster.
//
// nil, not an error: a local or test invocation of `gonk-gate sweep` must keep
// working, and runPodReap treats nil as "off". The namespace check is the
// important half -- see podReapNamespaceEnv for why an empty namespace is not
// merely useless but dangerous.
func newPodReaper(namespace string) *k8sPodReaper {
	return newPodReaperWith(namespace, inClusterClientset)
}

// newPodReaperWith is newPodReaper with the clientset construction injected.
//
// The seam exists for ONE reason and it is worth the extra function. Off a
// cluster, inClusterClientset fails, so newPodReaper returns nil for EVERY
// namespace -- including the empty one -- and a test of the empty-namespace
// refusal passes whether or not the refusal is there. That is a green test
// asserting nothing, on the guard that stands between this code and a
// CLUSTER-SCOPED pod list and delete. With the builder injected, the test can
// hand it one that always succeeds and see the refusal actually refuse.
func newPodReaperWith(namespace string, build func() (kubernetes.Interface, error)) *k8sPodReaper {
	if namespace == "" {
		return nil
	}
	cs, err := build()
	if err != nil {
		return nil
	}
	return &k8sPodReaper{cs: cs, namespace: namespace}
}

// podReaperOrNil converts a possibly-nil *k8sPodReaper into a podLister that is
// ACTUALLY nil when there is nothing there.
//
// THE TYPED-NIL TRAP, AND IT IS NOT THEORETICAL HERE. Assigning a nil
// *k8sPodReaper straight into an interface field yields a NON-nil interface
// holding a nil pointer, so runPodReap's `if dd.Pods == nil` would be false,
// ListAgentPods would return "no cluster access", and every sweep tick on every
// install with the feature OFF would log
// `reap: cannot list agent pods; deleting NOTHING this tick`. That is a
// permanent ERROR line describing a feature nobody turned on -- the kind of
// noise that teaches operators to ignore the reaper's log, which is the one
// place a real leak announces itself.
func podReaperOrNil(k *k8sPodReaper) podLister {
	if k == nil {
		return nil
	}
	return k
}

func (k *k8sPodReaper) ListAgentPods(ctx context.Context) ([]agentPod, error) {
	if k == nil || k.cs == nil || k.namespace == "" {
		return nil, fmt.Errorf("podreap: no cluster access")
	}
	list, err := k.cs.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: agentPodSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("podreap: list pods in %s: %w", k.namespace, err)
	}
	out := make([]agentPod, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		out = append(out, agentPod{
			Name:        p.Name,
			Alias:       p.Labels[agentAliasLabel],
			CreatedAt:   p.CreationTimestamp.Time,
			Phase:       string(p.Status.Phase),
			Terminating: p.DeletionTimestamp != nil,
		})
	}
	return out, nil
}

func (k *k8sPodReaper) DeletePod(ctx context.Context, name string) error {
	if k == nil || k.cs == nil || k.namespace == "" {
		return fmt.Errorf("podreap: no cluster access")
	}
	if name == "" {
		return fmt.Errorf("podreap: refusing to delete a pod with no name")
	}
	if err := k.cs.CoreV1().Pods(k.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("podreap: delete pod %s/%s: %w", k.namespace, name, err)
	}
	return nil
}

var _ podLister = (*k8sPodReaper)(nil)
