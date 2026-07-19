package corpus_test

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/test/corpus"
)

// TestEveryHostileConfigIsRejectedAndNothingPanics is the security contract.
//
// gonkcfg.Load is the only parser that may ever see these bytes (Plan 02's
// threat model). Every Reject entry must be REFUSED, and refusal must be an
// ERROR -- never a panic, never a crash, never a fallback to a permissive
// default. If a future change routes .gonk.yml through anything else, or
// regresses the finite guard, this test is where we find out.
func TestEveryHostileConfigIsRejectedAndNothingPanics(t *testing.T) {
	for _, c := range corpus.GonkYML(t) {
		if c.Disposition != corpus.Reject {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on hostile input %s: %v\n%s", c.Name, r, debug.Stack())
				}
			}()
			cfg, err := gonkcfg.Load(c.Bytes)
			if err == nil {
				t.Fatalf("hostile input ACCEPTED: %s -> %+v", c.Name, cfg)
			}
			// The error is echoed into a GitLab MR/issue comment (Plan 02's 422
			// path), so it must be a sane string, not a megabyte of YAML.
			if len(err.Error()) > 4096 {
				t.Fatalf("%s: error is %d bytes; it gets posted to GitLab", c.Name, len(err.Error()))
			}
			if c.WantErrContains != "" && !strings.Contains(err.Error(), c.WantErrContains) {
				t.Fatalf("%s: rejection message %q does not contain %q", c.Name, err.Error(), c.WantErrContains)
			}
		})
	}
}

// TestLoaderAcceptsWhatItCannotJudge documents the seam, so the corpus does not
// OVERCLAIM. Ladder ORDER, timezone VALIDITY, a BOM and CRLF are not gonkcfg's
// to reject at schema-validation time; the loader accepts them and the named
// downstream layer stops the ones that need stopping. Asserting the acceptance
// keeps us honest: if a future schema change starts rejecting one of these, the
// change is deliberate and this test makes us re-decide it on purpose.
func TestLoaderAcceptsWhatItCannotJudge(t *testing.T) {
	for _, c := range corpus.GonkYML(t) {
		if c.Disposition != corpus.AcceptByLoader {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on input %s: %v\n%s", c.Name, r, debug.Stack())
				}
			}()
			if _, err := gonkcfg.Load(c.Bytes); err != nil {
				t.Fatalf("%s: gonkcfg rejected an input it does not own (%s); rejection belongs to: %s",
					c.Name, err, c.DownstreamRejector)
			}
		})
	}
}

// TestHostileConfigsDoNotExhaustMemory: a YAML bomb must not be able to OOM the
// process that enforces every project's budget. Runs the WHOLE corpus (reject
// and accept alike) under one heap measurement.
func TestHostileConfigsDoNotExhaustMemory(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, c := range corpus.GonkYML(t) {
		_, _ = gonkcfg.Load(c.Bytes)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grew := after.HeapAlloc - min(after.HeapAlloc, before.HeapAlloc); grew > 256<<20 {
		t.Fatalf("hostile corpus grew the heap by %d MiB", grew>>20)
	}
}
