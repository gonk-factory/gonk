package glab

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// X-GLAB2 reads: test report (H1), tree with mode (H4), issue label events (H5).
// Every success test asserts a NAMED field of the reply; every failure row is a
// reply that is valid JSON (or a 200) and must still be an error.

const sha40 = "0123456789abcdef0123456789abcdef01234567"

func TestGetPipelineTestReport(t *testing.T) {
	good := `{"total_time":1.5,"total_count":3,"success_count":1,"failed_count":1,"skipped_count":1,"error_count":0,
	  "test_suites":[{"name":"go","total_count":3,"success_count":1,"failed_count":1,"skipped_count":1,"error_count":0,
	  "test_cases":[
	   {"status":"success","name":"TestA","classname":"pkg/a","file":"a_test.go","system_output":"noise"},
	   {"status":"failed","name":"TestB","classname":"pkg/b","file":"b_test.go","stack_trace":"boom"},
	   {"status":"skipped","name":"TestC","classname":"pkg/c","file":"c_test.go"}]}]}`
	empty := `{"total_time":0,"total_count":0,"success_count":0,"failed_count":0,"skipped_count":0,"error_count":0,"test_suites":[]}`
	rows := []struct {
		name    string
		status  int
		body    string
		wantErr string // "" = success
	}{
		{"report", 200, good, ""},
		{"no JUnit artifact is a valid empty report", 200, empty, ""},
		{"404 is an error", 404, `{"message":"404 Not found"}`, "404"},
		{"200 with an error body that parses", 200, `{"message":"404 Pipeline Not Found"}`, `no "total_count"`},
		{"200 with a bare array", 200, `[]`, "decode"},
		{"200 with null suites", 200, strings.Replace(empty, `"test_suites":[]`, `"test_suites":null`, 1), `no "test_suites"`},
		{"counts that do not add up", 200, strings.Replace(good, `"total_count":3,"success_count":1,"failed_count":1`, `"total_count":9,"success_count":1,"failed_count":1`, 1), "total_count 9"},
		{"unknown case status", 200, strings.Replace(good, `"status":"failed"`, `"status":"weird"`, 1), `status "weird"`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c, rc := setup(t, r.status, r.body)
			tr, err := c.GetPipelineTestReport(context.Background(), 75, 42)
			wantReq(t, rc.only(t), "GET", "/api/v4/projects/75/pipelines/42/test_report", nil)
			if r.wantErr != "" {
				if err == nil || tr != nil || !strings.Contains(err.Error(), r.wantErr) {
					t.Fatalf("got (%v, %v), want error containing %q", tr, err, r.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.name == "report" {
				tc := tr.TestSuites[0].TestCases[1]
				if tr.TotalCount != 3 || tr.FailedCount != 1 || tc.Name != "TestB" || tc.Classname != "pkg/b" || tc.File != "b_test.go" || tc.Status != "failed" {
					t.Fatalf("decoded wrong: %+v", tr)
				}
			} else if tr.TotalCount != 0 || len(tr.TestSuites) != 0 {
				t.Fatalf("empty report decoded as %+v", tr)
			}
		})
	}
}

// pagedServer answers GET with pages[page-1], advertising X-Next-Page.
func pagedServer(pages []string, seen *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("page")
		if p == "" {
			p = "1"
		}
		*seen = append(*seen, p)
		var n int
		_, _ = fmt.Sscanf(p, "%d", &n)
		if n < len(pages) {
			w.Header().Set("X-Next-Page", fmt.Sprint(n+1))
		}
		_, _ = fmt.Fprint(w, pages[n-1])
	})
}

func treeEntry(name, typ, mode, dir string) string {
	p := name
	if dir != "" {
		p = dir + "/" + name
	}
	return fmt.Sprintf(`{"id":"abc","name":%q,"type":%q,"path":%q,"mode":%q}`, name, typ, p, mode)
}

func TestListTreeModes(t *testing.T) {
	page1 := "[" + treeEntry("a.go", "blob", "100644", "") + "," + treeEntry("run.sh", "blob", "100755", "") + "]"
	page2 := "[" + treeEntry("link", "blob", "120000", "") + "," + treeEntry("sub", "commit", "160000", "") + "," + treeEntry("dir", "tree", "040000", "") + "]"
	var seen []string
	c := newTestClient(t, pagedServer([]string{page1, page2}, &seen))
	es, err := c.ListTree(context.Background(), 75, sha40, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || len(es) != 5 {
		t.Fatalf("pages fetched %v, entries %d: pagination must reach page two", seen, len(es))
	}
	want := map[string]struct {
		mode    string
		regular bool
	}{
		"a.go": {"100644", true}, "run.sh": {"100755", true}, "link": {"120000", false},
		"sub": {"160000", false}, "dir": {"040000", false},
	}
	for _, e := range es {
		w := want[e.Name]
		if e.Mode != w.mode || e.IsRegularFile() != w.regular {
			t.Errorf("%s: mode %q regular %v, want %q %v", e.Name, e.Mode, e.IsRegularFile(), w.mode, w.regular)
		}
	}
}

func TestListTreeQuery(t *testing.T) {
	c, rc := setup(t, 200, "["+treeEntry("x.go", "blob", "100644", "pkg")+"]")
	if _, err := c.ListTree(context.Background(), 75, sha40, "pkg"); err != nil {
		t.Fatal(err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/75/repository/tree", map[string]string{"ref": sha40, "path": "pkg", "per_page": "100"})
}

func TestListTreeRejectsBadReplies(t *testing.T) {
	rows := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"404", 404, `{"message":"404 Tree Not Found"}`, "404"},
		{"200 with an error object", 200, `{"message":"404 Tree Not Found"}`, "decode"},
		{"entry without a mode", 200, `[{"id":"a","name":"f","type":"blob","path":"f"}]`, "lacks"},
		{"path that is not dir/name", 200, "[" + treeEntry("f", "blob", "100644", "other") + "]", "has path"},
		{"blob with a tree mode", 200, "[" + treeEntry("f", "blob", "040000", "") + "]", "mode"},
		{"commit with a file mode", 200, "[" + treeEntry("f", "commit", "100644", "") + "]", "mode"},
		{"unknown type", 200, "[" + treeEntry("f", "weird", "100644", "") + "]", "type"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c, _ := setup(t, r.status, r.body)
			es, err := c.ListTree(context.Background(), 75, sha40, "")
			if err == nil || es != nil || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("got (%v, %v), want error containing %q", es, err, r.want)
			}
		})
	}
	c, rc := setup(t, 200, "[]")
	if _, err := c.ListTree(context.Background(), 75, "", ""); err == nil {
		t.Fatal("empty ref accepted")
	}
	rc.none(t)
}

func TestListTreeEmptyDirIsEmptyNotError(t *testing.T) {
	c, _ := setup(t, 200, "[]")
	es, err := c.ListTree(context.Background(), 75, sha40, "")
	if err != nil || len(es) != 0 {
		t.Fatalf("got (%v, %v)", es, err)
	}
}

func evt(id int, action string) string {
	return fmt.Sprintf(`{"id":%d,"user":{"id":7,"username":"maint"},"created_at":"2026-10-01T10:00:00.000Z","resource_type":"Issue","resource_id":9,"label":{"id":3,"name":"gonk::implement"},"action":%q}`, id, action)
}

func TestListIssueLabelEvents(t *testing.T) {
	var seen []string
	c := newTestClient(t, pagedServer([]string{"[" + evt(1, "add") + "]", "[" + evt(2, "remove") + "," +
		`{"id":3,"user":{"id":8,"username":"x"},"created_at":"2026-10-02T10:00:00Z","resource_type":"Issue","label":null,"action":"add"}` + "]"}, &seen))
	es, err := c.ListIssueLabelEvents(context.Background(), 75, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || len(es) != 3 {
		t.Fatalf("pages %v events %d: pagination must reach page two", seen, len(es))
	}
	if es[0].User.ID != 7 || es[0].Action != "add" || es[0].Label == nil || es[0].Label.Name != "gonk::implement" || es[0].CreatedAt.Year() != 2026 {
		t.Fatalf("event 0 decoded wrong: %+v", es[0])
	}
	if es[1].Action != "remove" || es[2].Label != nil {
		t.Fatalf("events 1/2 decoded wrong: %+v %+v", es[1], es[2])
	}
}

func TestListIssueLabelEventsPath(t *testing.T) {
	c, rc := setup(t, 200, "[]")
	es, err := c.ListIssueLabelEvents(context.Background(), 75, 9)
	if err != nil || len(es) != 0 {
		t.Fatalf("(%v, %v)", es, err)
	}
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/75/issues/9/resource_label_events", nil)
}

func TestListIssueLabelEventsRejectsBadReplies(t *testing.T) {
	good := evt(1, "add")
	rows := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"404", 404, `{"message":"404 Not found"}`, "404"},
		{"200 with an error object", 200, `{"message":"404 Not found"}`, "decode"},
		{"an array of non-events", 200, `[{"message":"x"}]`, "id 0"},
		{"unknown action", 200, "[" + strings.Replace(good, `"add"`, `"move"`, 1) + "]", "action"},
		{"no user id", 200, "[" + strings.Replace(good, `"id":7,`, ``, 1) + "]", "user.id"},
		{"wrong resource type", 200, "[" + strings.Replace(good, `"Issue"`, `"MergeRequest"`, 1) + "]", "resource_type"},
		{"no created_at", 200, "[" + strings.Replace(good, `"created_at":"2026-10-01T10:00:00.000Z",`, ``, 1) + "]", "created_at"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c, _ := setup(t, r.status, r.body)
			es, err := c.ListIssueLabelEvents(context.Background(), 75, 9)
			if err == nil || es != nil || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("got (%v, %v), want error containing %q", es, err, r.want)
			}
		})
	}
}
