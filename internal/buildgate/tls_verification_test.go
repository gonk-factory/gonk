package buildgate

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// NOTHING GONK SHIPS TURNS TLS VERIFICATION OFF (gonk-s2tk).
//
// The chart README says what these tests check, in the same words, and it must
// not say more: for a release the sentence was there and the test was not,
// while test/live set InsecureSkipVerify: true twice; and for v0.2.0 the
// sentence was wider than the test (no `curl -k`, no space form of git's
// sslVerify, no verify_ssl or rejectUnauthorized key, no sslmode=disable, and
// nothing outside the chart and the Go code). These four tests are that test:
//
//   - TestNoRenderedManifestDisablesTLSVerification reads what the chart
//     DEPLOYS: every golden manifest, parsed as YAML so that a comment cannot
//     trip it and a key or a value cannot hide in one.
//   - TestNoChartSourceDisablesTLSVerification reads the templates and
//     values.yaml themselves, because a golden only shows the branches its
//     profile takes.
//   - TestNoShippedScriptDisablesTLSVerification reads the non-Go, non-Markdown
//     files under images/, pack/ and hack/.
//   - TestNoGoCodeDisablesTLSVerification reads every Go file in the module,
//     test files and every build tag included, for InsecureSkipVerify alone.
//
// What none of them reads: Markdown; a Go string (a test's loopback DSN says
// sslmode=disable, and the ledger's DSN is the operator's own); a Go
// VerifyPeerCertificate or a custom dialer; any spelling not listed below.
//
// The goldens are held equal to a real render by TestGoldenManifests
// (internal/charttest, run by `make gate`), so reading them here is reading the
// render -- without needing helm in `go test ./...`.

// tlsOffPatterns are the spellings refused in a rendered key, a rendered string
// value, or a non-comment line of chart source or of a shipped script.
// Matching is case-insensitive unless a pattern says otherwise.
var tlsOffPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)insecure`),
	regexp.MustCompile(`(?i)skip[-_ ]?(tls[-_ ]?)?verify`),
	regexp.MustCompile(`(?i)ssl[-_.]?verify["']?\s*[:=]\s*["']?(false|0|no|off)\b`),
	// `git config http.sslVerify false`: the value after a space.
	regexp.MustCompile(`(?i)ssl[-_.]?verify\s+["']?(false|0|no|off)\b`),
	regexp.MustCompile(`(?i)ssl_no_verify`),
	regexp.MustCompile(`(?i)tls_reject_unauthorized`),
	regexp.MustCompile(`(?i)reject[-_]?unauthorized["']?\s*[:=]\s*["']?(false|0|no|off)\b`),
	regexp.MustCompile(`(?i)no-check-certificate`),
	regexp.MustCompile(`(?i)verify[-_]?(ssl|tls|peer|mode)?["']?\s*[:=]\s*["']?(false|none)\b`),
	regexp.MustCompile(`(?i)verify[-_](ssl|tls|peer)["']?\s*[:=]\s*["']?(0|no|off)\b`),
	// A Postgres connection that asks for no TLS at all. `require`, `prefer`
	// and `allow` are not matched: they are weaker than verify-full, but they
	// are a statement about a database the operator names, not a switch gonk
	// turns off.
	regexp.MustCompile(`(?i)sslmode["']?\s*[:=]\s*["']?disable\b`),
	// curl's short form of --insecure, alone or in a cluster (`-k`, `-sSk`).
	// Case-sensitive: `-K` reads a config file. A long option (`--keepalive-time`)
	// and a `-k` on a line without curl are not matched.
	regexp.MustCompile(`\bcurl\b[^|;&]*\s-[A-Za-z]*k[A-Za-z]*(\s|$)`),
	// Python.
	regexp.MustCompile(`\bCERT_NONE\b`),
	regexp.MustCompile(`_create_unverified_context`),
}

// verifyEnvName matches an environment variable whose VALUE is the switch, so
// the name alone carries no "false" for the patterns above to see. In a
// rendered manifest every such variable must be "true".
var verifyEnvName = regexp.MustCompile(`(?i)(ssl|tls)_?verify`)

// verifyKeyName / verifyOffValue: a mapping key that is itself the switch, in
// either word order (sslVerify, verify_ssl, rejectUnauthorized), and the
// scalar values that turn one off. sslModeKey is the one switch whose "off"
// is a word of its own.
var (
	verifyKeyName  = regexp.MustCompile(`(?i)(ssl|tls|cert(ificate)?s?)[-_.]?verif(y|ication)|verif(y|ication)[-_.]?(ssl|tls|peer|cert(ificate)?s?)|reject[-_]?unauthorized`)
	verifyOffValue = regexp.MustCompile(`(?i)^(false|0|no|off|none)$`)
	sslModeKey     = regexp.MustCompile(`(?i)^ssl[-_]?mode$`)
	curlShortK     = regexp.MustCompile(`^-[A-Za-z]*k[A-Za-z]*$`)
)

// curlArgvInsecure reports whether a list is an argument vector that runs
// curl with -k: the words are separate strings there, so no line pattern
// sees them together.
func curlArgvInsecure(list []any) bool {
	curl, k := false, false
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			continue
		}
		if s == "curl" || strings.HasSuffix(s, "/curl") {
			curl = true
		}
		if curlShortK.MatchString(s) {
			k = true
		}
	}
	return curl && k
}

func tlsOffHit(s string) string {
	for _, re := range tlsOffPatterns {
		if m := re.FindString(s); m != "" {
			return m
		}
	}
	return ""
}

// scanRendered walks one decoded YAML document and returns a finding for every
// key or string value that reads as "verification off", and for every
// container env entry named like a verify switch whose value is not "true".
func scanRendered(v any, path string, out *[]string) {
	switch n := v.(type) {
	case map[string]any:
		if name, ok := n["name"].(string); ok && verifyEnvName.MatchString(name) {
			if val, has := n["value"]; has && fmt.Sprint(val) != "true" {
				*out = append(*out, fmt.Sprintf("%s: env %s is %q, not \"true\"", path, name, fmt.Sprint(val)))
			}
		}
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if hit := tlsOffHit(k); hit != "" {
				*out = append(*out, fmt.Sprintf("%s: key %q (%q)", path, k, hit))
			}
			// A key that IS a verify switch (sslVerify, tls_verify, ...) with
			// a value that turns it off. The value is a YAML scalar here, so
			// no text pattern above can see the pair.
			if verifyKeyName.MatchString(k) && verifyOffValue.MatchString(fmt.Sprint(n[k])) {
				*out = append(*out, fmt.Sprintf("%s: key %q is %v", path, k, n[k]))
			}
			if sslModeKey.MatchString(k) && strings.EqualFold(fmt.Sprint(n[k]), "disable") {
				*out = append(*out, fmt.Sprintf("%s: key %q is %v", path, k, n[k]))
			}
			scanRendered(n[k], path+"."+k, out)
		}
	case []any:
		if curlArgvInsecure(n) {
			*out = append(*out, fmt.Sprintf("%s: an argument list runs curl with -k", path))
		}
		for i, e := range n {
			scanRendered(e, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case string:
		for i, line := range strings.Split(n, "\n") {
			if hit := tlsOffHit(line); hit != "" {
				*out = append(*out, fmt.Sprintf("%s (line %d of the value): %q in %q", path, i+1, hit, strings.TrimSpace(line)))
			}
		}
	}
}

// scanRenderedFile decodes every document of a multi-document manifest.
func scanRenderedFile(data []byte) (docs int, findings []string, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc any
		if derr := dec.Decode(&doc); derr != nil {
			if errors.Is(derr, io.EOF) {
				return docs, findings, nil
			}
			return docs, findings, derr
		}
		if doc == nil {
			continue
		}
		docs++
		id := fmt.Sprintf("doc %d", docs)
		if m, ok := doc.(map[string]any); ok {
			kind, _ := m["kind"].(string)
			name := ""
			if md, ok := m["metadata"].(map[string]any); ok {
				name, _ = md["name"].(string)
			}
			id = kind + "/" + name
		}
		scanRendered(doc, id, &findings)
	}
}

func TestNoRenderedManifestDisablesTLSVerification(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "charttest", "testdata", "golden")
	goldens, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Vacuity floor: a renamed directory must not turn this into a test of
	// nothing. There are eight golden profiles today; the list only grows.
	if len(goldens) < 8 {
		t.Fatalf("found %d golden manifests under %s, want at least 8: this test would be checking nothing", len(goldens), dir)
	}
	for _, g := range goldens {
		data, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		docs, findings, err := scanRenderedFile(data)
		if err != nil {
			t.Errorf("%s: not parseable as YAML: %v", filepath.Base(g), err)
			continue
		}
		if docs < 10 {
			t.Errorf("%s: only %d documents decoded; a golden renders dozens", filepath.Base(g), docs)
		}
		for _, f := range findings {
			t.Errorf("%s: %s\n\tA gonk service never skips TLS verification: trust the CA (gitlab.caCert) instead.", filepath.Base(g), f)
		}
	}
}

// The scanner must actually see the things it exists to refuse. Without this,
// a pattern that stopped matching would leave the test above green forever.
func TestTLSVerificationScannerSeesEachForbiddenForm(t *testing.T) {
	const wrap = "kind: Deployment\nmetadata: {name: x}\nspec:\n"
	bad := map[string]string{
		"an insecureSkipVerify key": wrap + "  tlsConfig: {insecureSkipVerify: true}\n",
		"an --insecure argument":    wrap + "  args: [\"--insecure\"]\n",
		// One spelling per fixture. Until v0.2.1 these two shared one, and the
		// fixture passed on the `=` line alone: the space form was not seen.
		"git config sslVerify false (space form)": wrap + "  script: |\n    git config --global http.sslVerify false\n",
		"git -c sslVerify=false":                  wrap + "  script: |\n    git -c http.sslVerify=false fetch\n",
		"curl -k in a script":                     wrap + "  script: |\n    curl -k https://x\n",
		"curl -sk in a script":                    wrap + "  script: curl -sSkL https://x\n",
		"curl -k as an argument list":             wrap + "  command: [\"/usr/bin/curl\", \"-fsk\", \"https://x\"]\n",
		"curl --insecure in a script":             wrap + "  script: curl --insecure https://x\n",
		"verify_ssl: false as a key":              wrap + "  http: {verify_ssl: false}\n",
		"verify_ssl: no in a blob":                wrap + "  config: |\n    verify_ssl: no\n",
		"rejectUnauthorized: false as a key":      wrap + "  tls: {rejectUnauthorized: false}\n",
		"rejectUnauthorized: false in a blob":     wrap + "  script: |\n    new Agent({rejectUnauthorized: false})\n",
		"sslmode=disable in a DSN":                wrap + "  env: [{name: DSN, value: \"postgres://u@h/db?sslmode=disable\"}]\n",
		"sslmode: disable as a key":               wrap + "  db: {sslmode: disable}\n",
		"python verify=False":                     wrap + "  script: |\n    requests.get(u, verify=False)\n",
		"python CERT_NONE":                        wrap + "  script: |\n    ctx.verify_mode = ssl.CERT_NONE\n",
		"python unverified context":               wrap + "  script: |\n    ssl._create_unverified_context()\n",
		"sslVerify: false as a key":               wrap + "  http: {sslVerify: false}\n",
		"GIT_SSL_NO_VERIFY":                       wrap + "  env: [{name: GIT_SSL_NO_VERIFY, value: \"1\"}]\n",
		"NODE_TLS_REJECT_UNAUTHORIZED":            wrap + "  env: [{name: NODE_TLS_REJECT_UNAUTHORIZED, value: \"0\"}]\n",
		"wget --no-check-certificate":             wrap + "  script: wget --no-check-certificate https://x\n",
		"a verify switch env not true":            wrap + "  env: [{name: GONK_WEBHOOK_SSL_VERIFY, value: \"false\"}]\n",
		"ssl_verify: false in a blob":             wrap + "  config: |\n    ssl_verify: false\n",
		"skip-tls-verify":                         wrap + "  args: [\"--skip-tls-verify\"]\n",
	}
	for name, doc := range bad {
		_, findings, err := scanRenderedFile([]byte(doc))
		if err != nil {
			t.Errorf("%s: fixture does not parse: %v", name, err)
			continue
		}
		if len(findings) == 0 {
			t.Errorf("%s: NOT reported. The scanner is blind to a form it exists to refuse.", name)
		}
	}
	good := map[string]string{
		"a comment naming the field":  wrap + "  # no code here ever sets InsecureSkipVerify\n  replicas: 1\n",
		"the verify switch left true": wrap + "  env: [{name: GONK_WEBHOOK_SSL_VERIFY, value: \"true\"}]\n",
		// Near misses of the v0.2.1 spellings: none of these turns anything off.
		"sslmode=verify-full":              wrap + "  env: [{name: DSN, value: \"postgres://u@h/db?sslmode=verify-full\"}]\n",
		"sslmode: require as a key":        wrap + "  db: {sslmode: require}\n",
		"curl with -K and a long k option": wrap + "  script: curl -fsSL -K cfg --keepalive-time 5 -o out-k https://x\n",
		"a -k that is not curl's":          wrap + "  command: [\"du\", \"-k\", \"/data\"]\n",
		"du -k in a script":                wrap + "  script: du -k /data\n",
		"sslVerify true (space form)":      wrap + "  script: git config http.sslVerify true\n",
		"verify_ssl: true as a key":        wrap + "  http: {verify_ssl: true}\n",
		"rejectUnauthorized: true":         wrap + "  tls: {rejectUnauthorized: true}\n",
		"python verify=True":               wrap + "  script: requests.get(u, verify=True)\n",
	}
	for name, doc := range good {
		_, findings, err := scanRenderedFile([]byte(doc))
		if err != nil {
			t.Errorf("%s: fixture does not parse: %v", name, err)
			continue
		}
		if len(findings) != 0 {
			t.Errorf("%s: reported %v; the scanner refuses something legitimate", name, findings)
		}
	}
}

// chartSourceLines returns the non-comment lines of a template or values file:
// a line whose first non-blank character is '#' is a comment in both, and a
// {{/* ... */}} template comment is dropped whole.
var templateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)

func chartSourceFindings(data []byte) []string {
	// Keep line numbers true: blank out a template comment, do not delete it.
	src := templateComment.ReplaceAllStringFunc(string(data), func(c string) string {
		return strings.Repeat("\n", strings.Count(c, "\n"))
	})
	var out []string
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A trailing comment is not configuration either.
		if j := strings.Index(line, " #"); j >= 0 {
			line = line[:j]
		}
		if hit := tlsOffHit(line); hit != "" {
			out = append(out, fmt.Sprintf("line %d: %q in %q", i+1, hit, strings.TrimSpace(line)))
		}
	}
	return out
}

func TestNoChartSourceDisablesTLSVerification(t *testing.T) {
	chart := filepath.Join(repoRoot(t), sealChartDir)
	files := []string{filepath.Join(chart, "values.yaml")}
	for _, sub := range []string{"templates", "ci"} {
		err := filepath.WalkDir(filepath.Join(chart, sub), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 20 {
		t.Fatalf("only %d chart source files found under %s: this test would be checking nothing", len(files), chart)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(repoRoot(t), f)
		for _, finding := range chartSourceFindings(data) {
			t.Errorf("%s: %s\n\tNo chart value or template may turn TLS verification off.", rel, finding)
		}
	}

	// And the scanner sees source the way it must.
	if got := chartSourceFindings([]byte("a: 1\n  # sslVerify: false is never set\n{{/* insecure\n */}}\nb: 2\n")); len(got) != 0 {
		t.Errorf("comments were reported as configuration: %v", got)
	}
	if got := chartSourceFindings([]byte("a: 1\n  insecureSkipVerify: true\n")); len(got) != 1 {
		t.Errorf("an insecureSkipVerify key in a template was not reported exactly once: %v", got)
	}
}

// shippedScriptDirs are the directories, besides the chart, whose non-Go files
// end up in an image, in the pack a deployment loads, or in a maintainer's
// shell: the Dockerfiles and the agent image's scripts, the pack's scripts and
// configuration, and the tooling under hack/.
var shippedScriptDirs = []string{"images", "pack", "hack"}

// shippedScriptSkipped reports whether a file under shippedScriptDirs is NOT
// read as script text. Go is read as syntax by
// TestNoGoCodeDisablesTLSVerification. Markdown is prose -- an agent prompt
// that says "never pass --insecure" is the opposite of a finding, and no
// pattern can tell the two apart.
func shippedScriptSkipped(name string) bool {
	return strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".md")
}

// TestNoShippedScriptDisablesTLSVerification reads every other file under
// images/, pack/ and hack/, a line at a time, comment lines left out, for the
// same spellings as the chart source.
func TestNoShippedScriptDisablesTLSVerification(t *testing.T) {
	root := repoRoot(t)
	read := map[string]int{}
	for _, dir := range shippedScriptDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || shippedScriptSkipped(d.Name()) {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			if bytes.IndexByte(data, 0) >= 0 { // a binary: not script text
				return nil
			}
			read[dir]++
			rel, _ := filepath.Rel(root, p)
			for _, finding := range chartSourceFindings(data) {
				t.Errorf("%s: %s\n\tNothing gonk builds, loads or runs may turn TLS verification off: trust the CA instead.", rel, finding)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Vacuity floors, per directory: a moved directory must not turn this
	// into a test of nothing.
	for dir, min := range map[string]int{"images": 5, "pack": 5, "hack": 20} {
		if read[dir] < min {
			t.Errorf("read only %d files under %s/, want at least %d: this test would be checking nothing", read[dir], dir, min)
		}
	}

	// The scanner reads a script the way it must: each spelling on a code
	// line is one finding, and a comment naming one is none.
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"curl -k":                      {"#!/bin/sh\ncurl -sk https://x | sh\n", 1},
		"curl --insecure":              {"RUN curl --insecure -o /x https://x\n", 1},
		"git config, the space form":   {"git config --global http.sslVerify false\n", 1},
		"python verify=False":          {"r = requests.get(u, verify=False)\n", 1},
		"python unverified context":    {"ctx = ssl._create_unverified_context()\n", 1},
		"toml verify_ssl":              {"[http]\nverify_ssl = false\n", 1},
		"node rejectUnauthorized":      {"const a = new Agent({ rejectUnauthorized: false })\n", 1},
		"a DSN with sslmode=disable":   {"DSN=postgres://u@h/db?sslmode=disable\n", 1},
		"a comment naming the flag":    {"# never pass -k or --insecure to curl\ncurl -fsSL https://x\n", 0},
		"curl with a pinned checksum":  {"RUN curl -fsSL -o /x https://x && echo \"abc  /x\" | sha256sum -c -\n", 0},
		"another tool's -k after pipe": {"curl -fsSL https://x | sort -k 2\n", 0},
	} {
		if got := chartSourceFindings([]byte(tc.src)); len(got) != tc.want {
			t.Errorf("%s: %d findings, want %d (%v)", name, len(got), tc.want, got)
		}
	}
	if !shippedScriptSkipped("x.go") || !shippedScriptSkipped("README.md") || shippedScriptSkipped("Dockerfile.agent") || shippedScriptSkipped("x.sh") {
		t.Error("the skip rule does not skip exactly Go and Markdown")
	}
}

// goTLSOffFindings parses one Go file and reports every place it SETS
// InsecureSkipVerify: a composite-literal field or an assignment. A comment or
// a string that merely names the field is not a finding, which is why this
// reads the syntax tree and not the text.
func goTLSOffFindings(fset *token.FileSet, name string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "InsecureSkipVerify" {
				out = append(out, fset.Position(x.Pos()).String())
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "InsecureSkipVerify" {
					out = append(out, fset.Position(lhs.Pos()).String())
				}
			}
		}
		return true
	})
	return out, nil
}

func TestNoGoCodeDisablesTLSVerification(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// vendor/ is third-party code (crypto/tls's own users); a nested
			// worktree or a dot-directory is not this module.
			if p != root && (d.Name() == "vendor" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, p)
		findings, perr := goTLSOffFindings(fset, rel, src)
		if perr != nil {
			// testdata may hold deliberately broken Go; it is not compiled.
			if strings.Contains(filepath.ToSlash(rel), "/testdata/") {
				return nil
			}
			return perr
		}
		files++
		for _, f := range findings {
			t.Errorf("%s sets InsecureSkipVerify.\n\tTrust the CA instead: crypto/x509 reads SSL_CERT_FILE and SSL_CERT_DIR with no code at all.", f)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 200 {
		t.Fatalf("parsed only %d Go files under %s: this test would be checking nothing", files, root)
	}

	// The detector sees both ways of setting the field, and not a mention.
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"literal":    {"package p\nimport \"crypto/tls\"\nvar c = &tls.Config{InsecureSkipVerify: true}\n", 1},
		"assignment": {"package p\nimport \"crypto/tls\"\nfunc f(c *tls.Config) { c.InsecureSkipVerify = true }\n", 1},
		"mention":    {"package p\n// never set InsecureSkipVerify\nconst s = \"InsecureSkipVerify\"\n", 0},
	} {
		got, err := goTLSOffFindings(token.NewFileSet(), name+".go", []byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d findings, want %d (%v)", name, len(got), tc.want, got)
		}
	}
}
