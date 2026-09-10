// Package corpus is the hostile-.gonk.yml corpus: a fixed set of malformed and
// adversarial project configs, each paired with how the SHIPPED loader
// (pkg/gonkcfg.Load) must handle it.
//
// Why this exists. A .gonk.yml is ATTACKER-CONTROLLED: anyone with push access
// to any project the bot is invited to controls those bytes. Plan 01 shipped a
// one-line remote crash -- budget.monthly_cost_usd: .nan nil-panicked the
// validator inside the process that enforces every project's budget and kill
// switch. That specific bug is fixed; the CLASS of bug is not, and nothing in
// the repo keeps it fixed. This corpus is that test. Its assertions live in
// corpus_test.go and fuzz_test.go; the seed inputs live in gonkyml/.
//
// The corpus feeds the REAL loader (gonkcfg.Load), never a copy: if a future
// change routes .gonk.yml through a different parser, or regresses the finite
// guard, these tests are where we find out.
package corpus

import (
	"embed"
	"path"
	"sort"
	"strings"
	"testing"
)

//go:embed gonkyml
var files embed.FS

//go:embed operatoryml
var operatorFiles embed.FS

// Disposition is how gonkcfg.Load must treat an input.
type Disposition int

const (
	// Reject: gonkcfg.Load MUST return a non-nil error. Never a panic, never a
	// crash, never a silent fallback to a permissive default. This is the
	// security contract: a hostile config is refused, and the refusal surfaces
	// as an HTTP 422 carrying gonkcfg.Load's message (meterapi 422-not-400 seam).
	Reject Disposition = iota

	// AcceptByLoader: gonkcfg.Load ACCEPTS the input, because the property under
	// attack is not gonkcfg's to judge. Ladder ORDER (enforce_ladder_order, Plan
	// 03 AD-4) is a meter/rung policy; timezone VALIDITY is resolved at
	// schedule-evaluation time; a leading BOM and CRLF line endings are
	// normalized by the YAML decoder. These entries exist so the corpus does not
	// OVERCLAIM what schema validation covers -- and so the no-panic and
	// bounded-memory guarantees are proven over them too. DownstreamRejector
	// names where the input is actually stopped.
	AcceptByLoader
)

// Entry is one corpus input paired with its expected handling.
type Entry struct {
	Name        string      // file base name without extension
	Bytes       []byte      // the raw input handed to gonkcfg.Load
	Disposition Disposition // Reject | AcceptByLoader

	// WantErrContains, when non-empty on a Reject entry, is a substring the
	// rejection message must contain. It is set only where the message is a
	// stable contract (the finite guard, the token-overflow guard, the YAML
	// decoder's own errors); schema-driven rejections carry the generic
	// jsonschema block and are asserted only as "rejected, and short enough to
	// post to GitLab".
	WantErrContains string

	// DownstreamRejector, on an AcceptByLoader entry, names the layer that
	// actually stops the input (never gonkcfg). Empty on Reject entries.
	DownstreamRejector string
}

// meta is the expected disposition for every committed corpus file, keyed by
// base name. GonkYML fails if a file has no entry here (an unclassified hostile
// input is a hole in the contract) or if an entry names a file that is gone.
var meta = map[string]Entry{
	// The Plan 01 crash and its non-finite siblings: caught by rejectNonFinite
	// before jsonschema's "minimum" comparison can dereference a nil big.Rat.
	"nan-cost":      {Disposition: Reject, WantErrContains: "not a finite number"},
	"inf-cost":      {Disposition: Reject, WantErrContains: "not a finite number"},
	"neg-inf-cost":  {Disposition: Reject, WantErrContains: "not a finite number"},
	"nan-in-nested": {Disposition: Reject, WantErrContains: "not a finite number"},

	// Schema-driven rejections (generic jsonschema message).
	"negative-budget":  {Disposition: Reject},
	"float-tokens":     {Disposition: Reject},
	"ladder-injection": {Disposition: Reject},
	"ladder-dup":       {Disposition: Reject},
	"version-2":        {Disposition: Reject},
	"unknown-key":      {Disposition: Reject},
	"bad-quiet-hours":  {Disposition: Reject},
	// quiet_hours with no timezone: rejected by the schema's dependentRequired.
	// The message is asserted because it must NAME the missing field -- it is
	// echoed straight back to the project author through the 422 path, and
	// "invalid config" would not tell them what to fix.
	"quiet-hours-no-timezone": {Disposition: Reject, WantErrContains: "'timezone' required"},
	"empty":                   {Disposition: Reject},

	// Loader-level rejections with stable, contract messages.
	"huge-tokens":    {Disposition: Reject, WantErrContains: "overflow"},
	"duplicate-keys": {Disposition: Reject, WantErrContains: "already defined"},
	"project-nul":    {Disposition: Reject, WantErrContains: "control characters are not allowed"},
	"not-yaml":       {Disposition: Reject, WantErrContains: "control characters are not allowed"},
	"billion-laughs": {Disposition: Reject, WantErrContains: "excessive aliasing"},

	// Accepted by gonkcfg -- stopped downstream. Not schema's job.
	"ladder-reorder": {Disposition: AcceptByLoader, DownstreamRejector: "meter/rung enforce_ladder_order (Plan 03 AD-4)"},
	"bad-timezone":   {Disposition: AcceptByLoader, DownstreamRejector: "schedule evaluation (tz database lookup), not schema"},
	"bom":            {Disposition: AcceptByLoader, DownstreamRejector: "none: a leading UTF-8 BOM is normalized by the YAML decoder"},
	"crlf":           {Disposition: AcceptByLoader, DownstreamRejector: "none: CRLF line endings are normalized by the YAML decoder"},
}

// GonkYML returns the corpus: every committed file under gonkyml/ paired with
// its expected disposition, plus the generated deep-nesting entry (a 10 000-deep
// document is a recursion-depth attack that is unwieldy to commit as a file).
// It is usable from a *testing.T (corpus_test.go) or a *testing.F
// (fuzz_test.go): both satisfy testing.TB.
func GonkYML(tb testing.TB) []Entry {
	tb.Helper()
	dirents, err := files.ReadDir("gonkyml")
	if err != nil {
		tb.Fatalf("corpus: read gonkyml/: %v", err)
	}
	seen := make(map[string]bool, len(dirents))
	var out []Entry
	for _, de := range dirents {
		if de.IsDir() {
			continue
		}
		name := strings.TrimSuffix(de.Name(), path.Ext(de.Name()))
		m, ok := meta[name]
		if !ok {
			tb.Fatalf("corpus: file %q has no metadata entry; every hostile input must be classified", de.Name())
		}
		b, err := files.ReadFile(path.Join("gonkyml", de.Name()))
		if err != nil {
			tb.Fatalf("corpus: read %s: %v", de.Name(), err)
		}
		seen[name] = true
		out = append(out, Entry{
			Name:               name,
			Bytes:              b,
			Disposition:        m.Disposition,
			WantErrContains:    m.WantErrContains,
			DownstreamRejector: m.DownstreamRejector,
		})
	}
	for name := range meta {
		if !seen[name] {
			tb.Fatalf("corpus: metadata names %q but no gonkyml/%s.yml exists", name, name)
		}
	}

	// deep-nesting: a 10 000-deep flow-mapping. The top-level key "x" is not in
	// the schema, so it is rejected -- but only AFTER the YAML decoder and the
	// recursive rejectNonFinite walk have each descended 10 000 levels. That is
	// the actual attack: stack exhaustion in the decoder/walker. Generated
	// rather than committed because a 40 KB single line is not a reviewable file.
	out = append(out, Entry{
		Name:        "deep-nesting",
		Bytes:       deepNesting(10000),
		Disposition: Reject,
	})

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func deepNesting(depth int) []byte {
	var sb strings.Builder
	sb.WriteString("version: 1\nenabled: true\nx: ")
	sb.WriteString(strings.Repeat("{a: ", depth))
	sb.WriteString("1")
	sb.WriteString(strings.Repeat("}", depth))
	sb.WriteString("\n")
	return []byte(sb.String())
}

// ==================================================================
// Operator config
// ==================================================================

// operatorMeta is the expected disposition of every committed file under
// operatoryml/, keyed by base name. Here Reject/AcceptByLoader are about
// opercfg.Load, not gonkcfg.Load.
//
// The threat model is DIFFERENT from gonkyml/'s and worth stating, because it
// is the reason these entries exist at all. An operator config is not
// attacker-controlled -- it is the operator's own ConfigMap. But it is
// HOT-RELOADED into a running meter, and it is folded onto every project, so a
// single typo has instance-wide blast radius: an operator `quiet_hours` with no
// timezone made every project fail to resolve and DELETED every project's
// LiteLLM virtual key on the next tick (R-20). The corpus's job here is not to
// stop an attacker; it is to keep one operator keystroke from being able to
// unregister a whole instance.
var operatorMeta = map[string]Entry{
	"quiet-hours-no-timezone-instance": {Disposition: Reject, WantErrContains: "timezone"},
	"quiet-hours-no-timezone-group":    {Disposition: Reject, WantErrContains: "timezone"},
	"group-key-trailing-slash":         {Disposition: Reject, WantErrContains: "group key"},

	// The control: an operator config that must LOAD. Without it, an
	// opercfg.Load that returned an error for every input would satisfy every
	// Reject entry above and the corpus would prove nothing.
	"valid": {Disposition: AcceptByLoader, DownstreamRejector: "none: this is a valid operator config and must load"},
}

// OperatorYML returns the operator-config corpus: every committed file under
// operatoryml/ paired with its expected disposition. Same contract as GonkYML
// -- an unclassified file is a hole and fails the test, as does a metadata
// entry naming a file that is gone.
func OperatorYML(tb testing.TB) []Entry {
	tb.Helper()
	dirents, err := operatorFiles.ReadDir("operatoryml")
	if err != nil {
		tb.Fatalf("corpus: read operatoryml/: %v", err)
	}
	seen := make(map[string]bool, len(dirents))
	var out []Entry
	for _, de := range dirents {
		if de.IsDir() {
			continue
		}
		name := strings.TrimSuffix(de.Name(), path.Ext(de.Name()))
		m, ok := operatorMeta[name]
		if !ok {
			tb.Fatalf("corpus: file %q has no metadata entry; every operator-config input must be classified", de.Name())
		}
		b, err := operatorFiles.ReadFile(path.Join("operatoryml", de.Name()))
		if err != nil {
			tb.Fatalf("corpus: read %s: %v", de.Name(), err)
		}
		seen[name] = true
		out = append(out, Entry{
			Name:               name,
			Bytes:              b,
			Disposition:        m.Disposition,
			WantErrContains:    m.WantErrContains,
			DownstreamRejector: m.DownstreamRejector,
		})
	}
	for name := range operatorMeta {
		if !seen[name] {
			tb.Fatalf("corpus: metadata names %q but no operatoryml/%s.yml exists", name, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
