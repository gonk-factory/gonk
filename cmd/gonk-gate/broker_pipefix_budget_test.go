package main

// gonk-x06m: the pipefix agent has its own whole-prompt budget, and the
// previous attempt's diff is cut only past a cap that covers the largest
// allowed fix -- and says so, outside the untrusted block, when it is.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// pfLargestFixDiff is previousFixChange's output for the largest allowed fix:
// 3 files, 40 changed lines in total (20 removed, 20 added), each line long
// enough to be a realistic source line.
func pfLargestFixDiff() string {
	var b strings.Builder
	lines := 0
	for f := 0; f < 3; f++ {
		fmt.Fprintf(&b, "--- internal/subsystem/component_%d/handler_with_a_long_name.go\n", f)
		n := 7
		if f == 2 {
			n = 6
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "-\tresult, err := computeSomethingWithAFairlyLongName(ctx, request.Field%d, option%d, 0)\n", i, f)
			lines++
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "+\tresult, err := computeSomethingWithAFairlyLongName(ctx, request.Field%d, option%d, 1)\n", i, f)
			lines++
		}
	}
	if lines != 40 {
		panic(fmt.Sprintf("fixture has %d lines", lines))
	}
	return b.String()
}

var pfCutNote = regexp.MustCompile(`(?m)^\[gonk: cut: (\d+) of (\d+) bytes shown\]$`)

func pfRetryPrompt(t *testing.T, diff string) (string, *pipefixContext) {
	t.Helper()
	pc := pfMaximalContext()
	pc.Req.Previous.Diff = diff
	return mustFitPipefix(t, pc, plSHA), pc
}

// The largest allowed fix's diff is shown WHOLE to a retry.
func TestPipefixPreviousDiffOfTheLargestFixIsNotCut(t *testing.T) {
	diff := pfLargestFixDiff()
	if len(diff) <= 1024 || len(diff) > pipefixCapPreviousDiff {
		t.Fatalf("fixture: the largest fix's diff is %d bytes; want it past the old cap 1024 and within %d", len(diff), pipefixCapPreviousDiff)
	}
	p, _ := pfRetryPrompt(t, diff)
	if !strings.Contains(p, diff) {
		t.Errorf("a 3-file / 40-line previous diff (%d bytes) is not shown whole", len(diff))
	}
	if pfCutNote.MatchString(p) {
		t.Errorf("a diff that was not cut carries a cut note")
	}
}

// A cut is NEVER silent: the controller's own line, outside every fence, says
// how much of how much is shown; a diff within the cap carries no such line.
func TestPipefixPreviousDiffCutIsMarked(t *testing.T) {
	diff := strings.Repeat("+a line of the previous change\n", 600)
	p, _ := pfRetryPrompt(t, diff)
	outside := pfOutsideFences(t, p)
	m := pfCutNote.FindStringSubmatch(outside)
	if m == nil {
		t.Fatalf("a %d-byte diff was cut to %d and nothing outside the fence says so", len(diff), pipefixCapPreviousDiff)
	}
	if want := fmt.Sprint(len(diff)); m[2] != want {
		t.Errorf("the note says the diff is %s bytes, want %s", m[2], want)
	}
	if shown := atoiOr(m[1]); shown <= 0 || shown > pipefixCapPreviousDiff {
		t.Errorf("the note says %s bytes are shown, want 1..%d", m[1], pipefixCapPreviousDiff)
	}
	if strings.Count(outside, "[gonk: cut:") != 1 {
		t.Errorf("want exactly one cut note outside the fences")
	}
	// The note is the controller's: the diff cannot forge one inside its fence
	// (it stays inside, where the rules call it data), and a short diff has none.
	short, _ := pfRetryPrompt(t, "+x\n")
	if strings.Contains(short, "[gonk: cut:") {
		t.Errorf("a diff within the cap carries a cut note")
	}
}

func atoiOr(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// The pipefix agent's budget is its own: 64 KiB, not the diagnosis agent's
// 28 KiB, which is unchanged.
func TestPipefixPromptBudgetIsItsOwn(t *testing.T) {
	if pipefixMaxPromptBytes != 64<<10 {
		t.Errorf("pipefixMaxPromptBytes = %d, want %d", pipefixMaxPromptBytes, 64<<10)
	}
	if pipelinectx.MaxPromptBytes != 28<<10 {
		t.Errorf("the diagnosis agent's cap is %d, want 28 KiB (unchanged)", pipelinectx.MaxPromptBytes)
	}
	if pipefixCapPreviousDiff != 6144 {
		t.Errorf("pipefixCapPreviousDiff = %d, want 6144", pipefixCapPreviousDiff)
	}
}

// THE WORST CASE at the new budget: nothing is left out and nothing shrinks,
// and there is real headroom -- the budget is not met at a floor.
func TestPipefixWorstCasePromptHasHeadroom(t *testing.T) {
	pc := pfMaximalContext()
	start := pc.ExcerptMaxBytes
	p, err := fitPipefixPrompt(pc, plSHA)
	if err != nil {
		t.Fatal(err)
	}
	const wantHeadroom = 8 << 10
	t.Logf("worst case: %d bytes of %d (headroom %d); excerpt cap %d (from %d)", len(p), pipefixMaxPromptBytes, pipefixMaxPromptBytes-len(p), pc.ExcerptMaxBytes, start)
	if pipefixMaxPromptBytes-len(p) < wantHeadroom {
		t.Errorf("headroom %d bytes, want at least %d", pipefixMaxPromptBytes-len(p), wantHeadroom)
	}
	if pc.ExcerptMaxBytes != start {
		t.Errorf("excerpts shrank to %d from %d in the worst case", pc.ExcerptMaxBytes, start)
	}
	for _, f := range pc.Files {
		if f.Editable && f.Shown != (f.NotShown == "") {
			t.Errorf("%s: inconsistent shown state", f.Path)
		}
	}
	shown := 0
	for _, f := range pc.Files {
		if f.Shown {
			shown++
		}
	}
	if shown != 2 {
		t.Errorf("%d of 2 files shown in the worst case, want all", shown)
	}
}
