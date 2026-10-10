package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// The identity of ONE pipeline fix (gonk-2usw plan section 3.2), and the two
// markers a fix is found by on GitLab. Everything here is pure and derived
// from the diagnosed run's anchor: one fix per diagnosed run.
//
//	fix anchor    the run anchor with ":pipeline:" replaced by ":pipefix:".
//	              It is the fix bead's BeadAnchor: the meter's attempts,
//	              ladder and budget for the fix are keyed on it, separately
//	              from the diagnosis's.
//	fix branch    FixBranchPrefix + the first 12 hex of sha256(fix anchor).
//	session key   "gonk-<pid>-pf-" + the same 12 hex.
//	fix note      ONE note per fix on the diagnosis landing, carrying
//	              FixMarker(fix anchor, state) and edited in place (8.1).
//	MR marker     FixMRMarker(fix anchor, cause key) in the fix MR's
//	              description: how a LATER look recognises "a merge request
//	              for this cause already exists / was closed unmerged" (E9).
//
// The writer and every reader use these functions, so the formats cannot
// drift between the sweep that dispatches a fix, the dispatch that says so on
// the landing, and the apply that opens the merge request.

// fixAnchorRe is anchorRe with the fix segment. Group 1 is the project id.
var fixAnchorRe = regexp.MustCompile(`^gonk:([1-9][0-9]*):pipefix:(?:mr[1-9][0-9]*|ref-[0-9a-f]{8}):[0-9a-f]{16}:s(?:0|[1-9][0-9]*)(?::f([1-9][0-9]*))?$`)

const (
	anchorRunSegment = ":pipeline:"
	anchorFixSegment = ":pipefix:"
)

// FixAnchor is the fix anchor of a diagnosed run. Anything that is not
// Anchor-shaped is refused.
func FixAnchor(runAnchor string) (string, error) {
	if !anchorRe.MatchString(runAnchor) {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline anchor", runAnchor)
	}
	return strings.Replace(runAnchor, anchorRunSegment, anchorFixSegment, 1), nil
}

// RunAnchorOfFix inverts FixAnchor.
func RunAnchorOfFix(fixAnchor string) (string, error) {
	if !fixAnchorRe.MatchString(fixAnchor) {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)
	}
	return strings.Replace(fixAnchor, anchorFixSegment, anchorRunSegment, 1), nil
}

// IsFixAnchor reports whether s is exactly a fix anchor.
func IsFixAnchor(s string) bool { return fixAnchorRe.MatchString(s) }

func fixAnchorHash(fixAnchor string) (pid, hash12 string, err error) {
	m := fixAnchorRe.FindStringSubmatch(fixAnchor)
	if m == nil {
		return "", "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)
	}
	sum := sha256.Sum256([]byte(fixAnchor))
	return m[1], hex.EncodeToString(sum[:])[:12], nil
}

// FixBranch is the branch a fix is committed to (plan 3.2). IsFixBranch
// accepts exactly what this returns.
func FixBranch(fixAnchor string) (string, error) {
	_, h, err := fixAnchorHash(fixAnchor)
	if err != nil {
		return "", err
	}
	return FixBranchPrefix + h, nil
}

// FixSessionKey is the fix's DNS-safe session key. It differs from the
// diagnosed run's SessionKey in its infix ("pf", not "pl") and in the anchor
// hashed, so the fix's spend is never booked on the diagnosis's session.
func FixSessionKey(fixAnchor string) (string, error) {
	pid, h, err := fixAnchorHash(fixAnchor)
	if err != nil {
		return "", err
	}
	return "gonk-" + pid + "-pf-" + h, nil
}

// ---- the fix note's marker (plan 8.1) ------------------------------------

// FixNoteNotEligible is the one fix-note state that is not a fix state of
// plan 7.1: the note says why NO fix is attempted (rows E4-E12 marked N).
// FixNoteProposing is pipefix.StateProposing, named here because this
// package cannot import pkg/pipefix (it imports this one).
const (
	FixNoteNotEligible = "not-eligible"
	FixNoteProposing   = "proposing"
)

// fixNoteStates is plan 7.1's closed set, in the table's order, plus
// FixNoteNotEligible. pkg/pipefix pins its States() to this list
// (TestFixNoteStatesCoverEveryFixState).
var fixNoteStates = []string{
	FixNoteNotEligible,
	FixNoteProposing, "needs-scope", "awaiting-run", "verifying", "fixed", "fixed-new-failure",
	"not-fixed", "flaky", "unverifiable", "regressed", "denied", "taken-over", "closed-unmerged",
	"merged", "confirmed", "recurred",
}

// FixNoteStates is the closed set of states a fix marker may carry.
func FixNoteStates() []string { return append([]string(nil), fixNoteStates...) }

// KnownFixNoteState reports whether s is one of FixNoteStates.
func KnownFixNoteState(s string) bool {
	for _, k := range fixNoteStates {
		if s == k {
			return true
		}
	}
	return false
}

const fixMarkerPrefix = "<!-- gonk:pipeline-fix:"

// FixMarker renders the fix note's marker:
//
//	<!-- gonk:pipeline-fix:<fix anchor> state=<state> -->
//
// Only a marker in a note AUTHORED BY THE BOT counts; enforcing that is the
// reader's job, as for RunMarker.
func FixMarker(fixAnchor, state string) (string, error) {
	if !fixAnchorRe.MatchString(fixAnchor) {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)
	}
	if !KnownFixNoteState(state) {
		return "", fmt.Errorf("pipelinectx: unknown fix note state %q", state)
	}
	return fixMarkerPrefix + fixAnchor + " state=" + state + " -->", nil
}

// fixMarkerRe matches any fix marker. The state group is wider than the
// closed set so a marker from a NEWER writer is found and reported.
var fixMarkerRe = regexp.MustCompile(`<!-- gonk:pipeline-fix:(\S+) state=([a-z_-]+) -->`)

// FixMarkerStates returns the state of every fix marker for exactly fixAnchor
// in body, in order of appearance (normally zero or one). A returned state
// may be outside FixNoteStates.
func FixMarkerStates(body, fixAnchor string) []string {
	if !strings.Contains(body, fixMarkerPrefix) {
		return nil
	}
	var out []string
	for _, m := range fixMarkerRe.FindAllStringSubmatch(body, -1) {
		if m[1] == fixAnchor {
			out = append(out, m[2])
		}
	}
	return out
}

// ---- the fix merge request's marker (plan 3.2 E9, 5.4) --------------------

var causeKeyRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

const fixMRMarkerPrefix = "<!-- gonk:pipeline-fix-mr:"

// FixMRMarker is the line of a fix MR's controller-authored metadata block
// that names the fix and its cause:
//
//	<!-- gonk:pipeline-fix-mr:<fix anchor> cause=<cause key> -->
//
// Part APPLY writes it into the MR description; the dispatch's dedup (E9)
// reads it back from bot-authored MRs on fix branches. The cause key must be
// CauseKey-shaped (16 lowercase hex).
func FixMRMarker(fixAnchor, causeKey string) (string, error) {
	if !fixAnchorRe.MatchString(fixAnchor) {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline-fix anchor", fixAnchor)
	}
	if !causeKeyRe.MatchString(causeKey) {
		return "", fmt.Errorf("pipelinectx: cause key %q is not 16 lowercase hex", causeKey)
	}
	return fixMRMarkerPrefix + fixAnchor + " cause=" + causeKey + " -->", nil
}

var fixMRMarkerRe = regexp.MustCompile(`<!-- gonk:pipeline-fix-mr:(\S+) cause=([0-9a-f]{16}) -->`)

// ParseFixMRMarker reads the fix anchor and cause key out of an MR
// description. ok is false when there is no well-formed marker, or when the
// description carries markers that DISAGREE: a description naming two fixes
// or two causes names none, and the caller treats the MR as unreadable
// rather than picking one.
func ParseFixMRMarker(description string) (fixAnchor, causeKey string, ok bool) {
	if !strings.Contains(description, fixMRMarkerPrefix) {
		return "", "", false
	}
	for _, m := range fixMRMarkerRe.FindAllStringSubmatch(description, -1) {
		if !fixAnchorRe.MatchString(m[1]) {
			return "", "", false
		}
		if ok && (m[1] != fixAnchor || m[2] != causeKey) {
			return "", "", false
		}
		fixAnchor, causeKey, ok = m[1], m[2], true
	}
	return fixAnchor, causeKey, ok
}
