//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// mustFail renders and requires the error message to contain `want`. A guard
// whose message does not say what is wrong is a guard nobody can act on.
func mustFail(t *testing.T, want string, extra ...string) {
	t.Helper()
	out, err := RenderErr(t, append(Minimum(), extra...)...)
	if err == nil {
		t.Fatalf("render SUCCEEDED and must not have (expected %q)", want)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("guard fired with the wrong message.\nwant substring: %q\ngot:\n%s", want, out)
	}
}

// G2: a ladder naming a rung the catalog does not price.
func TestGuardLadderRungNotInCatalog(t *testing.T) {
	mustFail(t, "not in the rung catalog",
		"--set", "operatorConfig.instance.ladder={qwen-local,ghost}")
}

// G2 applies to GROUP ladders too -- they are the layer people forget.
func TestGuardGroupLadderRungNotInCatalog(t *testing.T) {
	mustFail(t, "not in the rung catalog",
		"--set", "operatorConfig.groups.agentic.ladder={ghost}")
}

// G5: the onboarding template's rung must be in the instance ladder, or EVERY
// newly-onboarded project resolves to `disabled` and nothing ever runs.
func TestGuardOnboardingRungNotInInstanceLadder(t *testing.T) {
	mustFail(t, "every newly-onboarded project would resolve to `disabled`",
		"--set", "onboarding.defaultRung=glm",
		"--set", "operatorConfig.rungs[1].name=glm",
		"--set", "operatorConfig.rungs[1].kind=cloud",
		"--set", "operatorConfig.rungs[1].model=glm-5",
		// helm's plain --set never infers float64 (see Minimum()'s own comment);
		// est_cost_usd is a "number" in values.schema.json, so a decimal literal
		// needs --set-json or it arrives as a string and fails schema validation
		// before the guard this test targets ever runs.
		"--set-json", "operatorConfig.rungs[1].est_cost_usd=0.4",
		"--set", "operatorConfig.rungs[1].est_tokens=200K")
}

// RECONCILED, Task 0: the old G6 ("meter past 1 replica quietly doubles a
// budget") and its render-time fail() are DELETED, not implemented. AD-10 is
// lifted (ADR-004) -- meter's reservation atomicity lives in Postgres, across
// replicas, not in an in-process per-project lock. There is no
// meter.unsafeAllowMultipleReplicas value. A plain replicaCount override must
// render cleanly with no guard in the way.
//
// Un-skipped in Task 3: chart/gonk/templates/deployment-gonk-meter.yaml now
// exists, so there is a Deployment/gonk-meter to assert on.
func TestMeterMultipleReplicasRendersWithNoGuard(t *testing.T) {
	out := Render(t, append(Minimum(), "--set", "meter.replicaCount=2")...)
	d := MustObject(t, out, "Deployment", "gonk-meter")
	if !strings.Contains(d.Doc, "replicas: 2") {
		t.Fatalf("meter.replicaCount=2 did not take effect:\n%s", d.Doc)
	}
}

// G8: intake with nowhere to ask about budgets.
func TestGuardIntakeWithoutMeter(t *testing.T) {
	mustFail(t, "dispatch unmetered work", "--set", "meter.enabled=false")
}

// G9/G17: a BYO controller (gascity.enabled=false) with http dispatch and no
// supervisorURL. NOTE: with the controller BUNDLED (the default), the URL is
// DERIVED, so an empty supervisorURL is fine -- see TestBundledGasCityNeedsNoSupervisorURL.
// The guard only fires on the external path.
func TestGuardHTTPDispatchWithoutSupervisorURL(t *testing.T) {
	out, err := RenderErr(t, append(Minimum(),
		"--set", "gascity.enabled=false",
		"--set", "gascity.supervisorURL=")...)
	if err == nil {
		t.Fatal("gascity.enabled=false + dispatch=http with no supervisorURL rendered")
	}
	if !strings.Contains(out, "gascity.supervisorURL") {
		t.Fatalf("wrong message:\n%s", out)
	}
}

func TestLogDispatchIsAllowedWhenDeliberate(t *testing.T) {
	Render(t, append(Minimum(),
		"--set", "gascity.supervisorURL=",
		"--set", "gascity.dispatch=log")...)
}

// G10: a NetworkPolicy with an empty pod selector selects EVERY pod in the
// namespace -- which for an EGRESS policy is a catastrophe, and for the agent
// policy specifically means the spec-9 guarantee silently does not exist.
func TestGuardEmptyAgentPodSelector(t *testing.T) {
	mustFail(t, "agentPodSelector", "--set", "networkPolicy.agentPodSelector=null")
}

// G12. NOTE: values.yaml ships REAL defaults for ingress.host (gonk.orac.local)
// and ingress.className (traefik) -- OD-2, docs/environment.md -- unlike the
// site-local operator config, which ships empty on purpose. So exercising this
// guard means explicitly blanking the field under test, not just leaving it at
// its (non-empty) chart default.
func TestGuardIngressWithoutHost(t *testing.T) {
	mustFail(t, "ingress.host",
		"--set", "ingress.enabled=true", "--set", "ingress.className=nginx", "--set", "ingress.host=")
}

func TestGuardIngressWithoutClassName(t *testing.T) {
	mustFail(t, "ingress.className",
		"--set", "ingress.enabled=true", "--set", "ingress.host=gonk.test", "--set", "ingress.className=")
}

// G16: a disabled Dolt with no external server. Beads is Dolt-only, so this is a
// factory that cannot even store its issues.
func TestGuardDoltDisabledWithoutExternalHost(t *testing.T) {
	mustFail(t, "dolt.external.host",
		"--set", "dolt.enabled=false")
}

func TestExternalDoltIsAllowedWithHost(t *testing.T) {
	Render(t, append(Minimum(),
		"--set", "dolt.enabled=false",
		"--set", "dolt.external.host=dolt.databases.svc")...)
}

// G17: a BYO controller with no address. The bundled path (enabled=true) must NOT
// require a supervisorURL -- it is derived.
func TestGuardBYOGasCityWithoutSupervisorURL(t *testing.T) {
	mustFail(t, "gascity.supervisorURL",
		"--set", "gascity.enabled=false")
}

func TestBundledGasCityNeedsNoSupervisorURL(t *testing.T) {
	// Minimum() sets a supervisorURL for the historical default; clearing it must
	// still render when the controller is bundled, because the URL is derived.
	Render(t, append(Minimum(),
		"--set", "gascity.enabled=true",
		"--set", "gascity.supervisorURL=")...)
}

// G20/G11: the bundled controller needs a pinned image.
func TestGuardBundledGasCityNeedsImageTag(t *testing.T) {
	mustFail(t, "gascity.image.tag",
		"--set", "gascity.image.tag=")
}

// G19, RECONCILED (Task 0): the ledger is always Postgres now, and no
// ledger.postgres.mode derives a DSN on its own -- an empty
// secrets.ledger.existingSecret is fatal regardless of dolt.enabled (beads)
// or which postgres mode is selected.
//
// NOTE: values.schema.json's $defs.secretRef already requires
// existingSecret to be non-empty (minLength 1) for EVERY secretRef,
// including secrets.ledger, and additionalProperties:false means there is no
// --set-json route around it either. So this specific misconfig is caught by
// layer A (the schema) before layer B's (_guards.tpl's) G19 fail() ever runs
// -- exactly the "belt-and-braces" relationship G3/G4 document for the rung
// catalog: the guard is redundant TODAY, and stays in place only in case the
// schema's minLength is ever relaxed. The test below asserts what actually
// fires (the schema), not the plan's illustrative message text.
func TestGuardLedgerNeedsDSNSecret(t *testing.T) {
	mustFail(t, "secrets/ledger/existingSecret",
		"--set", "secrets.ledger.existingSecret=")
}

// The whole point of the default values: they render, and they are SAFE.
func TestDefaultsRenderAndCannotSpend(t *testing.T) {
	cm := MustObject(t, Render(t, Minimum()...), "ConfigMap", "gonk-operator-config")
	oc := cm.Data(t)["operator-config.yaml"]
	if !strings.Contains(oc, "monthly_cost_usd: 0") {
		t.Fatalf("the default instance cost ceiling is not $0:\n%s", oc)
	}
	if strings.Contains(oc, "kind: cloud") {
		t.Fatal("a cloud rung is in the DEFAULT catalog; the default install must not be able to spend")
	}
}

// The operator config the chart renders must be exactly what pkg/opercfg eats.
func TestOperatorConfigMapShape(t *testing.T) {
	cm := MustObject(t, Render(t, Minimum()...), "ConfigMap", "gonk-operator-config")
	oc := cm.Data(t)["operator-config.yaml"]
	for _, want := range []string{
		"version: 1", "instance:", "ladder:", "- qwen-local",
		"rungs:", "name: qwen-local", "kind: local",
		"synthetic_usd_per_1m_tokens: 0.2", "est_tokens: 200K",
		"enforce_ladder_order: true",
	} {
		if !strings.Contains(oc, want) {
			t.Errorf("operator config is missing %q:\n%s", want, oc)
		}
	}
}

// Decision 9: the synthetic prices the operator must ALSO configure in LiteLLM
// come from the SAME catalog, so they cannot drift within the chart. (They can
// still drift from LiteLLM itself -- that is Plan 06's.)
//
// NOTE: the plan's own example used the model name "qwen3-coder-30b", but
// Task 1's Minimum() (the fixture this test must render against, and which
// this task does not own) names its test rung's model "stub-local" -- so this
// assertion matches the SHIPPED fixture, not the plan's illustrative name.
func TestLiteLLMPriceConfigMapMatchesTheCatalog(t *testing.T) {
	out := Render(t, Minimum()...)
	prices := MustObject(t, out, "ConfigMap", "gonk-litellm-model-prices").Data(t)["model-prices.yaml"]
	if !strings.Contains(prices, "stub-local") {
		t.Fatalf("the local model is not in the advisory price list:\n%s", prices)
	}
	// 0.20 USD / 1M tokens == 2e-07 USD/token, in LiteLLM's per-token fields.
	if !strings.Contains(prices, "input_cost_per_token") {
		t.Fatalf("no per-token price:\n%s", prices)
	}
	// A cloud rung must NOT appear: its price is real and lives in LiteLLM already.
	if strings.Contains(prices, "claude") {
		t.Fatal("a cloud model is in the SYNTHETIC price list; that would be a lie")
	}
}
