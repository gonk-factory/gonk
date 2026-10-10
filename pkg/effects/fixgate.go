package effects

// The pre-apply gate of a fix batch (gonk-2usw plan 5.3, gates A1 and A4-A6;
// gonk-066 section 2.2's D-tier predicates): given the batch, the envelope the
// CONTROLLER chose for this dispatch, and the base blobs the controller read,
// either a plan of exactly what to commit, or the rule code of the predicate
// that refused it.
//
// WHERE EACH INPUT COMES FROM IS THE SECURITY PROPERTY. The batch is model
// output. Everything else -- the folded scope, the lift, the cited files, the
// base blobs, the secrets -- is an ARGUMENT built by the controller from the
// dispatch record and the pinned base commit. Nothing here reads policy from
// the batch (a Batch has no field that could carry any), and a diagnosis that
// rides in the same batch cannot widen the cited-files bound.
//
// The predicates, by rule code, and what each needs:
//
//	FX-SHAPE, SC-FORM          the batch alone                       (A1)
//	SC-PROT, SC-CLASS,
//	SC-CITED, SC-BEAD          + the envelope                        (A4)
//	ED-*                       + the base blobs                      (A5)
//	SC-SIZE, SC-SECRET,
//	SC-CLASS(generated)        + the diff facts                      (A6)
//
// NOT HERE: SC-WEAKEN (a Go AST predicate over a changed test file), the CI
// pre-check and the definition-integrity check (plan 5.3 A7, 6.2), and the
// text gates on a fix_meta's reason (A8: Redact, ValidateComments, the
// forged-marker check). PlanFix's result carries what each of those needs.

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// FixEnvelope is what ONE fix dispatch may touch. The controller builds it at
// dispatch and records it; no part of it is ever taken from a batch.
type FixEnvelope struct {
	// Scope is the folded scope (pack row AND operator AND project config, read
	// at the base SHA), already narrowed by the controller to the row of plan
	// 4.2 for the diagnosis's class (scope.Effective.Narrow), and carrying any
	// declared path scope (scope.Effective.WithBead; `pipeline_fix.paths`).
	// Its Caps are the size caps. The zero value allows nothing.
	Scope scope.Effective
	// Base are the content-derived class facts the controller read at the base
	// SHA (the pipeline definition's local includes). A file whose base blob
	// marks it generated is detected from the blob itself; it need not be
	// listed here.
	Base scope.BaseFacts
	// Lift is the per-dispatch lift of a protected class, from
	// scope.Effective.GrantLift. The zero value lifts nothing.
	Lift scope.Lift
	// Cited are the landed diagnosis's cited files, from the RECORD. An edit's
	// path must be one of them, by exact string: a citation is a file, not a
	// directory and not a pattern. Empty means no file may be edited.
	Cited []string
	// Log receives the classifier's reclassification lines (scope.Classifier
	// logs every one). nil means slog.Default().
	Log *slog.Logger
}

// Secret is a value the controller holds and that must never appear in a
// change: the session's own model key, the bot's token. Its value cannot be
// read back, printed, logged or marshalled.
type Secret struct {
	name  string
	value string
}

// NewSecret names a secret value. The name is what an SC-SECRET refusal
// reports (as its sub code); keep it a stable identifier such as "session_key".
// An empty value matches nothing.
func NewSecret(name, value string) Secret { return Secret{name: name, value: value} }

// Name returns the secret's name.
func (s Secret) Name() string { return s.name }

// String, GoString, LogValue and MarshalJSON all yield the name and never the
// value, so a Secret that reaches a log line or a record says which secret it
// is and nothing else.
func (s Secret) String() string               { return "secret(" + s.name + ")" }
func (s Secret) GoString() string             { return s.String() }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(s.String()) }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + "secret" + `"`), nil }

// secretPatterns are the high-confidence key shapes of 066 SC-SECRET. They are
// deliberately FEWER than pipelinectx.Redact's: Redact scrubs a log excerpt and
// can afford to over-match (`password=...`, `token: ...`), whereas this gate
// REFUSES a change, and a source edit that assigns a variable named token is
// not a leak. What gets a fix_meta's prose scrubbed is Redact, at plan gate A8.
var secretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"gitlab_token", regexp.MustCompile(`\b(?:glpat|gldt|glrt|glrtr|glptt|glcbt|glsoat|glffct|gloas|glimt|glagent|glft|glwt|glsa)-[0-9A-Za-z_\-]{20,}`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
}

// FindSecret reports whether text holds one of the given secrets (exact
// bytes) or a high-confidence key shape, and which: the secret's name, or
// `private_key`, `gitlab_token`, `aws_access_key`. Exact secrets are checked
// first. It never returns the secret itself.
func FindSecret(text string, secrets []Secret) (string, bool) {
	for _, s := range secrets {
		if s.value != "" && strings.Contains(text, s.value) { // secret:exact
			return s.name, true
		}
	}
	for _, p := range secretPatterns {
		if p.re.MatchString(text) { // secret:pattern
			return p.kind, true
		}
	}
	return "", false
}

// ScopeRequest is a needs-scope answer, validated: what to render into the
// needs-scope note. Text only -- nothing here is ever written to a repository.
type ScopeRequest struct {
	Paths  []string
	Reason string
}

// FixPlan is a fix batch that passed every predicate here: exactly what the
// apply may do with it, and nothing the batch said beyond that.
type FixPlan struct {
	// Verdict is VerdictFix or VerdictNeedsScope.
	Verdict Verdict
	// Files are the files to commit (verdict fix), sorted by path: `update`
	// actions only. Empty for needs-scope.
	Files []PatchedFile
	// Facts are the change's diff facts, with each file's class filled in.
	Facts DiffFacts
	// Reason is the fix_meta's text (verdict fix). It has NOT been through
	// the text gates of plan 5.3 A8; the caller renders it only after they pass.
	Reason string
	// Request is the scope_request (verdict needs-scope), else nil.
	Request *ScopeRequest
}

// PlanFix is THE gate for a fix batch: every predicate in this package, in
// order, on one batch. It returns the plan, or the FIRST refusal (a *Refusal;
// see RuleOf) and a zero plan -- a refused batch yields nothing to apply.
//
// It is pure and cheap, so call it with the final inputs even if the phase
// functions (ValidateFix, ValidateEditPaths, ApplyEdits, ValidateDiff) were
// used earlier to refuse before reading blobs: PlanFix re-runs all of them, so
// no ordering mistake in a caller can skip one.
//
// For verdict needs-scope, base and secrets are not consulted.
func PlanFix(b Batch, env FixEnvelope, base BaseBlobs, secrets []Secret) (FixPlan, error) {
	g := newFixGate(env)
	if v := g.violations(b, base, secrets, true); len(v) > 0 {
		return FixPlan{}, v[0]
	}
	plan := FixPlan{Verdict: b.Verdict}
	for _, e := range b.Effects {
		switch e.Kind {
		case KindFixMeta:
			plan.Reason = e.Reason
		case KindScopeRequest:
			plan.Request = &ScopeRequest{Paths: append([]string(nil), e.Paths...), Reason: e.Reason}
		}
	}
	if b.Verdict == VerdictFix {
		plan.Files, plan.Facts = g.files, g.facts
	}
	return plan, nil
}

// FixViolations returns EVERY refusal PlanFix could give for this batch, not
// only the first: each predicate is evaluated on its own. It exists so a log
// line (and a test) can say everything that was wrong with a batch. The diff
// predicates are evaluated only when the edits apply, since without a result
// there is no diff. An empty result means PlanFix accepts.
func FixViolations(b Batch, env FixEnvelope, base BaseBlobs, secrets []Secret) []*Refusal {
	return newFixGate(env).violations(b, base, secrets, false)
}

// ValidateEditPaths is plan gate A4 alone: every edit's path is clean, not
// protected (SC-PROT, lift-aware), of a class the envelope allows (SC-CLASS),
// cited (SC-CITED) and inside any declared path scope (SC-BEAD). It needs no
// blob, so the controller can refuse before reading any.
func ValidateEditPaths(b Batch, env FixEnvelope) error {
	if v := newFixGate(env).pathViolations(b, true); len(v) > 0 {
		return v[0]
	}
	return nil
}

// ValidateDiff is plan gate A6 alone: SC-SIZE and SC-SECRET over the diff
// facts, and SC-CLASS for a file the facts show to be generated.
func ValidateDiff(d DiffFacts, env FixEnvelope, secrets []Secret) error {
	if v := newFixGate(env).diffViolations(&d, secrets); len(v) > 0 {
		return v[0]
	}
	return nil
}

// fixGate holds one evaluation's envelope and what it computed.
type fixGate struct {
	env     FixEnvelope
	cited   map[string]bool
	classes map[string]scope.Class // by path, so each path is classified (and logged) once
	files   []PatchedFile
	facts   DiffFacts
}

func newFixGate(env FixEnvelope) *fixGate {
	g := &fixGate{env: env, cited: make(map[string]bool, len(env.Cited)), classes: map[string]scope.Class{}}
	for _, c := range env.Cited {
		g.cited[c] = true
	}
	return g
}

// classify returns p's class under the envelope and the base facts the
// controller supplied.
func (g *fixGate) classify(p string) scope.Class {
	if c, ok := g.classes[p]; ok {
		return c
	}
	c := g.env.Scope.Classifier(g.env.Base, g.env.Log).Classify(p).Class
	g.classes[p] = c
	return c
}

// classifyGenerated returns p's class given that its base blob marks it
// generated -- a fact only the blob can tell, learned after gate A4.
func (g *fixGate) classifyGenerated(p string) scope.Class {
	base := g.env.Base
	merged := make(map[string]bool, len(base.Generated)+1)
	for k, v := range base.Generated {
		merged[k] = v
	}
	merged[p] = true
	base.Generated = merged
	return g.env.Scope.Classifier(base, g.env.Log).Classify(p).Class
}

// violations evaluates the predicates in gate order. With firstOnly it stops
// at the first refusal (PlanFix); without, it collects them all.
func (g *fixGate) violations(b Batch, base BaseBlobs, secrets []Secret, firstOnly bool) []*Refusal {
	var out []*Refusal
	done := func(v []*Refusal) bool {
		out = append(out, v...)
		return firstOnly && len(out) > 0
	}
	if done(fixShapeViolations(b)) { // gate:shape
		return out
	}
	if done(g.pathViolations(b, false)) { // gate:paths
		return out
	}
	files, r := applyEdits(b, base)
	if r != nil { // gate:apply
		// ApplyEdits repeats the form rules for its own safety; the shape gate
		// above already reported any such refusal for this batch.
		if r.Rule != RuleForm {
			out = append(out, r)
		}
		return out
	}
	g.files = files
	g.facts = ComputeDiffFacts(files)
	done(g.diffViolations(&g.facts, secrets)) // gate:diff
	return out
}

// pathViolations is gate A4. Per edit the predicates are independent, with two
// exceptions that would otherwise report one cause twice: an unclean path has
// no honest class, so nothing further is asked of it (its SC-FORM refusal is
// included only when reportForm is set -- ValidateEditPaths called alone --
// because inside violations() the shape gate has reported it already); and a
// protected path is SC-PROT, not also SC-CLASS.
func (g *fixGate) pathViolations(b Batch, reportForm bool) []*Refusal {
	var out []*Refusal
	for i, e := range b.Effects {
		if e.Kind != KindEdit {
			// A non-edit effect carrying a write path is refused by the shape
			// gate (foreign field); the denylist still looks at it, so that
			// this function alone never passes a protected path of any kind.
			if p, what := protectedPath(e, g.env.Lift); what != "" { // prot:any-kind
				out = append(out, refuse(RuleProtected, string(floorClass(p)), i, p, "%s", protectedReason(p, what)))
			}
			continue
		}
		if r := editForm(i, e); r != nil && r.Sub == "path" { // paths:unclean
			if reportForm {
				out = append(out, r)
			}
			continue
		}
		class := g.classify(e.Path)
		switch p, what := protectedPath(e, g.env.Lift); {
		case what != "": // prot:denylist
			// The existing lift-aware denylist (protected.go), in its own words.
			sub := floorClass(p)
			if class.Sensitivity() == scope.Protected {
				sub = class
			}
			out = append(out, refuse(RuleProtected, string(sub), i, e.Path, "%s", protectedReason(p, what)))
		case class.Sensitivity() == scope.Protected && !g.env.Scope.Allows(class, g.env.Lift): // prot:class
			// Protected by CLASS though on no denylist: a secret-class file, a
			// path a layer declared protected, a pipeline-included file without
			// a lift this scope could have granted.
			out = append(out, refuse(RuleProtected, string(class), i, e.Path, "path %q is class %s, which is protected for this dispatch", e.Path, class))
		case !g.env.Scope.Allows(class, g.env.Lift): // class:envelope
			out = append(out, refuse(RuleClass, string(class), i, e.Path, "path %q is class %s; this dispatch may edit %s", e.Path, class, g.writable()))
		}
		if !g.cited[e.Path] { // cited:bound
			out = append(out, refuse(RuleCited, "", i, e.Path, "path %q is not one of the files the diagnosis cited; a fix may edit only those", e.Path))
		}
		if !g.env.Scope.InBead(e.Path) { // bead:bound
			out = append(out, refuse(RuleBead, "", i, e.Path, "path %q is outside the paths this project allows a fix to edit", e.Path))
		}
	}
	return out
}

// writable describes the envelope's classes for a refusal's detail.
func (g *fixGate) writable() string {
	if !g.env.Scope.Enabled {
		return "nothing (the workflow is disabled for this project)"
	}
	names := []string{}
	for _, c := range g.env.Scope.Classes {
		names = append(names, string(c))
	}
	for _, c := range g.env.Lift.Classes() {
		if g.env.Scope.Allows(c, g.env.Lift) {
			names = append(names, string(c)+" (lifted)")
		}
	}
	if len(names) == 0 {
		return "no class of file"
	}
	return "only: " + strings.Join(names, ", ")
}

// diffViolations is gate A6. It fills each file's Class in d.
func (g *fixGate) diffViolations(d *DiffFacts, secrets []Secret) []*Refusal {
	var out []*Refusal
	caps := g.env.Scope.Caps
	diff := func(sub, path, format string, args ...any) {
		out = append(out, refuse(RuleSize, sub, -1, path, format, args...))
	}
	if int64(d.FilesChanged) > caps.MaxFiles { // size:max_files
		diff("max_files", "", "%d files changed, max %d", d.FilesChanged, caps.MaxFiles)
	}
	if int64(d.Lines()) > caps.MaxLines { // size:max_lines
		diff("max_lines", "", "%d lines added + %d removed = %d, max %d", d.LinesAdded, d.LinesRemoved, d.Lines(), caps.MaxLines)
	}
	if int64(d.NewBytes) > caps.MaxPatchBytes { // size:max_patch_bytes
		diff("max_patch_bytes", "", "%d bytes of new text across the batch, max %d", d.NewBytes, caps.MaxPatchBytes)
	}
	for i := range d.Files {
		f := &d.Files[i]
		if int64(f.BaseBytes) > caps.MaxFileBytes || int64(f.ResultBytes) > caps.MaxFileBytes { // size:max_file_bytes
			diff("max_file_bytes", f.Path, "the file is %d bytes before and %d after, max %d", f.BaseBytes, f.ResultBytes, caps.MaxFileBytes)
		}
		// The class, now that the base blob has been seen. Only a file the
		// blob shows to be GENERATED can differ from what gate A4 decided by
		// path, so only that is refused here; the rest was A4's.
		f.Class = g.classify(f.Path)
		if f.Generated { // class:generated
			if c := g.classifyGenerated(f.Path); c != f.Class {
				f.Class = c
				if !g.env.Scope.Allows(c, g.env.Lift) {
					out = append(out, refuse(RuleClass, string(c), -1, f.Path, "the file is generated (its first line says so) and so is class %s; this dispatch may edit %s", c, g.writable()))
				}
			}
		}
		for _, line := range f.AddedLines {
			if kind, ok := FindSecret(line, secrets); ok { // secret:added-lines
				out = append(out, refuse(RuleSecret, kind, -1, f.Path, "an added line holds a secret (%s); the change is refused and the value is not repeated here", kind))
				break // one refusal per file says it
			}
		}
	}
	return out
}
