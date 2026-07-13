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
}
