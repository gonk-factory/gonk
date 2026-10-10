// Package cidef computes EFFECTIVE GitLab CI job definitions from the merged
// CI YAML and the base-vs-head delta between two of them (gonk-2usw plan
// section 6.2, "definition integrity", J2).
//
// It is pure: bytes and structs in, structs out. No network, no GitLab client,
// no clock.
//
// THE ONE RULE EVERYTHING HERE SERVES: what cannot be resolved is reported as
// UNRESOLVABLE, never as equal. A `!reference` tag, an include GitLab did not
// expand, a child pipeline, an `extends` that names nothing, a key this
// package has no rule for -- each of them is a statement that the comparison
// could not be made, and the exit check then answers `unverifiable` (ours,
// never the agent's fault). Even two byte-identical documents are NOT "equal"
// when either holds such a value: section 6.2's blob-SHA shortcut (SC-CIDEF)
// is what answers `none` for an untouched definition, and it never calls this
// package.
//
// WHAT THE INPUT REALLY IS (observed on GitLab 19.4.1, 2026-10-07; the plan's
// assumption G4 was REFUTED, and testdata/gitlab_merged holds the replies):
// the `merged_yaml` of the STATIC ci/lint at a commit. In it `include:` is
// expanded and the key is gone; `!reference` is resolved and no tag is left;
// anchors and merge keys are resolved; `extends` is resolved AND the key is
// kept (resolving it again here gives the same thing); `default:` is kept (it
// is applied here); and `stages` has GAINED the implicit `.pre` and `.post`
// (ImplicitStages). So two of the branches named above -- the `!reference` tag
// and the leftover `include:` -- DO NOT FIRE on GitLab-merged YAML, and a
// project that uses `!reference` is compared like any other. They are kept,
// fail-closed, for input that is not GitLab-merged YAML (a raw CI file, a
// reply cut short, a GitLab that stops resolving one of them): handed such a
// document this package still says "unresolvable", never "equal". Whether an
// include was pinned to the commit is NOT visible here; the reader decides it
// from the lint reply's `includes` (cmd/gonk-gate withUnpinnedIncludes).
//
// Values are compared STRUCTURALLY and are not interpreted. `script: make` and
// `script: [make]` differ; an absent `allow_failure` and `allow_failure:
// false` differ. That is section 6.4 C4's "any change refused, not
// interpreted". Only two defaults are normalised, because both are fixed
// GitLab behaviour and an existence rule depends on them: a job without
// `stage` is in stage `test`, and a document without `stages` has
// DefaultStages.
package cidef

// Class is the key group of section 6.2's table.
type Class string

const (
	// ClassExistence: the job's name and its `stage` (D1).
	ClassExistence Class = "existence"
	// ClassGating: must be EQUAL at head for every base job (D2). No lift
	// changes this.
	ClassGating Class = "gating"
	// ClassExec: equal -> `preserving`; different -> `exec-changing`, which is
	// permitted only on a diagnosed job (D3).
	ClassExec Class = "exec"
	// ClassFree: may change under the `ci` lift.
	ClassFree Class = "free"
	// ClassUnclassified: a key section 6.2's table does not name. A CHANGE to
	// one is ambiguous and is reported as unresolvable -- never as free.
	ClassUnclassified Class = "unclassified"
)

// The key lists. THEY ARE SECTION 6.2's TABLE, and TestKeyListsMatchThePlan
// reads that table out of the plan file and fails when the two differ, in
// either direction. Change the plan and this file together.

// KeyStage is the one job key in the existence group.
const KeyStage = "stage"

// GatingJobKeys are the job keys of the GATING row.
var GatingJobKeys = []string{
	"rules", "only", "except", "when", "allow_failure", "retry", "start_in", "trigger", "parallel",
}

// GatingGlobalKeys are the document-level keys of the GATING row.
// `default.retry` is the `retry` key of the top-level `default:` block.
var GatingGlobalKeys = []string{"workflow", "stages", "default.retry"}

// ExecJobKeys are the job keys of the EXEC row.
var ExecJobKeys = []string{
	"script", "before_script", "after_script", "image", "services", "variables",
	"hooks", "id_tokens", "secrets", "inherit",
}

// ExecGlobalKeys are the document-level keys of the EXEC row ("`variables`
// (job and global)").
var ExecGlobalKeys = []string{"variables"}

// FreeJobKeys are the job keys of the free row. New jobs and new stages
// APPENDED are free too; they are not keys and are reported separately
// (Delta.Added, GlobalChange "stages").
var FreeJobKeys = []string{
	"needs", "dependencies", "artifacts", "cache", "tags", "timeout", "interruptible",
	"coverage", "environment", "resource_group",
}

// keyExtends is resolved into the effective definition and then dropped: what
// a job inherits is compared key by key, the spelling of the inheritance is
// not.
const keyExtends = "extends"

// Global key names as they appear in GlobalChange, Violation and Unresolvable.
const (
	GlobalWorkflow     = "workflow"
	GlobalStages       = "stages"
	GlobalDefaultRetry = "default.retry"
	GlobalVariables    = "variables"
)

// DefaultStages is what GitLab uses when the document has no `stages:`.
var DefaultStages = []string{"build", "test", "deploy"}

// StagePre and StagePost are GitLab's two IMPLICIT stages: every pipeline has
// them, `.pre` before every other stage and `.post` after, whether or not
// `stages:` names them and wherever in the list it names them.
const (
	StagePre  = ".pre"
	StagePost = ".post"
)

// ImplicitStages are not stages OF THE LIST: GitLab's merged YAML always
// carries them (`.pre` first, `.post` last -- observed, project 75, GitLab
// 19.4.1), a hand-written document usually does not, and neither spelling
// changes the pipeline. They are taken out of both sides before the base
// stage order is compared (gonk-er0o: with them left in, a stage appended
// after the project's last one sits BEFORE the trailing `.post`, and the free
// change read as a D2 violation). A job's own `stage` is compared as written:
// moving a job into `.pre` or `.post` is a stage change like any other (D1).
// TestImplicitStagesMatchThePlan pins this list to section 6.2's paragraph.
var ImplicitStages = []string{StagePre, StagePost}

// DefaultStage is the stage of a job that names none.
const DefaultStage = "test"

var jobKeyClass = func() map[string]Class {
	m := map[string]Class{KeyStage: ClassExistence}
	for _, k := range GatingJobKeys {
		m[k] = ClassGating
	}
	for _, k := range ExecJobKeys {
		m[k] = ClassExec
	}
	for _, k := range FreeJobKeys {
		m[k] = ClassFree
	}
	return m
}()

// Classify returns the section 6.2 group of a JOB key, ClassUnclassified for a
// key the table does not name.
func Classify(jobKey string) Class {
	if c, ok := jobKeyClass[jobKey]; ok {
		return c
	}
	return ClassUnclassified
}
