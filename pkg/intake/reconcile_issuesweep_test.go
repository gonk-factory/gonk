package intake

import (
	"context"
	"errors"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// sweepGL is a GitLab fake for the issue sweep. Only ListIssues is meaningful;
// every other method panics, because sweepIssues must not call them.
type sweepGL struct {
	issues []glab.Issue
	err    error
	calls  int
	gotOpt glab.IssueListOptions
}

func (g *sweepGL) ListIssues(_ context.Context, _ int64, o glab.IssueListOptions) ([]glab.Issue, error) {
	g.calls++
	g.gotOpt = o
	return g.issues, g.err
}

func (g *sweepGL) ListMemberProjects(context.Context) ([]glab.Project, error) { panic("not used") }
func (g *sweepGL) GetRawFile(context.Context, int64, string, string, int64) ([]byte, error) {
	panic("not used")
}
func (g *sweepGL) DirExists(context.Context, int64, string, string) (bool, error) { panic("not used") }
func (g *sweepGL) ListHooks(context.Context, int64) ([]glab.Hook, error)          { panic("not used") }
func (g *sweepGL) CreateHook(context.Context, int64, glab.HookOptions) (*glab.Hook, error) {
	panic("not used")
}
func (g *sweepGL) EditHook(context.Context, int64, int64, glab.HookOptions) (*glab.Hook, error) {
	panic("not used")
}
func (g *sweepGL) ListMergeRequests(context.Context, int64, glab.MRListOptions) ([]glab.MergeRequest, error) {
	panic("not used")
}
func (g *sweepGL) ListMembers(context.Context, int64) ([]glab.Member, error) { panic("not used") }

// countingSweeper records every event the sweep hands to Dispatch.
type countingSweeper struct{ got []*ghook.Event }

func (c *countingSweeper) Handle(_ context.Context, ev *ghook.Event) { c.got = append(c.got, ev) }

func sweepReconciler(gl GitLab, s IssueSweeper) (*Reconciler, glab.Project) {
	e := validEntry()
	c := NewCache()
	c.Put(e.Project.ID, e)
	return &Reconciler{GL: gl, Cache: c, Issues: s, BotUserID: 7}, e.Project
}

// THE PROPERTY, and the reason this bead exists: an issue that was delivered,
// ACKed 200 and then lost is picked up on the next pass -- and once the broker
// has labelled it, it is left alone. Measured failure it closes: issues 65 and
// 67 on project 75, 2026-09-07, both delivered 200 and never triaged.
func TestSweepPicksUpAnUntriagedIssueThenLeavesItAlone(t *testing.T) {
	gl := &sweepGL{issues: []glab.Issue{
		{IID: 65, Title: "lost one", Description: "body", Author: glab.User{ID: 9}},
	}}
	sw := &countingSweeper{}
	r, p := sweepReconciler(gl, sw)

	n, err := r.sweepIssues(context.Background(), p)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(sw.got) != 1 {
		t.Fatalf("swept %d, handed %d events; want 1 and 1", n, len(sw.got))
	}

	// The broker labels what it triaged. The next pass must not re-fire.
	gl.issues[0].Labels = []string{"gonk::verdict-reply-only"}
	n, err = r.sweepIssues(context.Background(), p)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d on the second pass; a labelled issue is already triaged", n)
	}
	if len(sw.got) != 1 {
		t.Errorf("dispatched %d times across two passes, want exactly 1", len(sw.got))
	}
}

// The synthesized event must be the one Decide would have seen from a webhook,
// or the sweep silently takes a different path through the dispatch rules.
func TestSweepSynthesizesAFaithfulIssueEvent(t *testing.T) {
	gl := &sweepGL{issues: []glab.Issue{
		{IID: 91, Title: "t", Description: "d", Author: glab.User{ID: 9}},
	}}
	sw := &countingSweeper{}
	r, p := sweepReconciler(gl, sw)

	if _, err := r.sweepIssues(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(sw.got) != 1 {
		t.Fatalf("handed %d events, want 1", len(sw.got))
	}
	ev := sw.got[0]
	if ev.Kind != ghook.KindIssue {
		t.Errorf("kind = %q, want %q", ev.Kind, ghook.KindIssue)
	}
	if ev.Issue == nil {
		t.Fatal("event carries no issue")
	}
	// "open" is the ONLY action Decide dispatches on; anything else silently
	// becomes not_a_trigger and the sweep would do nothing at all.
	if ev.Issue.Action != "open" {
		t.Errorf("action = %q, want \"open\" (Decide dispatches on open/reopen only)", ev.Issue.Action)
	}
	if ev.Issue.IID != 91 || ev.Issue.Title != "t" || ev.Issue.Description != "d" {
		t.Errorf("issue not carried through: %+v", ev.Issue)
	}
	// Project comes from the CACHE, which is what Handle looks the entry up by
	// and what carries DefaultBranch -- the listing does not.
	if ev.Project.ID != p.ID || ev.Project.PathWithNamespace != p.PathWithNamespace {
		t.Errorf("project = %+v, want id %d / %q", ev.Project, p.ID, p.PathWithNamespace)
	}
	if o := gl.gotOpt.State; o != "opened" {
		t.Errorf("listed state = %q, want \"opened\": a closed issue must not be re-triaged", o)
	}
}

// The webhook path refuses bot-authored events (the infinite-loop guard). A
// swept issue has no event to carry a user, so the sweep must apply the same
// rule from the issue's author or it reopens the loop by the other door.
func TestSweepNeverTriagesAnIssueTheBotOpened(t *testing.T) {
	gl := &sweepGL{issues: []glab.Issue{
		{IID: 1, Title: "by the bot", Author: glab.User{ID: 7}},
		{IID: 2, Title: "by a human", Author: glab.User{ID: 9}},
	}}
	sw := &countingSweeper{}
	r, p := sweepReconciler(gl, sw)

	n, err := r.sweepIssues(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1 (the bot's own issue must be skipped)", n)
	}
	if sw.got[0].Issue.IID != 2 {
		t.Errorf("swept issue %d; the bot-authored one must never be triaged", sw.got[0].Issue.IID)
	}
}

func TestSweepIsSkippedEntirelyWhenItCouldNotDispatch(t *testing.T) {
	t.Run("no sweeper wired", func(t *testing.T) {
		gl := &sweepGL{}
		r, p := sweepReconciler(gl, nil)
		if n, err := r.sweepIssues(context.Background(), p); n != 0 || err != nil {
			t.Fatalf("n=%d err=%v, want 0/nil", n, err)
		}
		if gl.calls != 0 {
			t.Error("listed issues with no sweeper wired")
		}
	})

	t.Run("project cannot dispatch", func(t *testing.T) {
		gl := &sweepGL{}
		sw := &countingSweeper{}
		e := validEntry()
		e.Classification = Classification{State: StateUnmanaged}
		c := NewCache()
		c.Put(e.Project.ID, e)
		r := &Reconciler{GL: gl, Cache: c, Issues: sw, BotUserID: 7}

		n, err := r.sweepIssues(context.Background(), e.Project)
		if n != 0 || err != nil {
			t.Fatalf("n=%d err=%v, want 0/nil", n, err)
		}
		// Not merely "dispatched nothing": it must not spend the API call at all,
		// every pass, forever, on a project whose answer cannot change without a
		// reconcile updating the cache first.
		if gl.calls != 0 {
			t.Errorf("listed issues for an undispatachable project (%d calls)", gl.calls)
		}
	})

	t.Run("project not cached", func(t *testing.T) {
		gl := &sweepGL{}
		r := &Reconciler{GL: gl, Cache: NewCache(), Issues: &countingSweeper{}, BotUserID: 7}
		if n, err := r.sweepIssues(context.Background(), glab.Project{ID: 999}); n != 0 || err != nil {
			t.Fatalf("n=%d err=%v, want 0/nil", n, err)
		}
		if gl.calls != 0 {
			t.Error("listed issues for a project with no cache entry")
		}
	})
}

// A listing failure is reported, not swallowed: ReconcileOnce turns it into a
// per-project error line so a pass that could not sweep says so.
func TestSweepReportsAListingFailure(t *testing.T) {
	gl := &sweepGL{err: errors.New("boom")}
	sw := &countingSweeper{}
	r, p := sweepReconciler(gl, sw)

	n, err := r.sweepIssues(context.Background(), p)
	if err == nil {
		t.Fatal("a failed listing was swallowed")
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if len(sw.got) != 0 {
		t.Error("dispatched despite a failed listing")
	}
}

func TestLabelPrefixMatchIsCaseInsensitive(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{[]string{"gonk::bug"}, true},
		{[]string{"Gonk::Bug"}, true},   // GitLab matches labels case-insensitively
		{[]string{" gonk::bug "}, true}, // and the API can hand back padding
		{[]string{"bug", "wontfix"}, false},
		{[]string{"gonkish"}, false}, // prefix is "gonk::", not "gonk"
		{nil, false},
	} {
		if got := hasLabelPrefix(tc.labels, DefaultIssueLabelPrefix); got != tc.want {
			t.Errorf("hasLabelPrefix(%q) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

// A backlog must not become a spending event. Onboarding a project with many
// open issues once meant one triage order per issue in a single pass; the meter
// would refuse them eventually, but only after the budget was spent.
func TestSweepCapsHowMuchOnePassCanSpend(t *testing.T) {
	var many []glab.Issue
	for i := 1; i <= 25; i++ {
		many = append(many, glab.Issue{IID: int64(i), Author: glab.User{ID: 9}})
	}
	gl := &sweepGL{issues: many}
	sw := &countingSweeper{}
	r, p := sweepReconciler(gl, sw)

	n, err := r.sweepIssues(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n != DefaultIssueSweepLimit {
		t.Errorf("swept %d of 25 open issues, want the cap of %d", n, DefaultIssueSweepLimit)
	}
	if len(sw.got) != DefaultIssueSweepLimit {
		t.Errorf("dispatched %d orders in one pass, want at most %d", len(sw.got), DefaultIssueSweepLimit)
	}

	// An explicit limit is honoured, so an operator draining a backlog on purpose
	// can raise it.
	r.IssueSweepLimit = 2
	sw.got = nil
	if n, _ := r.sweepIssues(context.Background(), p); n != 2 {
		t.Errorf("swept %d with IssueSweepLimit=2, want 2", n)
	}
}
