package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// SignatureVersion names the error-signature canonicalisation. It is part of
// every signature string ("es1-..."), so signatures from two versions never
// compare equal by accident. Any change to CanonicalError that can change a
// single output byte MUST bump it; TestErrorSignatureGolden pins the bytes.
const SignatureVersion = 1

// signaturePrefix is "es<SignatureVersion>-".
const signaturePrefix = "es1-"

// canonRule replaces one volatile shape with a stable token. Applied in order:
// specific shapes before general ones (a timestamp before the numbers in it, a
// hex id before its digits).
type canonRule struct {
	re   *regexp.Regexp
	repl string              // ReplaceAllString template, when fn is nil
	fn   func(string) string // per-match replacement, when set
}

var canonRules = []canonRule{
	// Paths under the runner's build directory: /builds/<namespace>/<project>/...
	// (and the shell executor's /builds/<token>/<n>/...). The whole path goes:
	// the project part is what differs between two projects, and how deep the
	// namespace nests cannot be told from the text.
	{re: regexp.MustCompile(`/builds/[^\s:'"(),;\]]+`), repl: "<builds>"},
	// Temporary paths: random by construction.
	{re: regexp.MustCompile(`/tmp/[^\s:'"(),;\]]+`), repl: "<tmp>"},
	// URL authority (userinfo dropped, host replaced; a port is a number).
	{re: regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)(?:[^/\s@]*@)?[^/\s:?#'"]+(?::\d+)?`), repl: "${1}<host>"},
	// UUIDs.
	{re: regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), repl: "<uuid>"},
	// Timestamps: ISO date-time, bare dates, times of day.
	{re: regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?\b`), repl: "<ts>"},
	{re: regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`), repl: "<ts>"},
	// IPv4 (an optional port is left to the number rule) and IPv6 (eight
	// groups, or a compressed form with at least one group before "::").
	{re: regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), repl: "<ip>"},
	{re: regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b|(?i)\b(?:[0-9a-f]{1,4}:){1,7}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?`), repl: "<ip>"},
	// Host names where the text says it is one: before ":<port>" with at least
	// two dots, or after lookup/host/resolve/connect (to). A dotted name
	// elsewhere is NOT assumed to be a host: "java.lang.IllegalStateException"
	// or "pkg.module.func" must survive, they say what failed.
	{re: regexp.MustCompile(`(^|[\s(@'"=])[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?){2,}:(\d{1,5})\b`), repl: "${1}<host>:${2}"},
	{re: regexp.MustCompile(`(?i)\b(lookup|host|resolve|resolving|connect to|connecting to)\s+[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?)+\b`), repl: "${1} <host>"},
	// Hex ids: 0x-prefixed, or a run of >= 7 hex digits (commit SHAs, digests,
	// container ids). The run must hold a digit, so words spelled only with
	// a-f ("defaced", "acceded") are left alone.
	{re: regexp.MustCompile(`\b0[xX][0-9a-fA-F]+\b`), repl: "<hex>"},
	{re: regexp.MustCompile(`\b[0-9a-fA-F]{7,}\b`), fn: hexWithDigit},
	// Durations: Go style (1m30.5s, 250ms) and spelled out (3 seconds).
	{re: regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|\x{00B5}s|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|us|\x{00B5}s|ms|s|m|h))*\b`), repl: "<dur>"},
	{re: regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:nanoseconds?|microseconds?|milliseconds?|seconds?|secs?|minutes?|mins?|hours?|hrs?)\b`), repl: "<dur>"},
}

// hexWithDigit replaces a hex run only when it holds a digit (RE2 has no
// lookahead to say so in the pattern).
func hexWithDigit(m string) string {
	if strings.ContainsAny(m, "0123456789") {
		return "<hex>"
	}
	return m
}

// exitCodeRe keeps a process exit status: "exit code 1" and "exit code 137"
// are different failures, and an exit status is not a volatile id.
var exitCodeRe = regexp.MustCompile(`(?i)\b(exit (?:code|status)) (\d+)\b`)

var (
	numberRe = regexp.MustCompile(`\d+`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// CanonicalError canonicalises one error line so the same failure in two
// projects, or two runs of one project, reads the same: build paths, temp
// paths, URL hosts, UUIDs, timestamps, IPs, host names, hex ids, durations and
// remaining numbers become fixed tokens (<builds>, <tmp>, <host>, <uuid>,
// <ts>, <ip>, <hex>, <dur>, <n>); an exit status is kept; whitespace runs
// collapse to one space. A trailing LineTruncatedSuffix is removed first.
func CanonicalError(line string) string {
	s := strings.TrimSuffix(line, LineTruncatedSuffix)
	for _, r := range canonRules {
		if r.fn != nil {
			s = r.re.ReplaceAllStringFunc(s, r.fn)
			continue
		}
		s = r.re.ReplaceAllString(s, r.repl)
	}
	// Numbers, except an exit status: protect it, replace, restore.
	const keep = "\x00exit\x00"
	var kept []string
	s = exitCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		kept = append(kept, m)
		return keep
	})
	s = numberRe.ReplaceAllString(s, "<n>")
	for _, k := range kept {
		s = strings.Replace(s, keep, k, 1)
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// ErrorSignature is a stable, versioned key for one job's failure, derived from
// its first error line (JobExcerpt.FirstError): "es1-" + the first 16 hex of
// sha256 over CanonicalError(line). Equal signatures across jobs, pipelines or
// projects mean "the same error text, volatile parts aside" -- a shared fact
// to correlate on, nothing more. "" for a line with no content.
func ErrorSignature(firstErrorLine string) string {
	c := CanonicalError(firstErrorLine)
	if c == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("pipelinectx-error-signature\x00" + c))
	return signaturePrefix + hex.EncodeToString(sum[:])[:16]
}
