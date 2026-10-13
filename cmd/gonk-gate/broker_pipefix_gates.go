package main

// THE STATIC GATES OF A FIX BATCH THAT pkg/effects DOES NOT HOLD (gonk-2usw
// plan 5.3; part APPLY). pkg/effects.PlanFix is gates A1 and A4-A6 over the
// batch, the envelope and the base blobs. What is here needs something PlanFix
// is deliberately not given:
//
//	gate   rule code            needs
//	A2     FX-TARGET            (pkg/effects.ValidateTargets)
//	A4     SC-SHOWN(<rule>)     the recorded fix context: the files the
//	                            session was TOLD it may change
//	A5     SC-FORM(<reason>)    the tree entry's MODE at the base commit
//	                            (pkg/basefacts): a symlink's blob is editable
//	                            text, and an edit to it retargets the link
//	A6     SC-WEAKEN(<sub>)     a Go parser (pkg/weaken), and pkg/scope's
//	                            word on whether a predicate exists at all
//	A7     CI-PRE(<sub>)        a YAML parser; the added lines of a ci file
//	A8     TX-MARKER,           the forged-marker pattern, pipelinectx.Redact
//	       TX-SECRET(<kinds>),  and the comment gate, over the agent's prose
//	       TX-COMMENT
//	5.4/2  CI-DEF(<sub>)        the merged CI definition at the base and at
//	                            the commit (pkg/cidef); AFTER the commit
//
// Every one returns a *effects.Refusal: the agent's failure (gate-failed),
// reported by its Code(). A failure of OURS -- a read, a fold, a record that
// no longer hashes -- is never a Refusal.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gonk-factory/gonk/pkg/basefacts"
	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/scope"
	"github.com/gonk-factory/gonk/pkg/weaken"
)

// Rule codes of the gates in this file. With pkg/effects.Rules() they are the
// closed set a fix refusal can carry; docs/reference/effects-contract.md
// lists them, and TestEveryFixRefusalCodeIsDocumented reads that file.
const (
	ruleFixPackShape effects.Rule = "FX-PACK"    // A1: the pack's effect shape for the pipefix agent
	ruleFixTarget    effects.Rule = "FX-TARGET"  // A2: an effect names a target
	ruleFixTraject   effects.Rule = "FX-TRAJ"    // the trajectory floor (enforced only when enforcement is on)
	ruleShown        effects.Rule = "SC-SHOWN"   // A4: not a file the session was told it may change
	ruleWeaken       effects.Rule = "SC-WEAKEN"  // A6: a Go test removed, skipped or built out
	ruleCIPre        effects.Rule = "CI-PRE"     // A7: the CI pre-check
	ruleTextMarker   effects.Rule = "TX-MARKER"  // A8: agent text reads as a gonk marker
	ruleTextSecret   effects.Rule = "TX-SECRET"  // A8: agent text holds what Redact finds
	ruleTextComment  effects.Rule = "TX-COMMENT" // A8: agent text fails the comment gate
	ruleCIDef        effects.Rule = "CI-DEF"     // 5.4 step 2: definition integrity, after the commit
)

// pipefixApplyRules is every rule code this file can refuse with, for the
// documentation test.
func pipefixApplyRules() []effects.Rule {
	return []effects.Rule{ruleFixPackShape, ruleFixTarget, ruleFixTraject, ruleShown, ruleWeaken, ruleCIPre,
		ruleTextMarker, ruleTextSecret, ruleTextComment, ruleCIDef}
}

func fixRefusal(rule effects.Rule, sub, path, format string, args ...any) *effects.Refusal {
	return &effects.Refusal{Rule: rule, Sub: sub, Effect: -1, Path: path, Detail: fmt.Sprintf(format, args...)}
}

// ---- A4: the files the session was shown -------------------------------------

// shownRefusal refuses an edit to a path the prompt did not list under "Files
// you may change". The recorded context says, per cited file, whether it was
// editable and by which rule not; a path the context does not hold at all was
// never cited. This runs BEFORE any blob is read, so a batch aimed at a file
// the session was told to leave alone costs no read and can never reach the
// blob reader with a path gonk did not choose.
func shownRefusal(batch effects.Batch, fc beadstore.FixContext) *effects.Refusal {
	shown := map[string]beadstore.FixContextFile{}
	for _, f := range fc.Files {
		shown[f.Path] = f
	}
	for _, p := range effects.EditPaths(batch) {
		f, cited := shown[p]
		switch {
		case !cited:
			return fixRefusal(ruleShown, string(effects.RuleCited), p, "%q is not a file this fix was given", p)
		case !f.Editable: // shown:editable
			return fixRefusal(ruleShown, f.Rule, p, "the session was told it may not change %q (%s)", p, f.Rule)
		}
	}
	return nil
}

// ---- A5: only regular files ----------------------------------------------------

// modeRefusal turns the base reader's verdict on an edit path that is not a
// regular file into the agent's refusal: SC-FORM with the reason as its sub
// code (symlink, submodule, directory, parent-not-a-dir, not-a-regular-file).
func modeRefusal(refusals []basefacts.Refusal) *effects.Refusal {
	if len(refusals) == 0 {
		return nil
	}
	r := refusals[0]
	return fixRefusal(effects.RuleForm, string(r.Reason), r.Path,
		"%q is not a regular file at the base commit (%s, mode %s); a fix may only edit regular files", r.Path, r.Reason, r.Entry.Mode)
}

// ---- A6: SC-WEAKEN --------------------------------------------------------------

// weakeningFact is what the gate found out about test weakening.
type weakeningFact struct {
	// undecided are the changed paths for which NO deterministic predicate
	// exists: their tests may have been weakened and nothing here can tell.
	undecided []string
}

// state is the value recorded as PipelineFix.Weakening.
func (w weakeningFact) state() string {
	if len(w.undecided) > 0 {
		return beadstore.FixWeakeningNotDecidable
	}
	return beadstore.FixWeakeningDecided
}

// weakeningGate is SC-WEAKEN over an accepted plan.
//
// For every changed file it asks pkg/scope whether a deterministic predicate
// exists for that path (Decision.WeakeningDecidable: a source or test path is
// decidable only when the `go` profile classified it). Where one exists and
// the file is Go, pkg/weaken judges the change:
//
//   - a TEST file: any finding refuses (a removed test, an added skip, a
//     changed build constraint, a result that does not parse);
//   - a SOURCE file: a changed build constraint refuses -- a tag takes the
//     file out of the build -- and nothing else is asked of it.
//
// Where NO predicate exists the path is recorded as undecided and the change
// is NOT refused: refusing every python test edit would be pretending to
// have decided. A file of a predicate profile that is not a .go file (a
// golden file under testdata/) is undecided too: the predicate reads Go.
func weakeningGate(plan effects.FixPlan, base effects.BaseBlobs, cls *scope.Classifier) (weakeningFact, *effects.Refusal) {
	var w weakeningFact
	for _, f := range plan.Files {
		dec := cls.Classify(f.Path)
		if dec.Class != scope.ClassSource && dec.Class != scope.ClassTest {
			continue // no tests to weaken: decidable, and nothing to check
		}
		isGo := strings.HasSuffix(f.Path, ".go")
		if !dec.WeakeningDecidable() || !isGo { // weaken:decidable
			w.undecided = append(w.undecided, f.Path)
			continue
		}
		finding := weaken.Go(base[f.Path], f.Content)
		if !finding.Found() {
			continue
		}
		if dec.Class == scope.ClassTest || finding.Sub == weaken.BuildConstraint { // weaken:refuse
			return weakeningFact{}, fixRefusal(ruleWeaken, finding.Sub, f.Path, "%s", finding.Detail)
		}
	}
	sort.Strings(w.undecided)
	return w, nil
}

// ---- A7: the CI pre-check --------------------------------------------------------

// ciPreChecks are the cheap syntactic refusals of plan 5.3 A7, over the lines
// a change ADDS to a ci-class file. A COURTESY early refusal: each is one
// obvious spelling of "let the job fail without failing the pipeline" or
// "stop the job from running". The authoritative check is the definition
// comparison after the commit (CI-DEF) and the exit check's own.
//
// or-true reads `||` followed by one of true, /bin/true, /usr/bin/true, the
// null command `:` or `exit 0`, and then by the END OF THAT WORD: the end of
// the line, or any character a command name does not continue with (a space,
// `;`, `)`, a closing quote, `#`, `&`, ...). So `|| : ;` and `|| : # why` are
// refused (gonk-z7gr), and a fallback command that merely starts like one of
// them (`|| true-up.sh`, `|| :retry`) is not: it still fails the job.
var ciPreChecks = []struct {
	sub string
	re  *regexp.Regexp
}{
	{"allow_failure", regexp.MustCompile(`(?i)\ballow_failure\b`)},
	{"when", regexp.MustCompile(`(?i)\bwhen\s*:\s*["']?(?:manual|never)\b`)},
	{"retry", regexp.MustCompile(`(?i)^\s*(?:-\s*)?["']?retry["']?\s*:`)},
	{"or-true", regexp.MustCompile(`\|\|\s*(?:true|(?:/usr)?/bin/true|:|exit\s+0)(?:[^\w./-]|$)`)},
	{"set-e", regexp.MustCompile(`\bset\s+(?:\+e\b|\+o\s+errexit\b)`)},
	{"exit-0", regexp.MustCompile(`\bexit\s+0\b`)},
}

// ciPreCheck is gate A7 over an accepted plan: for every changed file of
// class ci, the patched content must parse as YAML, and no added line may
// match a ciPreChecks pattern.
func ciPreCheck(plan effects.FixPlan) *effects.Refusal {
	patched := map[string][]byte{}
	for _, f := range plan.Files {
		patched[f.Path] = f.Content
	}
	for _, fd := range plan.Facts.Files {
		if fd.Class != scope.ClassCI { // cipre:class
			continue
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(patched[fd.Path], &doc); err != nil { // cipre:parse
			return fixRefusal(ruleCIPre, "unparseable", fd.Path, "%q does not parse as YAML after the change", fd.Path)
		}
		for _, line := range fd.AddedLines {
			for _, c := range ciPreChecks {
				if c.re.MatchString(line) { // cipre:line
					return fixRefusal(ruleCIPre, c.sub, fd.Path,
						"an added line of %q would let a job fail without failing the pipeline, or stop it running (%s)", fd.Path, c.sub)
				}
			}
		}
	}
	return nil
}

// changesCI reports whether the plan changes any ci-class file.
func changesCI(plan effects.FixPlan) bool {
	for _, fd := range plan.Facts.Files {
		if fd.Class == scope.ClassCI {
			return true
		}
	}
	return false
}

// ---- A8: the agent's prose -------------------------------------------------------

// textGate is plan 5.3 A8 over ONE piece of agent-written text that gonk is
// about to render (a fix_meta's reason; a scope_request's reason or path):
// it may not read as a gonk marker, pipelinectx.Redact must find nothing in
// it, and the comment gate must accept it. Refused, never repaired -- a
// reason with a token in it is not rendered with a hole.
func textGate(what, text string) *effects.Refusal {
	if gonkMarkerRe.MatchString(text) { // text:marker
		return fixRefusal(ruleTextMarker, "", "", "the %s carries text that reads as a gonk marker", what)
	}
	if _, kinds := pipelinectx.Redact(text); len(kinds) > 0 { // text:secret
		return fixRefusal(ruleTextSecret, strings.Join(kinds, ","), "", "the %s holds what looks like a secret", what)
	}
	// The comment gate reads the text with the line endings it is RENDERED
	// with (fence turns a lone CR into a line end, as CommonMark does), so a
	// quick action after a carriage return is a line here too. This gate, not
	// the fence the text is shown in, is what stops a quick action
	// (gonk-2061).
	if err := effects.ValidateComments(effects.Batch{Effects: []effects.Effect{{Kind: effects.KindComment, Body: untrustedLines(text)}}}); err != nil { // text:comment
		return fixRefusal(ruleTextComment, "", "", "the %s: %v", what, err)
	}
	return nil
}

// fixTextGates runs textGate over everything of the plan that will be
// rendered: the reason of a fix; the reason and every path of a scope
// request.
func fixTextGates(plan effects.FixPlan) *effects.Refusal {
	if plan.Verdict == effects.VerdictFix {
		return textGate("fix_meta reason", plan.Reason)
	}
	if plan.Request == nil {
		return nil
	}
	if r := textGate("scope request's reason", plan.Request.Reason); r != nil {
		return r
	}
	for _, p := range plan.Request.Paths {
		if r := textGate("scope request's path", p); r != nil {
			return r
		}
	}
	return nil
}
