package pipelinectx

import (
	"regexp"
	"testing"
)

// All job names, stages and error texts here are invented for these tests
// (gonk-2usw plan section 0); none is copied from a real job log.

// GenericErrorLine: the runner's own summary says THAT a job failed, never
// why. A table of both sides, because a line wrongly flagged generic makes the
// exit check treat a different failure as the diagnosed one, and a generic
// line NOT flagged lets "the script still exits 1" count as the same
// specific failure only by coincidence of the exit code.
func TestGenericErrorLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		// --- the closed list ---
		{"docker/shell executor, exit code", "ERROR: Job failed: exit code 1", true},
		{"another exit code is as generic", "ERROR: Job failed: exit code 137", true},
		{"shell executor, exit status", "ERROR: Job failed: exit status 2", true},
		{"kubernetes executor", "ERROR: Job failed: command terminated with exit code 1", true},
		{"surrounding whitespace", "  ERROR: Job failed: exit code 1\t", true},
		{"inner whitespace run", "ERROR:  Job failed:   exit code 1", true},
		{"cut by the line cap", "ERROR: Job failed: exit code 1" + LineTruncatedSuffix, true},
		// --- everything else says something about the cause ---
		{"empty", "", false},
		{"blank", "   ", false},
		{"a tool's own error", "error: frob step failed", false},
		{"the summary with a cause appended", "ERROR: Job failed: exit code 1: frob step failed", false},
		{"the summary embedded in another line", "widgetd said ERROR: Job failed: exit code 1", false},
		{"a quoted summary", "echo 'ERROR: Job failed: exit code 1'", false},
		{"system failure names a cause", "ERROR: Job failed (system failure): frob volume not mounted", false},
		{"timeout is its own failure", "ERROR: Job failed: execution took longer than 1h0m0s seconds", false},
		{"no exit code at all", "ERROR: Job failed: exit code", false},
		{"a non-numeric code", "ERROR: Job failed: exit code one", false},
		{"lower case is not the runner", "error: job failed: exit code 1", false},
		{"a test runner's count", "1 failed, 1 passed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GenericErrorLine(c.line); got != c.want {
				t.Fatalf("GenericErrorLine(%q) = %v, want %v (canonical %q)", c.line, got, c.want, CanonicalError(c.line))
			}
		})
	}
}

// GenericFirstError is the plan's "generic signature" (3.2): no signature at
// all, or the runner's summary.
func TestGenericFirstError(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"", true},    // no first error line: nothing to compare on
		{"   ", true}, // no content: ErrorSignature is ""
		{"ERROR: Job failed: exit code 1", true},
		{"error: frob step failed", false},
	}
	for _, c := range cases {
		if got := GenericFirstError(c.line); got != c.want {
			t.Errorf("GenericFirstError(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func fk(stage, name, reason, sig string) JobFailureKey {
	return JobFailureKey{Stage: stage, Name: name, FailureReason: reason, ErrorSignature: sig}
}

// A job failure key is (stage, name, failure_reason, signature): changing any
// ONE field makes a different key, both as a value and as its string.
func TestJobFailureKeyEveryFieldCounts(t *testing.T) {
	base := fk("test", "unit", "script_failure", "es1-0123456789abcdef")
	variants := map[string]JobFailureKey{
		"stage":          fk("build", "unit", "script_failure", "es1-0123456789abcdef"),
		"name":           fk("test", "lint", "script_failure", "es1-0123456789abcdef"),
		"failure_reason": fk("test", "unit", "runner_system_failure", "es1-0123456789abcdef"),
		"signature":      fk("test", "unit", "script_failure", "es1-fedcba9876543210"),
		"no signature":   fk("test", "unit", "script_failure", ""),
	}
	for field, v := range variants {
		t.Run(field, func(t *testing.T) {
			if v == base || v.String() == base.String() {
				t.Fatalf("changing %s left the key equal: %q", field, v.String())
			}
			if CauseKey([]JobFailureKey{v}) == CauseKey([]JobFailureKey{base}) {
				t.Fatalf("changing %s left the cause key equal", field)
			}
		})
	}
	if same := fk("test", "unit", "script_failure", "es1-0123456789abcdef"); same != base || same.String() != base.String() {
		t.Fatal("equal fields must make equal keys")
	}
}

// The string form is injective: field boundaries cannot be moved by putting a
// separator inside a field (a job name is project-controlled text).
func TestJobFailureKeyStringIsInjective(t *testing.T) {
	pairs := []struct {
		name string
		a, b JobFailureKey
	}{
		{"boundary moved by concatenation", fk("ab", "c", "r", "s"), fk("a", "bc", "r", "s")},
		{"NUL inside a field", fk("a\x00b", "c", "r", "s"), fk("a", "b\x00c", "r", "s")},
		{"quote inside a field", fk(`a" "b`, "c", "r", "s"), fk("a", `b" "c`, "r", "s")},
		{"space inside a field", fk("a b", "c", "r", "s"), fk("a", "b c", "r", "s")},
		{"newline inside a field", fk("a", "b\nc", "r", "s"), fk("a", "b", "c\nr", "s")},
		{"empty field position", fk("", "a", "r", "s"), fk("a", "", "r", "s")},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			if p.a.String() == p.b.String() {
				t.Fatalf("two different keys share the string %q", p.a.String())
			}
			if CauseKey([]JobFailureKey{p.a}) == CauseKey([]JobFailureKey{p.b}) {
				t.Fatal("two different keys share a cause key")
			}
		})
	}
	// One line, ASCII only: it is hashed line-wise and may be logged.
	s := fk("st\nage", "n\u00e4me", "r\x00", "es1-0123456789abcdef").String()
	if !regexp.MustCompile(`^[\x20-\x7e]+$`).MatchString(s) {
		t.Fatalf("string form is not one printable-ASCII line: %q", s)
	}
}

// The cause key is a function of the SET of job failure keys: order and
// repetition do not matter; membership does.
func TestCauseKey(t *testing.T) {
	a := fk("test", "unit", "script_failure", "es1-0123456789abcdef")
	b := fk("test", "lint", "script_failure", "es1-fedcba9876543210")
	c := fk("build", "compile", "script_failure", "")
	abc := CauseKey([]JobFailureKey{a, b, c})

	t.Run("shape", func(t *testing.T) {
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(abc) {
			t.Fatalf("cause key %q is not 16 lowercase hex", abc)
		}
	})
	t.Run("order does not matter", func(t *testing.T) {
		for _, perm := range [][]JobFailureKey{{a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}} {
			if got := CauseKey(perm); got != abc {
				t.Fatalf("CauseKey(%v) = %s, want %s: the key changes with job order", perm, got, abc)
			}
		}
	})
	t.Run("the input is not reordered", func(t *testing.T) {
		in := []JobFailureKey{b, c, a}
		CauseKey(in)
		if in[0] != b || in[1] != c || in[2] != a {
			t.Fatalf("CauseKey sorted its caller's slice: %v", in)
		}
	})
	t.Run("repetition does not matter", func(t *testing.T) {
		if got := CauseKey([]JobFailureKey{a, b, a, c, b}); got != abc {
			t.Fatalf("a repeated key changed the cause key: %s vs %s", got, abc)
		}
	})
	t.Run("membership matters", func(t *testing.T) {
		seen := map[string]string{}
		for name, set := range map[string][]JobFailureKey{
			"a": {a}, "b": {b}, "c": {c}, "ab": {a, b}, "ac": {a, c}, "bc": {b, c}, "abc": {a, b, c},
		} {
			k := CauseKey(set)
			if other, dup := seen[k]; dup {
				t.Fatalf("sets %s and %s share cause key %s", name, other, k)
			}
			seen[k] = name
		}
	})
	t.Run("no jobs, no cause", func(t *testing.T) {
		if got := CauseKey(nil); got != "" {
			t.Fatalf("CauseKey(nil) = %q, want empty: a run with no judged job has no cause key", got)
		}
		if got := CauseKey([]JobFailureKey{}); got != "" {
			t.Fatalf("CauseKey(empty) = %q, want empty", got)
		}
	})
}

// The bytes are pinned: the cause key is written into a fix MR's metadata
// block and compared on later looks (dedup, plan 3.2 E9), so a silent change
// would let a closed fix be proposed again.
func TestFailureKeyGolden(t *testing.T) {
	k := fk("test", "unit-tests", "script_failure", "es1-b3197b9cdeb48a2d")
	if got, want := k.String(), `"test" "unit-tests" "script_failure" "es1-b3197b9cdeb48a2d"`; got != want {
		t.Fatalf("string form\n got  %s\n want %s", got, want)
	}
	if got, want := CauseKey([]JobFailureKey{k}), "86a526ee68dff6fd"; got != want {
		t.Fatalf("cause key of one job = %s, want %s -- if the encoding changed on purpose, say so in the plan: every open fix MR's metadata carries the old one", got, want)
	}
	two := []JobFailureKey{k, fk("test", "lint", "script_failure", "")}
	if got, want := CauseKey(two), "8b55ccca40ac7ed4"; got != want {
		t.Fatalf("cause key of two jobs = %s, want %s", got, want)
	}
}
