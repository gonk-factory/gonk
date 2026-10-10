package buildgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// NO IMAGE BAKES IN A TRUST SETTING.
//
// A published image must trust the public roots of its base and nothing else:
// no CA file, and no variable that points TLS trust at a path. A variable that
// names a private bundle puts one site's PKI layout into every user's image;
// worse, most of these variables REPLACE the system roots (Go's SSL_CERT_FILE
// and SSL_CERT_DIR, git's GIT_SSL_CAINFO, curl's CURL_CA_BUNDLE, the
// REQUESTS_CA_BUNDLE family), so a user with no private CA at all inherits a
// trust store aimed at a file that is not there.
//
// Trust for a private CA is added at RUN TIME -- the chart mounts the bundle
// and sets SSL_CERT_DIR to the system directory plus the mounted one
// (gonk.caTrustEnv; pinned by internal/catrust and internal/charttest) -- or by
// a user's own image FROM the published one. chart/gonk/README.md, "Trusting a
// private CA", documents both.

// trustVariables are the names that set or replace a TLS trust store.
var trustVariables = []string{
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
	"CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "AWS_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
}

// imageTrustFindings reports each Dockerfile instruction (not a comment) that sets
// one of them, or that installs a certificate into the image.
func imageTrustFindings(dockerfile string) []string {
	var out []string
	setRE := regexp.MustCompile(`\b(` + strings.Join(trustVariables, "|") + `)\b\s*=?`)
	caFileRE := regexp.MustCompile(`(?i)\.(crt|pem|cer)\b|update-ca-certificates|/usr/local/share/ca-certificates`)
	// Join continuation lines so an instruction is judged whole.
	var logical []string
	cur := ""
	for _, line := range strings.Split(dockerfile, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			continue
		}
		cur += " " + trim
		if strings.HasSuffix(trim, `\`) {
			cur = strings.TrimSuffix(cur, `\`)
			continue
		}
		if strings.TrimSpace(cur) != "" {
			logical = append(logical, strings.TrimSpace(cur))
		}
		cur = ""
	}
	for _, ins := range logical {
		word := strings.ToUpper(strings.SplitN(ins, " ", 2)[0])
		switch word {
		case "ENV", "ARG", "RUN", "ENTRYPOINT", "CMD":
			if m := setRE.FindString(ins); m != "" {
				out = append(out, word+" sets a trust variable: "+ins)
			}
		}
		switch word {
		case "COPY", "ADD", "RUN":
			if m := caFileRE.FindString(ins); m != "" {
				out = append(out, word+" puts a certificate into the image ("+m+"): "+ins)
			}
		}
	}
	return out
}

func TestNoImageBakesInATrustSetting(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "images", "Dockerfile.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 4 {
		t.Fatalf("found %d Dockerfiles under images/, want at least the 4 gonk images: looking in the wrong place", len(files))
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range imageTrustFindings(string(body)) {
			t.Errorf("%s: %s\nAn image trusts its base image's public roots and nothing else. A private CA is added "+
				"at run time by the chart (gitlab.caCert) or by extending the image: chart/gonk/README.md, "+
				"\"Trusting a private CA\".", filepath.Base(f), finding)
		}
	}
}

// The detector is seen red on each shape it exists for, and stays quiet on the
// shapes the real Dockerfiles do contain.
func TestImageTrustDetector(t *testing.T) {
	red := map[string]string{
		"env":            "FROM x\nENV SSL_CERT_FILE=/etc/ssl/site/ca.crt\n",
		"env, old form":  "FROM x\nENV SSL_CERT_DIR /etc/ssl/site\n",
		"env, continued": "FROM x\nENV HOME=/h \\\n    GIT_SSL_CAINFO=/etc/ssl/site/ca.pem\n",
		"node":           "FROM x\nENV NODE_EXTRA_CA_CERTS=/ca\n",
		"run export":     "FROM x\nRUN export CURL_CA_BUNDLE=/x && true\n",
		"copy a cert":    "FROM x\nCOPY site-ca.crt /usr/local/share/ca-certificates/\n",
		"rebuild store":  "FROM x\nRUN update-ca-certificates\n",
	}
	for name, df := range red {
		if len(imageTrustFindings(df)) == 0 {
			t.Errorf("%s: not detected:\n%s", name, df)
		}
	}
	green := map[string]string{
		"a comment naming one": "FROM x\n# no ENV such as SSL_CERT_FILE=/x here\nENV HOME=/h\n",
		"the public roots":     "FROM x\nRUN apt-get update && apt-get install -y --no-install-recommends \\\n      git ca-certificates jq \\\n    && rm -rf /var/lib/apt/lists/*\n",
		"another env":          "FROM x\nENV PAGER=cat \\\n    GIT_TERMINAL_PROMPT=0\n",
	}
	for name, df := range green {
		if got := imageTrustFindings(df); len(got) != 0 {
			t.Errorf("%s: reported %v on:\n%s", name, got, df)
		}
	}
}
