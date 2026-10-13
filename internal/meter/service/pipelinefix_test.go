package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/rung"
)

// gonk-2usw part CONFIG, end to end through the real service: registration,
// /decide, the reservation.

const fixYAMLHead = "version: 1\nenabled: true\nladder: [qwen-local, glm, sonnet]\nbudget: { monthly_cost_usd: 50 }\n"

// PLAN, must be seen red: a registration REFUSING the bad config. Each of
// these is a .gonk.yml a project could plausibly write; each must leave the
// project invalid, keyless, and unable to get a reservation -- for ANY
// trigger, so a broken pipeline_fix section cannot be half-applied.
func TestInvalidPipelineFixConfigIsRefusedAtRegistration(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"pipeline_fixes without pipelines", "actions: { triage: true, pipeline_fixes: true }\n", "pipeline"},
		{"pipeline_fixes with pipelines false", "actions: { triage: true, pipelines: false, pipeline_fixes: true }\n", "pipeline"},
		{"unknown key in pipeline_fix", "actions: { triage: true, pipelines: true, pipeline_fixes: true }\npipeline_fix: { banana: true }\n", "pipeline_fix"},
		{"unknown key in auto_merge", "actions: { triage: true }\npipeline_fix: { auto_merge: { approve: true } }\n", "auto_merge"},
		{"a class no config may enable", "actions: { triage: true }\npipeline_fix: { classes: [flaky] }\n", "classes"},
		{"above a ceiling", "actions: { triage: true }\npipeline_fix: { max_changed_lines: 400 }\n", "max_changed_lines"},
		{"verify_timeout above its ceiling (schema-valid)", "actions: { triage: true }\npipeline_fix: { verify_timeout: 24h }\n", "verify_timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestService(t)
			// A good config first, so there is a live key and a possible run to lose.
			f.register("group/repo", "group-repo", simpleYAML("qwen-local", 5))
			f.syncOnce()
			if d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-ok", "sess-ok")); err != nil || d.Kind != rung.Run {
				t.Fatalf("setup: decide on the valid config = %+v, %v; want run", d, err)
			}

			resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
				Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: fixYAMLHead + c.body,
			})
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if status != 422 || resp.State != meterapi.StateInvalid {
				t.Fatalf("status %d state %q, want 422 invalid", status, resp.State)
			}
			if !strings.Contains(resp.Error, c.want) {
				t.Errorf("Error = %q, want it to mention %q", resp.Error, c.want)
			}
			if resp.Effective != nil {
				t.Error("an invalid pipeline-fix config left an Effective config")
			}
			if _, ok := f.admin.Keys["gonk-group-repo"]; ok {
				t.Error("the virtual key survived an invalid config -- the project can still spend")
			}
			for _, trig := range []string{atags.TriggerPipelineFix, atags.TriggerPipelineDiagnosis, atags.TriggerIssueTriage} {
				req := decideReq("group/repo", "gk-new-"+trig, "sess-new-"+trig)
				req.Trigger = trig
				d, extras, err := f.svc.Decide(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if d.Kind != rung.Deny || d.Reason != rung.ReasonInvalidConfig {
					t.Errorf("%s: decision = %+v, want deny/%s", trig, d, rung.ReasonInvalidConfig)
				}
				if extras.Reservation.ID != "" {
					t.Errorf("%s: a project with an invalid config got a reservation", trig)
				}
			}
		})
	}
}

// PLAN, must be seen red: "trigger not gated by the action". Through /decide:
// a project that diagnoses pipelines but did not opt in to fixes is denied the
// pipeline-fix trigger and gets no reservation; the same project still gets
// its diagnosis; and opting in is what changes the answer.
func TestPipelineFixTriggerIsGatedByItsAction(t *testing.T) {
	f := newTestService(t)
	f.register("diag/only", "diag-only", fixYAMLHead+"actions: { triage: true, pipelines: true }\n")
	f.register("fix/on", "fix-on", fixYAMLHead+"actions: { pipelines: true, pipeline_fixes: true }\n")
	f.syncOnce()

	decide := func(project, trig string) (rung.Decision, DecideExtras) {
		t.Helper()
		req := decideReq(project, "gk-"+trig, "sess-"+trig)
		req.Trigger = trig
		d, extras, err := f.svc.Decide(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return d, extras
	}

	d, extras := decide("diag/only", atags.TriggerPipelineFix)
	if d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed {
		t.Fatalf("diagnose-only project, pipeline-fix: %+v, want deny/%s", d, rung.ReasonActionNotAllowed)
	}
	if extras.Reservation.ID != "" || len(extras.Metadata) != 0 {
		t.Fatalf("a denied pipeline-fix got a reservation or tags: %+v", extras)
	}
	if d, _ := decide("diag/only", atags.TriggerPipelineDiagnosis); d.Kind != rung.Run {
		t.Fatalf("diagnose-only project lost its diagnosis: %+v", d)
	}

	d, extras = decide("fix/on", atags.TriggerPipelineFix)
	// Floor 1: the first attempt runs the SECOND rung of [qwen-local, glm, sonnet].
	if d.Kind != rung.Run || d.Rung != "glm" || d.Attempt != 1 {
		t.Fatalf("opted-in project, pipeline-fix: %+v, want run on glm attempt 1", d)
	}
	if got := extras.Metadata[atags.KeyTrigger]; got != "pipeline-fix" {
		t.Fatalf("minted trigger tag = %q, want the literal pipeline-fix", got)
	}
	// And triage, which this project never enabled, is still refused.
	if d, _ := decide("fix/on", atags.TriggerIssueTriage); d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed {
		t.Fatalf("pipeline_fixes opened issue-triage: %+v", d)
	}
}

// An operator veto reaches /decide: the instance says pipeline_fixes: false,
// the project opts in, the trigger is denied.
func TestOperatorVetoOfPipelineFixesDeniesTheTrigger(t *testing.T) {
	f := newTestServiceWithConfig(t, strings.Replace(testOperatorYAML,
		"instance:\n  enabled: true\n", "instance:\n  enabled: true\n  actions: { pipeline_fixes: false }\n", 1))
	resp := f.register("fix/vetoed", "fix-vetoed", fixYAMLHead+"actions: { pipelines: true, pipeline_fixes: true }\n")
	if resp.Effective == nil || resp.Effective.Actions.PipelineFixes || !resp.Effective.Actions.Pipelines {
		t.Fatalf("effective actions = %+v, want pipelines on and pipeline_fixes vetoed", resp.Effective)
	}
	f.syncOnce()
	req := decideReq("fix/vetoed", "gk-fix", "sess-fix")
	req.Trigger = atags.TriggerPipelineFix
	d, extras, err := f.svc.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed || extras.Reservation.ID != "" {
		t.Fatalf("decision = %+v reservation %q, want deny/%s and none", d, extras.Reservation.ID, rung.ReasonActionNotAllowed)
	}
}

// The per-trigger reservation TTL (plan 3.4, 7.3), observed on the reservation
// the service actually wrote: reservation_ttl (60m in the test operator
// config) + the project's verify_timeout for pipeline-fix, and reservation_ttl
// alone for every other trigger of the SAME project.
func TestPipelineFixReservationOutlivesTheVerifyTimeout(t *testing.T) {
	for _, c := range []struct {
		name, fix string
		want      time.Duration
	}{
		{"default verify_timeout", "", 3 * time.Hour},
		{"verify_timeout 30m", "pipeline_fix: { verify_timeout: 30m }\n", 90 * time.Minute},
		{"verify_timeout at the ceiling", "pipeline_fix: { verify_timeout: 6h }\n", 7 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestService(t)
			f.register("fix/ttl", "fix-ttl", fixYAMLHead+"actions: { triage: true, pipelines: true, pipeline_fixes: true }\n"+c.fix)
			f.syncOnce()
			ttl := func(trig string) time.Duration {
				t.Helper()
				req := decideReq("fix/ttl", "gk-"+trig, "sess-"+trig)
				req.Trigger = trig
				d, extras, err := f.svc.Decide(context.Background(), req)
				if err != nil || d.Kind != rung.Run {
					t.Fatalf("%s: decide = %+v, %v; want run", trig, d, err)
				}
				return extras.Reservation.ExpiresAt.Sub(extras.Reservation.CreatedAt)
			}
			if got := ttl(atags.TriggerPipelineFix); got != c.want {
				t.Errorf("pipeline-fix reservation TTL = %s, want %s", got, c.want)
			}
			for _, trig := range []string{atags.TriggerPipelineDiagnosis, atags.TriggerIssueTriage} {
				if got := ttl(trig); got != time.Hour {
					t.Errorf("%s reservation TTL = %s, want the operator's 1h0m0s, unchanged", trig, got)
				}
			}
		})
	}
}

// Default OFF, on the wire: a project that never mentions the feature
// registers exactly as before -- pipeline_fixes false, the defaults in
// pipeline_fix -- and one that opts in sees its own values folded with the
// operator's.
func TestRegisterReportsThePipelineFixConfig(t *testing.T) {
	f := newTestServiceWithConfig(t, strings.Replace(testOperatorYAML,
		"  kubernetes:\n", "  kubernetes:\n    pipeline_fix: { max_chain: 2, classes: [code, configuration] }\n", 1))
	plain := f.register("plain/repo", "plain-repo", simpleYAML("qwen-local", 5))
	if plain.Effective == nil {
		t.Fatal("no Effective")
	}
	raw, err := json.Marshal(plain.Effective.PipelineFix)
	if err != nil {
		t.Fatal(err)
	}
	const defaults = `{"classes":["code","test"],"targets":["ref"],"allow_ci":false,"ci_exec_changes":"manual-run","max_attempts":2,"max_chain":5,` +
		`"verify_timeout_seconds":7200,"max_files":3,"max_changed_lines":40,"paths":[],"auto_merge":{"mode":"off","classes":["docs"],"max_per_day":3}}`
	if string(raw) != defaults || plain.Effective.Actions.PipelineFixes {
		t.Errorf("a project that never opted in: pipeline_fixes=%v\n got  %s\n want %s", plain.Effective.Actions.PipelineFixes, raw, defaults)
	}

	opted := f.register("kubernetes/kubectl", "kubernetes-kubectl", fixYAMLHead+"actions: { pipelines: true, pipeline_fixes: true }\n"+
		"pipeline_fix:\n  classes: [code, test, configuration]\n  targets: [ref, mr]\n  allow_ci: true\n  ci_exec_changes: deny\n"+
		"  max_attempts: 3\n  max_chain: 9\n  verify_timeout: 45m\n  max_files: 2\n  max_changed_lines: 12\n  paths: [\"internal/**\"]\n"+
		"  auto_merge: { mode: after-human-approval, classes: [docs, ci] }\n")
	if opted.Effective == nil {
		t.Fatal("no Effective")
	}
	raw, err = json.Marshal(opted.Effective.PipelineFix)
	if err != nil {
		t.Fatal(err)
	}
	// classes lost `test` and max_chain fell to 2: the group's tightening.
	const want = `{"classes":["code","configuration"],"targets":["ref","mr"],"allow_ci":true,"ci_exec_changes":"deny","max_attempts":3,"max_chain":2,` +
		`"verify_timeout_seconds":2700,"max_files":2,"max_changed_lines":12,"paths":["internal/**"],"auto_merge":{"mode":"after-human-approval","classes":["docs","ci"],"max_per_day":3}}`
	if string(raw) != want || !opted.Effective.Actions.PipelineFixes {
		t.Errorf("an opted-in project under a tightening group: pipeline_fixes=%v\n got  %s\n want %s", opted.Effective.Actions.PipelineFixes, raw, want)
	}
}
