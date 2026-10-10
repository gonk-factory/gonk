package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

// workStoreDSNFileEnv names the FILE holding the work store's Postgres DSN.
const workStoreDSNFileEnv = "GONK_WORKSTORE_DSN_FILE"

// workStoreOpenTimeout bounds connecting to the work store and checking it,
// inside a dispatch's or sweep's order budget.
const workStoreOpenTimeout = 15 * time.Second

// errWorkStoreMisconfigured marks a selection error that no retry will fix
// (exit 2), as opposed to a store that could not be reached (exit 1).
var errWorkStoreMisconfigured = errors.New("work store misconfigured")

// workStore is what a Postgres work store offers beyond beadstore.Store.
type workStore interface {
	beadstore.Store
	BdImportCompleted(ctx context.Context) (bool, error)
	Close()
}

// workStoreOpener opens the Postgres work store; a test substitutes a fake.
type workStoreOpener func(ctx context.Context, dsn string) (workStore, error)

func openPostgresWorkStore(ctx context.Context, dsn string) (workStore, error) {
	return beadstore.OpenPostgres(ctx, dsn)
}

// selectWorkStore decides what dispatch and sweep keep their state in
// (gonk-t41), and returns the store with its release func.
//
// EXPAND THEN CONTRACT (owner decision on the gonk-t41 review; gonk-4cox). THE
// MOUNT DECIDES, so this binary is safe under either chart:
//
//  1. GONK_WORKSTORE_DSN_FILE set (only the chart release that mounts the
//     gonk-workstore Secret sets it): the Postgres work store. An unreadable or
//     empty DSN file is a misconfiguration. So is a store no bd import has
//     completed against, and -- while GONK_BEAD_REPO_DIR is set -- a pod whose
//     own import did not run (no stamp): that chart's bootstrap runs `gonk-gate
//     workstore-import` before gc starts, so these refusals are reachable only if
//     that bootstrap was bypassed. A break-glass stamp is accepted, loudly.
//  2. unset, GONK_BEAD_REPO_DIR set (every chart up to that release): the bd
//     store, exactly as the gonk-gate before this one ran. An image that lands
//     ahead of its chart therefore keeps running on bd -- the status quo, no
//     outage, and no dispatch accepted by gc and then dropped by exit 2. The
//     cost, accepted by the owner: bd's duplicate risk (gonk-vred) persists until
//     the chart release lands.
//  3. neither: the in-memory store, with the warning it always had -- a smoke
//     test, never the controller.
func selectWorkStore(ctx context.Context, log *slog.Logger, dsnFile, beadRepoDir, bdBin string, open workStoreOpener) (beadstore.Store, func(), error) {
	switch {
	case dsnFile != "":
		dsn, err := readSecretFile(dsnFile)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", errWorkStoreMisconfigured, workStoreDSNFileEnv, err)
		}
		octx, cancel := context.WithTimeout(ctx, workStoreOpenTimeout)
		defer cancel()
		s, err := open(octx, dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open the Postgres work store: %w", err)
		}
		done, err := s.BdImportCompleted(octx)
		if err != nil {
			s.Close()
			return nil, nil, fmt.Errorf("check the work store's bd import: %w", err)
		}
		if !done {
			s.Close()
			return nil, nil, fmt.Errorf("%w: no bd import has completed against the Postgres work store, so it "+
				"does not hold the records bd did; the controller's bootstrap runs `gonk-gate workstore-import` "+
				"before gc starts (docs/reference/gonk-state-record.md, \"Cutover from bd, and rollback\")", errWorkStoreMisconfigured)
		}
		// While bd is still where the records came from (GONK_BEAD_REPO_DIR set),
		// an import must have completed IN THIS POD, not merely once: the import
		// is also what takes in writes a rolled-back bd-backed binary made, and a
		// pod whose bootstrap skipped it (its capability probe failed for any
		// reason) would otherwise run on Postgres without them.
		if beadRepoDir != "" {
			stamp, err := readImportStamp(beadRepoDir)
			if err != nil {
				s.Close()
				return nil, nil, fmt.Errorf("%w: no bd import completed in this pod (%v); the controller's "+
					"bootstrap runs `gonk-gate workstore-import` before gc starts, and it did not complete here",
					errWorkStoreMisconfigured, err)
			}
			if stamp.BreakGlass {
				log.Warn("BREAK GLASS: this pod's bd import did NOT complete and the controller was started anyway "+
					"(GONK_WORKSTORE_IMPORT_BREAK_GLASS). The work store holds the last complete import; writes a "+
					"rolled-back bd-backed gonk-gate made since then are NOT in it. Fix bd, then remove the break-glass "+
					"value so the next pod imports.", "import_error", stamp.Error, "stamped_at", stamp.ImportedAt)
			}
		}
		return s, s.Close, nil
	case beadRepoDir != "":
		return &beadstore.BdCLI{Bin: bdBin, Dir: beadRepoDir}, func() {}, nil
	default:
		log.Warn("neither " + workStoreDSNFileEnv + " nor GONK_BEAD_REPO_DIR is set; using an in-memory store " +
			"that does NOT survive between gonk-gate invocations -- fine for a smoke test, never for the controller")
		return beadstore.NewMemory(), func() {}, nil
	}
}

// workStoreExitCode is 2 for a misconfiguration and 1 for anything a retry
// could fix (the store was unreachable).
func workStoreExitCode(err error) int {
	if errors.Is(err, errWorkStoreMisconfigured) {
		return 2
	}
	return 1
}

// gateCapabilities are the subcommands the chart and the pack probe for with
// `gonk-gate capabilities` before relying on them. The controller's bootstrap
// runs the bd import only when this binary lists workstore-import: under
// gonk-xsk3 the chart can land before the image, and an older gonk-gate has no
// such subcommand.
//
// http-probe is here for the same reason and one more (gonk-hckm): the pack
// ships inside the image, but pack/doctor/meter-reachable/run.sh has to tell
// "this gonk-gate is too old to carry the probe" apart from "the meter is
// sick", and an unknown subcommand would otherwise fall through to
// loadGateConfig and exit 2 with the word "misconfiguration" -- which is
// exactly the unrunnable-check-reported-as-a-failed-check shape that bug was.
var gateCapabilities = []string{"workstore-import", "workstore-export", "workstore-survey", "workstore-resolve", "http-probe"}

// importStampName is written into GONK_BEAD_REPO_DIR (the controller's /city,
// an emptyDir shared by the bootstrap initContainer and the supervisor, fresh in
// every pod) when an import completes. selectWorkStore requires it.
const importStampName = ".gonk-workstore-imported"

func importStampPath(beadRepoDir string) string { return filepath.Join(beadRepoDir, importStampName) }

// runCapabilities prints one capability per line on stdout. It needs no
// configuration and touches nothing.
func runCapabilities(w io.Writer) int {
	for _, c := range gateCapabilities {
		if _, err := fmt.Fprintln(w, c); err != nil {
			return 1
		}
	}
	return 0
}

// workStoreSyncTimeout bounds a whole import or export. The import runs in the
// controller's bootstrap initContainer, which has no deadline of its own, and a
// bd call that hangs must not hang the pod forever.
const workStoreSyncTimeout = 30 * time.Minute

// importBreakGlassEnv, set to "1", lets the controller start when this pod's bd
// import cannot complete (review defect A3). See runWorkStoreSync.
const importBreakGlassEnv = "GONK_WORKSTORE_IMPORT_BREAK_GLASS"

// importStamp is the pod-local record that an import ran in this pod: written
// to $GONK_BEAD_REPO_DIR/.gonk-workstore-imported, read by selectWorkStore.
type importStamp struct {
	ImportedAt time.Time              `json:"imported_at"`
	Report     beadstore.BdSyncReport `json:"report"`
	// BreakGlass: the import did NOT complete; the operator's break-glass let
	// the pod start on the last complete import. Error says why it failed.
	BreakGlass bool   `json:"break_glass,omitempty"`
	Error      string `json:"error,omitempty"`
}

func writeImportStamp(beadRepoDir string, st importStamp) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(importStampPath(beadRepoDir), append(b, '\n'), 0o644)
}

func readImportStamp(beadRepoDir string) (importStamp, error) {
	var st importStamp
	b, err := os.ReadFile(importStampPath(beadRepoDir))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s is not a valid import stamp: %w", importStampPath(beadRepoDir), err)
	}
	return st, nil
}

// syncStore is what the cutover subcommands need from the work store; a test
// substitutes a fake.
type syncStore interface {
	ImportFromBd(ctx context.Context, bd *beadstore.BdCLI, log *slog.Logger, opts beadstore.ImportOptions) (beadstore.BdSyncReport, error)
	ExportToBd(ctx context.Context, bd *beadstore.BdCLI, log *slog.Logger) (beadstore.BdSyncReport, error)
	BdImportCompleted(ctx context.Context) (bool, error)
	Close()
}

type syncStoreOpener func(ctx context.Context, dsn string) (syncStore, error)

func openPostgresSyncStore(ctx context.Context, dsn string) (syncStore, error) {
	return beadstore.OpenPostgres(ctx, dsn)
}

// freshInstallEnv, set to "1", is the positive signal that this pod belongs to
// a release Helm is INSTALLING, not upgrading: the chart renders it only under
// .Release.IsInstall. It is the only thing that lets an import read bd's
// "database not found" as a fresh install's empty import; without it that is an
// error, because a wrong dolt_database on an existing install says the same
// thing (beadstore.ImportOptions.FreshInstall; verification defect 1).
const freshInstallEnv = "GONK_WORKSTORE_FRESH_INSTALL"

// syncConfig is everything the cutover subcommands read from the environment.
type syncConfig struct {
	DSNFile, BeadRepoDir, BdBin string
	BreakGlass                  bool
	FreshInstall                bool
}

func (c syncConfig) importOptions() beadstore.ImportOptions {
	return beadstore.ImportOptions{FreshInstall: c.FreshInstall}
}

func syncConfigFromEnv() syncConfig {
	return syncConfig{
		DSNFile:      os.Getenv(workStoreDSNFileEnv),
		BeadRepoDir:  os.Getenv("GONK_BEAD_REPO_DIR"),
		BdBin:        os.Getenv("GONK_BD_BIN"),
		BreakGlass:   os.Getenv(importBreakGlassEnv) == "1",
		FreshInstall: os.Getenv(freshInstallEnv) == "1",
	}
}

// runWorkStoreSync is `gonk-gate workstore-import` and `gonk-gate
// workstore-export` (gonk-t41; docs/plans/2026-09-14-t41-postgres-workstore.md
// sections 6 and 11). It reads only what it needs -- the work store DSN file,
// and bd's location -- so it runs in the bootstrap initContainer, which has none
// of dispatch's configuration.
//
// Exit codes: 2 misconfigured; 1 the run did not complete (the store or bd
// could not be reached, or an anchor could not be read or written) -- and, for
// an export, any conflict, because an export exists to make a rollback safe and
// a conflict means it did not; 0 otherwise. An import's conflicts are logged at
// ERROR and do not fail it: the anchor keeps the Postgres record, and failing the
// import would keep the whole controller down over one record.
//
// BREAK-GLASS (import only; GONK_WORKSTORE_IMPORT_BREAK_GLASS=1, chart value
// workstore.importBreakGlass). Without it, an import that cannot complete -- a
// Dolt outage, an anchor bd will not return -- fails the controller's
// initContainer on every pod start. With it, such an import still exits 0 and
// stamps the pod `break_glass`, so the controller starts and runs on POSTGRES,
// on the last complete import; every gonk-gate invocation in the pod then says
// so at WARN. It never puts the controller on bd for state. It is REFUSED for a
// store no import has ever completed against (it would start on an empty store:
// duplicate sessions, orphaned agents); there the remedy is to remove the
// work-store mount, which puts the binary back on bd -- safe exactly because
// Postgres holds nothing yet.
func runWorkStoreSync(ctx context.Context, direction string, log *slog.Logger, cfg syncConfig, open syncStoreOpener) int {
	if cfg.DSNFile == "" || cfg.BeadRepoDir == "" {
		log.Error("misconfiguration: "+direction+" needs "+workStoreDSNFileEnv+" and GONK_BEAD_REPO_DIR",
			"dsn_file_set", cfg.DSNFile != "", "bead_repo_dir", cfg.BeadRepoDir)
		return 2
	}
	dsn, err := readSecretFile(cfg.DSNFile)
	if err != nil {
		log.Error("misconfiguration: the work store DSN file", "err", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, workStoreSyncTimeout)
	defer cancel()
	store, err := open(ctx, dsn)
	if err != nil {
		log.Error(direction+": cannot open the Postgres work store", "err", err)
		return 1
	}
	defer store.Close()
	bd := &beadstore.BdCLI{Bin: cfg.BdBin, Dir: cfg.BeadRepoDir}

	start := time.Now()
	var rep beadstore.BdSyncReport
	if direction == "workstore-export" {
		rep, err = store.ExportToBd(ctx, bd, log)
	} else {
		rep, err = store.ImportFromBd(ctx, bd, log, cfg.importOptions())
	}
	elapsed := time.Since(start).Round(time.Millisecond).String()
	failure := ""
	switch {
	case err != nil:
		failure = err.Error()
	case !rep.Complete():
		failure = fmt.Sprintf("%d anchor(s) could not be read or written: %v", len(rep.Failed), rep.Failed)
	}

	if direction == "workstore-export" {
		switch {
		case failure != "":
			log.Error("workstore-export did not complete; re-run it", "err", failure, "report", rep, "elapsed", elapsed)
			return 1
		case len(rep.Conflicts) > 0:
			log.Error("workstore-export: CONFLICTS -- bd was not brought up to date for these anchors, so rolling back "+
				"to the bd-backed gonk-gate is NOT safe until a human resolves them (gonk-gate workstore-resolve)",
				"report", rep, "elapsed", elapsed)
			return 1
		}
		log.Info("workstore-export complete", "report", rep, "elapsed", elapsed)
		return 0
	}

	stamp := importStamp{ImportedAt: time.Now().UTC(), Report: rep}
	if failure != "" {
		if !cfg.BreakGlass {
			log.Error("workstore-import did not complete; the controller will not start until it does. If bd "+
				"cannot be fixed now, see the break-glass in docs/reference/gonk-state-record.md",
				"err", failure, "report", rep, "elapsed", elapsed)
			return 1
		}
		done, derr := store.BdImportCompleted(ctx)
		if derr != nil || !done {
			log.Error("workstore-import did not complete and BREAK-GLASS IS REFUSED: no import has ever completed "+
				"against this work store, so starting would run on an empty store (duplicate sessions, orphaned "+
				"agents). Remove the work-store mount instead: the binary then runs on bd, which is safe while "+
				"Postgres holds nothing.", "err", failure, "marker_err", derr, "report", rep)
			return 1
		}
		stamp.BreakGlass, stamp.Error = true, failure
		log.Error("workstore-import did not complete; BREAK GLASS: starting the controller on the last complete "+
			"import. Writes a rolled-back bd-backed gonk-gate made since then are NOT in the work store.",
			"err", failure, "report", rep, "elapsed", elapsed)
	}
	if err := writeImportStamp(cfg.BeadRepoDir, stamp); err != nil {
		log.Error("workstore-import: its stamp could not be written; gonk-gate will refuse to dispatch in this pod",
			"path", importStampPath(cfg.BeadRepoDir), "err", err)
		return 1
	}
	if !stamp.BreakGlass {
		log.Info("workstore-import complete", "report", rep, "elapsed", elapsed)
	}
	return 0
}

// surveyOutput is workstore-survey's JSON: bd's survey, plus -- when the work
// store DSN is available -- the import plan against the store as it is now
// (what an import would write, quarantine and report as conflicts, with why).
type surveyOutput struct {
	beadstore.BdSurvey
	ImportPlan *beadstore.BdSyncReport `json:"import_plan,omitempty"`
}

// planStore is what the survey needs from the work store.
type planStore interface {
	PlanImportFromBd(ctx context.Context, bd *beadstore.BdCLI, log *slog.Logger, opts beadstore.ImportOptions) (beadstore.BdSyncReport, error)
	Close()
}

type planStoreOpener func(ctx context.Context, dsn string) (planStore, error)

func openPostgresPlanStore(ctx context.Context, dsn string) (planStore, error) {
	return beadstore.OpenPostgres(ctx, dsn)
}

// runWorkStoreSurvey is `gonk-gate workstore-survey`: what an import would find
// in bd, as JSON on stdout, writing nothing anywhere -- per-state counts, AGES,
// and the records an import would quarantine, with reasons. It needs only bd's
// location. With GONK_WORKSTORE_DSN_FILE it also plans the import against the
// store (import_plan): written, in sync, quarantined and CONFLICTS with their
// detail -- the one place a conflict is visible without reading pod logs.
// Exit 1 if any read failed or the plan reports conflicts, so a script notices.
func runWorkStoreSurvey(ctx context.Context, log *slog.Logger, cfg syncConfig, open planStoreOpener, stdout io.Writer) int {
	if cfg.BeadRepoDir == "" {
		log.Error("misconfiguration: workstore-survey needs GONK_BEAD_REPO_DIR")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, workStoreSyncTimeout)
	defer cancel()
	bd := &beadstore.BdCLI{Bin: cfg.BdBin, Dir: cfg.BeadRepoDir}
	sv, err := beadstore.SurveyBd(ctx, bd, log)
	if err != nil {
		log.Error("workstore-survey did not complete", "err", err)
		return 1
	}
	out := surveyOutput{BdSurvey: sv}
	if cfg.DSNFile != "" {
		dsn, err := readSecretFile(cfg.DSNFile)
		if err != nil {
			log.Error("misconfiguration: the work store DSN file", "err", err)
			return 2
		}
		store, err := open(ctx, dsn)
		if err != nil {
			log.Error("workstore-survey: cannot open the Postgres work store for the import plan", "err", err)
			return 1
		}
		defer store.Close()
		plan, err := store.PlanImportFromBd(ctx, bd, log, cfg.importOptions())
		if err != nil {
			log.Error("workstore-survey: the import plan did not complete", "err", err)
			return 1
		}
		out.ImportPlan = &plan
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 1
	}
	if _, err := fmt.Fprintln(stdout, string(b)); err != nil {
		return 1
	}
	if len(sv.Failed) > 0 || (out.ImportPlan != nil && (len(out.ImportPlan.Conflicts) > 0 || !out.ImportPlan.Complete())) {
		return 1
	}
	return 0
}

// resolveStore is what workstore-resolve needs.
type resolveStore interface {
	ResolveConflict(ctx context.Context, bd *beadstore.BdCLI, anchor, take string, log *slog.Logger) error
	Close()
}

type resolveStoreOpener func(ctx context.Context, dsn string) (resolveStore, error)

func openPostgresResolveStore(ctx context.Context, dsn string) (resolveStore, error) {
	return beadstore.OpenPostgres(ctx, dsn)
}

// runWorkStoreResolve is `gonk-gate workstore-resolve --anchor A --take
// bd|postgres`: a human's decision on a CONFLICT the import, export or survey
// reported (beadstore.Postgres.ResolveConflict).
func runWorkStoreResolve(ctx context.Context, args []string, log *slog.Logger, cfg syncConfig, open resolveStoreOpener) int {
	fs := flag.NewFlagSet("workstore-resolve", flag.ContinueOnError)
	anchor := fs.String("anchor", "", "the conflicted bead anchor, e.g. gonk:75:issue:73")
	take := fs.String("take", "", "which side wins: bd or postgres")
	if err := fs.Parse(args); err != nil || *anchor == "" || (*take != beadstore.TakeBd && *take != beadstore.TakePostgres) {
		log.Error("usage: gonk-gate workstore-resolve --anchor <anchor> --take bd|postgres", "err", err)
		return 2
	}
	if cfg.DSNFile == "" || cfg.BeadRepoDir == "" {
		log.Error("misconfiguration: workstore-resolve needs " + workStoreDSNFileEnv + " and GONK_BEAD_REPO_DIR")
		return 2
	}
	dsn, err := readSecretFile(cfg.DSNFile)
	if err != nil {
		log.Error("misconfiguration: the work store DSN file", "err", err)
		return 2
	}
	store, err := open(ctx, dsn)
	if err != nil {
		log.Error("workstore-resolve: cannot open the Postgres work store", "err", err)
		return 1
	}
	defer store.Close()
	if err := store.ResolveConflict(ctx, &beadstore.BdCLI{Bin: cfg.BdBin, Dir: cfg.BeadRepoDir}, *anchor, *take, log); err != nil {
		log.Error("workstore-resolve failed", "anchor", *anchor, "take", *take, "err", err)
		return 1
	}
	log.Info("workstore-resolve: conflict settled; the next import and export follow this choice", "anchor", *anchor, "take", *take)
	return 0
}
