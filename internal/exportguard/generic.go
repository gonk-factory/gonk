package exportguard

import (
	"net/netip"
	"regexp"
	"strings"
)

// The generic rules. Each has a stable id that findings and the mutation
// script name.
//
// NOTE FOR EDITORS: this file is published, and the guard scans it. A rule
// must therefore not match its own source text. The patterns below are
// written so that they do not (a character class where a literal space would
// complete the phrase, a required value after a key), and
// TestTheGuardDoesNotTripOnItsOwnSource holds that.

// genericMatches returns every generic-rule hit in data. domainAllowed
// reports whether an e-mail domain is on the caller's allow-list (the
// reserved example domains never reach it; they are handled here).
func genericMatches(data []byte, domainAllowed func(string) bool) []match {
	var ms []match
	ms = scanIPv4(data, ms)
	ms = scanIPv6(data, ms)
	ms = scanKeyMaterial(data, ms)
	ms = scanURLCredentials(data, ms)
	ms = scanEmail(data, ms, domainAllowed)
	ms = scanLocalPaths(data, ms)
	return ms
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isHex(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// scanIPv4 finds dotted quads and range-checks them (G1, G2).
//
// It reads maximal runs of digits joined by single dots and looks only at
// runs of EXACTLY four groups, each one to three digits and at most 255. That
// is the left and right boundary: a quad embedded in a longer dotted run (an
// OID, a five-part version) is not an address, and neither is one with an
// out-of-range group. A four-part version string is range-checked like an
// address, because from the bytes alone it is one.
func scanIPv4(data []byte, ms []match) []match {
	n := len(data)
	for i := 0; i < n; {
		if !isDigit(data[i]) {
			i++
			continue
		}
		start := i
		var groups [4]int
		count, valid := 0, true
		for {
			gs := i
			v := 0
			for i < n && isDigit(data[i]) {
				if i-gs < 4 {
					v = v*10 + int(data[i]-'0')
				}
				i++
			}
			if i-gs > 3 || v > 255 {
				valid = false
			}
			if count < 4 {
				groups[count] = v
			}
			count++
			if i+1 < n && data[i] == '.' && isDigit(data[i+1]) {
				i++
				continue
			}
			break
		}
		if count != 4 || !valid {
			continue
		}
		if c, rule, ok := classifyIPv4(groups[0], groups[1]); ok {
			ms = append(ms, match{class: c, rule: rule, start: start, end: i})
		}
	}
	return ms
}

// classifyIPv4 is the range check, on the first two groups (every range here
// is decided by them).
func classifyIPv4(a, b int) (Class, string, bool) {
	switch {
	case a == 10:
		return G1, "rfc1918-10", true
	case a == 172 && b >= 16 && b <= 31:
		return G1, "rfc1918-172", true
	case a == 192 && b == 168:
		return G1, "rfc1918-192", true
	case a == 100 && b >= 64 && b <= 127:
		return G2, "cgnat", true
	}
	return "", "", false
}

var (
	// Built from bytes, not parsed from text: the textual form of either
	// prefix is itself a match for this rule, and this file is published.
	prefixULA       = netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfc}), 7)
	prefixLinkLocal = netip.PrefixFrom(netip.AddrFrom16([16]byte{0xfe, 0x80}), 10)
)

// scanIPv6 finds unique-local and link-local IPv6 addresses (G3). A candidate
// is a maximal run of hex digits and colons with at least two colons; it is
// parsed as an address, so a clock time, a MAC address or a hex dump is not
// one.
func scanIPv6(data []byte, ms []match) []match {
	n := len(data)
	for i := 0; i < n; {
		if !isHex(data[i]) && data[i] != ':' {
			i++
			continue
		}
		start := i
		colons := 0
		for i < n && (isHex(data[i]) || data[i] == ':') {
			if data[i] == ':' {
				colons++
			}
			i++
		}
		if colons < 2 {
			continue
		}
		s, e := start, i
		// A single leading or trailing colon belongs to the surrounding text
		// ("addr:<address>", "<address>:"), not to the address.
		if e-s > 2 && data[s] == ':' && data[s+1] != ':' {
			s++
		}
		if e-s > 2 && data[e-1] == ':' && data[e-2] != ':' {
			e--
		}
		addr, err := netip.ParseAddr(string(data[s:e]))
		if err != nil || !addr.Is6() || addr.Is4In6() {
			continue
		}
		switch {
		case prefixULA.Contains(addr):
			ms = append(ms, match{class: G3, rule: "ipv6-ula", start: s, end: e})
		case prefixLinkLocal.Contains(addr):
			ms = append(ms, match{class: G3, rule: "ipv6-link-local", start: s, end: e})
		}
	}
	return ms
}

var (
	rePEMCertificate = regexp.MustCompile(`BEGIN[ ]CERTIFICATE`)
	rePEMPrivateKey  = regexp.MustCompile(`BEGIN[ ](?:[A-Z0-9]+[ ])*PRIVATE[ ]KEY`)
	// A kubeconfig field WITH a literal value. The bare key name (in prose,
	// or with a placeholder) is not material.
	reKubeconfigData = regexp.MustCompile(`(?:client-key-data|client-certificate-data|certificate-authority-data)["']?[ \t]*[:=][ \t]*["']?[A-Za-z0-9+/]{16,}`)
	// Sixteen or more colon-separated hex pairs: a certificate fingerprint
	// (SHA-1 is 20 pairs, SHA-256 is 32). A MAC address is 6.
	reFingerprint = regexp.MustCompile(`(?:[0-9A-Fa-f]{2}:){15,}[0-9A-Fa-f]{2}`)
)

func scanKeyMaterial(data []byte, ms []match) []match {
	ms = appendAll(ms, G4, "pem-certificate", rePEMCertificate, data)
	ms = appendAll(ms, G4, "pem-private-key", rePEMPrivateKey, data)
	ms = appendAll(ms, G4, "kubeconfig-data", reKubeconfigData, data)
	ms = appendAll(ms, G4, "cert-fingerprint", reFingerprint, data)
	return ms
}

func appendAll(ms []match, c Class, rule string, re *regexp.Regexp, data []byte) []match {
	for _, loc := range re.FindAllIndex(data, -1) {
		ms = append(ms, match{class: c, rule: rule, start: loc[0], end: loc[1]})
	}
	return ms
}

// A URL whose authority part carries a user name AND a password. The host is
// captured so that reserved hosts can be let through.
var reURLCredentials = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@"'<>]+:[^\s/@"'<>]+@(\[[0-9A-Fa-f:]+\]|[^\s/:@"'<>?#\[\]]+)`)

func scanURLCredentials(data []byte, ms []match) []match {
	for _, loc := range reURLCredentials.FindAllSubmatchIndex(data, -1) {
		host := string(data[loc[2]:loc[3]])
		if reservedHost(host) {
			continue
		}
		ms = append(ms, match{class: G5, rule: "url-credentials", start: loc[0], end: loc[1]})
	}
	return ms
}

// The domain must end in an alphabetic label, so "module@v1.2.3" is not an
// address.
var reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})`)

func scanEmail(data []byte, ms []match, domainAllowed func(string) bool) []match {
	for _, loc := range reEmail.FindAllSubmatchIndex(data, -1) {
		domain := strings.ToLower(string(data[loc[2]:loc[3]]))
		if reservedName(domain) {
			continue
		}
		if domainAllowed != nil && domainAllowed(domain) {
			continue
		}
		ms = append(ms, match{class: G6, rule: "email", start: loc[0], end: loc[1]})
	}
	return ms
}

var (
	reHomePath     = regexp.MustCompile(`/home/[A-Za-z0-9._-]+/`)
	reMntUsersPath = regexp.MustCompile(`/mnt/[A-Za-z]/Users/[A-Za-z0-9._-]+/?`)
	reUsersPath    = regexp.MustCompile(`/Users/[A-Za-z0-9._-]+/`)
	reRootWorktree = regexp.MustCompile(`/root/[A-Za-z0-9._-]+-wt/`)
)

func scanLocalPaths(data []byte, ms []match) []match {
	ms = appendAll(ms, G7, "home-path", reHomePath, data)
	ms = appendAll(ms, G7, "mnt-users-path", reMntUsersPath, data)
	ms = appendAll(ms, G7, "users-path", reUsersPath, data)
	ms = appendAll(ms, G7, "root-worktree-path", reRootWorktree, data)
	return ms
}

// reservedZones are names nobody can register or resolve by accident: the
// RFC 2606 / RFC 6761 example domains and the project's reserved example
// zone. A name equal to or under one of them is not internal.
var reservedZones = []string{
	"example.com", "example.org", "example.net", "example.internal",
	"example", "test", "invalid", "localhost",
}

// reservedName reports whether a DNS name is under a reserved zone.
func reservedName(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, z := range reservedZones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

// reservedBlocks are the address blocks that identify nothing: the three
// RFC 5737 documentation blocks, the IPv6 documentation block, and loopback.
var reservedBlocks = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("::1/128"),
}

// reservedHost reports whether the host part of a URL is a reserved name or
// a reserved address.
func reservedHost(host string) bool {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if addr, err := netip.ParseAddr(h); err == nil {
		for _, p := range reservedBlocks {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	return reservedName(h)
}
