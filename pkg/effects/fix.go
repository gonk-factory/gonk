package effects

// The pipeline-fix vocabulary (gonk-2usw plan 4.1): what a fix session may
// PROPOSE. Three effect kinds and two verdicts:
//
//	edit           path, old, new   an exact-match replacement in an existing file
//	fix_meta       reason           the agent's explanation, for the MR description
//	scope_request  paths, reason    "this needs a change I may not make"
//	verdict fix          => >= 1 edit, exactly 1 fix_meta, no scope_request
//	verdict needs-scope  => exactly 1 scope_request, no edit, no fix_meta
//
// Deliberately absent, and to stay absent: any branch / commit / merge_request
// / label / assignee / reviewer / approve / retry / cancel kind, and file
// CREATION and DELETION (an edit needs an existing blob). A fix that needs one
// of those answers needs-scope.
//
// WHAT IS HERE AND WHAT IS NOT. This file is the vocabulary and its SHAPE
// rules (ValidateFix; plan 5.3 gate A1). Applying the edits to base blobs and
// the diff facts are edit.go; the scope predicates (SC-PROT, SC-CLASS,
// SC-CITED, SC-BEAD, SC-SIZE, SC-SECRET) and the one entry point PlanFix are
// fixgate.go. Everything is pure: no GitLab client, no network, no clock. The
// controller reads the blobs and builds the envelope; nothing here can.
//
// Every refusal is a *Refusal carrying a stable RULE CODE, so a rejection is
// countable and testable without matching prose (066 section 2.2).

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	// KindEdit is an exact-match replacement in an EXISTING file: `old` must
	// occur exactly once in the file as it stands after the batch's earlier
	// edits to the same path (ApplyEdits). It is the only way a fix session
	// proposes a change, and the controller -- never the agent -- turns the
	// result into a commit.
	KindEdit Kind = "edit"
	// KindFixMeta carries the agent's explanation of its fix.
	KindFixMeta Kind = "fix_meta"
	// KindScopeRequest is the needs-scope answer: paths the session would have
	// needed and why. It is rejection-shaped -- it lands a note, never a change.
	KindScopeRequest Kind = "scope_request"
)

const (
	// VerdictFix: the batch's edits are the proposed fix.
	VerdictFix Verdict = "fix"
	// VerdictNeedsScope: the fix needs a change outside what this dispatch may
	// make. A correct outcome, not a failure (066 4.2; plan 4.4).
	VerdictNeedsScope Verdict = "needs-scope"
)

// Caps on the vocabulary itself (plan 4.1, 4.3). The size caps on the CHANGE
// (files, lines, bytes) are scope.Caps, enforced as SC-SIZE in fixgate.go.
const (
	// MaxFixEdits bounds the `edit` effects of one batch. It is enforced here
	// as well as by the fix agent's effect-shape.toml, because that file may
	// only tighten it.
	MaxFixEdits = 10
	// MaxFixReasonChars bounds a fix_meta's reason, in characters (runes).
	MaxFixReasonChars = 600
	// MaxScopeRequestPaths bounds a scope_request's paths.
	MaxScopeRequestPaths = 5
	// MaxScopeRequestReasonChars bounds a scope_request's reason, in characters.
	MaxScopeRequestReasonChars = 600
)

// Rule is the stable code of the predicate that refused a fix batch.
type Rule string

// The rule codes. SC-* are gonk-066 section 2.2's, with SC-CITED added for the
// cited-files bound of gonk-2usw plan 4.2; ED-* are the ways an edit can fail
// to apply to its base blob (066 folds these into SC-BASE, "the patch does not
// apply"; they are split because each tells the session something different);
// FX-SHAPE is the shape of the fix batch itself.
const (
	// RuleFixShape: the verdict/kind rule, the edit-count cap, a foreign field,
	// or a malformed fix_meta / scope_request.
	RuleFixShape Rule = "FX-SHAPE"
	// RuleForm: an edit's path is not a clean relative path, or its old/new is
	// empty, identical, or not NUL-free UTF-8.
	RuleForm Rule = "SC-FORM"
	// RuleProtected: the path is protected -- on the denylist and not lifted
	// for this dispatch, or of a protected class (control, secret, or ci
	// without a lift this scope could grant).
	RuleProtected Rule = "SC-PROT"
	// RuleClass: the path's class is not one this dispatch's envelope allows.
	RuleClass Rule = "SC-CLASS"
	// RuleCited: the path is not one of the diagnosis's cited files.
	RuleCited Rule = "SC-CITED"
	// RuleBead: the path is outside a declared path scope (scope.WithBead).
	RuleBead Rule = "SC-BEAD"
	// RuleSize: a size cap is exceeded; the sub code names the cap.
	RuleSize Rule = "SC-SIZE"
	// RuleSecret: an added line holds a secret; the sub code names which.
	RuleSecret Rule = "SC-SECRET"
	// RuleEditMissing: no file at the edit's path at the base (no creation).
	RuleEditMissing Rule = "ED-MISSING"
	// RuleEditEncoding: the file is binary or not UTF-8.
	RuleEditEncoding Rule = "ED-ENCODING"
	// RuleEditNoMatch: `old` does not occur in the file.
	RuleEditNoMatch Rule = "ED-NOMATCH"
	// RuleEditAmbiguous: `old` occurs more than once in the file.
	RuleEditAmbiguous Rule = "ED-AMBIGUOUS"
	// RuleEditEmpties: the edits leave the file empty (a deletion in all but name).
	RuleEditEmpties Rule = "ED-EMPTIES"
	// RuleEditNoop: the edits leave the file byte-identical.
	RuleEditNoop Rule = "ED-NOOP"
)

var ruleOrder = []Rule{
	RuleFixShape, RuleForm, RuleProtected, RuleClass, RuleCited, RuleBead, RuleSize, RuleSecret,
	RuleEditMissing, RuleEditEncoding, RuleEditNoMatch, RuleEditAmbiguous, RuleEditEmpties, RuleEditNoop,
}

// Rules returns every rule code a fix refusal can carry (a copy). The list is
// a contract: metrics, notes and tests key on these strings.
func Rules() []Rule { return append([]Rule(nil), ruleOrder...) }

// Refusal is a fix batch refused by one predicate. It is the agent's failure
// (gate-failed), never ours: a problem of OURS -- an unreadable blob, an
// invalid policy -- is an ordinary error from whoever read it, and never a
// *Refusal.
type Refusal struct {
	// Rule is the predicate's stable code.
	Rule Rule
	// Sub narrows it with a second stable code where one rule has several
	// causes: the cap for SC-SIZE (`max_files`, `max_lines`, `max_patch_bytes`,
	// `max_file_bytes`), the secret's name or pattern for SC-SECRET, the class
	// for SC-CLASS and SC-PROT, the sub-rule for SC-FORM and FX-SHAPE.
	Sub string
	// Effect is the index of the effect refused, or -1 when the refusal is
	// about the batch or the diff as a whole.
	Effect int
	// Path is the repository path concerned ("" when none).
	Path string
	// Detail is for a human and the session's feedback. It never contains a
	// secret's value. Do not match on it.
	Detail string
}

// Code is the rule with its sub code: "SC-SIZE(max_lines)", or "SC-CITED".
func (r *Refusal) Code() string {
	if r.Sub == "" {
		return string(r.Rule)
	}
	return string(r.Rule) + "(" + r.Sub + ")"
}

func (r *Refusal) Error() string {
	var sb strings.Builder
	sb.WriteString("effects: " + r.Code() + ": ")
	switch {
	case r.Effect >= 0 && r.Path != "":
		fmt.Fprintf(&sb, "effect %d (%s): ", r.Effect, r.Path)
	case r.Effect >= 0:
		fmt.Fprintf(&sb, "effect %d: ", r.Effect)
	case r.Path != "":
		sb.WriteString(r.Path + ": ")
	}
	sb.WriteString(r.Detail)
	return sb.String()
}

// AsRefusal returns the *Refusal in err's chain, or nil.
func AsRefusal(err error) *Refusal {
	var r *Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// RuleOf returns the rule code of the refusal in err's chain, or "" when err
// is nil or not a refusal (an error of ours, not a gate decision).
func RuleOf(err error) Rule {
	if r := AsRefusal(err); r != nil {
		return r.Rule
	}
	return ""
}

// SubOf returns the sub code of the refusal in err's chain, or "".
func SubOf(err error) string {
	if r := AsRefusal(err); r != nil {
		return r.Sub
	}
	return ""
}

func refuse(rule Rule, sub string, effect int, path, format string, args ...any) *Refusal {
	return &Refusal{Rule: rule, Sub: sub, Effect: effect, Path: path, Detail: fmt.Sprintf(format, args...)}
}

// verdictKindRule ties the two fix verdicts to the three fix kinds, in both
// directions. It is called by Validate for EVERY agent's batch: a batch that
// states a fix verdict must carry the fix kinds, and a batch that carries a
// fix kind must state the matching verdict. Since only the fix agent's shape
// lists those kinds, no other agent can state either verdict or carry any of
// the kinds.
func verdictKindRule(b Batch) *Refusal {
	var edits, metas, requests int
	for _, e := range b.Effects {
		switch e.Kind {
		case KindEdit:
			edits++
		case KindFixMeta:
			metas++
		case KindScopeRequest:
			requests++
		}
	}
	batch := func(sub, format string, args ...any) *Refusal {
		return refuse(RuleFixShape, sub, -1, "", format, args...)
	}
	switch b.Verdict {
	case VerdictFix:
		switch {
		case requests > 0: // cross:fix-with-request
			return batch("fix-with-request", "verdict %q carries a scope_request; a fix that needs wider scope answers %q and proposes no edit", VerdictFix, VerdictNeedsScope)
		case edits == 0: // cross:fix-needs-edit
			return batch("fix-needs-edit", "verdict %q needs at least one edit", VerdictFix)
		case metas != 1: // cross:fix-needs-meta
			return batch("fix-needs-meta", "verdict %q needs exactly one fix_meta, got %d", VerdictFix, metas)
		}
	case VerdictNeedsScope:
		switch {
		case edits > 0: // cross:needs-scope-with-edit
			return batch("needs-scope-with-edit", "verdict %q carries %d edit(s); it proposes no change", VerdictNeedsScope, edits)
		case metas > 0: // cross:needs-scope-with-meta
			return batch("needs-scope-with-meta", "verdict %q carries a fix_meta; it proposes no fix to explain", VerdictNeedsScope)
		case requests != 1: // cross:needs-scope-needs-request
			return batch("needs-scope-needs-request", "verdict %q needs exactly one scope_request, got %d", VerdictNeedsScope, requests)
		}
	default:
		switch {
		case edits > 0: // cross:edit-needs-fix
			return batch("edit-needs-fix", "an edit is only valid under verdict %q, got %q", VerdictFix, b.EffectiveVerdict())
		case metas > 0: // cross:meta-needs-fix
			return batch("meta-needs-fix", "a fix_meta is only valid under verdict %q, got %q", VerdictFix, b.EffectiveVerdict())
		case requests > 0: // cross:request-needs-needs-scope
			return batch("request-needs-needs-scope", "a scope_request is only valid under verdict %q, got %q", VerdictNeedsScope, b.EffectiveVerdict())
		}
	}
	return nil
}

// fixKindFields names, per fix kind, the Effect fields that kind carries.
// Every other field is foreign to it -- including fields added to Effect after
// this was written, which is why the check is by reflection.
var fixKindFields = map[Kind]map[string]bool{
	KindEdit:         {"Path": true, "Old": true, "New": true},
	KindFixMeta:      {"Reason": true},
	KindScopeRequest: {"Paths": true, "Reason": true},
}

// targetFields are left to ValidateTargets, which refuses them for these kinds
// (effectTargetKinds lists none of the three) with its own message.
var targetFields = map[string]bool{"TargetIID": true, "TargetType": true}

// foreignField returns the name of the first field e carries that its kind
// does not own, or "". It is defined for the three fix kinds only.
func foreignField(e Effect) string {
	return foreignTo(e, fixKindFields[e.Kind])
}

// foreignTo returns the name of the first field e carries that is not Kind,
// not a target field and not in own, or "". The name is a Go field name of
// Effect: the controller's text, never the batch's.
func foreignTo(e Effect, own map[string]bool) string {
	rv := reflect.ValueOf(e)
	for i := 0; i < rv.NumField(); i++ {
		name := rv.Type().Field(i).Name
		if name == "Kind" || own[name] || targetFields[name] {
			continue
		}
		if !rv.Field(i).IsZero() {
			return name
		}
	}
	return ""
}

// ValidateFix is the shape gate of a FIX batch (plan 5.3 A1): the verdict is
// one of the two fix verdicts, the verdict/kind rule holds, the batch carries
// nothing but the three fix kinds, and every effect is well-formed. It needs
// nothing but the batch.
//
// It is NOT a no-op for other batches: a batch with no fix verdict is refused.
// Call it only in the fix agent's apply chain. (What keeps the fix vocabulary
// out of every OTHER agent's batch is Validate, not this.)
func ValidateFix(b Batch) error {
	if v := fixShapeViolations(b); len(v) > 0 {
		return v[0]
	}
	return nil
}

// fixShapeViolations returns every shape violation, batch-level ones first,
// then per effect in order (at most one per effect).
func fixShapeViolations(b Batch) []*Refusal {
	var out []*Refusal
	if b.Verdict != VerdictFix && b.Verdict != VerdictNeedsScope { // shape:verdict
		out = append(out, refuse(RuleFixShape, "verdict", -1, "", "a fix batch states verdict %q or %q, got %q", VerdictFix, VerdictNeedsScope, b.Verdict))
	} else if r := verdictKindRule(b); r != nil {
		out = append(out, r)
	}
	edits := 0
	for _, e := range b.Effects {
		if e.Kind == KindEdit {
			edits++
		}
	}
	if edits > MaxFixEdits { // shape:edit-count
		out = append(out, refuse(RuleFixShape, "edit-count", -1, "", "%d edits, max %d", edits, MaxFixEdits))
	}
	for i, e := range b.Effects {
		if r := fixEffectShape(i, e); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func fixEffectShape(i int, e Effect) *Refusal {
	if fixKindFields[e.Kind] == nil { // shape:kind
		return refuse(RuleFixShape, "kind", i, "", "kind %q does not belong in a fix batch", e.Kind)
	}
	// Above all no stray write field: a scope_request's paths are citations
	// the denylist deliberately does not check, so a `path` must not ride on it.
	if f := foreignField(e); f != "" { // shape:foreign-field
		return refuse(RuleFixShape, "foreign-field", i, "", "a %s effect carries another kind's field %s", e.Kind, f)
	}
	switch e.Kind {
	case KindEdit:
		return editForm(i, e)
	case KindFixMeta:
		if strings.TrimSpace(e.Reason) == "" { // shape:meta-reason-empty
			return refuse(RuleFixShape, "reason-empty", i, "", "fix_meta has no reason")
		}
		if n := utf8.RuneCountInString(e.Reason); n > MaxFixReasonChars { // shape:meta-reason-cap
			return refuse(RuleFixShape, "reason-cap", i, "", "reason is %d chars, max %d", n, MaxFixReasonChars)
		}
	case KindScopeRequest:
		if n := len(e.Paths); n > MaxScopeRequestPaths { // shape:paths-count
			return refuse(RuleFixShape, "paths-count", i, "", "scope_request names %d paths, max %d", n, MaxScopeRequestPaths)
		}
		for _, p := range e.Paths {
			if !cleanRelativePath(p) { // shape:paths-form
				return refuse(RuleFixShape, "paths-form", i, "", "scope_request path %q is not a clean repository-relative path", p)
			}
		}
		if strings.TrimSpace(e.Reason) == "" { // shape:reason-empty
			return refuse(RuleFixShape, "reason-empty", i, "", "scope_request has no reason")
		}
		if n := utf8.RuneCountInString(e.Reason); n > MaxScopeRequestReasonChars { // shape:reason-cap
			return refuse(RuleFixShape, "reason-cap", i, "", "reason is %d chars, max %d", n, MaxScopeRequestReasonChars)
		}
	}
	return nil
}

// editForm is SC-FORM for one edit: what can be said about it without the file.
//
// The path must ALREADY be clean -- non-empty, relative, no `..`, no `.` or
// empty segment, no trailing slash, no NUL -- and is refused, not repaired, so
// the path checked is the path cited, classified, read and committed. (066's
// other SC-FORM cases -- binary hunk, symlink, mode change, rename, gitlink --
// are not expressible by an edit at all.)
func editForm(i int, e Effect) *Refusal {
	if !cleanRelativePath(e.Path) { // form:path
		return refuse(RuleForm, "path", i, e.Path, "path %q is not a clean repository-relative path", e.Path)
	}
	if e.Old == "" { // form:old-empty
		return refuse(RuleForm, "old-empty", i, e.Path, "`old` is empty; an edit replaces existing text (a new file cannot be created)")
	}
	if e.Old == e.New { // form:identical
		return refuse(RuleForm, "identical", i, e.Path, "`old` and `new` are identical")
	}
	if strings.ContainsRune(e.Old, 0) || strings.ContainsRune(e.New, 0) { // form:nul
		return refuse(RuleForm, "nul", i, e.Path, "`old` or `new` contains a NUL byte")
	}
	if !utf8.ValidString(e.Old) || !utf8.ValidString(e.New) { // form:utf8
		return refuse(RuleForm, "utf8", i, e.Path, "`old` or `new` is not valid UTF-8")
	}
	return nil
}
