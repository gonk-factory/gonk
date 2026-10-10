package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ExcerptVersion names the excerpt algorithm. It is recorded on the bead at
// dispatch (Record.PipelineExcerptVersion) and checked when the sweep re-derives
// the excerpt: ANY change to Normalize, Redact, the selection or the caps that
// can change a single output byte MUST bump it, or every in-flight run would
// fail its context-integrity check (plan D8 step 5). TestExcerptGoldenSHA pins
// the bytes so such a change cannot land silently.
//
// An excerpt recorded under ANOTHER version -- older or newer -- is never
// checked against this build's algorithm: VerifyExcerpt answers
// ErrExcerptVersion, which the sweep judges UNKNOWN (the run is retried with a
// fresh dispatch), never a silent pass.
//
//	1  the P1 algorithm (2026-10-04).
//	2  Normalize strips GitLab's timestamped-trace line prefix and joins
//	   continuation lines (gonk-mf3v). The bytes change only for a trace
//	   that carries the prefix; the version is bumped all the same, because
//	   every real trace does.
const ExcerptVersion = 2

// Trace and excerpt caps (plan D7). Pinned by TestCapsPinned.
const (
	// MaxTraceBytes: at most this much of a trace is ever considered; a longer
	// trace keeps its LAST MaxTraceBytes and is marked head-truncated.
	MaxTraceBytes = 8 << 20
	// TailLines: the excerpt keeps the last TailLines lines...
	TailLines = 120
	// ...plus a window of up to ErrorWindowLines lines around the FIRST error
	// line, when that line lies before the tail: ErrorWindowBefore lines before
	// it, the line itself, and the rest after it.
	ErrorWindowLines  = 30
	ErrorWindowBefore = 10
	// MaxJobExcerptLines / MaxJobExcerptBytes: hard caps on one job's excerpt,
	// gonk's marker lines included.
	MaxJobExcerptLines = 150
	MaxJobExcerptBytes = 6 << 10
	// MaxTotalExcerptBytes: all excerpts of one pipeline together.
	MaxTotalExcerptBytes = 12 << 10
	// MaxExcerptJobs: failed jobs beyond this many are listed without excerpts.
	MaxExcerptJobs = 4
	// MaxLineBytes: one log line is cut to this many bytes (suffix included).
	MaxLineBytes = 512
	// minTailLines: when the caps force lines out, the tail is shortened to
	// this before the first-error window loses any line.
	minTailLines = 20
)

// Prompt block caps (plan D6 table), consumed by the context builder (P6).
// Pinned by TestCapsPinned.
const (
	CapPipelineBlock       = 1 << 10  // pipeline facts
	CapCommitBlock         = 2 << 10  // commit title/author/message
	CapMRBlock             = 2 << 10  // MR facts (MR target)
	CapRunFactsBlock       = 512      // run facts
	CapFailedJobPerJob     = 512      // per failed job facts
	CapJobDefinitionPerJob = 1536     // per failed job, merged-YAML block
	CapJobHistoryPerJob    = 512      // per failed job, last 5 terminal runs
	CapOtherJobsBlock      = 1 << 10  // names+status of non-failed jobs
	MaxPromptBytes         = 28 << 10 // the whole rendered prompt
	CapExcerptsTotal       = MaxTotalExcerptBytes
)

// errorLineRe finds the FIRST error line (plan D7).
var errorLineRe = regexp.MustCompile(`(?i)\b(error|fatal|failed|failure|panic|exception)\b`)

// Marker lines gonk inserts into an excerpt.
var (
	MarkerHeadTruncated = MarkerPrefix + "log truncated: earlier output was not read]"
	MarkerTailTruncated = MarkerPrefix + "log truncated: later output was not read]"
)

// OmittedMarker is the line standing in for n log lines left out.
func OmittedMarker(n int) string {
	return MarkerPrefix + strconv.Itoa(n) + " lines omitted]"
}

// Options tune Excerpt for a caller that has more facts than the bytes.
type Options struct {
	// HeadTruncated: the caller already dropped the start of the trace (it
	// read only a tail). The first, partial line is discarded and the excerpt
	// is marked. Excerpt also sets this itself for a trace > MaxTraceBytes.
	HeadTruncated bool
	// TailTruncated: the caller stopped reading before the end of the trace.
	TailTruncated bool
	// MaxBytes: per-job byte cap; 0 means MaxJobExcerptBytes, and a larger
	// value is clamped to it. Use JobBudget to share MaxTotalExcerptBytes.
	MaxBytes int
}

// JobExcerpt is one job's excerpt. Text is what the model is shown and what
// grounding checks against; SHA256 is what the bead records.
type JobExcerpt struct {
	Version int
	// Text: selected lines joined by "\n", with a trailing "\n"; "" when the
	// trace had no lines at all.
	Text string
	// SHA256: lowercase hex sha256 of Text.
	SHA256        string
	HeadTruncated bool
	TailTruncated bool
	// FirstError: the first line (normalised, redacted, cut to MaxLineBytes)
	// matching the error pattern, "" if none. It may lie outside Text when the
	// caps squeezed it out.
	FirstError string
	// ErrorSignature: ErrorSignature(FirstError); "" when FirstError is "".
	ErrorSignature string
	// TotalLines: lines in the normalised, redacted trace considered.
	TotalLines int
	// ShownLines: log lines (gonk markers excluded) in Text.
	ShownLines int
}

// Groundable reports whether the excerpt holds at least one log line a model
// could quote (gonk's markers alone are not evidence).
func (e JobExcerpt) Groundable() bool { return e.ShownLines > 0 }

// Excerpt is ExcerptWith(trace, Options{}).
func Excerpt(trace []byte) JobExcerpt { return ExcerptWith(trace, Options{}) }

// JobBudget is the per-job byte cap when n jobs share MaxTotalExcerptBytes
// evenly: min(MaxJobExcerptBytes, MaxTotalExcerptBytes/n). n is clamped to
// [1, MaxExcerptJobs]. Deterministic in n alone, so the sweep can re-derive the
// excerpt with the same cap from the recorded job list.
func JobBudget(n int) int {
	if n < 1 {
		n = 1
	}
	if n > MaxExcerptJobs {
		n = MaxExcerptJobs
	}
	return min(MaxJobExcerptBytes, MaxTotalExcerptBytes/n)
}

// ExcerptWith builds one job's excerpt (plan D7): consider at most the last
// MaxTraceBytes, Normalize, Redact, then keep the last TailLines lines plus up
// to ErrorWindowLines around the first error line when it lies before the tail,
// within MaxJobExcerptLines lines and the byte cap. Pure and deterministic.
//
// When the caps force lines out, the tail's oldest lines go first (down to
// minTailLines), then the error window's lines after the error, then those
// before it, then the rest of the tail, then the window itself.
func ExcerptWith(trace []byte, o Options) JobExcerpt {
	maxBytes := o.MaxBytes
	if maxBytes <= 0 || maxBytes > MaxJobExcerptBytes {
		maxBytes = MaxJobExcerptBytes
	}
	head := o.HeadTruncated
	if len(trace) > MaxTraceBytes {
		trace = trace[len(trace)-MaxTraceBytes:]
		head = true
	}
	if head {
		// The first line is a fragment of a line whose start was not read.
		if i := strings.IndexByte(string(trace), '\n'); i >= 0 {
			trace = trace[i+1:]
		}
	}

	norm := Normalize(trace)
	redacted, _ := Redact(strings.Join(norm, "\n"))
	var lines []string
	if len(norm) > 0 {
		lines = strings.Split(redacted, "\n")
	}

	fe := -1
	for i, l := range lines {
		if !isMarker(l) && errorLineRe.MatchString(l) {
			fe = i
			break
		}
	}

	s := selection{n: len(lines), head: head, tailTrunc: o.TailTruncated}
	s.ts, s.te = max(0, len(lines)-TailLines), len(lines)
	if fe >= 0 && fe < s.ts {
		s.window, s.fe = true, fe
		s.ws = max(0, fe-ErrorWindowBefore)
		s.we = min(fe+ErrorWindowLines-ErrorWindowBefore, s.ts)
	}

	var out []string
	for {
		out = s.render(lines)
		if len(out) <= MaxJobExcerptLines && textLen(out) <= maxBytes {
			break
		}
		if !s.shrink() {
			break
		}
	}

	e := JobExcerpt{
		Version:       ExcerptVersion,
		HeadTruncated: head,
		TailTruncated: o.TailTruncated,
		TotalLines:    len(lines),
	}
	if len(out) > 0 {
		e.Text = strings.Join(out, "\n") + "\n"
	}
	for _, l := range out {
		if !isMarker(l) {
			e.ShownLines++
		}
	}
	sum := sha256.Sum256([]byte(e.Text))
	e.SHA256 = hex.EncodeToString(sum[:])
	if fe >= 0 {
		e.FirstError = truncateLine(lines[fe], MaxLineBytes)
		e.ErrorSignature = ErrorSignature(e.FirstError)
	}
	return e
}

// selection is the pair of line ranges an excerpt shows: an optional window
// [ws,we) around the first error fe, and the tail [ts,te).
type selection struct {
	n               int
	head, tailTrunc bool
	window          bool
	fe, ws, we      int
	ts, te          int
}

func (s *selection) render(lines []string) []string {
	var out []string
	if s.head {
		out = append(out, MarkerHeadTruncated)
	}
	first := s.ts
	if s.window {
		first = s.ws
	}
	if first > 0 && first <= s.n {
		out = append(out, OmittedMarker(first))
	}
	if s.window {
		for _, l := range lines[s.ws:s.we] {
			out = append(out, truncateLine(l, MaxLineBytes))
		}
		if gap := s.ts - s.we; gap > 0 {
			out = append(out, OmittedMarker(gap))
		}
	}
	for _, l := range lines[s.ts:s.te] {
		out = append(out, truncateLine(l, MaxLineBytes))
	}
	if s.tailTrunc {
		out = append(out, MarkerTailTruncated)
	}
	return out
}

// shrink drops ONE line in priority order; false when nothing is left to drop.
func (s *selection) shrink() bool {
	tail := s.te - s.ts
	switch {
	case tail > minTailLines:
		s.ts++
	case s.window && s.we-1 > s.fe:
		s.we--
	case s.window && s.ws < s.fe:
		s.ws++
	case tail > 1:
		s.ts++
	case s.window:
		s.window = false
	case tail > 0:
		s.ts++
	default:
		return false
	}
	return true
}

func textLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

// Errors from VerifyExcerpt.
var (
	ErrExcerptVersion = errors.New("pipelinectx: excerpt version differs")
	ErrExcerptChanged = errors.New("pipelinectx: excerpt hash differs")
)

// VerifyExcerpt re-derives the excerpt of trace with o and checks it against
// what was recorded at dispatch: the version and the sha256. On success it
// returns the re-derived excerpt (whose Text grounding then checks against).
// A version mismatch or hash mismatch is an error wrapping ErrExcerptVersion /
// ErrExcerptChanged: the context the model saw cannot be reproduced, so its
// evidence cannot be checked (plan D8 step 5: UNKNOWN, not gate-failed).
func VerifyExcerpt(trace []byte, o Options, version int, wantSHA256 string) (JobExcerpt, error) {
	if version != ExcerptVersion {
		return JobExcerpt{}, fmt.Errorf("%w: recorded %d, this build %d", ErrExcerptVersion, version, ExcerptVersion)
	}
	e := ExcerptWith(trace, o)
	if e.SHA256 != strings.ToLower(wantSHA256) {
		return JobExcerpt{}, fmt.Errorf("%w: recorded %s, re-derived %s", ErrExcerptChanged, wantSHA256, e.SHA256)
	}
	return e, nil
}
