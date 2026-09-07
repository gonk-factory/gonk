package intake

import (
	"context"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

func TestBlocklistMatchesByPathAndID(t *testing.T) {
	b, err := ParseBlocklist([]string{"agentic/gonk-project", "69", " homelab/secret-repo "})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tc := range []struct {
		name string
		p    glab.Project
		want bool
	}{
		{"by path", glab.Project{ID: 999, PathWithNamespace: "agentic/gonk-project"}, true},
		{"by id", glab.Project{ID: 69, PathWithNamespace: "renamed/elsewhere"}, true},
		{"whitespace trimmed", glab.Project{ID: 5, PathWithNamespace: "homelab/secret-repo"}, true},
		// GitLab paths are case-preserving but compared case-insensitively, so a
		// project reachable as Agentic/Gonk-Project is the same project.
		{"case insensitive", glab.Project{ID: 5, PathWithNamespace: "Agentic/Gonk-Project"}, true},
		{"unrelated", glab.Project{ID: 63, PathWithNamespace: "homelab/talos-toolkit"}, false},
		// A blocked project renamed away from its listed path is still caught by
		// id, and a NEW project taking the old path is still caught by path.
		{"substring is not a match", glab.Project{ID: 5, PathWithNamespace: "agentic/gonk-project-docs"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.Blocked(tc.p); got != tc.want {
				t.Errorf("Blocked(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// FAIL CLOSED. The bead is explicit: prefer "unparseable list = refuse" over
// "unparseable list = watch everything". Each of these would otherwise parse to
// a list that blocks LESS than the operator wrote, which is the silent failure.
func TestBlocklistRefusesEntriesThatWouldBlockNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		says    string
	}{
		{
			// The dangerous typo: looks deliberate, matches nothing, and would
			// leave gonk watching its own repository.
			name: "bare name without a namespace", entries: []string{"gonk-project"},
			says: "block nothing",
		},
		{name: "zero id", entries: []string{"0"}, says: "positive project id"},
		{name: "negative id", entries: []string{"-1"}, says: "positive project id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBlocklist(tc.entries)
			if err == nil {
				t.Fatalf("accepted %v, which would block nothing", tc.entries)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not explain itself; wanted %q in %q", tc.says, err)
			}
		})
	}
}

func TestBlocklistTreatsBlankEntriesAsAbsent(t *testing.T) {
	b, err := ParseBlocklist([]string{"", "   ", "agentic/gonk-project"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := b.Entries(); len(got) != 1 {
		t.Errorf("entries = %v, want just the one real entry", got)
	}
	if b.Empty() {
		t.Error("a list with one entry reports Empty")
	}
}

func TestEmptyBlocklistIsLegalButDetectable(t *testing.T) {
	b, err := ParseBlocklist(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !b.Empty() {
		t.Error("an empty list does not report Empty, so nothing can warn about it")
	}
	if b.Blocked(glab.Project{ID: 69, PathWithNamespace: "agentic/gonk-project"}) {
		t.Error("an empty list blocked something")
	}
}

// Decide must refuse a blocked entry AHEAD of every other rule, so that no
// combination of classification, trigger or staleness can produce a dispatch.
func TestDecideRefusesABlockedProjectBeforeAnythingElse(t *testing.T) {
	e := validEntry()
	e.Blocked = true

	// An event that would otherwise dispatch: a freshly opened issue on a valid,
	// non-stale project.
	ev := issueEvent("open")
	if d := Decide(e, ev, "gonk"); d.Dispatch {
		t.Fatalf("dispatched for a blocked project: %+v", d)
	} else if d.Reason != "blocked_project" {
		t.Errorf("reason = %q, want blocked_project (it must be refused for the RIGHT reason, "+
			"or a later change to the other rules could silently start dispatching)", d.Reason)
	}

	// And the same entry unblocked must dispatch, or the test above proves
	// nothing.
	e.Blocked = false
	if d := Decide(e, ev, "gonk"); !d.Dispatch {
		t.Fatalf("the control case did not dispatch, so the blocked case proves nothing: %+v", d)
	}
}

// blockGL returns one project and panics on everything reconcileProject would
// do to it. If the blocklist works, none of those methods is ever reached --
// which is the property that matters: a blocked project must be dropped BEFORE
// it is observed, not reconciled and then refused.
type blockGL struct {
	sweepGL
	projects []glab.Project
}

func (g *blockGL) ListMemberProjects(context.Context) ([]glab.Project, error) {
	return g.projects, nil
}

func TestReconcileNeverObservesABlockedProject(t *testing.T) {
	blocked := glab.Project{ID: 69, PathWithNamespace: "agentic/gonk-project"}
	bl, err := ParseBlocklist([]string{"agentic/gonk-project"})
	if err != nil {
		t.Fatal(err)
	}
	gl := &blockGL{projects: []glab.Project{blocked}}
	r := &Reconciler{
		GL: gl, Cache: NewCache(), Obs: NopObserver{}, Blocked: bl,
		Meter: NewMeterClient("http://127.0.0.1:1", "unused", nil),
	}

	// sweepGL panics on GetRawFile/DirExists/ListHooks -- everything
	// reconcileProject calls. Reaching any of them fails the test loudly.
	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if sum.Blocked != 1 {
		t.Errorf("Summary.Blocked = %d, want 1", sum.Blocked)
	}
	if sum.Errors != 0 {
		t.Errorf("blocking a project produced %d errors: %v", sum.Errors, sum.ErrorMsgs)
	}
	if _, ok := r.Cache.Get(blocked.ID); ok {
		t.Error("a blocked project entered the cache; nothing downstream should be able to see it at all")
	}
}

// The control: without the blocklist the SAME setup reaches GitLab and panics.
// Without this, the test above would pass just as well if ReconcileOnce were
// broken and never looked at any project.
func TestTheBlockedProjectTestIsNotVacuous(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("reconcileProject did not touch GitLab for an UNBLOCKED project, " +
				"so the blocked case proves nothing")
		}
	}()
	gl := &blockGL{projects: []glab.Project{{ID: 69, PathWithNamespace: "agentic/gonk-project"}}}
	r := &Reconciler{
		GL: gl, Cache: NewCache(), Obs: NopObserver{},
		Meter: NewMeterClient("http://127.0.0.1:1", "unused", nil),
	}
	_, _ = r.ReconcileOnce(context.Background())
}
