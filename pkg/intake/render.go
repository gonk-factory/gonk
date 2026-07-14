package intake

import (
	"bytes"
	"fmt"
	"text/template"
)

// OnboardBranch is the branch the onboarding MR is opened from (spec, Appendix A).
const OnboardBranch = "gonk/onboard"

// DefaultConfigTemplate is the text of the .gonk.yml the onboarding MR commits:
// the most conservative configuration that still does something (spec 5.3) —
// triage only, zero cloud budget. A maintainer who merges this without reading it
// has authorized nothing that costs money.
//
// Every value is a fixed, conservative literal EXCEPT `ladder:`, which is
// RENDERED from the operator's instance ladder (OD-B) — intake must NOT hardcode a
// rung. A hardcoded rung that is absent from the operator's catalog would disable
// every freshly-onboarded project (ADR-002's empty-ladder rule); only the operator
// knows their catalog. The instance ladder reaches intake from operator/chart
// config (Plan 05's amendment); meter derives its own from the same source.
//
// It is comment-rich, not a marshaled struct: the comments ARE the product, and a
// YAML marshaler would strip them. TestDefaultConfigIsValidAndEnabled keeps it
// honest against the schema and the resolver.
var DefaultConfigTemplate = template.Must(template.New("gonkcfg").Parse(`# gonk configuration. https://gitlab.orac.local/agentic/gonk-project
# Schema: docs/schemas/gonk-config.v1.schema.json
#
# Removing this file, or removing the gonk bot from this project, de-onboards it.
version: 1

# Master switch for this project.
enabled: true

# What gonk is allowed to do here. Only triage is implemented today.
actions:
  triage: true
  pipelines: false
  features: false

# Hard ceilings. These only ever tighten: the instance and group may impose a
# lower limit, never a higher one.
budget:
  monthly_cost_usd: 0      # paid-model spend. 0 = no paid models, ever.
  monthly_tokens: "50M"
  per_task_tokens: "2M"

# Models gonk may use here, cheapest first. This is an allow-list: a model that
# is not listed cannot be used, whatever the budget says. Cloud rungs must be
# added deliberately (and need a non-zero monthly_cost_usd to be reachable).
# Seeded from the operator's instance ladder; trim or reorder as you like.
ladder:
{{- range .Ladder}}
  - {{.}}
{{- end}}

# resume: continue an interrupted session where it left off. fresh: start over.
continuity: resume

triage:
  label_prefix: "gonk::"
  respond_to_mentions: true

# Bot-authored commits carry git trailers naming what generated them.
provenance:
  commit_trailers: true
  include_usage: false     # true also records token/cost in commit trailers
`))

// RenderDefaultConfig renders the committed .gonk.yml, filling `ladder:` from the
// operator's instance ladder (OD-B). DETERMINISTIC for a given ladder, NO MODEL
// CALL.
//
// AN EMPTY LADDER IS A HARD ERROR, and this is the fail-closed backstop. Do NOT
// assume some upstream refuses it: intake does NOT run `opercfg` -- meter does --
// so nothing else on intake's side guards this. If `GONK_INSTANCE_LADDER` were
// unset and this happily emitted a `.gonk.yml` with an empty `ladder:`, ADR-002's
// empty-ladder rule would then disable EVERY freshly-onboarded project. So we
// refuse here, loudly, rather than write a config that silently onboards projects
// dead. `cmd/gonk-intake`'s loadConfig makes the same refusal at startup.
func RenderDefaultConfig(instanceLadder []string) ([]byte, error) {
	if len(instanceLadder) == 0 {
		return nil, fmt.Errorf("intake: refusing to render a .gonk.yml with an empty ladder: " +
			"GONK_INSTANCE_LADDER is unset or empty, and an empty ladder disables every " +
			"onboarded project (ADR-002)")
	}
	var buf bytes.Buffer
	if err := DefaultConfigTemplate.Execute(&buf, struct{ Ladder []string }{instanceLadder}); err != nil {
		return nil, fmt.Errorf("intake: render default config: %w", err)
	}
	return buf.Bytes(), nil
}

// OnboardingContext is everything the MR body varies on. Keep it small: every
// field here is a way for two renders to differ.
type OnboardingContext struct {
	Project     string // path_with_namespace
	BotUsername string
	Version     string   // gonk version, for the provenance trailer
	Ladder      []string // operator instance ladder (OD-B); NOT a hardcoded rung
}

// RenderOnboardingMR produces the MR description. It is a pure function of its
// input (including the operator instance ladder) — DETERMINISTIC, NO MODEL CALL
// (spec 5.3). The consequences it lists are rendered from the config values
// themselves — the SAME rendered bytes it embeds — so the text cannot drift from
// the settings, and it names whatever rungs the operator's ladder actually holds.
func RenderOnboardingMR(c OnboardingContext) (string, error) {
	cfg, err := RenderDefaultConfig(c.Ladder)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := onboardingTmpl.Execute(&buf, struct {
		OnboardingContext
		Config string
	}{c, string(cfg)}); err != nil {
		return "", fmt.Errorf("intake: render onboarding MR: %w", err)
	}
	return buf.String(), nil
}

var onboardingTmpl = template.Must(template.New("onboarding").Parse(
	`# Enable gonk on {{.Project}}

gonk is an automation bot. It was invited to **{{.Project}}**, and this merge
request is how it asks permission to actually do anything. Nothing happens until
this is merged. Closing this merge request declines gonk; it will not ask again
unless it is re-invited.

**No language model was used to produce this merge request.** It is rendered from
a template, and the explanation below is generated from the exact configuration
values committed here, so the two cannot disagree.

## What merging this authorizes

| Key | Value | What it means |
|---|---|---|
| ` + "`enabled`" + ` | ` + "`true`" + ` | gonk acts on this project. Set to ` + "`false`" + ` to switch it off without removing the file. |
| ` + "`actions.triage`" + ` | ` + "`true`" + ` | On a new issue, gonk reads it, applies labels, and posts one analysis comment. |
| ` + "`actions.pipelines`" + ` | ` + "`false`" + ` | gonk will not touch failing pipelines. (Not implemented yet.) |
| ` + "`actions.features`" + ` | ` + "`false`" + ` | gonk will not decompose designs into work items. (Not implemented yet.) |
| ` + "`budget.monthly_cost_usd`" + ` | ` + "`0`" + ` | **$0.** No paid model can be used on this project. Raising this is the only way to spend money here. |
| ` + "`budget.monthly_tokens`" + ` | ` + "`50M`" + ` | Ceiling on tokens per calendar month across all of gonk's work here. |
| ` + "`budget.per_task_tokens`" + ` | ` + "`2M`" + ` | Ceiling for a single work item, so one runaway task cannot eat the month. |
| ` + "`ladder`" + ` | ` + "`{{.Ladder}}`" + ` | The models gonk may use here, cheapest first — seeded from this instance's configured ladder, not a hardcoded rung. A model not listed cannot be used, whatever the budget says. Cloud rungs must be added deliberately (and need a non-zero budget). |
| ` + "`continuity`" + ` | ` + "`resume`" + ` | An interrupted session resumes rather than starting over. |
| ` + "`triage.label_prefix`" + ` | ` + "`gonk::`" + ` | Every label gonk creates starts with this, so its labels are always distinguishable from yours. |
| ` + "`triage.respond_to_mentions`" + ` | ` + "`true`" + ` | Mentioning ` + "`@{{.BotUsername}}`" + ` in an issue comment gets a reply in that thread. |
| ` + "`provenance.commit_trailers`" + ` | ` + "`true`" + ` | Commits gonk authors carry a trailer naming what generated them. |
| ` + "`provenance.include_usage`" + ` | ` + "`false`" + ` | Token counts and cost are NOT written into commit trailers. Set to ` + "`true`" + ` to publish them in git history. |
| ` + "`schedule.quiet_hours`" + ` | unset | Optional. Set (with ` + "`schedule.timezone`" + `) to hold gonk's work outside a window, e.g. ` + "`\"22:00-07:00\"`" + `. |
| ` + "`schedule.timezone`" + ` | unset | IANA zone name for ` + "`quiet_hours`" + `, e.g. ` + "`America/New_York`" + `. |
| ` + "`version`" + ` | ` + "`1`" + ` | Config schema version. |

## What happens after you merge

1. gonk opens a second merge request adding a ` + "`.agent/`" + ` directory: a short,
   written-by-reading-this-repo description of what the project is and how it is
   built. That is the first thing gonk does that uses a model, and it is
   authorized by the budget you just merged.
2. Until that lands, gonk does nothing else. Triage starts once ` + "`.agent/`" + ` exists.
3. gonk never merges anything itself, ever, and never pushes outside ` + "`gonk/*`" + `
   branches.

## The file this adds

` + "```yaml\n{{.Config}}```" + `

## Turning it off

Remove this file, or remove ` + "`@{{.BotUsername}}`" + ` from the project's members. Either alone
de-onboards gonk. Setting ` + "`enabled: false`" + ` keeps the config but stops all activity.

---
Generated-By: gonk/{{.Version}} (deterministic onboarding; no model was used)
`))
