// Package scope is gonk-066's workflow scope: the deterministic answer to
// "which paths may this dispatch's change touch". It classifies a repository
// path, folds the pack / operator / project policy layers into one effective
// scope that is never wider than the pack row, and mints the per-dispatch lift
// of a protected class.
//
// Everything here is a total function of its arguments. No model is asked, and
// a PASS grants nothing (066 section 3.4, invariant 1).
//
// WHERE POLICY COMES FROM IS THE SECURITY PROPERTY (invariant 3). This package
// reads exactly one file, the baked pack's effect-shape.toml (pack.go). The
// operator and project layers arrive as already-parsed values from pkg/opercfg
// and pkg/gonkcfg; the base facts arrive as data the CONTROLLER read at a
// pinned commit. It imports no other gonk package -- in particular not
// pkg/effects -- so it cannot see a batch, a session or `.agent/`, and a test
// (contract_test.go) keeps it that way.
//
// Plans: docs/plans/2026-10-03-gonk-066-workflow-scope.md (slices S1, S3) and
// docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md (part SCOPE). Reference:
// docs/reference/gonk-scope.md.
package scope

import (
	"fmt"
	"path"
	"strings"
)

// Class is the one class every repository path has.
type Class string

// The classes, in the order they are matched: the first class whose patterns
// match a path is that path's class.
//
// DEVIATION FROM 066 TABLE 2.1, DELIBERATE: `secret` is matched BEFORE `ci`
// (the table has them the other way round). The table predates the owner's
// decision that a dispatch may lift `ci` (066 section 9, items a/b). With `ci`
// first, `.gitlab/deploy.key` would be class `ci`, and a CI-lifted dispatch
// could write a key file. The floor must outrank the one liftable class.
const (
	ClassControl      Class = "control"       // .git/, CODEOWNERS, .gonk.yml, and every layer's protected_paths
	ClassSecret       Class = "secret"        // key material and env files
	ClassCI           Class = "ci"            // pipeline definitions
	ClassDeploy       Class = "deploy"        // charts, manifests, Dockerfiles
	ClassTestConfig   Class = "test-config"   // what decides which tests run and how (every profile's, always)
	ClassDeps         Class = "deps"          // dependency manifests and lock files (every profile's, always)
	ClassAgentContext Class = "agent-context" // .agent/ (scaffold only)
	ClassGenerated    Class = "generated"     // "Code generated ... DO NOT EDIT." at the base, and every profile's generated names
	ClassTest         Class = "test"
	ClassDocs         Class = "docs"
	ClassSource       Class = "source"
	ClassOther        Class = "other" // anything unmatched
)

var classOrder = []Class{
	ClassControl, ClassSecret, ClassCI, ClassDeploy, ClassTestConfig, ClassDeps,
	ClassAgentContext, ClassGenerated, ClassTest, ClassDocs, ClassSource, ClassOther,
}

// Classes returns every class, in matching order.
func Classes() []Class { return append([]Class(nil), classOrder...) }

// Valid reports whether c is one of the classes.
func (c Class) Valid() bool { return classRank(c) >= 0 }

func classRank(c Class) int {
	for i, k := range classOrder {
		if k == c {
			return i
		}
	}
	return -1
}

// Sensitivity groups the classes (066 table 2.1).
type Sensitivity string

const (
	// Protected: refused for every dispatch. `ci` alone can be lifted, per
	// dispatch, by the controller (Lift); `control` and `secret` are the floor.
	Protected Sensitivity = "protected"
	// Sensitive: writable only by a workflow whose pack row lists the class.
	Sensitive Sensitivity = "sensitive"
	// Normal: the classes an ordinary code-writing workflow is given.
	Normal Sensitivity = "normal"
)

// Sensitivity returns the class's group. An unknown class is Protected.
func (c Class) Sensitivity() Sensitivity {
	switch c {
	case ClassTest, ClassDocs, ClassSource:
		return Normal
	case ClassDeploy, ClassTestConfig, ClassDeps, ClassAgentContext, ClassGenerated, ClassOther:
		return Sensitive
	}
	return Protected
}

// Liftable reports whether a dispatch may be granted a lift of c. Only `ci`:
// the owner's example is a pipeline-fix session permitted to change a pipeline
// definition (066 section 9, a/b).
func Liftable(c Class) bool { return c == ClassCI }

// Floor returns the protected classes NOTHING lifts: not a dispatch, not a
// pack row, not an operator or project key (gonk-2usw plan 4.2, last row).
func Floor() []Class { return []Class{ClassControl, ClassSecret} }

// core holds the rows of the built-in pattern table that no ecosystem profile
// owns: the protected classes, deploy, and agent-context. The rest of the
// table -- test-config, deps, generated, test, docs, source -- comes from the
// shipped profiles (profile.go), and buildTable puts the two together in class
// order. `generated` and the base .gitlab-ci.yml's local includes are also
// facts, not only patterns: see BaseFacts.
//
// THE `control` AND `ci` ROWS MUST AGREE WITH pkg/effects/protected.go, the
// floor every existing agent is held to. pkg/effects has the test that says so
// (TestFloorAgreesWithScopeClassifier).
var core = []struct {
	class    Class
	patterns []string
}{
	{ClassControl, []string{".git/**", "**/CODEOWNERS", "**/.gonk.yml"}},
	{ClassSecret, []string{"**/*.pem", "**/*.key", "**/.env*", "**/id_rsa*", "**/*.p12", "**/secrets/**"}},
	{ClassCI, []string{".gitlab-ci.yml", ".gitlab-ci.yaml", ".gitlab/**", ".github/**"}},
	{ClassDeploy, []string{"chart/**", "charts/**", "deploy/**", "k8s/**", "**/Dockerfile*", "**/kustomization.yaml"}},
	{ClassAgentContext, []string{".agent/**"}},
}

// cleanRel reports whether p is a clean, relative, in-repository path.
func cleanRel(p string) bool {
	if p == "" || p == "." || strings.ContainsRune(p, 0) || path.IsAbs(p) {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	return p != ".." && !strings.HasPrefix(p, "../")
}

// Builtin classifies p by the built-in patterns alone, under the DEFAULT
// profile selection (go, docs-md): no layer, no base facts. A project that
// selected other profiles is classified by Effective.Classifier.
//
// A path that is not a clean relative path (absolute, `..`, a doubled or
// trailing slash, NUL, empty) has no honest class, so it gets the one class
// nothing can write. SC-FORM rejects such a path with a better message; this is
// the answer when something forgot to ask.
func Builtin(p string) Class {
	c, _ := defaultTable.classify(p, BaseFacts{})
	return c
}

// included reports whether p is one of the base pipeline definition's local
// includes. An include is compared LITERALLY as well as as a pattern: a file
// really named `ci/[x].yml` is not a pattern this package accepts, and a fact
// that cannot be matched as one must not quietly stop being CI.
func included(includes []string, p string) bool {
	for _, inc := range includes {
		if inc == p || Match(inc, p) {
			return true
		}
	}
	return false
}

// Layer names who wrote a policy value.
type Layer string

const (
	LayerOperator Layer = "operator" // opercfg instance / group policy
	LayerProject  Layer = "project"  // the project's .gonk.yml
)

// unlocked are the only classes a layer may move a path OUT of. Everything
// else -- control, secret, ci, deploy, test-config, deps, agent-context,
// generated -- is locked: a path there leaves only toward `control`.
func unlocked(c Class) bool {
	return c == ClassSource || c == ClassOther || c == ClassTest || c == ClassDocs
}

// CheckMove is the reclassification lattice (066 section 3.2; owner decision
// Q3, "downward only, everything else operator-only"). It returns nil when the
// named layer may move a path from one class to another.
//
//	anything          -> control   always (protected_paths: strictly tightening)
//	source/other/test/docs -> ci   (ci_paths) -- but never out of a locked class:
//	                               ci is the one protected class a dispatch can
//	                               lift, so moving a deploy or secret path INTO
//	                               it would be a widening wearing a tightening
//	source/other      -> test/docs (test_paths, docs_paths) -- the Q3 downward move
//	other             -> source    operator only (source_paths)
//	everything else   refused, including deploy -> docs
func CheckMove(from, to Class, layer Layer) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("scope: lattice: unknown class in %s -> %s", from, to)
	}
	if from == to || to == ClassControl {
		return nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("scope: lattice: the %s layer may not reclassify %s -> %s: %s", layer, from, to, why)
	}
	if !unlocked(from) {
		return refuse(fmt.Sprintf("%s is locked; a path leaves it only toward control", from))
	}
	switch to {
	case ClassCI:
		return nil
	case ClassTest, ClassDocs:
		if from == ClassSource || from == ClassOther {
			return nil
		}
		return refuse("only source and other may become test or docs")
	case ClassSource:
		if from != ClassOther {
			return refuse("only other may become source")
		}
		if layer != LayerOperator {
			return refuse("declaring source paths is operator-only")
		}
		return nil
	}
	return refuse("no configuration key reclassifies into " + string(to))
}
