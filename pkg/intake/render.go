package intake

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// OnboardBranch is the branch the onboarding MR is opened from (spec, Appendix A).
const OnboardBranch = "gonk/onboard"

// DefaultConfigTemplate is the text of the .gonk.yml the onboarding MR commits:
// the most conservative configuration that still does something (spec 5.3) —
// triage only, zero cloud budget. A maintainer who merges this without reading it
// has authorized nothing that costs money.
//
// Every value is a fixed, conservative literal EXCEPT `ladder:` and
// `scope.profiles:`. `scope.profiles` is the ecosystem-profile selection
// onboarding PROPOSES from marker files on the default branch (gonk-wj14; see
// ProfileDetection) -- a proposal a human merges or edits, never something
// re-detected later. `ladder:` is
// RENDERED from the operator's instance ladder (OD-B) — intake must NOT hardcode a
// rung. A hardcoded rung that is absent from the operator's catalog would disable
// every freshly-onboarded project (ADR-002's empty-ladder rule); only the operator
// knows their catalog. The instance ladder reaches intake from operator/chart
// config (Plan 05's amendment); meter derives its own from the same source.
//
// It is comment-rich, not a marshaled struct: the comments ARE the product, and a
// YAML marshaler would strip them. TestDefaultConfigIsValidAndEnabled keeps it
// honest against the schema and the resolver.
var DefaultConfigTemplate = template.Must(template.New("gonkcfg").Parse(`# gonk configuration. https://github.com/gonk-factory/gonk
# Schema: docs/schemas/gonk-config.v1.schema.json
#
# Removing this file, or removing the gonk bot from this project, de-onboards it.
version: 1

# Master switch for this project.
enabled: true

# What gonk is allowed to do here. features is not implemented yet.
actions:
  triage: true
  pipelines: false        # true: gonk diagnoses failed pipelines (default
                           # branch and merge requests) and posts one note per
                           # failure. It changes nothing and retries nothing.
  pipeline_fixes: false    # true (needs pipelines: true): a diagnosis may be
                           # followed by one small fix merge request. Limits
                           # go in a pipeline_fix: section; see the table in
                           # this merge request. gonk then checks that merge
                           # request's own pipeline and says what it found.
                           # It merges nothing: a person does.
  features: false
  scaffold: false          # let gonk spend tokens drafting .agent/ for you.
                           # off by default: this merge request already adds a
                           # .agent/ seed, and filling it in by hand costs
                           # nothing and is more accurate.
  reviews: false           # NOT ACTIVE YET: no version of gonk reviews
                           # anything so far. Reserved for an independent
                           # review of merge requests gonk itself opened
                           # (needs pipeline_fixes: true or features: true).

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

# Which files gonk treats as source code, tests and documentation here, chosen
# from the profiles gonk ships: go, python, node (JavaScript and TypeScript),
# docs-md, docs-rst, docs-txt, docs-adoc. This list was filled in by this
# merge request from files at the root of the default branch (the merge
# request says which) and is NOT detected again: correct it here. It matters
# only to workflows that write code, and nothing enabled above does.
# Dependency manifests, lock files and test-runner configuration of every
# listed AND unlisted ecosystem are off limits to an ordinary change either way.
scope:
  profiles: [{{.Profiles}}]
`))

// RenderDefaultConfig renders the .gonk.yml onboarding commits for a project in
// which no ecosystem marker was found: `ladder:` from the operator's instance
// ladder (OD-B) and `scope.profiles` at the default selection. DETERMINISTIC
// for a given ladder, NO MODEL CALL. It is RenderConfig with nothing detected.
func RenderDefaultConfig(instanceLadder []string) ([]byte, error) {
	return RenderConfig(instanceLadder, scope.DefaultProfiles())
}

// RenderConfig renders the committed .gonk.yml with the given ladder and
// ecosystem-profile selection.
//
// AN EMPTY LADDER IS A HARD ERROR, and this is the fail-closed backstop. Do NOT
// assume some upstream refuses it: intake does NOT run `opercfg` -- meter does --
// so nothing else on intake's side guards this. If `GONK_INSTANCE_LADDER` were
// unset and this happily emitted a `.gonk.yml` with an empty `ladder:`, ADR-002's
// empty-ladder rule would then disable EVERY freshly-onboarded project. So we
// refuse here, loudly, rather than write a config that silently onboards projects
// dead. `cmd/gonk-intake`'s loadConfig makes the same refusal at startup.
//
// A profile that is not on the shipped menu is refused for the same reason: the
// file this renders must be one gonk then accepts.
func RenderConfig(instanceLadder, profiles []string) ([]byte, error) {
	if len(instanceLadder) == 0 {
		return nil, fmt.Errorf("intake: refusing to render a .gonk.yml with an empty ladder: " +
			"GONK_INSTANCE_LADDER is unset or empty, and an empty ladder disables every " +
			"onboarded project (ADR-002)")
	}
	if err := (scope.Policy{Profiles: append([]string{}, profiles...)}).Validate(scope.LayerProject); err != nil {
		return nil, fmt.Errorf("intake: refusing to render a .gonk.yml gonk would reject: %w", err)
	}
	var buf bytes.Buffer
	if err := DefaultConfigTemplate.Execute(&buf, struct {
		Ladder   []string
		Profiles string
	}{instanceLadder, strings.Join(profiles, ", ")}); err != nil {
		return nil, fmt.Errorf("intake: render default config: %w", err)
	}
	return buf.Bytes(), nil
}

// ProfileDetection is what onboarding learned about a project's ecosystems,
// ONCE, from marker files at the root of its default branch (gonk-wj14; 066
// section 11 item 5: "onboarding proposes, a human disposes"). It decides what
// `scope.profiles` is pre-filled with, and the merge request text says exactly
// what was looked for and what was found.
//
// Nothing like this runs at dispatch time: after the merge only the committed
// `.gonk.yml` counts, so repository content never chooses its own scope.
//
// The zero value means "nothing was read": the default selection, and the
// merge request says so.
type ProfileDetection struct {
	// Ref is the branch that was read. "" means no probe was made.
	Ref string
	// Probed are the root-level file names looked for (scope.ProfileMarkers).
	Probed []string
	// Found are the ones that exist at Ref.
	Found []string
}

// Selection is the profile list to pre-fill, and the marker behind each
// ecosystem in it. No marker found (or nothing read) is the default selection
// with no evidence.
func (d ProfileDetection) Selection() ([]string, []scope.ProfileEvidence) {
	return scope.ProposeProfiles(d.Found)
}

// OnboardingContext is everything the MR body varies on. Keep it small: every
// field here is a way for two renders to differ.
type OnboardingContext struct {
	Project     string // path_with_namespace
	BotUsername string
	Version     string   // gonk version, for the provenance trailer
	Ladder      []string // operator instance ladder (OD-B); NOT a hardcoded rung
	// Detection is what the marker-file probe found. The zero value renders
	// the default profile selection and says nothing was read.
	Detection ProfileDetection
}

// renderConfig renders the .gonk.yml this context commits: the one place the
// merge request body, the commit and the .agent/ seed all get it from.
func (c OnboardingContext) renderConfig() ([]byte, error) {
	profiles, _ := c.Detection.Selection()
	return RenderConfig(c.Ladder, profiles)
}

// RenderOnboardingMR produces the MR description. It is a pure function of its
// input (including the operator instance ladder) — DETERMINISTIC, NO MODEL CALL
// (spec 5.3). The consequences it lists are rendered from the config values
// themselves — the SAME rendered bytes it embeds — so the text cannot drift from
// the settings, and it names whatever rungs the operator's ladder actually holds.
func RenderOnboardingMR(c OnboardingContext) (string, error) {
	cfg, err := c.renderConfig()
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
	// The profile text is generated from the SAME detection that chose the
	// value in the config above, so the two cannot disagree.
	profiles, evidence := c.Detection.Selection()
	found := map[string]bool{}
	for _, f := range c.Detection.Found {
		found[f] = true
	}
	type markerRow struct {
		File, Profile string
		Found         bool
	}
	var markers []markerRow
	for _, pr := range scope.Profiles() {
		for _, m := range pr.Markers {
			for _, probed := range c.Detection.Probed {
				if probed == m {
					markers = append(markers, markerRow{File: m, Profile: pr.Name, Found: found[m]})
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := onboardingTmpl.Execute(&buf, struct {
		OnboardingContext
		Config    string
		SeedPaths []string
		Profiles  string
		Menu      string
		Default   string
		Markers   []markerRow
		Evidence  []scope.ProfileEvidence
	}{c, string(cfg), paths, strings.Join(profiles, ", "), strings.Join(scope.ProfileNames(), "`, `"),
		strings.Join(scope.DefaultProfiles(), ", "), markers, evidence}); err != nil {
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
| ` + "`actions.pipelines`" + ` | ` + "`false`" + ` | gonk will not look at failing pipelines. Set to ` + "`true`" + ` and, when a pipeline on the default branch or on a merge request fails, gonk reads the failed jobs' logs and posts ONE diagnosis per failure (on the merge request, or on a single tracking issue for the default branch). It changes nothing and retries nothing. Set it back to ` + "`false`" + ` to stop; gonk also stops receiving this project's pipeline events. |
| ` + "`actions.pipeline_fixes`" + ` | ` + "`false`" + ` | gonk will not propose changes for a failing pipeline. **In this version of gonk: with ` + "`true`" + ` it opens the fix merge request, checks that merge request's pipeline, labels the result, tries again within ` + "`pipeline_fix.max_attempts`" + ` when the failure is still there, and after a person merges it checks the next pipeline on the branch. It merges nothing itself: ` + "`pipeline_fix.auto_merge`" + ` is read and not yet acted on.** What ` + "`true`" + ` is for: after a diagnosis (so it needs ` + "`actions.pipelines: true`" + ` in this file; writing it without is refused as a configuration error), gonk may open ONE small merge request for that diagnosed cause, linked to the diagnosis, and judge it on whether that specific failure stops recurring in the merge request's own pipeline -- not on the pipeline merely being green. gonk never assigns, approves or merges it unless ` + "`pipeline_fix.auto_merge`" + ` says so. |
| ` + "`actions.features`" + ` | ` + "`false`" + ` | gonk will not decompose designs into work items. (Not implemented yet.) |
| ` + "`actions.scaffold`" + ` | ` + "`false`" + ` | gonk will not spend tokens drafting this project's ` + "`.agent/`" + ` context. This merge request already adds a seed you can fill in by hand for free; set this to ` + "`true`" + ` only if you would rather gonk read the repository and propose a draft. |
| ` + "`actions.reviews`" + ` | ` + "`false`" + ` | gonk will not review merge requests. **Not active yet: nothing in this version of gonk starts this workflow, so this key changes nothing today.** What ` + "`true`" + ` is for: a merge request gonk itself opened (a pipeline fix, or a change made from an issue) that passed its own checks is read by a second, independent session, which posts ONE note on it. That session never approves, never assigns itself, never opens a discussion thread and never merges. Merge requests written by people are not reviewed. It needs ` + "`actions.pipeline_fixes: true`" + ` or ` + "`actions.features: true`" + ` in this file; writing it with neither is refused as a configuration error. |
| ` + "`budget.monthly_cost_usd`" + ` | ` + "`0`" + ` | **$0.** No paid model can be used on this project. Raising this is the only way to spend money here. |
| ` + "`budget.monthly_tokens`" + ` | ` + "`50M`" + ` | Ceiling on tokens per calendar month across all of gonk's work here. |
| ` + "`budget.per_task_tokens`" + ` | ` + "`2M`" + ` | Ceiling for a single work item, so one runaway task cannot eat the month. |
| ` + "`ladder`" + ` | ` + "`{{.Ladder}}`" + ` | The models gonk may use here, cheapest first — seeded from this instance's configured ladder, not a hardcoded rung. A model not listed cannot be used, whatever the budget says. Cloud rungs must be added deliberately (and need a non-zero budget). |
| ` + "`continuity`" + ` | ` + "`resume`" + ` | An interrupted session resumes rather than starting over. |
| ` + "`triage.label_prefix`" + ` | ` + "`gonk::`" + ` | Every label gonk creates starts with this, so its labels are always distinguishable from yours. |
| ` + "`triage.respond_to_mentions`" + ` | ` + "`true`" + ` | Mentioning ` + "`@{{.BotUsername}}`" + ` in an issue comment gets a reply in that thread. |
| ` + "`schedule.quiet_hours`" + ` | unset | Optional. Set (with ` + "`schedule.timezone`" + `) to hold gonk's work outside a window, e.g. ` + "`\"22:00-07:00\"`" + `. |
| ` + "`schedule.timezone`" + ` | unset | IANA zone name for ` + "`quiet_hours`" + `, e.g. ` + "`America/New_York`" + `. |
| ` + "`scope.classes`" + ` | unset | Optional, and inert today: ` + "`scope:`" + ` only bounds workflows that WRITE CODE, and nothing this file enables does (triage writes none). Lists the kinds of path such a workflow may change here (` + "`source`" + `, ` + "`test`" + `, ` + "`docs`" + `, ...). It can only remove kinds from what gonk allows by default, never add one. |
| ` + "`scope.caps`" + ` | unset | Optional. Size limits for one change: ` + "`max_files`" + `, ` + "`max_lines`" + `, ` + "`max_deletes`" + `, ` + "`max_file_bytes`" + `, ` + "`max_patch_bytes`" + `. A value can only lower gonk's built-in limit; a higher one has no effect. |
| ` + "`scope.protected_paths`" + ` | unset | Optional. Extra paths no gonk session may ever change, on top of the built-in ones (` + "`.gonk.yml`" + `, ` + "`CODEOWNERS`" + `, CI definitions). Patterns, e.g. ` + "`internal/auth/**`" + `. |
| ` + "`scope.ci_paths`" + ` | unset | Optional. Extra files that are part of your pipeline definition (scripts it runs, templates it includes), protected the same way as ` + "`.gitlab-ci.yml`" + `. |
| ` + "`scope.test_paths`" + ` | unset | Optional. Paths gonk should treat as test code although their names do not say so, e.g. ` + "`internal/testutil/**`" + `. Only ordinary source files can be reclassified this way, and each time one is, it is logged. |
| ` + "`scope.docs_paths`" + ` | unset | Optional. The same, for documentation kept outside ` + "`docs/`" + ` and ` + "`*.md`" + ` files. |
| ` + "`scope.profiles`" + ` | ` + "`[{{.Profiles}}]`" + ` | Which files count as source code, tests and documentation here, picked from the profiles gonk ships: ` + "`{{.Menu}}`" + `. Pre-filled by this merge request -- see "Which languages gonk assumes here" below for what it looked at. Like the rest of ` + "`scope:`" + ` it is inert until a workflow that writes code is enabled. It can only pick from that list: it cannot name paths of its own, and whatever it says, every listed and unlisted ecosystem's dependency manifests, lock files and test-runner configuration (` + "`package.json`" + `, ` + "`requirements.txt`" + `, ` + "`go.mod`" + `, ` + "`conftest.py`" + `, ` + "`tsconfig.json`" + `, ...) stay off limits to an ordinary change. Removing the key means ` + "`[{{.Default}}]`" + `; an empty list means gonk treats no file as source, test or documentation. A name that is not on the list makes this file invalid, and gonk then does nothing here until it is fixed. |
| ` + "`pipeline_fix.classes`" + ` | unset | Optional. Which diagnoses may be followed by a fix: ` + "`code`" + `, ` + "`test`" + `, ` + "`configuration`" + `. Unset means ` + "`[code, test]`" + `. A failure diagnosed as flaky, infrastructure or unknown is never fixed. |
| ` + "`pipeline_fix.targets`" + ` | unset | Optional. Which failed pipelines may get a fix: ` + "`ref`" + ` (the default branch) and ` + "`mr`" + ` (merge request pipelines). Unset means ` + "`[ref]`" + `. |
| ` + "`pipeline_fix.allow_ci`" + ` | unset | Optional, default ` + "`false`" + `. ` + "`true`" + ` lets a fix for a ` + "`configuration`" + ` diagnosis edit your pipeline definition (` + "`.gitlab-ci.yml`" + ` and the files it includes). Nothing else unlocks those files, and ` + "`.gonk.yml`" + `, ` + "`CODEOWNERS`" + ` and secret files stay off limits under every setting. |
| ` + "`pipeline_fix.ci_exec_changes`" + ` | unset | Optional, default ` + "`manual-run`" + `: a pipeline-definition fix that changes what a job executes is committed with ` + "`[skip ci]`" + ` and waits for a maintainer to start its pipeline. ` + "`deny`" + ` refuses such fixes; ` + "`auto`" + ` lets their pipeline run unattended. Such a fix is never merged automatically. |
| ` + "`pipeline_fix.max_attempts`" + ` | unset | Optional, default 2, at most 3. Attempts at one fix before gonk stops and labels it for a human. Each attempt costs you a pipeline run. |
| ` + "`pipeline_fix.max_chain`" + ` | unset | Optional, default 5, at most 10. Fix merge requests per red streak (since the branch was last green). Follow-up fixes come one at a time, each after the previous one is merged. |
| ` + "`pipeline_fix.verify_timeout`" + ` | unset | Optional, default ` + "`2h`" + `, at most ` + "`6h`" + `. How long gonk waits for the fix's pipeline before reporting the fix as unverifiable. |
| ` + "`pipeline_fix.max_files`" + ` | unset | Optional, default and maximum 3. Files one fix may change. |
| ` + "`pipeline_fix.max_changed_lines`" + ` | unset | Optional, default and maximum 40. Added plus removed lines in one fix. |
| ` + "`pipeline_fix.paths`" + ` | unset | Optional. Patterns a fix's paths must also match, e.g. ` + "`internal/**`" + `. It can only narrow what a fix may touch. |
| ` + "`pipeline_fix.auto_merge`" + ` | unset | Optional, and off unless you set ` + "`mode`" + `. ` + "`mode: after-human-approval`" + ` lets gonk press merge on a fix that met its exit criterion once a project member other than the bot approved it; ` + "`mode: after-agentic-review`" + ` needs an independent review workflow that does not exist yet and merges nothing. ` + "`classes`" + ` limits which changes qualify: ` + "`docs`" + ` (documentation only; the default) and ` + "`ci`" + ` (a pipeline-definition change that leaves every job's execution unchanged). A change to source or test files is never merged automatically. ` + "`max_per_day`" + ` (default and maximum 3) caps automatic merges in 24 hours. |
| ` + "`change.triggers`" + ` | unset | **Not active yet: nothing in this version of gonk starts this workflow, so this key changes nothing today.** Optional. ` + "`change:`" + ` bounds a workflow in which gonk turns an issue into ONE merge request for a human to review, once ` + "`actions.features`" + ` is ` + "`true`" + `. It starts only on an explicit request from a project **Maintainer or Owner**, in one of the forms listed here: ` + "`label`" + ` (the implement label put on the issue), ` + "`command`" + ` (a slash-style command in an issue comment), ` + "`mention`" + ` (mentioning the bot and asking it to implement the issue). Unset means all three; a list here can only remove forms. The same request from anyone below Maintainer does nothing, and no setting changes that. gonk never decides on its own that an issue should be implemented. |
| ` + "`change.create_classes`" + ` | unset | Not active yet. Optional. The kinds of path in which a change may create a NEW file: ` + "`test`" + `, ` + "`docs`" + `, ` + "`source`" + `. Unset means ` + "`[test, docs]`" + `; ` + "`source`" + ` must be asked for. Dependency manifests, test-runner configuration, CI definitions and dotfiles can never be created. |
| ` + "`change.require_docs`" + ` | unset | Not active yet. Optional, default ` + "`when-labelled`" + `: a change to source code must also change documentation when the issue carries one of ` + "`change.docs_labels`" + `. ` + "`always`" + ` requires it for every change to source; ` + "`never`" + ` does not require it. |
| ` + "`change.docs_labels`" + ` | unset | Not active yet. Optional. The issue labels that make documentation required under ` + "`when-labelled`" + `. Unset means ` + "`feature`" + `, ` + "`enhancement`" + `, ` + "`type::feature`" + `. A label can only add the requirement. |
| ` + "`change.require_test_report`" + ` | unset | Not active yet. Optional, default ` + "`false`" + `. ` + "`true`" + `: gonk does not start unless your default-branch pipeline publishes a JUnit test report, and reports a change whose pipeline publishes none as unverifiable. Without a report gonk cannot tell whether new tests ran, and says so on the merge request. |
| ` + "`change.max_attempts`" + ` | unset | Not active yet. Optional, default 2, at most 3. Attempts at one issue before gonk stops and labels it for a human. Each attempt costs you a pipeline run. |
| ` + "`change.max_open`" + ` | unset | Not active yet. Optional, default 1, at most 3. How many of these merge requests gonk may have open here at once. |
| ` + "`change.verify_timeout`" + ` | unset | Not active yet. Optional, default ` + "`2h`" + `, at most ` + "`6h`" + `. How long gonk waits for the merge request's pipeline before reporting the change as unverifiable. gonk runs nothing itself: your pipeline is the only thing that tests the change. |
| ` + "`change.max_files`" + ` | unset | Not active yet. Optional, default 6, at most 8. Files one change may touch, edited plus created. |
| ` + "`change.max_changed_lines`" + ` | unset | Not active yet. Optional, default 200, at most 300. Added plus removed lines in one change; every line of a new file counts. |
| ` + "`change.paths`" + ` | unset | Not active yet. Optional. Patterns a change's paths must also match, e.g. ` + "`internal/**`" + `. It can only narrow what a change may touch. |
| ` + "`change.auto_merge`" + ` | unset | Not active yet, and gonk merges nothing. Optional, and off unless you set ` + "`mode`" + ` (which needs ` + "`actions.features: true`" + `). ` + "`mode: after-human-approval`" + ` is for gonk pressing merge once a project member other than the bot approved; ` + "`mode: after-agentic-review`" + ` (needs ` + "`actions.reviews: true`" + `) for merging after an independent review on a different model than the author found nothing blocking -- where the operator has configured no such reviewer that mode is unavailable and merges nothing. ` + "`classes`" + ` can only be ` + "`[docs]`" + `, meaning prose files only (` + "`.md`" + `, ` + "`.rst`" + `, ` + "`.txt`" + `, ` + "`.adoc`" + `): a change to source or test files is never merged automatically. ` + "`max_per_day`" + ` (default and maximum 3) caps automatic merges in 24 hours. |
| ` + "`review.targets`" + ` | unset | **Not active yet: nothing in this version of gonk starts this workflow, so this key changes nothing today.** Optional. ` + "`review:`" + ` bounds the independent review that ` + "`actions.reviews`" + ` switches on. ` + "`targets`" + ` can only be ` + "`[gonk]`" + `: merge requests gonk itself opened. Which model does the reviewing is chosen by the operator of this gonk instance, not in this file; writing ` + "`rung`" + ` here is refused. A review counts towards an automatic merge only when the reviewer ran on a different model than the author; no setting changes that, and the review note says which model reviewed. |
| ` + "`review.max_notes`" + ` | unset | Not active yet. Optional, default and maximum 20. Findings one review may report. |
| ` + "`review.max_reviews_per_mr`" + ` | unset | Not active yet. Optional, default and maximum 3. Reviews of one merge request, one per head commit. |
| ` + "`version`" + ` | ` + "`1`" + ` | Config schema version. |

## Which languages gonk assumes here

` + "`scope.profiles`" + ` in the file below is pre-filled with ` + "`[{{.Profiles}}]`" + `.
{{if .Detection.Ref}}
To choose that, gonk checked whether these files exist at the root of
` + "`{{.Detection.Ref}}`" + ` at the moment it rendered this merge request:

| File | Found | If found, proposes |
|---|---|---|
{{- range .Markers}}
| ` + "`{{.File}}`" + ` | {{if .Found}}**yes**{{else}}no{{end}} | ` + "`{{.Profile}}`" + ` |
{{- end}}
{{if .Evidence}}{{range .Evidence}}
- ` + "`{{.Profile}}`" + ` is proposed because ` + "`{{.Marker}}`" + ` exists.{{end}}
- ` + "`docs-md`" + ` is always proposed: the documentation style is not detected.
{{else}}
None of them was found, so the list is gonk's default, ` + "`[{{.Default}}]`" + `.
That default is not a finding about this repository.
{{end}}
What this check cannot see: it asks for those exact names at the repository
root and nothing else. gonk cannot list this repository's files from here, so a
` + "`package.json`" + ` or ` + "`go.mod`" + ` in a subdirectory, a
` + "`requirements-dev.txt`" + `, and any language without one of these files are not
noticed.
{{else}}
gonk did not read this repository's files for this merge request, so that is
its default and not a finding about this repository.
{{end}}
**This is a proposal. Edit the list in this merge request before merging if it
is wrong.** gonk does not detect it again: after the merge, only what
` + "`.gonk.yml`" + ` says counts, and a later change to the list is an ordinary
merge request to that file.

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
	raw, err := c.renderConfig()
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
