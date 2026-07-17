//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// The public Service exposes ONE port, and it is the hook listener.
func TestPublicServiceExposesOnlyTheHookPort(t *testing.T) {
	svc := MustObject(t, Render(t, Minimum()...), "Service", "gonk-intake")
	if strings.Contains(svc.Doc, "9090") {
		t.Fatalf("the PUBLIC service exposes the private listener:\n%s", svc.Doc)
	}
	if !strings.Contains(svc.Doc, "8080") {
		t.Fatalf("the public service does not expose the hook port:\n%s", svc.Doc)
	}
}

// G13: the private listener serves /metrics, /healthz and POST /admin/reconcile
// -- an UNAUTHENTICATED reconcile trigger. If the Ingress can reach it, anyone
// who can reach the ingress can drive gonk's reconcile loop.
func TestIngressNeverExposesThePrivateListener(t *testing.T) {
	out := Render(t, append(Minimum(),
		"--set", "ingress.enabled=true",
		"--set", "ingress.className=nginx",
		"--set", "ingress.host=gonk.test")...)
	ing := MustObject(t, out, "Ingress", "gonk-intake")
	if strings.Contains(ing.Doc, "9090") || strings.Contains(ing.Doc, "gonk-intake-internal") {
		t.Fatalf("the Ingress can reach the private listener:\n%s", ing.Doc)
	}
	if !strings.Contains(ing.Doc, "/hook/gitlab") {
		t.Fatalf("the Ingress does not route the webhook:\n%s", ing.Doc)
	}
	if !strings.Contains(ing.Doc, "pathType: Exact") {
		t.Fatalf("the Ingress path is not Exact; a Prefix match on /hook/gitlab is wider than the one endpoint that exists:\n%s", ing.Doc)
	}
}

func TestNoIngressByDefault(t *testing.T) {
	NoObject(t, Render(t, Minimum()...), "Ingress", "gonk-intake")
}

// The internal Service is where Prometheus scrapes and where /readyz is checked.
func TestInternalServiceExposesOnlyThePrivatePort(t *testing.T) {
	svc := MustObject(t, Render(t, Minimum()...), "Service", "gonk-intake-internal")
	if strings.Contains(svc.Doc, "8080") {
		t.Fatalf("the internal service exposes the hook port too:\n%s", svc.Doc)
	}
}

// Intake's probes MUST live on the private listener: the public one 404s
// everything but the hook, so a probe against it would fail forever.
func TestIntakeProbesUseThePrivateListener(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	for _, want := range []string{"path: /readyz", "path: /healthz", "port: http-private"} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("intake probes are missing %q", want)
		}
	}
}

func TestIntakeCarriesTheDispatchAndMeterWiring(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	for _, want := range []string{
		"GONK_METER_URL", "http://gonk-meter:8080",
		"GONK_SUPERVISOR_URL",
		"GONK_BOT_USERNAME", "GONK_WEBHOOK_PUBLIC_URL", "GONK_WEBHOOK_TOKEN_GEN",
		"GONK_RECONCILE_INTERVAL",
	} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("intake deployment is missing %q", want)
		}
	}
}

// dispatch=log must leave GONK_SUPERVISOR_URL EMPTY -- that is how intake selects
// the LogDispatcher [P02 Task 10]. Setting it to "" explicitly, rather than
// omitting it, means a stale env from a previous release cannot resurrect the
// HTTP dispatcher.
func TestLogDispatchLeavesSupervisorURLEmpty(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "gascity.dispatch=log")...)
	d := MustObject(t, out, "Deployment", "gonk-intake")
	if !strings.Contains(d.Doc, `value: ""`) {
		t.Fatalf("dispatch=log did not blank GONK_SUPERVISOR_URL:\n%s", d.Doc)
	}
}

// A private GitLab CA must reach the process, or every GitLab call fails TLS.
func TestGitLabCAIsMountedWhenConfigured(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "gitlab.caCert.existingConfigMap=gitlab-ca")...)
	d := MustObject(t, out, "Deployment", "gonk-intake")
	for _, want := range []string{"SSL_CERT_FILE", "gitlab-ca"} {
		if !strings.Contains(d.Doc, want) {
			t.Errorf("the GitLab CA is not wired: missing %q", want)
		}
	}
}

// GONK_INSTANCE_LADDER is intake's OWN fail-closed guard (main.go: unset/empty
// refuses to start -- an empty ladder onboards every new project disabled,
// ADR-002). The chart must derive it from the SAME operator ladder gonk-meter
// resolves rungs against, not a separate value that could drift.
func TestIntakeCarriesTheInstanceLadder(t *testing.T) {
	d := MustObject(t, Render(t, Minimum()...), "Deployment", "gonk-intake")
	if !strings.Contains(d.Doc, "GONK_INSTANCE_LADDER") || !strings.Contains(d.Doc, "qwen-local") {
		t.Fatalf("intake deployment does not carry GONK_INSTANCE_LADDER from operatorConfig.instance.ladder:\n%s", d.Doc)
	}
}
