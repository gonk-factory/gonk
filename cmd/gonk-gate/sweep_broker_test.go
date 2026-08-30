package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// repoPackDir is the repo's own pack (with agents/triage/effect-shape.toml), so
// the broker validates against the REAL triage shape rather than a fixture.
const repoPackDir = "../../pack"

type noteCall struct {
	ProjectID, IssueIID int64
	Body                string
}
type labelCall struct {
	ProjectID, IssueIID int64
	Label               string
}

// recordingApplier is a brokerApplier that records the exact write calls, so a
// test can assert precisely what the broker applied (the plan's requirement).
type recordingApplier struct {
	mu      sync.Mutex
	notes   []noteCall
	labels  []labelCall
	noteErr error
	// The scaffold half: what the broker committed and which MR it opened.
	commits   []glab.CommitOptions
	mrs       []glab.MROptions
	openMRs   []glab.MergeRequest // pre-existing MRs ListMergeRequests returns
	commitErr error
	project   *glab.Project
}

func (a *recordingApplier) CreateIssueNote(_ context.Context, pid, iid int64, body string) (*glab.Note, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notes = append(a.notes, noteCall{pid, iid, body})
	return &glab.Note{}, a.noteErr
}

func (a *recordingApplier) AddIssueLabel(_ context.Context, pid, iid int64, label string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.labels = append(a.labels, labelCall{pid, iid, label})
	return nil
}

func (a *recordingApplier) CreateCommit(_ context.Context, pid int64, o glab.CommitOptions) (*glab.Commit, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commits = append(a.commits, o)
	return &glab.Commit{ID: "deadbeef"}, a.commitErr
}

func (a *recordingApplier) CreateMergeRequest(_ context.Context, pid int64, o glab.MROptions) (*glab.MergeRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mrs = append(a.mrs, o)
	return &glab.MergeRequest{IID: 7, WebURL: "https://example/mr/7"}, nil
}

func (a *recordingApplier) ListMergeRequests(_ context.Context, pid int64, o glab.MRListOptions) ([]glab.MergeRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.openMRs, nil
}

func (a *recordingApplier) GetProject(_ context.Context, pid int64) (*glab.Project, error) {
	if a.project != nil {
		return a.project, nil
	}
	return &glab.Project{ID: pid, DefaultBranch: "main"}, nil
}

// brokerRunningRecord is a running triage bead with a SessionID -- the v2 broker
// path (a running record without a SessionID falls to the v1 artifactPresent
// path). pid is the glabtest project id.
func brokerRunningRecord(pid int64) beadstore.Record {
	rec := baseRunningRecord() // Trigger issue-triage, IssueIID 3, ended a minute ago
	rec.ProjectID = pid
	rec.SessionID = "gonk.triage.p42.i3.a1"
	return rec
}

// A valid, in-shape batch: the broker posts exactly the comment (with the bead
// marker it stamps) + the labels, under the bot PAT, and the bead finishes.
func TestSweepBrokerAppliesValidBatch(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."},{"kind":"label","add":["gonk::bug","gonk::frontend"]}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	// Exactly one comment, carrying the agent's body AND the bead marker.
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one comment", applier.notes)
	}
	n := applier.notes[0]
	if n.ProjectID != p.ID || n.IssueIID != 3 {
		t.Fatalf("comment targeted %d/%d, want %d/3", n.ProjectID, n.IssueIID, p.ID)
	}
	if !strings.Contains(n.Body, "Safari-only CSS bug") || !strings.Contains(n.Body, "<!-- gonk:bead:gk-1a2b -->") {
		t.Fatalf("comment body missing text or marker:\n%s", n.Body)
	}
	// Both labels, on the issue.
	if len(applier.labels) != 2 || applier.labels[0].Label != "gonk::bug" || applier.labels[1].Label != "gonk::frontend" {
		t.Fatalf("labels = %+v, want gonk::bug, gonk::frontend", applier.labels)
	}
	// The apply is the artifact => success => done, no re-sling.
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want one success", reqs)
	}
	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateDone {
		t.Fatalf("state = %q, want done", got.State)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a successful triage must not re-sling")
	}
}

// An OUT-OF-SHAPE batch (two comments; triage allows exactly one) must apply
// NOTHING and fall to the ladder. This is the shape-gate's mutation guard:
// delete the effects.Validate call in applyBrokerBatch and the broker would
// post two comments -> applier.notes != 0 -> this test fails.
// gonk-5k5: a session that is STILL RUNNING but has already closed its fence
// must be judged, not waited on.
//
// This is the bug that kept gonk from ever posting a triage comment. opencode's
// TUI does not exit after answering, so the provider reports the session Running
// forever; the agent emitted a complete batch in a 32.6s model turn and the
// sweep answered "nothing to judge yet" every 60s until the reservation expired
// and the bead was classified infra-failed. The verdict was gated on a process
// lifecycle detail instead of on the work.
//
// Running the harness non-interactively is the primary fix. This is the
// backstop, and it is the half that survives a model/harness pair that pauses
// or prompts instead of exiting -- so it is worth a test of its own.
func TestSweepBrokerJudgesRunningSessionOnceFenceIsClosed(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	// RunSession, not FinishSession: the provider still says running.
	gc.RunSession("gonk.triage.p42.i3.a1", "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one comment -- a closed fence on a "+
			"still-running session must be judged, not waited on (gonk-5k5)", applier.notes)
	}
}

// The other half of the same rule, and the reason the check is "closed fence"
// rather than "any output": a session that is running and has NOT closed its
// fence is still working, and judging it would re-sling a live agent.
func TestSweepBrokerWaitsWhileRunningWithNoClosedFence(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	// Mid-emission: the fence is open but not closed.
	gc.RunSession("gonk.triage.p42.i3.a1", "thinking...\nGONK_BATCH_START\n{\"effects\":[")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 0 {
		t.Fatalf("notes = %+v, want none -- an unclosed fence means the agent is "+
			"still working and must not be judged", applier.notes)
	}
}

func TestSweepBrokerRejectsOutOfShapeBatch(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", "GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"one"},{"kind":"comment","body":"two"}]}`+
		"\nGONK_BATCH_END")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	// Tokens were spent but no artifact landed => gate-failed => escalate.
	fm := &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1234, AsOf: rec.SessionEndedAt.Add(time.Second)},
	}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 0 || len(applier.labels) != 0 {
		t.Fatalf("an out-of-shape batch must apply NOTHING: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want one gate-failed (spent tokens, no artifact)", reqs)
	}
	if names := gc.PouredNames(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("a rejected batch that spent tokens must re-sling: %v", names)
	}
}

// No fence in the output => produced no batch => nothing applied => ladder.
func TestSweepBrokerNoBatchAppliesNothing(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", "I could not decide. Sorry.")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "retry"} // no spend proof => infra-failed => retry

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.notes) != 0 || len(applier.labels) != 0 {
		t.Fatalf("no batch => nothing applied: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	if len(fm.requests()) != 1 {
		t.Fatalf("expected exactly one outcome report")
	}
}

func TestExtractBatch(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"clean", "GONK_BATCH_START\n{\"effects\":[]}\nGONK_BATCH_END", `{"effects":[]}`, true},
		{"surrounded", "chatter\nGONK_BATCH_START {\"a\":1} GONK_BATCH_END\nbye", `{"a":1}`, true},
		{"takes last start", "GONK_BATCH_START old GONK_BATCH_END GONK_BATCH_START new GONK_BATCH_END", "new", true},
		{"no fence", "just some text", "", false},
		{"start only", "GONK_BATCH_START {\"a\":1}", "", false},
		{"empty payload", "GONK_BATCH_START   GONK_BATCH_END", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractBatch(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
