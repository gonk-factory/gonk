package pipelinectx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCapsPinned: the caps are a contract with the prompt builder (P6) and
// owner question Q1; changing one is a decision, not a refactor.
func TestCapsPinned(t *testing.T) {
	pins := []struct {
		name      string
		got, want int
	}{
		{"ExcerptVersion", ExcerptVersion, 2},
		{"SignatureVersion", SignatureVersion, 1},
		{"MaxTraceBytes", MaxTraceBytes, 8 * 1024 * 1024},
		{"TailLines", TailLines, 120},
		{"ErrorWindowLines", ErrorWindowLines, 30},
		{"ErrorWindowBefore", ErrorWindowBefore, 10},
		{"MaxJobExcerptLines", MaxJobExcerptLines, 150},
		{"MaxJobExcerptBytes", MaxJobExcerptBytes, 6 * 1024},
		{"MaxTotalExcerptBytes", MaxTotalExcerptBytes, 12 * 1024},
		{"MaxExcerptJobs", MaxExcerptJobs, 4},
		{"MaxLineBytes", MaxLineBytes, 512},
		{"MinGroundedRunes", MinGroundedRunes, 12},
		{"MinRepeatCollapse", MinRepeatCollapse, 4},
		{"CapPipelineBlock", CapPipelineBlock, 1024},
		{"CapCommitBlock", CapCommitBlock, 2048},
		{"CapMRBlock", CapMRBlock, 2048},
		{"CapRunFactsBlock", CapRunFactsBlock, 512},
		{"CapFailedJobPerJob", CapFailedJobPerJob, 512},
		{"CapJobDefinitionPerJob", CapJobDefinitionPerJob, 1536},
		{"CapJobHistoryPerJob", CapJobHistoryPerJob, 512},
		{"CapOtherJobsBlock", CapOtherJobsBlock, 1024},
		{"CapExcerptsTotal", CapExcerptsTotal, 12 * 1024},
		{"MaxPromptBytes", MaxPromptBytes, 28 * 1024},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %d, pinned %d", p.name, p.got, p.want)
		}
	}
}

func numbered(n int, f string) []byte {
	var b bytes.Buffer
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, f+"\n", i)
	}
	return b.Bytes()
}

func lines(e JobExcerpt) []string {
	return strings.Split(strings.TrimSuffix(e.Text, "\n"), "\n")
}

func TestExcerptShortTraceWhole(t *testing.T) {
	e := Excerpt(numbered(5, "step %d ok"))
	want := "step 1 ok\nstep 2 ok\nstep 3 ok\nstep 4 ok\nstep 5 ok\n"
	if e.Text != want {
		t.Fatalf("got %q", e.Text)
	}
	if e.Version != ExcerptVersion || e.HeadTruncated || e.ShownLines != 5 || e.TotalLines != 5 || e.FirstError != "" || e.ErrorSignature != "" {
		t.Fatalf("meta %+v", e)
	}
}

func TestExcerptEmpty(t *testing.T) {
	for _, in := range []string{"", "\n\n", "   \n\t\n", "\x1b[0K\n"} {
		e := Excerpt([]byte(in))
		if e.Text != "" || e.Groundable() {
			t.Fatalf("%q -> %+v", in, e)
		}
	}
}

func TestExcerptTailOnly(t *testing.T) {
	e := Excerpt(numbered(500, "step %d ok"))
	got := lines(e)
	if got[0] != OmittedMarker(380) {
		t.Fatalf("first line %q", got[0])
	}
	if len(got) != TailLines+1 || got[1] != "step 381 ok" || got[len(got)-1] != "step 500 ok" {
		t.Fatalf("tail wrong: %d lines, %q..%q", len(got), got[1], got[len(got)-1])
	}
}

// The FIRST error line, when it lies before the tail, is shown with a window:
// 10 lines before it, it, and 19 after, then an omission marker, then the tail.
func TestExcerptFirstErrorWindow(t *testing.T) {
	var b bytes.Buffer
	for i := 1; i <= 500; i++ {
		switch i {
		case 100:
			b.WriteString("widget.c: fatal: first thing went wrong\n")
		case 200:
			b.WriteString("a later error: second thing\n")
		default:
			fmt.Fprintf(&b, "step %d ok\n", i)
		}
	}
	e := Excerpt(b.Bytes())
	got := lines(e)
	if len(got) != MaxJobExcerptLines {
		t.Fatalf("lines %d, want exactly the cap %d", len(got), MaxJobExcerptLines)
	}
	if got[0] != OmittedMarker(89) || got[1] != "step 90 ok" || got[11] != "widget.c: fatal: first thing went wrong" {
		t.Fatalf("window head wrong: %q", got[:12])
	}
	if e.FirstError != "widget.c: fatal: first thing went wrong" {
		t.Fatalf("FirstError %q", e.FirstError)
	}
	if !strings.Contains(e.Text, "step 119 ok\n") || strings.Contains(e.Text, "a later error") {
		t.Fatalf("window after-lines wrong:\n%s", e.Text)
	}
	if got[len(got)-1] != "step 500 ok" {
		t.Fatalf("tail end %q", got[len(got)-1])
	}
	// 30 window lines + 120 tail + 2 markers = 152 > 150: the TAIL gives up
	// its two oldest lines, the window keeps all 30.
	if !strings.Contains(e.Text, OmittedMarker(500-118-119)+"\nstep 383 ok\n") {
		t.Fatalf("gap marker / tail start wrong:\n%s", e.Text)
	}
}

func TestExcerptErrorInTailNoWindow(t *testing.T) {
	b := numbered(200, "step %d ok")
	b = append(b, []byte("ERROR: job failed\n")...)
	e := Excerpt(b)
	if strings.Count(e.Text, MarkerPrefix) != 1 || !strings.HasSuffix(e.Text, "ERROR: job failed\n") {
		t.Fatalf("unexpected:\n%s", e.Text)
	}
	if e.FirstError != "ERROR: job failed" {
		t.Fatalf("FirstError %q", e.FirstError)
	}
}

func TestExcerptErrorWordBoundaries(t *testing.T) {
	// "errors"/"terror"/"failures" are not the error words; "Error:" is.
	for in, want := range map[string]string{
		"0 errors\nterror\nfailures: 0\nError: real one\n": "Error: real one",
		"no problems here\n":  "",
		"x PANIC y\n":         "x PANIC y",
		"Exception in main\n": "Exception in main",
	} {
		if got := Excerpt([]byte(in)).FirstError; got != want {
			t.Errorf("%q: FirstError %q, want %q", in, got, want)
		}
	}
}

func TestExcerptByteCapExact(t *testing.T) {
	line := strings.Repeat("x", 99) // 100 bytes with its newline
	var b bytes.Buffer
	for i := 0; i < 300; i++ {
		b.WriteString(line + "\n")
		fmt.Fprintf(&b, "%d\n", i) // defeat repeat collapse
	}
	for _, cap := range []int{MaxJobExcerptBytes, JobBudget(3), JobBudget(4), 1000} {
		e := ExcerptWith(b.Bytes(), Options{MaxBytes: cap})
		if len(e.Text) > cap {
			t.Fatalf("cap %d: excerpt %d bytes", cap, len(e.Text))
		}
		// Tight: one more line of up to 101 bytes would not have fit.
		if len(e.Text) < cap-101 {
			t.Fatalf("cap %d: excerpt only %d bytes, dropped more than needed", cap, len(e.Text))
		}
		if !strings.HasSuffix(e.Text, "299\n") {
			t.Fatalf("cap %d: lost the last line", cap)
		}
	}
	// A cap above the per-job maximum is clamped to it.
	if e := ExcerptWith(b.Bytes(), Options{MaxBytes: 1 << 20}); len(e.Text) > MaxJobExcerptBytes {
		t.Fatalf("clamp: %d bytes", len(e.Text))
	}
}

func TestJobBudget(t *testing.T) {
	for n, want := range map[int]int{0: 6144, 1: 6144, 2: 6144, 3: 4096, 4: 3072, 9: 3072} {
		if got := JobBudget(n); got != want {
			t.Errorf("JobBudget(%d) = %d, want %d", n, got, want)
		}
	}
	if JobBudget(MaxExcerptJobs)*MaxExcerptJobs > MaxTotalExcerptBytes {
		t.Fatal("budgets exceed the total")
	}
}

func TestExcerptLongLineCut(t *testing.T) {
	long := strings.Repeat("\u00e9", 400) // 800 bytes, 2-byte runes
	e := Excerpt([]byte("a\n" + long + "\nb\n"))
	got := lines(e)
	if len(got[1]) > MaxLineBytes || !strings.HasSuffix(got[1], LineTruncatedSuffix) {
		t.Fatalf("not cut: %d bytes", len(got[1]))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got[1], LineTruncatedSuffix)) {
		t.Fatal("cut split a rune or changed text")
	}
}

// A secret straddling the line-cut must be redacted BEFORE the cut, or the
// cut would leave a token too short for its pattern but still a secret prefix.
func TestExcerptRedactsBeforeCut(t *testing.T) {
	// The cut falls 11 bytes into the token: "glpat-AbCd" alone is too short
	// for the gitlab_token pattern, so cutting first would leak it.
	pad := strings.Repeat("p", MaxLineBytes-len(LineTruncatedSuffix)-12)
	secret := fxGLPAT
	e := Excerpt([]byte(pad + " " + secret + " " + strings.Repeat("q", 100) + "\n"))
	if !strings.Contains(e.Text, LineTruncatedSuffix) {
		t.Fatal("fixture did not reach the line cut")
	}
	if strings.Contains(e.Text, "glpat-") {
		t.Fatalf("secret prefix survived the cut:\n%s", e.Text)
	}
}

func TestExcerptRedacts(t *testing.T) {
	raw := secretsLog(t)
	e := Excerpt(raw)
	if _, kinds := Redact(e.Text); kinds != nil {
		t.Fatalf("excerpt still holds %v:\n%s", kinds, e.Text)
	}
	if !strings.Contains(e.Text, "[REDACTED:private_key]") {
		t.Fatalf("not redacted:\n%s", e.Text)
	}
}

// 9 MiB trace: only the last MaxTraceBytes count, the excerpt says so, the
// fragment line at the cut is not shown, and the end of the log survives.
func TestExcerptHeadTruncation(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("very first line: error in the part that was never read\n")
	for i := 0; b.Len() < 9<<20; i++ {
		fmt.Fprintf(&b, "filler line %08d of the synthetic trace\n", i)
	}
	b.WriteString("the very last line\n")
	e := Excerpt(b.Bytes())
	got := lines(e)
	if !e.HeadTruncated || got[0] != MarkerHeadTruncated {
		t.Fatalf("not marked: %v %q", e.HeadTruncated, got[0])
	}
	if got[len(got)-1] != "the very last line" {
		t.Fatalf("end lost: %q", got[len(got)-1])
	}
	if e.FirstError != "" || strings.Contains(e.Text, "never read") {
		t.Fatal("text from beyond MaxTraceBytes leaked in")
	}
	if e.TotalLines >= strings.Count(b.String(), "\n") {
		t.Fatal("did not drop the head")
	}
}

func TestExcerptCallerTruncationFlags(t *testing.T) {
	e := ExcerptWith([]byte("ment of a cut line\nwhole line\n"), Options{HeadTruncated: true, TailTruncated: true})
	want := MarkerHeadTruncated + "\nwhole line\n" + MarkerTailTruncated + "\n"
	if e.Text != want || !e.HeadTruncated || !e.TailTruncated {
		t.Fatalf("got %q", e.Text)
	}
	if only := ExcerptWith(nil, Options{HeadTruncated: true}); only.Groundable() {
		t.Fatal("markers alone must not be groundable")
	}
}

// Determinism: same input -> same bytes, and the hash is pinned so a change to
// the algorithm that does not bump ExcerptVersion fails here.
func TestExcerptGoldenSHA(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitlab_sections.log")
	if err != nil {
		t.Fatal(err)
	}
	a, b := Excerpt(raw), Excerpt(append([]byte(nil), raw...))
	if a.Text != b.Text || a.SHA256 != b.SHA256 {
		t.Fatal("not deterministic")
	}
	const golden = "03a98513b8d1436104c5148f4c52a3745d6b58a30236a2d6d62dcfd3e4e84cc5"
	if a.SHA256 != golden {
		t.Fatalf("excerpt sha256 %s, pinned %s -- if the algorithm changed on purpose, bump ExcerptVersion and re-pin\n%s", a.SHA256, golden, a.Text)
	}
}

// The excerpt of a REAL GitLab trace (gonk-mf3v). The first live pipeline
// diagnosis quoted the failing command, "$ exit 3", and was refused as
// ungrounded: every excerpt line still carried GitLab's "<timestamp> 01O "
// prefix, so the 8-rune quote was neither a whole line nor long enough to
// match inside one.
func TestExcerptOfARealGitLabTrace(t *testing.T) {
	raw, err := os.ReadFile(realTraceFixture)
	if err != nil {
		t.Fatal(err)
	}
	e := Excerpt(raw)
	if m := tracePrefixResidueRe.FindString(e.Text); m != "" {
		t.Errorf("the excerpt still carries a trace prefix (%q):\n%s", m, e.Text)
	}
	if strings.Contains(e.Text, "2026-10-04T") {
		t.Errorf("the excerpt carries a trace timestamp:\n%s", e.Text)
	}
	// A short command line is groundable only as a WHOLE line.
	for _, quote := range []string{"$ exit 3", "$ M=GONK_SMOKE_DELIBERATE", "GONK_SMOKE_DELIBERATE_FAILURE 20261004T212509Z",
		"ERROR: Job failed: command terminated with exit code 3", `Preparing the "kubernetes" executor`, "Preparing environment"} {
		if !Grounded(e.Text, quote) {
			t.Errorf("Grounded(excerpt, %q) = false; the line is in the job's log", quote)
		}
	}
	// ...and what the prefix used to make "groundable" is not log text.
	for _, quote := range []string{"01O $ exit 3", "2026-10-04T21:25:31.525472Z 01O $ exit 3", "$ exit 4", "exit 3"} {
		if Grounded(e.Text, quote) {
			t.Errorf("Grounded(excerpt, %q) = true", quote)
		}
	}
	const firstError = "ERROR: Job failed: command terminated with exit code 3"
	if e.FirstError != firstError {
		t.Errorf("FirstError = %q, want %q", e.FirstError, firstError)
	}
	if e.TotalLines != 38 || e.ShownLines != 38 || e.HeadTruncated || e.TailTruncated || e.Version != ExcerptVersion {
		t.Errorf("TotalLines=%d ShownLines=%d head=%v tail=%v version=%d, want 38 38 false false %d",
			e.TotalLines, e.ShownLines, e.HeadTruncated, e.TailTruncated, e.Version, ExcerptVersion)
	}
	// The envelope cost excerpt budget: 47 prefixes of 32 bytes each are gone.
	if len(e.Text) != 1792 || len(raw) != 4058 {
		t.Errorf("excerpt is %d bytes of a %d-byte trace, want 1792 of 4058", len(e.Text), len(raw))
	}
	const golden = "49c63889c14238be14bf55e0dda1593cb1542c531ddb9f9f4a775a6c6e91c3ae"
	if e.SHA256 != golden {
		t.Errorf("excerpt sha256 %s, pinned %s -- if the algorithm changed on purpose, bump ExcerptVersion and re-pin\n%s", e.SHA256, golden, e.Text)
	}

	// The same failure, logged at another time on another stream number, has
	// the same excerpt and the same error signature -- and that signature is
	// the one of the bare error line, with no envelope in it.
	later := regexp.MustCompile(`(?m)^2026-10-04T21:25:[0-9]{2}\.[0-9]{6}Z 0[01]`).ReplaceAllFunc(raw, func(m []byte) []byte {
		return []byte("2027-03-09T08:01:07.000042Z 1" + string(m[len(m)-1:]))
	})
	if bytes.Equal(later, raw) || len(later) != len(raw) {
		t.Fatal("the re-stamped trace is not a re-stamped trace")
	}
	l := Excerpt(later)
	if l.ErrorSignature == "" || l.ErrorSignature != e.ErrorSignature || l.FirstError != e.FirstError {
		t.Errorf("signature %q (%q) at one time, %q (%q) at another", e.ErrorSignature, e.FirstError, l.ErrorSignature, l.FirstError)
	}
	if l.SHA256 != e.SHA256 {
		t.Errorf("the excerpt depends on WHEN the job logged its lines:\n%s\n---\n%s", e.Text, l.Text)
	}
	if want := ErrorSignature(firstError); e.ErrorSignature != want {
		t.Errorf("ErrorSignature = %q, want %q (the signature of the bare error line)", e.ErrorSignature, want)
	}
	if _, err := VerifyExcerpt(raw, Options{}, ExcerptVersion, e.SHA256); err != nil {
		t.Errorf("VerifyExcerpt of the real trace at this version: %v", err)
	}
}

func TestVerifyExcerpt(t *testing.T) {
	raw, err := os.ReadFile("testdata/gitlab_sections.log")
	if err != nil {
		t.Fatal(err)
	}
	e := Excerpt(raw)
	if got, err := VerifyExcerpt(raw, Options{}, ExcerptVersion, e.SHA256); err != nil || got.Text != e.Text {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := VerifyExcerpt(raw, Options{}, ExcerptVersion, strings.ToUpper(e.SHA256)); err != nil {
		t.Fatalf("upper-case hash: %v", err)
	}
	changed := append(append([]byte(nil), raw...), []byte("one more line\n")...)
	if _, err := VerifyExcerpt(changed, Options{}, ExcerptVersion, e.SHA256); !errors.Is(err, ErrExcerptChanged) {
		t.Fatalf("changed trace: %v", err)
	}
	if _, err := VerifyExcerpt(raw, Options{MaxBytes: 100}, ExcerptVersion, e.SHA256); !errors.Is(err, ErrExcerptChanged) {
		t.Fatalf("different cap: %v", err)
	}
	if _, err := VerifyExcerpt(raw, Options{}, ExcerptVersion+1, e.SHA256); !errors.Is(err, ErrExcerptVersion) {
		t.Fatalf("version: %v", err)
	}
	// An excerpt recorded by an OLDER build (gonk-mf3v bumped 1 -> 2) is not
	// checked against this build's algorithm either -- not even when, as for
	// this prefix-less trace, the bytes happen to be the same: the answer is
	// "cannot be reproduced" (the sweep's UNKNOWN), never a pass.
	for _, older := range []int{ExcerptVersion - 1, 1, 0} {
		if got, err := VerifyExcerpt(raw, Options{}, older, e.SHA256); !errors.Is(err, ErrExcerptVersion) || got.Text != "" {
			t.Fatalf("recorded version %d (this build %d): err=%v text=%q, want ErrExcerptVersion and no excerpt", older, ExcerptVersion, err, got.Text)
		}
	}
	// B7 (verify report): the WHOLE hash is compared. A recorded hash that
	// differs from the re-derived one in any position -- first, last or only
	// past a short prefix -- is a changed excerpt; so is a truncated one.
	flip := func(h string, i int) string {
		b := []byte(h)
		if b[i] == '0' {
			b[i] = '1'
		} else {
			b[i] = '0'
		}
		return string(b)
	}
	for name, h := range map[string]string{
		"first hex differs":    flip(e.SHA256, 0),
		"last hex differs":     flip(e.SHA256, len(e.SHA256)-1),
		"differs after 8 hex":  flip(e.SHA256, 8),
		"differs after 32 hex": flip(e.SHA256, 32),
		"truncated to 8 hex":   e.SHA256[:8],
		"truncated by one":     e.SHA256[:len(e.SHA256)-1],
		"empty recorded hash":  "",
		"hash with a suffix":   e.SHA256 + "0",
	} {
		if _, err := VerifyExcerpt(raw, Options{}, ExcerptVersion, h); !errors.Is(err, ErrExcerptChanged) {
			t.Errorf("%s: VerifyExcerpt accepted %q for %q: %v", name, h, e.SHA256, err)
		}
	}
}
