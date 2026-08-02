package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

// Scaffold is the first trigger whose artifact is repository CONTENT. The agent
// holds no forge credentials, so it proposes files and the BROKER commits them
// and opens the merge request -- the same inversion as triage, where the agent
// proposes a comment and the broker posts it.

func scaffoldBatchJSON(files ...[2]string) string {
	var b strings.Builder
	b.WriteString(`{"effects":[`)
	for i, f := range files {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"kind":"file","path":"` + f[0] + `","content":"` + f[1] + `"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// scaffoldSweepFixture wires the pieces applyBrokerBatch needs: a supervisor
// whose session finished with the given batch fenced in its transcript, a
// GitLab the existence check can ask about, and the recording applier.
func scaffoldSweepFixture(t *testing.T, applier *recordingApplier, batchJSON string) (sweepDeps, beadstore.Record) {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)

	rec := scaffoldRecord(p.ID)
	gc := gcapitest.New(t)
	gc.FinishSession(rec.SessionID, "thinking...\nGONK_BATCH_START\n"+batchJSON+"\nGONK_BATCH_END\n")

	return sweepDeps{
		GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: beadstore.NewMemory(), BotUsername: "gonk", PackDir: repoPackDir,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, rec
}

// runningView is the session view the sweep already decided was finished; the
// apply path only reads the transcript, so this just has to be non-nil.
func runningView(alias string) *gcapi.SessionView {
	return &gcapi.SessionView{ID: alias, Alias: alias, State: "stopped"}
}

// scaffoldRecord is a finished, project-scoped scaffold bead. IssueIID is 0:
// scaffold is dispatched for a PROJECT, and a scaffold record carrying an issue
// would be a different bug.
func scaffoldRecord(pid int64) beadstore.Record {
	rec := baseRunningRecord()
	rec.Trigger = "scaffold"
	rec.IssueIID = 0
	rec.ProjectID = pid
	rec.SessionID = brokerSessionAlias("scaffold", pid, 0, 1)
	return rec
}

func TestScaffoldBatchIsCommittedAndOpensOneMR(t *testing.T) {
	applier := &recordingApplier{}
	d, rec := scaffoldSweepFixture(t, applier, scaffoldBatchJSON(
		[2]string{".agent/README.md", "what this is"},
		[2]string{".agent/testing.md", "how to test it"},
	))

	applied, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", rec, runningView(rec.SessionID))
	if err != nil {
		t.Fatalf("applyBrokerBatch = %v", err)
	}
	if violation != "" {
		t.Fatalf("violation = %q, want none", violation)
	}
	if !applied {
		t.Fatal("applied = false, want true")
	}

	if len(applier.commits) != 1 {
		t.Fatalf("commits = %d, want exactly 1 -- the whole batch is one commit", len(applier.commits))
	}
	c := applier.commits[0]
	if c.Branch != scaffoldBranch {
		t.Errorf("committed to %q, want %q", c.Branch, scaffoldBranch)
	}
	// StartBranch is what creates the branch on the first run. Without it the
	// very first scaffold on a repository fails with "branch not found".
	if c.StartBranch != "main" {
		t.Errorf("StartBranch = %q, want the project's default branch", c.StartBranch)
	}
	if len(c.Actions) != 2 {
		t.Fatalf("actions = %+v, want one per file", c.Actions)
	}

	if len(applier.mrs) != 1 {
		t.Fatalf("merge requests = %d, want exactly 1 (the gate requires exactly one)", len(applier.mrs))
	}
	mr := applier.mrs[0]
	if mr.SourceBranch != scaffoldBranch || mr.TargetBranch != "main" {
		t.Errorf("MR %s -> %s, want %s -> main", mr.SourceBranch, mr.TargetBranch, scaffoldBranch)
	}
	// The marker is what gonk-gate check looks for. Without it the run is
	// recorded as a failure however good the content is.
	if !strings.Contains(mr.Description, "<!-- gonk:bead:"+rec.BeadID+" -->") {
		t.Errorf("MR description is missing the bead marker:\n%s", mr.Description)
	}
}

// A re-sling runs the apply again. It must update the existing branch and reuse
// the open MR rather than littering the repo with one per attempt -- "exactly
// one merge request" is the gate's requirement, not a preference.
func TestScaffoldReusesTheOpenMROnResling(t *testing.T) {
	applier := &recordingApplier{
		openMRs: []glab.MergeRequest{{IID: 3, WebURL: "https://example/mr/3"}},
	}
	d, rec := scaffoldSweepFixture(t, applier, scaffoldBatchJSON([2]string{".agent/README.md", "x"}))

	if _, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", rec, runningView(rec.SessionID)); err != nil || violation != "" {
		t.Fatalf("apply = (%q, %v)", violation, err)
	}
	if len(applier.mrs) != 0 {
		t.Fatalf("opened %d new MRs, want 0 -- the open one must be reused", len(applier.mrs))
	}
	if len(applier.commits) != 1 {
		t.Fatalf("commits = %d, want 1 -- the branch is still updated", len(applier.commits))
	}
}

// THE SECURITY CASE. The paths are model output derived from an untrusted
// repository and the broker writes them under its own credentials, so a batch
// reaching outside .agent/ must be rejected IN FULL -- nothing committed, no MR.
func TestScaffoldRefusesToWriteOutsideAgentDir(t *testing.T) {
	for _, path := range []string{
		".gitlab-ci.yml",           // arbitrary code execution wearing a commit
		".gonk.yml",                // would let a run rewrite its own budget
		".agent/../.gitlab-ci.yml", // the same, via traversal
	} {
		applier := &recordingApplier{}
		d, rec := scaffoldSweepFixture(t, applier, scaffoldBatchJSON(
			[2]string{".agent/README.md", "legitimate"},
			[2]string{path, "malicious"},
		))

		applied, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", rec, runningView(rec.SessionID))
		if err != nil {
			t.Fatalf("%s: err = %v, want a clean violation", path, err)
		}
		if applied {
			t.Errorf("%s: applied = true, want false", path)
		}
		if violation == "" {
			t.Errorf("%s: violation = empty, want a path rejection", path)
		}
		// The legitimate file in the same batch must NOT be committed either: a
		// partial apply of a rejected batch is the worst outcome available.
		if len(applier.commits) != 0 || len(applier.mrs) != 0 {
			t.Errorf("%s: committed %d / opened %d despite the violation -- must be all or nothing",
				path, len(applier.commits), len(applier.mrs))
		}
	}
}

// The shape gate forbids a scaffold emitting comments or labels: a scaffold run
// is project-scoped and has no issue to comment on, so one that does is
// confused about what it is doing.
func TestScaffoldBatchMayNotComment(t *testing.T) {
	applier := &recordingApplier{}
	d, rec := scaffoldSweepFixture(t, applier,
		`{"effects":[{"kind":"file","path":".agent/README.md","content":"x"},{"kind":"comment","body":"hi"}]}`)

	applied, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", rec, runningView(rec.SessionID))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if applied || violation == "" {
		t.Fatalf("applied=%v violation=%q, want a shape rejection", applied, violation)
	}
	if len(applier.notes) != 0 {
		t.Errorf("posted %d comments, want 0", len(applier.notes))
	}
}
