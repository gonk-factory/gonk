package effects

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The pipeline-fix vocabulary (gonk-2usw plan 4.1): `edit`, `fix_meta`,
// `scope_request`, verdicts `fix` and `needs-scope`.
//
// Every fixture in this file and in edit_test.go / fixgate_test.go is INVENTED
// (plan section 0): no path, snippet or failure here is taken from, or shaped
// after, any real project.

func editEffect(path, old, new string) Effect {
	return Effect{Kind: KindEdit, Path: path, Old: old, New: new}
}

func metaEffect() Effect {
	return Effect{Kind: KindFixMeta, Reason: "Sum added its operands in the wrong order of magnitude; the constant is corrected."}
}

func requestEffect() Effect {
	return Effect{Kind: KindScopeRequest, Paths: []string{"go.mod"}, Reason: "The fix needs a dependency bump, which is outside what this session may edit."}
}

// goodFix passes every fix gate against goodEnvelope/goodBase (fixgate_test.go).
func goodFix() Batch {
	return Batch{Verdict: VerdictFix, Effects: []Effect{
		editEffect("pkg/example/sum.go", "\treturn a - b\n", "\treturn a + b\n"),
		metaEffect(),
	}}
}

func goodNeedsScope() Batch {
	return Batch{Verdict: VerdictNeedsScope, Effects: []Effect{requestEffect()}}
}

// The wire format, as the reference doc states it. These two documents are the
// examples in docs/reference/effects-contract.md.
func TestFixWireFormat(t *testing.T) {
	fix := `{"verdict":"fix","effects":[
	  {"kind":"edit","path":"pkg/example/sum.go","old":"\treturn a - b\n","new":"\treturn a + b\n"},
	  {"kind":"fix_meta","reason":"Sum subtracted instead of adding."}]}`
	b, err := ParseBatch([]byte(fix))
	if err != nil {
		t.Fatalf("ParseBatch(fix): %v", err)
	}
	want := Batch{Verdict: VerdictFix, Effects: []Effect{
		{Kind: KindEdit, Path: "pkg/example/sum.go", Old: "\treturn a - b\n", New: "\treturn a + b\n"},
		{Kind: KindFixMeta, Reason: "Sum subtracted instead of adding."},
	}}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("fix batch = %+v\nwant %+v", b, want)
	}
	if err := ValidateFix(b); err != nil {
		t.Fatalf("ValidateFix(fix): %v", err)
	}

	ns := `{"verdict":"needs-scope","effects":[
	  {"kind":"scope_request","paths":["go.mod"],"reason":"The fix needs a dependency bump."}]}`
	b, err = ParseBatch([]byte(ns))
	if err != nil {
		t.Fatalf("ParseBatch(needs-scope): %v", err)
	}
	want = Batch{Verdict: VerdictNeedsScope, Effects: []Effect{
		{Kind: KindScopeRequest, Paths: []string{"go.mod"}, Reason: "The fix needs a dependency bump."},
	}}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("needs-scope batch = %+v\nwant %+v", b, want)
	}
	if err := ValidateFix(b); err != nil {
		t.Fatalf("ValidateFix(needs-scope): %v", err)
	}

	// A deletion is an edit whose `new` is absent/empty: omitempty must not
	// make it unparseable.
	del := `{"verdict":"fix","effects":[{"kind":"edit","path":"a.go","old":"x\n"},{"kind":"fix_meta","reason":"r"}]}`
	b, err = ParseBatch([]byte(del))
	if err != nil || b.Effects[0].New != "" || b.Effects[0].Old != "x\n" {
		t.Fatalf("deletion edit: %+v, %v", b, err)
	}
}

func TestFixVerdictsAreInTheClosedSet(t *testing.T) {
	for _, v := range []Verdict{VerdictFix, VerdictNeedsScope} {
		raw := `{"verdict":"` + string(v) + `","effects":[]}`
		b, err := ParseBatch([]byte(raw))
		if err != nil || b.EffectiveVerdict() != v {
			t.Errorf("ParseBatch(verdict %q) = %+v, %v", v, b, err)
		}
	}
	for _, v := range []string{"needs_scope", "needsscope", "FIX", "fixed", "patch", "needs-scope "} {
		if _, err := ParseBatch([]byte(`{"verdict":"` + v + `","effects":[]}`)); err == nil {
			t.Errorf("ParseBatch accepted unknown verdict %q", v)
		}
	}
	for _, k := range []string{"patch", "commit", "merge_request", "branch", "delete", "create", "approve", "retry", "assignee", "reviewer"} {
		if _, err := ParseBatch([]byte(`{"effects":[{"kind":"` + k + `"}]}`)); err == nil {
			t.Errorf("ParseBatch accepted kind %q; plan 4.1 keeps it deliberately absent", k)
		}
	}
}

// The verdict/kind cross rule lives in Validate, the gate EVERY agent's batch
// goes through, so it cannot be skipped by an apply chain that forgot it.
func TestVerdictKindCrossRule(t *testing.T) {
	open := Shape{Kinds: map[Kind]Card{
		KindEdit: {0, 10}, KindFixMeta: {0, 1}, KindScopeRequest: {0, 1}, KindComment: {0, N},
	}}
	e, m, r := editEffect("a.go", "x", "y"), metaEffect(), requestEffect()
	c := Effect{Kind: KindComment, Body: "hello"}
	cases := []struct {
		name    string
		verdict Verdict
		effects []Effect
		sub     string // "" = accepted
	}{
		{"fix: one edit, one meta", VerdictFix, []Effect{e, m}, ""},
		{"fix: ten edits, one meta", VerdictFix, append(repeat(e, 10), m), ""},
		{"needs-scope: one request", VerdictNeedsScope, []Effect{r}, ""},
		{"reply-only with a comment is untouched", VerdictReplyOnly, []Effect{c}, ""},
		{"absent verdict with a comment is untouched", "", []Effect{c}, ""},

		{"fix without an edit", VerdictFix, []Effect{m}, "fix-needs-edit"},
		{"fix without a fix_meta", VerdictFix, []Effect{e}, "fix-needs-meta"},
		{"fix with a scope_request", VerdictFix, []Effect{e, m, r}, "fix-with-request"},
		{"fix with nothing", VerdictFix, nil, "fix-needs-edit"},
		{"fix with only a comment", VerdictFix, []Effect{c}, "fix-needs-edit"},
		{"needs-scope without a request", VerdictNeedsScope, nil, "needs-scope-needs-request"},
		{"needs-scope with only a comment", VerdictNeedsScope, []Effect{c}, "needs-scope-needs-request"},
		{"needs-scope with an edit", VerdictNeedsScope, []Effect{r, e}, "needs-scope-with-edit"},
		{"needs-scope with a fix_meta", VerdictNeedsScope, []Effect{r, m}, "needs-scope-with-meta"},
		{"edit under reply-only", VerdictReplyOnly, []Effect{e, c}, "edit-needs-fix"},
		{"edit under no verdict", "", []Effect{e}, "edit-needs-fix"},
		{"edit under code-change", VerdictCodeChange, []Effect{e, m}, "edit-needs-fix"},
		{"fix_meta under close", VerdictClose, []Effect{m}, "meta-needs-fix"},
		{"scope_request under reply-only", VerdictReplyOnly, []Effect{r}, "request-needs-needs-scope"},
		{"scope_request under no verdict", "", []Effect{r}, "request-needs-needs-scope"},
	}
	for _, tc := range cases {
		err := Validate(Batch{Verdict: tc.verdict, Effects: tc.effects}, open)
		if tc.sub == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v, want accepted", tc.name, err)
			}
			continue
		}
		wantRefusal(t, tc.name, err, RuleFixShape, tc.sub)
	}
}

func repeat(e Effect, n int) []Effect {
	out := make([]Effect, n)
	for i := range out {
		out[i] = e
	}
	return out
}

// wantRefusal asserts err is a *Refusal with exactly this rule code and sub
// code. Never by prose.
func wantRefusal(t *testing.T, name string, err error, rule Rule, sub string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted, want %s(%s)", name, rule, sub)
		return
	}
	if got := RuleOf(err); got != rule {
		t.Errorf("%s: rule = %q (%v), want %s(%s)", name, got, err, rule, sub)
		return
	}
	if got := SubOf(err); got != sub {
		t.Errorf("%s: sub = %q (%v), want %s(%s)", name, got, err, rule, sub)
	}
}

// NO EXISTING AGENT MAY GAIN THE FIX VOCABULARY. This reads the REAL pack: for
// every agent directory that is not the fix agent itself, its shape lists none
// of the three kinds, and Validate -- the gate its apply chain calls -- refuses
// a batch that carries one, or that states a fix verdict.
var fixAgents = map[string]bool{"pipefix": true} // part AGENT's; the only agent allowed these kinds

func TestExistingAgentShapesRefuseTheFixVocabulary(t *testing.T) {
	packDir := filepath.Join("..", "..", "pack")
	entries, err := os.ReadDir(filepath.Join(packDir, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	checked := map[string]bool{}
	for _, ent := range entries {
		agent := ent.Name()
		if !ent.IsDir() || fixAgents[agent] {
			continue
		}
		if _, err := os.Stat(filepath.Join(packDir, "agents", agent, "effect-shape.toml")); err != nil {
			continue // an agent with no shape file has no batch gate to widen
		}
		shape, err := LoadShape(packDir, agent)
		if err != nil {
			t.Fatalf("LoadShape(%s): %v", agent, err)
		}
		checked[agent] = true
		for _, k := range []Kind{KindEdit, KindFixMeta, KindScopeRequest} {
			if _, ok := shape.Kinds[k]; ok {
				t.Errorf("agent %s: effect-shape.toml lists %q; only the fix agent may", agent, k)
			}
		}

		// A batch this agent's shape ACCEPTS, built from the shape itself, so the
		// refusals below are caused by the fix vocabulary and nothing else.
		var base []Effect
		kinds := make([]string, 0, len(shape.Kinds))
		for k := range shape.Kinds {
			kinds = append(kinds, string(k))
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			base = append(base, repeat(Effect{Kind: Kind(k)}, shape.Kinds[Kind(k)].Min)...)
		}
		if err := Validate(Batch{Effects: base}, shape); err != nil {
			t.Fatalf("agent %s: the control batch is refused (%v); the checks below would be vacuous", agent, err)
		}
		with := func(extra ...Effect) []Effect { return append(append([]Effect(nil), base...), extra...) }
		refused := []struct {
			name string
			b    Batch
		}{
			{"an edit rides along", Batch{Effects: with(editEffect("src/a.go", "x", "y"))}},
			{"a fix_meta rides along", Batch{Effects: with(metaEffect())}},
			{"a scope_request rides along", Batch{Effects: with(requestEffect())}},
			{"a complete fix rides along", Batch{Verdict: VerdictFix, Effects: with(goodFix().Effects...)}},
			{"a complete needs-scope rides along", Batch{Verdict: VerdictNeedsScope, Effects: with(requestEffect())}},
			{"a complete fix alone", goodFix()},
			{"a complete needs-scope alone", goodNeedsScope()},
			{"verdict fix on its own batch", Batch{Verdict: VerdictFix, Effects: base}},
			{"verdict needs-scope on its own batch", Batch{Verdict: VerdictNeedsScope, Effects: base}},
		}
		for _, r := range refused {
			if err := Validate(r.b, shape); err == nil {
				t.Errorf("agent %s: %s: Validate accepted it", agent, r.name)
			}
		}
	}
	for _, must := range []string{"triage", "scaffold", "pipeline"} {
		if !checked[must] {
			t.Errorf("agent %q was not checked (checked: %v)", must, checked)
		}
	}
}

// One fixture per ValidateFix rule: each differs from a good batch in one
// thing and is refused by exactly that rule and sub code.
func TestValidateFixEachRule(t *testing.T) {
	if err := ValidateFix(goodFix()); err != nil {
		t.Fatalf("good fix refused: %v", err)
	}
	if err := ValidateFix(goodNeedsScope()); err != nil {
		t.Fatalf("good needs-scope refused: %v", err)
	}
	fix := func(f func(b *Batch)) Batch { b := goodFix(); f(&b); return b }
	ns := func(f func(b *Batch)) Batch { b := goodNeedsScope(); f(&b); return b }
	cases := []struct {
		name string
		b    Batch
		rule Rule
		sub  string
	}{
		// verdict and kinds
		{"verdict absent", Batch{}, RuleFixShape, "verdict"},
		{"verdict reply-only", Batch{Verdict: VerdictReplyOnly}, RuleFixShape, "verdict"},
		{"verdict code-change", Batch{Verdict: VerdictCodeChange}, RuleFixShape, "verdict"},
		{"a comment in a fix batch", fix(func(b *Batch) { b.Effects = append(b.Effects, Effect{Kind: KindComment, Body: "x"}) }), RuleFixShape, "kind"},
		{"a file in a fix batch", fix(func(b *Batch) { b.Effects = append(b.Effects, Effect{Kind: KindFile, Path: ".agent/a", Content: "x"}) }), RuleFixShape, "kind"},
		{"a label in a needs-scope batch", ns(func(b *Batch) { b.Effects = append(b.Effects, Effect{Kind: KindLabel, Add: []string{"x"}}) }), RuleFixShape, "kind"},
		{"cross rule: no fix_meta", fix(func(b *Batch) { b.Effects = b.Effects[:1] }), RuleFixShape, "fix-needs-meta"},
		{"cross rule: two fix_meta", fix(func(b *Batch) { b.Effects = append(b.Effects, metaEffect()) }), RuleFixShape, "fix-needs-meta"},
		{"cross rule: two requests", ns(func(b *Batch) { b.Effects = append(b.Effects, requestEffect()) }), RuleFixShape, "needs-scope-needs-request"},
		{"eleven edits", fix(func(b *Batch) {
			b.Effects = append(repeat(editEffect("pkg/example/sum.go", "a", "b"), MaxFixEdits+1), metaEffect())
		}), RuleFixShape, "edit-count"},

		// edit form
		{"edit path empty", fix(func(b *Batch) { b.Effects[0].Path = "" }), RuleForm, "path"},
		{"edit path absolute", fix(func(b *Batch) { b.Effects[0].Path = "/etc/passwd" }), RuleForm, "path"},
		{"edit path dotdot", fix(func(b *Batch) { b.Effects[0].Path = "../x.go" }), RuleForm, "path"},
		{"edit path not canonical", fix(func(b *Batch) { b.Effects[0].Path = "pkg//example/sum.go" }), RuleForm, "path"},
		{"edit path dot segment", fix(func(b *Batch) { b.Effects[0].Path = "./pkg/example/sum.go" }), RuleForm, "path"},
		{"edit path trailing slash", fix(func(b *Batch) { b.Effects[0].Path = "pkg/example/" }), RuleForm, "path"},
		{"edit path NUL", fix(func(b *Batch) { b.Effects[0].Path = "pkg/a\x00.go" }), RuleForm, "path"},
		{"edit old empty", fix(func(b *Batch) { b.Effects[0].Old = "" }), RuleForm, "old-empty"},
		{"edit old equals new", fix(func(b *Batch) { b.Effects[0].New = b.Effects[0].Old }), RuleForm, "identical"},
		{"edit NUL in old", fix(func(b *Batch) { b.Effects[0].Old = "a\x00b" }), RuleForm, "nul"},
		{"edit NUL in new", fix(func(b *Batch) { b.Effects[0].New = "a\x00b" }), RuleForm, "nul"},
		{"edit invalid UTF-8 in old", fix(func(b *Batch) { b.Effects[0].Old = "a\xffb" }), RuleForm, "utf8"},
		{"edit invalid UTF-8 in new", fix(func(b *Batch) { b.Effects[0].New = "a\xc3" }), RuleForm, "utf8"},
		{"edit carries content", fix(func(b *Batch) { b.Effects[0].Content = "whole file" }), RuleFixShape, "foreign-field"},
		{"edit carries body", fix(func(b *Batch) { b.Effects[0].Body = "x" }), RuleFixShape, "foreign-field"},
		{"edit carries files", fix(func(b *Batch) { b.Effects[0].Files = []string{"a.go"} }), RuleFixShape, "foreign-field"},
		{"edit carries paths", fix(func(b *Batch) { b.Effects[0].Paths = []string{"a.go"} }), RuleFixShape, "foreign-field"},
		{"edit carries rationale", fix(func(b *Batch) { b.Effects[0].Reason = "x" }), RuleFixShape, "foreign-field"},

		// fix_meta
		{"rationale blank", fix(func(b *Batch) { b.Effects[1].Reason = " \n\t" }), RuleFixShape, "reason-empty"},
		{"rationale over cap", fix(func(b *Batch) { b.Effects[1].Reason = strings.Repeat("\u00e9", MaxFixReasonChars+1) }), RuleFixShape, "reason-cap"},
		{"fix_meta carries a path", fix(func(b *Batch) { b.Effects[1].Path = "src/a.go" }), RuleFixShape, "foreign-field"},
		{"fix_meta carries old", fix(func(b *Batch) { b.Effects[1].Old = "x" }), RuleFixShape, "foreign-field"},
		{"fix_meta carries a summary", fix(func(b *Batch) { b.Effects[1].Summary = "x" }), RuleFixShape, "foreign-field"},

		// scope_request
		{"request too many paths", ns(func(b *Batch) { b.Effects[0].Paths = []string{"a", "b", "c", "d", "e", "f"} }), RuleFixShape, "paths-count"},
		{"request path absolute", ns(func(b *Batch) { b.Effects[0].Paths = []string{"/etc/passwd"} }), RuleFixShape, "paths-form"},
		{"request path dotdot", ns(func(b *Batch) { b.Effects[0].Paths = []string{"a/../../b"} }), RuleFixShape, "paths-form"},
		{"request path empty", ns(func(b *Batch) { b.Effects[0].Paths = []string{""} }), RuleFixShape, "paths-form"},
		{"request reason blank", ns(func(b *Batch) { b.Effects[0].Reason = "  " }), RuleFixShape, "reason-empty"},
		{"request reason over cap", ns(func(b *Batch) { b.Effects[0].Reason = strings.Repeat("x", MaxScopeRequestReasonChars+1) }), RuleFixShape, "reason-cap"},
		{"request carries a write path", ns(func(b *Batch) { b.Effects[0].Path = "go.mod" }), RuleFixShape, "foreign-field"},
		{"request carries new", ns(func(b *Batch) { b.Effects[0].New = "x" }), RuleFixShape, "foreign-field"},
		{"request carries content", ns(func(b *Batch) { b.Effects[0].Content = "x" }), RuleFixShape, "foreign-field"},
	}
	for _, tc := range cases {
		wantRefusal(t, tc.name, ValidateFix(tc.b), tc.rule, tc.sub)
	}

	// The caps are AT the boundary, not below it.
	atCap := fix(func(b *Batch) {
		b.Effects = append(repeat(editEffect("pkg/example/sum.go", "a", "b"), MaxFixEdits), metaEffect())
		b.Effects[len(b.Effects)-1].Reason = strings.Repeat("\u00e9", MaxFixReasonChars)
	})
	if err := ValidateFix(atCap); err != nil {
		t.Errorf("a batch exactly at the edit and rationale caps is refused: %v", err)
	}
	five := ns(func(b *Batch) {
		b.Effects[0].Paths = []string{"a", "b", "c", "d", "e"}
		b.Effects[0].Reason = strings.Repeat("x", MaxScopeRequestReasonChars)
	})
	if err := ValidateFix(five); err != nil {
		t.Errorf("a request exactly at the path and reason caps is refused: %v", err)
	}
}

// The foreign-field rule is by REFLECTION: a field added to Effect later is
// foreign to all three kinds until someone lists it.
func TestFixKindsRefuseEveryForeignField(t *testing.T) {
	good := map[Kind]Effect{KindEdit: goodFix().Effects[0], KindFixMeta: metaEffect(), KindScopeRequest: requestEffect()}
	rt := reflect.TypeOf(Effect{})
	checked := 0
	for kind, base := range good {
		for i := 0; i < rt.NumField(); i++ {
			name := rt.Field(i).Name
			if name == "Kind" || fixKindFields[kind][name] || targetFields[name] {
				continue
			}
			e := base
			fv := reflect.ValueOf(&e).Elem().Field(i)
			switch fv.Kind() {
			case reflect.String:
				fv.SetString("x")
			case reflect.Slice:
				fv.Set(reflect.MakeSlice(fv.Type(), 1, 1))
			default:
				t.Fatalf("Effect.%s has kind %s; extend this test", name, fv.Kind())
			}
			if f := foreignField(e); f != name {
				t.Errorf("%s carrying %s: foreignField = %q, want %q", kind, name, f, name)
			}
			checked++
		}
	}
	if checked < 20 {
		t.Fatalf("checked %d (kind, field) pairs; the guard is vacuous", checked)
	}
}

// A fix effect names no target: the controller chooses branch, MR and where
// notes land (plan 5.3 A2, 5.4).
func TestFixEffectsMayNotNameATarget(t *testing.T) {
	all := Targets{Issues: map[int64]bool{7: true}, MergeRequests: map[int64]bool{7: true}, Pipelines: map[int64]bool{7: true}}
	for _, e := range []Effect{goodFix().Effects[0], metaEffect(), requestEffect()} {
		for _, tk := range []TargetKind{"", TargetIssue, TargetMergeRequest, TargetPipeline} {
			e.TargetIID, e.TargetType = 7, tk
			if err := ValidateTargets(Batch{Effects: []Effect{e}}, all); err == nil {
				t.Errorf("kind %q naming %s 7 passed ValidateTargets", e.Kind, tk)
			}
		}
	}
}

// A scope_request's paths are citations: it must be able to NAME a protected
// path ("this needs a CI change"), and naming one writes nothing.
func TestScopeRequestMayCiteProtectedPaths(t *testing.T) {
	b := goodNeedsScope()
	b.Effects[0].Paths = []string{".gitlab-ci.yml", ".gonk.yml", "CODEOWNERS", "go.mod"}
	if err := ValidateFix(b); err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if err := ValidateProtectedPaths(b); err != nil {
		t.Fatalf("ValidateProtectedPaths: %v", err)
	}
	plan, err := PlanFix(b, goodEnvelope(t), nil, nil)
	if err != nil {
		t.Fatalf("PlanFix: %v", err)
	}
	if len(plan.Files) != 0 || plan.Request == nil || !reflect.DeepEqual(plan.Request.Paths, b.Effects[0].Paths) {
		t.Fatalf("plan = %+v, want no files and the request", plan)
	}
}
