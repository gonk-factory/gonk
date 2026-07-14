package tagmint

import (
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

func req() Request {
	return Request{
		Project: "group/repo", Rig: "group-repo", BeadID: "gk-1a2b",
		SessionKey: "sess-9", Rung: "qwen-local", Attempt: 2,
		Trigger: atags.TriggerIssueTriage,
	}
}

func TestMintValid(t *testing.T) {
	tags, err := Mint(req())
	if err != nil {
		t.Fatalf("Mint = %v", err)
	}
	md := tags.Metadata()
	if md[atags.KeyProject] != "group/repo" || md[atags.KeyAttempt] != "2" {
		t.Fatalf("metadata = %v", md)
	}
	if err := tags.Validate(); err != nil {
		t.Fatalf("minted tags fail the atags contract: %v", err)
	}
}

// Everything here is accepted by atags.Validate today. None of it may reach a
// log line, a metadata header, or a CSV column.
func TestMintRejectsHostileValues(t *testing.T) {
	bad := map[string]func(*Request){
		"newline in project":     func(r *Request) { r.Project = "group/repo\ninjected: true" },
		"CR in project":          func(r *Request) { r.Project = "group/repo\rX" },
		"tab in rig":             func(r *Request) { r.Rig = "repo\tX" },
		"comma in rung":          func(r *Request) { r.Rung = "glm,sonnet" },
		"NUL in bead id":         func(r *Request) { r.BeadID = "gk-1\x00" },
		"DEL in session key":     func(r *Request) { r.SessionKey = "sess\x7f9" },
		"unicode line separator": func(r *Request) { r.Project = "group/repo X" },
		"RTL override":           func(r *Request) { r.Project = "group/\u202erepo" },
		"quote in project":       func(r *Request) { r.Project = `group/"repo"` },
		"backslash in project":   func(r *Request) { r.Project = `group\repo` },
		"leading space":          func(r *Request) { r.Project = " group/repo" },
		"overlong value":         func(r *Request) { r.Project = strings.Repeat("a", 300) },
		"uppercase rung":         func(r *Request) { r.Rung = "GLM" },
		"empty project":          func(r *Request) { r.Project = "" },
		"attempt zero":           func(r *Request) { r.Attempt = 0 },
		"unknown trigger":        func(r *Request) { r.Trigger = "vibes" },
	}
	for name, mut := range bad {
		r := req()
		mut(&r)
		if _, err := Mint(r); err == nil {
			t.Errorf("%s: Mint accepted %+v", name, r)
		}
	}
}

// The legal shapes must keep working: GitLab paths nest, bead IDs and session
// keys carry dots and colons.
func TestMintAcceptsRealisticValues(t *testing.T) {
	ok := []func(*Request){
		func(r *Request) { r.Project = "agentic/experiments/deep-nest.v2" },
		func(r *Request) { r.BeadID = "gk-1a2b.3" },
		func(r *Request) { r.SessionKey = "triage:issue-42:sess-9" },
		func(r *Request) { r.Rung = "qwen-local-30b" },
		func(r *Request) { r.Attempt = 99 },
		func(r *Request) { r.Trigger = atags.TriggerScaffold },
	}
	for i, mut := range ok {
		r := req()
		mut(&r)
		if _, err := Mint(r); err != nil {
			t.Errorf("case %d: Mint rejected a legitimate value %+v: %v", i, r, err)
		}
	}
}

// Minted tags are the ledger join key: they must survive the full
// Metadata()/FromMetadata() round trip unchanged, not just pass Validate().
func TestMintRoundTripsThroughMetadata(t *testing.T) {
	tags, err := Mint(req())
	if err != nil {
		t.Fatalf("Mint = %v", err)
	}
	got, err := atags.FromMetadata(tags.Metadata())
	if err != nil {
		t.Fatalf("FromMetadata(Metadata()) = %v", err)
	}
	if got != tags {
		t.Fatalf("round trip mismatch: minted %+v, recovered %+v", tags, got)
	}
}
