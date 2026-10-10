package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE FROZEN ONBOARDING FIXTURE WAS EDITED ONCE, IN A COMMENT, AND THIS IS
// WHAT KEEPS THAT HONEST.
//
// testdata/existing/onboarding-at-8037e70.gonk.yml is the .gonk.yml gonk wrote
// into newly onboarded repositories at that commit, and
// TestExistingConfigsResolveByteIdentically holds every later change to
// resolving it exactly as it resolved then. Its first line is a comment that
// carried the project's home URL. When the project's public home changed, that
// one comment line was rewritten here (the old URL named an internal host and
// could not be published).
//
// A comment is not part of the YAML document: the parser discards it before
// the schema or the resolver sees anything. So the edit cannot change what the
// configuration MEANS -- but "cannot" is a claim, and this test is its
// evidence. frozenOnboardingDocument is the sha256 of the fixture's PARSED
// document (as canonical JSON), computed from the unedited file before the
// edit. If anything but a comment or whitespace ever changes in the fixture,
// this fails.
//
// What the comment edit DID change is config_hash, which is by definition the
// sha256 of the file's bytes (configHash, service.go): two lines of
// golden/existing_configs_before.json were changed by hand to the new file's
// hash, and the second test below derives that value independently, so the
// golden line is "the hash of this input" and not a number copied out of a
// test failure. Repositories onboarded before the change keep their bytes and
// therefore their hash.
const frozenOnboardingDocument = "55cda5f7db7728a38c68096f539b95942733764d305a10cb12facb648e26d202"

func frozenOnboardingFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "existing", "onboarding-at-8037e70.gonk.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFrozenOnboardingFixtureIsStillTheDocumentOf8037e70(t *testing.T) {
	var doc any
	if err := yaml.Unmarshal(frozenOnboardingFixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	if m, ok := doc.(map[string]any); !ok || len(m) < 3 {
		t.Fatalf("the fixture parsed to %T with too few keys: %v", doc, doc)
	}
	canon, err := json.Marshal(doc) // map keys are sorted: canonical for this purpose
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	if got := hex.EncodeToString(sum[:]); got != frozenOnboardingDocument {
		t.Fatalf("the frozen onboarding fixture no longer parses to the document of 8037e70.\n"+
			"  parsed-document sha256 %s\n  want                   %s\n"+
			"Only its comments may ever change. A changed key or value makes "+
			"TestExistingConfigsResolveByteIdentically compare against a configuration no project has.\n"+
			"document: %s", got, frozenOnboardingDocument, canon)
	}
}

func TestFrozenGoldenConfigHashIsTheHashOfTheFixture(t *testing.T) {
	sum := sha256.Sum256(frozenOnboardingFixture(t))
	want := `"config_hash": "sha256:` + hex.EncodeToString(sum[:]) + `"`
	golden, err := os.ReadFile(filepath.Join("testdata", "golden", "existing_configs_before.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Once per operator configuration in the corpus (plain, tight).
	if n := strings.Count(string(golden), want); n != 2 {
		t.Fatalf("existing_configs_before.json states %s %d times, want 2 (the onboarding-default case under each operator)", want, n)
	}
}
