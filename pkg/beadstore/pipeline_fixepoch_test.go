package beadstore

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixlessPipelineRecord() Record {
	return Record{
		BeadAnchor: "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880", BeadID: "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880",
		Project: "group/repo", ProjectID: 42, Rig: "group-repo", SessionKey: "gonk-42-pl-0123456789ab",
		Trigger: "pipeline-diagnosis", State: StateRunning, Rung: "local-1", Attempt: 1,
		PipelineID: 900, PipelineTarget: "ref:main", PipelineFingerprint: "0123456789abcdef", PipelineLastSuccess: 880,
		PipelineSHA: "1111111111111111111111111111111111111111", PipelineRunOpen: true,
	}
}

// gonk-2usw EPOCH: the record of a run that follows no merged fix serialises
// to exactly the bytes it did before PipelineFixEpoch existed.
//
// testdata/record-pipeline-fixless.golden.json was generated from the code
// BEFORE the field existed (origin/main 4da8e68, by a one-off write that is
// deliberately not kept). Never regenerate it to make this test pass.
func TestPipelineRecordWithoutAFixUnchanged(t *testing.T) {
	got, err := json.MarshalIndent(fixlessPipelineRecord(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/record-pipeline-fixless.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("the record of a run with no fix changed:\n got: %s\nwant: %s", got, want)
	}
}

// With a fix the epoch is stored under its own name and reads back; a record
// written before the field existed reads as "follows no fix".
func TestPipelineFixEpochRoundTrips(t *testing.T) {
	rec := fixlessPipelineRecord()
	rec.BeadAnchor += ":f12"
	rec.PipelineFixEpoch = 12
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"PipelineFixEpoch":12`) {
		t.Fatalf("the fix epoch is not stored as PipelineFixEpoch: %s", raw)
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil || back.PipelineFixEpoch != 12 || back.BeadAnchor != rec.BeadAnchor {
		t.Fatalf("read back %+v, %v", back, err)
	}
	var old Record
	if err := json.Unmarshal([]byte(oldPipelineRecordJSON), &old); err != nil || old.PipelineFixEpoch != 0 {
		t.Fatalf("an old record reads fix epoch %d, %v; want 0", old.PipelineFixEpoch, err)
	}
}
