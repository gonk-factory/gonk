package gonkcfg

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func b(v bool) *bool                    { return &v }
func f(v float64) *float64              { return &v }
func tq(v TokenQuantity) *TokenQuantity { return &v }
func s(v string) *string                { return &v }

func project(mut func(*ProjectConfig)) ProjectConfig {
	pc := ProjectConfig{Version: 1, Policy: Policy{
		Enabled: b(true),
		Actions: ActionsPolicy{Triage: b(true)},
		Ladder:  []string{"qwen-local"},
	}}
	if mut != nil {
		mut(&pc)
	}
	return pc
}

func TestResolveDefaults(t *testing.T) {
	eff := Resolve(Policy{}, Policy{}, project(nil))
	if !eff.Enabled || !eff.Actions.Triage || eff.Actions.Pipelines {
		t.Fatalf("enabled/actions wrong: %+v", eff)
	}
	if eff.Continuity != "resume" {
		t.Fatalf("continuity default wrong: %q", eff.Continuity)
	}
	if eff.Triage.LabelPrefix != "gonk::" || !eff.Triage.RespondToMentions {
		t.Fatalf("triage defaults wrong: %+v", eff.Triage)
	}
	if !eff.Provenance.CommitTrailers || eff.Provenance.IncludeUsage {
		t.Fatalf("provenance defaults wrong: %+v", eff.Provenance)
	}
	if !math.IsInf(eff.Budget.MonthlyCostUSD, 1) {
		t.Fatalf("unset cost should be unlimited: %v", eff.Budget.MonthlyCostUSD)
	}
	if eff.Budget.MonthlyTokens != math.MaxInt64 {
		t.Fatalf("unset tokens should be unlimited: %v", eff.Budget.MonthlyTokens)
	}
}

func TestResolveKillSwitchAndActionAND(t *testing.T) {
	eff := Resolve(Policy{Enabled: b(false)}, Policy{}, project(nil))
	if eff.Enabled {
		t.Fatal("instance kill switch ignored")
	}
	eff = Resolve(Policy{}, Policy{Actions: ActionsPolicy{Triage: b(false)}}, project(nil))
	if eff.Actions.Triage {
		t.Fatal("group action veto ignored")
	}
	eff = Resolve(Policy{}, Policy{}, project(func(pc *ProjectConfig) {
		pc.Actions = ActionsPolicy{} // project silent on all actions
	}))
	if eff.Actions.Triage {
		t.Fatal("project must opt in to actions")
	}
}

func TestResolveBudgetTightenOnly(t *testing.T) {
	inst := Policy{Budget: BudgetPolicy{MonthlyCostUSD: f(100), MonthlyTokens: tq(1_000_000_000)}}
	grp := Policy{Budget: BudgetPolicy{MonthlyCostUSD: f(10)}}
	proj := project(func(pc *ProjectConfig) {
		pc.Budget = BudgetPolicy{MonthlyCostUSD: f(50), MonthlyTokens: tq(50_000_000)}
	})
	eff := Resolve(inst, grp, proj)
	if eff.Budget.MonthlyCostUSD != 10 { // min(100, 10, 50)
		t.Fatalf("cost = %v, want 10", eff.Budget.MonthlyCostUSD)
	}
	if eff.Budget.MonthlyTokens != 50_000_000 { // min(1G, unset, 50M)
		t.Fatalf("tokens = %v, want 50M", eff.Budget.MonthlyTokens)
	}
}

func TestResolveLadderIntersection(t *testing.T) {
	inst := Policy{Ladder: []string{"qwen-local", "glm", "sonnet"}}
	grp := Policy{Ladder: []string{"qwen-local", "glm"}}
	proj := project(func(pc *ProjectConfig) {
		pc.Ladder = []string{"glm", "qwen-local", "opus"}
	})
	eff := Resolve(inst, grp, proj)
	want := []string{"glm", "qwen-local"} // project order, opus not allowed upstream
	if !reflect.DeepEqual(eff.Ladder, want) {
		t.Fatalf("ladder = %v, want %v", eff.Ladder, want)
	}
	// project silent -> inherits group's list
	eff = Resolve(inst, grp, project(func(pc *ProjectConfig) { pc.Ladder = nil }))
	if !reflect.DeepEqual(eff.Ladder, []string{"qwen-local", "glm"}) {
		t.Fatalf("inherited ladder = %v", eff.Ladder)
	}
}

func TestResolveMostSpecificWins(t *testing.T) {
	inst := Policy{Continuity: s("fresh"), Schedule: &Schedule{QuietHours: "01:00-02:00", Timezone: "UTC"}}
	proj := project(func(pc *ProjectConfig) { pc.Continuity = s("resume") })
	eff := Resolve(inst, Policy{}, proj)
	if eff.Continuity != "resume" {
		t.Fatalf("continuity = %q", eff.Continuity)
	}
	if eff.Schedule == nil || eff.Schedule.QuietHours != "01:00-02:00" {
		t.Fatalf("schedule not inherited: %+v", eff.Schedule)
	}
}

// 1. Group-only kill switch: instance silent, group vetoes.
func TestResolveGroupKillSwitch(t *testing.T) {
	eff := Resolve(Policy{}, Policy{Enabled: b(false)}, project(nil))
	if eff.Enabled {
		t.Fatal("group kill switch ignored")
	}
	if eff.DisabledReason != "disabled by group policy" {
		t.Fatalf("reason = %q", eff.DisabledReason)
	}
}

// 2. Instance-only action veto: group silent, instance vetoes.
func TestResolveInstanceActionVeto(t *testing.T) {
	inst := Policy{Actions: ActionsPolicy{Triage: b(false)}}
	eff := Resolve(inst, Policy{}, project(nil))
	if eff.Actions.Triage {
		t.Fatal("instance action veto ignored")
	}
	if !eff.Enabled {
		t.Fatal("an action veto must not disable the project")
	}
}

// 3. PerTaskTokens participates in the tighten-only fold.
func TestResolvePerTaskTokensTightenOnly(t *testing.T) {
	inst := Policy{Budget: BudgetPolicy{PerTaskTokens: tq(500_000)}}
	grp := Policy{Budget: BudgetPolicy{PerTaskTokens: tq(100_000)}}
	proj := project(func(pc *ProjectConfig) {
		pc.Budget = BudgetPolicy{PerTaskTokens: tq(250_000)}
	})
	eff := Resolve(inst, grp, proj)
	if eff.Budget.PerTaskTokens != 100_000 { // min(500K, 100K, 250K)
		t.Fatalf("per-task tokens = %v, want 100000", eff.Budget.PerTaskTokens)
	}
	// A coarser layer alone still applies when project is silent.
	eff = Resolve(inst, Policy{}, project(nil))
	if eff.Budget.PerTaskTokens != 500_000 {
		t.Fatalf("inherited per-task tokens = %v, want 500000", eff.Budget.PerTaskTokens)
	}
}

// 4. An explicit 0 is a hard "nothing allowed" ceiling, never "unset".
func TestResolveBudgetZeroIsHardCeiling(t *testing.T) {
	grp := Policy{Budget: BudgetPolicy{
		MonthlyCostUSD: f(0),
		MonthlyTokens:  tq(0),
		PerTaskTokens:  tq(0),
	}}
	proj := project(func(pc *ProjectConfig) {
		pc.Budget = BudgetPolicy{MonthlyCostUSD: f(50), MonthlyTokens: tq(50_000_000)}
	})
	eff := Resolve(Policy{}, grp, proj)
	if eff.Budget.MonthlyCostUSD != 0 {
		t.Fatalf("cost = %v, want a hard 0 ceiling (not unlimited)", eff.Budget.MonthlyCostUSD)
	}
	if eff.Budget.MonthlyTokens != 0 {
		t.Fatalf("tokens = %v, want a hard 0 ceiling (not unlimited)", eff.Budget.MonthlyTokens)
	}
	if eff.Budget.PerTaskTokens != 0 {
		t.Fatalf("per-task = %v, want a hard 0 ceiling (not unlimited)", eff.Budget.PerTaskTokens)
	}
}

// 5. Empty ladder intersection fails closed.
func TestResolveEmptyLadderIntersectionFailsClosed(t *testing.T) {
	inst := Policy{Ladder: []string{"qwen-local"}}
	proj := project(func(pc *ProjectConfig) { pc.Ladder = []string{"opus"} })
	eff := Resolve(inst, Policy{}, proj)
	if eff.Enabled {
		t.Fatal("empty ladder intersection must disable the project")
	}
	if len(eff.Ladder) != 0 {
		t.Fatalf("ladder = %v, want empty", eff.Ladder)
	}
	if !strings.Contains(eff.DisabledReason, "ladder") {
		t.Fatalf("reason %q should mention the ladder", eff.DisabledReason)
	}
	// The reason must name the constraining layers and their rungs.
	for _, want := range []string{"project", "[opus]", "instance", "[qwen-local]"} {
		if !strings.Contains(eff.DisabledReason, want) {
			t.Fatalf("reason %q missing %q", eff.DisabledReason, want)
		}
	}
	// A silent layer must not be named.
	if strings.Contains(eff.DisabledReason, "group") {
		t.Fatalf("reason %q names a layer that set no ladder", eff.DisabledReason)
	}
}

// 6. Resolve must not alias or mutate its inputs.
func TestResolveDoesNotAliasInputs(t *testing.T) {
	orig := &Schedule{QuietHours: "01:00-02:00", Timezone: "UTC"}
	inst := Policy{Schedule: orig}
	eff := Resolve(inst, Policy{}, project(nil))
	if eff.Schedule == orig {
		t.Fatal("eff.Schedule aliases the caller's Schedule")
	}
	eff.Schedule.QuietHours = "MUTATED"
	eff.Schedule.Timezone = "MUTATED"
	if orig.QuietHours != "01:00-02:00" || orig.Timezone != "UTC" {
		t.Fatalf("Resolve leaked a pointer; caller's Schedule mutated: %+v", orig)
	}
	// The project's ladder slice must not be aliased into the result either.
	pl := []string{"qwen-local", "glm"}
	proj := project(func(pc *ProjectConfig) { pc.Ladder = pl })
	eff = Resolve(Policy{}, Policy{}, proj)
	if len(eff.Ladder) > 0 {
		eff.Ladder[0] = "MUTATED"
	}
	if pl[0] != "qwen-local" {
		t.Fatalf("Resolve aliased the project's ladder slice: %v", pl)
	}
}

// 7. No layer sets a ladder at all -> nothing is allowed, fail closed.
func TestResolveNoLadderAnywhereFailsClosed(t *testing.T) {
	proj := project(func(pc *ProjectConfig) { pc.Ladder = nil })
	eff := Resolve(Policy{}, Policy{}, proj)
	if eff.Enabled {
		t.Fatal("no ladder at any layer must disable the project")
	}
	if len(eff.Ladder) != 0 {
		t.Fatalf("ladder = %v, want empty", eff.Ladder)
	}
	if eff.DisabledReason != "ladder empty: no ladder configured at any layer" {
		t.Fatalf("reason = %q", eff.DisabledReason)
	}
}

// 8. DisabledReason is non-empty iff Enabled is false, and coarse-layer
// vetoes outrank the ladder reason.
func TestResolveDisabledReasonInvariant(t *testing.T) {
	eff := Resolve(Policy{}, Policy{}, project(nil))
	if !eff.Enabled {
		t.Fatalf("expected an enabled resolve, got %+v", eff)
	}
	if eff.DisabledReason != "" {
		t.Fatalf("enabled project must have no reason, got %q", eff.DisabledReason)
	}

	// Instance veto wins over the (also empty) ladder reason.
	inst := Policy{Enabled: b(false), Ladder: []string{"qwen-local"}}
	proj := project(func(pc *ProjectConfig) { pc.Ladder = []string{"opus"} })
	eff = Resolve(inst, Policy{}, proj)
	if eff.Enabled {
		t.Fatal("instance kill switch ignored")
	}
	if eff.DisabledReason != "disabled by instance policy" {
		t.Fatalf("reason = %q, want the instance reason to outrank the ladder reason", eff.DisabledReason)
	}

	// A project that never opts in reports its own reason.
	eff = Resolve(Policy{}, Policy{}, project(func(pc *ProjectConfig) { pc.Enabled = nil }))
	if eff.Enabled {
		t.Fatal("project must opt in")
	}
	if eff.DisabledReason != "disabled by project .gonk.yml" {
		t.Fatalf("reason = %q", eff.DisabledReason)
	}

	// Reason precedence, pairwise: instance outranks group.
	eff = Resolve(Policy{Enabled: b(false)}, Policy{Enabled: b(false)}, project(nil))
	if eff.DisabledReason != "disabled by instance policy" {
		t.Fatalf("reason = %q, want instance to outrank group", eff.DisabledReason)
	}
	// Group outranks the project's own non-opt-in.
	eff = Resolve(Policy{}, Policy{Enabled: b(false)}, project(func(pc *ProjectConfig) { pc.Enabled = nil }))
	if eff.DisabledReason != "disabled by group policy" {
		t.Fatalf("reason = %q, want group to outrank project", eff.DisabledReason)
	}
	// The project's non-opt-in outranks the ladder reason.
	eff = Resolve(Policy{}, Policy{}, project(func(pc *ProjectConfig) {
		pc.Enabled = nil
		pc.Ladder = nil
	}))
	if eff.DisabledReason != "disabled by project .gonk.yml" {
		t.Fatalf("reason = %q, want project to outrank the ladder reason", eff.DisabledReason)
	}
}

// A non-finite cost ceiling must fail closed, never silently become
// "unlimited". NaN loses every comparison, so a naive min-fold skips it and
// leaves the +Inf unlimited sentinel standing -- a silent budget escape.
func TestResolveNonFiniteBudgetFailsClosed(t *testing.T) {
	nan := math.NaN()
	posInf := math.Inf(1)
	negInf := math.Inf(-1)

	for _, tc := range []struct {
		name  string
		bad   float64
		layer string
	}{
		{"instance NaN", nan, "instance"},
		{"instance +Inf", posInf, "instance"},
		{"instance -Inf", negInf, "instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := Policy{Budget: BudgetPolicy{MonthlyCostUSD: &tc.bad}}
			eff := Resolve(inst, Policy{}, project(nil))
			if eff.Enabled {
				t.Fatal("a non-finite cost ceiling must disable the project")
			}
			if math.IsInf(eff.Budget.MonthlyCostUSD, 1) {
				t.Fatal("non-finite ceiling silently resolved to unlimited")
			}
			for _, want := range []string{"invalid budget", "monthly_cost_usd", tc.layer} {
				if !strings.Contains(eff.DisabledReason, want) {
					t.Fatalf("reason %q missing %q", eff.DisabledReason, want)
				}
			}
		})
	}

	// Every layer is checked, and the layer is named correctly.
	grp := Policy{Budget: BudgetPolicy{MonthlyCostUSD: &nan}}
	eff := Resolve(Policy{}, grp, project(nil))
	if eff.Enabled || !strings.Contains(eff.DisabledReason, "group") {
		t.Fatalf("group NaN not caught: enabled=%v reason=%q", eff.Enabled, eff.DisabledReason)
	}
	proj := project(func(pc *ProjectConfig) {
		pc.Budget = BudgetPolicy{MonthlyCostUSD: &nan}
	})
	eff = Resolve(Policy{}, Policy{}, proj)
	if eff.Enabled || !strings.Contains(eff.DisabledReason, "project") {
		t.Fatalf("project NaN not caught: enabled=%v reason=%q", eff.Enabled, eff.DisabledReason)
	}

	// An invalid budget outranks the ladder reason but not a coarser veto.
	eff = Resolve(Policy{Enabled: b(false), Budget: BudgetPolicy{MonthlyCostUSD: &nan}}, Policy{}, project(nil))
	if eff.DisabledReason != "disabled by instance policy" {
		t.Fatalf("reason = %q, want the instance veto to outrank the budget reason", eff.DisabledReason)
	}
	eff = Resolve(Policy{Budget: BudgetPolicy{MonthlyCostUSD: &nan}}, Policy{},
		project(func(pc *ProjectConfig) { pc.Ladder = nil }))
	if !strings.Contains(eff.DisabledReason, "invalid budget") {
		t.Fatalf("reason = %q, want the budget reason to outrank the ladder reason", eff.DisabledReason)
	}
}

// Each action's veto must be wired to both coarser layers. andAction is
// shared, but per-action wiring is where copy-paste bugs land -- and
// pipelines is the action that spends money.
func TestResolveActionVetoWiringAllThree(t *testing.T) {
	allOn := func(pc *ProjectConfig) {
		pc.Actions = ActionsPolicy{Triage: b(true), Pipelines: b(true), Features: b(true)}
	}
	// Baseline: project opts into all three, no vetoes -> all on.
	eff := Resolve(Policy{}, Policy{}, project(allOn))
	if !eff.Actions.Triage || !eff.Actions.Pipelines || !eff.Actions.Features {
		t.Fatalf("baseline actions should all be on: %+v", eff.Actions)
	}

	for _, tc := range []struct {
		name   string
		veto   ActionsPolicy
		got    func(Actions) bool
		others func(Actions) bool
	}{
		{"triage", ActionsPolicy{Triage: b(false)},
			func(a Actions) bool { return a.Triage },
			func(a Actions) bool { return a.Pipelines && a.Features }},
		{"pipelines", ActionsPolicy{Pipelines: b(false)},
			func(a Actions) bool { return a.Pipelines },
			func(a Actions) bool { return a.Triage && a.Features }},
		{"features", ActionsPolicy{Features: b(false)},
			func(a Actions) bool { return a.Features },
			func(a Actions) bool { return a.Triage && a.Pipelines }},
	} {
		// Instance vetoes this action.
		eff := Resolve(Policy{Actions: tc.veto}, Policy{}, project(allOn))
		if tc.got(eff.Actions) {
			t.Errorf("%s: instance veto ignored", tc.name)
		}
		if !tc.others(eff.Actions) {
			t.Errorf("%s: instance veto leaked into other actions: %+v", tc.name, eff.Actions)
		}
		// Group vetoes this action.
		eff = Resolve(Policy{}, Policy{Actions: tc.veto}, project(allOn))
		if tc.got(eff.Actions) {
			t.Errorf("%s: group veto ignored", tc.name)
		}
		if !tc.others(eff.Actions) {
			t.Errorf("%s: group veto leaked into other actions: %+v", tc.name, eff.Actions)
		}
	}
}

// Every most-specific-wins fold, asserted with a NON-default value. With
// default-valued expectations a resolver that ignored all three layers and
// returned only its hardcoded defaults would still pass.
func TestResolveInheritsNonDefaultValues(t *testing.T) {
	// Inherit each field from the instance when finer layers are silent.
	inst := Policy{
		Continuity: s("fresh"),
		Triage:     TriagePolicy{LabelPrefix: s("bot::"), RespondToMentions: b(false)},
		Provenance: ProvenancePolicy{CommitTrailers: b(false), IncludeUsage: b(true)},
		Schedule:   &Schedule{QuietHours: "22:00-07:00", Timezone: "America/New_York"},
	}
	eff := Resolve(inst, Policy{}, project(nil))
	if eff.Continuity != "fresh" {
		t.Errorf("continuity = %q, want inherited %q", eff.Continuity, "fresh")
	}
	if eff.Triage.LabelPrefix != "bot::" {
		t.Errorf("label_prefix = %q, want inherited %q", eff.Triage.LabelPrefix, "bot::")
	}
	if eff.Triage.RespondToMentions {
		t.Error("respond_to_mentions = true, want inherited false")
	}
	if eff.Provenance.CommitTrailers {
		t.Error("commit_trailers = true, want inherited false")
	}
	if !eff.Provenance.IncludeUsage {
		t.Error("include_usage = false, want inherited true")
	}
	if eff.Schedule == nil || eff.Schedule.Timezone != "America/New_York" {
		t.Errorf("schedule not inherited: %+v", eff.Schedule)
	}

	// Most specific wins across all three layers: project's value survives.
	grp := Policy{
		Continuity: s("resume"),
		Triage:     TriagePolicy{LabelPrefix: s("group::"), RespondToMentions: b(true)},
		Provenance: ProvenancePolicy{CommitTrailers: b(true), IncludeUsage: b(false)},
		Schedule:   &Schedule{QuietHours: "01:00-02:00", Timezone: "UTC"},
	}
	proj := project(func(pc *ProjectConfig) {
		pc.Continuity = s("fresh")
		pc.Triage = TriagePolicy{LabelPrefix: s("proj::"), RespondToMentions: b(false)}
		pc.Provenance = ProvenancePolicy{CommitTrailers: b(false), IncludeUsage: b(true)}
		pc.Schedule = &Schedule{QuietHours: "03:00-04:00", Timezone: "Asia/Tokyo"}
	})
	eff = Resolve(inst, grp, proj)
	if eff.Continuity != "fresh" {
		t.Errorf("continuity = %q, want project's %q", eff.Continuity, "fresh")
	}
	if eff.Triage.LabelPrefix != "proj::" {
		t.Errorf("label_prefix = %q, want project's %q", eff.Triage.LabelPrefix, "proj::")
	}
	if eff.Triage.RespondToMentions {
		t.Error("respond_to_mentions should be the project's false")
	}
	if eff.Provenance.CommitTrailers {
		t.Error("commit_trailers should be the project's false")
	}
	if !eff.Provenance.IncludeUsage {
		t.Error("include_usage should be the project's true")
	}
	if eff.Schedule == nil || eff.Schedule.Timezone != "Asia/Tokyo" {
		t.Errorf("schedule = %+v, want the project's", eff.Schedule)
	}

	// Group beats instance when the project is silent.
	eff = Resolve(inst, grp, project(nil))
	if eff.Continuity != "resume" {
		t.Errorf("continuity = %q, want group's %q", eff.Continuity, "resume")
	}
	if eff.Triage.LabelPrefix != "group::" {
		t.Errorf("label_prefix = %q, want group's %q", eff.Triage.LabelPrefix, "group::")
	}
	if eff.Schedule == nil || eff.Schedule.Timezone != "UTC" {
		t.Errorf("schedule = %+v, want the group's", eff.Schedule)
	}
}

// schedule has no default: silence at every layer means no schedule at all.
func TestResolveScheduleDefaultsToNone(t *testing.T) {
	eff := Resolve(Policy{}, Policy{}, project(nil))
	if eff.Schedule != nil {
		t.Fatalf("schedule = %+v, want nil when no layer sets one", eff.Schedule)
	}
}
