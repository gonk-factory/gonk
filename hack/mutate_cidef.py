#!/usr/bin/env python3
"""mutate_cidef.py -- "seen red" evidence for gonk-2usw part CIDEF (pkg/cidef).

Disables one predicate at a time in pkg/cidef, runs ONLY the test that guards
it (each mutation names its own -run pattern, so a kill means that predicate's
own fixture went red, not that some unrelated test noticed), and requires it to
FAIL; then restores every file byte for byte (try/finally). A mutation whose
text is not found is VACUOUS, one the named test does not notice SURVIVED, and
one that does not compile NOCOMPILE -- each is a failure of this script.

The key-list mutations are generated from section 6.2's lists: every GATING,
EXEC and free key is dropped from its list in turn, and the fixture test for
THAT key must go red.

Usage: hack/mutate_cidef.py            (all mutations)
       hack/mutate_cidef.py 3 7        (only these indexes)
       hack/mutate_cidef.py --list     (names and indexes, nothing run)
       hack/mutate_cidef.py --show 3   (also print the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate
(gonk-5ed1); its output is pasted into the plan's evidence log.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
P = 'pkg/cidef/'
K = P + 'keys.go'
D = P + 'delta.go'
E = P + 'effective.go'
V = P + 'value.go'

GATING = ["rules", "only", "except", "when", "allow_failure", "retry", "start_in", "trigger", "parallel"]
EXEC = ["script", "before_script", "after_script", "image", "services", "variables",
        "hooks", "id_tokens", "secrets", "inherit"]
FREE = ["needs", "dependencies", "artifacts", "cache", "tags", "timeout", "interruptible",
        "coverage", "environment", "resource_group"]

T_GATING = 'TestEachGatingJobKeyChangedIsReported'
T_GGLOBAL = 'TestEachGatingGlobalKeyChangedIsReported'
T_EXEC_OTHER = 'TestExecChangeOnANonDiagnosedJobIsAViolation'
T_EXEC_DIAG = 'TestExecChangeOnADiagnosedJobIsExecChanging'
T_GVARS = 'TestGlobalVariablesAreAnExecKeyOfEveryJob'
T_FREE = 'TestFreeKeysNewJobsAndAppendedStagesArePreserving'
T_REF = 'TestReferenceIsUnresolvableNotEqual'
T_UNRES = 'TestUnresolvableConstructs'
T_BROKEN = 'TestADocumentThatCannotBeReadIsUnresolvable'
T_IMPL = 'TestImplicitStagesAreNotStagesOfTheList'


def drop(key):
    """Remove one key from whichever list in keys.go holds it."""
    src = open(K).read()
    for old, new in (('"%s", ' % key, ''), (', "%s",\n' % key, ',\n'), ('\t"%s",' % key, '\t')):
        if src.count(old) == 1:
            return (K, old, new)
    return (K, '"%s"' % key, '')  # reported as VACUOUS or NOCOMPILE, never silently skipped


# (name, [(file, old, new), ...], -run pattern of the guarding test)
muts = []
# ---- the key lists: section 6.2's table, key by key ----
for k in GATING:
    muts.append(("keys: GATING key `%s` is no longer in the list" % k, [drop(k)], '^%s$/^%s$' % (T_GATING, k)))
for k in EXEC:
    muts.append(("keys: EXEC key `%s` is no longer in the list" % k, [drop(k)], '^%s$/^%s$' % (T_EXEC_OTHER, k)))
for k in FREE:
    muts.append(("keys: free key `%s` is no longer in the list" % k, [drop(k)], '^%s$/^free_%s$' % (T_FREE, k)))
muts += [
 ("keys: a GATING key moved to the free list (`retry`)",
  [(K, '"allow_failure", "retry", "start_in"', '"allow_failure", "start_in"'),
   (K, '"coverage", "environment", "resource_group",', '"coverage", "environment", "resource_group", "retry",')],
  '^%s$/^retry$' % T_GATING),
 ("keys: an EXEC key moved to the free list (`image`)",
  [(K, '"after_script", "image", "services"', '"after_script", "services"'),
   (K, '"coverage", "environment", "resource_group",', '"coverage", "environment", "resource_group", "image",')],
  '^%s$/^image$' % T_EXEC_OTHER),
 ("keys: the plan's table and the code lists may differ (a key added to the code only)",
  [(K, '"coverage", "environment", "resource_group",', '"coverage", "environment", "resource_group", "pages",')],
  '^TestKeyListsMatchThePlan$'),
 ("keys: a key the table does not name is free",
  [(K, "\treturn ClassUnclassified\n}", "\treturn ClassFree\n}")],
  '^(TestEveryKeyIsInExactlyOneClass|%s)$/^unknown_key$' % T_UNRES),
 # ---- D2: GATING ----
 ("D2: a changed GATING job key is not a violation",
  [(D, '\t\t\tcase ClassGating:\n\t\t\t\tviolation(RuleD2, name, key, "a GATING key differs from the base")\n',
       '\t\t\tcase ClassGating:\n')],
  '^%s$' % T_GATING),
 ("D2: `workflow` is not compared",
  [(D, "if global(GlobalWorkflow, base.Workflow, base.HasWorkflow, head.Workflow, head.HasWorkflow) {",
       "if false && global(GlobalWorkflow, base.Workflow, base.HasWorkflow, head.Workflow, head.HasWorkflow) {")],
  '^%s$/^gating_global_workflow$' % T_GGLOBAL),
 ("D2: the base stage ORDER is not checked (a reorder or an insertion passes)",
  [(D, "\t\tif head[i] != s {\n\t\t\treturn nil, false\n\t\t}", "\t\tif false && head[i] != s {\n\t\t\treturn nil, false\n\t\t}")],
  '^%s$/^gating_global_stages_reordered$' % T_GGLOBAL),
 ("D2: stages need only keep the base order as a SUBSEQUENCE (an inserted stage passes)",
  [(D, "\tfor i, s := range base {\n\t\tif head[i] != s {\n\t\t\treturn nil, false\n\t\t}\n\t}\n\treturn append([]string(nil), head[len(base):]...), true",
       "\tj := 0\n\tfor _, s := range head {\n\t\tif j < len(base) && base[j] == s {\n\t\t\tj++\n\t\t} else {\n\t\t\tappended = append(appended, s)\n\t\t}\n\t}\n\treturn appended, j == len(base)")],
  '^%s$/^gating_global_stages_inserted$' % T_GGLOBAL),
 # ---- gonk-er0o: GitLab's implicit stages, on the REAL merged YAML ----
 ("stages: the implicit stages are not set aside at all (the defect: a stage appended before `.post` is D2)",
  [(D, "stagesAppended(listedStages(base.Stages), listedStages(head.Stages))", "stagesAppended(base.Stages, head.Stages)")],
  '^TestGitLabMergedYAMLDeltas$/^stage_appended$'),
 ("stages: the implicit stages are set aside on the HEAD side only",
  [(D, "stagesAppended(listedStages(base.Stages), listedStages(head.Stages))", "stagesAppended(base.Stages, listedStages(head.Stages))")],
  '^%s$/^appended,_both_merged$' % T_IMPL),
 ("stages: the implicit stages are set aside on the BASE side only",
  [(D, "stagesAppended(listedStages(base.Stages), listedStages(head.Stages))", "stagesAppended(listedStages(base.Stages), head.Stages)")],
  '^%s$/^the_same_list,_merged_on_the_head_side_only$' % T_IMPL),
 ("stages: `.post` is not implicit (only `.pre` is set aside)",
  [(K, "var ImplicitStages = []string{StagePre, StagePost}", "var ImplicitStages = []string{StagePre}")],
  '^TestGitLabMergedYAMLDeltas$/^stage_appended$'),
 ("stages: `.pre` is not implicit (only `.post` is set aside)",
  [(K, "var ImplicitStages = []string{StagePre, StagePost}", "var ImplicitStages = []string{StagePost}")],
  '^%s$/^appended,_base_hand-written_and_head_merged$' % T_IMPL),
 ("stages: only a TRAILING `.post` is set aside (one named elsewhere is a stage of the list)",
  [(D, "\t\tif !implicit {\n\t\t\tout = append(out, s)\n\t\t}",
       "\t\tif !implicit || (s == StagePost && &s != &stages[len(stages)-1] && stages[len(stages)-1] != s) {\n\t\t\tout = append(out, s)\n\t\t}")],
  '^%s$/^appended,_the_head_names_only_.post,_and_not_last$' % T_IMPL),
 ("stages: every stage after the first difference is 'appended' (an INSERTED stage is free on merged YAML)",
  [(D, "\tfor i, s := range base {\n\t\tif head[i] != s {\n\t\t\treturn nil, false\n\t\t}\n\t}\n\treturn append([]string(nil), head[len(base):]...), true",
       "\tj := 0\n\tfor _, s := range head {\n\t\tif j < len(base) && base[j] == s {\n\t\t\tj++\n\t\t} else {\n\t\t\tappended = append(appended, s)\n\t\t}\n\t}\n\treturn appended, j == len(base)")],
  '^TestGitLabMergedYAMLDeltas$/^stage_inserted$'),
 ("stages: a job's `stage` is normalised too (a job moved into `.pre` is not a change)",
  [(D, "\t\t\tbv, bHas := bj.Keys[key]\n\t\t\thv, hHas := hj.Keys[key]\n",
       "\t\t\tbv, bHas := bj.Keys[key]\n\t\t\thv, hHas := hj.Keys[key]\n\t\t\tif key == KeyStage && (hv == StagePre || hv == StagePost) {\n\t\t\t\thv = bv\n\t\t\t}\n")],
  '^%s$/^a_job_moved_INTO_.p' % T_IMPL),
 ("stages: the plan's implicit-stages paragraph and the code's list may differ",
  [(K, "var ImplicitStages = []string{StagePre, StagePost}", 'var ImplicitStages = []string{StagePre, StagePost, ".gonk"}')],
  '^TestImplicitStagesMatchThePlan$'),
 ("merged YAML: `default:` kept by GitLab is not applied to its jobs",
  [(E, "\t\t\tif _, own := keys[k]; !own && inheritDefault(k) {", "\t\t\tif _, own := keys[k]; false && !own && inheritDefault(k) {")],
  '^TestGitLabMergedYAMLResolves$'),
 ("merged YAML: the `extends` GitLab keeps stays a key of the effective definition",
  [(E, "\t\t\tif k != keyExtends {\n\t\t\t\tkeys[k] = v\n\t\t\t}", "\t\t\tkeys[k] = v")],
  '^TestGitLabMergedYAMLResolves$'),
 ("merged YAML: a change that is only in an INCLUDED file is not a D3 violation",
  [(D, "\t\t\t\tif !inT[name] {\n", "\t\t\t\tif false && !inT[name] {\n")],
  '^TestGitLabMergedYAMLDeltas$/^include$'),
 ("merged YAML: a job added with a resolved `!reference` is not reported",
  [(D, "\t\t\td.Added = append(d.Added, name)\n", "")],
  '^TestGitLabMergedYAMLDeltas$/^job_added$'),
 ("D2: `default.retry` is not compared as a global key",
  [(D, "if global(GlobalDefaultRetry, base.DefaultRetry,", "if false && global(GlobalDefaultRetry, base.DefaultRetry,")],
  '^%s$/^gating_global_default_retry$' % T_GGLOBAL),
 ("D2: `default.retry` does not reach a job's effective retry",
  [(E, "\t\t\tif _, own := keys[k]; !own && inheritDefault(k) {", '\t\t\tif _, own := keys[k]; !own && k != "retry" && inheritDefault(k) {')],
  '^%s$/^gating_global_default_retry$' % T_GGLOBAL),
 # ---- D3: EXEC ----
 ("D3: an EXEC change on a job that was not diagnosed is not a violation",
  [(D, "\t\t\t\tif !inT[name] {\n", "\t\t\t\tif false && !inT[name] {\n")],
  '^%s$' % T_EXEC_OTHER),
 ("D3: an EXEC change is a violation even on the diagnosed job",
  [(D, "\t\t\t\tif !inT[name] {\n", "\t\t\t\tif true || !inT[name] {\n")],
  '^%s$' % T_EXEC_DIAG),
 ("D3: an EXEC change on the diagnosed job is `preserving`",
  [(D, "\t\t\tcase ClassExec:\n\t\t\t\texecChanged = true\n", "\t\t\tcase ClassExec:\n\t\t\t\totherChanged = true\n")],
  '^%s$' % T_EXEC_DIAG),
 ("D3: the diagnosed set is not mapped through `parallel` job names (T is empty)",
  [(D, "\t\tinT[defName] = true\n", "\t\t_ = defName\n")],
  '^%s$' % T_EXEC_DIAG),
 ("D3: a changed global `variables` is not a violation for the jobs outside T",
  [(D, "\t\tif outsideT {\n", "\t\tif false && outsideT {\n")],
  '^%s$' % T_GVARS),
 ("D3: a changed global `variables` is a violation even when every job was diagnosed",
  [(D, "\t\tif outsideT {\n", "\t\tif true || outsideT {\n")],
  '^%s$' % T_GVARS),
 ("D3: global `variables` do not reach a job's effective variables",
  [(E, "\tinherited := map[string]any{}\n\tfor name, v := range global {\n\t\tif inherit(name) {",
       "\tinherited := map[string]any{}\n\tfor name, v := range global {\n\t\tif false && inherit(name) {")],
  '^%s$' % T_GVARS),
 # ---- D1: existence ----
 ("D1: a removed job is not a violation",
  [(D, '\t\t\tviolation(RuleD1, name, "", "the job is not defined at head")\n', '')],
  '^TestRemovedRenamedAndHiddenJobs$'),
 ("D1: a removed job is not listed",
  [(D, "\t\t\td.Removed = append(d.Removed, name)\n", "")],
  '^TestRemovedRenamedAndHiddenJobs$'),
 ("D1: a hidden job (leading dot) still counts as the job",
  [(E, '\t\tif reservedTopLevel[name] || strings.HasPrefix(name, ".") {', '\t\tif reservedTopLevel[name] {')],
  '^TestRemovedRenamedAndHiddenJobs$/^job_hidden$'),
 ("D1: a rename is paired with a job whose definition differs",
  [(D, "\t\treturn reflect.DeepEqual(bj.Keys, hj.Keys)\n\t}\n\tmatchesOf", "\t\treturn true\n\t}\n\tmatchesOf")],
  '^TestRemovedRenamedAndHiddenJobs$/^job_replaced$'),
 ("D1: a changed stage is not a violation",
  [(D, '\t\t\tcase ClassExistence:\n\t\t\t\tviolation(RuleD1, name, key, "the job\'s stage differs from the base")\n',
       '\t\t\tcase ClassExistence:\n')],
  '^TestAChangedStageIsAnExistenceViolation$'),
 ("D1: the default stage is not normalised (an explicit `stage: test` reads as a move)",
  [(E, "\t\t\tkeys[KeyStage] = DefaultStage\n", "")],
  '^TestAChangedStageIsAnExistenceViolation$'),
 # ---- free ----
 ("free: an added job is not reported",
  [(D, "\t\t\td.Added = append(d.Added, name)\n", "")],
  '^%s$/^free_new_job$' % T_FREE),
 ("free: appended stages are not reported",
  [(D, "\t} else if len(appended) > 0 {", "\t} else if false && len(appended) > 0 {")],
  '^%s$/^free_stage_appended$' % T_FREE),
 ("free: a changed free key is exec-changing",
  [(D, "\t\t\tcase ClassFree:\n\t\t\t\totherChanged = true\n", "\t\t\tcase ClassFree:\n\t\t\t\texecChanged = true\n")],
  '^%s$' % T_FREE),
 # ---- unresolvable is never "equal" ----
 ("unresolvable: a custom tag (!reference) is read as the plain value under it",
  [(V, "\tif tag, custom := customTag(n); custom {\n", "\tif tag, custom := customTag(n); custom && false {\n")],
  '^%s$' % T_REF),
 ("unresolvable: the same unresolved value on both sides compares equal",
  [(D, "\tcase bun && hun:\n\t\treturn SideBoth, br, true\n", "\tcase bun && hun:\n\t\treturn \"\", \"\", false\n")],
  '^%s$' % T_REF),
 ("unresolvable: an unresolved value is compared like any other",
  [(D, "\t\t\tif side, reason, un := unresolvedIn(bv, bHas, hv, hHas); un {\n\t\t\t\tif class == ClassFree {",
       "\t\t\tif side, reason, un := unresolvedIn(bv, bHas, hv, hHas); un && false {\n\t\t\t\tif class == ClassFree {")],
  '^%s$' % T_REF),
 ("unresolvable: Kind stays `none` when something could not be resolved",
  [(D, "\tcase len(d.Unresolvable) > 0:\n\t\td.Kind = KindUnresolvable\n", "\tcase false:\n\t\td.Kind = KindUnresolvable\n")],
  '^%s$' % T_REF),
 ("unresolvable: an unresolved FREE key is reported as equal",
  [(D, "\t\t\t\t\tjd.Changes = append(jd.Changes, change)\n\t\t\t\t\totherChanged = true\n\t\t\t\t\tcontinue\n", "\t\t\t\t\tcontinue\n")],
  '^%s$/^reference_free_key$' % T_UNRES),
 ("unresolvable: an `include:` left in the merged YAML is ignored",
  [(E, '\tif _, has := root["include"]; has {', '\tif _, has := root["include"]; has && false {')],
  '^%s$/^include_left$' % T_UNRES),
 ("unresolvable: a child pipeline (trigger: include) is compared as a value",
  [(E, '\t\t\tif _, child := t["include"]; child {', '\t\t\tif _, child := t["include"]; child && false {')],
  '^%s$/^child_pipeline$' % T_UNRES),
 ("unresolvable: an `extends` that names nothing extends nothing",
  [(E, '\tif !has {\n\t\treturn &resolved{problems: []string{fmt.Sprintf("`extends` names %q, which the merged YAML does not define", name)}}',
       '\tif !has {\n\t\treturn &resolved{keys: map[string]any{}}')],
  '^%s$/^extends_missing$' % T_UNRES),
 ("unresolvable: a job whose definition has a problem is compared key by key anyway",
  [(D, "\t\tif len(bj.Problems)+len(hj.Problems) > 0 {\n\t\t\tfor _, p := range sided(bj.Problems, hj.Problems) {",
       "\t\tif false {\n\t\t\tfor _, p := range sided(bj.Problems, hj.Problems) {")],
  '^%s$/^extends_missing$' % T_UNRES),
 ("unresolvable: an `extends` that is not a name is ignored",
  [(E, '\tif !ok {\n\t\treturn &resolved{problems: []string{fmt.Sprintf("the `extends` of %q is not a name or a list of names", name)}}',
       '\tif !ok {\n\t\treturn &resolved{keys: own}')],
  '^%s$/^extends_not_names$' % T_UNRES),
 ("unresolvable: an `extends` chain of any depth is resolved",
  [(E, "\tif depth > maxExtendsDepth {", "\tif false && depth > maxExtendsDepth {")],
  '^(TestExtendsDepth|%s)$/^extends_deep$' % T_UNRES),
 ("unresolvable: a malformed `inherit` inherits everything",
  [(E, "\t\tif err != nil {\n\t\t\tjob.Problems = append(job.Problems, err.Error())\n\t\t\tcontinue\n\t\t}",
       "\t\tif err != nil {\n\t\t\tinheritDefault, inheritVars = func(string) bool { return true }, func(string) bool { return true }\n\t\t}")],
  '^%s$/^inherit_malformed$' % T_UNRES),
 ("unresolvable: a changed key the table does not name is a free change",
  [(D, "\t\t\tdefault:\n\t\t\t\tunresolvable(SideBoth, name, key,\n\t\t\t\t\tfmt.Sprintf(\"the key %q changed and section 6.2's table has no rule for it\", key))\n",
       "\t\t\tdefault:\n\t\t\t\totherChanged = true\n")],
  '^%s$/^unknown_key$' % T_UNRES),
 ("unresolvable: an unresolved GLOBAL variable does not reach the jobs that inherit it",
  [(E, "\tfor name, v := range global {\n\t\tif inherit(name) {", "\tfor name, v := range global {\n\t\tif false && inherit(name) {")],
  '^%s$/^global_variables_reference$' % T_UNRES),
 ("unresolvable: an unresolved global variable reaches a job that inherits NO variables",
  [(E, "\tfor name, v := range global {\n\t\tif inherit(name) {", "\tfor name, v := range global {\n\t\tif true || inherit(name) {")],
  '^%s$/^global_variables_reference$' % T_UNRES),
 ("unresolvable: a merge key of something that is not a mapping is dropped",
  [(V, "\t\t\tif !ok {\n\t\t\t\treturn Unresolved{Reason: fmt.Sprintf(\"a merge key (<<) whose value is not a mapping (line %d)\", s.Line)}\n\t\t\t}",
       "\t\t\tif !ok {\n\t\t\t\tcontinue\n\t\t\t}")],
  '^%s$/^merge_not_mapping$' % T_UNRES),
 ("unresolvable: a document that cannot be read is compared as an empty one",
  [(D, "\tif bp, hp := documentProblems(base), documentProblems(head); len(bp)+len(hp) > 0 {",
       "\tif bp, hp := documentProblems(base), documentProblems(head); len(bp)+len(hp) > 0 && (base == nil || head == nil) {")],
  '^%s$' % T_BROKEN),
 ("unresolvable: a second YAML document in the stream is ignored",
  [(E, "\tif err := dec.Decode(&second); !errors.Is(err, io.EOF) {", "\tif err := dec.Decode(&second); false && !errors.Is(err, io.EOF) {")],
  '^%s$/^two_documents$' % T_BROKEN),
 ("unresolvable: a duplicate key is accepted (the last one wins)",
  [(V, "\t\tif _, dup := out[k.Value]; dup {", "\t\tif _, dup := out[k.Value]; dup && false {")],
  '^(%s|TestADuplicateKeyInsideAJobIsUnresolvable)$/^duplicate_job$' % T_BROKEN),
 ("unresolvable: an empty document is a definition with no jobs",
  [(E, "\tif len(bytes.TrimSpace(mergedYAML)) == 0 {\n\t\treturn nil, []string{\"no merged CI YAML\"}\n\t}\n", ""),
   (E, "\t\tif errors.Is(err, io.EOF) {\n\t\t\treturn nil, []string{\"the merged CI YAML holds no document\"}\n\t\t}",
       "\t\tif errors.Is(err, io.EOF) {\n\t\t\treturn map[string]any{}, nil\n\t\t}")],
  '^%s$/^empty$' % T_BROKEN),
 ("unresolvable: a top-level key that is not a mapping is a job with no keys",
  [(E, '\t\treturn &resolved{problems: []string{fmt.Sprintf("%q is not a mapping", name)}}', "\t\treturn &resolved{keys: map[string]any{}}")],
  '^%s$/^job_scalar$' % T_BROKEN),
 ("unresolvable: a key set both at the top level and in `default:` takes the top-level value",
  [(E, "\t\tif _, both := defaults[k]; both {", "\t\tif _, both := defaults[k]; both && false {")],
  '^%s$/^default_twice$' % T_BROKEN),
 ("unresolvable: a diagnosed job with no definition is ignored",
  [(D, '\t\t\tunresolvable(SideBase, name, "", fmt.Sprintf("the diagnosed job %q has no definition in the base document", name))\n', "")],
  '^TestADiagnosedJobWithNoDefinitionIsUnresolvable$'),
 ("verdict order: unresolvable outranks a violation",
  [(D, "\tcase len(d.Violations) > 0:\n\t\td.Kind = KindViolation\n\tcase len(d.Unresolvable) > 0:\n\t\td.Kind = KindUnresolvable\n",
       "\tcase len(d.Unresolvable) > 0:\n\t\td.Kind = KindUnresolvable\n\tcase len(d.Violations) > 0:\n\t\td.Kind = KindViolation\n")],
  '^TestAViolationOutranksAnUnresolvable$'),
 # ---- resolution ----
 ("resolve: `extends` is not applied",
  [(E, "\treturn &resolved{keys: deepMerge(merged, own), depth: depth}", "\treturn &resolved{keys: own, depth: depth}")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: a template's keys beat the job's own",
  [(E, "\treturn &resolved{keys: deepMerge(merged, own), depth: depth}", "\treturn &resolved{keys: deepMerge(own, merged), depth: depth}")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: with a list of templates the EARLIER one wins",
  [(E, "\t\tmerged = deepMerge(merged, parent.keys)\n", "\t\tmerged = deepMerge(parent.keys, merged)\n")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: `extends` replaces nested hashes instead of merging them",
  [(V, "\t\tif uok && ook {\n", "\t\tif uok && ook && false {\n")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: `extends` stays a key of the effective definition",
  [(E, "\t\t\tif k != keyExtends {\n\t\t\t\tkeys[k] = v\n\t\t\t}", "\t\t\tkeys[k] = v")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: a document without `stages` has none",
  [(E, "\tdef.Stages = append([]string{}, DefaultStages...)\n", "\tdef.Stages = []string{}\n")],
  '^TestEffectiveResolvesExtends$'),
 ("resolve: `default:` is not applied",
  [(E, "\t\t\tif _, own := keys[k]; !own && inheritDefault(k) {", "\t\t\tif _, own := keys[k]; false && !own && inheritDefault(k) {")],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: `default:` beats the job's own key",
  [(E, "\t\t\tif _, own := keys[k]; !own && inheritDefault(k) {", "\t\t\tif _, own := keys[k]; (true || !own) && inheritDefault(k) {")],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: `inherit.default` is ignored",
  [(E, "\t\t\tif _, own := keys[k]; !own && inheritDefault(k) {", "\t\t\tif _, own := keys[k]; !own && (true || inheritDefault(k)) {")],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: `inherit.variables` is ignored",
  [(E, "\tfor name, v := range global {\n\t\tif inherit(name) {", "\tfor name, v := range global {\n\t\tif true || inherit(name) {")],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: a global variable beats the job's own of the same name",
  [(E, "\t\tfor k, v := range own {\n\t\t\tout[k] = v\n\t\t}\n\t\tkeys[\"variables\"] = out",
       "\t\tfor k, v := range own {\n\t\t\tif _, has := out[k]; !has {\n\t\t\t\tout[k] = v\n\t\t\t}\n\t\t}\n\t\tkeys[\"variables\"] = out")],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: the deprecated top-level `before_script` is not a default",
  [(E, 'var legacyDefaultKeys = []string{"image", "services", "cache", "before_script", "after_script"}',
       'var legacyDefaultKeys = []string{"image", "services", "cache", "after_script"}')],
  '^TestEffectiveAppliesDefaultAndInherit$'),
 ("resolve: a merged key (<<) beats an explicit one",
  [(V, "\t\t\t\tif _, has := out[k]; !has {\n\t\t\t\t\tout[k] = v\n\t\t\t\t}", "\t\t\t\tout[k] = v")],
  '^TestEffectiveResolvesAnchorsAndMergeKeys$'),
 ("resolve: merge keys are not applied",
  [(V, "\tfor _, m := range merges {\n", "\tfor _, m := range merges[:0] {\n")],
  '^TestEffectiveResolvesAnchorsAndMergeKeys$'),
 ("resolve: an alias is not followed",
  [(V, "\t\tc.active[target] = true\n\t\tv := c.convert(target)\n", "\t\tc.active[target] = true\n\t\tv := any(n.Value)\n")],
  '^TestEffectiveResolvesAnchorsAndMergeKeys$'),
 ("resolve: the resolved forms need not equal their hand expansion (anchors dropped on one side only)",
  [(V, "\tfor _, m := range merges {\n", "\tfor _, m := range merges[:0] {\n")],
  '^TestResolvedFormsEqualTheirHandExpansion$'),
 ("resolve: a change inside an anchored block does not reach the jobs using it",
  [(V, "\t\tc.active[target] = true\n\t\tv := c.convert(target)\n", "\t\tc.active[target] = true\n\t\tv := any(n.Value)\n")],
  '^TestAChangeInsideAnAnchorReachesEveryJobThatUsesIt$'),
 # ---- names ----
 ("names: a parallel suffix is stripped from a job that has no `parallel`",
  [(E, '\t\t\tif _, parallel := j.Keys["parallel"]; parallel {', '\t\t\tif _, parallel := j.Keys["parallel"]; parallel || true {')],
  '^TestDefinitionName$'),
 ("names: a suffix beats an exact match (parallel names are never mapped)",
  [(E, "\tfor _, re := range []*regexp.Regexp{parallelIndex, parallelMatrix} {", "\tfor _, re := range []*regexp.Regexp{} {")],
  '^TestDefinitionName$'),
 ("an edit with no effect is reported as a change (templates are compared)",
  [(E, '\t\tif reservedTopLevel[name] || strings.HasPrefix(name, ".") {', '\t\tif reservedTopLevel[name] {')],
  '^TestAnEditWithNoEffectIsNone$'),
 ("order: jobs are walked in map order (the delta is not deterministic)",
  [(D, "\tbaseNames := sortedKeys(base.Jobs)\n",
       "\tbaseNames := make([]string, 0, len(base.Jobs))\n\tfor n := range base.Jobs {\n\t\tbaseNames = append(baseNames, n)\n\t}\n")],
  '^TestCompareIsDeterministic$'),
]

if '--list' in sys.argv[1:]:
    for i, (name, _, run) in enumerate(muts):
        print(f"[{i}] {name}    -run {run}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']
res = []
for i, (name, edits, run) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, _, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if old not in cur[f]:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous or cur == originals:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        r = subprocess.run([GO, "test", "./pkg/cidef/", "-run", run, "-count=1"],
                           capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails))
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
    if show:
        print(out, flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
