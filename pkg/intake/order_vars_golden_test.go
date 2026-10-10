package intake

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/gonk-factory/gonk/pkg/atags"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// gonk-tios D12: the pipeline fields on OrderRequest are ADDITIVE. An issue
// order's vars must stay byte-identical to what they were before them, or every
// gonk-gate already deployed would see a changed order for the same work.
//
// testdata/order-vars-issue.golden.json was generated from the code BEFORE the
// pipeline fields existed (at feat/gonk-tios-integration 0e7486a, by a
// one-off write that is deliberately not kept). Never regenerate it to make this test pass.
func issueOrderForGolden() OrderRequest {
	return OrderRequest{
		Trigger: atags.TriggerIssueTriage, Project: "group/repo", ProjectID: 42, Rig: "group-repo",
		IssueIID: 3, SessionKey: SessionKey(42, 3), BeadAnchor: BeadAnchor(42, 3), BeadID: BeadAnchor(42, 3),
		ConfigHash: "sha256:abc", Rung: "local-1", Model: "qwen-local",
		MetadataJSON:  json.RawMessage(`{"gonk_attempt":"1"}`),
		KeyRef:        meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		ReservationID: "res-1",
	}
}

func TestIssueOrderVarsUnchanged(t *testing.T) {
	vars, err := orderVars(issueOrderForGolden())
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(vars, "", "  ") // map keys are sorted: stable bytes
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/order-vars-issue.golden.json"
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("issue order vars changed:\n got: %s\nwant: %s", got, want)
	}
}
