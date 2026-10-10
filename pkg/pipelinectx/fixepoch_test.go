package pipelinectx

import (
	"math"
	"strings"
	"testing"
	"time"
)

// gonk-2usw part EPOCH (plan 3.3). Everything here is invented: project ids,
// refs, iids, SHAs and times encode nothing about any real project.

// fixlessGolden is what the code produced BEFORE fix epochs existed
// (origin/main 4da8e68, printed by a one-off test that is deliberately not
// kept). A run that follows no merged fix must produce exactly these bytes:
// the anchor is the meter's bead key and the landing's marker, so one byte of
// drift orphans every open run and re-diagnoses every red target.
//
// Never regenerate these to make the test pass.
var fixlessGolden = []struct {
	pid    int64
	tgt    Target
	fp     string
	ls     int64
	anchor string
	key    string
	marker string // RunMarker(anchor, diagnosed)
}{
	{84, MRTarget(7), "0123456789abcdef", 0,
		"gonk:84:pipeline:mr7:0123456789abcdef:s0",
		"gonk-84-pl-800ad6503688",
		"<!-- gonk:pipeline-run:gonk:84:pipeline:mr7:0123456789abcdef:s0 state=diagnosed -->"},
	{84, MRTarget(7), "0123456789abcdef", 1,
		"gonk:84:pipeline:mr7:0123456789abcdef:s1",
		"gonk-84-pl-5550c1abe117",
		"<!-- gonk:pipeline-run:gonk:84:pipeline:mr7:0123456789abcdef:s1 state=diagnosed -->"},
	{75, RefTarget("main"), "0123456789abcdef", 3200,
		"gonk:75:pipeline:ref-0d6e4079:0123456789abcdef:s3200",
		"gonk-75-pl-ea2c7e7c628a",
		"<!-- gonk:pipeline-run:gonk:75:pipeline:ref-0d6e4079:0123456789abcdef:s3200 state=diagnosed -->"},
	{75, RefTarget("main"), "f082455952b2ced7", 880,
		"gonk:75:pipeline:ref-0d6e4079:f082455952b2ced7:s880",
		"gonk-75-pl-e7d85fcd30fa",
		"<!-- gonk:pipeline-run:gonk:75:pipeline:ref-0d6e4079:f082455952b2ced7:s880 state=diagnosed -->"},
	{1, RefTarget("release/1.x"), "ffffffffffffffff", 0,
		"gonk:1:pipeline:ref-81caa6e9:ffffffffffffffff:s0",
		"gonk-1-pl-7e7862a183c3",
		"<!-- gonk:pipeline-run:gonk:1:pipeline:ref-81caa6e9:ffffffffffffffff:s0 state=diagnosed -->"},
	{math.MaxInt64, MRTarget(math.MaxInt64), "ffffffffffffffff", math.MaxInt64,
		"gonk:9223372036854775807:pipeline:mr9223372036854775807:ffffffffffffffff:s9223372036854775807",
		"gonk-9223372036854775807-pl-ad21d544701d",
		"<!-- gonk:pipeline-run:gonk:9223372036854775807:pipeline:mr9223372036854775807:ffffffffffffffff:s9223372036854775807 state=diagnosed -->"},
}

func TestFixlessIdentityGolden(t *testing.T) {
	for _, g := range fixlessGolden {
		a, err := Anchor(g.pid, g.tgt, g.fp, g.ls, 0)
		if err != nil {
			t.Fatalf("Anchor(%d, %v, %s, %d, 0) = %v", g.pid, g.tgt, g.fp, g.ls, err)
		}
		if a != g.anchor {
			t.Errorf("anchor without a fix changed (%d byte(s) vs %d):\n got %q\nwant %q", len(a), len(g.anchor), a, g.anchor)
		}
		// The key and the marker are checked from the PINNED anchor too, so a
		// drifted Anchor cannot hide a drifted SessionKey or RunMarker.
		for _, in := range []string{a, g.anchor} {
			if k, err := SessionKey(in); err != nil || k != g.key {
				t.Errorf("SessionKey(%q) = %q, %v; want %q", in, k, err, g.key)
			}
			if m, err := RunMarker(in, RunStateDiagnosed); err != nil || m != g.marker {
				t.Errorf("RunMarker(%q) = %q, %v; want %q", in, m, err, g.marker)
			}
		}
		if e, err := AnchorFixEpoch(g.anchor); err != nil || e != 0 {
			t.Errorf("AnchorFixEpoch(%q) = %d, %v; want 0", g.anchor, e, err)
		}
	}
}

// With a fix epoch the anchor is the fix-less one plus ":f<iid>", nothing
// else moves, and the session key and the marker are the new run's own.
func TestAnchorFixEpochSuffix(t *testing.T) {
	for _, g := range fixlessGolden {
		for _, epoch := range []int64{1, 12, math.MaxInt64} {
			a, err := Anchor(g.pid, g.tgt, g.fp, g.ls, epoch)
			if err != nil {
				t.Fatalf("Anchor(..., %d) = %v", epoch, err)
			}
			want := g.anchor + ":f" + itoa(epoch)
			if a != want {
				t.Fatalf("anchor after merged fix !%d = %q, want %q", epoch, a, want)
			}
			if a == g.anchor {
				t.Fatalf("after merged fix !%d the anchor is still the old run's: %q", epoch, a)
			}
			k, err := SessionKey(a)
			if err != nil {
				t.Fatalf("SessionKey(%q) = %v", a, err)
			}
			if k == g.key {
				t.Fatalf("after merged fix !%d the session key is still the old run's: %q", epoch, k)
			}
			if len(k) > 63 || !dnsLabelRe.MatchString(k) || !strings.HasPrefix(k, "gonk-"+itoa(g.pid)+"-pl-") {
				t.Fatalf("session key %q is not gonk-<pid>-pl-<hex> / a DNS label", k)
			}
			m, err := RunMarker(a, RunStateDiagnosed)
			if err != nil || m != "<!-- gonk:pipeline-run:"+a+" state=diagnosed -->" {
				t.Fatalf("RunMarker(%q) = %q, %v", a, m, err)
			}
			if got, err := AnchorFixEpoch(a); err != nil || got != epoch {
				t.Fatalf("AnchorFixEpoch(%q) = %d, %v; want %d", a, got, err, epoch)
			}
			if len(a) > maxAnchorLen || !anchorCharRe.MatchString(a) {
				t.Fatalf("anchor %q (%d bytes) exceeds the bead-key bounds", a, len(a))
			}
		}
	}
	// Two different fixes are two different runs.
	a1, _ := Anchor(75, RefTarget("main"), "0123456789abcdef", 880, 12)
	a2, _ := Anchor(75, RefTarget("main"), "0123456789abcdef", 880, 13)
	if a1 == a2 {
		t.Fatal("two fix epochs, one anchor")
	}
	// Pinned, so the suffix's spelling cannot drift either.
	if a1 != "gonk:75:pipeline:ref-0d6e4079:0123456789abcdef:s880:f12" {
		t.Fatalf("pinned epoch anchor: %q", a1)
	}
	if k, _ := SessionKey(a1); k != "gonk-75-pl-97317edcdefb" {
		t.Fatalf("pinned epoch session key: %q", k)
	}
}

func itoa(n int64) string {
	a, _ := Anchor(1, MRTarget(1), "0000000000000000", n, 0)
	return strings.TrimPrefix(a, "gonk:1:pipeline:mr1:0000000000000000:s")
}

// "No fix" has exactly one spelling, and nothing that merely resembles an
// epoch is an anchor.
func TestAnchorRefusesBadFixEpoch(t *testing.T) {
	if a, err := Anchor(1, MRTarget(1), "0123456789abcdef", 0, -1); err == nil {
		t.Errorf("negative fix epoch accepted: %q", a)
	}
	base := "gonk:84:pipeline:mr7:0123456789abcdef:s0"
	for _, suffix := range []string{":f0", ":f", ":f012", ":f-1", ":F12", ":f12 ", ":f12\n", ":f12:f13", ":f1x", ":g12", "f12", ":f12:"} {
		bad := base + suffix
		if k, err := SessionKey(bad); err == nil {
			t.Errorf("SessionKey(%q) accepted: %q", bad, k)
		}
		if m, err := RunMarker(bad, RunStateDiagnosed); err == nil {
			t.Errorf("RunMarker(%q) accepted: %q", bad, m)
		}
		if e, err := AnchorFixEpoch(bad); err == nil {
			t.Errorf("AnchorFixEpoch(%q) accepted: %d", bad, e)
		}
	}
	if _, err := AnchorFixEpoch("gonk:84:issue:7"); err == nil {
		t.Error("AnchorFixEpoch accepted an issue anchor")
	}
}

// The old run's marker says nothing about the new run and vice versa: one
// anchor is a strict PREFIX of the other, which is exactly the confusion an
// exact comparison rules out.
func TestRunMarkersDoNotCrossFixEpochs(t *testing.T) {
	old, _ := Anchor(75, RefTarget("main"), "0123456789abcdef", 880, 0)
	next, _ := Anchor(75, RefTarget("main"), "0123456789abcdef", 880, 12)
	later, _ := Anchor(75, RefTarget("main"), "0123456789abcdef", 880, 123)
	mOld, _ := RunMarker(old, RunStateDiagnosed)
	mNext, _ := RunMarker(next, RunStateDiagnosing)
	mLater, _ := RunMarker(later, RunStateDenied)
	body := "note\n" + mOld + "\n" + mNext + "\n" + mLater + "\n"
	for _, c := range []struct{ anchor, want string }{
		{old, RunStateDiagnosed}, {next, RunStateDiagnosing}, {later, RunStateDenied},
	} {
		got := RunMarkerStates(body, c.anchor)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("RunMarkerStates(%q) = %v, want [%s]", c.anchor, got, c.want)
		}
	}
	if got := RunMarkerStates(mOld, next); len(got) != 0 {
		t.Errorf("the old run's marker answered for the run after the fix: %v", got)
	}
	if got := RunMarkerStates(mNext, old); len(got) != 0 {
		t.Errorf("the new run's marker answered for the run the fix ended: %v", got)
	}
}

func TestIsFixBranch(t *testing.T) {
	for name, want := range map[string]bool{
		"gonk/pipefix-0123456789ab":   true,
		"gonk/pipefix-ffffffffffff":   true,
		"gonk/pipefix-":               false,
		"gonk/pipefix-0123456789a":    false, // 11
		"gonk/pipefix-0123456789abc":  false, // 13
		"gonk/pipefix-0123456789AB":   false,
		"gonk/pipefix-0123456789ab/x": false,
		"x/gonk/pipefix-0123456789ab": false,
		"gonk/onboard":                false,
		"gonk/pipefix":                false,
		"":                            false,
		"gonk/pipefix-0123456789ab\n": false,
	} {
		if got := IsFixBranch(name); got != want {
			t.Errorf("IsFixBranch(%q) = %v, want %v", name, got, want)
		}
		if want && !strings.HasPrefix(name, FixBranchPrefix) {
			t.Errorf("%q is a fix branch without FixBranchPrefix", name)
		}
	}
}

const (
	shaMerge  = "1111111111111111111111111111111111111111"
	shaSquash = "2222222222222222222222222222222222222222"
	shaHead   = "3333333333333333333333333333333333333333"
	shaOther  = "9999999999999999999999999999999999999999"
)

func at(min int) time.Time { return time.Date(2026, 3, 1, 12, min, 0, 0, time.UTC) }

func fix(iid int64, mergedMin int, shas ...string) MergedFix {
	return MergedFix{IID: iid, TargetBranch: "main", MergedAt: at(mergedMin), LandedSHAs: shas}
}

func TestFixEpoch(t *testing.T) {
	cases := []struct {
		name  string
		fixes []MergedFix
		f     FixEpochFacts
		want  int64
	}{
		{"no merged fix: nothing is examined, not even missing times", nil, FixEpochFacts{}, 0},
		{"a fix merged before the pipeline was created",
			[]MergedFix{fix(12, 10)}, FixEpochFacts{PipelineSHA: shaOther, PipelineCreatedAt: at(20)}, 12},
		{"a pipeline created at the very instant of the merge is after it",
			[]MergedFix{fix(12, 10)}, FixEpochFacts{PipelineSHA: shaOther, PipelineCreatedAt: at(10)}, 12},
		{"a pipeline created BEFORE the merge belongs to the run the fix ended",
			[]MergedFix{fix(12, 10)}, FixEpochFacts{PipelineSHA: shaOther, PipelineCreatedAt: at(9)}, 0},
		{"the merge commit's own pipeline, created before merged_at was stamped",
			[]MergedFix{fix(12, 10, shaMerge, "", shaHead)}, FixEpochFacts{PipelineSHA: shaMerge, PipelineCreatedAt: at(9)}, 12},
		{"a squash commit is a landed commit",
			[]MergedFix{fix(12, 10, "", shaSquash, shaHead)}, FixEpochFacts{PipelineSHA: shaSquash, PipelineCreatedAt: at(9)}, 12},
		{"a fast-forwarded head is a landed commit",
			[]MergedFix{fix(12, 10, "", "", shaHead)}, FixEpochFacts{PipelineSHA: shaHead, PipelineCreatedAt: at(9)}, 12},
		{"an empty pipeline SHA matches no empty landed SHA",
			[]MergedFix{fix(12, 10, "", "", "")}, FixEpochFacts{PipelineSHA: "", PipelineCreatedAt: at(9)}, 0},
		{"no last success: any earlier merged fix counts",
			[]MergedFix{fix(12, 1)}, FixEpochFacts{PipelineCreatedAt: at(50)}, 12},
		{"no last success: the last-success fields are not read at all",
			[]MergedFix{fix(12, 10, shaMerge)},
			FixEpochFacts{PipelineCreatedAt: at(50), LastSuccess: 0, LastSuccessSHA: shaMerge, LastSuccessCreatedAt: at(30)}, 12},
		{"a fix merged before the last green was created is over",
			[]MergedFix{fix(12, 10)},
			FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessSHA: shaOther, LastSuccessCreatedAt: at(30)}, 0},
		{"a fix merged at the very instant the last green was created is over",
			[]MergedFix{fix(12, 30)},
			FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessSHA: shaOther, LastSuccessCreatedAt: at(30)}, 0},
		{"a fix merged after the last green was created counts",
			[]MergedFix{fix(12, 31)},
			FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessSHA: shaOther, LastSuccessCreatedAt: at(30)}, 12},
		{"the last green ran ON the fix (created before merged_at was stamped): the fix is over",
			[]MergedFix{fix(12, 31, shaMerge)},
			FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessSHA: shaMerge, LastSuccessCreatedAt: at(30)}, 0},
		{"the newest by merged_at wins, whatever the order and the iids",
			[]MergedFix{fix(30, 10), fix(12, 20), fix(40, 5)}, FixEpochFacts{PipelineCreatedAt: at(50)}, 12},
		{"a tie on merged_at goes to the larger iid",
			[]MergedFix{fix(12, 10), fix(14, 10), fix(13, 10)}, FixEpochFacts{PipelineCreatedAt: at(50)}, 14},
		{"a LATER fix does not move an earlier pipeline's epoch",
			[]MergedFix{fix(12, 10), fix(13, 30)}, FixEpochFacts{PipelineSHA: shaOther, PipelineCreatedAt: at(20)}, 12},
		{"between two fixes with a green after the first: only the second counts",
			[]MergedFix{fix(12, 10), fix(13, 30)},
			FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessSHA: shaOther, LastSuccessCreatedAt: at(20)}, 13},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FixEpoch(c.fixes, c.f)
			if err != nil || got != c.want {
				t.Fatalf("FixEpoch = %d, %v; want %d", got, err, c.want)
			}
		})
	}
}

// Unknown is not "no fix".
func TestFixEpochFailsClosed(t *testing.T) {
	ok := FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880, LastSuccessCreatedAt: at(5)}
	cases := []struct {
		name  string
		fixes []MergedFix
		f     FixEpochFacts
	}{
		{"pipeline creation time unknown", []MergedFix{fix(12, 10)}, FixEpochFacts{}},
		{"last success creation time unknown", []MergedFix{fix(12, 10)}, FixEpochFacts{PipelineCreatedAt: at(40), LastSuccess: 880}},
		{"a fix without merged_at", []MergedFix{{IID: 12, TargetBranch: "main"}}, ok},
		{"a fix without merged_at behind a good one", []MergedFix{fix(13, 10), {IID: 12}}, ok},
		{"a fix without an iid", []MergedFix{{MergedAt: at(10)}}, ok},
		{"a fix that would not even count still must be well-formed", []MergedFix{{IID: 12}}, FixEpochFacts{PipelineCreatedAt: at(1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := FixEpoch(c.fixes, c.f); err == nil {
				t.Fatalf("FixEpoch = %d with no error; an unknown fact must not read as an answer", got)
			}
		})
	}
}
