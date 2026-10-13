package main

// WHERE A PIPELINE DIAGNOSIS LANDS (gonk-tios D9). Deterministic and chosen by
// the CONTROLLER from the record -- never by the agent: the diagnosis effect
// may name only the recorded pipeline (ValidateTargets), and nothing here
// reads a target from a batch.
//
//	mr:<iid>      ONE note per run on the MR, edited in place through its
//	              lifecycle, plus one gonk::pipeline-<class> label (the other
//	              five removed).
//	ref:<branch>  ONE open gonk-owned issue per ref ("gonk: pipeline failing
//	              on <ref>", gonk::pipeline-failing + the class label, a
//	              description carrying pipelinectx.RefIssueMarker and a status
//	              table), and ONE note per run on it, edited in place. Closed
//	              when the branch goes green.
//
// THE MARKERS ARE P3's, AND MUST BE. gonk-intake decides whether a run is
// already handled by reading pipelinectx.RunMarker(anchor, state) out of
// bot-authored, non-system notes on these landings (and the tracking issue by
// RefIssueMarker in a bot-authored issue's description). Every note and issue
// here is written through those two functions, so the writer and the reader
// cannot drift (TestRunNoteMarkersAreWhatIntakeReads).
//
// A RUN NOTE IS FOUND BY ITS MARKER, NEVER BY AN ID KEPT ELSEWHERE, and only
// among notes the token's own user authored: a human quoting a marker must
// never get their comment edited by the bot. The bot's identity is asked of
// GitLab (CurrentUser) rather than taken from a configured name.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// pipelineForge is everything the pipeline path asks of GitLab: the reads the
// context builder and the sweep's checks make, and the landing's writes. All
// of it is scoped to ONE project id, the record's. Satisfied by *glab.Client.
type pipelineForge interface {
	pipelineReader
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
	CurrentUser(ctx context.Context) (*glab.User, error)

	ListMRNotes(ctx context.Context, projectID, mrIID int64) ([]glab.Note, error)
	CreateMRNote(ctx context.Context, projectID, mrIID int64, body string) (*glab.Note, error)
	UpdateMRNote(ctx context.Context, projectID, mrIID, noteID int64, body string) (*glab.Note, error)
	UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) (*glab.MergeRequest, error)

	ListIssues(ctx context.Context, projectID int64, opts glab.IssueListOptions) ([]glab.Issue, error)
	CreateIssue(ctx context.Context, projectID int64, opts glab.IssueOptions) (*glab.Issue, error)
	UpdateIssue(ctx context.Context, projectID, issueIID int64, u glab.IssueUpdate) (*glab.Issue, error)
	ListIssueNotes(ctx context.Context, projectID, issueIID int64) ([]glab.Note, error)
	CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (*glab.Note, error)
	UpdateIssueNote(ctx context.Context, projectID, issueIID, noteID int64, body string) (*glab.Note, error)
	CloseIssue(ctx context.Context, projectID, issueIID int64) error
}

var _ pipelineForge = (*glab.Client)(nil)

// pipelineRunOf reads a run's landing identity off its record: the target it
// lands on and the anchor its marker names. A record that cannot produce both
// is refused -- writing a marker for a malformed anchor would be a note intake
// cannot read.
func pipelineRunOf(rec beadstore.Record) (pipelinectx.Target, error) {
	if rec.ProjectID < 1 {
		return pipelinectx.Target{}, fmt.Errorf("pipeline record %q has no project id", rec.BeadAnchor)
	}
	t, err := pipelinectx.ParseTarget(rec.PipelineTarget)
	if err != nil {
		return pipelinectx.Target{}, fmt.Errorf("pipeline record %q: %w", rec.BeadAnchor, err)
	}
	if _, err := pipelinectx.RunMarker(rec.BeadAnchor, pipelinectx.RunStateDiagnosing); err != nil {
		return pipelinectx.Target{}, err
	}
	return t, nil
}

// The run note's preserved section. The diagnosis goes inside it, so the
// sweep can change the note's status (latest failing pipeline, resolved,
// superseded) and keep the diagnosis exactly as it was validated. The agent's
// text can never contain these (refused as would_forge_marker before render).
const (
	runNoteBodyOpen  = "<!-- gonk:pipeline-body -->"
	runNoteBodyClose = "<!-- /gonk:pipeline-body -->"
)

// runNote is one rendering of a run's note: the controller's status text, the
// preserved body ("" for a lifecycle note with no diagnosis yet) and the
// state the marker carries.
type runNote struct {
	State  string
	Status string
	Body   string
}

func (n runNote) render(anchor string) (string, error) {
	marker, err := pipelinectx.RunMarker(anchor, n.State)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(n.Status))
	if n.Body != "" {
		b.WriteString("\n\n" + runNoteBodyOpen + "\n" + strings.TrimSpace(n.Body) + "\n" + runNoteBodyClose)
	}
	b.WriteString("\n\n" + marker)
	return b.String(), nil
}

// preservedBody returns the fenced body of an existing run note ("" if none).
func preservedBody(note string) string {
	i := strings.Index(note, runNoteBodyOpen)
	if i < 0 {
		return ""
	}
	rest := note[i+len(runNoteBodyOpen):]
	j := strings.Index(rest, runNoteBodyClose)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// terminalRunState: a note in one of these states is never moved back to an
// in-flight one (diagnosing/deferred/denied) or re-diagnosed -- a late
// lifecycle write must not reopen a run the sweep closed.
func terminalRunState(s string) bool {
	return s == pipelinectx.RunStateResolved || s == pipelinectx.RunStateSuperseded
}

// errRunClosed: the run's note is already resolved or superseded; nothing was
// written.
var errRunClosed = errors.New("the run's note is already closed (resolved or superseded); not rewriting it")

// pipelineLander writes one project's landings as the token's own user.
type pipelineLander struct {
	f   pipelineForge
	log *slog.Logger
	me  glab.User
}

func newPipelineLander(ctx context.Context, f pipelineForge, log *slog.Logger) (*pipelineLander, error) {
	if f == nil {
		return nil, errors.New("no pipeline forge configured")
	}
	me, err := f.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("who is the bot: %w", err)
	}
	if me == nil || me.ID == 0 {
		return nil, errors.New("who is the bot: GitLab named no user for this token")
	}
	if log == nil {
		log = slog.Default()
	}
	return &pipelineLander{f: f, log: log, me: *me}, nil
}

// botNote: a note the bot wrote (not GitLab's own system note).
func (l *pipelineLander) botNote(n glab.Note) bool {
	return !n.System && n.Author.ID == l.me.ID
}

// runNoteIn returns the newest bot-authored note in notes carrying a marker
// for exactly anchor, and the state that marker carries.
func (l *pipelineLander) runNoteIn(notes []glab.Note, anchor string) (*glab.Note, string) {
	var found *glab.Note
	state := ""
	for i := range notes {
		n := notes[i]
		if !l.botNote(n) {
			continue
		}
		if states := pipelinectx.RunMarkerStates(n.Body, anchor); len(states) > 0 {
			found, state = &n, states[len(states)-1]
		}
	}
	return found, state
}

// refIssues lists the bot's tracking issues for ref, in ANY state, newest
// (highest iid) first. Bot authorship is re-checked client-side: the
// author_id filter is the server's, and a proxy that dropped it must not let
// a human's issue be adopted.
func (l *pipelineLander) refIssues(ctx context.Context, pid int64, ref string) ([]glab.Issue, error) {
	all, err := l.f.ListIssues(ctx, pid, glab.IssueListOptions{State: "all", AuthorID: l.me.ID})
	if err != nil {
		return nil, err
	}
	marker := pipelinectx.RefIssueMarker(ref)
	var out []glab.Issue
	for _, is := range all {
		if is.Author.ID == l.me.ID && strings.Contains(is.Description, marker) {
			out = append(out, is)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].IID > out[b].IID })
	return out, nil
}

// runLanding is where one run's note lives (or will live).
type runLanding struct {
	target pipelinectx.Target
	// issue is the ref target's tracking issue; nil for an MR target, and nil
	// for a ref target with none yet.
	issue *glab.Issue
	// note is the run's note; nil when there is none yet. state is its marker's.
	note  *glab.Note
	state string
}

// findRunLanding finds the run's note wherever it is. For a ref target it
// looks in EVERY bot tracking issue for the ref (a human may have closed the
// one it is on), and otherwise names the newest OPEN one as the place a new
// note goes (issue nil: none open, one must be created).
func (l *pipelineLander) findRunLanding(ctx context.Context, rec beadstore.Record, t pipelinectx.Target) (runLanding, error) {
	ld := runLanding{target: t}
	if t.Kind == pipelinectx.TargetKindMR {
		notes, err := l.f.ListMRNotes(ctx, rec.ProjectID, t.MRIID)
		if err != nil {
			return ld, fmt.Errorf("read the merge request's notes: %w", err)
		}
		ld.note, ld.state = l.runNoteIn(notes, rec.BeadAnchor)
		return ld, nil
	}
	issues, err := l.refIssues(ctx, rec.ProjectID, t.Ref)
	if err != nil {
		return ld, fmt.Errorf("list the bot's tracking issues: %w", err)
	}
	for i := range issues {
		notes, err := l.f.ListIssueNotes(ctx, rec.ProjectID, issues[i].IID)
		if err != nil {
			return ld, fmt.Errorf("read tracking issue #%d's notes: %w", issues[i].IID, err)
		}
		if n, st := l.runNoteIn(notes, rec.BeadAnchor); n != nil {
			ld.issue, ld.note, ld.state = &issues[i], n, st
			return ld, nil
		}
	}
	for i := range issues {
		if issues[i].State == "opened" {
			ld.issue = &issues[i]
			return ld, nil
		}
	}
	return ld, nil
}

// refIssueTitle is the tracking issue's title.
func refIssueTitle(ref string) string { return "gonk: pipeline failing on " + ref }

// writeRunNote puts n on the run's landing: edits the run's note in place, or
// creates it (and, for a ref target with no open tracking issue, the issue
// first). It returns the landing it wrote to. Writing nothing because the
// note already says exactly this is success. A note already resolved or
// superseded is never rewritten (errRunClosed), unless n is itself one of
// those states.
func (l *pipelineLander) writeRunNote(ctx context.Context, rec beadstore.Record, n runNote) (runLanding, error) {
	t, err := pipelineRunOf(rec)
	if err != nil {
		return runLanding{}, err
	}
	ld, err := l.findRunLanding(ctx, rec, t)
	if err != nil {
		return ld, err
	}
	if ld.note != nil && terminalRunState(ld.state) && !terminalRunState(n.State) {
		return ld, errRunClosed
	}
	body, err := n.render(rec.BeadAnchor)
	if err != nil {
		return ld, err
	}
	if ld.note != nil && ld.note.Body == body {
		ld.state = n.State
		return ld, nil
	}
	pid := rec.ProjectID
	if t.Kind == pipelinectx.TargetKindMR {
		if ld.note != nil {
			_, err = l.f.UpdateMRNote(ctx, pid, t.MRIID, ld.note.ID, body)
		} else {
			ld.note, err = l.f.CreateMRNote(ctx, pid, t.MRIID, body)
		}
		if err != nil {
			return ld, fmt.Errorf("write the run note on merge request !%d: %w", t.MRIID, err)
		}
		ld.state = n.State
		return ld, nil
	}
	if ld.issue == nil {
		is, err := l.f.CreateIssue(ctx, pid, glab.IssueOptions{
			Title:       refIssueTitle(t.Ref),
			Description: refIssueDescription(t.Ref, rec, n.State),
			Labels:      labelPipelineFailing,
		})
		if err != nil {
			return ld, fmt.Errorf("open the tracking issue for %q: %w", t.Ref, err)
		}
		ld.issue = is
		l.log.Info("pipeline: opened the tracking issue for a failing ref",
			"bead", rec.BeadAnchor, "project_id", pid, "issue", is.IID)
	}
	if ld.note != nil {
		_, err = l.f.UpdateIssueNote(ctx, pid, ld.issue.IID, ld.note.ID, body)
	} else {
		ld.note, err = l.f.CreateIssueNote(ctx, pid, ld.issue.IID, body)
	}
	if err != nil {
		return ld, fmt.Errorf("write the run note on tracking issue #%d: %w", ld.issue.IID, err)
	}
	ld.state = n.State
	return ld, nil
}

// classLabelsExcept is every gonk::pipeline-<class> label but keep's ("" =
// all of them).
func classLabelsExcept(keep effects.Classification) []string {
	var out []string
	for _, c := range effects.Classifications() {
		if c != keep {
			out = append(out, pipelineClassLabel(c))
		}
	}
	return out
}

// applyLandingLabels sets the landing's labels and (ref) the tracking issue's
// description for a run in state. class is the diagnosis's ("" when the state
// carries none). BEST EFFORT (D8: after the run note, label and description
// edits WARN only): every failure is logged, none is returned.
func (l *pipelineLander) applyLandingLabels(ctx context.Context, rec beadstore.Record, ld runLanding, state string, class effects.Classification) {
	pid := rec.ProjectID
	var add, remove []string
	switch {
	case state == pipelinectx.RunStateDiagnosed && class != "":
		add, remove = []string{pipelineClassLabel(class)}, classLabelsExcept(class)
	case state == pipelinectx.RunStateResolved:
		remove = classLabelsExcept("")
	}
	if ld.target.Kind == pipelinectx.TargetKindMR {
		if len(add)+len(remove) == 0 {
			return
		}
		if _, err := l.f.UpdateMRLabels(ctx, pid, ld.target.MRIID, add, remove); err != nil {
			l.log.Warn("pipeline: merge request label update failed (the run note is already written)",
				"bead", rec.BeadAnchor, "mr", ld.target.MRIID, "err", err)
		}
		return
	}
	if ld.issue == nil || state == pipelinectx.RunStateSuperseded {
		return // the next run's own note keeps the issue current
	}
	desc := refIssueDescription(ld.target.Ref, rec, state)
	u := glab.IssueUpdate{Description: &desc}
	switch state {
	case pipelinectx.RunStateResolved:
		u.RemoveLabels = append(remove, labelPipelineFailing)
	default:
		u.AddLabels = append([]string{labelPipelineFailing}, add...)
		u.RemoveLabels = remove
	}
	if _, err := l.f.UpdateIssue(ctx, pid, ld.issue.IID, u); err != nil {
		l.log.Warn("pipeline: tracking issue update failed (the run note is already written)",
			"bead", rec.BeadAnchor, "issue", ld.issue.IID, "err", err)
	}
}

// codeSpan renders s as one inline code span that s cannot close early: the
// fence is one backtick longer than the longest run of backticks inside it.
func codeSpan(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 || strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

// refIssueDescription is the tracking issue's description: what it is, the
// controller's status table for the run that last wrote it, and the marker
// gonk-intake and this file find the issue by.
func refIssueDescription(ref string, rec beadstore.Record, state string) string {
	latest := rec.PipelineLatestFailed
	if latest == 0 {
		latest = rec.PipelineID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gonk opened this issue because the pipeline on %s is failing. It keeps this issue "+
		"current, writes one note per run of failures, and closes the issue when the branch passes again. "+
		"gonk changes nothing in the repository and retries nothing.\n\n", codeSpan(ref))
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Current run | %s (%s) |\n", pipelineRef(rec, rec.PipelineID), state)
	fmt.Fprintf(&b, "| First failing pipeline | %s |\n", orUnknown(rec, rec.PipelineFirstFailed))
	fmt.Fprintf(&b, "| Latest failing pipeline | %s |\n", pipelineRef(rec, latest))
	fmt.Fprintf(&b, "| Last success | %s |\n", pipelineRef(rec, rec.PipelineLastSuccess))
	b.WriteString("\n" + pipelinectx.RefIssueMarker(ref))
	return b.String()
}

func orUnknown(rec beadstore.Record, id int64) string {
	if id <= 0 {
		return "unknown"
	}
	return pipelineRef(rec, id)
}

// ---- lifecycle text (zero tokens: controller-written, D9) ----------------

const pipelineNoteFooter = "This note is updated in place. gonk changes nothing in the repository and retries nothing: " +
	"what it writes here is advice for a human."

func diagnosingStatus(rec beadstore.Record, attempt int, rung string) string {
	return fmt.Sprintf("gonk is diagnosing why %s failed (attempt %d, using %s).\n\n%s",
		pipelineRef(rec, rec.PipelineID), max(attempt, 1), rungText(rung), pipelineNoteFooter)
}

func deferredStatus(rec beadstore.Record, reason, detail string, retryAfter time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gonk will diagnose why %s failed, but cannot yet.", pipelineRef(rec, rec.PipelineID))
	if s := deferReasonText(reason); s != "" {
		b.WriteString(" " + s)
	}
	if d := strings.TrimSpace(detail); d != "" {
		b.WriteString("\n\nDetails: " + d)
	}
	if !retryAfter.IsZero() {
		fmt.Fprintf(&b, "\n\nIt will try again after %s.", plainUTC(retryAfter))
	} else {
		b.WriteString("\n\nIt will try again automatically.")
	}
	b.WriteString("\n\n" + pipelineNoteFooter)
	return b.String()
}

func deniedStatus(rec beadstore.Record, reason, detail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gonk has stopped and will not diagnose why %s failed. %s",
		pipelineRef(rec, rec.PipelineID), denyReasonText(reason))
	if d := strings.TrimSpace(detail); d != "" {
		b.WriteString("\n\nDetails: " + d)
	}
	b.WriteString("\n\nA human is needed: gonk will not retry this run on its own.\n\n" + pipelineNoteFooter)
	return b.String()
}

// diagnosedStatus LEADS with diagnosedHeading: two notes on one tracking
// issue are told apart by their first line (gonk-385g).
func diagnosedStatus(rec beadstore.Record) string {
	latest := rec.PipelineLatestFailed
	if latest == 0 {
		latest = rec.PipelineID
	}
	var b strings.Builder
	b.WriteString(diagnosedHeading(rec) + "\n\n")
	fmt.Fprintf(&b, "- Latest failing pipeline in this run: %s\n", pipelineRef(rec, latest))
	fmt.Fprintf(&b, "- Last success before this run: %s\n", pipelineRef(rec, rec.PipelineLastSuccess))
	if rec.PipelineRunFixMR > 0 {
		// The fix merge request gonk opened for this run, and where it
		// stands (gonk-2usw plan 5.4 step 4; written by the exit check).
		b.WriteString(fixLineText(rec.PipelineRunFixMR, rec.PipelineRunFixState))
	}
	b.WriteString("\n")
	b.WriteString("Later pipelines that fail with the same failing jobs belong to this run and are not " +
		"diagnosed again until the target passes.\n\n" + pipelineNoteFooter)
	return b.String()
}

// resolvedStatus and supersededStatus REPLACE the note's status, heading
// included, so each names both pipelines: the one that closed the run and the
// one the note (and the diagnosis kept below it) is about.
func resolvedStatus(rec beadstore.Record, green glab.Pipeline) string {
	when := green.UpdatedAt
	if green.FinishedAt != nil {
		when = *green.FinishedAt
	}
	at := ""
	if !when.IsZero() {
		at = ", " + plainUTC(when)
	}
	return fmt.Sprintf("Resolved: %s passed at commit %s%s. gonk's diagnosis of %s is kept below.",
		pipelineRef(rec, green.ID), codeSpan(shortSHA(green.SHA)), at, pipelineRef(rec, rec.PipelineID))
}

func supersededStatus(rec beadstore.Record, next int64) string {
	return fmt.Sprintf("Superseded: %s failed with a different set of failing jobs, which gonk treats "+
		"as a new run. Anything below is about the earlier failure, %s.", pipelineRef(rec, next), pipelineRef(rec, rec.PipelineID))
}

// renderDiagnosisBody is the preserved section of a diagnosed note: the D7a
// fields, every evidence item quoted, the cited files, and what gonk did not
// do.
//
// The summary and the remedy are the MODEL's words, seeded by a job log
// anyone who can push may write. They are QUOTED (fence, pipefix_render.go):
// until 2026-10-12 they were written into the note as they came, and GitLab
// made what it found in them -- `@all` notified every member, `group/p#12`
// left a "mentioned in" on an issue of another project the bot can see
// (gonk-d2ct). Not inert: a note is not searched for closing patterns, and
// inert's rewriting would misname what the summary names. Evidence is rendered as `- <job>: <code span>` so no rendered line can
// begin with a slash (a log line starting with "/" survives the quick-action
// gate; TestALogLineBeginningWithASlashSurvivesTheCommentGate). <job> is the
// job's name in a code span, linked to the job's page when the record knows
// its id (evidenceJobRef, gonk-385g).
func renderDiagnosisBody(e effects.Effect, rec beadstore.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Classification:** %s\n\n", codeSpan(string(e.Classification)))
	b.WriteString("**What failed and why**\n\n" + fence(strings.TrimSpace(e.Summary)) + "\n\n") // diagnosis:quote-summary
	b.WriteString("**Evidence**\n\n")
	for _, ev := range e.Evidence {
		job := "(pipeline)"
		if ev.Job != "" {
			job = evidenceJobRef(rec, ev.Job)
		}
		if ev.Line != "" {
			fmt.Fprintf(&b, "- %s: %s\n", job, codeSpan(pipelinectx.Norm(ev.Line)))
		} else {
			fmt.Fprintf(&b, "- %s: the %s fact gonk supplied\n", job, codeSpan(string(ev.Fact)))
		}
	}
	b.WriteString("\n**Suggested remedy**\n\n" + fence(strings.TrimSpace(e.Remedy)) + "\n") // diagnosis:quote-remedy
	if len(e.Files) > 0 {
		fmt.Fprintf(&b, "\n**Files cited** (each exists at %s)\n\n", codeSpan(shortSHA(rec.PipelineSHA)))
		for _, f := range e.Files {
			fmt.Fprintf(&b, "- %s\n", codeSpan(f))
		}
	}
	fmt.Fprintf(&b, "\n_Diagnosed on attempt %d using %s. Every quoted line was checked against the job's log._",
		max(rec.Attempt, 1), rungText(rec.Rung))
	return b.String()
}
