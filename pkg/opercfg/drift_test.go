package opercfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPublishedSchemaMatchesCanonical(t *testing.T) {
	published, err := os.ReadFile("../../docs/schemas/gonk-operator.v1.schema.json")
	if err != nil {
		t.Fatalf("published schema missing: %v", err)
	}
	if !bytes.Equal(published, schemaJSON) {
		t.Fatal("docs/schemas copy differs from the embedded canonical schema; re-run the publish copy step")
	}
}

func TestSchemaChangeIsDeliberate(t *testing.T) {
	want, err := os.ReadFile("testdata/schema.v1.sha256")
	if err != nil {
		t.Fatalf("checksum file missing: %v", err)
	}
	sum := sha256.Sum256(schemaJSON)
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("operator schema changed (sha256 %s). Additive change -> update testdata/schema.v1.sha256 and the docs copy; breaking change -> new version file + SchemaVersion bump. See spec 10.1.", got)
	}
}

// Plan 01's carry-forward for whoever cuts a v2: keep the const and the schema
// in step. Exactly one test enforces it, and this is it.
func TestSchemaVersionConstMatchesEmbeddedSchema(t *testing.T) {
	var s struct {
		Properties struct {
			Version struct {
				Const int `json:"const"`
			} `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties.Version.Const != SchemaVersion {
		t.Fatalf("SchemaVersion = %d but schema says version const = %d", SchemaVersion, s.Properties.Version.Const)
	}
}
