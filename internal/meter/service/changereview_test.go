package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/rung"
)

// gonk-cuwn part X-CONFIG, end to end through the real service: registration,
// /decide, the reservation, the project response.

const crYAMLHead = "version: 1\nenabled: true\nladder: [qwen-local, glm, sonnet]\nbudget: { monthly_cost_usd: 50 }\n"

// withReviewRung is the test operator config with an instance reviewer rung.
func withReviewRung(rungName string) string {
	return strings.Replace(testOperatorYAML, "instance:\n  enabled: true\n", "instance:\n  enabled: true\n  review: { rung: "+rungName+" }\n", 1)
}

// PLAN, must be seen red: a registration REFUSING each bad config. Each of
// these is a .gonk.yml a project could plausibly write; each must leave the
// project invalid, keyless, and unable to get a reservation for ANY trigger.
func TestInvalidChangeOrReviewConfigIsRefusedAtRegistration(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		// "unknown key accepted"
		{"unknown key in change", "actions: { triage: true, features: true }\nchange: { banana: true }\n", "change"},
		{"unknown key in change.auto_merge", "actions: { triage: true, features: true }\nchange: { auto_merge: { approve: true } }\n", "auto_merge"},
		{"unknown key in review", "actions: { triage: true, features: true, reviews: true }\nreview: { threads: true }\n", "review"},
		{"unknown action", "actions: { triage: true, features: true, review: true }\n", "actions"},
		// "`reviews` on without its opt-in dependency accepted"
		// (the schema's message names the two actions it wanted, at /actions)
		{"reviews without an author workflow", "actions: { triage: true, pipelines: true, reviews: true }\n", "missing property 'pipeline_fixes'"},
		{"reviews with both author workflows false", "actions: { triage: true, pipeline_fixes: false, features: false, reviews: true }\n", "'/actions/features': value must be true"},
		{"change auto-merge without features", "actions: { triage: true }\nchange: { auto_merge: { mode: after-human-approval } }\n", "features"},
		{"change agentic auto-merge without reviews", "actions: { triage: true, features: true }\nchange: { auto_merge: { mode: after-agentic-review } }\n", "reviews"},
		// "a ceiling exceeded is accepted"
		{"change.max_files above its ceiling", "actions: { triage: true, features: true }\nchange: { max_files: 9 }\n", "max_files"},
		{"change.max_changed_lines above its ceiling", "actions: { triage: true, features: true }\nchange: { max_changed_lines: 301 }\n", "max_changed_lines"},
		{"change.max_attempts above its ceiling", "actions: { triage: true, features: true }\nchange: { max_attempts: 4 }\n", "max_attempts"},
		{"change.max_open above its ceiling", "actions: { triage: true, features: true }\nchange: { max_open: 4 }\n", "max_open"},
		{"change.verify_timeout above its ceiling (schema-valid)", "actions: { triage: true, features: true }\nchange: { verify_timeout: 24h }\n", "verify_timeout"},
		{"change.auto_merge.max_per_day above its ceiling", "actions: { triage: true, features: true }\nchange: { auto_merge: { max_per_day: 4 } }\n", "max_per_day"},
		{"pipeline_fix.auto_merge.max_per_day above its ceiling", "actions: { triage: true }\npipeline_fix: { auto_merge: { max_per_day: 4 } }\n", "max_per_day"},
		{"review.max_notes above its ceiling", "actions: { triage: true, features: true, reviews: true }\nreview: { max_notes: 21 }\n", "max_notes"},
		{"review.max_reviews_per_mr above its ceiling", "actions: { triage: true, features: true, reviews: true }\nreview: { max_reviews_per_mr: 4 }\n", "max_reviews_per_mr"},
		// "`trigger: triage-verdict` is the default or accepted"
		{"trigger triage-verdict", "actions: { triage: true, features: true }\nchange: { triggers: [triage-verdict] }\n", "triggers"},
		{"trigger triage-verdict beside label", "actions: { triage: true, features: true }\nchange: { triggers: [label, triage-verdict] }\n", "triggers"},
		{"the plan's singular trigger key", "actions: { triage: true, features: true }\nchange: { trigger: triage-verdict }\n", "change"},
		// "a project sets `review.rung`"
		{"a project sets review.rung", "actions: { triage: true, features: true, reviews: true }\nreview: { rung: qwen-local }\n", "review"},
		// owner decisions
		{"a project tries to waive the distinct model", "actions: { triage: true, features: true, reviews: true }\nreview: { require_distinct_model: false }\n", "review"},
		{"review target human", "actions: { triage: true, features: true, reviews: true }\nreview: { targets: [human] }\n", "targets"},
		{"auto-merge class source", "actions: { triage: true, features: true }\nchange: { auto_merge: { classes: [source] } }\n", "classes"},
		{"auto-merge class ci for a change", "actions: { triage: true, features: true }\nchange: { auto_merge: { classes: [ci] } }\n", "classes"},
		{"create class deps", "actions: { triage: true, features: true }\nchange: { create_classes: [deps] }\n", "create_classes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTestServiceWithConfig(t, withReviewRung("glm"))
			// A good config first, so there is a live key and a possible run to lose.
			f.register("group/repo", "group-repo", simpleYAML("qwen-local", 5))
			f.syncOnce()
			if d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-ok", "sess-ok")); err != nil || d.Kind != rung.Run {
				t.Fatalf("setup: decide on the valid config = %+v, %v; want run", d, err)
			}

			resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
				Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: crYAMLHead + c.body,
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
				t.Error("an invalid change/review config left an Effective config")
			}
			if _, ok := f.admin.Keys["gonk-group-repo"]; ok {
				t.Error("the virtual key survived an invalid config -- the project can still spend")
			}
			for _, trig := range []string{atags.TriggerIssueChange, atags.TriggerMRReview, atags.TriggerPipelineFix, atags.TriggerIssueTriage} {
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

// And the valid neighbours of those register cleanly: the refusals above are
// not "everything with a change: section is refused".
func TestValidChangeAndReviewConfigsRegister(t *testing.T) {
	for name, body := range map[string]string{
		"change section, defaults spelled out": "actions: { features: true }\nchange: { triggers: [label, command, mention], create_classes: [test, docs], require_docs: when-labelled, max_attempts: 2, max_open: 1, verify_timeout: 2h, max_files: 6, max_changed_lines: 200 }\n",
		"change at its ceilings":               "actions: { features: true }\nchange: { create_classes: [source, test, docs], max_attempts: 3, max_open: 3, verify_timeout: 6h, max_files: 8, max_changed_lines: 300, auto_merge: { mode: after-human-approval, max_per_day: 3 } }\n",
		"reviews with features":                "actions: { features: true, reviews: true }\nreview: { targets: [gonk], max_notes: 20, max_reviews_per_mr: 3 }\n",
		"agentic merge with reviews":           "actions: { features: true, reviews: true }\nchange: { auto_merge: { mode: after-agentic-review } }\n",
		"a staged change section":              "actions: { triage: true }\nchange: { triggers: [label], auto_merge: { mode: \"off\" } }\n",
	} {
		f := newTestServiceWithConfig(t, withReviewRung("glm"))
		resp := f.register("group/repo", "group-repo", crYAMLHead+body)
		if resp.State != meterapi.StateActive || resp.Effective == nil {
			t.Errorf("%s: state %q, want active", name, resp.State)
		}
	}
}

// PLAN, must be seen red: "`reviews` reviewed without opt-in" and the trigger
// gates. Through /decide: issue-change needs actions.features, mr-review needs
// actions.reviews, neither opens the other, and mr-review runs on the
// OPERATOR'S reviewer rung, not on the project's ladder.
func TestChangeAndReviewTriggersAreGatedByTheirActions(t *testing.T) {
	f := newTestServiceWithConfig(t, withReviewRung("sonnet"))
	f.register("triage/only", "triage-only", crYAMLHead+"actions: { triage: true, pipelines: true, pipeline_fixes: true }\n")
	f.register("change/on", "change-on", crYAMLHead+"actions: { features: true }\n")
	f.register("review/on", "review-on", crYAMLHead+"actions: { pipelines: true, pipeline_fixes: true, reviews: true }\n")
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
	denied := func(project, trig string) {
		t.Helper()
		d, extras := decide(project, trig)
		if d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed {
			t.Errorf("%s / %s: %+v, want deny/%s", project, trig, d, rung.ReasonActionNotAllowed)
		}
		if extras.Reservation.ID != "" || len(extras.Metadata) != 0 {
			t.Errorf("%s / %s: a denied trigger got a reservation or tags: %+v", project, trig, extras)
		}
	}
	denied("triage/only", atags.TriggerIssueChange)
	denied("triage/only", atags.TriggerMRReview)
	denied("change/on", atags.TriggerMRReview)
	denied("change/on", atags.TriggerIssueTriage)
	denied("review/on", atags.TriggerIssueChange)

	// Floor 1: the first attempt runs the SECOND rung of [qwen-local, glm, sonnet].
	d, extras := decide("change/on", atags.TriggerIssueChange)
	if d.Kind != rung.Run || d.Rung != "glm" || d.Attempt != 1 {
		t.Fatalf("opted-in project, issue-change: %+v, want run on glm attempt 1", d)
	}
	if got := extras.Metadata[atags.KeyTrigger]; got != "issue-change" {
		t.Fatalf("minted trigger tag = %q, want the literal issue-change", got)
	}
	// The reviewer rung is the operator's: sonnet, not the project's floor.
	d, extras = decide("review/on", atags.TriggerMRReview)
	if d.Kind != rung.Run || d.Rung != "sonnet" || d.Attempt != 1 {
		t.Fatalf("opted-in project, mr-review: %+v, want run on the operator's reviewer rung sonnet", d)
	}
	if got := extras.Metadata[atags.KeyTrigger]; got != "mr-review" {
		t.Fatalf("minted trigger tag = %q, want the literal mr-review", got)
	}
	if ttl := extras.Reservation.ExpiresAt.Sub(extras.Reservation.CreatedAt); ttl != time.Hour {
		t.Errorf("mr-review reservation TTL = %s, want the operator's 1h0m0s", ttl)
	}
}

// With no operator reviewer rung, a project that opted in to reviews gets no
// review: the operator has declared no model adequate.
func TestMRReviewIsDeniedWithoutAnOperatorReviewerRung(t *testing.T) {
	f := newTestService(t)
	resp := f.register("review/on", "review-on", crYAMLHead+"actions: { pipelines: true, pipeline_fixes: true, reviews: true }\n")
	if resp.Effective == nil || !resp.Effective.Actions.Reviews || resp.Effective.Review.Rung != "" {
		t.Fatalf("effective = %+v, want reviews on and no rung", resp.Effective)
	}
	f.syncOnce()
	req := decideReq("review/on", "gk-rv", "sess-rv")
	req.Trigger = atags.TriggerMRReview
	d, extras, err := f.svc.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonLadderExhausted || !strings.Contains(d.Detail, "review.rung") || extras.Reservation.ID != "" {
		t.Fatalf("decision = %+v reservation %q, want deny/%s naming review.rung and no reservation", d, extras.Reservation.ID, rung.ReasonLadderExhausted)
	}
}

// An operator veto reaches /decide.
func TestOperatorVetoOfReviewsDeniesTheTrigger(t *testing.T) {
	f := newTestServiceWithConfig(t, strings.Replace(withReviewRung("glm"),
		"instance:\n  enabled: true\n", "instance:\n  enabled: true\n  actions: { reviews: false }\n", 1))
	resp := f.register("review/vetoed", "review-vetoed", crYAMLHead+"actions: { features: true, reviews: true }\n")
	if resp.Effective == nil || resp.Effective.Actions.Reviews || !resp.Effective.Actions.Features {
		t.Fatalf("effective actions = %+v, want features on and reviews vetoed", resp.Effective)
	}
	if st := resp.Effective.Review.AgenticMerge.Change; st.Available || st.Reason != "reviews-off" {
		t.Errorf("agentic merge status under a veto = %+v, want unavailable/reviews-off", st)
	}
	f.syncOnce()
	req := decideReq("review/vetoed", "gk-rv", "sess-rv")
	req.Trigger = atags.TriggerMRReview
	d, extras, err := f.svc.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonActionNotAllowed || extras.Reservation.ID != "" {
		t.Fatalf("decision = %+v reservation %q, want deny/%s and none", d, extras.Reservation.ID, rung.ReasonActionNotAllowed)
	}
}

// The per-trigger reservation TTL, observed on the reservation the service
// actually wrote: reservation_ttl (60m) + the project's change.verify_timeout
// for issue-change, and reservation_ttl alone for every other trigger of the
// SAME project -- including pipeline-fix's own, different, timeout.
func TestIssueChangeReservationOutlivesTheVerifyTimeout(t *testing.T) {
	for _, c := range []struct {
		name, change string
		want         time.Duration
	}{
		{"default verify_timeout", "", 3 * time.Hour},
		{"verify_timeout 30m", "change: { verify_timeout: 30m }\n", 90 * time.Minute},
		{"verify_timeout at the ceiling", "change: { verify_timeout: 6h }\n", 7 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestServiceWithConfig(t, withReviewRung("glm"))
			f.register("change/ttl", "change-ttl", crYAMLHead+"actions: { triage: true, pipelines: true, pipeline_fixes: true, features: true, reviews: true }\n"+
				"pipeline_fix: { verify_timeout: 1h }\n"+c.change)
			f.syncOnce()
			ttl := func(trig string) time.Duration {
				t.Helper()
				req := decideReq("change/ttl", "gk-"+trig, "sess-"+trig)
				req.Trigger = trig
				d, extras, err := f.svc.Decide(context.Background(), req)
				if err != nil || d.Kind != rung.Run {
					t.Fatalf("%s: decide = %+v, %v; want run", trig, d, err)
				}
				return extras.Reservation.ExpiresAt.Sub(extras.Reservation.CreatedAt)
			}
			if got := ttl(atags.TriggerIssueChange); got != c.want {
				t.Errorf("issue-change reservation TTL = %s, want %s", got, c.want)
			}
			if got := ttl(atags.TriggerPipelineFix); got != 2*time.Hour {
				t.Errorf("pipeline-fix reservation TTL = %s, want 2h0m0s (its own verify_timeout)", got)
			}
			for _, trig := range []string{atags.TriggerMRReview, atags.TriggerPipelineDiagnosis, atags.TriggerIssueTriage} {
				if got := ttl(trig); got != time.Hour {
					t.Errorf("%s reservation TTL = %s, want the operator's 1h0m0s, unchanged", trig, got)
				}
			}
		})
	}
}

// Default OFF on the wire, and an opted-in project sees its own values folded
// with the operator's -- read back with GET as well, so the values are what
// the store holds, not only what Register computed.
func TestRegisterReportsTheChangeAndReviewConfig(t *testing.T) {
	op := strings.Replace(withReviewRung("glm"),
		"  kubernetes:\n", "  kubernetes:\n    change: { max_open: 1, triggers: [label, command], docs_labels: [api], require_test_report: true }\n    review: { max_notes: 5 }\n", 1)
	f := newTestServiceWithConfig(t, op)
	plain := f.register("plain/repo", "plain-repo", simpleYAML("qwen-local", 5))
	if plain.Effective == nil {
		t.Fatal("no Effective")
	}
	const changeDefaults = `{"triggers":["label"],"create_classes":["test","docs"],"require_docs":"when-labelled",` +
		`"docs_labels":["feature","enhancement","type::feature"],"require_test_report":false,"max_attempts":2,"max_open":1,"verify_timeout_seconds":7200,` +
		`"max_files":6,"max_changed_lines":200,"paths":[],"auto_merge":{"mode":"off","classes":["docs"],"max_per_day":3}}`
	const reviewDefaults = `{"targets":["gonk"],"rung":"glm","max_notes":20,"max_reviews_per_mr":3,` +
		`"agentic_merge":{"pipeline_fix":{"available":false,"reason":"reviews-off"},"change":{"available":false,"reason":"reviews-off"}}}`
	check := func(who string, e *meterapi.Effective, wantChange, wantReview string, wantReviews bool) {
		t.Helper()
		c, _ := json.Marshal(e.Change)
		r, _ := json.Marshal(e.Review)
		if string(c) != wantChange {
			t.Errorf("%s: change\n got  %s\n want %s", who, c, wantChange)
		}
		if string(r) != wantReview {
			t.Errorf("%s: review\n got  %s\n want %s", who, r, wantReview)
		}
		if e.Actions.Reviews != wantReviews {
			t.Errorf("%s: actions.reviews = %v, want %v", who, e.Actions.Reviews, wantReviews)
		}
	}
	check("a project that never opted in", plain.Effective, changeDefaults, reviewDefaults, false)

	opted := f.register("kubernetes/kubectl", "kubernetes-kubectl", crYAMLHead+"actions: { features: true, reviews: true }\n"+
		"change:\n  triggers: [label, mention]\n  create_classes: [source, test]\n  require_docs: never\n  docs_labels: [ux]\n"+
		"  max_attempts: 3\n  max_open: 3\n  verify_timeout: 45m\n  max_files: 4\n  max_changed_lines: 120\n  paths: [\"internal/**\"]\n"+
		"  auto_merge: { mode: after-agentic-review, classes: [docs], max_per_day: 1 }\n"+
		"review: { max_notes: 9, max_reviews_per_mr: 2 }\n")
	// The group removed `mention`, added a docs label, forced the test report,
	// capped max_open at 1 and max_notes at 5. The reviewer (glm) is one of
	// the author's rungs but not the only one (sonnet): available.
	const wantChange = `{"triggers":["label"],"create_classes":["source","test"],"require_docs":"never",` +
		`"docs_labels":["ux","api"],"require_test_report":true,"max_attempts":3,"max_open":1,"verify_timeout_seconds":2700,` +
		`"max_files":4,"max_changed_lines":120,"paths":["internal/**"],"auto_merge":{"mode":"after-agentic-review","classes":["docs"],"max_per_day":1}}`
	const wantReview = `{"targets":["gonk"],"rung":"glm","max_notes":5,"max_reviews_per_mr":2,` +
		`"agentic_merge":{"pipeline_fix":{"available":true,"reason":""},"change":{"available":true,"reason":""}}}`
	check("an opted-in project under a tightening group", opted.Effective, wantChange, wantReview, true)

	got, found, err := f.svc.Get(context.Background(), "kubernetes/kubectl")
	if err != nil || !found || got.Effective == nil {
		t.Fatalf("GetProject: %+v, %v, %v", got, found, err)
	}
	check("the same project, read back", got.Effective, wantChange, wantReview, true)
}

// Owner decision Q4, end to end: the status the meter reports for the
// agentic-review merge mode under real operator configurations, and that an
// operator change is picked up on reresolve.
func TestRegisterReportsWhetherAgenticReviewCanAuthoriseAMerge(t *testing.T) {
	const project = "actions: { pipelines: true, pipeline_fixes: true, features: true, reviews: true }\n" +
		"pipeline_fix: { auto_merge: { mode: after-agentic-review } }\nchange: { auto_merge: { mode: after-agentic-review } }\n"
	for _, c := range []struct {
		name, operator, ladder, extra string
		wantAvail                     bool
		wantReason                    string
	}{
		{"no reviewer rung configured", testOperatorYAML, "qwen-local, glm, sonnet", "", false, "no-review-rung"},
		{"the reviewer is the only model the author can run on", withReviewRung("glm"), "qwen-local, glm", "", false, "no-distinct-model"},
		{"the author can also run on another model", withReviewRung("glm"), "qwen-local, glm, sonnet", "", true, ""},
		{"the reviewer is a model the author never runs on", withReviewRung("sonnet"), "qwen-local, glm", "", true, ""},
		{"the ladder has no rung at the author's floor", withReviewRung("glm"), "qwen-local", "", false, "no-author-rung"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestServiceWithConfig(t, c.operator)
			resp := f.register("group/repo", "group-repo", "version: 1\nenabled: true\nladder: ["+c.ladder+"]\nbudget: { monthly_cost_usd: 50 }\n"+project+c.extra)
			e := resp.Effective
			if e == nil || !e.Actions.Reviews {
				t.Fatalf("effective = %+v", e)
			}
			// The MODE the project asked for is reported as asked...
			if e.PipelineFix.AutoMerge.Mode != "after-agentic-review" || e.Change.AutoMerge.Mode != "after-agentic-review" {
				t.Errorf("modes = %q / %q, want the project's after-agentic-review", e.PipelineFix.AutoMerge.Mode, e.Change.AutoMerge.Mode)
			}
			// ...and whether it can be USED is stated beside it, by name.
			for wf, st := range map[string]meterapi.ReviewMergeStatus{"pipeline_fix": e.Review.AgenticMerge.PipelineFix, "change": e.Review.AgenticMerge.Change} {
				if st.Available != c.wantAvail || st.Reason != c.wantReason {
					t.Errorf("%s: status = %+v, want available=%v reason=%q", wf, st, c.wantAvail, c.wantReason)
				}
			}
			r := e.Review.EffectiveReview()
			mode, why := gonkcfg.UsableAutoMergeMode(e.Actions.Features, e.Change.EffectiveChange().AutoMerge, r.ChangeMerge)
			if c.wantAvail && mode != gonkcfg.AutoMergeAfterAgenticReview {
				t.Errorf("usable mode = %q (%q), want after-agentic-review", mode, why)
			}
			if !c.wantAvail && (mode != gonkcfg.AutoMergeOff || why != c.wantReason) {
				t.Errorf("usable mode = %q (%q), want off (%q): the mode must be UNAVAILABLE, not silently satisfied", mode, why, c.wantReason)
			}
		})
	}

	// An operator who then names a distinct reviewer makes it available on the
	// next reresolve, with no change to the project's file.
	f := newTestServiceWithConfig(t, withReviewRung("glm"))
	resp := f.register("group/repo", "group-repo", "version: 1\nenabled: true\nladder: [qwen-local, glm]\nbudget: { monthly_cost_usd: 50 }\n"+project)
	if st := resp.Effective.Review.AgenticMerge.Change; st.Available || st.Reason != "no-distinct-model" {
		t.Fatalf("before: %+v", st)
	}
	f.svc.SetConfig(mustLoadOperator(t, withReviewRung("sonnet")))
	if err := f.svc.Reresolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, found, err := f.svc.Get(context.Background(), "group/repo")
	if err != nil || !found || got.Effective == nil {
		t.Fatalf("GetProject: %v %v", found, err)
	}
	if st := got.Effective.Review.AgenticMerge.Change; !st.Available || got.Effective.Review.Rung != "sonnet" {
		t.Errorf("after the operator named a distinct reviewer: rung %q status %+v, want sonnet and available", got.Effective.Review.Rung, st)
	}
}

func mustLoadOperator(t *testing.T, yaml string) *opercfg.OperatorConfig {
	t.Helper()
	cfg, err := opercfg.Load([]byte(yaml))
	if err != nil {
		t.Fatalf("opercfg.Load: %v", err)
	}
	return cfg
}
