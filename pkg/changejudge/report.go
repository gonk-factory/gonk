package changejudge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gonk-factory/gonk/pkg/glab"
)

// Report is a pipeline's test report as a fact (plan 2.5: TB at the base, TH
// at the head; assumption H1). The zero value is "not read". Read with Total
// 0 is a pipeline that published no report -- the plan's "none".
type Report struct {
	Read bool
	// Total: GitLab's total_count.
	Total int
	// Cases: EVERY case of every suite. A report whose cases do not add up
	// to Total is not a fact about the suite and is treated as not read.
	Cases []Case
}

// Case is one test case. Its identity is (Suite, Classname, Name) -- plan
// assumption H2.
type Case struct {
	Suite     string
	Classname string
	Name      string
	// Status: GitLab's own: success, failed, skipped or error.
	Status string
}

// GitLab's test case statuses.
const (
	CaseSuccess = "success"
	CaseFailed  = "failed"
	CaseSkipped = "skipped"
	CaseError   = "error"
)

// ReportOf turns a report READ from GitLab into the fact. A report that could
// not be read is the zero Report, never ReportOf of an empty one: pass nil
// (or do not call this) when glab.GetPipelineTestReport returned an error.
func ReportOf(tr *glab.TestReport) Report {
	if tr == nil {
		return Report{}
	}
	r := Report{Read: true, Total: tr.TotalCount}
	for _, s := range tr.TestSuites {
		for _, c := range s.TestCases {
			r.Cases = append(r.Cases, Case{Suite: s.Name, Classname: c.Classname, Name: c.Name, Status: c.Status})
		}
	}
	return r
}

// wellFormed: the report was read and is complete -- as many cases as its
// total, each with a name and a status GitLab defines.
func (r Report) wellFormed() bool {
	if !r.Read {
		return false
	}
	if len(r.Cases) != r.Total {
		return false
	}
	for _, c := range r.Cases {
		if c.Name == "" {
			return false
		}
		switch c.Status {
		case CaseSuccess, CaseFailed, CaseSkipped, CaseError:
		default:
			return false
		}
	}
	return true
}

// caseID is a test case's identity.
type caseID struct{ suite, classname, name string }

func (id caseID) String() string {
	return id.suite + " / " + id.classname + " / " + id.name
}

// tally counts a report's cases per identity: how many passed, how many
// failed (failed or error). Counts, not sets: two cases of one identity are
// two passes, and losing one of them is a loss.
type tally struct {
	passed map[caseID]int
	failed map[caseID]int
	cases  []Case
}

func tallyOf(r Report) tally {
	t := tally{passed: map[caseID]int{}, failed: map[caseID]int{}, cases: r.Cases}
	for _, c := range r.Cases {
		id := caseID{c.Suite, c.Classname, c.Name}
		switch c.Status {
		case CaseSuccess:
			t.passed[id]++
		case CaseFailed, CaseError:
			t.failed[id]++
		}
	}
	return t
}

// goPasses counts the passing cases named for a Go test function: the case
// name IS the function (a subtest `TestX/sub` is not the function passing),
// in the function's package when the directory is known.
func (t tally) goPasses(g GoTest) int {
	n := 0
	for _, c := range t.cases {
		if c.Status != CaseSuccess || c.Name != g.Name {
			continue
		}
		if g.Dir != "" && c.Classname != g.Dir && !strings.HasSuffix(c.Classname, "/"+g.Dir) {
			continue
		}
		n++
	}
	return n
}

func (g GoTest) String() string {
	if g.Dir == "" {
		return g.Name
	}
	return g.Dir + "." + g.Name
}

// testsApply: K4 runs unless the diff is documentation only. The plan says
// "only when the diff touches `source` or `test`"; read fail-closed, a class
// this package does not know is not a reason to skip looking for tests.
func testsApply(classes []string) bool {
	for _, c := range classes {
		if c != ClassDocs {
			return true
		}
	}
	return false
}

// greenCandidates: the candidates whose report K4 reads -- the ones that
// finished `success`. A pipeline that failed, was canceled or never finished
// has its K1/K3 finding already; its report is truncated at wherever it
// stopped and proves nothing about which tests were collected.
func greenCandidates(candidates []Pipeline) []Pipeline {
	var out []Pipeline
	for _, c := range candidates {
		if c.Status == StatusSuccess {
			out = append(out, c)
		}
	}
	return out
}

// unreadReports names the green candidates whose report is unread.
func unreadReports(candidates []Pipeline) []string {
	var out []string
	for _, c := range greenCandidates(candidates) {
		if !c.Report.wellFormed() {
			out = append(out, fmt.Sprintf("test report: the test report of pipeline %d was not read, or its cases do not match its total", c.ID))
		}
	}
	return out
}

// judgeTests is K4, in the plan's order. See Judge for the readings taken.
func judgeTests(ch ChangeFacts, candidates []Pipeline) Tests {
	if !testsApply(ch.DiffClasses) {
		return Tests{Finding: TestsNotApplicable, Rule: RuleK4NotApplicable}
	}
	green := greenCandidates(candidates)
	if len(green) == 0 {
		return Tests{}
	}
	// TH unreadable: UNKNOWN, no verdict this pass.
	if len(unreadReports(candidates)) > 0 {
		return Tests{Finding: TestsUnknown, Rule: RuleK4Unknown}
	}
	base := tallyOf(ch.BaseReport)
	// A green pipeline that published nothing says nothing; the ones that
	// published a report are the evidence.
	var heads []tally
	for _, g := range green {
		if g.Report.Total > 0 {
			heads = append(heads, tallyOf(g.Report))
		}
	}
	// TH.total == 0.
	if len(heads) == 0 {
		if len(base.passed) > 0 {
			return Tests{Finding: TestsReduced, Rule: RuleK4Vanished}
		}
		return Tests{Finding: TestsUnobserved, Rule: RuleK4Unobserved}
	}
	// P(B) not a subset of P(H), in ANY reporting pipeline.
	lost := map[caseID]bool{}
	for _, h := range heads {
		for id, n := range base.passed {
			if h.passed[id] < n {
				lost[id] = true
			}
		}
	}
	if len(lost) > 0 {
		return Tests{Finding: TestsReduced, Rule: RuleK4Reduced, Lost: sortedIDs(lost)}
	}
	// The new tests ran: in at least ONE reporting pipeline.
	if ch.GoProfile {
		var notRun []string
		for _, g := range ch.AddedGoTests {
			ran := false
			for _, h := range heads {
				if h.goPasses(g) > base.goPasses(g) {
					ran = true
				}
			}
			if !ran {
				notRun = append(notRun, g.String())
			}
		}
		if len(notRun) > 0 {
			sort.Strings(notRun)
			return Tests{Finding: TestsNewNotRun, Rule: RuleK4NewNotRunGo, NotRun: notRun}
		}
	} else {
		// P(H) minus P(B) is empty. The plan adds "TB present"; without a
		// base report P(B) is empty, so this asks for ONE passing case at the
		// head -- a report of nothing but skipped cases is not tests that ran.
		anyNew := false
		for _, h := range heads {
			for id, n := range h.passed {
				if n > base.passed[id] {
					anyNew = true
				}
			}
		}
		if !anyNew {
			return Tests{Finding: TestsNewNotRun, Rule: RuleK4NewNotRunNone}
		}
	}
	// A case that fails at the head and did not at the base, on a pipeline
	// that is green all the same.
	failing := map[caseID]bool{}
	for _, h := range heads {
		for id, n := range h.failed {
			if n > base.failed[id] {
				failing[id] = true
			}
		}
	}
	if len(failing) > 0 {
		return Tests{Finding: TestsFailed, Rule: RuleK4Failed, Failing: sortedIDs(failing)}
	}
	return Tests{Finding: TestsObserved, Rule: RuleK4Observed}
}

func sortedIDs(set map[caseID]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}
