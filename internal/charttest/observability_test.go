//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// containerByName pulls one container out of a rendered workload.
func containerByName(t *testing.T, o Object, name string) map[string]any {
	t.Helper()
	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pspec, _ := tmpl["spec"].(map[string]any)
	list, _ := pspec["containers"].([]any)
	var have []string
	for _, c := range list {
		cm, _ := c.(map[string]any)
		n, _ := cm["name"].(string)
		have = append(have, n)
		if n == name {
			return cm
		}
	}
	t.Fatalf("no container %q in %s/%s; got %v", name, o.Kind, o.Metadata.Name, have)
	return nil
}

func podVolumes(t *testing.T, o Object) map[string]any {
	t.Helper()
	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pspec, _ := tmpl["spec"].(map[string]any)
	list, _ := pspec["volumes"].([]any)
	out := map[string]any{}
	for _, v := range list {
		vm, _ := v.(map[string]any)
		n, _ := vm["name"].(string)
		out[n] = vm
	}
	return out
}

func controllerWorkload(t *testing.T, extra ...string) Object {
	t.Helper()
	return MustObject(t, Render(t, append(Minimum(), extra...)...), "Deployment", "gonk-controller")
}

// THE DEAD-SWEEP ALARM (gonk-pop3 item 4 / gonk-p7qh).
//
// gonk-sweep could not reach its own database for DAYS and its only signal was
// a log line nobody read. The controller's readiness is now that signal: a
// `kubectl get pods` showing 0/1 READY is visible without knowing to look for
// it, and an unready controller leaves the Service's endpoints so intake stops
// dispatching work whose outcome could never be recorded.
//
// This test exists because a probe that quietly reverted to tcpSocket would be
// indistinguishable from one that works -- both stay green.
func TestControllerReadinessProbeIsTheDeadSweepAlarm(t *testing.T) {
	o := controllerWorkload(t)
	c := containerByName(t, o, "controller")

	probe, ok := c["readinessProbe"].(map[string]any)
	if !ok {
		t.Fatal("the supervisor container has no readinessProbe")
	}
	if _, isTCP := probe["tcpSocket"]; isTCP {
		t.Fatal("readinessProbe is still a bare tcpSocket: a dead outcome path would stay READY")
	}
	execProbe, ok := probe["exec"].(map[string]any)
	if !ok {
		t.Fatalf("readinessProbe is not an exec probe: %+v", probe)
	}
	cmd, _ := execProbe["command"].([]any)
	var parts []string
	for _, a := range cmd {
		s, _ := a.(string)
		parts = append(parts, s)
	}
	joined := strings.Join(parts, " ")

	if !strings.Contains(joined, "gonk-gate") || !strings.Contains(joined, "sweep-health") {
		t.Errorf("readinessProbe does not run `gonk-gate sweep-health`: %q", joined)
	}
	// A container gets exactly ONE readinessProbe, so the exec probe must also
	// do what the tcpSocket probe did or this trades one blind spot for another.
	if !strings.Contains(joined, "--supervisor-addr=127.0.0.1:9443") {
		t.Errorf("readinessProbe no longer checks the supervisor port: %q", joined)
	}
	if !strings.Contains(joined, "--max-age=15m") {
		t.Errorf("readinessProbe does not carry gascity.sweepHealthMaxAge: %q", joined)
	}

	// The probe and the sweep must agree on WHICH FILE. A probe reading a
	// different path than the sweep writes is a probe that is always green.
	env := workloadEnv(t, o)
	if env["GONK_SWEEP_HEALTH_FILE"] != "/city/gonk-sweep-health.json" {
		t.Errorf("GONK_SWEEP_HEALTH_FILE = %q, want the /city path the probe's default reads",
			env["GONK_SWEEP_HEALTH_FILE"])
	}
	// ...and it must be on a WRITABLE volume. containerSecurityContext sets
	// readOnlyRootFilesystem: true, so a health file outside a mounted volume
	// would silently never be written.
	if !strings.HasPrefix(env["GONK_SWEEP_HEALTH_FILE"], "/city/") {
		t.Errorf("GONK_SWEEP_HEALTH_FILE = %q is not on the writable /city emptyDir",
			env["GONK_SWEEP_HEALTH_FILE"])
	}

	// Liveness must NOT have been touched: a dead sweep is self-healing and
	// must not restart the pod.
	live, ok := c["livenessProbe"].(map[string]any)
	if !ok {
		t.Fatal("the supervisor container lost its livenessProbe")
	}
	if _, isTCP := live["tcpSocket"]; !isTCP {
		t.Errorf("livenessProbe is no longer a tcpSocket: %+v -- a failing sweep must not kill the pod", live)
	}
}

// THE TRANSCRIPT ARCHIVE IS DEV-ONLY AND OFF BY DEFAULT (gonk-pop3 item 2).
// It holds untrusted model output over untrusted user input; a default-on
// archive would be a commitment nobody asked for.
func TestTranscriptArchiveIsOffByDefault(t *testing.T) {
	o := controllerWorkload(t)
	if got := workloadEnv(t, o)["GONK_TRANSCRIPT_DIR"]; got != "" {
		t.Errorf("GONK_TRANSCRIPT_DIR = %q by default, want unset", got)
	}
	if _, ok := podVolumes(t, o)["transcripts"]; ok {
		t.Error("the transcripts volume renders by default; the archive must be opt-in")
	}
}

// Turned on, it must reach the pod as a real, bounded, writable mount --
// readOnlyRootFilesystem is true, so a directory that is not a volume would
// leave the archive silently failing every write.
func TestTranscriptArchiveWiresAWritableBoundedVolumeWhenEnabled(t *testing.T) {
	o := controllerWorkload(t, "--set", "dev.transcripts.enabled=true")

	if got := workloadEnv(t, o)["GONK_TRANSCRIPT_DIR"]; got != "/transcripts" {
		t.Fatalf("GONK_TRANSCRIPT_DIR = %q, want /transcripts", got)
	}

	vol, ok := podVolumes(t, o)["transcripts"].(map[string]any)
	if !ok {
		t.Fatal("dev.transcripts.enabled=true renders no transcripts volume")
	}
	ed, ok := vol["emptyDir"].(map[string]any)
	if !ok {
		t.Fatalf("the transcripts volume is not an emptyDir: %+v -- a PVC is durable storage of untrusted text", vol)
	}
	if ed["sizeLimit"] != "512Mi" {
		t.Errorf("transcripts emptyDir sizeLimit = %v, want 512Mi -- an unbounded archive evicts the pod", ed["sizeLimit"])
	}

	c := containerByName(t, o, "controller")
	mounts, _ := c["volumeMounts"].([]any)
	found := ""
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "transcripts" {
			found, _ = mm["mountPath"].(string)
		}
	}
	if found != "/transcripts" {
		t.Errorf("transcripts mountPath = %q, want /transcripts (it must match GONK_TRANSCRIPT_DIR)", found)
	}
}

// The readinessProbe treats a MISSING health record as READY, so that a pod
// which has not yet completed its first 30s sweep pass is not held out of its
// own Service. That leaves one hole: a gonk-sweep cooldown order that never
// runs AT ALL writes no record ever, and the probe would stay green through a
// total outage -- the same class of invisible failure as gonk-p7qh.
//
// The bootstrap initContainer closes it by seeding an ok record stamped at pod
// start, so the staleness check runs from then. This test exists because the
// seed is one line in a shell script and deleting it would break nothing
// visible.
func TestBootstrapSeedsTheSweepHealthRecord(t *testing.T) {
	o := controllerWorkload(t)
	spec, _ := o.Raw["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pspec, _ := tmpl["spec"].(map[string]any)
	inits, _ := pspec["initContainers"].([]any)

	script := ""
	for _, c := range inits {
		cm, _ := c.(map[string]any)
		if cm["name"] != "bootstrap-city" {
			continue
		}
		args, _ := cm["args"].([]any)
		for _, a := range args {
			s, _ := a.(string)
			script += s
		}
	}
	if script == "" {
		t.Fatal("no bootstrap-city initContainer script in the render")
	}
	if !strings.Contains(script, "/city/gonk-sweep-health.json") {
		t.Errorf("the bootstrap does not seed the sweep health record; a sweep that never runs would stay READY forever:\n%s", script)
	}
	// It must seed OK, not failed: a pod whose sweep is merely still starting
	// must not be born unready.
	if !strings.Contains(script, `"ok":true`) {
		t.Errorf("the seeded record is not an ok record:\n%s", script)
	}
	// And it must be written to the SAME path the sweep and the probe use.
	env := workloadEnv(t, o)
	if !strings.Contains(script, env["GONK_SWEEP_HEALTH_FILE"]) {
		t.Errorf("the bootstrap seeds a different path (%s) than GONK_SWEEP_HEALTH_FILE (%s)",
			"see script", env["GONK_SWEEP_HEALTH_FILE"])
	}
}
