package intake

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

// Metrics implements Observer (and ghook.Observer). Series names and label
// values are a contract with the shipped Grafana dashboards (spec 8): renaming
// one silently blanks a panel.
//
// SECURITY NOTE (why this type is safe against an unauthenticated flood): every
// label value that reaches WebhookOutcome comes from ghook, which has already
// collapsed the attacker-controlled X-Gitlab-Event header onto a bounded set
// (the handled events, or "other") before calling Observer.WebhookOutcome.
// Metrics never sees, and never labels on, the raw header.
type Metrics struct {
	webhook      *prometheus.CounterVec
	reconcile    *prometheus.CounterVec
	reconDur     prometheus.Histogram
	projects     *prometheus.GaugeVec
	meterPush    *prometheus.CounterVec
	onboarding   *prometheus.CounterVec
	dispatched   *prometheus.CounterVec
	dropped      *prometheus.CounterVec
	authFallback prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		webhook: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_webhook_total",
			Help: "GitLab webhook deliveries by event and outcome (accepted, bad_token, duplicate, bot_authored, ...).",
		}, []string{"event", "outcome"}),
		reconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_reconcile_total",
			Help: "Reconciliation passes by result (ok, partial, error).",
		}, []string{"result"}),
		reconDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gonk_intake_reconcile_duration_seconds",
			Help:    "Duration of a full reconciliation pass.",
			Buckets: prometheus.DefBuckets,
		}),
		projects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_intake_projects",
			Help: "Projects by state. GitLab-determined: unmanaged, absent, declined. Meter-determined: invalid, disabled, key-missing. Neither: unsynced. The join: pending, valid.",
		}, []string{"state"}),
		meterPush: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_meter_push_total",
			Help: "Raw-config registrations with gonk-meter by result (ok, invalid, error).",
		}, []string{"result"}),
		onboarding: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_onboarding_total",
			Help: "Onboarding outcomes (mr_opened, access_requested, error).",
		}, []string{"result"}),
		dispatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_dispatched_total",
			Help: "Orders fired by trigger.",
		}, []string{"trigger"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_dispatch_dropped_total",
			Help: "Events that did not become orders, by reason.",
		}, []string{"reason"}),
		authFallback: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_intake_gitlab_auth_fallback_total",
			Help: "GitLab calls that were rejected on PAT rotation slot 1 and succeeded on slot 2 (AD-4b). Nonzero means a rotation is half-done: finish it.",
		}),
	}
	reg.MustRegister(m.webhook, m.reconcile, m.reconDur, m.projects, m.meterPush, m.onboarding, m.dispatched, m.dropped, m.authFallback)
	// Pre-create the state gauges so a dashboard shows 0 rather than nothing.
	for _, s := range AllStates {
		m.projects.WithLabelValues(string(s)).Set(0)
	}
	return m
}

func (m *Metrics) WebhookOutcome(event string, o ghook.Outcome) {
	m.webhook.WithLabelValues(event, string(o)).Inc()
}

func (m *Metrics) ReconcileResult(result string, d time.Duration) {
	m.reconcile.WithLabelValues(result).Inc()
	m.reconDur.Observe(d.Seconds())
}

func (m *Metrics) ProjectStates(counts map[State]int) {
	for _, s := range AllStates {
		m.projects.WithLabelValues(string(s)).Set(float64(counts[s]))
	}
}

func (m *Metrics) MeterPush(result string)        { m.meterPush.WithLabelValues(result).Inc() }
func (m *Metrics) OnboardingResult(result string) { m.onboarding.WithLabelValues(result).Inc() }
func (m *Metrics) Dispatched(trigger string)      { m.dispatched.WithLabelValues(trigger).Inc() }
func (m *Metrics) DispatchDropped(reason string)  { m.dropped.WithLabelValues(reason).Inc() }
func (m *Metrics) GitLabAuthFallback()            { m.authFallback.Inc() }
