package glab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Reads added for the pipeline-fix exit check (gonk-2usw, plan 6.3);
// CILintStatic is also the pipeline-diagnosis prompt's lint. Both are GETs,
// and both assert the NAMED FIELDS their callers then reason from: a
// reply that merely parses (GitLab's own `{"message": "..."}` error object
// decodes into any struct) is an error here, never a fact.

// ListPipelineAttempts returns EVERY job object of a pipeline: every attempt
// of every job, retried ones included (include_retried=true is not the
// caller's to forget -- without it GitLab returns only the latest attempt of
// each job, which is exactly what a retry is pressed to hide; observed on the
// synthetic project 75, GitLab 19.4.1).
//
// Each returned object must be a job OF THAT PIPELINE: an id, a name, a status
// and `pipeline.id` equal to the id asked for. One that is not fails the whole
// call -- a partial list of attempts would read as "this job never failed".
func (c *Client) ListPipelineAttempts(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	jobs, err := c.ListPipelineJobs(ctx, projectID, pipelineID, "", true)
	if err != nil {
		return nil, err
	}
	for i, j := range jobs {
		switch {
		case j.ID == 0 || j.Name == "" || j.Status == "":
			return nil, fmt.Errorf("gitlab: jobs of pipeline %d: entry %d carries id %d, name %q, status %q: not a job object",
				pipelineID, i, j.ID, j.Name, j.Status)
		case j.Pipeline.ID != pipelineID:
			return nil, fmt.Errorf("gitlab: jobs of pipeline %d: job %d carries pipeline.id %d", pipelineID, j.ID, j.Pipeline.ID)
		}
	}
	return jobs, nil
}

// CILintInclude is one entry of the lint reply's `includes`: a file GitLab
// pulled in through `include:` and the context it read it in.
type CILintInclude struct {
	Type           string `json:"type"` // local | file | remote | template | component
	Location       string `json:"location"`
	ContextProject string `json:"context_project"`
	// ContextSHA is the commit the include was resolved at. For a `local`
	// include it must be the commit that was linted.
	ContextSHA string `json:"context_sha"`
}

// IncludeTypeLocal is the `type` of an `include: local:` entry.
const IncludeTypeLocal = "local"

// CILintStatic returns the project's CI configuration AS WRITTEN AT commit
// sha, `include:`d files expanded: GET
// .../ci/lint?content_ref=<sha>&include_merged_yaml=true -- a STATIC lint, with
// NO dry_run.
//
// Why NEVER dry_run=true (gonk-z6gb): a dry run resolves `include: local:` in
// the context of `dry_run_ref`, which defaults to the project's DEFAULT BRANCH
// -- not at content_ref. Observed on the synthetic project 75 (GitLab 19.4.1,
// 2026-10-06/07; testdata/ci_lint_include_*.json are the two replies for one
// commit): linted with dry_run=true, a commit whose included file exists only
// on its branch answers valid=false, "Local file ... does not exist!", no
// merged YAML -- and with a file of that name on the default branch it answers
// with THAT file's text and `includes[].context_sha` naming the other commit.
// Linted statically it carries its own, with context_sha == content_ref. This
// package used to have a second call, CILintMergedYAML, that sent
// dry_run=true; it fed the pipeline-diagnosis prompt's job definitions, which
// could therefore be another commit's, or missing. It is gone: both consumers
// (the diagnosis prompt, cmd/gonk-gate broker_pipeline_context.go, and the
// pipeline-fix exit check, plan 6.2 J2) use this one. A dry run also judges
// whether a pipeline WOULD BE CREATED for its own source ("The pipeline did
// not run", gonk-mf3v), which says nothing about a pipeline that already ran;
// a static lint does not.
//
// Asserted, by name: `valid` must be PRESENT (an error object has none and
// would otherwise read as "the configuration is invalid"); a valid reply must
// carry `merged_yaml`; an invalid one must say why in `errors`; every include
// must carry a `type`; and every `local` include must carry `context_sha`
// equal to sha. An invalid configuration is a successful call (Valid=false,
// Errors set, MergedYAML possibly empty) -- the caller decides what it means.
// Whether an include that is NOT local can be compared is the caller's rule
// too (plan 6.2: "an include whose content cannot be pinned to the SHA").
func (c *Client) CILintStatic(ctx context.Context, projectID int64, sha string) (*CILint, error) {
	if !IsCommitSHA(sha) {
		return nil, fmt.Errorf("gitlab: ci lint: %q is not a full commit SHA", sha)
	}
	rq := request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/ci/lint", projectID),
		query:  map[string]string{"content_ref": sha, "include_merged_yaml": "true"},
	}
	body, _, err := c.do(ctx, rq)
	if err != nil {
		return nil, err
	}
	var present struct {
		Valid *bool `json:"valid"`
	}
	var l CILint
	if err := json.Unmarshal(body, &present); err != nil {
		return nil, fmt.Errorf("gitlab: GET %s: decode: %w", rq.path, err)
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, fmt.Errorf("gitlab: GET %s: decode: %w", rq.path, err)
	}
	switch {
	case present.Valid == nil:
		return nil, fmt.Errorf("gitlab: ci lint at %s: reply carries no `valid`: not a lint result", sha)
	case l.Valid && strings.TrimSpace(l.MergedYAML) == "":
		return nil, fmt.Errorf("gitlab: ci lint at %s: reply is valid but has no merged_yaml", sha)
	case !l.Valid && len(l.Errors) == 0:
		return nil, fmt.Errorf("gitlab: ci lint at %s: reply is invalid but names no error", sha)
	}
	for _, inc := range l.Includes {
		switch {
		case inc.Type == "":
			return nil, fmt.Errorf("gitlab: ci lint at %s: include %q carries no type", sha, inc.Location)
		case inc.Type == IncludeTypeLocal && inc.ContextSHA != sha:
			return nil, fmt.Errorf("gitlab: ci lint at %s: local include %q was resolved at %q, not at the commit asked for",
				sha, inc.Location, inc.ContextSHA)
		}
	}
	return &l, nil
}
