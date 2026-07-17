package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func TestCheckExitsZeroWhenMarkerPresent(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	gl.AddNote(p.ID, 3, gl.Me, "Looked at this.\n\n<!-- gonk:bead:gk-1a2b -->\n", false)

	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "issue-triage", BotUsername: "gonk"},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestCheckExitsThreeWhenMarkerAbsent(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "issue-triage", BotUsername: "gonk"},
	})
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (keep polling)", code)
	}
}

// A DIFFERENT bead's marker must not satisfy this bead's gate: otherwise one
// successful triage would mark every other bead on the issue as done.
func TestCheckIgnoresAForeignBeadsMarker(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	gl.AddNote(p.ID, 3, gl.Me, "<!-- gonk:bead:gk-OTHER -->", false)

	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "issue-triage", BotUsername: "gonk"},
	})
	if code != 3 {
		t.Fatalf("exit = %d, want 3 -- a foreign bead's marker must not satisfy this gate", code)
	}
}

// A human quoting gonk's comment must not satisfy the gate: the gate asks "did
// the BOT post it", not "does this text appear anywhere".
func TestCheckIgnoresAHumanQuotingTheMarker(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")
	human := glab.User{ID: 2, Username: "alice"}
	gl.AddNote(p.ID, 3, human, "gonk said: <!-- gonk:bead:gk-1a2b -->", false)

	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "issue-triage", BotUsername: "gonk"},
	})
	if code != 3 {
		t.Fatalf("exit = %d, want 3 -- a human's comment must not satisfy the gate", code)
	}
}

func TestCheckExitsOneOnGitLabOutage(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	gl := glab.New(down.URL, "tok")
	gl.RetryBackoff = func(int) time.Duration { return 0 }

	code := runCheck(context.Background(), checkDeps{
		GL:   gl,
		Args: checkArgs{ProjectID: 1, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "issue-triage", BotUsername: "gonk"},
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (infra -- not the same as 'not there')", code)
	}
}

func TestCheckRefusesUnknownTrigger(t *testing.T) {
	gl := glabtest.New(t)
	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: 1, IssueIID: 3, BeadID: "gk-1a2b", Trigger: "something-new", BotUsername: "gonk"},
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// Scaffold's artifact is an MR from gonk/scaffold, not a comment.
func TestCheckScaffoldLooksForTheMR(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)

	code := runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, BeadID: "gk-1a2b", Trigger: "scaffold", BotUsername: "gonk"},
	})
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (no MR yet)", code)
	}

	if _, err := gl.Client().CreateBranch(context.Background(), p.ID, "gonk/scaffold", "main"); err != nil {
		t.Fatalf("CreateBranch = %v", err)
	}
	if _, err := gl.Client().CreateMergeRequest(context.Background(), p.ID, glab.MROptions{
		SourceBranch: "gonk/scaffold", TargetBranch: "main", Title: "scaffold",
		Description: "<!-- gonk:bead:gk-1a2b -->",
	}); err != nil {
		t.Fatalf("CreateMergeRequest = %v", err)
	}

	code = runCheck(context.Background(), checkDeps{
		GL:   gl.Client(),
		Args: checkArgs{ProjectID: p.ID, BeadID: "gk-1a2b", Trigger: "scaffold", BotUsername: "gonk"},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 once the marker-carrying MR exists", code)
	}
}
