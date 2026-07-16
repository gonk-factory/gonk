package gcapitest_test

import (
	"context"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
)

func TestFakeRecordsPoursForAssertion(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")

	res, err := c.RunOrder(context.Background(), "gonk-dispatch", map[string]string{
		"bead_anchor": "gonk:42:issue:3",
		"rung":        "qwen-local",
	})
	if err != nil {
		t.Fatalf("RunOrder = %v", err)
	}
	if res.Status != "queued" || res.TrackingID == "" {
		t.Fatalf("res = %+v", res)
	}
	if len(s.Poured) != 1 {
		t.Fatalf("Poured = %+v, want 1 entry", s.Poured)
	}
	got := s.Poured[0]
	if got.Order != "gonk-dispatch" || got.Vars["bead_anchor"] != "gonk:42:issue:3" || got.Vars["rung"] != "qwen-local" {
		t.Fatalf("Poured[0] = %+v", got)
	}
	if names := s.PouredNames(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("PouredNames = %v", names)
	}
}

// A second Pour is a second bead/session slot burned. A test driving Task 3's
// re-sling path needs to see exactly how many times, and with what vars, each
// named order was actually poured -- append-only, in order.
func TestFakeRecordsMultiplePoursInOrder(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	ctx := context.Background()

	if _, err := c.RunOrder(ctx, "gonk-dispatch", map[string]string{"attempt": "1"}); err != nil {
		t.Fatalf("RunOrder 1 = %v", err)
	}
	if _, err := c.RunOrder(ctx, "gonk-dispatch", map[string]string{"attempt": "2"}); err != nil {
		t.Fatalf("RunOrder 2 = %v", err)
	}
	if _, err := c.RunOrder(ctx, "gonk-scaffold", nil); err != nil {
		t.Fatalf("RunOrder 3 = %v", err)
	}
	names := s.PouredNames()
	if len(names) != 3 || names[0] != "gonk-dispatch" || names[1] != "gonk-dispatch" || names[2] != "gonk-scaffold" {
		t.Fatalf("PouredNames = %v", names)
	}
	if s.Poured[0].Vars["attempt"] != "1" || s.Poured[1].Vars["attempt"] != "2" {
		t.Fatalf("Poured = %+v", s.Poured)
	}
}

// Fail lets a test drive the real client's retry path (Task 3's dispatch
// tests need to prove a transient supervisor 5xx does not abort a poured
// order) without a second, hand-rolled httptest.Server per test.
func TestFakeEmitsConfiguredFailuresThenSucceeds(t *testing.T) {
	s := gcapitest.New(t)
	s.Fail = map[string]int{"gonk-dispatch": 2}
	c := s.Client("gonk-city")

	if _, err := c.RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("RunOrder should have retried through the failures: %v", err)
	}
	if len(s.Poured) != 1 {
		t.Fatalf("Poured = %+v, want exactly 1 (the failed attempts must not be recorded as pours)", s.Poured)
	}
	if s.Fail["gonk-dispatch"] != 0 {
		t.Fatalf("Fail[gonk-dispatch] = %d, want 0 (fully consumed)", s.Fail["gonk-dispatch"])
	}
}

// Client must return a real *gcapi.Client wired to this fake, not a stub --
// callers (Task 3's dispatch code) construct it exactly like they would the
// real supervisor client.
func TestFakeClientIsWiredToTheFake(t *testing.T) {
	s := gcapitest.New(t)
	c := s.Client("gonk-city")
	if c.City != "gonk-city" || c.BaseURL != s.URL() {
		t.Fatalf("client = %+v, want City=gonk-city BaseURL=%s", c, s.URL())
	}
	if _, err := gcapi.New(s.URL(), "gonk-city").RunOrder(context.Background(), "gonk-dispatch", nil); err != nil {
		t.Fatalf("a client built directly against the fake's URL should work identically: %v", err)
	}
}
