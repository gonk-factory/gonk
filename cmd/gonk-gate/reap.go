package main

// THIS FILE IS UNREFERENCED FROM DISPATCH AND SWEEP (gonk-t55 plan D7). The
// reaper's work is closing Gas City sessions through the supervisor API, and
// once dispatch starts Kubernetes Jobs the sweep holds no Gas City client (exit
// criterion 2; it sees Gas City only through two one-method interfaces wired
// in main.go, orderRunner and legacySessionCloser) and gonk creates no Gas City
// sessions to reap. (It would NOT
// have deleted Job pods: its pod half only touches pods whose gc-agent label
// is a gonk alias, and Job pods have no gc-agent label.) The sessions in flight
// at the cutover are handled by the sweep's legacy park and legacy pod pass
// (sweep_legacy.go, plan 3.4 C1). TestSweepNeverRunsTheGasCityReaper walks
// runSweep's call graph to keep runReap and runPodReap off it. Left in the
// tree, with its tests, for T-56 to delete with the rest of the Gas City
// surface.

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/gcapi"
)

// reaperDeps is the reaper's own dependency set. It used to be sweepDeps; the
// sweep no longer carries a Gas City client or a pod lister (gonk-t55), so the
// reaper names what it needs itself.
type reaperDeps struct {
	GC    *gcapi.Client
	Store beadstore.Store
	// Pods is the POD-LEVEL recovery path (reap_pods.go). Nil means off.
	Pods podLister
	Log  *slog.Logger
	Now  func() time.Time
}

func (d reaperDeps) withDefaults() reaperDeps {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// errStoreDown is returned by test doubles standing in for an unreadable bead
// store. Declared in production code because the reaper's fail-closed path is
// the most important thing about it and deserves to be exercised.
var errStoreDown = errors.New("bead store unavailable")

// reapGrace is how old a session must be before the reaper will consider it an
// orphan.
//
// IT MUST EXCEED dispatchOrderTimeout -- a dispatch's whole life -- and
// TestReaperGraceWindowComfortablyExceedsTheDeliveryDeadline enforces that. A dispatch creates the session
// FIRST and writes the bead record only after prompt delivery succeeds, so for
// the whole delivery window a perfectly healthy session has no record and is
// indistinguishable from an orphan. Reaping inside that window would kill live
// agents at exactly the moment they were starting work -- and it would look like
// a flaky infrastructure problem, not like a bug in the reaper.
//
// 10 minutes against a 300s order timeout leaves the whole dispatch plus a slow
// cluster.
const reapGrace = 10 * time.Minute

// gonkSessionAlias matches ONLY the aliases brokerSessionAlias mints:
//
//	gonk.{agent}.p{projectID}.a{attempt}.{nonce}            (project-scoped, e.g. scaffold)
//	gonk.{agent}.p{projectID}.i{issueIID}.a{attempt}.{nonce}
//
// The trailing nonce segment is brokerSessionAlias's 128-bit crypto/rand
// suffix, base32-encoded with no padding: always 26 characters from [A-Z2-7]
// (ceil(128/5)). It is REQUIRED here, not optional -- an alias without one is
// either pre-nonce (gonk-mzd made the nonce load-bearing for every alias this
// broker mints) or not ours, and either way the reaper must not touch it.
//
// Anchored at both ends on purpose. The city holds sessions gonk did not create
// -- Gas City's own control-dispatcher, pool sessions, a human's ad-hoc probe --
// and closing one of those is not a leak fix, it is an outage. A near-miss like
// "gonkish.triage.p1.a1" must not match, which is what the leading `^gonk\.`
// and the strict segment shapes buy.
var gonkSessionAlias = regexp.MustCompile(`^gonk\.[a-z][a-z0-9-]*\.p\d+(\.i\d+)?\.a\d+\.[A-Z2-7]{26}$`)

// runReap closes gonk-created sessions that no live bead claims.
//
// It is the ONLY teardown path that can recover a session whose dispatch died
// between creating it and recording it (pod evicted, order timeout, controller
// restarted mid-dispatch). Those sessions have no bead in StateRunning, so
// gonk-sweep never looks at them: they are invisible rather than merely
// long-lived, and nothing else in the system will ever notice them.
//
// WHAT ITS BEAD-LEVEL HALF CANNOT DO, AND THIS IS NOT A DETAIL (gonk-wmb7). The
// session loop below sees the city only through ListSessions, and that route
// lists only sessions whose SESSION BEAD IS
// OPEN: upstream builds it from the session read model with IncludeClosed unset
// (GASCITY_REF internal/api/huma_handlers_sessions_query.go,
// humaHandleSessionList -> ListAllWithResponses). A pod whose bead gascity has
// closed is therefore not a row here, and no grace window, alias check or store
// read in this function is ever reached for it. The reaper does not skip it;
// IT NEVER HEARS OF IT, and logs nothing, which is indistinguishable from a
// clean tick that found no orphans.
//
// That is not hypothetical. On 2026-09-21 this reaper ran every 60s against a
// city holding a running agent pod (s-go-whg1) whose bead had been closed by a
// create rollback: GET /v0/city/gonk/sessions answered 200 with
// {"items":[],"total":0} on every tick, while the same supervisor's
// /beads?type=session&all=true answered 832 rows including that session. Twenty
// minutes, no log line, pod still up. Nor could the reaper have acted if it had
// seen it -- close and kill resolve with allowClosed=false and answer 404 for
// exactly this class (see closeSession in sweep.go).
//
// So the session loop covers sessions whose bead is still OPEN and that no
// running record claims -- the dispatch-died-before-recording case below, which
// is real and is what it was built for. It is NOT the backstop for a closed
// bead.
//
// THE CLOSED-BEAD CLASS IS NOW RECOVERED, BUT SOMEWHERE ELSE (gonk-mfsr). It
// needs a source of truth about PODS rather than about beads, and that is
// runPodReap (reap_pods.go), called at the end of this function with the two
// sets this one has already read. It is OFF unless GONK_POD_REAP_NAMESPACE is
// set, so on an unconfigured install every sentence above is still the whole
// story. Nothing in the session loop below changed: it still never hears of a
// closed-bead session, and this function still logs nothing about one.
//
// THE BIAS IS TOWARD DOING NOTHING. A leaked pod costs money and, at enough
// scale, wedges the scheduler. A wrongly reaped session kills a live agent
// mid-turn, destroys the work it had not written yet, and burns a ladder
// attempt. The leak is the cheaper failure, so every ambiguity here -- an
// unreadable store, an unparseable timestamp, an alias we do not recognise --
// resolves toward leaving the session alone.
//
// It reports nothing and returns nothing: it runs inside gonk-sweep's tick and
// must never fail the sweep. Its findings go to the log.
func runReap(ctx context.Context, d reaperDeps) {
	dd := d.withDefaults()

	// FAIL CLOSED ON THE STORE. This read is what tells us which sessions are
	// live; without it every gonk session in the city looks unclaimed and a
	// reaper that proceeded would close all of them. This is the single most
	// destructive thing this function could do, so it is the first thing
	// guarded.
	claimed, err := claimedSessionIDs(ctx, dd.Store)
	if err != nil {
		dd.Log.Error("reap: cannot read the bead store; reaping NOTHING this tick", "err", err)
		return
	}

	sessions, partial, err := dd.GC.ListSessions(ctx)
	if err != nil {
		dd.Log.Warn("reap: could not list sessions", "err", err)
		return
	}
	if partial {
		// STILL SAFE TO CONTINUE, BUT NOT FOR THE REASON IT ONCE WAS. Until
		// runPodReap existed, an incomplete list could only make this function
		// do LESS: a session it omitted was a session the loop below never
		// judged. That is no longer true (gonk-fpdp). The omitted alias is also
		// missing from `listed`, so it does not trip the pod half's "gascity
		// still lists it, leave it alone" guard, and the pod half evaluates its
		// pod instead.
		//
		// What that CANNOT do is delete it. The last gate down there is a close
		// against gascity, and only a 404 -- the signature of an already-closed
		// session bead -- authorizes deletion. An open session's close returns
		// 200, which ENDS the handling of that pod: gascity owns it and tears
		// the runtime down through its own provider.
		//
		// What it CAN do is close it, from the pod rather than from the list.
		// That is the same action the loop below would have taken had the list
		// been complete, gated on the same claim set and the same grace -- the
		// pod half just measures the grace against the pod's creationTimestamp
		// instead of the session's created_at. So the OUTCOME is what a complete
		// list would have produced; only the path to it differs.
		//
		// Worth saying out loud regardless, so a persistently partial list is
		// not mistaken for a clean run that found nothing.
		dd.Log.Warn("reap: the session list came back incomplete; some orphans may be invisible this tick")
	}

	// listed is every alias this tick's session list returned, gonk's or not.
	// runPodReap will not touch a pod whose alias appears here: gascity still
	// lists it, so its bead is OPEN, the loop below has already judged it
	// against the same claim set and the same grace, and a close in flight
	// tears the pod down properly -- ConfigMaps and session record included.
	listed := make(map[string]struct{}, len(sessions))

	now := dd.Now()
	for _, s := range sessions {
		alias := s.Alias
		if alias == "" {
			alias = s.ID
		}
		listed[alias] = struct{}{}
		if !gonkSessionAlias.MatchString(alias) {
			continue // not ours; not our business
		}
		if _, live := claimed[alias]; live {
			continue // a running bead claims it: this is a working agent
		}
		created, perr := time.Parse(time.RFC3339, s.CreatedAt)
		if perr != nil {
			// UNKNOWN AGE IS TREATED AS TOO YOUNG. Falling back to the zero time
			// would read as "created in year 1, therefore ancient" and reap it
			// immediately -- the failure direction we cannot afford.
			dd.Log.Warn("reap: session has an unreadable created_at; leaving it alone",
				"session", alias, "created_at", s.CreatedAt)
			continue
		}
		if now.Sub(created) < reapGrace {
			continue // a dispatch may still be setting it up
		}

		if err := dd.GC.CloseSession(ctx, alias); err != nil {
			if gcapi.IsNotFound(err) {
				// Already gone, which is the desired state -- and here, unlike in
				// closeSession (sweep.go), that reading is earned rather than
				// assumed: this alias came back from a session list read
				// milliseconds ago, so its bead was OPEN then, and a 404 now means
				// something closed it in between. The same 404 reached from a bead
				// gonk has not just seen listed carries no such evidence.
				continue
			}
			dd.Log.Error("reap: could not close orphaned session -- ITS POD IS LEAKED",
				"session", alias, "err", err)
			continue
		}
		dd.Log.Warn("reap: closed an ORPHANED session -- no bead claimed it",
			"session", alias, "created_at", s.CreatedAt, "age", now.Sub(created).String())
	}

	// THE POD-LEVEL HALF (gonk-mfsr). It runs LAST and on the SAME two reads
	// this function already failed closed on -- a failed store read or a failed
	// session list returned above and never reach here, which is the point: a
	// function that deletes pods must not run on information this one refused to
	// act on.
	runPodReap(ctx, dd, claimed, listed)
}

// claimedSessionIDs is the set of session aliases a RUNNING bead claims.
//
// Only StateRunning protects a session. A record in a terminal state (done,
// needs-human) means sweep has already judged it and its session should not be
// alive -- so reaping it is the recovery path for a close that failed and logged
// "ITS POD IS LEAKED". Parked beads have no session yet by construction.
func claimedSessionIDs(ctx context.Context, store beadstore.Store) (map[string]struct{}, error) {
	running, err := store.List(ctx, beadstore.StateRunning)
	if err != nil {
		return nil, err
	}
	claimed := make(map[string]struct{}, len(running))
	for _, rec := range running {
		if rec.SessionID != "" {
			claimed[rec.SessionID] = struct{}{}
		}
	}
	return claimed, nil
}

// brokerSessionAlias is the Gas City session alias dispatch USED to mint, and
// the only alias shape gonkSessionAlias recognises. NOTHING IN PRODUCTION MINTS
// ONE ANY MORE (gonk-t55): sessions are named by runtime.Name. It stays beside
// the reaper that recognises it, for that reaper's tests, until T-56 deletes
// both. What follows is its history.
//
// It was the correlation key stamped on the created session and recorded on
// the bead (Record.SessionID). gonk-sweep reads the session back by
// this alias via GetSessionOutput (the START observation uses GetSessionState:
// it reads only view.State).
//
// It carries 128 bits of crypto/rand as a base32 suffix, and THAT ENTROPY IS
// LOAD-BEARING (gonk-mzd): the alias is the capability the agent pod presents to
// fetch its own prompt, on the meter's one unauthenticated route. A deterministic
// alias -- which this used to be -- is reconstructible from a project and issue
// number by anyone who can reach the meter, which would make that route a public
// read of issue text. The meter refuses a nonce-free alias by shape for the same
// reason.
//
// The attempt segment stays: gascity rejects a create whose alias is taken, so a
// re-sling must not collide with the prior attempt's still-present session. The
// nonce makes that automatic, but keeping the segment keeps the alias legible in
// logs. session.ValidateAlias forbids colons (bead anchors use them) and caps
// length at 64; this form uses only [a-z0-9.A-Z2-7] and stays under the cap at
// ~49 characters.
//
// NOTHING IN PRODUCTION RECONSTRUCTS AN ALIAS. Sweep reads Record.SessionID;
// this is the one call site. Tests that used to recompute it must capture
// gc.Created[0].Alias instead -- a better assertion anyway, since it checks what
// was actually sent rather than re-running the generator and agreeing with
// itself.
func brokerSessionAlias(agent string, projectID, issueIID int64, attempt int) string {
	var b [16]byte // 128 bits
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not survivable here: a predictable alias is a
		// public read of the prompt, so refuse rather than degrade.
		panic("gonk-gate: crypto/rand unavailable, refusing to mint a guessable session alias: " + err.Error())
	}
	nonce := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	// Scaffold is PROJECT-scoped: it is dispatched with no issue, so an `.i0`
	// segment would be a lie about what the session is for. Two agents working
	// the same project must not collide, hence the agent in the alias.
	if issueIID == 0 {
		return fmt.Sprintf("gonk.%s.p%d.a%d.%s", agent, projectID, attempt, nonce)
	}
	return fmt.Sprintf("gonk.%s.p%d.i%d.a%d.%s", agent, projectID, issueIID, attempt, nonce)
}
