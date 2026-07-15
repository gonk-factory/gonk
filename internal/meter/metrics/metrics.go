// Package metrics is gonk-meter's Prometheus surface (spec 8).
//
// *** THE ONE CARDINALITY RULE: NEVER LABEL A METRIC WITH bead_id OR
// session_key. *** Both are unbounded (one value per work item / per session,
// forever), and an unbounded Prometheus label kills the TSDB -- this is the
// same class of bug intake's review caught (pkg/intake/metrics.go). Per-bead
// and per-session cost lives in the cost API (pkg/meterapi) and in Loki
// (where bead_id is already a label there, not here), never in a metric.
// TestNoUnboundedLabels gathers every series this package registers and
// fails if either label name appears anywhere.
//
// Every OTHER label here (project, rung, trigger, decision, reason, outcome,
// state) is drawn from a small, operator- or contract-bounded set: the
// project count is the number of onboarded repos, the rung count is the
// operator's catalog, and decision/reason/outcome are meterapi's own
// contract constants. That is bounded cardinality, not zero-label purity --
// the table in Plan 03 Task 9 is explicit that project/rung/trigger labels
// are fine.
//
// Registration is on a PRIVATE registry (New takes a prometheus.Registerer),
// matching cmd/gonk-intake's house style: no default go_*/process_* series
// unless the caller explicitly registers prometheus.NewGoCollector() etc.
// against the same registry.
package metrics

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// legalReasons is meterapi's bounded reason set, plus "" for a Run decision
// (which carries no reason). Anything else collapses to unknownReason before
// it ever reaches a label -- so a future bug that starts passing free text
// (a LiteLLM error string, say) becomes a bounded, obviously-wrong label
// value instead of a cardinality bomb.
var legalReasons = map[string]bool{
	"":                                    true,
	meterapi.ReasonNotRegistered:          true,
	meterapi.ReasonDisabled:               true,
	meterapi.ReasonInvalidConfig:          true,
	meterapi.ReasonActionNotAllowed:       true,
	meterapi.ReasonKeyMissing:             true,
	meterapi.ReasonQuietHours:             true,
	meterapi.ReasonSpendStale:             true,
	meterapi.ReasonLadderExhausted:        true,
	meterapi.ReasonInfraRetriesExhausted:  true,
	meterapi.ReasonPerTaskTokensExhausted: true,
	meterapi.ReasonMonthlyTokensExhausted: true,
	meterapi.ReasonMonthlyCostExhausted:   true,
}

var legalDecisions = map[string]bool{
	meterapi.DecisionRun:   true,
	meterapi.DecisionDefer: true,
	meterapi.DecisionDeny:  true,
}

var legalOutcomes = map[string]bool{
	meterapi.OutcomeSuccess:     true,
	meterapi.OutcomeGateFailed:  true,
	meterapi.OutcomeInfraFailed: true,
	meterapi.OutcomeAborted:     true,
}

const unknownLabel = "unknown"

// Metrics is gonk-meter's whole Prometheus surface: spec 8's required series
// plus the money-safety labels (synthetic=true/false) that keep a local
// rung's accounting fiction off the real-spend panels (Decision 9).
type Metrics struct {
	spendUSD              *prometheus.CounterVec
	tokens                *prometheus.CounterVec
	budgetRemainingUSD    *prometheus.GaugeVec
	budgetRemainingTokens *prometheus.GaugeVec
	policyDecisions       *prometheus.CounterVec
	ladderEscalations     *prometheus.CounterVec
	gateOutcomes          *prometheus.CounterVec
	deferredBeads         *prometheus.GaugeVec
	projectState          *prometheus.GaugeVec
	virtualKeys           *prometheus.GaugeVec
	spendSyncAgeSeconds   prometheus.Gauge
	spendSyncedAtSeconds  prometheus.Gauge
	spendSyncFailures     prometheus.Counter
	spendRowsUnattributed prometheus.Counter
	reservationsOpen      *prometheus.GaugeVec
	reservationsExpired   *prometheus.CounterVec
	clockSkewSeconds      prometheus.Gauge
	clockSkewUnknown      prometheus.Counter
	budgetWindowStart     prometheus.Gauge
	coldStart             prometheus.Counter
	reservationRacesLost  *prometheus.CounterVec
	catalogDrift          prometheus.Counter
}

// New builds Metrics and registers every series on reg. reg is normally a
// fresh prometheus.NewRegistry() (a private registry, per house style) --
// never prometheus.DefaultRegisterer, which would pull in the default
// go_*/process_* collectors nobody asked for here.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		spendUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_spend_usd_total",
			Help: "Dollars billed by LiteLLM, by project/rung/trigger, split real vs synthetic (Decision 9). NEVER sum synthetic=true into real spend.",
		}, []string{"project", "rung", "trigger", "synthetic"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_tokens_total",
			Help: "Prompt/completion tokens by project/rung/trigger. Local rungs meter tokens even though they cost 0 real dollars (spec 6.3).",
		}, []string{"project", "rung", "trigger", "kind"}),
		budgetRemainingUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_budget_remaining_usd",
			Help: "Remaining monthly USD headroom by project. +Inf for an unlimited ceiling -- Prometheus's text format supports it; the JSON cost API uses null for the same fact (pkg/budget).",
		}, []string{"project"}),
		budgetRemainingTokens: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_budget_remaining_tokens",
			Help: "Remaining monthly token headroom by project. +Inf for an unlimited ceiling -- NOT the raw MaxInt64 sentinel, which would render as ~9.22e18 in Grafana.",
		}, []string{"project"}),
		policyDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_policy_decisions_total",
			Help: "POST /v1/policy/decide outcomes by project/decision/reason. decision and reason are both meterapi contract constants -- a bounded set.",
		}, []string{"project", "decision", "reason"}),
		ladderEscalations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_ladder_escalations_total",
			Help: "Ladder escalations (gate-failed outcomes) by project and rung transition.",
		}, []string{"project", "from_rung", "to_rung"}),
		gateOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_gate_outcomes_total",
			Help: "POST /v1/policy/outcome reports by project/rung/outcome.",
		}, []string{"project", "rung", "outcome"}),
		deferredBeads: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_deferred_beads",
			Help: "Beads currently waiting on a defer decision (budget exhausted, quiet hours, stale spend, key not yet provisioned), by project.",
		}, []string{"project"}),
		projectState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_project_state",
			Help: "1 for the project's current registration state, 0 for the others (active, disabled, invalid, key-missing).",
		}, []string{"project", "state"}),
		virtualKeys: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_virtual_keys",
			Help: "Registered projects by virtual-key state (active, key-missing).",
		}, []string{"state"}),
		spendSyncAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gonk_meter_spend_sync_age_seconds",
			Help: "now - spend_as_of of the last successful sync. The ALERTING series: 'is our spend view stale?'",
		}),
		spendSyncedAtSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gonk_meter_spend_synced_at_seconds",
			Help: "Unix timestamp of the spend_as_of of the last successful sync. The WAIT-ON-AS-A-PREDICATE series: 'has my call landed yet?', distinct from the age series used for alerting.",
		}),
		spendSyncFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_meter_spend_sync_failures_total",
			Help: "Spend-log poll attempts that failed (LiteLLM unreachable, bad response, store write failure).",
		}),
		spendRowsUnattributed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_meter_spend_rows_unattributed_total",
			Help: "Spend-log rows dropped because no project could be resolved from their metadata at all (internal/meter/litellm.HTTPSpendSource.Unattributed).",
		}),
		reservationsOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_meter_reservations_open",
			Help: "Reservations currently holding budget (in-flight or settled-but-not-yet-expired), by project.",
		}, []string{"project"}),
		reservationsExpired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_reservations_expired_total",
			Help: "Reservations the janitor reclaimed past their TTL (a session that died silently), by project.",
		}, []string{"project"}),
		clockSkewSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gonk_meter_clock_skew_seconds",
			Help: "abs(our clock - LiteLLM's HTTP Date header) on the last spend sync that reported one.",
		}),
		clockSkewUnknown: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_meter_clock_skew_unknown_total",
			Help: "Spend syncs where the source sent no Date header, so skew could not be measured.",
		}),
		budgetWindowStart: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gonk_meter_budget_window_start_seconds",
			Help: "Unix timestamp of the start of the budget window currently in force.",
		}),
		coldStart: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_meter_cold_start_total",
			Help: "Times this process completed its first spend sync after starting with no prior sync (a fresh replica, not genuine staleness).",
		}),
		reservationRacesLost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_meter_reservation_races_lost_total",
			Help: "Decide calls that lost a concurrent reservation race (rung.Decide said yes on a now-stale snapshot) and deferred instead, by project.",
		}, []string{"project"}),
		catalogDrift: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gonk_meter_catalog_drift_total",
			Help: "Mismatches found between the operator rung catalog and LiteLLM's real /model/info list (AD-3). Live-LiteLLM verification is Plan 06's job; this series exists so a producer can be wired there without a contract change.",
		}),
	}

	reg.MustRegister(
		m.spendUSD, m.tokens, m.budgetRemainingUSD, m.budgetRemainingTokens,
		m.policyDecisions, m.ladderEscalations, m.gateOutcomes,
		m.deferredBeads, m.projectState, m.virtualKeys,
		m.spendSyncAgeSeconds, m.spendSyncedAtSeconds, m.spendSyncFailures, m.spendRowsUnattributed,
		m.reservationsOpen, m.reservationsExpired,
		m.clockSkewSeconds, m.clockSkewUnknown, m.budgetWindowStart,
		m.coldStart, m.reservationRacesLost, m.catalogDrift,
	)
	return m
}

// tokenLimitToFloat converts the token sentinel to +Inf explicitly. THIS IS
// THE CONVERSION THE TASK CALLS OUT: budget.TokenLimit's unlimited value is
// math.MaxInt64, not +Inf, so a naive float64(int64(t)) here is exactly what
// produces the 9.22e18 gauge in Grafana. The USD side needs no such
// conversion because budget.CostLimit's unlimited value already IS math.Inf(1).
func tokenLimitToFloat(t budget.TokenLimit) float64 {
	if t.Unlimited() {
		return math.Inf(1)
	}
	return float64(t)
}

func costLimitToFloat(c budget.CostLimit) float64 {
	if c.Unlimited() {
		return math.Inf(1) // budget.CostLimit's sentinel already is +Inf; spelled out for the reader.
	}
	return float64(c)
}

func syntheticLabel(synthetic bool) string {
	if synthetic {
		return "true"
	}
	return "false"
}

func legalReason(reason string) string {
	if legalReasons[reason] {
		return reason
	}
	return unknownLabel
}

func legalDecision(decision string) string {
	if legalDecisions[decision] {
		return decision
	}
	return unknownLabel
}

func legalOutcome(outcome string) string {
	if legalOutcomes[outcome] {
		return outcome
	}
	return unknownLabel
}

// ---------------------------------------------------------------- spend / tokens

// RecordSpendRow folds one newly-landed spend row into the counters. It is
// called from the sync loop as new rows land -- NEVER from a full recompute,
// because a Prometheus counter that goes backwards breaks rate().
//
// synthetic selects the label, never the amount: LiteLLM has one `spend`
// column (spend.Row.CostUSD) for both real and synthetic dollars, and
// `synthetic` is the ONLY thing that keeps a local rung's accounting fiction
// off the "real spend" panel (Decision 9). Callers must pass spend.Row.Synthetic
// verbatim, never re-derive it.
func (m *Metrics) RecordSpendRow(project, rung, trigger string, synthetic bool, costUSD float64, promptTokens, completionTokens int64) {
	if costUSD > 0 {
		m.spendUSD.WithLabelValues(project, rung, trigger, syntheticLabel(synthetic)).Add(costUSD)
	}
	if promptTokens > 0 {
		m.tokens.WithLabelValues(project, rung, trigger, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.tokens.WithLabelValues(project, rung, trigger, "completion").Add(float64(completionTokens))
	}
}

// ---------------------------------------------------------------- budget gauges

// SetBudgetRemaining sets both remaining-headroom gauges for a project. See
// tokenLimitToFloat: the token gauge needs an explicit +Inf conversion that
// the USD one does not.
func (m *Metrics) SetBudgetRemaining(project string, usd budget.CostLimit, tokens budget.TokenLimit) {
	m.budgetRemainingUSD.WithLabelValues(project).Set(costLimitToFloat(usd))
	m.budgetRemainingTokens.WithLabelValues(project).Set(tokenLimitToFloat(tokens))
}

// ---------------------------------------------------------------- decisions / outcomes

// RecordDecision records one POST /v1/policy/decide outcome. decision and
// reason are normalized against the bounded contract sets (legalDecisions,
// legalReasons) before they ever become a label value -- see
// TestDecisionCounterUsesTheBoundedReasonSet.
func (m *Metrics) RecordDecision(project, decision, reason string) {
	m.policyDecisions.WithLabelValues(project, legalDecision(decision), legalReason(reason)).Inc()
}

// RecordGateOutcome records one POST /v1/policy/outcome report, keyed on the
// STORE's reservation rung -- never a caller-supplied one, which is exactly
// as forgeable as a caller-supplied outcome (Decision 2).
func (m *Metrics) RecordGateOutcome(project, rung, outcome string) {
	m.gateOutcomes.WithLabelValues(project, rung, legalOutcome(outcome)).Inc()
}

// RecordEscalation records a ladder escalation (a gate-failed outcome that
// advances the rung index).
func (m *Metrics) RecordEscalation(project, fromRung, toRung string) {
	m.ladderEscalations.WithLabelValues(project, fromRung, toRung).Inc()
}

// RecordReservationRaceLost records a Decide call that lost a concurrent
// reservation race and deferred instead of running.
func (m *Metrics) RecordReservationRaceLost(project string) {
	m.reservationRacesLost.WithLabelValues(project).Inc()
}

// ---------------------------------------------------------------- gauges refreshed on a tick

// SetDeferredBeads sets the current count of beads waiting on a defer
// decision for one project.
func (m *Metrics) SetDeferredBeads(project string, n int) {
	m.deferredBeads.WithLabelValues(project).Set(float64(n))
}

// allStates is store.State's set, mirrored here (as a []string, not an
// import of internal/meter/store -- this package stays free of a store
// dependency) so a project's inactive states are pinned to 0 rather than
// simply absent from the exposition.
var allStates = []string{"active", "disabled", "invalid", "key-missing"}

// SetProjectState sets state to 1 for a project's current state and 0 for
// every other legal state, so a dashboard panel sees an explicit 0 rather
// than a gap.
func (m *Metrics) SetProjectState(project, state string) {
	for _, s := range allStates {
		v := 0.0
		if s == state {
			v = 1
		}
		m.projectState.WithLabelValues(project, s).Set(v)
	}
}

// SetVirtualKeys sets the instance-wide count of registered projects by
// virtual-key state (no project label -- this is a fleet-wide gauge).
func (m *Metrics) SetVirtualKeys(active, keyMissing int) {
	m.virtualKeys.WithLabelValues("active").Set(float64(active))
	m.virtualKeys.WithLabelValues("key-missing").Set(float64(keyMissing))
}

// SetReservationsOpen sets the current open-reservation count for a project.
func (m *Metrics) SetReservationsOpen(project string, n int) {
	m.reservationsOpen.WithLabelValues(project).Set(float64(n))
}

// RecordReservationsExpired adds n janitor-reclaimed reservations to a
// project's counter.
func (m *Metrics) RecordReservationsExpired(project string, n int) {
	if n > 0 {
		m.reservationsExpired.WithLabelValues(project).Add(float64(n))
	}
}

// SetSpendSyncAge sets the alerting series: now - spend_as_of.
func (m *Metrics) SetSpendSyncAge(age time.Duration) {
	m.spendSyncAgeSeconds.Set(age.Seconds())
}

// SetSpendSyncedAt sets the wait-on-as-a-predicate series: the absolute
// Unix timestamp of the last successful sync's spend_as_of.
func (m *Metrics) SetSpendSyncedAt(t time.Time) {
	m.spendSyncedAtSeconds.Set(float64(t.Unix()))
}

// RecordSpendSyncFailure records one failed spend-log poll.
func (m *Metrics) RecordSpendSyncFailure() {
	m.spendSyncFailures.Inc()
}

// AddSpendRowsUnattributed adds n to the unattributed-rows counter. Callers
// pass the DELTA since the last call (HTTPSpendSource.Unattributed() is a
// cumulative counter itself); never pass the raw cumulative value, or this
// counter would double-count on every tick.
func (m *Metrics) AddSpendRowsUnattributed(n int) {
	if n > 0 {
		m.spendRowsUnattributed.Add(float64(n))
	}
}

// SetClockSkew sets the last-measured clock skew against the spend source.
func (m *Metrics) SetClockSkew(skew time.Duration) {
	if skew < 0 {
		skew = -skew
	}
	m.clockSkewSeconds.Set(skew.Seconds())
}

// RecordClockSkewUnknown records a spend sync whose source sent no Date
// header, so skew could not be measured.
func (m *Metrics) RecordClockSkewUnknown() {
	m.clockSkewUnknown.Inc()
}

// SetBudgetWindowStart sets the Unix timestamp of the budget window
// currently in force.
func (m *Metrics) SetBudgetWindowStart(start time.Time) {
	m.budgetWindowStart.Set(float64(start.Unix()))
}

// RecordColdStart records this process completing its first spend sync
// after starting with no prior one.
func (m *Metrics) RecordColdStart() {
	m.coldStart.Inc()
}

// RecordCatalogDrift records one mismatch between the operator rung catalog
// and LiteLLM's real model list. No producer calls this yet in Plan 03 --
// the /model/info startup check it measures is live-LiteLLM verification,
// which AD-3 assigns to Plan 06 -- but the series is declared now so a
// dashboard and the eventual producer share one contract.
func (m *Metrics) RecordCatalogDrift() {
	m.catalogDrift.Inc()
}
