//go:build images

package images

// THE REAL LOADER, OFFLINE. No cluster, no deployed city -- just the parser
// (and, for the two order-semantics tests below, the order-discovery scanner)
// that will accept or reject our pack in production, run against our pack
// now, inside the gonk-controller image (Task 6, Plan 04).
//
// Two DIFFERENT real entry points are exercised here, because `gc` offers no
// single command that covers everything internal/config/pack.go's loader
// enforces:
//
//   - `gc lint <pack>` (cmd/gc/cmd_lint.go) runs config.LoadPackForLint, which
//     decodes pack.toml with toml.DecodeStrict-style unknown-key rejection
//     and discovers agents/formulas/orders exactly as a live city would.
//     CONFIRMED (by hand, before writing this test): it rejects an invented
//     top-level table and NAMES the key.
//   - `gc order list --city <dir> --json` (cmd/gc/cmd_order.go) exercises
//     orderdiscovery.ScanAll, THE SAME CODE PATH cmd/gc/order_dispatch.go
//     uses to build the controller's real dispatch set. `gc lint` does NOT
//     call this path (confirmed by hand: an order declaring both `formula`
//     and `exec` passed `gc lint` with zero diagnostics) -- order-level
//     semantic rules (formula XOR exec, no pool on an exec order,
//     internal/orders/order.go's `Validate`) are only enforced here.
//
// REAL FINDING, worth stating loudly: neither of gascity's own order-scanning
// callers treats a `Validate` failure as fatal. Every caller in cmd/gc
// (order list/show, and the controller's own order_dispatch.go) wires
// `OnValidateError` to log the message and return nil, which
// internal/orderdiscovery/discovery.go's `validateOrders` treats as "drop
// this one order and keep going" -- confirmed by hand: injecting a
// formula+exec order made `gc order list --json`'s order COUNT drop from 5
// to 4, filenames the bad order and the exact rule on stderr, and STILL
// EXITS 0. So the assertion these two tests make is NOT "the command exits
// non-zero" (it does not, and asserting that would be testing a behavior the
// real binary does not have) -- it is "the malformed order is named on
// stderr AND absent from the loaded order set", which is what the real
// production controller does with it.
//
// This whole file is not a substitute for Task 4's `internal/packtest`
// (an OFFLINE structural allow-list transcribed from the loader's source,
// with no `gc` binary and no container) -- it is the complementary check:
// the actual code, not a transcription of it, run against our pack now.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// decodeFirstJSONObject decodes the FIRST JSON value in s and ignores
// whatever follows it -- needed because this file's helpers concatenate a
// container's stdout (the JSON gc/gc-order-list emit) with its stderr
// (podman's own noisy CNI-validation warnings on this box, plus gc's own
// human-readable warnings), and encoding/json.Unmarshal, unlike Decoder,
// rejects any trailing bytes after a valid top-level value.
func decodeFirstJSONObject(t *testing.T, s string, v any) {
	t.Helper()
	i := strings.IndexByte(s, '{')
	if i < 0 {
		t.Fatalf("no JSON object found in output:\n%s", s)
	}
	if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(v); err != nil {
		t.Fatalf("decoding JSON object: %v\nraw:\n%s", err, s)
	}
}

// runInController execs entrypoint inside the controller image, exactly like
// runIn (controller_smoke_test.go / agent_smoke_test.go), with one addition:
// an optional bind mount, needed here because the negative-control tests must
// hand the container a MUTATED copy of the pack, not the one baked into the
// image at /opt/gonk/pack.
func runInController(t *testing.T, mounts []string, entrypoint string, args ...string) (out string, code int) {
	t.Helper()
	image, _ := controllerImage(t)
	full := []string{"run", "--rm", "--network=host"}
	for _, m := range mounts {
		full = append(full, "-v", m)
	}
	full = append(full, "--entrypoint", entrypoint, image)
	full = append(full, args...)
	bin := containerBin(t)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("%s run %v: %v\nstderr:\n%s", bin, full, err, stderr.String())
		}
	}
	// Both streams matter for these tests (gc's diagnostics land on stdout
	// for `lint --json`, on stderr for `order list`'s validate warnings) --
	// unlike the smoke tests, concatenate rather than pick one.
	return stdout.String() + stderr.String(), code
}

// copyPackToTemp copies the real pack/ tree to a host temp dir, world-
// readable (the container runs as uid 65532, not the host's build uid), so a
// test can mutate a throwaway copy without touching the tracked pack/ or the
// one baked into the image.
func copyPackToTemp(t *testing.T) string {
	t.Helper()
	src := filepath.Join(repoRoot(t), "pack")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0o777)
	})
	if err != nil {
		t.Fatalf("copying pack/ to temp dir: %v", err)
	}
	return dst
}

// scratchCityToml is the minimal [workspace]/[providers] pair `gc order
// list --city <dir>` needs to resolve a city at all -- confirmed by hand
// against the real binary; a bare pack dir with no city.toml is not
// resolvable as a --city target. `builtin:claude` needs no network and no
// real credential (nothing in these tests ever starts a session).
const scratchCityToml = `
[workspace]
provider = "claude"

[providers.claude]
base = "builtin:claude"
`

// writeScratchCity turns a (possibly mutated) copy of the pack into
// something `gc order list --city` will resolve: the pack's own files ARE
// the city's local pack (gascity's convention-based v2 layout -- pack-spec.md
// 1.2.14), so a city.toml dropped alongside them is the entire difference
// between "a pack directory" and "a city".
func writeScratchCity(t *testing.T, packDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(packDir, "city.toml"), []byte(scratchCityToml), 0o666); err != nil {
		t.Fatalf("writing scratch city.toml: %v", err)
	}
}

// orderListEnv is the env `gc order list`/`gc lint` need to run OFFLINE, no
// real beads/Dolt store, confirmed by hand against the real binary:
// GC_SESSION=fake avoids needing a real session provider, GC_BEADS=file +
// GC_DOLT=skip avoid needing a live Dolt server for a read-only scan.
var orderListEnv = []string{"-e", "GC_SESSION=fake", "-e", "GC_BEADS=file", "-e", "GC_DOLT=skip"}

func runOrderList(t *testing.T, cityHostDir string) (stdout string, code int) {
	t.Helper()
	image, _ := controllerImage(t)
	const mountPoint = "/tmp/scratch-city"
	full := []string{"run", "--rm", "--network=host"}
	full = append(full, orderListEnv...)
	full = append(full, "-v", cityHostDir+":"+mountPoint)
	full = append(full, image, "gc", "order", "list", "--json", "--city", mountPoint)
	bin := containerBin(t)
	cmd := exec.Command(bin, full...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("%s run %v: %v\nstderr:\n%s", bin, full, err, errBuf.String())
		}
	}
	return out.String() + "\n" + errBuf.String(), code
}

type orderListJSON struct {
	OK     bool `json:"ok"`
	Orders []struct {
		Name string `json:"name"`
	} `json:"orders"`
}

func parseOrderList(t *testing.T, out string) orderListJSON {
	t.Helper()
	// gc may print a leading "warning: ... core ..." human line before the
	// JSON, and (see decodeFirstJSONObject) trailing stderr noise after it --
	// confirmed by hand.
	var parsed orderListJSON
	decodeFirstJSONObject(t, out, &parsed)
	return parsed
}

func orderNames(o orderListJSON) map[string]bool {
	m := make(map[string]bool, len(o.Orders))
	for _, order := range o.Orders {
		m[order.Name] = true
	}
	return m
}

// TestGasCityLoaderAcceptsTheGonkPack: `gc lint` against the pack baked into
// the controller image. This is the exact real-loader validation Task 4's
// internal/packtest cannot provide (it is an offline transcription of the
// rules, not the loader) -- exit 0 here means Gas City's own parser, not our
// guess at it, is satisfied.
func TestGasCityLoaderAcceptsTheGonkPack(t *testing.T) {
	out, code := runInController(t, nil, "gc", "lint", "/opt/gonk/pack", "--json")
	if code != 0 {
		t.Fatalf("Gas City's loader REJECTED the gonk pack (exit %d):\n%s", code, out)
	}
	var report struct {
		Passed     bool `json:"passed"`
		ErrorCount int  `json:"error_count"`
	}
	decodeFirstJSONObject(t, out, &report)
	if !report.Passed || report.ErrorCount != 0 {
		t.Fatalf("gc lint reported passed=%v error_count=%d, want passed=true error_count=0:\n%s",
			report.Passed, report.ErrorCount, out)
	}
}

// The negative control, and it is the one that gives the positive above its
// meaning. A test that "validates" a pack but would also pass on a pack with
// a garbage key is not validating anything. Inject an unknown key, assert
// the loader rejects it, and assert the ERROR NAMES THE KEY.
func TestGasCityLoaderRejectsAnUnknownKey(t *testing.T) {
	dir := copyPackToTemp(t)
	appendToFile(t, filepath.Join(dir, "pack.toml"), "\n[gonk_invented_table]\nnope = true\n")

	out, code := runInController(t, []string{dir + ":/tmp/badpack"}, "gc", "lint", "/tmp/badpack", "--json")
	if code == 0 {
		t.Fatal("the loader ACCEPTED an unknown top-level table.\n" +
			"That means this test proves NOTHING about our pack, and Task 4's\n" +
			"internal/packtest allow-list is the only thing standing between us and\n" +
			"a pack that silently ignores half of what we wrote.")
	}
	if !strings.Contains(out, "gonk_invented_table") {
		t.Fatalf("the loader rejected the pack but did not name the offending key:\n%s", out)
	}
}

// TestGasCityLoaderLoadsBothGonkOrders is the positive control for the test
// below: proves order_dispatch.go's real scan path (not `gc lint`, which
// does not exercise it -- see the package doc) accepts every order this pack
// ships, with none silently dropped, BEFORE the negative-control test shows
// what dropping one looks like.
//
// ADR-007 §3 deleted gonk's formula layer -- formulas, [steps.check], and
// the three formula orders (gonk-triage/gonk-scaffold/gonk-mention) that
// used to pour them. The reduced pack ships exactly two orders now, both
// exec: gonk-dispatch (Gate 2) and gonk-sweep (the classifier).
func TestGasCityLoaderLoadsBothGonkOrders(t *testing.T) {
	dir := copyPackToTemp(t)
	writeScratchCity(t, dir)

	out, code := runOrderList(t, dir)
	if code != 0 {
		t.Fatalf("gc order list --json exited %d (want 0):\n%s", code, out)
	}
	parsed := parseOrderList(t, out)
	want := []string{"gonk-dispatch", "gonk-sweep"}
	got := orderNames(parsed)
	for _, name := range want {
		if !got[name] {
			t.Errorf("order %q is missing from gc order list's real scan; full output:\n%s", name, out)
		}
	}
	if len(parsed.Orders) != len(want) {
		t.Errorf("gc order list returned %d orders, want %d: %v", len(parsed.Orders), len(want), got)
	}
}

// The negative control for the one mistake the format actively invites -- run
// through the REAL order-discovery scan, not `gc lint` (confirmed by hand
// not to exercise this path at all: an order declaring both `formula` and
// `exec` passes `gc lint` with zero diagnostics).
//
// REAL FINDING (see the package doc): the real loader does not hard-fail an
// invalid order. It logs the exact rule violated, by order name, and DROPS
// that one order from the set -- the command still exits 0. So this test
// asserts the real, verified behavior: the order is ABSENT from the loaded
// set, and the violation is named on the combined output.
func TestLoaderDropsAnExecOrderWithAPool(t *testing.T) {
	dir := copyPackToTemp(t)
	writeScratchCity(t, dir)
	replaceInFile(t, filepath.Join(dir, "orders", "gonk-dispatch.toml"),
		`exec = "scripts/gonk-dispatch.sh"     # XOR `+"`formula`"+`. NEVER BOTH.`,
		`exec = "scripts/gonk-dispatch.sh"     # XOR `+"`formula`"+`. NEVER BOTH.`+"\n"+`pool = "dispatch"`)

	out, code := runOrderList(t, dir)
	if code != 0 {
		t.Fatalf("gc order list --json exited %d; the real loader does not hard-fail this "+
			"(it warns and drops the order) -- an exit-code change here means gascity's own "+
			"behavior changed and this test's other assertions need re-checking:\n%s", code, out)
	}
	if !strings.Contains(out, "gonk-dispatch") || !strings.Contains(out, "cannot have a pool") {
		t.Fatalf("expected the output to name order %q and \"cannot have a pool\":\n%s", "gonk-dispatch", out)
	}
	parsed := parseOrderList(t, out)
	if orderNames(parsed)["gonk-dispatch"] {
		t.Fatalf("the invalid gonk-dispatch order was NOT dropped from the loaded set:\n%s", out)
	}
	if len(parsed.Orders) != 1 {
		t.Fatalf("got %d orders, want 1 (the 2 real orders minus the one dropped for exec+pool): %v", len(parsed.Orders), parsed.Orders)
	}
}

func appendToFile(t *testing.T, path, suffix string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(suffix); err != nil {
		t.Fatalf("appending to %s: %v", path, err)
	}
}

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	s := string(data)
	if !strings.Contains(s, old) {
		t.Fatalf("%s does not contain expected substring %q -- this test's fixture is stale", path, old)
	}
	s = strings.Replace(s, old, new, 1)
	if err := os.WriteFile(path, []byte(s), 0o666); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
