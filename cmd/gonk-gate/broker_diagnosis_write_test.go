package main

import (
	"context"
	"testing"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
)

// A diagnosis can never be applied as a write (gonk-tios P5). Its files are
// citations the protected-path denylist deliberately does not check
// (effects.citationOnlyPathFields), so the claim that holds the line is that
// the broker's ONE file-write path -- applyScaffoldFiles -- commits nothing for
// a diagnosis, even one citing the CI definition, and only the `file` effect
// beside it when both are present.
func TestScaffoldApplyNeverWritesADiagnosis(t *testing.T) {
	diag := effects.Effect{
		Kind: effects.KindDiagnosis, Classification: effects.ClassConfiguration,
		Summary: "s", Remedy: "r", Files: []string{".gitlab-ci.yml", "src/a.go"},
		Evidence: []effects.EvidenceItem{{Fact: effects.FactPipelineError}},
	}
	rec := beadstore.Record{ProjectID: 1, IssueIID: 2}

	forge := newNoticeForge()
	d := sweepDeps{GL: forge, Apply: forge}
	url, err := applyScaffoldFiles(context.Background(), d, rec, effects.Batch{Effects: []effects.Effect{diag}}, "<!-- m -->")
	if err != nil || url != "" {
		t.Fatalf("applyScaffoldFiles(diagnosis only) = %q, %v; want nothing applied", url, err)
	}
	if n := len(forge.commits); n != 0 {
		t.Fatalf("a diagnosis produced %d commit(s): %+v", n, forge.commits)
	}

	forge = newNoticeForge()
	d = sweepDeps{GL: forge, Apply: forge}
	file := effects.Effect{Kind: effects.KindFile, Path: ".agent/README.md", Content: "hi"}
	if _, err := applyScaffoldFiles(context.Background(), d, rec, effects.Batch{Effects: []effects.Effect{diag, file}}, "<!-- m -->"); err != nil {
		t.Fatal(err)
	}
	if len(forge.commits) != 1 {
		t.Fatalf("commits = %d, want 1", len(forge.commits))
	}
	acts := forge.commits[0].Actions
	if len(acts) != 1 || acts[0].FilePath != ".agent/README.md" {
		t.Fatalf("commit actions = %+v, want only the file effect", acts)
	}
}
