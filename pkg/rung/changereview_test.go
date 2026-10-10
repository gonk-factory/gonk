package rung

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/opercfg"
)

// gonk-cuwn part X-CONFIG: the issue-change and mr-review triggers at the
// meter's decision -- action gate, ladder, floor, reservation TTL -- and the
// per-workflow answer to "can an agentic review authorise a merge here".

func str(s string) *string { return &s }

// crInput is a project that opts in to everything, under an instance naming
// `sonnet` as the reviewer rung. Ladder: [qwen-local, glm, sonnet].
func crInput(t *testing.T, trigger string, mut func(instance, project *gonkcfg.Policy)) Input {
	t.Helper()
	instance := instancePolicy()
	instance.Review.Rung = str("sonnet")
	project := projectPolicy()
	project.Ladder = []string{"qwen-local", "glm", "sonnet"}
	project.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true), PipelineFixes: b(true), Features: b(true), Reviews: b(true)}
	if mut != nil {
		mut(&instance, &project)
	}
	in := baseInput(t)
	in.CloudAllowed = true
	in.Effective = effective(t, instance, gonkcfg.Policy{}, project)
	in.Trigger = trigger
	return in
}

func TestIssueChangeIsGatedByFeaturesAndStartsAtTheFloor(t *testing.T) {
	got := Decide(crInput(t, atags.TriggerIssueChange, nil))
	if got.Kind != Run || got.Rung != "glm" || got.Attempt != 1 || got.Escalated {
		t.Fatalf("issue-change with features on = %+v, want run on ladder index 1 (glm), attempt 1", got)
	}
	for _, want := range []string{`"glm"`, "position 2 of 3", "issue-change", "0 of 1 floor retries used"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q does not say %q", got.Detail, want)
		}
	}
	for name, mut := range map[string]func(instance, project *gonkcfg.Policy){
		"features not set":            func(_, p *gonkcfg.Policy) { p.Actions.Features = nil },
		"features false":              func(_, p *gonkcfg.Policy) { p.Actions.Features = b(false) },
		"features vetoed by operator": func(i, _ *gonkcfg.Policy) { i.Actions.Features = b(false) },
		"only every OTHER action on": func(_, p *gonkcfg.Policy) {
			p.Actions = gonkcfg.ActionsPolicy{Triage: b(true), Pipelines: b(true), PipelineFixes: b(true), Scaffold: b(true), Reviews: b(true)}
		},
	} {
		if got := Decide(crInput(t, atags.TriggerIssueChange, mut)); got.Kind != Deny || got.Reason != ReasonActionNotAllowed {
			t.Errorf("%s: issue-change = %+v, want deny/%s", name, got, ReasonActionNotAllowed)
		}
	}
	// One gate failure re-runs the floor rung; the second escalates.
	in := crInput(t, atags.TriggerIssueChange, nil)
	in.Prior = []Attempt{{Attempt: 1, Rung: "glm", Outcome: OutcomeGateFailed}}
	if got := Decide(in); got.Kind != Run || got.Rung != "glm" || got.Escalated {
		t.Errorf("after one gate failure = %+v, want a floor retry on glm", got)
	}
	in.Prior = append(in.Prior, Attempt{Attempt: 2, Rung: "glm", Outcome: OutcomeGateFailed})
	if got := Decide(in); got.Kind != Run || got.Rung != "sonnet" || !got.Escalated {
		t.Errorf("after two gate failures = %+v, want an escalation to sonnet", got)
	}
	// A ladder with no rung at the floor: denied, not run on the 16K rung.
	short := crInput(t, atags.TriggerIssueChange, func(_, p *gonkcfg.Policy) { p.Ladder = []string{"qwen-local"} })
	if got := Decide(short); got.Kind != Deny || got.Reason != ReasonLadderExhausted {
		t.Errorf("a one-rung ladder = %+v, want deny/%s", got, ReasonLadderExhausted)
	}
}

// Which rung reviews is the OPERATOR'S setting. mr-review runs on review.rung
// and nowhere else: not the project's first rung, not its floor, not its top.
func TestMRReviewRunsOnlyOnTheOperatorsReviewerRung(t *testing.T) {
	got := Decide(crInput(t, atags.TriggerMRReview, nil))
	if got.Kind != Run || got.Rung != "sonnet" || got.Model != "claude-sonnet" || got.Attempt != 1 || got.Escalated {
		t.Fatalf("mr-review = %+v, want run on the reviewer rung sonnet", got)
	}
	if !strings.Contains(got.Detail, "position 1 of 1") {
		t.Errorf("detail %q does not show a one-rung ladder", got.Detail)
	}
	// The project's ladder does not matter: narrowed to the cheapest rung, the
	// review still runs on the operator's rung.
	narrowed := crInput(t, atags.TriggerMRReview, func(_, p *gonkcfg.Policy) { p.Ladder = []string{"qwen-local", "glm"} })
	if got := Decide(narrowed); got.Kind != Run || got.Rung != "sonnet" {
		t.Errorf("with the project ladder [qwen-local, glm]: %+v, want run on sonnet", got)
	}
	// A different operator choice moves it; the project cannot.
	local := crInput(t, atags.TriggerMRReview, func(i, _ *gonkcfg.Policy) { i.Review.Rung = str("qwen-local") })
	if got := Decide(local); got.Kind != Run || got.Rung != "qwen-local" {
		t.Errorf("with review.rung qwen-local: %+v, want run on qwen-local", got)
	}
	projectTries := crInput(t, atags.TriggerMRReview, func(_, p *gonkcfg.Policy) { p.Review.Rung = str("qwen-local") })
	if got := Decide(projectTries); got.Kind != Run || got.Rung != "sonnet" {
		t.Errorf("a project naming its own reviewer rung moved the review: %+v", got)
	}

	// No reviewer rung: denied, whatever the project's ladder holds.
	none := crInput(t, atags.TriggerMRReview, func(i, _ *gonkcfg.Policy) { i.Review.Rung = nil })
	if got := Decide(none); got.Kind != Deny || got.Reason != ReasonLadderExhausted || !strings.Contains(got.Detail, "review.rung") {
		t.Errorf("with no review.rung: %+v, want deny/%s naming review.rung", got, ReasonLadderExhausted)
	}
	// A reviewer rung that is not in the catalog: an invalid configuration.
	ghost := crInput(t, atags.TriggerMRReview, func(i, _ *gonkcfg.Policy) { i.Review.Rung = str("opus") })
	if got := Decide(ghost); got.Kind != Deny || got.Reason != ReasonInvalidConfig {
		t.Errorf("with an unknown review.rung: %+v, want deny/%s", got, ReasonInvalidConfig)
	}

	// One refused batch re-runs the reviewer; a second ends the review. It
	// NEVER climbs: there is nowhere to climb to.
	in := crInput(t, atags.TriggerMRReview, func(i, _ *gonkcfg.Policy) { i.Review.Rung = str("qwen-local") })
	in.Prior = []Attempt{{Attempt: 1, Rung: "qwen-local", Outcome: OutcomeGateFailed}}
	if got := Decide(in); got.Kind != Run || got.Rung != "qwen-local" || got.Escalated {
		t.Errorf("after one gate failure = %+v, want the reviewer rung again", got)
	}
	in.Prior = append(in.Prior, Attempt{Attempt: 2, Rung: "qwen-local", Outcome: OutcomeGateFailed})
	if got := Decide(in); got.Kind != Deny || got.Reason != ReasonLadderExhausted {
		t.Errorf("after two gate failures = %+v, want deny/%s (a review does not escalate)", got, ReasonLadderExhausted)
	}

	// Gated by its own action.
	for name, mut := range map[string]func(instance, project *gonkcfg.Policy){
		"reviews not set":            func(_, p *gonkcfg.Policy) { p.Actions.Reviews = nil },
		"reviews vetoed by operator": func(i, _ *gonkcfg.Policy) { i.Actions.Reviews = b(false) },
		"no author workflow effective": func(i, _ *gonkcfg.Policy) {
			i.Actions.PipelineFixes, i.Actions.Features = b(false), b(false)
		},
	} {
		if got := Decide(crInput(t, atags.TriggerMRReview, mut)); got.Kind != Deny || got.Reason != ReasonActionNotAllowed {
			t.Errorf("%s: mr-review = %+v, want deny/%s", name, got, ReasonActionNotAllowed)
		}
	}
	// The cloud allowance still applies to a cloud reviewer.
	noCloud := crInput(t, atags.TriggerMRReview, nil)
	noCloud.CloudAllowed = false
	if got := Decide(noCloud); got.Kind != Deny || got.Reason != ReasonCloudNotAllowed {
		t.Errorf("a cloud reviewer with no cloud allowance = %+v, want deny/%s", got, ReasonCloudNotAllowed)
	}
}

func TestLadderFor(t *testing.T) {
	eff := gonkcfg.Effective{Ladder: []string{"a", "b"}, Review: gonkcfg.EffectiveReview{Rung: "r"}}
	for _, trig := range []string{atags.TriggerIssueTriage, atags.TriggerPipelineDiagnosis, atags.TriggerPipelineFix, atags.TriggerIssueChange,
		atags.TriggerMentionReply, atags.TriggerScaffold, atags.TriggerOnboarding, "no-such-trigger"} {
		if got := LadderFor(eff, trig); len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("LadderFor(%s) = %v, want the project's ladder", trig, got)
		}
	}
	if got := LadderFor(eff, atags.TriggerMRReview); len(got) != 1 || got[0] != "r" {
		t.Errorf("LadderFor(mr-review) = %v, want [r]", got)
	}
	eff.Review.Rung = ""
	if got := LadderFor(eff, atags.TriggerMRReview); len(got) != 0 {
		t.Errorf("LadderFor(mr-review) with no reviewer rung = %v, want empty", got)
	}
}

// The per-trigger reservation TTL: issue-change is held for the base plus the
// project's change.verify_timeout; mr-review for the base.
func TestChangeReservationTTL(t *testing.T) {
	const base = 60 * time.Minute
	change := func(d time.Duration) gonkcfg.Effective {
		return gonkcfg.Effective{Change: gonkcfg.EffectiveChange{VerifyTimeout: d}, PipelineFix: gonkcfg.EffectivePipelineFix{VerifyTimeout: 5 * time.Hour}}
	}
	resolved := func(yaml string) gonkcfg.Effective {
		t.Helper()
		pc, err := gonkcfg.Load([]byte("version: 1\nenabled: true\nactions: { features: true, reviews: true }\n" + yaml))
		if err != nil {
			t.Fatal(err)
		}
		return gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *pc)
	}
	for _, c := range []struct {
		name    string
		trigger string
		eff     gonkcfg.Effective
		want    time.Duration
	}{
		{"issue-change, default verify_timeout (2h)", atags.TriggerIssueChange, resolved(""), 3 * time.Hour},
		{"issue-change, project sets 30m", atags.TriggerIssueChange, resolved("change: { verify_timeout: 30m }\n"), 90 * time.Minute},
		{"issue-change, project sets the ceiling", atags.TriggerIssueChange, resolved("change: { verify_timeout: 6h }\n"), 7 * time.Hour},
		{"issue-change reads change, not pipeline_fix", atags.TriggerIssueChange, resolved("change: { verify_timeout: 30m }\npipeline_fix: { verify_timeout: 6h }\n"), 90 * time.Minute},
		{"issue-change, an unresolved Effective adds nothing", atags.TriggerIssueChange, gonkcfg.Effective{}, base},
		{"issue-change, a timeout past the ceiling is clamped to it", atags.TriggerIssueChange, change(1000 * time.Hour), 7 * time.Hour},
		{"issue-change, a negative timeout never shortens the base", atags.TriggerIssueChange, change(-30 * time.Minute), base},
		{"mr-review is the base", atags.TriggerMRReview, change(6 * time.Hour), base},
		{"pipeline-fix reads pipeline_fix, not change", atags.TriggerPipelineFix, change(30 * time.Minute), 6 * time.Hour},
		{"pipeline-diagnosis is the base", atags.TriggerPipelineDiagnosis, change(6 * time.Hour), base},
		{"issue-triage is the base", atags.TriggerIssueTriage, change(6 * time.Hour), base},
	} {
		if got := ReservationTTL(c.trigger, base, c.eff); got != c.want {
			t.Errorf("%s: ReservationTTL = %s, want %s", c.name, got, c.want)
		}
	}
}

// Owner decision Q4: when no distinct adequate reviewer model is configured,
// the agentic-review merge mode is UNAVAILABLE, with a named reason.
func TestResolveReviewMerge(t *testing.T) {
	cat := map[string]opercfg.RungSpec{
		"small": {Name: "small", Kind: opercfg.KindLocal, Model: "m-small"},
		"big":   {Name: "big", Kind: opercfg.KindLocal, Model: "m-big"},
		"big2":  {Name: "big2", Kind: opercfg.KindLocal, Model: "m-big"}, // another rung, the SAME model
		"cloud": {Name: "cloud", Kind: opercfg.KindCloud, Model: "m-cloud"},
	}
	type st = gonkcfg.ReviewMergeStatus
	ok := st{Available: true}
	no := func(r string) st { return st{Reason: r} }
	on := gonkcfg.ActionsPolicy{Pipelines: b(true), PipelineFixes: b(true), Features: b(true), Reviews: b(true)}
	cases := []struct {
		name    string
		ladder  []string
		rung    *string
		actions gonkcfg.ActionsPolicy
		want    st
	}{
		// The owner's case: one adequate model. The author runs on it (index 1
		// up), the reviewer is it. Unavailable.
		{"the reviewer is the only model the author can run on", []string{"small", "big"}, str("big"), on, no("no-distinct-model")},
		{"the same model under another rung name", []string{"small", "big"}, str("big2"), on, no("no-distinct-model")},
		{"the author can also run on a different model", []string{"small", "big", "cloud"}, str("big"), on, ok},
		{"the reviewer is a model the author never runs on", []string{"small", "big"}, str("cloud"), on, ok},
		// Index 0 is below the author's floor: it does not count as an author rung.
		{"the reviewer is the rung BELOW the author's floor", []string{"small", "big"}, str("small"), on, ok},
		{"a different model only below the floor does not help", []string{"small", "big"}, str("big"), on, no("no-distinct-model")},
		{"no reviewer rung", []string{"small", "big"}, nil, on, no("no-review-rung")},
		{"reviewer rung not in the catalog", []string{"small", "big"}, str("opus"), on, no("review-rung-not-in-catalog")},
		{"the ladder has no rung at the author's floor", []string{"small"}, str("big"), on, no("no-author-rung")},
		{"reviews off", []string{"small", "big", "cloud"}, str("big"), gonkcfg.ActionsPolicy{Features: b(true)}, no("reviews-off")},
	}
	for _, c := range cases {
		instance := gonkcfg.Policy{Review: gonkcfg.ReviewPolicy{Rung: c.rung}}
		project := gonkcfg.Policy{Enabled: b(true), Ladder: c.ladder, Actions: c.actions}
		before := effective(t, instance, gonkcfg.Policy{}, project)
		if before.Review.PipelineFixMerge.Available || before.Review.ChangeMerge.Available {
			t.Fatalf("%s: gonkcfg.Resolve alone called the mode available: %+v", c.name, before.Review)
		}
		got := ResolveReviewMerge(before, cat)
		if got.Review.PipelineFixMerge != c.want || got.Review.ChangeMerge != c.want {
			t.Errorf("%s: statuses = %+v / %+v, want %+v", c.name, got.Review.PipelineFixMerge, got.Review.ChangeMerge, c.want)
		}
		// And what a merge decision is told.
		am := gonkcfg.EffectiveAutoMerge{Mode: gonkcfg.AutoMergeAfterAgenticReview}
		mode, why := gonkcfg.UsableAutoMergeMode(true, am, got.Review.ChangeMerge)
		if c.want.Available && (mode != gonkcfg.AutoMergeAfterAgenticReview || why != "") {
			t.Errorf("%s: usable mode = %q (%q), want after-agentic-review", c.name, mode, why)
		}
		if !c.want.Available && (mode != gonkcfg.AutoMergeOff || why != c.want.Reason) {
			t.Errorf("%s: usable mode = %q (%q), want off (%q)", c.name, mode, why, c.want.Reason)
		}
		// Nothing else about the Effective changes.
		got.Review.PipelineFixMerge, got.Review.ChangeMerge = before.Review.PipelineFixMerge, before.Review.ChangeMerge
		if !reflect.DeepEqual(got, before) {
			t.Errorf("%s: ResolveReviewMerge changed more than the statuses", c.name)
		}
	}
	// A zero Effective (nobody resolved it) stays unavailable.
	z := ResolveReviewMerge(gonkcfg.Effective{}, cat)
	if z.Review.PipelineFixMerge.Available || z.Review.ChangeMerge.Available {
		t.Errorf("a zero Effective became available: %+v", z.Review)
	}
}
