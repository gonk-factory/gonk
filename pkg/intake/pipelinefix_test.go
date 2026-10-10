package intake

import (
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-2usw part CONFIG: intake's pre-filter for the pipeline-fix trigger
// (plan 3.2 row E1). It must be a SUBSET of meter's rule -- pipeline_fixes
// AND pipelines -- and must not be opened by any other action.
func TestMayFixPipelines(t *testing.T) {
	withActions := func(a meterapi.Actions) func(*meterapi.ProjectResponse) {
		return func(r *meterapi.ProjectResponse) { r.Effective.Actions = a }
	}
	for _, c := range []struct {
		name string
		cls  Classification
		want bool
	}{
		{"both on", Classify(obs(nil), active(withActions(meterapi.Actions{Pipelines: true, PipelineFixes: true}))), true},
		{"both on, no .agent/ yet (pending)", Classify(obs(func(o *Observation) { o.AgentDirPresent = false }),
			active(withActions(meterapi.Actions{Pipelines: true, PipelineFixes: true}))), true},
		{"pipelines only", Classify(obs(nil), active(withActions(meterapi.Actions{Pipelines: true}))), false},
		{"fixes without pipelines (a meter that predates the conjunction)", Classify(obs(nil), active(withActions(meterapi.Actions{PipelineFixes: true}))), false},
		{"every OTHER action on", Classify(obs(nil), active(withActions(meterapi.Actions{Triage: true, Pipelines: true, Features: true, Scaffold: true}))), false},
		{"triage only (the onboarding default)", Classify(obs(nil), active(nil)), false},
		{"meter unreachable", Classify(obs(nil), nil), false},
		{"not a member", Classify(obs(func(o *Observation) { o.Member = false }),
			active(withActions(meterapi.Actions{Pipelines: true, PipelineFixes: true}))), false},
		{"disabled by meter, actions still reported on", Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
			r.State, r.DisabledReason = meterapi.StateDisabled, "disabled by group policy"
			r.Effective.Actions = meterapi.Actions{Pipelines: true, PipelineFixes: true}
		})), false},
		{"key missing, actions on", Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
			r.State = meterapi.StateKeyMissing
			r.Effective.Actions = meterapi.Actions{Pipelines: true, PipelineFixes: true}
		})), false},
		{"no config", Classify(obs(func(o *Observation) { o.ConfigBytes = nil }), nil), false},
	} {
		if got := c.cls.MayFixPipelines(); got != c.want {
			t.Errorf("%s: MayFixPipelines = %v, want %v (state %q)", c.name, got, c.want, c.cls.State)
		}
		// mayRun is the per-trigger switch gate() uses; it must agree.
		if got := mayRun(c.cls, atags.TriggerPipelineFix); got != c.want {
			t.Errorf("%s: mayRun(pipeline-fix) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The gate, end to end: a project that only diagnoses is refused the
// pipeline-fix trigger by name, and one that opted in passes.
func TestGateRefusesPipelineFixWithoutItsAction(t *testing.T) {
	diagnoseOnly := pipelinesEntry(42)
	if d, ok := gate(diagnoseOnly, atags.TriggerPipelineFix); ok || d.Reason != "action_disabled" {
		t.Fatalf("a diagnose-only project passed the pipeline-fix gate: ok=%v %+v", ok, d)
	}
	if _, ok := gate(diagnoseOnly, atags.TriggerPipelineDiagnosis); !ok {
		t.Fatal("a diagnose-only project no longer passes the pipeline-diagnosis gate")
	}
	if d, ok := gate(triageOnlyEntry(42), atags.TriggerPipelineFix); ok || d.Reason != "action_disabled" {
		t.Fatalf("a triage-only project passed the pipeline-fix gate: ok=%v %+v", ok, d)
	}
	optedIn := pipelinesEntry(42)
	optedIn.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions = meterapi.Actions{Pipelines: true, PipelineFixes: true}
	}))
	if d, ok := gate(optedIn, atags.TriggerPipelineFix); !ok {
		t.Fatalf("an opted-in project was refused the pipeline-fix gate: %+v", d)
	}
}

// The onboarding .gonk.yml leaves pipeline fixes OFF and says so on its own
// line; and the merge request explains the action and every pipeline_fix key
// (TestMRBodyDocumentsEveryConfigKey is the general gate; this pins the two
// statements a maintainer must not be misled about).
func TestOnboardingLeavesPipelineFixesOff(t *testing.T) {
	raw, err := RenderDefaultConfig([]string{"qwen-local"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  pipeline_fixes: false ") {
		t.Errorf("the onboarding .gonk.yml does not state pipeline_fixes: false:\n%s", raw)
	}
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Actions.PipelineFixes == nil || *cfg.Actions.PipelineFixes || !cfg.PipelineFix.IsZero() {
		t.Errorf("onboarding config: pipeline_fixes = %v, pipeline_fix = %+v; want explicit false and no section", cfg.Actions.PipelineFixes, cfg.PipelineFix)
	}
	eff := gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *cfg)
	if eff.Actions.PipelineFixes || eff.PipelineFix.AllowCI || eff.PipelineFix.AutoMerge.Mode != gonkcfg.AutoMergeOff {
		t.Errorf("merging the onboarding config would authorize a pipeline fix: %+v / %+v", eff.Actions, eff.PipelineFix)
	}

	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0", Ladder: []string{"qwen-local"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| `actions.pipeline_fixes` | `false` |",
		"it needs `actions.pipelines: true`",
		"refused as a configuration error",
		"| `pipeline_fix.allow_ci` | unset | Optional, default `false`.",
		"| `pipeline_fix.ci_exec_changes` | unset | Optional, default `manual-run`",
		"| `pipeline_fix.targets` | unset |",
		"Unset means `[ref]`",
		"| `pipeline_fix.auto_merge` | unset | Optional, and off unless you set `mode`.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the onboarding merge request does not say %q", want)
		}
	}
}
