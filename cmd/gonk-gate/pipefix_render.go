package main

// WHAT A FIX LOOKS LIKE ON GITLAB (gonk-2usw plan 5.4 step 3, 8.1): the fix
// merge request's description and the fix note's texts for the states the
// apply decides. Every word is the controller's except the agent's rationale
// and scope request, which are rendered ONLY after gate A8, inside a fence
// they cannot close, and labelled as the agent's.
//
// THE DESCRIPTION NEVER CLOSES ANYTHING. It links the diagnosis with
// `Related to`: the tracking issue is closed by run maintenance when the
// target is green, and a fix that reveals a further failure must not close it
// (plan 5.4). GitLab finds closing patterns by scanning the RAW text, so a
// fence is no protection: the agent's prose is made unable to form a
// reference (inert), every NAME is written so that the raw text holds none of
// the characters a reference is spelled with (ident), the title and the
// commit message use refSafe and defuseClosing, and the finished title,
// commit message and description are each checked against the pattern before
// anything is sent (checkNoClosingPattern).

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/cidef"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/pipefix"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// closingPattern approximates GitLab's default issue closing pattern: a
// closing verb, then an issue reference (`#12`, `group/project#12`, a URL, or
// an external tracker's key such as ABC-12). GitLab SCANS THE RAW TEXT of a
// merge request's title and description and of its commit messages for it --
// a fence or a code span does not hide a match -- and closes what it finds
// when the merge request merges.
var closingPattern = regexp.MustCompile(`(?i)\b(?:clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b:?\s+` +
	`(?:issues?\s+)?(?:[\w./-]*#\d|https?://|[A-Z][A-Z0-9_]+-\d)`)

// inert makes PROSE gonk did not write unable to form an issue reference, by
// the smallest visible change: a space after a `#` that precedes a digit
// ("#12" -> "# 12"), after the `:` of a URL scheme ("https: //") and after
// the hyphen of a tracker key ("ABC- 12"; in either case, because an external
// tracker's key may be lower-case). It is for text that is READ -- the
// agent's rationale, shown fenced -- and it is not reversible and is not
// meant to be. The title and the commit message use refSafe, which is
// stricter.
//
// IT IS NOT FOR A NAME (gonk-htl3). Until 2026-10-07 every job name, branch,
// path, error signature, rung and session name in the description went
// through it too, and the tracker-key rule rewrote each one that had a digit
// after a hyphen: the description named the job `smoke-pipefix- 2026...`,
// which does not exist, the signature `es1- 3404...`, the model
// `qwen3- 6-35b`. A name is rendered by ident, which changes nothing a
// person sees.
func inert(s string) string {
	s = inertHash.ReplaceAllString(s, "# $1")
	s = strings.ReplaceAll(s, "://", ": //")
	return inertKey.ReplaceAllString(s, "$1- $2")
}

var (
	inertHash = regexp.MustCompile(`#(\d)`)
	inertKey  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]+)-(\d)`)
)

// ident renders a NAME -- a job, a stage, a branch, a path, an error
// signature, a rung, a session, an anchor -- as inline code that shows it
// EXACTLY and that GitLab can read nothing into.
//
// Every rune of the name that is not an ASCII letter, an ASCII digit or a
// space is written as a numeric character reference (`-` is `&#x2d;`, `#` is
// `&#x23;`), between <code> and </code>. GitLab's markdown decodes a
// character reference to the literal character and gives it no meaning as
// markup, so the name is displayed as it is, character for character --
// while the RAW text, which is what GitLab scans for closing patterns and
// what a reference would have to be spelled in, holds no `#`, no `!`, no
// `://`, no hyphen, no `@`, no `|`, no backtick and no `<` of the name's own.
// A job called `closes #12`, `fix-1234` or `TRACK-7` therefore closes
// nothing, links nothing and is not rewritten; nor can a name end its code
// element, split a table cell or start emphasis.
//
// It is at least as strong as inert was for the same strings: inert left a
// name's `#` and `-` in the raw text with a space after them; ident leaves
// neither. The finished description is still checked against closingPattern
// before it is sent.
func ident(s string) string {
	var b strings.Builder
	b.WriteString("<code>")
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' { // ident:plain
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "&#x%x;", r)
	}
	b.WriteString("</code>")
	return b.String()
}

// fixRungText names the rung in the description: rungText's words, with the
// rung's own name through ident.
func fixRungText(rung string) string {
	if strings.TrimSpace(rung) == "" {
		return "the configured model"
	}
	return "model " + ident(rung)
}

// fixFacts is what the description states that is not on the record yet.
type fixFacts struct {
	commit  string
	state   pipefix.State
	delta   cidef.Kind
	weak    weakeningFact
	landing runLanding
}

// fence QUOTES text gonk did not write -- the model's summary, remedy,
// rationale and scope reason -- as a code block the text cannot end. It is
// the one rendering of untrusted prose (the rule is written out in
// docs/reference/gonk-pipeline-fix.md, "How untrusted text is rendered"):
//
//   - The fence is ALWAYS three backticks. Until 2026-10-12 it grew to be
//     longer than any run of backticks in the text, which CommonMark honours
//     -- but whether GitLab's quick-action extractor does was assumed, not
//     known (gonk-2061): read as "a block ends at a line of exactly three
//     backticks", a four-backtick fence is not a block at all, and a
//     three-backtick line of the text's own ends it early.
//   - Every line of the text that begins (after spaces or tabs) with three
//     or more backticks is moved FOUR SPACES right. CommonMark lets a closing
//     fence be indented by at most three, so such a line cannot close the
//     block; and it is no longer a line of exactly three backticks either.
//     Both readings of a fence then agree on where this block ends: at
//     gonk's own last line. The change is visible and small -- a summary
//     that quotes code in a fence of its own shows that fence indented --
//     and nothing is refused for it.
//   - Carriage returns become line feeds first: CommonMark ends a line at a
//     lone CR, so "x\r```" would otherwise hide a fence line from the
//     line-by-line reading here, and "x\r/close" a quick action from the
//     comment gate (which reads what this returns, or untrustedLines).
//
// WHAT THE QUOTE IS RELIED ON FOR, and what it is not. Inside a code block
// GitLab renders no mention, no issue or merge request reference, no label
// and no link: those are made from the RENDERED document, and this is a code
// block under CommonMark whatever the text holds. It is NOT relied on to stop
// a quick action -- the comment gate refuses any line of the text that reads
// as one, fence or no fence (effects.ValidateComments; textGate for a fix,
// step 8 for a diagnosis) -- and NOT to stop a closing pattern, which GitLab
// finds in the raw text of a merge request's description: that is inert and
// the closingPattern check, for the one quote that lands in a description.
func fence(text string) string {
	lines := strings.Split(strings.TrimRight(untrustedLines(text), "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") { // quote:fence-line
			lines[i] = "    " + line
		}
	}
	return "```text\n" + strings.Join(lines, "\n") + "\n```"
}

// untrustedLines gives untrusted text the line endings it is rendered and
// gated with: CRLF and a lone CR are LF.
func untrustedLines(text string) string {
	return strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
}

// defuseClosing makes a controller-built line of PLAIN text (the title, the
// commit message's first line) unable to close an issue, where it holds a
// project-chosen name that cannot go through ident: a title is not markdown.
// Only what closingPattern matches is changed, and by inert's smallest
// visible change ("fixes ABC- 12", "closes https: //..."), so a job called
// `unit-tests-3.12` or `lint-fixes` is named exactly -- the opposite choice
// to inert's own, which rewrites every key-shaped word of the agent's prose
// because prose need not be exact and a name must be (gonk-htl3, gonk-vu0n).
func defuseClosing(s string) string {
	for i := 0; i < 4 && closingPattern.MatchString(s); i++ { // title:defuse
		s = closingPattern.ReplaceAllStringFunc(s, inert)
	}
	return s
}

// checkNoClosingPattern is THE INVARIANT on text about to be sent where
// GitLab looks for closing patterns -- a merge request's title and
// description, a commit message: nothing in it can close an issue. A match
// is OURS (some text got there without inert, ident or defuseClosing), and
// nothing is written with it.
func checkNoClosingPattern(what, text string) error {
	if m := closingPattern.FindString(text); m != "" { // render:closing
		return fmt.Errorf("the fix %s would carry a closing pattern (%q); not writing it", what, m)
	}
	return nil
}

// titleInvariant checks the title and the commit message before the first
// write (gonk-vu0n: until 2026-10-12 only the description was checked).
func (a *fixApply) titleInvariant() error {
	return titleInvariantOf(a.title(), a.commitMessage())
}

// titleInvariantOf is titleInvariant over two given texts. It is separate so
// a test can hand it a title that title() itself can no longer produce.
func titleInvariantOf(title, message string) error {
	if err := checkNoClosingPattern("merge request title", title); err != nil { // title:invariant
		return err
	}
	return checkNoClosingPattern("commit message", message) // commit:invariant
}

// fixStatusLine is the one sentence of the status block.
func fixStatusLine(state pipefix.State, commit string) string {
	if state == pipefix.StateAwaitingRun {
		return "**Waiting for a maintainer.** This change edits the pipeline definition, so gonk committed it with `[skip ci]`: " +
			"review it, then start this merge request's pipeline. gonk judges the pipeline you start."
	}
	return fmt.Sprintf("**Verifying.** gonk is waiting for this merge request's pipeline on commit %s.", codeSpan(shortSHA(commit)))
}

// relatedLine links the diagnosis: `Related to #<issue>` for a ref target's
// tracking issue, `Related to !<mr>` for a merge request target. NEVER a
// closing keyword.
func relatedLine(ld runLanding) (string, error) {
	switch {
	case ld.target.Kind == pipelinectx.TargetKindMR && ld.target.MRIID > 0:
		return fmt.Sprintf("Related to !%d", ld.target.MRIID), nil // render:related-mr
	case ld.issue != nil && ld.issue.IID > 0:
		return fmt.Sprintf("Related to #%d", ld.issue.IID), nil // render:related-issue
	}
	return "", fmt.Errorf("the diagnosis has no landing to link the fix to")
}

// description renders the fix merge request's description.
func (a *fixApply) description(f fixFacts) (string, error) {
	pf := a.rec.PipelineFix
	marker, err := pipelinectx.FixMRMarker(a.rec.BeadAnchor, pf.CauseKey)
	if err != nil {
		return "", err
	}
	related, err := relatedLine(f.landing)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(fixStatusOpen + "\n" + fixStatusLine(f.state, f.commit) + "\n" + fixStatusClose + "\n\n")
	fmt.Fprintf(&b, "gonk proposes this change for the failure it diagnosed in %s on %s.\n\n",
		pipelineRef(a.rec, pf.BasePipelineID), ident(pf.TargetBranch))
	b.WriteString(related + "\n\n")

	b.WriteString("### What decides whether this worked\n\n")
	b.WriteString("In this merge request's pipeline, every job below must still RUN and must PASS, under the same job definition " +
		"as before. gonk checks that itself. A green pipeline is not what is checked: a different job failing does not " +
		"make this fix wrong, and a job that was removed, skipped or allowed to fail does not make it right.\n\n")
	b.WriteString("| Job | Stage | Failure it fixes (signature) |\n|---|---|---|\n")
	for _, t := range pf.Targets {
		sig := t.ErrorSignature
		if sig == "" {
			sig = "none recorded"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", ident(t.Name), ident(t.Stage), ident(sig))
	}
	if len(pf.AlsoFailing) > 0 {
		names := make([]string, 0, len(pf.AlsoFailing))
		for _, n := range pf.AlsoFailing {
			names = append(names, ident(n))
		}
		b.WriteString("\nAlso failing in that pipeline and NOT addressed here: " + strings.Join(names, ", ") + ".\n")
	}

	b.WriteString("\n### What changed\n\n")
	for _, fd := range a.plan.Facts.Files {
		fmt.Fprintf(&b, "- %s (%s): +%d -%d lines\n", ident(fd.Path), fd.Class, fd.Added, fd.Removed)
	}
	switch {
	case len(f.weak.undecided) > 0:
		paths := make([]string, 0, len(f.weak.undecided))
		for _, p := range f.weak.undecided {
			paths = append(paths, ident(p))
		}
		b.WriteString("\n**gonk cannot tell whether this change weakens a test.** It has no check for " +
			strings.Join(paths, ", ") + ": review it as you would any change to code that tests cover. " +
			"gonk never merges such a change on its own.\n")
	default:
		b.WriteString("\nNo test was removed, skipped or built out by this change (checked for Go; see the limits below).\n")
	}
	switch f.delta {
	case cidef.KindNone:
		b.WriteString("The pipeline definition is unchanged.\n")
	case cidef.KindPreserving:
		b.WriteString("The pipeline definition changed, but not what any job executes, nor when it runs or may fail.\n")
	case cidef.KindExecChanging:
		b.WriteString("**This change alters what the diagnosed job EXECUTES.** Its passing is then a claim the change makes " +
			"about itself: read the new commands.\n")
	}

	b.WriteString("\n### The agent's explanation\n\n")
	b.WriteString("Written by the model that proposed the change. gonk has not checked that it is true.\n\n")
	b.WriteString(fence(inert(a.plan.Reason)) + "\n")

	b.WriteString("\n### Details\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n| fix | %s |\n| cause | %s |\n| base commit | %s |\n| base pipeline | %s |\n| diagnosis class | %s |\n| attempt | %d (%s) |\n| session | %s |\n",
		ident(a.rec.BeadAnchor), ident(pf.CauseKey), ident(pf.BaseSHA), pipelineRef(a.rec, pf.BasePipelineID),
		ident(pf.Class), max(a.rec.Attempt, 1), fixRungText(a.rec.Rung), ident(a.rec.SessionID))
	b.WriteString("\ngonk cannot tell whether a change to product code fixes the defect or only satisfies the test, nor whether a " +
		"passing job still ran every test it used to. A person decides whether this merges: gonk assigns no one, " +
		"requests no review and approves nothing.\n\n")
	b.WriteString(marker)
	desc := b.String()
	// THE INVARIANTS, checked on what is about to be sent: nothing in the
	// description can close an issue, and no line of it reads as a quick
	// action. A failure is OURS -- some text reached the description without
	// going through inert, ident or gate A8 -- and no merge request is
	// opened with it.
	if err := checkNoClosingPattern("merge request description", desc); err != nil {
		return "", err
	}
	if err := effects.ValidateComments(effects.Batch{Effects: []effects.Effect{{Kind: effects.KindComment, Body: desc}}}); err != nil { // render:quick
		return "", fmt.Errorf("the fix description: %w", err)
	}
	return desc, nil
}

// fixOpenedText is the fix note for `verifying` and `awaiting-run` (plan 8.1).
func fixOpenedText(fix beadstore.Record, mrIID int64, commit string, state pipefix.State) string {
	var b strings.Builder
	if state == pipefix.StateAwaitingRun {
		fmt.Fprintf(&b, "**gonk opened a fix merge request: !%d.** It changes the pipeline definition, so a maintainer must review it "+
			"and start its pipeline; gonk judges the pipeline that person starts.", mrIID)
	} else {
		fmt.Fprintf(&b, "**gonk opened a fix merge request: !%d.** It is waiting for that merge request's pipeline on commit %s.",
			mrIID, codeSpan(shortSHA(commit)))
	}
	b.WriteString("\n\nThe fix counts as working when the diagnosed job still runs and passes there, not when the pipeline is green.")
	b.WriteString("\n\n" + fixNoteFooter)
	return b.String()
}

// scopeRequestText is the needs-scope note for a session's own verdict (plan
// 4.4 way 2): what would have to change, in the agent's words, fenced and
// labelled; the paths as code spans, text only.
func scopeRequestText(run, fix beadstore.Record, req *effects.ScopeRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**No fix merge request: more scope is needed.** gonk looked at the failure diagnosed in %s and did not propose a change "+
		"within what a fix may touch here.", pipelineRef(run, fix.PipelineFix.BasePipelineID))
	if req != nil {
		if len(req.Paths) > 0 {
			paths := make([]string, 0, len(req.Paths))
			for _, p := range req.Paths {
				paths = append(paths, codeSpan(p))
			}
			b.WriteString("\n\nFiles the session says would have to change: " + strings.Join(paths, ", ") + ".")
		}
		b.WriteString("\n\nIts reason, in its own words (gonk has not checked that it is true):\n\n" + fence(req.Reason))
	}
	b.WriteString("\n\nA person can make the change, or widen what gonk may change in `.gonk.yml`. gonk will not retry this on its own.")
	b.WriteString("\n\n" + fixNoteFooter)
	return b.String()
}

// execDeniedText is the needs-scope note for an execution-changing CI edit in
// a project whose pipeline_fix.ci_exec_changes is `deny`.
func execDeniedText(run, fix beadstore.Record) string {
	return fmt.Sprintf("**No fix merge request: more scope is needed.** The change gonk prepared for the failure diagnosed in %s alters "+
		"what a pipeline job executes, and this project does not let a fix do that (`pipeline_fix.ci_exec_changes: deny`). "+
		"Nothing was left in the repository.\n\nA person can make the change. gonk will not retry this on its own.\n\n%s",
		pipelineRef(run, fix.PipelineFix.BasePipelineID), fixNoteFooter)
}
