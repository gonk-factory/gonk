#!/usr/bin/env python3
"""mutate_pipefix_config.py -- "seen red" evidence for gonk-2usw part CONFIG.

Applies one named break at a time to the pipeline-fix configuration surface
(`actions.pipeline_fixes`, the `pipeline_fix:` section in both schemas and both
loaders, the fold, the `pipeline-fix` trigger, its action gate, ladder floor and
reservation TTL, the wire types, intake's pre-filter and the onboarding text),
runs the packages that guard it, and requires a test to FAIL; then restores
every file byte for byte (try/finally). A mutation whose text is not found is
VACUOUS, one the tests do not notice SURVIVED, and one that does not compile
NOCOMPILE -- each is a failure of this script.

The first block is the breaks the plan names for part CONFIG (section 11):
"unknown key accepted", "`pipeline_fixes` without `pipelines` accepted",
"trigger not gated".

RESEALING. Three files are guarded by a checksum test that fails on ANY edit
(the two JSON Schemas, pkg/meterapi/meterapi.go), and each schema has a
published copy under docs/schemas that must be byte-identical. Left alone,
those drift tests would "kill" every mutant of those files and prove nothing
about behaviour. So after applying a mutant this script re-seals: it copies the
mutated schema over its published copy and rewrites the checksum files. A
mutant of those files is then killed only by a test that noticed what changed.

Usage: hack/mutate_pipefix_config.py            (all mutations)
       hack/mutate_pipefix_config.py 0 1 2      (only these indexes)
       hack/mutate_pipefix_config.py -v ...     (also print each failing test's message)
       hack/mutate_pipefix_config.py -l         (list the mutations, run nothing)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate;
its output is pasted into the plan's evidence log.
"""
import hashlib
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
PKGS = ['./pkg/gonkcfg/', './pkg/opercfg/', './pkg/atags/', './pkg/rung/', './pkg/meterapi/',
        './pkg/intake/', './internal/meter/service/']

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
ST = 'pkg/intake/state.go'
DI = 'pkg/intake/dispatch.go'
RE = 'pkg/intake/render.go'

# file -> (checksum file, published copy or None)
SEALED = {
    PS: ('pkg/gonkcfg/testdata/schema.v1.sha256', 'docs/schemas/gonk-config.v1.schema.json'),
    OS_: ('pkg/opercfg/testdata/schema.v1.sha256', 'docs/schemas/gonk-operator.v1.schema.json'),
    MA: ('pkg/meterapi/testdata/contract.sha256', None),
}

FIX_SECTION = '"pipeline_fix": {\n      "type": "object",\n      "additionalProperties": false,'
OP_FIX_SECTION = '"pipeline_fix": {\n          "type": "object",\n          "additionalProperties": false,'
MERGE_SECTION = '"auto_merge": {\n          "type": "object",\n          "additionalProperties": false,'
OP_MERGE_SECTION = '"auto_merge": {\n              "type": "object",\n              "additionalProperties": false,'
IF_THEN = ('      "if": { "properties": { "pipeline_fixes": { "const": true } }, "required": ["pipeline_fixes"] },\n'
           '      "then": { "properties": { "pipelines": { "const": true } }, "required": ["pipelines"] }\n')
FIX_CASE = "\tcase atags.TriggerPipelineFix:\n"
FIX_GATE = "\t\treturn eff.Actions.PipelineFixes, true\n"
RESOLVE_ACTION = ("\teff.Actions.PipelineFixes = eff.Actions.Pipelines &&\n"
                  "\t\tandAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes, group.Actions.PipelineFixes)\n")
# No closing brace: gonk-cuwn part X-CONFIG added entries after pipeline-fix.
FLOORS = "var LadderFloors = map[string]int{\n\tatags.TriggerPipelineDiagnosis: 1,\n\tatags.TriggerPipelineFix:       1,\n"
RETRIES = "var FloorRetries = map[string]int{\n\tatags.TriggerPipelineDiagnosis: 1,\n\tatags.TriggerPipelineFix:       1,\n"
MAYFIX = ("\tswitch c.State {\n\tcase StateValid, StatePending:\n"
          "\t\treturn c.eff().Actions.PipelineFixes && c.eff().Actions.Pipelines\n\t}\n\treturn false\n")
TTL_RETURN = "\treturn base + min(max(eff.PipelineFix.VerifyTimeout, 0), gonkcfg.FixVerifyTimeoutCeiling)\n"
SVC_TTL = "rung.ReservationTTL(req.Trigger, cfg.Meter.ReservationTTL, reg.Effective)"

# (name, [(file, old, new), ...])
muts = [
 # ---- the three breaks the plan names (section 11, row CONFIG) ----
 ("PLAN: unknown key accepted (.gonk.yml pipeline_fix section)",
  [(PS, FIX_SECTION, FIX_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (.gonk.yml pipeline_fix.auto_merge)",
  [(PS, MERGE_SECTION, MERGE_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (operator pipeline_fix section)",
  [(OS_, OP_FIX_SECTION, OP_FIX_SECTION.replace("false", "true"))]),
 ("PLAN: unknown key accepted (operator pipeline_fix.auto_merge)",
  [(OS_, OP_MERGE_SECTION, OP_MERGE_SECTION.replace("false", "true"))]),
 ("PLAN: pipeline_fixes without pipelines accepted (schema if/then removed)",
  [(PS, '      },\n' + IF_THEN, '      }\n')]),
 ("PLAN: pipeline_fixes without pipelines accepted (Go check removed)",
  [(PF, "\tif on(p.Actions.PipelineFixes) && !on(p.Actions.Pipelines) {", "\tif false {")]),
 ("PLAN: pipeline_fixes without pipelines accepted (BOTH guards removed: the end-to-end registration test)",
  [(PS, '      },\n' + IF_THEN, '      }\n'),
   (PF, "\tif on(p.Actions.PipelineFixes) && !on(p.Actions.Pipelines) {", "\tif false {")]),
 ("PLAN: pipeline_fixes without pipelines accepted (`pipelines: false` counts as present)",
  [(PS, '"then": { "properties": { "pipelines": { "const": true } }, "required": ["pipelines"] }', '"then": { "required": ["pipelines"] }'),
   (PF, "\tif on(p.Actions.PipelineFixes) && !on(p.Actions.Pipelines) {", "\tif on(p.Actions.PipelineFixes) && p.Actions.Pipelines == nil {")]),
 ("PLAN: trigger not gated (pipeline-fix gated by nothing)",
  [(RD, FIX_GATE, "\t\treturn true, false\n")]),
 ("PLAN: trigger not gated (pipeline-fix gated by actions.pipelines)",
  [(RD, FIX_GATE, "\t\treturn eff.Actions.Pipelines, true\n")]),
 ("PLAN: trigger not gated (pipeline-fix gated by actions.triage)",
  [(RD, FIX_GATE, "\t\treturn eff.Actions.Triage, true\n")]),
 ("PLAN: trigger not gated (the resolved action ignores the project's opt-in: fixes follow pipelines)",
  [(RS, RESOLVE_ACTION, "\teff.Actions.PipelineFixes = eff.Actions.Pipelines\n")]),

 # ---- the schema, key by key ----
 ("schema: classes accepts flaky",
  [(PS, '"items": { "enum": ["code", "test", "configuration"] }', '"items": { "enum": ["code", "test", "configuration", "flaky", "infrastructure", "unknown"] }')]),
 ("schema: targets accepts tag",
  [(PS, '"items": { "enum": ["ref", "mr"] }', '"items": { "enum": ["ref", "mr", "tag"] }')]),
 ("schema: ci_exec_changes accepts always",
  [(PS, '"ci_exec_changes": { "enum": ["deny", "manual-run", "auto"]', '"ci_exec_changes": { "enum": ["deny", "manual-run", "auto", "always"]')]),
 ("schema: max_attempts ceiling raised",
  [(PS, '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 3,', '"max_attempts": { "type": "integer", "minimum": 1, "maximum": 30,')]),
 ("schema: max_attempts may be zero",
  [(PS, '"max_attempts": { "type": "integer", "minimum": 1,', '"max_attempts": { "type": "integer", "minimum": 0,')]),
 ("schema: max_chain ceiling raised",
  [(PS, '"max_chain": { "type": "integer", "minimum": 1, "maximum": 10,', '"max_chain": { "type": "integer", "minimum": 1, "maximum": 100,')]),
 ("schema: max_files ceiling raised",
  [(PS, '"max_files": { "type": "integer", "minimum": 1, "maximum": 3, "description": "Files one fix', '"max_files": { "type": "integer", "minimum": 1, "maximum": 30, "description": "Files one fix')]),
 ("schema: max_changed_lines ceiling raised",
  [(PS, '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 40,', '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 400,')]),
 ("schema: verify_timeout accepts any string",
  [(PS, '"verify_timeout": { "type": "string", "pattern": "^[0-9]+(s|m|h)$",', '"verify_timeout": { "type": "string",')]),
 ("schema: verify_timeout accepts a bare number",
  [(PS, '"verify_timeout": { "type": "string", "pattern": "^[0-9]+(s|m|h)$",', '"verify_timeout": {')]),
 ("schema: paths accepts any list",
  [(PS, '"paths": { "$ref": "#/$defs/scopePatterns",', '"paths": { "type": "array",')]),
 ("schema: auto_merge.mode accepts always",
  [(PS, '"mode": { "enum": ["off", "after-human-approval", "after-agentic-review"], "description": "Default off.', '"mode": { "enum": ["off", "after-human-approval", "after-agentic-review", "always"], "description": "Default off.')]),
 ("schema: auto_merge.classes accepts source and test",
  [(PS, '"items": { "enum": ["docs", "ci"] }', '"items": { "enum": ["docs", "ci", "source", "test"] }')]),
 ("schema: allow_ci accepts any type",
  [(PS, '"allow_ci": { "type": "boolean", "description": "Default false.', '"allow_ci": { "description": "Default false.')]),
 ("schema: the action accepts any type",
  [(PS, '"pipeline_fixes": { "type": "boolean", "description": "Default false.', '"pipeline_fixes": { "description": "Default false.')]),
 ("operator schema: pipeline_fix gains the project-only paths",
  [(OS_, '            "auto_merge": {\n              "type": "object",', '            "paths": { "$ref": "#/$defs/scopePatterns" },\n            "auto_merge": {\n              "type": "object",')]),
 ("operator schema: classes accepts flaky",
  [(OS_, '"items": { "enum": ["code", "test", "configuration"] }', '"items": { "enum": ["code", "test", "configuration", "flaky"] }')]),
 ("operator schema: max_changed_lines ceiling raised",
  [(OS_, '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 40,', '"max_changed_lines": { "type": "integer", "minimum": 1, "maximum": 400,')]),
 ("operator schema: the veto accepts any type",
  [(OS_, '"pipeline_fixes": { "type": "boolean", "description": "Operator VETO', '"pipeline_fixes": { "description": "Operator VETO')]),
 ("a key loses its description in the published schema (owner rule: documented where the schema lives)",
  [(PS, '"max_chain": { "type": "integer", "minimum": 1, "maximum": 10, "description": "Fix merge requests per red streak, counted since the target was last green. Default 5, ceiling 10. MINIMUM across layers." }',
        '"max_chain": { "type": "integer", "minimum": 1, "maximum": 10 }')]),

 # ---- the Go half of validation ----
 ("load: CheckPipelineFix is not called",
  [(GC, "\tif err := CheckPipelineFix(pc.Policy); err != nil {", "\tif err := error(nil); err != nil {")]),
 ("load: the project's section is validated as an operator's (paths refused)",
  [(PF, "\treturn p.PipelineFix.Validate(scope.LayerProject)", "\treturn p.PipelineFix.Validate(scope.LayerOperator)")]),
 ("validate: list members are not checked",
  [(PF, "\t\t\tif !contains(l.allowed, v) {", "\t\t\tif false {")]),
 ("validate: ci_exec_changes is not checked",
  [(PF, "\t\tif _, ok := ciExecRank[*v]; !ok {", "\t\tif false {")]),
 ("validate: auto_merge.mode is not checked",
  [(PF, "\tif v := p.AutoMerge.Mode; v != nil && !autoMergeModes[*v] {", "\tif v := p.AutoMerge.Mode; v != nil && false {")]),
 ("validate: a number above its ceiling is accepted",
  [(PF, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {", "\t\tif n.v != nil && (*n.v < 1) {")]),
 ("validate: a number below 1 is accepted",
  [(PF, "\t\tif n.v != nil && (*n.v < 1 || *n.v > n.ceiling) {", "\t\tif n.v != nil && (*n.v > n.ceiling) {")]),
 ("validate: verify_timeout above its ceiling is accepted",
  [(PF, "(*v <= 0 || time.Duration(*v) > FixVerifyTimeoutCeiling) {", "(*v <= 0) {")]),
 ("validate: verify_timeout of zero is accepted",
  [(PF, "(*v <= 0 || time.Duration(*v) > FixVerifyTimeoutCeiling) {", "(*v < 0 || time.Duration(*v) > FixVerifyTimeoutCeiling) {")]),
 ("validate: an operator layer may carry paths",
  [(PF, "\tif len(p.Paths) > 0 && layer == scope.LayerOperator {", "\tif false {")]),
 ("validate: path patterns are not checked",
  [(PF, "\t\tif err := scope.ValidatePattern(pat); err != nil {", "\t\tif err := scope.ValidatePattern(pat); err != nil && false {")]),
 ("validate: any number of path patterns",
  [(PF, "\tif len(p.Paths) > scope.MaxPatterns {", "\tif false {")]),
 ("validate: an unknown layer is accepted",
  [(PF, "\tif layer != scope.LayerOperator && layer != scope.LayerProject {", "\tif false {")]),
 ("duration: a bare number is read as nanoseconds",
  [(PF, '\tif n.Kind != yaml.ScalarNode || n.Tag != "!!str" {', '\tif n.Kind != yaml.ScalarNode {'),
   (PF, "\tv, err := time.ParseDuration(n.Value)\n", "\tv, err := time.ParseDuration(n.Value)\n\tif n.Tag == \"!!int\" {\n\t\tv, err = time.ParseDuration(n.Value + \"ns\")\n\t}\n"),
   (PS, '"verify_timeout": { "type": "string", "pattern": "^[0-9]+(s|m|h)$",', '"verify_timeout": {')]),
 ("ceiling: max_attempts ceiling constant raised",
  [(PF, "\tFixMaxAttemptsCeiling     = 3\n", "\tFixMaxAttemptsCeiling     = 30\n")]),
 ("ceiling: max_chain ceiling constant raised",
  [(PF, "\tFixMaxChainCeiling        = 10\n", "\tFixMaxChainCeiling        = 100\n")]),
 ("ceiling: verify_timeout ceiling constant raised",
  [(PF, "\tFixVerifyTimeoutCeiling   = 6 * time.Hour\n", "\tFixVerifyTimeoutCeiling   = 60 * time.Hour\n")]),
 ("ceiling: max_files ceiling constant raised",
  [(PF, "\tFixMaxFilesCeiling        = 3\n", "\tFixMaxFilesCeiling        = 30\n")]),
 ("ceiling: max_changed_lines ceiling constant raised",
  [(PF, "\tFixMaxChangedLinesCeiling = 40\n", "\tFixMaxChangedLinesCeiling = 400\n")]),

 # ---- defaults: everything OFF / conservative ----
 ("default: pipeline fixes are ON for a project that said nothing",
  [(RS, RESOLVE_ACTION, "\teff.Actions.PipelineFixes = eff.Actions.Pipelines &&\n\t\t(project.Actions.PipelineFixes == nil || andAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes, group.Actions.PipelineFixes))\n")]),
 ("default: classes include configuration",
  [(PF, "\tdefaultFixClasses       = []string{FixClassCode, FixClassTest}", "\tdefaultFixClasses       = []string{FixClassCode, FixClassTest, FixClassConfiguration}")]),
 ("default: targets include mr (owner Q7: default branch only)",
  [(PF, "\tdefaultFixTargets       = []string{FixTargetRef}", "\tdefaultFixTargets       = []string{FixTargetRef, FixTargetMR}")]),
 ("default: auto-merge classes include ci",
  [(PF, "\tdefaultAutoMergeClasses = []string{AutoMergeClassDocs}", "\tdefaultAutoMergeClasses = []string{AutoMergeClassDocs, AutoMergeClassCI}")]),
 ("default: ci_exec_changes is auto (owner Q6: manual-run)",
  [(PF, "\texec := CIExecManualRun\n", "\texec := CIExecAuto\n")]),
 ("default: auto-merge mode is after-human-approval (plan 9: default OFF)",
  [(PF, "\tmode := AutoMergeOff\n\tif project.AutoMerge.Mode != nil {", "\tmode := AutoMergeAfterHumanApproval\n\tif project.AutoMerge.Mode != nil {")]),
 ("default: max_attempts is 3",
  [(PF, "\tDefaultFixMaxAttempts     = 2\n", "\tDefaultFixMaxAttempts     = 3\n")]),
 ("default: max_chain is 10",
  [(PF, "\tDefaultFixMaxChain        = 5\n", "\tDefaultFixMaxChain        = 10\n")]),
 ("default: verify_timeout is 6h",
  [(PF, "\tDefaultFixVerifyTimeout   = 2 * time.Hour\n", "\tDefaultFixVerifyTimeout   = 6 * time.Hour\n")]),
 ("default: max_files is 2",
  [(PF, "\tDefaultFixMaxFiles        = 3\n", "\tDefaultFixMaxFiles        = 2\n")]),
 ("default: max_changed_lines is 20",
  [(PF, "\tDefaultFixMaxChangedLines = 40\n", "\tDefaultFixMaxChangedLines = 20\n")]),
 ("default: allow_ci is on without the project's opt-in (operator true grants it)",
  [(PF, "\t\tAllowCI:         andAction(project.AllowCI, op.AllowCI),", "\t\tAllowCI:         on(project.AllowCI) || on(op.AllowCI),")]),

 # ---- the fold: an operator layer only tightens ----
 ("resolve: Resolve does not fold the pipeline_fix section",
  [(RS, "\teff.PipelineFix = ResolvePipelineFix(instance.PipelineFix, group.PipelineFix, project.PipelineFix)\n", "")]),
 ("resolve: the group layer is dropped",
  [(RS, "ResolvePipelineFix(instance.PipelineFix, group.PipelineFix, project.PipelineFix)", "ResolvePipelineFix(instance.PipelineFix, PipelineFixPolicy{}, project.PipelineFix)")]),
 ("resolve: the instance layer is dropped",
  [(RS, "ResolvePipelineFix(instance.PipelineFix, group.PipelineFix, project.PipelineFix)", "ResolvePipelineFix(PipelineFixPolicy{}, group.PipelineFix, project.PipelineFix)")]),
 ("resolve: fixes stay on when an operator vetoes DIAGNOSES",
  [(RS, RESOLVE_ACTION, "\teff.Actions.PipelineFixes = andAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes, group.Actions.PipelineFixes)\n")]),
 ("resolve: the instance veto of pipeline_fixes is ignored",
  [(RS, "andAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes, group.Actions.PipelineFixes)", "andAction(project.Actions.PipelineFixes, group.Actions.PipelineFixes)")]),
 ("resolve: the group veto of pipeline_fixes is ignored",
  [(RS, "andAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes, group.Actions.PipelineFixes)", "andAction(project.Actions.PipelineFixes, instance.Actions.PipelineFixes)")]),
 ("fold: an operator list is ignored",
  [(PF, "\t\tif oper != nil {\n\t\t\tbase = intersectLists(base, oper)\n\t\t}\n", "")]),
 ("fold: an operator list REPLACES the project's (an operator can opt a project in)",
  [(PF, "\t\tif oper != nil {\n\t\t\tbase = intersectLists(base, oper)\n\t\t}\n", "\t\tif oper != nil {\n\t\t\tbase = oper\n\t\t}\n")]),
 ("fold: the project's list is ignored (always the default)",
  [(PF, "\t\tbase := def\n\t\tif proj != nil {\n\t\t\tbase = proj\n\t\t}\n", "\t\tbase := def\n")]),
 ("fold: unknown list members survive",
  [(PF, "\t\tfor _, v := range order {\n\t\t\tif contains(base, v) {\n\t\t\t\tout = append(out, v)\n\t\t\t}\n\t\t}\n\t\treturn out", "\t\tout = append(out, base...)\n\t\t_ = order\n\t\treturn out")]),
 ("fold: an operator number RAISES the project's",
  [(PF, "\t\tif oper != nil && *oper < v {\n\t\t\tv = *oper\n\t\t}\n\t\treturn int(", "\t\tif oper != nil {\n\t\t\tv = *oper\n\t\t}\n\t\treturn int(")]),
 ("fold: an operator number is ignored",
  [(PF, "\t\tif oper != nil && *oper < v {\n\t\t\tv = *oper\n\t\t}\n\t\treturn int(", "\t\treturn int(")]),
 ("fold: numbers are not clamped to the ceiling",
  [(PF, "\t\treturn int(min(max(v, 0), ceiling))", "\t\t_ = ceiling\n\t\treturn int(max(v, 0))")]),
 ("fold: negative numbers are not clamped",
  [(PF, "\t\treturn int(min(max(v, 0), ceiling))", "\t\treturn int(min(v, ceiling))")]),
 ("fold: the operator veto of allow_ci is ignored",
  [(PF, "\t\tAllowCI:         andAction(project.AllowCI, op.AllowCI),", "\t\tAllowCI:         on(project.AllowCI),")]),
 ("fold: the project's ci_exec_changes is ignored",
  [(PF, "\tif project.CIExecChanges != nil {\n\t\texec = *project.CIExecChanges\n\t}\n", "")]),
 ("fold: an operator ci_exec_changes is ignored",
  [(PF, "\tif s := stricterExec(&exec, op.CIExecChanges); s != nil {\n\t\texec = *s\n\t}\n", "")]),
 ("fold: the LOOSER ci_exec_changes wins",
  [(PF, "\tcase rank(b) < rank(a):\n\t\treturn b\n\t}\n\treturn a\n}\n\n// foldMode", "\tcase rank(b) > rank(a):\n\t\treturn b\n\t}\n\treturn a\n}\n\n// foldMode")]),
 ("fold: an unknown ci_exec_changes is kept (not read as deny)",
  [(PF, "\tif _, ok := ciExecRank[exec]; !ok {\n\t\texec = CIExecDeny\n\t}\n", "")]),
 ("fold: an unknown ci_exec_changes ranks as the loosest",
  [(PF, "\t\tif r, ok := ciExecRank[*s]; ok {\n\t\t\treturn r\n\t\t}\n\t\treturn -1", "\t\tif r, ok := ciExecRank[*s]; ok {\n\t\t\treturn r\n\t\t}\n\t\treturn 99"),
   (PF, "\tif _, ok := ciExecRank[exec]; !ok {\n\t\texec = CIExecDeny\n\t}\n", "\tif _, ok := ciExecRank[exec]; !ok {\n\t\texec = CIExecAuto\n\t}\n")]),
 ("fold: the project's verify_timeout is ignored",
  [(PF, "\tif project.VerifyTimeout != nil {\n\t\ttimeout = time.Duration(*project.VerifyTimeout)\n\t}\n", "")]),
 ("fold: an operator verify_timeout RAISES the project's",
  [(PF, "\tif op.VerifyTimeout != nil && time.Duration(*op.VerifyTimeout) < timeout {", "\tif op.VerifyTimeout != nil {")]),
 ("fold: an operator verify_timeout is ignored",
  [(PF, "\tif op.VerifyTimeout != nil && time.Duration(*op.VerifyTimeout) < timeout {", "\tif false {")]),
 ("fold: verify_timeout is not clamped to its ceiling",
  [(PF, "\teff.VerifyTimeout = min(max(timeout, 0), FixVerifyTimeoutCeiling)", "\teff.VerifyTimeout = max(timeout, 0)")]),
 ("fold: a negative verify_timeout is kept",
  [(PF, "\teff.VerifyTimeout = min(max(timeout, 0), FixVerifyTimeoutCeiling)", "\teff.VerifyTimeout = min(timeout, FixVerifyTimeoutCeiling)")]),
 ("fold: the project's paths are dropped",
  [(PF, "\t\tPaths:           append([]string{}, project.Paths...),", "\t\tPaths:           []string{},")]),
 ("fold: the project's auto-merge mode is ignored",
  [(PF, "\tif project.AutoMerge.Mode != nil {\n\t\tmode = *project.AutoMerge.Mode\n\t}\n", "")]),
 ("fold: an operator auto-merge mode is ignored (no veto)",
  [(PF, "\tif m := foldMode(&mode, op.AutoMerge.Mode); m != nil {\n\t\tmode = *m\n\t}\n", "")]),
 ("fold: an operator auto-merge mode WINS (an operator can switch auto-merge on)",
  [(PF, "\toff := AutoMergeOff\n\treturn &off\n}", "\treturn b\n}")]),
 ("fold: an unknown auto-merge mode is kept",
  [(PF, "\tif !autoMergeModes[mode] {\n\t\tmode = AutoMergeOff\n\t}\n", "")]),
 ("operator fold: the instance layer's lists are dropped",
  [(PF, "\t\tClasses:         intersectLists(base.Classes, next.Classes),\n\t\tTargets:         intersectLists(base.Targets, next.Targets),",
        "\t\tClasses:         next.Classes,\n\t\tTargets:         next.Targets,")]),
 ("operator fold: disjoint lists fold to nil (silent) instead of none",
  [(PF, "\tout := []string{}\n\tfor _, v := range a {\n\t\tif contains(b, v) {", "\tvar out []string\n\tfor _, v := range a {\n\t\tif contains(b, v) {")]),
 ("operator fold: two lists are unioned",
  [(PF, "\tout := []string{}\n\tfor _, v := range a {\n\t\tif contains(b, v) {\n\t\t\tout = append(out, v)\n\t\t}\n\t}\n\treturn out",
        "\tout := append([]string{}, a...)\n\tfor _, v := range b {\n\t\tif !contains(a, v) {\n\t\t\tout = append(out, v)\n\t\t}\n\t}\n\treturn out")]),
 ("operator fold: a nested allow_ci true undoes its parent's veto",
  [(PF, "\t\tAllowCI:         vetoFold(base.AllowCI, next.AllowCI),", "\t\tAllowCI:         next.AllowCI,")]),
 ("operator fold: vetoFold lets a later true win",
  [(PF, "\tif a != nil && !*a {\n\t\treturn a\n\t}\n\tif b != nil {\n\t\treturn b\n\t}\n\treturn a", "\tif b != nil {\n\t\treturn b\n\t}\n\treturn a")]),
 ("operator fold: numbers take the LARGER value",
  [(PF, "\tcase *b < *a:\n\t\treturn b\n\t}\n\treturn a\n}\n\n// stricterExec", "\tcase *b > *a:\n\t\treturn b\n\t}\n\treturn a\n}\n\n// stricterExec")]),
 ("operator fold: verify_timeout takes the LARGER value",
  [(PF, "\tcase next.VerifyTimeout == nil || *base.VerifyTimeout <= *next.VerifyTimeout:", "\tcase next.VerifyTimeout == nil || *base.VerifyTimeout >= *next.VerifyTimeout:")]),
 ("operator fold: the instance's ci_exec_changes is dropped",
  [(PF, "\t\tCIExecChanges:   stricterExec(base.CIExecChanges, next.CIExecChanges),", "\t\tCIExecChanges:   next.CIExecChanges,")]),
 ("operator fold: the instance's auto-merge mode is dropped",
  [(PF, "\t\t\tMode:    foldMode(base.AutoMerge.Mode, next.AutoMerge.Mode),", "\t\t\tMode:    next.AutoMerge.Mode,")]),
 ("operator fold: the auto-merge classes are not folded",
  [(PF, "\t\t\tClasses: intersectLists(base.AutoMerge.Classes, next.AutoMerge.Classes),", "\t\t\tClasses: next.AutoMerge.Classes,")]),
 ("operator fold: max_attempts is not folded",
  [(PF, "\t\tMaxAttempts:     minInt(base.MaxAttempts, next.MaxAttempts),", "\t\tMaxAttempts:     next.MaxAttempts,")]),
 ("operator fold: the targets are not folded",
  [(PF, "\t\tTargets:         intersectLists(base.Targets, next.Targets),", "\t\tTargets:         next.Targets,")]),
 ("operator fold: max_chain is not folded",
  [(PF, "\t\tMaxChain:        minInt(base.MaxChain, next.MaxChain),", "\t\tMaxChain:        next.MaxChain,")]),
 ("operator fold: max_files is not folded",
  [(PF, "\t\tMaxFiles:        minInt(base.MaxFiles, next.MaxFiles),", "\t\tMaxFiles:        next.MaxFiles,")]),
 ("operator fold: max_changed_lines is not folded",
  [(PF, "\t\tMaxChangedLines: minInt(base.MaxChangedLines, next.MaxChangedLines),", "\t\tMaxChangedLines: next.MaxChangedLines,")]),

 # ---- allow_ci -> scope.Input.LiftOptIn ----
 ("lift: LiftOptIn opts in without allow_ci",
  [(PF, "\tif !e.AllowCI {\n\t\treturn nil\n\t}\n", "")]),
 ("lift: LiftOptIn never opts in",
  [(PF, "\treturn []scope.Class{scope.ClassCI}\n}", "\treturn nil\n}")]),
 ("lift: TightenScope RAISES a lower scope.caps",
  [(PF, "\t\tif have != nil && *have < v {\n\t\t\tv = *have\n\t\t}", "\t\t_ = have")]),
 ("lift: TightenScope lets a higher scope.caps stand",
  [(PF, "\t\tif have != nil && *have < v {", "\t\tif have != nil {")]),
 ("lift: TightenScope leaves max_lines alone",
  [(PF, "\tproject.Caps.MaxLines = lower(project.Caps.MaxLines, e.MaxChangedLines)\n", "")]),
 ("lift: TightenScope leaves max_files alone",
  [(PF, "\tproject.Caps.MaxFiles = lower(project.Caps.MaxFiles, e.MaxFiles)\n", "")]),

 # ---- the operator config ----
 ("opercfg: the operator's pipeline_fix is not validated in Go",
  [(OC, "\tif err := p.PipelineFix.Validate(scope.LayerOperator); err != nil {", "\tif err := p.PipelineFix.Validate(scope.LayerOperator); err != nil && false {")]),
 ("opercfg: the operator's pipeline_fix is validated as a project's (paths accepted in Go)",
  [(OC, "p.PipelineFix.Validate(scope.LayerOperator)", "p.PipelineFix.Validate(scope.LayerProject)")]),
 ("opercfg: a nested group re-enables pipeline_fixes",
  [(OC, "\t\tPipelineFixes: foldVeto(base.Actions.PipelineFixes, next.Actions.PipelineFixes),", "\t\tPipelineFixes: next.Actions.PipelineFixes,")]),
 ("opercfg: GroupFor drops the pipeline_fixes veto",
  [(OC, "\t\tPipelineFixes: foldVeto(base.Actions.PipelineFixes, next.Actions.PipelineFixes),\n", "")]),
 ("opercfg: GroupFor drops the pipeline_fix section",
  [(OC, "\tout.PipelineFix = gonkcfg.FoldPipelineFix(base.PipelineFix, next.PipelineFix)\n", "")]),
 ("opercfg: a nested group's pipeline_fix replaces its parent's",
  [(OC, "\tout.PipelineFix = gonkcfg.FoldPipelineFix(base.PipelineFix, next.PipelineFix)\n", "\tout.PipelineFix = next.PipelineFix\n")]),

 # ---- the trigger: literal, floor, floor retry, reservation TTL ----
 ("trigger: the ledger literal is renamed",
  [(AT, 'TriggerPipelineFix = "pipeline-fix"', 'TriggerPipelineFix = "pipeline_fix"')]),
 ("trigger: pipeline-fix is not a valid trigger",
  [(AT, "\tTriggerPipelineFix:       {},\n", "")]),
 ("trigger: pipeline-fix has no ActionFor case (falls to the unknown-trigger deny)",
  [(RD, FIX_CASE, "\tcase \"pipeline-fix-disabled\":\n")]),
 ("ladder: pipeline-fix has no floor",
  [(RD, FLOORS, FLOORS.replace("\tatags.TriggerPipelineFix:       1,\n", ""))]),
 ("ladder: pipeline-fix floor is 2",
  [(RD, FLOORS, FLOORS.replace("\tatags.TriggerPipelineFix:       1,\n", "\tatags.TriggerPipelineFix:       2,\n"))]),
 ("ladder: pipeline-fix has no floor retry",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerPipelineFix:       1,\n", ""))]),
 ("ladder: pipeline-fix has two floor retries",
  [(RD, RETRIES, RETRIES.replace("\tatags.TriggerPipelineFix:       1,\n", "\tatags.TriggerPipelineFix:       2,\n"))]),
 ("ttl: pipeline-fix gets the base TTL",
  [(RD, "\tif trigger != atags.TriggerPipelineFix {\n\t\treturn base\n\t}\n\treturn base +", "\tif true {\n\t\treturn base\n\t}\n\treturn base +")]),
 ("ttl: EVERY trigger gets the longer TTL",
  [(RD, "\tif trigger != atags.TriggerPipelineFix {\n\t\treturn base\n\t}\n\treturn base +", "\treturn base +")]),
 ("ttl: pipeline-diagnosis gets the longer TTL",
  [(RD, "\tif trigger != atags.TriggerPipelineFix {\n\t\treturn base\n\t}\n\treturn base +", "\tif trigger != atags.TriggerPipelineFix && trigger != atags.TriggerPipelineDiagnosis {\n\t\treturn base\n\t}\n\treturn base +")]),
 ("ttl: the verify_timeout REPLACES the base",
  [(RD, TTL_RETURN, "\treturn min(max(eff.PipelineFix.VerifyTimeout, 0), gonkcfg.FixVerifyTimeoutCeiling)\n")]),
 ("ttl: no upper clamp",
  [(RD, TTL_RETURN, "\treturn base + max(eff.PipelineFix.VerifyTimeout, 0)\n")]),
 ("ttl: a negative verify_timeout shortens the base",
  [(RD, TTL_RETURN, "\treturn base + min(eff.PipelineFix.VerifyTimeout, gonkcfg.FixVerifyTimeoutCeiling)\n")]),
 ("ttl: always the ceiling, whatever the project's verify_timeout",
  [(RD, TTL_RETURN, "\treturn base + gonkcfg.FixVerifyTimeoutCeiling\n")]),
 ("service: the reservation uses the global TTL for every trigger",
  [(SV, SVC_TTL, "cfg.Meter.ReservationTTL")]),
 ("service: the reservation TTL is computed for the wrong trigger",
  [(SV, SVC_TTL, "rung.ReservationTTL(\"pipeline-fix\", cfg.Meter.ReservationTTL, reg.Effective)")]),
 ("service: the reservation TTL ignores the project's config",
  [(SV, SVC_TTL, "rung.ReservationTTL(req.Trigger, cfg.Meter.ReservationTTL, gonkcfg.Effective{})")]),

 # ---- the wire ----
 ("wire: pipeline_fixes is not sent",
  [(MA, "PipelineFixes: e.Actions.PipelineFixes,\n", "\n")]),
 ("wire: pipeline_fixes mirrors pipelines",
  [(MA, "PipelineFixes: e.Actions.PipelineFixes,", "PipelineFixes: e.Actions.Pipelines,")]),
 ("wire: the action key is renamed",
  [(MA, '`json:"pipeline_fixes"`', '`json:"pipelineFixes"`')]),
 ("wire: the section key is renamed",
  [(MA, '`json:"pipeline_fix"`', '`json:"pipelineFix"`')]),
 ("wire: the action is omitempty (absent instead of false)",
  [(MA, '`json:"pipeline_fixes"`', '`json:"pipeline_fixes,omitempty"`')]),
 ("wire: classes are not sent",
  [(MA, "\t\t\tClasses:              append([]string{}, e.PipelineFix.Classes...),", "\t\t\tClasses:              []string{},")]),
 ("wire: targets are not sent",
  [(MA, "\t\t\tTargets:              append([]string{}, e.PipelineFix.Targets...),", "\t\t\tTargets:              []string{},")]),
 ("wire: classes alias the resolved slice",
  [(MA, "\t\t\tClasses:              append([]string{}, e.PipelineFix.Classes...),", "\t\t\tClasses:              e.PipelineFix.Classes,")]),
 ("wire: an empty list is null",
  [(MA, "\t\t\tPaths:                append([]string{}, e.PipelineFix.Paths...),", "\t\t\tPaths:                append([]string(nil), e.PipelineFix.Paths...),")]),
 ("wire: allow_ci is not sent",
  [(MA, "\t\t\tAllowCI:              e.PipelineFix.AllowCI,\n", "")]),
 ("wire: ci_exec_changes is not sent",
  [(MA, "\t\t\tCIExecChanges:        e.PipelineFix.CIExecChanges,\n", "")]),
 ("wire: max_attempts carries max_chain",
  [(MA, "\t\t\tMaxAttempts:          e.PipelineFix.MaxAttempts,", "\t\t\tMaxAttempts:          e.PipelineFix.MaxChain,")]),
 ("wire: max_chain is not sent",
  [(MA, "\t\t\tMaxChain:             e.PipelineFix.MaxChain,\n", "")]),
 ("wire: verify_timeout is sent in nanoseconds under a seconds key",
  [(MA, "int64(e.PipelineFix.VerifyTimeout / time.Second)", "int64(e.PipelineFix.VerifyTimeout)")]),
 ("wire: max_files carries max_changed_lines",
  [(MA, "\t\t\tMaxFiles:             e.PipelineFix.MaxFiles,", "\t\t\tMaxFiles:             e.PipelineFix.MaxChangedLines,")]),
 ("wire: max_changed_lines is not sent",
  [(MA, "\t\t\tMaxChangedLines:      e.PipelineFix.MaxChangedLines,\n", "")]),
 ("wire: paths are not sent",
  [(MA, "\t\t\tPaths:                append([]string{}, e.PipelineFix.Paths...),", "\t\t\tPaths:                []string{},")]),
 ("wire: the auto-merge mode is not sent",
  [(MA, "\t\t\t\tMode:    e.PipelineFix.AutoMerge.Mode,\n", "")]),
 ("wire: the auto-merge classes are not sent",
  [(MA, "\t\t\t\tClasses: append([]string{}, e.PipelineFix.AutoMerge.Classes...),", "\t\t\t\tClasses: []string{},")]),

 # ---- intake: the pre-filter, the onboarding text ----
 ("intake: MayFixPipelines does not need pipelines",
  [(ST, "\t\treturn c.eff().Actions.PipelineFixes && c.eff().Actions.Pipelines\n", "\t\treturn c.eff().Actions.PipelineFixes\n")]),
 ("intake: MayFixPipelines does not need pipeline_fixes",
  [(ST, "\t\treturn c.eff().Actions.PipelineFixes && c.eff().Actions.Pipelines\n", "\t\treturn c.eff().Actions.Pipelines\n")]),
 ("intake: MayFixPipelines ignores the project's state",
  [(ST, MAYFIX, "\treturn c.eff().Actions.PipelineFixes && c.eff().Actions.Pipelines\n")]),
 ("intake: the gate sends pipeline-fix through the diagnosis predicate",
  [(DI, "\tcase atags.TriggerPipelineFix:\n\t\treturn cls.MayFixPipelines()\n", "\tcase atags.TriggerPipelineFix:\n\t\treturn cls.MayDiagnosePipelines()\n")]),
 ("intake: the gate has no pipeline-fix case",
  [(DI, "\tcase atags.TriggerPipelineFix:\n\t\treturn cls.MayFixPipelines()\n", "")]),
 ("onboarding: the committed .gonk.yml switches pipeline_fixes ON",
  [(RE, "  pipeline_fixes: false    # true (needs pipelines: true)", "  pipeline_fixes: true     # true (needs pipelines: true)")]),
 ("onboarding: the committed .gonk.yml switches fixes AND diagnoses on",
  [(RE, "  pipeline_fixes: false    # true (needs pipelines: true)", "  pipeline_fixes: true     # true (needs pipelines: true)"),
   (RE, "  pipelines: false        # true: gonk diagnoses", "  pipelines: true         # true: gonk diagnoses")]),
 ("onboarding: the .gonk.yml does not mention pipeline_fixes",
  [(RE, "  pipeline_fixes: false    # true (needs pipelines: true): a diagnosis may be\n", "                           # (needs pipelines: true): a diagnosis may be\n")]),
 ("onboarding: the merge request does not explain allow_ci",
  [(RE, "| ` + \"`pipeline_fix.allow_ci`\" + ` | unset |", "| ` + \"`pipeline_fix.lift`\" + ` | unset |")]),
 ("onboarding: the merge request says fixes do not need pipelines",
  [(RE, "(so it needs ` + \"`actions.pipelines: true`\" + ` in this file; writing it without is refused as a configuration error)", "")]),
 ("onboarding: the merge request says manual-run is not the default",
  [(RE, "Optional, default ` + \"`manual-run`\" + `: a pipeline-definition fix", "Optional, default ` + \"`auto`\" + `: a pipeline-definition fix")]),
]


def sha(path):
    return hashlib.sha256(open(path, 'rb').read()).hexdigest() + "\n"


args = sys.argv[1:]
verbose = '-v' in args
if '-l' in args:
    for i, (name, _) in enumerate(muts):
        print(f"[{i}] {name}")
    sys.exit(0)
only = [a for a in args if a != '-v']
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
        r = subprocess.run([GO, "test"] + PKGS + ["-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        lines = out.splitlines()
        fails = [l.strip() for l in lines if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
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
