package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProcStartTicksCountsFieldsAfterTheLastParen(t *testing.T) {
	// comm with a space and a ')' in it: naive splitting would misnumber fields.
	stat := "4242 (gonk) gate) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 7 0 987654 1000 200 18446744073709551615"
	got, err := procStartTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 987654 {
		t.Fatalf("starttime = %d, want 987654 (field 22)", got)
	}
	if _, err := procStartTicks("4242 no-parens"); err == nil {
		t.Fatal("malformed stat accepted")
	}
}

func TestReadLaunchTimingComputesTheExecStart(t *testing.T) {
	files := map[string]string{
		"/proc/uptime":    "1000.50 9999.00\n",
		"/proc/self/stat": "1 (gonk-gate) S 0 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 99975 0 0",
	}
	read := func(p string) ([]byte, error) {
		v, ok := files[p]
		if !ok {
			return nil, errors.New("no such file")
		}
		return []byte(v), nil
	}
	start := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	lt := readLaunchTiming(start, read)
	if lt.Err != nil {
		t.Fatal(lt.Err)
	}
	// started at tick 99975 = 999.75s after boot; now is 1000.50s: 750ms ago.
	if gap := start.Sub(lt.ExecStart); gap != 750*time.Millisecond {
		t.Fatalf("exec-to-main = %v, want 750ms", gap)
	}
}

// Against this very process: the gap must be small and non-negative, and the
// record must carry the fields the deploy decision joins on.
func TestLaunchTimingOnThisProcess(t *testing.T) {
	lt := readLaunchTiming(time.Now(), os.ReadFile)
	if lt.Err != nil {
		t.Skipf("/proc unavailable: %v", lt.Err)
	}
	gap := lt.ProcessStart.Sub(lt.ExecStart)
	if gap < -20*time.Millisecond { // /proc resolution is 10ms
		t.Fatalf("exec start %v is after process start %v", lt.ExecStart, lt.ProcessStart)
	}
	var buf bytes.Buffer
	logLaunchGap(slog.New(slog.NewJSONHandler(&buf, nil)), lt, "gonk:1:issue:1")
	for _, f := range []string{`"exec_start"`, `"process_start"`, `"exec_to_main_ms"`, `"bead":"gonk:1:issue:1"`} {
		if !strings.Contains(buf.String(), f) {
			t.Fatalf("launch record lacks %s: %s", f, buf.String())
		}
	}
	t.Logf("test binary: exec to test-start %v", gap)
}
