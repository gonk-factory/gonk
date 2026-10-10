package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
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
	// Verdict actions (gonk-aib): which issues the broker closed, and whether
	// closing was made to fail.
	closed   []int64
	closeErr error
	updated  []noteUpdate
}

func (r *recordingApplier) UpdateIssueNote(_ context.Context, _, issueIID, noteID int64, body string) (*glab.Note, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updated = append(r.updated, noteUpdate{IssueIID: issueIID, NoteID: noteID, Body: body})
	return &glab.Note{ID: noteID, Body: body}, nil
}

// batchNotes is every created note that carries a batch's content -- the
// bead marker -- leaving out gonk's own status notes (gonk-l900). A rejected
// or unread batch must apply NOTHING of the agent's; the sweep's re-sling
// status note ("attempt 1 did not produce a valid answer") is the broker's
// own canned text, asserted separately where it matters.
func (r *recordingApplier) batchNotes() []noteCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []noteCall
	for _, n := range r.notes {
		if strings.Contains(n.Body, "<!-- gonk:status:") && !strings.Contains(n.Body, "<!-- gonk:bead:") {
			continue
		}
		out = append(out, n)
	}
	return out
}

type noteUpdate struct {
	IssueIID int64
	NoteID   int64
	Body     string
}

func (r *recordingApplier) CloseIssue(_ context.Context, _, issueIID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeErr != nil {
		return r.closeErr
	}
	r.closed = append(r.closed, issueIID)
	return nil
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
	rec.SessionID = testSession
	return rec
}

// A valid, in-shape batch: the broker posts exactly the comment (with the bead
// marker it stamps) + the labels, under the bot PAT, and the bead finishes.
func TestSweepBrokerAppliesValidBatch(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."},{"kind":"label","add":["gonk::bug","gonk::frontend"]}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
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
	// Both of the agent's labels, then the broker's verdict audit label
	// (gonk-kxg): the batch carries no verdict, so it is reply-only.
	if len(applier.labels) != 3 ||
		applier.labels[0].Label != "gonk::bug" ||
		applier.labels[1].Label != "gonk::frontend" ||
		applier.labels[2].Label != "gonk::verdict-reply-only" {
		t.Fatalf("labels = %+v, want gonk::bug, gonk::frontend, gonk::verdict-reply-only", applier.labels)
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
	if len(orders.orders()) != 0 {
		t.Fatal("a successful triage must not re-sling")
	}
}

// TestSweepBrokerAppliesLargeTranscriptBatch is T-07 (R-10): the triage prompt
// now tells the model to list directories and read code before answering, so a
// real transcript -- every turn, tool call and tool result, concatenated --
// routinely clears the generic 64 KiB API cap that GetSessionTranscript used to
// share with every other supervisor route. Before the dedicated transcript cap,
// this exact shape (a real, valid batch sitting past 64 KiB of exploration
// output) made GetSessionTranscript error, which the broker cannot distinguish
// from "nothing to judge yet" -- the bead falls to the ladder with a completed
// session's work unread and the sweep loops infra-failed forever (the Goal this
// task closes).
//
// It also pins WHICH fence wins when the sentinel appears twice: an agent can
// echo the "GONK_BATCH_START ... GONK_BATCH_END" example straight out of its
// own injected prompt while narrating what it is about to do, well before the
// real, final batch. extractBatch takes the LAST start (see the "takes last
// start" case in TestExtractBatch below); this test proves that holds all the
// way through the transcript-cap change, at full broker scope, not just in
// extractBatch's own table test -- the mid-transcript echo's body
// ("EXAMPLE - ignore me") must never reach GitLab.
func TestSweepBrokerAppliesLargeTranscriptBatch(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	// Padding that stands in for an agent listing directories and reading code
	// -- exactly what the triage prompt now asks for -- large enough alone to
	// clear the old 64 KiB generic cap several times over.
	pad := strings.Repeat("$ ls -la pkg/gcapi && cat pkg/gcapi/client.go\n... 200 lines of source ...\n", 4500)
	if len(pad) <= 64*1024 {
		t.Fatalf("test setup: padding is only %d bytes, want > 64 KiB", len(pad))
	}

	decoyFence := "Here is an example of the format I will use:\n" +
		"GONK_BATCH_START\n" +
		`{"effects":[{"kind":"comment","body":"EXAMPLE - ignore me"}]}` +
		"\nGONK_BATCH_END\n" +
		"Now let me actually look at the issue.\n"
	realFence := "GONK_BATCH_START\n" +
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."},{"kind":"label","add":["gonk::bug"]}]}` +
		"\nGONK_BATCH_END\n"
	transcript := "thinking...\n" + decoyFence + pad + realFence
	if len(transcript) <= 300*1024 {
		t.Fatalf("test setup: transcript is only %d bytes, want > 300 KiB", len(transcript))
	}

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, transcript)
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}

	// The REAL (tail) batch was applied, not the decoy echoed mid-transcript.
	if len(applier.notes) != 1 {
		t.Fatalf("notes = %+v, want exactly one comment", applier.notes)
	}
	n := applier.notes[0]
	if strings.Contains(n.Body, "EXAMPLE - ignore me") {
		t.Fatalf("applied the MID-TRANSCRIPT decoy fence, not the tail one: %s", n.Body)
	}
	if !strings.Contains(n.Body, "Safari-only CSS bug") {
		t.Fatalf("comment body missing the real text:\n%s", n.Body)
	}
	if len(applier.labels) != 2 || applier.labels[0].Label != "gonk::bug" || applier.labels[1].Label != "gonk::verdict-reply-only" {
		t.Fatalf("labels = %+v, want gonk::bug, gonk::verdict-reply-only", applier.labels)
	}

	// The apply is the artifact => success => done, no re-sling -- exactly the
	// "classifies complete" outcome the Goal says a >64 KiB transcript broke.
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
		t.Fatalf("outcome = %+v, want one success", reqs)
	}
	got, _, _ := store.Get(context.Background(), rec.BeadAnchor)
	if got.State != beadstore.StateDone {
		t.Fatalf("state = %q, want done (a valid batch past 64 KiB must not loop infra-failed)", got.State)
	}
	if len(orders.orders()) != 0 {
		t.Fatal("a successful triage must not re-sling")
	}
}

// A session that is still RUNNING is not judged -- not even one whose output
// already holds a closed fence (gonk-t55 plan D9).
//
// Under Gas City this was the other way round (gonk-5k5): opencode's TUI never
// exited, so a closed fence on a "running" session was the ONLY completion
// signal and the sweep judged on it. Under the Jobs runtime `opencode run`
// exits and the Job reports Succeeded or Failed, so the runtime's phase is the
// completion signal and the fence backstop is deleted, not ported: judging a
// session the runtime says is still running would read a log that is still
// being written.
func TestSweepBrokerDoesNotJudgeARunningSessionEvenWithAClosedFence(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	rt.runSession(t, testSession, "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	if code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: &fakeOrders{}, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
	}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.batchNotes()) != 0 {
		t.Fatalf("notes = %+v, want none -- a running session is not judged", applier.notes)
	}
	if reqs := fm.requests(); len(reqs) != 0 {
		t.Fatalf("outcomes = %+v, want none reported for a running session", reqs)
	}
	if n := rt.stopCount(); n != 0 {
		t.Fatalf("stopped a running session inside its reservation (%d stops)", n)
	}
}

// An OUT-OF-SHAPE batch (two comments; triage allows exactly one) must apply
// NOTHING and fall to the ladder. This is the shape-gate's mutation guard:
// delete the effects.Validate call in applyBrokerBatch and the broker would
// post two comments -> applier.notes != 0 -> this test fails.
func TestSweepBrokerRejectsOutOfShapeBatch(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "GONK_BATCH_START\n"+
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
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.batchNotes()) != 0 || len(applier.labels) != 0 {
		t.Fatalf("an out-of-shape batch must apply NOTHING: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want one gate-failed (spent tokens, no artifact)", reqs)
	}
	if names := orders.names(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("a rejected batch that spent tokens must re-sling: %v", names)
	}
}

// T-05 (closes R-02): a batch that is otherwise shape-valid but proposes a
// label the broker refuses -- here gonk::fix-queued, one of gonk's own audit
// labels -- must apply NOTHING, not just skip the bad label effect. This is
// the label gate's mutation guard: move the label-gate loop in
// applyBrokerBatch to AFTER the comment-apply loop (or delete it) and this
// test starts seeing a posted comment, because the batch's one comment effect
// would already have gone out before the bad label was ever looked at.
func TestSweepBrokerRefusesWholeBatchOnInvalidLabel(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."},{"kind":"label","add":["gonk::bug","gonk::fix-queued"]}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	// Tokens were spent but no artifact landed => gate-failed => escalate,
	// exactly like the out-of-shape case: a refused batch is a refused batch,
	// whichever gate refused it.
	fm := &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1234, AsOf: rec.SessionEndedAt.Add(time.Second)},
	}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// ZERO GitLab writes: not the comment, not the good label alongside the bad
	// one, not the broker's own verdict label -- the whole batch is refused
	// before any of applyBrokerBatch's writes run.
	if len(applier.batchNotes()) != 0 || len(applier.labels) != 0 {
		t.Fatalf("a batch with one refused label must apply NOTHING: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want one gate-failed (spent tokens, no artifact)", reqs)
	}
	if names := orders.names(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("a rejected batch that spent tokens must re-sling: %v", names)
	}
}

// T-06 (closes R-03, R-16): a batch that is otherwise shape-valid but proposes
// a comment containing a GitLab quick action (`/close`, on its own line) must
// apply NOTHING -- not the label effect riding alongside it, and above all not
// the comment itself, which is exactly the note GitLab would parse the quick
// action out of. Mirrors TestSweepBrokerRefusesWholeBatchOnInvalidLabel: this
// is the comment gate's mutation guard -- move the ValidateComments call in
// applyBrokerBatch to AFTER the comment-apply loop (or delete it) and this
// test starts seeing a posted comment, because by the time anything looked at
// the body, it would already be out under the bot's own PAT.
func TestSweepBrokerRefusesWholeBatchOnQuickAction(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Closing this as a duplicate.\n/close"},{"kind":"label","add":["gonk::bug"]}]}`+
		"\nGONK_BATCH_END\n")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	// Tokens were spent but no artifact landed => gate-failed => escalate,
	// exactly like the out-of-shape and bad-label cases: a refused batch is a
	// refused batch, whichever gate refused it.
	fm := &fakeOutcomeMeter{
		outcomeNext: "escalate", spendSynced: true,
		sessionCost: meterapi.SessionCostResponse{TotalTokens: 1234, AsOf: rec.SessionEndedAt.Add(time.Second)},
	}

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// ZERO GitLab writes: not the comment carrying the quick action, not the
	// label riding alongside it, not the broker's own verdict label -- the
	// whole batch is refused before any of applyBrokerBatch's writes run.
	if len(applier.batchNotes()) != 0 || len(applier.labels) != 0 {
		t.Fatalf("a batch with a quick-action comment must apply NOTHING: notes=%+v labels=%+v", applier.notes, applier.labels)
	}
	reqs := fm.requests()
	if len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
		t.Fatalf("outcome = %+v, want one gate-failed (spent tokens, no artifact)", reqs)
	}
	if names := orders.names(); len(names) != 1 || names[0] != "gonk-dispatch" {
		t.Fatalf("a rejected batch that spent tokens must re-sling: %v", names)
	}
}

// No fence in the output => produced no batch => nothing applied => ladder.
func TestSweepBrokerNoBatchAppliesNothing(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	rt := newTestRuntime()
	orders := &fakeOrders{}
	rt.finishSession(t, testSession, "I could not decide. Sorry.")
	applier := &recordingApplier{}

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "retry"} // no spend proof => infra-failed => retry

	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), Runtime: rt, Orders: orders, GL: gl.Client(), Apply: applier,
		Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		SpendPollInterval: time.Millisecond, SpendDeadline: 20 * time.Millisecond,
	})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(applier.batchNotes()) != 0 || len(applier.labels) != 0 {
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
