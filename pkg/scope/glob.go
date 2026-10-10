package scope

import (
	"fmt"
	"path"
	"strings"
)

// MaxPatternLen bounds one path pattern; MaxPatterns bounds one list of them.
// Project patterns are untrusted repository content matched on every path of
// every change, so both are capped (and the JSON Schemas say the same).
const (
	MaxPatternLen = 256
	MaxPatterns   = 64
)

// ValidatePattern checks one path pattern. The syntax is deliberately small:
// a star matches any run of characters inside one path segment (never a
// slash); a question mark matches one character inside a segment; and a
// double star, as a WHOLE segment, matches zero or more segments -- except as
// the last segment, where it needs at least one (so `chart/**` is everything
// under chart/ and not a file named chart).
//
// No character classes and no escapes: `[`, `]` and `\` are refused, so a
// pattern means the same thing to a reader as to the matcher.
func ValidatePattern(p string) error {
	bad := func(why string) error { return fmt.Errorf("scope: pattern %q: %s", p, why) }
	switch {
	case p == "":
		return bad("empty")
	case len(p) > MaxPatternLen:
		return fmt.Errorf("scope: pattern longer than %d bytes", MaxPatternLen)
	case strings.ContainsAny(p, "[]\\\x00"):
		return bad("character classes, escapes and NUL are not supported")
	case path.IsAbs(p):
		return bad("absolute; patterns are relative to the repository root")
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			return bad("empty path segment")
		case seg == "." || seg == "..":
			return bad("`.` and `..` segments are not allowed")
		case seg != "**" && strings.Contains(seg, "**"):
			return bad("`**` must be a whole path segment")
		}
	}
	return nil
}

// Match reports whether pattern matches the clean relative path p. An invalid
// pattern matches nothing.
func Match(pattern, p string) bool {
	if ValidatePattern(pattern) != nil || !cleanRel(p) {
		return false
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return len(segs) >= 1
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		// ValidatePattern refused `[` and `\`, so path.Match cannot return
		// ErrBadPattern here and means exactly `*` and `?`.
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// matchAny returns the first pattern matching p, or "".
func matchAny(patterns []string, p string) string {
	for _, pat := range patterns {
		if Match(pat, p) {
			return pat
		}
	}
	return ""
}

// probes returns sample paths a pattern matches, built by filling its
// wildcards the two simplest ways (with nothing, and with one "x"). They are
// what Policy.Validate classifies to decide what a pattern is AIMED at.
func probes(pattern string) []string {
	var out []string
	seen := map[string]bool{}
	segs := strings.Split(pattern, "/")
	for _, starFill := range []string{"x", ""} {
		for _, deep := range []bool{true, false} {
			var b []string
			ok := true
			for i, seg := range segs {
				if seg == "**" {
					if deep || i == len(segs)-1 {
						b = append(b, "x")
					}
					continue
				}
				s := strings.ReplaceAll(strings.ReplaceAll(seg, "*", starFill), "?", "x")
				if s == "" {
					ok = false
					break
				}
				b = append(b, s)
			}
			if p := strings.Join(b, "/"); ok && p != "" && !seen[p] && Match(pattern, p) {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
