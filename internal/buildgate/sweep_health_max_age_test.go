package buildgate

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// The controller's readinessProbe fails once the gonk-sweep health record is
// older than gascity.sweepHealthMaxAge. That window is also how long a dead bead
// store takes to show as 0/1 (gonk-7s9p: Gas City never launched gonk-sweep
// while its store was down, so only the record's age tripped readiness --
// 14m31s after the break, on 15m). So there is a standing temptation to tighten
// it. This test is the brake on that temptation. It is NOT a proof that the
// chosen value never flaps.
//
// WHAT IT ASSERTS: the value is not below ONE NAMED SCENARIO of a healthy sweep
// on a controller under disk pressure (which gonk must tolerate), computed from
// the pack's own order file and Gas City's constants. See pressureScenarioGap.
//
// WHAT IT DOES NOT CLAIM: that the scenario is a worst case. Gas City's source
// does not bound it. Nothing counts consecutive open-work gate timeouts or fails
// open for a non-idempotent order, so every further miss adds another forced
// tick (two misses: 14m30s); and a tick that overruns its patrol slot makes
// Go's ticker drop the ticks it missed. That unbounded tail is why the owner
// kept 15m rather than the scenario's floor (gonk-7s9p), and why there is no
// ceiling here: anything at or above the floor passes.
//
// Every input is read from where it lives, except Gas City's constants, which
// are not in this repo: those are restated below AGAINST THE GASCITY_REF THEY
// WERE READ AT, and a test fails the moment images/versions.env pins a
// different ref, so a Gas City bump cannot silently invalidate the scenario.

// gcConstantsRef is the Gas City commit the constants below were read from.
const gcConstantsRef = "4fda5a28445f42d6e789fc7f5751645ac4fecd19"

const (
	// cmd/gc/fs_pressure.go: maxConsecutiveFSPressureSkips. Under sustained
	// pressure the supervisor skips this many ticks, then forces one.
	gcMaxConsecutiveFSPressureSkips = 5

	// internal/config/config.go: DaemonConfig.PatrolIntervalDuration's default,
	// used when city.toml sets no [daemon] patrol_interval. Neither the pack nor
	// the chart's bootstrap sets one (asserted below).
	gcDefaultPatrolInterval = 30 * time.Second

	// cmd/gc/order_dispatch.go: orderGateTimeout / orderGateBackoffDuration.
	// A non-idempotent order whose open-work gate times out is skipped for the
	// tick and suppressed for the backoff. gonk-sweep is not idempotent.
	gcOrderGateTimeout         = 8 * time.Second
	gcOrderGateBackoffDuration = 24 * time.Second
)

// launchPeriod is how long a due cooldown order can wait for a tick that
// reaches dispatchOrders under sustained pressure: cmd/gc/city_runtime.go's
// tick() returns before dispatchOrders on a pressure-skipped tick, and only
// every (skips+1)th patrol tick is forced through.
func launchPeriod() time.Duration {
	return time.Duration(gcMaxConsecutiveFSPressureSkips+1) * gcDefaultPatrolInterval
}

// pressureScenarioGap is the gap between two HEALTHY health records in one
// named scenario -- "one gate miss, no overrun" -- on a controller under
// sustained IO pressure:
//
//	interval          cooldown is counted from the previous LAUNCH
//	+ launch period   waiting for a forced tick
//	+ ONE gate miss   that tick's order gates time out once; each tick runs two
//	                  (open tracking, then open work), and either one timing
//	                  out skips the order until the next forced tick
//	+ timeout         the pass itself, up to the order's timeout
//
// It deliberately assumes exactly one miss and no tick overrun. Neither is
// bounded by Gas City's source; see the file comment.
func pressureScenarioGap(interval, timeout time.Duration) time.Duration {
	gateMiss := launchPeriod()
	if backoff := gcOrderGateTimeout + gcOrderGateBackoffDuration; backoff > launchPeriod() {
		// Not the case at gcConstantsRef. If it ever is, the backoff outlasts a
		// period and the next chance is the first forced tick after it.
		gateMiss = backoff + launchPeriod()
	}
	return interval + launchPeriod() + gateMiss + timeout
}

// sweepHealthMaxAgeMargin is how far above the scenario the floor sits. It
// absorbs what the scenario leaves out even on its own terms: the forced tick's
// work before dispatchOrders, and gc's kill grace at the order timeout.
const sweepHealthMaxAgeMargin = 30 * time.Second

func TestGasCityConstantsArePinnedToTheImagesGascityRef(t *testing.T) {
	f, err := os.Open(filepath.Join(repoRoot(t), "images", "versions.env"))
	if err != nil {
		t.Fatalf("open images/versions.env: %v", err)
	}
	defer func() { _ = f.Close() }()
	var ref string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "GASCITY_REF="); ok {
			ref = strings.TrimSpace(strings.SplitN(v, "#", 2)[0])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read images/versions.env: %v", err)
	}
	if ref == "" {
		t.Fatal("images/versions.env has no GASCITY_REF")
	}
	if ref != gcConstantsRef {
		t.Fatalf("GASCITY_REF is %s but the readiness bound's Gas City constants were read at %s. "+
			"Re-read maxConsecutiveFSPressureSkips (cmd/gc/fs_pressure.go), the patrol_interval default "+
			"(internal/config/config.go), orderGateTimeout and orderGateBackoffDuration "+
			"(cmd/gc/order_dispatch.go), and whether tick() still returns before dispatchOrders under "+
			"pressure (cmd/gc/city_runtime.go); then update gcConstantsRef.", ref, gcConstantsRef)
	}
}

// The model assumes the default patrol interval. A patrol_interval set anywhere
// gonk writes city config would change the launch period.
func TestNothingOverridesThePatrolIntervalTheBoundAssumes(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"pack", filepath.Join("chart", "gonk", "templates")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "patrol_interval") {
				t.Errorf("%s sets patrol_interval; the sweep-health bound assumes Gas City's default %s",
					path, gcDefaultPatrolInterval)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

func TestSweepHealthMaxAgeIsNotTightenedBelowThePressureScenario(t *testing.T) {
	root := repoRoot(t)
	interval, timeout := sweepOrderTiming(t, root)
	maxAge := chartSweepHealthMaxAge(t, root)

	gap := pressureScenarioGap(interval, timeout)
	if floor := gap + sweepHealthMaxAgeMargin; maxAge < floor {
		t.Errorf("gascity.sweepHealthMaxAge = %s is below %s. Even the \"one gate miss, no overrun\" disk-pressure "+
			"scenario puts %s between two HEALTHY sweep records (interval %s + launch wait %s + one gate-miss wait %s "+
			"+ timeout %s), and Gas City does not bound the misses beyond it. A false unready takes a healthy "+
			"controller out of its Service, and refused mentions are never replayed. The owner kept 15m on purpose "+
			"(gonk-7s9p); faster dead-store detection is gonk-hkjh, not a tighter window.",
			maxAge, floor, gap, interval, launchPeriod(), launchPeriod(), timeout)
	}
}

func sweepOrderTiming(t *testing.T, root string) (interval, timeout time.Duration) {
	t.Helper()
	var order struct {
		Order struct {
			Trigger    string `toml:"trigger"`
			Interval   string `toml:"interval"`
			Timeout    string `toml:"timeout"`
			Idempotent bool   `toml:"idempotent"`
		} `toml:"order"`
	}
	if _, err := toml.DecodeFile(filepath.Join(root, "pack", "orders", "gonk-sweep.toml"), &order); err != nil {
		t.Fatalf("decode pack/orders/gonk-sweep.toml: %v", err)
	}
	if order.Order.Trigger != "cooldown" {
		t.Fatalf("gonk-sweep trigger = %q; the bound models a cooldown order", order.Order.Trigger)
	}
	if order.Order.Idempotent {
		// An idempotent order fails OPEN on a gate timeout, which removes the
		// gate-miss term: the bound would be stale, not unsafe.
		t.Fatal("gonk-sweep is now idempotent; drop the gate-miss term from pressureScenarioGap")
	}
	var err error
	if interval, err = time.ParseDuration(order.Order.Interval); err != nil {
		t.Fatalf("gonk-sweep interval %q: %v", order.Order.Interval, err)
	}
	if timeout, err = time.ParseDuration(order.Order.Timeout); err != nil {
		t.Fatalf("gonk-sweep timeout %q: %v", order.Order.Timeout, err)
	}
	return interval, timeout
}

func chartSweepHealthMaxAge(t *testing.T, root string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "chart", "gonk", "values.yaml"))
	if err != nil {
		t.Fatalf("read chart values: %v", err)
	}
	var values struct {
		Gascity struct {
			SweepHealthMaxAge string `yaml:"sweepHealthMaxAge"`
		} `yaml:"gascity"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse chart values: %v", err)
	}
	d, err := time.ParseDuration(values.Gascity.SweepHealthMaxAge)
	if err != nil {
		t.Fatalf("gascity.sweepHealthMaxAge %q: %v", values.Gascity.SweepHealthMaxAge, err)
	}
	return d
}
