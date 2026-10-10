package glab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two reads the pipeline-fix exit check adds (gonk-2usw part PROBE). Each
// asserts a NAMED FIELD of the reply; the bodies in these tables are shapes a
// real GitLab or a proxy can answer 200 with.

func TestListPipelineAttempts(t *testing.T) {
	const two = `[
	 {"id":28775,"name":"unit","stage":"test","status":"failed","failure_reason":"script_failure","allow_failure":false,"pipeline":{"id":7,"sha":"x"}},
	 {"id":28774,"name":"unit","stage":"test","status":"failed","failure_reason":"script_failure","allow_failure":false,"pipeline":{"id":7,"sha":"x"}}]`
	c, rc := setup(t, 200, two)
	jobs, err := c.ListPipelineAttempts(context.Background(), 9, 7)
	if err != nil {
		t.Fatal(err)
	}
	// include_retried=true is not the caller's to forget: without it GitLab
	// returns only the LATEST attempt of each job (observed on project 75).
	wantReq(t, rc.only(t), "GET", "/api/v4/projects/9/pipelines/7/jobs",
		map[string]string{"include_retried": "true", "per_page": "100", "page": "1"})
	if len(jobs) != 2 || jobs[0].ID != 28775 || jobs[1].ID != 28774 || jobs[0].Name != "unit" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestListPipelineAttemptsRejectsWhatIsNotAJobOfThatPipeline(t *testing.T) {
	for name, body := range map[string]string{
		"an error object in the list (parses as a job)": `[{"message":"404 Not found"}]`,
		"a job of ANOTHER pipeline":                     `[{"id":1,"name":"unit","status":"failed","pipeline":{"id":8}}]`,
		"a job with no pipeline object":                 `[{"id":1,"name":"unit","status":"failed"}]`,
		"a job with no id":                              `[{"name":"unit","status":"failed","pipeline":{"id":7}}]`,
		"a job with no name":                            `[{"id":1,"status":"failed","pipeline":{"id":7}}]`,
		"a job with no status":                          `[{"id":1,"name":"unit","pipeline":{"id":7}}]`,
		"the second job is of another pipeline":         `[{"id":2,"name":"unit","status":"success","pipeline":{"id":7}},{"id":1,"name":"lint","status":"success","pipeline":{"id":9}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := setup(t, 200, body)
			if jobs, err := c.ListPipelineAttempts(context.Background(), 9, 7); err == nil {
				t.Fatalf("got %+v, nil; want an error", jobs)
			}
		})
	}
}

func TestListPipelineAttempts404IsAnErrorNotAnEmptyPipeline(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	if jobs, err := c.ListPipelineAttempts(context.Background(), 9, 7); !IsNotFound(err) || jobs != nil {
		t.Fatalf("got %+v, %v", jobs, err)
	}
}

const staticLintBody = `{"valid":true,"errors":[],"warnings":[],"merged_yaml":"---\nunit:\n  script: [make test]\n",
 "includes":[{"type":"local","location":"ci/common.yml","context_project":"g/p","context_sha":"` + sha1a + `"}]}`

func TestCILintStatic(t *testing.T) {
	c, rc := setup(t, 200, staticLintBody)
	l, err := c.CILintStatic(context.Background(), 9, sha1a)
	if err != nil {
		t.Fatal(err)
	}
	got := rc.only(t)
	wantReq(t, got, "GET", "/api/v4/projects/9/ci/lint", map[string]string{"content_ref": sha1a, "include_merged_yaml": "true"})
	// NOT a dry run: a dry run resolves `include: local:` on the DEFAULT
	// BRANCH (dry_run_ref), not at content_ref (observed on project 75,
	// GitLab 19.4.1, 2026-10-06).
	if _, sent := got.query["dry_run"]; sent {
		t.Fatalf("a static lint must not send dry_run: %v", got.query)
	}
	if !l.Valid || !strings.Contains(l.MergedYAML, "unit:") || len(l.Includes) != 1 ||
		l.Includes[0].Type != "local" || l.Includes[0].Location != "ci/common.yml" || l.Includes[0].ContextSHA != sha1a {
		t.Fatalf("lint = %+v", l)
	}
}

// TestCILintStaticOnTheRecordedReplies serves what the real GitLab answered
// (testdata/ci_lint_include_static.json and ..._dry_run.json: both raw replies
// for ONE commit of the synthetic project 75, GitLab CE 19.4.1, recorded by
// hack/pipefix_probe.py run 20261007T145957Z -- a commit that changed only its
// locally included CI file). The server answers the dry-run reply to a request
// that carries dry_run, and the static one otherwise, as GitLab did.
func TestCILintStaticOnTheRecordedReplies(t *testing.T) {
	const sha = "4a774003b2e8f5c8c4497827273c1aaf6d28710b"
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	static, dry := read("ci_lint_include_static.json"), read("ci_lint_include_dry_run.json")
	var queries []url.Values
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		if _, isDry := r.URL.Query()["dry_run"]; isDry {
			_, _ = w.Write(dry)
			return
		}
		_, _ = w.Write(static)
	}))
	l, err := c.CILintStatic(context.Background(), 75, sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].Get("content_ref") != sha {
		t.Fatalf("requests = %v, want one at content_ref=%s", queries, sha)
	}
	if !l.Valid || len(l.Errors) != 0 {
		t.Errorf("valid = %v errors = %q; the commit's own configuration is valid (a dry run says it is not)", l.Valid, l.Errors)
	}
	if len(l.Includes) != 1 || l.Includes[0].Type != IncludeTypeLocal || l.Includes[0].Location != "ci/probe-common.yml" ||
		l.Includes[0].ContextSHA != sha {
		t.Errorf("includes = %+v, want the local file resolved at %s", l.Includes, sha)
	}
	// The line this commit added to its INCLUDED file, and what GitLab's
	// merged YAML looks like (plan 2.2, G4): no `include:` key left, `.pre`
	// and `.post` added.
	for _, want := range []string{`echo "one more line"`, "stages:\n- \".pre\"\n- check\n- report\n- \".post\"\n"} {
		if !strings.Contains(l.MergedYAML, want) {
			t.Errorf("merged_yaml lacks %q", want)
		}
	}
	if strings.Contains(l.MergedYAML, "include:") {
		t.Errorf("merged_yaml still holds an `include:`")
	}

	// What the dry run answered for the same commit, read by the same
	// decoder: a well-formed "invalid" that is NOT about this commit's
	// configuration. Nothing in the reply says so -- which is why the request
	// must never be a dry run.
	var d CILint
	if err := json.Unmarshal(dry, &d); err != nil {
		t.Fatal(err)
	}
	if d.Valid || d.MergedYAML != "" || len(d.Errors) != 1 || d.Errors[0] != "Local file `ci/probe-common.yml` does not exist!" {
		t.Errorf("the recorded dry-run reply = %+v", d)
	}
}

func TestCILintStaticAssertsItsFields(t *testing.T) {
	for name, body := range map[string]string{
		"an error object (parses; no `valid` at all)": `{"message":"404 Commit Not Found"}`,
		"a reason but no `valid`":                     `{"errors":["boom"],"merged_yaml":""}`,
		"an empty object":                             `{}`,
		"valid with no merged_yaml":                   `{"valid":true,"errors":[]}`,
		"valid with a blank merged_yaml":              `{"valid":true,"errors":[],"merged_yaml":"  \n"}`,
		"invalid with no reason":                      `{"valid":false,"errors":[],"merged_yaml":""}`,
		"a local include resolved at ANOTHER commit":  strings.Replace(staticLintBody, sha1a, sha1b, 1),
		"a local include with no context_sha":         strings.Replace(staticLintBody, `,"context_sha":"`+sha1a+`"`, "", 1),
		"an include with no type":                     strings.Replace(staticLintBody, `"type":"local",`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := setup(t, 200, body)
			if l, err := c.CILintStatic(context.Background(), 9, sha1a); err == nil {
				t.Fatalf("got %+v, nil; want an error", l)
			}
		})
	}
}

func TestCILintStaticInvalidConfigIsAResultNotAnError(t *testing.T) {
	c, _ := setup(t, 200, `{"valid":false,"errors":["jobs:unit script can't be blank"],"warnings":[],"merged_yaml":null,"includes":null}`)
	l, err := c.CILintStatic(context.Background(), 9, sha1a)
	if err != nil || l.Valid || len(l.Errors) != 1 || l.MergedYAML != "" {
		t.Fatalf("lint = %+v, %v", l, err)
	}
}

func TestCILintStaticAnIncludeThatIsNotLocalIsReturnedNotJudged(t *testing.T) {
	// Whether a remote / template / project include can be compared is the
	// caller's rule (plan 6.2); the read only refuses a LOCAL include that
	// GitLab resolved somewhere other than at the commit asked for.
	body := strings.Replace(staticLintBody, `"type":"local","location":"ci/common.yml","context_project":"g/p","context_sha":"`+sha1a+`"`,
		`"type":"remote","location":"https://example.invalid/ci.yml","context_project":"g/p","context_sha":"`+sha1b+`"`, 1)
	c, _ := setup(t, 200, body)
	l, err := c.CILintStatic(context.Background(), 9, sha1a)
	if err != nil || len(l.Includes) != 1 || l.Includes[0].Type != "remote" {
		t.Fatalf("lint = %+v, %v", l, err)
	}
}

func TestCILintStaticRejectsNonSHAWithoutARequest(t *testing.T) {
	c, rc := setup(t, 200, `{}`)
	if _, err := c.CILintStatic(context.Background(), 9, "main"); err == nil {
		t.Fatal("want error for a ref name")
	}
	rc.none(t)
}

func TestCILintStatic404IsAnError(t *testing.T) {
	c, _ := setup(t, 404, notFoundBody)
	if l, err := c.CILintStatic(context.Background(), 9, sha1a); !IsNotFound(err) || l != nil {
		t.Fatalf("got %+v, %v", l, err)
	}
}
