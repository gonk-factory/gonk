//go:build chart

package charttest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// NOTES.txt NAMES EVERY SECRET THE RENDER MOUNTS, AND ONLY THOSE (gonk-tomt).
//
// The chart creates no Secret. A first-time installer reads the notes to learn
// which ones to create, and a missing one is a pod stuck in
// CreateContainerConfigError. The notes listed five while the workloads mounted
// up to eight (gonk-dolt, gonk-gc-write-key and gonk-workstore were absent).
//
// So this test does not hold a list of names. It reads the Secrets and keys the
// rendered workloads actually mount and compares them with what the rendered
// notes say, in both directions, for every ci/ profile and for the toggles that
// add or remove a Secret or a key.
//
// `helm template` does not print NOTES.txt, and `helm install --dry-run` needs
// a cluster. notesRender therefore renders the notes with helm's own engine: it
// copies the chart to a temporary directory, puts NOTES.txt beside Chart.yaml
// as a plain file, and adds one template that runs it through `tpl` into a
// ConfigMap. The text is the same text helm would print.

const notesProbeTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: gonk-notes-probe
data:
  notes: |
{{ tpl (.Files.Get "notes-probe.txt") . | indent 4 }}
`

// notesRender returns the rendered notes and the rendered manifests (without
// the probe) for the given extra helm arguments.
func notesRender(t *testing.T, extra ...string) (notes, manifests string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "gonk")
	if err := os.CopyFS(tmp, os.DirFS(ChartDir())); err != nil {
		t.Fatalf("copy the chart: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(tmp, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatalf("read NOTES.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "notes-probe.txt"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "templates", "zz-notes-probe.yaml"), []byte(notesProbeTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"template", "gonk", tmp, "--namespace", "gonk"}, extra...)
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template (with the notes probe) failed: %v\n%s", err, out)
	}
	probe := MustObject(t, out, "ConfigMap", "gonk-notes-probe")
	notes = probe.Data(t)["notes"]
	if strings.TrimSpace(notes) == "" {
		t.Fatal("the notes rendered empty: the probe is not reading NOTES.txt")
	}
	// The manifests, from the real chart: what an install deploys.
	return notes, Render(t, extra...)
}

// mountedSecrets is every Secret a rendered workload mounts, with the keys it
// projects: name -> sorted keys. A projected source with no `items` would
// mount every key; the chart never does that, and it is refused here so the
// comparison cannot silently lose a key.
func mountedSecrets(t *testing.T, manifests string) map[string][]string {
	t.Helper()
	set := map[string]map[string]bool{}
	add := func(name string, items []any, where string) {
		if name == "" {
			t.Fatalf("%s: a secret volume with no name", where)
		}
		if len(items) == 0 {
			t.Fatalf("%s: Secret %s is mounted with no `items`; this test compares keys and cannot see which are needed", where, name)
		}
		if set[name] == nil {
			set[name] = map[string]bool{}
		}
		for _, it := range items {
			m, _ := it.(map[string]any)
			k, _ := m["key"].(string)
			set[name][k] = true
		}
	}
	workloads := 0
	for _, o := range Objects(t, manifests) {
		spec, _ := o.Raw["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		vols, _ := podSpec["volumes"].([]any)
		if podSpec == nil {
			continue
		}
		workloads++
		where := o.Kind + "/" + o.Metadata.Name
		for _, v := range vols {
			vol, _ := v.(map[string]any)
			if sec, ok := vol["secret"].(map[string]any); ok {
				name, _ := sec["secretName"].(string)
				items, _ := sec["items"].([]any)
				add(name, items, where)
			}
			proj, _ := vol["projected"].(map[string]any)
			sources, _ := proj["sources"].([]any)
			for _, s := range sources {
				src, _ := s.(map[string]any)
				sec, ok := src["secret"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := sec["name"].(string)
				items, _ := sec["items"].([]any)
				add(name, items, where)
			}
		}
	}
	if workloads == 0 {
		t.Fatal("no workload with a pod template in the render: nothing to compare the notes with")
	}
	return flatten(set)
}

func flatten(set map[string]map[string]bool) map[string][]string {
	out := map[string][]string{}
	for name, keys := range set {
		for k := range keys {
			out[name] = append(out[name], k)
		}
		sort.Strings(out[name])
	}
	return out
}

var notesSecretLine = regexp.MustCompile(`^\s+(\S+)\s+\(keys: ([^)]*)\)\s*$`)

// notedSecrets parses the "<name>  (keys: a, b)" lines of the rendered notes.
func notedSecrets(t *testing.T, notes string) map[string][]string {
	t.Helper()
	set := map[string]map[string]bool{}
	for _, line := range strings.Split(notes, "\n") {
		m := notesSecretLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if set[m[1]] != nil {
			t.Errorf("the notes list Secret %s twice", m[1])
		}
		set[m[1]] = map[string]bool{}
		for _, k := range strings.Split(m[2], ",") {
			if k = strings.TrimSpace(k); k != "" {
				set[m[1]][k] = true
			}
		}
	}
	return flatten(set)
}

func describeSecrets(m map[string][]string) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString("    " + n + ": " + strings.Join(m[n], ", ") + "\n")
	}
	return b.String()
}

func TestNotesNameEverySecretTheRenderMounts(t *testing.T) {
	type tc struct {
		name string
		args []string
		// mustMount is a Secret this case exists to bring in (or "" for none):
		// it guards the case itself against going vacuous.
		mustMount string
		mustKey   string
	}
	var cases []tc
	for _, p := range goldenProfiles {
		cases = append(cases, tc{name: p, args: Profile(p)})
	}
	def := Profile("values-default")
	with := func(sets ...string) []string {
		out := append([]string{}, def...)
		for _, s := range sets {
			out = append(out, "--set", s)
		}
		return out
	}
	cases = append(cases,
		tc{name: "default profile mounts the dolt secret", args: def, mustMount: "gonk-dolt", mustKey: "gc-password"},
		tc{name: "default profile mounts the write key", args: def, mustMount: "gonk-gc-write-key", mustKey: "key"},
		tc{name: "work store on", args: WithWorkStore(def), mustMount: "gonk-workstore", mustKey: "dsn"},
		tc{name: "split credential", args: with("gitlab.mode=split-credential"), mustMount: "gonk-gitlab", mustKey: "admin-token"},
		tc{name: "every rotation slot", args: with(
			"secrets.rotation.gitlabPrevious=true", "secrets.rotation.webhookPrevious=true",
			"secrets.rotation.meterAPIPrevious=true", "secrets.rotation.litellmAdminPrevious=true"),
			mustMount: "gonk-litellm", mustKey: "admin-key-previous"},
		tc{name: "renamed secret", args: with("secrets.dolt.existingSecret=my-dolt"), mustMount: "my-dolt", mustKey: "gc-password"},
	)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			notes, manifests := notesRender(t, c.args...)
			mounted := mountedSecrets(t, manifests)
			noted := notedSecrets(t, notes)

			if c.mustMount != "" {
				found := false
				for _, k := range mounted[c.mustMount] {
					found = found || k == c.mustKey
				}
				if !found {
					t.Fatalf("this case is meant to mount key %q of Secret %s and the render does not:\n%s",
						c.mustKey, c.mustMount, describeSecrets(mounted))
				}
			}
			if len(mounted) < 3 {
				t.Fatalf("the render mounts only %d Secrets; the comparison would prove nothing:\n%s", len(mounted), describeSecrets(mounted))
			}
			if got, want := describeSecrets(noted), describeSecrets(mounted); got != want {
				t.Errorf("NOTES.txt does not list exactly the Secrets and keys the workloads mount.\n"+
					"  mounted by the render:\n%s  listed in the notes:\n%s"+
					"A Secret the notes omit is a pod in CreateContainerConfigError for a first-time installer.",
					want, got)
			}
		})
	}
}

// The notes of the default install, read as an installer reads them: the three
// Secrets the old list left out are there by name.
func TestNotesListTheSecretsTheOldListOmitted(t *testing.T) {
	notes, _ := notesRender(t, WithWorkStore(Profile("values-default"))...)
	for _, name := range []string{"gonk-dolt", "gonk-gc-write-key", "gonk-workstore"} {
		if !strings.Contains(notes, name) {
			t.Errorf("the rendered notes do not name %s:\n%s", name, notes)
		}
	}
	if !strings.Contains(notes, "CreateContainerConfigError") {
		t.Errorf("the notes no longer say what a missing Secret looks like:\n%s", notes)
	}
}
