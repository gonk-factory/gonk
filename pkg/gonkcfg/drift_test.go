package gonkcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The embedded schema is canonical. The docs/ copy is the published
// artifact, and the recorded checksum is the breaking-change gate
// (spec 10.1): changing the schema without consciously updating the
// checksum (and bumping the version on breaking changes) fails CI.

func TestPublishedSchemaMatchesCanonical(t *testing.T) {
	published, err := os.ReadFile("../../docs/schemas/gonk-config.v1.schema.json")
	if err != nil {
		t.Fatalf("published schema missing: %v", err)
	}
	if !bytes.Equal(published, schemaJSON) {
		t.Fatal("docs/schemas copy differs from embedded canonical schema; re-run the publish copy step")
	}
}

func TestSchemaChangeIsDeliberate(t *testing.T) {
	want, err := os.ReadFile("testdata/schema.v1.sha256")
	if err != nil {
		t.Fatalf("checksum file missing: %v", err)
	}
	sum := sha256.Sum256(schemaJSON)
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("schema content changed (sha256 %s). If this is intentional: additive change -> update testdata/schema.v1.sha256 and docs copy; breaking change -> new schema version file + version const. See spec 10.1.", got)
	}
}

// TestSchemaVersionConstMatchesEmbeddedSchema is the enforcement the previous
// test's failure message promises but did not deliver: SchemaVersion is a
// plain Go const that nothing checks against the schema it claims to
// describe. Without this test, SchemaVersion can drift from
// properties.version.const (or vice versa) and every other test still
// passes -- silently breaking Plan 03's schema-versioning story.
func TestSchemaVersionConstMatchesEmbeddedSchema(t *testing.T) {
	var doc struct {
		Properties struct {
			Version struct {
				Const int `json:"const"`
			} `json:"version"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &doc); err != nil {
		t.Fatalf("embedded schema unreadable: %v", err)
	}
	if doc.Properties.Version.Const != SchemaVersion {
		t.Fatalf("schema properties.version.const = %d, but SchemaVersion = %d; "+
			"these must be updated together (see spec 10.1)",
			doc.Properties.Version.Const, SchemaVersion)
	}
}
