package cidef

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every fixture under testdata is hand-written, invented YAML (the plan's
// held-out rule, section 0). None is copied from, or shaped after, a project.

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}
	return b
}

// keysDiff compares testdata/keys/base.yml with testdata/keys/<name>.yml.
func keysDiff(t *testing.T, name string, diagnosed ...string) Delta {
	t.Helper()
	return Diff(fixture(t, "keys/base.yml"), fixture(t, "keys/"+name+".yml"), diagnosed)
}

// pairDiff compares testdata/<dir>/base.yml with testdata/<dir>/head.yml.
func pairDiff(t *testing.T, dir string, diagnosed ...string) Delta {
	t.Helper()
	return Diff(fixture(t, dir+"/base.yml"), fixture(t, dir+"/head.yml"), diagnosed)
}

// vio renders the violations as sorted "RULE job/key" strings.
func vio(d Delta) []string {
	out := []string{}
	for _, v := range d.Violations {
		out = append(out, v.Rule+" "+v.Job+"/"+v.Key)
	}
	sort.Strings(out)
	return out
}

// unres renders the unresolvable findings as sorted "side job/key" strings.
func unres(d Delta) []string {
	out := []string{}
	for _, u := range d.Unresolvable {
		out = append(out, string(u.Side)+" "+u.Job+"/"+u.Key)
	}
	sort.Strings(out)
	return out
}

// changes renders the per-job key changes as sorted "job/key=class" strings.
func changes(d Delta) []string {
	out := []string{}
	for _, j := range d.Jobs {
		for _, c := range j.Changes {
			out = append(out, j.Job+"/"+c.Key+"="+string(c.Class))
		}
	}
	sort.Strings(out)
	return out
}

func globals(d Delta) []string {
	out := []string{}
	for _, g := range d.Globals {
		s := g.Key + "=" + string(g.Class)
		if len(g.Appended) > 0 {
			s += "+" + strings.Join(g.Appended, ",")
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n  got  %q\n  want %q", what, got, want)
	}
}

// keysJobs are the jobs of testdata/keys/base.yml.
var keysJobs = []string{"bundle", "downstream", "fetch-parts", "lint-widgets", "ship-later", "test-widgets"}

// ---------------------------------------------------------------------------
// The key lists and the key-group table cannot drift.
//
// The table is the product's rule and lives in the reference, which is
// published with the code (it used to be section 6.2 of a build plan, which is
// not). The tests below keep their names.

const rulesFile = "../../docs/reference/gonk-pipeline-fix-rules.md"

// keyGroupSection returns section 2 of the rules page: the key-group table
// and the implicit-stages paragraph.
func keyGroupSection(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatalf("the rules page is the authority for the key lists and it cannot be read: %v", err)
	}
	text := string(b)
	start := strings.Index(text, "\n## 2. CI definition integrity: the key groups\n")
	end := strings.Index(text, "\n## 3. ")
	if start < 0 || end < start {
		t.Fatalf("section 2 (the key groups) not found in %s", rulesFile)
	}
	return text[start:end]
}

var backticked = regexp.MustCompile("`([^`]+)`")

// planKeyRow returns the Keys cell of the row of the key-group table whose
// first cell is group.
func planKeyRow(t *testing.T, group string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(keyGroupSection(t), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) >= 4 && strings.TrimSpace(cells[1]) == group {
			found = append(found, cells[2])
		}
	}
	if len(found) != 1 {
		t.Fatalf("the key-group table has %d rows for key group %q, want exactly 1", len(found), group)
	}
	return found[0]
}

func ticked(cell string) []string {
	out := []string{}
	for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

// planKeys returns the job keys of the GATING, EXEC and free rows AS THE PLAN
// LISTS THEM. The per-key tests below iterate these, not the code's lists: a
// key that falls out of the code keeps its test, and the test goes red.
func planKeys(t *testing.T) (gating, exec, free []string) {
	t.Helper()
	jobPart, _, ok := strings.Cut(planKeyRow(t, "GATING"), "; global ")
	if !ok {
		t.Fatal("the GATING row no longer separates job keys from global keys with \"; global \"")
	}
	gating, exec, free = ticked(jobPart), ticked(planKeyRow(t, "EXEC")), ticked(planKeyRow(t, "free"))
	if len(gating) == 0 || len(exec) == 0 || len(free) == 0 {
		t.Fatalf("a key row of the key-group table is empty: gating %q exec %q free %q", gating, exec, free)
	}
	return gating, exec, free
}

func TestKeyListsMatchThePlan(t *testing.T) {
	gating := planKeyRow(t, "GATING")
	jobPart, globalPart, ok := strings.Cut(gating, "; global ")
	if !ok {
		t.Fatalf("the GATING row no longer separates job keys from global keys with \"; global \": %q", gating)
	}
	exec := planKeyRow(t, "EXEC")
	if !strings.Contains(exec, "`variables` (job and global)") {
		t.Errorf("the EXEC row no longer says `variables` is both a job and a global key: %q", exec)
	}
	for _, c := range []struct {
		name string
		code []string
		plan []string
	}{
		{"GATING job keys", GatingJobKeys, ticked(jobPart)},
		{"GATING global keys", GatingGlobalKeys, ticked(globalPart)},
		{"EXEC job keys", ExecJobKeys, ticked(exec)},
		{"EXEC global keys", ExecGlobalKeys, []string{"variables"}},
		{"free job keys", FreeJobKeys, ticked(planKeyRow(t, "free"))},
		{"existence keys", []string{KeyStage}, ticked(planKeyRow(t, "existence"))},
	} {
		// Same keys in the same ORDER: the lists are the table, read left to
		// right, so a diff of one is a diff of the other.
		if !reflect.DeepEqual(c.code, c.plan) {
			t.Errorf("%s drifted from the key-group table:\n  code %q\n  page %q", c.name, c.code, c.plan)
		}
	}
	if !strings.Contains(planKeyRow(t, "free"), "new jobs, new stages appended") {
		t.Errorf("the free row no longer names new jobs and appended stages")
	}
}

func TestEveryKeyIsInExactlyOneClass(t *testing.T) {
	seen := map[string]string{}
	for _, l := range []struct {
		name  string
		keys  []string
		class Class
	}{
		{"existence", []string{KeyStage}, ClassExistence},
		{"gating", GatingJobKeys, ClassGating},
		{"exec", ExecJobKeys, ClassExec},
		{"free", FreeJobKeys, ClassFree},
	} {
		if len(l.keys) == 0 {
			t.Errorf("the %s list is empty", l.name)
		}
		for _, k := range l.keys {
			if prev, dup := seen[k]; dup {
				t.Errorf("key %q is in both %s and %s", k, prev, l.name)
			}
			seen[k] = l.name
			if got := Classify(k); got != l.class {
				t.Errorf("Classify(%q) = %q, want %q", k, got, l.class)
			}
		}
	}
	for _, k := range []string{"extends", "mystery_knob", "", "Script", "release", "run"} {
		if got := Classify(k); got != ClassUnclassified {
			t.Errorf("Classify(%q) = %q, want %q: a key the table does not name is never free", k, got, ClassUnclassified)
		}
	}
}

// ---------------------------------------------------------------------------
// GATING (D2): each key changed is reported.

func TestEachGatingJobKeyChangedIsReported(t *testing.T) {
	// Which jobs' effective definition the fixture changes. Not always the
	// job whose text changed: gating_rules edits a template two jobs extend.
	jobs := map[string][]string{
		"rules":         {"lint-widgets", "test-widgets"},
		"only":          {"bundle"},
		"except":        {"bundle"},
		"when":          {"lint-widgets"},
		"allow_failure": {"test-widgets"},
		"retry":         {"test-widgets"},
		"start_in":      {"ship-later"},
		"trigger":       {"downstream"},
		"parallel":      {"test-widgets"},
	}
	planGating, _, _ := planKeys(t)
	for _, key := range planGating {
		t.Run(key, func(t *testing.T) {
			js, ok := jobs[key]
			if !ok {
				t.Fatalf("GATING key %q has no fixture: add testdata/keys/gating_%s.yml and its row here", key, key)
			}
			// The diagnosed job is test-widgets: being diagnosed never
			// excuses a GATING change.
			d := keysDiff(t, "gating_"+key, "test-widgets 1/2")
			var wantV, wantC []string
			for _, j := range js {
				wantV = append(wantV, "D2 "+j+"/"+key)
				wantC = append(wantC, j+"/"+key+"=gating")
			}
			wantStrings(t, "violations", vio(d), wantV)
			wantStrings(t, "changes", changes(d), wantC)
			wantStrings(t, "unresolvable", unres(d), nil)
			if d.Kind != KindViolation {
				t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
			}
		})
	}
}

func TestEachGatingGlobalKeyChangedIsReported(t *testing.T) {
	cases := map[string][]struct {
		fixture string
		vio     []string
		changes []string
		globals []string
	}{
		"workflow": {{
			fixture: "gating_global_workflow",
			vio:     []string{"D2 /workflow"},
			globals: []string{"workflow=gating"},
		}},
		"stages": {
			{
				fixture: "gating_global_stages_reordered",
				vio:     []string{"D2 /stages"},
				globals: []string{"stages=gating"},
			},
			{
				// Inserted, not appended: the base order survives as a
				// subsequence, and that is not enough.
				fixture: "gating_global_stages_inserted",
				vio:     []string{"D2 /stages"},
				globals: []string{"stages=gating"},
			},
		},
		"default.retry": {{
			fixture: "gating_global_default_retry",
			// The global key, and every job that takes its retry from it.
			// test-widgets sets its own retry and is untouched.
			vio: []string{
				"D2 /default.retry", "D2 bundle/retry", "D2 downstream/retry", "D2 fetch-parts/retry",
				"D2 lint-widgets/retry", "D2 ship-later/retry",
			},
			changes: []string{
				"bundle/retry=gating", "downstream/retry=gating", "fetch-parts/retry=gating",
				"lint-widgets/retry=gating", "ship-later/retry=gating",
			},
			globals: []string{"default.retry=gating"},
		}},
	}
	for _, key := range GatingGlobalKeys {
		cs, ok := cases[key]
		if !ok {
			t.Errorf("GATING global key %q has no fixture", key)
			continue
		}
		for _, c := range cs {
			t.Run(c.fixture, func(t *testing.T) {
				d := keysDiff(t, c.fixture, "test-widgets 1/2")
				wantStrings(t, "violations", vio(d), c.vio)
				wantStrings(t, "changes", changes(d), c.changes)
				wantStrings(t, "globals", globals(d), c.globals)
				wantStrings(t, "unresolvable", unres(d), nil)
				if d.Kind != KindViolation {
					t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// EXEC (D3).

func TestExecChangeOnANonDiagnosedJobIsAViolation(t *testing.T) {
	_, planExec, _ := planKeys(t)
	for _, key := range planExec {
		t.Run(key, func(t *testing.T) {
			// lint-widgets changed; test-widgets was diagnosed.
			d := keysDiff(t, "exec_"+key+"_other", "test-widgets 1/2")
			wantStrings(t, "violations", vio(d), []string{"D3 lint-widgets/" + key})
			wantStrings(t, "changes", changes(d), []string{"lint-widgets/" + key + "=exec"})
			wantStrings(t, "unresolvable", unres(d), nil)
			if d.Kind != KindViolation {
				t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
			}
			if len(d.Jobs) == 1 && d.Jobs[0].Diagnosed {
				t.Errorf("lint-widgets is reported as diagnosed")
			}
		})
	}
}

func TestExecChangeOnADiagnosedJobIsExecChanging(t *testing.T) {
	_, planExec, _ := planKeys(t)
	for _, key := range planExec {
		t.Run(key, func(t *testing.T) {
			d := keysDiff(t, "exec_"+key+"_diagnosed", "test-widgets 1/2")
			wantStrings(t, "violations", vio(d), nil)
			wantStrings(t, "changes", changes(d), []string{"test-widgets/" + key + "=exec"})
			wantStrings(t, "unresolvable", unres(d), nil)
			if d.Kind != KindExecChanging {
				t.Errorf("Kind = %q, want %q", d.Kind, KindExecChanging)
			}
			if len(d.Jobs) != 1 || !d.Jobs[0].Diagnosed {
				t.Errorf("test-widgets is not reported as diagnosed: %+v", d.Jobs)
			}

			// The SAME change with nothing diagnosed is a violation: the
			// permission comes from the diagnosed set and from nowhere else.
			d = keysDiff(t, "exec_"+key+"_diagnosed")
			wantStrings(t, "violations (nothing diagnosed)", vio(d), []string{"D3 test-widgets/" + key})
		})
	}
}

func TestGlobalVariablesAreAnExecKeyOfEveryJob(t *testing.T) {
	var jobChanges, jobVio []string
	for _, j := range keysJobs {
		jobChanges = append(jobChanges, j+"/variables=exec")
		if j != "test-widgets" {
			jobVio = append(jobVio, "D3 "+j+"/variables")
		}
	}
	for _, key := range ExecGlobalKeys {
		if key != GlobalVariables {
			t.Errorf("EXEC global key %q has no fixture", key)
		}
	}

	d := keysDiff(t, "exec_global_variables", "test-widgets 1/2")
	wantStrings(t, "violations", vio(d), append([]string{"D3 /variables"}, jobVio...))
	wantStrings(t, "changes", changes(d), jobChanges)
	wantStrings(t, "globals", globals(d), []string{"variables=exec"})
	if d.Kind != KindViolation {
		t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
	}

	// With EVERY base job diagnosed there is no job outside T to protect.
	d = keysDiff(t, "exec_global_variables", "bundle", "downstream", "fetch-parts", "lint-widgets", "ship-later", "test-widgets 2/2")
	wantStrings(t, "violations (all diagnosed)", vio(d), nil)
	wantStrings(t, "globals (all diagnosed)", globals(d), []string{"variables=exec"})
	if d.Kind != KindExecChanging {
		t.Errorf("all diagnosed: Kind = %q, want %q", d.Kind, KindExecChanging)
	}
}

// ---------------------------------------------------------------------------
// free.

func TestFreeKeysNewJobsAndAppendedStagesArePreserving(t *testing.T) {
	type want struct {
		changes []string
		added   []string
		globals []string
	}
	cases := map[string]want{
		"free_new_job":        {added: []string{"extra-report"}},
		"free_stage_appended": {added: []string{"tidy-up"}, globals: []string{"stages=free+after"}},
	}
	_, _, planFree := planKeys(t)
	for _, key := range planFree {
		cases["free_"+key] = want{changes: []string{"lint-widgets/" + key + "=free"}}
	}
	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			d := keysDiff(t, name, "test-widgets 1/2")
			wantStrings(t, "violations", vio(d), nil)
			wantStrings(t, "unresolvable", unres(d), nil)
			wantStrings(t, "changes", changes(d), w.changes)
			wantStrings(t, "added", append([]string{}, d.Added...), w.added)
			wantStrings(t, "globals", globals(d), w.globals)
			wantStrings(t, "removed", append([]string{}, d.Removed...), nil)
			if d.Kind != KindPreserving {
				t.Errorf("Kind = %q, want %q", d.Kind, KindPreserving)
			}
		})
	}
}

func TestIdenticalDefinitionsAreNone(t *testing.T) {
	b := fixture(t, "keys/base.yml")
	d := Diff(b, b, []string{"test-widgets 1/2"})
	if d.Kind != KindNone {
		t.Errorf("Kind = %q, want %q", d.Kind, KindNone)
	}
	if len(d.Jobs)+len(d.Removed)+len(d.Added)+len(d.Renamed)+len(d.Globals)+len(d.Violations)+len(d.Unresolvable) != 0 {
		t.Errorf("an identical definition reports something: %+v", d)
	}
	// And the base really was read: six jobs, three stages, a workflow.
	def := Effective(b)
	var names []string
	for n := range def.Jobs {
		names = append(names, n)
	}
	wantStrings(t, "jobs of keys/base.yml", sortedCopy(names), keysJobs)
	if !reflect.DeepEqual(def.Stages, []string{"prepare", "check", "pack"}) || !def.HasWorkflow || !def.HasDefaultRetry || !def.HasVariables {
		t.Errorf("globals of keys/base.yml not read: stages %q workflow %v default.retry %v variables %v",
			def.Stages, def.HasWorkflow, def.HasDefaultRetry, def.HasVariables)
	}
	if len(def.Problems) != 0 {
		t.Errorf("keys/base.yml has problems: %q", def.Problems)
	}
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

func TestAnEditWithNoEffectIsNone(t *testing.T) {
	d := pairDiff(t, "unused_template", "assemble")
	if d.Kind != KindNone || len(d.Jobs)+len(d.Violations)+len(d.Unresolvable) != 0 {
		t.Errorf("a template nobody extends changed and the delta is %+v, want none", d)
	}
}

// ---------------------------------------------------------------------------
// unresolvable is never "equal".

func TestReferenceIsUnresolvableNotEqual(t *testing.T) {
	base, head := fixture(t, "reference_identical/base.yml"), fixture(t, "reference_identical/head.yml")
	if string(base) != string(head) {
		t.Fatal("the fixture's point is that the two sides are byte-identical")
	}
	d := Diff(base, head, []string{"assemble"})
	if d.Kind != KindUnresolvable {
		t.Errorf("Kind = %q, want %q: a !reference was compared as if it had been resolved", d.Kind, KindUnresolvable)
	}
	wantStrings(t, "unresolvable", unres(d), []string{"both assemble/script"})
	wantStrings(t, "violations", vio(d), nil)
	for _, u := range d.Unresolvable {
		if !strings.Contains(u.Reason, "!reference") {
			t.Errorf("the reason does not name the tag: %q", u.Reason)
		}
	}
	// The value in the effective definition is marked, not silently a list.
	def := Effective(base)
	if j := def.Jobs["assemble"]; j == nil || !hasUnresolved(j.Keys["script"]) {
		t.Errorf("Effective left no Unresolved marker in assemble.script: %#v", def.Jobs["assemble"])
	}
}

// hasUnresolved is the TEST's own walk, so the check above does not lean on
// the implementation's.
func hasUnresolved(v any) bool {
	switch x := v.(type) {
	case Unresolved:
		return true
	case []any:
		for _, e := range x {
			if hasUnresolved(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if hasUnresolved(e) {
				return true
			}
		}
	}
	return false
}

func TestUnresolvableConstructs(t *testing.T) {
	for _, c := range []struct {
		dir       string
		diagnosed []string
		kind      Kind
		unres     []string
		changes   []string
		reason    string // a substring every reason must hold
	}{
		{dir: "reference_identical", kind: KindUnresolvable, unres: []string{"both assemble/script"}, reason: "!reference"},
		// The tag is in a template key the job overrides: nothing effective
		// holds it, so there is nothing unresolved to report.
		{dir: "reference_overridden", kind: KindNone},
		// In a FREE key the tag is reported as a (permitted) change, never as
		// equal and never as a reason to refuse.
		{dir: "reference_free_key", kind: KindPreserving, changes: []string{"assemble/cache=free"}},
		{dir: "include_left", kind: KindUnresolvable, unres: []string{"both /"}, reason: "include"},
		{dir: "child_pipeline", kind: KindUnresolvable, unres: []string{"both fan-out/trigger"}, reason: "child pipeline"},
		{dir: "extends_missing", kind: KindUnresolvable, unres: []string{"head assemble/"}, reason: ".tmpl-gone"},
		{dir: "extends_cycle", kind: KindUnresolvable, unres: []string{"both assemble/"}, reason: "cycle"},
		{dir: "unknown_key", kind: KindUnresolvable, unres: []string{"both assemble/mystery_knob"},
			changes: []string{"assemble/mystery_knob=unclassified"}, reason: "6.2"},
		{dir: "extends_not_names", kind: KindUnresolvable, unres: []string{"both assemble/"}, reason: "extends"},
		{dir: "inherit_malformed", kind: KindUnresolvable, unres: []string{"both assemble/"}, reason: "inherit.default"},
		// A global variable that is a !reference reaches the job that
		// inherits it, and not the job that inherits no variables.
		{dir: "global_variables_reference", diagnosed: []string{"measure"}, kind: KindUnresolvable,
			unres: []string{"both /variables", "both assemble/variables"}, reason: "!reference"},
		{dir: "merge_not_mapping", kind: KindUnresolvable, unres: []string{"both assemble/"}, reason: "merge key"},
		// Twelve templates deep is refused; eleven is resolved.
		{dir: "extends_deep", diagnosed: []string{"deep-enough", "too-deep"}, kind: KindUnresolvable,
			unres: []string{"base assemble/", "both too-deep/"}, reason: ""},
	} {
		t.Run(c.dir, func(t *testing.T) {
			d := pairDiff(t, c.dir, append([]string{"assemble"}, c.diagnosed...)...)
			if d.Kind != c.kind {
				t.Errorf("Kind = %q, want %q", d.Kind, c.kind)
			}
			wantStrings(t, "unresolvable", unres(d), c.unres)
			wantStrings(t, "changes", changes(d), c.changes)
			wantStrings(t, "violations", vio(d), nil)
			for _, u := range d.Unresolvable {
				if !strings.Contains(u.Reason, c.reason) {
					t.Errorf("reason %q does not mention %q", u.Reason, c.reason)
				}
			}
		})
	}
}

func TestADocumentThatCannotBeReadIsUnresolvable(t *testing.T) {
	ok := fixture(t, "broken/ok.yml")
	for _, name := range []string{
		"not_yaml", "duplicate_job", "root_list", "job_scalar", "two_documents", "alias_loop", "default_twice", "empty",
	} {
		bad := fixture(t, "broken/"+name+".yml")
		for _, c := range []struct {
			what       string
			base, head []byte
			side       Side
		}{
			{"head", ok, bad, SideHead},
			{"base", bad, ok, SideBase},
			{"both", bad, bad, SideBoth},
		} {
			t.Run(name+"/"+c.what, func(t *testing.T) {
				d := Diff(c.base, c.head, []string{"assemble"})
				if d.Kind != KindUnresolvable {
					t.Errorf("Kind = %q, want %q", d.Kind, KindUnresolvable)
				}
				// A document that cannot be read has no jobs, and that must
				// never be reported as "every job was removed" (FORGED).
				wantStrings(t, "violations", vio(d), nil)
				wantStrings(t, "removed", append([]string{}, d.Removed...), nil)
				if len(d.Unresolvable) == 0 {
					t.Fatal("nothing reported as unresolvable")
				}
				for _, u := range d.Unresolvable {
					if u.Side != c.side || u.Reason == "" {
						t.Errorf("finding %+v: want side %q and a reason", u, c.side)
					}
				}
			})
		}
	}
	// nil is not a definition either.
	if d := Compare(nil, Effective(ok), nil); d.Kind != KindUnresolvable {
		t.Errorf("Compare(nil, ...) Kind = %q, want %q", d.Kind, KindUnresolvable)
	}
	if d := Compare(Effective(ok), nil, nil); d.Kind != KindUnresolvable {
		t.Errorf("Compare(..., nil) Kind = %q, want %q", d.Kind, KindUnresolvable)
	}
}

func TestADuplicateKeyInsideAJobIsUnresolvable(t *testing.T) {
	d := Diff(fixture(t, "broken/ok.yml"), fixture(t, "broken/duplicate_key.yml"), []string{"assemble"})
	if d.Kind != KindUnresolvable {
		t.Errorf("Kind = %q, want %q", d.Kind, KindUnresolvable)
	}
	wantStrings(t, "unresolvable", unres(d), []string{"head assemble/"})
	wantStrings(t, "violations", vio(d), nil)
}

func TestADiagnosedJobWithNoDefinitionIsUnresolvable(t *testing.T) {
	b := fixture(t, "keys/base.yml")
	d := Diff(b, b, []string{"no-such-job"})
	if d.Kind != KindUnresolvable {
		t.Errorf("Kind = %q, want %q", d.Kind, KindUnresolvable)
	}
	wantStrings(t, "unresolvable", unres(d), []string{"base no-such-job/"})
}

// A violation and an unresolvable construct in one comparison: both are
// reported in full, and Kind follows J5 (FORGED outranks UNVERIFIABLE).
func TestAViolationOutranksAnUnresolvable(t *testing.T) {
	d := pairDiff(t, "violation_and_unresolvable", "assemble")
	wantStrings(t, "violations", vio(d), []string{"D2 measure/allow_failure"})
	wantStrings(t, "unresolvable", unres(d), []string{"both assemble/script"})
	if d.Kind != KindViolation {
		t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
	}
}

func TestExtendsDepth(t *testing.T) {
	def := Effective(fixture(t, "extends_deep/base.yml"))
	wantValue(t, "deep-enough.script (eleven levels)", jobKeys(t, def, "deep-enough")["script"], []any{"echo bottom"})
	if j := def.Jobs["too-deep"]; j == nil || len(j.Problems) != 1 || !strings.Contains(j.Problems[0], "deeper than 11") {
		t.Errorf("too-deep (twelve levels) was resolved: %+v", j)
	}
}

func TestExtendsOf(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal(fixture(t, "extends_chain/base.yml"), &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	node := func(name string) *yaml.Node {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == name {
				return root.Content[i+1]
			}
		}
		t.Fatalf("no key %q", name)
		return nil
	}
	wantValue(t, "one name", ExtendsOf(node("deep")), []string{".leafish"})
	wantValue(t, "a list, in order", ExtendsOf(node("both")), []string{".middle", ".other"})
	wantValue(t, "no extends", ExtendsOf(node(".root")), []string(nil))
	wantValue(t, "not a mapping", ExtendsOf(node("deep").Content[1]), []string(nil))
	wantValue(t, "nil", ExtendsOf(nil), []string(nil))
}

// ---------------------------------------------------------------------------
// Resolution: anchors, merge keys, extends, default, inherit. Each fixture's
// head.yml is the base written out BY HAND with the construct removed; the
// two must be the same definition. A direct look at the values follows, so
// "none" cannot be earned by resolving nothing on both sides.

func TestResolvedFormsEqualTheirHandExpansion(t *testing.T) {
	for _, dir := range []string{"anchors_inlined", "extends_chain", "default_inherit"} {
		t.Run(dir, func(t *testing.T) {
			d := pairDiff(t, dir)
			if d.Kind != KindNone {
				t.Errorf("Kind = %q, want %q", d.Kind, KindNone)
			}
			wantStrings(t, "changes", changes(d), nil)
			wantStrings(t, "violations", vio(d), nil)
			wantStrings(t, "unresolvable", unres(d), nil)
			wantStrings(t, "removed", append([]string{}, d.Removed...), nil)
			wantStrings(t, "added", append([]string{}, d.Added...), nil)
		})
	}
}

func jobKeys(t *testing.T, def *Definition, job string) map[string]any {
	t.Helper()
	j := def.Jobs[job]
	if j == nil {
		t.Fatalf("no job %q in the definition (jobs: %v)", job, reflect.ValueOf(def.Jobs).MapKeys())
	}
	if len(j.Problems) != 0 {
		t.Fatalf("job %q has problems: %q", job, j.Problems)
	}
	return j.Keys
}

func wantValue(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n  got  %#v\n  want %#v", what, got, want)
	}
}

func TestEffectiveResolvesAnchorsAndMergeKeys(t *testing.T) {
	def := Effective(fixture(t, "anchors_inlined/base.yml"))
	steps := []any{"./tools/one.sh", "./tools/two.sh"}
	first := jobKeys(t, def, "first")
	wantValue(t, "first.script (an alias)", first["script"], steps)
	wantValue(t, "first.image (a merge key)", first["image"], "registry.invalid/tools/a:1")
	second := jobKeys(t, def, "second")
	wantValue(t, "second.image (the EARLIER entry of a merge list wins)", second["image"], "registry.invalid/tools/a:1")
	wantValue(t, "second.tags (from the later entry)", second["tags"], []any{"wide"})
	wantValue(t, "second.variables (an explicit key replaces a merged one whole)", second["variables"], map[string]any{"LANE": "fast"})
	third := jobKeys(t, def, "third")
	wantValue(t, "third.image (an explicit key beats a merged one)", third["image"], "registry.invalid/tools/c:3")
	if _, leaked := first["<<"]; leaked {
		t.Errorf("the merge key itself is a key of the job")
	}
	if _, isJob := def.Jobs[".fragments"]; isJob {
		t.Errorf("a hidden key is listed as a job")
	}
}

func TestEffectiveResolvesExtends(t *testing.T) {
	def := Effective(fixture(t, "extends_chain/base.yml"))
	deep := jobKeys(t, def, "deep")
	wantValue(t, "deep.image (three levels up)", deep["image"], "registry.invalid/tools/root:1")
	wantValue(t, "deep.variables (hashes merge, the nearer wins)", deep["variables"],
		map[string]any{"LANE": "slow", "DEPTH": "2", "EXTRA": "yes"})
	wantValue(t, "deep.script (arrays are replaced)", deep["script"], []any{"echo leafish"})
	wantValue(t, "deep.tags", deep["tags"], []any{"narrow"})
	if _, kept := deep["extends"]; kept {
		t.Errorf("`extends` is still a key of the effective definition")
	}
	both := jobKeys(t, def, "both")
	wantValue(t, "both.image (the LATER template of a list wins)", both["image"], "registry.invalid/tools/other:7")
	wantValue(t, "both.variables", both["variables"], map[string]any{"LANE": "slow", "DEPTH": "2", "COLOUR": "teal"})
	wantValue(t, "both.script (the job's own)", both["script"], []any{"echo both"})
	plain := def.Jobs["plain"]
	if plain == nil || plain.Stage != "deploy" {
		t.Errorf("plain (extends a visible job) stage: %+v", plain)
	}
	if def.Jobs["deep"].Stage != DefaultStage {
		t.Errorf("a job without `stage` is in %q, got %q", DefaultStage, def.Jobs["deep"].Stage)
	}
	if !reflect.DeepEqual(def.Stages, DefaultStages) {
		t.Errorf("a document without `stages` has %q, got %q", DefaultStages, def.Stages)
	}
}

func TestEffectiveAppliesDefaultAndInherit(t *testing.T) {
	def := Effective(fixture(t, "default_inherit/base.yml"))
	all := jobKeys(t, def, "takes-all")
	wantValue(t, "takes-all.image (default)", all["image"], "registry.invalid/tools/base:1")
	wantValue(t, "takes-all.before_script (deprecated top-level key)", all["before_script"], []any{"echo global-before"})
	wantValue(t, "takes-all.variables (global)", all["variables"], map[string]any{"LANE": "slow", "COLOUR": "teal"})
	own := jobKeys(t, def, "own-image")
	wantValue(t, "own-image.image (the job's own beats default)", own["image"], "registry.invalid/tools/own:5")
	wantValue(t, "own-image.variables (job over global, by name)", own["variables"], map[string]any{"LANE": "fast", "COLOUR": "teal"})
	none := jobKeys(t, def, "takes-none")
	for _, k := range []string{"image", "tags", "before_script", "variables"} {
		if v, has := none[k]; has {
			t.Errorf("takes-none (inherit: false) has %s = %#v", k, v)
		}
	}
	some := jobKeys(t, def, "takes-some")
	wantValue(t, "takes-some.image (listed in inherit.default)", some["image"], "registry.invalid/tools/base:1")
	if v, has := some["tags"]; has {
		t.Errorf("takes-some has tags = %#v, not listed in inherit.default", v)
	}
	wantValue(t, "takes-some.variables (only the listed global)", some["variables"], map[string]any{"COLOUR": "teal"})

	// default.retry reaches a job's effective retry.
	keys := Effective(fixture(t, "keys/base.yml"))
	wantValue(t, "lint-widgets.retry (default.retry)", jobKeys(t, keys, "lint-widgets")["retry"], 1)
	wantValue(t, "test-widgets.retry (its own)", jobKeys(t, keys, "test-widgets")["retry"], 0)
	wantValue(t, "lint-widgets.image (extends beats default)", jobKeys(t, keys, "lint-widgets")["image"], "registry.invalid/tools/check:2")
	wantValue(t, "fetch-parts.cache (an alias)", jobKeys(t, keys, "fetch-parts")["cache"],
		map[string]any{"key": "widgets", "paths": []any{".cache/"}})
}

func TestAChangeInsideAnAnchorReachesEveryJobThatUsesIt(t *testing.T) {
	d := pairDiff(t, "anchor_changed", "first")
	wantStrings(t, "changes", changes(d), []string{"first/script=exec", "second/script=exec"})
	wantStrings(t, "violations", vio(d), []string{"D3 second/script"})
	if d.Kind != KindViolation {
		t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
	}
}

// ---------------------------------------------------------------------------
// Existence (D1).

func TestRemovedRenamedAndHiddenJobs(t *testing.T) {
	for _, c := range []struct {
		dir     string
		removed []string
		added   []string
		renamed []Rename
	}{
		{dir: "job_removed", removed: []string{"measure"}},
		{dir: "job_renamed", removed: []string{"measure"}, added: []string{"measure-later"},
			renamed: []Rename{{From: "measure", To: "measure-later"}}},
		// A hidden job is a template: GitLab never runs it.
		{dir: "job_hidden", removed: []string{"measure"}},
		// Gone, and something else added: not a rename.
		{dir: "job_replaced", removed: []string{"measure"}, added: []string{"survey"}},
	} {
		t.Run(c.dir, func(t *testing.T) {
			d := pairDiff(t, c.dir, "assemble")
			wantStrings(t, "removed", append([]string{}, d.Removed...), c.removed)
			wantStrings(t, "added", append([]string{}, d.Added...), c.added)
			wantStrings(t, "violations", vio(d), []string{"D1 measure/"})
			if !reflect.DeepEqual(d.Renamed, c.renamed) {
				t.Errorf("renamed = %+v, want %+v", d.Renamed, c.renamed)
			}
			if d.Kind != KindViolation {
				t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
			}
		})
	}
}

func TestAChangedStageIsAnExistenceViolation(t *testing.T) {
	d := pairDiff(t, "stage_changed", "assemble")
	// assemble moved. measure's explicit `stage: test` is its default
	// spelled out, and is not a change.
	wantStrings(t, "violations", vio(d), []string{"D1 assemble/stage"})
	wantStrings(t, "changes", changes(d), []string{"assemble/stage=existence"})
	if d.Kind != KindViolation {
		t.Errorf("Kind = %q, want %q", d.Kind, KindViolation)
	}
}

// ---------------------------------------------------------------------------

func TestDefinitionName(t *testing.T) {
	def := Effective(fixture(t, "keys/base.yml"))
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"lint-widgets", "lint-widgets", true},
		{"test-widgets", "test-widgets", true},
		{"test-widgets 1/2", "test-widgets", true},
		{"test-widgets 2/2", "test-widgets", true},
		{"test-widgets: [a, b]", "test-widgets", true},
		// lint-widgets has no `parallel`: a suffix on it is not a job of it.
		{"lint-widgets 1/2", "", false},
		{"lint-widgets: [a]", "", false},
		{".tmpl-check", "", false},
		{"nope", "", false},
		{"", "", false},
	} {
		got, ok := DefinitionName(def, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("DefinitionName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if _, ok := DefinitionName(nil, "x"); ok {
		t.Errorf("DefinitionName(nil) found a job")
	}
}

func TestCompareIsDeterministic(t *testing.T) {
	base, head := fixture(t, "keys/base.yml"), fixture(t, "keys/exec_global_variables.yml")
	first := fmt.Sprintf("%+v", Diff(base, head, []string{"test-widgets 1/2"}))
	for i := 0; i < 20; i++ {
		if again := fmt.Sprintf("%+v", Diff(base, head, []string{"test-widgets 1/2"})); again != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
}

// ---------------------------------------------------------------------------
// GitLab's own merged YAML (gonk-er0o).
//
// testdata/gitlab_merged/*.yml are NOT hand-written: each is the `merged_yaml`
// of a STATIC ci/lint reply (GET .../ci/lint?content_ref=<sha>&
// include_merged_yaml=true), byte for byte, recorded by the maintainers'
// pipefix probe (pipefix_probe.py --keep-json, not published) on the synthetic
// project 75 (GitLab CE 19.4.1, run
// 20261007T145957Z): the probe's base commit and four of its case commits.
// What they show that no hand-written fixture did (plan 2.2, G4 REFUTED):
// `stages` GAINS ".pre" first and ".post" last; `include:` is expanded and the
// key is gone; `extends` is resolved AND the key is kept; `!reference` is
// resolved and no tag is left; anchors are resolved; `default:` is kept.

func mergedDiff(t *testing.T, head string, diagnosed ...string) Delta {
	t.Helper()
	return Diff(fixture(t, "gitlab_merged/base.yml"), fixture(t, "gitlab_merged/"+head+".yml"), diagnosed)
}

func TestGitLabMergedYAMLResolves(t *testing.T) {
	for _, name := range []string{"base", "stage_appended", "stage_inserted", "job_added", "include"} {
		raw := fixture(t, "gitlab_merged/"+name+".yml")
		// The shape this package is fed, asserted on the bytes so a fixture
		// somebody "tidied" is caught.
		for _, want := range []string{"stages:\n- \".pre\"\n", "\n- \".post\"\n", "\n  extends: \".probe-job\"\n", "\ndefault:\n"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s: the recorded merged YAML does not hold %q", name, want)
			}
		}
		for _, never := range []string{"include:", "!reference", "<<", ": &", ": *"} {
			if strings.Contains(string(raw), never) {
				t.Errorf("%s: the recorded merged YAML holds %q", name, never)
			}
		}
		def := Effective(raw)
		if len(def.Problems) != 0 {
			t.Fatalf("%s: problems %q", name, def.Problems)
		}
		for _, job := range []string{"probe-lint", "probe-status", "probe-report"} {
			j := def.Jobs[job]
			if j == nil || len(j.Problems) != 0 {
				t.Fatalf("%s: job %s = %+v", name, job, j)
			}
			if _, un := firstUnresolved(j.Keys); un {
				t.Errorf("%s: job %s holds an unresolved value", name, job)
			}
		}
		// `default:` is kept by GitLab and applied here; `extends` arrives
		// resolved and is resolved again, to the same thing.
		keys := def.Jobs["probe-status"].Keys
		wantValue(t, name+": interruptible (from default:)", keys["interruptible"], true)
		wantValue(t, name+": tags (from the template)", keys["tags"], []any{"cluster"})
		if _, has := keys["extends"]; has {
			t.Errorf("%s: `extends` is a key of the effective definition", name)
		}
	}
	// Stages are kept AS WRITTEN: the implicit two are in the document.
	if got, want := Effective(fixture(t, "gitlab_merged/base.yml")).Stages, []string{".pre", "check", "report", ".post"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stages of the base = %q, want %q", got, want)
	}
}

// TestGitLabMergedYAMLDeltas: section 6.2 over the real documents. The first
// row is gonk-er0o: an APPENDED stage sits before GitLab's trailing ".post",
// so the base list is not a prefix of the head list -- and it is still free.
func TestGitLabMergedYAMLDeltas(t *testing.T) {
	for _, tc := range []struct {
		head    string
		kind    Kind
		vio     []string
		globals []string
		added   []string
		changes []string
	}{
		{head: "base", kind: KindNone},
		{head: "stage_appended", kind: KindPreserving, globals: []string{"stages=free+later"}},
		{head: "stage_inserted", kind: KindViolation, vio: []string{"D2 /stages"}, globals: []string{"stages=gating"}},
		// A new job written with `!reference`: GitLab resolved the tag, so
		// nothing here is unresolvable and the change is the free one it is.
		{head: "job_added", kind: KindPreserving, added: []string{"probe-extra"}},
		// The change is in the locally INCLUDED file only (every job's
		// before_script): seen, because the include is expanded at the commit.
		{head: "include", kind: KindViolation,
			vio:     []string{"D3 probe-lint/before_script", "D3 probe-report/before_script"},
			changes: []string{"probe-lint/before_script=exec", "probe-report/before_script=exec", "probe-status/before_script=exec"}},
	} {
		t.Run(tc.head, func(t *testing.T) {
			d := mergedDiff(t, tc.head, "probe-status")
			if d.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q (violations %q, unresolvable %q)", d.Kind, tc.kind, vio(d), unres(d))
			}
			wantStrings(t, "violations", vio(d), tc.vio)
			wantStrings(t, "unresolvable", unres(d), nil)
			wantStrings(t, "globals", globals(d), tc.globals)
			wantStrings(t, "changes", changes(d), tc.changes)
			if tc.added == nil {
				tc.added = []string{}
			}
			if got := append([]string{}, d.Added...); !reflect.DeepEqual(got, tc.added) {
				t.Errorf("added = %q, want %q", got, tc.added)
			}
		})
	}
}

// stagesDoc is a small document with the given `stages:` (nil: no key) and
// one job per (name, stage) pair.
func stagesDoc(stages []string, jobs ...string) []byte {
	var b strings.Builder
	if stages != nil {
		b.WriteString("stages:\n")
		for _, s := range stages {
			fmt.Fprintf(&b, "- %q\n", s)
		}
	}
	for i := 0; i+1 < len(jobs); i += 2 {
		fmt.Fprintf(&b, "%s:\n  stage: %q\n  script:\n  - count crates\n", jobs[i], jobs[i+1])
	}
	return []byte(b.String())
}

// TestImplicitStagesAreNotStagesOfTheList: ".pre" and ".post" exist in every
// pipeline, first and last, whether or not `stages:` names them and wherever
// it names them. GitLab's merged YAML always carries them; a hand-written
// document usually does not. They are taken out of BOTH sides before the
// base order is compared -- and out of nothing else: a job's `stage` is still
// compared as written.
func TestImplicitStagesAreNotStagesOfTheList(t *testing.T) {
	pre, post := StagePre, StagePost
	jobs := []string{"tally", "a", "pack", "b"}
	for _, tc := range []struct {
		name       string
		base, head []string
		headJobs   []string
		kind       Kind
		vio        []string
		globals    []string
	}{
		{name: "the same list, merged on the head side only",
			base: []string{"a", "b"}, head: []string{pre, "a", "b", post}, kind: KindNone},
		{name: "the same list, merged on the base side only",
			base: []string{pre, "a", "b", post}, head: []string{"a", "b"}, kind: KindNone},
		{name: "appended, both merged",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", "b", "c", post},
			kind: KindPreserving, globals: []string{"stages=free+c"}},
		{name: "two appended, both merged",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", "b", "c", "d", post},
			kind: KindPreserving, globals: []string{"stages=free+c,d"}},
		{name: "appended, base hand-written and head merged",
			base: []string{"a", "b"}, head: []string{pre, "a", "b", "c", post},
			kind: KindPreserving, globals: []string{"stages=free+c"}},
		{name: "appended, base merged and head hand-written",
			base: []string{pre, "a", "b", post}, head: []string{"a", "b", "c"},
			kind: KindPreserving, globals: []string{"stages=free+c"}},
		{name: "appended, the head names only .post, and not last",
			base: []string{"a", "b"}, head: []string{"a", "b", post, "c"},
			kind: KindPreserving, globals: []string{"stages=free+c"}},
		{name: "appended, the base names only .pre",
			base: []string{pre, "a", "b"}, head: []string{pre, "a", "b", "c", post},
			kind: KindPreserving, globals: []string{"stages=free+c"}},
		{name: "inserted between two of the base's",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", "c", "b", post},
			kind: KindViolation, vio: []string{"D2 /stages"}, globals: []string{"stages=gating"}},
		{name: "inserted first, right after .pre",
			base: []string{pre, "a", "b", post}, head: []string{pre, "c", "a", "b", post},
			kind: KindViolation, vio: []string{"D2 /stages"}, globals: []string{"stages=gating"}},
		{name: "inserted first, base hand-written",
			base: []string{"a", "b"}, head: []string{pre, "c", "a", "b", post},
			kind: KindViolation, vio: []string{"D2 /stages"}, globals: []string{"stages=gating"}},
		{name: "reordered",
			base: []string{pre, "a", "b", post}, head: []string{pre, "b", "a", post},
			kind: KindViolation, vio: []string{"D2 /stages"}, globals: []string{"stages=gating"}},
		{name: "a stage removed from the end",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", post}, headJobs: []string{"tally", "a", "pack", "a"},
			kind: KindViolation, vio: []string{"D1 pack/stage", "D2 /stages"}, globals: []string{"stages=gating"}},
		{name: "a job moved INTO .pre is a stage change like any other",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", "b", post}, headJobs: []string{"tally", pre, "pack", "b"},
			kind: KindViolation, vio: []string{"D1 tally/stage"}},
		{name: "a job moved INTO .post is a stage change like any other",
			base: []string{pre, "a", "b", post}, head: []string{pre, "a", "b", post}, headJobs: []string{"tally", "a", "pack", post},
			kind: KindViolation, vio: []string{"D1 pack/stage"}},
		{name: "a job moved into .post of a hand-written document that does not name it",
			base: []string{"a", "b"}, head: []string{"a", "b"}, headJobs: []string{"tally", "a", "pack", post},
			kind: KindViolation, vio: []string{"D1 pack/stage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hj := tc.headJobs
			if hj == nil {
				hj = jobs
			}
			d := Diff(stagesDoc(tc.base, jobs...), stagesDoc(tc.head, hj...), []string{"tally"})
			if d.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", d.Kind, tc.kind)
			}
			wantStrings(t, "violations", vio(d), tc.vio)
			wantStrings(t, "unresolvable", unres(d), nil)
			wantStrings(t, "globals", globals(d), tc.globals)
		})
	}
	// No `stages:` at all is GitLab's default list, which has neither: a
	// merged document of the same project carries it with both.
	t.Run("the default stages, merged on one side", func(t *testing.T) {
		merged := append([]string{pre}, append(append([]string{}, DefaultStages...), post)...)
		d := Diff(stagesDoc(nil, "tally", "test"), stagesDoc(merged, "tally", "test"), []string{"tally"})
		if d.Kind != KindNone {
			t.Errorf("Kind = %q, want none (violations %q)", d.Kind, vio(d))
		}
	})
}

// The plan names the implicit stages where it gives the stage rule, and the
// code's list is that one.
func TestImplicitStagesMatchThePlan(t *testing.T) {
	if !reflect.DeepEqual(ImplicitStages, []string{StagePre, StagePost}) || StagePre != ".pre" || StagePost != ".post" {
		t.Fatalf("ImplicitStages = %q", ImplicitStages)
	}
	sec := keyGroupSection(t)
	const sentence = "Implicit stages."
	i := strings.Index(sec, sentence)
	if i < 0 {
		t.Fatalf("the key-group section has no %q paragraph", sentence)
	}
	para := sec[i:]
	if j := strings.Index(para, "\n\n"); j >= 0 {
		para = para[:j]
	}
	got := []string{}
	for _, m := range backticked.FindAllStringSubmatch(para, -1) {
		if strings.HasPrefix(m[1], ".") && !contains(got, m[1]) {
			got = append(got, m[1])
		}
	}
	if !reflect.DeepEqual(got, ImplicitStages) {
		t.Errorf("the rules page's implicit-stages paragraph names %q, the code %q", got, ImplicitStages)
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// Every fixture file is used by a test, and every fixture is ASCII.
func TestFixturesAreAllUsedAndASCII(t *testing.T) {
	src, err := os.ReadFile("cidef_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = filepath.WalkDir("testdata", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for i, c := range b {
			if c > 0x7e || (c < 0x20 && c != '\n') {
				t.Errorf("%s: byte %d is 0x%02x, fixtures are printable ASCII", p, i, c)
				break
			}
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, "testdata"+string(filepath.Separator)))
		dir, file := filepath.ToSlash(filepath.Dir(rel)), strings.TrimSuffix(filepath.Base(rel), ".yml")
		used := false
		switch {
		case dir == "keys" && file == "base":
			used = true
		case dir == "keys":
			// gating_<key>, exec_<key>_<who> and free_<key> are reached by
			// iterating the key lists; the rest are named.
			for _, k := range GatingJobKeys {
				used = used || file == "gating_"+k
			}
			for _, k := range ExecJobKeys {
				used = used || file == "exec_"+k+"_other" || file == "exec_"+k+"_diagnosed"
			}
			for _, k := range FreeJobKeys {
				used = used || file == "free_"+k
			}
			used = used || strings.Contains(string(src), `"`+file+`"`)
		case dir == "gitlab_merged":
			used = strings.Contains(string(src), `"`+file+`"`)
		case dir == "broken":
			used = strings.Contains(string(src), `"`+file+`"`) || strings.Contains(string(src), "broken/"+file+".yml")
		default:
			used = (file == "base" || file == "head") && strings.Contains(string(src), `"`+dir+`"`)
		}
		if !used {
			t.Errorf("fixture %s is used by no test", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 80 {
		t.Errorf("found %d fixtures under testdata, expected the full set (a missing directory passes nothing)", n)
	}
}
