package main

import (
	"context"
	"errors"
	"regexp"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
)

// errStoreDown is returned by test doubles standing in for an unreadable bead
// store. Declared in production code because the reaper's fail-closed path is
// the most important thing about it and deserves to be exercised.
var errStoreDown = errors.New("bead store unavailable")

// reapGrace is how old a session must be before the reaper will consider it an
// orphan.
//
// IT MUST EXCEED defaultSubmitDeadline, and TestReaperGraceWindowComfortably-
// ExceedsTheDeliveryDeadline enforces that. A dispatch creates the session
// FIRST and writes the bead record only after prompt delivery succeeds, so for
// the whole delivery window a perfectly healthy session has no record and is
// indistinguishable from an orphan. Reaping inside that window would kill live
// agents at exactly the moment they were starting work -- and it would look like
// a flaky infrastructure problem, not like a bug in the reaper.
//
// 10 minutes against a 240s deadline leaves room for the meter call, the issue
// fetch and the create that precede it, plus a slow cluster.
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
// THE BIAS IS TOWARD DOING NOTHING. A leaked pod costs money and, at enough
// scale, wedges the scheduler. A wrongly reaped session kills a live agent
// mid-turn, destroys the work it had not written yet, and burns a ladder
// attempt. The leak is the cheaper failure, so every ambiguity here -- an
// unreadable store, an unparseable timestamp, an alias we do not recognise --
// resolves toward leaving the session alone.
//
// It reports nothing and returns nothing: it runs inside gonk-sweep's tick and
// must never fail the sweep. Its findings go to the log.
func runReap(ctx context.Context, d sweepDeps) {
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
		// Safe to continue: an incomplete list can only make us reap LESS. Worth
		// saying out loud so a persistently partial list is not mistaken for a
		// clean run that found nothing.
		dd.Log.Warn("reap: the session list came back incomplete; some orphans may be invisible this tick")
	}

	now := dd.Now()
	for _, s := range sessions {
		alias := s.Alias
		if alias == "" {
			alias = s.ID
		}
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
				continue // already gone, which is the desired state
			}
			dd.Log.Error("reap: could not close orphaned session -- ITS POD IS LEAKED",
				"session", alias, "err", err)
			continue
		}
		dd.Log.Warn("reap: closed an ORPHANED session -- no bead claimed it",
			"session", alias, "created_at", s.CreatedAt, "age", now.Sub(created).String())
	}
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
