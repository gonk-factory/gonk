#!/usr/bin/env python3
"""mutate_review_pure.py -- "seen red" evidence for gonk-cuwn parts RV-DIFFMAP
(pkg/diffmap) and RV-CTX (pkg/reviewctx), plan
docs/plans/2026-10-05-gonk-cuwn-epic-remainder.md sections 3.2-3.4 and 6.2.

Breaks one rule at a time, runs ONLY the test that guards it (each mutation
names its package and its own -run pattern, so a kill means that rule's own
test went red, not that some unrelated test noticed), and requires it to FAIL;
then restores every file byte for byte (try/finally). A mutation whose text is
not found is VACUOUS, one the named test does not notice SURVIVED, and one
that does not compile NOCOMPILE -- each is a failure of this script. A
mutation may edit several places; an edit whose `old` is None CREATES the file
and removes it afterwards. Before anything is mutated the two packages must be
green, or a kill would mean nothing.

The six breaks the plan names for these parts (6.2) are marked [NAMED]:
  RV-DIFFMAP  an anchor on a line outside every hunk is found
              `old` and `new` sides confused
              a renamed file's old path does not resolve
  RV-CTX      the rationale reaches the prompt
              the author's model name reaches the prompt
              the package imports beadstore

Usage: hack/mutate_review_pure.py            (all mutations)
       hack/mutate_review_pure.py 3 7        (only these indexes)
       hack/mutate_review_pure.py --list     (names and indexes, nothing run)
       hack/mutate_review_pure.py -v ...     (also print what red looked like)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any
gate; its output is pasted into the evidence file.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
D = './pkg/diffmap/'
R = './pkg/reviewctx/'
DM = 'pkg/diffmap/diffmap.go'
DR = 'pkg/diffmap/render.go'
RC = 'pkg/reviewctx/reviewctx.go'
MOD = 'github.com/gonk-factory/gonk/'

LOOKUP = 'TestLookup'
MAPS = 'TestBuildLineMaps'
NOTREV = 'TestNotReviewable'
HDR = 'TestParseHunkHeader'
QUOTE = 'TestQuote'
KEY = 'TestBuildRefusesWhatItCannotKey'
SHOW = 'TestLineTextIsMadeSafeToShow'
RT = 'TestRenderingRoundTrips'
FORGE = 'TestRenderingCannotBeForgedByFileText'
GOLD = 'TestGoldenRenderings'
OLDP = 'TestRenderingNamesTheOldPathWheneverItDiffers'

CANARY = 'TestCanaryNothingOfTheAuthorSessionReachesThePrompt'
FIELDS = 'TestReviewFactsHoldsOnlyAllowListedFields'
IMPORTS = 'TestImportsNothingOfTheAuthorSession'
ONLY = 'TestBuildIsTheOnlyWayIn'
SKIPS = 'TestBuildSkips'
BAD = 'TestBuildRefusesBadFacts'
FACTS = 'TestBuildShowsEveryFact'
PFORGE = 'TestProjectTextCannotForgeThePrompt'
CUT = 'TestLongTextIsCutAndSaysSo'
REDACT = 'TestIssueTextIsRedacted'

IDX_NEW = "\t\t\t\tif l.Op != OpRemoved { // rule:sides\n\t\t\t\t\tm.index[Anchor{f.NewPath, SideNew, l.New}] = ref\n"
IDX_OLD = "\t\t\t\tif l.Op != OpAdded {\n\t\t\t\t\tm.index[Anchor{f.OldPath, SideOld, l.Old}] = ref // rule:rename\n"
FACTS_STRUCT = "type ReviewFacts struct {\n"
RENDER_AT = "\tb.WriteString(\"\\n== Linked issue ==\\n\")\n"


def leak(field, label):
    """A ReviewFacts string field that Build renders: the shape of every way
    author-side text could be given a way in."""
    return [(RC, FACTS_STRUCT, FACTS_STRUCT + "\t%s string\n" % field),
            (RC, RENDER_AT, "\tfmt.Fprintf(&b, \"%s: %%s\\n\", oneLine(f.%s, 600))\n" % (label, field) + RENDER_AT)]


# (name, package, test, [(file, old, new), ...])
muts = [
 # ================= RV-DIFFMAP =================
 ("[NAMED] an anchor on a line outside every hunk is found (up to 5 lines past a hunk)", D, LOOKUP,
  [(DM, "\tif !ok { // rule:outside-hunk\n\t\treturn Line{}, false\n\t}\n",
        "\tfor d := 1; d <= 5 && !ok; d++ { // rule:outside-hunk\n\t\tref, ok = m.index[Anchor{a.Path, a.Side, a.Line - d}]\n\t}\n\tif !ok {\n\t\treturn Line{}, false\n\t}\n")]),
 ("[NAMED] sides confused: an old-side anchor is looked up in the new numbering", D, LOOKUP,
  [(DM, "\tref, ok := m.index[a]\n", "\tif a.Side == SideOld {\n\t\ta.Side = SideNew\n\t}\n\tref, ok := m.index[a]\n")]),
 ("[NAMED] sides confused: a context line is indexed on the new side under its OLD number", D, LOOKUP,
  [(DM, IDX_NEW, "\t\t\t\tif l.Op != OpRemoved { // rule:sides\n\t\t\t\t\tn := l.New\n\t\t\t\t\tif l.Op == OpContext {\n\t\t\t\t\t\tn = l.Old\n\t\t\t\t\t}\n\t\t\t\t\tm.index[Anchor{f.NewPath, SideNew, n}] = ref\n")]),
 ("[NAMED] sides confused: a removed line is given a place on the new side", D, LOOKUP,
  [(DM, "\t\t\t\tif l.Op != OpRemoved { // rule:sides\n", "\t\t\t\tif true { // rule:sides\n")]),
 ("[NAMED] sides confused: an added line is given a place on the old side", D, LOOKUP,
  [(DM, "\t\t\t\tif l.Op != OpAdded {\n", "\t\t\t\tif true {\n")]),
 ("[NAMED] a renamed file's old path does not resolve (the old side is keyed by the new path)", D, LOOKUP,
  [(DM, IDX_OLD, IDX_OLD.replace("f.OldPath", "f.NewPath"))]),
 ("rename: the new side is keyed by the old path", D, LOOKUP,
  [(DM, IDX_NEW, IDX_NEW.replace("f.NewPath", "f.OldPath"))]),

 ("hunk header: an omitted count is zero lines", D, HDR,
  [(DM, "\tcount = 1 // rule:omitted-count\n", "\tcount = 0 // rule:omitted-count\n")]),
 ("hunk header: a number over MaxLineNumber is accepted", D, HDR,
  [(DM, "\tif err != nil || v > MaxLineNumber {", "\tif err != nil {")]),
 ("hunk header: text glued to the closing @@ is accepted", D, HDR,
  [(DM, "\t\tif rest[0] != ' ' {\n", "\t\tif false {\n")]),
 ("hunk header: the new range may be missing its +", D, HDR,
  [(DM, "\tif rest, ok = strings.CutPrefix(rest, \" +\"); !ok {\n\t\treturn bad(`no \" +\" after the old range`)\n\t}\n",
        "\tif r2, found := strings.CutPrefix(rest, \" +\"); found {\n\t\trest = r2\n\t} else {\n\t\trest = strings.TrimPrefix(rest, \" \")\n\t}\n")]),
 ("hunk header: the closing @@ may be missing", D, HDR,
  [(DM, "\tif rest, ok = strings.CutPrefix(rest, \" @@\"); !ok {\n\t\treturn bad(`no \" @@\" after the new range`)\n\t}\n",
        "\trest = strings.TrimPrefix(rest, \" @@\")\n\tif rest != \"\" && rest[0] != ' ' {\n\t\trest = \"\"\n\t}\n")]),

 ("numbering: a context line does not advance the old counter", D, MAPS,
  [(DM, "oldN, newN, remOld, remNew = oldN+1, newN+1, remOld-1, remNew-1", "oldN, newN, remOld, remNew = oldN, newN+1, remOld-1, remNew-1")]),
 ("numbering: a removed line advances the new counter too", D, MAPS,
  [(DM, "\t\t\t\toldN, remOld = oldN+1, remOld-1\n", "\t\t\t\toldN, remOld = oldN+1, remOld-1\n\t\t\t\tnewN++\n")]),
 ("numbering: an added line advances the old counter too", D, MAPS,
  [(DM, "\t\t\t\tnewN, remNew = newN+1, remNew-1\n", "\t\t\t\tnewN, remNew = newN+1, remNew-1\n\t\t\t\toldN++\n")]),
 ("no-newline marker inside a hunk is not recorded", D, MAPS,
  [(DM, "\t\t\t\th.Lines[len(h.Lines)-1].NoEOL = true\n\t\t\tdefault:", "\t\t\tdefault:")]),
 ("no-newline marker after the last line of a hunk is not recorded", D, MAPS,
  [(DM, "\t\t\th.Lines[len(h.Lines)-1].NoEOL = true\n\t\t\ti++\n", "\t\t\ti++\n")]),
 ("no-newline marker counted as a line of the file", D, MAPS,
  [(DM, "\t\t\tcase '\\\\': // rule:no-eol -- a marker about the line above, not a line\n\t\t\t\tif len(h.Lines) == 0 {",
        "\t\t\tcase '\\\\': // rule:no-eol -- a marker about the line above, not a line\n\t\t\t\th.Lines = append(h.Lines, Line{New: newN, Op: OpAdded, Text: body})\n\t\t\t\tif len(h.Lines) == 0 {")]),
 ("CRLF: the carriage return is kept", D, MAPS,
  [(DM, "\t\tlines[i] = strings.TrimSuffix(lines[i], \"\\r\") // rule:crlf\n", "\t\t_ = i // rule:crlf\n")]),
 ("the hunk range check is off by one: the largest line number is refused", D, MAPS,
  [(DM, "hh.OldStart-1 > MaxLineNumber-hh.OldLines || hh.NewStart-1 > MaxLineNumber-hh.NewLines", "hh.OldStart > MaxLineNumber-hh.OldLines || hh.NewStart > MaxLineNumber-hh.NewLines")]),
 ("file header lines before the first hunk are refused", D, MAPS,
  [(DM, "\t\"diff --git \", \"index \", \"--- \", \"+++ \",", "\t\"diff --git \", \"index \",")]),

 ("not reviewable: too_large is parsed as if the text were whole", D, NOTREV,
  [(DM, "\tcase d.TooLarge: // rule:too-large -- the flag wins over any text that came with it\n\t\tf.Status = StatusTooLarge\n\t\treturn f\n", "")]),
 ("not reviewable: collapsed is parsed as if the text were whole", D, NOTREV,
  [(DM, "\tcase d.Collapsed: // rule:collapsed\n\t\tf.Status = StatusCollapsed\n\t\treturn f\n", "")]),
 ("not reviewable: a binary file reads as 'no lines changed'", D, NOTREV,
  [(DM, "\tcase binary: // rule:binary\n\t\tf.Status = StatusBinary\n", "\tcase binary: // rule:binary\n\t\tf.Status = StatusNoLines\n")]),
 ("not reviewable: 'GIT binary patch' is not recognised", D, NOTREV,
  [(DM, "\t\tif l == \"GIT binary patch\" || (", "\t\tif (")]),
 ("not reviewable: a file with no diff text reads as 'no lines changed'", D, NOTREV,
  [(DM, "\t\tf.Status = StatusNoText // rule:no-text\n", "\t\tf.Status = StatusNoLines // rule:no-text\n")]),
 ("not reviewable: any empty diff is a pure rename", D, NOTREV,
  [(DM, "\t\tif d.RenamedFile && !d.NewFile && !d.DeletedFile && d.OldPath != d.NewPath && d.Diff == \"\" {", "\t\tif d.Diff == \"\" {")]),
 ("not reviewable: header lines with no hunk are a pure rename", D, NOTREV,
  [(DM, "d.OldPath != d.NewPath && d.Diff == \"\" {", "d.OldPath != d.NewPath {"),
   (DM, "\t\tif d.RenamedFile && !d.NewFile && !d.DeletedFile && ", "\t\tif !d.NewFile && !d.DeletedFile || ")]),
 ("not reviewable: a malformed file does not make the map unreviewable", D, NOTREV,
  [(DM, "\t\tif !f.Status.Reviewable() { // rule:unreadable-file\n", "\t\tif !f.Status.Reviewable() && f.Status != StatusMalformed { // rule:unreadable-file\n")]),
 ("not reviewable: only too-large files make the map unreviewable", D, NOTREV,
  [(DM, "\t\tif !f.Status.Reviewable() { // rule:unreadable-file\n", "\t\tif f.Status == StatusTooLarge { // rule:unreadable-file\n")]),
 ("not reviewable: a map with no line at all is reviewable", D, NOTREV,
  [(DM, "\tif len(reasons) == 0 && len(m.index) == 0 { // rule:empty-map\n", "\tif false { // rule:empty-map\n")]),
 ("Status: everything but too-large counts as read in full", D, 'TestStatusReviewable',
  [(DM, "\treturn s == StatusText || s == StatusNoLines\n", "\treturn s != StatusTooLarge\n")]),
 ("strict: a hunk shorter than its counts is accepted", D, NOTREV,
  [(DM, "\t\t\tif i >= len(lines) {\n\t\t\t\treturn nil, false, fmt.Errorf(\"line %d: the hunk ends early: %d old and %d new lines missing\", i, remOld, remNew)\n\t\t\t}\n",
        "\t\t\tif i >= len(lines) {\n\t\t\t\tbreak\n\t\t\t}\n")]),
 ("strict: a context line past a side's count is accepted", D, NOTREV,
  [(DM, "\t\t\t\tif remOld == 0 || remNew == 0 {\n", "\t\t\t\tif false {\n")]),
 ("strict: a removed line past the old count is accepted", D, NOTREV,
  [(DM, "\t\t\t\tif remOld == 0 {\n", "\t\t\t\tif false {\n")]),
 ("strict: an added line past the new count is accepted", D, NOTREV,
  [(DM, "\t\t\t\tif remNew == 0 {\n", "\t\t\t\tif false {\n")]),
 ("strict: an unknown line prefix is read as context", D, NOTREV,
  [(DM, "\t\t\tdefault:\n\t\t\t\treturn nil, false, fmt.Errorf(\"line %d: unknown line prefix %q in a hunk\", i, raw[:1])\n",
        "\t\t\tdefault:\n\t\t\t\th.Lines = append(h.Lines, Line{Old: oldN, New: newN, Op: OpContext, Text: body})\n\t\t\t\toldN, newN, remOld, remNew = oldN+1, newN+1, remOld-1, remNew-1\n")]),
 ("strict: a no-newline marker with no line before it is ignored", D, NOTREV,
  [(DM, "\t\t\t\tif len(h.Lines) == 0 {\n\t\t\t\t\treturn nil, false, fmt.Errorf(\"line %d: a no-newline marker with no line before it\", i)\n\t\t\t\t}\n\t\t\t\th.Lines[len(h.Lines)-1].NoEOL = true\n",
        "\t\t\t\tif len(h.Lines) > 0 {\n\t\t\t\t\th.Lines[len(h.Lines)-1].NoEOL = true\n\t\t\t\t}\n")]),
 ("strict: an empty hunk is kept", D, NOTREV,
  [(DM, "\t\tif hh.OldLines == 0 && hh.NewLines == 0 {\n", "\t\tif false {\n")]),
 ("strict: a hunk with lines may start at line 0", D, NOTREV,
  [(DM, "\t\tif (hh.OldLines > 0 && hh.OldStart == 0) || (hh.NewLines > 0 && hh.NewStart == 0) {\n", "\t\tif false {\n")]),
 ("strict: overlapping or out-of-order hunks are accepted", D, NOTREV,
  [(DM, "\t\tif (hh.OldLines > 0 && hh.OldStart <= lastOld) || (hh.NewLines > 0 && hh.NewStart <= lastNew) {\n", "\t\tif lastOld < 0 || lastNew < 0 {\n")]),
 ("strict: hunk order is checked on the new side only", D, NOTREV,
  [(DM, "\t\tif (hh.OldLines > 0 && hh.OldStart <= lastOld) || (hh.NewLines > 0 && hh.NewStart <= lastNew) {\n", "\t\tif lastOld < 0 || (hh.NewLines > 0 && hh.NewStart <= lastNew) {\n")]),
 ("strict: hunk order is checked on the old side only", D, NOTREV,
  [(DM, "\t\tif (hh.OldLines > 0 && hh.OldStart <= lastOld) || (hh.NewLines > 0 && hh.NewStart <= lastNew) {\n", "\t\tif (hh.OldLines > 0 && hh.OldStart <= lastOld) || lastNew < 0 {\n")]),
 ("strict: a hunk may run past the largest line number", D, NOTREV,
  [(DM, "hh.OldStart-1 > MaxLineNumber-hh.OldLines || hh.NewStart-1 > MaxLineNumber-hh.NewLines { // rule:hunk-range", "false { // rule:hunk-range")]),
 ("strict: text before the first hunk is skipped", D, NOTREV,
  [(DM, "\t\tif !ok { // rule:preamble\n", "\t\tif false { // rule:preamble\n")]),
 ("strict: a new file may have old-side lines", D, NOTREV,
  [(DM, "\t\t\tif d.NewFile && l.Op != OpAdded { // rule:new-file-old-side\n", "\t\t\tif false { // rule:new-file-old-side\n")]),
 ("strict: a deleted file may have new-side lines", D, NOTREV,
  [(DM, "\t\t\tif d.DeletedFile && l.Op != OpRemoved { // rule:deleted-file-new-side\n", "\t\t\tif false { // rule:deleted-file-new-side\n")]),
 ("strict: a file may be both new and deleted", D, NOTREV,
  [(DM, "\tcase d.NewFile && d.DeletedFile:\n", "\tcase false:\n")]),
 ("not reviewable: the rendering does not say so", D, NOTREV,
  [(DR, "\t\t\tfmt.Fprintf(&b, \") NOT REVIEWABLE (%s): gonk could not read this file's changes; no line of it can be cited\\n\", f.Status)\n", "\t\t\tb.WriteString(\")\\n\")\n")]),

 ("keys: two files may claim one path on one side", D, KEY,
  [(DM, "\t\t\tif owner[k] { // rule:duplicate-path\n", "\t\t\tif false { // rule:duplicate-path\n")]),
 ("keys: an empty path is accepted", D, KEY,
  [(DM, "\t\tif d.OldPath == \"\" || d.NewPath == \"\" {\n", "\t\tif false {\n")]),
 ("keys: a new file claims its path on the old side (a file created where one was renamed away clashes)", D, KEY,
  [(DM, "\t\tif !d.NewFile {\n\t\t\tkeys = append(keys, sideKey{d.OldPath, SideOld})\n\t\t}\n", "\t\tkeys = append(keys, sideKey{d.OldPath, SideOld})\n")]),

 ("quote: a quote spanning lines is accepted", D, QUOTE,
  [(DM, "\tif strings.ContainsAny(quote, \"\\n\\r\") { // rule:quote-one-line\n", "\tif false { // rule:quote-one-line\n")]),
 ("quote: an empty quote matches", D, QUOTE,
  [(DM, "\tif q == \"\" { // rule:quote-empty\n", "\tif false { // rule:quote-empty\n")]),
 ("quote: the quote is not normalised", D, QUOTE,
  [(DM, "\tq := pipelinectx.Norm(quote)\n", "\tq := quote\n")]),
 ("quote: the line's text is not normalised", D, QUOTE,
  [(DM, "\ttext := pipelinectx.Norm(l.Text)\n", "\ttext := l.Text\n")]),
 ("quote: a whole-line quote is reported as a part", D, QUOTE,
  [(DM, "\tcase text == q:\n\t\treturn QuoteWhole\n", "\tcase text == q:\n\t\treturn QuoteWithin\n")]),
 ("quote: a part of the line is reported as the whole line", D, QUOTE,
  [(DM, "\tcase strings.Contains(text, q):\n\t\treturn QuoteWithin\n", "\tcase strings.Contains(text, q):\n\t\treturn QuoteWhole\n")]),
 ("quote: matching ignores case", D, QUOTE,
  [(DM, "\tcase strings.Contains(text, q):\n", "\tcase strings.Contains(strings.ToLower(text), strings.ToLower(q)):\n")]),
 ("quote: all whitespace is ignored, not collapsed", D, QUOTE,
  [(DM, "\tq := pipelinectx.Norm(quote)\n", "\tq := strings.ReplaceAll(pipelinectx.Norm(quote), \" \", \"\")\n"),
   (DM, "\ttext := pipelinectx.Norm(l.Text)\n", "\ttext := strings.ReplaceAll(pipelinectx.Norm(l.Text), \" \", \"\")\n")]),
 ("quote: a missing anchor reads as 'no match' (the gate cannot tell RV-ANCHOR from RV-QUOTE)", D, QUOTE,
  [(DM, "\tif !ok {\n\t\treturn QuoteNoAnchor\n\t}\n", "\tif !ok {\n\t\treturn QuoteNoMatch\n\t}\n")]),
 ("quote: QuoteRunes counts bytes", D, QUOTE,
  [(DM, "\treturn len([]rune(pipelinectx.Norm(quote)))\n", "\treturn len(pipelinectx.Norm(quote))\n")]),
 ("quote: QuoteRunes counts the raw quote", D, QUOTE,
  [(DM, "\treturn len([]rune(pipelinectx.Norm(quote)))\n", "\treturn len([]rune(quote))\n")]),

 ("shown text: characters that break the line are kept", D, SHOW,
  [(DM, "r == 0x85 || r == 0x2028 || r == 0x2029:\n\t\t\treturn ' '\n", "r == 0x85 || r == 0x2028 || r == 0x2029:\n\t\t\treturn r\n")]),
 ("shown text: control characters are kept", D, SHOW,
  [(DM, "\t\tcase unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:\n", "\t\tcase unicode.IsControl(r) && r > 0x9f, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:\n")]),
 ("shown text: bidirectional overrides are kept", D, SHOW,
  [(DM, "\t\tcase unicode.IsControl(r), r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:\n", "\t\tcase unicode.IsControl(r):\n")]),
 ("shown text: invalid UTF-8 is kept", D, SHOW,
  [(DM, "\ts = strings.ToValidUTF8(s, \"?\")\n\treturn strings.Map(func(r rune) rune { // rule:showable\n", "\treturn strings.Map(func(r rune) rune { // rule:showable\n\t\tif r == utf8.RuneError {\n\t\t\treturn r\n\t\t}\n"),
   (DM, "\t\"unicode\"\n", "\t\"unicode\"\n\t\"unicode/utf8\"\n")]),

 ("render: a context line's OLD and NEW numbers are swapped", D, 'TestCoord',
  [(DR, "\treturn \"[OLD:\" + strconv.Itoa(l.Old) + \",NEW:\" + strconv.Itoa(l.New) + \"]\"\n", "\treturn \"[OLD:\" + strconv.Itoa(l.New) + \",NEW:\" + strconv.Itoa(l.Old) + \"]\"\n")]),
 ("render: an added line is labelled OLD", D, 'TestCoord',
  [(DR, "\t\treturn \"[NEW:\" + strconv.Itoa(l.New) + \"]\"\n", "\t\treturn \"[OLD:\" + strconv.Itoa(l.New) + \"]\"\n")]),
 ("render: a removed line is labelled NEW", D, 'TestCoord',
  [(DR, "\t\treturn \"[OLD:\" + strconv.Itoa(l.Old) + \"]\"\n", "\t\treturn \"[NEW:\" + strconv.Itoa(l.Old) + \"]\"\n")]),
 ("render: removed lines carry the + mark", D, RT,
  [(DR, "\t\t\t\tcase OpRemoved:\n\t\t\t\t\tmark = \"-\"\n", "\t\t\t\tcase OpRemoved:\n\t\t\t\t\tmark = \"+\"\n")]),
 ("render: the text delimiter is dropped (leading whitespace becomes ambiguous)", D, RT,
  [(DR, "mark + \" |\" + l.Text + \"\\n\")", "mark + \" \" + l.Text + \"\\n\")")]),
 ("render: a hunk's last line is not shown", D, RT,
  [(DR, "\t\t\tfor _, l := range h.Lines {\n\t\t\t\tc := Coord(l)\n", "\t\t\tfor _, l := range h.Lines[:len(h.Lines)-1] {\n\t\t\t\tc := Coord(l)\n")]),
 ("render: the path is printed raw (a file name can forge a file line)", D, FORGE,
  [(DR, "len(m.files), strconv.Quote(f.NewPath), what(f))", "len(m.files), `\"`+f.NewPath+`\"`, what(f))")]),
 ("render: the old path is named only for a plain rename (the fuzz crasher)", D, OLDP,
  [(DR, "\tif f.OldPath != f.NewPath { // rule:render-old-path\n", "\tif f.OldPath != f.NewPath && !f.New && !f.Deleted { // rule:render-old-path\n")]),
 ("render: the no-newline marker is not shown", D, GOLD,
  [(DR, "\t\t\t\tif l.NoEOL {\n", "\t\t\t\tif false {\n")]),
 ("render: hunks are not separated", D, GOLD,
  [(DR, "\t\t\tfmt.Fprintf(&b, \"@@ hunk %d of %d: OLD %s, NEW %s\\n\", hi+1, len(f.Hunks), span(h.OldStart, h.OldLines), span(h.NewStart, h.NewLines))\n", "\t\t\t_ = hi\n")]),
 ("render: a pure rename is shown as modified text with counts", D, GOLD,
  [(DR, "\t\tcase StatusNoLines:\n\t\t\tb.WriteString(\"; no line changed, nothing to cite)\\n\")\n", "\t\tcase StatusNoLines:\n\t\t\tfmt.Fprintf(&b, \"; %d added, %d removed)\\n\", f.Added, f.Removed)\n")]),
 ("contract: pkg/diffmap imports the forge client", D, 'TestDiffmapIsPure',
  [('pkg/diffmap/zz_leak.go', None, 'package diffmap\n\nimport "' + MOD + 'pkg/glab"\n\nvar _ = glab.MRDiff{}\n')]),
 ("contract: FileDiff drifts from glab.MRDiff (a JSON tag)", D, 'TestFileDiffIsGlabMRDiff',
  [(DM, 'TooLarge    bool   `json:"too_large"`', 'TooLarge    bool   `json:"toolarge"`')]),

 # ================= RV-CTX =================
 ("[NAMED] the rationale reaches the prompt (ReviewFacts gains Rationale, Build renders it) -- canary", R, CANARY,
  leak("Rationale", "author's explanation")),
 ("[NAMED] the rationale reaches the prompt -- the same break, seen by the field contract", R, FIELDS,
  leak("Rationale", "author's explanation")),
 ("[NAMED] the author's model name reaches the prompt (ReviewFacts gains AuthorModel) -- canary", R, CANARY,
  leak("AuthorModel", "written by model")),
 ("[NAMED] the author's model name reaches the prompt -- the same break, seen by the field contract", R, FIELDS,
  leak("AuthorModel", "written by model")),
 ("[NAMED] the package imports beadstore", R, IMPORTS,
  [('pkg/reviewctx/zz_leak.go', None, 'package reviewctx\n\nimport "' + MOD + 'pkg/beadstore"\n\nvar _ = beadstore.StateDone\n')]),
 ("N3: the MR description is passed through (ReviewFacts gains MRDescription) -- canary", R, CANARY,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tMRDescription string\n"),
   (RC, RENDER_AT, "\tb.WriteString(\"description:\\n\" + untrustedBlock(\"merge request description\", capText(f.MRDescription, CapIssueBody)))\n" + RENDER_AT)]),
 ("N3: the MR description is passed through -- seen by the field contract", R, FIELDS,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tMRDescription string\n")]),
 ("N2: the author's transcript reaches the prompt (a field with a harmless name) -- canary", R, CANARY,
  leak("Background", "background")),
 ("N2: the author's rung reaches the prompt, nested in the pipeline facts -- canary", R, CANARY,
  [(RC, "type PipelineFacts struct {\n", "type PipelineFacts struct {\n\tRung string\n"),
   (RC, RENDER_AT, "\tfmt.Fprintf(&b, \"rung: %s\\n\", oneLine(f.Pipeline.Rung, 80))\n" + RENDER_AT),
   (RC, "\t\tif p.ID != 0 || p.SHA != \"\" || p.Status != \"\" || len(p.Jobs) != 0 { // rule:facts-unread", "\t\tif p.ID != 0 || p.SHA != \"\" || p.Status != \"\" || len(p.Jobs) != 0 || p.Rung != \"\" { // rule:facts-unread")]),
 ("N2: the session name reaches the prompt through a per-job field -- canary", R, CANARY,
  [(RC, "type JobFact struct {\n", "type JobFact struct {\n\tSessionName string\n"),
   (RC, "\t\t\tallowed := \"\"\n", "\t\t\tallowed := \" \" + oneLine(j.SessionName, 80)\n")]),
 ("contract: pkg/diffmap (which reviewctx imports) imports the forge client", R, IMPORTS,
  [('pkg/diffmap/zz_leak.go', None, 'package diffmap\n\nimport "' + MOD + 'pkg/glab"\n\nvar _ = glab.MRDiff{}\n')]),
 ("contract: pkg/reviewctx imports pkg/effects (the author's batch type)", R, IMPORTS,
  [('pkg/reviewctx/zz_leak.go', None, 'package reviewctx\n\nimport "' + MOD + 'pkg/effects"\n\nvar _ = effects.Batch{}\n')]),
 ("contract: pkg/reviewctx imports os (it could read a file the author wrote)", R, IMPORTS,
  [('pkg/reviewctx/zz_leak.go', None, 'package reviewctx\n\nimport "os"\n\nvar _ = os.ReadFile\n')]),
 ("contract: ReviewFacts gains an untyped field", R, FIELDS,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tExtra any\n")]),
 ("contract: ReviewFacts gains a map", R, FIELDS,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tLabels map[string]string\n")]),
 ("contract: ReviewFacts gains a harmless-looking string", R, FIELDS,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tHint string\n")]),
 ("contract: a ReviewFacts field is embedded", R, FIELDS,
  [(RC, FACTS_STRUCT, FACTS_STRUCT + "\tJobFact\n")]),
 ("contract: Build takes more than ReviewFacts", R, ONLY,
  [(RC, "func Build(f ReviewFacts) (Context, error) {\n", "func Build(f ReviewFacts, extra ...string) (Context, error) {\n\t_ = extra\n")]),
 ("contract: a second exported entry point", R, ONLY,
  [('pkg/reviewctx/zz_leak.go', None, 'package reviewctx\n\n// BuildWithNote is the way around ReviewFacts.\nfunc BuildWithNote(f ReviewFacts, note string) (Context, error) {\n\tc, err := Build(f)\n\tc.Prompt += note\n\treturn c, err\n}\n')]),
 ("contract: an exported package variable Build could read", R, ONLY,
  [('pkg/reviewctx/zz_leak.go', None, 'package reviewctx\n\n// Preamble is package state.\nvar Preamble = ""\n')]),
 ("contract: Context gains a field", R, ONLY,
  [(RC, "\tPromptSHA256 string\n", "\tPromptSHA256 string\n\tNotes        []string\n")]),

 ("skip: an unreviewable diff is built anyway", R, SKIPS,
  [(RC, "\tif err := m.Reviewable(); err != nil { // rule:skip-unreviewable\n", "\tif err := m.Reviewable(); err != nil && false { // rule:skip-unreviewable\n")]),
 ("skip: the changed-line cap is off by one", R, SKIPS,
  [(RC, "n > MaxChangedLines { // rule:skip-lines", "n > MaxChangedLines+1 { // rule:skip-lines")]),
 ("skip: the changed-line cap refuses a diff exactly at the cap", R, SKIPS,
  [(RC, "n > MaxChangedLines { // rule:skip-lines", "n >= MaxChangedLines { // rule:skip-lines")]),
 ("skip: the byte cap is not enforced", R, SKIPS,
  [(RC, "\tif len(annotated) > MaxDiffBytes { // rule:skip-bytes\n", "\tif false { // rule:skip-bytes\n")]),
 ("skip: a withheld file is reported as not-reviewable, not too-large", R, SKIPS,
  [(RC, "\t\t\t\t\treason = SkipDiffTooLarge\n", "\t\t\t\t\treason = SkipDiffNotReviewable\n")]),
 ("skip: a collapsed file is not 'too large'", R, SKIPS,
  [(RC, "if r.Status == diffmap.StatusTooLarge || r.Status == diffmap.StatusCollapsed {", "if r.Status == diffmap.StatusTooLarge {")]),
 ("skip: everything unreadable is called too-large", R, SKIPS,
  [(RC, "\t\treason := SkipDiffNotReviewable\n", "\t\treason := SkipDiffTooLarge\n")]),
 ("skip: the fact is not 'skipped(<reason>)'", R, SKIPS,
  [(RC, "func (s *Skip) Fact() string { return \"skipped(\" + s.Reason + \")\" }", "func (s *Skip) Fact() string { return s.Reason }")]),

 ("facts: iid 0 is accepted", R, BAD, [(RC, "\tcase f.MRIID < 1: // rule:facts-iid\n", "\tcase f.MRIID < 0: // rule:facts-iid\n")]),
 ("facts: a blank title is accepted", R, BAD, [(RC, "\tcase strings.TrimSpace(f.MRTitle) == \"\":\n", "\tcase false:\n")]),
 ("facts: a blank target branch is accepted", R, BAD, [(RC, "\tcase strings.TrimSpace(f.TargetBranch) == \"\":\n", "\tcase false:\n")]),
 ("facts: the head may be a name or a short id", R, BAD, [(RC, "\tcase !shaRe.MatchString(f.HeadSHA): // rule:facts-sha\n", "\tcase f.HeadSHA == \"\": // rule:facts-sha\n")]),
 ("facts: the base may be anything non-empty", R, BAD, [(RC, "\tcase !shaRe.MatchString(f.BaseSHA):\n", "\tcase f.BaseSHA == \"\":\n")]),
 ("facts: the base is not checked", R, BAD, [(RC, "\tcase !shaRe.MatchString(f.BaseSHA):\n", "\tcase false:\n")]),
 ("facts: commit ids are matched unanchored", R, BAD, [(RC, "`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`", "`(?:[0-9a-f]{40}|[0-9a-f]{64})`")]),
 ("facts: commit ids may be upper case", R, BAD, [(RC, "`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`", "`^(?i:[0-9a-f]{40}|[0-9a-f]{64})$`")]),
 ("facts: head may equal base", R, BAD, [(RC, "\tcase f.HeadSHA == f.BaseSHA:\n", "\tcase false:\n")]),
 ("facts: any non-empty ci delta is shown", R, BAD, [(RC, "\tcase !ciDeltas[f.CIDelta]: // rule:facts-cidelta\n", "\tcase f.CIDelta == \"\": // rule:facts-cidelta\n")]),
 ("facts: a missing ci delta is shown", R, BAD, [(RC, "\tcase !ciDeltas[f.CIDelta]: // rule:facts-cidelta\n", "\tcase f.CIDelta != \"\" && !ciDeltas[f.CIDelta]: // rule:facts-cidelta\n")]),
 ("facts: an unread pipeline may carry values", R, BAD, [(RC, "len(p.Jobs) != 0 { // rule:facts-unread", "len(p.Jobs) != 0 && false { // rule:facts-unread")]),
 ("facts: an unread pipeline may carry jobs", R, BAD, [(RC, "\t\tif p.ID != 0 || p.SHA != \"\" || p.Status != \"\" || len(p.Jobs) != 0 { // rule:facts-unread", "\t\tif p.ID != 0 || p.SHA != \"\" || p.Status != \"\" { // rule:facts-unread")]),
 ("facts: a pipeline of another commit is shown as the head's", R, BAD, [(RC, "\t\tcase p.SHA != f.HeadSHA: // rule:facts-pipeline-sha\n", "\t\tcase false: // rule:facts-pipeline-sha\n")]),
 ("facts: a read pipeline needs no id", R, BAD, [(RC, "\t\tcase p.ID < 1:\n", "\t\tcase false:\n")]),
 ("facts: a read pipeline needs no status", R, BAD, [(RC, "\t\tcase strings.TrimSpace(p.Status) == \"\":\n", "\t\tcase false:\n")]),
 ("facts: a job needs no name", R, BAD, [(RC, "\t\t\tif strings.TrimSpace(j.Name) == \"\" || strings.TrimSpace(j.Status) == \"\" {", "\t\t\tif strings.TrimSpace(j.Status) == \"\" {")]),
 ("facts: a job needs no status", R, BAD, [(RC, "\t\t\tif strings.TrimSpace(j.Name) == \"\" || strings.TrimSpace(j.Status) == \"\" {", "\t\t\tif strings.TrimSpace(j.Name) == \"\" {")]),
 ("facts: a linked issue needs no iid", R, BAD, [(RC, "\tif f.Issue != nil && (f.Issue.IID < 1 || strings.TrimSpace(f.Issue.Title) == \"\") {", "\tif f.Issue != nil && strings.TrimSpace(f.Issue.Title) == \"\" {")]),
 ("facts: a linked issue needs no title", R, BAD, [(RC, "\tif f.Issue != nil && (f.Issue.IID < 1 || strings.TrimSpace(f.Issue.Title) == \"\") {", "\tif f.Issue != nil && f.Issue.IID < 1 {")]),
 ("facts: a diff that cannot be keyed is skipped quietly instead of refused", R, BAD,
  [(RC, "\tif err != nil {\n\t\treturn bad(\"diff: %v\", err)\n\t}\n", "\tif err != nil {\n\t\treturn Context{}, &Skip{Reason: SkipDiffNotReviewable, Detail: err.Error()}\n\t}\n")]),

 ("render: an unread pipeline is shown with values", R, FACTS,
  [(RC, "\tif !p.Read { // rule:unread-pipeline\n", "\tif false { // rule:unread-pipeline\n")]),
 ("render: 'allowed to fail' is not shown", R, FACTS, [(RC, "\t\t\tif j.AllowFailure {\n", "\t\t\tif false {\n")]),
 ("render: the prompt hash is not the prompt's", R, FACTS, [(RC, "\tsum := sha256.Sum256([]byte(prompt))\n", "\tsum := sha256.Sum256([]byte(annotated))\n")]),
 ("render: a missing issue is not said to be missing", R, FACTS,
  [(RC, "\t\tb.WriteString(\"(none: this merge request has no linked issue)\\n\")\n", "\t\tb.WriteString(\"\\n\")\n")]),
 ("render: the issue body is left out", R, FACTS,
  [(RC, "\t\tb.WriteString(untrustedBlock(\"issue body\", capText(f.Issue.Body, CapIssueBody)))\n", "\t\tb.WriteString(untrustedBlock(\"issue body\", \"\"))\n")]),
 ("render: the base commit is shown as the head", R, FACTS,
  [(RC, "\tfmt.Fprintf(&b, \"base commit: %s\\n\", f.BaseSHA)\n", "\tfmt.Fprintf(&b, \"base commit: %s\\n\", f.HeadSHA)\n")]),
 ("render: the map returned is not the one that was rendered", R, 'TestPromptCoordinatesResolveInTheMap',
  [(RC, "PromptSHA256: hex.EncodeToString(sum[:]), Map: m}, nil", "PromptSHA256: hex.EncodeToString(sum[:]), Map: emptyMap()}, nil"),
   (RC, "func clip(s string) string {", "func emptyMap() *diffmap.Map {\n\tm, _ := diffmap.Build(nil)\n\treturn m\n}\n\nfunc clip(s string) string {")]),

 ("forge: a scalar keeps its line breaks", R, PFORGE,
  [(RC, "\ts = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), \" \") // rule:one-line\n", "")]),
 ("forge: a scalar keeps its control characters", R, PFORGE,
  [(RC, "func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), \" \") // rule:one-line", "unicode.IsSpace), \" \") // rule:one-line")]),
 ("forge: a scalar keeps fence tokens", R, PFORGE,
  [(RC, "\ts = fenceTokenRe.ReplaceAllStringFunc(s, func(m string) string { return m[:2] }) // rule:fence-token\n", "")]),
 ("forge: the fence id is not a hash of the body", R, PFORGE,
  [(RC, "\tsum := sha256.Sum256([]byte(label + \"\\x00\" + body)) // rule:fence-id\n", "\tsum := sha256.Sum256([]byte(label)) // rule:fence-id\n")]),
 ("forge: fenced text keeps its control characters", R, PFORGE,
  [(RC, "\t\tif unicode.IsControl(r) { // rule:text-controls\n\t\t\treturn -1\n", "\t\tif unicode.IsControl(r) { // rule:text-controls\n\t\t\treturn r\n")]),
 ("forge: the issue body is shown outside a fence", R, PFORGE,
  [(RC, "\t\tb.WriteString(untrustedBlock(\"issue body\", capText(f.Issue.Body, CapIssueBody)))\n", "\t\tb.WriteString(capText(f.Issue.Body, CapIssueBody) + \"\\n\")\n")]),
 ("forge: the annotated diff is shown outside a fence", R, PFORGE,
  [(RC, "\tb.WriteString(untrustedBlock(\"annotated diff\", annotated))\n", "\tb.WriteString(annotated)\n")]),
 ("forge: a job name is not made one line", R, PFORGE,
  [(RC, "strconv.Quote(oneLine(j.Name, CapJobField))", "strconv.Quote(\"\")[:1]+j.Name+strconv.Quote(\"\")[:1]")]),
 ("forge: a job's stage is not made one line", R, PFORGE,
  [(RC, "oneLine(j.Stage, CapJobField), oneLine(j.Status, CapJobField), allowed)", "j.Stage, oneLine(j.Status, CapJobField), allowed)")]),
 ("forge: the pipeline status is not made one line", R, PFORGE,
  [(RC, "\t\tfmt.Fprintf(&b, \"status: %s\\n\", oneLine(p.Status, CapJobField))\n", "\t\tfmt.Fprintf(&b, \"status: %s\\n\", p.Status)\n")]),
 ("forge: the target branch is not made one line", R, PFORGE,
  [(RC, "\tfmt.Fprintf(&b, \"target branch: %s\\n\", oneLine(f.TargetBranch, CapBranch))\n", "\tfmt.Fprintf(&b, \"target branch: %s\\n\", f.TargetBranch)\n")]),
 ("forge: the issue title is not made one line", R, PFORGE,
  [(RC, "\t\tfmt.Fprintf(&b, \"title: %s\\n\", oneLine(f.Issue.Title, CapTitle))\n", "\t\tfmt.Fprintf(&b, \"title: %s\\n\", f.Issue.Title)\n")]),

 ("caps: a cut issue body is not marked truncated", R, CUT,
  [(RC, "\treturn kept + fmt.Sprintf(\"%sissue text truncated: %d more bytes not shown]\\n\", pipelinectx.MarkerPrefix, len(s)-max) // rule:truncation-marked\n", "\treturn kept // rule:truncation-marked\n")]),
 ("caps: the issue body is not cut", R, CUT, [(RC, "\tif len(s) <= max {\n\t\treturn s\n\t}\n\tkept := cutUTF8(s, max)", "\tif true {\n\t\treturn s\n\t}\n\tkept := cutUTF8(s, max)")]),
 ("caps: an issue body exactly at the cap is marked truncated", R, CUT, [(RC, "\tif len(s) <= max {\n\t\treturn s\n\t}\n\tkept := cutUTF8(s, max)", "\tif len(s) < max {\n\t\treturn s\n\t}\n\tkept := cutUTF8(s, max)")]),
 ("caps: a long scalar is not cut", R, CUT, [(RC, "\tif len(s) > max {\n\t\ts = cutUTF8(s, max-len(truncated)) + truncated\n\t}\n", "")]),
 ("caps: a cut scalar is not marked", R, CUT, [(RC, "\t\ts = cutUTF8(s, max-len(truncated)) + truncated\n", "\t\ts = cutUTF8(s, max)\n")]),
 ("caps: every job is listed", R, CUT, [(RC, "\t\t\tif i == MaxJobs { // rule:cap-jobs\n", "\t\t\tif i < 0 { // rule:cap-jobs\n")]),
 ("caps: jobs past the cap are dropped without a marker", R, CUT,
  [(RC, "\t\t\t\tfmt.Fprintf(&b, \"%s%d more jobs not listed]\\n\", pipelinectx.MarkerPrefix, len(p.Jobs)-MaxJobs)\n", "")]),
 ("redaction: fenced project text is not redacted", R, REDACT,
  [(RC, "\ts, _ = pipelinectx.Redact(s)\n\tif len(s) <= max {\n", "\tif len(s) <= max {\n")]),
 ("redaction: a scalar is not redacted", R, REDACT,
  [(RC, "\ts, _ = pipelinectx.Redact(s)\n\ts = fenceTokenRe", "\ts = fenceTokenRe")]),
]

args = sys.argv[1:]
verbose = '-v' in args
if '--list' in args:
    for i, (name, pkg, test, _) in enumerate(muts):
        print(f"[{i}] {pkg} {test}: {name}")
    sys.exit(0)
only = [a for a in args if a.isdigit()]

base = subprocess.run([GO, "test", D, R, "-count=1"], capture_output=True, text=True)
if base.returncode != 0:
    print("the unmutated packages are not green; a kill would mean nothing:\n" + base.stdout + base.stderr)
    sys.exit(2)
print("baseline: green (" + " ".join(l.split('\t')[1].rsplit('/', 1)[-1] for l in base.stdout.splitlines() if l.startswith("ok")) + ")", flush=True)

res = []
for i, (name, pkg, test, edits) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals, created = {}, []
    vacuous = False
    for f, old, _ in edits:
        if old is None:
            if os.path.exists(f):
                vacuous = True
            continue
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if old is None:
            continue
        if old not in cur[f] or old == new:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        for f, old, new in edits:
            if old is None:
                open(f, 'w').write(new)
                created.append(f)
        r = subprocess.run([GO, "test", pkg, "-run", "^%s$" % test, "-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        lines = out.splitlines()
        fails = [l.strip() for l in lines if l.startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}  ({test})")
        elif not fails or "[build failed]" in out:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:1200]}")
        else:
            msg = f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails)
            if verbose:
                first = lines.index(next(l for l in lines if l.startswith("--- FAIL")))
                detail = [l.strip() for l in lines[first + 1:first + 14]
                          if l.startswith("    ") and not l.strip().startswith("--- ")][:2]
                msg += "\n             > " + "\n             > ".join(d[:400] for d in detail)
            res.append(msg)
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
        for f in created:
            os.remove(f)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} of {len(res)}")
sys.exit(1 if bad else 0)
