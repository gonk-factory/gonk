package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/gcapi/gcapitest"
)

// THE ORPHAN REAPER (gonk-4d7). gonk-xkm closed the three teardown paths gonk
// controls directly. This covers the one none of them can: a dispatch process
// that DIES between creating the session and reaching any close call -- pod
// evicted, order timeout, controller restarted mid-dispatch. Such a session has
// no bead in StateRunning, so gonk-sweep will never look at it. It is invisible,
// not merely long-lived.
//
// EVERY TEST BELOW IS ABOUT WHAT THE REAPER MUST NOT KILL. That asymmetry is
// deliberate: a leaked pod costs money and eventually wedges the scheduler, but
// a reaper that is too eager kills a live agent mid-turn, destroying work that
// was in progress and burning a ladder attempt for nothing. The leak is the
// cheaper failure, so every ambiguity here resolves toward leaving things alone.

const reapNow = "2026-08-03T12:00:00Z"

// testNonce is a syntactically valid nonce segment -- 26 characters from
// brokerSessionAlias's base32 alphabet, matching the length its 128-bit
// crypto/rand suffix always produces -- for aliases this file hand-builds
// rather than minting through brokerSessionAlias itself.
const testNonce = "AAAAAAAAAAAAAAAAAAAAAAAAAA"

func reapClock(t *testing.T) func() time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, reapNow)
	if err != nil {
		t.Fatalf("bad test clock: %v", err)
	}
	return func() time.Time { return now }
}

func reapDeps(t *testing.T, gc *gcapitest.Server, store beadstore.Store, log *strings.Builder) reaperDeps {
	t.Helper()
	d := reaperDeps{
		GC: gc.Client("gonk-city"), Store: store, Now: reapClock(t),
	}
	if log != nil {
		d.Log = testLogger(log)
	}
	return d
}

// The case the reaper exists for: a session gonk created, with nothing in the
// bead store claiming it, old enough to be past the grace window.
func TestReaperClosesAnOrphanedSession(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 3, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("orphan survived the reaper: %v", live)
	}
	if len(gc.Closed) != 1 || gc.Closed[0] != alias {
		t.Fatalf("CloseSession was not called for the orphan: Closed = %v, want [%s]", gc.Closed, alias)
	}
}

// THE LOAD-BEARING ASSERTION. A session whose bead is in StateRunning is a live
// agent doing work. Reaping it destroys the batch it has not written yet and
// burns the attempt. This must never happen, and it is worth more than every
// leak the reaper prevents.
func TestReaperNeverTouchesASessionWithARunningBead(t *testing.T) {
	gc := gcapitest.New(t)
	alias := brokerSessionAlias("triage", 42, 3, 1)
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(42)
	rec.SessionID = alias
	rec.State = beadstore.StateRunning
	_ = store.Put(context.Background(), rec)

	runReap(context.Background(), reapDeps(t, gc, store, nil))

	if live := gc.LiveSessions(); len(live) != 1 || live[0] != alias {
		t.Fatalf("THE REAPER KILLED A LIVE AGENT. live = %v, want [%s]", live, alias)
	}
	if len(gc.Closed) != 0 {
		t.Fatalf("CloseSession was called for a session a running bead claims: Closed = %v", gc.Closed)
	}
}

// The grace window. A dispatch creates the session and only writes its record
// after prompt delivery, which is bounded by dispatchOrderTimeout (300s). For
// that entire window a perfectly healthy session has no record -- and looks
// exactly like an orphan. The window must comfortably exceed that deadline.
func TestReaperLeavesYoungSessionsAloneEvenWithNoRecord(t *testing.T) {
	gc := gcapitest.New(t)
	// Created one minute ago: mid-dispatch, no record written yet.
	gc.CreateSessionAt("gonk.triage.p42.i9.a1."+testNonce, gcapitest.SessionRunning, "2026-08-03T11:59:00Z")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("the reaper killed a session that a dispatch was still setting up: " +
			"the create precedes the record by the whole prompt-delivery window")
	}
}

func TestReaperGraceWindowComfortablyExceedsTheDeliveryDeadline(t *testing.T) {
	if reapGrace <= dispatchOrderTimeout {
		t.Fatalf("reapGrace %v must exceed dispatchOrderTimeout %v, or the reaper races "+
			"every dispatch it overlaps with", reapGrace, dispatchOrderTimeout)
	}
}

// gonkSessionAlias must match every shape brokerSessionAlias actually mints:
// the issue-scoped triage form and the project-scoped scaffold form (issue 0).
// A reaper that cannot match either of these matches nothing gonk creates.
func TestGonkSessionAliasMatchesBrokerAliases(t *testing.T) {
	triage := brokerSessionAlias("triage", 75, 35, 1)
	if !gonkSessionAlias.MatchString(triage) {
		t.Errorf("gonkSessionAlias did not match a triage alias: %q", triage)
	}

	scaffold := brokerSessionAlias("scaffold", 75, 0, 1)
	if !gonkSessionAlias.MatchString(scaffold) {
		t.Errorf("gonkSessionAlias did not match a scaffold alias: %q", scaffold)
	}
}

// The nonce is REQUIRED, not optional. An alias in the pre-nonce shape is
// either stale (nothing mints it any more) or not ours, and a near-miss on the
// prefix must still fail even with a syntactically valid trailer.
func TestGonkSessionAliasRejectsAliasesWithoutTheNonce(t *testing.T) {
	for _, alias := range []string{
		"gonk.triage.p1.a1",    // old-style, pre-nonce, project-scoped
		"gonk.triage.p1.i1.a1", // old-style, pre-nonce, issue-scoped
		"gonkish.triage.p1.a1", // near-miss on the prefix
	} {
		if gonkSessionAlias.MatchString(alias) {
			t.Errorf("gonkSessionAlias matched %q; want no match", alias)
		}
	}
}

// Match on OUR alias scheme only. The city may hold sessions gonk did not create
// -- pool sessions, the control-dispatcher, a human's ad-hoc session. Closing
// someone else's session is not a leak fix, it is an outage.
func TestReaperNeverTouchesSessionsGonkDidNotCreate(t *testing.T) {
	gc := gcapitest.New(t)
	for _, id := range []string{
		"control-dispatcher",      // Gas City's own
		"scaffold-1",              // a pool session
		"triage-adhoc-58f5c50f2a", // someone's manual probe
		"gonkish.triage.p1.a1",    // near-miss on our prefix
		"notgonk.triage.p1.a1",
		"gonk.triage.p1.a1", // old-style, pre-nonce: no broker mints this any more
	} {
		gc.CreateSessionAt(id, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")
	}

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 6 {
		t.Fatalf("the reaper closed sessions gonk did not create. survivors = %v", live)
	}
}

// A record in a TERMINAL state means the session should not be alive: sweep
// judged it and its close either failed or never happened. Reaping is correct --
// this is the recovery path for gonk-xkm's loud "ITS POD IS LEAKED" error.
func TestReaperClosesASessionWhoseBeadIsAlreadyDone(t *testing.T) {
	gc := gcapitest.New(t)
	alias := "gonk.triage.p42.i3.a1." + testNonce
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(42)
	rec.SessionID = alias
	rec.State = beadstore.StateDone
	_ = store.Put(context.Background(), rec)

	runReap(context.Background(), reapDeps(t, gc, store, nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("a session whose bead is done should be reaped: %v", live)
	}
}

// IF WE CANNOT READ THE BEAD STORE, WE KNOW NOTHING ABOUT WHAT IS LIVE. Reaping
// on a failed store read would close every gonk session in the city, because
// every one of them would look unclaimed. This is the single most destructive
// thing the reaper could do and it must fail closed.
func TestReaperRefusesToActWhenTheBeadStoreCannotBeRead(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSessionAt("gonk.triage.p42.i3.a1."+testNonce, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")

	var logged strings.Builder
	d := reapDeps(t, gc, failingStore{}, &logged)
	runReap(context.Background(), d)

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("THE REAPER CLOSED SESSIONS WITHOUT KNOWING WHICH WERE LIVE. " +
			"An unreadable bead store makes every session look unclaimed.")
	}
	if !strings.Contains(logged.String(), "reap: cannot read the bead store") {
		t.Fatalf("failing closed must be reported. log:\n%s", logged.String())
	}
}

// Pagination. The real route is keyset-paginated with a server cap, so a reaper
// that reads one page and stops silently under-reaps -- reporting success having
// missed most of what it exists to find.
func TestReaperFollowsPaginationToTheEnd(t *testing.T) {
	gc := gcapitest.New(t)
	gc.ListPageSize = 2
	for _, id := range []string{
		"gonk.triage.p1.i1.a1." + testNonce, "gonk.triage.p1.i2.a1." + testNonce, "gonk.triage.p1.i3.a1." + testNonce,
		"gonk.triage.p1.i4.a1." + testNonce, "gonk.triage.p1.i5.a1." + testNonce,
	} {
		gc.CreateSessionAt(id, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")
	}

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 0 {
		t.Fatalf("the reaper stopped at the first page: %v still live", live)
	}
}

// A session with an unparseable created_at must be treated as TOO YOUNG, not as
// the zero time (which would read as "created in year 1, therefore ancient" and
// reap it immediately). Unknown age resolves toward leaving it alone.
func TestReaperTreatsAnUnreadableAgeAsTooYoung(t *testing.T) {
	gc := gcapitest.New(t)
	gc.CreateSessionAt("gonk.triage.p42.i3.a1."+testNonce, gcapitest.SessionRunning, "not-a-timestamp")

	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), nil))

	if live := gc.LiveSessions(); len(live) != 1 {
		t.Fatal("a session whose age could not be read was reaped; unknown age must fail safe")
	}
}

// THE WIRING, INVERTED (gonk-t55 plan D7; plan 3.4 C3). This used to prove
// runSweep ran the reaper. Under the Jobs runtime it must not: runReap's job is
// to CLOSE GAS CITY SESSIONS through the supervisor API, and the sweep holds no
// Gas City client by design (exit criterion 2). (Its pod half would NOT touch
// Job pods -- it acts only on pods whose gc-agent label is an old-style alias,
// gonkSessionAlias, and Job pods carry no gc-agent label.)
//
// A GREP OF sweep.go WAS NOT THE GUARD IT CLAIMED TO BE: a runReap call added
// to sweep_skew.go -- which runSweep reaches -- passed it (verifier M15). So
// this walks the package's call graph instead: every non-test file is parsed,
// and every function or method reachable from runSweep, through any
// identifier or selector naming one -- or naming a package var or struct
// field that is given one as a VALUE (plan 3.5 R1, verifier V16; see
// reachableFrom) -- is checked. Name-based, so it
// over-approximates (a method call reaches every method of that name), which
// is the safe direction for a "must never reach" guard.
func TestSweepNeverRunsTheGasCityReaper(t *testing.T) {
	reach, via := reachableFrom(t, ".", "runSweep")
	// Not vacuous: the walk must cross files, into the sweep's own helpers in
	// sweep_skew.go and the stale-session pass.
	for _, must := range []string{"parkIfImageSkew", "reapStaleSessions", "closeSession", "sweepRunning"} {
		if !reach[must] {
			t.Fatalf("the call-graph walk from runSweep did not reach %s; the guard below would prove nothing", must)
		}
	}
	// CLOSING ONE LEGACY SESSION IS NOT THE REAPER, and the guard tells them
	// apart by WHAT IS REACHED, not by what Gas City calls happen: the sweep
	// reaches gcLegacyCloser.CloseLegacySession (main.go), which closes the
	// ONE alias a work record named, and the guard asserts that it does. It
	// bans the reaper's own functions, runReap and runPodReap -- the ones that
	// LIST Gas City's sessions or pods and close/delete what no record claims.
	// The legacy pod pass is not them either: it never asks Gas City anything.
	if !reach["CloseLegacySession"] {
		t.Fatalf("the walk from runSweep did not reach CloseLegacySession; the legacy park's close is not wired as the plan says")
	}
	for _, banned := range []string{"runReap", "runPodReap"} {
		if reach[banned] {
			path := []string{banned}
			for f := via[banned]; f != ""; f = via[f] {
				path = append([]string{f}, path...)
			}
			t.Fatalf("runSweep reaches %s (%s) -- the Gas City reaper must stay off the sweep path (plan D7)",
				banned, strings.Join(path, " -> "))
		}
	}
}

// reachableFrom parses every non-test .go file in dir and returns the set of
// names reachable from root, plus, for each, the name it was first reached
// from.
//
// FUNCTION VALUES ARE FOLLOWED (plan 3.5 R1; verifier V16). Indexing only
// FuncDecls left the walk blind to a function reached as a VALUE: `var hook =
// runPodReap` at package level, called as hook(...) from a swept function,
// passed the guard, because nothing walked the var's initialiser. So a name
// maps to every expression that can give it a callable value, and reaching the
// name walks all of them:
//
//   - a func declaration or method: its body;
//   - a package-level var: its initialiser, AND the right-hand side of every
//     assignment to that name anywhere in the package (so `func init() { hook
//     = runPodReap }`, which nothing calls, is still followed);
//   - a struct field (reached through a selector, x.F): the value of every
//     `F: v` key in a composite literal and of every `x.F = v` assignment in
//     the package -- the dependency-injection shape, `sweepDeps{F: runPodReap}`
//     built in main and called as d.F(...) from the sweep.
//
// All of it is name-based, so it over-approximates (a selector .F reaches
// every value any field or method named F is given), which is the safe
// direction for a "must never reach" guard.
func reachableFrom(t *testing.T, dir, root string) (map[string]bool, map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	// bodies: name -> every node to walk once that name is reached.
	bodies := map[string][]ast.Node{}
	add := func(name string, n ast.Node) {
		if name != "" && name != "_" && n != nil {
			bodies[name] = append(bodies[name], n)
		}
	}
	// addAll pairs names with values; an unpaired form (`var a, b = f()`)
	// gives every name every value.
	addAll := func(names []string, values []ast.Expr) {
		for i, name := range names {
			if len(values) == len(names) {
				add(name, values[i])
				continue
			}
			for _, v := range values {
				add(name, v)
			}
		}
	}
	pkgVars := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					add(d.Name.Name, d.Body)
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					var names []string
					for _, id := range vs.Names {
						names = append(names, id.Name)
						pkgVars[id.Name] = true
					}
					addAll(names, vs.Values)
				}
			}
		}
	}
	// Assignments and keyed composite-literal fields, anywhere in the package.
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				var names []string
				for _, lhs := range x.Lhs {
					switch l := lhs.(type) {
					case *ast.Ident:
						if pkgVars[l.Name] {
							names = append(names, l.Name)
						} else {
							names = append(names, "")
						}
					case *ast.SelectorExpr:
						names = append(names, l.Sel.Name)
					default:
						names = append(names, "")
					}
				}
				addAll(names, x.Rhs)
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok {
					add(k.Name, x.Value)
				}
			}
			return true
		})
	}
	if len(bodies[root]) == 0 {
		t.Fatalf("no function %s in %s", root, dir)
	}
	reach := map[string]bool{root: true}
	via := map[string]string{root: ""}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, body := range bodies[cur] {
			ast.Inspect(body, func(n ast.Node) bool {
				var name string
				switch x := n.(type) {
				case *ast.Ident:
					name = x.Name
				case *ast.SelectorExpr:
					name = x.Sel.Name
				default:
					return true
				}
				if _, known := bodies[name]; known && !reach[name] {
					reach[name], via[name] = true, cur
					queue = append(queue, name)
				}
				return true
			})
		}
	}
	return reach, via
}

// THE WALK ITSELF, on a synthetic package (plan 3.5 R1): each way a function
// can be reached as a value -- the V16 shape the verifier found blind, plus
// the other shapes of the same hole -- must be followed. This is what makes
// the guard above mean something for the shapes nobody has written yet into
// the real package.
func TestReachableFromFollowsFunctionValues(t *testing.T) {
	const banned = `
func banned() {}
`
	cases := map[string]string{
		// V16, verbatim in shape: a package-level func var called from the root.
		"package var initialiser": `
var hook = banned
func root() { hook() }
`,
		// Assigned only in init(), which nothing calls.
		"package var assigned in init": `
var hook func()
func init() { hook = banned }
func root() { hook() }
`,
		// Injected through a struct field in a composite literal (main's shape).
		"struct field, composite literal": `
type deps struct{ Hook func() }
func wire() deps { return deps{Hook: banned} }
func root(d deps) { d.Hook() }
`,
		// Injected through a field assignment.
		"struct field, assignment": `
type deps struct{ Hook func() }
func wire(d *deps) { d.Hook = banned }
func root(d deps) { d.Hook() }
`,
		// A func literal in the initialiser that calls it.
		"package var func literal": `
var hook = func() { banned() }
func root() { hook() }
`,
		// A method value.
		"method value": `
type r struct{}
func (r) Banned() { banned() }
var hook = r{}.Banned
func root() { hook() }
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte("package p\n"+src+banned), 0o600); err != nil {
				t.Fatal(err)
			}
			reach, _ := reachableFrom(t, dir, "root")
			if !reach["banned"] {
				t.Fatalf("the walk from root did not reach banned through a function value (%s); the D7 guard is blind to it", name)
			}
		})
	}
	// And not everything: an unrelated var is not reached.
	t.Run("unreferenced var is not reached", func(t *testing.T) {
		dir := t.TempDir()
		src := "package p\nvar other = banned\nfunc root() {}\n" + banned
		if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if reach, via := reachableFrom(t, dir, "root"); reach["banned"] {
			t.Fatalf("the walk reached banned from a root that never names it (via %q); it over-approximates too far to be useful", via["banned"])
		}
	})
}

// THE BLIND SPOT, PINNED (gonk-wmb7). The reaper is routinely described -- in
// this tree and in the bug reports that reach it -- as the thing that destroys
// any gonk session no running record claims. It is not. It destroys any such
// session THAT THE CITY STILL LISTS, and gascity's session list is the open-bead
// list: humaHandleSessionList reads the session read model with IncludeClosed
// unset, so a pod whose bead has been closed is not a row.
//
// This test exists so that the next person to ask "why did nothing reap it?"
// finds the answer asserted rather than having to rediscover it against a live
// cluster, as gonk-wmb7 did: on 2026-09-21 pod s-go-whg1 ran for 25 minutes
// after its bead was closed, while every 60s tick got 200 {"items":[],"total":0}
// and logged nothing at all.
//
// IT IS STILL NOT A TEST OF A FIX, AND THE REASON HAS CHANGED (gonk-mfsr). At
// the BEAD layer there is no fix: the session is invisible to the list AND
// unreachable by close (404, allowClosed=false), so no amount of rewriting the
// loop above recovers the class. What recovers it is a different source of
// truth -- the POD list -- and that is runPodReap in reap_pods.go, which this
// test deliberately does not configure (sweepDeps.Pods is nil here, as it is on
// any install without podReap.enabled). So this keeps asserting exactly what it
// always did: with the bead-level view alone, gonk never hears of the session
// and says nothing about it.
//
// The recovery has its own file. TestPodReaperDeletesALeakedPodWhoseSessionBeadWasAlreadyClosed
// is the same scenario with the pod reaper armed, and it ends with the pod gone.
func TestReaperIsBlindToASessionWhoseBeadGasCityClosed(t *testing.T) {
	gc := gcapitest.New(t)
	alias := "gonk.triage.p42.i3.a1." + testNonce
	// Old enough to be reapable, unclaimed by any record, and a perfect match
	// for gonkSessionAlias: every test above would have it destroyed.
	gc.CreateSessionAt(alias, gcapitest.SessionRunning, "2026-08-01T00:00:00Z")
	gc.BeadClosedPodAlive(alias)

	var log strings.Builder
	runReap(context.Background(), reapDeps(t, gc, beadstore.NewMemory(), &log))

	if attempts := gc.CloseAttemptCount(); attempts != 0 {
		t.Fatalf("close attempts = %d, want 0: the reaper cannot have tried to close a session "+
			"the city never listed -- if this now fails, the reaper has grown a second source of "+
			"truth and the comment on runReap needs rewriting", attempts)
	}
	if live := gc.LiveSessions(); len(live) != 1 || live[0] != alias {
		t.Fatalf("live sessions = %v, want exactly [%s]: the pod outlives the reaper", live, alias)
	}
	if got := log.String(); strings.Contains(got, "reap:") {
		t.Fatalf("the reaper said something about a session it cannot see. Log was:\n%s", got)
	}
}

// failingStore makes every List fail, so the reaper's fail-closed path can be
// driven. Put/Get are unused by the reaper.
type failingStore struct{}

func (failingStore) Put(context.Context, beadstore.Record) error { return errStoreDown }
func (failingStore) PutIf(context.Context, beadstore.Record, string) (string, error) {
	return "", errStoreDown
}
func (failingStore) Get(context.Context, string) (beadstore.Record, bool, error) {
	return beadstore.Record{}, false, errStoreDown
}
func (failingStore) List(context.Context, beadstore.State) ([]beadstore.Record, error) {
	return nil, errStoreDown
}
func (failingStore) ListOpenPipelineRuns(context.Context) ([]beadstore.Record, error) {
	return nil, errStoreDown
}
