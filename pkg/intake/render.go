package intake

import (
	"bytes"
	"fmt"
	"text/template"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
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
  scaffold: false          # let gonk spend tokens drafting .agent/ for you.
                           # off by default: this merge request already adds a
                           # .agent/ seed, and filling it in by hand costs
                           # nothing and is more accurate.

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
	seed, err := RenderAgentSeed(c)
	if err != nil {
		return "", err
	}
	paths := make([]string, len(seed))
	for i, f := range seed {
		paths[i] = f.Path
	}
	var buf bytes.Buffer
	if err := onboardingTmpl.Execute(&buf, struct {
		OnboardingContext
		Config    string
		SeedPaths []string
	}{c, string(cfg), paths}); err != nil {
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
| ` + "`actions.scaffold`" + ` | ` + "`false`" + ` | gonk will not spend tokens drafting this project's ` + "`.agent/`" + ` context. This merge request already adds a seed you can fill in by hand for free; set this to ` + "`true`" + ` only if you would rather gonk read the repository and propose a draft. |
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

1. Triage starts. On the next issue opened here, gonk reads it, applies its
   labels and posts one analysis comment. There is no second step to wait for.
2. The ` + "`.agent/`" + ` directory this merge request adds is yours to fill in, and
   filling it in is optional. It is where this project tells automated sessions
   what it is, how it is built and what rules a change has to follow; gonk reads
   it before the issue it is working on. Without it, triage answers from the code
   alone -- a thinner answer, not a refused one. ` + "`.agent/README.md`" + ` explains
   the two ways to fill it in.
3. gonk never merges anything itself, ever, and never pushes outside ` + "`gonk/*`" + `
   branches.

## The files this adds

` + "```yaml\n{{.Config}}```" + `

It also adds a ` + "`.agent/`" + ` seed -- ` + "`{{range $i, $f := .SeedPaths}}{{if $i}}`, `{{end}}{{$f}}{{end}}`" + ` --
rendered from the same template as the file above, with no model involved. Every
one of them is a skeleton with the headings filled in and the prose left to you.

## Turning it off

Remove this file, or remove ` + "`@{{.BotUsername}}`" + ` from the project's members. Either alone
de-onboards gonk. Setting ` + "`enabled: false`" + ` keeps the config but stops all activity.

---
Generated-By: gonk/{{.Version}} (deterministic onboarding; no model was used)
`))

// ---------------------------------------------------------------- .agent/ seed

// AgentSeedFile is one file the onboarding merge request commits under
// `.agent/`. Order is fixed and meaningful: it is the commit order, the order
// the MR body lists, and the order a session's prompt splices them in.
type AgentSeedFile struct {
	Path    string
	Content string
}

// AgentSeedPaths are the files the onboarding merge request seeds, in order.
//
// It is ALSO the list a triage prompt loads (cmd/gonk-gate's agentContextFiles,
// which has a drift test against this one). pkg/glab has no tree-listing call
// and this needs none: gonk seeds these names, so these are the names it can
// ask for. A project is free to keep other files here -- people should read
// them -- but v1's loader is thin on purpose and splices only these.
var AgentSeedPaths = []string{
	AgentDir + "/README.md",
	AgentDir + "/overview.md",
	AgentDir + "/build-and-test.md",
	AgentDir + "/conventions.md",
}

// RenderAgentSeed renders the `.agent/` seed the onboarding merge request
// commits alongside `.gonk.yml`.
//
// DETERMINISTIC, NO MODEL CALL -- the same discipline as the `.gonk.yml`
// explanation, and for the same reason: this used to arrive from a metered
// scaffold session, which meant a project could not be triaged until it had
// paid for one. Seeding it here costs nothing and happens before the first
// issue.
//
// The values it quotes are READ BACK OUT OF THE RENDERED CONFIG rather than
// typed alongside it, so the seed cannot claim a label prefix or a ladder the
// committed `.gonk.yml` does not actually set.
func RenderAgentSeed(c OnboardingContext) ([]AgentSeedFile, error) {
	raw, err := RenderDefaultConfig(c.Ladder)
	if err != nil {
		return nil, err
	}
	cfg, err := gonkcfg.Load(raw)
	if err != nil {
		// Unreachable while TestDefaultConfigIsValidAndEnabled passes, and a
		// hard error anyway: a seed rendered from a config gonk cannot read
		// would be describing settings nobody can confirm.
		return nil, fmt.Errorf("intake: render .agent/ seed: the default config does not load: %w", err)
	}
	return renderAgentSeedFrom(c, cfg)
}

// renderAgentSeedFrom is RenderAgentSeed with the config supplied, so a test can
// render the seed from a config that does NOT use the default values and prove
// the prose follows them. Nothing in production calls it with anything but the
// rendered default.
func renderAgentSeedFrom(c OnboardingContext, cfg *gonkcfg.ProjectConfig) ([]AgentSeedFile, error) {
	// NOT gonkcfg.Resolve. Intake has no operator policy and must never
	// resolve one (see this package's doc comment); it reads the project
	// layer's own literal values, which is all the seed quotes.
	if cfg.Triage.LabelPrefix == nil {
		return nil, fmt.Errorf("intake: render .agent/ seed: the config sets no triage.label_prefix, " +
			"so the seed cannot state which labels gonk will use")
	}
	if len(cfg.Ladder) == 0 {
		return nil, fmt.Errorf("intake: render .agent/ seed: the config has an empty ladder")
	}
	data := struct {
		OnboardingContext
		LabelPrefix string
		Ladder      []string
		SeedPaths   []string
		AgentDir    string
	}{c, *cfg.Triage.LabelPrefix, cfg.Ladder, AgentSeedPaths, AgentDir}

	out := make([]AgentSeedFile, 0, len(AgentSeedPaths))
	for _, path := range AgentSeedPaths {
		tmpl, ok := agentSeedTmpl[path]
		if !ok {
			return nil, fmt.Errorf("intake: render .agent/ seed: no template for %q", path)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("intake: render %s: %w", path, err)
		}
		out = append(out, AgentSeedFile{Path: path, Content: buf.String()})
	}
	return out, nil
}

// SeedNotFilledIn is the marker every skeleton section carries until a human
// replaces it. It is the honest signal a reader (and a session) needs: an empty
// heading is indistinguishable from a heading somebody decided to leave empty.
const SeedNotFilledIn = "NOT FILLED IN YET"

var agentSeedTmpl = map[string]*template.Template{
	AgentDir + "/README.md": template.Must(template.New("agent-readme").Parse(
		`# ` + "`{{.AgentDir}}/`" + ` — this project's context for automated sessions

This directory is the project's own description of itself, written for the
automated sessions gonk runs here. What it says overrides anything a session
would otherwise infer from reading the code.

**gonk committed this seed as part of its onboarding merge request.** No model
was used: it is a template with the headings filled in and the prose left to
you. Every section still marked ` + "`" + SeedNotFilledIn + "`" + ` is one nobody has
written yet.

## It is optional

gonk triages issues on **{{.Project}}** whether or not this directory has
anything in it. Without it, a session answers from the code alone, which is a
thinner answer. Filling it in is the cheapest way to make those answers
specific to this repository.

## What gonk reads

A triage session is given these files, in this order, **before** the issue it
was asked to look at:
{{range .SeedPaths}}
- ` + "`{{.}}`" + `{{end}}

Other files kept here are not read by v1 of that loader. Put anything a person
should read wherever you like; put anything a *session* must know in the files
above.

## Two ways to fill this in

1. **Write it yourself, or run Navigator locally.** Navigator's conventions and
   lint half is what this directory's shape comes from, and running it in a
   local session fills these files from the repository in front of you. This is
   the recommended route: it costs gonk nothing and you review every word as it
   is written.
2. **Ask gonk to draft it.** Set ` + "`actions.scaffold: true`" + ` in ` + "`.gonk.yml`" + `.
   gonk will then read this repository in a metered session and open a second
   merge request proposing content for these files, which you review and edit
   like any other merge request. It is **off by default** and stays off until
   you turn it on: it spends tokens against this project's budget, and a draft
   written by reading the repo is a guess where you have knowledge.

## What gonk already does here, without this directory

These come from ` + "`.gonk.yml`" + ` and are quoted from the file this project
actually committed, not from documentation that could drift from it:

- Every label gonk applies starts with ` + "`{{.LabelPrefix}}`" + `, so its labels are
  always distinguishable from yours.
- It may use only these models, cheapest first: {{range $i, $r := .Ladder}}{{if $i}}, {{end}}` + "`{{$r}}`" + `{{end}}.
- Mentioning ` + "`@{{.BotUsername}}`" + ` in an issue comment reaches it.

---
Generated-By: gonk/{{.Version}} (deterministic onboarding seed; no model was used)
`)),

	AgentDir + "/overview.md": template.Must(template.New("agent-overview").Parse(
		`# What {{.Project}} is

` + SeedNotFilledIn + ` — one paragraph: what this project does, and who or what
consumes it. Say the thing a newcomer would otherwise have to infer from the
directory names.

## Main components

` + SeedNotFilledIn + ` — the two or three parts somebody has to know about
before they can read a diff here, and where each one lives.

## What this project is NOT

` + SeedNotFilledIn + ` — the wrong assumption a reader most often arrives with.
This section earns its keep: it is the one a session cannot derive from the code.

## Open questions

` + SeedNotFilledIn + ` — anything genuinely undecided. An honest gap here is
worth more than a confident guess, because every automated session reads this
file and will repeat what it says.
`)),

	AgentDir + "/build-and-test.md": template.Must(template.New("agent-build").Parse(
		`# Building, running and testing {{.Project}}

## Build

` + SeedNotFilledIn + ` — the exact command, and anything that has to exist
before it will work.

## Test

` + SeedNotFilledIn + ` — the exact command that runs the tests, and what
"green" means here. Name the check a change is expected to pass before it is
proposed.

## Run it locally

` + SeedNotFilledIn + ` — how to get a working instance in front of you, or a
plain statement that you cannot and why.

## Things that look broken but are not

` + SeedNotFilledIn + ` — the failures every newcomer reports once. Writing them
down here is how a session stops reporting them too.
`)),

	AgentDir + "/conventions.md": template.Must(template.New("agent-conventions").Parse(
		`# Conventions for {{.Project}}

Rules a change here has to follow. Write the ones that are actually enforced;
an aspiration recorded as a rule teaches a session to claim compliance it does
not have.

## Code

` + SeedNotFilledIn + ` — layout, naming, error handling, anything a reviewer
reliably asks for.

## Commits and merge requests

` + SeedNotFilledIn + ` — message shape, branch naming, what has to be in a
merge request description.

## Do not touch

` + SeedNotFilledIn + ` — generated files, vendored trees, anything with an
owner outside this repository.
`)),
}
