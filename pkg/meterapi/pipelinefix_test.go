package meterapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
)

// gonk-2usw part CONFIG: the folded pipeline-fix configuration crosses the
// wire field for field. Every value here differs from its default AND from its
// zero, so a field EffectiveFrom forgot shows up as a mismatch.
func TestEffectiveFromCarriesPipelineFix(t *testing.T) {
	in := gonkcfg.Effective{
		Actions: gonkcfg.Actions{Pipelines: true, PipelineFixes: true},
		PipelineFix: gonkcfg.EffectivePipelineFix{
			Classes: []string{"test", "configuration"}, Targets: []string{"ref", "mr"},
			AllowCI: true, CIExecChanges: "deny", MaxAttempts: 3, MaxChain: 7, VerifyTimeout: 90 * time.Minute,
			MaxFiles: 2, MaxChangedLines: 25, Paths: []string{"internal/**"},
			AutoMerge: gonkcfg.EffectiveAutoMerge{Mode: "after-human-approval", Classes: []string{"docs", "ci"}},
		},
	}
	got := EffectiveFrom(in)
	want := PipelineFix{
		Classes: []string{"test", "configuration"}, Targets: []string{"ref", "mr"},
		AllowCI: true, CIExecChanges: "deny", MaxAttempts: 3, MaxChain: 7, VerifyTimeoutSeconds: 5400,
		MaxFiles: 2, MaxChangedLines: 25, Paths: []string{"internal/**"},
		AutoMerge: AutoMerge{Mode: "after-human-approval", Classes: []string{"docs", "ci"}},
	}
	if !reflect.DeepEqual(got.PipelineFix, want) {
		t.Errorf("PipelineFix = %+v\nwant          %+v", got.PipelineFix, want)
	}
	if !got.Actions.PipelineFixes || !got.Actions.Pipelines {
		t.Errorf("Actions = %+v, want pipelines and pipeline_fixes true", got.Actions)
	}
	// The wire copy does not alias the resolved config.
	got.PipelineFix.Classes[0] = "code"
	if in.PipelineFix.Classes[0] != "test" {
		t.Error("EffectiveFrom aliased the resolved Classes slice")
	}
}

// The action is OFF on the wire unless resolved on, and independent of the
// neighbouring actions.
func TestEffectiveFromPipelineFixesIsItsOwnAction(t *testing.T) {
	for _, c := range []struct {
		in   gonkcfg.Actions
		want bool
	}{
		{gonkcfg.Actions{Triage: true, Pipelines: true, Features: true, Scaffold: true}, false},
		{gonkcfg.Actions{Pipelines: true, PipelineFixes: true}, true},
		{gonkcfg.Actions{}, false},
	} {
		if got := EffectiveFrom(gonkcfg.Effective{Actions: c.in}).Actions.PipelineFixes; got != c.want {
			t.Errorf("%+v: wire pipeline_fixes = %v, want %v", c.in, got, c.want)
		}
	}
}

// The literal key names, and "none" as [] rather than null: a client reading
// `classes: null` cannot tell "nothing allowed" from "field missing".
func TestPipelineFixWireLiterals(t *testing.T) {
	raw, err := json.Marshal(EffectiveFrom(gonkcfg.Effective{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"pipeline_fixes":false`,
		`"pipeline_fix":{"classes":[],"targets":[],"allow_ci":false,"ci_exec_changes":"","max_attempts":0,"max_chain":0,` +
			`"verify_timeout_seconds":0,"max_files":0,"max_changed_lines":0,"paths":[],"auto_merge":{"mode":"","classes":[],"max_per_day":0}}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("wire JSON lacks %s\ngot %s", want, raw)
		}
	}
	// A response from a meter that predates the fields decodes to "nothing
	// allowed", not to the defaults.
	var old Effective
	if err := json.Unmarshal([]byte(`{"enabled":true,"actions":{"triage":true,"pipelines":true}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Actions.PipelineFixes || len(old.PipelineFix.Classes) != 0 || len(old.PipelineFix.Targets) != 0 ||
		old.PipelineFix.MaxAttempts != 0 || old.PipelineFix.AllowCI || old.PipelineFix.AutoMerge.Mode != "" {
		t.Errorf("an older meter's response decoded to something permissive: %+v", old)
	}
}
