//go:build chart

package charttest

import (
	"strings"
	"testing"
)

// The chart must lint and render with the default values plus the minimum set of
// required values (image tags, URLs). If this fails, nothing else in this plan
// can be tested.
func TestChartLintsAndRendersWithDefaults(t *testing.T) {
	Lint(t, Minimum()...)
	out := Render(t, Minimum()...)
	objs := Objects(t, out)
	if len(objs) == 0 {
		t.Fatal("chart rendered zero objects")
	}
}

// Helm enforces values.schema.json itself, before any template runs. These are
// the layer-A guards: they are not "loud", they are IMPOSSIBLE.
func TestSchemaRejectsMissingImageTag(t *testing.T) {
	_, err := RenderErr(t, "--set", "intake.image.tag=")
	if err == nil {
		t.Fatal("an empty image tag was accepted")
	}
}

func TestSchemaRejectsLatestTag(t *testing.T) {
	_, err := RenderErr(t, append(Minimum(), "--set", "intake.image.tag=latest")...)
	if err == nil {
		t.Fatal("image tag 'latest' was accepted; a deploy must be reproducible")
	}
}

// G1: the load-bearing one (PLAN.md carry-forward, ADR-002).
func TestSchemaRejectsEmptyInstanceLadder(t *testing.T) {
	_, err := RenderErr(t, append(Minimum(), "--set", "operatorConfig.instance.ladder=null")...)
	if err == nil {
		t.Fatal("an empty instance ladder was accepted; every project could then name any rung")
	}
}

// G15: the memory store loses reservations on restart -- fail-open on headroom.
func TestSchemaRejectsMemoryLedger(t *testing.T) {
	_, err := RenderErr(t, append(Minimum(), "--set", "ledger.backend=memory")...)
	if err == nil {
		t.Fatal("ledger.backend=memory was accepted")
	}
}

// G15, RECONCILED (Task 0): "dolt" is ALSO not a valid ledger.backend any more
// -- it was the chart's own default before Task 0b's spike ran. There is no
// store.OpenDolt for the ledger (cmd/gonk-meter/main.go's openStore has
// exactly one case, "postgres"), so a chart that let this render would produce
// a meter that fails at startup instead of failing at `helm template`.
func TestSchemaRejectsDoltLedger(t *testing.T) {
	_, err := RenderErr(t, append(Minimum(), "--set", "ledger.backend=dolt")...)
	if err == nil {
		t.Fatal("ledger.backend=dolt was accepted; there is no dolt-backed ledger any more (ADR-004 Decision 10)")
	}
}

// The chart ships NO rung catalog and NO instance ladder, and it must FAIL to
// render without an operator-supplied one -- a site-local default naming a model
// the operator's LiteLLM has never heard of is worse than no default (Plan 04
// AD-1; there is no model called `qwen-local`). This is the fail-closed inverse
// of the old "defaults ship a ladder" expectation: pure defaults (no operator
// override) must NOT render.
func TestChartFailsToRenderWithoutOperatorRungCatalogAndLadder(t *testing.T) {
	// Everything Minimum() supplies EXCEPT the operator rung catalog / ladder /
	// defaultRung: ALL FOUR component image tags and the URLs, so the failure is
	// unambiguously the empty operator config (G1/G2/G5), not a missing tag or a
	// component guard.
	base := []string{
		"--set", "intake.image.tag=v0.1.0",
		"--set", "meter.image.tag=v0.1.0",
		"--set", "gascity.image.tag=v0.1.0",
		"--set", "dolt.image.tag=v1.43.0",
		"--set", "intake.webhookPublicURL=https://gonk.example.test/hook/gitlab",
		"--set", "litellm.externalURL=http://litellm.litellm.svc:4000",
	}
	if _, err := RenderErr(t, base...); err == nil {
		t.Fatal("chart rendered with an EMPTY rung catalog and instance ladder; it must fail closed (a phantom-model default 404s at first token)")
	}
	// And with a full operator config (Minimum), it renders and the ladder is present.
	// NOTE (Task 1): the operator-config ConfigMap itself is Task 2's; until it
	// exists this second half is skipped rather than faked.
	t.Skip("Task 2: gonk-operator-config ConfigMap does not exist yet")
	cm := MustObject(t, Render(t, Minimum()...), "ConfigMap", "gonk-operator-config")
	oc := cm.Data(t)["operator-config.yaml"]
	if !strings.Contains(oc, "ladder:") || !strings.Contains(oc, "qwen-local") {
		t.Fatalf("operator-supplied ladder did not reach the rendered config:\n%s", oc)
	}
}
