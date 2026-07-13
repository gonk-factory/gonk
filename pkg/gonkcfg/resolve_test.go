package gonkcfg

import (
	"math"
	"reflect"
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
