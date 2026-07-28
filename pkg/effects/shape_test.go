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
