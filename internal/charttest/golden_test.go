//go:build chart

package charttest

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden manifests")

// goldenProfiles is every ci/ value profile whose whole rendered output is
// snapshotted: the four Task 9 profiles plus the four Task 8.5 toggle-matrix
// profiles. Each is self-sufficient (renders with no extra --set), so the
// golden is the AUTHORITATIVE record of what that profile deploys.
var goldenProfiles = []string{
	"values-default",
	"values-byo-everything",
	"values-cnpg",
	"values-full-monitoring",
	"values-all-bundled",
	"values-external-dolt",
	"values-byo-gascity",
	"values-ledger-cnpg",
}

// Golden manifests: the whole rendered output of each profile, byte for byte.
// This is the drift gate. A change to a helper that quietly moves a volumeMount,
// or a values default that quietly opens a port, shows up here as a diff a human
// has to look at and approve. Regenerate deliberately:
//
//	go test -tags chart ./internal/charttest/ -run Golden -update
func TestGoldenManifests(t *testing.T) {
	for _, profile := range goldenProfiles {
		t.Run(profile, func(t *testing.T) {
			got := Render(t, "--values", filepath.Join(ChartDir(), "ci", profile+".yaml"))
			golden := filepath.Join("testdata", "golden", profile+".yaml")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("no golden manifest (%v). Run with -update, then READ THE DIFF.", err)
			}
			if got != string(want) {
				t.Errorf("%s drifted from its golden manifest.\n"+
					"Run `go test -tags chart ./internal/charttest/ -run Golden -update` and READ the diff:\n"+
					"a moved volumeMount or an opened port looks exactly like a formatting change.", profile)
			}
		})
	}
}

// Every profile must lint and validate, not merely render.
func TestEveryProfileLintsAndValidates(t *testing.T) {
	crds := filepath.Join(ChartDir(), "tests", "crd-schemas")
	for _, profile := range goldenProfiles {
		t.Run(profile, func(t *testing.T) {
			vals := []string{"--values", filepath.Join(ChartDir(), "ci", profile+".yaml")}
			Lint(t, vals...)
			Kubeconform(t, Render(t, vals...), crds)
		})
	}
}
