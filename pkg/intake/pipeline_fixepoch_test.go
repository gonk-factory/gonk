package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/ghook"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// gonk-2usw part EPOCH (plan 3.3). Synthetic throughout: project group/repo,
// made-up jobs, SHAs, iids and times. Nothing encodes a real failure.

const (
	fixBranch  = "gonk/pipefix-0123456789ab"
	fixBranch2 = "gonk/pipefix-ba9876543210"
	shaMergeC  = "cccccccccccccccccccccccccccccccccccccccc"
	shaFixHead = "dddddddddddddddddddddddddddddddddddddddd"
)

// mergedFix seeds a merged, bot-authored fix MR into target, merged at `at`.
func (x *plFixture) mergedFix(iid int64, target string, at time.Time, mut func(*glab.MergeRequest)) {
	mr := glab.MergeRequest{
		IID: iid, State: "merged", SourceBranch: fixBranch, TargetBranch: target,
		Author: botUser, MergedAt: &at, MergeCommitSHA: shaMergeC, SHA: shaFixHead,
		Title: "gonk: fix unit on " + target,
	}
	if mut != nil {
		mut(&mr)
	}
	x.gl.AddMR(x.pid, mr)
}

// between is an instant strictly between the creation of two pipelines.
func between(a, b glab.Pipeline) time.Time {
	return a.CreatedAt.Add(b.CreatedAt.Sub(a.CreatedAt) / 2)
}

func (x *plFixture) gather(t *testing.T, ev *ghook.Event) *PipelineFacts {
	t.Helper()
	f, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), ev)
	if err != nil {
		t.Fatalf("Gather = %v", err)
	}
	return f
}

// THE BUG THIS PART EXISTS FOR (plan F5): a run was diagnosed, gonk's fix MR
// for it merged, and the SAME jobs fail again on the target. The fingerprint
// and the last green pipeline are unchanged -- and before the fix epoch so was
// the anchor, so the landing answered "diagnosed" and the failure the fix
// revealed was never looked at. It must be a NEW run.
//
// And the run the fix ended must stay exactly what it was: its own failing
// pipeline, re-delivered after the merge, is still the old run.
func TestAMergedFixStartsANewRunForTheSameFingerprint(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaB})
	green := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB})
	before := x.failedPipeline("main", shaA, "push", "unit")

	a := x.decide(t, x.event(before, nil))
	wantA, _ := pipelinectx.Anchor(x.pid, pipelinectx.RefTarget("main"), pipelinectx.Fingerprint(jobs("unit"), ""), green.ID, 0)
	if !a.Dispatch || a.BeadAnchor != wantA || a.PipelineFixEpoch != 0 {
		t.Fatalf("before any fix: decision = %+v, want a dispatch on %s with no fix epoch", a, wantA)
	}
	// The run is diagnosed: its marker is on the ref's tracking issue.
	const issueIID = 21
	x.gl.AddIssue(x.pid, issueIID, "opened", "gonk::pipeline-failing")
	x.setIssue(issueIID, botUser, pipelinectx.RefIssueMarker("main"))
	x.gl.AddNote(x.pid, issueIID, botUser, x.marker(t, a.BeadAnchor, pipelinectx.RunStateDiagnosed), false)
	if d := x.decide(t, x.event(before, nil)); d.Dispatch || d.Reason != ReasonRunAlreadyHandled {
		t.Fatalf("the diagnosed run, before the fix: %+v, want %s", d, ReasonRunAlreadyHandled)
	}

	// gonk's fix MR !12 merges; the same job then fails again.
	after := x.failedPipeline("main", shaB, "push", "unit")
	x.mergedFix(12, "main", between(before, after), nil)

	b := x.decide(t, x.event(after, nil))
	if b.PipelineFingerprint != a.PipelineFingerprint || b.PipelineLastSuccess != a.PipelineLastSuccess {
		t.Fatalf("the test's premise is broken: fingerprint %s/%s, last success %d/%d must be unchanged",
			a.PipelineFingerprint, b.PipelineFingerprint, a.PipelineLastSuccess, b.PipelineLastSuccess)
	}
	if b.BeadAnchor == a.BeadAnchor {
		t.Fatalf("after merged fix !12, the same fingerprint yields the SAME anchor %s: the failure the fix revealed is absorbed into the run the fix ended (decision %+v)",
			b.BeadAnchor, b)
	}
	if want := wantA + ":f12"; b.BeadAnchor != want || b.PipelineFixEpoch != 12 {
		t.Fatalf("after merged fix !12: anchor %s epoch %d, want %s epoch 12", b.BeadAnchor, b.PipelineFixEpoch, want)
	}
	if b.SessionKey == a.SessionKey {
		t.Fatalf("the new run has the old run's session key %s", b.SessionKey)
	}
	if !b.Dispatch {
		t.Fatalf("the failure after the merged fix was not dispatched: %+v", b)
	}

	// The old run's pipeline, re-delivered AFTER the merge (a webhook retry,
	// or the backfill sweep finding it still the newest terminal pipeline): it
	// is still the old run, and still handled.
	if d := x.decide(t, x.event(before, nil)); d.BeadAnchor != a.BeadAnchor || d.PipelineFixEpoch != 0 || d.Dispatch || d.Reason != ReasonRunAlreadyHandled {
		t.Fatalf("the old run's pipeline after the merge: %+v, want anchor %s, %s", d, a.BeadAnchor, ReasonRunAlreadyHandled)
	}

	// Once the new run is diagnosed too, IT is handled -- by its own marker.
	x.gl.AddNote(x.pid, issueIID, botUser, x.marker(t, b.BeadAnchor, pipelinectx.RunStateDiagnosed), false)
	if d := x.decide(t, x.event(after, nil)); d.Dispatch || d.Reason != ReasonRunAlreadyHandled || d.BeadAnchor != b.BeadAnchor {
		t.Fatalf("the new run after its own diagnosis: %+v", d)
	}
}

// The merge commit's OWN pipeline can be created before GitLab stamps
// merged_at (the stamp was observed 3 s after the merge commit). It runs on
// the fix, so it is after the fix whatever the clocks say.
func TestTheMergeCommitsOwnPipelineIsAfterTheFix(t *testing.T) {
	for _, landed := range []struct {
		name string
		mut  func(*glab.MergeRequest)
		sha  string
	}{
		{"merge commit", nil, shaMergeC},
		{"squash commit", func(mr *glab.MergeRequest) { mr.MergeCommitSHA, mr.SquashCommitSHA = "", shaB }, shaB},
		{"fast-forwarded head", func(mr *glab.MergeRequest) { mr.MergeCommitSHA = "" }, shaFixHead},
	} {
		t.Run(landed.name, func(t *testing.T) {
			x := newPLFixture(t)
			x.gl.AddCommit(x.pid, glab.Commit{ID: landed.sha})
			pl := x.failedPipeline("main", landed.sha, "push", "unit")
			x.mergedFix(12, "main", pl.CreatedAt.Add(3*time.Second), landed.mut)
			if f := x.gather(t, x.event(pl, nil)); f.FixEpoch != 12 {
				t.Fatalf("fix epoch = %d, want 12: the pipeline ran on the fix's %s", f.FixEpoch, landed.name)
			}
			// A pipeline on any OTHER commit, created before merged_at, is not.
			x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
			other := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "failed", Ref: "main", SHA: shaA, Source: "push",
				CreatedAt: pl.CreatedAt.Add(time.Second)})
			x.gl.AddJob(x.pid, other.ID, glab.Job{Name: "unit", Stage: "test", Status: "failed", FailureReason: "script_failure"})
			if f := x.gather(t, x.event(other, nil)); f.FixEpoch != 0 {
				t.Fatalf("fix epoch = %d for a pipeline on another commit created before merged_at, want 0", f.FixEpoch)
			}
		})
	}
}

// A green pipeline after the fix starts a new run by itself; the epoch is
// dropped, so the anchor is the plain one again.
func TestAGreenAfterTheFixDropsTheEpoch(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	before := x.failedPipeline("main", shaA, "push", "unit")
	mid := x.failedPipeline("main", shaA, "push", "unit")
	x.mergedFix(12, "main", between(before, mid), nil)
	if f := x.gather(t, x.event(mid, nil)); f.FixEpoch != 12 || f.LastSuccess != 0 {
		t.Fatalf("no green yet: epoch %d last success %d, want 12 and 0", f.FixEpoch, f.LastSuccess)
	}
	green := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB})
	later := x.failedPipeline("main", shaA, "push", "unit")
	d := x.decide(t, x.event(later, nil))
	want, _ := pipelinectx.Anchor(x.pid, pipelinectx.RefTarget("main"), pipelinectx.Fingerprint(jobs("unit"), ""), green.ID, 0)
	if d.BeadAnchor != want || d.PipelineFixEpoch != 0 {
		t.Fatalf("after a green that followed the fix: anchor %s epoch %d, want %s and 0", d.BeadAnchor, d.PipelineFixEpoch, want)
	}
	// The green that ran ON the merge commit, created before merged_at was
	// stamped, ends the fix's epoch as well.
	y := newPLFixture(t)
	y.gl.AddCommit(y.pid, glab.Commit{ID: shaA})
	onFix := y.gl.AddPipeline(y.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaMergeC})
	y.mergedFix(12, "main", onFix.CreatedAt.Add(3*time.Second), nil)
	fail := y.failedPipeline("main", shaA, "push", "unit")
	if f := y.gather(t, y.event(fail, nil)); f.FixEpoch != 0 || f.LastSuccess != onFix.ID {
		t.Fatalf("green on the merge commit: epoch %d last success %d, want 0 and %d", f.FixEpoch, f.LastSuccess, onFix.ID)
	}
}

// The newest merged fix a pipeline comes after is its epoch, and a LATER
// merge never moves the anchor of an earlier pipeline (the identity of a
// given pipeline must be the same every time it is computed).
func TestFixEpochIsStableAcrossLaterMerges(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	p0 := x.failedPipeline("main", shaA, "push", "unit")
	p1 := x.failedPipeline("main", shaA, "push", "unit")
	x.mergedFix(12, "main", between(p0, p1), nil)
	first := x.decide(t, x.event(p1, nil))
	p2 := x.failedPipeline("main", shaA, "push", "unit")
	x.mergedFix(13, "main", between(p1, p2), func(mr *glab.MergeRequest) { mr.SourceBranch, mr.MergeCommitSHA, mr.SHA = fixBranch2, shaB, shaB })

	for i, want := range []struct {
		pl    glab.Pipeline
		epoch int64
	}{{p0, 0}, {p1, 12}, {p2, 13}} {
		d := x.decide(t, x.event(want.pl, nil))
		if d.PipelineFixEpoch != want.epoch {
			t.Fatalf("pipeline %d of 3: fix epoch %d, want %d", i+1, d.PipelineFixEpoch, want.epoch)
		}
	}
	if again := x.decide(t, x.event(p1, nil)); again.BeadAnchor != first.BeadAnchor || again.SessionKey != first.SessionKey {
		t.Fatalf("a later merged fix moved an earlier pipeline's identity: %s -> %s", first.BeadAnchor, again.BeadAnchor)
	}
}

// What is NOT a merged gonk fix into this target sets no epoch -- and the
// anchor is then byte-for-byte the anchor of a project with no MRs at all.
// Each row fails exactly one condition. Every row is run twice: against a
// server that honours the list filters and one that ignores them, because a
// filter that only works when the server cooperates is not a guard.
func TestOnlyABotAuthoredMergedFixIntoTheTargetSetsTheEpoch(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*glab.MergeRequest)
	}{
		{"authored by a human (anyone can name a branch gonk/pipefix-*)", func(mr *glab.MergeRequest) { mr.Author = humanUser }},
		{"no author at all", func(mr *glab.MergeRequest) { mr.Author = glab.User{} }},
		{"the bot's onboarding MR", func(mr *glab.MergeRequest) { mr.SourceBranch = OnboardBranch }},
		{"a branch that only starts like a fix branch", func(mr *glab.MergeRequest) { mr.SourceBranch = fixBranch + "-mine" }},
		{"still open", func(mr *glab.MergeRequest) { mr.State, mr.MergedAt = "opened", nil }},
		{"closed without merging", func(mr *glab.MergeRequest) { mr.State, mr.MergedAt = "closed", nil }},
		{"merged into another branch", func(mr *glab.MergeRequest) { mr.TargetBranch = "release" }},
		{"from a fork", func(mr *glab.MergeRequest) { mr.SourceProjectID = 9999 }},
		{"targeting another project", func(mr *glab.MergeRequest) { mr.TargetProjectID = 9999 }},
	}
	for _, tc := range cases {
		for _, ignore := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/server_ignores_filters=%v", tc.name, ignore), func(t *testing.T) {
				x := newPLFixture(t)
				x.gl.IgnoreMRListFilters = ignore
				x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
				p0 := x.failedPipeline("main", shaA, "push", "unit")
				pl := x.failedPipeline("main", shaA, "push", "unit")
				x.mergedFix(12, "main", between(p0, pl), tc.mut)
				d := x.decide(t, x.event(pl, nil))
				want, _ := pipelinectx.Anchor(x.pid, pipelinectx.RefTarget("main"), pipelinectx.Fingerprint(jobs("unit"), ""), 0, 0)
				if d.PipelineFixEpoch != 0 || d.BeadAnchor != want {
					t.Fatalf("anchor %s epoch %d, want the fix-less %s", d.BeadAnchor, d.PipelineFixEpoch, want)
				}
			})
		}
	}
	// The control: the unmutated MR does set it, under both servers.
	for _, ignore := range []bool{false, true} {
		x := newPLFixture(t)
		x.gl.IgnoreMRListFilters = ignore
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
		p0 := x.failedPipeline("main", shaA, "push", "unit")
		pl := x.failedPipeline("main", shaA, "push", "unit")
		x.mergedFix(12, "main", between(p0, pl), nil)
		if f := x.gather(t, x.event(pl, nil)); f.FixEpoch != 12 {
			t.Fatalf("control (server ignores filters=%v): fix epoch %d, want 12", ignore, f.FixEpoch)
		}
	}
}

// An MR target's fix merges into the MR's SOURCE branch. A branch pipeline
// names that branch itself; an MR-event pipeline's ref does not, so the MR is
// read -- but only when the project has a merged fix at all.
func TestFixEpochOnAnMRTarget(t *testing.T) {
	t.Run("branch pipeline with one open MR", func(t *testing.T) {
		x := newPLFixture(t)
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
		x.gl.AddMR(x.pid, glab.MergeRequest{IID: 5, SourceBranch: "feature", SHA: shaA})
		x.gl.SetCommitMRs(x.pid, shaA, 5)
		p0 := x.failedPipeline("feature", shaA, "push", "unit")
		pl := x.failedPipeline("feature", shaA, "push", "unit")
		x.mergedFix(12, "feature", between(p0, pl), nil)
		x.mergedFix(13, "main", between(p0, pl), func(mr *glab.MergeRequest) { mr.SourceBranch = fixBranch2 }) // another target's fix
		d := x.decide(t, x.event(pl, nil))
		if d.PipelineTarget != "mr:5" || d.PipelineFixEpoch != 12 || !strings.HasSuffix(d.BeadAnchor, ":f12") {
			t.Fatalf("decision = %+v, want mr:5 after fix !12", d)
		}
	})
	mrEvent := func(x *plFixture) (glab.Pipeline, glab.Pipeline, *ghook.Event) {
		x.gl.AddMR(x.pid, glab.MergeRequest{IID: 8, SourceBranch: "feature", SHA: shaA})
		p0 := x.failedPipeline("refs/merge-requests/8/head", shaA, "merge_request_event", "unit")
		pl := x.failedPipeline("refs/merge-requests/8/head", shaA, "merge_request_event", "unit")
		x.gl.AddMRPipeline(x.pid, 8, p0.ID)
		x.gl.AddMRPipeline(x.pid, 8, pl.ID)
		return p0, pl, x.event(pl, func(p *ghook.Pipeline) { p.MRIID, p.MRSourceProjectID = 8, x.pid })
	}
	t.Run("MR-event pipeline", func(t *testing.T) {
		x := newPLFixture(t)
		p0, pl, ev := mrEvent(x)
		x.mergedFix(12, "feature", between(p0, pl), nil)
		d := x.decide(t, ev)
		if d.PipelineTarget != "mr:8" || d.PipelineFixEpoch != 12 {
			t.Fatalf("decision = %+v, want mr:8 after fix !12", d)
		}
	})
	t.Run("MR-event pipeline, the only merged fix went into another branch", func(t *testing.T) {
		x := newPLFixture(t)
		p0, pl, ev := mrEvent(x)
		x.mergedFix(12, "main", between(p0, pl), nil)
		if d := x.decide(t, ev); d.PipelineFixEpoch != 0 {
			t.Fatalf("a fix merged into main set the epoch of an MR whose branch is feature: %+v", d)
		}
	})
	t.Run("MR-event pipeline, no merged fix: the MR is not read", func(t *testing.T) {
		x := newPLFixture(t)
		_, _, ev := mrEvent(x)
		if d := x.decide(t, ev); d.PipelineFixEpoch != 0 {
			t.Fatalf("decision = %+v", d)
		}
		for _, r := range x.gl.Requests() {
			if r == fmt.Sprintf("GET /api/v4/projects/%d/merge_requests/8", x.pid) {
				t.Fatalf("read the MR although the project has no merged fix: %v", x.gl.Requests())
			}
		}
	})
	t.Run("MR-event pipeline, the MR cannot be read", func(t *testing.T) {
		x := newPLFixture(t)
		p0, pl, ev := mrEvent(x)
		x.mergedFix(12, "feature", between(p0, pl), nil)
		if f := x.gather(t, ev); f.FixEpoch != 12 {
			t.Fatalf("healthy fixture: fix epoch %d, want 12", f.FixEpoch)
		}
		// ONLY the MR's own read fails (a path-prefix failure on the fake
		// would also fail the MR's pipelines and notes, and prove nothing
		// about this read).
		x.f.GL = &failingReads{PipelineGitLab: x.gl.Client(), failMR: true}
		if f, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), ev); err == nil {
			t.Fatalf("Gather hid an unreadable MR behind fix epoch %d", f.FixEpoch)
		}
	})
}

// On a target no fix has merged into, the epoch costs exactly ONE more GET
// (the bot's merged MRs) and nothing else is read for it.
func TestFixEpochCostsOneReadWithoutAFix(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB})
	pl := x.failedPipeline("main", shaA, "push", "unit")
	at := len(x.gl.Requests())
	x.gather(t, x.event(pl, nil))
	list, detail := 0, 0
	for _, r := range x.gl.Requests()[at:] {
		switch r {
		case fmt.Sprintf("GET /api/v4/projects/%d/merge_requests", x.pid):
			list++
		case fmt.Sprintf("GET /api/v4/projects/%d/pipelines/%d", x.pid, pl.ID):
			detail++
		}
	}
	if list != 1 || detail != 0 {
		t.Fatalf("merged-MR lists = %d (want 1), pipeline detail reads = %d (want 0): %v", list, detail, x.gl.Requests()[at:])
	}
}

// Unreadable is not "no fix": every read the epoch depends on failing is a
// Gather error, and Handle then records pipeline_facts_error and fires
// nothing (the backfill sweep retries).
func TestFixEpochReadErrorsFailClosed(t *testing.T) {
	setup := func(t *testing.T) (*plFixture, glab.Pipeline) {
		x := newPLFixture(t)
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
		p0 := x.failedPipeline("main", shaA, "push", "unit")
		pl := x.failedPipeline("main", shaA, "push", "unit")
		x.mergedFix(12, "main", between(p0, pl), nil)
		return x, pl
	}
	cases := []struct {
		name string
		do   func(x *plFixture, pl glab.Pipeline)
	}{
		{"the merged-MR list", func(x *plFixture, _ glab.Pipeline) {
			x.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/merge_requests", x.pid), http.StatusInternalServerError)
		}},
		// Only the pipeline DETAIL read fails: a path-prefix failure on the
		// fake would fail the jobs read first and prove nothing about this one.
		{"the pipeline's creation time", func(x *plFixture, _ glab.Pipeline) {
			x.f.GL = &failingReads{PipelineGitLab: x.gl.Client(), failPipeline: true}
		}},
		{"a merged fix with no merged_at", func(x *plFixture, _ glab.Pipeline) {
			x.mergedFix(13, "main", time.Time{}, func(mr *glab.MergeRequest) { mr.SourceBranch, mr.MergedAt = fixBranch2, nil })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, pl := setup(t)
			if f := x.gather(t, x.event(pl, nil)); f.FixEpoch != 12 {
				t.Fatalf("healthy fixture: fix epoch %d, want 12", f.FixEpoch)
			}
			tc.do(x, pl)
			if f, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), x.event(pl, nil)); err == nil {
				t.Fatalf("Gather answered fix epoch %d although %s could not be read", f.FixEpoch, tc.name)
			}
			meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Metadata: map[string]string{}}}
			disp := &recordingDispatcher{}
			d, o := pipelineDispatch(pipelinesEntry(x.pid), meter, disp, nil, x.f)
			if d.Handle(context.Background(), x.event(pl, nil)) || o.lastDropped != ReasonPipelineFactsError || len(meter.reqs) != 0 || len(disp.orders) != 0 {
				t.Fatalf("Handle: dropped=%q decide=%d orders=%d; want %s and nothing asked or fired",
					o.lastDropped, len(meter.reqs), len(disp.orders), ReasonPipelineFactsError)
			}
		})
	}
	if _, err := (&GitLabMergedFixes{GL: newPLFixture(t).gl.Client()}).MergedFixes(context.Background(), 1, "main"); err == nil {
		t.Fatal("MergedFixes ran with BotUserID 0: the authorship check would match nothing")
	}
}

// failingReads is the real client with exactly one read made to fail, so a
// test can show that THAT read is the one failing closed.
type failingReads struct {
	PipelineGitLab
	failMR, failPipeline bool
}

func (f *failingReads) GetMergeRequest(ctx context.Context, pid, iid int64) (*glab.MergeRequest, error) {
	if f.failMR {
		return nil, errors.New("merge request unreadable")
	}
	return f.PipelineGitLab.GetMergeRequest(ctx, pid, iid)
}

func (f *failingReads) GetPipeline(ctx context.Context, pid, id int64) (*glab.Pipeline, error) {
	if f.failPipeline {
		return nil, errors.New("pipeline unreadable")
	}
	return f.PipelineGitLab.GetPipeline(ctx, pid, id)
}

// fakeFixes is a MergedFixSource that needs no GitLab.
type fakeFixes struct {
	fixes []pipelinectx.MergedFix
	err   error
	asked []string // "<pid> <branch>"
}

func (f *fakeFixes) MergedFixes(_ context.Context, pid int64, branch string) ([]pipelinectx.MergedFix, error) {
	f.asked = append(f.asked, fmt.Sprintf("%d %s", pid, branch))
	return f.fixes, f.err
}

// The merged-fix facts come through the interface: a source that is not
// GitLab is asked instead (with the target's branch), and GitLab's MR list is
// then never read for it.
func TestFixEpochFromAnotherSource(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	pl := x.failedPipeline("main", shaA, "push", "unit")
	src := &fakeFixes{fixes: []pipelinectx.MergedFix{
		{IID: 31, TargetBranch: "main", MergedAt: pl.CreatedAt.Add(-time.Minute)},
		{IID: 32, TargetBranch: "release", MergedAt: pl.CreatedAt.Add(-time.Second)}, // another branch: re-checked
	}}
	x.f.Fixes = src
	if f := x.gather(t, x.event(pl, nil)); f.FixEpoch != 31 {
		t.Fatalf("fix epoch = %d, want 31 (from the source; the release fix is another target's)", f.FixEpoch)
	}
	if len(src.asked) != 1 || src.asked[0] != fmt.Sprintf("%d main", x.pid) {
		t.Fatalf("the source was asked %v, want once for this project's main", src.asked)
	}
	for _, r := range x.gl.Requests() {
		if r == fmt.Sprintf("GET /api/v4/projects/%d/merge_requests", x.pid) {
			t.Fatal("GitLab's MR list was read although another source is wired")
		}
	}
	src.fixes, src.err = nil, errors.New("fix records unreadable")
	if _, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), x.event(pl, nil)); err == nil {
		t.Fatal("Gather hid the source's error")
	}
}

// The pure decision: the epoch in the facts is the epoch in the anchor, the
// session key and the decision; an invalid one is an invalid identity.
func TestDecidePipelineFixEpoch(t *testing.T) {
	base := func(epoch int64) *PipelineFacts {
		return &PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LastSuccess: 10, FixEpoch: epoch}
	}
	plain := decidePipeline(pipelinesEntry(42), plEvent(42, nil), base(0))
	with := decidePipeline(pipelinesEntry(42), plEvent(42, nil), base(12))
	if !plain.Dispatch || !with.Dispatch {
		t.Fatalf("decisions = %+v / %+v", plain, with)
	}
	if e, err := pipelinectx.AnchorFixEpoch(plain.BeadAnchor); err != nil || e != 0 || plain.PipelineFixEpoch != 0 {
		t.Fatalf("no fix: anchor %s epoch %d", plain.BeadAnchor, plain.PipelineFixEpoch)
	}
	if with.BeadAnchor != plain.BeadAnchor+":f12" || with.PipelineFixEpoch != 12 || with.SessionKey == plain.SessionKey {
		t.Fatalf("fix !12: anchor %s epoch %d key %s (plain %s / %s)", with.BeadAnchor, with.PipelineFixEpoch, with.SessionKey, plain.BeadAnchor, plain.SessionKey)
	}
	if want, _ := pipelinectx.SessionKey(with.BeadAnchor); with.SessionKey != want {
		t.Fatalf("session key %s is not the key of anchor %s (%s)", with.SessionKey, with.BeadAnchor, want)
	}
	if d := decidePipeline(pipelinesEntry(42), plEvent(42, nil), base(-1)); d.Dispatch || d.Reason != ReasonPipelineIdentityInvalid {
		t.Fatalf("negative fix epoch: %+v, want %s", d, ReasonPipelineIdentityInvalid)
	}
	// The old run's marker does not answer for the new run.
	f := base(12)
	f.RunStates = nil
	if d := decidePipeline(pipelinesEntry(42), plEvent(42, nil), f); !d.Dispatch {
		t.Fatalf("the run after the fix was not dispatched: %+v", d)
	}
}

func pipelineOrderForGolden() OrderRequest {
	return OrderRequest{
		Trigger: atags.TriggerPipelineDiagnosis, Project: "group/repo", ProjectID: 42, Rig: "group-repo",
		SessionKey: "gonk-42-pl-0123456789ab", BeadAnchor: "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880",
		BeadID:     "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880",
		ConfigHash: "sha256:abc", Rung: "local-1", Model: "qwen-local",
		MetadataJSON:  json.RawMessage(`{"gonk_attempt":"1"}`),
		KeyRef:        meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		ReservationID: "res-1",
		PipelineID:    900, PipelineTarget: "ref:main", PipelineFingerprint: "0123456789abcdef", PipelineLastSuccess: 880,
	}
}

// A pipeline order for a run that follows no merged fix must put exactly the
// vars on the wire that it did before fix epochs existed: every gonk-gate
// already deployed reads them.
//
// testdata/order-vars-pipeline.golden.json was generated from the code BEFORE
// the field existed (origin/main 4da8e68, by a one-off write that is
// deliberately not kept). Never regenerate it to make this test pass.
func TestPipelineOrderVarsWithoutAFixUnchanged(t *testing.T) {
	vars, err := orderVars(pipelineOrderForGolden())
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/order-vars-pipeline.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("pipeline order vars without a fix changed:\n got: %s\nwant: %s", got, want)
	}

	// With a fix, exactly one var is added.
	o := pipelineOrderForGolden()
	o.PipelineFixEpoch = 12
	with, err := orderVars(o)
	if err != nil {
		t.Fatal(err)
	}
	if with["pipeline_fix_epoch"] != "12" || len(with) != len(vars)+1 {
		t.Fatalf("with fix !12: vars = %v, want the fix-less ones plus pipeline_fix_epoch=12", with)
	}
}

// The order Handle fires carries the epoch the decision carries, and the
// decision record names it -- only when there is one.
func TestHandleCarriesTheFixEpochOnTheOrderAndTheRecord(t *testing.T) {
	for _, epoch := range []int64{0, 12} {
		var log bytes.Buffer
		meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "local-2", Model: "m", ReservationID: "r1", Metadata: map[string]string{"gonk_attempt": "1"}}}
		disp := &recordingDispatcher{}
		facts := &stubFacts{f: &PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LastSuccess: 10, FixEpoch: epoch}}
		d, _ := pipelineDispatch(pipelinesEntry(42), meter, disp, nil, facts)
		d.Log = slog.New(slog.NewTextHandler(&log, nil))
		if !d.Handle(context.Background(), plEvent(42, nil)) || len(disp.orders) != 1 {
			t.Fatalf("epoch %d: not fired; orders %d\n%s", epoch, len(disp.orders), log.String())
		}
		o := disp.orders[0]
		want := decidePipeline(pipelinesEntry(42), plEvent(42, nil), facts.f)
		if o.PipelineFixEpoch != epoch || o.BeadAnchor != want.BeadAnchor || o.SessionKey != want.SessionKey {
			t.Fatalf("epoch %d: order = %+v, want epoch %d on anchor %s", epoch, o, epoch, want.BeadAnchor)
		}
		if meter.reqs[0].BeadID != want.BeadAnchor {
			t.Fatalf("epoch %d: meter was asked about %s, want %s", epoch, meter.reqs[0].BeadID, want.BeadAnchor)
		}
		named := strings.Contains(log.String(), "pipeline_fix_epoch=")
		if epoch == 0 && named {
			t.Fatalf("a run with no fix names a fix epoch in its record:\n%s", log.String())
		}
		if epoch != 0 && !strings.Contains(log.String(), "pipeline_fix_epoch=12") {
			t.Fatalf("the record of a run after fix !12 does not name it:\n%s", log.String())
		}
	}
}
