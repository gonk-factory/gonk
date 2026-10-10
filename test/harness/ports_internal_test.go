package harness

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The verdict must be exactly the probe's verdict: nil iff every probe
// succeeded, otherwise an error naming every port whose probe failed and no
// port whose probe succeeded. The probe is injected so the test controls the
// state it asserts about -- a real port's freedom cannot be held still on a
// shared machine (gonk-th5r).
func TestCheckPortsFreeVerdictIsTheProbesVerdict(t *testing.T) {
	ports := []int{6303, 4101, 5202}
	cases := []struct {
		name     string
		busy     []int
		wantBusy []int // nil means CheckPortsFree must return nil
	}{
		{"all free", nil, nil},
		{"one busy among free ports", []int{4101}, []int{4101}},
		{"first and last busy", []int{6303, 5202}, []int{5202, 6303}},
		{"all busy", []int{6303, 4101, 5202}, []int{4101, 5202, 6303}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busy := map[int]bool{}
			for _, p := range tc.busy {
				busy[p] = true
			}
			var probed []int
			probe := func(p int) error {
				probed = append(probed, p)
				if busy[p] {
					return &fakeBindErr{p}
				}
				return nil
			}

			err := checkPortsFree(probe, ports...)

			// Every port is probed, whatever the earlier verdicts were.
			if !reflect.DeepEqual(probed, ports) {
				t.Fatalf("probed %v, want every port in order %v", probed, ports)
			}
			if tc.wantBusy == nil {
				if err != nil {
					t.Fatalf("every probe succeeded, but CheckPortsFree = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("probe reported %v busy, but CheckPortsFree = nil", tc.busy)
			}
			if got := namedPorts(t, err.Error()); !reflect.DeepEqual(got, tc.wantBusy) {
				t.Fatalf("error names ports %v, want exactly %v (every busy port, no free one): %v", got, tc.wantBusy, err)
			}
		})
	}
}

type fakeBindErr struct{ port int }

func (e *fakeBindErr) Error() string { return "fake: address in use: " + strconv.Itoa(e.port) }

// namedPorts extracts the port list from CheckPortsFree's error, which reads
// "harness: port(s) already in use: A, B, C (stop the leftover ...".
func namedPorts(t *testing.T, msg string) []int {
	t.Helper()
	const lead = "already in use: "
	i := strings.Index(msg, lead)
	if i < 0 {
		t.Fatalf("error does not contain %q: %s", lead, msg)
	}
	rest := msg[i+len(lead):]
	if j := strings.Index(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	var out []int
	for _, f := range strings.Split(rest, ", ") {
		p, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			t.Fatalf("port list %q has a non-port %q: %s", rest, f, msg)
		}
		out = append(out, p)
	}
	if !sort.IntsAreSorted(out) {
		t.Fatalf("ports %v are not sorted: %s", out, msg)
	}
	return out
}
