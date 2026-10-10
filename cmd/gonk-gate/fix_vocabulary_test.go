package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/glab/glabtest"
)

// The pipeline-fix vocabulary (gonk-2usw plan 4.1: kinds `edit`, `fix_meta`,
// `scope_request`; verdicts `fix`, `needs-scope`) is in the parser's closed
// sets for every agent, and usable by NO agent that exists today. These tests
// observe that at the three apply chains themselves, with the real pack's
// shapes: a triage, scaffold or pipeline batch carrying any of it is refused
// at the shape gate and writes nothing. (pkg/effects holds the same claim
// against every shape file in the pack; this is the broker's side of it.)
//
// All content is invented.

func fixVocabularyVariants(valid effects.Batch) map[string]effects.Batch {
	edit := effects.Effect{Kind: effects.KindEdit, Path: "src/example.go", Old: "a - b", New: "a + b"}
	meta := effects.Effect{Kind: effects.KindFixMeta, Reason: "An invented explanation."}
	request := effects.Effect{Kind: effects.KindScopeRequest, Paths: []string{"go.mod"}, Reason: "An invented reason."}
	with := func(v effects.Verdict, extra ...effects.Effect) effects.Batch {
		b := effects.Batch{Verdict: valid.Verdict, Effects: append(append([]effects.Effect(nil), valid.Effects...), extra...)}
		if v != "" {
			b.Verdict = v
		}
		return b
	}
	return map[string]effects.Batch{
		"an edit rides along":                with("", edit),
		"a fix_meta rides along":             with("", meta),
		"a scope_request rides along":        with("", request),
		"a whole fix rides along":            with(effects.VerdictFix, edit, meta),
		"a whole needs-scope rides along":    with(effects.VerdictNeedsScope, request),
		"verdict fix on its own batch":       with(effects.VerdictFix),
		"verdict needs-scope, its own batch": with(effects.VerdictNeedsScope),
		"a fix batch and nothing else":       {Verdict: effects.VerdictFix, Effects: []effects.Effect{edit, meta}},
		"a needs-scope batch, nothing else":  {Verdict: effects.VerdictNeedsScope, Effects: []effects.Effect{request}},
	}
}

func fencedBatch(t *testing.T, b effects.Batch) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	// The wire form must be one the parser ACCEPTS, or the refusal below would
	// be a parse failure and prove nothing about the shape gate.
	if _, err := effects.ParseBatch(raw); err != nil {
		t.Fatalf("the variant does not parse (%v): %s", err, raw)
	}
	return "thinking...\nGONK_BATCH_START\n" + string(raw) + "\nGONK_BATCH_END\n"
}

func TestTriageBatchCarryingTheFixVocabularyIsRefused(t *testing.T) {
	valid := effects.Batch{Effects: []effects.Effect{
		{Kind: effects.KindComment, Body: "An invented reply."},
		{Kind: effects.KindLabel, Add: []string{"gonk::bug"}},
	}}
	run := func(b effects.Batch) (bool, string, *recordingApplier, error) {
		gl := glabtest.New(t)
		gl.Me = glab.User{ID: 1, Username: "gonk"}
		p := gl.AddProject("group/repo", glab.AccessMaintainer)
		gl.AddIssue(p.ID, 3, "opened")
		rec := brokerRunningRecord(p.ID)
		rt := newTestRuntime()
		rt.finishSession(t, rec.SessionID, fencedBatch(t, b))
		applier := &recordingApplier{}
		d := sweepDeps{
			Runtime: rt, Orders: &fakeOrders{}, GL: gl.Client(), Apply: applier,
			Store: beadstore.NewMemory(), BotUsername: "gonk", PackDir: repoPackDir,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		applied, violation, err := applyBrokerBatch(context.Background(), d, "triage", &rec, finishedStatus())
		return applied, violation, applier, err
	}
	// Control: the same batch without the fix vocabulary is applied.
	if applied, violation, ap, err := run(valid); !applied || violation != "" || err != nil || len(ap.notes) == 0 {
		t.Fatalf("control: applied=%v violation=%q err=%v notes=%d; the refusals below would prove nothing", applied, violation, err, len(ap.notes))
	}
	for name, b := range fixVocabularyVariants(valid) {
		applied, violation, ap, err := run(b)
		if err != nil || applied || !strings.HasPrefix(violation, "shape: ") {
			t.Errorf("%s: applied=%v violation=%q err=%v, want a shape refusal", name, applied, violation, err)
		}
		if len(ap.batchNotes()) != 0 || len(ap.labels) != 0 || len(ap.commits) != 0 || len(ap.mrs) != 0 || len(ap.closed) != 0 {
			t.Errorf("%s: a refused batch wrote: notes=%d labels=%d commits=%d mrs=%d closed=%d",
				name, len(ap.batchNotes()), len(ap.labels), len(ap.commits), len(ap.mrs), len(ap.closed))
		}
	}
}

func TestScaffoldBatchCarryingTheFixVocabularyIsRefused(t *testing.T) {
	valid := effects.Batch{Effects: []effects.Effect{{Kind: effects.KindFile, Path: ".agent/README.md", Content: "invented"}}}
	run := func(b effects.Batch) (bool, string, *recordingApplier, error) {
		applier := &recordingApplier{}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := effects.ParseBatch(raw); err != nil {
			t.Fatalf("the variant does not parse (%v): %s", err, raw)
		}
		d, rec := scaffoldSweepFixture(t, applier, string(raw))
		applied, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", &rec, finishedStatus())
		return applied, violation, applier, err
	}
	if applied, violation, ap, err := run(valid); !applied || violation != "" || err != nil || len(ap.commits) != 1 {
		t.Fatalf("control: applied=%v violation=%q err=%v commits=%d; the refusals below would prove nothing", applied, violation, err, len(ap.commits))
	}
	for name, b := range fixVocabularyVariants(valid) {
		applied, violation, ap, err := run(b)
		if err != nil || applied || !strings.HasPrefix(violation, "shape: ") {
			t.Errorf("%s: applied=%v violation=%q err=%v, want a shape refusal", name, applied, violation, err)
		}
		if len(ap.commits) != 0 || len(ap.mrs) != 0 || len(ap.batchNotes()) != 0 || len(ap.labels) != 0 {
			t.Errorf("%s: a refused batch wrote: commits=%d mrs=%d notes=%d labels=%d",
				name, len(ap.commits), len(ap.mrs), len(ap.batchNotes()), len(ap.labels))
		}
	}
}

func TestPipelineBatchCarryingTheFixVocabularyIsRefused(t *testing.T) {
	// Control: the plain diagnosis lands.
	if applied, violation, err := newPLRun(t, plOpts{}).apply(t, plDiagnosis(nil), io.Discard); !applied || violation != "" || err != nil {
		t.Fatalf("control: applied=%v violation=%q err=%v; the refusals below would prove nothing", applied, violation, err)
	}
	for name, b := range fixVocabularyVariants(plDiagnosis(nil)) {
		t.Run(name, func(t *testing.T) {
			// D8 step 2 is the shape gate; requireRefused also asserts zero
			// GitLab writes.
			requireRefused(t, newPLRun(t, plOpts{}), b, "2", causeShape)
		})
	}
}
