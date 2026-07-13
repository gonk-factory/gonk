package gonkcfg

import (
	"fmt"
	"math"
	"strings"
)

// Effective is the fully-resolved configuration for one project: defaults
// applied, precedence folded, budgets tightened. This is the only type the
// rest of gonk consumes.
type Effective struct {
	Enabled bool
	// DisabledReason explains why Enabled is false. Invariant: it is
	// non-empty if and only if Enabled is false; it is always "" when the
	// project is enabled.
	DisabledReason string
	Actions        Actions
	Schedule       *Schedule
	Budget         EffectiveBudget
	Ladder         []string
	Continuity     string
	Triage         EffectiveTriage
	Provenance     EffectiveProvenance
}

type Actions struct{ Triage, Pipelines, Features bool }

type EffectiveBudget struct {
	MonthlyCostUSD float64       // +Inf when unlimited
	MonthlyTokens  TokenQuantity // MaxInt64 when unlimited
	PerTaskTokens  TokenQuantity // MaxInt64 when unlimited
}

type EffectiveTriage struct {
	LabelPrefix       string
	RespondToMentions bool
}

type EffectiveProvenance struct {
	CommitTrailers bool
	IncludeUsage   bool
}

// Resolve folds instance defaults, group overrides, and the project config
// into an Effective. Precedence: most specific wins, except (a) enabled and
// actions are vetoable by coarser layers, and (b) budget ceilings are the
// minimum across layers (spec 5.4: ceilings only tighten downward).
//
// Resolve fails closed: a project whose effective ladder is empty -- because
// no layer configured one, or because the layers' allow-lists do not overlap
// -- cannot run any rung, so it is reported as disabled with a
// DisabledReason rather than handed back enabled with a zero-rung ladder.
// See ADR-002.
func Resolve(instance, group Policy, project ProjectConfig) Effective {
	layers := []Policy{instance, group, project.Policy} // coarse -> specific

	eff := Effective{
		Enabled:    project.Enabled != nil && *project.Enabled,
		Continuity: "resume",
		Triage:     EffectiveTriage{LabelPrefix: "gonk::", RespondToMentions: true},
		Provenance: EffectiveProvenance{CommitTrailers: true, IncludeUsage: false},
		Budget: EffectiveBudget{
			MonthlyCostUSD: math.Inf(1),
			MonthlyTokens:  TokenQuantity(math.MaxInt64),
			PerTaskTokens:  TokenQuantity(math.MaxInt64),
		},
	}

	// Vetoes: any coarser layer explicitly disabling wins.
	for _, l := range []Policy{instance, group} {
		if l.Enabled != nil && !*l.Enabled {
			eff.Enabled = false
		}
	}
	eff.Actions = Actions{
		Triage:    andAction(project.Actions.Triage, instance.Actions.Triage, group.Actions.Triage),
		Pipelines: andAction(project.Actions.Pipelines, instance.Actions.Pipelines, group.Actions.Pipelines),
		Features:  andAction(project.Actions.Features, instance.Actions.Features, group.Actions.Features),
	}

	// Tighten-only budgets.
	for _, l := range layers {
		if v := l.Budget.MonthlyCostUSD; v != nil && *v < eff.Budget.MonthlyCostUSD {
			eff.Budget.MonthlyCostUSD = *v
		}
		if v := l.Budget.MonthlyTokens; v != nil && *v < eff.Budget.MonthlyTokens {
			eff.Budget.MonthlyTokens = *v
		}
		if v := l.Budget.PerTaskTokens; v != nil && *v < eff.Budget.PerTaskTokens {
			eff.Budget.PerTaskTokens = *v
		}
	}

	// Ladder: most specific list is the base (ordering), coarser set lists
	// act as allow-lists.
	for i := len(layers) - 1; i >= 0; i-- {
		if layers[i].Ladder != nil {
			eff.Ladder = append([]string(nil), layers[i].Ladder...)
			for j := 0; j < i; j++ {
				if layers[j].Ladder != nil {
					eff.Ladder = intersect(eff.Ladder, layers[j].Ladder)
				}
			}
			break
		}
	}

	// Most-specific-wins scalars.
	for _, l := range layers {
		if l.Schedule != nil {
			sc := *l.Schedule
			eff.Schedule = &sc
		}
		if l.Continuity != nil {
			eff.Continuity = *l.Continuity
		}
		if l.Triage.LabelPrefix != nil {
			eff.Triage.LabelPrefix = *l.Triage.LabelPrefix
		}
		if l.Triage.RespondToMentions != nil {
			eff.Triage.RespondToMentions = *l.Triage.RespondToMentions
		}
		if l.Provenance.CommitTrailers != nil {
			eff.Provenance.CommitTrailers = *l.Provenance.CommitTrailers
		}
		if l.Provenance.IncludeUsage != nil {
			eff.Provenance.IncludeUsage = *l.Provenance.IncludeUsage
		}
	}

	// Fail closed on an empty ladder, and explain why the project is off.
	// Precedence: a project killed by a coarser layer says so rather than
	// blaming its ladder.
	if len(eff.Ladder) == 0 {
		eff.Enabled = false
	}
	switch {
	case instance.Enabled != nil && !*instance.Enabled:
		eff.DisabledReason = "disabled by instance policy"
	case group.Enabled != nil && !*group.Enabled:
		eff.DisabledReason = "disabled by group policy"
	case project.Enabled == nil || !*project.Enabled:
		eff.DisabledReason = "disabled by project .gonk.yml"
	case len(eff.Ladder) == 0:
		eff.DisabledReason = ladderEmptyReason(instance, group, project)
	}
	return eff
}

// ladderEmptyReason explains an empty effective ladder, naming each layer
// that constrained it (most specific first) and omitting silent layers.
func ladderEmptyReason(instance, group Policy, project ProjectConfig) string {
	var parts []string
	for _, l := range []struct {
		name  string
		rungs []string
	}{
		{"project", project.Ladder},
		{"group", group.Ladder},
		{"instance", instance.Ladder},
	} {
		if l.rungs != nil {
			parts = append(parts, fmt.Sprintf("%s %v", l.name, l.rungs))
		}
	}
	if len(parts) == 0 {
		return "ladder empty: no ladder configured at any layer"
	}
	return fmt.Sprintf("ladder empty: no rung allowed by all layers (%s)",
		strings.Join(parts, ", "))
}

// andAction: project must opt in (nil -> false); coarser layers veto
// (nil -> allow).
func andAction(project *bool, vetoes ...*bool) bool {
	if project == nil || !*project {
		return false
	}
	for _, v := range vetoes {
		if v != nil && !*v {
			return false
		}
	}
	return true
}

func intersect(base, allowed []string) []string {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	out := base[:0:0]
	for _, b := range base {
		if _, ok := set[b]; ok {
			out = append(out, b)
		}
	}
	return out
}
