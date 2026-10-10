//go:build chart

package charttest

// THE GOLDENS ARE A FUNCTION OF THE RENDERER (gonk-3zcs, R-55). This box carries
// helm 3.19.0 in /usr/bin and helm 4.3.0 in /snap/bin; helm 4 adds 27 blank
// lines per profile, so whichever one PATH finds first decides whether `make
// gate` is green on a clean tree. Since the golden render is now IN that gate,
// an unpinned helm is a red gate nobody caused -- and its advice ("run
// -update") would rewrite all eight goldens in the wrong shape.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTheHelmVersionGateRejectsEveryHelmTheGoldensDoNotMatch(t *testing.T) {
	cases := []struct {
		version  string
		wantSaid string // "" means the version must be ACCEPTED
	}{
		{"v3.19.0+g3d8990f", ""},        // what the goldens were rendered with
		{"v3.14.0+gabcdef0", ""},        // the documented floor
		{"v3.99.1", ""},                 // any later helm 3
		{"v4.3.0+gbec5b06", "helm 4.3"}, // the one on /snap/bin here
		{"v5.0.0", "helm 5.0"},          // any future major
		{"v3.13.3", "older than the 3.14"},
		{"v2.17.0", "helm 2.17"},
		{"helm version is a mystery", "cannot read a version"},
		{"", "cannot read a version"},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			got := helmVersionProblem(tc.version)
			if tc.wantSaid == "" {
				if got != "" {
					t.Fatalf("helm %q was rejected: %s", tc.version, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("helm %q was ACCEPTED; the goldens would be compared against a renderer that does not produce them", tc.version)
			}
			if !strings.Contains(got, tc.wantSaid) {
				t.Errorf("the refusal for %q does not say %q: %s", tc.version, tc.wantSaid, got)
			}
		})
	}
}

// And the helm this run will actually use passes its own gate -- otherwise the
// table above could be right while every render used something else.
func TestTheHelmThisRunUsesIsOneTheGoldensMatch(t *testing.T) {
	bin := helmBin()
	out, err := exec.Command(bin, "version", "--short").CombinedOutput()
	if err != nil {
		t.Fatalf("%s version --short: %v\n%s", bin, err, out)
	}
	if p := helmVersionProblem(string(out)); p != "" {
		t.Fatalf("%s reports %s, which this gate refuses: %s", bin, strings.TrimSpace(string(out)), p)
	}
	t.Logf("rendering with %s (%s)", bin, strings.TrimSpace(string(out)))
}
