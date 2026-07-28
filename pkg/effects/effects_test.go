package effects

import "testing"

func TestBatchUnmarshalKnownKinds(t *testing.T) {
	raw := `{"effects":[{"kind":"comment","body":"hi"},{"kind":"label","add":["gonk::bug"]}]}`
	b, err := ParseBatch([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Effects) != 2 || b.Effects[0].Kind != KindComment || b.Effects[1].Kind != KindLabel {
		t.Fatalf("unexpected batch: %+v", b)
	}
}

func TestBatchRejectsUnknownKind(t *testing.T) {
	if _, err := ParseBatch([]byte(`{"effects":[{"kind":"nuke"}]}`)); err == nil {
		t.Fatal("want error for unknown effect kind")
	}
}

func TestBatchRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseBatch([]byte(`{not json`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}
