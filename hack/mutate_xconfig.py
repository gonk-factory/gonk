#!/usr/bin/env python3
"""mutate_xconfig.py -- "seen red" evidence for gonk-cuwn part X-CONFIG.

Applies one named break at a time to the issue-change / review configuration
surface (`actions.reviews`, the `change:` and `review:` sections and
`auto_merge.max_per_day` in both schemas and both loaders, the folds, the
`issue-change` and `mr-review` triggers with their action gates, ladder, floor
and reservation TTL, the agentic-review merge status, the wire types, the
persisted registration, intake's pre-filters and the onboarding text), runs the
packages that guard it, and requires a test to FAIL; then restores every file
byte for byte (try/finally). A mutation whose text is not found is VACUOUS, one
the tests do not notice SURVIVED, one that only a whole-output golden
comparison notices GOLDEN-ONLY, and one that does not compile NOCOMPILE -- each
is a failure of this script.

The first block is the breaks the plan names for part X-CONFIG (section 6.2)
and the owner decisions of section 8.1: "unknown key accepted", "`reviews` on
without its opt-in dependency accepted", "a ceiling exceeded is accepted",
"`trigger: triage-verdict` is the default or accepted", "a project sets
`review.rung`".

RESEALING. Three files are guarded by a checksum test that fails on ANY edit
(the two JSON Schemas, pkg/meterapi/meterapi.go), and each schema has a
published copy under docs/schemas that must be byte-identical. Left alone,
those drift tests would "kill" every mutant of those files and prove nothing
about behaviour. So after applying a mutant this script re-seals: it copies the
mutated schema over its published copy and rewrites the checksum files. A
mutant of those files is then killed only by a test that noticed what changed.

Usage: hack/mutate_xconfig.py            (all mutations)
       hack/mutate_xconfig.py 0 1 2      (only these indexes)
       hack/mutate_xconfig.py -v ...     (also print each failing test's message)
       hack/mutate_xconfig.py -l         (list the mutations, run nothing)
       hack/mutate_xconfig.py -c         (check that every mutation's text is found; run no test)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate;
its output is pasted into the plan's evidence log. If it is killed mid-run,
`git status` and `git checkout -- .` restore the mutated files.
"""
import hashlib
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
PKGS = ['./pkg/gonkcfg/', './pkg/opercfg/', './pkg/atags/', './pkg/rung/', './pkg/meterapi/',
        './pkg/intake/', './internal/meter/service/', './internal/meter/store/']

CH = 'pkg/gonkcfg/change.go'
RV = 'pkg/gonkcfg/review.go'
PF = 'pkg/gonkcfg/pipelinefix.go'
GC = 'pkg/gonkcfg/config.go'
RS = 'pkg/gonkcfg/resolve.go'
PS = 'pkg/gonkcfg/gonk-config.v1.schema.json'
OS_ = 'pkg/opercfg/gonk-operator.v1.schema.json'
OC = 'pkg/opercfg/opercfg.go'
AT = 'pkg/atags/tags.go'
RD = 'pkg/rung/decide.go'
MA = 'pkg/meterapi/meterapi.go'
SV = 'internal/meter/service/service.go'
PW = 'internal/meter/store/postgres_wire.go'
ST = 'pkg/intake/state.go'
DI = 'pkg/intake/dispatch.go'
RE = 'pkg/intake/render.go'

# WHICH PACKAGES A MUTANT IS RUN AGAINST: every package of PKGS that imports
# the mutated file's package, directly or not (`go list -test -deps`), plus the
# package itself. A test that cannot see the mutated code cannot kill it, so
# nothing is lost; a mutant of the onboarding text runs one package, not eight.
G, O, A, R, M, I, S, T = PKGS
DEPENDENTS = {
    CH: [G, O, R, M, I, S, T], RV: [G, O, R, M, I, S, T], PF: [G, O, R, M, I, S, T],
    GC: [G, O, R, M, I, S, T], RS: [G, O, R, M, I, S, T], PS: [G, O, R, M, I, S, T],
    OC: [O, R, S, T], OS_: [O, R, S, T],
    AT: [A, R, I, S, T],
    RD: [R, S, T],
    MA: [M, R, I, S, T],
    SV: [S],
    PW: [T, S],
    ST: [I], DI: [I], RE: [I],
}

# Tests that compare whole rendered output with a stored file. They fail on
# ANY change to that output, as the checksum tests do, so a mutant that ONLY
# they notice has not been shown to matter: it is reported GOLDEN-ONLY and
# counts as not killed. (The files cannot be re-sealed per mutant the way a
# checksum can: regenerating a golden needs the test run that is the evidence.)
GOLDEN_TESTS = {"TestRenderMatchesGolden", "TestWireContractLiterals", "TestExistingConfigsResolveByteIdentically"}

# file -> (checksum file, published copy or None)
SEALED = {
    PS: ('pkg/gonkcfg/testdata/schema.v1.sha256', 'docs/schemas/gonk-config.v1.schema.json'),
    OS_: ('pkg/opercfg/testdata/schema.v1.sha256', 'docs/schemas/gonk-operator.v1.schema.json'),
    MA: ('pkg/meterapi/testdata/contract.sha256', None),
}

# ---- text the mutants are cut from -------------------------------------
CHANGE_SECTION = '"change": {\n      "type": "object",\n      "additionalProperties": false,'
REVIEW_SECTION = '"review": {\n      "type": "object",\n      "additionalProperties": false,'
CHANGE_MERGE = ('"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 300,', '"auto_merge": {\n          "type": "object",\n          "additionalProperties": false,')
OP_CHANGE_SECTION = '"change": {\n          "type": "object",\n          "additionalProperties": false,'
OP_REVIEW_SECTION = '"review": {\n          "type": "object",\n          "additionalProperties": false,'
REVIEWS_IF = ('          "if": { "properties": { "reviews": { "const": true } }, "required": ["reviews"] },\n'
              '          "then": { "anyOf": [\n'
              '            { "properties": { "pipeline_fixes": { "const": true } }, "required": ["pipeline_fixes"] },\n'
              '            { "properties": { "features": { "const": true } }, "required": ["features"] }\n'
              '          ] }\n')
MERGE_IF_FEATURES = '"then": { "properties": { "actions": { "properties": { "features": { "const": true } }, "required": ["features"] } }, "required": ["actions"] }'
MERGE_IF_REVIEWS = '"then": { "properties": { "actions": { "properties": { "reviews": { "const": true } }, "required": ["reviews"] } }, "required": ["actions"] }'
GO_REVIEWS_REQ = "\tif on(p.Actions.Reviews) && !on(p.Actions.PipelineFixes) && !on(p.Actions.Features) {"
GO_MERGE_FEATURES = "\t\tif !on(p.Actions.Features) {"
GO_MERGE_REVIEWS = "\t\tif *m == AutoMergeAfterAgenticReview && !on(p.Actions.Reviews) {"
TRIGGER_ENUM = '"items": { "enum": ["label", "command", "mention"] }'
CREATE_ENUM = '"items": { "enum": ["source", "test", "docs"] }'
TARGET_ENUM = '"items": { "enum": ["gonk"] }'
OP_RUNG = '            "rung": { "type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$",'
RESOLVE_REVIEWS = ("\teff.Actions.Reviews = (eff.Actions.PipelineFixes || eff.Actions.Features) &&\n"
                   "\t\tandAction(project.Actions.Reviews, instance.Actions.Reviews, group.Actions.Reviews)\n")
CHANGE_GATE = "\t\treturn eff.Actions.Features, true\n"
REVIEW_GATE = "\t\treturn eff.Actions.Reviews, true\n"
FLOORS = ("\tatags.TriggerPipelineFix:       1,\n\tatags.TriggerIssueChange:       1,\n}\n\n// LadderFloor returns")
RETRIES = ("\tatags.TriggerPipelineFix:       1,\n\tatags.TriggerIssueChange:       1,\n\tatags.TriggerMRReview:          1,\n}")
TTL_CHANGE = ("\tif trigger == atags.TriggerIssueChange {\n"
              "\t\treturn base + min(max(eff.Change.VerifyTimeout, 0), gonkcfg.ChangeVerifyTimeoutCeiling)\n\t}\n")
TTL_RET = "\t\treturn base + min(max(eff.Change.VerifyTimeout, 0), gonkcfg.ChangeVerifyTimeoutCeiling)\n"
LADDER_FOR = ("\tif trigger != atags.TriggerMRReview {\n\t\treturn eff.Ladder\n\t}\n"
              "\tif eff.Review.Rung == \"\" {\n\t\treturn nil\n\t}\n\treturn []string{eff.Review.Rung}\n")
DECIDE_LADDER = "\tladder := LadderFor(in.Effective, in.Trigger)\n"
NO_RUNG_DENY = "\tif in.Trigger == atags.TriggerMRReview && len(ladder) == 0 {"
SVC_RESOLVE = "eff := rung.ResolveReviewMerge(gonkcfg.Resolve(cfg.Instance, group, *pc), cfg.Catalog)"
MAYCHANGE = "\t\treturn c.eff().Actions.Features\n\t}\n\treturn false\n}\n\n// MayReview"
MAYREVIEW = "\t\treturn e.Actions.Reviews && (e.Actions.PipelineFixes || e.Actions.Features) && e.Review.Rung != \"\"\n"
NA_ROW = "**Not active yet: nothing in this version of gonk starts this workflow, so this key changes nothing today.** "

# (name, [(file, old, new), ...])
muts = [
 # ---- the breaks the plan's row names (6.2) ----
 ("PLAN: unknown key accepted (.gonk.yml change section)",
  [(PS, CHANGE_SECTION, CHANGE_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (.gonk.yml change.auto_merge)",
  [(PS, CHANGE_MERGE[1] + '\n          "description": "Automatic merge of a change', CHANGE_MERGE[1].replace("false", "true") + '\n          "description": "Automatic merge of a change')]),
 ("PLAN: unknown key accepted (.gonk.yml review section)",
  [(PS, REVIEW_SECTION, REVIEW_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (operator change section)",
  [(OS_, OP_CHANGE_SECTION, OP_CHANGE_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (operator change.auto_merge)",
  [(OS_, '"auto_merge": {\n              "type": "object",\n              "additionalProperties": false,\n              "description": "Automatic merge of a change',
         '"auto_merge": {\n              "type": "object",\n              "additionalProperties": true,\n              "description": "Automatic merge of a change')]),
 ("PLAN: unknown key accepted (operator review section)",
  [(OS_, OP_REVIEW_SECTION, OP_REVIEW_SECTION.replace("false", "true"))]),
 ("PLAN: reviews on without its dependency accepted (schema if/then removed)",
  [(PS, REVIEWS_IF, '          "if": false\n')]),
 ("PLAN: reviews on without its dependency accepted (Go check removed)",
  [(RV, GO_REVIEWS_REQ, "\tif false {")]),
 ("PLAN: reviews on without its dependency accepted (BOTH guards removed: the end-to-end registration test)",
  [(PS, REVIEWS_IF, '          "if": false\n'), (RV, GO_REVIEWS_REQ, "\tif false {")]),
 ("PLAN: reviews on without its dependency accepted (an author workflow written `false` counts)",
  [(PS, '            { "properties": { "pipeline_fixes": { "const": true } }, "required": ["pipeline_fixes"] },\n            { "properties": { "features": { "const": true } }, "required": ["features"] }\n',
        '            { "required": ["pipeline_fixes"] },\n            { "required": ["features"] }\n'),
   (RV, GO_REVIEWS_REQ, "\tif on(p.Actions.Reviews) && p.Actions.PipelineFixes == nil && p.Actions.Features == nil {")]),
 ("PLAN: reviews on without its dependency accepted (triage counts as an author workflow)",
  [(PS, '            { "properties": { "features": { "const": true } }, "required": ["features"] }\n          ] }\n',
        '            { "properties": { "features": { "const": true } }, "required": ["features"] },\n            { "properties": { "triage": { "const": true } }, "required": ["triage"] }\n          ] }\n'),
   (RV, GO_REVIEWS_REQ, "\tif on(p.Actions.Reviews) && !on(p.Actions.PipelineFixes) && !on(p.Actions.Features) && !on(p.Actions.Triage) {")]),
 ("PLAN: reviews without opt-in (the resolved action ignores the project's opt-in)",
  [(RS, RESOLVE_REVIEWS, "\teff.Actions.Reviews = eff.Actions.PipelineFixes || eff.Actions.Features\n")]),
 ("PLAN: reviews without opt-in (mr-review gated by nothing)",
  [(RD, REVIEW_GATE, "\t\treturn true, false\n")]),
 ("PLAN: reviews without opt-in (mr-review gated by actions.features)",
  [(RD, REVIEW_GATE, "\t\treturn eff.Actions.Features, true\n")]),
 ("PLAN: reviews without opt-in (mr-review gated by actions.pipeline_fixes)",
  [(RD, REVIEW_GATE, "\t\treturn eff.Actions.PipelineFixes, true\n")]),
 ("PLAN: a ceiling exceeded is accepted (change.max_files, schema AND Go)",
  [(PS, '"max_files": { "type": "integer", "minimum": 1, "maximum": 8,', '"max_files": { "type": "integer", "minimum": 1, "maximum": 80,'),
   (CH, "\tChangeMaxFilesCeiling        = 8\n", "\tChangeMaxFilesCeiling        = 80\n")]),
 ("PLAN: a ceiling exceeded is accepted (change.max_changed_lines, schema AND Go)",
  [(PS, '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 300,', '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 3000,'),
   (CH, "\tChangeMaxChangedLinesCeiling = 300\n", "\tChangeMaxChangedLinesCeiling = 3000\n")]),
 ("PLAN: a ceiling exceeded is accepted (review.max_notes, schema AND Go)",
  [(PS, '"max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '"max_notes": { "type": "integer", "minimum": 1, "maximum": 200,'),
   (RV, "\tReviewMaxNotesCeiling        = 20\n", "\tReviewMaxNotesCeiling        = 200\n")]),
 ("PLAN: a ceiling exceeded is accepted (Go validation of every change number)",
  [(CH, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d", "\t\tif n.v != nil && (*n.v < 1) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d")]),
 ("PLAN: a ceiling exceeded is CLAMPED, not refused (change.max_files: the schema bound is dropped and Go lowers the value)",
  [(PS, '"max_files": { "type": "integer", "minimum": 1, "maximum": 8,', '"max_files": { "type": "integer", "minimum": 1,'),
   (CH, "\tfor _, n := range p.ints() {\n\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d",
        "\tfor _, n := range p.ints() {\n\t\tif n.v != nil && *n.v > n.ceiling {\n\t\t\t*n.v = n.ceiling\n\t\t}\n\t\tif n.v != nil && (*n.v < 1) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d")]),
 ("PLAN: triage-verdict accepted (.gonk.yml schema)",
  [(PS, TRIGGER_ENUM, '"items": { "enum": ["label", "command", "mention", "triage-verdict"] }')]),
 ("PLAN: triage-verdict accepted (operator schema)",
  [(OS_, TRIGGER_ENUM, '"items": { "enum": ["label", "command", "mention", "triage-verdict"] }')]),
 ("PLAN: triage-verdict accepted (Go: the trigger menu gains it)",
  [(CH, "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}",
        "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention, \"triage-verdict\"}")]),
 ("PLAN: triage-verdict accepted (everywhere: schema, menu)",
  [(PS, TRIGGER_ENUM, '"items": { "enum": ["label", "command", "mention", "triage-verdict"] }'),
   (CH, "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}",
        "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention, \"triage-verdict\"}")]),
 ("PLAN: triage-verdict is the default",
  [(CH, "\tdefaultChangeTriggers    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}",
        "\tdefaultChangeTriggers    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention, \"triage-verdict\"}"),
   (CH, "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}",
        "\tchangeTriggerOrder    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention, \"triage-verdict\"}")]),
 ("PLAN: the plan's old `trigger:` key is accepted",
  [(PS, '        "create_classes": {\n          "type": "array",', '        "trigger": { "enum": ["label", "triage-verdict"] },\n        "create_classes": {\n          "type": "array",')]),
 ("PLAN: a project sets review.rung (the project schema offers the key)",
  [(PS, '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '        "rung": { "type": "string" },\n        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,')]),
 ("PLAN: a project sets review.rung (Go check removed)",
  [(RV, "\tif p.Rung != nil && layer == scope.LayerProject {", "\tif false {")]),
 ("PLAN: a project sets review.rung (BOTH guards removed: the end-to-end registration test)",
  [(PS, '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '        "rung": { "type": "string" },\n        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,'),
   (RV, "\tif p.Rung != nil && layer == scope.LayerProject {", "\tif false {")]),
 ("PLAN: a project sets review.rung (the fold USES the project's rung)",
  [(RV, "\tif op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {\n\t\teff.Rung = *op.Rung\n\t}\n",
        "\tif op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {\n\t\teff.Rung = *op.Rung\n\t}\n\tif project.Rung != nil {\n\t\teff.Rung = *project.Rung\n\t}\n")]),
 ("PLAN: a project sets review.rung (a project rung fills in when the operator named none)",
  [(RV, "\tif op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {\n\t\teff.Rung = *op.Rung\n\t}\n",
        "\tif op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {\n\t\teff.Rung = *op.Rung\n\t} else if project.Rung != nil {\n\t\teff.Rung = *project.Rung\n\t}\n")]),

 # ---- owner decisions (8.1) ----
 ("OWNER Q2: the minimum access level is Developer",
  [(CH, "const ChangeMinAccessLevel = 40", "const ChangeMinAccessLevel = 30")]),
 ("OWNER Q5: review.targets accepts human (.gonk.yml schema)",
  [(PS, TARGET_ENUM, '"items": { "enum": ["gonk", "human"] }')]),
 ("OWNER Q5: review.targets accepts human (operator schema)",
  [(OS_, TARGET_ENUM, '"items": { "enum": ["gonk", "human"] }')]),
 ("OWNER Q5: review.targets accepts human (Go)",
  [(RV, "var reviewTargetOrder = []string{ReviewTargetGonk}", "var reviewTargetOrder = []string{ReviewTargetGonk, \"human\"}")]),
 ("OWNER Q4: change auto-merge is ON by default",
  [(CH, "\tmode := AutoMergeOff\n\tif proj != nil {", "\tmode := AutoMergeAfterHumanApproval\n\tif proj != nil {")]),
 ("OWNER Q4: change.auto_merge.classes accepts source (schema)",
  [(PS, '"items": { "enum": ["docs"] }', '"items": { "enum": ["docs", "source", "test"] }')]),
 ("OWNER Q4: change.auto_merge.classes accepts ci (Go)",
  [(CH, "\tchangeMergeClassOrder = []string{AutoMergeClassDocs}", "\tchangeMergeClassOrder = []string{AutoMergeClassDocs, AutoMergeClassCI}")]),
 ("OWNER Q4: the same model silently satisfies after-agentic-review (the model comparison always passes)",
  [(RD, "\t\t\tif spec, known := catalog[r]; known && spec.Model != reviewer.Model {", "\t\t\tif spec, known := catalog[r]; known && (spec.Model != reviewer.Model || true) {")]),
 ("OWNER Q4: the same model silently satisfies after-agentic-review (no-distinct-model reads as available)",
  [(RD, "\t\treturn gonkcfg.ReviewMergeStatus{Reason: gonkcfg.ReviewMergeNoDistinctModel}", "\t\treturn gonkcfg.ReviewMergeStatus{Available: true}")]),
 ("OWNER Q4: the comparison is by RUNG NAME, so one model under two rung names counts as distinct",
  [(RD, "\t\t\tif spec, known := catalog[r]; known && spec.Model != reviewer.Model {", "\t\t\tif spec, known := catalog[r]; known && spec.Name != reviewer.Name {")]),
 ("OWNER Q4: a rung BELOW the author's floor counts as an author rung",
  [(RD, "\t\tfor _, r := range eff.Ladder[floor:] {", "\t\tfor _, r := range eff.Ladder {")]),
 ("OWNER Q4: no reviewer rung, and the mode is available anyway",
  [(RV, "\tcase r.Rung == \"\":\n\t\treturn ReviewMergeStatus{Reason: ReviewMergeNoRung}\n", ""),
   (RD, "\t\treviewer, ok := catalog[eff.Review.Rung]\n\t\tif !ok {\n\t\t\treturn gonkcfg.ReviewMergeStatus{Reason: gonkcfg.ReviewMergeRungUnknown}\n\t\t}\n",
        "\t\treviewer, ok := catalog[eff.Review.Rung]\n\t\tif !ok {\n\t\t\treturn gonkcfg.ReviewMergeStatus{Available: true}\n\t\t}\n")]),
 ("OWNER Q4: reviews off, and the mode is available anyway",
  [(RV, "\tcase !reviewsOn:\n\t\treturn ReviewMergeStatus{Reason: ReviewMergeReviewsOff}\n", "\tcase !reviewsOn && r.Rung == \"\":\n\t\treturn ReviewMergeStatus{Reason: ReviewMergeReviewsOff}\n")]),
 ("OWNER Q4: gonkcfg.Resolve alone calls the mode available",
  [(RV, "\treturn ReviewMergeStatus{Reason: ReviewMergeModelsNotCompared}\n}", "\treturn ReviewMergeStatus{Available: true}\n}")]),
 ("OWNER Q4: the meter does not compare models (ResolveReviewMerge is not called)",
  [(SV, SVC_RESOLVE, "eff := gonkcfg.Resolve(cfg.Instance, group, *pc)")]),
 ("OWNER Q4: the meter compares against an EMPTY catalog",
  [(SV, SVC_RESOLVE, "eff := rung.ResolveReviewMerge(gonkcfg.Resolve(cfg.Instance, group, *pc), nil)")]),
 ("OWNER Q4: the change workflow's status is computed for the wrong trigger's floor",
  [(RD, "\teff.Review.ChangeMerge = status(eff.Review.ChangeMerge, atags.TriggerIssueChange)", "\teff.Review.ChangeMerge = status(eff.Review.ChangeMerge, atags.TriggerIssueTriage)")]),
 ("OWNER Q4: the pipeline-fix workflow's status is not completed",
  [(RD, "\teff.Review.PipelineFixMerge = status(eff.Review.PipelineFixMerge, atags.TriggerPipelineFix)\n", "")]),
 ("OWNER Q4: UsableAutoMergeMode returns the agentic mode although it is unavailable",
  [(RV, "\tcase am.Mode == AutoMergeAfterAgenticReview && !st.Available:", "\tcase am.Mode == AutoMergeAfterAgenticReview && !st.Available && false:")]),
 ("OWNER Q4: UsableAutoMergeMode drops the reason",
  [(RV, "\t\treturn AutoMergeOff, st.Reason\n", "\t\treturn AutoMergeOff, \"\"\n")]),
 ("OWNER Q4: UsableAutoMergeMode ignores that the workflow is off",
  [(RV, "\tcase !actionOn:\n\t\treturn AutoMergeOff, \"action-off\"\n", "")]),
 ("OWNER Q4: UsableAutoMergeMode passes an unknown mode through",
  [(RV, "\tcase !autoMergeModes[am.Mode]:\n\t\treturn AutoMergeOff, \"unknown-mode\"\n", "")]),
 ("OWNER Q4: UsableAutoMergeMode holds human approval to the review status too",
  [(RV, "\tcase am.Mode == AutoMergeAfterAgenticReview && !st.Available:", "\tcase !st.Available:")]),
 ("OWNER Q4: which rung reviews -- mr-review runs on the PROJECT'S ladder",
  [(RD, LADDER_FOR, "\t_ = trigger\n\treturn eff.Ladder\n")]),
 ("OWNER Q4: which rung reviews -- with no reviewer rung, mr-review falls back to the project's ladder",
  [(RD, "\tif eff.Review.Rung == \"\" {\n\t\treturn nil\n\t}\n\treturn []string{eff.Review.Rung}\n", "\tif eff.Review.Rung == \"\" {\n\t\treturn eff.Ladder\n\t}\n\treturn []string{eff.Review.Rung}\n"),
   (RD, NO_RUNG_DENY, "\tif false {")]),
 ("OWNER Q4: which rung reviews -- the reviewer rung is APPENDED to the project's ladder (a review can run below it)",
  [(RD, "\treturn []string{eff.Review.Rung}\n", "\treturn append(append([]string{}, eff.Ladder...), eff.Review.Rung)\n")]),
 ("OWNER Q4: which rung reviews -- every trigger runs on the reviewer rung",
  [(RD, "\tif trigger != atags.TriggerMRReview {\n\t\treturn eff.Ladder\n\t}\n", "")]),
 ("OWNER Q4: which rung reviews -- Decide ignores the per-trigger ladder",
  [(RD, DECIDE_LADDER, "\tladder := in.Effective.Ladder\n")]),
 ("OWNER Q4: the no-reviewer denial is removed (falls to a generic ladder message)",
  [(RD, NO_RUNG_DENY, "\tif false {")]),
 ("OWNER Q4: two operator layers naming different rungs: the more specific one wins",
  [(RV, "\tnone := \"\"\n\treturn &none\n}", "\treturn b\n}")]),
 ("OWNER Q4: two operator layers naming different rungs: the coarser one wins",
  [(RV, "\tnone := \"\"\n\treturn &none\n}", "\treturn a\n}")]),
 ("OWNER Q4: a rung conflict folds to nil (silent), so a third layer brings a reviewer back",
  [(RV, "\tnone := \"\"\n\treturn &none\n}", "\treturn nil\n}")]),
 ("OWNER Q4: review.rung is not checked against the rung catalog",
  [(OC, "\t\tif _, ok := oc.Catalog[*r]; !ok {\n\t\t\treturn fmt.Errorf(\"operator config: %s review.rung", "\t\tif _, ok := oc.Catalog[*r]; !ok && false {\n\t\t\treturn fmt.Errorf(\"operator config: %s review.rung")]),

 # ---- owner decision (after the plan): NO distinct-model opt-out, in any layer ----
 ("OWNER: a project key waives the distinct model (.gonk.yml schema offers require_distinct_model)",
  [(PS, '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '        "require_distinct_model": { "type": "boolean" },\n        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,')]),
 ("OWNER: an operator key waives the distinct model (operator schema offers require_distinct_model)",
  [(OS_, '            "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '            "require_distinct_model": { "type": "boolean" },\n            "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,')]),
 ("OWNER: the wire gains a same-model switch",
  [(MA, '\tRung            string      `json:"rung"`\n', '\tRung            string      `json:"rung"`\n\tSameModelAllowed bool `json:"same_model_allowed"`\n')]),

 # ---- the requires-relations of change.auto_merge ----
 ("requires: change auto-merge without features (schema then removed)",
  [(PS, MERGE_IF_FEATURES, '"then": true')]),
 ("requires: change auto-merge without features (Go check removed)",
  [(CH, GO_MERGE_FEATURES, "\t\tif false {")]),
 ("requires: change auto-merge without features (BOTH removed)",
  [(PS, MERGE_IF_FEATURES, '"then": true'), (CH, GO_MERGE_FEATURES, "\t\tif false {")]),
 ("requires: change agentic auto-merge without reviews (schema then removed)",
  [(PS, MERGE_IF_REVIEWS, '"then": true')]),
 ("requires: change agentic auto-merge without reviews (Go check removed)",
  [(CH, GO_MERGE_REVIEWS, "\t\tif false {")]),
 ("requires: change agentic auto-merge without reviews (BOTH removed)",
  [(PS, MERGE_IF_REVIEWS, '"then": true'), (CH, GO_MERGE_REVIEWS, "\t\tif false {")]),
 ("requires: `features: false` counts as present (schema AND Go)",
  [(PS, MERGE_IF_FEATURES, '"then": { "properties": { "actions": { "required": ["features"] } }, "required": ["actions"] }'),
   (CH, GO_MERGE_FEATURES, "\t\tif p.Actions.Features == nil {")]),
 ("requires: the pipeline-fix block's agentic mode is now REFUSED without reviews (breaks existing configs)",
  [(CH, "\treturn nil\n}\n\n// EffectiveChange is the folded",
        "\tif m := p.PipelineFix.AutoMerge.Mode; m != nil && *m == AutoMergeAfterAgenticReview && !on(p.Actions.Reviews) {\n\t\treturn fmt.Errorf(\"pipeline_fix: auto_merge.mode needs actions.reviews\")\n\t}\n\treturn nil\n}\n\n// EffectiveChange is the folded")]),
 ("load: CheckChange is not called",
  [(GC, "\tif err := CheckChange(pc.Policy); err != nil {", "\tif err := error(nil); err != nil {")]),
 # CheckReview holds no rule the schema lacks (unlike CheckChange, whose
 # verify_timeout ceiling is Go-only), so "Load does not call it" is invisible
 # while the schema stands. What must be seen is that Load STILL refuses when
 # the schema's half is gone -- i.e. that the call is there as the second guard.
 ("load: CheckReview is not called AND the schema's reviews rule is gone (Load accepts reviews with no author workflow)",
  [(GC, "\tif err := CheckReview(pc.Policy); err != nil {", "\tif err := error(nil); err != nil {"), (PS, REVIEWS_IF, '          "if": false\n')]),
 ("load: CheckReview is not called AND the project schema offers review.rung (Load accepts a project's reviewer)",
  [(GC, "\tif err := CheckReview(pc.Policy); err != nil {", "\tif err := error(nil); err != nil {"),
   (PS, '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '        "rung": { "type": "string" },\n        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20,')]),
 ("load: the project's change section is validated as an operator's (paths refused)",
  [(CH, "\tif err := p.Change.Validate(scope.LayerProject); err != nil {", "\tif err := p.Change.Validate(scope.LayerOperator); err != nil {")]),
 ("load: the project's review section is validated as an operator's (rung accepted in Go)",
  [(RV, "\treturn p.Review.Validate(scope.LayerProject)", "\treturn p.Review.Validate(scope.LayerOperator)")]),

 # ---- the schema, key by key ----
 ("schema: create_classes accepts deps and ci",
  [(PS, CREATE_ENUM, '"items": { "enum": ["source", "test", "docs", "deps", "ci", "other"] }')]),
 ("schema: require_docs accepts sometimes",
  [(PS, '"require_docs": { "enum": ["never", "when-labelled", "always"]', '"require_docs": { "enum": ["never", "when-labelled", "always", "sometimes"]')]),
 ("schema: require_docs accepts any type",
  [(PS, '"require_docs": { "enum": ["never", "when-labelled", "always"],', '"require_docs": {')]),
 ("schema: docs_labels accepts any number of labels",
  [(PS, '          "maxItems": 16,\n', '')]),
 ("schema: docs_labels accepts blank and padded labels",
  [(PS, '"items": { "type": "string", "minLength": 1, "maxLength": 255, "pattern": "^\\\\S(.*\\\\S)?$" }', '"items": { "type": "string", "maxLength": 255 }')]),
 ("schema: docs_labels accepts labels of any length",
  [(PS, '"items": { "type": "string", "minLength": 1, "maxLength": 255, "pattern": "^\\\\S(.*\\\\S)?$" }', '"items": { "type": "string", "minLength": 1, "pattern": "^\\\\S(.*\\\\S)?$" }')]),
 ("schema: docs_labels accepts a repeated label",
  [(PS, '          "maxItems": 16,\n          "uniqueItems": true,\n', '          "maxItems": 16,\n')]),
 ("schema: docs_labels accepts non-strings",
  [(PS, '"items": { "type": "string", "minLength": 1, "maxLength": 255, "pattern": "^\\\\S(.*\\\\S)?$" }', '"items": {}')]),
 ("schema: require_test_report accepts any type",
  [(PS, '"require_test_report": { "type": "boolean",', '"require_test_report": {')]),
 ("schema: change.max_attempts ceiling raised",
  [(PS, '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Attempts at one issue', '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Attempts at one issue')]),
 ("schema: change.max_attempts may be zero",
  [(PS, '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Attempts at one issue', '"max_attempts": { "type": "integer", "minimum": 0, "maximum": 3, "description": "Attempts at one issue')]),
 ("schema: max_open ceiling raised",
  [(PS, '"max_open": { "type": "integer", "minimum": 1, "maximum": 3,', '"max_open": { "type": "integer", "minimum": 1, "maximum": 30,')]),
 ("schema: max_open may be zero",
  [(PS, '"max_open": { "type": "integer", "minimum": 1,', '"max_open": { "type": "integer", "minimum": 0,')]),
 ("schema: change.max_files may be zero",
  [(PS, '"max_files": { "type": "integer", "minimum": 1, "maximum": 8,', '"max_files": { "type": "integer", "minimum": 0, "maximum": 8,')]),
 ("schema: change.max_changed_lines accepts a fraction",
  [(PS, '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 300,', '"max_changed_lines": { "type": "number", "minimum": 1, "maximum": 300,')]),
 ("schema: change.verify_timeout accepts a bare number",
  [(PS, '"verify_timeout": { "type": "string", "pattern": "^[0-9]+(s|m|h)$", "description": "How long gonk waits for the change', '"verify_timeout": { "description": "How long gonk waits for the change')]),
 ("schema: change.verify_timeout accepts any string",
  [(PS, '"verify_timeout": { "type": "string", "pattern": "^[0-9]+(s|m|h)$", "description": "How long gonk waits for the change', '"verify_timeout": { "type": "string", "description": "How long gonk waits for the change')]),
 ("schema: change.paths accepts any list",
  [(PS, '"paths": { "$ref": "#/$defs/scopePatterns", "description": "Optional. Extra narrowing: when set, a change', '"paths": { "type": "array", "description": "Optional. Extra narrowing: when set, a change')]),
 ("schema: change.auto_merge.mode accepts always",
  [(PS, '"mode": { "enum": ["off", "after-human-approval", "after-agentic-review"], "description": "Default off: a human merges every change', '"mode": { "enum": ["off", "after-human-approval", "after-agentic-review", "always"], "description": "Default off: a human merges every change')]),
 ("schema: change.auto_merge.max_per_day ceiling raised",
  [(PS, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges gonk may make on this project', '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Automatic merges gonk may make on this project')]),
 ("schema: change.auto_merge.max_per_day may be zero",
  [(PS, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges gonk may make on this project', '"max_per_day": { "type": "integer", "minimum": 0, "maximum": 3, "description": "Automatic merges gonk may make on this project')]),
 ("schema: pipeline_fix.auto_merge.max_per_day ceiling raised",
  [(PS, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges of fix merge requests', '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Automatic merges of fix merge requests')]),
 ("schema: pipeline_fix.auto_merge.max_per_day accepts any type",
  [(PS, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges of fix merge requests', '"max_per_day": { "description": "Automatic merges of fix merge requests')]),
 ("schema: pipeline_fix.auto_merge loses max_per_day (an existing block refuses the new key)",
  [(PS, ',\n            "max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges of fix merge requests gonk may make on this project in any 24 hours. Default and ceiling 3. MINIMUM across layers; a value above the ceiling is refused." }', '')]),
 ("schema: review.max_notes may be zero",
  [(PS, '"max_notes": { "type": "integer", "minimum": 1,', '"max_notes": { "type": "integer", "minimum": 0,')]),
 ("schema: review.max_reviews_per_mr ceiling raised",
  [(PS, '"max_reviews_per_mr": { "type": "integer", "minimum": 1, "maximum": 3,', '"max_reviews_per_mr": { "type": "integer", "minimum": 1, "maximum": 30,')]),
 ("schema: review.max_reviews_per_mr may be zero",
  [(PS, '"max_reviews_per_mr": { "type": "integer", "minimum": 1,', '"max_reviews_per_mr": { "type": "integer", "minimum": 0,')]),
 ("schema: actions.reviews accepts any type",
  [(PS, '"reviews": { "type": "boolean", "description": "Default false.', '"reviews": { "description": "Default false.')]),
 ("schema: actions.reviews does not say it is not active",
  [(PS, 'Bounds are the `review:` section. An operator layer may veto it. NOT ACTIVE YET: this version of gonk validates and resolves the key and nothing dispatches the workflow." }',
        'Bounds are the `review:` section. An operator layer may veto it." }')]),
 ("schema: the change section does not say it is not active",
  [(PS, 'An invalid section makes the project\'s configuration invalid (no dispatch, no spend). NOT ACTIVE YET: this version of gonk validates and resolves the key and nothing dispatches the workflow. Reference: docs/reference/gonk-change.md.',
        'An invalid section makes the project\'s configuration invalid (no dispatch, no spend). Reference: docs/reference/gonk-change.md.')]),
 ("schema: a change key loses its description (owner rule: documented where the schema lives)",
  [(PS, '"max_open": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Open change merge requests gonk may have on this project at once. Default 1, ceiling 3. Further requests wait. MINIMUM across layers; a value above the ceiling is refused." }',
        '"max_open": { "type": "integer", "minimum": 1, "maximum": 3 }')]),
 ("schema: a review key loses its description",
  [(PS, '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20, "description": "Findings one review may report. Default and ceiling 20. A review with more is refused as a whole. MINIMUM across layers; a value above the ceiling is refused." },\n        "max_reviews_per_mr"',
        '        "max_notes": { "type": "integer", "minimum": 1, "maximum": 20 },\n        "max_reviews_per_mr"')]),
 ("operator schema: change gains the project-only paths",
  [(OS_, '            "auto_merge": {\n              "type": "object",\n              "additionalProperties": false,\n              "description": "Automatic merge of a change',
         '            "paths": { "$ref": "#/$defs/scopePatterns" },\n            "auto_merge": {\n              "type": "object",\n              "additionalProperties": false,\n              "description": "Automatic merge of a change')]),
 ("operator schema: create_classes accepts deps",
  [(OS_, CREATE_ENUM, '"items": { "enum": ["source", "test", "docs", "deps"] }')]),
 ("operator schema: require_docs accepts sometimes",
  [(OS_, '"require_docs": { "enum": ["never", "when-labelled", "always"]', '"require_docs": { "enum": ["never", "when-labelled", "always", "sometimes"]')]),
 ("operator schema: change.max_changed_lines ceiling raised",
  [(OS_, '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 300,', '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 3000,')]),
 ("operator schema: change.max_files ceiling raised",
  [(OS_, '"max_files": { "type": "integer", "minimum": 1, "maximum": 8,', '"max_files": { "type": "integer", "minimum": 1, "maximum": 80,')]),
 ("operator schema: change.max_attempts ceiling raised",
  [(OS_, '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Attempts at one issue', '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Attempts at one issue')]),
 ("operator schema: max_open ceiling raised",
  [(OS_, '"max_open": { "type": "integer", "minimum": 1, "maximum": 3,', '"max_open": { "type": "integer", "minimum": 1, "maximum": 30,')]),
 ("operator schema: change.auto_merge.classes accepts ci",
  [(OS_, '"items": { "enum": ["docs"] }', '"items": { "enum": ["docs", "ci"] }')]),
 ("operator schema: change.auto_merge.max_per_day ceiling raised",
  [(OS_, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges gonk may make on this project', '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Automatic merges gonk may make on this project')]),
 ("operator schema: pipeline_fix.auto_merge.max_per_day ceiling raised",
  [(OS_, '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Automatic merges of fix merge requests', '"max_per_day": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Automatic merges of fix merge requests')]),
 ("operator schema: review.rung accepts any string",
  [(OS_, OP_RUNG, '            "rung": { "type": "string",')]),
 ("operator schema: review.rung accepts a list of rungs",
  [(OS_, OP_RUNG, '            "rung": {')]),
 ("operator schema: review.max_notes ceiling raised",
  [(OS_, '"max_notes": { "type": "integer", "minimum": 1, "maximum": 20,', '"max_notes": { "type": "integer", "minimum": 1, "maximum": 200,')]),
 ("operator schema: review.max_reviews_per_mr ceiling raised",
  [(OS_, '"max_reviews_per_mr": { "type": "integer", "minimum": 1, "maximum": 3,', '"max_reviews_per_mr": { "type": "integer", "minimum": 1, "maximum": 30,')]),
 ("operator schema: the reviews veto accepts any type",
  [(OS_, '"reviews": { "type": "boolean", "description": "Operator VETO', '"reviews": { "description": "Operator VETO')]),
 ("operator schema: docs_labels accepts blank labels",
  [(OS_, '"items": { "type": "string", "minLength": 1, "maxLength": 255, "pattern": "^\\\\S(.*\\\\S)?$" }', '"items": { "type": "string" }')]),

 # ---- the Go half of validation ----
 ("validate(change): list members are not checked",
  [(CH, "\t\t\tif !contains(l.allowed, v) {\n\t\t\t\treturn fmt.Errorf(\"change: %s: %q is not one of %v\", l.key, v, l.allowed)", "\t\t\tif false {\n\t\t\t\treturn fmt.Errorf(\"change: %s: %q is not one of %v\", l.key, v, l.allowed)")]),
 ("validate(change): require_docs is not checked",
  [(CH, "\t\tif _, ok := requireDocsRank[*v]; !ok {\n\t\t\treturn fmt.Errorf(\"change: require_docs", "\t\tif false {\n\t\t\treturn fmt.Errorf(\"change: require_docs")]),
 ("validate(change): the number of docs labels is not checked",
  [(CH, "\tif len(p.DocsLabels) > MaxDocsLabels {", "\tif false {")]),
 ("validate(change): docs labels are not checked",
  [(CH, "\t\tif !validDocsLabel(l) {", "\t\tif false {")]),
 ("validate(change): a repeated docs label is accepted",
  [(CH, "\t\tif contains(p.DocsLabels[:i], l) {", "\t\tif false && contains(p.DocsLabels[:i], l) {")]),
 ("validate(change): a docs label of any length is accepted",
  [(CH, "\treturn docsLabelRe.MatchString(l) && utf8.RuneCountInString(l) <= MaxDocsLabelLen", "\treturn docsLabelRe.MatchString(l) && utf8.RuneCountInString(l) >= 0")]),
 ("validate(change): a padded or two-line docs label is accepted",
  [(CH, "\treturn docsLabelRe.MatchString(l) && utf8.RuneCountInString(l) <= MaxDocsLabelLen", "\treturn l != \"\" && utf8.RuneCountInString(l) <= MaxDocsLabelLen")]),
 ("validate(change): auto_merge.mode is not checked",
  [(CH, "\tif v := p.AutoMerge.Mode; v != nil && !autoMergeModes[*v] {\n\t\treturn fmt.Errorf(\"change: auto_merge.mode", "\tif v := p.AutoMerge.Mode; v != nil && false {\n\t\treturn fmt.Errorf(\"change: auto_merge.mode")]),
 ("validate(change): a number below 1 is accepted",
  [(CH, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d", "\t\tif n.v != nil && (*n.v > n.ceiling) {\n\t\t\treturn fmt.Errorf(\"change: %s is %d")]),
 ("validate(change): max_per_day is not among the bounded numbers",
  [(CH, "\t\t{\"auto_merge.max_per_day\", p.AutoMerge.MaxPerDay, AutoMergeMaxPerDayCeiling},\n\t}\n}\n\n// validDocsLabel", "\t}\n}\n\n// validDocsLabel")]),
 ("validate(change): max_open is not among the bounded numbers",
  [(CH, "\t\t{\"max_open\", p.MaxOpen, ChangeMaxOpenCeiling},\n", "")]),
 ("validate(change): verify_timeout above its ceiling is accepted",
  [(CH, "(*v <= 0 || time.Duration(*v) > ChangeVerifyTimeoutCeiling) {", "(*v <= 0) {")]),
 ("validate(change): verify_timeout of zero is accepted",
  [(CH, "(*v <= 0 || time.Duration(*v) > ChangeVerifyTimeoutCeiling) {", "(*v < 0 || time.Duration(*v) > ChangeVerifyTimeoutCeiling) {")]),
 ("validate(change): an operator layer may carry paths",
  [(CH, "\tif len(p.Paths) > 0 && layer == scope.LayerOperator {\n\t\treturn fmt.Errorf(\"change: paths is project-only", "\tif false {\n\t\treturn fmt.Errorf(\"change: paths is project-only")]),
 ("validate(change): path patterns are not checked",
  [(CH, "\t\tif err := scope.ValidatePattern(pat); err != nil {\n\t\t\treturn fmt.Errorf(\"change: paths", "\t\tif err := scope.ValidatePattern(pat); err != nil && false {\n\t\t\treturn fmt.Errorf(\"change: paths")]),
 ("validate(change): any number of path patterns",
  [(CH, "\tif len(p.Paths) > scope.MaxPatterns {\n\t\treturn fmt.Errorf(\"change: paths", "\tif false {\n\t\treturn fmt.Errorf(\"change: paths")]),
 ("validate(change): an unknown layer is accepted",
  [(CH, "\tif layer != scope.LayerOperator && layer != scope.LayerProject {\n\t\treturn fmt.Errorf(\"change: unknown layer", "\tif false {\n\t\treturn fmt.Errorf(\"change: unknown layer")]),
 ("validate(pipeline_fix): max_per_day is not among the bounded numbers",
  [(PF, "\t\t{\"auto_merge.max_per_day\", p.AutoMerge.MaxPerDay, AutoMergeMaxPerDayCeiling},\n", "")]),
 ("validate(review): targets are not checked",
  [(RV, "\t\tif !contains(reviewTargetOrder, v) {", "\t\tif false {")]),
 ("validate(review): the rung's form is not checked",
  [(RV, "\tif p.Rung != nil && !reviewRungRe.MatchString(*p.Rung) {", "\tif false {")]),
 ("validate(review): a number above its ceiling is accepted",
  [(RV, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {", "\t\tif n.v != nil && (*n.v < 1) {")]),
 ("validate(review): a number below 1 is accepted",
  [(RV, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {", "\t\tif n.v != nil && (*n.v > n.ceiling) {")]),
 ("validate(review): max_reviews_per_mr is not among the bounded numbers",
  [(RV, "\t\t{\"max_reviews_per_mr\", p.MaxReviewsPerMR, ReviewMaxReviewsPerMRCeiling},\n", "")]),
 ("validate(review): an unknown layer is accepted",
  [(RV, "\tif layer != scope.LayerOperator && layer != scope.LayerProject {", "\tif false {")]),
 ("ceiling: change.max_attempts ceiling constant raised",
  [(CH, "\tChangeMaxAttemptsCeiling     = 3\n", "\tChangeMaxAttemptsCeiling     = 30\n")]),
 ("ceiling: change.max_open ceiling constant raised",
  [(CH, "\tChangeMaxOpenCeiling         = 3\n", "\tChangeMaxOpenCeiling         = 30\n")]),
 ("ceiling: change.verify_timeout ceiling constant raised",
  [(CH, "\tChangeVerifyTimeoutCeiling   = 6 * time.Hour\n", "\tChangeVerifyTimeoutCeiling   = 60 * time.Hour\n")]),
 ("ceiling: change.max_files ceiling constant raised",
  [(CH, "\tChangeMaxFilesCeiling        = 8\n", "\tChangeMaxFilesCeiling        = 80\n")]),
 ("ceiling: change.max_changed_lines ceiling constant raised",
  [(CH, "\tChangeMaxChangedLinesCeiling = 300\n", "\tChangeMaxChangedLinesCeiling = 3000\n")]),
 ("ceiling: auto_merge.max_per_day ceiling constant raised",
  [(CH, "\tAutoMergeMaxPerDayCeiling = 3\n", "\tAutoMergeMaxPerDayCeiling = 30\n")]),
 ("ceiling: the docs-label count limit raised",
  [(CH, "\tMaxDocsLabels   = 16\n", "\tMaxDocsLabels   = 160\n")]),
 ("ceiling: the docs-label length limit raised",
  [(CH, "\tMaxDocsLabelLen = 255\n", "\tMaxDocsLabelLen = 2550\n")]),
 ("ceiling: review.max_notes ceiling constant raised",
  [(RV, "\tReviewMaxNotesCeiling        = 20\n", "\tReviewMaxNotesCeiling        = 200\n")]),
 ("ceiling: review.max_reviews_per_mr ceiling constant raised",
  [(RV, "\tReviewMaxReviewsPerMRCeiling = 3\n", "\tReviewMaxReviewsPerMRCeiling = 30\n")]),

 # ---- defaults: everything OFF / conservative ----
 ("default: reviews are ON for a project that said nothing",
  [(RS, RESOLVE_REVIEWS, "\teff.Actions.Reviews = (eff.Actions.PipelineFixes || eff.Actions.Features) &&\n\t\t(project.Actions.Reviews == nil || andAction(project.Actions.Reviews, instance.Actions.Reviews, group.Actions.Reviews))\n")]),
 ("default: triggers are only [label]",
  [(CH, "\tdefaultChangeTriggers    = []string{ChangeTriggerLabel, ChangeTriggerCommand, ChangeTriggerMention}", "\tdefaultChangeTriggers    = []string{ChangeTriggerLabel}")]),
 ("default: create_classes include source",
  [(CH, "\tdefaultCreateClasses     = []string{CreateClassTest, CreateClassDocs}", "\tdefaultCreateClasses     = []string{CreateClassSource, CreateClassTest, CreateClassDocs}")]),
 ("default: require_docs is never",
  [(CH, "\tdocs := RequireDocsWhenLabelled\n", "\tdocs := RequireDocsNever\n")]),
 ("default: require_docs is always",
  [(CH, "\tdocs := RequireDocsWhenLabelled\n", "\tdocs := RequireDocsAlways\n")]),
 ("default: docs_labels lose type::feature",
  [(CH, '\tdefaultDocsLabels        = []string{"feature", "enhancement", "type::feature"}', '\tdefaultDocsLabels        = []string{"feature", "enhancement"}')]),
 ("default: require_test_report is true",
  [(CH, "\t\tRequireTestReport: on(project.RequireTestReport) || on(op.RequireTestReport),", "\t\tRequireTestReport: true,")]),
 ("default: change.max_attempts is 3",
  [(CH, "\tDefaultChangeMaxAttempts     = 2\n", "\tDefaultChangeMaxAttempts     = 3\n")]),
 ("default: change.max_open is 3",
  [(CH, "\tDefaultChangeMaxOpen         = 1\n", "\tDefaultChangeMaxOpen         = 3\n")]),
 ("default: change.verify_timeout is 6h",
  [(CH, "\tDefaultChangeVerifyTimeout   = 2 * time.Hour\n", "\tDefaultChangeVerifyTimeout   = 6 * time.Hour\n")]),
 ("default: change.max_files is 8",
  [(CH, "\tDefaultChangeMaxFiles        = 6\n", "\tDefaultChangeMaxFiles        = 8\n")]),
 ("default: change.max_changed_lines is 300",
  [(CH, "\tDefaultChangeMaxChangedLines = 200\n", "\tDefaultChangeMaxChangedLines = 300\n")]),
 ("default: auto_merge.max_per_day is 1",
  [(CH, "\tDefaultAutoMergeMaxPerDay = 3\n", "\tDefaultAutoMergeMaxPerDay = 1\n")]),
 ("default: change auto-merge classes are empty",
  [(CH, "\tdefaultChangeMergeClass  = []string{AutoMergeClassDocs}", "\tdefaultChangeMergeClass  = []string{}")]),
 ("default: review targets are empty",
  [(CH, "\tdefaultReviewTargetsList = []string{ReviewTargetGonk}", "\tdefaultReviewTargetsList = []string{}")]),
 ("default: review.max_notes is 10",
  [(RV, "\tDefaultReviewMaxNotes        = 20\n", "\tDefaultReviewMaxNotes        = 10\n")]),
 ("default: review.max_reviews_per_mr is 1",
  [(RV, "\tDefaultReviewMaxReviewsPerMR = 3\n", "\tDefaultReviewMaxReviewsPerMR = 1\n")]),

 # ---- the fold: an operator layer only tightens ----
 ("resolve: Resolve does not fold the change section",
  [(RS, "\teff.Change = ResolveChange(instance.Change, group.Change, project.Change)\n", "")]),
 ("resolve: the group's change layer is dropped",
  [(RS, "ResolveChange(instance.Change, group.Change, project.Change)", "ResolveChange(instance.Change, ChangePolicy{}, project.Change)")]),
 ("resolve: the instance's change layer is dropped",
  [(RS, "ResolveChange(instance.Change, group.Change, project.Change)", "ResolveChange(ChangePolicy{}, group.Change, project.Change)")]),
 ("resolve: Resolve does not fold the review section",
  [(RS, "\teff.Review = ResolveReview(instance.Review, group.Review, project.Review)\n", "")]),
 ("resolve: the group's review layer is dropped",
  [(RS, "ResolveReview(instance.Review, group.Review, project.Review)", "ResolveReview(instance.Review, ReviewPolicy{}, project.Review)")]),
 ("resolve: the instance's review layer is dropped",
  [(RS, "ResolveReview(instance.Review, group.Review, project.Review)", "ResolveReview(ReviewPolicy{}, group.Review, project.Review)")]),
 ("resolve: reviews stay on when operators veto BOTH author workflows",
  [(RS, RESOLVE_REVIEWS, "\teff.Actions.Reviews = andAction(project.Actions.Reviews, instance.Actions.Reviews, group.Actions.Reviews)\n")]),
 ("resolve: reviews need BOTH author workflows",
  [(RS, "(eff.Actions.PipelineFixes || eff.Actions.Features) &&", "(eff.Actions.PipelineFixes && eff.Actions.Features) &&")]),
 ("resolve: reviews follow pipeline fixes only",
  [(RS, "(eff.Actions.PipelineFixes || eff.Actions.Features) &&", "(eff.Actions.PipelineFixes) &&")]),
 ("resolve: reviews follow features only",
  [(RS, "(eff.Actions.PipelineFixes || eff.Actions.Features) &&", "(eff.Actions.Features) &&")]),
 ("resolve: the instance veto of reviews is ignored",
  [(RS, "andAction(project.Actions.Reviews, instance.Actions.Reviews, group.Actions.Reviews)", "andAction(project.Actions.Reviews, group.Actions.Reviews)")]),
 ("resolve: the group veto of reviews is ignored",
  [(RS, "andAction(project.Actions.Reviews, instance.Actions.Reviews, group.Actions.Reviews)", "andAction(project.Actions.Reviews, instance.Actions.Reviews)")]),
 ("resolve: the change workflow's merge status is not set",
  [(RS, "\teff.Review.ChangeMerge = reviewMergeStatus(eff.Actions.Reviews, eff.Review)\n", "")]),
 ("resolve: the merge status is computed as if reviews were always on",
  [(RS, "\teff.Review.PipelineFixMerge = reviewMergeStatus(eff.Actions.Reviews, eff.Review)\n", "\teff.Review.PipelineFixMerge = reviewMergeStatus(true, eff.Review)\n")]),
 ("fold(change): an operator list is ignored",
  [(CH, "\tif oper != nil {\n\t\tbase = intersectLists(base, oper)\n\t}\n", "\t_ = oper\n")]),
 ("fold(change): an operator list REPLACES the project's (an operator can opt a project in)",
  [(CH, "\tif oper != nil {\n\t\tbase = intersectLists(base, oper)\n\t}\n", "\tif oper != nil {\n\t\tbase = oper\n\t}\n")]),
 ("fold(change): the project's list is ignored (always the default)",
  [(CH, "\tbase := def\n\tif proj != nil {\n\t\tbase = proj\n\t}\n", "\tbase := def\n\t_ = proj\n")]),
 ("fold(change): unknown list members survive",
  [(CH, "\tfor _, v := range order {\n\t\tif contains(base, v) {\n\t\t\tout = append(out, v)\n\t\t}\n\t}\n\treturn out", "\tout = append(out, base...)\n\t_ = order\n\treturn out")]),
 ("fold(change): an operator number RAISES the project's",
  [(CH, "\tif oper != nil && *oper < v {\n\t\tv = *oper\n\t}\n\treturn int(", "\tif oper != nil {\n\t\tv = *oper\n\t}\n\treturn int(")]),
 ("fold(change): an operator number is ignored",
  [(CH, "\tif oper != nil && *oper < v {\n\t\tv = *oper\n\t}\n\treturn int(", "\t_ = oper\n\treturn int(")]),
 ("fold(change): numbers are not clamped to the ceiling",
  [(CH, "\treturn int(min(max(v, 0), ceiling))", "\t_ = ceiling\n\treturn int(max(v, 0))")]),
 ("fold(change): negative numbers are not clamped",
  [(CH, "\treturn int(min(max(v, 0), ceiling))", "\treturn int(min(v, ceiling))")]),
 ("fold(change): the project's verify_timeout is ignored",
  [(CH, "\tif proj != nil {\n\t\tv = time.Duration(*proj)\n\t}\n", "\t_ = proj\n")]),
 ("fold(change): an operator verify_timeout RAISES the project's",
  [(CH, "\tif oper != nil && time.Duration(*oper) < v {", "\tif oper != nil {")]),
 ("fold(change): an operator verify_timeout is ignored",
  [(CH, "\tif oper != nil && time.Duration(*oper) < v {", "\tif false {")]),
 ("fold(change): verify_timeout is not clamped to its ceiling",
  [(CH, "\treturn min(max(v, 0), ceiling)\n}", "\t_ = ceiling\n\treturn max(v, 0)\n}")]),
 ("fold(change): a negative verify_timeout is kept",
  [(CH, "\treturn min(max(v, 0), ceiling)\n}", "\treturn min(v, ceiling)\n}")]),
 ("fold(change): the project's require_docs is ignored",
  [(CH, "\tif project.RequireDocs != nil {\n\t\tdocs = *project.RequireDocs\n\t}\n", "")]),
 ("fold(change): an operator require_docs is ignored",
  [(CH, "\tif s := stricterDocs(&docs, op.RequireDocs); s != nil {\n\t\tdocs = *s\n\t}\n", "")]),
 ("fold(change): the LOOSER require_docs wins",
  [(CH, "\tcase rank(b) > rank(a):\n\t\treturn b\n\t}\n\treturn a\n}\n\n// unionLists", "\tcase rank(b) < rank(a):\n\t\treturn b\n\t}\n\treturn a\n}\n\n// unionLists")]),
 ("fold(change): an unknown require_docs is kept (not read as always)",
  [(CH, "\tif _, ok := requireDocsRank[docs]; !ok {\n\t\tdocs = RequireDocsAlways\n\t}\n", "")]),
 ("fold(change): an unknown require_docs ranks as the loosest",
  [(CH, "\t\tif r, ok := requireDocsRank[*s]; ok {\n\t\t\treturn r\n\t\t}\n\t\treturn 99", "\t\tif r, ok := requireDocsRank[*s]; ok {\n\t\t\treturn r\n\t\t}\n\t\treturn -1"),
   (CH, "\tif _, ok := requireDocsRank[docs]; !ok {\n\t\tdocs = RequireDocsAlways\n\t}\n", "\tif _, ok := requireDocsRank[docs]; !ok {\n\t\tdocs = RequireDocsNever\n\t}\n")]),
 ("fold(change): the project's docs_labels are ignored",
  [(CH, "\tlabels := defaultDocsLabels\n\tif project.DocsLabels != nil {\n\t\tlabels = project.DocsLabels\n\t}\n", "\tlabels := defaultDocsLabels\n")]),
 ("fold(change): an operator's docs_labels are ignored",
  [(CH, "\tfor _, l := range unionLists(labels, op.DocsLabels) {", "\tfor _, l := range labels {")]),
 ("fold(change): an operator's docs_labels REPLACE the project's (an operator can remove a label)",
  [(CH, "\tfor _, l := range unionLists(labels, op.DocsLabels) {", "\tif op.DocsLabels != nil {\n\t\tlabels = op.DocsLabels\n\t}\n\tfor _, l := range labels {")]),
 ("fold(change): operator docs_labels are INTERSECTED with the project's",
  [(CH, "\tfor _, l := range unionLists(labels, op.DocsLabels) {", "\tfor _, l := range intersectLists(labels, op.DocsLabels) {")]),
 ("fold(change): malformed docs labels survive",
  [(CH, "\t\tif validDocsLabel(l) && !contains(eff.DocsLabels, l) {", "\t\tif !contains(eff.DocsLabels, l) {")]),
 ("fold(change): repeated docs labels survive",
  [(CH, "\t\tif validDocsLabel(l) && !contains(eff.DocsLabels, l) {", "\t\tif validDocsLabel(l) {")]),
 ("fold(change): an operator cannot force require_test_report",
  [(CH, "\t\tRequireTestReport: on(project.RequireTestReport) || on(op.RequireTestReport),", "\t\tRequireTestReport: on(project.RequireTestReport),")]),
 ("fold(change): an operator `false` waives the project's require_test_report",
  [(CH, "\t\tRequireTestReport: on(project.RequireTestReport) || on(op.RequireTestReport),", "\t\tRequireTestReport: andAction(project.RequireTestReport, op.RequireTestReport),")]),
 ("fold(change): the project's paths are dropped",
  [(CH, "\t\tPaths:             append([]string{}, project.Paths...),", "\t\tPaths:             []string{},")]),
 ("fold(change): the project's auto-merge mode is ignored",
  [(CH, "\tif proj != nil {\n\t\tmode = *proj\n\t}\n", "\t_ = proj\n")]),
 ("fold(change): an operator auto-merge mode is ignored (no veto)",
  [(CH, "\tif m := foldMode(&mode, oper); m != nil {\n\t\tmode = *m\n\t}\n", "\t_ = oper\n")]),
 ("fold(change): an unknown auto-merge mode is kept",
  [(CH, "\tif !autoMergeModes[mode] {\n\t\tmode = AutoMergeOff\n\t}\n\treturn mode", "\treturn mode")]),
 ("fold(change): max_open is resolved from max_attempts",
  [(CH, "resolveNum(DefaultChangeMaxOpen, ChangeMaxOpenCeiling, project.MaxOpen, op.MaxOpen)", "resolveNum(DefaultChangeMaxOpen, ChangeMaxOpenCeiling, project.MaxAttempts, op.MaxAttempts)")]),
 ("fold(pipeline_fix): max_per_day is not resolved",
  [(PF, "\teff.AutoMerge.MaxPerDay = num(DefaultAutoMergeMaxPerDay, AutoMergeMaxPerDayCeiling, project.AutoMerge.MaxPerDay, op.AutoMerge.MaxPerDay)\n", "")]),
 ("fold(pipeline_fix): an operator's max_per_day is ignored",
  [(PF, "num(DefaultAutoMergeMaxPerDay, AutoMergeMaxPerDayCeiling, project.AutoMerge.MaxPerDay, op.AutoMerge.MaxPerDay)", "num(DefaultAutoMergeMaxPerDay, AutoMergeMaxPerDayCeiling, project.AutoMerge.MaxPerDay, nil)")]),
 ("operator fold(pipeline_fix): max_per_day is not folded",
  [(PF, "\tout.AutoMerge.MaxPerDay = minInt(base.AutoMerge.MaxPerDay, next.AutoMerge.MaxPerDay)\n", "\tout.AutoMerge.MaxPerDay = next.AutoMerge.MaxPerDay\n")]),
 ("operator fold(change): the triggers are not folded",
  [(CH, "\t\tTriggers:          intersectLists(base.Triggers, next.Triggers),", "\t\tTriggers:          next.Triggers,")]),
 ("operator fold(change): the create classes are not folded",
  [(CH, "\t\tCreateClasses:     intersectLists(base.CreateClasses, next.CreateClasses),", "\t\tCreateClasses:     next.CreateClasses,")]),
 ("operator fold(change): the instance's require_docs is dropped",
  [(CH, "\t\tRequireDocs:       stricterDocs(base.RequireDocs, next.RequireDocs),", "\t\tRequireDocs:       next.RequireDocs,")]),
 ("operator fold(change): the instance's docs_labels are dropped",
  [(CH, "\t\tDocsLabels:        unionLists(base.DocsLabels, next.DocsLabels),", "\t\tDocsLabels:        next.DocsLabels,")]),
 ("operator fold(change): a nested `require_test_report: false` undoes its parent's true",
  [(CH, "\t\tRequireTestReport: stickyTrue(base.RequireTestReport, next.RequireTestReport),", "\t\tRequireTestReport: next.RequireTestReport,")]),
 ("operator fold: stickyTrue lets a later false win",
  [(CH, "\tif a != nil && *a {\n\t\treturn a\n\t}\n\tif b != nil {\n\t\treturn b\n\t}\n\treturn a\n}\n\nfunc minDuration", "\tif b != nil {\n\t\treturn b\n\t}\n\treturn a\n}\n\nfunc minDuration")]),
 ("operator fold(change): max_attempts is not folded",
  [(CH, "\t\tMaxAttempts:       minInt(base.MaxAttempts, next.MaxAttempts),", "\t\tMaxAttempts:       next.MaxAttempts,")]),
 ("operator fold(change): max_open is not folded",
  [(CH, "\t\tMaxOpen:           minInt(base.MaxOpen, next.MaxOpen),", "\t\tMaxOpen:           next.MaxOpen,")]),
 ("operator fold(change): verify_timeout takes the LARGER value",
  [(CH, "\tcase *b < *a:\n\t\treturn b\n\t}\n\treturn a\n}\n", "\tcase *b > *a:\n\t\treturn b\n\t}\n\treturn a\n}\n")]),
 ("operator fold(change): max_files is not folded",
  [(CH, "\t\tMaxFiles:          minInt(base.MaxFiles, next.MaxFiles),", "\t\tMaxFiles:          next.MaxFiles,")]),
 ("operator fold(change): max_changed_lines is not folded",
  [(CH, "\t\tMaxChangedLines:   minInt(base.MaxChangedLines, next.MaxChangedLines),", "\t\tMaxChangedLines:   next.MaxChangedLines,")]),
 ("operator fold(change): the instance's auto-merge mode is dropped",
  [(CH, "\t\t\tMode:      foldMode(base.AutoMerge.Mode, next.AutoMerge.Mode),", "\t\t\tMode:      next.AutoMerge.Mode,")]),
 ("operator fold(change): the auto-merge classes are not folded",
  [(CH, "\t\t\tClasses:   intersectLists(base.AutoMerge.Classes, next.AutoMerge.Classes),", "\t\t\tClasses:   next.AutoMerge.Classes,")]),
 ("operator fold(change): max_per_day is not folded",
  [(CH, "\t\t\tMaxPerDay: minInt(base.AutoMerge.MaxPerDay, next.AutoMerge.MaxPerDay),", "\t\t\tMaxPerDay: next.AutoMerge.MaxPerDay,")]),
 ("operator fold(change): two docs-label lists are intersected",
  [(CH, "\tout := append([]string{}, a...)\n\tfor _, v := range b {\n\t\tif !contains(out, v) {\n\t\t\tout = append(out, v)\n\t\t}\n\t}\n\treturn out",
        "\tout := []string{}\n\tfor _, v := range a {\n\t\tif contains(b, v) {\n\t\t\tout = append(out, v)\n\t\t}\n\t}\n\treturn out")]),
 ("operator fold(review): the targets are not folded",
  [(RV, "\t\tTargets:         intersectLists(base.Targets, next.Targets),", "\t\tTargets:         next.Targets,")]),
 ("operator fold(review): max_notes is not folded",
  [(RV, "\t\tMaxNotes:        minInt(base.MaxNotes, next.MaxNotes),", "\t\tMaxNotes:        next.MaxNotes,")]),
 ("operator fold(review): max_reviews_per_mr is not folded",
  [(RV, "\t\tMaxReviewsPerMR: minInt(base.MaxReviewsPerMR, next.MaxReviewsPerMR),", "\t\tMaxReviewsPerMR: next.MaxReviewsPerMR,")]),
 ("operator fold(review): the rung is dropped",
  [(RV, "\t\tRung:            foldRung(base.Rung, next.Rung),\n", "")]),
 ("fold(review): the project's targets are ignored",
  [(RV, "resolveList(reviewTargetOrder, defaultReviewTargetsList, project.Targets, op.Targets)", "resolveList(reviewTargetOrder, defaultReviewTargetsList, nil, op.Targets)")]),
 ("fold(review): an operator's targets are ignored",
  [(RV, "resolveList(reviewTargetOrder, defaultReviewTargetsList, project.Targets, op.Targets)", "resolveList(reviewTargetOrder, defaultReviewTargetsList, project.Targets, nil)")]),
 ("fold(review): an operator's max_notes is ignored",
  [(RV, "resolveNum(DefaultReviewMaxNotes, ReviewMaxNotesCeiling, project.MaxNotes, op.MaxNotes)", "resolveNum(DefaultReviewMaxNotes, ReviewMaxNotesCeiling, project.MaxNotes, nil)")]),
 ("fold(review): the project's max_reviews_per_mr is ignored",
  [(RV, "resolveNum(DefaultReviewMaxReviewsPerMR, ReviewMaxReviewsPerMRCeiling, project.MaxReviewsPerMR, op.MaxReviewsPerMR)", "resolveNum(DefaultReviewMaxReviewsPerMR, ReviewMaxReviewsPerMRCeiling, nil, op.MaxReviewsPerMR)")]),
 ("fold(review): an unvalidated rung that is not a rung name is kept",
  [(RV, "\tif op.Rung != nil && reviewRungRe.MatchString(*op.Rung) {", "\tif op.Rung != nil {")]),

 # ---- helpers ----
 ("helper: AllowsTrigger says yes to everything",
  [(CH, "func (e EffectiveChange) AllowsTrigger(form string) bool { return contains(e.Triggers, form) }", "func (e EffectiveChange) AllowsTrigger(form string) bool { return form != \"\" }")]),
 ("helper: AllowsCreateClass reads the trigger list",
  [(CH, "\treturn contains(e.CreateClasses, class)\n", "\treturn contains(e.Triggers, class) || class == \"test\" || class == \"docs\"\n")]),
 ("helper: DocsRequired under never",
  [(CH, "\tcase RequireDocsNever:\n\t\treturn false\n", "\tcase RequireDocsNever:\n\t\treturn true\n")]),
 ("helper: DocsRequired ignores the labels under when-labelled",
  [(CH, "\t\t\tif contains(e.DocsLabels, l) {\n\t\t\t\treturn true\n\t\t\t}\n", "\t\t\t_ = l\n")]),
 ("helper: DocsRequired is true for any labelled issue",
  [(CH, "\t\t\tif contains(e.DocsLabels, l) {\n\t\t\t\treturn true\n\t\t\t}\n", "\t\t\tif l != \"\" {\n\t\t\t\treturn true\n\t\t\t}\n")]),
 ("helper: DocsRequired reads an unknown value as not required",
  [(CH, "\t\treturn false\n\t}\n\treturn true\n}\n\n// TightenScope", "\t\treturn false\n\t}\n\treturn e.RequireDocs == RequireDocsAlways\n}\n\n// TightenScope")]),
 ("helper: change TightenScope RAISES a lower scope.caps",
  [(CH, "\t\tif have != nil && *have < v {\n\t\t\tv = *have\n\t\t}\n\t\treturn &v", "\t\t_ = have\n\t\treturn &v")]),
 ("helper: change TightenScope lets a higher scope.caps stand",
  [(CH, "\t\tif have != nil && *have < v {\n\t\t\tv = *have\n\t\t}\n\t\treturn &v", "\t\tif have != nil {\n\t\t\tv = *have\n\t\t}\n\t\treturn &v")]),
 ("helper: change TightenScope leaves max_lines alone",
  [(CH, "\tproject.Caps.MaxLines = lower(project.Caps.MaxLines, maxLines)\n", "\t_ = maxLines\n")]),
 ("helper: change TightenScope leaves max_files alone",
  [(CH, "\tproject.Caps.MaxFiles = lower(project.Caps.MaxFiles, maxFiles)\n", "\t_ = maxFiles\n")]),
 ("helper: MergeStatus answers the change status for pipeline-fix",
  [(RV, "\tcase WorkflowPipelineFix:\n\t\treturn e.PipelineFixMerge\n", "\tcase WorkflowPipelineFix:\n\t\treturn e.ChangeMerge\n")]),
 ("helper: MergeStatus of an unknown workflow is available",
  [(RV, "\t\treturn e.ChangeMerge\n\t}\n\treturn ReviewMergeStatus{}\n", "\t\treturn e.ChangeMerge\n\t}\n\treturn ReviewMergeStatus{Available: true}\n")]),
 ("helper: review AllowsTarget says yes to everything",
  [(RV, "func (e EffectiveReview) AllowsTarget(target string) bool { return contains(e.Targets, target) }", "func (e EffectiveReview) AllowsTarget(target string) bool { return target != \"\" }")]),

 # ---- the operator config ----
 ("opercfg: the operator's change section is not validated in Go",
  [(OC, "\tif err := p.Change.Validate(scope.LayerOperator); err != nil {", "\tif err := p.Change.Validate(scope.LayerOperator); err != nil && false {")]),
 ("opercfg: the operator's change section is validated as a project's (paths accepted in Go)",
  [(OC, "p.Change.Validate(scope.LayerOperator)", "p.Change.Validate(scope.LayerProject)")]),
 ("opercfg: the operator's review section is not validated in Go",
  [(OC, "\tif err := p.Review.Validate(scope.LayerOperator); err != nil {", "\tif err := p.Review.Validate(scope.LayerOperator); err != nil && false {")]),
 ("opercfg: the operator's review section is validated as a project's (the rung is refused)",
  [(OC, "p.Review.Validate(scope.LayerOperator)", "p.Review.Validate(scope.LayerProject)")]),
 ("opercfg: a nested group re-enables reviews",
  [(OC, "\t\tReviews: foldVeto(base.Actions.Reviews, next.Actions.Reviews),", "\t\tReviews: next.Actions.Reviews,")]),
 ("opercfg: GroupFor drops the reviews veto",
  [(OC, "\t\tReviews: foldVeto(base.Actions.Reviews, next.Actions.Reviews),\n", "")]),
 ("opercfg: GroupFor drops the change section",
  [(OC, "\tout.Change = gonkcfg.FoldChange(base.Change, next.Change)\n", "")]),
 ("opercfg: a nested group's change section replaces its parent's",
  [(OC, "\tout.Change = gonkcfg.FoldChange(base.Change, next.Change)\n", "\tout.Change = next.Change\n")]),
 ("opercfg: GroupFor drops the review section",
  [(OC, "\tout.Review = gonkcfg.FoldReview(base.Review, next.Review)\n", "")]),
 ("opercfg: a nested group's review section replaces its parent's (it can swap the reviewer)",
  [(OC, "\tout.Review = gonkcfg.FoldReview(base.Review, next.Review)\n", "\tout.Review = next.Review\n")]),

 # ---- the triggers: literals, gates, floor, floor retry, reservation TTL ----
 ("trigger: the issue-change ledger literal is renamed",
  [(AT, 'TriggerIssueChange = "issue-change"', 'TriggerIssueChange = "issue_change"')]),
 ("trigger: the mr-review ledger literal is renamed",
  [(AT, 'TriggerMRReview = "mr-review"', 'TriggerMRReview = "review"')]),
 ("trigger: issue-change is not a valid trigger",
  [(AT, "\tTriggerIssueChange:       {},\n", "")]),
 ("trigger: mr-review is not a valid trigger",
  [(AT, "\tTriggerMRReview:          {},\n", "")]),
 ("trigger: issue-change gated by nothing",
  [(RD, CHANGE_GATE, "\t\treturn true, false\n")]),
 ("trigger: issue-change gated by actions.triage",
  [(RD, CHANGE_GATE, "\t\treturn eff.Actions.Triage, true\n")]),
 ("trigger: issue-change gated by actions.pipeline_fixes",
  [(RD, CHANGE_GATE, "\t\treturn eff.Actions.PipelineFixes, true\n")]),
 ("trigger: issue-change gated by actions.reviews",
  [(RD, CHANGE_GATE, "\t\treturn eff.Actions.Reviews, true\n")]),
 ("trigger: issue-change has no ActionFor case (falls to the unknown-trigger deny)",
  [(RD, "\tcase atags.TriggerIssueChange:\n", "\tcase \"issue-change-disabled\":\n")]),
 ("trigger: mr-review has no ActionFor case",
  [(RD, "\tcase atags.TriggerMRReview:\n\t\t// Gated by its OWN action", "\tcase \"mr-review-disabled\":\n\t\t// Gated by its OWN action")]),
 ("ladder: issue-change has no floor (it would start on the 16K rung)",
  [(RD, FLOORS, FLOORS.replace("\tatags.TriggerIssueChange:       1,\n", ""))]),
 ("ladder: issue-change floor is 2",
  [(RD, FLOORS, FLOORS.replace("\tatags.TriggerIssueChange:       1,\n", "\tatags.TriggerIssueChange:       2,\n"))]),
 ("ladder: mr-review has a floor of 1 (its one-rung ladder would never run)",
  [(RD, FLOORS, FLOORS.replace("\tatags.TriggerIssueChange:       1,\n", "\tatags.TriggerIssueChange:       1,\n\tatags.TriggerMRReview:          1,\n"))]),
 ("ladder: issue-change has no floor retry",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerIssueChange:       1,\n", ""))]),
 ("ladder: issue-change has two floor retries",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerIssueChange:       1,\n", "\tatags.TriggerIssueChange:       2,\n"))]),
 ("ladder: mr-review has no floor retry",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerMRReview:          1,\n", ""))]),
 ("ladder: mr-review has two floor retries",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerMRReview:          1,\n", "\tatags.TriggerMRReview:          2,\n"))]),
 ("ttl: issue-change gets the base TTL",
  [(RD, TTL_CHANGE, "")]),
 ("ttl: mr-review gets the longer TTL too",
  [(RD, "\tif trigger == atags.TriggerIssueChange {\n", "\tif trigger == atags.TriggerIssueChange || trigger == atags.TriggerMRReview {\n")]),
 ("ttl: issue-change reads pipeline_fix.verify_timeout",
  [(RD, TTL_RET, "\t\treturn base + min(max(eff.PipelineFix.VerifyTimeout, 0), gonkcfg.ChangeVerifyTimeoutCeiling)\n")]),
 ("ttl: the change verify_timeout REPLACES the base",
  [(RD, TTL_RET, "\t\treturn min(max(eff.Change.VerifyTimeout, 0), gonkcfg.ChangeVerifyTimeoutCeiling)\n")]),
 ("ttl: issue-change has no upper clamp",
  [(RD, TTL_RET, "\t\treturn base + max(eff.Change.VerifyTimeout, 0)\n")]),
 ("ttl: a negative change verify_timeout shortens the base",
  [(RD, TTL_RET, "\t\treturn base + min(eff.Change.VerifyTimeout, gonkcfg.ChangeVerifyTimeoutCeiling)\n")]),
 ("ttl: issue-change always gets the ceiling",
  [(RD, TTL_RET, "\t\treturn base + gonkcfg.ChangeVerifyTimeoutCeiling\n")]),
 ("service: the outcome preview escalates a review along the project's ladder",
  [(SV, "\t\tif ladder := rung.LadderFor(eff, trigger); pos.Index < len(ladder) {", "\t\tif ladder := eff.Ladder; pos.Index < len(ladder) {")]),

 # ---- the wire ----
 ("wire: reviews is not sent",
  [(MA, "\tout.Actions.Reviews = e.Actions.Reviews\n", "")]),
 ("wire: reviews mirrors features",
  [(MA, "\tout.Actions.Reviews = e.Actions.Reviews\n", "\tout.Actions.Reviews = e.Actions.Features\n")]),
 ("wire: the reviews key is renamed",
  [(MA, '`json:"reviews"`', '`json:"review"`')]),
 ("wire: reviews is omitempty (absent instead of false)",
  [(MA, '`json:"reviews"`', '`json:"reviews,omitempty"`')]),
 ("wire: the change key is renamed",
  [(MA, '\tChange Change `json:"change"`', '\tChange Change `json:"issue_change"`')]),
 ("wire: the review key is renamed",
  [(MA, '\tReview Review `json:"review"`', '\tReview Review `json:"reviewer"`')]),
 ("wire: pipeline_fix max_per_day is not sent",
  [(MA, "\tout.PipelineFix.AutoMerge.MaxPerDay = e.PipelineFix.AutoMerge.MaxPerDay\n", "")]),
 ("wire: the max_per_day key is renamed",
  [(MA, '`json:"max_per_day"`', '`json:"maxPerDay"`')]),
 ("wire: triggers are not sent",
  [(MA, "\t\tTriggers:             append([]string{}, e.Change.Triggers...),", "\t\tTriggers:             []string{},")]),
 ("wire: triggers alias the resolved slice",
  [(MA, "\t\tTriggers:             append([]string{}, e.Change.Triggers...),", "\t\tTriggers:             e.Change.Triggers,")]),
 ("wire: an empty trigger list is null",
  [(MA, "\t\tTriggers:             append([]string{}, e.Change.Triggers...),", "\t\tTriggers:             append([]string(nil), e.Change.Triggers...),")]),
 ("wire: create_classes carry the triggers",
  [(MA, "\t\tCreateClasses:        append([]string{}, e.Change.CreateClasses...),", "\t\tCreateClasses:        append([]string{}, e.Change.Triggers...),")]),
 ("wire: require_docs is not sent",
  [(MA, "\t\tRequireDocs:          e.Change.RequireDocs,\n", "")]),
 ("wire: docs_labels are not sent",
  [(MA, "\t\tDocsLabels:           append([]string{}, e.Change.DocsLabels...),", "\t\tDocsLabels:           []string{},")]),
 ("wire: require_test_report is not sent",
  [(MA, "\t\tRequireTestReport:    e.Change.RequireTestReport,\n", "")]),
 ("wire: change max_attempts carries max_open",
  [(MA, "\t\tMaxAttempts:          e.Change.MaxAttempts,", "\t\tMaxAttempts:          e.Change.MaxOpen,")]),
 ("wire: max_open is not sent",
  [(MA, "\t\tMaxOpen:              e.Change.MaxOpen,\n", "")]),
 ("wire: change verify_timeout is sent in nanoseconds under a seconds key",
  [(MA, "int64(e.Change.VerifyTimeout / time.Second)", "int64(e.Change.VerifyTimeout)")]),
 ("wire: change max_files carries max_changed_lines",
  [(MA, "\t\tMaxFiles:             e.Change.MaxFiles,", "\t\tMaxFiles:             e.Change.MaxChangedLines,")]),
 ("wire: change max_changed_lines is not sent",
  [(MA, "\t\tMaxChangedLines:      e.Change.MaxChangedLines,\n", "")]),
 ("wire: change paths are not sent",
  [(MA, "\t\tPaths:                append([]string{}, e.Change.Paths...),", "\t\tPaths:                []string{},")]),
 ("wire: the change auto-merge mode is the pipeline-fix one",
  [(MA, "\t\t\tMode:      e.Change.AutoMerge.Mode,", "\t\t\tMode:      e.PipelineFix.AutoMerge.Mode,")]),
 ("wire: the change auto-merge classes are not sent",
  [(MA, "\t\t\tClasses:   append([]string{}, e.Change.AutoMerge.Classes...),", "\t\t\tClasses:   []string{},")]),
 ("wire: the change max_per_day is not sent",
  [(MA, "\t\t\tMaxPerDay: e.Change.AutoMerge.MaxPerDay,\n", "")]),
 ("wire: review targets are not sent",
  [(MA, "\t\tTargets:         append([]string{}, e.Review.Targets...),", "\t\tTargets:         []string{},")]),
 ("wire: the reviewer rung is not sent",
  [(MA, "\t\tRung:            e.Review.Rung,\n", "")]),
 ("wire: max_notes is not sent",
  [(MA, "\t\tMaxNotes:        e.Review.MaxNotes,\n", "")]),
 ("wire: max_reviews_per_mr carries max_notes",
  [(MA, "\t\tMaxReviewsPerMR: e.Review.MaxReviewsPerMR,", "\t\tMaxReviewsPerMR: e.Review.MaxNotes,")]),
 ("wire: the pipeline-fix merge status carries the change one",
  [(MA, "PipelineFix: ReviewMergeStatus{Available: e.Review.PipelineFixMerge.Available, Reason: e.Review.PipelineFixMerge.Reason},",
        "PipelineFix: ReviewMergeStatus{Available: e.Review.ChangeMerge.Available, Reason: e.Review.ChangeMerge.Reason},")]),
 ("wire: the change merge status is always available",
  [(MA, "Change:      ReviewMergeStatus{Available: e.Review.ChangeMerge.Available, Reason: e.Review.ChangeMerge.Reason},",
        "Change:      ReviewMergeStatus{Available: true, Reason: e.Review.ChangeMerge.Reason},")]),
 ("wire: the merge status reason is not sent",
  [(MA, "Change:      ReviewMergeStatus{Available: e.Review.ChangeMerge.Available, Reason: e.Review.ChangeMerge.Reason},",
        "Change:      ReviewMergeStatus{Available: e.Review.ChangeMerge.Available},")]),
 ("wire: the reader drops max_per_day of the pipeline-fix block",
  [(MA, "AutoMerge: gonkcfg.EffectiveAutoMerge{Mode: p.AutoMerge.Mode, Classes: append([]string{}, p.AutoMerge.Classes...),\n\t\t\tMaxPerDay: p.AutoMerge.MaxPerDay},",
        "AutoMerge: gonkcfg.EffectiveAutoMerge{Mode: p.AutoMerge.Mode, Classes: append([]string{}, p.AutoMerge.Classes...)},")]),
 ("wire: the reader's change conversion drops the docs labels",
  [(MA, "RequireDocs: c.RequireDocs, DocsLabels: append([]string{}, c.DocsLabels...),", "RequireDocs: c.RequireDocs, DocsLabels: []string{},")]),
 ("wire: the reader's change conversion aliases the triggers",
  [(MA, "Triggers: append([]string{}, c.Triggers...), CreateClasses:", "Triggers: c.Triggers, CreateClasses:")]),
 ("wire: the reader's review conversion reports the change status as available",
  [(MA, "ChangeMerge:      gonkcfg.ReviewMergeStatus{Available: r.AgenticMerge.Change.Available, Reason: r.AgenticMerge.Change.Reason},",
        "ChangeMerge:      gonkcfg.ReviewMergeStatus{Available: true, Reason: r.AgenticMerge.Change.Reason},")]),
 ("wire: the reader's review conversion drops the rung",
  [(MA, "Targets: append([]string{}, r.Targets...), Rung: r.Rung,\n", "Targets: append([]string{}, r.Targets...),\n")]),

 # ---- the persisted registration (gonk-mzip) ----
 ("store: pipeline_fix is not persisted (the defect as found)",
  [(PW, "\t\tPipelineFix:    e.PipelineFix,\n", "")]),
 ("store: change is not persisted",
  [(PW, "\t\tChange:         e.Change,\n", "")]),
 ("store: review is not persisted",
  [(PW, "\t\tReview:         e.Review,\n", "")]),
 ("store: pipeline_fix is written but not read back",
  [(PW, "\t\tPipelineFix: w.PipelineFix,\n", "")]),
 ("store: change is written but not read back",
  [(PW, "\t\tChange:      w.Change,\n", "")]),
 ("store: review is read back from the change key",
  [(PW, '\tReview      gonkcfg.EffectiveReview      `json:"review"`', '\tReview      gonkcfg.EffectiveReview      `json:"change"`')]),

 # ---- intake: the pre-filters, the onboarding text ----
 ("intake: MayChangeIssues ignores the project's state",
  [(ST, "\tswitch c.State {\n\tcase StateValid, StatePending:\n\t\treturn c.eff().Actions.Features\n\t}\n\treturn false\n", "\treturn c.eff().Actions.Features\n")]),
 ("intake: MayChangeIssues is opened by triage",
  [(ST, MAYCHANGE, MAYCHANGE.replace("c.eff().Actions.Features", "c.eff().Actions.Features || c.eff().Actions.Triage"))]),
 ("intake: MayChangeIssues is opened by pipeline_fixes",
  [(ST, MAYCHANGE, MAYCHANGE.replace("c.eff().Actions.Features", "c.eff().Actions.Features || c.eff().Actions.PipelineFixes"))]),
 ("intake: MayReview does not need reviews",
  [(ST, MAYREVIEW, "\t\treturn (e.Actions.PipelineFixes || e.Actions.Features) && e.Review.Rung != \"\"\n")]),
 ("intake: MayReview does not need an author workflow",
  [(ST, MAYREVIEW, "\t\treturn e.Actions.Reviews && e.Review.Rung != \"\"\n")]),
 ("intake: MayReview does not need a reviewer rung",
  [(ST, MAYREVIEW, "\t\treturn e.Actions.Reviews && (e.Actions.PipelineFixes || e.Actions.Features)\n")]),
 ("intake: the gate sends issue-change through the triage predicate",
  [(DI, "\tcase atags.TriggerIssueChange:\n\t\treturn cls.MayChangeIssues()\n", "\tcase atags.TriggerIssueChange:\n\t\treturn cls.MayTriage()\n")]),
 ("intake: the gate has no issue-change case",
  [(DI, "\tcase atags.TriggerIssueChange:\n\t\treturn cls.MayChangeIssues()\n", "")]),
 ("intake: the gate sends mr-review through the change predicate",
  [(DI, "\tcase atags.TriggerMRReview:\n\t\treturn cls.MayReview()\n", "\tcase atags.TriggerMRReview:\n\t\treturn cls.MayChangeIssues()\n")]),
 ("intake: the gate has no mr-review case",
  [(DI, "\tcase atags.TriggerMRReview:\n\t\treturn cls.MayReview()\n", "")]),
 ("onboarding: the committed .gonk.yml switches reviews ON",
  [(RE, "  reviews: false           # NOT ACTIVE YET", "  reviews: true            # NOT ACTIVE YET")]),
 ("onboarding: the committed .gonk.yml switches reviews AND features on",
  [(RE, "  reviews: false           # NOT ACTIVE YET", "  reviews: true            # NOT ACTIVE YET"),
   (RE, "  features: false\n", "  features: true\n")]),
 ("onboarding: the .gonk.yml does not say reviews is not active",
  [(RE, "  reviews: false           # NOT ACTIVE YET: no version of gonk reviews", "  reviews: false           # no version of gonk reviews")]),
 ("onboarding: the .gonk.yml does not mention reviews",
  [(RE, "  reviews: false           # NOT ACTIVE YET: no version of gonk reviews\n", "                           # no version of gonk reviews\n")]),
 ("onboarding: the .gonk.yml commits a change section",
  [(RE, "# Which files gonk treats as source code, tests and documentation here, chosen", "change:\n  max_open: 1\n\n# Which files gonk treats as source code, tests and documentation here, chosen")]),
 ("onboarding: the reviews row does not say it is not active",
  [(RE, "gonk will not review merge requests. " + NA_ROW, "gonk will not review merge requests. ")]),
 ("onboarding: the change rows do not say they are not active (first row)",
  [(RE, "| ` + \"`change.triggers`\" + ` | unset | " + NA_ROW, "| ` + \"`change.triggers`\" + ` | unset | ")]),
 ("onboarding: a change row does not say it is not active (max_open)",
  [(RE, "| ` + \"`change.max_open`\" + ` | unset | Not active yet. ", "| ` + \"`change.max_open`\" + ` | unset | ")]),
 ("onboarding: a review row does not say it is not active (max_notes)",
  [(RE, "| ` + \"`review.max_notes`\" + ` | unset | Not active yet. ", "| ` + \"`review.max_notes`\" + ` | unset | ")]),
 ("onboarding: the review rows do not say they are not active (first row)",
  [(RE, "| ` + \"`review.targets`\" + ` | unset | " + NA_ROW, "| ` + \"`review.targets`\" + ` | unset | ")]),
 ("onboarding: the merge request says a Developer may ask",
  [(RE, "from a project **Maintainer or Owner**", "from a project **Developer, Maintainer or Owner**")]),
 ("onboarding: the merge request does not say who may ask",
  [(RE, "It starts only on an explicit request from a project **Maintainer or Owner**, in one of the forms listed here:", "It starts on a request in one of the forms listed here:")]),
 ("onboarding: the merge request offers a triage-verdict trigger",
  [(RE, "(mentioning the bot and asking it to implement the issue). Unset means all three;", "(mentioning the bot and asking it to implement the issue), ` + \"`triage-verdict`\" + `. Unset means all three;")]),
 ("onboarding: the merge request does not explain change.require_test_report",
  [(RE, "| ` + \"`change.require_test_report`\" + ` | unset |", "| ` + \"`change.test_report`\" + ` | unset |")]),
 ("onboarding: the merge request says reviews do not need an author workflow",
  [(RE, " It needs ` + \"`actions.pipeline_fixes: true`\" + ` or ` + \"`actions.features: true`\" + ` in this file; writing it with neither is refused as a configuration error.", "")]),
 ("onboarding: the merge request says a review may approve",
  [(RE, "That session never approves, never assigns itself, never opens a discussion thread and never merges.", "That session may approve the merge request.")]),
 ("onboarding: the merge request says people's merge requests are reviewed",
  [(RE, "Merge requests written by people are not reviewed. ", "")]),
 ("onboarding: the merge request says a project may choose its reviewer",
  [(RE, "Which model does the reviewing is chosen by the operator of this gonk instance, not in this file; writing ` + \"`rung`\" + ` here is refused.", "Set ` + \"`rung`\" + ` to choose the reviewing model.")]),
 ("onboarding: the merge request says docs means everything under docs/",
  [(RE, "meaning prose files only (` + \"`.md`\" + `, ` + \"`.rst`\" + `, ` + \"`.txt`\" + `, ` + \"`.adoc`\" + `)", "meaning anything under docs/")]),
 ("onboarding: the merge request says the agentic mode works without a distinct reviewer",
  [(RE, " -- where the operator has configured no such reviewer that mode is unavailable and merges nothing", "")]),
 ("onboarding: the merge request says gonk runs the tests itself",
  [(RE, " gonk runs nothing itself: your pipeline is the only thing that tests the change.", " gonk runs your tests in its own sandbox.")]),
 ("onboarding: the fix auto_merge row does not mention max_per_day",
  [(RE, "A change to source or test files is never merged automatically. ` + \"`max_per_day`\" + ` (default and maximum 3) caps automatic merges in 24 hours. |\n| ` + \"`change.triggers`\"",
        "A change to source or test files is never merged automatically. |\n| ` + \"`change.triggers`\"")]),
]


def sha(path):
    return hashlib.sha256(open(path, 'rb').read()).hexdigest() + "\n"


args = sys.argv[1:]
verbose = '-v' in args
check = '-c' in args
if '-l' in args:
    for i, (name, _) in enumerate(muts):
        print(f"[{i}] {name}")
    sys.exit(0)
only = [a for a in args if a not in ('-v', '-c')]
res = []
for i, (name, edits) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, old, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if old not in cur[f] or old == new:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    if check:
        res.append(f"KILLED   [{i}] (text found, not run) {name}")
        continue
    # Seal files are saved too, so the finally block puts them back.
    for f in list(cur):
        if f in SEALED:
            for extra in SEALED[f]:
                if extra and extra not in originals:
                    originals[extra] = open(extra).read()
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        for f in cur:
            if f in SEALED:
                sumfile, published = SEALED[f]
                open(sumfile, 'w').write(sha(f))
                if published:
                    open(published, 'w').write(cur[f])
        pkgs = [p for p in PKGS if any(p in DEPENDENTS[f] for f in cur)]
        r = subprocess.run([GO, "test", "-vet=off"] + pkgs + ["-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        lines = out.splitlines()
        fails = [l.strip() for l in lines if l.strip().startswith("--- FAIL")]
        tops = {l.split()[2] for l in fails if "/" not in l.split()[2]}
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif fails and tops <= GOLDEN_TESTS:
            res.append(f"GOLDEN-ONLY [{i}] {name}\n           only " + ", ".join(sorted(tops)) + " failed")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            shown = fails[:6] + ([f"... and {len(fails) - 6} more"] if len(fails) > 6 else [])
            msg = f"KILLED   [{i}] {name}\n           " + "\n           ".join(shown)
            if verbose:
                # The first message line under the first failure: what red looked like.
                first = lines.index(next(l for l in lines if l.strip().startswith("--- FAIL")))
                detail = [l.strip() for l in lines[first + 1:first + 12]
                          if l.startswith("    ") and not l.strip().startswith("--- ")][:2]
                msg += "\n             > " + "\n             > ".join(d[:300] for d in detail)
            res.append(msg)
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} of {len(res)}")
sys.exit(1 if bad else 0)
