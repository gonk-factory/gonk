package pipelinectx

import (
	"regexp"
	"sort"
	"strings"
)

// Redaction kinds, rendered as "[REDACTED:<kind>]".
const (
	KindGitLabToken    = "gitlab_token"
	KindPrivateKey     = "private_key"
	KindAWSAccessKey   = "aws_access_key"
	KindAPIKey         = "api_key"
	KindJWT            = "jwt"
	KindURLUserinfo    = "url_userinfo"
	KindCredentialPair = "credential"
)

// RedactedPrefix starts every placeholder Redact writes.
const RedactedPrefix = "[REDACTED:"

func placeholder(kind string) string { return RedactedPrefix + kind + "]" }

// redactionKinds is every kind Redact writes.
var redactionKinds = map[string]bool{
	KindGitLabToken: true, KindPrivateKey: true, KindAWSAccessKey: true, KindAPIKey: true,
	KindJWT: true, KindURLUserinfo: true, KindCredentialPair: true,
}

// alreadyRedacted reports whether a matched secret is EXACTLY a value that is
// not a secret: one of Redact's own placeholders, or GitLab's "[MASKED]",
// optionally quoted. Exactly: a value that merely CONTAINS "[REDACTED:" is a
// secret like any other (verify report R7) -- "[REDACTED:hunter2" used to be
// waved through, so a secret pre-wrapped to look like a placeholder passed
// the would_post_secret gate.
func alreadyRedacted(secret string) bool {
	v := strings.Trim(secret, `"'`)
	if v == "[MASKED]" {
		return true
	}
	kind, ok := strings.CutPrefix(v, RedactedPrefix)
	if !ok {
		return false
	}
	kind, ok = strings.CutSuffix(kind, "]")
	return ok && redactionKinds[kind]
}

// redactRule is one secret shape. secretGroup is the submatch index that is the
// secret itself (0 = the whole match); everything outside that group is kept,
// so "password=hunter2" becomes "password=[REDACTED:credential]".
type redactRule struct {
	kind        string
	re          *regexp.Regexp
	secretGroup int
	// hint is a cheap necessary condition: a line for which it is false
	// cannot match re. It exists for speed only (an 8 MiB trace), never
	// changes a result, and TestRedactHintsAreNecessary checks it.
	hint func(line string) bool
}

func containsAny(subs ...string) func(string) bool {
	return func(s string) bool {
		for _, sub := range subs {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
}

func credentialHint(s string) bool {
	if !strings.ContainsAny(s, ":=") {
		return false
	}
	l := strings.ToLower(s)
	return containsAny("authorization", "private", "x-api-key", "api", "passw", "secret", "token")(l)
}

// pemRule runs first, over the whole text: a key block spans lines.
var pemRule = redactRule{
	// PEM private keys, whole block. An unterminated BEGIN (the trace ended,
	// or was cut, inside the key) redacts to the end of the text: fail closed.
	KindPrivateKey, regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----|[\s\S]*$)`), 0,
	containsAny("PRIVATE KEY-----"),
}

// redactRules run line by line after pemRule (none of them may span a line),
// IN THIS ORDER: specific shapes first, so a GitLab token after
// "PRIVATE-TOKEN:" is reported as gitlab_token, not credential. Plan D7 lists
// the shapes; each widening below is noted where it is made.
var redactRules = []redactRule{
	// GitLab tokens. Plan names glpat-; the other documented GitLab token
	// prefixes (deploy, runner, CI job, OAuth, feed, ...) are the same risk,
	// plus the legacy runner registration token GR1348941.
	{KindGitLabToken, regexp.MustCompile(`\b(?:glpat|gldt|glrt|glrtr|glptt|glcbt|glsoat|glffct|gloas|glimt|glagent|glft|glwt|glsa)-[0-9A-Za-z_\-]{20,}|\bGR1348941[0-9A-Za-z_\-]{20,}`), 0, containsAny("gl", "GR1348941")},
	// AWS access key ids: AKIA (long-term, the plan's shape) and ASIA (STS).
	{KindAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0, containsAny("AKIA", "ASIA")},
	// sk- API keys. The plan's [A-Za-z0-9]{20,} misses keys with internal
	// dashes/underscores (sk-proj-..., sk-ant-api03-...): the run after "sk-"
	// stops at the first dash. Widened to include - and _.
	{KindAPIKey, regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`), 0, containsAny("sk-")},
	// JWT-shaped: header.payload.signature, base64url, header starting eyJ.
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), 0, containsAny("eyJ")},
	// URL userinfo with a password: scheme://user:pass@ -> the user:pass part.
	{KindURLUserinfo, regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://([^/\s:@]+:[^/\s@]+)@`), 1, containsAny("://")},
	// The value after a credential-looking key. Widened from the plan's
	// `\b(key)\s*[:=]` in three ways, each a real leak shape:
	//   - an identifier prefix (CI_JOB_TOKEN=, DB_PASSWORD=): `_` is a word
	//     character, so `\btoken` never matches inside GITLAB_TOKEN;
	//   - a closing quote before the separator ("password": "x" in JSON);
	//   - an auth scheme word (Authorization: Bearer <tok>): without it only
	//     "Bearer" would be redacted and the token itself would survive.
	// api_key / private_token spellings are added alongside x-api-key and
	// private-token.
	{KindCredentialPair, regexp.MustCompile(`(?i)[A-Za-z0-9_\-]*(?:authorization|private[-_]token|x-api-key|api[-_]key|password|passwd|secret|token)["']?[ \t]*[:=][ \t]*(?:(?:bearer|basic|token)[ \t]+)?("[^"\n]*"|'[^'\n]*'|[^\s"']+)`), 1, credentialHint},
}

// Redact replaces every secret-shaped substring of s with
// "[REDACTED:<kind>]" and returns the result plus the sorted, de-duplicated
// kinds it replaced (nil when it found nothing).
//
// It is idempotent: Redact(Redact(s)) finds nothing. That matters because the
// rendered comment is gated on Redact finding NOTHING (plan D8 step 8), and
// that comment quotes excerpt lines that were already redacted. A value that is
// EXACTLY a placeholder (or GitLab's own "[MASKED]") is left alone; one that
// only contains the placeholder prefix is redacted (alreadyRedacted).
func Redact(s string) (string, []string) {
	found := map[string]bool{}
	apply := func(rule redactRule, text string) string {
		if !rule.hint(text) {
			return text
		}
		return replaceGroup(rule.re, text, rule.secretGroup, func(secret string) (string, bool) {
			if alreadyRedacted(secret) {
				return "", false
			}
			found[rule.kind] = true
			return placeholder(rule.kind), true
		})
	}
	s = apply(pemRule, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		for _, rule := range redactRules {
			l = apply(rule, l)
		}
		lines[i] = l
	}
	s = strings.Join(lines, "\n")
	if len(found) == 0 {
		return s, nil
	}
	kinds := make([]string, 0, len(found))
	for k := range found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return s, kinds
}

// replaceGroup replaces submatch `group` of every match of re in s with
// repl(secret), when repl says to.
func replaceGroup(re *regexp.Regexp, s string, group int, repl func(string) (string, bool)) string {
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var sb strings.Builder
	last := 0
	for _, m := range idx {
		start, end := m[2*group], m[2*group+1]
		if start < 0 {
			continue
		}
		r, ok := repl(s[start:end])
		if !ok {
			continue
		}
		sb.WriteString(s[last:start])
		sb.WriteString(r)
		last = end
	}
	sb.WriteString(s[last:])
	return sb.String()
}
