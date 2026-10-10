package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// MaxNameLen caps a session name. A Job's name feeds its pod's name (the Job
// controller appends a 5-character suffix and more) and the job-name label,
// whose limit is 63; 52 leaves that headroom (D2).
const MaxNameLen = 52

// hashLen is how many hex characters of sha256 a shortened name keeps.
const hashLen = 8

// Kind says what a session is about: an issue, a merge request, or a failed
// pipeline (gonk-tios D6: the iid of a pipeline session is the GitLab pipeline
// ID it diagnoses, e.g. gonk-pipeline-p84-pl3200-a1).
type Kind string

const (
	KindIssue        Kind = "i"
	KindMergeRequest Kind = "m"
	KindPipeline     Kind = "pl"
)

// dns1123Label is the Kubernetes DNS-1123 label grammar
// (k8s.io/apimachinery validation.IsDNS1123Label), without its 63 limit,
// which MaxNameLen is stricter than.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name can be a session name: a DNS-1123 label of
// at most MaxNameLen characters. Name always produces one; this exists so a
// runtime can refuse a Spec whose Name was built some other way.
func ValidName(name string) error {
	if len(name) > MaxNameLen {
		return fmt.Errorf("runtime: session name %q is %d characters, max %d", name, len(name), MaxNameLen)
	}
	if !dns1123Label.MatchString(name) {
		return fmt.Errorf("runtime: session name %q is not a DNS-1123 label", name)
	}
	return nil
}

// Name builds the deterministic session name (D2):
//
//	gonk-<agent>-p<projID>-<kind><iid>-a<attempt>
//
// e.g. gonk-triage-p42-i7-a1. A project-scoped session (scaffold, iid 0) has
// no issue segment -- gonk-scaffold-p42-a1 -- because "i0" would claim the
// session was about an issue it is not; this matches how the Gas City alias
// (brokerSessionAlias) treated the same case.
//
// The same inputs always give the same name; that is the whole point, since
// the name is the idempotency key. The result is always a valid DNS-1123
// label of at most MaxNameLen characters. If the plain form is not -- too
// long, or an input carries characters a label cannot (upper case,
// underscores) -- the name becomes a sanitized, truncated prefix plus "-" and
// the first 8 hex characters of sha256 of the PLAIN form. Hashing the plain
// form, not the sanitized one, keeps two inputs that sanitize alike (agent
// "a_b" and "a-b") from colliding onto one session.
func Name(agent string, projID int64, kind Kind, iid int64, attempt int) string {
	var plain string
	if iid == 0 {
		plain = fmt.Sprintf("gonk-%s-p%d-a%d", agent, projID, attempt)
	} else {
		plain = fmt.Sprintf("gonk-%s-p%d-%s%d-a%d", agent, projID, kind, iid, attempt)
	}
	if ValidName(plain) == nil {
		return plain
	}
	sum := sha256.Sum256([]byte(plain))
	suffix := hex.EncodeToString(sum[:])[:hashLen]

	prefix := sanitize(plain)
	if limit := MaxNameLen - 1 - hashLen; len(prefix) > limit {
		prefix = prefix[:limit]
	}
	// Cosmetic only: a doubled hyphen is legal, but "x--1a2b3c4d" reads as a
	// bug. The prefix cannot trim to empty -- it always starts "gonk".
	prefix = strings.TrimRight(prefix, "-")
	return prefix + "-" + suffix
}

// sanitize lowercases s and replaces every character a DNS-1123 label cannot
// hold with '-'.
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
