package main

// gonk-37kg part D: ONE rule for text gonk did not write (the model's
// summary, remedy, rationale and scope reason; the project's job and branch
// names where they cannot go through ident). The rule is in
// docs/reference/gonk-pipeline-fix.md, "How untrusted text is rendered".
// Every name and URL here is invented.

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
)

// ---- the quote (gonk-2061, gonk-d2ct) -----------------------------------------------

var (
	// closesAFence: a line CommonMark reads as closing a backtick fence -- at
	// most three spaces, three or more backticks, nothing else.
	closesAFence = regexp.MustCompile("^ {0,3}`{3,}[ \t]*$")
	// opensAFence: a line that would open a block of its own once a fence is
	// closed (any info string).
	opensAFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
)

// quoteBody is the lines between a quote's own two fence lines; it fails
// unless the fence is exactly three backticks, opened with `text`.
func quoteBody(t *testing.T, what, q string) []string {
	t.Helper()
	lines := strings.Split(q, "\n")
	if len(lines) < 2 || lines[0] != "```text" || lines[len(lines)-1] != "```" {
		t.Fatalf("%s: not a three-backtick `text` fence:\n%s", what, q)
	}
	return lines[1 : len(lines)-1]
}

// hostileProse is text shaped to end its fence, under either reading of one.
var hostileProse = []string{
	"```\n/close",
	"````\n/close\n````",
	"`````",
	"  ```\n@all",
	"   ````\n@all",
	"\t```",
	"```text",
	"``` \t",
	"before\n```\nafter\n```\n```\n/label ~x",
	"a\r```\r/close",
	"a\r\n```\r\n/close\r\n",
	"~~~\n```\n~~~",
	"```\n```\n```\n```",
	"````text\nx\n```\ny\n````",
}

func TestQuoteIsAThreeBacktickFenceItsTextCannotClose(t *testing.T) {
	for _, in := range hostileProse {
		q := fence(in)
		body := quoteBody(t, in, q)
		if strings.Contains(q, "\r") {
			t.Errorf("fence(%q) keeps a carriage return, which CommonMark reads as a line ending", in)
		}
		for _, l := range body {
			// The CommonMark reading: the block ends at the first closing fence.
			if closesAFence.MatchString(l) {
				t.Errorf("fence(%q): the line %q closes the fence", in, l)
			}
			// The exact-three reading (a line of exactly three backticks):
			if strings.TrimRight(l, " \t") == "```" {
				t.Errorf("fence(%q): a line of exactly three backticks inside the quote", in)
			}
		}
		// What is shown is the text: taking the four spaces off the lines
		// that were moved gives the input back, line endings normalised.
		var back []string
		for _, l := range body {
			if strings.HasPrefix(l, "    ") && opensAFence.MatchString(strings.TrimLeft(l, " \t")) {
				l = l[4:]
			}
			back = append(back, l)
		}
		want := strings.TrimRight(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(in), "\n")
		if got := strings.Join(back, "\n"); got != want {
			t.Errorf("fence(%q) shows %q, want %q", in, got, want)
		}
	}
}

// THE NEAR MISSES: ordinary prose is not touched at all, and a summary that
// quotes code in a fence of its own is RENDERED, not refused -- its fence
// lines moved four spaces right, every other byte as written.
func TestQuoteLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"The constant was 100 instead of 1000.",
		"two\nlines",
		"inline `code`, ``two``, and ``` in the middle of a line",
		"    indented code\n\tand a tab",
		"~~~\ntildes do not close a backtick fence\n~~~",
		"`` two at the start",
		"- a list\n- of things\n\n> and a quote",
		"/not-a-quick-action-here: the gate decides that, not the quote",
	} {
		if got, want := fence(in), "```text\n"+in+"\n```"; got != want {
			t.Errorf("fence(%q) = %q, want the text unchanged in a fence", in, got)
		}
	}
	in := "The test expects 4:\n```go\nif got != 4 {\n```\nand the code returns 3."
	want := "```text\nThe test expects 4:\n    ```go\nif got != 4 {\n    ```\nand the code returns 3.\n```"
	if got := fence(in); got != want {
		t.Errorf("a summary with fenced code:\n got %q\nwant %q", got, want)
	}
}

// ---- the diagnosis note (gonk-d2ct) -------------------------------------------------

// liveInANote is what GitLab materialises from a note's text outside code: a
// mention, an issue or merge request reference (bare or cross-project), a
// label, a milestone, a snippet, a commit range, a URL.
var liveInANote = regexp.MustCompile(`@[A-Za-z]|[\w/.-]*#\d|[\w/.-]*!\d|~\w|%\w|\$\d|https?://`)

func TestDiagnosisNoteQuotesTheModelsSummaryAndRemedy(t *testing.T) {
	if !liveInANote.MatchString("@all") || !liveInANote.MatchString("group/proj#12") || liveInANote.MatchString("plain words, 100% sure") {
		t.Fatal("the test's own pattern is wrong; the assertions below would be vacuous")
	}
	rec := linkRec()
	d := plDiagnosis(nil).Effects[0]
	d.Summary = "@all please look: group/other-project#12 and !5 broke it, see https://attacker.example.invalid/x ~urgent %v1 $3"
	d.Remedy = "ask @root\n```\n@everyone other/group/proj!77\n```\n/cc @maintainers"
	body := renderDiagnosisBody(d, rec)
	out := faOutsideFences(body)
	// Vacuity: the text IS in the note, and the test's fence reader removes it.
	for _, s := range []string{"@all please look", "group/other-project#12", "@everyone other/group/proj!77", "/cc @maintainers"} {
		if !strings.Contains(body, s) {
			t.Fatalf("the note does not carry %q at all:\n%s", s, body)
		}
		if strings.Contains(out, s) {
			t.Errorf("%q is rendered OUTSIDE a fence, where GitLab materialises it:\n%s", s, body)
		}
	}
	// Outside the fences only gonk's own words remain: its links and code
	// spans. No mention, and no reference but the ones gonk wrote.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "@") {
			t.Errorf("a line outside the fences holds an @: %q", line)
		}
	}
	for _, s := range []string{d.Summary, d.Remedy} {
		for _, m := range liveInANote.FindAllString(s, -1) {
			if strings.Contains(out, m) && !strings.Contains(renderDiagnosisBody(plDiagnosis(nil).Effects[0], rec), m) {
				t.Errorf("%q from the model's text is outside the fences", m)
			}
		}
	}
	// Both quotes are the three-backtick fence, labelled, in their sections.
	for _, q := range []string{fence(strings.TrimSpace(d.Summary)), fence(strings.TrimSpace(d.Remedy))} {
		if !strings.Contains(body, "\n\n"+q+"\n") {
			t.Errorf("the note does not hold the quote\n%s\nin:\n%s", q, body)
		}
	}
	if !strings.Contains(body, "**What failed and why**") || !strings.Contains(body, "**Suggested remedy**") {
		t.Errorf("the sections are gone:\n%s", body)
	}
	// The remedy's own fence did not end the quote.
	for _, l := range quoteBody(t, "remedy", fence(strings.TrimSpace(d.Remedy))) {
		if closesAFence.MatchString(l) {
			t.Errorf("the remedy closes its quote at %q", l)
		}
	}
}

// THE NEAR MISS, through the real gate: a diagnosis whose summary quotes code
// in a fence and names people and issues is APPLIED -- quoted, not refused.
func TestADiagnosisWithFencedCodeAndReferencesIsAppliedQuoted(t *testing.T) {
	r := newPLRun(t, plOpts{})
	summary := "The assertion in `TestSum` expects 4 (see #12, cc @someone):\n```go\nif got != 4 {\n```\nand sum() returns 3."
	applied, v, err := r.apply(t, plDiagnosis(func(e *effects.Effect) { e.Summary = summary }), io.Discard)
	if !applied {
		t.Fatalf("a summary with fenced code and a mention was refused: %q %v", v, err)
	}
	notes := r.gl.IssueNotesOf(r.pid, 1)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, fence(summary)) {
		t.Fatalf("the note does not quote the summary: %+v", notes)
	}
	if out := faOutsideFences(notes[0].Body); strings.Contains(out, "@someone") || strings.Contains(out, "see #12") {
		t.Fatalf("the summary's mention or reference is outside the fence:\n%s", notes[0].Body)
	}
}

// THE GATE IS THE COMMENT GATE, NOT THE FENCE (gonk-2061): a line of model
// text that reads as a quick action is refused wherever it stands -- inside
// the quote, after a fence line of the model's own, after a carriage return.
// Whether GitLab's quick-action extractor skips a fenced block is not relied
// on anywhere.
func TestAQuickActionLineInModelTextIsRefusedWhateverTheFence(t *testing.T) {
	for name, text := range map[string]string{
		"plain":                      "It failed.\n/close",
		"after a fence of its own":   "It failed.\n```\n/close\n```",
		"after a longer fence":       "It failed.\n````\n/label ~bug\n````",
		"inside tildes":              "~~~\n/assign @root\n~~~",
		"after a carriage return":    "It failed.\r/close",
		"indented":                   "It failed.\n   /close",
		"after a fence that is long": "`````\n/close",
	} {
		for _, field := range []string{"summary", "remedy"} {
			r := newPLRun(t, plOpts{})
			applied, v, _ := r.apply(t, plDiagnosis(func(e *effects.Effect) {
				if field == "summary" {
					e.Summary = text
				} else {
					e.Remedy = text
				}
			}), io.Discard)
			if applied || !strings.Contains(v, "quick") {
				t.Errorf("%s in the %s: applied=%v violation=%q, want the quick-action refusal", name, field, applied, v)
			}
			if n := r.gl.IssueNotesOf(r.pid, 1); len(n) != 0 {
				t.Errorf("%s in the %s: a note was written", name, field)
			}
		}
		// And for the fix agent's reason, at gate A8.
		if ref := textGate("fix_meta reason", text); ref == nil || ref.Code() != string(ruleTextComment) {
			t.Errorf("%s in a fix reason: %v, want %s", name, ref, ruleTextComment)
		}
	}
	// The near miss: a slash that is not at the start of a line, and a path.
	for _, text := range []string{"1/2 of the tests failed", "see a/b and c /d", "the file is at\n./calc/sum.py", "ratio\n/ 2"} {
		if ref := textGate("fix_meta reason", text); ref != nil {
			t.Errorf("textGate(%q) = %v, want accepted", text, ref)
		}
	}
}

// ---- the title and the commit message (gonk-vu0n) -----------------------------------

func titleFor(job, branch string, more int) *fixApply {
	rec := beadstore.Record{BeadAnchor: "gonk:1:pipefix:ref-0d6e4079:f082455952b2ced7:s0", Attempt: 1}
	rec.PipelineFix.TargetBranch = branch
	rec.PipelineFix.Targets = []beadstore.PipelineFixTarget{{Name: job}}
	for i := 0; i < more; i++ {
		rec.PipelineFix.Targets = append(rec.PipelineFix.Targets, beadstore.PipelineFixTarget{Name: "other"})
	}
	return &fixApply{rec: rec}
}

// closingJobNames are job (or branch) names that, in the title, read as a
// closing statement: a verb, then a URL or a tracker key. refSafe already
// takes `#`, `!` and `@` out; these need neither.
var closingJobNames = []string{
	"closes https://gitlab.example.invalid/g/p/-/issues/12",
	"fixes ABC-12",
	"fix UTF-8 names",
	"resolves: TRACK_2-9",
	"Implements http://tracker.example.invalid/browse/X-1",
	"closing issue https://gitlab.example.invalid/g/p/-/issues/3",
	"fixed  ab-1",
	"x closes group/project#12",
	"resolve https://a.invalid resolve https://b.invalid fixes K-1 fixes K-2",
}

func TestFixTitleAndCommitMessageCarryNoClosingPattern(t *testing.T) {
	if !closingPattern.MatchString("gonk: fix job fixes ABC-12 on main") {
		t.Fatal("fixture: the title this test is about would not match the closing pattern")
	}
	for _, name := range closingJobNames {
		for what, a := range map[string]*fixApply{
			"as the job":    titleFor(name, "main", 0),
			"as the branch": titleFor("unit-tests", name, 0),
			"with more":     titleFor(name, name, 2),
			"skip ci":       {rec: titleFor(name, "main", 0).rec, skip: true},
		} {
			title, msg := a.title(), a.commitMessage()
			for pn, re := range map[string]*regexp.Regexp{"closingPattern": closingPattern, "the test's pattern": faClosing} {
				if m := re.FindString(title); m != "" {
					t.Errorf("%q %s: %s matches %q in the title %q", name, what, pn, m, title)
				}
				if m := re.FindString(msg); m != "" {
					t.Errorf("%q %s: %s matches %q in the commit message %q", name, what, pn, m, msg)
				}
			}
			if strings.Contains(msg, "://") || strings.ContainsAny(msg, "#!@") {
				t.Errorf("%q %s: the commit message can still spell a URL, a reference or a mention: %q", name, what, msg)
			}
			if !strings.HasPrefix(msg, title+"\n\n") || strings.Contains(title, "\n") {
				t.Errorf("%q %s: the title is not the commit's first line", name, what)
			}
			if err := a.titleInvariant(); err != nil {
				t.Errorf("%q %s: the invariant refuses what the rendering made safe: %v", name, what, err)
			}
		}
	}
}

// THE NEAR MISSES: a job that merely has "fix" or "fixes" in its name, or a
// digit after a hyphen, or a colon, is named in the title EXACTLY. Only a
// name that would read as a closing statement is changed, and only there.
func TestFixTitleNamesOrdinaryJobsExactly(t *testing.T) {
	for _, name := range []string{
		"lint-fixes", "fixes", "apply fixes", "fix", "hotfixes v2-1", "unit-tests-3.12", "build:linux", "test:unit 3/5",
		"release-2026", "PY-3 tests", "python-3-11", "smoke-pipefix-20261008t025825z", "closes", "resolve deps", "TRACK-7",
		"implements_interface", "fix_UTF-8", "prefix UTF-8",
	} {
		if got, want := titleFor(name, "main", 0).title(), "gonk: fix job "+name+" on main"; got != want {
			t.Errorf("title for job %q = %q, want %q", name, got, want)
		}
		if got, want := titleFor("unit-tests", name, 0).title(), "gonk: fix job unit-tests on "+strings.ReplaceAll(name, " ", " "); got != want {
			t.Errorf("title for branch %q = %q, want %q", name, got, want)
		}
	}
	// What a defused name looks like: the smallest visible change, inert's.
	for name, want := range map[string]string{
		"fixes ABC-12":    "gonk: fix job fixes ABC- 12 on main",
		"fix UTF-8 names": "gonk: fix job fix UTF- 8 names on main",
		"closes https://gitlab.example.invalid/g/p/-/issues/12": "gonk: fix job closes https: //gitlab.example.invalid/g/p/-/issues/12 on main",
		// A URL with no closing verb is still a reference GitLab would link
		// and cross-reference from a title: `://` never survives.
		"docs https://example.invalid/x": "gonk: fix job docs https: //example.invalid/x on main",
	} {
		if got := titleFor(name, "main", 0).title(); got != want {
			t.Errorf("title for job %q = %q, want %q", name, got, want)
		}
	}
}

// The invariant is checked on what is about to be SENT, before any write: a
// title or commit message that still matched would be ours, and stops the
// apply.
func TestFixApplyChecksTheTitleInvariantBeforeWriting(t *testing.T) {
	a := titleFor("unit-tests", "main", 0)
	if err := a.titleInvariant(); err != nil {
		t.Fatalf("an ordinary title: %v", err)
	}
	if err := checkNoClosingPattern("commit message", "gonk: fix job x on main\n\nCloses #12"); err == nil || !strings.Contains(err.Error(), "commit message") {
		t.Errorf("a closing pattern in a commit message: %v", err)
	}
	if err := checkNoClosingPattern("title", "gonk: fix job fixes ABC-12 on main"); err == nil {
		t.Errorf("a closing pattern in a title passes the invariant")
	}
	// Each of the two texts is looked at, and named in the refusal: a title
	// title() can no longer produce, and a commit message that closes while
	// its title does not.
	const ok, closes = "gonk: fix job unit-tests on main", "gonk: fix job fixes ABC-12 on main"
	if err := titleInvariantOf(ok, ok+"\n\ngonk-fix: x attempt=1"); err != nil {
		t.Errorf("two ordinary texts: %v", err)
	}
	if err := titleInvariantOf(closes, ok); err == nil || !strings.Contains(err.Error(), "merge request title") {
		t.Errorf("a closing title: %v, want a refusal naming the title", err)
	}
	if err := titleInvariantOf(ok, ok+"\n\nResolves https://tracker.example.invalid/X-1"); err == nil || !strings.Contains(err.Error(), "commit message") {
		t.Errorf("a closing commit message under an ordinary title: %v, want a refusal naming the commit message", err)
	}
	// What the apply asks about is what it SENDS. title() can no longer be
	// made to close, so the text is smuggled in through the one other thing
	// the commit message is built from: the record's anchor (the
	// controller's own value; no project or model writes it).
	smuggled := titleFor("unit-tests", "main", 0)
	smuggled.rec.BeadAnchor = "x\n\nCloses #12"
	if !strings.Contains(smuggled.commitMessage(), "Closes #12") {
		t.Fatal("fixture: the anchor is not in the commit message; the check below would be vacuous")
	}
	if err := smuggled.titleInvariant(); err == nil || !strings.Contains(err.Error(), "commit message") {
		t.Errorf("a commit message that would close: titleInvariant = %v, want a refusal naming the commit message", err)
	}
}

// Through the real apply: a project whose failing job is NAMED like a closing
// statement still gets its fix merge request, and neither the title nor the
// commit closes anything.
func TestAFixForAJobNamedLikeAClosingStatementIsOpenedAndClosesNothing(t *testing.T) {
	for _, job := range closingJobNames[:3] {
		w := newFAWorld(t, faOpts{})
		rec, _ := w.fixRecord()
		rec.PipelineFix.Targets[0].Name = job
		if err := w.store.Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		if applied, v, _, err := w.apply(faGood()); !applied {
			t.Fatalf("job %q: the fix was not applied: %q %v\n%s", job, v, err, w.log)
		}
		mr := w.fixMRs()[0]
		commits := 0
		for _, wr := range w.written() {
			if !strings.HasSuffix(wr.Path, "/repository/commits") {
				continue
			}
			commits++
			var c glab.CommitOptions
			_ = json.Unmarshal([]byte(wr.Body), &c)
			for _, text := range []string{mr.Title, c.CommitMessage} {
				if faClosing.MatchString(text) || closingPattern.MatchString(text) || strings.Contains(text, "://") {
					t.Errorf("job %q: %q can close or link", job, text)
				}
			}
		}
		if commits != 1 {
			t.Fatalf("job %q: %d commits", job, commits)
		}
		// The description still names the job exactly (ident).
		if !shownNames(mr.Description)[job] {
			t.Errorf("job %q: the description does not show the name exactly", job)
		}
	}
}

// The description's own invariant (render:quick): were gate A8 ever bypassed,
// a description with a quick-action line of the agent's is not sent.
func TestFixDescriptionIsCheckedForAQuickActionLine(t *testing.T) {
	w := newFAWorld(t, faOpts{})
	rec, _ := w.fixRecord()
	facts := fixFacts{commit: plSHA, landing: runLanding{issue: &glab.Issue{IID: 1}}}
	good := &fixApply{rec: rec, plan: effects.FixPlan{Reason: "Total skipped the first element; 1/2 of the cases failed."}}
	if desc, err := good.description(facts); err != nil || !strings.Contains(desc, "1/2 of the cases") {
		t.Fatalf("an ordinary description: %v", err)
	}
	for _, reason := range []string{"It failed.\n/close", "```\n/assign @root", "x\r/label ~y"} {
		bad := &fixApply{rec: rec, plan: effects.FixPlan{Reason: reason}}
		if desc, err := bad.description(facts); err == nil || !strings.Contains(err.Error(), "quick action") || desc != "" {
			t.Errorf("reason %q: description = %q, %v; want the quick-action refusal and no text", reason, desc, err)
		}
	}
}
