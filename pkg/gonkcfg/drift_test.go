package gonkcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
