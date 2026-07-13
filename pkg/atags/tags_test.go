package atags

import (
	"reflect"
	"testing"
)

func valid() Tags {
	return Tags{
		Project: "group/repo", Rig: "repo", BeadID: "gk-1a2b",
		SessionKey: "sess-9", Rung: "qwen-local", Attempt: 1,
		Trigger: TriggerIssueTriage,
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid tags rejected: %v", err)
	}
	bad := []func(*Tags){
		func(x *Tags) { x.Project = "" },
		func(x *Tags) { x.Rig = "" },
		func(x *Tags) { x.BeadID = "" },
		func(x *Tags) { x.SessionKey = "" },
		func(x *Tags) { x.Rung = "" },
		func(x *Tags) { x.Attempt = 0 },
		func(x *Tags) { x.Trigger = "vibes" },
	}
	for i, mut := range bad {
		tags := valid()
		mut(&tags)
		if err := tags.Validate(); err == nil {
			t.Errorf("case %d: invalid tags accepted: %+v", i, tags)
		}
	}
}

// The literal key and trigger strings ARE the ledger contract (spec 10.1):
// a rename must fail CI even though the round-trip test, which uses the
// constants symmetrically, would still pass.
func TestContractLiterals(t *testing.T) {
	wantKeys := map[string]string{
		KeyProject: "gonk_project", KeyRig: "gonk_rig",
		KeyBeadID: "gonk_bead_id", KeySessionKey: "gonk_session_key",
		KeyRung: "gonk_rung", KeyAttempt: "gonk_attempt",
		KeyTrigger: "gonk_trigger",
	}
	for got, want := range wantKeys {
		if got != want {
			t.Errorf("metadata key changed: %q != %q (breaking ledger change; see spec 10.1)", got, want)
		}
	}
	wantTriggers := map[string]string{
		TriggerIssueTriage: "issue-triage", TriggerOnboarding: "onboarding",
		TriggerScaffold: "scaffold", TriggerMentionReply: "mention-reply",
	}
	for got, want := range wantTriggers {
		if got != want {
			t.Errorf("trigger changed: %q != %q (breaking ledger change)", got, want)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	in := valid()
	out, err := FromMetadata(in.Metadata())
	if err != nil {
		t.Fatalf("FromMetadata = %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: got %+v want %+v", out, in)
	}
}
