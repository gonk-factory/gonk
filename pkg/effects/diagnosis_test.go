package effects

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Synthetic, neutral context: two failed jobs, one with a log excerpt. No
// fixture here describes any real project's failure (gonk-tios section 0).
func diagCtx() DiagnosisContext {
	return DiagnosisContext{FailedJobs: []FailedJob{
		{Name: "unit-tests", HasExcerpt: true},
		{Name: "lint", HasExcerpt: false},
	}}
}

// goodDiagnosis passes every ValidateDiagnosis rule; each rule fixture below
// changes exactly one thing about it.
func goodDiagnosis() Effect {
	return Effect{
		Kind:           KindDiagnosis,
		Classification: ClassCode,
		Summary:        "The example widget test fails because Sum returns the wrong total.",
		Evidence: []EvidenceItem{
			{Job: "unit-tests", Line: "--- FAIL: TestSum (0.00s) got 3 want 4"},
			{Job: "unit-tests", Fact: FactFailureReason},
		},
		Remedy: "Correct the addition in Sum in pkg/example/sum.go.",
		Files:  []string{"pkg/example/sum.go"},
	}
}

func one(e Effect) Batch { return Batch{Effects: []Effect{e}} }

func TestValidateDiagnosisAcceptsGood(t *testing.T) {
	if err := ValidateDiagnosis(one(goodDiagnosis()), diagCtx()); err != nil {
		t.Fatalf("good diagnosis refused: %v", err)
	}
}

// One fixture per rule: each passes every OTHER rule and fails exactly this
// one, asserted by the rule's sentinel error.
func TestValidateDiagnosisEachRule(t *testing.T) {
	mut := func(f func(*Effect)) Effect { e := goodDiagnosis(); f(&e); return e }
	cases := []struct {
		name string
		e    Effect
		ctx  DiagnosisContext
		want error
	}{
		{"classification", mut(func(e *Effect) { e.Classification = "network" }), diagCtx(), ErrDiagnosisClassification},
		{"summary-empty", mut(func(e *Effect) { e.Summary = "  \n " }), diagCtx(), ErrDiagnosisSummaryEmpty},
		{"summary-cap", mut(func(e *Effect) { e.Summary = strings.Repeat("é", MaxDiagnosisSummaryChars+1) }), diagCtx(), ErrDiagnosisSummaryTooLong},
		{"remedy-empty", mut(func(e *Effect) { e.Remedy = "" }), diagCtx(), ErrDiagnosisRemedyEmpty},
		{"remedy-cap", mut(func(e *Effect) { e.Remedy = strings.Repeat("x", MaxDiagnosisRemedyChars+1) }), diagCtx(), ErrDiagnosisRemedyTooLong},
		{"evidence-none", mut(func(e *Effect) { e.Evidence = nil }), DiagnosisContext{FailedJobs: []FailedJob{{Name: "unit-tests"}}}, ErrDiagnosisEvidenceCount},
		{"evidence-too-many", mut(func(e *Effect) {
			for len(e.Evidence) <= MaxDiagnosisEvidence {
				e.Evidence = append(e.Evidence, EvidenceItem{Job: "lint", Fact: FactStage})
			}
		}), diagCtx(), ErrDiagnosisEvidenceCount},
		{"evidence-both", mut(func(e *Effect) { e.Evidence[1].Line = "both a line and a fact here" }), diagCtx(), ErrDiagnosisEvidenceShape},
		{"evidence-neither", mut(func(e *Effect) { e.Evidence[1].Fact = "" }), diagCtx(), ErrDiagnosisEvidenceShape},
		{"evidence-blank-line", mut(func(e *Effect) { e.Evidence[1] = EvidenceItem{Job: "lint", Line: "   "} }), diagCtx(), ErrDiagnosisEvidenceShape},
		{"evidence-blank-line-with-fact", mut(func(e *Effect) { e.Evidence[1].Line = " \t " }), diagCtx(), ErrDiagnosisEvidenceShape},
		{"evidence-fact", mut(func(e *Effect) { e.Evidence[1].Fact = "exit_code" }), diagCtx(), ErrDiagnosisEvidenceFact},
		{"evidence-job-unknown", mut(func(e *Effect) { e.Evidence[1].Job = "deploy" }), diagCtx(), ErrDiagnosisEvidenceJob},
		{"evidence-job-empty-line", mut(func(e *Effect) { e.Evidence[0].Job = "" }), diagCtx(), ErrDiagnosisEvidenceJob},
		{"evidence-job-empty-fact", mut(func(e *Effect) { e.Evidence[1].Job = "" }), diagCtx(), ErrDiagnosisEvidenceJob},
		{"line-required", mut(func(e *Effect) { e.Evidence = e.Evidence[1:] }), diagCtx(), ErrDiagnosisNoLineEvidence},
		{"file-count", mut(func(e *Effect) {
			e.Files = nil
			for i := 0; i <= MaxDiagnosisFiles; i++ {
				e.Files = append(e.Files, "src/f"+string(rune('a'+i))+".go")
			}
		}), diagCtx(), ErrDiagnosisFileCount},
		{"file-dotdot", mut(func(e *Effect) { e.Files = []string{"src/../../etc/passwd"} }), diagCtx(), ErrDiagnosisFilePath},
		{"file-dotdot-canonical", mut(func(e *Effect) { e.Files = []string{"../outside.txt"} }), diagCtx(), ErrDiagnosisFilePath},
		{"file-absolute", mut(func(e *Effect) { e.Files = []string{"/etc/passwd"} }), diagCtx(), ErrDiagnosisFilePath},
		{"file-nul", mut(func(e *Effect) { e.Files = []string{"src/a\x00.go"} }), diagCtx(), ErrDiagnosisFilePath},
		{"file-empty", mut(func(e *Effect) { e.Files = []string{""} }), diagCtx(), ErrDiagnosisFilePath},
		{"file-noncanonical", mut(func(e *Effect) { e.Files = []string{"src/./a.go"} }), diagCtx(), ErrDiagnosisFilePath},
		{"files-required-code", mut(func(e *Effect) { e.Files = nil }), diagCtx(), ErrDiagnosisFilesRequired},
		{"files-required-test", mut(func(e *Effect) { e.Classification = ClassTest; e.Files = nil }), diagCtx(), ErrDiagnosisFilesRequired},
		{"files-required-configuration", mut(func(e *Effect) { e.Classification = ClassConfiguration; e.Files = nil }), diagCtx(), ErrDiagnosisFilesRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDiagnosis(one(c.e), c.ctx)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want rule %v", err, c.want)
			}
		})
	}
}

// The boundaries are accepted: caps are inclusive, measured in characters.
func TestValidateDiagnosisBoundariesAccepted(t *testing.T) {
	e := goodDiagnosis()
	e.Summary = strings.Repeat("é", MaxDiagnosisSummaryChars) // 3000 bytes, 1500 chars
	e.Remedy = strings.Repeat("x", MaxDiagnosisRemedyChars)
	e.Evidence = e.Evidence[:1]
	for len(e.Evidence) < MaxDiagnosisEvidence {
		e.Evidence = append(e.Evidence, EvidenceItem{Job: "lint", Fact: FactStage})
	}
	e.Files = nil
	for i := 0; i < MaxDiagnosisFiles; i++ {
		e.Files = append(e.Files, "src/f"+string(rune('a'+i))+".go")
	}
	if err := ValidateDiagnosis(one(e), diagCtx()); err != nil {
		t.Fatalf("diagnosis at every cap refused: %v", err)
	}
}

// Classes that make no claim about repository content need no file; a
// pipeline-level fact needs no job; with no excerpt at all no line is needed.
func TestValidateDiagnosisOptionalParts(t *testing.T) {
	for _, c := range []Classification{ClassFlaky, ClassInfrastructure, ClassUnknown} {
		e := goodDiagnosis()
		e.Classification, e.Files = c, nil
		if err := ValidateDiagnosis(one(e), diagCtx()); err != nil {
			t.Errorf("class %q without files refused: %v", c, err)
		}
	}
	// A pipeline that failed with zero jobs: no failed jobs, no excerpts.
	e := goodDiagnosis()
	e.Classification = ClassConfiguration
	e.Files = []string{"ci/build.yml"}
	e.Evidence = []EvidenceItem{{Fact: FactPipelineError}}
	if err := ValidateDiagnosis(one(e), DiagnosisContext{}); err != nil {
		t.Errorf("pipeline-level diagnosis refused: %v", err)
	}
	// Every fact key in the closed set is accepted.
	for f := range knownFacts {
		e := goodDiagnosis()
		e.Evidence[1].Fact = f
		if err := ValidateDiagnosis(one(e), diagCtx()); err != nil {
			t.Errorf("fact %q refused: %v", f, err)
		}
	}
}

func TestValidateDiagnosisIgnoresOtherKinds(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindLabel, Add: []string{"a"}}}}
	if err := ValidateDiagnosis(b, DiagnosisContext{}); err != nil {
		t.Fatalf("non-diagnosis batch refused: %v", err)
	}
}

// The class set is closed and exactly the documented six, in order.
func TestClassificationsClosedSet(t *testing.T) {
	want := "code,test,flaky,infrastructure,configuration,unknown"
	var got []string
	for _, c := range Classifications() {
		got = append(got, string(c))
	}
	if strings.Join(got, ",") != want {
		t.Fatalf("Classifications() = %v, want %s", got, want)
	}
	if len(knownClassifications) != len(got) {
		t.Fatalf("knownClassifications has %d entries, Classifications() %d", len(knownClassifications), len(got))
	}
}

// The wire format: the D7a example round-trips through ParseBatch and passes
// ValidateDiagnosis and the target gate.
func TestDiagnosisWireFormat(t *testing.T) {
	raw := `{"effects":[{"kind":"diagnosis","classification":"test",` +
		`"summary":"An assertion in the example suite expects an outdated value.",` +
		`"evidence":[{"job":"unit-tests","line":"--- FAIL: TestSum (0.00s) got 3 want 4"},` +
		`{"job":"unit-tests","fact":"failure_reason"}],` +
		`"remedy":"Update the expected value in the example test.",` +
		`"files":["pkg/example/sum_test.go"]}]}`
	b, err := ParseBatch([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	e := b.Effects[0]
	if e.Kind != KindDiagnosis || e.Classification != ClassTest || len(e.Evidence) != 2 ||
		e.Evidence[0].Job != "unit-tests" || e.Evidence[1].Fact != FactFailureReason ||
		len(e.Files) != 1 || e.Remedy == "" || e.Summary == "" {
		t.Fatalf("decoded %+v", e)
	}
	if err := ValidateDiagnosis(b, diagCtx()); err != nil {
		t.Fatalf("ValidateDiagnosis: %v", err)
	}
	if err := ValidateTargets(b, Targets{Pipelines: map[int64]bool{3200: true}}); err != nil {
		t.Fatalf("ValidateTargets: %v", err)
	}
	if got := DiagnosisLines(b); len(got) != 1 || got[0].Job != "unit-tests" || got[0].Item != 0 {
		t.Fatalf("DiagnosisLines = %+v", got)
	}
}

// Decision (plan P5): an unknown classification is refused at PARSE.
func TestParseRejectsUnknownClassification(t *testing.T) {
	for _, c := range []string{"", "network", "CODE", "infra", " flaky"} {
		raw := `{"effects":[{"kind":"diagnosis","classification":"` + c + `","summary":"s","remedy":"r","evidence":[{"fact":"pipeline_error"}]}]}`
		if _, err := ParseBatch([]byte(raw)); err == nil {
			t.Errorf("ParseBatch accepted classification %q", c)
		}
	}
	for _, c := range Classifications() {
		raw := `{"effects":[{"kind":"diagnosis","classification":"` + string(c) + `"}]}`
		if _, err := ParseBatch([]byte(raw)); err != nil {
			t.Errorf("ParseBatch refused known classification %q: %v", c, err)
		}
	}
}

// An evidence item is a closed struct too: an unknown key is refused at parse.
func TestParseRejectsUnknownEvidenceKey(t *testing.T) {
	raw := `{"effects":[{"kind":"diagnosis","classification":"unknown","evidence":[{"job":"a","quote":"x"}]}]}`
	if _, err := ParseBatch([]byte(raw)); err == nil {
		t.Fatal("ParseBatch accepted an unknown evidence key")
	}
}

// D10: a diagnosis may name only the recorded pipeline, or nothing.
func TestDiagnosisTargets(t *testing.T) {
	const recorded = 3200
	ctx := Targets{
		Issues:        map[int64]bool{5: true},
		MergeRequests: map[int64]bool{5: true},
		Pipelines:     map[int64]bool{recorded: true},
	}
	d := func(iid int64, tk TargetKind) Batch {
		e := goodDiagnosis()
		e.TargetIID, e.TargetType = iid, tk
		return one(e)
	}
	for name, b := range map[string]Batch{
		"merge request 5":         d(5, TargetMergeRequest),
		"issue 5":                 d(5, TargetIssue),
		"other pipeline":          d(recorded+1, TargetPipeline),
		"other pipeline, no type": d(recorded+1, ""),
		"pipeline type, no id":    d(0, TargetPipeline),
	} {
		if err := ValidateTargets(b, ctx); err == nil {
			t.Errorf("%s: want refusal", name)
		}
	}
	for name, b := range map[string]Batch{
		"primary (0)":             d(0, ""),
		"recorded pipeline":       d(recorded, TargetPipeline),
		"recorded pipeline, bare": d(recorded, ""),
	} {
		if err := ValidateTargets(b, ctx); err != nil {
			t.Errorf("%s: want accept, got %v", name, err)
		}
	}
	// Comment and label stay issue-only: neither may name a pipeline even
	// when that pipeline is in context.
	for _, k := range []Kind{KindComment, KindLabel} {
		b := one(Effect{Kind: k, Body: "x", Add: []string{"a"}, TargetIID: recorded, TargetType: TargetPipeline})
		if err := ValidateTargets(b, ctx); err == nil {
			t.Errorf("%s targeting a pipeline: want refusal", k)
		}
	}
}

func TestLoadShapeAcceptsDiagnosis(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "agents/pipeline/effect-shape.toml", `
[diagnosis]
min = 1
max = 1
`)
	s, err := LoadShape(dir, "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if s.Kinds[KindDiagnosis] != (Card{1, 1}) {
		t.Fatalf("diagnosis card %+v", s.Kinds[KindDiagnosis])
	}
	// Exactly one diagnosis and nothing else.
	if err := Validate(one(goodDiagnosis()), s); err != nil {
		t.Fatalf("one diagnosis: %v", err)
	}
	if err := Validate(Batch{Effects: []Effect{goodDiagnosis(), goodDiagnosis()}}, s); err == nil {
		t.Fatal("two diagnoses accepted")
	}
	if err := Validate(Batch{Effects: []Effect{goodDiagnosis(), {Kind: KindComment, Body: "x"}}}, s); err == nil {
		t.Fatal("diagnosis + comment accepted by a diagnosis-only shape")
	}
}

// The triage shape forbids a diagnosis: adding the kind widened nothing.
func TestTriageShapeForbidsDiagnosis(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, goodDiagnosis()}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("triage shape accepted a diagnosis effect")
	}
}

// A diagnosis's files are CITATIONS, not writes: the protected-path denylist
// exists to stop gonk WRITING repository control files, and a configuration
// diagnosis must be able to cite the CI definition. Every protected path
// validates OK as a citation through every gate a diagnosis meets...
func TestDiagnosisMayCiteProtectedPaths(t *testing.T) {
	e := goodDiagnosis()
	e.Classification = ClassConfiguration
	e.Files = []string{".gitlab-ci.yml", ".gitlab/ci/build.yml", "CODEOWNERS", ".gonk.yml", ".github/workflows/x.yml"}
	b := one(e)
	if err := ValidateProtectedPaths(b); err != nil {
		t.Errorf("ValidateProtectedPaths refused a diagnosis's citations: %v", err)
	}
	if err := ValidatePaths(b); err != nil {
		t.Errorf("ValidatePaths refused a diagnosis's citations: %v", err)
	}
	if err := ValidateDiagnosis(b, diagCtx()); err != nil {
		t.Errorf("ValidateDiagnosis refused a configuration diagnosis citing CI files: %v", err)
	}
	// ...and none of them can become a write.
	if w := FileWrites(b); len(w) != 0 {
		t.Errorf("FileWrites turned a diagnosis into writes: %+v", w)
	}
}

// A diagnosis may carry ONLY its own fields: a `path`/`content` (or any other
// kind's field) on a diagnosis is refused, so a batch cannot smuggle a write
// shape onto a kind whose paths the denylist does not check.
func TestValidateDiagnosisRefusesForeignFields(t *testing.T) {
	for name, f := range map[string]func(*Effect){
		"path":    func(e *Effect) { e.Path = ".gitlab-ci.yml" },
		"content": func(e *Effect) { e.Content = "x" },
		"body":    func(e *Effect) { e.Body = "x" },
		"add":     func(e *Effect) { e.Add = []string{"a"} },
		"remove":  func(e *Effect) { e.Remove = []string{"a"} },
		"title":   func(e *Effect) { e.Title = "x" },
		// gonk-8yux: the fix vocabulary, added to Effect after this rule was
		// written by hand, rode on a diagnosis unread.
		"old":    func(e *Effect) { e.Old = "x" },
		"new":    func(e *Effect) { e.New = "x" },
		"paths":  func(e *Effect) { e.Paths = []string{"a.go"} },
		"reason": func(e *Effect) { e.Reason = "x" },
	} {
		e := goodDiagnosis()
		f(&e)
		if err := ValidateDiagnosis(one(e), diagCtx()); !errors.Is(err, ErrDiagnosisForeignField) {
			t.Errorf("%s on a diagnosis: got %v, want %v", name, err, ErrDiagnosisForeignField)
		}
	}
}

// gonk-8yux: the rule is by REFLECTION over diagnosisFields, as the fix kinds'
// is over fixKindFields, so a field added to Effect later is foreign to a
// diagnosis until someone lists it. Every field of Effect is tried: the
// diagnosis's own are accepted, the target fields are left to ValidateTargets
// (which accepts them on a diagnosis), and every other one is refused.
func TestDiagnosisRefusesEveryForeignField(t *testing.T) {
	rt := reflect.TypeOf(Effect{})
	foreign := 0
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if name == "Kind" {
			continue
		}
		e := goodDiagnosis()
		setField(t, &e, name)
		if name == "TargetType" {
			e.TargetType = TargetPipeline
		}
		err := ValidateDiagnosis(one(e), diagCtx())
		switch {
		case diagnosisFields[name] || targetFields[name]:
			if errors.Is(err, ErrDiagnosisForeignField) {
				t.Errorf("a diagnosis carrying its own field %s is refused as foreign", name)
			}
		default:
			foreign++
			if !errors.Is(err, ErrDiagnosisForeignField) {
				t.Errorf("a diagnosis carrying %s: got %v, want %v", name, err, ErrDiagnosisForeignField)
			}
		}
	}
	if foreign < 10 {
		t.Errorf("only %d fields of Effect were tried as foreign: the loop is not covering the struct", foreign)
	}
	// The near miss: a good diagnosis with the target fields set is not
	// refused by THIS rule.
	e := goodDiagnosis()
	e.TargetIID, e.TargetType = 7, TargetPipeline
	if err := ValidateDiagnosis(one(e), diagCtx()); err != nil {
		t.Errorf("a diagnosis with its target fields: %v", err)
	}
}
