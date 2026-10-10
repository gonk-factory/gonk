package exportguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every positive in this file is ASSEMBLED AT RUN TIME. This file is
// published and the guard scans it: a private address written out here would
// be refused by the very rule it tests. cat, ip4, ip6 and at exist so that no
// positive ever appears whole in the source.

func cat(parts ...string) string { return strings.Join(parts, "") }

func ip4(a, b, c, d int) string { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }

func ip6(groups ...string) string { return strings.Join(groups, ":") }

func at(local, domain string) string { return local + "@" + domain }

// hexPairs returns n colon-separated hex pairs.
func hexPairs(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("%02X", (i*37+11)%256)
	}
	return strings.Join(p, ":")
}

// testDeny is SYNTHETIC specific-rule data: one or more rules per class, with
// made-up names. The real data is internal-only and is tested where it lives.
const testDeny = `# synthetic
I1 zone        *      (?i)zork\.lan
I2 cluster     *      (?i)\bzork(0[0-9])?\b
I2 node        *      (?i)\bgrue\b
I3 ca-name     *      zork-ca
I3 ca-default  chart/ (?m):[ \t]*["']?lamp-bundle["']?[ \t]*$
I4 gitops      *      clusters/zork
I5 context     *      admin@zork
`

const testAllow = "domain\tusers.noreply.github.com\tupstream contact addresses\n"

func newGuard(t *testing.T, deny, allow string) *Guard {
	t.Helper()
	var d, a []byte
	if deny != "" {
		d = []byte(deny)
	}
	if allow != "" {
		a = []byte(allow)
	}
	g, err := New(d, a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

type positive struct {
	class Class
	rule  string
	text  string
}

// positives is one instance per RULE (not per class): every rule id the
// guard can report appears here, so deleting any one rule turns a row red.
func positives() []positive {
	begin := cat("BEG", "IN ")
	return []positive{
		{G1, "rfc1918-10", ip4(10, 1, 2, 3)},
		{G1, "rfc1918-10", ip4(10, 0, 0, 0)},
		{G1, "rfc1918-10", ip4(10, 255, 255, 255)},
		{G1, "rfc1918-172", ip4(172, 16, 0, 1)},
		{G1, "rfc1918-172", ip4(172, 31, 255, 254)},
		{G1, "rfc1918-192", ip4(192, 168, 1, 142)},
		{G2, "cgnat", ip4(100, 64, 0, 1)},
		{G2, "cgnat", ip4(100, 127, 255, 254)},
		{G3, "ipv6-ula", ip6("fc00", "", "1")},
		{G3, "ipv6-ula", ip6("fdff", "ab", "", "9")},
		{G3, "ipv6-ula", ip6("FD12", "3456", "789a", "1", "", "1")},
		{G3, "ipv6-link-local", ip6("fe80", "", "1")},
		{G3, "ipv6-link-local", ip6("febf", "", "2")},
		{G4, "pem-certificate", cat(begin, "CERTIFICATE")},
		{G4, "pem-private-key", cat(begin, "PRIVATE KEY")},
		{G4, "pem-private-key", cat(begin, "RSA PRIVATE KEY")},
		{G4, "pem-private-key", cat(begin, "OPENSSH PRIVATE KEY")},
		{G4, "kubeconfig-data", cat("client-key", "-data: ", "TFMwdExTMUNSVWRKVGlCRAAA")},
		{G4, "kubeconfig-data", cat("client-certificate", "-data: ", "TFMwdExTMUNSVWRKVGlCRAAA")},
		{G4, "kubeconfig-data", cat("certificate-authority", "-data: \"", "TFMwdExTMUNSVWRKVGlCRAAA")},
		{G4, "cert-fingerprint", hexPairs(16)},
		{G4, "cert-fingerprint", hexPairs(32)},
		{G5, "url-credentials", cat("https://bot", ":hunter2", "@", "git.corp.zz")},
		{G5, "url-credentials", cat("postgres://u", ":p", "@", ip4(203, 0, 114, 5))},
		{G6, "email", at("someone", "corp.zz")},
		{G6, "email", at("first.last+tag", "mail.corp.zz")},
		{G7, "home-path", cat("/ho", "me/", "alice/")},
		{G7, "mnt-users-path", cat("/mn", "t/c/", "Users/", "alice")},
		{G7, "users-path", cat("/Us", "ers/", "alice/")},
		{G7, "root-worktree-path", cat("/ro", "ot/", "proj-wt/")},
		{I1, "zone", cat("zork", ".lan")},
		{I2, "cluster", "zork"},
		{I2, "cluster", "ZORK03"},
		{I2, "node", "grue"},
		{I3, "ca-name", cat("zork", "-ca")},
		{I4, "gitops", cat("clusters/", "zork")},
		{I5, "context", cat("admin", "@", "zork")},
	}
}

func TestEveryRuleIsSeenRedInContentPathAndMessage(t *testing.T) {
	covered := map[Class]bool{}
	for _, p := range positives() {
		p := p
		covered[p.class] = true
		t.Run(string(p.class)+"/"+p.rule+"/"+fmt.Sprint(len(p.text)), func(t *testing.T) {
			prefix := "first line\nsecond: "
			check := func(kind Kind, name string, fs []Finding, wantLine, wantCol, wantOff int) {
				t.Helper()
				if len(fs) != 1 {
					t.Fatalf("%s: want exactly 1 finding, got %d: %v", kind, len(fs), fs)
				}
				f := fs[0]
				if f.Class != p.class || f.Rule != p.rule || f.Kind != kind || f.Path != name {
					t.Fatalf("%s: got %v, want class %s rule %s at %s", kind, f, p.class, p.rule, name)
				}
				if f.Line != wantLine || f.Col != wantCol || f.Offset != wantOff || f.Length != len(p.text) {
					t.Fatalf("%s: position line %d col %d offset %d length %d, want %d %d %d %d",
						kind, f.Line, f.Col, f.Offset, f.Length, wantLine, wantCol, wantOff, len(p.text))
				}
			}

			g := newGuard(t, testDeny, testAllow)
			content := prefix + p.text + " tail\n"
			check(KindContent, "docs/a.md", g.Content("docs/a.md", []byte(content)), 2, len("second: ")+1, len(prefix))

			path := "docs/x " + p.text + " y/file.txt"
			check(KindPath, path, g.Path(path), 1, len("docs/x ")+1, len("docs/x "))

			check(KindMessage, "commit-message", g.Message("commit-message", []byte(content), ""), 2, len("second: ")+1, len(prefix))
		})
	}
	for _, c := range AllClasses() {
		if !covered[c] {
			t.Errorf("class %s has no positive", c)
		}
	}
	if len(AllClasses()) != 12 {
		t.Fatalf("the plan counts 12 classes, the guard has %d", len(AllClasses()))
	}
}

// nearMisses are values that sit next to a rule and must stay green: the
// reserved documentation values, the first address outside each range, and
// text that looks like a match from a distance.
func nearMisses() []string {
	begin := cat("BEG", "IN ")
	return []string{
		// reserved documentation addresses
		ip4(192, 0, 2, 1), ip4(198, 51, 100, 0) + "/24", ip4(203, 0, 113, 9),
		// one outside each IPv4 range, both sides
		ip4(9, 255, 255, 255), ip4(11, 0, 0, 1),
		ip4(172, 15, 255, 255), ip4(172, 32, 0, 1),
		ip4(192, 167, 1, 1), ip4(192, 169, 1, 1), ip4(191, 168, 1, 1), ip4(193, 168, 1, 1),
		ip4(100, 63, 255, 255), ip4(100, 128, 0, 1), ip4(99, 64, 0, 1), ip4(101, 64, 0, 1),
		ip4(171, 16, 0, 1), ip4(173, 16, 0, 1),
		// not addresses at all
		ip4(1, 10, 0, 5),         // a version string
		ip4(10, 89, 2, 300),      // a group out of range
		ip4(10, 1, 2, 3) + ".4",  // five groups
		ip4(10, 1, 2, 3)[:6],     // three groups
		ip4(10, 1, 2, 1234),      // a four-digit group
		ip4(10, 1, 2, 0) + "001", // a four-digit group that is numerically small
		// IPv6: documentation block, and one outside each range
		ip6("2001", "db8", "", "1"),
		ip6("fbff", "", "1"), ip6("fe7f", "", "1"), ip6("fec0", "", "1"), ip6("fe00", "", "1"),
		"12:30:45", "aa:bb:cc:dd:ee:ff", "fd00", "::",
		// key material
		cat(begin, "PUBLIC KEY"), cat(begin, "CERTIFICAT"), cat("BEGIN", "CERTIFICATE"),
		cat("client-key", "-data: <base64>"), cat("certificate-authority", "-data"),
		cat("client-key", "-data: ", "c2hvcnQ="), // a value shorter than a key
		hexPairs(15),
		strings.Repeat("ab", 32), // a 64-hex digest
		// credentials in URLs to reserved hosts, and URLs without credentials
		cat("https://bot", ":hunter2", "@", "gitlab.example.internal/x"),
		cat("https://bot", ":hunter2", "@", ip4(192, 0, 2, 5), "/x"),
		cat("https://bot", ":hunter2", "@", "localhost:8080/x"),
		cat("https://bot", ":hunter2", "@", ip4(198, 51, 100, 5), "/x"),
		cat("https://bot", ":hunter2", "@", ip4(203, 0, 113, 5), ":8443/x"),
		cat("https://bot", ":hunter2", "@", ip4(127, 0, 0, 1), ":5432/db"),
		cat("https://bot", ":hunter2", "@", "[2001:db8::7]:443/x"),
		cat("https://bot", ":hunter2", "@", "[::1]:443/x"),
		cat("https://bot", ":hunter2", "@", "db.example.com/x"),
		cat("https://bot", ":hunter2", "@", "db.example.net/x"),
		cat("https://bot", ":hunter2", "@", "db.example.org/x"),
		cat("https://bot", ":hunter2", "@", "db.test/x"),
		cat("https://bot", ":hunter2", "@", "db.invalid/x"),
		cat("https://bot", ":hunter2", "@", "db.example/x"),
		"https://host.zz/a:b@c", "https://host.zz:8443/path",
		// e-mail at reserved and allowed domains; not e-mail
		at("someone", "example.com"), at("a", "sub.example.org"), at("b", "host.test"),
		at("c", "x.invalid"), at("x", "example.net"), at("d", "gitlab.example.internal"), at("e", "thing.example"),
		at("1234+bot", "users.noreply.github.com"),
		at("module", "v1.2.3"), at("actions/checkout", "v4"), "@decorator.name",
		// local paths with a placeholder where a user name would be
		"/home/<user>/x", "$HOME/x", "/Users/<user>/x", "/root/work/x", "/mnt/c/Userz/x",
		"/home/", "/rooted/a-wt/x", "/root/-wt",
		// names next to the synthetic specific rules
		"zorkmid", "azork", "grues", "gitlab.example.internal", "zork_ca", "clusters/example",
		"zork1", "zork10",
	}
}

func TestNearMissesStayGreen(t *testing.T) {
	for _, s := range nearMisses() {
		g := newGuard(t, testDeny, testAllow)
		for _, text := range []string{s, "see " + s + " for details\n", "x=" + s} {
			if fs := g.Content("docs/a.md", []byte(text)); len(fs) != 0 {
				t.Errorf("near miss #%d was refused in content: %v", indexOf(nearMisses(), s), fs)
			}
		}
		if fs := g.Message("tag-message", []byte(s), ""); len(fs) != 0 {
			t.Errorf("near miss #%d was refused in a message: %v", indexOf(nearMisses(), s), fs)
		}
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestIPv6CandidateIsTrimmedOfSurroundingColons(t *testing.T) {
	g := newGuard(t, "", "")
	addr := ip6("fe80", "", "1")
	for _, text := range []string{"addr:" + addr, addr + ": link", "[" + addr + "]:443", addr + "/64", addr + "%eth0"} {
		fs := g.Content("a", []byte(text))
		if len(fs) != 1 || fs[0].Class != G3 || fs[0].Length != len(addr) {
			t.Errorf("want one G3 finding of length %d in a wrapped address, got %v", len(addr), fs)
		}
	}
}

func TestBinaryBlobsAreScanned(t *testing.T) {
	g := newGuard(t, testDeny, "")
	blob := append([]byte{0x00, 0x01, 0xff, 0xfe, 0x00}, []byte(ip4(10, 9, 8, 7))...)
	blob = append(blob, 0x00, 0x00)
	fs := g.Content("bin/blob.dat", blob)
	if len(fs) != 1 || fs[0].Class != G1 || fs[0].Offset != 5 {
		t.Fatalf("want one G1 finding at offset 5 in a NUL-containing blob, got %v", fs)
	}
}

func TestAFindingNeverCarriesTheMatchedText(t *testing.T) {
	for _, p := range positives() {
		g := newGuard(t, testDeny, "")
		fs := g.Content("docs/a.md", []byte("x "+p.text+" y"))
		if len(fs) == 0 {
			t.Fatalf("%s: no finding", p.rule)
		}
		js, err := json.Marshal(fs)
		if err != nil {
			t.Fatal(err)
		}
		for _, form := range []string{string(js), fs[0].String(), fmt.Sprintf("%+v", fs[0])} {
			if strings.Contains(form, p.text) {
				t.Fatalf("%s: the finding repeats the matched text", p.rule)
			}
		}
	}
}

func TestALongerMatchSuppressesTheOneInsideIt(t *testing.T) {
	g := newGuard(t, testDeny, "")
	// A host under the zone also contains the bare cluster name; it is
	// reported once, as the zone.
	fs := g.Content("a", []byte(cat("gitlab.", "zork", ".lan")))
	if len(fs) != 1 || fs[0].Class != I1 {
		t.Fatalf("want exactly one I1 finding, got %v", fs)
	}
	// Two separate matches are both reported.
	fs = g.Content("a", []byte(cat("zork", " and ", "zork", ".lan")))
	if len(fs) != 2 || fs[0].Class != I2 || fs[1].Class != I1 {
		t.Fatalf("want I2 then I1, got %v", fs)
	}
	// A mount path contains a users path; reported once, as the longer rule.
	fs = g.Content("a", []byte(cat("/mn", "t/c/", "Users/", "alice", "/x")))
	if len(fs) != 1 || fs[0].Rule != "mnt-users-path" {
		t.Fatalf("want one mnt-users-path finding, got %v", fs)
	}
	// A URL with credentials contains an address-shaped e-mail; one finding.
	fs = g.Content("a", []byte(cat("https://bot", ":hunter2", "@", "git.corp.zz/")))
	if len(fs) != 1 || fs[0].Class != G5 {
		t.Fatalf("want one G5 finding, got %v", fs)
	}
}

func TestScopedRuleAppliesOnlyUnderItsScope(t *testing.T) {
	line := cat("  existingConfigMap: ", "lamp", "-bundle\n")
	g := newGuard(t, testDeny, "")
	fs := g.Content("chart/gonk/values.yaml", []byte(line))
	if len(fs) != 1 || fs[0].Class != I3 || fs[0].Rule != "ca-default" {
		t.Fatalf("want one I3 ca-default finding under chart/, got %v", fs)
	}
	if fs := g.Content("docs/guide.md", []byte(line)); len(fs) != 0 {
		t.Fatalf("a scoped rule fired outside its scope: %v", fs)
	}
	if fs := g.Message("commit-message", []byte(line), ""); len(fs) != 0 {
		t.Fatalf("a scoped rule fired on a message: %v", fs)
	}
	// Prose under the scope, not a value, stays green.
	if fs := g.Content("chart/gonk/README.md", []byte(cat("the ", "lamp", "-bundle ConfigMap is conventional\n"))); len(fs) != 0 {
		t.Fatalf("prose was refused: %v", fs)
	}
}

func TestPublicModeAppliesGenericRulesOnly(t *testing.T) {
	g := newGuard(t, "", "")
	if fs := g.Content("a", []byte(cat("gitlab.", "zork", ".lan"))); len(fs) != 0 {
		t.Fatalf("a guard without deny data applied a specific rule: %v", fs)
	}
	if fs := g.Content("a", []byte(ip4(10, 1, 2, 3))); len(fs) != 1 {
		t.Fatalf("a guard without deny data must still apply the generic rules, got %v", fs)
	}
}

func TestAllowListIsExact(t *testing.T) {
	addr := at("maintainer", "corp.zz")
	cidr := ip4(192, 168, 0, 0) + "/16"
	allow := "path\tchart/Chart.yaml\t" + addr + "\tmaintainer field\n" +
		"path\tci/netpol.yml\t" + cidr + "\tdocumented default pod CIDR\n"

	t.Run("the literal in its file passes and the entry is used", func(t *testing.T) {
		g := newGuard(t, testDeny, allow)
		if fs := g.Content("chart/Chart.yaml", []byte("email: "+addr+"\n")); len(fs) != 0 {
			t.Fatalf("allowed literal refused: %v", fs)
		}
		if fs := g.Content("ci/netpol.yml", []byte("cidr: "+cidr+"\n")); len(fs) != 0 {
			t.Fatalf("allowed literal refused: %v", fs)
		}
		if st := g.Stale(); len(st) != 0 {
			t.Fatalf("used entries reported stale: %v", st)
		}
	})
	t.Run("the right literal in the wrong file is red", func(t *testing.T) {
		g := newGuard(t, testDeny, allow)
		fs := g.Content("docs/other.md", []byte("email: "+addr+"\n"))
		if len(fs) != 1 || fs[0].Class != G6 {
			t.Fatalf("want one G6 finding, got %v", fs)
		}
	})
	t.Run("a different literal in the right file is red", func(t *testing.T) {
		g := newGuard(t, testDeny, allow)
		fs := g.Content("chart/Chart.yaml", []byte("email: "+at("other", "corp.zz")+"\n"))
		if len(fs) != 1 || fs[0].Class != G6 {
			t.Fatalf("want one G6 finding, got %v", fs)
		}
		// The allowed network does not excuse a host inside it written
		// differently, nor a neighbouring one.
		fs = g.Content("ci/netpol.yml", []byte("cidr: "+ip4(192, 168, 0, 1)+"/16\n"))
		if len(fs) != 1 || fs[0].Class != G1 {
			t.Fatalf("want one G1 finding, got %v", fs)
		}
	})
	t.Run("a second, unexcused match in the same file is still red", func(t *testing.T) {
		g := newGuard(t, testDeny, allow)
		fs := g.Content("chart/Chart.yaml", []byte(addr+"\n"+ip4(10, 0, 0, 9)+"\n"))
		if len(fs) != 1 || fs[0].Class != G1 || fs[0].Line != 2 {
			t.Fatalf("want one G1 finding on line 2, got %v", fs)
		}
	})
	t.Run("a path entry does not excuse the path itself or a message", func(t *testing.T) {
		g := newGuard(t, testDeny, "path\tdocs/"+addr+"\t"+addr+"\tx\n")
		if fs := g.Path("docs/" + addr); len(fs) != 1 {
			t.Fatalf("want the path refused, got %v", fs)
		}
		if fs := g.Message("commit-message", []byte(addr), ""); len(fs) != 1 {
			t.Fatalf("want the message refused, got %v", fs)
		}
	})
	t.Run("a message is excused what its source file is excused", func(t *testing.T) {
		g := newGuard(t, testDeny, "path\tdocs/releases/v1.2.3.md\t"+addr+"\tcontributor credit\n")
		if fs := g.Message("commit-message", []byte("Co-authored-by: A <"+addr+">\n"), "docs/releases/v1.2.3.md"); len(fs) != 0 {
			t.Fatalf("message refused: %v", fs)
		}
		if st := g.Stale(); len(st) != 0 {
			t.Fatalf("stale: %v", st)
		}
	})
	t.Run("a stale entry is red", func(t *testing.T) {
		g := newGuard(t, testDeny, allow)
		g.Content("chart/Chart.yaml", []byte("email: "+addr+"\n"))
		st := g.Stale()
		if len(st) != 1 || !strings.Contains(st[0], "ci/netpol.yml") {
			t.Fatalf("want exactly the unused entry reported, got %v", st)
		}
		if strings.Contains(st[0], cidr) {
			t.Fatalf("a stale report repeats the literal: %v", st)
		}
	})
}

func TestAllowDomainAnywhereAndUpstream(t *testing.T) {
	t.Run("domain covers the domain and its subdomains, nothing else", func(t *testing.T) {
		g := newGuard(t, "", "domain\tcorp.zz\tx\n")
		for _, ok := range []string{at("a", "corp.zz"), at("a", "mail.corp.zz"), at("a", "CORP.ZZ")} {
			if fs := g.Content("a", []byte(ok)); len(fs) != 0 {
				t.Fatalf("allowed domain refused: %v", fs)
			}
		}
		for _, bad := range []string{at("a", "notcorp.zz"), at("a", "corp.zz.evil.zz"), at("a", "zz.zz")} {
			if fs := g.Content("a", []byte(bad)); len(fs) != 1 {
				t.Fatalf("want one finding for a domain that is not allowed, got %v", fs)
			}
		}
	})
	t.Run("a stale domain is red", func(t *testing.T) {
		g := newGuard(t, "", "domain\tcorp.zz\tx\n")
		// An address at ANOTHER domain consults the entry and does not use it.
		if fs := g.Content("a", []byte(at("a", "other.zz"))); len(fs) != 1 {
			t.Fatalf("want one finding, got %v", fs)
		}
		if st := g.Stale(); len(st) != 1 || !strings.Contains(st[0], "domain corp.zz") {
			t.Fatalf("want the unused domain reported by name, got %v", st)
		}
	})
	t.Run("anywhere excuses one literal in every file, path and message", func(t *testing.T) {
		lit := cat("gonk.", "zork", ".lan/")
		g := newGuard(t, testDeny, "anywhere\t"+lit+"\ta non-resolving identifier\n")
		if fs := g.Content("pkg/a.go", []byte("key: "+lit+"session\n")); len(fs) != 0 {
			t.Fatalf("content refused: %v", fs)
		}
		if fs := g.Message("commit-message", []byte(lit+"x"), ""); len(fs) != 0 {
			t.Fatalf("message refused: %v", fs)
		}
		if fs := g.Path("docs/" + lit + "x"); len(fs) != 0 {
			t.Fatalf("path refused: %v", fs)
		}
		// The same zone as a HOST NAME (no trailing slash) is not the
		// literal, and neither is another host under it.
		fs := g.Content("pkg/a.go", []byte(cat("https://gonk.", "zork", ".lan:8443")))
		if len(fs) != 1 || fs[0].Class != I1 {
			t.Fatalf("want one I1 finding for the host form, got %v", fs)
		}
		fs = g.Content("pkg/a.go", []byte(cat("gitlab.", "zork", ".lan/")))
		if len(fs) != 1 || fs[0].Class != I1 {
			t.Fatalf("want one I1 finding for another host, got %v", fs)
		}
		if st := g.Stale(); len(st) != 0 {
			t.Fatalf("stale: %v", st)
		}
	})
	t.Run("upstream switches off the generic rules below it, not the specific ones", func(t *testing.T) {
		g := newGuard(t, testDeny, "upstream\tvendor/\tthird-party code\n")
		text := []byte(ip4(10, 1, 2, 3) + " " + at("author", "corp.zz") + "\n")
		if fs := g.Content("vendor/mod/a.go", text); len(fs) != 0 {
			t.Fatalf("generic rule applied under upstream: %v", fs)
		}
		if fs := g.Content("vendored/a.go", text); len(fs) != 2 {
			t.Fatalf("want 2 findings outside the prefix, got %v", fs)
		}
		fs := g.Content("vendor/mod/a.go", []byte(cat("gitlab.", "zork", ".lan")))
		if len(fs) != 1 || fs[0].Class != I1 {
			t.Fatalf("a specific rule must still apply under upstream, got %v", fs)
		}
		if fs := g.Path("vendor/" + ip4(10, 1, 2, 3) + "/a.go"); len(fs) != 0 {
			t.Fatalf("generic rule applied to an upstream path: %v", fs)
		}
		// A message is never upstream, whatever it is called.
		if fs := g.Message("vendor/x", text, ""); len(fs) != 2 {
			t.Fatalf("want 2 findings in a message, got %v", fs)
		}
	})
	t.Run("a stale upstream is red", func(t *testing.T) {
		g := newGuard(t, "", "upstream\tvendor/\tx\n")
		g.Content("pkg/a.go", []byte("x"))
		if st := g.Stale(); len(st) != 1 {
			t.Fatalf("want the unused upstream reported, got %v", st)
		}
	})
}

func TestDataIsRefusedWhenMalformed(t *testing.T) {
	denyCases := map[string]string{
		"a missing class":      strings.Replace(testDeny, "I4 gitops      *      clusters/zork\n", "", 1),
		"an unknown class":     testDeny + "G1 x * foo\n",
		"too few fields":       testDeny + "I1 x *\n",
		"a bad regex":          testDeny + "I1 x * (\n",
		"an empty match":       testDeny + "I1 x * a*\n",
		"a duplicate name":     testDeny + "I1 zone * other\n",
		"a bad name":           testDeny + "I1 Zone_2 * other\n",
		"a scope without /":    testDeny + "I1 x chart other\n",
		"an absolute scope":    testDeny + "I1 x /chart/ other\n",
		"a glob scope":         testDeny + "I1 x ch*/ other\n",
		"no rules whatsoever":  "# nothing\n",
		"only one of the five": "I1 zone * zork\n",
	}
	for name, d := range denyCases {
		if _, err := New([]byte(d), nil); err == nil {
			t.Errorf("deny data with %s was accepted", name)
		}
	}
	allowCases := map[string]string{
		"an unknown kind":                   "glob\t*\tx\n",
		"a missing reason":                  "domain\tcorp.zz\t \n",
		"a missing field":                   "path\ta\tb\n",
		"an extra field":                    "domain\tcorp.zz\tx\ty\n",
		"a glob path":                       "path\tdocs/*.md\tlit\tx\n",
		"a directory path":                  "path\tdocs/\tlit\tx\n",
		"an absolute path":                  "path\t/docs/a\tlit\tx\n",
		"an empty literal":                  "path\tdocs/a\t\tx\n",
		"an empty anywhere literal":         "anywhere\t\tx\n",
		"a duplicate":                       "domain\tcorp.zz\tx\ndomain\tcorp.zz\ty\n",
		"a wildcard domain":                 "domain\t*.zz\tx\n",
		"a bare TLD as a domain":            "domain\tzz\tx\n",
		"an address as a domain":            "domain\t" + at("a", "corp.zz") + "\tx\n",
		"an upstream without /":             "upstream\tvendor\tx\n",
		"an absolute upstream":              "upstream\t/vendor/\tx\n",
		"spaces instead of TABs":            "domain corp.zz reason\n",
		"a scanner-config for another file": "scanner-config\tdocs/.gitleaks.toml\tx\n",
		"a scanner-config without a reason": "scanner-config\t.gitleaks.toml\t\n",
	}
	for name, a := range allowCases {
		if _, err := New(nil, []byte(a)); err == nil {
			t.Errorf("allow-list with %s was accepted", name)
		}
	}
	if _, err := New([]byte(testDeny), []byte("# comment\n\n"+testAllow)); err != nil {
		t.Fatalf("well-formed data refused: %v", err)
	}
}

func TestSummarizeCountsPerClassAndListsFiles(t *testing.T) {
	g := newGuard(t, testDeny, "")
	var fs []Finding
	fs = append(fs, g.Content("b.md", []byte(ip4(10, 0, 0, 1)+" "+ip4(10, 0, 0, 2)))...)
	fs = append(fs, g.Content("a.md", []byte(ip4(10, 0, 0, 3)+" "+at("x", "corp.zz")))...)
	sum := Summarize(fs)
	if len(sum) != 12 {
		t.Fatalf("want a row for each of the 12 classes, got %d", len(sum))
	}
	for _, s := range sum {
		switch s.Class {
		case G1:
			if s.Matches != 3 || strings.Join(s.Files, ",") != "a.md,b.md" {
				t.Fatalf("G1: %+v", s)
			}
		case G6:
			if s.Matches != 1 || strings.Join(s.Files, ",") != "a.md" {
				t.Fatalf("G6: %+v", s)
			}
		default:
			if s.Matches != 0 || len(s.Files) != 0 {
				t.Fatalf("%s: %+v", s.Class, s)
			}
		}
	}
}

// TestTheGuardDoesNotTripOnItsOwnSource: this package is published. Its
// source -- rules, comments and these tests -- must pass the generic rules.
func TestTheGuardDoesNotTripOnItsOwnSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 6 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	g := newGuard(t, "", "")
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range g.Content(f, data) {
			t.Errorf("own source refused: %s", fd)
		}
	}
}
