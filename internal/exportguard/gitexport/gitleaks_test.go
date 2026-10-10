package gitexport

import (
	"os"
	"strings"
	"testing"
)

// TestThePinnedGitleaksBinary runs the REAL scanner. The binary is not in the
// repository and not on every machine, so this test needs
// RELEXPORT_GITLEAKS=/path/to/gitleaks and says so when it is not set; the
// wiring of the scanner into Check is covered without it (TestCheckRefusals).
// Its output goes into the evidence log whenever the scanner code changes.
func TestThePinnedGitleaksBinary(t *testing.T) {
	bin := os.Getenv("RELEXPORT_GITLEAKS")
	if bin == "" {
		t.Skip("RELEXPORT_GITLEAKS is not set: the real gitleaks " + GitleaksVersion + " binary was NOT exercised by this run")
	}
	scan, err := Gitleaks(bin)
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	// A token-shaped fixture, assembled here so that this file holds none.
	token := "gl" + "pat-" + "q7Rk2mZx" + "9LpT4vBn" + "c8Wd"
	leaky := w.origin.Commit("a token-shaped string", map[string]string{"cmd/tool/config.go": "package main\n\nconst token = \"" + token + "\"\n"})
	r := w.clone()

	tr, err := r.Tree(w.src, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Check(tr, CheckOptions{Scanner: scan})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.ScannerRan || len(res.ScanFindings) != 0 {
		t.Fatalf("the clean tree did not pass the real scanner:\n%s", res.Refusal())
	}

	tr, err = r.Tree(leaky, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err = r.Check(tr, CheckOptions{Scanner: scan})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.ScanFindings) != 1 {
		t.Fatalf("want exactly one scanner finding, got %+v", res.ScanFindings)
	}
	f := res.ScanFindings[0]
	if f.File != "cmd/tool/config.go" || f.Line != 3 || f.Rule == "" {
		t.Fatalf("finding %+v; want cmd/tool/config.go line 3", f)
	}
	if strings.Contains(res.Refusal(), token) {
		t.Fatal("the refusal repeats the secret")
	}
	// The scanner loads .gitleaks.toml from the root of the tree it is
	// handed, with no flag (measured; plan assumption A7). So a configuration
	// committed with the tree decides what counts as a finding -- which is
	// why Check refuses one that the allow-list does not name.
	q := strings.Repeat("'", 3)
	config := "[extend]\nuseDefault = true\n\n[[allowlists]]\ndescription = \"fixture\"\nregexes = [" + q + token[:9] + "[A-Za-z0-9]+" + q + "]\n"
	withConfig := w.origin.Commit("a scanner configuration that excuses the fixture", map[string]string{
		".gitleaks.toml":    config,
		"release/allow.txt": "scanner-config\t.gitleaks.toml\tfor this test\n",
	})
	r = w.clone()
	tr, err = r.Tree(withConfig, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err = r.Check(tr, CheckOptions{Scanner: scan})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("the committed configuration was not honoured by the real scanner:\n%s", res.Refusal())
	}
}
