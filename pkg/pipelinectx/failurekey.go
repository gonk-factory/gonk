package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Per-job failure identity for the pipeline-fix exit check (gonk-2usw plan
// 3.2, 6.3). Pure functions over recorded facts; nothing here reads GitLab.
//
// WHY PER JOB. The run fingerprint (Fingerprint) reads only stage, name and
// failure_reason and skips allow_failure jobs, so it cannot tell "the same job
// now fails with a different error" from "the diagnosed failure recurs", and
// it goes quiet when a job is merely allowed to fail (plan F3). A fix is
// judged job by job on the key below.

// genericErrorLines is the CLOSED list of first-error lines that are the
// runner's own summary of a failed job: they say THAT the script exited
// non-zero, never why. Matched against CanonicalError(line), whole line.
//
// An exit status survives canonicalisation, so "exit code 1" and "exit code
// 137" have different signatures -- but neither names a cause, and two
// unrelated failures of one job share "exit code 1" as a matter of course.
//
// NOT on the list, deliberately: "ERROR: Job failed (system failure): ..."
// (it carries a cause) and "ERROR: Job failed: execution took longer than
// ..." (a timeout is a specific failure, and GitLab reports it in
// failure_reason as well). Adding a line here widens what the exit check
// treats as "cannot tell the failures apart"; it never changes a signature,
// so SignatureVersion is not involved.
var genericErrorLines = []*regexp.Regexp{
	// docker, shell, ssh, ... executors.
	regexp.MustCompile(`^ERROR: Job failed: exit (?:code|status) \d+$`),
	// kubernetes executor.
	regexp.MustCompile(`^ERROR: Job failed: command terminated with exit code \d+$`),
}

// GenericErrorLine reports whether a first-error line (JobExcerpt.FirstError)
// is the runner's own job-failed summary. A signature built from such a line
// identifies the exit status, not the failure, so the exit check may not read
// "same signature" as "same failure" nor "different signature" as "a
// different failure" (plan 6.3 J3, the conservative rule). False for an empty
// line: that case is GenericFirstError's.
func GenericErrorLine(line string) bool {
	c := CanonicalError(line)
	for _, re := range genericErrorLines {
		if re.MatchString(c) {
			return true
		}
	}
	return false
}

// GenericFirstError is the plan's "generic signature" (3.2): the line yields
// no signature at all, or it is the runner's summary. It is what dispatch
// records per failed job (beadstore.PipelineJob.ErrorGeneric) -- the flag
// cannot be recovered from the signature, which is a hash.
func GenericFirstError(line string) bool {
	return ErrorSignature(line) == "" || GenericErrorLine(line)
}

// JobFailureKey identifies ONE job's failure (plan 3.2): the job (stage and
// name), GitLab's failure_reason, and the ErrorSignature of its first error
// line ("" when it had none, or its log was not read). Comparable: two keys
// are the same failure of the same job iff they are ==.
//
// Name is whatever the caller uses as the job's name on BOTH sides of a
// comparison. gonk-gate records the canonical single-line form of the name
// (Record.PipelineJobs[].Name); a key built from a live GitLab job must put
// the name through the same canonicalisation first.
type JobFailureKey struct {
	Stage          string
	Name           string
	FailureReason  string
	ErrorSignature string
}

// String is the key's canonical one-line form, the bytes CauseKey hashes:
// the four fields, each Go-quoted in ASCII, joined by single spaces. Quoting
// makes it injective -- a separator inside a job name cannot move a field
// boundary -- and keeps it printable. Pinned by TestFailureKeyGolden.
func (k JobFailureKey) String() string {
	return strconv.QuoteToASCII(k.Stage) + " " + strconv.QuoteToASCII(k.Name) + " " +
		strconv.QuoteToASCII(k.FailureReason) + " " + strconv.QuoteToASCII(k.ErrorSignature)
}

// CauseKey identifies a diagnosed root cause (plan 3.2): the first 16 hex of
// sha256 over the SET of job failure keys -- their String forms, sorted, each
// terminated by "\n", under a domain prefix. Order and repetition of keys do
// not matter. "" for no keys: a run with no job to judge has no cause key
// (plan E5), and "" can never be mistaken for one.
//
// The caller's slice is not modified.
func CauseKey(keys []JobFailureKey) string {
	if len(keys) == 0 {
		return ""
	}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k.String())
	}
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString("pipelinectx-cause-key\x00")
	for i, l := range lines {
		if i > 0 && l == lines[i-1] {
			continue // a set: a repeated key counts once
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}
