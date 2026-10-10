package main

// LINKS IN PIPELINE RUN NOTES (gonk-385g). The owner read a tracking issue
// carrying two diagnosis notes, about two different pipelines, and could not
// tell which note was about which: a note named its pipeline as plain text
// ("pipeline 900"), with nothing to click and nothing leading the note. So
// every controller-written note and the tracking issue's description now name
// a pipeline as a LINK to it, a diagnosed note leads with a heading naming
// its pipeline, commit and failing jobs, and an evidence item links its job.
//
// EVERY LINK IS THE CONTROLLER'S. The target is built here from three facts
// the record holds -- Record.PipelineProjectURL (derived from the web_url of
// GitLab's own pipeline object, see projectWebURLOf), a pipeline id and a
// job id (Record.PipelineJobs, from GitLab's job list) -- and the link text
// is "pipeline <id>", "job <id>" or the RECORDED job name in a code span.
// Nothing in a link's text or target comes from the diagnosis: these
// functions are not even handed it, except evidenceJobRef, which uses the
// name an evidence item cites only to LOOK UP the recorded job.
//
// NEVER A BROKEN LINK. A record with no project URL (written before the
// field existed; parked or denied before any pipeline was read and the
// best-effort read failed) or with one that is not a plain http(s) URL safe
// inside a markdown link gets the plain text these notes always had.

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// safeProjectURL returns u if it may be used as the base of a link target,
// and "" otherwise. It must be an absolute http(s) URL with a host and no
// trailing slash, made ONLY of characters that cannot end or escape a
// markdown link destination: letters, digits and . _ ~ - : / (a GitLab
// project path is letters, digits, ". _ -"; ":" is the scheme's and a
// port's). That alphabet is also what rules out credentials ("@"), a query
// ("?"), a fragment ("#"), spaces, brackets and parentheses -- there is no
// separate check for those to fall out of step with. It is applied when the URL is derived
// AND again every time a link is rendered: a record is read back from a
// store, and whatever it holds is never trusted to be well formed.
func safeProjectURL(u string) string {
	if u == "" || strings.HasSuffix(u, "/") {
		return ""
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._~-:/", r):
		default:
			return ""
		}
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return ""
	}
	return u
}

// projectWebURLOf derives a project's web URL from one of ITS pipelines, as
// GitLab's API returned it: the pipeline's web_url must be exactly
// "<project web url>/-/pipelines/<that pipeline's id>", and what precedes the
// suffix must be a safe URL. Anything else answers "" (no links), never a
// guess: the configured API base URL is deliberately not used, because it may
// be an in-cluster address nobody can open in a browser.
func projectWebURLOf(p glab.Pipeline) string {
	if p.ID <= 0 {
		return ""
	}
	base, ok := strings.CutSuffix(p.WebURL, fmt.Sprintf("/-/pipelines/%d", p.ID))
	if !ok {
		return ""
	}
	return safeProjectURL(base)
}

// pipelineRef names a pipeline in a note: a link to it, "[pipeline
// 900](<project>/-/pipelines/900)", or plain "pipeline 900" when the record
// has no safe project URL. Never "#900" -- GitLab markdown links "#900" to
// ISSUE 900.
func pipelineRef(rec beadstore.Record, id int64) string {
	if id <= 0 {
		return "none"
	}
	text := fmt.Sprintf("pipeline %d", id)
	if base := safeProjectURL(rec.PipelineProjectURL); base != "" {
		return fmt.Sprintf("[%s](%s/-/pipelines/%d)", text, base, id)
	}
	return text
}

// jobLink wraps text (already safe markdown: a code span, or "job <id>") in a
// link to the job's page when the record has a safe project URL and the job
// id is real; otherwise it returns text unchanged.
func jobLink(rec beadstore.Record, id int64, text string) string {
	if base := safeProjectURL(rec.PipelineProjectURL); base != "" && id > 0 {
		return fmt.Sprintf("[%s](%s/-/jobs/%d)", text, base, id)
	}
	return text
}

// evidenceJobRef renders the job an evidence item cites: the name in a code
// span (as before), linked to the job's page when EXACTLY ONE recorded job
// carries that name and its id is known. The cited name has already been
// checked against the record (D8 step 4) and for markers (step 8); it is used
// here only to find the recorded job. Two recorded jobs with one name (it
// should not happen: the record holds latest attempts only) are not guessed
// between.
func evidenceJobRef(rec beadstore.Record, name string) string {
	var found *beadstore.PipelineJob
	for i := range rec.PipelineJobs {
		if rec.PipelineJobs[i].Name != name {
			continue
		}
		if found != nil {
			return codeSpan(name)
		}
		found = &rec.PipelineJobs[i]
	}
	if found == nil {
		return codeSpan(name)
	}
	return jobLink(rec, found.ID, codeSpan(found.Name))
}

const (
	// headingJobs is how many failed jobs a diagnosed note's heading names;
	// the rest are counted ("+N more").
	headingJobs = 3
	// headingJobNameRunes caps one job name in the heading (GitLab allows 255
	// characters); a longer one is cut and marked "...".
	headingJobNameRunes = 60
)

// headingJobRef names one recorded failed job in the heading. THE NAME IS THE
// PUSHER'S, and the heading lists every failed job -- cited by the diagnosis
// or not -- and is re-rendered by run maintenance with no validation chain in
// front of it. So a name that reads as a gonk marker (it could forge another
// run's state for gonk-intake), that the secret redactor would flag, or that
// is empty is NOT written: the job is named by its id instead.
func headingJobRef(rec beadstore.Record, j beadstore.PipelineJob) string {
	_, secret := pipelinectx.Redact(j.Name)
	if strings.TrimSpace(j.Name) == "" || gonkMarkerRe.MatchString(j.Name) || len(secret) > 0 {
		if j.ID > 0 {
			return jobLink(rec, j.ID, fmt.Sprintf("job %d", j.ID))
		}
		return "(unnamed job)"
	}
	name := j.Name
	if r := []rune(name); len(r) > headingJobNameRunes {
		name = string(r[:headingJobNameRunes]) + "..."
	}
	return jobLink(rec, j.ID, codeSpan(name))
}

// diagnosedHeading is the FIRST LINE of a diagnosed note: the pipeline it is
// about (linked), its commit, and the jobs that failed it -- everything the
// reader needs to tell this note from the next one on the same issue. One
// line, bounded: at most headingJobs jobs by name. Built from the record
// alone.
func diagnosedHeading(rec beadstore.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Diagnosis of %s -- commit %s -- ", pipelineRef(rec, rec.PipelineID), codeSpan(shortSHA(rec.PipelineSHA)))
	jobs := rec.PipelineJobs
	switch len(jobs) {
	case 0:
		b.WriteString("no failed job recorded")
		return b.String()
	case 1:
		b.WriteString("failed job: ")
	default:
		b.WriteString("failed jobs: ")
	}
	shown := min(len(jobs), headingJobs)
	names := make([]string, 0, shown)
	for _, j := range jobs[:shown] {
		names = append(names, headingJobRef(rec, j))
	}
	b.WriteString(strings.Join(names, ", "))
	if more := len(jobs) - shown; more > 0 {
		fmt.Fprintf(&b, " +%d more", more)
	}
	return b.String()
}

// withPipelineProjectURL returns rec with its project URL filled in, when it
// has none, from ONE read of the record's own pipeline. For the notes written
// by a dispatch that read no pipeline (a first dispatch that is deferred or
// denied): called only AFTER the record write those notes report, so it adds
// nothing in front of that write and the URL it finds is used for the note
// alone. BEST EFFORT, bounded by statusNoteTimeout: any failure leaves rec as
// it was and the note names its pipeline in plain text.
func (d dispatchDeps) withPipelineProjectURL(ctx context.Context, rec beadstore.Record) beadstore.Record {
	if rec.PipelineProjectURL != "" || rec.PipelineID < 1 || d.Pipelines == nil || terminated(ctx) {
		return rec
	}
	rctx, cancel := context.WithTimeout(ctx, statusNoteTimeout)
	defer cancel()
	p, err := d.Pipelines.GetPipeline(rctx, rec.ProjectID, rec.PipelineID)
	if err != nil || p == nil {
		d.Log.Info("pipeline run note will not link its pipeline: the pipeline could not be read",
			"bead", rec.BeadAnchor, "pipeline", rec.PipelineID, "err", err)
		return rec
	}
	rec.PipelineProjectURL = projectWebURLOf(*p)
	return rec
}
