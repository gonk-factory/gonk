package ghook

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestParseIssueOpen(t *testing.T) {
	ev, err := ParseEvent("Issue Hook", fixture(t, "issue-open"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.Kind != KindIssue || ev.Project.ID != 42 || ev.Project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", ev.Project)
	}
	if ev.Project.DefaultBranch != "main" || ev.User.ID != 9 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Issue == nil || ev.Issue.IID != 3 || ev.Issue.Action != "open" {
		t.Fatalf("issue = %+v", ev.Issue)
	}
}

func TestParseNoteOnIssue(t *testing.T) {
	ev, err := ParseEvent("Note Hook", fixture(t, "note-mention-issue"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.Note == nil || ev.Note.NoteableType != "Issue" || !strings.Contains(ev.Note.Body, "@gonk") {
		t.Fatalf("note = %+v", ev.Note)
	}
	if ev.Issue == nil || ev.Issue.IID != 3 {
		t.Fatalf("note event must carry the issue iid it hangs off: %+v", ev.Issue)
	}
	if ev.Note.DiscussionID != "d1" {
		t.Fatalf("discussion id = %q (needed to route the reply to the right thread)", ev.Note.DiscussionID)
	}
}

func TestParseMergeRequest(t *testing.T) {
	ev, err := ParseEvent("Merge Request Hook", fixture(t, "mr-onboard-merged"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.MergeRequest == nil || ev.MergeRequest.SourceBranch != "gonk/onboard" || ev.MergeRequest.Action != "merge" {
		t.Fatalf("mr = %+v", ev.MergeRequest)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ header, body string }{
		"not json":             {"Issue Hook", "{{{"},
		"empty":                {"Issue Hook", ""},
		"header/kind mismatch": {"Issue Hook", `{"object_kind":"note","project":{"id":1,"path_with_namespace":"a/b"}}`},
		"unknown kind":         {"Issue Hook", `{"object_kind":"wiki_page"}`},
		"no project id":        {"Issue Hook", `{"object_kind":"issue","project":{"path_with_namespace":"a/b"}}`},
		"no project path":      {"Issue Hook", `{"object_kind":"issue","project":{"id":1}}`},
		"issue without iid":    {"Issue Hook", `{"object_kind":"issue","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"action":"open"}}`},
		"note on a wiki":       {"Note Hook", `{"object_kind":"note","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"id":1,"noteable_type":"Snippet"}}`},
	}
	for name, c := range cases {
		if _, err := ParseEvent(c.header, []byte(c.body)); err == nil {
			t.Errorf("%s: ParseEvent accepted %q", name, c.body)
		}
	}
}

// PLAN.md carry-forward: atags accepts any string for Project/Rig, so a value
// with a newline or comma could shift a column in the ledger. GitLab paths
// cannot contain those — enforce it at the boundary anyway, which is here.
func TestParseRejectsAttributionUnsafePath(t *testing.T) {
	for _, bad := range []string{"a/b\nc", "a/b,c", "a/b\rc"} {
		body := `{"object_kind":"issue","project":{"id":1,"path_with_namespace":` +
			strconv.Quote(bad) + `},"object_attributes":{"iid":1,"action":"open"}}`
		if _, err := ParseEvent("Issue Hook", []byte(body)); err == nil {
			t.Errorf("accepted attribution-unsafe path %q", bad)
		}
	}
}
