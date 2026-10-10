package pipelinectx

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
)

var (
	dnsLabelRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	anchorCharRe = regexp.MustCompile(`^[a-z0-9:\-]+$`)
)

// MaxAnchorLen bounds the anchor the way every bead key is bounded in tests:
// short, one line, no separator characters a ledger or CSV would split on.
const maxAnchorLen = 128

func TestAnchorShapeAndGolden(t *testing.T) {
	fp := "0123456789abcdef"
	cases := []struct {
		pid  int64
		tgt  Target
		ls   int64
		want string
	}{
		{84, MRTarget(7), 0, "gonk:84:pipeline:mr7:0123456789abcdef:s0"},
		{75, RefTarget("main"), 3200, "gonk:75:pipeline:ref-" + RefTarget("main").Key()[4:] + ":0123456789abcdef:s3200"},
	}
	for _, c := range cases {
		got, err := Anchor(c.pid, c.tgt, fp, c.ls, 0)
		if err != nil || got != c.want {
			t.Fatalf("Anchor = %q, %v; want %q", got, err, c.want)
		}
	}
	if k := RefTarget("main").Key(); k != "ref-0d6e4079" {
		t.Fatalf("ref key %q", k)
	}
	if k := RefTarget("release/1.x").Key(); !regexp.MustCompile(`^ref-[0-9a-f]{8}$`).MatchString(k) {
		t.Fatalf("ref key with slash %q", k)
	}
	if RefTarget("main").Key() == RefTarget("master").Key() {
		t.Fatal("ref keys collide")
	}
}

// Worst case: every number at its maximum. The anchor must stay short, one
// line and comma-free; the session key must be a DNS label.
func TestAnchorAndSessionKeyLimits(t *testing.T) {
	targets := []Target{MRTarget(math.MaxInt64), RefTarget("refs/heads/" + strings.Repeat("very-long-branch/", 20))}
	for _, tgt := range targets {
		a, err := Anchor(math.MaxInt64, tgt, "ffffffffffffffff", math.MaxInt64, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) > maxAnchorLen || !anchorCharRe.MatchString(a) || strings.ContainsAny(a, ",\n ") {
			t.Fatalf("anchor %q (%d bytes)", a, len(a))
		}
		sk, err := SessionKey(a)
		if err != nil {
			t.Fatal(err)
		}
		if len(sk) > 63 || !dnsLabelRe.MatchString(sk) {
			t.Fatalf("session key %q not a DNS label", sk)
		}
	}
}

func TestSessionKey(t *testing.T) {
	a, _ := Anchor(84, MRTarget(7), "0123456789abcdef", 0, 0)
	sk, err := SessionKey(a)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^gonk-84-pl-[0-9a-f]{12}$`).MatchString(sk) {
		t.Fatalf("shape %q", sk)
	}
	if sk != "gonk-84-pl-800ad6503688" {
		t.Fatalf("pinned: %q", sk)
	}
	b, _ := Anchor(84, MRTarget(7), "0123456789abcdef", 1, 0)
	if sk2, _ := SessionKey(b); sk2 == sk {
		t.Fatal("different anchors, same session key")
	}
	for _, bad := range []string{"", "gonk:84:issue:7", "gonk:84:pipeline:mr7:0123456789abcdef:s0\n", "gonk:84:pipeline:mr7:0123456789ABCDEF:s0", "gonk:0:pipeline:mr7:0123456789abcdef:s0"} {
		if _, err := SessionKey(bad); err == nil {
			t.Errorf("SessionKey(%q) accepted", bad)
		}
	}
}

func TestAnchorRefusesBadInput(t *testing.T) {
	fp := "0123456789abcdef"
	bad := []struct {
		name string
		pid  int64
		tgt  Target
		fp   string
		ls   int64
	}{
		{"pid 0", 0, MRTarget(1), fp, 0},
		{"mr iid 0", 1, MRTarget(0), fp, 0},
		{"empty ref", 1, RefTarget(""), fp, 0},
		{"ref with newline", 1, RefTarget("a\nb"), fp, 0},
		{"ref with space", 1, RefTarget("a b"), fp, 0},
		{"unknown kind", 1, Target{Kind: "tag", Ref: "v1"}, fp, 0},
		{"fp too short", 1, MRTarget(1), "0123", 0},
		{"fp upper case", 1, MRTarget(1), "0123456789ABCDEF", 0},
		{"fp with comma", 1, MRTarget(1), "0123456789abcde,", 0},
		{"negative last success", 1, MRTarget(1), fp, -1},
	}
	for _, c := range bad {
		if a, err := Anchor(c.pid, c.tgt, c.fp, c.ls, 0); err == nil {
			t.Errorf("%s: accepted %q", c.name, a)
		}
	}
}

func TestTargetStringRoundTrip(t *testing.T) {
	for _, tgt := range []Target{MRTarget(12), RefTarget("main"), RefTarget("release/1.x"), RefTarget("a:b")} {
		got, err := ParseTarget(tgt.String())
		if err != nil || got != tgt {
			t.Fatalf("%q -> %+v, %v", tgt.String(), got, err)
		}
	}
	if MRTarget(12).String() != "mr:12" || RefTarget("main").String() != "ref:main" {
		t.Fatal("recorded form changed")
	}
	for _, bad := range []string{"", "mr", "mr:", "mr:0", "mr:-1", "mr:01", "mr:x", "ref:", "tag:v1", "ref:a b"} {
		if _, err := ParseTarget(bad); !errors.Is(err, ErrBadTarget) {
			t.Errorf("ParseTarget(%q) err %v", bad, err)
		}
	}
}

func TestTerminalForHealth(t *testing.T) {
	for s, want := range map[string]bool{
		"success": true, "failed": true,
		"skipped": false, "canceled": false, "manual": false, "created": false,
		"running": false, "pending": false, "preparing": false, "waiting_for_resource": false,
		"scheduled": false, "": false, "FAILED": false,
	} {
		if got := TerminalForHealth(s); got != want {
			t.Errorf("TerminalForHealth(%q) = %v", s, got)
		}
	}
}
