package main

// gonk-htl3: a NAME in the fix merge request's description is shown exactly,
// and GitLab can read nothing into it. Every name here is invented; the
// shapes are the ones the dev smoke check found mangled.

import (
	"context"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/pipefix"
)

// identRaw is what ident may put in the raw text: ASCII letters, digits,
// spaces and numeric character references, inside one code element.
var (
	identRaw = regexp.MustCompile(`^<code>(?:[A-Za-z0-9 ]|&#x[0-9a-f]+;)*</code>$`)
	codeElem = regexp.MustCompile(`<code>(.*?)</code>`)
	// anyRefRaw is what a GitLab reference, mention, label or URL is spelled
	// with. A `#` followed by a digit is an issue; the `#` of ident's own
	// character references is always followed by `x`.
	anyRefRaw = regexp.MustCompile(`#[^x]|#$|[!@~$%]|://|-|[|<>` + "`" + `*_\[\]]`)
)

// shownNames is every name a description shows in a code element, as a
// reader sees it: the character references decoded.
func shownNames(desc string) map[string]bool {
	out := map[string]bool{}
	for _, m := range codeElem.FindAllStringSubmatch(desc, -1) {
		out[html.UnescapeString(m[1])] = true
	}
	return out
}

// hostileNames are names shaped to close, link or break out.
var hostileNames = []string{
	"smoke-pipefix-20261008t025825z", "smoke/pipefix-20261008T025825Z-head", "es1-3404057d69deaf95", "qwen3-6-35b",
	"fix-1234", "closes-12", "unit-tests-3.12", "release-2026", "python-3-11",
	"closes #12", "Closes #1", "fixes TRACK-7", "TRACK-7 closes #1", "resolves https://gitlab.example.invalid/g/p/-/issues/1",
	"a|b", "_under_ *star* `tick` ``two``", "</code><b>x</b>", "&amp; &#35;12 &#x23;12", "@root !5 ~bug $9 %m [x](y)",
	"/close", "www.example.invalid", "café-2", "tab\there", "", " ",
}

func TestIdentShowsANameExactlyAndGitLabCanReadNothingIntoIt(t *testing.T) {
	for _, name := range hostileNames {
		got := ident(name)
		if !identRaw.MatchString(got) {
			t.Errorf("ident(%q) = %q: raw text outside letters, digits, spaces and character references", name, got)
		}
		if shown := html.UnescapeString(strings.TrimSuffix(strings.TrimPrefix(got, "<code>"), "</code>")); shown != name {
			t.Errorf("ident(%q) shows %q: the name is altered", name, shown)
		}
		// Nothing a reference is spelled with is in the raw text, in any
		// surrounding: after a closing verb, after another name, in a table row.
		for _, text := range []string{got, "Closes " + got, "this fixes: " + got, ident("closes") + " " + got,
			"| " + ident("fixes") + " | " + got + " |", got + " " + got} {
			if m := closingPattern.FindString(text); m != "" {
				t.Errorf("closingPattern matches %q in %q", m, text)
			}
			if m := faClosing.FindString(text); m != "" {
				t.Errorf("the test's closing pattern matches %q in %q", m, text)
			}
		}
		if m := anyRefRaw.FindString(strings.TrimSuffix(strings.TrimPrefix(got, "<code>"), "</code>")); m != "" {
			t.Errorf("ident(%q) = %q leaves %q, which a reference can be spelled with", name, got, m)
		}
	}
	// THE OLD RENDERING, for contrast: this is the defect.
	if got := codeSpan(inert("smoke-pipefix-20261008t025825z")); got != "`smoke-pipefix- 20261008t025825z`" {
		t.Errorf("fixture: inert no longer rewrites a name (%q); it is still what the rationale goes through", got)
	}
	if got := fixRungText("qwen3-6-35b"); got != "model "+ident("qwen3-6-35b") || fixRungText(" ") != "the configured model" {
		t.Errorf("fixRungText = %q", got)
	}
}

// inert is unchanged for PROSE: the three rules, in either case of key.
func TestInertStillDefusesProse(t *testing.T) {
	for in, want := range map[string]string{
		"Closes #12":                        "Closes # 12",
		"fixes TRACK-7 and track-7":         "fixes TRACK- 7 and track- 7",
		"see https://x.invalid/-/issues/1":  "see https: //x.invalid/-/issues/1",
		"Resolves group/project#3, ABC_D-9": "Resolves group/project# 3, ABC_D- 9",
		"nothing to defuse here":            "nothing to defuse here",
	} {
		got := inert(in)
		if got != want {
			t.Errorf("inert(%q) = %q, want %q", in, got, want)
		}
		if m := closingPattern.FindString(got); m != "" {
			t.Errorf("closingPattern still matches %q in inert(%q)", m, in)
		}
	}
}

// THE DEFECT (gonk-htl3), through the real apply: the description of a fix
// whose job, stage, branch, signature, rung, session and further failing jobs
// all have a digit after a hyphen -- and some are named like a closing
// statement -- shows each one exactly, in the table that names what decides
// whether the fix worked, and closes and links nothing.
func TestFixMRDescriptionShowsNamesExactly(t *testing.T) {
	const (
		job    = "smoke-pipefix-20261008t025825z"
		stage  = "test-2"
		sig    = "es1-3404057d69deaf95"
		rung   = "qwen3-6-35b"
		sess   = "gonk-pipefix-p75-pl3404-a1"
		branch = "main"
	)
	also := []string{"fix-1234", "closes-12", "closes #1", "TRACK-7"}
	w := newFAWorld(t, faOpts{})
	rec, _ := w.fixRecord()
	rec.PipelineFix.Targets[0].Name, rec.PipelineFix.Targets[0].Stage, rec.PipelineFix.Targets[0].ErrorSignature = job, stage, sig
	rec.PipelineFix.AlsoFailing = also
	rec.Rung, rec.SessionID = rung, sess
	if err := w.store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	hostile := "Closes #1. Fixes TRACK-7 in fix-1234; see https://gitlab.example.invalid/g/p/-/issues/1"
	b := effects.Batch{Verdict: effects.VerdictFix, Effects: []effects.Effect{faEdit(pfSource, pfOld, pfNew), faMeta(hostile)}}
	if applied, v, _, err := w.apply(b); !applied {
		t.Fatalf("setup: %q %v\n%s", v, err, w.log)
	}
	desc := w.fixMRs()[0].Description
	shown := shownNames(desc)
	for _, name := range append([]string{job, stage, sig, rung, sess, branch, rec.BeadAnchor, rec.PipelineFix.CauseKey, rec.PipelineFix.BaseSHA, pfSource}, also...) {
		if !shown[name] {
			t.Errorf("the description does not show %q exactly", name)
		}
	}
	// The row a reviewer is told to look for names the job that exists.
	if row := "| " + ident(job) + " | " + ident(stage) + " | " + ident(sig) + " |\n"; !strings.Contains(desc, row) {
		t.Errorf("the criterion table does not hold the job's row:\n%s", desc)
	}
	if !strings.Contains(desc, "| attempt | 1 (model "+ident(rung)+") |") {
		t.Errorf("the details table does not name the rung:\n%s", desc)
	}
	for _, mangled := range []string{"pipefix- 2026", "es1- 3404", "qwen3- 6", "test- 2", "fix- 1234", "closes- 12", "p75- pl3404", "ref- "} {
		if strings.Contains(strings.ReplaceAll(desc, hostile, ""), mangled) && !strings.Contains(inert(hostile), mangled) {
			t.Errorf("the description holds the rewritten name %q", mangled)
		}
	}
	for name, re := range map[string]*regexp.Regexp{"the test's pattern": faClosing, "closingPattern": closingPattern} {
		if m := re.FindString(desc); m != "" {
			t.Fatalf("%s matches in the description: %q\n%s", name, m, desc)
		}
	}
	// Every code element's raw text is letters, digits, spaces and character
	// references: no name contributes a character a reference is spelled with.
	for _, m := range codeElem.FindAllString(desc, -1) {
		if !identRaw.MatchString(m) {
			t.Errorf("a code element's raw text can be read as markup or a reference: %q", m)
		}
	}
	// THE PROSE DEFENCE IS AS IT WAS: the rationale is fenced and inert.
	if !strings.Contains(desc, inert(hostile)) || strings.Contains(desc, hostile) || strings.Contains(faOutsideFences(desc), "TRACK- 7") {
		t.Errorf("the agent's rationale is not rendered inert inside its fence:\n%s", desc)
	}
	// The same names in the STATUS BLOCK the exit check writes into this
	// description later (fixVerdictTexts' second result), and in its note.
	note, status := fixVerdictTexts(rec, fixSettlement{state: pipefix.StateFixed, res: fixResultFor(job, "closes #1")}, pipefix.StateFixed, false, "")
	if !strings.Contains(status, "- "+ident(job)+":") || !strings.Contains(status, "- "+ident("closes #1")+":") ||
		closingPattern.MatchString(status) || strings.Contains(status, "- 2026") {
		t.Errorf("the status block's job lines: %q", status)
	}
	if !strings.Contains(note, "- `"+job+"`:") {
		t.Errorf("the fix note's job lines: %q", note)
	}
}

// fixResultFor is a judged result naming these jobs, for the status block.
func fixResultFor(jobs ...string) pipefix.Result {
	var res pipefix.Result
	for _, j := range jobs {
		res.Jobs = append(res.Jobs, pipefix.JobFinding{Job: j, Attempts: 1, Succeeded: 1})
	}
	return res
}
