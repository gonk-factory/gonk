package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// pfBaseSHA is declared in pipefix_facts_test.go (the same value).

// pfRow is the pack row part AGENT is expected to ship for the pipefix agent
// (plan 4.2 and 4.3): every class any row of the table writes, ci liftable,
// the size ceilings.
var pfRow = scope.Row{
	Classes:  []scope.Class{scope.ClassSource, scope.ClassTest, scope.ClassDocs},
	Liftable: []scope.Class{scope.ClassCI},
	Caps:     scope.Caps{MaxFiles: 3, MaxLines: 40, MaxDeletes: 0, MaxFileBytes: 256 << 10, MaxPatchBytes: 16 << 10},
}

func pfFold(t *testing.T, in scope.Input) scope.Effective {
	t.Helper()
	if in.Row.Classes == nil && in.Row.Liftable == nil {
		in.Row = pfRow
	}
	in.BaseSHA, in.Enabled = pfBaseSHA, true
	eff, err := scope.Fold(in)
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

var pfCI = []scope.Class{scope.ClassCI}

// PLAN 4.2, the table, as built: for each diagnosis class, exactly which path
// classes are writable, under every combination that matters. Each row asserts
// the WHOLE answer over every class pkg/scope knows, so a class that became
// writable by accident fails the row it leaked into.
func TestEnvelopeTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class string
		in    scope.Input
		want  []string // the writable classes, in class order
		lift  []string
	}{
		{"code: source and docs, nothing lifted", pipefix.ClassCode, scope.Input{}, []string{"docs", "source"}, nil},
		{"test: test only", pipefix.ClassTest, scope.Input{}, []string{"test"}, nil},
		{"configuration without allow_ci: nothing at all", pipefix.ClassConfiguration, scope.Input{}, nil, nil},
		{"configuration with allow_ci: ci, lifted, and nothing else", pipefix.ClassConfiguration,
			scope.Input{LiftOptIn: pfCI}, []string{"ci"}, []string{"ci"}},
		// allow_ci lifts ci for a CONFIGURATION fix only: the class is the
		// selector, so a code or test fix on the same project gets no lift.
		{"code with allow_ci: still no ci", pipefix.ClassCode, scope.Input{LiftOptIn: pfCI}, []string{"docs", "source"}, nil},
		{"test with allow_ci: still no ci", pipefix.ClassTest, scope.Input{LiftOptIn: pfCI}, []string{"test"}, nil},
		// The project and the operator can only narrow the row.
		{"code, project allows only docs", pipefix.ClassCode,
			scope.Input{Project: scope.Policy{Classes: []scope.Class{scope.ClassDocs}}}, []string{"docs"}, nil},
		{"code, operator allows only test: nothing", pipefix.ClassCode,
			scope.Input{Instance: scope.Policy{Classes: []scope.Class{scope.ClassTest}}}, nil, nil},
		{"test, group allows only source: nothing", pipefix.ClassTest,
			scope.Input{Group: scope.Policy{Classes: []scope.Class{scope.ClassSource}}}, nil, nil},
		{"configuration with allow_ci, operator vetoes the lift: nothing", pipefix.ClassConfiguration,
			scope.Input{LiftOptIn: pfCI, Instance: scope.Policy{DenyLift: pfCI}}, nil, nil},
		{"configuration with allow_ci, pack row does not list ci: nothing", pipefix.ClassConfiguration,
			scope.Input{LiftOptIn: pfCI, Row: scope.Row{Classes: pfRow.Classes, Caps: pfRow.Caps}}, nil, nil},
		// The pack row is the maximum: a row that lacks a class cannot have it
		// added by the table.
		{"code, pack row has no source: docs only", pipefix.ClassCode,
			scope.Input{Row: scope.Row{Classes: []scope.Class{scope.ClassDocs, scope.ClassTest}, Caps: pfRow.Caps}}, []string{"docs"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := envelopeFor(pfFold(t, tc.in), tc.class)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(env.classes, tc.want) {
				t.Errorf("writable classes = %v, want %v", env.classes, tc.want)
			}
			if !reflect.DeepEqual(env.liftNames(), tc.lift) {
				t.Errorf("lift = %v, want %v", env.liftNames(), tc.lift)
			}
			want := map[string]bool{}
			for _, c := range tc.want {
				want[c] = true
			}
			for _, c := range scope.Classes() {
				if got := env.allows(c); got != want[string(c)] {
					t.Errorf("allows(%s) = %v, want %v", c, got, want[string(c)])
				}
			}
		})
	}
}

// PLAN 4.2, the "Never, under any config" column and the floor row: for every
// diagnosis class, under the WIDEST configuration there is (the pack row lists
// every non-protected class, ci is liftable and opted in, no layer narrows),
// these classes are still not writable.
func TestEnvelopeNeverColumn(t *testing.T) {
	var everyOrdinary []scope.Class
	for _, c := range scope.Classes() {
		if c.Sensitivity() != scope.Protected {
			everyOrdinary = append(everyOrdinary, c)
		}
	}
	widest := scope.Input{LiftOptIn: pfCI, Row: scope.Row{Classes: everyOrdinary, Liftable: pfCI, Caps: pfRow.Caps}}
	never := map[string][]scope.Class{
		pipefix.ClassCode: {scope.ClassTest, scope.ClassCI, scope.ClassTestConfig, scope.ClassDeps, scope.ClassDeploy,
			scope.ClassAgentContext, scope.ClassGenerated, scope.ClassOther, scope.ClassControl, scope.ClassSecret},
		pipefix.ClassTest: {scope.ClassSource, scope.ClassCI, scope.ClassTestConfig, scope.ClassDocs, scope.ClassDeps,
			scope.ClassDeploy, scope.ClassAgentContext, scope.ClassGenerated, scope.ClassOther, scope.ClassControl, scope.ClassSecret},
		pipefix.ClassConfiguration: {scope.ClassSource, scope.ClassTest, scope.ClassDocs, scope.ClassTestConfig, scope.ClassDeps,
			scope.ClassDeploy, scope.ClassAgentContext, scope.ClassGenerated, scope.ClassOther, scope.ClassControl, scope.ClassSecret},
	}
	for class, classes := range never {
		env, err := envelopeFor(pfFold(t, widest), class)
		if err != nil {
			t.Fatal(err)
		}
		if len(env.classes) == 0 {
			t.Fatalf("%s: the widest configuration allows nothing; the assertions below would be vacuous", class)
		}
		for _, c := range classes {
			if env.allows(c) {
				t.Errorf("a %s fix may write class %s under the widest configuration", class, c)
			}
		}
	}
}

// E4 and the table cannot disagree: every class a fix may ever be dispatched
// for has a row, and no other class does.
func TestEveryFixableClassHasAnEnvelopeRow(t *testing.T) {
	fixable := map[string]bool{}
	for _, c := range pipefix.FixableClasses() {
		fixable[c] = true
		if _, ok := pipefixClassRows[c]; !ok {
			t.Errorf("fixable class %q has no envelope row", c)
		}
	}
	for c := range pipefixClassRows {
		if !fixable[c] {
			t.Errorf("class %q has an envelope row but is not fixable", c)
		}
	}
	for _, c := range []string{"flaky", "infrastructure", "unknown", ""} {
		if _, err := envelopeFor(pfFold(t, scope.Input{LiftOptIn: pfCI}), c); err == nil {
			t.Errorf("class %q got an envelope", c)
		}
	}
}

// The lift is the dispatch's, named for the log, and comes from the project's
// opt-in: the same fold WITHOUT the opt-in grants none.
func TestTheCILiftIsNamedAndComesFromTheProjectOptIn(t *testing.T) {
	env, err := envelopeFor(pfFold(t, scope.Input{LiftOptIn: pfCI}), pipefix.ClassConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if !env.lift.Has(scope.ClassCI) || env.lift.Trigger() != "pipeline-fix" || env.lift.Workflow() != "configuration" {
		t.Fatalf("lift = %s, want ci for trigger pipeline-fix workflow configuration", env.lift)
	}
	if !strings.Contains(env.lift.String(), "pipeline-fix") {
		t.Errorf("the lift's log form %q does not name the dispatch", env.lift)
	}
	none, err := envelopeFor(pfFold(t, scope.Input{}), pipefix.ClassConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if !none.lift.Empty() {
		t.Fatalf("a project that did not set allow_ci got the lift %s", none.lift)
	}
	// A disabled workflow grants nothing whatever was opted in.
	off := pfFold(t, scope.Input{LiftOptIn: pfCI})
	off.Enabled = false
	env, err = envelopeFor(off, pipefix.ClassConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.classes) != 0 || !env.lift.Empty() {
		t.Fatalf("a disabled scope yields the envelope %v with lift %s", env.classes, env.lift)
	}
}
