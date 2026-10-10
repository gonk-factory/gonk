package intake

import (
	"context"
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
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// gonk-tios P3 tests. Everything here is synthetic: project group/repo, made-up
// job names, made-up SHAs. Nothing encodes a real failure.

const (
	plBotID   = 7
	plHumanID = 9
	shaA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	botUser   = glab.User{ID: plBotID, Username: "gonk"}
	humanUser = glab.User{ID: plHumanID, Username: "human"}
)

// pipelinesEntry is validEntry with actions.pipelines ON (triage too).
func pipelinesEntry(pid int64) Entry {
	return Entry{
		Project: glab.Project{ID: pid, PathWithNamespace: "group/repo", DefaultBranch: "main"},
		Classification: Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
			r.Effective.Actions = meterapi.Actions{Triage: true, Pipelines: true}
		})),
		LastReconcile: time.Now(),
	}
}

// triageOnlyEntry: actions.triage on, actions.pipelines OFF -- a project that
// has not opted in.
func triageOnlyEntry(pid int64) Entry {
	e := pipelinesEntry(pid)
	e.Classification = Classify(obs(nil), active(nil))
	return e
}

func plEvent(pid int64, mut func(*ghook.Pipeline)) *ghook.Event {
	p := &ghook.Pipeline{ID: 900, IID: 4, Ref: "main", SHA: shaA, Source: "push", Status: "failed"}
	if mut != nil {
		mut(p)
	}
	return &ghook.Event{
		Kind:     ghook.KindPipeline,
		Project:  ghook.Project{ID: pid, PathWithNamespace: "group/repo", DefaultBranch: "main"},
		User:     ghook.User{ID: plHumanID, Username: "human"},
		Pipeline: p,
	}
}

func jobs(names ...string) []pipelinectx.FailedJob {
	var out []pipelinectx.FailedJob
	for _, n := range names {
		out = append(out, pipelinectx.FailedJob{Stage: "test", Name: n, FailureReason: "script_failure"})
	}
	return out
}

func lookedFacts(mrs ...glab.MergeRequest) *PipelineFacts {
	return &PipelineFacts{CommitMRsLooked: true, CommitMRs: mrs, FailedJobs: jobs("unit"), LastSuccess: 10}
}

func openMR(iid int64, branch string, srcProject int64) glab.MergeRequest {
	return glab.MergeRequest{IID: iid, State: "opened", SourceBranch: branch, SourceProjectID: srcProject}
}

// ---------------------------------------------------------------- pure decision

// Every drop reason decidePipeline can give, each from a case that passes
// every OTHER rule.
func TestDecidePipelineDropReasons(t *testing.T) {
	const pid = 42
	cases := []struct {
		name  string
		entry Entry
		ev    *ghook.Event
		facts *PipelineFacts
		want  string
	}{
		{"action off (not opted in)", triageOnlyEntry(pid), plEvent(pid, nil), lookedFacts(), "action_disabled"},
		{"success", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Status = "success" }), lookedFacts(), ReasonPipelineNotFailed},
		{"running", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Status = "running" }), lookedFacts(), ReasonPipelineNotFailed},
		{"canceled", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Status = "canceled" }), lookedFacts(), ReasonPipelineNotFailed},
		{"child", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Source = "parent_pipeline" }), lookedFacts(), ReasonChildPipelineUnsupported},
		{"fork MR", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) {
			p.Source, p.Ref, p.MRIID, p.MRSourceProjectID = "merge_request_event", "refs/merge-requests/8/head", 8, 99
		}), lookedFacts(), ReasonForkMRUnsupported},
		{"MR pipeline naming no MR", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) {
			p.Source, p.Ref = "merge_request_event", "refs/merge-requests/8/head"
		}), lookedFacts(), ReasonMRPipelineWithoutMR},
		{"tag", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Ref, p.Tag = "v1.0.0", true }), lookedFacts(), ReasonRefNotConfigured},
		// A tag whose NAME equals the default branch is still a tag: the
		// default-branch rule must not catch it.
		{"tag named like the default branch", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Tag = true }), lookedFacts(), ReasonRefNotConfigured},
		{"other branch, no MR", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Ref = "feature" }), lookedFacts(), ReasonRefNotConfigured},
		{"other branch, two open MRs", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Ref = "feature" }),
			lookedFacts(openMR(3, "feature", pid), openMR(4, "feature", pid)), ReasonRefNotConfigured},
		{"other branch, only a closed MR", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Ref = "feature" }),
			lookedFacts(glab.MergeRequest{IID: 3, State: "closed", SourceBranch: "feature", SourceProjectID: pid}), ReasonRefNotConfigured},
		{"other branch, only a fork MR from a same-named branch", pipelinesEntry(pid), plEvent(pid, func(p *ghook.Pipeline) { p.Ref = "feature" }),
			lookedFacts(openMR(3, "feature", 99)), ReasonRefNotConfigured},
		{"landing unreadable", pipelinesEntry(pid), plEvent(pid, nil),
			&PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LandingErr: errors.New("boom")}, ReasonLandingUnreadable},
		{"landing marker in an unknown state", pipelinesEntry(pid), plEvent(pid, nil),
			&PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), RunStates: []string{"paused"}}, ReasonLandingUnreadable},
		{"no pipeline object", pipelinesEntry(pid), &ghook.Event{Kind: ghook.KindPipeline, Project: ghook.Project{ID: pid}}, lookedFacts(), ReasonMalformedEvent},
		{"unsynced project", Entry{Project: glab.Project{ID: pid}, Classification: Classify(obs(nil), nil), LastReconcile: time.Now()},
			plEvent(pid, nil), lookedFacts(), "state_unsynced"},
	}
	for _, st := range []string{pipelinectx.RunStateDiagnosed, pipelinectx.RunStateDenied, pipelinectx.RunStateResolved, pipelinectx.RunStateSuperseded} {
		cases = append(cases, struct {
			name  string
			entry Entry
			ev    *ghook.Event
			facts *PipelineFacts
			want  string
		}{"already handled: " + st, pipelinesEntry(pid), plEvent(pid, nil),
			&PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), RunStates: []string{st}}, ReasonRunAlreadyHandled})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decidePipeline(tc.entry, tc.ev, tc.facts)
			if d.Dispatch || d.Reason != tc.want {
				t.Fatalf("decision = %+v, want a drop with reason %q", d, tc.want)
			}
		})
	}
}

// An in-flight or absent run proceeds (D3): the anchor dedupes it downstream.
func TestDecidePipelineProceeds(t *testing.T) {
	for _, states := range [][]string{nil, {pipelinectx.RunStateDiagnosing}, {pipelinectx.RunStateDeferred}} {
		f := &PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LastSuccess: 10, RunStates: states}
		d := decidePipeline(pipelinesEntry(42), plEvent(42, nil), f)
		if !d.Dispatch || d.Trigger != atags.TriggerPipelineDiagnosis {
			t.Fatalf("states %v: decision = %+v, want a pipeline-diagnosis dispatch", states, d)
		}
	}
}

// Every IO-free rule is applied BEFORE any fact is needed: without facts,
// a drop is still a drop, and only a pipeline that survives them asks for
// facts. Decide (the pure entry point) routes a pipeline event here.
func TestDecidePipelineWithoutFacts(t *testing.T) {
	if d := Decide(pipelinesEntry(42), plEvent(42, nil), "gonk"); d.Dispatch || d.Reason != ReasonPipelineFactsNeeded {
		t.Fatalf("default-branch failure without facts = %+v, want %q", d, ReasonPipelineFactsNeeded)
	}
	if d := Decide(pipelinesEntry(42), plEvent(42, func(p *ghook.Pipeline) { p.Status = "success" }), "gonk"); d.Reason != ReasonPipelineNotFailed {
		t.Fatalf("success without facts = %+v", d)
	}
	if d := Decide(triageOnlyEntry(42), plEvent(42, nil), "gonk"); d.Reason != "action_disabled" {
		t.Fatalf("not-opted-in project without facts = %+v, want action_disabled", d)
	}
	blocked := pipelinesEntry(42)
	blocked.Blocked = true
	if d := Decide(blocked, plEvent(42, nil), "gonk"); d.Reason != "blocked_project" {
		t.Fatalf("blocked project = %+v", d)
	}
}

// D1 targets: an MR-event pipeline and a branch pipeline with exactly one open
// same-project MR both land on mr:<iid>; a default-branch pipeline on
// ref:<default>.
func TestDecidePipelineTargets(t *testing.T) {
	const pid = 42
	cases := []struct {
		name  string
		ev    *ghook.Event
		facts *PipelineFacts
		want  string
	}{
		{"MR event", plEvent(pid, func(p *ghook.Pipeline) {
			p.Source, p.Ref, p.MRIID, p.MRSourceProjectID = "merge_request_event", "refs/merge-requests/8/head", 8, pid
		}), &PipelineFacts{FailedJobs: jobs("unit")}, "mr:8"},
		{"branch with one open MR", plEvent(pid, func(p *ghook.Pipeline) { p.Ref = "feature" }),
			lookedFacts(openMR(5, "feature", pid), glab.MergeRequest{IID: 6, State: "merged", SourceBranch: "feature", SourceProjectID: pid}), "mr:5"},
		{"default branch", plEvent(pid, nil), lookedFacts(), "ref:main"},
	}
	for _, tc := range cases {
		d := decidePipeline(pipelinesEntry(pid), tc.ev, tc.facts)
		if !d.Dispatch || d.PipelineTarget != tc.want {
			t.Fatalf("%s: decision = %+v, want a dispatch on %s", tc.name, d, tc.want)
		}
		key := strings.Replace(strings.TrimPrefix(tc.want, "mr:"), "ref:main", "", 1)
		if strings.HasPrefix(tc.want, "mr:") && !strings.Contains(d.BeadAnchor, ":pipeline:mr"+key+":") {
			t.Fatalf("%s: anchor %q does not key the MR", tc.name, d.BeadAnchor)
		}
		if sk, _ := pipelinectx.SessionKey(d.BeadAnchor); sk != d.SessionKey || sk == "" {
			t.Fatalf("%s: session key %q is not SessionKey(anchor)", tc.name, d.SessionKey)
		}
	}
}

// D2: same failing-job set + same last green -> SAME anchor (one run, one
// session); different jobs -> different; a green in between -> different.
func TestPipelineRunIdentity(t *testing.T) {
	e := pipelinesEntry(42)
	decide := func(plID int64, js []pipelinectx.FailedJob, lastSuccess int64) Decision {
		ev := plEvent(42, func(p *ghook.Pipeline) { p.ID = plID })
		d := decidePipeline(e, ev, &PipelineFacts{CommitMRsLooked: true, FailedJobs: js, LastSuccess: lastSuccess})
		if !d.Dispatch {
			t.Fatalf("decision = %+v", d)
		}
		return d
	}
	first := decide(900, jobs("unit", "lint"), 10)
	sameRun := decide(901, jobs("lint", "unit"), 10) // a later pipeline, jobs in another order
	if first.BeadAnchor != sameRun.BeadAnchor || first.SessionKey != sameRun.SessionKey {
		t.Fatalf("same failing jobs, same last green -> different runs:\n%s\n%s", first.BeadAnchor, sameRun.BeadAnchor)
	}
	if sameRun.PipelineID != 901 {
		t.Fatalf("the order must still name the pipeline the event was about: %d", sameRun.PipelineID)
	}
	if other := decide(902, jobs("unit"), 10); other.BeadAnchor == first.BeadAnchor {
		t.Fatal("a different failing-job set reused the run's anchor")
	}
	if afterGreen := decide(903, jobs("unit", "lint"), 950); afterGreen.BeadAnchor == first.BeadAnchor {
		t.Fatal("a failure after a green pipeline reused the earlier run's anchor")
	}
}

// The ghook fixtures, end to end through the pure decision: each synthetic
// payload lands on its D1 outcome. (bot-triggered never gets here: the
// receiver drops it bot_authored -- TestReceiverDropsBotTriggeredPipeline.)
func TestPipelineFixturesDecide(t *testing.T) {
	for fixture, want := range map[string]string{
		"pipeline-failed-default":  ReasonPipelineFactsNeeded, // survives every IO-free rule
		"pipeline-failed-mr-event": ReasonPipelineFactsNeeded,
		"pipeline-failed-branch":   ReasonPipelineFactsNeeded,
		"pipeline-success":         ReasonPipelineNotFailed,
		"pipeline-running":         ReasonPipelineNotFailed,
		"pipeline-child":           ReasonChildPipelineUnsupported,
		"pipeline-tag":             ReasonRefNotConfigured,
	} {
		body, err := os.ReadFile("../ghook/testdata/" + fixture + ".json")
		if err != nil {
			t.Fatal(err)
		}
		ev, err := ghook.ParseEvent("Pipeline Hook", body)
		if err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
		if d := Decide(pipelinesEntry(ev.Project.ID), ev, "gonk"); d.Dispatch || d.Reason != want {
			t.Errorf("%s: decision = %+v, want reason %q", fixture, d, want)
		}
	}
}

// ---------------------------------------------------------------- facts (IO)

type plFixture struct {
	gl   *glabtest.Server
	proj *glabtest.Project
	pid  int64
	f    *GitLabPipelineFacts
}

func newPLFixture(t *testing.T) *plFixture {
	t.Helper()
	gl := glabtest.New(t)
	gl.Me = botUser
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	return &plFixture{gl: gl, proj: p, pid: p.ID, f: &GitLabPipelineFacts{GL: gl.Client(), BotUserID: plBotID}}
}

// failedPipeline seeds a failed pipeline with one failed job per name.
func (x *plFixture) failedPipeline(ref, sha, source string, jobNames ...string) glab.Pipeline {
	pl := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "failed", Ref: ref, SHA: sha, Source: source, User: humanUser})
	for _, n := range jobNames {
		x.gl.AddJob(x.pid, pl.ID, glab.Job{Name: n, Stage: "test", Status: "failed", FailureReason: "script_failure"})
	}
	return pl
}

func (x *plFixture) event(pl glab.Pipeline, mut func(*ghook.Pipeline)) *ghook.Event {
	ev := plEvent(x.pid, func(p *ghook.Pipeline) {
		p.ID, p.IID, p.Ref, p.SHA, p.Source, p.Status = pl.ID, pl.IID, pl.Ref, pl.SHA, pl.Source, pl.Status
	})
	if mut != nil {
		mut(ev.Pipeline)
	}
	return ev
}

func (x *plFixture) decide(t *testing.T, ev *ghook.Event) Decision {
	t.Helper()
	facts, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), ev)
	if err != nil {
		t.Fatalf("Gather = %v", err)
	}
	return decidePipeline(pipelinesEntry(x.pid), ev, facts)
}

func (x *plFixture) marker(t *testing.T, anchor, state string) string {
	t.Helper()
	m, err := pipelinectx.RunMarker(anchor, state)
	if err != nil {
		t.Fatal(err)
	}
	return "rendered run note\n\n" + m
}

// The last green on a REF is the newest status=success pipeline on that ref;
// it enters the anchor.
func TestGatherRefLastSuccessAndJobs(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB})
	green := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB})
	x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "other", SHA: shaB}) // other ref: ignored
	pl := x.failedPipeline("main", shaA, "push", "unit")
	x.gl.AddJob(x.pid, pl.ID, glab.Job{Name: "optional", Stage: "test", Status: "failed", AllowFailure: true})

	d := x.decide(t, x.event(pl, nil))
	if !d.Dispatch || d.PipelineTarget != "ref:main" || d.PipelineLastSuccess != green.ID {
		t.Fatalf("decision = %+v, want ref:main with last success %d", d, green.ID)
	}
	if want := pipelinectx.Fingerprint(jobs("unit"), ""); d.PipelineFingerprint != want {
		t.Fatalf("fingerprint = %s, want %s (allow_failure job excluded)", d.PipelineFingerprint, want)
	}
}

// A pipeline that failed with no counting job uses the pipeline's own
// failure_reason, which only the detail call returns.
func TestGatherZeroJobsUsesPipelineFailureReason(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	pl := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "failed", Ref: "main", SHA: shaA, Source: "push", FailureReason: "config_error"})
	d := x.decide(t, x.event(pl, nil))
	if want := pipelinectx.Fingerprint(nil, "config_error"); !d.Dispatch || d.PipelineFingerprint != want {
		t.Fatalf("decision = %+v, want fingerprint %s", d, want)
	}
}

// The MR target's last green is the newest success among the MR's pipelines.
// An MR-event pipeline needs no commit lookup.
func TestGatherMRLastSuccessAndNoCommitLookup(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddMR(x.pid, glab.MergeRequest{IID: 8, SourceBranch: "feature", SHA: shaA})
	green := x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "refs/merge-requests/8/head", SHA: shaB, Source: "merge_request_event"})
	x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: "main", SHA: shaB}) // not the MR's
	pl := x.failedPipeline("refs/merge-requests/8/head", shaA, "merge_request_event", "unit")
	x.gl.AddMRPipeline(x.pid, 8, green.ID)
	x.gl.AddMRPipeline(x.pid, 8, pl.ID)

	d := x.decide(t, x.event(pl, func(p *ghook.Pipeline) { p.MRIID, p.MRSourceProjectID = 8, x.pid }))
	if !d.Dispatch || d.PipelineTarget != "mr:8" || d.PipelineLastSuccess != green.ID {
		t.Fatalf("decision = %+v, want mr:8 with last success %d", d, green.ID)
	}
	for _, r := range x.gl.Requests() {
		if strings.Contains(r, "/repository/commits/") {
			t.Fatalf("an MR-event pipeline looked up its commit's MRs: %s", r)
		}
	}
}

// A branch pipeline whose SHA has exactly one open same-project MR from that
// branch is that MR's (D1, via the commit's MRs).
func TestGatherBranchPipelineWithOneOpenMR(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	x.gl.AddMR(x.pid, glab.MergeRequest{IID: 5, SourceBranch: "feature", SHA: shaA})
	x.gl.SetCommitMRs(x.pid, shaA, 5)
	pl := x.failedPipeline("feature", shaA, "push", "unit")
	if d := x.decide(t, x.event(pl, nil)); !d.Dispatch || d.PipelineTarget != "mr:5" {
		t.Fatalf("decision = %+v, want mr:5", d)
	}
}

// D3 on the MR landing: only a BOT-AUTHORED, non-system note's marker counts.
// A human pasting `state=diagnosed` must not be able to switch gonk off for a
// run, and a system note is never the bot's.
func TestGatherMRLandingMarkers(t *testing.T) {
	setup := func(t *testing.T) (*plFixture, *ghook.Event, string) {
		x := newPLFixture(t)
		x.gl.AddMR(x.pid, glab.MergeRequest{IID: 8, SourceBranch: "feature", SHA: shaA})
		pl := x.failedPipeline("refs/merge-requests/8/head", shaA, "merge_request_event", "unit")
		x.gl.AddMRPipeline(x.pid, 8, pl.ID)
		ev := x.event(pl, func(p *ghook.Pipeline) { p.MRIID, p.MRSourceProjectID = 8, x.pid })
		d := x.decide(t, ev)
		if !d.Dispatch {
			t.Fatalf("fresh run did not dispatch: %+v", d)
		}
		return x, ev, d.BeadAnchor
	}
	for _, st := range []string{pipelinectx.RunStateDiagnosed, pipelinectx.RunStateDenied, pipelinectx.RunStateResolved, pipelinectx.RunStateSuperseded} {
		t.Run("bot "+st, func(t *testing.T) {
			x, ev, anchor := setup(t)
			x.gl.AddMRNote(x.pid, 8, botUser, x.marker(t, anchor, st), false)
			if d := x.decide(t, ev); d.Dispatch || d.Reason != ReasonRunAlreadyHandled {
				t.Fatalf("decision = %+v, want %s", d, ReasonRunAlreadyHandled)
			}
		})
	}
	t.Run("human-authored marker ignored", func(t *testing.T) {
		x, ev, anchor := setup(t)
		x.gl.AddMRNote(x.pid, 8, humanUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosed), false)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("a human's marker counted: %+v", d)
		}
	})
	t.Run("system note ignored", func(t *testing.T) {
		x, ev, anchor := setup(t)
		x.gl.AddMRNote(x.pid, 8, botUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosed), true)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("a system note's marker counted: %+v", d)
		}
	})
	t.Run("bot marker for another run ignored", func(t *testing.T) {
		x, ev, _ := setup(t)
		other, _ := pipelinectx.Anchor(x.pid, pipelinectx.MRTarget(8), "0123456789abcdef", 0, 0)
		x.gl.AddMRNote(x.pid, 8, botUser, x.marker(t, other, pipelinectx.RunStateDiagnosed), false)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("another run's marker counted: %+v", d)
		}
	})
	t.Run("in-flight diagnosing proceeds", func(t *testing.T) {
		x, ev, anchor := setup(t)
		x.gl.AddMRNote(x.pid, 8, botUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosing), false)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("diagnosing must proceed (the anchor dedupes downstream): %+v", d)
		}
	})
	t.Run("unreadable notes fail closed", func(t *testing.T) {
		x, ev, _ := setup(t)
		x.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/merge_requests/8/notes", x.pid), http.StatusInternalServerError)
		d := x.decide(t, ev)
		if d.Dispatch || d.Reason != ReasonLandingUnreadable {
			t.Fatalf("decision = %+v, want %s", d, ReasonLandingUnreadable)
		}
		if d.BeadAnchor == "" {
			t.Fatal("the landing_unreadable drop must still name the run")
		}
	})
}

// D3 on the REF landing: the bot-authored tracking issue for the ref (by its
// description marker), in ANY state -- a human closing it does not reset the
// run (D2). A human-authored issue carrying the same markers is ignored.
func TestGatherRefLandingMarkers(t *testing.T) {
	setup := func(t *testing.T) (*plFixture, *ghook.Event, string) {
		x := newPLFixture(t)
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
		pl := x.failedPipeline("main", shaA, "push", "unit")
		ev := x.event(pl, nil)
		d := x.decide(t, ev)
		if !d.Dispatch {
			t.Fatalf("fresh run did not dispatch: %+v", d)
		}
		return x, ev, d.BeadAnchor
	}
	addIssue := func(x *plFixture, iid int64, author glab.User, state, desc string) {
		x.gl.AddIssue(x.pid, iid, state, "gonk::pipeline-failing")
		x.setIssue(iid, author, desc)
	}
	t.Run("bot issue, closed by a human, diagnosed note", func(t *testing.T) {
		x, ev, anchor := setup(t)
		addIssue(x, 21, botUser, "closed", pipelinectx.RefIssueMarker("main")+"\nstatus table")
		x.gl.AddNote(x.pid, 21, botUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosed), false)
		if d := x.decide(t, ev); d.Dispatch || d.Reason != ReasonRunAlreadyHandled {
			t.Fatalf("decision = %+v, want %s", d, ReasonRunAlreadyHandled)
		}
	})
	t.Run("human-authored issue ignored", func(t *testing.T) {
		x, ev, anchor := setup(t)
		addIssue(x, 22, humanUser, "opened", pipelinectx.RefIssueMarker("main"))
		x.gl.AddNote(x.pid, 22, botUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosed), false)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("a human-authored issue spoke for the bot: %+v", d)
		}
	})
	t.Run("bot issue for another ref ignored", func(t *testing.T) {
		x, ev, anchor := setup(t)
		addIssue(x, 23, botUser, "opened", pipelinectx.RefIssueMarker("release"))
		x.gl.AddNote(x.pid, 23, botUser, x.marker(t, anchor, pipelinectx.RunStateDiagnosed), false)
		if d := x.decide(t, ev); !d.Dispatch {
			t.Fatalf("another ref's issue counted: %+v", d)
		}
	})
	t.Run("unreadable issues fail closed", func(t *testing.T) {
		x, ev, _ := setup(t)
		x.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/issues", x.pid), http.StatusBadGateway)
		if d := x.decide(t, ev); d.Dispatch || d.Reason != ReasonLandingUnreadable {
			t.Fatalf("decision = %+v, want %s", d, ReasonLandingUnreadable)
		}
	})
	// B23 (verify report): the tracking issue is found, but ITS NOTES cannot be
	// read. That is not "no marker": the run may already be diagnosed there.
	t.Run("unreadable tracking-issue notes fail closed", func(t *testing.T) {
		x, ev, _ := setup(t)
		addIssue(x, 24, botUser, "opened", pipelinectx.RefIssueMarker("main")+"\nstatus table")
		x.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/issues/24/notes", x.pid), http.StatusBadGateway)
		d := x.decide(t, ev)
		if d.Dispatch || d.Reason != ReasonLandingUnreadable {
			t.Fatalf("decision = %+v, want %s", d, ReasonLandingUnreadable)
		}
		if d.BeadAnchor == "" {
			t.Fatal("the landing_unreadable drop must still name the run")
		}
	})
}

// setIssue fills the author and description of a seeded glabtest issue.
func (x *plFixture) setIssue(iid int64, author glab.User, desc string) {
	p := x.proj
	for i := range p.Issues {
		if p.Issues[i].IID == iid {
			p.Issues[i].Author, p.Issues[i].Description = author, desc
		}
	}
}

// A read the IDENTITY depends on failing is an error, not a guess: Gather
// returns it and Handle records pipeline_facts_error and fires nothing.
func TestGatherIdentityReadErrors(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	pl := x.failedPipeline("main", shaA, "push", "unit")
	x.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/pipelines/%d/jobs", x.pid, pl.ID), http.StatusInternalServerError)
	if _, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), x.event(pl, nil)); err == nil {
		t.Fatal("Gather hid a failed jobs read")
	}
}

// BotUserID 0 would make the bot-authorship filter match nothing, so every run
// would read as unhandled: Gather refuses to run (on an otherwise healthy
// fixture, so nothing else can be what fails).
func TestGatherRefusesZeroBotUserID(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	pl := x.failedPipeline("main", shaA, "push", "unit")
	if _, err := x.f.Gather(context.Background(), pipelinesEntry(x.pid), x.event(pl, nil)); err != nil {
		t.Fatalf("healthy fixture: %v", err)
	}
	// It refuses BEFORE reading anything. (The fix epoch's own source refuses
	// a zero id too, gonk-2usw EPOCH -- but only after the jobs and the last
	// success were read, so "an error came back" alone no longer shows that
	// this guard is the one that fired.)
	at := len(x.gl.Requests())
	if _, err := (&GitLabPipelineFacts{GL: x.gl.Client()}).Gather(context.Background(), pipelinesEntry(x.pid), x.event(pl, nil)); err == nil {
		t.Fatal("Gather ran with BotUserID 0: the bot-authorship filter would match nothing")
	}
	if got := x.gl.Requests()[at:]; len(got) != 0 {
		t.Fatalf("Gather read GitLab with BotUserID 0 before refusing: %v", got)
	}
}

// ---------------------------------------------------------------- Handle

type stubFacts struct {
	f     *PipelineFacts
	err   error
	calls int
}

func (s *stubFacts) Gather(context.Context, Entry, *ghook.Event) (*PipelineFacts, error) {
	s.calls++
	return s.f, s.err
}

func pipelineDispatch(e Entry, m DecideClient, disp Dispatcher, lbl DenyLabeler, facts PipelineFactSource) (*Dispatch, *countingObserver) {
	cache := NewCache()
	cache.Put(e.Project.ID, e)
	o := &countingObserver{}
	return &Dispatch{
		Dispatcher: disp, Meter: m, Labeler: lbl, Cache: cache, Pipelines: facts,
		BotUsername: "gonk", Obs: o, Log: slog.New(slog.DiscardHandler), KickReconcile: func() {},
	}, o
}

// F4: Handle used ev.Issue.IID unconditionally, so a pipeline event would
// nil-deref on the run path and on the deny path. Every meter answer, with a
// labeler wired, ev.Issue == nil: no panic, no label, and on run an order
// carrying the run identity and no issue iid.
func TestHandlePipelineEventNeverReadsIssue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		resp  *meterapi.DecideResponse
		err   error
		fired bool
	}{
		{"run", &meterapi.DecideResponse{Decision: "run", Rung: "local-2", Model: "m", ReservationID: "r1", Metadata: map[string]string{"gonk_attempt": "1"}}, nil, true},
		{"defer", &meterapi.DecideResponse{Decision: "defer", Reason: "quiet-hours", RetryAfter: time.Now().Add(time.Hour)}, nil, false},
		{"deny", &meterapi.DecideResponse{Decision: "deny", Reason: "action-not-allowed"}, nil, false},
		{"unknown", &meterapi.DecideResponse{Decision: "maybe"}, nil, false},
		{"decide error", nil, errors.New("meter down"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter := &fakeDecide{resp: tc.resp, err: tc.err}
			disp, lbl := &recordingDispatcher{}, &recordingLabeler{}
			facts := &stubFacts{f: &PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LastSuccess: 10}}
			d, _ := pipelineDispatch(pipelinesEntry(42), meter, disp, lbl, facts)
			ev := plEvent(42, nil)
			if ev.Issue != nil {
				t.Fatal("test event must have no issue")
			}
			if got := d.Handle(context.Background(), ev); got != tc.fired {
				t.Fatalf("Handle = %v, want %v", got, tc.fired)
			}
			if lbl.calls != 0 {
				t.Fatalf("Gate-1 wrote a label on a pipeline (D9: intake writes nothing): %+v", lbl)
			}
			if len(meter.reqs) != 1 || meter.reqs[0].Trigger != atags.TriggerPipelineDiagnosis {
				t.Fatalf("decide requests = %+v", meter.reqs)
			}
			want := decidePipeline(pipelinesEntry(42), ev, facts.f)
			if meter.reqs[0].BeadID != want.BeadAnchor || meter.reqs[0].SessionKey != want.SessionKey {
				t.Fatalf("decide keyed on %q/%q, want the run anchor %q/%q", meter.reqs[0].BeadID, meter.reqs[0].SessionKey, want.BeadAnchor, want.SessionKey)
			}
			if !tc.fired {
				if len(disp.orders) != 0 {
					t.Fatalf("fired %d orders", len(disp.orders))
				}
				return
			}
			o := disp.orders[0]
			if o.IssueIID != 0 || o.PipelineID != 900 || o.PipelineTarget != "ref:main" ||
				o.PipelineFingerprint != want.PipelineFingerprint || o.PipelineLastSuccess != 10 ||
				o.BeadAnchor != want.BeadAnchor || o.BeadID != want.BeadAnchor || o.Trigger != atags.TriggerPipelineDiagnosis {
				t.Fatalf("order = %+v", o)
			}
			vars, err := orderVars(o)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := vars["issue_iid"]; ok {
				t.Fatalf("a pipeline order carries issue_iid: %v", vars)
			}
			for _, k := range []string{"pipeline_id", "pipeline_target", "pipeline_fingerprint", "pipeline_last_success"} {
				if vars[k] == "" {
					t.Fatalf("order vars miss %s: %v", k, vars)
				}
			}
		})
	}
}

// A project that has NOT opted in: the pipeline event is dropped
// action_disabled before ANY GitLab read and before meter is asked.
func TestHandleDropsPipelineForProjectNotOptedIn(t *testing.T) {
	meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run"}}
	disp := &recordingDispatcher{}
	facts := &stubFacts{f: &PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit")}}
	d, o := pipelineDispatch(triageOnlyEntry(42), meter, disp, nil, facts)
	if d.Handle(context.Background(), plEvent(42, nil)) {
		t.Fatal("fired for a project without actions.pipelines")
	}
	if o.lastDropped != "action_disabled" {
		t.Fatalf("dropped %q, want action_disabled", o.lastDropped)
	}
	if facts.calls != 0 || len(meter.reqs) != 0 || len(disp.orders) != 0 {
		t.Fatalf("gather=%d decide=%d orders=%d; want all zero", facts.calls, len(meter.reqs), len(disp.orders))
	}
}

// Already handled / unreadable: dropped by name, meter never asked.
func TestHandlePipelineAlreadyHandledAndUnreadable(t *testing.T) {
	for _, tc := range []struct {
		facts *PipelineFacts
		err   error
		want  string
	}{
		{&PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), RunStates: []string{pipelinectx.RunStateDiagnosed}}, nil, ReasonRunAlreadyHandled},
		{&PipelineFacts{CommitMRsLooked: true, FailedJobs: jobs("unit"), LandingErr: errors.New("502")}, nil, ReasonLandingUnreadable},
		{nil, errors.New("jobs read failed"), ReasonPipelineFactsError},
	} {
		meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run"}}
		disp := &recordingDispatcher{}
		d, o := pipelineDispatch(pipelinesEntry(42), meter, disp, nil, &stubFacts{f: tc.facts, err: tc.err})
		if d.Handle(context.Background(), plEvent(42, nil)) || o.lastDropped != tc.want || len(meter.reqs) != 0 || len(disp.orders) != 0 {
			t.Fatalf("want drop %q with no decide/fire; got dropped=%q decide=%d orders=%d", tc.want, o.lastDropped, len(meter.reqs), len(disp.orders))
		}
	}
	// No fact source wired: fail closed, never fire.
	meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run"}}
	d, o := pipelineDispatch(pipelinesEntry(42), meter, &recordingDispatcher{}, nil, nil)
	if d.Handle(context.Background(), plEvent(42, nil)) || o.lastDropped != ReasonPipelineFactsError || len(meter.reqs) != 0 {
		t.Fatalf("nil Pipelines: dropped=%q decide=%d", o.lastDropped, len(meter.reqs))
	}
}

// Two failures of the same run through the REAL fact source compute the same
// anchor, so meter sees one bead (and hands back its open reservation).
func TestHandleSameRunSameAnchorThroughGitLab(t *testing.T) {
	x := newPLFixture(t)
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
	x.gl.AddCommit(x.pid, glab.Commit{ID: shaB})
	p1 := x.failedPipeline("main", shaA, "push", "unit", "lint")
	p2 := x.failedPipeline("main", shaB, "push", "lint", "unit")
	meter := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Metadata: map[string]string{}}}
	disp := &recordingDispatcher{}
	d, _ := pipelineDispatch(pipelinesEntry(x.pid), meter, disp, nil, x.f)
	if !d.Handle(context.Background(), x.event(p1, nil)) || !d.Handle(context.Background(), x.event(p2, nil)) {
		t.Fatal("both failures should reach Gate 1 (nothing has landed yet)")
	}
	if len(meter.reqs) != 2 || meter.reqs[0].BeadID != meter.reqs[1].BeadID {
		t.Fatalf("same run, different beads: %+v", meter.reqs)
	}
	if disp.orders[0].PipelineID != p1.ID || disp.orders[1].PipelineID != p2.ID {
		t.Fatalf("orders must name their own pipelines: %d %d", disp.orders[0].PipelineID, disp.orders[1].PipelineID)
	}
}

// The issue path is untouched by the per-trigger gate: triage on, pipelines
// off still triages; and a pipelines-only project (triage OFF) is refused
// issue triage, as before.
func TestGateIsPerTrigger(t *testing.T) {
	if d := Decide(triageOnlyEntry(42), issueEvent("open"), "gonk"); !d.Dispatch {
		t.Fatalf("triage-only project no longer triages: %+v", d)
	}
	pipelinesOnly := pipelinesEntry(42)
	pipelinesOnly.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions = meterapi.Actions{Pipelines: true}
	}))
	if d := Decide(pipelinesOnly, issueEvent("open"), "gonk"); d.Dispatch || d.Reason != "action_disabled" {
		t.Fatalf("pipelines-only project triaged an issue: %+v", d)
	}
	if d := Decide(pipelinesOnly, plEvent(42, nil), "gonk"); d.Reason != ReasonPipelineFactsNeeded {
		t.Fatalf("pipelines-only project refused a pipeline (it must not need actions.triage): %+v", d)
	}
	if _, ok := gate(pipelinesEntry(42), "no-such-trigger"); ok {
		t.Fatal("gate allowed an unknown trigger")
	}
}

// Issue/note events with their kind-specific object missing are dropped by
// name, not dereferenced.
func TestDecideGuardsMissingObjects(t *testing.T) {
	e := validEntry()
	if d := Decide(e, &ghook.Event{Kind: ghook.KindIssue, Project: ghook.Project{ID: 42}}, "gonk"); d.Reason != ReasonMalformedEvent {
		t.Fatalf("issue event without issue: %+v", d)
	}
	if d := Decide(e, &ghook.Event{Kind: ghook.KindNote, Project: ghook.Project{ID: 42}}, "gonk"); d.Dispatch {
		t.Fatalf("note event without note: %+v", d)
	}
}

// R3 (verify report): a green CHILD pipeline (source parent_pipeline) is not
// a green of the target. A red parent, then a green child on the same ref,
// then the same red again is ONE run: the same anchor, so no second session.
// Run maintenance and the backfill already pass child pipelines over; the
// intake last-success lookup must agree with them.
func TestGatherAGreenChildPipelineDoesNotEndTheRun(t *testing.T) {
	for _, mr := range []bool{false, true} {
		x := newPLFixture(t)
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaA})
		x.gl.AddCommit(x.pid, glab.Commit{ID: shaB})
		ref, source := "main", "push"
		mut := func(*ghook.Pipeline) {}
		if mr {
			ref, source = "refs/merge-requests/8/head", "merge_request_event"
			x.gl.AddMR(x.pid, glab.MergeRequest{IID: 8, SourceBranch: "feature", SHA: shaA})
			mut = func(p *ghook.Pipeline) { p.MRIID, p.MRSourceProjectID = 8, x.pid }
		}
		onMR := func(pl glab.Pipeline) glab.Pipeline {
			if mr {
				x.gl.AddMRPipeline(x.pid, 8, pl.ID)
			}
			return pl
		}
		green := onMR(x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: ref, SHA: shaB, Source: source}))
		p1 := onMR(x.failedPipeline(ref, shaA, source, "unit"))
		d1 := x.decide(t, x.event(p1, mut))
		child := onMR(x.gl.AddPipeline(x.pid, glab.Pipeline{Status: "success", Ref: ref, SHA: shaA, Source: "parent_pipeline"}))
		p2 := onMR(x.failedPipeline(ref, shaB, source, "unit"))
		d2 := x.decide(t, x.event(p2, mut))
		if !d1.Dispatch || d1.PipelineLastSuccess != green.ID {
			t.Fatalf("mr=%v: first failure = %+v, want last success %d", mr, d1, green.ID)
		}
		if d2.PipelineLastSuccess != green.ID || d2.BeadAnchor != d1.BeadAnchor || d2.SessionKey != d1.SessionKey {
			t.Fatalf("mr=%v: a green child pipeline (%d) split the run: last success %d -> %d, anchor %s -> %s",
				mr, child.ID, d1.PipelineLastSuccess, d2.PipelineLastSuccess, d1.BeadAnchor, d2.BeadAnchor)
		}
	}
}
