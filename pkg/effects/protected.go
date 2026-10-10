package effects

// The protected-path denylist.
//
// WHY THIS EXISTS WHEN THE ALLOWLIST ALREADY COVERS IT. Today every file effect
// must sit under `.agent/` (see file.go), which already excludes everything
// here -- so this gate is, right now, redundant. It is here for the agent that
// does not exist yet.
//
// The first agent that legitimately edits source code needs a far wider
// allowlist than one prefix, and on the day someone writes it the incidental
// protection vanishes with no test failing and nothing to notice. A denylist
// refuses these paths ON THEIR OWN ACCOUNT, independently of whatever an
// agent's shape permits, so widening an allowlist cannot silently widen this.
//
// It is also the only DECIDABLE answer to a question the shape gate cannot
// otherwise touch: "did this run delete a CI job to turn a red pipeline green
// instead of fixing it?" That is a judgement about intent, and spec 6.3 forbids
// a model making it -- but "a non-CI agent may not edit CI definitions at all"
// is a fact a validator can check. See gonk-066 for the rest of that argument.

//
// THE DENYLIST IS A DEFAULT, AND ONE CLASS OF IT CAN BE LIFTED PER DISPATCH
// (gonk-066 section 9, owner decisions a/b; gonk-2usw plan 4.2). The owner's
// example: a pipeline-failure analyser may recommend a change to a pipeline
// definition, so the session dispatched to MAKE that change must be able to
// edit one. ValidateProtectedPathsWithLift takes that permission as a
// scope.Lift, and three things about it are the design:
//
//   - The lift is an ARGUMENT, supplied by the controller from the dispatch.
//     It is never read from the batch (a Batch has no field that could carry
//     one -- TestBatchCannotCarryALiftOrScopePolicy), from `.agent/`, or from
//     issue text, and this package cannot mint one: only
//     scope.Effective.GrantLift does.
//   - Only the `ci` entries can be lifted. `.gonk.yml`, CODEOWNERS and `.git/`
//     are the FLOOR: no lift, pack row or config key opens them.
//   - With no lift -- every agent that exists today -- behaviour is what it
//     was before a lift existed, error text included.

import (
	"fmt"
	"path"
	"strings"

	"github.com/gonk-factory/gonk/pkg/scope"
)

// protectedExact are paths refused wherever they appear in the tree, matched on
// the final element. CODEOWNERS is meaningful in several locations, so it is
// matched by basename rather than by full path.
var protectedBase = map[string]bool{
	"CODEOWNERS": true, // changes who must approve the change
	".gonk.yml":  true, // gonk's own ladder, budget and permission to act
}

// protectedPrefixes are directory subtrees refused entirely. Each carries the
// scope class it belongs to: `ci` is the one class a dispatch can be granted a
// lift of; `control` is the floor.
var protectedPrefixes = []struct {
	prefix string
	class  scope.Class
}{
	{".gitlab/", scope.ClassCI},   // CI config, agent config, issue templates
	{".github/", scope.ClassCI},   // the same, for a mirrored repo
	{".git/", scope.ClassControl}, // the repository's own machinery
}

// protectedExactPaths are refused only at these exact paths (a file with the
// same name deeper in the tree is ordinary content).
var protectedExactPaths = map[string]bool{
	".gitlab-ci.yml":  true,
	".gitlab-ci.yaml": true,
}

// effectPaths returns every repository path an effect may cause to be
// WRITTEN, whatever its kind. This is the single enumeration the denylist runs
// over: a path-carrying field must be listed here OR in citationOnlyPathFields,
// and TestEveryPathFieldIsProtected fails (by reflection over Effect) if a new
// Path- or File-named field is added to neither.
//
// It is deliberately NOT keyed on Kind. `path` is the write path of a `file`
// effect AND of an `edit` effect (fix.go), and of whatever kind is added next:
// a kind check here is how a new write kind would walk past the denylist.
func effectPaths(e Effect) []string {
	if e.Path == "" { // paths:any-kind
		return nil
	}
	return []string{e.Path}
}

// citationOnlyPathFields names the Effect fields that carry repository paths
// gonk only CITES -- renders as text -- and never writes, each with the reason.
// They are deliberately NOT in effectPaths: the denylist exists to stop gonk
// WRITING repository control files, and a diagnosis must be able to say "the
// job definition in .gitlab-ci.yml is wrong" (gonk-tios, configuration class).
//
// An entry here is a claim that no apply path turns the field into a write.
// There are exactly two constructors of repository writes, and each reads one
// kind's Path: FileWrites (file.go) reads a `file` effect's Path/Content, and
// ApplyEdits (edit.go) reads an `edit` effect's Path/Old/New.
// TestOnlyFileEffectsBecomeWrites, TestOnlyEditEffectsBecomePatchedFiles and
// TestScaffoldApplyNeverWritesADiagnosis (cmd/gonk-gate) hold that claim. The
// sanity rules for a citation (relative, canonical, no `..`, no NUL) are
// ValidateDiagnosis's and ValidateFix's.
var citationOnlyPathFields = map[string]string{
	"Files": "a diagnosis's files are citations rendered into a note; no apply path writes them (only FileWrites and ApplyEdits make writes, and they read KindFile's and KindEdit's Path)",
	"Paths": "a scope_request's paths name what the session says it WOULD have needed; they are rendered into a needs-scope note and no apply path writes them (gonk-2usw plan 4.4)",
}

// ValidateProtectedPaths refuses effects that touch repository control
// surfaces, regardless of the agent's shape, allowlist or effect KIND: any
// effect naming a path is checked, not only KindFile, so a future patch-style
// kind cannot bypass the denylist. It is a no-op for effects naming no path.
//
// It takes NO lift: this is the gate every existing agent goes through, and it
// is exactly ValidateProtectedPathsWithLift with nothing lifted.
func ValidateProtectedPaths(b Batch) error {
	return ValidateProtectedPathsWithLift(b, scope.Lift{})
}

// ValidateProtectedPathsWithLift is ValidateProtectedPaths for a dispatch the
// CONTROLLER granted a lift (see the file header). A path on the denylist
// passes only if all of these hold:
//
//   - the denylist entry it hit is a `ci` entry, not a floor entry;
//   - the lift covers `ci`;
//   - the path AS WRITTEN is one pkg/scope classifies `ci`. That is stricter
//     than the entry on purpose: scope ranks `secret` above `ci`, so
//     `.gitlab/deploy.key` stays refused, and a path that only CLEANS to a CI
//     definition (`./.gitlab-ci.yml`) is not one a lift was granted for.
//
// A zero Lift lifts nothing.
func ValidateProtectedPathsWithLift(b Batch, lift scope.Lift) error {
	for i, e := range b.Effects {
		if p, what := protectedPath(e, lift); what != "" {
			return protectedErr(i, p, what)
		}
	}
	return nil
}

// protectedPath is the denylist decision for ONE effect: the first of its
// write paths that is protected and not lifted, and the phrase for the entry
// it hit ("" when the effect is clear). ValidateProtectedPathsWithLift and the
// fix gate's SC-PROT (fixgate.go) both decide through it, so there is one
// definition of "protected, unless lifted".
func protectedPath(e Effect, lift scope.Lift) (string, string) {
	for _, p := range effectPaths(e) {
		class, what := floorEntry(p)
		if class == "" {
			continue
		}
		if class == scope.ClassCI && lift.Has(scope.ClassCI) && scope.Builtin(p) == scope.ClassCI {
			continue
		}
		return p, what
	}
	return "", ""
}

// floorEntry reports which denylist entry p hits: its scope class and the
// phrase the refusal uses for it, or "" when p is not on the denylist.
func floorEntry(p string) (scope.Class, string) {
	clean := path.Clean(p)
	if protectedExactPaths[clean] {
		return scope.ClassCI, "a CI definition"
	}
	if protectedBase[path.Base(clean)] {
		return scope.ClassControl, "a repository control file"
	}
	for _, pfx := range protectedPrefixes {
		// Exact-prefix match on the trailing-slash form, so `.github/x` is
		// caught and `src/github/client.go` is not.
		if strings.HasPrefix(clean, pfx.prefix) {
			return pfx.class, "a protected directory"
		}
	}
	return "", ""
}

// floorClass is floorEntry's class alone.
func floorClass(p string) scope.Class {
	c, _ := floorEntry(p)
	return c
}

func protectedErr(i int, p, what string) error {
	return fmt.Errorf("effects: effect %d: %s", i, protectedReason(p, what))
}

func protectedReason(p, what string) string {
	return fmt.Sprintf("path %q is %s and is protected -- no agent may propose changes to it", p, what)
}
