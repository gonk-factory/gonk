package exportguard

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// specificRule is one line of the deny data.
type specificRule struct {
	class Class
	name  string
	scope string // "" = everywhere; otherwise a path prefix ending in "/"
	re    *regexp.Regexp
}

var reRuleName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// parseDeny (the DENY DATA format) reads the specific-rule data. One rule per line, four fields
// separated by blanks; the fourth runs to the end of the line:
//
//	CLASS  NAME  SCOPE  REGEX
//
// CLASS is I1..I5. NAME identifies the rule in findings and must be unique.
// SCOPE is "*" (content, paths and messages, everywhere) or a path prefix
// ending in "/" (the content and paths of files under it only). REGEX is Go
// regexp syntax, matched against bytes. Lines beginning with "#" and blank
// lines are ignored.
//
// Every class I1..I5 must have at least one rule: a class with none is an
// error, so that removing a class is a visible act, not an edit that quietly
// narrows what is checked.
func parseDeny(data []byte) ([]specificRule, error) {
	var rules []specificRule
	names := map[string]bool{}
	have := map[Class]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil, fmt.Errorf("deny data line %d: want CLASS NAME SCOPE REGEX", n+1)
		}
		class, name, scope := Class(f[0]), f[1], f[2]
		// The regex is everything after the third field, verbatim.
		rest := line
		for i := 0; i < 3; i++ {
			rest = strings.TrimLeft(rest, " \t")
			rest = rest[len(f[i]):]
		}
		expr := strings.TrimLeft(rest, " \t")

		if !isSpecific(class) {
			return nil, fmt.Errorf("deny data line %d: class %q is not one of I1..I5", n+1, f[0])
		}
		if !reRuleName.MatchString(name) {
			return nil, fmt.Errorf("deny data line %d: rule name %q must be lower-case letters, digits and dashes", n+1, name)
		}
		if names[name] {
			return nil, fmt.Errorf("deny data line %d: rule name %q is used twice", n+1, name)
		}
		names[name] = true
		switch {
		case scope == "*":
			scope = ""
		case strings.HasSuffix(scope, "/") && !strings.HasPrefix(scope, "/") && !strings.ContainsAny(scope, "*?[\\"):
		default:
			return nil, fmt.Errorf("deny data line %d: scope %q must be * or a path prefix ending in /", n+1, scope)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("deny data line %d: %v", n+1, err)
		}
		if re.Match(nil) {
			return nil, fmt.Errorf("deny data line %d: rule %q matches the empty string", n+1, name)
		}
		rules = append(rules, specificRule{class: class, name: name, scope: scope, re: re})
		have[class] = true
	}
	for _, c := range SpecificClasses {
		if !have[c] {
			return nil, fmt.Errorf("deny data: class %s has no rule", c)
		}
	}
	return rules, nil
}

func isSpecific(c Class) bool {
	for _, s := range SpecificClasses {
		if c == s {
			return true
		}
	}
	return false
}

// allowEntry is one exemption. used records that it excused something.
type allowEntry struct {
	kind    string // path, anywhere, domain, upstream
	path    string
	literal string
	reason  string
	line    int
	used    bool
}

type allowList struct {
	entries []*allowEntry
}

// parseAllow (the ALLOW-LIST format) reads the allow-list. One entry per line, fields separated by a
// single TAB (literals may contain spaces). Four kinds:
//
//	path      PATH     LITERAL  REASON   LITERAL is excused in the content of PATH only
//	anywhere  LITERAL  REASON            LITERAL is excused in every file, path and message
//	domain    DOMAIN   REASON            e-mail addresses at DOMAIN or below are not findings
//	upstream  PREFIX/  REASON            the GENERIC rules are not applied under PREFIX/
//	scanner-config  PATH  REASON         the secret scanner's configuration file PATH may be in the tree
//
// scanner-config is not a deny-rule exemption: it is the permission for the
// exported tree to carry a file that changes what the SECRET SCANNER accepts
// (see ScannerConfigPath). Without the entry such a file is refused.
//
// An exemption is always an exact literal (or an exact domain, or an exact
// directory): there are no globs and no way to switch a rule off. A finding
// is excused when the matched text lies wholly inside an occurrence of the
// literal. Every entry needs a reason, and an entry that excuses nothing is
// reported by Guard.Stale.
func parseAllow(data []byte) (*allowList, error) {
	a := &allowList{}
	seen := map[string]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		e := &allowEntry{kind: f[0], line: n + 1}
		want := 3
		if e.kind == "path" {
			want = 4
		}
		if len(f) != want {
			return nil, fmt.Errorf("allow-list line %d: kind %q wants %d TAB-separated fields, got %d", n+1, e.kind, want, len(f))
		}
		switch e.kind {
		case "path":
			e.path, e.literal, e.reason = f[1], f[2], f[3]
			if e.path == "" || strings.HasPrefix(e.path, "/") || strings.HasSuffix(e.path, "/") || strings.ContainsAny(e.path, "*?[\\") {
				return nil, fmt.Errorf("allow-list line %d: %q is not a literal file path", n+1, e.path)
			}
		case "anywhere":
			e.literal, e.reason = f[1], f[2]
		case "domain":
			e.literal, e.reason = strings.ToLower(f[1]), f[2]
			if strings.ContainsAny(e.literal, "*?[\\@/ ") || !strings.Contains(e.literal, ".") {
				return nil, fmt.Errorf("allow-list line %d: %q is not a domain name", n+1, f[1])
			}
		case "scanner-config":
			e.path, e.reason = f[1], f[2]
			if e.path != ScannerConfigPath {
				return nil, fmt.Errorf("allow-list line %d: the only scanner configuration file is %s", n+1, ScannerConfigPath)
			}
		case "upstream":
			e.path, e.reason = f[1], f[2]
			if !strings.HasSuffix(e.path, "/") || strings.HasPrefix(e.path, "/") || strings.ContainsAny(e.path, "*?[\\") {
				return nil, fmt.Errorf("allow-list line %d: upstream %q must be a directory prefix ending in /", n+1, e.path)
			}
		default:
			return nil, fmt.Errorf("allow-list line %d: unknown kind %q", n+1, e.kind)
		}
		if e.kind != "upstream" && e.kind != "scanner-config" && e.literal == "" {
			return nil, fmt.Errorf("allow-list line %d: empty literal", n+1)
		}
		if strings.TrimSpace(e.reason) == "" {
			return nil, fmt.Errorf("allow-list line %d: an entry needs a reason", n+1)
		}
		key := e.kind + "\t" + e.path + "\t" + e.literal
		if seen[key] {
			return nil, fmt.Errorf("allow-list line %d: duplicate entry", n+1)
		}
		seen[key] = true
		a.entries = append(a.entries, e)
	}
	return a, nil
}

// excuse removes the matches that lie wholly inside an occurrence of an
// allowed literal. allowPath selects the path-scoped entries ("" = none).
func (a *allowList) excuse(ms []match, allowPath string, data []byte) []match {
	type span struct {
		e          *allowEntry
		start, end int
	}
	var spans []span
	for _, e := range a.entries {
		switch e.kind {
		case "anywhere":
		case "path":
			if allowPath == "" || e.path != allowPath {
				continue
			}
		default:
			continue
		}
		lit := []byte(e.literal)
		for off := 0; ; {
			i := bytes.Index(data[off:], lit)
			if i < 0 {
				break
			}
			spans = append(spans, span{e: e, start: off + i, end: off + i + len(lit)})
			off += i + 1
		}
	}
	if len(spans) == 0 {
		return ms
	}
	out := ms[:0:0]
	for _, m := range ms {
		excused := false
		for _, s := range spans {
			if s.start <= m.start && m.end <= s.end {
				s.e.used = true
				excused = true
			}
		}
		if !excused {
			out = append(out, m)
		}
	}
	return out
}

// domainAllowed reports whether an e-mail domain is an allowed domain or a
// subdomain of one.
func (a *allowList) domainAllowed(domain string) bool {
	ok := false
	for _, e := range a.entries {
		if e.kind != "domain" {
			continue
		}
		if domain == e.literal || strings.HasSuffix(domain, "."+e.literal) {
			e.used = true
			ok = true
		}
	}
	return ok
}

// underUpstream reports whether path is under an upstream prefix.
func (a *allowList) underUpstream(path string) bool {
	ok := false
	for _, e := range a.entries {
		if e.kind == "upstream" && strings.HasPrefix(path, e.path) {
			e.used = true
			ok = true
		}
	}
	return ok
}

// scannerConfigAllowed reports whether the tree may carry the scanner's
// configuration file at path.
func (a *allowList) scannerConfigAllowed(path string) bool {
	ok := false
	for _, e := range a.entries {
		if e.kind == "scanner-config" && e.path == path {
			e.used = true
			ok = true
		}
	}
	return ok
}

func (a *allowList) stale() []string {
	var out []string
	for _, e := range a.entries {
		if e.used {
			continue
		}
		// Name the entry by its path, or for a domain by the domain. The
		// literal of a path or anywhere entry is never printed: it is, by
		// construction, text a rule matches.
		what := e.kind
		switch {
		case e.path != "":
			what += " " + e.path
		case e.kind == "domain":
			what += " " + e.literal
		}
		out = append(out, fmt.Sprintf("allow-list line %d (%s) excuses nothing", e.line, what))
	}
	return out
}
