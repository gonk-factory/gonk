package packtest

// The `pipefix` agent (gonk-2usw plan 5.2, part AGENT). The agent-wide rules
// in pack_test.go range over agents/ and cover it the moment the directory
// exists. What is pinned HERE is what is specific to it:
//
//   - its harness, env and permission map are the `pipeline` agent's, and the
//     OPENCODE_PERMISSION line is compared as RAW FILE BYTES, not as decoded
//     TOML: the plan's claim is "byte-identical" (5.1: "nothing about the
//     pod's threat model changes from the diagnosis session that is already
//     live"), and a decoded comparison would pass a copy that differs in a
//     way TOML normalises away. A fix session PROPOSES edits as effects; it
//     has no tool that could make one.
//   - its effect shape is the fix vocabulary and nothing else, with the
//     verdict/kind rule holding in both directions;
//   - it is the ONE agent with a pack [scope] row, and the row is plan 4.3's
//     ceilings.

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/scope"
	"github.com/gonk-factory/gonk/pkg/trace"
)

const pipefixAgentName = "pipefix"

// permissionLine returns the raw bytes of the one OPENCODE_PERMISSION line of
// an agent.toml, line terminator excluded.
func permissionLine(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var found [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("OPENCODE_PERMISSION")) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s has %d OPENCODE_PERMISSION lines, want exactly 1", path, len(found))
	}
	return found[0]
}

func TestPipefixPermissionMapIsPipelinesByteForByte(t *testing.T) {
	agents := filepath.Join(packRoot(t), "agents")
	pipelinePath := filepath.Join(agents, "pipeline", "agent.toml")
	pipefixPath := filepath.Join(agents, pipefixAgentName, "agent.toml")

	want, got := permissionLine(t, pipelinePath), permissionLine(t, pipefixPath)
	if !bytes.Equal(got, want) {
		at := 0
		for at < len(got) && at < len(want) && got[at] == want[at] {
			at++
		}
		t.Errorf("agents/pipefix/agent.toml OPENCODE_PERMISSION differs from the pipeline agent's at byte %d "+
			"(lengths %d and %d):\n pipefix:  %s\n pipeline: %s\n"+
			"plan 5.2: the fix session is READ-ONLY, on the diagnosis session's exact permission map; "+
			"it proposes edits as effects and the controller applies them",
			at, len(got), len(want), got, want)
	}

	pipeline := decodeTOMLFile(t, pipelinePath)
	pipefix := decodeTOMLFile(t, pipefixPath)
	for _, k := range harnessKeys {
		if !reflect.DeepEqual(pipefix[k], pipeline[k]) {
			t.Errorf("agents/pipefix/agent.toml %s = %#v, pipeline has %#v", k, pipefix[k], pipeline[k])
		}
	}
	if d, _ := pipefix["description"].(string); d == "" {
		t.Error("agents/pipefix/agent.toml has no description")
	}
	pEnv, _ := pipeline["env"].(map[string]any)
	fEnv, _ := pipefix["env"].(map[string]any)
	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if !reflect.DeepEqual(keys(fEnv), keys(pEnv)) {
		t.Errorf("agents/pipefix/agent.toml [env] keys %v, pipeline's %v -- must be identical", keys(fEnv), keys(pEnv))
	}
	for k, pv := range pEnv {
		if fv, ok := fEnv[k]; ok && fv != pv {
			t.Errorf("agents/pipefix/agent.toml [env] %s differs from pipeline's:\n pipefix:  %v\n pipeline: %v", k, fv, pv)
		}
	}
}

// The effective permission, evaluated the way opencode does (last match
// wins): the fix session can read the checkout, and has no shell and no edit
// tool. opencode gates its write, patch and multiedit tools on the `edit`
// permission, so `edit` denied is every file-writing tool denied.
func TestPipefixAgentCanReadAndCannotWriteOrRun(t *testing.T) {
	cfg := decodeTOMLFile(t, filepath.Join(packRoot(t), "agents", pipefixAgentName, "agent.toml"))
	env, _ := cfg["env"].(map[string]any)
	raw, _ := env["OPENCODE_PERMISSION"].(string)
	entries, err := orderedJSONObject(raw)
	if err != nil {
		t.Fatalf("OPENCODE_PERMISSION: %v", err)
	}
	rules := flattenPermissionConfig(entries)
	for _, p := range []string{"read", "glob", "grep"} {
		if got := evaluatePermissionRules(rules, p, "*"); got != "allow" {
			t.Errorf("pipefix agent: %s resolves to %q, want allow -- the session must be able to read the checkout", p, got)
		}
	}
	for _, p := range deniedPermissions {
		if got := evaluatePermissionRules(rules, p, "*"); got != "deny" {
			t.Errorf("pipefix agent: %s resolves to %q, want deny -- a fix session proposes edits as effects; "+
				"it runs nothing and writes nothing (plan 5.1)", p, got)
		}
	}
	if got := evaluatePermissionRules(rules, "external_directory", "/tmp/gonk/*"); got != "ask" {
		t.Errorf("pipefix agent: external_directory for /tmp/gonk/* resolves to %q, want ask", got)
	}
}

func TestPipefixEffectShapeIsTheFixVocabularyAndNothingElse(t *testing.T) {
	root := packRoot(t)
	shape, err := effects.LoadShape(root, pipefixAgentName)
	if err != nil {
		t.Fatalf("LoadShape(pipefix): %v", err)
	}
	want := map[effects.Kind]effects.Card{
		effects.KindEdit:         {Min: 0, Max: effects.MaxFixEdits},
		effects.KindFixMeta:      {Min: 0, Max: 1},
		effects.KindScopeRequest: {Min: 0, Max: 1},
	}
	if !reflect.DeepEqual(shape.Kinds, want) {
		t.Errorf("pipefix effect shape = %v, want exactly %v (every other kind forbidden)", shape.Kinds, want)
	}

	edit := effects.Effect{Kind: effects.KindEdit, Path: "a/b.txt", Old: "x", New: "y"}
	meta := effects.Effect{Kind: effects.KindFixMeta, Reason: "r"}
	request := effects.Effect{Kind: effects.KindScopeRequest, Paths: []string{"a/c.txt"}, Reason: "r"}
	many := make([]effects.Effect, 0, effects.MaxFixEdits+2)
	for i := 0; i <= effects.MaxFixEdits; i++ {
		many = append(many, edit)
	}
	many = append(many, meta)

	for name, b := range map[string]effects.Batch{
		"a fix":       {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta}},
		"needs-scope": {Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{request}},
	} {
		if err := effects.Validate(b, shape); err != nil {
			t.Errorf("pipefix shape refused %s: %v", name, err)
		}
	}
	// The shape alone accepts an EMPTY batch -- every minimum is 0, because
	// which kinds are required depends on the verdict. What refuses it is
	// effects.ValidateFix (gate A1), which every fix apply must run: pinned
	// here so nobody concludes the shape is the whole gate.
	if err := effects.Validate(effects.Batch{}, shape); err != nil {
		t.Errorf("the shape alone now refuses an empty batch (%v); update the comment in effect-shape.toml", err)
	}
	refused := func(b effects.Batch) bool {
		return effects.Validate(b, shape) != nil || effects.ValidateFix(b) != nil
	}
	for name, b := range map[string]effects.Batch{
		"empty":                       {},
		"fix without an edit":         {Verdict: effects.VerdictFix, Effects: []effects.Effect{meta}},
		"fix without a fix_meta":      {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit}},
		"fix with two fix_meta":       {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, meta}},
		"fix with a scope_request":    {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, request}},
		"one edit too many":           {Verdict: effects.VerdictFix, Effects: many},
		"needs-scope with an edit":    {Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{request, edit}},
		"needs-scope with two":        {Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{request, request}},
		"an edit under no verdict":    {Effects: []effects.Effect{edit, meta}},
		"an edit under reply-only":    {Verdict: effects.VerdictReplyOnly, Effects: []effects.Effect{edit, meta}},
		"plus a comment":              {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, {Kind: effects.KindComment, Body: "x"}}},
		"plus a label":                {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, {Kind: effects.KindLabel, Add: []string{"x"}}}},
		"plus a file":                 {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, {Kind: effects.KindFile, Path: ".agent/x", Content: "x"}}},
		"plus a diagnosis":            {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, {Kind: effects.KindDiagnosis}}},
		"plus a new issue":            {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta, {Kind: effects.KindNewIssue, Title: "x"}}},
		"a comment instead of a fix":  {Effects: []effects.Effect{{Kind: effects.KindComment, Body: "x"}}},
		"needs-scope without request": {Verdict: effects.VerdictNeedsScope},
	} {
		if !refused(b) {
			t.Errorf("the pipefix shape and effects.ValidateFix together accepted a batch: %s", name)
		}
	}
	for name, b := range map[string]effects.Batch{
		"a fix":       {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta}},
		"needs-scope": {Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{request}},
	} {
		if refused(b) {
			t.Errorf("the pipefix shape and effects.ValidateFix together refused %s", name)
		}
	}

	// The trajectory floor (plan 4.4): a session that refuses the task
	// (needs-scope) must have opened at least one file first.
	pol, err := trace.LoadPolicy(root, pipefixAgentName)
	if err != nil {
		t.Fatalf("LoadPolicy(pipefix): %v", err)
	}
	if !reflect.DeepEqual(pol.ReadTools, []string{"read", "Read"}) {
		t.Errorf("pipefix trajectory read_tools = %v, want [read Read]", pol.ReadTools)
	}
	if want := []string{string(effects.VerdictNeedsScope)}; !reflect.DeepEqual(pol.RequireAnyReadFor, want) {
		t.Errorf("pipefix trajectory require_any_read_for = %v, want %v (plan 4.4: refused without looking)", pol.RequireAnyReadFor, want)
	}
	if len(pol.ForbiddenTools)+len(pol.RequiredTools)+len(pol.MinCalls)+len(pol.RequireTargetReadFor) != 0 {
		t.Errorf("pipefix trajectory carries predicates the plan does not name: %+v", pol)
	}
}

// The pack [scope] row (066 3.1; plan 4.2, 4.3): pipefix is the one agent
// with one, so it is the one agent whose edits can pass effects.PlanFix at
// all (the zero Row allows no class). Its numbers are the plan's ceilings;
// operator and project config may only tighten them.
func TestPipefixIsTheOnlyAgentWithAScopeRow(t *testing.T) {
	root := packRoot(t)
	row, err := scope.LoadRow(root, pipefixAgentName)
	if err != nil {
		t.Fatalf("LoadRow(pipefix): %v", err)
	}
	classes := append([]scope.Class(nil), row.Classes...)
	sort.Slice(classes, func(a, b int) bool { return classes[a] < classes[b] })
	if want := []scope.Class{scope.ClassDocs, scope.ClassSource, scope.ClassTest}; !reflect.DeepEqual(classes, want) {
		t.Errorf("pipefix [scope] classes = %v, want %v (plan 4.2: the code and test rows)", classes, want)
	}
	if want := []scope.Class{scope.ClassCI}; !reflect.DeepEqual(row.Liftable, want) {
		t.Errorf("pipefix [scope] liftable = %v, want %v (plan 4.2: the configuration row, per dispatch, opt-in)", row.Liftable, want)
	}
	if want := (scope.Caps{MaxFiles: 3, MaxLines: 40, MaxDeletes: 0, MaxFileBytes: 256 << 10, MaxPatchBytes: 16 << 10}); row.Caps != want {
		t.Errorf("pipefix [scope] caps = %+v, want plan 4.3's %+v", row.Caps, want)
	}

	entries, err := os.ReadDir(filepath.Join(root, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	others := 0
	for _, ent := range entries {
		if !ent.IsDir() || ent.Name() == pipefixAgentName {
			continue
		}
		others++
		r, err := scope.LoadRow(root, ent.Name())
		if err != nil {
			t.Fatalf("LoadRow(%s): %v", ent.Name(), err)
		}
		if !reflect.DeepEqual(r, scope.Row{}) {
			t.Errorf("agent %s has a [scope] row %+v; pipefix is the only agent that may propose repository edits", ent.Name(), r)
		}
	}
	if others == 0 {
		t.Fatal("no other agent was checked")
	}
}
