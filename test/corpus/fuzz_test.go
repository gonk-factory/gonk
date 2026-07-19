package corpus_test

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/test/corpus"
)

// FuzzGonkYMLNeverPanics is the net under the whole class of bug that Plan 01
// shipped. The corpus is the seed set; the fuzzer finds the ones we did not
// think of. Load may return ANY error for any input; it may never panic and it
// may never hang.
//
//	go test ./test/corpus/ -run '^$' -fuzz FuzzGonkYMLNeverPanics -fuzztime 60s
//
// 60s in the standing gate is enough to catch a regression; a long soak is a
// deliberate act, not a merge gate.
func FuzzGonkYMLNeverPanics(f *testing.F) {
	for _, c := range corpus.GonkYML(f) {
		f.Add(c.Bytes)
	}
	f.Add([]byte("version: 1\nenabled: true\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			// The size cap is enforced UPSTREAM, in glab.GetRawFile (Plan 02's
			// GetRawFile size cap fires before the parse). gonkcfg.Load is never
			// asked to parse more than that, so neither is the fuzzer.
			t.Skip()
		}
		_, _ = gonkcfg.Load(b)
	})
}
