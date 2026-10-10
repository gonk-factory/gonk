package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Secret-SHAPED fixtures, assembled at run time.
//
// Every value below is fake by construction (alphabet runs, a truncated key
// body); none was ever valid anywhere. They are split across string
// concatenations so that no committed LINE holds one whole: the public
// mirror's `secrets` workflow runs gitleaks over the full history and fails on
// any finding, and a scanner cannot tell a redactor's test input from a leak.
// The compiler joins the pieces, so the tests see exactly the bytes they saw
// when these were literals -- the split changes what a scanner reads, not what
// Redact is asked to redact.
//
// When adding a case: put the whole secret-shaped value HERE, split inside its
// recognisable prefix, and reference the constant. docs/releases/
// PRE-PUBLISH-CHECKLIST.md has the scan that proves a new one did not slip in.
const (
	fxGLPAT          = "glp" + "at-" + "AbCdEfGhIjKlMnOpQrSt" + "1234"
	fxGLCBT          = "glc" + "bt-" + "64_AbCdEfGhIjKlMnOpQrSt12"
	fxGLRT           = "gl" + "rt-" + "AbCdEfGhIjKlMnOpQrSt12"
	fxGLRegistration = "GR13" + "48941" + "AbCdEfGhIjKlMnOpQrSt"
	fxAKIA           = "AK" + "IA" + "ABCDEFGHIJKLMNOP"
	fxASIA           = "AS" + "IA" + "ABCDEFGHIJKLMNOP"
	fxSK             = "s" + "k-" + "Abc123Def456Ghi789Jkl0"
	fxSKAnt          = "s" + "k-ant-" + "api03-Abc123Def456Ghi789_Jkl0"
	fxSKProj         = "s" + "k-proj-" + "Abc123Def456Ghi789Jkl0"
	fxUserinfo       = "deployer:" + "s3cr3t" + "Passw0rd"

	fxBeginRSA     = "-----BEGIN RSA PRIVATE" + " KEY-----"
	fxBeginOpenSSH = "-----BEGIN OPENSSH PRIVATE" + " KEY-----"
	fxBeginPKCS8   = "-----BEGIN PRIVATE" + " KEY-----"
)

// logFixtureDigest pins the EXPANDED fixture. The log used to be committed
// whole as testdata/secrets.log; this is that file's digest, so the template
// plus the constants above must reproduce it byte for byte. A placeholder left
// unexpanded, or a constant edited without its test being reconsidered, fails
// here rather than quietly handing Redact an easier input.
const logFixtureDigest = "83b200fc2e99a1c3d47d2a52fa2749813bbe7a245fe4aae907eb034d2b6c9c46"

// secretsLog returns the job-log fixture: testdata/secrets.log.tmpl with its
// @NAME@ placeholders replaced by the constants above.
func secretsLog(t *testing.T) []byte {
	t.Helper()
	tmpl, err := os.ReadFile("testdata/secrets.log.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	out := strings.NewReplacer(
		"@GLPAT@", fxGLPAT,
		"@AKIA@", fxAKIA,
		"@SKPROJ@", fxSKProj,
		"@BEGIN_RSA@", fxBeginRSA,
		"@USERINFO@", fxUserinfo,
	).Replace(string(tmpl))
	sum := sha256.Sum256([]byte(out))
	if got := hex.EncodeToString(sum[:]); got != logFixtureDigest {
		t.Fatalf("expanded testdata/secrets.log.tmpl has sha256 %s, want %s:\n%s", got, logFixtureDigest, out)
	}
	return []byte(out)
}
