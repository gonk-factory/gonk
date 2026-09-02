package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/effects"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// projectPolicyServer serves /v1/project/... with a fixed actions.features, or
// fails outright when features is nil, so a test can distinguish "policy says
// no" from "we could not find out".
func projectPolicyServer(t *testing.T, features *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if features == nil {
			http.Error(w, "meter unavailable", http.StatusInternalServerError)
			return
		}
		resp := meterapi.ProjectResponse{
			Project:   "grp/proj",
			Effective: &meterapi.Effective{Actions: meterapi.Actions{Features: *features}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func verdictDeps(t *testing.T, applier *recordingApplier, features *bool) sweepDeps {
	t.Helper()
	return sweepDeps{
		Meter: meterClient(projectPolicyServer(t, features).URL),
		Apply: applier,
		Log:   slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func rec() beadstore.Record {
	return beadstore.Record{BeadAnchor: "gonk:7:issue:3", ProjectID: 7, IssueIID: 3, Project: "grp/proj"}
}

// reply-only must not touch the issue's state. It is the default, so a bug here
// would silently close issues for every agent that predates verdicts.
func TestReplyOnlyClosesNothing(t *testing.T) {
	for _, b := range []effects.Batch{
		{Verdict: effects.VerdictReplyOnly},
		{}, // absent verdict -> reply-only
	} {
		ap := &recordingApplier{}
		applyVerdict(context.Background(), verdictDeps(t, ap, boolp(true)), rec(), b)
		if len(ap.closed) != 0 {
			t.Fatalf("verdict %q closed issues %v; it must change no state", b.EffectiveVerdict(), ap.closed)
		}
		// The audit label is expected; what must NOT appear is a routing label.
		if !hasLabel(ap, "gonk::verdict-reply-only") {
			t.Fatalf("verdict %q must be recorded for audit; labels = %v", b.EffectiveVerdict(), ap.labels)
		}
		if hasLabel(ap, labelNeedsMaintainer) || hasLabel(ap, labelFixQueued) {
			t.Fatalf("verdict %q routed work somewhere; the comment is the whole action", b.EffectiveVerdict())
		}
	}
}

func TestCloseVerdictClosesTheIssue(t *testing.T) {
	ap := &recordingApplier{}
	applyVerdict(context.Background(), verdictDeps(t, ap, boolp(true)), rec(),
		effects.Batch{Verdict: effects.VerdictClose})
	if len(ap.closed) != 1 || ap.closed[0] != 3 {
		t.Fatalf("closed = %v, want [3]", ap.closed)
	}
}

// A close that fails must not look like a batch failure: the comment explaining
// the closure is already posted, and re-running would double-post it.
func TestACloseThatFailsDoesNotFailTheBatch(t *testing.T) {
	ap := &recordingApplier{closeErr: errors.New("gitlab said no")}
	applyVerdict(context.Background(), verdictDeps(t, ap, boolp(true)), rec(),
		effects.Batch{Verdict: effects.VerdictClose})
	if len(ap.closed) != 0 {
		t.Fatal("close should not have been recorded")
	}
}

// features=false: the finding must still reach a human. An identified fix that
// nobody is told about is worse than no triage, because the project now believes
// the issue was triaged.
func TestCodeChangeWithoutFeaturesRoutesToAMaintainer(t *testing.T) {
	ap := &recordingApplier{}
	applyVerdict(context.Background(), verdictDeps(t, ap, boolp(false)), rec(),
		effects.Batch{Verdict: effects.VerdictCodeChange})
	if !hasLabel(ap, labelNeedsMaintainer) {
		t.Fatalf("labels = %v, want %q", ap.labels, labelNeedsMaintainer)
	}
	if hasLabel(ap, labelFixQueued) {
		t.Fatal("must not queue a fix run on a project that has not opted in")
	}
}

func TestCodeChangeWithFeaturesQueuesAFixRun(t *testing.T) {
	ap := &recordingApplier{}
	applyVerdict(context.Background(), verdictDeps(t, ap, boolp(true)), rec(),
		effects.Batch{Verdict: effects.VerdictCodeChange})
	if !hasLabel(ap, labelFixQueued) {
		t.Fatalf("labels = %v, want %q", ap.labels, labelFixQueued)
	}
}

// NOT KNOWING THE POLICY IS NOT PERMISSION. An unreachable meter must be
// treated exactly like an explicit "no" -- route to a human, never touch the
// repository on a guess.
func TestUnknownPolicyFailsClosed(t *testing.T) {
	ap := &recordingApplier{}
	applyVerdict(context.Background(), verdictDeps(t, ap, nil), rec(),
		effects.Batch{Verdict: effects.VerdictCodeChange})
	if !hasLabel(ap, labelNeedsMaintainer) {
		t.Fatalf("labels = %v, want %q when the policy is unknown", ap.labels, labelNeedsMaintainer)
	}
	if hasLabel(ap, labelFixQueued) {
		t.Fatal("an unreadable policy must never be treated as opt-in")
	}
}

func boolp(b bool) *bool { return &b }

func hasLabel(ap *recordingApplier, want string) bool {
	for _, l := range ap.labels {
		if l.Label == want {
			return true
		}
	}
	return false
}
