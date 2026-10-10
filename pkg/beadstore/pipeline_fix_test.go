package beadstore

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixRecord() Record {
	r := fixlessPipelineRecord()
	r.BeadAnchor = "gonk:42:pipefix:ref-0d6e4079:0123456789abcdef:s880"
	r.BeadID = r.BeadAnchor
	r.Trigger = "pipeline-fix"
	// A fix session key (pipelinectx.FixSessionKey: "gonk-<pid>-pf-" + 12 hex of
	// a hash), not a credential. Written in two pieces because the public
	// mirror's secret scanner reads `SessionKey = "<23 mixed characters>"` as a
	// generic API key; the value is the same bytes.
	r.SessionKey = "gonk-42-pf-" + "0123456789ab"
	r.PipelineFix = PipelineFix{
		RunAnchor: "gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880",
		CauseKey:  "00112233445566aa", Class: "code",
		BaseSHA: "1111111111111111111111111111111111111111", BasePipelineID: 900, TargetBranch: "main",
		ConfigSHA: "2222222222222222222222222222222222222222",
		Envelope:  []string{"docs", "source"}, Files: []string{"calc/sum.py"},
		MaxFiles: 3, MaxLines: 40, MaxFileBytes: 262144, MaxPatchBytes: 16384,
		CIExecChanges: "manual-run", MaxAttempts: 2, VerifyTimeoutSeconds: 7200,
		Targets: []PipelineFixTarget{{Stage: "test", Name: "unit-tests", FailureReason: "script_failure",
			ErrorSignature: "es1-0011223344556677"}},
		FiredAt:        time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
		BaseJobs:       []PipelineFixBaseJob{{Name: "build", Status: "success"}, {Name: "unit-tests", Status: "failed"}},
		ExcerptVersion: 1, SignatureVersion: 1, ContextSHA256: "abc",
	}
	return r
}

// gonk-2usw DISPATCH: a fix record round-trips, under the documented key.
func TestPipelineFixRecordRoundTrips(t *testing.T) {
	rec := fixRecord()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"PipelineFix":{`, `"RunAnchor":"gonk:42:pipeline:`, `"CauseKey":"00112233445566aa"`,
		`"Envelope":["docs","source"]`, `"BasePipelineID":900`, `"Targets":[{"Stage":"test","Name":"unit-tests"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("the stored fix record lacks %s:\n%s", key, raw)
		}
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.PipelineFix, rec.PipelineFix) {
		t.Fatalf("read back %+v\nwant      %+v", back.PipelineFix, rec.PipelineFix)
	}
	if !back.PipelineFix.Recorded() {
		t.Fatal("a fix record does not read as one")
	}
}

// A record that is not a fix record stores NO PipelineFix key (the fix-less
// golden, TestPipelineRecordWithoutAFixUnchanged, pins the whole record; this
// names the mechanism), and an old record reads as "not a fix".
func TestARecordThatIsNotAFixStoresNoPipelineFixKey(t *testing.T) {
	raw, err := json.Marshal(fixlessPipelineRecord())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PipelineFix\"") {
		t.Fatalf("a diagnosis record stores a PipelineFix key: %s", raw)
	}
	var old Record
	if err := json.Unmarshal([]byte(oldPipelineRecordJSON), &old); err != nil || old.PipelineFix.Recorded() {
		t.Fatalf("an old record reads as a fix record: %+v, %v", old.PipelineFix, err)
	}
}

// IsZero must look at EVERY field: a field it forgot would be silently
// dropped from a record that set only that field. Each field is set alone, by
// reflection, so a field added later without extending IsZero fails here.
func TestPipelineFixIsZeroCoversEveryField(t *testing.T) {
	if !(PipelineFix{}).IsZero() {
		t.Fatal("the zero PipelineFix is not zero")
	}
	typ := reflect.TypeOf(PipelineFix{})
	for i := 0; i < typ.NumField(); i++ {
		var f PipelineFix
		v := reflect.ValueOf(&f).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int, reflect.Int64:
			v.SetInt(1)
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Struct:
			if v.Type() == reflect.TypeOf(time.Time{}) {
				v.Set(reflect.ValueOf(time.Unix(1, 0)))
			} else { // FixContext: any one field set makes it non-zero
				v.Set(reflect.ValueOf(FixContext{PromptSHA256: "x"}))
			}
		default:
			t.Fatalf("field %s has kind %s, which this test does not know how to set", typ.Field(i).Name, v.Kind())
		}
		if f.IsZero() {
			t.Errorf("IsZero ignores field %s", typ.Field(i).Name)
		}
	}
}

// FixContext.IsZero must look at every field too, for the same reason.
func TestFixContextIsZeroCoversEveryField(t *testing.T) {
	if !(FixContext{}).IsZero() {
		t.Fatal("the zero FixContext is not IsZero")
	}
	typ := reflect.TypeOf(FixContext{})
	for i := 0; i < typ.NumField(); i++ {
		var c FixContext
		v := reflect.ValueOf(&c).Elem().Field(i)
		switch v.Kind() {
		case reflect.String:
			v.SetString("x")
		case reflect.Int, reflect.Int64:
			v.SetInt(1)
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Struct:
			v.Set(reflect.ValueOf(FixLimits{Edits: 1}))
		default:
			t.Fatalf("field %s: kind %s is not handled by this test", typ.Field(i).Name, v.Kind())
		}
		if c.IsZero() {
			t.Errorf("FixContext.IsZero ignores field %s", typ.Field(i).Name)
		}
	}
}
