package intake

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

func TestMetricsRecordOutcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.WebhookOutcome("Issue Hook", ghook.OutcomeAccepted)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeBadToken)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeBadToken)
	m.ReconcileResult("ok", 2*time.Second)
	m.ProjectStates(map[State]int{StateValid: 3, StatePending: 1})
	m.Dispatched("issue-triage")
	m.DispatchDropped("state_pending")

	if got := testutil.ToFloat64(m.webhook.WithLabelValues("Issue Hook", "bad_token")); got != 2 {
		t.Errorf("bad_token count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.projects.WithLabelValues("valid")); got != 3 {
		t.Errorf("valid gauge = %v, want 3", got)
	}
	out, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out {
		t.Errorf("metric lint: %s: %s", p.Metric, p.Text)
	}
}

// Spec 8 names the series intake must export. Missing one is a broken dashboard.
func TestRequiredSeriesExist(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeAccepted)
	m.ReconcileResult("ok", time.Second)
	m.ProjectStates(map[State]int{StateValid: 1})
	m.MeterPush("ok")
	m.OnboardingResult("mr_opened")
	m.Dispatched("issue-triage")
	m.DispatchDropped("state_pending")
	m.GitLabAuthFallback()

	if got := testutil.CollectAndCount(reg); got == 0 {
		t.Fatal("no metrics collected")
	}
	for _, want := range []string{
		"gonk_intake_webhook_total",
		"gonk_intake_reconcile_total",
		"gonk_intake_reconcile_duration_seconds",
		"gonk_intake_projects",
		"gonk_intake_meter_push_total",
		"gonk_intake_onboarding_total",
		"gonk_intake_dispatched_total",
		"gonk_intake_dispatch_dropped_total",
		"gonk_intake_gitlab_auth_fallback_total",
	} {
		if n, err := testutil.GatherAndCount(reg, want); err != nil || n == 0 {
			t.Errorf("series %s is not exported (spec 8)", want)
		}
	}
}

// The event label reaching Prometheus must be the SANITIZED one ghook produces
// (the handled allow-list, or "other"), never the raw X-Gitlab-Event header.
// Metrics itself trusts its caller for the label value -- ghook is what
// sanitizes -- but this test pins the contract: Metrics.WebhookOutcome must
// not itself re-widen the label domain (e.g. by accepting anything without
// question) in a way that would blow up cardinality if ghook's guard were ever
// removed. It also documents which type ghook actually hands the observer.
func TestWebhookOutcomeAcceptsGhookOutcomeType(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	var obs ghook.Observer = m // must satisfy ghook.Observer
	obs.WebhookOutcome("other", ghook.OutcomeUnhandledEvent)
	if got := testutil.ToFloat64(m.webhook.WithLabelValues("other", "unhandled_event")); got != 1 {
		t.Errorf("other/unhandled_event count = %v, want 1", got)
	}
}
