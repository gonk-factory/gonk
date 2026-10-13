package main

// gonk-385g: every controller-written pipeline note LINKS the pipeline (and,
// in a diagnosis, the jobs) it is about, and a diagnosed note leads with a
// heading that names its subject -- so two notes on one tracking issue are
// told apart from their first line.
//
// EVERY FIXTURE HERE IS SYNTHETIC AND GENERIC (plan section 0, the held-out
// rule): the made-up example/calc project of broker_pipeline_apply_test.go.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

const linkBase = "https://gitlab.example.test/group/sub-group/calc"

// plLink is the link a note must name pipeline id by.
func plLink(base string, id int64) string {
	return fmt.Sprintf("[pipeline %d](%s/-/pipelines/%d)", id, base, id)
}

// plJobLink is the link an evidence item or heading must name a job by.
func plJobLink(base, name string, id int64) string {
	return fmt.Sprintf("[`%s`](%s/-/jobs/%d)", name, base, id)
}

// linkRec is a diagnosed run's record as dispatch leaves it, with a project
// URL: pipeline 900 at plSHA, first failure 890, latest 920, last green 880.
func linkRec() beadstore.Record {
	anchor, _ := pipelinectx.Anchor(7, pipelinectx.RefTarget("main"), "0123456789abcdef", 880, 0)
	return beadstore.Record{
		BeadAnchor: anchor, ProjectID: 7, Project: "group/sub-group/calc", Attempt: 1, Rung: "r2",
		PipelineID: 900, PipelineTarget: "ref:main", PipelineFingerprint: "0123456789abcdef",
		PipelineSHA: plSHA, PipelineLastSuccess: 880, PipelineFirstFailed: 890, PipelineLatestFailed: 920,
		PipelineProjectURL: linkBase,
		PipelineJobs:       []beadstore.PipelineJob{{ID: 9002, Name: "unit-tests", Excerpted: true, Groundable: true}},
	}
}

// linkNote is one controller-written text and the pipelines it must name.
type linkNote struct {
	name string
	text string
	ids  []int64
}

// everyPipelineNote renders every text gonk writes about a run from rec:
// the six note states, the full diagnosed note, and the tracking issue's
// description.
func everyPipelineNote(t *testing.T, rec beadstore.Record) []linkNote {
	t.Helper()
	green := glab.Pipeline{ID: 950, SHA: plSHAGreen, FinishedAt: plTime(40)}
	diagnosed := runNote{State: pipelinectx.RunStateDiagnosed, Status: diagnosedStatus(rec),
		Body: renderDiagnosisBody(plDiagnosis(nil).Effects[0], rec)}
	full, err := diagnosed.render(rec.BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	return []linkNote{
		{"diagnosing", diagnosingStatus(rec, 1, "r2"), []int64{900}},
		{"deferred", deferredStatus(rec, meterapi.ReasonQuietHours, "", time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)), []int64{900}},
		{"denied", deniedStatus(rec, meterapi.ReasonLadderExhausted, ""), []int64{900}},
		{"diagnosed", full, []int64{900, 920, 880}},
		{"resolved", resolvedStatus(rec, green), []int64{950, 900}},
		{"superseded", supersededStatus(rec, 960), []int64{960, 900}},
		{"description", refIssueDescription("main", rec, pipelinectx.RunStateDiagnosed), []int64{900, 890, 920, 880}},
	}
}

var (
	pipelineLinkRe  = regexp.MustCompile(`\[pipeline (\d+)\]\(([^)\s]*)\)`)
	pipelineNamedRe = regexp.MustCompile(`(?i)pipeline #?\d+`)
)

// EVERY place a note names a pipeline is a link to THAT pipeline: the owner
// could not tell which of two notes was about which pipeline (gonk-385g).
// Resolved and superseded notes name the pipeline they were written about as
// well as the one that closed the run.
func TestEveryPipelineNoteLinksEveryPipelineItNames(t *testing.T) {
	rec := linkRec()
	for _, n := range everyPipelineNote(t, rec) {
		for _, m := range pipelineLinkRe.FindAllStringSubmatch(n.text, -1) {
			if want := linkBase + "/-/pipelines/" + m[1]; m[2] != want {
				t.Errorf("%s: pipeline %s links to %q, want %q", n.name, m[1], m[2], want)
			}
		}
		if rest := pipelineLinkRe.ReplaceAllString(n.text, ""); pipelineNamedRe.MatchString(rest) {
			t.Errorf("%s: names %q without a link:\n%s", n.name, pipelineNamedRe.FindString(rest), n.text)
		}
		for _, id := range n.ids {
			if !strings.Contains(n.text, plLink(linkBase, id)) {
				t.Errorf("%s: does not link pipeline %d:\n%s", n.name, id, n.text)
			}
		}
		// "#900" is an ISSUE reference in GitLab markdown: never written.
		if regexp.MustCompile(`#\d`).MatchString(n.text) {
			t.Errorf("%s: carries a #<number> reference, which GitLab links to an issue:\n%s", n.name, n.text)
		}
	}
}

// NEVER A BROKEN LINK. A record with no project URL (written before the field
// existed, or parked before any pipeline was read) -- or one carrying anything
// but a plain http(s) URL that is safe inside a markdown link -- gets today's
// plain text: no link syntax at all.
func TestAPipelineNoteWithoutASafeProjectURLNamesThePipelineInPlainText(t *testing.T) {
	for _, u := range []string{
		"", "javascript:alert(1)", "ftp://gitlab.example.test/group/calc", "//gitlab.example.test/group/calc",
		"/group/calc", "gitlab.example.test/group/calc", "https://", "https:///group/calc",
		"https://gitlab.example.test/group/ca lc", "https://gitlab.example.test/group/calc)x",
		"https://gitlab.example.test/group/calc](https://elsewhere.example.test", "https://gitlab.example.test/group/calc?x=1",
		"https://gitlab.example.test/group/calc#frag", "https://user:secret@gitlab.example.test/group/calc",
		"https://gitlab.example.test/group/<calc>", "https://gitlab.example.test/group/calc\nmore",
		"https://gitlab.example.test/group/calc\"", "https://gitlab.example.test/group/calc/",
	} {
		rec := linkRec()
		rec.PipelineProjectURL = u
		for _, n := range everyPipelineNote(t, rec) {
			if strings.Contains(n.text, "](") || strings.Contains(n.text, "[pipeline") {
				t.Errorf("project URL %q, %s: a link was rendered:\n%s", u, n.name, n.text)
			}
			for _, id := range n.ids {
				if !strings.Contains(n.text, fmt.Sprintf("pipeline %d", id)) {
					t.Errorf("project URL %q, %s: pipeline %d is not named:\n%s", u, n.name, id, n.text)
				}
			}
			if u != "" && strings.Contains(n.text, u) {
				t.Errorf("project URL %q, %s: the unsafe URL was written into the note", u, n.name)
			}
			if n.name == "diagnosed" && !strings.Contains(n.text, "- `unit-tests`: `"+plGoodLine+"`") {
				t.Errorf("project URL %q: the evidence job is not a plain code span:\n%s", u, n.text)
			}
		}
	}
}

// The project URL is DERIVED from GitLab's own pipeline object, and only when
// that object's web_url is exactly "<project>/-/pipelines/<its id>".
func TestTheProjectURLIsDerivedOnlyFromThePipelineObjectsOwnWebURL(t *testing.T) {
	for _, tc := range []struct {
		id   int64
		web  string
		want string
	}{
		{900, linkBase + "/-/pipelines/900", linkBase},
		{900, "http://127.0.0.1:4567/example/calc/-/pipelines/900", "http://127.0.0.1:4567/example/calc"},
		// The shape GitLab really returns, read off the synthetic e2e project
		// (project 75, pipeline 3251, 2026-10-04, read-only).
		{3251, "https://gitlab.example.internal/gonk-factory/gonk-e2e-1784441480/-/pipelines/3251", "https://gitlab.example.internal/gonk-factory/gonk-e2e-1784441480"},
		{900, linkBase + "/-/pipelines/901", ""}, // another pipeline's URL
		{900, linkBase + "/-/pipelines/9000", ""},
		{900, linkBase, ""},
		{900, "", ""},
		{0, linkBase + "/-/pipelines/0", ""},
		{900, "javascript:x/-/pipelines/900", ""},
		{900, "https://gitlab.example.test/a b/-/pipelines/900", ""},
		{900, "https://gitlab.example.test/a)b/-/pipelines/900", ""},
		{900, "https://u:p@gitlab.example.test/a/-/pipelines/900", ""},
		{900, "https://gitlab.example.test/a?x=/-/pipelines/900", ""},
		// An IPv6-literal host is bracketed, and "[" "]" are outside the link
		// alphabet: plain text, never a link. This is why the fake GitLab must
		// never be served from [::1] (gonk-ltep: httptest.NewServer moves there
		// by itself when it cannot bind an IPv4 port, and every test asserting
		// on a link went red for as long as that lasted).
		{900, "http://[::1]:4567/example/calc/-/pipelines/900", ""},
	} {
		if got := projectWebURLOf(glab.Pipeline{ID: tc.id, WebURL: tc.web}); got != tc.want {
			t.Errorf("projectWebURLOf(id %d, %q) = %q, want %q", tc.id, tc.web, got, tc.want)
		}
	}
}

// Dispatch RECORDS the project URL from the pipeline it read (the running
// write), and a later dispatch of the same run carries it over -- so a park or
// deny written without reading the pipeline again still links.
func TestPipelineDispatchRecordsTheProjectURLAndCarriesItOver(t *testing.T) {
	r := newPLRun(t, plOpts{})
	if r.proj.WebURL == "" || r.rec.PipelineProjectURL != r.proj.WebURL {
		t.Fatalf("recorded project URL = %q, want the project's web URL %q", r.rec.PipelineProjectURL, r.proj.WebURL)
	}
	var base beadstore.Record
	setPipelineIdentity(&base, r.args(), decidedOnRecord{rec: r.rec, found: true})
	if base.PipelineProjectURL != r.proj.WebURL {
		t.Fatalf("a later dispatch of the run carries project URL %q, want %q", base.PipelineProjectURL, r.proj.WebURL)
	}

	store := beadstore.NewMemory()
	if _, code := r.dispatch(t, plRunDecision(), store, r.args()); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	rec, _, _ := store.Get(context.Background(), r.rec.BeadAnchor)
	if rec.PipelineProjectURL != r.proj.WebURL {
		t.Fatalf("the running record's project URL = %q, want %q", rec.PipelineProjectURL, r.proj.WebURL)
	}
	notes := r.gl.IssueNotesOf(r.pid, 1)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "gonk is diagnosing why "+plLink(r.proj.WebURL, 900)+" failed") {
		t.Fatalf("the diagnosing note does not link the pipeline: %+v", notes)
	}
}

// A FIRST dispatch that is deferred or denied has read no pipeline yet: the
// note still links, from one best-effort read of the pipeline -- and when that
// read fails the note is still written, in plain text.
func TestADeferredOrDeniedPipelineNoteLinksThePipelineOnAFirstDispatch(t *testing.T) {
	for _, tc := range []struct {
		dec  meterapi.DecideResponse
		lead string
	}{
		{meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours}, "gonk will diagnose why %s failed, but cannot yet."},
		{meterapi.DecideResponse{Decision: meterapi.DecisionDeny, Reason: meterapi.ReasonLadderExhausted}, "gonk has stopped and will not diagnose why %s failed."},
	} {
		t.Run(tc.dec.Decision, func(t *testing.T) {
			r := newPLRun(t, plOpts{})
			if _, code := r.dispatch(t, tc.dec, beadstore.NewMemory(), r.args()); code != 0 {
				t.Fatalf("exit = %d", code)
			}
			notes := r.gl.IssueNotesOf(r.pid, 1)
			if want := fmt.Sprintf(tc.lead, plLink(r.proj.WebURL, 900)); len(notes) != 1 || !strings.HasPrefix(notes[0].Body, want) {
				t.Fatalf("note = %+v\nwant it to begin %q", notes, want)
			}
			if d := r.gl.IssuesOf(r.pid)[0].Description; !strings.Contains(d, "| Current run | "+plLink(r.proj.WebURL, 900)) {
				t.Fatalf("the tracking issue's description does not link the pipeline:\n%s", d)
			}

			// The pipeline is read for the NOTE, after the record it reports:
			// a record write that fails is followed by no GitLab request at all.
			r3 := newPLRun(t, plOpts{})
			before := len(r3.gl.Requests())
			r3.dispatch(t, tc.dec, &plFailWritesStore{Store: beadstore.NewMemory(), failPutIf: true}, r3.args())
			if reqs := r3.gl.Requests()[before:]; len(reqs) != 0 {
				t.Fatalf("GitLab was asked although the record write failed: %v", reqs)
			}

			r2 := newPLRun(t, plOpts{})
			r2.gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/pipelines/900", r2.pid), 500)
			if _, code := r2.dispatch(t, tc.dec, beadstore.NewMemory(), r2.args()); code != 0 {
				t.Fatalf("exit = %d with the pipeline unreadable", code)
			}
			notes = r2.gl.IssueNotesOf(r2.pid, 1)
			if want := fmt.Sprintf(tc.lead, "pipeline 900"); len(notes) != 1 || !strings.HasPrefix(notes[0].Body, want) ||
				strings.Contains(notes[0].Body, "](") {
				t.Fatalf("with the pipeline unreadable the note must still be written, in plain text: %+v", notes)
			}
		})
	}
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// THE OWNER'S CASE: two diagnosed notes on ONE tracking issue, about two
// different pipelines. Each note's FIRST LINE names its own pipeline (linked),
// its commit and its failing jobs, and nothing of the other's.
func TestTwoDiagnosedNotesOnOneIssueAreToldApartByTheirFirstLine(t *testing.T) {
	r := newPLRun(t, plOpts{lintJob: true})
	if applied, v, err := r.apply(t, plDiagnosis(nil), io.Discard); !applied {
		t.Fatalf("refused: %q %v", v, err)
	}
	// A second run on the same ref: another pipeline, another commit, and only
	// one of the two jobs failing.
	second := r.rec
	second.PipelineID, second.PipelineLatestFailed, second.PipelineSHA = 910, 910, plSHAOld
	second.PipelineFingerprint = "0123456789abcdef"
	second.BeadAnchor, _ = pipelinectx.Anchor(r.pid, r.target, second.PipelineFingerprint, 880, 0)
	second.PipelineJobs = nil
	for _, j := range r.rec.PipelineJobs {
		if j.Name == "unit-tests" {
			second.PipelineJobs = append(second.PipelineJobs, j)
		}
	}
	r.proj.PutFileOn(plSHAOld, "calc/sum.py", []byte("def total(xs):\n    return 0\n"))
	if applied, v, err := applyPipelineBatch(context.Background(), r.deps(t, io.Discard), &second, plDiagnosis(nil)); !applied {
		t.Fatalf("second run refused: %q %v", v, err)
	}
	if n := len(r.gl.IssuesOf(r.pid)); n != 1 {
		t.Fatalf("issues = %d, want both runs on one tracking issue", n)
	}
	notes := r.gl.IssueNotesOf(r.pid, 1)
	if len(notes) != 2 {
		t.Fatalf("notes = %d, want one per run", len(notes))
	}
	u := r.proj.WebURL
	want := []string{
		"### Diagnosis of " + plLink(u, 900) + " -- commit `111111111111` -- failed jobs: " +
			plJobLink(u, "lint", 9003) + ", " + plJobLink(u, "unit-tests", 9002),
		"### Diagnosis of " + plLink(u, 910) + " -- commit `222222222222` -- failed job: " +
			plJobLink(u, "unit-tests", 9002),
	}
	for i, n := range notes {
		if got := firstLine(n.Body); got != want[i] {
			t.Errorf("note %d first line:\n got %q\nwant %q", i, got, want[i])
		}
	}
	if strings.Contains(firstLine(notes[0].Body), "pipelines/910") || strings.Contains(firstLine(notes[1].Body), "pipelines/900") {
		t.Errorf("a first line names the other note's pipeline:\n%s\n%s", firstLine(notes[0].Body), firstLine(notes[1].Body))
	}
}

// The heading is BOUNDED: at most three failing jobs by name, the rest
// counted; a very long job name is cut; no failed job at all is said so.
func TestTheDiagnosedHeadingListsAtMostThreeFailedJobsAndCountsTheRest(t *testing.T) {
	rec := linkRec()
	rec.PipelineJobs = nil
	for i := 1; i <= 6; i++ {
		rec.PipelineJobs = append(rec.PipelineJobs, beadstore.PipelineJob{ID: int64(9100 + i), Name: fmt.Sprintf("check-%d", i)})
	}
	want := "### Diagnosis of " + plLink(linkBase, 900) + " -- commit `111111111111` -- failed jobs: " +
		plJobLink(linkBase, "check-1", 9101) + ", " + plJobLink(linkBase, "check-2", 9102) + ", " +
		plJobLink(linkBase, "check-3", 9103) + " +3 more"
	if got := firstLine(diagnosedStatus(rec)); got != want {
		t.Errorf("six failed jobs:\n got %q\nwant %q", got, want)
	}

	rec.PipelineJobs = rec.PipelineJobs[:3]
	if got := firstLine(diagnosedStatus(rec)); strings.Contains(got, "more") || !strings.HasSuffix(got, plJobLink(linkBase, "check-3", 9103)) {
		t.Errorf("three failed jobs must all be named and none counted: %q", got)
	}

	rec.PipelineJobs = nil
	want = "### Diagnosis of " + plLink(linkBase, 900) + " -- commit `111111111111` -- no failed job recorded"
	if got := firstLine(diagnosedStatus(rec)); got != want {
		t.Errorf("no failed job:\n got %q\nwant %q", got, want)
	}

	long := strings.Repeat("n", 255)
	rec.PipelineJobs = []beadstore.PipelineJob{{ID: 9201, Name: long}, {ID: 9202, Name: long + "2"}, {ID: 9203, Name: long + "3"}}
	got := firstLine(diagnosedStatus(rec))
	if strings.Contains(got, long) || !strings.Contains(got, "[`"+strings.Repeat("n", 60)+"...`]("+linkBase+"/-/jobs/9201)") {
		t.Errorf("a 255-character job name must be cut to 60 characters and an ellipsis: %q", got)
	}
	if len(got) > 700 {
		t.Errorf("the heading is %d bytes with three maximal job names; it must stay bounded", len(got))
	}
}

// Evidence: a job name links to ITS job page when the controller knows the
// job's id (the record dispatch wrote) and stays a code span otherwise.
func TestAnEvidenceJobLinksToItsJobPageWhenTheControllerKnowsItsID(t *testing.T) {
	diag := plDiagnosis(nil).Effects[0]
	rec := linkRec()
	body := renderDiagnosisBody(diag, rec)
	for _, want := range []string{
		"- " + plJobLink(linkBase, "unit-tests", 9002) + ": `" + plGoodLine + "`\n",
		"- " + plJobLink(linkBase, "unit-tests", 9002) + ": the `failure_reason` fact gonk supplied\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence does not carry %q:\n%s", want, body)
		}
	}

	plain := "- `unit-tests`: `" + plGoodLine + "`\n"
	for name, mutate := range map[string]func(*beadstore.Record){
		"no job id":             func(r *beadstore.Record) { r.PipelineJobs[0].ID = 0 },
		"no project URL":        func(r *beadstore.Record) { r.PipelineProjectURL = "" },
		"job not in the record": func(r *beadstore.Record) { r.PipelineJobs[0].Name = "another" },
		"two jobs carry that name": func(r *beadstore.Record) {
			r.PipelineJobs = append(r.PipelineJobs, beadstore.PipelineJob{ID: 9009, Name: "unit-tests"})
		},
		"no jobs recorded at all":   func(r *beadstore.Record) { r.PipelineJobs = nil },
		"a negative job id":         func(r *beadstore.Record) { r.PipelineJobs[0].ID = -4 },
		"an unsafe project URL":     func(r *beadstore.Record) { r.PipelineProjectURL = "https://gitlab.example.test/a)b" },
		"a record of another shape": func(r *beadstore.Record) { r.PipelineJobs = []beadstore.PipelineJob{{ID: 9002}} },
	} {
		rr := linkRec()
		mutate(&rr)
		if b := renderDiagnosisBody(diag, rr); !strings.Contains(b, plain) || strings.Contains(b, "/-/jobs/") {
			t.Errorf("%s: the evidence job must stay a code span:\n%s", name, b)
		}
	}

	// A job name GitLab allows but markdown would trip on: brackets and a
	// backtick. The link text is still one code span holding the exact name.
	odd := "build [amd64, 1/2] `x`"
	rr := linkRec()
	rr.PipelineJobs = []beadstore.PipelineJob{{ID: 9300, Name: odd, Excerpted: true, Groundable: true}}
	d := diag
	d.Evidence = []effects.EvidenceItem{{Job: odd, Fact: effects.FactFailureReason}}
	if b, want := renderDiagnosisBody(d, rr), "- [`` "+odd+" ``]("+linkBase+"/-/jobs/9300): the "; !strings.Contains(b, want) {
		t.Errorf("an odd job name is not linked as one code span, want %q:\n%s", want, b)
	}
}

// A job's NAME is the pusher's. One that reads as a gonk marker, or that the
// secret redactor would flag, never reaches the heading by name (the heading
// lists every failed job, cited or not, and is re-rendered by run maintenance
// with no validation chain in front of it): it is named by its id instead.
func TestAJobNameThatReadsAsAGonkMarkerNeverReachesTheHeading(t *testing.T) {
	other, _ := pipelinectx.Anchor(7, pipelinectx.RefTarget("main"), "fedcba9876543210", 880, 0)
	forged, _ := pipelinectx.RunMarker(other, pipelinectx.RunStateResolved)
	rec := linkRec()
	rec.PipelineJobs = append(rec.PipelineJobs,
		beadstore.PipelineJob{ID: 9005, Name: "x " + forged},
		beadstore.PipelineJob{ID: 9006, Name: "deploy password=hunter22hunter"})
	status := diagnosedStatus(rec)
	note, err := runNote{State: pipelinectx.RunStateDiagnosed, Status: status}.render(rec.BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if gonkMarkerRe.MatchString(status) || len(pipelinectx.RunMarkerStates(note, other)) != 0 {
		t.Fatalf("a job name forged another run's marker into the note:\n%s", note)
	}
	if st := pipelinectx.RunMarkerStates(note, rec.BeadAnchor); len(st) != 1 || st[0] != pipelinectx.RunStateDiagnosed {
		t.Fatalf("the note's own marker reads %v, want [diagnosed]", st)
	}
	if _, kinds := pipelinectx.Redact(note); len(kinds) != 0 {
		t.Fatalf("the note holds what the redactor calls a secret (%v):\n%s", kinds, note)
	}
	want := "failed jobs: " + plJobLink(linkBase, "unit-tests", 9002) + ", [job 9005](" + linkBase + "/-/jobs/9005), [job 9006](" + linkBase + "/-/jobs/9006)"
	if !strings.HasSuffix(firstLine(status), want) {
		t.Fatalf("heading = %q\nwant it to end %q", firstLine(status), want)
	}
}

// The linked note keeps every safety property of the plain one: it passes the
// secret check and the comment gate, no line begins with a slash, the marker
// and the preserved body read back exactly, and every link target is one the
// controller built from the record -- none comes from the diagnosis.
func TestALinkedDiagnosedNoteKeepsEverySafetyProperty(t *testing.T) {
	rec := linkRec()
	diag := plDiagnosis(func(e *effects.Effect) {
		e.Evidence = append(e.Evidence, effects.EvidenceItem{Job: "unit-tests", Line: plSlashLine})
	}).Effects[0]
	body := renderDiagnosisBody(diag, rec)
	note, err := runNote{State: pipelinectx.RunStateDiagnosed, Status: diagnosedStatus(rec), Body: body}.render(rec.BeadAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if _, kinds := pipelinectx.Redact(note); len(kinds) != 0 {
		t.Fatalf("the linked note holds what the redactor calls a secret (%v):\n%s", kinds, note)
	}
	if err := effects.ValidateComments(effects.Batch{Effects: []effects.Effect{{Kind: effects.KindComment, Body: note}}}); err != nil {
		t.Fatalf("the linked note does not pass the comment gate: %v\n%s", err, note)
	}
	for _, l := range strings.Split(note, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "/") {
			t.Fatalf("a rendered line begins with a slash: %q", l)
		}
	}
	if st := pipelinectx.RunMarkerStates(note, rec.BeadAnchor); len(st) != 1 || st[0] != pipelinectx.RunStateDiagnosed {
		t.Fatalf("marker states = %v, want [diagnosed]", st)
	}
	if got := preservedBody(note); got != strings.TrimSpace(body) {
		t.Fatalf("the preserved body does not read back:\n%s", got)
	}
	targets := regexp.MustCompile(`\]\(([^)\s]*)\)`).FindAllStringSubmatch(note, -1)
	if len(targets) < 5 {
		t.Fatalf("link targets = %d, want the heading's, the status lines' and the evidence's:\n%s", len(targets), note)
	}
	okTarget := regexp.MustCompile(`^` + regexp.QuoteMeta(linkBase) + `/-/(pipelines|jobs)/\d+$`)
	for _, m := range targets {
		if !okTarget.MatchString(m[1]) {
			t.Errorf("link target %q was not built by the controller from the record", m[1])
		}
	}
}

// AN OLD RECORD (no project URL) is not left unlinked for ever: run
// maintenance learns the project URL from the pipelines it observes, links the
// note it writes, and records the URL.
func TestRunMaintenanceLinksAnOldPipelineRecordFromThePipelinesItSees(t *testing.T) {
	r, store := landed(t, plOpts{})
	old := r.rec
	old.PipelineProjectURL = ""
	if err := store.Put(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	r.gl.AddPipeline(r.pid, glab.Pipeline{ID: 950, Ref: "main", SHA: plSHAGreen, Status: "success", Source: "push"})
	rec := r.maintain(t, store, time.Now())
	u := r.proj.WebURL
	st, body := r.runNote(t)
	if st != pipelinectx.RunStateResolved || !strings.HasPrefix(body, "Resolved: "+plLink(u, 950)+" passed at commit ") ||
		!strings.Contains(firstLine(body), "gonk's diagnosis of "+plLink(u, 900)+" is kept below.") {
		t.Fatalf("state %s; the resolved note of an old record is not linked:\n%s", st, body)
	}
	if rec.PipelineProjectURL != u {
		t.Fatalf("maintenance recorded project URL %q, want %q", rec.PipelineProjectURL, u)
	}
}
