package glab

import (
	"context"
	"encoding/json"
	"fmt"
)

// Pipeline test report read (gonk-cuwn X-GLAB2, plan hypothesis H1). A GET.
//
// maxTestReportBytes bounds the body: a report with thousands of cases is far
// past the 1 MiB default, but it is still finite.
const maxTestReportBytes int64 = 16 << 20

// TestCase is one case of a suite, as GitLab returns it. system_output and
// stack_trace are deliberately NOT decoded: they are attacker-influenced
// (anything the test printed) and no caller needs them.
type TestCase struct {
	Name      string `json:"name"`
	Classname string `json:"classname"`
	File      string `json:"file"`
	// Status is GitLab's own: success, failed, skipped or error.
	Status string `json:"status"`
}

// TestSuite is one suite of a report.
type TestSuite struct {
	Name         string     `json:"name"`
	TotalCount   int        `json:"total_count"`
	SuccessCount int        `json:"success_count"`
	FailedCount  int        `json:"failed_count"`
	SkippedCount int        `json:"skipped_count"`
	ErrorCount   int        `json:"error_count"`
	TestCases    []TestCase `json:"test_cases"`
}

// TestReport is a pipeline's test report. A pipeline with no JUnit artifact
// answers 200 with TotalCount 0 and no suites: that is a valid report that says
// "nothing was reported", not an error.
type TestReport struct {
	TotalCount   int         `json:"total_count"`
	SuccessCount int         `json:"success_count"`
	FailedCount  int         `json:"failed_count"`
	SkippedCount int         `json:"skipped_count"`
	ErrorCount   int         `json:"error_count"`
	TestSuites   []TestSuite `json:"test_suites"`
}

// GetPipelineTestReport reads GET /projects/:id/pipelines/:pid/test_report.
//
// The reply is asserted on, not merely parsed: GitLab answers a missing
// pipeline as {"message":"404 ..."}, which is valid JSON and would decode into
// a zero TestReport -- indistinguishable from "a pipeline that reported no
// tests". So the five count keys and test_suites must be PRESENT in the body
// (a key check, not a value check), the counts must add up, and every case
// must carry a name and a known status. Anything else is an error.
func (c *Client) GetPipelineTestReport(ctx context.Context, projectID, pipelineID int64) (*TestReport, error) {
	rq := request{
		method:   "GET",
		path:     fmt.Sprintf("/api/v4/projects/%d/pipelines/%d/test_report", projectID, pipelineID),
		maxBytes: maxTestReportBytes,
	}
	body, _, err := c.do(ctx, rq)
	if err != nil {
		return nil, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, fmt.Errorf("gitlab: GET %s: decode: %w", rq.path, err)
	}
	for _, k := range []string{"total_count", "success_count", "failed_count", "skipped_count", "error_count", "test_suites"} {
		if v, ok := keys[k]; !ok || string(v) == "null" {
			return nil, fmt.Errorf("gitlab: GET %s: reply has no %q: not a test report", rq.path, k)
		}
	}
	var tr TestReport
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("gitlab: GET %s: decode: %w", rq.path, err)
	}
	if sum := tr.SuccessCount + tr.FailedCount + tr.SkippedCount + tr.ErrorCount; tr.TotalCount < 0 || sum != tr.TotalCount {
		return nil, fmt.Errorf("gitlab: GET %s: total_count %d != success+failed+skipped+error %d", rq.path, tr.TotalCount, sum)
	}
	for _, s := range tr.TestSuites {
		for _, tc := range s.TestCases {
			switch tc.Status {
			case "success", "failed", "skipped", "error":
			default:
				return nil, fmt.Errorf("gitlab: GET %s: suite %q case %q has status %q", rq.path, s.Name, tc.Name, tc.Status)
			}
			if tc.Name == "" {
				return nil, fmt.Errorf("gitlab: GET %s: suite %q has a case with no name", rq.path, s.Name)
			}
		}
	}
	return &tr, nil
}
