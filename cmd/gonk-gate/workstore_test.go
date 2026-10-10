package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
)

type fakeWorkStore struct {
	*beadstore.Memory
	imported  bool
	importErr error
	closed    bool
}

func (f *fakeWorkStore) BdImportCompleted(context.Context) (bool, error) {
	return f.imported, f.importErr
}
func (f *fakeWorkStore) Close() { f.closed = true }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func dsnFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
}

// The controller path: the DSN file is read (one trailing newline trimmed, as
// every mounted secret) and handed to the opener, and the store that comes
// back is the one dispatch and sweep get.
func TestWorkStoreIsPostgresWhenTheDSNFileIsSet(t *testing.T) {
	fake := &fakeWorkStore{Memory: beadstore.NewMemory(), imported: true}
	var gotDSN string
	open := func(_ context.Context, dsn string) (workStore, error) { gotDSN = dsn; return fake, nil }

	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, importStampName), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, closeStore, err := selectWorkStore(context.Background(), quietLog(), dsnFile(t, "postgres://u:p@db.example.internal/db\n"), city, "", open)
	if err != nil {
		t.Fatalf("selectWorkStore = %v", err)
	}
	if gotDSN != "postgres://u:p@db.example.internal/db" {
		t.Fatalf("opener got DSN %q", gotDSN)
	}
	if s != beadstore.Store(fake) {
		t.Fatalf("store = %T, want the opened Postgres store", s)
	}
	closeStore()
	if !fake.closed {
		t.Fatal("the release func did not close the store")
	}
}

// A Postgres store that no bd import has completed against holds none of the
// records bd did. Running on it would show dispatch no reservation and the
// sweep no running session -- so it is refused, and the store is closed.
func TestWorkStoreRefusesAStoreWithNoCompletedBdImport(t *testing.T) {
	fake := &fakeWorkStore{Memory: beadstore.NewMemory(), imported: false}
	open := func(context.Context, string) (workStore, error) { return fake, nil }
	_, _, err := selectWorkStore(context.Background(), quietLog(), dsnFile(t, "postgres://x"), "", "", open)
	if err == nil || !strings.Contains(err.Error(), "workstore-import") {
		t.Fatalf("selectWorkStore = %v, want a refusal naming workstore-import", err)
	}
	// The refusal points at a document that ships, never at a plan.
	if !strings.Contains(err.Error(), "docs/reference/gonk-state-record.md") || strings.Contains(err.Error(), "docs/plans/") {
		t.Fatalf("selectWorkStore = %v, want the pointer to docs/reference/gonk-state-record.md and no docs/plans path", err)
	}
	if workStoreExitCode(err) != 2 || !fake.closed {
		t.Fatalf("exit code %d, closed %v: want 2 (misconfiguration) and the store closed", workStoreExitCode(err), fake.closed)
	}
}

// EXPAND THEN CONTRACT (owner decision; gonk-4cox). Every chart before the
// work-store release sets GONK_BEAD_REPO_DIR and no DSN. This binary under such a
// chart -- an image ahead of its chart -- runs on bd EXACTLY as the gonk-gate
// before it did: the same BdCLI, same bd binary and directory, no Postgres
// connection attempted, no refusal. Refusing here was the review's outage.
func TestWorkStoreWithoutTheMountIsBdAsBefore(t *testing.T) {
	open := func(context.Context, string) (workStore, error) {
		t.Fatal("no DSN mount, yet the Postgres opener was called")
		return nil, nil
	}
	s, closeStore, err := selectWorkStore(context.Background(), quietLog(), "", "/city", "/usr/local/bin/bd", open)
	if err != nil {
		t.Fatalf("selectWorkStore without the mount = %v; want the bd store", err)
	}
	defer closeStore()
	bd, ok := s.(*beadstore.BdCLI)
	if !ok || bd.Dir != "/city" || bd.Bin != "/usr/local/bin/bd" || bd.Run != nil {
		t.Fatalf("store = %#v; want BdCLI{Bin: /usr/local/bin/bd, Dir: /city} as before", s)
	}
}

// The binary's store construction under the previous chart matches the
// gonk-gate on the base commit: main.go still reads GONK_BD_BIN and
// GONK_BEAD_REPO_DIR, and with the chart's values the store is that BdCLI.
func TestGateConfigWithThePreviousChartSelectsBd(t *testing.T) {
	t.Setenv("GONK_CITY", "gonk")
	t.Setenv("GONK_METER_TOKEN_FILE", "/x")
	t.Setenv("GONK_BEAD_REPO_DIR", "/city") // what the previous chart sets
	t.Setenv(workStoreDSNFileEnv, "")
	cfg, err := loadGateConfig()
	if err != nil {
		t.Fatal(err)
	}
	s, closeStore, err := cfg.openWorkStore(context.Background(), quietLog())
	if err != nil {
		t.Fatalf("openWorkStore under the previous chart = %v", err)
	}
	defer closeStore()
	if bd, ok := s.(*beadstore.BdCLI); !ok || bd.Dir != "/city" {
		t.Fatalf("store = %#v, want BdCLI on /city", s)
	}
}

func TestWorkStoreDSNFileErrors(t *testing.T) {
	open := func(context.Context, string) (workStore, error) {
		t.Fatal("opener called with no usable DSN")
		return nil, nil
	}
	for name, path := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "absent"),
		"empty":   dsnFile(t, "\n"),
	} {
		_, _, err := selectWorkStore(context.Background(), quietLog(), path, "", "", open)
		if err == nil || workStoreExitCode(err) != 2 {
			t.Errorf("%s DSN file: err = %v (exit %d), want a misconfiguration", name, err, workStoreExitCode(err))
		}
	}
}

// An unreachable store is not a misconfiguration: exit 1, and the DSN -- which
// carries the password -- is not in the error.
func TestWorkStoreUnreachableIsExit1AndDoesNotLeakTheDSN(t *testing.T) {
	open := func(context.Context, string) (workStore, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	_, _, err := selectWorkStore(context.Background(), quietLog(), dsnFile(t, "postgres://gonk:hunter2@db.example.internal/db"), "", "", open)
	if err == nil || workStoreExitCode(err) != 1 {
		t.Fatalf("err = %v (exit %d), want exit 1", err, workStoreExitCode(err))
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the DSN: %v", err)
	}
}

// The real opener must not leak a malformed DSN either (pgx's own parse error
// can quote the connection string).
func TestOpenPostgresDoesNotLeakAMalformedDSN(t *testing.T) {
	_, err := beadstore.OpenPostgres(context.Background(), "postgres://gonk:hunter2@db.example.internal:notaport/db")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("OpenPostgres(malformed) = %v, want an error that does not quote the DSN", err)
	}
}

func TestWorkStoreIsMemoryOnlyWithNothingConfigured(t *testing.T) {
	s, closeStore, err := selectWorkStore(context.Background(), quietLog(), "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if _, ok := s.(*beadstore.Memory); !ok {
		t.Fatalf("store = %T, want Memory", s)
	}
}

// The env name must survive Gas City's exec-order strip: a marker in it and
// every dispatch reads an empty path.
func TestWorkStoreDSNEnvCarriesNoSecretMarker(t *testing.T) {
	for _, marker := range []string{"PASSWORD", "TOKEN", "SECRET", "PRIVATE_KEY", "PRIVATE-KEY", "API_KEY", "API-KEY",
		"ACCESS_KEY", "ACCESS-KEY", "CREDENTIAL", "OAUTH", "AUTH_JSON", "AUTH-JSON"} {
		if strings.Contains(strings.ToUpper(workStoreDSNFileEnv), marker) {
			t.Fatalf("%s contains Gas City secret marker %q and would be stripped from exec orders", workStoreDSNFileEnv, marker)
		}
	}
	t.Setenv("GONK_CITY", "gonk")
	t.Setenv("GONK_METER_TOKEN_FILE", "/x")
	t.Setenv(workStoreDSNFileEnv, "/etc/gonk/secrets/ledger/dsn")
	cfg, err := loadGateConfig()
	if err != nil || cfg.WorkStoreDSNFile != "/etc/gonk/secrets/ledger/dsn" {
		t.Fatalf("loadGateConfig WorkStoreDSNFile = %q, %v", cfg.WorkStoreDSNFile, err)
	}
}

// The chart's bootstrap runs the bd import only if `gonk-gate capabilities`
// prints workstore-import as a whole line on stdout (grep -qx). An older
// gonk-gate prints nothing there, and the import is skipped.
func TestCapabilitiesListsTheCutoverSubcommandsOneWholeLineEach(t *testing.T) {
	var out strings.Builder
	if code := runCapabilities(&out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for _, want := range []string{"workstore-import", "workstore-export"} {
		found := false
		for _, l := range lines {
			found = found || l == want
		}
		if !found {
			t.Fatalf("capabilities output %q has no line %q", out.String(), want)
		}
	}
}

// The import runs in an initContainer with only the DSN file and bd's location.
// Missing either is a misconfiguration (exit 2), decided before connecting to
// anything.
func TestWorkStoreSyncMisconfiguration(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct{ dsn, repo string }{
		"no DSN file":      {"", "/city"},
		"no bead repo dir": {dsnFile(t, "postgres://x"), ""},
		"unreadable DSN":   {filepath.Join(t.TempDir(), "absent"), "/city"},
		"empty DSN":        {dsnFile(t, ""), "/city"},
	} {
		for _, dir := range []string{"workstore-import", "workstore-export"} {
			open := func(context.Context, string) (syncStore, error) {
				t.Fatal("opener called on a misconfiguration")
				return nil, nil
			}
			if code := runWorkStoreSync(ctx, dir, quietLog(), syncConfig{DSNFile: c.dsn, BeadRepoDir: c.repo}, open); code != 2 {
				t.Errorf("%s %s: exit %d, want 2", dir, name, code)
			}
		}
	}
}

// An unreachable store is exit 1 (retry), not 2.
func TestWorkStoreSyncUnreachableStoreIsExit1(t *testing.T) {
	code := runWorkStoreSync(context.Background(), "workstore-import", quietLog(),
		syncConfig{DSNFile: dsnFile(t, "postgres://gonk:pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=2"), BeadRepoDir: "/city"},
		openPostgresSyncStore)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// The import must have completed IN THIS POD while bd is still the source: the
// stamp on the pod's /city emptyDir. A store marked imported by an earlier pod
// is not enough -- this pod's bootstrap may have skipped the import (a failed
// capability probe), and then writes a rolled-back bd-backed binary made would
// never be merged.
func TestWorkStoreRefusesAPodWhoseImportDidNotRun(t *testing.T) {
	fake := &fakeWorkStore{Memory: beadstore.NewMemory(), imported: true}
	open := func(context.Context, string) (workStore, error) { return fake, nil }
	_, _, err := selectWorkStore(context.Background(), quietLog(), dsnFile(t, "postgres://x"), t.TempDir(), "", open)
	if err == nil || !strings.Contains(err.Error(), "this pod") || workStoreExitCode(err) != 2 || !fake.closed {
		t.Fatalf("err = %v (exit %d, closed %v); want a misconfiguration naming this pod's import, store closed",
			err, workStoreExitCode(err), fake.closed)
	}
}

func TestWorkStoreSurveyNeedsTheBeadRepoDir(t *testing.T) {
	var out strings.Builder
	if code := runWorkStoreSurvey(context.Background(), quietLog(), syncConfig{}, openPostgresPlanStore, &out); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// fakeSyncStore scripts an import's result and the store's import marker.
type fakeSyncStore struct {
	rep        beadstore.BdSyncReport
	err        error
	imported   bool
	closed     bool
	importOpts *beadstore.ImportOptions
}

func (f *fakeSyncStore) ImportFromBd(_ context.Context, _ *beadstore.BdCLI, _ *slog.Logger, opts beadstore.ImportOptions) (beadstore.BdSyncReport, error) {
	f.importOpts = &opts
	return f.rep, f.err
}
func (f *fakeSyncStore) ExportToBd(context.Context, *beadstore.BdCLI, *slog.Logger) (beadstore.BdSyncReport, error) {
	return f.rep, f.err
}
func (f *fakeSyncStore) BdImportCompleted(context.Context) (bool, error) { return f.imported, nil }
func (f *fakeSyncStore) Close()                                          { f.closed = true }

func runImport(t *testing.T, f *fakeSyncStore, breakGlass bool) (code int, city string) {
	t.Helper()
	city = t.TempDir()
	open := func(context.Context, string) (syncStore, error) { return f, nil }
	code = runWorkStoreSync(context.Background(), "workstore-import", quietLog(),
		syncConfig{DSNFile: dsnFile(t, "postgres://x"), BeadRepoDir: city, BreakGlass: breakGlass}, open)
	return code, city
}

// REVIEW DEFECT A3: an import that cannot complete after the cutover (Dolt
// down, an anchor bd will not return) failed the initContainer on every pod
// start, with no way out. Without break-glass that is still the default -- the
// pod must not start blind -- but the operator now has one.
func TestImportFailureWithoutBreakGlassFailsAndStampsNothing(t *testing.T) {
	for name, f := range map[string]*fakeSyncStore{
		"bd unreachable": {err: errors.New("enumerate bd: connection refused"), imported: true},
		"anchor unread":  {rep: beadstore.BdSyncReport{Direction: "import", Failed: []string{"gonk:1:issue:1"}}, imported: true},
	} {
		code, city := runImport(t, f, false)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", name, code)
		}
		if _, err := readImportStamp(city); err == nil {
			t.Errorf("%s: a failed import stamped the pod", name)
		}
	}
}

// With break-glass, on a store an import HAS completed against: exit 0, the pod
// stamp says break_glass with the error, and selectWorkStore runs on POSTGRES
// (never bd) and warns on every invocation.
func TestImportBreakGlassStartsOnPostgresAndSaysSo(t *testing.T) {
	f := &fakeSyncStore{err: errors.New("enumerate bd: connection refused"), imported: true}
	code, city := runImport(t, f, true)
	if code != 0 {
		t.Fatalf("break-glass import exit %d, want 0", code)
	}
	st, err := readImportStamp(city)
	if err != nil || !st.BreakGlass || !strings.Contains(st.Error, "connection refused") {
		t.Fatalf("stamp = %+v, %v; want break_glass with the error", st, err)
	}

	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ws := &fakeWorkStore{Memory: beadstore.NewMemory(), imported: true}
	s, closeStore, err := selectWorkStore(context.Background(), log, dsnFile(t, "postgres://x"), city, "",
		func(context.Context, string) (workStore, error) { return ws, nil })
	if err != nil {
		t.Fatalf("selectWorkStore after break-glass = %v", err)
	}
	defer closeStore()
	if s != beadstore.Store(ws) {
		t.Fatalf("store = %T, want the Postgres work store (break-glass never selects bd)", s)
	}
	if !strings.Contains(logs.String(), "BREAK GLASS") {
		t.Fatalf("no BREAK GLASS warning on a break-glass pod: %q", logs.String())
	}
}

// Break-glass is REFUSED on a store no import has ever completed against: it
// would start the controller on an empty store.
func TestImportBreakGlassRefusedOnANeverImportedStore(t *testing.T) {
	f := &fakeSyncStore{err: errors.New("enumerate bd: connection refused"), imported: false}
	code, city := runImport(t, f, true)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if _, err := readImportStamp(city); err == nil {
		t.Fatal("a refused break-glass stamped the pod")
	}
}

// A complete import stamps the pod without break_glass.
func TestImportCompleteStampsThePod(t *testing.T) {
	f := &fakeSyncStore{rep: beadstore.BdSyncReport{Direction: "import", Seen: 3, Written: 3}}
	code, city := runImport(t, f, false)
	st, err := readImportStamp(city)
	if code != 0 || err != nil || st.BreakGlass || st.Report.Written != 3 {
		t.Fatalf("exit %d, stamp %+v, %v", code, st, err)
	}
}

func TestBreakGlassEnvCarriesNoSecretMarker(t *testing.T) {
	for _, marker := range []string{"PASSWORD", "TOKEN", "SECRET", "PRIVATE_KEY", "API_KEY", "ACCESS_KEY", "CREDENTIAL", "OAUTH", "AUTH_JSON"} {
		if strings.Contains(importBreakGlassEnv, marker) {
			t.Fatalf("%s contains %q and would be stripped from exec orders", importBreakGlassEnv, marker)
		}
	}
	t.Setenv(importBreakGlassEnv, "1")
	if !syncConfigFromEnv().BreakGlass {
		t.Fatal("syncConfigFromEnv ignores the break-glass env")
	}
}

// Verification defect 1: the import may read bd's "database not found" as a
// fresh install only on a positive signal. The signal is the env the chart sets
// under .Release.IsInstall, and it reaches the import only when it is exactly
// "1"; unset (every upgrade, and every chart before release 2) it is false.
func TestFreshInstallSignalReachesTheImportOnlyFromItsEnv(t *testing.T) {
	for _, marker := range []string{"PASSWORD", "TOKEN", "SECRET", "PRIVATE_KEY", "API_KEY", "ACCESS_KEY", "CREDENTIAL", "OAUTH", "AUTH_JSON"} {
		if strings.Contains(freshInstallEnv, marker) {
			t.Fatalf("%s contains %q and would be stripped from exec orders", freshInstallEnv, marker)
		}
	}
	for val, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		t.Setenv(freshInstallEnv, val)
		cfg := syncConfigFromEnv()
		cfg.DSNFile, cfg.BeadRepoDir = dsnFile(t, "postgres://x"), t.TempDir()
		f := &fakeSyncStore{rep: beadstore.BdSyncReport{Direction: "import"}}
		open := func(context.Context, string) (syncStore, error) { return f, nil }
		if code := runWorkStoreSync(context.Background(), "workstore-import", quietLog(), cfg, open); code != 0 {
			t.Fatalf("%s=%q: exit %d", freshInstallEnv, val, code)
		}
		if f.importOpts == nil || f.importOpts.FreshInstall != want {
			t.Errorf("%s=%q: import options %+v, want FreshInstall=%v", freshInstallEnv, val, f.importOpts, want)
		}
	}
}

// Without the signal, an import beadstore refuses as not-a-fresh-install exits
// non-zero and stamps nothing: the controller does not start on an unimported
// store, and break-glass cannot rescue a store no import has completed against.
func TestImportNotAFreshInstallFailsAndStampsNothing(t *testing.T) {
	for _, breakGlass := range []bool{false, true} {
		f := &fakeSyncStore{err: fmt.Errorf("beadstore: %w (%w)", beadstore.ErrNotAFreshInstall, beadstore.ErrBdDatabaseMissing)}
		code, city := runImport(t, f, breakGlass)
		if _, err := readImportStamp(city); code == 0 || err == nil {
			t.Fatalf("break-glass %v: exit %d, stamp err %v; want non-zero and no stamp", breakGlass, code, err)
		}
	}
}

func TestWorkStoreResolveUsage(t *testing.T) {
	cfg := syncConfig{DSNFile: dsnFile(t, "postgres://x"), BeadRepoDir: t.TempDir()}
	open := func(context.Context, string) (resolveStore, error) {
		t.Fatal("opened the store on a usage error")
		return nil, nil
	}
	for _, args := range [][]string{{}, {"--anchor", "gonk:1:issue:1"}, {"--anchor", "gonk:1:issue:1", "--take", "both"}, {"--take", "bd"}} {
		if code := runWorkStoreResolve(context.Background(), args, quietLog(), cfg, open); code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
	}
}
