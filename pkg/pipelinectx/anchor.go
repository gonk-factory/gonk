package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Target kinds (plan D1). A Target's String form is what the bead records
// (Record.PipelineTarget): "mr:<iid>" or "ref:<name>".
const (
	TargetKindMR  = "mr"
	TargetKindRef = "ref"
)

// Target is where a pipeline failure belongs: an MR, or a ref (v1: only the
// default branch).
type Target struct {
	Kind  string // TargetKindMR | TargetKindRef
	MRIID int64  // Kind == mr
	Ref   string // Kind == ref
}

// MRTarget is the target of an MR pipeline (or a branch pipeline with exactly
// one open MR).
func MRTarget(iid int64) Target { return Target{Kind: TargetKindMR, MRIID: iid} }

// RefTarget is the target of a pipeline on a ref.
func RefTarget(ref string) Target { return Target{Kind: TargetKindRef, Ref: ref} }

// ErrBadTarget: a target that cannot be keyed.
var ErrBadTarget = errors.New("pipelinectx: bad target")

// Validate rejects a target that cannot be keyed: an MR iid < 1, or an empty
// ref or one holding whitespace or control characters (git refs cannot).
func (t Target) Validate() error {
	switch t.Kind {
	case TargetKindMR:
		if t.MRIID < 1 {
			return fmt.Errorf("%w: mr iid %d", ErrBadTarget, t.MRIID)
		}
	case TargetKindRef:
		if t.Ref == "" {
			return fmt.Errorf("%w: empty ref", ErrBadTarget)
		}
		for _, r := range t.Ref {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return fmt.Errorf("%w: ref %q holds whitespace or a control character", ErrBadTarget, t.Ref)
			}
		}
	default:
		return fmt.Errorf("%w: kind %q", ErrBadTarget, t.Kind)
	}
	return nil
}

// String is "mr:<iid>" or "ref:<name>" (the recorded form).
func (t Target) String() string {
	if t.Kind == TargetKindMR {
		return TargetKindMR + ":" + strconv.FormatInt(t.MRIID, 10)
	}
	return TargetKindRef + ":" + t.Ref
}

// ParseTarget inverts Target.String and validates.
func ParseTarget(s string) (Target, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Target{}, fmt.Errorf("%w: %q has no kind", ErrBadTarget, s)
	}
	var t Target
	switch kind {
	case TargetKindMR:
		iid, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || strconv.FormatInt(iid, 10) != rest {
			return Target{}, fmt.Errorf("%w: mr iid %q", ErrBadTarget, rest)
		}
		t = MRTarget(iid)
	case TargetKindRef:
		t = RefTarget(rest)
	default:
		return Target{}, fmt.Errorf("%w: kind %q", ErrBadTarget, kind)
	}
	return t, t.Validate()
}

// Key is the target's part of the anchor: "mr<iid>", or "ref-" + the first 8
// hex of sha256(ref) (a ref may hold '/', ':' and other characters an anchor
// and a session name cannot).
func (t Target) Key() string {
	if t.Kind == TargetKindMR {
		return "mr" + strconv.FormatInt(t.MRIID, 10)
	}
	sum := sha256.Sum256([]byte(t.Ref))
	return "ref-" + hex.EncodeToString(sum[:])[:8]
}

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Anchor is the BeadAnchor of a run (gonk-tios D2, gonk-2usw 3.3):
//
//	gonk:<pid>:pipeline:<target-key>:<fp16>:s<last_success>[:f<fix_epoch>]
//
// lastSuccess is the id of the newest green pipeline on the target, 0 if none.
// fixEpoch is the iid of the gonk pipeline-fix MR whose merge ENDED the
// previous run on this target (see FixEpoch), 0 if none. With fixEpoch == 0
// the suffix is ABSENT, not ":f0": every anchor computed before fix epochs
// existed, and every anchor of a target no fix has merged into, is
// byte-identical to what it always was (TestFixlessIdentityGolden).
//
// Every input is a GitLab fact, so the anchor is restart-safe and two
// near-simultaneous failures of one run compute the same one.
func Anchor(projectID int64, t Target, fingerprint string, lastSuccess, fixEpoch int64) (string, error) {
	if projectID < 1 {
		return "", fmt.Errorf("pipelinectx: project id %d", projectID)
	}
	if err := t.Validate(); err != nil {
		return "", err
	}
	if !fingerprintRe.MatchString(fingerprint) {
		return "", fmt.Errorf("pipelinectx: fingerprint %q is not 16 lowercase hex", fingerprint)
	}
	if lastSuccess < 0 {
		return "", fmt.Errorf("pipelinectx: last success %d", lastSuccess)
	}
	if fixEpoch < 0 {
		return "", fmt.Errorf("pipelinectx: fix epoch %d", fixEpoch)
	}
	a := fmt.Sprintf("gonk:%d:pipeline:%s:%s:s%d", projectID, t.Key(), fingerprint, lastSuccess)
	if fixEpoch != 0 {
		a += ":f" + strconv.FormatInt(fixEpoch, 10)
	}
	return a, nil
}

// anchorRe is the exact shape Anchor produces. Group 1 is the project id,
// group 2 the fix epoch ("" when the anchor has none). ":f0" is NOT an anchor:
// "no fix" has exactly one spelling.
var anchorRe = regexp.MustCompile(`^gonk:([1-9][0-9]*):pipeline:(?:mr[1-9][0-9]*|ref-[0-9a-f]{8}):[0-9a-f]{16}:s(?:0|[1-9][0-9]*)(?::f([1-9][0-9]*))?$`)

// SessionKey is the run's DNS-safe session key (plan D2):
// "gonk-<pid>-pl-" + the first 12 hex of sha256(anchor), pid read from the
// anchor. Anything that is not an Anchor-shaped string is refused. The fix
// epoch needs no handling of its own: it is part of the anchor that is hashed,
// so a run after a merged fix has its own key and a run without one keeps the
// key it always had.
func SessionKey(anchor string) (string, error) {
	m := anchorRe.FindStringSubmatch(anchor)
	if m == nil {
		return "", fmt.Errorf("pipelinectx: %q is not a pipeline anchor", anchor)
	}
	sum := sha256.Sum256([]byte(anchor))
	return "gonk-" + m[1] + "-pl-" + hex.EncodeToString(sum[:])[:12], nil
}

// AnchorFixEpoch reads the fix epoch back out of an anchor: the iid of the
// merged fix MR the run follows, 0 for a run that follows none. Anything that
// is not Anchor-shaped is refused.
func AnchorFixEpoch(anchor string) (int64, error) {
	m := anchorRe.FindStringSubmatch(anchor)
	if m == nil {
		return 0, fmt.Errorf("pipelinectx: %q is not a pipeline anchor", anchor)
	}
	if m[2] == "" {
		return 0, nil
	}
	iid, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pipelinectx: fix epoch of %q: %w", anchor, err)
	}
	return iid, nil
}

// TerminalForHealth reports whether a pipeline status says anything about the
// target's health (plan D4): only "success" and "failed". skipped, canceled,
// manual, created and the running states are passed over.
func TerminalForHealth(status string) bool {
	return status == "success" || status == "failed"
}
