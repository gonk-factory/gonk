package main

// THE SCOPE ENVELOPE OF ONE PIPELINE FIX (gonk-2usw plan 4.2).
//
// The diagnosis class SELECTS which pre-authorised envelope applies; it can
// never obtain one the project did not opt in to (owner decision Q2: the
// class is a veto and a selector, always intersected with the cited files and
// the project's configuration). The table:
//
//	class          writable path classes   lift granted by the dispatch
//	code           source, docs            none
//	test           test                    none
//	configuration  (none)                  ci -- ONLY when the fold says ci is
//	                                       liftable: the pack row lists it, the
//	                                       project set pipeline_fix.allow_ci,
//	                                       and no operator layer vetoes it
//	any            --                      never control or secret (the floor)
//
// Every row is applied INSIDE the fold (pack row AND operator AND project):
// scope.Effective.Narrow can only remove classes, and scope.Effective.GrantLift
// is the only way to hold a lift. Nothing here reads a batch, a session's
// answer or an order var; the class comes from the record of a diagnosis the
// sweep already validated, and it can only NARROW.

import (
	"fmt"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// pipefixClassRow is one row of plan 4.2's table.
type pipefixClassRow struct {
	// writable are the ordinary path classes the row may write.
	writable []scope.Class
	// lift are the protected classes the dispatch ASKS to have lifted. Asking
	// is not getting: scope.Effective.GrantLift decides.
	lift []scope.Class
}

// pipefixClassRows is plan 4.2, keyed by the diagnosis class. A class with no
// row has no envelope at all -- which is every class outside
// pipefix.FixableClasses (TestEveryFixableClassHasAnEnvelopeRow).
var pipefixClassRows = map[string]pipefixClassRow{
	pipefix.ClassCode:          {writable: []scope.Class{scope.ClassSource, scope.ClassDocs}},
	pipefix.ClassTest:          {writable: []scope.Class{scope.ClassTest}},
	pipefix.ClassConfiguration: {lift: []scope.Class{scope.ClassCI}},
}

// pipefixEnvelope is the envelope chosen for one dispatch.
type pipefixEnvelope struct {
	// scope is the fold narrowed to the class row (and to pipeline_fix.paths).
	scope scope.Effective
	// lift is the dispatch's lift: empty unless the row asks for ci AND the
	// fold grants it.
	lift scope.Lift
	// classes is every path class the dispatch may write, in class order:
	// scope.Classes plus the lifted ones. It is what E6 compares the cited
	// files' classes against and what the fix record keeps.
	classes []string
}

// allows reports whether a path of class c is inside the envelope.
func (e pipefixEnvelope) allows(c scope.Class) bool { return e.scope.Allows(c, e.lift) }

// liftNames is the lift as the record keeps it.
func (e pipefixEnvelope) liftNames() []string {
	var out []string
	for _, c := range e.lift.Classes() {
		out = append(out, string(c))
	}
	return out
}

// envelopeFor applies plan 4.2's row for a diagnosis class to a folded scope.
//
// A class with no row is an error (the caller never asks: E4 refuses such a
// class first). A lift the fold will not grant is NOT an error: the row then
// simply has no lifted class, the cited CI file is outside the envelope, and
// E6 answers needs-scope -- which is exactly what a `configuration` diagnosis
// on a project without `allow_ci` should get.
func envelopeFor(eff scope.Effective, class string) (pipefixEnvelope, error) {
	row, ok := pipefixClassRows[class]
	if !ok {
		return pipefixEnvelope{}, fmt.Errorf("pipeline fix: diagnosis class %q has no scope envelope", class)
	}
	env := pipefixEnvelope{scope: eff.Narrow(row.writable...)}
	if len(row.lift) > 0 {
		grantable := map[scope.Class]bool{}
		for _, c := range eff.Liftable() {
			grantable[c] = true
		}
		var ask []scope.Class
		for _, c := range row.lift {
			if grantable[c] {
				ask = append(ask, c)
			}
		}
		if len(ask) > 0 {
			// The lift is minted HERE, by the controller, for this dispatch
			// (066 section 9 a/b), and named for the log.
			lift, err := eff.GrantLift(scope.Grant{Trigger: atags.TriggerPipelineFix, Workflow: class, Classes: ask})
			if err != nil {
				return pipefixEnvelope{}, fmt.Errorf("pipeline fix: lift for a %s fix: %w", class, err)
			}
			env.lift = lift
		}
	}
	for _, c := range scope.Classes() {
		if env.allows(c) {
			env.classes = append(env.classes, string(c))
		}
	}
	return env, nil
}
