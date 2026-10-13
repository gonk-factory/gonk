#!/usr/bin/env python3
"""mutate_scope.py -- "seen red" evidence for gonk-066 S1/S3 and gonk-2usw part SCOPE.

Applies one named break at a time to the workflow-scope code (pkg/scope, the
lift in pkg/effects/protected.go, the two config loaders), runs the packages
that guard it, and requires a test to FAIL; then restores every file byte for
byte (try/finally). A mutation whose text is not found is VACUOUS, one the
tests do not notice SURVIVED, and one that does not compile NOCOMPILE -- each
is a failure of this script. A mutation may edit several places (a list of
edits, all applied); an edit whose `old` is None CREATES the file and removes
it afterwards.

The first three mutations are the breaks the gonk-2usw plan names for part
SCOPE (section 11); the rest cover 066's "tests that must be seen red" for S1
and S3, the property test and the contract tests. The last block is gonk-wj14
(ecosystem profiles, 066 section 11): one or more mutants per rule.

Usage: hack/mutate_scope.py            (all mutations)
       hack/mutate_scope.py 0 1 2      (only these indexes)
       hack/mutate_scope.py -v ...     (also print each failing test's message)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate;
its output is pasted into the plan's evidence log.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
PKGS = ['./pkg/scope/', './pkg/effects/', './pkg/gonkcfg/', './pkg/opercfg/', './internal/meter/service/', './pkg/intake/']

CL = 'pkg/scope/class.go'
FO = 'pkg/scope/fold.go'
PO = 'pkg/scope/policy.go'
CY = 'pkg/scope/classify.go'
CI_ = 'pkg/scope/ciincludes.go'
PK = 'pkg/scope/pack.go'
PR = 'pkg/effects/protected.go'
SL = 'pkg/effects/shape_load.go'
EF = 'pkg/effects/effects.go'
GC = 'pkg/gonkcfg/config.go'
OC = 'pkg/opercfg/opercfg.go'
PF = 'pkg/scope/profile.go'
SV = 'internal/meter/service/service.go'
IO = 'pkg/intake/onboard.go'
IR = 'pkg/intake/render.go'

# The class order (class.go). Since gonk-wj14 the built-in table is assembled
# in THIS order from the core rows and the profile rows, so a mutant that
# reorders classes edits this, not the order of a table literal.
ORDER = "\tClassControl, ClassSecret, ClassCI, ClassDeploy, ClassTestConfig, ClassDeps,\n\tClassAgentContext, ClassGenerated, ClassTest, ClassDocs, ClassSource, ClassOther,\n"

# The lift condition lives in protectedPath (protected.go) since gonk-2usw part
# EFFECTS, which shares it with the fix gate's SC-PROT.
LIFT_IF = "\t\tif class == scope.ClassCI && lift.Has(scope.ClassCI) && scope.Builtin(p) == scope.ClassCI {\n"

# (name, [(file, old, new), ...])
muts = [
 # ---- the three breaks the gonk-2usw plan names (section 11, row SCOPE) ----
 ("PLAN: a ci path passes without a lift",
  [(PR, LIFT_IF, "\t\tif class == scope.ClassCI && scope.Builtin(p) == scope.ClassCI {\n")]),
 ("PLAN: .gonk.yml passes WITH a lift",
  [(PR, LIFT_IF, "\t\tif lift.Has(scope.ClassCI) {\n")]),
 ("PLAN: a project config widens past the pack row (classes are unioned)",
  [(FO, "\t\tif l.p.Classes != nil {\n\t\t\tallowed = intersectSet(allowed, l.p.Classes)\n\t\t}",
        "\t\tfor _, c := range l.p.Classes {\n\t\t\tallowed[c] = true\n\t\t}")]),
 ("PLAN: a project config widens past the pack row (caps take the larger value)",
  [(FO, "\t\t\tif c.v != nil && *c.v < *c.dst {", "\t\t\tif c.v != nil && *c.v > *c.dst {")]),
 ("PLAN: a project config widens past the pack row (project lift opt-in alone grants a lift)",
  [(FO, "\tfor _, c := range in.Row.Liftable {\n\t\teff.liftable[c] = optIn[c]\n\t}",
        "\tfor _, c := range in.LiftOptIn {\n\t\teff.liftable[c] = true\n\t}")]),

 # ---- the lift ----
 ("lift: a secret-class file under .gitlab/ passes with the ci lift (scope classifier not consulted)",
  [(PR, LIFT_IF, "\t\tif class == scope.ClassCI && lift.Has(scope.ClassCI) {\n")]),
 ("lift: .git/ is filed under ci, so the lift opens it",
  [(PR, '{".git/", scope.ClassControl},', '{".git/", scope.ClassCI},     ')]),
 ("lift: GrantLift grants any class asked for (the floor is grantable)",
  [(FO, "\t\tcase !Liftable(c):\n\t\t\treturn Lift{}, fmt.Errorf(\"scope: lift: %q cannot be lifted by any dispatch (only ci can; control and secret are the floor)\", c)\n\t\tcase !e.liftable[c]:\n\t\t\treturn Lift{}, fmt.Errorf(\"scope: lift: %q is not liftable for this workflow in this project (needs the pack row, the project's opt-in, and no operator veto)\", c)\n",
        "")]),
 ("lift: GrantLift ignores the project opt-in and the operator veto",
  [(FO, "\t\tcase !e.liftable[c]:\n", "\t\tcase false:\n")]),
 ("lift: a lift is honoured by a fold that could not have granted it",
  [(FO, "\t\treturn Liftable(c) && lift.Has(c) && e.liftable[c]", "\t\treturn Liftable(c) && lift.Has(c)")]),
 ("lift: a disabled workflow still grants a lift",
  [(FO, "\tif !e.Enabled {\n\t\treturn Lift{}, fmt.Errorf(\"scope: the workflow is disabled; nothing can be lifted\")\n\t}\n", "")]),
 ("lift: an unnamed grant is accepted (nothing to log it under)",
  [(FO, "\tif strings.TrimSpace(g.Trigger) == \"\" || strings.TrimSpace(g.Workflow) == \"\" {", "\tif false {")]),
 ("lift: the operator's deny_lift is ignored",
  [(FO, "\t\tfor _, c := range l.p.DenyLift {\n\t\t\teff.liftable[c] = false\n\t\t}\n", "")]),
 ("lift: the Lift type gains an exported, decodable field",
  [(FO, "type Lift struct {\n\tclasses  string", "type Lift struct {\n\tCI       bool `json:\"ci\"`\n\tclasses  string"),
   (FO, "func (l Lift) Empty() bool { return l.classes == \"\" }", "func (l Lift) Empty() bool { return l.classes == \"\" && !l.CI }")]),
 ("lift: the Lift decoders stop refusing (a decode silently yields the zero lift)",
  [(FO, "func (l *Lift) UnmarshalJSON([]byte) error { return errLiftDecode }", "func (l *Lift) UnmarshalJSON([]byte) error { return nil }"),
   (FO, "func (l *Lift) UnmarshalText([]byte) error { return errLiftDecode }", "func (l *Lift) UnmarshalText([]byte) error { return nil }"),
   (FO, "func (l *Lift) UnmarshalYAML(*yaml.Node) error { return errLiftDecode }", "func (l *Lift) UnmarshalYAML(*yaml.Node) error { return nil }")]),
 ("lift: a pack row may list control as liftable",
  [(FO, "\tfor _, c := range r.Liftable {\n\t\tif !Liftable(c) {", "\tfor _, c := range r.Liftable {\n\t\tif false && !Liftable(c) {"),
   (FO, "\tfor _, c := range in.Row.Liftable {\n\t\teff.liftable[c] = optIn[c]\n\t}",
        "\tfor _, c := range in.Row.Liftable {\n\t\teff.liftable[c] = optIn[c] || c == ClassControl\n\t}")]),

 # ---- the fold ----
 ("fold: a disabled workflow still allows its classes",
  [(FO, "\tif !e.Enabled || !c.Valid() {\n\t\treturn false\n\t}", "\tif !c.Valid() {\n\t\treturn false\n\t}")]),
 ("fold: Narrow adds the classes it is given",
  [(FO, "\te.Classes = orderedClasses(intersectSet(have, classes))",
        "\tfor _, c := range classes {\n\t\thave[c] = true\n\t}\n\te.Classes = orderedClasses(have)")]),
 ("fold: a pack row may list a protected class in classes",
  [(FO, "\t\tif c.Sensitivity() == Protected {\n\t\t\treturn fmt.Errorf(\"scope: pack row: classes lists",
        "\t\tif false {\n\t\t\treturn fmt.Errorf(\"scope: pack row: classes lists"),
   (FO, "\tif c.Sensitivity() == Protected {\n\t\treturn Liftable(c) && lift.Has(c) && e.liftable[c]\n\t}\n", "")]),
 ("fold: a branch name is accepted as the base SHA",
  [(FO, "\tif !shaRe.MatchString(in.BaseSHA) {", "\tif in.BaseSHA == \"\" {")]),
 ("fold: the group layer is not validated",
  [(FO, '{"instance", LayerOperator, in.Instance}, {"group", LayerOperator, in.Group}, {"project", LayerProject, in.Project}}\n\tfor _, l := range layers {\n\t\tif err := l.p.Validate(l.layer); err != nil {',
        '{"instance", LayerOperator, in.Instance}, {"group", LayerOperator, in.Group}, {"project", LayerProject, in.Project}}\n\tfor _, l := range layers {\n\t\tif err := l.p.Validate(l.layer); err != nil && l.name != "group" {')]),
 ("fold: a second bead scope replaces the first instead of intersecting",
  [(FO, "\tbead = append(bead, e.bead...)\n", "")]),
 ("fold: protected_paths of the instance layer are dropped",
  [(FO, "\t\tfor _, k := range l.p.patternKeys() {\n\t\t\tfor _, pat := range k.patterns {\n\t\t\t\teff.patterns",
        "\t\tfor _, k := range l.p.patternKeys() {\n\t\t\tif l.name == \"instance\" && k.to == ClassControl {\n\t\t\t\tcontinue\n\t\t\t}\n\t\t\tfor _, pat := range k.patterns {\n\t\t\t\teff.patterns")]),

 # ---- the lattice ----
 ("lattice: nothing is locked (deploy -> docs allowed)",
  [(CL, "\tif !unlocked(from) {\n\t\treturn refuse(fmt.Sprintf(\"%s is locked; a path leaves it only toward control\", from))\n\t}\n", "")]),
 ("lattice: a project may declare source paths",
  [(CL, "\t\tif layer != LayerOperator {\n\t\t\treturn refuse(\"declaring source paths is operator-only\")\n\t\t}\n", ""),
   (PO, "\tif layer == LayerProject && len(p.SourcePaths) > 0 {", "\tif false {")]),
 ("lattice: test and docs may become each other",
  [(CL, "\t\tif from == ClassSource || from == ClassOther {\n\t\t\treturn nil\n\t\t}\n\t\treturn refuse(\"only source and other may become test or docs\")",
        "\t\treturn nil")]),
 ("lattice: not enforced per path (Classify trusts the load-time probe)",
  [(CY, "\t\t\tif err := CheckMove(d.Builtin, lp.to, lp.layer); err != nil {", "\t\t\tif err := error(nil); err != nil {")]),
 ("lattice: not enforced at load (Policy.Validate skips the probe)",
  [(PO, "\t\t\tfor _, probe := range probes(pat) {", "\t\t\tfor _, probe := range probes(pat)[:0] {")]),
 ("lattice: ci_paths may pull a deploy path into the liftable class",
  [(CL, "\tif !unlocked(from) {", "\tif !unlocked(from) && to != ClassCI {")]),

 # ---- the classifier ----
 ("classes: ci is matched before secret (066 table order)",
  [(CL, ORDER, ORDER.replace("ClassSecret, ClassCI,", "ClassCI, ClassSecret,"))]),
 ("classes: .gonk.yml is control only at the root (disagrees with the effects floor)",
  [(CL, '{".git/**", "**/CODEOWNERS", "**/.gonk.yml"}', '{".git/**", "**/CODEOWNERS", ".gonk.yml"}')]),
 ("classes: test-config is matched after test",
  [(CL, ORDER, ORDER.replace("ClassTestConfig, ClassDeps,", "ClassDeps,").replace("ClassTest, ClassDocs,", "ClassTest, ClassTestConfig, ClassDocs,"))]),
 ("classes: an unclean path is classified by its text instead of failing closed",
  [(PF, "\tif !cleanRel(p) {\n\t\treturn ClassControl, \"\"\n\t}\n\tsegs := strings.Split(p, \"/\")", "\tsegs := strings.Split(p, \"/\")"),
   ('pkg/scope/glob.go', "\tif ValidatePattern(pattern) != nil || !cleanRel(p) {", "\tif ValidatePattern(pattern) != nil {")]),
 ("classes: generated files are classified by name",
  [(PF, "\t\tif row.class == ClassGenerated && base.Generated[p] {", "\t\tif false && row.class == ClassGenerated && base.Generated[p] {")]),
 ("classes: the base .gitlab-ci.yml's local includes are not ci",
  [(PF, "\t\tif row.class == ClassCI && included(base.CILocalIncludes, p) {", "\t\tif false {")]),
 ("classes: an include that is not a valid pattern silently stops being ci",
  [(CL, "\t\tif inc == p || Match(inc, p) {", "\t\tif Match(inc, p) {")]),
 # ---- gonk-9q0y: includes of includes ----
 ("gonk-9q0y: an included file's own includes are not followed",
  [(CI_, "\t\tadd(nested)\n", "\t\t_ = nested\n")]),
 ("gonk-9q0y: a wildcard include leaves the set marked complete",
  [(CI_, "\t\t\t\tout.Complete = false\n", "")]),
 ("gonk-9q0y: a wildcard include is read as a file",
  [(CI_, "\t\t\tif CIIncludeIsPattern(e) { // includes:pattern", "\t\t\tif false && CIIncludeIsPattern(e) { // includes:pattern")]),
 ("gonk-9q0y: a file is walked every time it is named (a cycle does not end)",
  [(CI_, "\t\t\tif seen[e] { // includes:once", "\t\t\tif false && seen[e] { // includes:once")]),
 ("gonk-9q0y: the walk has no bound",
  [(CI_, "\t\tif files++; files > MaxCIIncludeFiles { // includes:bound", "\t\tif files++; false && files > MaxCIIncludeFiles { // includes:bound")]),
 ("gonk-9q0y: the bound is one short (a definition AT the limit is refused)",
  [(CI_, "\t\tif files++; files > MaxCIIncludeFiles { // includes:bound", "\t\tif files++; files >= MaxCIIncludeFiles { // includes:bound")]),
 ("gonk-9q0y: a failed read is taken for an absent file",
  [(CI_, "\t\tif rerr != nil {\n\t\t\treturn CIIncludes{}, fmt.Errorf(\"scope: read the included file %q: %w\", p, rerr)\n\t\t}\n", "\t\t_ = rerr\n")]),
 ("gonk-9q0y: an included file that is not YAML is skipped",
  [(CI_, "\t\tif nerr != nil {\n\t\t\treturn CIIncludes{}, fmt.Errorf(\"scope: the included file %q: %w\", p, nerr)\n\t\t}\n", "\t\t_ = nerr\n")]),
 ("gonk-9q0y: a missing included file ends the walk with an error",
  [(CI_, "\t\tif !found { // includes:missing\n\t\t\tcontinue\n", "\t\tif !found { // includes:missing\n\t\t\treturn CIIncludes{}, fmt.Errorf(\"missing\")\n")]),
 ("gonk-9q0y: nested files are walked depth first (the order of Paths changes)",
  [(CI_, "\t\tp := queue[0]\n\t\tqueue = queue[1:]\n", "\t\tp := queue[len(queue)-1]\n\t\tqueue = queue[:len(queue)-1]\n")]),
 ("gonk-9q0y: `?` is not a wildcard (such an entry is read as a path)",
  [(CI_, 'strings.ContainsAny(entry, "*?")', 'strings.ContainsAny(entry, "*")')]),
 # ---- v0.2.1 verification: forms for which Complete was wrongly true ----
 ("gonk-9q0y: only the first YAML document is read (a `spec:` header hides the includes)",
  [(CY, "\t\t\treturn nil, false, fmt.Errorf(\"scope: .gitlab-ci.yml: %w\", derr)\n\t\t}\n\t\tdocs = append(docs, doc)\n",
        "\t\t\treturn nil, false, fmt.Errorf(\"scope: .gitlab-ci.yml: %w\", derr)\n\t\t}\n\t\tdocs = append(docs, doc)\n\t\tbreak\n")]),
 ("gonk-9q0y: a later document that is not YAML ends the read quietly",
  [(CY, "\t\t} else if derr != nil {\n\t\t\treturn nil, false, fmt.Errorf(\"scope: .gitlab-ci.yml: %w\", derr)\n\t\t}\n",
        "\t\t} else if derr != nil {\n\t\t\tbreak\n\t\t}\n")]),
 ("gonk-9q0y: a later document that is not a mapping is skipped",
  [(CY, "\t\tif derr := n.Decode(&doc); derr != nil {\n\t\t\treturn nil, false, fmt.Errorf(\"scope: .gitlab-ci.yml: %w\", derr)\n\t\t}\n",
        "\t\tif derr := n.Decode(&doc); derr != nil {\n\t\t\tcontinue\n\t\t}\n")]),
 ("gonk-9q0y: an entry with a variable is read as a file and the set stays complete",
  [(CI_, "\t\t\tif CIIncludeHasVariable(e) { // includes:variable", "\t\t\tif false && CIIncludeHasVariable(e) { // includes:variable")]),
 ("gonk-9q0y: an entry with a variable is not read, but the set stays complete",
  [(CI_, "\t\t\tif CIIncludeHasVariable(e) { // includes:variable\n\t\t\t\tout.Complete = false\n", "\t\t\tif CIIncludeHasVariable(e) { // includes:variable\n")]),
 ("gonk-9q0y: a `!reference` include is read as a list of paths",
  [(CY, "\tif strings.HasPrefix(n.Tag, \"!\") && !strings.HasPrefix(n.Tag, \"!!\") { // includes:tag", "\tif false { // includes:tag")]),
 ("gonk-9q0y: every node counts as custom-tagged (a plain include is blanked)",
  [(CY, "\tif strings.HasPrefix(n.Tag, \"!\") && !strings.HasPrefix(n.Tag, \"!!\") { // includes:tag", "\tif strings.HasPrefix(n.Tag, \"!\") { // includes:tag")]),
 ("gonk-9q0y: a `!reference` anywhere in the file makes the includes opaque",
  [(CY, "\t\t\t\tif m.Content[i].Value == \"include\" && customTagged(m.Content[i+1], 0) {", "\t\t\t\tif customTagged(m.Content[i+1], 0) {")]),
 ("gonk-9q0y: a `!reference` include in the ROOT file leaves the set complete",
  [(CI_, "\tout := CIIncludes{Complete: !opaque} // includes:opaque-root", "\tout := CIIncludes{Complete: opaque || !opaque} // includes:opaque-root")]),
 ("gonk-9q0y: a `!reference` include in a NESTED file leaves the set complete",
  [(CI_, "\t\tif nopaque { // includes:opaque-nested", "\t\tif false && nopaque { // includes:opaque-nested")]),
 ("gonk-9q0y: a `!reference` entry inside an include list is not looked for (only the include node itself)",
  [(CY, "\t\tif customTagged(c, depth+1) {\n\t\t\tfound = true\n", "\t\tif false && customTagged(c, depth+1) {\n\t\t\tfound = true\n")]),
 ("facts: a remote include URL is taken for a local path",
  [(CY, "\t\tif strings.Contains(s, \"://\") {\n\t\t\treturn nil\n\t\t}\n", "")]),
 ("facts: an include that escapes the repository is accepted",
  [(CY, "\t\tif err := ValidatePattern(s); err != nil {\n\t\t\treturn fmt.Errorf(\"scope: .gitlab-ci.yml include: %w\", err)\n\t\t}\n", "")]),
 ("facts: `local:` entries of an include list are ignored",
  [(CY, "\t\t\tif l, ok := t[\"local\"].(string); ok {\n\t\t\t\treturn add(l)\n\t\t\t}\n", "")]),
 ("facts: the generated marker is honoured on any line",
  [(CY, "\tline, _, _ := bytes.Cut(content, []byte(\"\\n\"))\n\treturn generatedRe.Match(bytes.TrimSuffix(line, []byte(\"\\r\")))",
        "\tfor _, line := range bytes.Split(content, []byte(\"\\n\")) {\n\t\tif generatedRe.Match(bytes.TrimSuffix(line, []byte(\"\\r\"))) {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false")]),
 ("policy: deny_lift of a non-liftable class accepted",
  [(PO, "\tfor _, c := range p.DenyLift {\n\t\tif !Liftable(c) {", "\tfor _, c := range p.DenyLift {\n\t\tif false {")]),
 ("policy: the pattern count is unbounded",
  [(PO, "\t\tif len(k.patterns) > MaxPatterns {", "\t\tif false {"),
   (PO, "\t\t\tif err := ValidatePattern(pat); err != nil {", "\t\t\tif err := ValidatePattern(pat); err != nil && pat != \"\" {")]),
 ("policy: character classes accepted in a pattern",
  [('pkg/scope/glob.go', 'strings.ContainsAny(p, "[]\\\\\\x00")', 'strings.ContainsAny(p, "\\x00")')]),
 ("classes: a star crosses a slash",
  [('pkg/scope/glob.go', "\t\tif ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {\n\t\t\treturn false\n\t\t}\n\t\tpat, segs = pat[1:], segs[1:]",
        "\t\tif ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {\n\t\t\tif len(segs) > 1 && strings.Contains(pat[0], \"*\") {\n\t\t\t\treturn matchSegs(pat, append([]string{segs[0] + segs[1]}, segs[2:]...))\n\t\t\t}\n\t\t\treturn false\n\t\t}\n\t\tpat, segs = pat[1:], segs[1:]")]),
 ("classes: a trailing ** matches the directory name itself",
  [('pkg/scope/glob.go', "\t\t\tif len(pat) == 1 {\n\t\t\t\treturn len(segs) >= 1\n\t\t\t}", "\t\t\tif len(pat) == 1 {\n\t\t\t\treturn true\n\t\t\t}")]),
 ("log: a reclassification is not logged (Q3)",
  [(CY, "\tif d.Reclassified() {\n\t\tc.log.Info(", "\tif false && d.Reclassified() {\n\t\tc.log.Info(")]),
 ("log: a refused reclassification is not logged",
  [(CY, "\tfor _, r := range d.Refused {\n", "\tfor _, r := range d.Refused[:0] {\n")]),

 # ---- config surfaces (S3) ----
 ("S3: invalid .gonk.yml scope does NOT deny at dispatch (gonkcfg.Load skips the lattice)",
  [(GC, "\tif err := pc.Scope.Validate(scope.LayerProject); err != nil {", "\tif err := error(nil); err != nil {")]),
 ("S3: a project's scope is validated as an operator's (source_paths accepted in Go)",
  [(GC, "pc.Scope.Validate(scope.LayerProject)", "pc.Scope.Validate(scope.LayerOperator)"),
   (FO, '{"project", LayerProject, in.Project}}', '{"project", LayerOperator, in.Project}}')]),
 ("S3: the operator config's scope is not validated",
  [(OC, "\tif err := p.Scope.Validate(scope.LayerOperator); err != nil {", "\tif err := p.Scope.Validate(scope.LayerOperator); err != nil && false {")]),
 ("S3: LoadShape no longer skips [scope]",
  [(SL, "\t\tif k == TrajectorySection || k == ScopeSection {", "\t\tif k == TrajectorySection {")]),
 ("S3: LoadShape skips any section that looks like scope",
  [(SL, "\t\tif k == TrajectorySection || k == ScopeSection {", "\t\tif k == TrajectorySection || strings.HasPrefix(k, ScopeSection) {"),
   (SL, 'import (\n\t"fmt"\n', 'import (\n\t"fmt"\n\t"strings"\n')]),
 ("S3: an unknown key in the pack [scope] table is ignored",
  [(PK, "\t\tif len(k) > 0 && k[0] == PackSection {", "\t\tif false {")]),
 ("S3: a pack row with a missing cap loads (as cap 0)",
  [(PK, "\t\t\tif len(r.Classes)+len(r.Liftable) > 0 {", "\t\t\tif false {")]),
 ("S3: the pack row is not validated on load",
  [(PK, "\tif err := row.validate(); err != nil {\n\t\treturn Row{}, fmt.Errorf(\"scope: load pack row %s: %w\", path, err)\n\t}\n", "")]),
 ("S3: negative caps accepted in a layer",
  [(PO, "\t\tif c.v != nil && *c.v < 0 {", "\t\tif false {")]),
 ("S3: a protected class accepted in a layer's classes",
  [(PO, "\t\tif c.Sensitivity() == Protected {\n\t\t\treturn fmt.Errorf(\"scope: classes:", "\t\tif false {\n\t\t\treturn fmt.Errorf(\"scope: classes:")]),

 # ---- the contract: policy never from the batch or .agent/ ----
 ("CONTRACT: pkg/scope reads policy from .agent/ in the working tree",
  [('pkg/scope/zz_leak.go', None,
    'package scope\n\nimport "os"\n\n// agentOverride is the self-widening hole.\nfunc agentOverride() []byte {\n\tb, _ := os.ReadFile(".agent/scope.toml")\n\treturn b\n}\n\nvar _ = agentOverride\n')]),
 ("CONTRACT: pkg/scope reads a second file (the /city copy) without naming .agent",
  [(PK, "\tmd, err := toml.DecodeFile(path, &doc)", "\tif b, rerr := os.ReadFile(filepath.Join(\"/city\", agent)); rerr == nil {\n\t\t_ = b\n\t}\n\tmd, err := toml.DecodeFile(path, &doc)")]),
 ("CONTRACT: pkg/scope imports another gonk package (it could see a batch)",
  [('pkg/scope/zz_leak.go', None,
    'package scope\n\nimport "github.com/gonk-factory/gonk/pkg/atags"\n\nvar _ = atags.TriggerIssueTriage\n')]),
 ("CONTRACT: the pack row is opened from a different path",
  [(PK, 'filepath.Join(packDir, "agents", agent, "effect-shape.toml")', 'filepath.Join(packDir, "agents", agent, "scope.toml")')]),
 ("CONTRACT: the batch gains a lift field",
  [(EF, "type Effect struct {\n\tKind   Kind     `json:\"kind\"`", "type Effect struct {\n\tLift   []string `json:\"lift,omitempty\"`\n\tKind   Kind     `json:\"kind\"`")]),
 ("CONTRACT: the batch carries a scope.Lift value",
  [(EF, "type Effect struct {\n\tKind   Kind     `json:\"kind\"`", "type Effect struct {\n\tGranted scope.Lift `json:\"-\"`\n\tKind   Kind     `json:\"kind\"`"),
   (EF, 'import (\n\t"bytes"\n\t"encoding/json"\n\t"errors"\n\t"fmt"\n\t"strings"\n)', 'import (\n\t"bytes"\n\t"encoding/json"\n\t"errors"\n\t"fmt"\n\t"strings"\n\n\t"github.com/gonk-factory/gonk/pkg/scope"\n)')]),
 ("CONTRACT: Fold's Input gains an untyped field (a batch could be passed in)",
  [(FO, "\t// Row: the baked pack (LoadRow).\n\tRow Row\n", "\t// Row: the baked pack (LoadRow).\n\tRow Row\n\tHint any\n")]),
 # ---- the group fold (gonk-a82p): GroupFor(project).Scope ----
 ("fold: GroupFor drops the scope section (gonk-a82p)",
  [(OC, "\tout.Scope = scope.FoldPolicies(base.Scope, next.Scope)\n", "")]),
 ("fold: a nested group's scope replaces its parent's instead of combining",
  [(OC, "\tout.Scope = scope.FoldPolicies(base.Scope, next.Scope)\n", "\tout.Scope = next.Scope\n")]),
 ("fold: group caps take the LARGER value (a nested group loosens its parent)",
  [(PO, "\tcase b == nil || *a <= *b:", "\tcase b == nil || *a >= *b:")]),
 ("fold: group classes are unioned (a nested group widens its parent)",
  [(PO, "\t\tif containsClass(b, c) && !containsClass(out, c) {", "\t\tif !containsClass(out, c) {"),
   (PO, "\t\t}\n\t}\n\treturn out\n}\n\nfunc containsClass", "\t\t}\n\t}\n\tfor _, c := range b {\n\t\tif !containsClass(out, c) {\n\t\t\tout = append(out, c)\n\t\t}\n\t}\n\treturn out\n}\n\nfunc containsClass")]),
 ("fold: an empty class intersection becomes nil (silent) instead of allow-nothing",
  [(PO, "\tout := []Class{}\n\tfor _, c := range a {", "\tvar out []Class\n\tfor _, c := range a {")]),
 ("fold: group deny_lift keeps only the nested group's veto",
  [(PO, "for _, c := range append(append([]Class(nil), base.DenyLift...), next.DenyLift...) {", "for _, c := range next.DenyLift {")]),
 ("fold: group protected_paths keep only the nested group's patterns",
  [(PO, "ProtectedPaths: unionStrings(base.ProtectedPaths, next.ProtectedPaths),", "ProtectedPaths: next.ProtectedPaths,")]),
 ("fold: the group ci_paths of the coarser group are dropped",
  [(PO, "CIPaths:        unionStrings(base.CIPaths, next.CIPaths),", "CIPaths:        unionStrings(nil, next.CIPaths),")]),
 ("fold: one group cap (max_lines) is not folded",
  [(PO, "MaxLines:      minCap(base.Caps.MaxLines, next.Caps.MaxLines),", "")]),
 ("fold: foldPolicy forgets Actions.Scaffold (the structural guard)",
  [(OC, "\t\tScaffold:      foldVeto(base.Actions.Scaffold, next.Actions.Scaffold),\n", "")]),
 # ---- ecosystem profiles (gonk-wj14; 066 section 11) ----
 # rule 2: sensitive rows always apply; permissive rows only when selected
 ("PROFILES: a sensitive row applies only when its profile is selected (package.json is other on a go project)",
  [(PF, "\t\t\tif !alwaysApplies(c) && !sel[pr.Name] {", "\t\t\tif !sel[pr.Name] {")]),
 ("PROFILES: permissive rows of every shipped profile apply, selected or not (a .py file is source by default)",
  [(PF, "\t\t\tif !alwaysApplies(c) && !sel[pr.Name] {", "\t\t\tif false {")]),
 ("PROFILES: deps is matched after source (a selected permissive row beats a sensitive one)",
  [(CL, ORDER, ORDER.replace("ClassTestConfig, ClassDeps,", "ClassTestConfig,").replace("ClassSource, ClassOther,", "ClassSource, ClassDeps, ClassOther,"))]),
 ("PROFILES: python's setup.py is not a sensitive row (source once python is selected)",
  [(PF, '"**/pyproject.toml", "**/setup.py", "**/Pipfile*"', '"**/pyproject.toml", "**/Pipfile*"')]),
 ("PROFILES: conftest.py is not a sensitive row (test once python is selected)",
  [(PF, '{ClassTestConfig, []string{"**/conftest.py", "**/pytest.ini"', '{ClassTestConfig, []string{"**/pytest.ini"')]),
 ("PROFILES: the classifier ignores the selection (always the default table)",
  [(CY, "\treturn &Classifier{eff: e, tbl: e.table(), base: base, log: log}", "\treturn &Classifier{eff: e, tbl: defaultTable, base: base, log: log}")]),
 ("PROFILES: the table's file-name shortcut matches the FIRST path segment",
  [(PF, "\t\tok, err := path.Match(tp.base, segs[len(segs)-1])", "\t\tok, err := path.Match(tp.base, segs[0])")]),
 # rule 3: the default, and byte-identity of go + docs-md with the old table
 ("PROFILES: the default selection gains python",
  [(PF, 'var defaultProfiles = []string{"go", "docs-md"}', 'var defaultProfiles = []string{"go", "python", "docs-md"}')]),
 ("PROFILES: the default selection loses docs-md",
  [(PF, 'var defaultProfiles = []string{"go", "docs-md"}', 'var defaultProfiles = []string{"go"}')]),
 ("PROFILES: the go profile is not the table before profiles (testdata/ dropped)",
  [(PF, '{ClassTest, []string{"**/*_test.go", "**/testdata/**"}},', '{ClassTest, []string{"**/*_test.go"}},')]),
 ("PROFILES: the go profile is not the table before profiles (go.mod matched anywhere)",
  [(PF, '{ClassDeps, []string{"go.mod", "go.sum", "vendor/**"}},', '{ClassDeps, []string{"**/go.mod", "go.sum", "vendor/**"}},')]),
 ("PROFILES: docs-md is not the docs row before profiles (rst added)",
  [(PF, '{ClassDocs, []string{"**/*.md", "docs/**", "CHANGELOG*"}},', '{ClassDocs, []string{"**/*.md", "**/*.rst", "docs/**", "CHANGELOG*"}},')]),
 ("PROFILES: a new always-on row tightens a path the tightened golden does not list",
  [(PF, '{ClassGenerated, []string{"**/*.min.js", "**/*.min.css", "**/*.js.map", "**/*.css.map"}},', '{ClassGenerated, []string{"**/*.min.js", "**/*.min.css", "**/*.js.map", "**/*.css.map", "**/*.d.ts"}},')]),
 # rule 3: selection -- unknown refused, the operator's menu
 ("PROFILES: an unknown profile name is accepted",
  [(PF, "\t\tif _, ok := LookupProfile(n); !ok {", "\t\tif false {")]),
 ("PROFILES: a profile listed twice is accepted",
  [(PF, "\t\tif seen[n] {\n\t\t\treturn fmt.Errorf(\"scope: %s: profile %q is listed twice\", key, n)\n\t\t}\n", "")]),
 ("PROFILES: a project may select a profile outside the operator's allowed_profiles",
  [(PF, "\t\t\tif !allowed[n] {\n\t\t\t\treturn nil, fmt.Errorf(\"scope: profiles: %q is not in the operator's allowed_profiles", "\t\t\tif false {\n\t\t\t\treturn nil, fmt.Errorf(\"scope: profiles: %q is not in the operator's allowed_profiles")]),
 ("PROFILES: only the first operator layer restricts the menu (the group's allowed_profiles is ignored)",
  [(PF, "\tfor _, op := range operator {\n\t\tif op.AllowedProfiles == nil {", "\tfor _, op := range operator[:min(1, len(operator))] {\n\t\tif op.AllowedProfiles == nil {")]),
 ("PROFILES: a later operator layer's menu replaces the earlier one instead of intersecting",
  [(PF, "\t\t\tif allowed[n] {\n\t\t\t\tnext[n] = true\n\t\t\t}\n\t\t}\n\t\tallowed = next", "\t\t\tnext[n] = true\n\t\t}\n\t\tallowed = next")]),
 ("PROFILES: the default selection ignores the operator's menu",
  [(PF, "\t\tfor _, n := range defaultProfiles {\n\t\t\tif allowed[n] {\n\t\t\t\twant[n] = true\n\t\t\t}\n\t\t}", "\t\tfor _, n := range defaultProfiles {\n\t\t\twant[n] = true\n\t\t}")]),
 ("PROFILES: Fold does not check the selection against the operator layers",
  [(FO, "\tprofiles, err := SelectProfiles(in.Project, in.Instance, in.Group)", "\tprofiles, err := SelectProfiles(in.Project)")]),
 ("PROFILES: the meter does not check the selection at registration (a disallowed profile dispatches)",
  [(SV, "\tif _, err := scope.SelectProfiles(pc.Scope, cfg.Instance.Scope, group.Scope); err != nil {", "\tif _, err := scope.SelectProfiles(pc.Scope, cfg.Instance.Scope, group.Scope); err != nil && false {")]),
 ("PROFILES: the meter checks the instance menu only (a group's allowed_profiles does not bind)",
  [(SV, "scope.SelectProfiles(pc.Scope, cfg.Instance.Scope, group.Scope)", "scope.SelectProfiles(pc.Scope, cfg.Instance.Scope)")]),
 ("PROFILES: the meter checks the group menu only (the instance's allowed_profiles does not bind)",
  [(SV, "scope.SelectProfiles(pc.Scope, cfg.Instance.Scope, group.Scope)", "scope.SelectProfiles(pc.Scope, group.Scope)")]),
 ("PROFILES: an operator layer may select profiles",
  [(PO, "\tif layer == LayerOperator && p.Profiles != nil {", "\tif false {")]),
 ("PROFILES: a project may set allowed_profiles",
  [(PO, "\tif layer == LayerProject && p.AllowedProfiles != nil {", "\tif false {")]),
 ("PROFILES: a layer's patterns are probed under the default selection, not its own",
  [(PO, "\tif p.Profiles != nil {\n\t\ttbl = buildTable(shipped, p.Profiles)\n\t}", "\tif false {\n\t\ttbl = buildTable(shipped, p.Profiles)\n\t}")]),
 # the group fold of allowed_profiles (and the structural guard)
 ("PROFILES: GroupFor drops allowed_profiles (the structural guard)",
  [(PO, "\n\t\tAllowedProfiles: foldAllowedProfiles(base.AllowedProfiles, next.AllowedProfiles),\n", "\n")]),
 ("PROFILES: a nested group's allowed_profiles replaces its parent's",
  [(PO, "AllowedProfiles: foldAllowedProfiles(base.AllowedProfiles, next.AllowedProfiles),", "AllowedProfiles: next.AllowedProfiles,")]),
 ("PROFILES: group menus are unioned (a nested group re-opens what its parent closed)",
  [(PO, "\tfor _, n := range a {\n\t\tif in[n] {\n\t\t\tout = append(out, n)\n\t\t\tin[n] = false\n\t\t}\n\t}\n\treturn out",
        "\tout = append(out, a...)\n\tfor _, n := range b {\n\t\tdup := false\n\t\tfor _, x := range a {\n\t\t\tdup = dup || x == n\n\t\t}\n\t\tif !dup {\n\t\t\tout = append(out, n)\n\t\t}\n\t}\n\t_ = in\n\treturn out")]),
 ("PROFILES: an empty menu intersection becomes nil (silent) instead of allow-none",
  [(PO, "\tout := []string{}\n\tfor _, n := range a {\n\t\tif in[n] {", "\tvar out []string\n\tfor _, n := range a {\n\t\tif in[n] {")]),
 ("PROFILES: a fold of operator layers carries the project-only `profiles` (the structural guard)",
  [(PO, "\n\t\tAllowedProfiles: foldAllowedProfiles(base.AllowedProfiles, next.AllowedProfiles),\n", "\n\t\tAllowedProfiles: foldAllowedProfiles(base.AllowedProfiles, next.AllowedProfiles),\n\t\tProfiles:        next.Profiles,\n")]),
 # profiles cannot move anything out of control / secret / ci
 ("PROFILES: a profile row may name ci (a profile could pull a path into the liftable class)",
  [(PF, "\tcase ClassDeploy, ClassTestConfig, ClassDeps, ClassGenerated, ClassTest, ClassDocs, ClassSource:\n\t\treturn true", "\tcase ClassCI, ClassDeploy, ClassTestConfig, ClassDeps, ClassGenerated, ClassTest, ClassDocs, ClassSource:\n\t\treturn true")]),
 ("PROFILES: a profile row may name any class (control, secret, agent-context, other)",
  [(PF, "\t\t\tif !profileClass(row.Class) {", "\t\t\tif !row.Class.Valid() {")]),
 ("PROFILES: profile rows are matched before the floor (.gitlab/package.json is deps, secrets/index.js is source)",
  [(PF, "\tsegs := strings.Split(p, \"/\")\n\tfor _, row := range t.rows {\n\t\t// The two fact-derived",
        "\tsegs := strings.Split(p, \"/\")\n\tfor _, row := range t.rows {\n\t\tfor _, tp := range row.pats {\n\t\t\tif tp.profile != \"\" && tp.match(segs) {\n\t\t\t\treturn row.class, tp.profile\n\t\t\t}\n\t\t}\n\t}\n\tfor _, row := range t.rows {\n\t\t// The two fact-derived")]),
 ("PROFILES: the shipped menu is not validated (a bad pattern, a silent ecosystem)",
  [(PF, "\tif err := validateProfiles(menu); err != nil {\n\t\tpanic(err)\n\t}\n\treturn menu", "\treturn menu"),
   (PF, "\t\t\tfor _, pat := range row.Patterns {\n\t\t\t\tif err := ValidatePattern(pat); err != nil {\n\t\t\t\t\treturn bad(\"class %s: %v\", row.Class, err)\n\t\t\t\t}\n\t\t\t}\n", "")]),
 ("PROFILES: a documentation profile may declare source paths",
  [(PF, "\t\t\tif pr.Kind == ProfileDocs && (row.Class == ClassTest || row.Class == ClassSource) {", "\t\t\tif false {")]),
 ("PROFILES: the accessors hand out the menu itself (a caller can edit it)",
  [(PF, "\tfor i, pr := range shipped {\n\t\tout[i] = copyProfile(pr)\n\t}", "\tfor i, pr := range shipped {\n\t\tout[i] = pr\n\t}"),
   (PF, "\t\tif pr.Name == name {\n\t\t\treturn copyProfile(pr), true\n\t\t}", "\t\tif pr.Name == name {\n\t\t\treturn pr, true\n\t\t}")]),
 # rule 5: testing methodology is a recorded property
 ("PROFILES: python claims a deterministic test-weakening predicate",
  [(PF, "Name: \"python\", Kind: ProfileEcosystem, Summary: \"Python (pytest conventions)\",\n\t\tWeakening: WeakeningNone,", "Name: \"python\", Kind: ProfileEcosystem, Summary: \"Python (pytest conventions)\",\n\t\tWeakening: WeakeningDeterministic,")]),
 ("PROFILES: go no longer declares its test-weakening predicate",
  [(PF, "Name: \"go\", Kind: ProfileEcosystem, Summary: \"Go modules\",\n\t\tWeakening: WeakeningDeterministic,", "Name: \"go\", Kind: ProfileEcosystem, Summary: \"Go modules\",\n\t\tWeakening: WeakeningNone,")]),
 ("PROFILES: an ecosystem profile may be silent about test weakening",
  [(PF, "\t\t\tif pr.Weakening != WeakeningDeterministic && pr.Weakening != WeakeningNone {", "\t\t\tif false {")]),
 ("PROFILES: WeakeningPredicate is true for a profile that is not shipped",
  [(PF, "\t\t\treturn pr.Weakening == WeakeningDeterministic\n\t\t}\n\t}\n\treturn false", "\t\t\treturn pr.Weakening == WeakeningDeterministic\n\t\t}\n\t}\n\treturn true")]),
 ("PROFILES: every source and test path is weakening-decidable (non-Go fixes look checkable)",
  [(CY, "\treturn WeakeningPredicate(d.Profile)", "\treturn true")]),
 ("PROFILES: a path that is not source or test is not weakening-decidable (docs-only is blocked)",
  [(CY, "\tif d.Class != ClassSource && d.Class != ClassTest {\n\t\treturn true\n\t}", "\tif d.Class != ClassSource && d.Class != ClassTest {\n\t\treturn false\n\t}")]),
 # rule 6: the reference documents the data
 ("PROFILES: a pattern is shipped that the reference does not document",
  [(PF, '{ClassSource, []string{"**/*.py"}},', '{ClassSource, []string{"**/*.py", "**/*.pyw"}},')]),
 # rule 4: onboarding proposes from marker files, once, on the default branch
 ("ONBOARD: every ecosystem is proposed whatever was found",
  [(PF, "\t\t\tif have[m] {\n\t\t\t\tprofiles = append(profiles, pr.Name)", "\t\t\tif true {\n\t\t\t\tprofiles = append(profiles, pr.Name)")]),
 ("ONBOARD: with no marker found, nothing is selected instead of the default",
  [(PF, "\tif len(profiles) == 0 {\n\t\treturn DefaultProfiles(), nil\n\t}", "\tif len(profiles) == 0 {\n\t\treturn []string{}, nil\n\t}")]),
 ("ONBOARD: package.json is not a marker for node",
  [(PF, 'Markers:   []string{"package.json"},', 'Markers:   []string{"package-lock.json"},')]),
 ("ONBOARD: the markers are read on the onboarding branch, not the default branch",
  [(IO, "\t\t_, err := o.GL.GetRawFile(ctx, p.ID, marker, ref, MaxOnboardFileBytes)", "\t\t_, err := o.GL.GetRawFile(ctx, p.ID, marker, OnboardBranch, MaxOnboardFileBytes)")]),
 ("ONBOARD: a marker probe that fails is read as 'absent' (the merge request is opened on a guess)",
  [(IO, "\t\tdefault:\n\t\t\treturn ProfileDetection{}, fmt.Errorf(\"onboard: look for %s on %s: %w\", marker, ref, err)\n", "\t\tdefault:\n")]),
 ("ONBOARD: an oversize marker file is not counted as present",
  [(IO, "\t\tcase err == nil, errors.Is(err, glab.ErrResponseTooLarge):", "\t\tcase err == nil && !errors.Is(err, glab.ErrResponseTooLarge):")]),
 ("ONBOARD: the detection is not written into the committed .gonk.yml (always the default)",
  [(IR, "\tprofiles, _ := c.Detection.Selection()\n\treturn RenderConfig(c.Ladder, profiles)", "\treturn RenderDefaultConfig(c.Ladder)")]),
 ("ONBOARD: the merge request does not say which markers were found",
  [(IR, "{{if .Found}}**yes**{{else}}no{{end}}", "no")]),
 ("ONBOARD: the merge request does not say why a profile was proposed",
  [(IR, "- ` + \"`{{.Profile}}`\" + ` is proposed because ` + \"`{{.Marker}}`\" + ` exists.{{end}}", "{{end}}")]),
 ("ONBOARD: the merge request does not say what the probe cannot see",
  [(IR, "What this check cannot see: it asks for those exact names at the repository\n", "It asks for those exact names at the repository\n")]),
 ("ONBOARD: RenderConfig renders a profile gonk would reject",
  [(IR, "\tif err := (scope.Policy{Profiles: append([]string{}, profiles...)}).Validate(scope.LayerProject); err != nil {", "\tif err := (scope.Policy{Profiles: append([]string{}, profiles...)}).Validate(scope.LayerProject); err != nil && false {")]),
 ("CONTRACT: profiles are detected somewhere other than onboarding (repository content chooses its own scope)",
  [('pkg/intake/zz_redetect.go', None,
    'package intake\n\nimport "github.com/gonk-factory/gonk/pkg/scope"\n\n// redetect is detection at dispatch time: the hole.\nfunc redetect(found []string) []string {\n\tp, _ := scope.ProposeProfiles(found)\n\treturn p\n}\n\nvar _ = redetect\n')]),
]


args = sys.argv[1:]
verbose = '-v' in args
only = [a for a in args if a != '-v']
res = []
for i, (name, edits) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals, created = {}, []
    vacuous = False
    for f, old, _ in edits:
        if old is None:
            if os.path.exists(f):
                vacuous = True
            continue
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if old is None:
            continue
        if old not in cur[f]:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        for f, old, new in edits:
            if old is None:
                open(f, 'w').write(new)
                created.append(f)
        r = subprocess.run([GO, "test"] + PKGS + ["-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        lines = out.splitlines()
        fails = [l.strip() for l in lines if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            msg = f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails)
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
        for f in created:
            os.remove(f)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} of {len(res)}")
sys.exit(1 if bad else 0)
