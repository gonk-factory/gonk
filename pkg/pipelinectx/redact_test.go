package pipelinectx

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactPatterns(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		kinds []string
	}{
		{"gitlab PAT", "tok " + fxGLPAT + " x", "tok [REDACTED:gitlab_token] x", []string{KindGitLabToken}},
		{"gitlab CI job token prefix", fxGLCBT, "[REDACTED:gitlab_token]", []string{KindGitLabToken}},
		{"gitlab runner token prefix", fxGLRT, "[REDACTED:gitlab_token]", []string{KindGitLabToken}},
		{"gitlab legacy runner registration", fxGLRegistration, "[REDACTED:gitlab_token]", []string{KindGitLabToken}},
		{"PEM private key block", "a\n" + fxBeginRSA + "\nMIIBOgIBAAJ\nKUpRKfF==\n-----END RSA PRIVATE KEY-----\nb", "a\n[REDACTED:private_key]\nb", []string{KindPrivateKey}},
		{"PEM openssh key", fxBeginOpenSSH + "\nb3Blbg==\n-----END OPENSSH PRIVATE KEY-----", "[REDACTED:private_key]", []string{KindPrivateKey}},
		{"PEM unterminated redacts to end", "x\n" + fxBeginPKCS8 + "\nMIIE\nmore", "x\n[REDACTED:private_key]", []string{KindPrivateKey}},
		{"AWS AKIA", "id " + fxAKIA + ".", "id [REDACTED:aws_access_key].", []string{KindAWSAccessKey}},
		{"AWS ASIA", fxASIA, "[REDACTED:aws_access_key]", []string{KindAWSAccessKey}},
		{"sk- key plain", fxSK, "[REDACTED:api_key]", []string{KindAPIKey}},
		{"sk- key with dashes", "key " + fxSKAnt + " end", "key [REDACTED:api_key] end", []string{KindAPIKey}},
		{"JWT", "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl end", "jwt [REDACTED:jwt] end", []string{KindJWT}},
		{"URL userinfo", "https://deployer:s3cr3t@git.example.test/a.git", "https://[REDACTED:url_userinfo]@git.example.test/a.git", []string{KindURLUserinfo}},
		{"URL userinfo other scheme", "redis://u:p4ss@db.example.test:6379", "redis://[REDACTED:url_userinfo]@db.example.test:6379", []string{KindURLUserinfo}},
		{"password=", "password=hunter2 next", "password=[REDACTED:credential] next", []string{KindCredentialPair}},
		{"passwd:", "passwd: hunter2", "passwd: [REDACTED:credential]", []string{KindCredentialPair}},
		{"secret =", "secret = abc", "secret = [REDACTED:credential]", []string{KindCredentialPair}},
		{"token:", "token: abcdef", "token: [REDACTED:credential]", []string{KindCredentialPair}},
		{"x-api-key header", "X-Api-Key: abc123", "X-Api-Key: [REDACTED:credential]", []string{KindCredentialPair}},
		{"private-token header", "PRIVATE-TOKEN: abc123", "PRIVATE-TOKEN: [REDACTED:credential]", []string{KindCredentialPair}},
		{"authorization bearer: the token, not the scheme", "Authorization: Bearer abcdef0123", "Authorization: Bearer [REDACTED:credential]", []string{KindCredentialPair}},
		{"authorization basic", "authorization: Basic dXNlcjpwYXNz", "authorization: Basic [REDACTED:credential]", []string{KindCredentialPair}},
		{"prefixed env var", "CI_JOB_TOKEN=zzzz DB_PASSWORD=yyy", "CI_JOB_TOKEN=[REDACTED:credential] DB_PASSWORD=[REDACTED:credential]", []string{KindCredentialPair}},
		{"json quoted key and value", `{"password": "two words"}`, `{"password": [REDACTED:credential]}`, []string{KindCredentialPair}},
		{"specific kind wins over credential", "PRIVATE-TOKEN: " + fxGLPAT, "PRIVATE-TOKEN: [REDACTED:gitlab_token]", []string{KindGitLabToken}},
		{"several kinds sorted", fxAKIA + " password=x", "[REDACTED:aws_access_key] password=[REDACTED:credential]", []string{KindAWSAccessKey, KindCredentialPair}},

		// Look-alikes that are NOT secrets and must be kept byte for byte.
		{"keep: word ending in sk-", "risk-assessment-for-the-next-quarter", "risk-assessment-for-the-next-quarter", nil},
		{"keep: short sk-", "sk-short", "sk-short", nil},
		{"keep: glpat too short", "glpat-abc", "glpat-abc", nil},
		{"keep: AKIA too short", "AKIA1234", "AKIA1234", nil},
		{"keep: tokens count", "Tokens: 1234 processed", "Tokens: 1234 processed", nil},
		{"keep: secret_name", "secret_name refers to a path", "secret_name refers to a path", nil},
		{"keep: url without password", "https://example.test/path@v1.2 and ssh://git@host.test/x", "https://example.test/path@v1.2 and ssh://git@host.test/x", nil},
		{"keep: eyJ without three parts", "eyJhbGciOiJIUzI1NiJ9 only", "eyJhbGciOiJIUzI1NiJ9 only", nil},
		{"keep: public key", "-----BEGIN PUBLIC KEY-----\nMFww\n-----END PUBLIC KEY-----", "-----BEGIN PUBLIC KEY-----\nMFww\n-----END PUBLIC KEY-----", nil},
		{"keep: GitLab masked value", "password=[MASKED]", "password=[MASKED]", nil},
		{"keep: our own placeholder", "password=[REDACTED:credential]", "password=[REDACTED:credential]", nil},
		{"keep: quoted placeholder", `{"token": "[REDACTED:credential]"}`, `{"token": "[REDACTED:credential]"}`, nil},
		// R7 (verify report): a value that merely CONTAINS the placeholder
		// prefix is not a placeholder. It used to be waved through, so a secret
		// pre-wrapped as "[REDACTED:<secret>" passed would_post_secret.
		{"R7: pre-wrapped secret", "password=[REDACTED:hunter2hunter2", "password=[REDACTED:credential]", []string{KindCredentialPair}},
		{"R7: placeholder then secret", "password=[REDACTED:credential]hunter2", "password=[REDACTED:credential]", []string{KindCredentialPair}},
		{"R7: unknown kind", "token: [REDACTED:hunter2hunter2]", "token: [REDACTED:credential]", []string{KindCredentialPair}},
		{"R7: url userinfo pre-wrapped", "https://u:[REDACTED:p4ss@host.test/x", "https://[REDACTED:url_userinfo]@host.test/x", []string{KindURLUserinfo}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, kinds := Redact(c.in)
			if got != c.want {
				t.Fatalf("Redact(%q)\n got  %q\n want %q", c.in, got, c.want)
			}
			if !reflect.DeepEqual(kinds, c.kinds) {
				t.Fatalf("kinds %v, want %v", kinds, c.kinds)
			}
		})
	}
}

// TestRedactIdempotent: the landing comment quotes ALREADY-redacted excerpt
// lines, and is gated on Redact finding nothing (plan D8 step 8). If a
// placeholder re-triggered a rule, every honest comment would be refused.
func TestRedactIdempotent(t *testing.T) {
	raw := secretsLog(t)
	once, kinds := Redact(string(raw))
	if len(kinds) == 0 {
		t.Fatal("fixture redacted nothing")
	}
	twice, kinds2 := Redact(once)
	if twice != once || kinds2 != nil {
		t.Fatalf("second pass changed text or found %v:\n%s", kinds2, twice)
	}
}

func TestRedactFixture(t *testing.T) {
	raw := secretsLog(t)
	got, kinds := Redact(string(raw))
	for _, secret := range []string{
		"s3cr3tPassw0rd", "glpat-AbCd", fxAKIA, "sk-proj-Abc",
		"eyJhbGciOiJIUzI1NiJ9.", "abcdef0123456789", "zzzzzzzzzzzzzzzz",
		"MIIBOgIBAAJ", "KUpRKfFLfRYC",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived:\n%s", secret, got)
		}
	}
	for _, keep := range []string{
		"risk-assessment-for-the-next-quarter-is-fine", "Tokens: 1234 processed",
		"secret_name refers to a vault path", "https://example.test/path@v1.2",
		"password=[MASKED]",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("look-alike %q was not kept:\n%s", keep, got)
		}
	}
	want := []string{KindAPIKey, KindAWSAccessKey, KindCredentialPair, KindGitLabToken, KindJWT, KindPrivateKey, KindURLUserinfo}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
}

// TestRedactHintsAreNecessary: a hint is a speed-only prefilter. If a hint
// were ever false for a line its rule matches, that secret would pass
// unredacted, so check it over every input this file's tests use.
func TestRedactHintsAreNecessary(t *testing.T) {
	raw := secretsLog(t)
	inputs := strings.Split(string(raw), "\n")
	inputs = append(inputs,
		"Authorization: Bearer abc", "AUTHORIZATION: x", "Private_Token=x", "X-API-KEY: x",
		"Api-Key: x", "PASSWD=x", "Secret: x", "TOKEN=x", fxGLPAT,
		fxGLRegistration, fxASIA, fxSK,
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl", "redis://u:p@cache.example.internal",
	)
	for _, rule := range append([]redactRule{pemRule}, redactRules...) {
		matched := 0
		for _, in := range inputs {
			if rule.re.MatchString(in) {
				matched++
				if !rule.hint(in) {
					t.Errorf("%s: hint false for matching %q", rule.kind, in)
				}
			}
		}
		if matched == 0 && rule.kind != KindPrivateKey {
			t.Errorf("%s: no input exercised it", rule.kind)
		}
	}
	// The PEM hint over a whole block.
	if !pemRule.hint("-----BEGIN EC " + "PRIVATE KEY-----\nx\n") {
		t.Error("pem hint")
	}
}

// Line-wise rules must not reach across a line: a line ending in "token:"
// does not redact the next line.
func TestRedactDoesNotSpanLines(t *testing.T) {
	in := "Fetching token:\ndone fetching"
	if got, kinds := Redact(in); got != in || kinds != nil {
		t.Fatalf("got %q %v", got, kinds)
	}
}
