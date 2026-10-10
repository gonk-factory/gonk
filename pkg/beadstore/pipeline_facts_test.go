package beadstore

import (
	"encoding/json"
	"strings"
	"testing"
)

// oldPipelineRecordJSON is a pipeline-diagnosis record exactly as a gonk-gate
// built BEFORE gonk-2usw FACTS serialised it: PipelineJobs entries carry five
// fields and there is no PipelineDiagnosis key. Synthetic values. It is a
// string on purpose -- built from today's struct it would prove nothing about
// yesterday's bytes.
const oldPipelineRecordJSON = `{"BeadAnchor":"gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880",` +
	`"BeadID":"gk-1","Project":"example/calc","ProjectID":42,"Rig":"example-calc","SessionKey":"k","Trigger":"pipeline-diagnosis",` +
	`"IssueIID":0,"ConfigHash":"","State":"done","Rung":"r2","Model":"m","Attempt":1,"ReservationID":"rsv-1",` +
	`"ReservationExpiresAt":"0001-01-01T00:00:00Z","RetryAfter":"0001-01-01T00:00:00Z","SessionEndedAt":"0001-01-01T00:00:00Z",` +
	`"SessionID":"gonk-pipeline-p42-pl900-a1","CheckoutSHA":"","UpdatedAt":"2026-10-04T10:00:00Z",` +
	`"PipelineID":900,"PipelineTarget":"ref:main","PipelineFingerprint":"0123456789abcdef","PipelineLastSuccess":880,` +
	`"PipelineSHA":"1111111111111111111111111111111111111111","PipelineProjectURL":"https://gitlab.example/example/calc",` +
	`"PipelineJobs":[{"ID":9002,"Name":"unit-tests","Excerpted":true,"ExcerptSHA256":"aa","Groundable":true},` +
	`{"ID":9003,"Name":"lint","Excerpted":false,"ExcerptSHA256":"","Groundable":false}],` +
	`"PipelineExcerptVersion":2,"PipelineExcerptMaxBytes":6144,"PipelineFirstFailed":900,"PipelineLatestFailed":910,` +
	`"PipelineRunOpen":true,"PipelineCheckedAt":"2026-10-04T10:05:00Z","Revision":"00112233445566778899aabbccddeeff",` +
	`"ReslungAt":"0001-01-01T00:00:00Z","ReslingFireBy":"0001-01-01T00:00:00Z"}`

// A record written before the failure facts existed keeps reading: every old
// field as it was, and the new ones as "not recorded" -- never as a fact
// ("stage is empty", "not generic", "no diagnosis class") that nobody observed.
func TestRecordWrittenBeforeFailureFactsStillReads(t *testing.T) {
	var r Record
	if err := json.Unmarshal([]byte(oldPipelineRecordJSON), &r); err != nil {
		t.Fatalf("an old record no longer decodes: %v", err)
	}
	if r.BeadAnchor != "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880" || r.State != StateDone ||
		r.PipelineID != 900 || r.PipelineLatestFailed != 910 || !r.PipelineRunOpen ||
		r.PipelineExcerptVersion != 2 || r.Revision != "00112233445566778899aabbccddeeff" {
		t.Fatalf("old fields changed on read: %+v", r)
	}
	if len(r.PipelineJobs) != 2 {
		t.Fatalf("PipelineJobs = %+v", r.PipelineJobs)
	}
	j := r.PipelineJobs[0]
	if j.ID != 9002 || j.Name != "unit-tests" || !j.Excerpted || j.ExcerptSHA256 != "aa" || !j.Groundable {
		t.Fatalf("old job fields changed on read: %+v", j)
	}
	for _, j := range r.PipelineJobs {
		if j.FailureFactsRecorded() {
			t.Errorf("job %s of an old record claims recorded failure facts: %+v", j.Name, j)
		}
		if !j.GenericFailure() {
			t.Errorf("job %s of an old record has no signature, so it must read as generic (the conservative side)", j.Name)
		}
	}
	if r.PipelineDiagnosis.Recorded() {
		t.Fatalf("an old record claims a recorded diagnosis: %+v", r.PipelineDiagnosis)
	}

	// Written back (run maintenance rewrites old open runs), it loses nothing
	// and still reads as not recorded.
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var again Record
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if again.PipelineDiagnosis.Recorded() || again.PipelineJobs[0].FailureFactsRecorded() || again.PipelineJobs[0].ExcerptSHA256 != "aa" {
		t.Fatalf("an old record changed across a rewrite: %+v", again)
	}
	// Every key the old binary wrote is still a key this one writes: a
	// rollback's reader finds what it expects.
	var oldKeys, newKeys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(oldPipelineRecordJSON), &oldKeys); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &newKeys); err != nil {
		t.Fatal(err)
	}
	for k := range oldKeys {
		if _, ok := newKeys[k]; !ok {
			t.Errorf("record key %q is no longer written", k)
		}
	}
}

// GenericFailure is "no signature, or the flag": the flag alone would call an
// unsigned job of an old record specific.
func TestPipelineJobGenericFailure(t *testing.T) {
	cases := []struct {
		name string
		job  PipelineJob
		want bool
	}{
		{"signature, not flagged", PipelineJob{ErrorSignature: "es1-0123456789abcdef", SignatureVersion: 1}, false},
		{"signature, flagged (the runner's summary)", PipelineJob{ErrorSignature: "es1-0123456789abcdef", ErrorGeneric: true, SignatureVersion: 1}, true},
		{"no signature, flagged", PipelineJob{ErrorGeneric: true, SignatureVersion: 1}, true},
		{"no signature, flag never written (old record)", PipelineJob{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.job.GenericFailure(); got != c.want {
				t.Fatalf("GenericFailure() = %v, want %v for %+v", got, c.want, c.job)
			}
		})
	}
}

// The new record keys are spelled as documented (docs/reference/
// gonk-state-record.md): the JSON key IS the schema.
func TestPipelineFactsJSONKeys(t *testing.T) {
	r := Record{
		PipelineJobs: []PipelineJob{{Stage: "test", FailureReason: "script_failure", AllowFailure: true,
			ErrorSignature: "es1-0123456789abcdef", ErrorGeneric: true, SignatureVersion: 1}},
		PipelineDiagnosis: PipelineDiagnosis{Class: "code", Files: []string{"a"}, EvidenceJobs: []string{"j"}, Summary: "s", Remedy: "r"},
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Stage":"test"`, `"FailureReason":"script_failure"`, `"AllowFailure":true`,
		`"ErrorSignature":"es1-0123456789abcdef"`, `"ErrorGeneric":true`, `"SignatureVersion":1`,
		`"PipelineDiagnosis":{"Class":"code","Files":["a"],"EvidenceJobs":["j"],"Summary":"s","Remedy":"r"}`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("record JSON lacks %s:\n%s", want, out)
		}
	}
}
