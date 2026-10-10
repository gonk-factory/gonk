package effects

import "testing"

func triageShape() Shape {
	return Shape{Kinds: map[Kind]Card{KindComment: {1, 1}, KindLabel: {0, N}}}
}

func TestShapeAcceptsExactTriage(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindLabel, Add: []string{"gonk::bug"}}}}
	if err := Validate(b, triageShape()); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestShapeRejectsSecondComment(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "a"}, {Kind: KindComment, Body: "b"}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: comment {1,1} but got 2")
	}
}

func TestShapeRejectsForbiddenKind(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindNewIssue, Title: "t"}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: new_issue not allowed by triage shape")
	}
}

func TestShapeRejectsMissingRequiredComment(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindLabel, Add: []string{"gonk::bug"}}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: comment min 1 but got 0")
	}
}

func TestTargetBindingRejectsUnhandedResource(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x", TargetIID: 999}}}
	if err := ValidateTargets(b, Targets{Issues: map[int64]bool{11: true}}); err == nil {
		t.Fatal("want reject: effect targets iid 999 not in context {11}")
	}
}

func TestTargetBindingAcceptsPrimaryAndHanded(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindLabel, Add: []string{"l"}, TargetIID: 11}}}
	if err := ValidateTargets(b, Targets{Issues: map[int64]bool{11: true}}); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestTargetsAreTypedIssue5IsNotMR5(t *testing.T) {
	issue5 := Targets{Issues: map[int64]bool{5: true}}
	mr5 := Targets{MergeRequests: map[int64]bool{5: true}}
	asIssue := Batch{Effects: []Effect{{Kind: KindComment, Body: "x", TargetIID: 5}}}
	explicitIssue := Batch{Effects: []Effect{{Kind: KindComment, Body: "x", TargetIID: 5, TargetType: TargetIssue}}}
	asMR := Batch{Effects: []Effect{{Kind: KindComment, Body: "x", TargetIID: 5, TargetType: TargetMergeRequest}}}

	if err := ValidateTargets(asIssue, issue5); err != nil {
		t.Errorf("issue 5 in context, bare target_iid 5: want accept, got %v", err)
	}
	if err := ValidateTargets(explicitIssue, issue5); err != nil {
		t.Errorf("issue 5 in context, explicit issue: want accept, got %v", err)
	}
	// Only MR !5 handed: an effect meaning "issue 5" (the default) is refused.
	if err := ValidateTargets(asIssue, mr5); err == nil {
		t.Error("only MR !5 in context: a bare/issue target 5 must be refused")
	}
	// Only issue 5 handed: naming MR 5 is refused.
	if err := ValidateTargets(asMR, issue5); err == nil {
		t.Error("only issue 5 in context: target_type merge_request 5 must be refused")
	}
	// Even with MR !5 in context, a comment may not target an MR (apply is
	// issue-only), so the batch is refused rather than written to issue 5.
	if err := ValidateTargets(asMR, Targets{Issues: map[int64]bool{5: true}, MergeRequests: map[int64]bool{5: true}}); err == nil {
		t.Error("comment effect may not name an MR while no MR apply path exists")
	}
}

func TestTargetTypeEdgeCases(t *testing.T) {
	ctx := Targets{Issues: map[int64]bool{5: true}}
	for name, e := range map[string]Effect{
		"unknown type":        {Kind: KindComment, TargetIID: 5, TargetType: "commit"},
		"type without iid":    {Kind: KindComment, TargetType: TargetIssue},
		"file may not target": {Kind: KindFile, Path: ".agent/a", TargetIID: 5},
	} {
		if err := ValidateTargets(Batch{Effects: []Effect{e}}, ctx); err == nil {
			t.Errorf("%s: want refusal", name)
		}
	}
}

func TestTargetTypeWireFormat(t *testing.T) {
	// The current triage wire format (no target_type) still parses.
	b, err := ParseBatch([]byte(`{"effects":[{"kind":"comment","body":"x","target_iid":5}]}`))
	if err != nil || b.Effects[0].TargetType != "" {
		t.Fatalf("legacy batch: %v %+v", err, b)
	}
	b, err = ParseBatch([]byte(`{"effects":[{"kind":"comment","body":"x","target_iid":5,"target_type":"merge_request"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTargets(b, Targets{Issues: map[int64]bool{5: true}}); err == nil {
		t.Fatal("wire-format MR target must be refused when only issue 5 is in context")
	}
}
