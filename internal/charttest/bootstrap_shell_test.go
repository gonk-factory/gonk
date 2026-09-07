//go:build chart

package charttest

import (
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE CONTROLLER'S BOOTSTRAP IS A SHELL SCRIPT EMBEDDED IN YAML, and nothing was
// checking that it parses.
//
// It crash-looped the controller on 2026-09-07 for a reason no reviewer would
// spot by reading the diff: a COMMENT inside the bootstrap contained an
// apostrophe, and that comment sits inside a single-quoted awk program. The
// apostrophe terminated the awk script, the shell then tried to parse awk syntax
// as shell, and the init container died with "Syntax error: word unexpected
// (expecting then)" -- pointing at a line far from the edit that caused it.
//
// The goldens did not catch it because a golden asserts the rendered TEXT, and
// the text was exactly what was intended. It was valid YAML and valid Helm
// output; it simply was not a valid shell program. That is the gap this closes.
func TestRenderedInitScriptsParseAsShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}

	var checked int
	for _, o := range Objects(t, Render(t, Minimum()...)) {
		if o.Kind != "Deployment" && o.Kind != "StatefulSet" {
			continue
		}
		var doc struct {
			Spec struct {
				Template struct {
					Spec struct {
						InitContainers []struct {
							Name    string   `yaml:"name"`
							Command []string `yaml:"command"`
							Args    []string `yaml:"args"`
						} `yaml:"initContainers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(o.Doc), &doc); err != nil {
			continue
		}
		for _, c := range doc.Spec.Template.Spec.InitContainers {
			for _, script := range append(append([]string{}, c.Command...), c.Args...) {
				// Only the multi-line embedded programs are worth checking; a
				// bare "sh" or "-c" is not a script.
				if !strings.Contains(script, "\n") {
					continue
				}
				checked++
				cmd := exec.Command("sh", "-n")
				cmd.Stdin = strings.NewReader(script)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("%s init container %q: rendered script is NOT valid shell: %v\n%s\n"+
						"HINT: an apostrophe inside the single-quoted awk program does this, "+
						"including one inside a comment.",
						o.Metadata.Name, c.Name, err, strings.TrimSpace(string(out)))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no embedded init scripts found to check; this test would pass vacuously")
	}
	t.Logf("shell-checked %d rendered init scripts", checked)
}
