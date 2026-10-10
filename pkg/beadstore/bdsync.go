package beadstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// THE CUTOVER FROM bd (gonk-t41, docs/plans/2026-09-14-t41-postgres-workstore.md
// section 6). The bd-backed gonk-gate left its records in bd; the Postgres work
// store must hold them before any dispatch or sweep runs on it, and a rollback
// to the bd-backed binary must not find bd stale.
//
// Both directions are a THREE-WAY MERGE against work_items_bd_sync, which holds,
// per anchor, a digest of bd's record and the work_items revision at the last
// moment the two were known to agree.
//
// WHY bd's SIDE IS A DIGEST AND NOT ITS REVISION. The gonk-gate deployed before
// this stack writes records with no Revision at all (the field arrived with
// gonk-p8w8), so every record on the cluster has revision "", and so does every
// write a rollback to that binary makes. A merge keyed on bd's revision would
// see those writes as "bd unchanged" and silently keep Postgres's copy
// (TestImportSeesWritesByABinaryThatHasNoRevisions). A digest of the whole
// record changes whenever the record does. The Postgres side can use the
// revision: every write this store makes stamps a fresh one. Only a bd-backed gonk-gate writes gonk records
// to bd, and only a Postgres-backed one writes work_items, so relative to that
// base:
//
//   - bd changed, Postgres did not: the bd-backed binary ran since (a rollback,
//     or a bd commit that landed after the import read it). bd is newer.
//   - Postgres changed, bd did not: the Postgres-backed binary ran since.
//     Postgres is newer.
//   - both changed: a CONFLICT. Neither side is overwritten; the anchor is
//     reported, loudly, for a human.
//
// Nothing here is ever silently clobbered.

// BdSyncReport is what one import or export did, anchor by anchor where it
// matters.
type BdSyncReport struct {
	Direction string `json:"direction"` // "import" (bd -> Postgres) or "export" (Postgres -> bd)
	Seen      int    `json:"seen"`      // anchors examined
	Written   int    `json:"written"`   // records copied into the other store
	InSync    int    `json:"in_sync"`   // anchors with nothing to copy
	NoRecord  int    `json:"no_record"` // bd beads with an anchor label and no gonk-state record
	// Conflicts: both sides changed since the last sync; neither was touched.
	// ConflictDetails says, per anchor, what each side did.
	Conflicts       []string          `json:"conflicts,omitempty"`
	ConflictDetails map[string]string `json:"conflict_details,omitempty"`
	// Undecodable: bd's current gonk-state comment does not decode; skipped.
	Undecodable []string `json:"undecodable,omitempty"`
	// Duplicates: more than one bd bead carries the anchor; the one the
	// bd-backed binary's Get picked (findBeadID) is the one used.
	Duplicates []string `json:"duplicates,omitempty"`
	// Reanchored: a bd record whose BeadAnchor field differed from its bead's
	// anchor label; it is stored under the label, which is what Get found it by.
	Reanchored []string `json:"reanchored,omitempty"`
	// Failed: a read or write that did not complete. The run is incomplete.
	Failed []string `json:"failed,omitempty"`
	// BdDatabaseMissing: bd's Dolt database does not exist yet (a fresh
	// install); there was nothing to import (ErrBdDatabaseMissing).
	// BdDatabase is the database bd looked for, so an operator can tell a fresh
	// install from a MISCONFIGURED database name, which bd reports identically.
	BdDatabaseMissing bool   `json:"bd_database_missing,omitempty"`
	BdDatabase        string `json:"bd_database,omitempty"`
	// Quarantined: records written as needs-human instead of their own state
	// (see quarantineFor), by reason, and which anchors.
	Quarantined        int            `json:"quarantined"`
	QuarantineByReason map[string]int `json:"quarantine_by_reason,omitempty"`
	QuarantinedAnchors []string       `json:"quarantined_anchors,omitempty"`
}

// Complete reports whether every anchor was read and decided. Conflicts and
// undecodable records are decisions (reported, left alone); a failed read is not.
func (r BdSyncReport) Complete() bool { return len(r.Failed) == 0 }

type syncOutcome int

const (
	syncWritten syncOutcome = iota
	syncInSync
	syncConflict
)

// bdReadTries and bdReadBackoff bound the retries of one bd read in the cutover:
// a Dolt hiccup on one anchor must not fail a whole import (and so a pod start).
// A record that does not decode is not retried -- it will not decode next time.
//
// bdReadTimeout bounds ONE try: bd has no timeout of its own, and against a
// stalled Dolt server (observed: SIGSTOPped locally) a bd call hangs for as long
// as the server does.
var (
	bdReadTries   = 3
	bdReadBackoff = time.Second
	bdReadTimeout = 60 * time.Second
)

// withBdRetry runs read up to bdReadTries times, backing off linearly, and
// returns the last error. Undecodable records and a missing database are final.
func withBdRetry[T any](ctx context.Context, read func(context.Context) (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for try := 1; ; try++ {
		tctx, cancel := context.WithTimeout(ctx, bdReadTimeout)
		v, err = read(tctx)
		cancel()
		if err == nil || errors.Is(err, errUndecodableRecord) || errors.Is(err, ErrBdDatabaseMissing) || try >= bdReadTries {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(time.Duration(try) * bdReadBackoff):
		}
	}
}

// bdRecordOf reads the record the bd-backed gonk-gate's Get returned for anchor,
// given the beads that carry its label. Retried (withBdRetry).
func bdRecordOf(ctx context.Context, bd *BdCLI, anchor string, ids []string) (bdRecord, error) {
	return withBdRetry(ctx, func(ctx context.Context) (bdRecord, error) { return bdRecordOfOnce(ctx, bd, anchor, ids) })
}

func bdRecordOfOnce(ctx context.Context, bd *BdCLI, anchor string, ids []string) (bdRecord, error) {
	switch len(ids) {
	case 0:
		return bdRecord{}, nil
	case 1:
		return bd.readRecord(ctx, ids[0])
	default:
		id, ok, err := bd.findBeadID(ctx, anchor) // the bead the bd-backed Get read
		if err != nil || !ok {
			return bdRecord{}, err
		}
		return bd.readRecord(ctx, id)
	}
}

// bdDigest identifies the bd side of a sync base: sha256 of the RAW text of the
// gonk-state comment that is bd's current record (review defect A2).
//
// NOT a digest of the decoded Record. That was the first version, and it made
// the sync base a function of this binary's Go struct: add one field to Record
// and every record's digest changes, so every anchor Postgres had moved reads as
// "both sides changed" -- a permanent conflict, and workstore-export exits 1 on
// every anchor, blocking the rollback it exists for. The comment text is what
// bd stores; it changes only when bd's record does.
func bdDigest(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// QUARANTINE (owner decision on the gonk-t41 review). The Postgres store's List
// returns every record in a state; the bd-backed one did not. Records whose bead
// lost its state label (gonk-9sf9) and records past bd's default 50 rows
// (gonk-kx3) were never judged by any sweep, and a record nothing has written
// for a day is work the system has already stopped doing. Imported as they
// were, the first Postgres-backed sweep would act on that whole backlog at once:
// re-slings, unparks, sessions and spend nobody planned.
//
// So at import, a record in a state the sweep lists is written as needs-human --
// which no sweep List reads -- when bd's List could not see it or its last write
// is older than quarantineAge. Its original record, the reason and its age go to
// work_items_quarantine for a human. Everything else imports as it is.

// sweptStates are the states gonk-sweep Lists (cmd/gonk-gate/sweep.go).
var sweptStates = []State{StateRunning, StateParked, StatePendingPrompt}

// quarantineAge: a swept record whose last write is older than this is
// quarantined at import.
const quarantineAge = 24 * time.Hour

// Quarantine reasons.
const (
	QuarantineHidden     = "hidden-from-bd-list"
	QuarantineStale      = "older-than-24h"
	QuarantineAgeUnknown = "age-unknown"
)

// nowFunc is the import's clock (a test substitutes one).
var nowFunc = time.Now

// bdVisibility is, per swept state, the beads the bd-backed List returned.
type bdVisibility map[State]map[string]bool

func bdListVisibility(ctx context.Context, bd *BdCLI) (bdVisibility, error) {
	vis := bdVisibility{}
	for _, st := range sweptStates {
		ids, err := withBdRetry(ctx, func(ctx context.Context) (map[string]bool, error) { return bd.listedBy(ctx, st) })
		if err != nil {
			return nil, fmt.Errorf("beadstore: list %s beads as the bd-backed sweep did: %w", st, err)
		}
		vis[st] = ids
	}
	return vis, nil
}

// lastWrite is when br's current record was written: bd's created_at for its
// comment, else the record's own UpdatedAt, else zero (unknown).
func lastWrite(br bdRecord) time.Time {
	if !br.WrittenAt.IsZero() {
		return br.WrittenAt
	}
	return br.Record.UpdatedAt
}

// quarantineFor returns why br must be quarantined, or "" if it imports as it is.
func quarantineFor(br bdRecord, vis bdVisibility, now time.Time) string {
	swept := false
	for _, st := range sweptStates {
		swept = swept || br.Record.State == st
	}
	if !swept {
		return ""
	}
	var reasons []string
	if !vis[br.Record.State][br.ID] {
		reasons = append(reasons, QuarantineHidden)
	}
	switch lw := lastWrite(br); {
	case lw.IsZero():
		reasons = append(reasons, QuarantineAgeUnknown)
	case now.Sub(lw) > quarantineAge:
		reasons = append(reasons, QuarantineStale)
	}
	return strings.Join(reasons, "+")
}

// SurveyRecord is one record in workstore-survey's output.
type SurveyRecord struct {
	Anchor          string     `json:"anchor"`
	State           string     `json:"state"`
	LastWriteAt     *time.Time `json:"last_write_at,omitempty"`
	AgeHours        *float64   `json:"age_hours,omitempty"` // absent = unknown
	VisibleToBdList bool       `json:"visible_to_bd_list"`
	Quarantine      string     `json:"quarantine,omitempty"` // the reason an import would quarantine it
}

// ageBucket names the survey's age buckets.
func ageBucket(lw, now time.Time) string {
	if lw.IsZero() {
		return "unknown"
	}
	switch d := now.Sub(lw); {
	case d < time.Hour:
		return "<1h"
	case d <= quarantineAge:
		return "1h-24h"
	case d <= 7*24*time.Hour:
		return "1d-7d"
	case d <= 30*24*time.Hour:
		return "7d-30d"
	default:
		return ">30d"
	}
}

// BdSurvey is what an import would find in bd, read without touching any store:
// `gonk-gate workstore-survey`, for sizing a cutover -- and its quarantine --
// before it happens.
type BdSurvey struct {
	Anchors  int            `json:"anchors"`   // anchors bd's default status filter shows
	ByState  map[string]int `json:"by_state"`  // records by State ("" = no state, the go-ffoe shape)
	NoRecord int            `json:"no_record"` // beads with an anchor label and no gonk-state record
	// AgeBuckets: per state, records by the age of their last write.
	AgeBuckets map[string]map[string]int `json:"age_buckets"`
	// Quarantine: how many records an import would quarantine, by reason.
	Quarantine      map[string]int `json:"quarantine"`
	QuarantineTotal int            `json:"quarantine_total"`
	// Records: every record, with its age, whether the bd-backed List saw it,
	// and its quarantine reason if any.
	Records []SurveyRecord `json:"records"`
	// HiddenFromBdList: swept-state records the bd-backed sweep's List never
	// returned (a lost state label, or past bd's 50-row default).
	HiddenFromBdList []string `json:"hidden_from_bd_list,omitempty"`
	Undecodable      []string `json:"undecodable,omitempty"`
	Duplicates       []string `json:"duplicates,omitempty"`
	Failed           []string `json:"failed,omitempty"`
	// BdDatabaseMissing: bd's Dolt database does not exist yet (fresh install);
	// BdDatabase is the name bd looked for (a misconfigured name looks the same).
	BdDatabaseMissing bool   `json:"bd_database_missing,omitempty"`
	BdDatabase        string `json:"bd_database,omitempty"`
}

// enumerateBd is anchorBeads, retried.
func enumerateBd(ctx context.Context, bd *BdCLI) (map[string][]string, map[string][]string, error) {
	type both struct{ anchors, labels map[string][]string }
	r, err := withBdRetry(ctx, func(ctx context.Context) (both, error) {
		a, l, err := bd.anchorBeads(ctx)
		return both{a, l}, err
	})
	return r.anchors, r.labels, err
}

// SurveyBd reads every gonk record in bd the way ImportFromBd does, and writes
// nothing anywhere.
func SurveyBd(ctx context.Context, bd *BdCLI, log *slog.Logger) (BdSurvey, error) {
	sv := BdSurvey{ByState: map[string]int{}, AgeBuckets: map[string]map[string]int{}, Quarantine: map[string]int{}}
	now := nowFunc()
	anchors, _, err := enumerateBd(ctx, bd)
	if errors.Is(err, ErrBdDatabaseMissing) {
		sv.BdDatabaseMissing, sv.BdDatabase = true, bdMissingDatabaseName(err)
		return sv, nil
	} else if err != nil {
		return sv, fmt.Errorf("beadstore: enumerate bd work records: %w", err)
	}
	vis, err := bdListVisibility(ctx, bd)
	if err != nil {
		return sv, err
	}
	for _, anchor := range sortedKeys(anchors) {
		sv.Anchors++
		ids := anchors[anchor]
		if len(ids) > 1 {
			sv.Duplicates = append(sv.Duplicates, anchor)
		}
		br, err := bdRecordOf(ctx, bd, anchor, ids)
		rec := br.Record
		switch {
		case errors.Is(err, errUndecodableRecord):
			sv.Undecodable = append(sv.Undecodable, anchor)
			continue
		case err != nil:
			sv.Failed = append(sv.Failed, anchor)
			log.Error("bd survey: could not read the bd record", "anchor", anchor, "err", err)
			continue
		case !br.Found:
			sv.NoRecord++
			continue
		}
		sv.ByState[string(rec.State)]++
		lw := lastWrite(br)
		st := string(rec.State)
		if sv.AgeBuckets[st] == nil {
			sv.AgeBuckets[st] = map[string]int{}
		}
		sv.AgeBuckets[st][ageBucket(lw, now)]++
		sr := SurveyRecord{Anchor: anchor, State: st, VisibleToBdList: vis[rec.State][br.ID],
			Quarantine: quarantineFor(br, vis, now)}
		if !lw.IsZero() {
			lwc, age := lw, now.Sub(lw).Hours()
			sr.LastWriteAt, sr.AgeHours = &lwc, &age
		}
		for _, s := range sweptStates {
			if rec.State == s && !sr.VisibleToBdList {
				sv.HiddenFromBdList = append(sv.HiddenFromBdList, anchor)
			}
		}
		if sr.Quarantine != "" {
			sv.Quarantine[sr.Quarantine]++
			sv.QuarantineTotal++
		}
		sv.Records = append(sv.Records, sr)
	}
	return sv, nil
}

// ImportFromBd copies bd's gonk records into the work store, by the merge above,
// and marks the store imported (workstore_meta.bd_imported_at) if the run was
// Complete. The error is for a run that could not start (bd could not be
// enumerated, the marker could not be written); per-anchor trouble is in the
// report.
func (p *Postgres) ImportFromBd(ctx context.Context, bd *BdCLI, log *slog.Logger, opts ImportOptions) (BdSyncReport, error) {
	return p.importFromBd(ctx, bd, log, opts, false)
}

// ImportOptions are the import's inputs that are not in bd or the store.
type ImportOptions struct {
	// FreshInstall is the POSITIVE signal that this is a brand-new install, set
	// by the caller only when something outside bd says so (gonk-gate: the env
	// GONK_WORKSTORE_FRESH_INSTALL=1, which the chart renders only when Helm is
	// installing the release, .Release.IsInstall).
	//
	// It decides what bd's "database not found" means. On a fresh install gc has
	// not created bd's database yet, so there is nothing to import. On an
	// EXISTING install the same message means dolt_database names the wrong
	// database, and reading it as an empty import would mark the store imported
	// and start the controller without bd's in-flight records (verification
	// defect 1). bd cannot tell the two apart, so the import does not try:
	// without FreshInstall a missing database is an error and nothing is marked.
	// Even with it, a store that already holds work records is not fresh, and
	// the import refuses.
	FreshInstall bool
}

// ErrNotAFreshInstall is returned, wrapping ErrBdDatabaseMissing, when bd's
// database does not exist and the import has no proof this is a fresh install.
var ErrNotAFreshInstall = errors.New("bd's database does not exist and this is not known to be a fresh install")

// PlanImportFromBd is ImportFromBd without writing anything: every anchor's
// decision is made in its transaction exactly as the import would make it, and
// the transaction is rolled back; the store is not marked imported. For
// `gonk-gate workstore-survey`: which anchors an import would write, quarantine,
// or report as conflicts -- with why (ConflictDetails).
func (p *Postgres) PlanImportFromBd(ctx context.Context, bd *BdCLI, log *slog.Logger, opts ImportOptions) (BdSyncReport, error) {
	return p.importFromBd(ctx, bd, slog.New(discardHandler{}), opts, true)
}

// discardHandler drops log records (a plan must not log as if it imported).
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

func (p *Postgres) importFromBd(ctx context.Context, bd *BdCLI, log *slog.Logger, opts ImportOptions, dryRun bool) (BdSyncReport, error) {
	rep := BdSyncReport{Direction: "import"}
	if dryRun {
		rep.Direction = "import-plan"
	}
	anchors, _, err := enumerateBd(ctx, bd)
	if errors.Is(err, ErrBdDatabaseMissing) {
		rep.BdDatabaseMissing, rep.BdDatabase = true, bdMissingDatabaseName(err)
		// bd says the same thing on a fresh install and on an existing install
		// whose dolt_database is wrong (ImportOptions.FreshInstall). Only a
		// positive fresh-install signal, on a store holding no work records,
		// makes it an empty import; anything else fails and marks nothing.
		if !opts.FreshInstall {
			return rep, fmt.Errorf("beadstore: %w (%w): bd found no database %q. On an EXISTING install this "+
				"means dolt_database names the wrong database, and importing nothing would start the controller "+
				"without bd's records -- fix the name. On a genuinely fresh install the chart sets "+
				"GONK_WORKSTORE_FRESH_INSTALL=1 when Helm installs the release: %v",
				ErrNotAFreshInstall, ErrBdDatabaseMissing, rep.BdDatabase, err)
		}
		var hasRecords bool
		if qerr := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_items)`).Scan(&hasRecords); qerr != nil {
			return rep, fmt.Errorf("beadstore: check the work store is empty before an empty import: %w", qerr)
		}
		if hasRecords {
			return rep, fmt.Errorf("beadstore: %w (%w): bd found no database %q, and the work store already holds "+
				"work records, so this is not a fresh install whatever the fresh-install signal says; "+
				"check dolt_database: %v", ErrNotAFreshInstall, ErrBdDatabaseMissing, rep.BdDatabase, err)
		}
		// A fresh install: gc has not created bd's database yet, so bd holds no
		// gonk records. An empty import, and a complete one.
		log.Warn("bd import: FRESH INSTALL (GONK_WORKSTORE_FRESH_INSTALL) and bd's Dolt database does not exist "+
			"yet (gc start creates it); there is nothing to import", "bd_database", rep.BdDatabase, "err", err)
		anchors = map[string][]string{}
	} else if err != nil {
		return rep, fmt.Errorf("beadstore: enumerate bd work records: %w", err)
	}
	vis := bdVisibility{}
	if !rep.BdDatabaseMissing {
		if vis, err = bdListVisibility(ctx, bd); err != nil {
			return rep, err
		}
	}
	now := nowFunc()
	for _, anchor := range sortedKeys(anchors) {
		rep.Seen++
		ids := anchors[anchor]
		if len(ids) > 1 {
			rep.Duplicates = append(rep.Duplicates, anchor)
			log.Warn("bd import: more than one bead carries this anchor; importing the one the bd-backed gonk-gate read",
				"anchor", anchor, "beads", ids)
		}
		br, err := bdRecordOf(ctx, bd, anchor, ids)
		rec := br.Record
		switch {
		case errors.Is(err, errUndecodableRecord):
			rep.Undecodable = append(rep.Undecodable, anchor)
			log.Error("bd import: the bead's gonk-state record does not decode; NOT imported", "anchor", anchor, "err", err)
			continue
		case err != nil:
			rep.Failed = append(rep.Failed, anchor)
			log.Error("bd import: could not read the bd record", "anchor", anchor, "err", err)
			continue
		case !br.Found:
			rep.NoRecord++
			continue
		}
		if rec.BeadAnchor != anchor {
			rep.Reanchored = append(rep.Reanchored, anchor)
			log.Warn("bd import: the record's BeadAnchor differs from its bead's anchor label; storing it under the label",
				"anchor", anchor, "record_anchor", rec.BeadAnchor)
			rec.BeadAnchor = anchor
		}
		var q *quarantined
		if reason := quarantineFor(br, vis, now); reason != "" {
			orig, err := encodeRecord(rec)
			if err != nil {
				return rep, err
			}
			lw := lastWrite(br)
			q = &quarantined{Reason: reason, OriginalState: rec.State, BdRecord: orig}
			if !lw.IsZero() {
				q.LastWrite = &lw
			}
			rec.State = StateNeedsHuman
		}
		outcome, detail, err := p.importOne(ctx, rec, bdDigest(br.Raw), q, dryRun)
		if err == nil && outcome == syncWritten && q != nil {
			rep.Quarantined++
			if rep.QuarantineByReason == nil {
				rep.QuarantineByReason = map[string]int{}
			}
			rep.QuarantineByReason[q.Reason]++
			rep.QuarantinedAnchors = append(rep.QuarantinedAnchors, anchor)
			log.Warn("bd import: record QUARANTINED as needs-human; no sweep will act on it until a human does "+
				"(work_items_quarantine holds the original)", "anchor", anchor, "reason", q.Reason,
				"original_state", string(q.OriginalState), "last_write_at", q.LastWrite, "session", rec.SessionID)
		}
		switch {
		case err != nil:
			rep.Failed = append(rep.Failed, anchor)
			log.Error("bd import: could not write the record", "anchor", anchor, "err", err)
		case outcome == syncConflict:
			rep.Conflicts = append(rep.Conflicts, anchor)
			if rep.ConflictDetails == nil {
				rep.ConflictDetails = map[string]string{}
			}
			rep.ConflictDetails[anchor] = detail
			log.Error("bd import: CONFLICT -- bd and the Postgres work store both changed this record since they "+
				"last agreed; neither was changed, a human must decide", "anchor", anchor, "detail", detail,
				"bd_state", string(rec.State), "bd_session", rec.SessionID, "bd_attempt", rec.Attempt)
		case outcome == syncWritten:
			rep.Written++
			log.Info("bd import: record imported", "anchor", anchor, "state", string(rec.State),
				"attempt", rec.Attempt, "revision", rec.Revision)
		default:
			rep.InSync++
		}
	}
	if rep.Complete() && !dryRun {
		if _, err := p.pool.Exec(ctx, `UPDATE workstore_meta SET bd_imported_at = now() WHERE id`); err != nil {
			return rep, fmt.Errorf("beadstore: mark the bd import complete: %w", err)
		}
	}
	return rep, nil
}

// importOne decides and applies the merge for one bd record b, in one
// transaction with the work_items row and its sync row locked.
//
// The write is b as bd holds it -- UpdatedAt kept, not stamped, because a
// pending-prompt reservation's age must survive the copy -- with ONE exception:
// a record with no Revision gets a fresh one (review defect A1). Every record the
// the maintainers' binary wrote has none, and in this store PutIf(r, "") means "no record
// exists"; a stored empty revision would let dispatch's unreadable-store
// reservation match, and its release then destroy, a live session's record
// (TestImportedRevisionlessRecordCannotBeDestroyedByPutIfEmpty). The table's
// CHECK work_items_revision_set refuses such a row in any case.
//
// q, if not nil, marks b as a quarantined copy (its State already needs-human);
// the original goes to work_items_quarantine in the same transaction, and only
// if the record is actually written.
// quarantined is what importOne records alongside a quarantined write.
type quarantined struct {
	Reason        string
	OriginalState State
	LastWrite     *time.Time
	BdRecord      string // the record as bd held it, JSON
}

func (p *Postgres) importOne(ctx context.Context, b Record, bDigest string, q *quarantined, dryRun bool) (syncOutcome, string, error) {
	bdRevision := b.Revision
	if b.Revision == "" {
		b.Revision = newRevision()
	}
	payload, err := encodeRecord(b)
	if err != nil {
		return 0, "", err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	pRev, pFound, err := lockedRevision(ctx, tx, b.BeadAnchor)
	if err != nil {
		return 0, "", err
	}
	sBd, sPg, sFound, err := lockedSync(ctx, tx, b.BeadAnchor)
	if err != nil {
		return 0, "", err
	}
	sameRecord := false
	if pFound {
		pRec, err := scanRecord(tx.QueryRow(ctx, `SELECT record FROM work_items WHERE anchor = $1`, b.BeadAnchor))
		if err != nil {
			return 0, "", err
		}
		bCmp := b
		bCmp.Revision = bdRevision
		if sameRecord, err = sameExceptRevision(pRec, bCmp); err != nil {
			return 0, "", err
		}
	}

	write := false
	switch {
	case !sFound && !pFound:
		write = true
	case !sFound && sameRecord:
		// The same record on both sides (a revision the import assigned aside);
		// only the base is missing.
	case !sFound:
		return syncConflict, fmt.Sprintf("never synced, and Postgres (revision %q) holds a different record from bd (revision %q)",
			pRev, bdRevision), nil
	case bDigest == sBd:
		return syncInSync, "", nil // bd has not changed since the last sync: Postgres is current
	case !pFound || pRev == sPg:
		write = true // bd changed and Postgres did not
	default:
		return syncConflict, fmt.Sprintf("bd's record changed (its revision is now %q) and Postgres moved %q -> %q since the last sync",
			bdRevision, sPg, pRev), nil
	}

	outcome := syncInSync
	if write {
		var affected int64
		if pFound {
			tag, err := tx.Exec(ctx, `UPDATE work_items SET revision = $2, state = $3, record = $4::jsonb, written_at = now()
				WHERE anchor = $1`, b.BeadAnchor, b.Revision, string(b.State), payload)
			if err != nil {
				return 0, "", err
			}
			affected = tag.RowsAffected()
		} else {
			// No row to lock: a concurrent writer may insert one between our read
			// and this insert. It wins, and this anchor is a conflict.
			tag, err := tx.Exec(ctx, `INSERT INTO work_items (anchor, revision, state, record) VALUES ($1, $2, $3, $4::jsonb)
				ON CONFLICT (anchor) DO NOTHING`, b.BeadAnchor, b.Revision, string(b.State), payload)
			if err != nil {
				return 0, "", err
			}
			affected = tag.RowsAffected()
		}
		if affected != 1 {
			return syncConflict, "a record appeared in Postgres while this one was being imported", nil
		}
		if q != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO work_items_quarantine (anchor, reason, original_state, last_write_at, bd_record)
				VALUES ($1, $2, $3, $4, $5::jsonb)`, b.BeadAnchor, q.Reason, string(q.OriginalState), q.LastWrite, q.BdRecord); err != nil {
				return 0, "", err
			}
		}
		outcome = syncWritten
	}
	pgRevision := b.Revision
	if !write {
		pgRevision = pRev
	}
	if err := upsertSync(ctx, tx, b.BeadAnchor, bDigest, bdRevision, pgRevision); err != nil {
		return 0, "", err
	}
	if dryRun {
		return outcome, "", nil // the deferred Rollback discards it
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return outcome, "", nil
}

// Sides a conflict can be resolved to.
const (
	TakeBd       = "bd"
	TakePostgres = "postgres"
)

// resolvedTakePostgres is the pg_revision a take-postgres resolution records. It
// matches no real revision, so the export sees Postgres as changed since the
// sync (and writes it to bd), while bd_digest records bd as it is now (so the
// import sees bd unchanged and keeps Postgres).
const resolvedTakePostgres = "resolved:take-postgres"

// ResolveConflict settles one anchor that the import or export reports as a
// CONFLICT (`gonk-gate workstore-resolve --anchor A --take bd|postgres`). A
// human decides; this makes the decision stick in both directions:
//
//   - take bd: bd's current record is written to work_items (a fresh revision if
//     bd's is empty; never quarantined -- the human chose it) and the sync base
//     records the two as agreeing. The replaced Postgres record is logged at WARN
//     in full.
//   - take postgres: work_items is left as it is; the sync base records bd's
//     current record as seen and Postgres as moved since, so the next import
//     keeps Postgres and the next export writes it to bd.
func (p *Postgres) ResolveConflict(ctx context.Context, bd *BdCLI, anchor, take string, log *slog.Logger) error {
	if take != TakeBd && take != TakePostgres {
		return fmt.Errorf("beadstore: resolve %s: --take must be %q or %q, not %q", anchor, TakeBd, TakePostgres, take)
	}
	id, ok, err := bd.findBeadID(ctx, anchor)
	if err != nil {
		return fmt.Errorf("beadstore: resolve %s: find bd bead: %w", anchor, err)
	}
	var br bdRecord
	if ok {
		if br, err = bd.readRecord(ctx, id); err != nil {
			return fmt.Errorf("beadstore: resolve %s: read bd record: %w", anchor, err)
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, pFound, err := lockedRevision(ctx, tx, anchor)
	if err != nil {
		return err
	}
	switch take {
	case TakeBd:
		if !br.Found {
			return fmt.Errorf("beadstore: resolve %s: --take bd, but bd holds no record for it", anchor)
		}
		b := br.Record
		b.BeadAnchor = anchor
		bdRevision := b.Revision
		if b.Revision == "" {
			b.Revision = newRevision()
		}
		payload, err := encodeRecord(b)
		if err != nil {
			return err
		}
		if pFound {
			old, err := scanRecord(tx.QueryRow(ctx, `SELECT record FROM work_items WHERE anchor = $1`, anchor))
			if err != nil {
				return err
			}
			log.Warn("workstore-resolve: replacing the Postgres record with bd's", "anchor", anchor, "replaced", old)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_items (anchor, revision, state, record) VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (anchor) DO UPDATE SET revision = EXCLUDED.revision, state = EXCLUDED.state,
			record = EXCLUDED.record, written_at = now()`, anchor, b.Revision, string(b.State), payload); err != nil {
			return err
		}
		if err := upsertSync(ctx, tx, anchor, bdDigest(br.Raw), bdRevision, b.Revision); err != nil {
			return err
		}
	case TakePostgres:
		if !pFound {
			return fmt.Errorf("beadstore: resolve %s: --take postgres, but the work store holds no record for it", anchor)
		}
		digest, bdRevision := "", ""
		if br.Found {
			digest, bdRevision = bdDigest(br.Raw), br.Record.Revision
		}
		if err := upsertSync(ctx, tx, anchor, digest, bdRevision, resolvedTakePostgres); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ExportToBd writes back to bd every work item the Postgres-backed gonk-gate
// changed since the last sync, so a rollback to the bd-backed binary finds bd
// current. Run it from the live controller before reverting the image
// (`gonk-gate workstore-export`). Records are written with BdCLI.Put, which
// restamps UpdatedAt (except a pending-prompt reservation's) and assigns a bd
// revision; the sync row records it, so the next import sees bd unchanged.
func (p *Postgres) ExportToBd(ctx context.Context, bd *BdCLI, log *slog.Logger) (BdSyncReport, error) {
	rep := BdSyncReport{Direction: "export"}
	anchors, _, err := enumerateBd(ctx, bd)
	if err != nil {
		return rep, fmt.Errorf("beadstore: enumerate bd work records: %w", err)
	}
	items, err := p.allRecords(ctx)
	if err != nil {
		return rep, err
	}
	for _, pr := range items {
		anchor := pr.BeadAnchor
		rep.Seen++
		ids := anchors[anchor]
		if len(ids) > 1 {
			rep.Duplicates = append(rep.Duplicates, anchor)
		}
		br, err := bdRecordOf(ctx, bd, anchor, ids)
		b, bFound := br.Record, br.Found
		switch {
		case errors.Is(err, errUndecodableRecord):
			rep.Undecodable = append(rep.Undecodable, anchor)
			log.Error("bd export: bd's record for this anchor does not decode; not overwriting it", "anchor", anchor, "err", err)
			continue
		case err != nil:
			rep.Failed = append(rep.Failed, anchor)
			log.Error("bd export: could not read the bd record", "anchor", anchor, "err", err)
			continue
		}
		sBd, sPg, sFound, err := p.syncRow(ctx, anchor)
		if err != nil {
			rep.Failed = append(rep.Failed, anchor)
			log.Error("bd export: could not read the sync base", "anchor", anchor, "err", err)
			continue
		}

		bDigest, same := "", false
		if bFound {
			bDigest = bdDigest(br.Raw)
			if same, err = sameExceptRevision(b, pr); err != nil {
				return rep, err
			}
		}

		write, recordBase := false, false
		detail := ""
		switch {
		case sFound && pr.Revision == sPg:
			// Postgres unchanged since the last sync: bd is current (or newer).
		case sFound && ((bFound && bDigest == sBd) || (!bFound && sBd == "")):
			write = true // bd unchanged since the sync (still absent, or the same comment)
		case sFound:
			detail = fmt.Sprintf("bd's record changed or vanished (found=%v, revision %q) and Postgres moved %q -> %q since the last sync",
				bFound, b.Revision, sPg, pr.Revision)
		case !bFound:
			write = true
		case same:
			recordBase = true
		default:
			detail = fmt.Sprintf("never synced, and Postgres (revision %q) holds a different record from bd (revision %q)",
				pr.Revision, b.Revision)
		}
		switch {
		case detail != "":
			rep.Conflicts = append(rep.Conflicts, anchor)
			log.Error("bd export: CONFLICT -- bd and the Postgres work store both changed this record since they "+
				"last agreed; bd was NOT overwritten, a human must decide", "anchor", anchor, "detail", detail)
		case write:
			if err := bd.Put(ctx, pr); err != nil {
				rep.Failed = append(rep.Failed, anchor)
				log.Error("bd export: bd write failed", "anchor", anchor, "err", err)
				continue
			}
			id, ok, err := bd.findBeadID(ctx, anchor)
			var after bdRecord
			if err == nil && ok {
				after, err = bd.readRecord(ctx, id)
			}
			if err != nil || !ok || !after.Found || after.Record.State != pr.State ||
				after.Record.SessionID != pr.SessionID || after.Record.Attempt != pr.Attempt {
				rep.Failed = append(rep.Failed, anchor)
				log.Error("bd export: the record read back from bd is not the one written", "anchor", anchor,
					"found", ok && after.Found, "err", err, "state", string(after.Record.State), "want_state", string(pr.State))
				continue
			}
			if err := p.setSync(ctx, anchor, bdDigest(after.Raw), after.Record.Revision, pr.Revision); err != nil {
				rep.Failed = append(rep.Failed, anchor)
				log.Error("bd export: written to bd, but the sync base could not be recorded", "anchor", anchor, "err", err)
				continue
			}
			rep.Written++
			log.Info("bd export: record written back to bd", "anchor", anchor, "state", string(pr.State), "attempt", pr.Attempt)
		case recordBase:
			if err := p.setSync(ctx, anchor, bDigest, b.Revision, pr.Revision); err != nil {
				rep.Failed = append(rep.Failed, anchor)
				continue
			}
			rep.InSync++
		default:
			rep.InSync++
		}
	}
	return rep, nil
}

func lockedRevision(ctx context.Context, tx pgx.Tx, anchor string) (string, bool, error) {
	var rev string
	err := tx.QueryRow(ctx, `SELECT revision FROM work_items WHERE anchor = $1 FOR UPDATE`, anchor).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return rev, err == nil, err
}

// lockedSync returns the sync base: bd's record digest and the work_items
// revision at the last sync.
func lockedSync(ctx context.Context, tx pgx.Tx, anchor string) (bdDigest, pgRev string, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT bd_digest, pg_revision FROM work_items_bd_sync WHERE anchor = $1 FOR UPDATE`,
		anchor).Scan(&bdDigest, &pgRev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return bdDigest, pgRev, err == nil, err
}

// sameExceptRevision reports whether two records are the same write, ignoring
// Revision: a record the import copied from bd carries a revision the import
// assigned when bd's was empty. It compares within one binary, so the struct's
// JSON is a fair comparison here (it is never stored; the sync base digests bd's
// raw text instead).
func sameExceptRevision(a, b Record) (bool, error) {
	a.Revision, b.Revision = "", ""
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ja) == string(jb), nil
}

// execer is what both a pool and a transaction offer.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// upsertSync records a sync base. bd_revision is informational (for a human
// reading the table); bd_digest is what the merge compares.
func upsertSync(ctx context.Context, q execer, anchor, bdDigest, bdRev, pgRev string) error {
	_, err := q.Exec(ctx, `INSERT INTO work_items_bd_sync (anchor, bd_digest, bd_revision, pg_revision) VALUES ($1, $2, $3, $4)
		ON CONFLICT (anchor) DO UPDATE SET bd_digest = EXCLUDED.bd_digest, bd_revision = EXCLUDED.bd_revision,
		pg_revision = EXCLUDED.pg_revision, synced_at = now()`, anchor, bdDigest, bdRev, pgRev)
	return err
}

func (p *Postgres) setSync(ctx context.Context, anchor, bdDigest, bdRev, pgRev string) error {
	return upsertSync(ctx, p.pool, anchor, bdDigest, bdRev, pgRev)
}

func (p *Postgres) syncRow(ctx context.Context, anchor string) (bdDigest, pgRev string, found bool, err error) {
	err = p.pool.QueryRow(ctx, `SELECT bd_digest, pg_revision FROM work_items_bd_sync WHERE anchor = $1`,
		anchor).Scan(&bdDigest, &pgRev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return bdDigest, pgRev, err == nil, err
}

func (p *Postgres) allRecords(ctx context.Context) ([]Record, error) {
	rows, err := p.pool.Query(ctx, `SELECT record FROM work_items ORDER BY anchor COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("beadstore: read work items: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
