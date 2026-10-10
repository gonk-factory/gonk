package buildgate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// THE SECRET SCANNER'S CONFIGURATION MAY ALLOW EXACTLY ONE THING.
//
// .gitleaks.toml is loaded by gitleaks from the root of whatever tree it
// scans, so this file decides what the public "zero findings" gate accepts.
// An allowlist is the cheapest way there is to make a finding go away, and the
// next one added in a hurry would be a path, a stop word, or a regex without
// anchors -- each of which hides real credentials, silently, for ever.
//
// So the shape is pinned: the default rules extended, no rule of our own, ONE
// allowlist, for ONE rule, matched against the captured secret, whose regexes
// are anchored at both ends and accept the session-key grammars and nothing
// near them. Widening the file fails here and has to be argued in this test.

const scannerConfigFile = ".gitleaks.toml"

type scannerConfig struct {
	Title  string `toml:"title"`
	Extend struct {
		UseDefault    bool     `toml:"useDefault"`
		Path          string   `toml:"path"`
		DisabledRules []string `toml:"disabledRules"`
	} `toml:"extend"`
	Rules      []map[string]any `toml:"rules"`
	Allowlist  map[string]any   `toml:"allowlist"`
	Allowlists []struct {
		Description string   `toml:"description"`
		TargetRules []string `toml:"targetRules"`
		RegexTarget string   `toml:"regexTarget"`
		Regexes     []string `toml:"regexes"`
		Paths       []string `toml:"paths"`
		Commits     []string `toml:"commits"`
		StopWords   []string `toml:"stopwords"`
		Condition   string   `toml:"condition"`
	} `toml:"allowlists"`
}

// scannerConfigProblems is the whole check, pure so that the negative controls
// below can drive it with configurations that are not the committed one.
func scannerConfigProblems(text string) []string {
	var c scannerConfig
	md, err := toml.Decode(text, &c)
	if err != nil {
		return []string{"not valid TOML: " + err.Error()}
	}
	var p []string
	// Anything this struct does not name is something this test does not
	// understand, and so cannot have judged.
	for _, k := range md.Undecoded() {
		p = append(p, "unexpected key "+k.String()+": this test does not know what it allows")
	}
	if !c.Extend.UseDefault {
		p = append(p, "extend.useDefault is not true: the default rule set is not in force")
	}
	if c.Extend.Path != "" || len(c.Extend.DisabledRules) > 0 {
		p = append(p, "extend names another base config or disables rules")
	}
	if len(c.Rules) > 0 {
		p = append(p, fmt.Sprintf("%d [[rules]] defined: a rule of the same id REPLACES the default one", len(c.Rules)))
	}
	if c.Allowlist != nil {
		p = append(p, "a global [allowlist] table: it applies to every rule")
	}
	if len(c.Allowlists) != 1 {
		return append(p, fmt.Sprintf("%d [[allowlists]], want exactly 1", len(c.Allowlists)))
	}
	a := c.Allowlists[0]
	if len(a.TargetRules) != 1 || a.TargetRules[0] != "generic-api-key" {
		p = append(p, fmt.Sprintf("targetRules = %q, want exactly [generic-api-key]; without it the allowlist applies to EVERY rule", a.TargetRules))
	}
	if a.RegexTarget != "secret" {
		p = append(p, fmt.Sprintf("regexTarget = %q, want \"secret\": matched against the line or the match, a real credential beside a session key is excused", a.RegexTarget))
	}
	if len(a.Paths)+len(a.Commits)+len(a.StopWords) > 0 || a.Condition != "" {
		p = append(p, "the allowlist has paths, commits, stopwords or a condition: only anchored secret regexes are accepted")
	}
	if strings.TrimSpace(a.Description) == "" {
		p = append(p, "the allowlist has no description")
	}
	if len(a.Regexes) == 0 {
		p = append(p, "the allowlist has no regexes")
	}
	for _, src := range a.Regexes {
		if !strings.HasPrefix(src, "^gonk-") || !strings.HasSuffix(src, "$") || strings.HasSuffix(src, `\$`) {
			p = append(p, fmt.Sprintf("regex %q is not anchored at both ends on the gonk- session-key prefix", src))
			continue
		}
		re, err := regexp.Compile(src)
		if err != nil {
			p = append(p, fmt.Sprintf("regex %q does not compile: %v", src, err))
			continue
		}
		for _, near := range scannerNearMisses() {
			if re.MatchString(near) {
				p = append(p, fmt.Sprintf("regex %q accepts %q, which is not a session key", src, near))
			}
		}
	}
	return p
}

// scannerNearMisses are strings one step away from a session key, and the
// shape of a real token. They are ASSEMBLED, so that this file holds nothing
// a scanner (ours or anyone's) would report.
func scannerNearMisses() []string {
	hex12 := strings.Repeat("0123456789ab", 1)
	token := strings.Repeat("aB3", 13) + "x" // 40 mixed characters
	return []string{
		"gonk-42-pl-" + hex12 + "c",            // 13 hex
		"gonk-42-pl-" + hex12[:11],             // 11 hex
		"gonk-42-pl-" + hex12[:11] + "g",       // not hex
		"gonk-42-pl-" + strings.ToUpper(hex12), // upper case
		"gonk-42-pl-" + token,                  // a token after the prefix
		"gonk-042-pl-" + hex12,                 // not a project id
		"gonk--pl-" + hex12,                    // no project id
		"xgonk-42-pl-" + hex12,                 // a prefix in front
		"gonk-42-pl-" + hex12 + "\n" + token,   // a second line
		"gonk-42-px-" + hex12,                  // another kind
		"gonk-42-scaffold-" + token,            // a suffix
		"gonk-42-scaffolds",
		"gonk-0-scaffold",
		token,
		"",
	}
}

func scannerConfigText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), scannerConfigFile))
	if err != nil {
		t.Fatalf("read %s: %v", scannerConfigFile, err)
	}
	return string(b)
}

func TestScannerConfigAllowsOnlyTheSessionKeyGrammar(t *testing.T) {
	text := scannerConfigText(t)
	if p := scannerConfigProblems(text); len(p) > 0 {
		t.Fatalf("%s is wider than one anchored allowlist for generic-api-key:\n  %s", scannerConfigFile, strings.Join(p, "\n  "))
	}
	var c scannerConfig
	if _, err := toml.Decode(text, &c); err != nil {
		t.Fatal(err)
	}
	var res []*regexp.Regexp
	for _, src := range c.Allowlists[0].Regexes {
		res = append(res, regexp.MustCompile(src))
	}
	// The keys the real code produces must be the ones allowed -- otherwise the
	// file allows a grammar nothing uses, and the findings it exists for stay.
	key, err := pipelinectx.SessionKey("gonk:42:pipeline:ref-0d6e4079:0123456789abcdef:s880")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{
		"a pipeline session key (pipelinectx.SessionKey)": key,
		"a pipeline key with a long project id":           strings.Replace(key, "gonk-42-", "gonk-1234567-", 1),
		"a scaffold session key (pkg/intake)":             fmt.Sprintf("gonk-%d-scaffold", 63),
	}
	for what, k := range keys {
		n := 0
		for _, re := range res {
			if re.MatchString(k) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s, %q, is matched by %d of the allowlist regexes, want exactly 1", what, k, n)
		}
	}
	// And every regex earns its place: one that matches none of the keys above
	// allows something this test has not looked at.
	for _, re := range res {
		used := false
		for _, k := range keys {
			used = used || re.MatchString(k)
		}
		if !used {
			t.Errorf("allowlist regex %q matches no session key this test knows", re)
		}
	}
}

// The check, against configurations that are each ONE step wider.
func TestScannerConfigWideningIsRefused(t *testing.T) {
	good := scannerConfigText(t)
	edit := func(old, repl string) string {
		t.Helper()
		if strings.Count(good, old) != 1 {
			t.Fatalf("%s holds %q %d times, want once (this negative control edits it)", scannerConfigFile, old, strings.Count(good, old))
		}
		return strings.Replace(good, old, repl, 1)
	}
	for name, c := range map[string]struct{ text, want string }{
		"no targetRules":           {edit(`targetRules = ["generic-api-key"]`+"\n", ""), "targetRules"},
		"a second target rule":     {edit(`targetRules = ["generic-api-key"]`, `targetRules = ["generic-api-key", "gitlab-pat"]`), "targetRules"},
		"matched against the line": {edit(`regexTarget = "secret"`, `regexTarget = "line"`), "regexTarget"},
		"regexTarget left out":     {edit(`regexTarget = "secret"`+"\n", ""), "regexTarget"},
		"no end anchor":            {edit(`-pl-[0-9a-f]{12}$'''`, `-pl-[0-9a-f]{12}'''`), "not anchored"},
		"no start anchor":          {edit(`'''^gonk-[1-9][0-9]*-scaffold$'''`, `'''gonk-[1-9][0-9]*-scaffold$'''`), "not anchored"},
		"anchored but open-ended":  {edit(`-pl-[0-9a-f]{12}$'''`, `-pl-.*$'''`), "which is not a session key"},
		"any length of hex":        {edit(`[0-9a-f]{12}$`, `[0-9a-f]+$`), "which is not a session key"},
		"a path allowlist":         {edit(`regexTarget = "secret"`, "regexTarget = \"secret\"\npaths = ['''testdata/''']"), "paths"},
		"a stop word":              {edit(`regexTarget = "secret"`, "regexTarget = \"secret\"\nstopwords = [\"gonk\"]"), "stopwords"},
		"defaults not extended":    {edit(`useDefault = true`, `useDefault = false`), "useDefault"},
		"a rule disabled":          {edit(`useDefault = true`, "useDefault = true\ndisabledRules = [\"generic-api-key\"]"), "disables rules"},
		"a rule replaced":          {good + "\n[[rules]]\nid = \"generic-api-key\"\nregex = '''x'''\n", "[[rules]]"},
		"a second allowlist":       {good + "\n[[allowlists]]\ndescription = \"x\"\nregexes = ['''^gonk-x$''']\n", "want exactly 1"},
		"a global allowlist":       {edit(`[[allowlists]]`, "[allowlist]\nregexes = ['''^gonk-x$''']\n\n[[allowlists]]"), "global [allowlist]"},
		"an unknown key":           {edit(`regexTarget = "secret"`, "regexTarget = \"secret\"\nsomethingNew = true"), "unexpected key"},
	} {
		p := strings.Join(scannerConfigProblems(c.text), "\n")
		if !strings.Contains(p, c.want) {
			t.Errorf("%s: not refused for %q; problems:\n%s", name, c.want, p)
		}
	}
}
