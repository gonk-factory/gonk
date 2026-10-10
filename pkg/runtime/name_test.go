package runtime_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/runtime"
)

// nameCase is one set of Name inputs. The table deliberately spans the plain
// path, the project-scoped path, the too-long path and the unsanitary path,
// because every property below must hold for ALL of them.
type nameCase struct {
	label   string
	agent   string
	projID  int64
	kind    runtime.Kind
	iid     int64
	attempt int
}

var nameCases = []nameCase{
	{"triage issue", "triage", 42, runtime.KindIssue, 7, 1},
	{"mention mr", "mention", 42, runtime.KindMergeRequest, 7, 1},
	{"scaffold project-scoped", "scaffold", 42, runtime.KindIssue, 0, 3},
	{"large ids", "triage", 987654321, runtime.KindIssue, 123456, 12},
	{"max ids", "scaffold", 9223372036854775807, runtime.KindMergeRequest, 9223372036854775807, 2147483647},
	{"long agent", strings.Repeat("agent", 20), 1, runtime.KindIssue, 1, 1},
	{"upper and underscore", "Tri_Age", 1, runtime.KindIssue, 1, 1},
	{"unicode", "triäge", 1, runtime.KindIssue, 1, 1},
	{"empty agent", "", 1, runtime.KindIssue, 1, 1},
	{"negative project", "triage", -5, runtime.KindIssue, 1, 1},
	{"trailing hyphen at cut", strings.Repeat("a", 37) + "-" + strings.Repeat("b", 30), 1, runtime.KindIssue, 1, 1},
}

func (c nameCase) name() string {
	return runtime.Name(c.agent, c.projID, c.kind, c.iid, c.attempt)
}

// The pinned forms: what P3 will store as Record.SessionID and what an
// operator will read in `kubectl get jobs`.
func TestNamePlainForm(t *testing.T) {
	for _, tc := range []struct {
		c    nameCase
		want string
	}{
		{nameCases[0], "gonk-triage-p42-i7-a1"},
		{nameCases[1], "gonk-mention-p42-m7-a1"},
		{nameCases[2], "gonk-scaffold-p42-a3"},
		{nameCases[3], "gonk-triage-p987654321-i123456-a12"},
	} {
		if got := tc.c.name(); got != tc.want {
			t.Errorf("%s: Name = %q, want %q", tc.c.label, got, tc.want)
		}
	}
}

func TestNameIsAlwaysAValidDNS1123LabelWithinTheCap(t *testing.T) {
	for _, c := range nameCases {
		got := c.name()
		if len(got) > runtime.MaxNameLen {
			t.Errorf("%s: Name = %q is %d chars, cap %d", c.label, got, len(got), runtime.MaxNameLen)
		}
		if err := runtime.ValidName(got); err != nil {
			t.Errorf("%s: %v", c.label, err)
		}
		// Independent of ValidName, so a regression in ValidName itself
		// cannot make this test agree with a broken Name.
		for i, r := range got {
			alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !alnum && (r != '-' || i == 0 || i == len(got)-1) {
				t.Errorf("%s: Name = %q has %q at %d", c.label, got, r, i)
			}
		}
	}
}

func TestNameIsDeterministic(t *testing.T) {
	for _, c := range nameCases {
		first := c.name()
		for i := 0; i < 5; i++ {
			if again := c.name(); again != first {
				t.Fatalf("%s: Name gave %q then %q", c.label, first, again)
			}
		}
	}
}

// Every case must name a DIFFERENT session: a collision is two beads sharing
// one Job, and the second dispatch silently getting the first one's result.
func TestNameDistinguishesEveryInput(t *testing.T) {
	seen := map[string]string{}
	add := func(label, n string) {
		if prev, ok := seen[n]; ok {
			t.Errorf("%s and %s both map to %q", prev, label, n)
		}
		seen[n] = label
	}
	for _, c := range nameCases {
		add(c.label, c.name())
	}
	// Each coordinate, varied alone.
	add("attempt 2", runtime.Name("triage", 42, runtime.KindIssue, 7, 2))
	add("iid 8", runtime.Name("triage", 42, runtime.KindIssue, 8, 1))
	add("project 43", runtime.Name("triage", 42+1, runtime.KindIssue, 7, 1))
	add("agent mention issue", runtime.Name("mention", 42, runtime.KindIssue, 7, 1))
	// Inputs that sanitize to the same prefix still differ, via the hash.
	add("a_b", runtime.Name("a_b", 1, runtime.KindIssue, 1, 1))
	add("a.b", runtime.Name("a.b", 1, runtime.KindIssue, 1, 1))
	add("a-b", runtime.Name("a-b", 1, runtime.KindIssue, 1, 1))
	// Long names sharing the whole truncated prefix differ only in the hash.
	long := strings.Repeat("x", 60)
	add("long attempt 1", runtime.Name(long, 1, runtime.KindIssue, 1, 1))
	add("long attempt 2", runtime.Name(long, 1, runtime.KindIssue, 1, 2))
}

// The hash-suffix path: an over-long name is a truncated readable prefix, a
// hyphen, and the first 8 hex of sha256 of the PLAIN (unshortened) name.
func TestNameHashSuffixPath(t *testing.T) {
	agent := strings.Repeat("agent", 20)
	plain := "gonk-" + agent + "-p1-i1-a1"
	sum := sha256.Sum256([]byte(plain))
	wantSuffix := "-" + hex.EncodeToString(sum[:])[:8]

	got := runtime.Name(agent, 1, runtime.KindIssue, 1, 1)
	if len(got) != runtime.MaxNameLen {
		t.Errorf("Name = %q is %d chars; a truncated name should use the full %d", got, len(got), runtime.MaxNameLen)
	}
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("Name = %q, want suffix %q (sha256 of %q)", got, wantSuffix, plain)
	}
	if !strings.HasPrefix(got, "gonk-agentagent") {
		t.Errorf("Name = %q lost its readable prefix", got)
	}

	// Unsanitary but short: hashed too, and still legible.
	got = runtime.Name("Tri_Age", 1, runtime.KindIssue, 1, 1)
	sum = sha256.Sum256([]byte("gonk-Tri_Age-p1-i1-a1"))
	if want := "gonk-tri-age-p1-i1-a1-" + hex.EncodeToString(sum[:])[:8]; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}

	// A cut that lands on a hyphen does not leave a doubled one.
	got = nameCases[len(nameCases)-1].name()
	if strings.Contains(got, "--") {
		t.Errorf("Name = %q has a doubled hyphen at the cut", got)
	}
}

// The plain form is used unhashed whenever it fits: the boundary is exactly
// MaxNameLen.
func TestNameUsesThePlainFormUpToTheCap(t *testing.T) {
	// "gonk-" + agent + "-p1-i1-a1" = 14 + len(agent).
	fits := strings.Repeat("a", runtime.MaxNameLen-14)
	if got, want := runtime.Name(fits, 1, runtime.KindIssue, 1, 1), "gonk-"+fits+"-p1-i1-a1"; got != want {
		t.Errorf("at exactly the cap: Name = %q, want the plain %q", got, want)
	}
	over := fits + "a"
	if got := runtime.Name(over, 1, runtime.KindIssue, 1, 1); got == "gonk-"+over+"-p1-i1-a1" || len(got) > runtime.MaxNameLen {
		t.Errorf("one over the cap: Name = %q (%d chars) should be the hashed form", got, len(got))
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "gonk-triage-p1-i1-a1", "0", strings.Repeat("a", runtime.MaxNameLen)} {
		if err := runtime.ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a.b", "a b", strings.Repeat("a", runtime.MaxNameLen+1)} {
		if err := runtime.ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", bad)
		}
	}
}

// A pipeline session is named for the pipeline it diagnoses (gonk-tios D6):
// the plan's own example, and a realistic instance-global pipeline id with a
// two-digit attempt, both in the plain (unhashed) form.
func TestNamePipelineForm(t *testing.T) {
	if got := runtime.Name("pipeline", 84, runtime.KindPipeline, 3200, 1); got != "gonk-pipeline-p84-pl3200-a1" {
		t.Errorf("Name = %q, want gonk-pipeline-p84-pl3200-a1", got)
	}
	got := runtime.Name("pipeline", 9999999, runtime.KindPipeline, 99999999, 99)
	if got != "gonk-pipeline-p9999999-pl99999999-a99" {
		t.Errorf("Name = %q, want the plain form", got)
	}
	if err := runtime.ValidName(got); err != nil {
		t.Error(err)
	}
}
