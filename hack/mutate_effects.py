#!/usr/bin/env python3
"""mutate_effects.py -- "seen red" evidence for gonk-2usw part EFFECTS.

The pipeline-fix vocabulary (`edit`, `fix_meta`, `scope_request`, verdicts
`fix` / `needs-scope`), the apply-edits function, DiffFacts, and the pre-apply
predicates by rule code (SC-PROT, SC-CLASS, SC-CITED, SC-BEAD, SC-SIZE,
SC-SECRET, ED-*, SC-FORM, FX-SHAPE) live in pkg/effects (fix.go, edit.go,
fixgate.go). This script disables or breaks ONE of them at a time, runs the
tests that guard it, and requires:

  - a test to FAIL (else the mutation SURVIVED), and
  - the NAMED test to be among the failures (else it is MISSED: something went
    red, but not the fixture written for that predicate).

Every file is restored byte for byte afterwards (try/finally). A mutation
whose text is not found is VACUOUS and one that does not compile NOCOMPILE --
each is a failure of this script.

The first four mutations are the breaks the plan names for this part (section
11, row EFFECTS): `old` matching twice accepted; an edit to an uncited file
accepted; the reflection test not failing when the path field is unlisted (two
ways); then each cap.

Usage: hack/mutate_effects.py            (all mutations)
       hack/mutate_effects.py 0 1 2      (only these indexes)
       hack/mutate_effects.py -v ...     (also print each failing test's message)
Exit:  0 iff every selected mutation applied and was killed by its named test.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate;
its output is pasted into the plan's evidence log.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
# The whole of pkg/effects, and the three broker tests that observe the same
# refusals at the apply chains (the rest of cmd/gonk-gate does not bear on this).
RUNS = [
    [GO, 'test', './pkg/effects/', '-count=1'],
    [GO, 'test', './cmd/gonk-gate/', '-count=1', '-run', 'CarryingTheFixVocabulary'],
]

FX = 'pkg/effects/fix.go'
ED = 'pkg/effects/edit.go'
FG = 'pkg/effects/fixgate.go'
PR = 'pkg/effects/protected.go'
SH = 'pkg/effects/shape.go'
EF = 'pkg/effects/effects.go'

EACH = 'TestFixGateEachPredicateAlone'
APPLY = 'TestApplyEdits'
RULES = 'TestValidateFixEachRule'
CROSS = 'TestVerdictKindCrossRule'
AGENTS = 'TestExistingAgentShapesRefuseTheFixVocabulary'
REFLECT = 'TestEveryPathFieldIsProtected'


def off(marker, kw='if'):
    """Disable the one `if <cond> { // <marker>` (or `case <cond>: // <marker>`) line."""
    return ('MARK', marker, kw)


# (name, expected failing test, [(file, old, new) | (file, ('MARK', marker, kw))...])
muts = [
 # ---- the breaks the plan names (section 11, row EFFECTS) ----
 ("PLAN: `old` matching twice is accepted (the first occurrence is replaced)", APPLY,
  [(ED, off('apply:ambiguous', 'case'))]),
 ("PLAN: an edit to an uncited file is accepted", EACH,
  [(FG, off('cited:bound'))]),
 ("PLAN: the path field is unlisted -- effectPaths reads only a file effect's path (edit.path unchecked)", REFLECT,
  [(PR, 'if e.Path == "" { // paths:any-kind', 'if e.Path == "" || e.Kind != KindFile { // paths:any-kind')]),
 ("PLAN: the path field is unlisted -- scope_request.paths is in neither effectPaths nor the citation list", REFLECT,
  [(PR, '\t"Paths": "a scope_request', '\t"PathsX": "a scope_request')]),
 ("PLAN cap: files changed (max_files) not enforced", EACH, [(FG, off('size:max_files'))]),
 ("PLAN cap: added + removed lines (max_lines) not enforced", EACH, [(FG, off('size:max_lines'))]),
 ("PLAN cap: bytes of `new` across the batch (max_patch_bytes) not enforced", EACH, [(FG, off('size:max_patch_bytes'))]),
 ("PLAN cap: size of a file that may be edited (max_file_bytes) not enforced", EACH, [(FG, off('size:max_file_bytes'))]),
 ("PLAN cap: max_file_bytes checks only the result, not the base", 'TestFixCapsAreInclusive',
  [(FG, 'int64(f.BaseBytes) > caps.MaxFileBytes || int64(f.ResultBytes) > caps.MaxFileBytes', 'int64(f.ResultBytes) > caps.MaxFileBytes')]),
 ("PLAN cap: max_file_bytes checks only the base, not the result", 'TestFixCapsAreInclusive',
  [(FG, 'int64(f.BaseBytes) > caps.MaxFileBytes || int64(f.ResultBytes) > caps.MaxFileBytes', 'int64(f.BaseBytes) > caps.MaxFileBytes')]),
 ("PLAN cap: edit effects (10) not enforced", EACH, [(FX, off('shape:edit-count'))]),
 ("PLAN cap: secrets in added lines (0) not enforced", EACH, [(FG, off('secret:added-lines'))]),
 ("PLAN cap: max_lines counts only added lines", EACH,
  [(ED, 'func (d DiffFacts) Lines() int { return d.LinesAdded + d.LinesRemoved }', 'func (d DiffFacts) Lines() int { return d.LinesAdded }')]),
 ("PLAN cap: a cap is exclusive of its own value (>= instead of >) for files", 'TestFixCapsAreInclusive',
  [(FG, 'if int64(d.FilesChanged) > caps.MaxFiles {', 'if int64(d.FilesChanged) >= caps.MaxFiles {')]),
 ("PLAN cap: a cap is exclusive of its own value for lines", 'TestFixCapsAreInclusive',
  [(FG, 'if int64(d.Lines()) > caps.MaxLines {', 'if int64(d.Lines()) >= caps.MaxLines {')]),
 ("PLAN cap: a cap is exclusive of its own value for new bytes", 'TestFixCapsAreInclusive',
  [(FG, 'if int64(d.NewBytes) > caps.MaxPatchBytes {', 'if int64(d.NewBytes) >= caps.MaxPatchBytes {')]),

 # ---- SC-SECRET ----
 ("SC-SECRET: the exact secrets the controller holds are not looked for", EACH, [(FG, off('secret:exact'))]),
 ("SC-SECRET: no key pattern is looked for", EACH, [(FG, off('secret:pattern'))]),
 ("SC-SECRET: the PEM private key header pattern is dropped", EACH,
  [(FG, '\t{"private_key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},\n', '')]),
 ("SC-SECRET: the GitLab token pattern is dropped", EACH,
  [(FG, '\t{"gitlab_token", regexp.MustCompile(', '\t{"gitlab_token", regexp.MustCompile(`\\bNEVER-MATCHES-ANYTHING-[0-9]{99}`)},\n\t{"unused_token", regexp.MustCompile(`\\bNEVER-MATCHES-EITHER-[0-9]{99}|`+')]),
 ("SC-SECRET: the AWS access key pattern is dropped", EACH,
  [(FG, '\t{"aws_access_key", regexp.MustCompile(`\\b(?:AKIA|ASIA)[0-9A-Z]{16}\\b`)},\n', '')]),
 ("SC-SECRET: an empty secret matches every line", 'TestSecretPatternsDoNotOverReach',
  [(FG, 'if s.value != "" && strings.Contains(text, s.value) {', 'if strings.Contains(text, s.value) {')]),
 ("SC-SECRET: a Secret prints its value", 'TestSecretsDoNotLeak',
  [(FG, 'func (s Secret) String() string               { return "secret(" + s.name + ")" }', 'func (s Secret) String() string               { return "secret(" + s.value + ")" }')]),
 ("SC-SECRET: a Secret marshals its value", 'TestSecretsDoNotLeak',
  [(FG, 'func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + "secret" + `"`), nil }', 'func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.value + `"`), nil }')]),

 # ---- SC-CLASS, SC-PROT, SC-BEAD, SC-CITED ----
 ("SC-CLASS: the envelope's class set is not enforced", EACH, [(FG, off('class:envelope', 'case'))]),
 ("SC-CLASS: a file the base blob shows to be generated is not reclassified", EACH, [(FG, off('class:generated'))]),
 ("SC-CLASS: DiffFacts does not record that the base is generated", 'TestDiffFactsMarksGeneratedFiles',
  [(ED, 'Generated: scope.IsGenerated(pf.Base)', 'Generated: false')]),
 ("SC-PROT: the lift-aware denylist is not consulted for an edit", 'TestEditPathGoesThroughTheProtectedPathGate',
  [(FG, off('prot:denylist', 'case'))]),
 ("SC-PROT: a protected CLASS that is on no denylist is not refused as protected", EACH, [(FG, off('prot:class', 'case'))]),
 ("SC-PROT: neither the denylist nor the protected classes are consulted", EACH,
  [(FG, off('prot:denylist', 'case')), (FG, off('prot:class', 'case'))]),
 ("SC-PROT: the gate drops the dispatch's lift (a configuration fix can edit nothing)", 'TestFixGateWithTheCILift',
  [(FG, 'switch p, what := protectedPath(e, g.env.Lift); {', 'switch p, what := protectedPath(e, scope.Lift{}); {')]),
 ("SC-PROT: a lift is honoured although this scope could not have granted it", EACH,
  [(FG, 'case class.Sensitivity() == scope.Protected && !g.env.Scope.Allows(class, g.env.Lift): // prot:class',
        'case class.Sensitivity() == scope.Protected && g.env.Lift.Empty(): // prot:class'),
   (FG, 'case !g.env.Scope.Allows(class, g.env.Lift): // class:envelope',
        'case class.Sensitivity() != scope.Protected && !g.env.Scope.Allows(class, g.env.Lift): // class:envelope')]),
 ("SC-PROT: a protected path on a non-edit effect passes the path gate", 'TestFixPhaseFunctions', [(FG, off('prot:any-kind'))]),
 ("SC-BEAD: a declared path scope is not enforced", EACH, [(FG, off('bead:bound'))]),
 ("SC-CITED: citing a directory cites every file under it", EACH,
  [(FG, 'if !g.cited[e.Path] { // cited:bound',
        'if !g.cited[e.Path] && !g.cited[e.Path[:max(strings.LastIndex(e.Path, "/"), 0)]] { // cited:bound')]),
 ("SC-CITED: a citation is matched as a pattern", EACH,
  [(FG, 'if !g.cited[e.Path] { // cited:bound', 'if !g.cited[e.Path] && !g.citedByPattern(e.Path) { // cited:bound'),
   (FG, '// classify returns p\'s class under the envelope and the base facts the',
        'func (g *fixGate) citedByPattern(p string) bool {\n\tfor c := range g.cited {\n\t\tif scope.Match(c, p) {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\n// classify returns p\'s class under the envelope and the base facts the')]),
 ("SC-CITED: the cited files are read from a diagnosis in the batch", 'TestFixEnvelopeIsNotInTheBatch',
  [(FG, '\tif done(fixShapeViolations(b)) { // gate:shape',
        '\tfor _, e := range b.Effects {\n\t\tfor _, f := range e.Files {\n\t\t\tg.cited[f] = true\n\t\t}\n\t}\n\tif done(fixShapeViolations(b)) { // gate:shape')]),

 # ---- the apply ----
 ("ED-NOMATCH: an edit whose `old` occurs nowhere is accepted", APPLY, [(ED, off('apply:nomatch', 'case'))]),
 ("ED-MISSING: an edit to a file that does not exist is not refused as such (creation)", APPLY, [(ED, off('apply:missing'))]),
 ("ED-ENCODING: a binary / non-UTF-8 base blob is edited", APPLY, [(ED, off('apply:encoding'))]),
 ("ED-EMPTIES: an edit may empty a file", APPLY, [(ED, off('apply:empties'))]),
 ("ED-NOOP: edits that cancel out are committed", APPLY, [(ED, off('apply:noop'))]),
 ("ED-AMBIGUOUS: overlapping occurrences are counted once", APPLY,
  [(ED, 'from += i + 1 // count:overlap', 'from += i + len(sub) // count:overlap')]),
 ("apply: `old` is matched against the BASE, not the file after earlier edits", APPLY,
  [(ED, 'switch n := occurrences(pf.Content, []byte(e.Old)); {', 'switch n := occurrences(pf.Base, []byte(e.Old)); {')]),
 ("apply: a non-edit effect carrying old/new is applied as an edit", 'TestOnlyEditEffectsBecomePatchedFiles',
  [(ED, 'if e.Kind != KindEdit { // apply:edit-only', 'if e.Kind != KindEdit && e.Old == "" { // apply:edit-only')]),
 ("apply: the base blobs are modified in place", APPLY,
  [(ED, 'pf.Content = bytes.Replace(pf.Content, []byte(e.Old), []byte(e.New), 1)',
        'pf.Content = bytes.Replace(pf.Content, []byte(e.Old), []byte(e.New), 1)\n\t\tif len(e.Old) == len(e.New) {\n\t\t\tcopy(base[e.Path], pf.Content)\n\t\t}')]),

 # ---- DiffFacts ----
 ("diff: lines lose their terminator (a changed final newline is invisible)", 'TestDiffFacts',
  [(ED, 'lines := strings.SplitAfter(string(content), "\\n")', 'lines := strings.Split(string(content), "\\n")')]),
 ("diff: NewBytes counts only a file's last edit", 'TestDiffFacts',
  [(ED, 'pf.NewBytes += len(e.New)', 'pf.NewBytes = len(e.New)')]),
 ("diff: the common prefix is not trimmed AND the search gives up immediately (whole file counted)", 'TestDiffFacts',
  [(ED, '\tlimit := n + m\n\tif limit > maxExactDiffLines {\n\t\tlimit = maxExactDiffLines\n\t}', '\tlimit := 0')]),
 ("diff: the upper-bound fallback claims to be exact", 'TestLineDiffFallsBackToAnUpperBound',
  [(ED, '\treturn a, b, false\n}', '\treturn a, b, true\n}')]),
 ("diff: the backtrack swaps added and removed", 'TestLineDiffIsMinimal',
  [(ED, '\t\tif pk == k+1 {\n\t\t\tadded = append(added, b[py])', '\t\tif pk != k+1 {\n\t\t\tadded = append(added, b[min(py, len(b)-1)])'),
   (ED, '\t\t\tremoved = append(removed, a[px])', '\t\t\tremoved = append(removed, a[min(px, len(a)-1)])')]),

 # ---- SC-FORM ----
 ("SC-FORM: an unclean edit path is accepted", RULES, [(FX, off('form:path'))]),
 ("SC-FORM: an empty `old` is accepted", RULES, [(FX, off('form:old-empty'))]),
 ("SC-FORM: `old` == `new` is accepted", RULES, [(FX, off('form:identical'))]),
 ("SC-FORM: a NUL in old/new is accepted", RULES, [(FX, off('form:nul'))]),
 ("SC-FORM: invalid UTF-8 in old/new is accepted", RULES, [(FX, off('form:utf8'))]),
 ("SC-FORM: an unclean path is classified and cited instead of being set aside", EACH,
  [(FG, 'if r := editForm(i, e); r != nil && r.Sub == "path" { // paths:unclean', 'if r := editForm(i, e); r != nil && false { // paths:unclean')]),
 ("SC-FORM: ValidateEditPaths alone passes an unclean path", 'TestFixPhaseFunctions',
  [(FG, '\t\t\tif reportForm {', '\t\t\tif false {')]),

 # ---- FX-SHAPE: ValidateFix ----
 ("FX-SHAPE: any verdict is accepted for a fix batch", RULES, [(FX, off('shape:verdict'))]),
 ("FX-SHAPE: other kinds may ride in a fix batch", RULES,
  [(FX, 'if fixKindFields[e.Kind] == nil { // shape:kind\n\t\treturn refuse(', 'if fixKindFields[e.Kind] == nil { // shape:kind\n\t\treturn nil\n\t}\n\tif false {\n\t\treturn refuse(')]),
 ("FX-SHAPE: a fix effect may carry another kind's fields", RULES, [(FX, off('shape:foreign-field'))]),
 ("FX-SHAPE: a fix_meta may carry a scope_request's paths (gonk-58bp: reason is now shared)", 'TestFixMetaReasonKeyForeignFieldRules',
  [(FX, 'KindFixMeta:      {"Reason": true},', 'KindFixMeta:      {"Reason": true, "Paths": true},')]),
 ("FX-SHAPE: an edit may carry a reason (gonk-58bp: reason is now shared)", 'TestFixMetaReasonKeyForeignFieldRules',
  [(FX, 'KindEdit:         {"Path": true, "Old": true, "New": true},', 'KindEdit:         {"Path": true, "Old": true, "New": true, "Reason": true},')]),
 ("FX-SHAPE: the foreign-field rule forgets a field (content rides on an edit)", 'TestFixKindsRefuseEveryForeignField',
  [(FX, 'if name == "Kind" || own[name] || targetFields[name] {', 'if name == "Kind" || own[name] || targetFields[name] || name == "Content" {')]),
 ("FX-SHAPE: a blank fix_meta reason is accepted", RULES, [(FX, off('shape:meta-reason-empty'))]),
 ("FX-SHAPE: the fix_meta reason cap is not enforced", RULES, [(FX, off('shape:meta-reason-cap'))]),
 ("FX-SHAPE: the scope_request path count is not enforced", RULES, [(FX, off('shape:paths-count'))]),
 ("FX-SHAPE: a scope_request path need not be clean", RULES, [(FX, off('shape:paths-form'))]),
 ("FX-SHAPE: a blank scope_request reason is accepted", RULES, [(FX, off('shape:reason-empty'))]),
 ("FX-SHAPE: the scope_request reason cap is not enforced", RULES, [(FX, off('shape:reason-cap'))]),

 # ---- the verdict/kind rule, each arm ----
 ("cross: verdict fix may carry a scope_request", CROSS, [(FX, off('cross:fix-with-request', 'case'))]),
 ("cross: verdict fix needs no edit", CROSS, [(FX, off('cross:fix-needs-edit', 'case'))]),
 ("cross: verdict fix needs no fix_meta", CROSS, [(FX, off('cross:fix-needs-meta', 'case'))]),
 ("cross: verdict needs-scope may carry edits (a refused edit silently converted)", CROSS, [(FX, off('cross:needs-scope-with-edit', 'case'))]),
 ("cross: verdict needs-scope may carry a fix_meta", CROSS, [(FX, off('cross:needs-scope-with-meta', 'case'))]),
 ("cross: verdict needs-scope needs no scope_request", CROSS, [(FX, off('cross:needs-scope-needs-request', 'case'))]),
 ("cross: an edit is accepted under another verdict", CROSS, [(FX, off('cross:edit-needs-fix', 'case'))]),
 ("cross: a fix_meta is accepted under another verdict", CROSS, [(FX, off('cross:meta-needs-fix', 'case'))]),
 ("cross: a scope_request is accepted under another verdict", CROSS, [(FX, off('cross:request-needs-needs-scope', 'case'))]),

 # ---- no existing agent gains the vocabulary ----
 ("agents: Validate does not apply the verdict/kind rule (verdict fix reaches a triage broker)", AGENTS,
  [(SH, 'if r := verdictKindRule(b); r != nil { // gate:verdict-kind', 'if r := verdictKindRule(b); r != nil && false { // gate:verdict-kind')]),
 ("agents: the same break, observed at the triage apply chain", 'TestTriageBatchCarryingTheFixVocabularyIsRefused',
  [(SH, 'if r := verdictKindRule(b); r != nil { // gate:verdict-kind', 'if r := verdictKindRule(b); r != nil && false { // gate:verdict-kind')]),
 ("agents: the same break, observed at the scaffold apply chain", 'TestScaffoldBatchCarryingTheFixVocabularyIsRefused',
  [(SH, 'if r := verdictKindRule(b); r != nil { // gate:verdict-kind', 'if r := verdictKindRule(b); r != nil && false { // gate:verdict-kind')]),
 ("agents: the same break, observed at the pipeline apply chain", 'TestPipelineBatchCarryingTheFixVocabularyIsRefused',
  [(SH, 'if r := verdictKindRule(b); r != nil { // gate:verdict-kind', 'if r := verdictKindRule(b); r != nil && false { // gate:verdict-kind')]),
 ("agents: triage's shape gains [edit], [fix_meta] and [scope_request]", AGENTS,
  [('pack/agents/triage/effect-shape.toml', '[comment]\n', '[edit]\nmin = 0\nmax = 10\n\n[fix_meta]\nmin = 0\nmax = 1\n\n[scope_request]\nmin = 0\nmax = 1\n\n[comment]\n')]),
 ("agents: the same, observed at the triage apply chain", 'TestTriageBatchCarryingTheFixVocabularyIsRefused',
  [('pack/agents/triage/effect-shape.toml', '[comment]\n', '[edit]\nmin = 0\nmax = 10\n\n[fix_meta]\nmin = 0\nmax = 1\n\n[scope_request]\nmin = 0\nmax = 1\n\n[comment]\n')]),
 ("agents: scaffold's shape gains [edit] and [fix_meta]", 'TestScaffoldBatchCarryingTheFixVocabularyIsRefused',
  [('pack/agents/scaffold/effect-shape.toml', '[file]\n', '[edit]\nmin = 0\nmax = 10\n\n[fix_meta]\nmin = 0\nmax = 1\n\n[file]\n')]),
 ("agents: pipeline's shape gains [scope_request]", 'TestPipelineBatchCarryingTheFixVocabularyIsRefused',
  [('pack/agents/pipeline/effect-shape.toml', '[diagnosis]\n', '[scope_request]\nmin = 0\nmax = 1\n\n[diagnosis]\n')]),
 ("agents: pipeline's shape gains [edit] and [fix_meta]", AGENTS,
  [('pack/agents/pipeline/effect-shape.toml', '[diagnosis]\n', '[edit]\nmin = 0\nmax = 10\n\n[fix_meta]\nmin = 0\nmax = 1\n\n[diagnosis]\n')]),

 # ---- the closed sets ----
 ("parse: verdict needs-scope is not in the closed set", 'TestFixVerdictsAreInTheClosedSet',
  [(EF, '\tVerdictFix: true, VerdictNeedsScope: true,\n', '\tVerdictFix: true,\n')]),
 ("parse: an unknown verdict is accepted", 'TestFixVerdictsAreInTheClosedSet',
  [(EF, 'if b.Verdict != "" && !knownVerdicts[b.Verdict] {', 'if false {')]),
 ("parse: an unknown kind is accepted (a `patch` or `commit` kind)", 'TestFixVerdictsAreInTheClosedSet',
  [(EF, '\t\tif !knownKinds[e.Kind] {', '\t\tif false {')]),

 # ---- PlanFix runs every gate ----
 ("PlanFix: the shape gate is skipped", EACH,
  [(FG, 'if done(fixShapeViolations(b)) { // gate:shape', 'if done(nil) { // gate:shape')]),
 ("PlanFix: the path gate is skipped", EACH,
  [(FG, 'if done(g.pathViolations(b, false)) { // gate:paths', 'if done(nil) { // gate:paths')]),
 ("PlanFix: the diff gate is skipped", EACH,
  [(FG, 'done(g.diffViolations(&g.facts, secrets)) // gate:diff', 'done(nil) // gate:diff')]),
 ("PlanFix: a refused batch still returns its files", EACH,
  [(FG, '\t\treturn FixPlan{}, v[0]\n', '\t\treturn FixPlan{Verdict: b.Verdict, Files: g.files, Facts: g.facts}, v[0]\n')]),
 ("PlanFix: the first refusal is not the one returned (PlanFix collects instead of stopping)", 'TestScopeRequestMayCiteProtectedPaths|TestFixGateEachPredicateAlone',
  [(FG, '\tif v := g.violations(b, base, secrets, true); len(v) > 0 {\n\t\treturn FixPlan{}, v[0]', '\tif v := g.violations(b, base, secrets, true); len(v) > 1 {\n\t\treturn FixPlan{}, v[0]')]),
 ("refusal: the rule-code list changes without the contract test noticing", 'TestRefusalIsTyped',
  [(FX, '\tRuleCited Rule = "SC-CITED"', '\tRuleCited Rule = "SC-CITE"')]),
]


def resolve(f, text, edit):
    """Turn an edit spec into (old, new) against the file's current text."""
    if len(edit) == 2:
        _, marker, kw = edit[1]
        tag = ' // ' + marker
        hits = [l for l in text.split('\n') if l.endswith(tag)]
        if len(hits) != 1:
            return None
        line = hits[0]
        indent = line[:len(line) - len(line.lstrip('\t'))]
        body = line.lstrip('\t')
        # The condition is kept and AND-ed with false, so every name it uses
        # (and any init statement before a `;`) still compiles.
        if kw == 'if':
            if not body.startswith('if ') or not line.endswith(' {' + tag):
                return None
            stmt = body[len('if '):-len(' {' + tag)]
            init, sep, cond = stmt.rpartition('; ')
            return line, indent + 'if ' + init + sep + '(' + cond + ') && false {' + tag
        if not body.startswith('case ') or not line.endswith(':' + tag):
            return None
        cond = body[len('case '):-len(':' + tag)]
        return line, indent + 'case (' + cond + ') && false:' + tag
    return edit[1], edit[2]


args = sys.argv[1:]
verbose = '-v' in args
only = [a for a in args if a != '-v']
res = []
for i, (name, expect, edits) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    for e in edits:
        if e[0] not in originals:
            originals[e[0]] = open(e[0]).read()
    cur = dict(originals)
    vacuous = False
    for e in edits:
        pair = resolve(e[0], cur[e[0]], e)
        if pair is None or pair[0] not in cur[e[0]] or pair[0] == pair[1]:
            vacuous = True
            break
        cur[e[0]] = cur[e[0]].replace(pair[0], pair[1], 1)
    if vacuous:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        out, code = '', 0
        for run in RUNS:
            r = subprocess.run(run, capture_output=True, text=True)
            out += r.stdout + r.stderr
            code = code or r.returncode
        lines = out.splitlines()
        fails = [l.strip() for l in lines if l.strip().startswith("--- FAIL")]
        wanted = expect.split('|')
        if code == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:1200]}")
        elif not any(f.startswith(f"--- FAIL: {w} ") or f.startswith(f"--- FAIL: {w}/") for f in fails for w in wanted):
            res.append(f"MISSED   [{i}] {name}\n           wanted {expect}, got:\n           " + "\n           ".join(fails))
        else:
            msg = f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails)
            if verbose:
                # The first message lines under the expected test: what red looked like.
                first = next(n for n, l in enumerate(lines)
                             if any(l.strip().startswith(f"--- FAIL: {w} ") for w in wanted))
                detail = [l.strip() for l in lines[first + 1:first + 14]
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
