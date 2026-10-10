package pipelinectx

import (
	"regexp"
	"testing"
)

// All error texts here are invented for these tests; none is copied from a
// real job log and none describes a specific real failure.

// The same failure in two synthetic projects, on two different runs, yields
// the same signature.
func TestErrorSignatureSameFailureAcrossProjects(t *testing.T) {
	projA := "" +
		"Running on runner-a via host-a...\n" +
		"$ ./tool frob\n" +
		"2026-01-02T03:04:05Z widgetd: error: frob step failed for /builds/alpha/one/src/x.go after 1.5s (req 3f9a2c1d7e, id 6f1c0a2e-1b2c-4d5e-8f90-123456789abc) from 192.0.2.3 via frob-a.cluster.example.test:9000\n" +
		"ERROR: Job failed: exit code 1\n"
	projB := "" +
		"Running on runner-b via host-b...\n" +
		"$ ./tool frob\n" +
		"2026-02-11T22:15:59.123+01:00 widgetd: error: frob step failed for /builds/beta/team/two/lib/y.go after 250ms (req 00aa11bb22cc, id 0e0e0e0e-aaaa-4bbb-8ccc-0d0d0d0d0d0d) from 198.51.100.7 via frob-b.other.example.test:9443\n" +
		"ERROR: Job failed: exit code 1\n"
	a, b := Excerpt([]byte(projA)), Excerpt([]byte(projB))
	if a.ErrorSignature == "" || a.ErrorSignature != b.ErrorSignature {
		t.Fatalf("signatures differ:\n %s %q\n %s %q", a.ErrorSignature, CanonicalError(a.FirstError), b.ErrorSignature, CanonicalError(b.FirstError))
	}
	if !regexp.MustCompile(`^es1-[0-9a-f]{16}$`).MatchString(a.ErrorSignature) {
		t.Fatalf("shape %q", a.ErrorSignature)
	}
}

// Each volatile class on its own: the variant canonicalises to the same thing.
func TestErrorSignatureVolatileVariants(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"hex id", "error: object 3f9a2c1d7e not found", "error: object 00ff11ee22dd not found"},
		{"0x hex", "panic at 0x7ffd1234", "panic at 0xdeadbeef"},
		{"number", "error: 3 of 17 shards failed", "error: 12 of 400 shards failed"},
		{"iso timestamp", "2026-01-02T03:04:05Z error: frob", "2027-12-31 23:59:59.999 error: frob"},
		{"time of day", "[03:04:05] error: frob", "[23:00:01.5] error: frob"},
		{"build path", "error: cannot open /builds/alpha/one/data/in.txt", "error: cannot open /builds/b/c/d/e/other.txt"},
		{"temp path", "error: cannot open /tmp/tmp.Xy7Qa/f", "error: cannot open /tmp/abc/def"},
		{"uuid", "error: req 6f1c0a2e-1b2c-4d5e-8f90-123456789abc", "error: req 0e0e0e0e-aaaa-4bbb-8ccc-0d0d0d0d0d0d"},
		{"ipv4", "error: frob from 192.0.2.3 refused", "error: frob from 203.0.113.254 refused"},
		{"ipv6", "error: frob from 2001:db8::1 refused", "error: frob from 2001:db8:0:0:0:0:0:7 refused"},
		{"url host", "error: GET https://frob-a.example.test/v1/x failed", "error: GET https://u@frob-b.other.test:8443/v1/x failed"},
		{"host:port", "error: frob via frob-a.cluster.example.test:9000", "error: frob via b.c.d:1"},
		{"named host", "error: lookup frob-a.example.test: frob", "error: lookup other.host.test: frob"},
		{"duration go", "error: frob gave up after 1m30.5s", "error: frob gave up after 250ms"},
		{"duration words", "error: frob gave up after 3 seconds", "error: frob gave up after 12 minutes"},
		{"whitespace", "error:  frob\t failed", "error: frob failed"},
		{"truncation suffix", "error: frob failed" + LineTruncatedSuffix, "error: frob failed"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if sa, sb := ErrorSignature(p.a), ErrorSignature(p.b); sa != sb {
				t.Fatalf("differ:\n %q\n %q", CanonicalError(p.a), CanonicalError(p.b))
			}
		})
	}
}

// Different failures stay different.
func TestErrorSignatureDifferentFailuresDiffer(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"different message", "error: frob step failed", "error: blit step failed"},
		{"exit status kept", "ERROR: Job failed: exit code 1", "ERROR: Job failed: exit code 2"},
		{"exit status words", "process exited: exit status 1", "process exited: exit status 9"},
		{"dotted type names are not hosts", "widgets.core.FrobError: bad input", "widgets.core.BlitError: bad input"},
		{"letters-only hex-ish word kept", "error: defaced record", "error: acceded record"},
		{"file name outside builds kept", "error: cannot open frob.conf", "error: cannot open blit.conf"},
		{"case kept", "Error: frob", "ERROR: frob"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if ErrorSignature(p.a) == ErrorSignature(p.b) {
				t.Fatalf("same signature for\n %q\n %q (canonical %q)", p.a, p.b, CanonicalError(p.a))
			}
		})
	}
}

func TestErrorSignatureGolden(t *testing.T) {
	in := "2026-01-02T03:04:05Z widgetd: error: frob step failed for /builds/alpha/one/src/x.go after 1.5s (req 3f9a2c1d7e) from 192.0.2.3: exit code 3"
	wantCanon := "<ts> widgetd: error: frob step failed for <builds> after <dur> (req <hex>) from <ip>: exit code 3"
	if got := CanonicalError(in); got != wantCanon {
		t.Fatalf("canonical\n got  %q\n want %q", got, wantCanon)
	}
	if got := ErrorSignature(in); got != "es1-b3197b9cdeb48a2d" {
		t.Fatalf("signature %q -- if canonicalisation changed on purpose, bump SignatureVersion and re-pin", got)
	}
	for _, empty := range []string{"", "   ", LineTruncatedSuffix} {
		if got := ErrorSignature(empty); got != "" {
			t.Errorf("ErrorSignature(%q) = %q, want empty", empty, got)
		}
	}
}

func TestExcerptCarriesSignature(t *testing.T) {
	e := Excerpt([]byte("ok\nerror: frob 12 failed\nlater error: other\n"))
	if e.FirstError != "error: frob 12 failed" || e.ErrorSignature != ErrorSignature("error: frob 99 failed") {
		t.Fatalf("%q %q", e.FirstError, e.ErrorSignature)
	}
}
