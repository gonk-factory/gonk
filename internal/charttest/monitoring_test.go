//go:build chart

package charttest

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func withMonitoring() []string {
	return append(Minimum(),
		"--set", "monitoring.serviceMonitor.enabled=true",
		"--set", "monitoring.prometheusRule.enabled=true",
		"--set", "monitoring.dashboards.enabled=true")
}

func TestMonitoringIsOffByDefault(t *testing.T) {
	out := Render(t, Minimum()...)
	for _, o := range Objects(t, out) {
		switch o.Kind {
		case "ServiceMonitor", "PrometheusRule":
			t.Fatalf("%s rendered by default; the chart must not assume a Prometheus Operator", o.Kind)
		}
	}
}

// Intake's metrics are on the PRIVATE listener. A ServiceMonitor pointed at the
// public Service would scrape the hook listener and get 404 forever.
func TestIntakeServiceMonitorScrapesThePrivateService(t *testing.T) {
	sm := MustObject(t, Render(t, withMonitoring()...), "ServiceMonitor", "gonk-intake")
	if !strings.Contains(sm.Doc, "http-private") {
		t.Fatalf("the intake ServiceMonitor does not target the private port:\n%s", sm.Doc)
	}
	if strings.Contains(sm.Doc, "exposure: public") {
		t.Fatal("the intake ServiceMonitor selects the PUBLIC service, which serves only /hook/gitlab")
	}
}

// AD-2 in Plan 02: an invalid .gonk.yml makes a project go dark SILENTLY. Plan 02
// says in as many words that Plan 05 must ship the alert. This is that alert.
func TestAlertOnInvalidProjects(t *testing.T) {
	pr := MustObject(t, Render(t, withMonitoring()...), "PrometheusRule", "gonk")
	for _, want := range []string{
		"GonkProjectConfigInvalid",
		`gonk_intake_projects{state="invalid"}`,
		"GonkMeterSpendSyncStale",
		"gonk_meter_spend_sync_age_seconds",
		"GonkMeterDown",
		"GonkReservationRacesLost",
		"GonkDeferredBeadsStuck",
	} {
		if !strings.Contains(pr.Doc, want) {
			t.Errorf("PrometheusRule is missing %q", want)
		}
	}
}

// THE RULE. Every money panel filters synthetic="false".
func TestCostDashboardNeverPresentsSyntheticDollarsAsSpend(t *testing.T) {
	cm := MustObject(t, Render(t, withMonitoring()...), "ConfigMap", "gonk-dashboard-cost")
	raw := cm.Data(t)["cost.json"]

	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("the cost dashboard is not valid JSON: %v", err)
	}
	if len(dash.Panels) == 0 {
		t.Fatal("the cost dashboard has no panels")
	}
	for _, p := range dash.Panels {
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, "gonk_meter_spend_usd_total") {
				continue // not a money panel
			}
			// A money panel either filters to REAL spend, or it is the one panel
			// that is explicitly and unmistakably labelled as synthetic.
			real := strings.Contains(tg.Expr, `synthetic="false"`)
			labelled := strings.Contains(strings.ToLower(p.Title), "synthetic")
			if !real && !labelled {
				t.Errorf("panel %q charts gonk_meter_spend_usd_total without synthetic=\"false\" "+
					"and without saying SYNTHETIC in its title. Local inference is priced "+
					"synthetically so LiteLLM's USD ceiling can gate TOKENS -- those dollars were "+
					"never billed to anyone. Presenting one as spend is the one way this design "+
					"can do real harm.\n  expr: %s", p.Title, tg.Expr)
			}
			if !real && labelled && !strings.Contains(tg.Expr, `synthetic="true"`) {
				t.Errorf("panel %q says synthetic but does not filter synthetic=\"true\"", p.Title)
			}
		}
	}
}

// All three dashboards, discoverable by the Grafana sidecar.
func TestThreeDashboardsAreShipped(t *testing.T) {
	out := Render(t, withMonitoring()...)
	for _, name := range []string{"gonk-dashboard-factory", "gonk-dashboard-cost", "gonk-dashboard-health"} {
		cm := MustObject(t, out, "ConfigMap", name)
		if cm.Metadata.Labels["grafana_dashboard"] != "1" {
			t.Errorf("%s is not discoverable by the Grafana sidecar: labels=%v", name, cm.Metadata.Labels)
		}
		for _, v := range cm.Data(t) {
			var any map[string]any
			if err := json.Unmarshal([]byte(v), &any); err != nil {
				t.Errorf("%s contains invalid dashboard JSON: %v", name, err)
			}
		}
	}
}

// meter's ServiceMonitor: one listener, /metrics unauthenticated on it.
func TestMeterServiceMonitorTargetsTheApiPort(t *testing.T) {
	sm := MustObject(t, Render(t, withMonitoring()...), "ServiceMonitor", "gonk-meter")
	if !strings.Contains(sm.Doc, "http-api") {
		t.Fatalf("the meter ServiceMonitor does not target http-api:\n%s", sm.Doc)
	}
	if !strings.Contains(sm.Doc, "/metrics") {
		t.Fatalf("the meter ServiceMonitor does not scrape /metrics:\n%s", sm.Doc)
	}
}

// Every ServiceMonitor is gated on its OWN component's toggle, not just the
// global monitoring switch -- a disabled component must never grow a scrape
// target aimed at a Service that was never rendered.
func TestServiceMonitorsAreGatedOnTheirOwnComponent(t *testing.T) {
	out := Render(t, append(withMonitoring(), "--set", "intake.enabled=false")...)
	NoObject(t, out, "ServiceMonitor", "gonk-intake")
	MustObject(t, out, "ServiceMonitor", "gonk-meter") // meter is unaffected

	// meter.enabled=false requires intake.enabled=false too (guard G8), so
	// disable both to exercise meter's gate.
	out2 := Render(t, append(withMonitoring(),
		"--set", "intake.enabled=false", "--set", "meter.enabled=false")...)
	NoObject(t, out2, "ServiceMonitor", "gonk-meter")
	NoObject(t, out2, "ServiceMonitor", "gonk-intake")
}

// knownMetrics is the ground truth: every series actually registered by the
// binaries (internal/meter/metrics/metrics.go, pkg/intake/metrics.go), plus the
// standard Prometheus `up` series that every ServiceMonitor target gets for
// free. An alert or dashboard panel referencing anything outside this set is
// referencing a metric the code does not emit.
var knownMetrics = map[string]bool{
	"up": true,

	"gonk_intake_webhook_total":              true,
	"gonk_intake_reconcile_total":            true,
	"gonk_intake_reconcile_duration_seconds": true,
	"gonk_intake_projects":                   true,
	"gonk_intake_meter_push_total":           true,
	"gonk_intake_onboarding_total":           true,
	"gonk_intake_dispatched_total":           true,
	"gonk_intake_dispatch_dropped_total":     true,
	"gonk_intake_gitlab_auth_fallback_total": true,

	"gonk_meter_spend_usd_total":               true,
	"gonk_meter_tokens_total":                  true,
	"gonk_meter_budget_remaining_usd":          true,
	"gonk_meter_budget_remaining_tokens":       true,
	"gonk_meter_policy_decisions_total":        true,
	"gonk_meter_ladder_escalations_total":      true,
	"gonk_meter_gate_outcomes_total":           true,
	"gonk_meter_deferred_beads":                true,
	"gonk_meter_project_state":                 true,
	"gonk_meter_virtual_keys":                  true,
	"gonk_meter_spend_sync_age_seconds":        true,
	"gonk_meter_spend_synced_at_seconds":       true,
	"gonk_meter_spend_sync_failures_total":     true,
	"gonk_meter_spend_rows_unattributed_total": true,
	"gonk_meter_reservations_open":             true,
	"gonk_meter_reservations_expired_total":    true,
	"gonk_meter_clock_skew_seconds":            true,
	"gonk_meter_clock_skew_unknown_total":      true,
	"gonk_meter_budget_window_start_seconds":   true,
	"gonk_meter_cold_start_total":              true,
	"gonk_meter_reservation_races_lost_total":  true,
	"gonk_meter_catalog_drift_total":           true,
}

// metricNameRE finds gonk_*_total/gauge-shaped identifiers plus the bare `up`
// series. It also matches the *_bucket suffix a histogram_quantile() query
// uses over gonk_intake_reconcile_duration_seconds, which is stripped before
// the lookup.
var metricNameRE = regexp.MustCompile(`\b(up|gonk_[a-z_]+)\b`)

func assertOnlyKnownMetrics(t *testing.T, label, text string) {
	t.Helper()
	for _, m := range metricNameRE.FindAllString(text, -1) {
		name := strings.TrimSuffix(strings.TrimSuffix(m, "_bucket"), "_sum")
		name = strings.TrimSuffix(name, "_count")
		if !knownMetrics[name] {
			t.Errorf("%s references metric %q, which internal/meter/metrics and pkg/intake/metrics do not register", label, name)
		}
	}
}

// Every alert expression names a metric the binaries actually export.
func TestAlertsReferenceOnlyRealMetrics(t *testing.T) {
	pr := MustObject(t, Render(t, withMonitoring()...), "PrometheusRule", "gonk")
	assertOnlyKnownMetrics(t, "PrometheusRule/gonk", pr.Doc)
}

// Every dashboard panel expression names a metric the binaries actually export.
func TestDashboardsReferenceOnlyRealMetrics(t *testing.T) {
	out := Render(t, withMonitoring()...)
	for _, name := range []string{"gonk-dashboard-factory", "gonk-dashboard-cost", "gonk-dashboard-health"} {
		cm := MustObject(t, out, "ConfigMap", name)
		for file, raw := range cm.Data(t) {
			var dash struct {
				Panels []struct {
					Title   string `json:"title"`
					Targets []struct {
						Expr string `json:"expr"`
					} `json:"targets"`
				} `json:"panels"`
			}
			if err := json.Unmarshal([]byte(raw), &dash); err != nil {
				t.Fatalf("%s/%s: %v", name, file, err)
			}
			for _, p := range dash.Panels {
				for _, tg := range p.Targets {
					assertOnlyKnownMetrics(t, name+"/"+file+" panel "+p.Title, tg.Expr)
				}
			}
		}
	}
}

// kubeconform, against the vendored CRD schemas -- NOT skipped.
func TestKubeconformMonitoring(t *testing.T) {
	out := Render(t, withMonitoring()...)
	Kubeconform(t, out, filepath.Join(ChartDir(), "tests", "crd-schemas"))
}
