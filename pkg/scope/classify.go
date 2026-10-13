package scope

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// BaseFacts are the two classifications that depend on repository CONTENT.
// The controller computes them from blobs it read at the pinned base SHA --
// read-only data, never executed, never the session's working tree.
type BaseFacts struct {
	// CILocalIncludes are the `include: local:` paths (or patterns) of the base
	// .gitlab-ci.yml and of every file those name, as ResolveCILocalIncludes
	// returns them. A file the pipeline includes IS pipeline definition,
	// wherever it lives and at whatever depth. What is NOT in it: the files a
	// wildcard-included file includes (CIIncludes.Complete).
	CILocalIncludes []string
	// Generated holds the paths whose base content IsGenerated.
	Generated map[string]bool
}

// Refusal records a configured pattern that matched a path but that the
// lattice did not let move it.
type Refusal struct {
	To      Class
	Layer   string // "instance", "group" or "project"
	Key     string // the configuration key, e.g. "docs_paths"
	Pattern string
	Reason  string
}

// Decision is one path's classification and how it was reached.
type Decision struct {
	Path string
	// Class is the path's class.
	Class Class
	// Builtin is what the built-in patterns and the base facts alone say.
	// When Class differs, a configured pattern reclassified the path.
	Builtin Class
	// Profile is the shipped profile whose row gave Builtin ("" when a core
	// row, a base fact or nothing did). It is about the BUILT-IN class, so it
	// survives a reclassification: a Go helper moved into `test` by test_paths
	// is still a `go` file.
	Profile string
	// Layer, Key and Pattern name the configured pattern that reclassified it
	// ("" when Class == Builtin).
	Layer, Key, Pattern string
	// Refused lists matching patterns the lattice overruled.
	Refused []Refusal
}

// Reclassified reports whether configuration moved the path out of its
// built-in class.
func (d Decision) Reclassified() bool { return d.Class != d.Builtin }

// WeakeningDecidable reports whether a deterministic check can say that a
// change to this path did not weaken the tests. It is what a later part asks
// before letting a fix anywhere near auto-merge (gonk-2usw U4): false means
// "not caught, human review, no auto-merge".
//
// A path that is neither source nor test has no tests to weaken, so it is
// decidable. A source or test path is decidable only when a shipped profile
// that declares a predicate (go, and only go) classified it. A file that
// became source or test through source_paths or test_paths alone -- no profile
// vouches for what it is -- is not.
func (d Decision) WeakeningDecidable() bool {
	if d.Class != ClassSource && d.Class != ClassTest {
		return true
	}
	return WeakeningPredicate(d.Profile)
}

// Classifier classifies paths under one Effective scope.
type Classifier struct {
	eff  Effective
	tbl  *table
	base BaseFacts
	log  *slog.Logger
}

// Classifier returns the path classifier for this scope and these base facts.
//
// EVERY RECLASSIFICATION IS LOGGED HERE, not by the caller (owner decision Q3:
// maintainer reclassification is provisional, "log every reclassification so
// the review has data"). A nil logger means slog.Default().
func (e Effective) Classifier(base BaseFacts, log *slog.Logger) *Classifier {
	if log == nil {
		log = slog.Default()
	}
	return &Classifier{eff: e, tbl: e.table(), base: base, log: log}
}

// Classify returns p's class. It is total: every string gets exactly one class.
//
// The built-in class comes first, under this scope's profile selection
// (Effective.Profiles). Then the configured patterns are consulted
// in a fixed order -- protected_paths, ci_paths, test_paths, docs_paths,
// source_paths -- and the first one that matches AND that the lattice permits
// for this path's built-in class decides. A matching pattern the lattice
// refuses changes nothing and is recorded (and logged) as a refusal: the
// lattice is enforced per path here, whatever Policy.Validate let through.
func (c *Classifier) Classify(p string) Decision {
	d := Decision{Path: p}
	d.Builtin, d.Profile = c.tbl.classify(p, c.base)
	d.Class = d.Builtin
	if !cleanRel(p) {
		return d
	}
	for _, key := range []string{"protected_paths", "ci_paths", "test_paths", "docs_paths", "source_paths"} {
		moved := false
		for _, lp := range c.eff.patterns {
			if lp.key != key || lp.to == d.Builtin || !Match(lp.pattern, p) {
				continue
			}
			if err := CheckMove(d.Builtin, lp.to, lp.layer); err != nil {
				d.Refused = append(d.Refused, Refusal{To: lp.to, Layer: lp.name, Key: lp.key, Pattern: lp.pattern, Reason: err.Error()})
				break // one refusal per key is enough to say what happened
			}
			d.Class, d.Layer, d.Key, d.Pattern = lp.to, lp.name, lp.key, lp.pattern
			moved = true
			break
		}
		if moved {
			break
		}
	}
	for _, r := range d.Refused {
		c.log.Warn("scope: reclassification refused", "path", p, "from", string(d.Builtin), "to", string(r.To),
			"layer", r.Layer, "key", r.Key, "pattern", r.Pattern, "base_sha", c.eff.BaseSHA, "reason", r.Reason)
	}
	if d.Reclassified() {
		c.log.Info("scope: path reclassified", "path", p, "from", string(d.Builtin), "to", string(d.Class),
			"layer", d.Layer, "key", d.Key, "pattern", d.Pattern, "base_sha", c.eff.BaseSHA)
	}
	return d
}

var generatedRe = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// IsGenerated reports whether a file's content marks it as generated: the Go
// convention, on line 1 exactly (066 table 2.1).
func IsGenerated(content []byte) bool {
	line, _, _ := bytes.Cut(content, []byte("\n"))
	return generatedRe.Match(bytes.TrimSuffix(line, []byte("\r")))
}

// CILocalIncludes returns the local include paths ONE file states (the root
// .gitlab-ci.yml or a file it includes; ResolveCILocalIncludes walks them), with
// any leading slash removed (GitLab's `local:` paths are rooted at the
// repository). GitLab allows `*` and `**` in them, and they are returned as
// patterns; an entry with a `$` variable is returned as written
// (CIIncludeHasVariable). Remote, project, template and component includes
// name nothing in this repository and are skipped.
//
// EVERY YAML document of the file is read: a definition with a `spec:` header
// is two documents, and the includes are in the second.
//
// It is given the BASE blob by the controller. An include that is not a clean
// in-repository pattern is an error: the definition cannot be understood, so
// the caller must not guess which files are CI.
//
// What it cannot say is that an include was not readable as a list of
// entries at all (a custom YAML tag such as `!reference`): such a node names
// nothing here, and only ResolveCILocalIncludes reports it (Complete false).
func CILocalIncludes(ciYAML []byte) ([]string, error) {
	out, _, err := ciLocalIncludes(ciYAML)
	return out, err
}

// customTagged blanks every node under n that carries a custom YAML tag
// (`!reference`; anything but the `!!` core tags) and reports whether there
// was one. The decoder ignores such a tag, so `!reference [a, b]` would
// otherwise be read as the list of two strings it is not.
func customTagged(n *yaml.Node, depth int) bool {
	if n == nil || depth > 64 {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return customTagged(n.Alias, depth+1)
	}
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") { // includes:tag
		*n = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		return true
	}
	found := false
	for _, c := range n.Content {
		if customTagged(c, depth+1) {
			found = true
		}
	}
	return found
}

// ciLocalIncludes is CILocalIncludes plus opaque: some part of an `include:`
// carried a custom tag, was not read, and may name files this result lacks.
func ciLocalIncludes(ciYAML []byte) (entries []string, opaque bool, err error) {
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(ciYAML))
	for {
		var n yaml.Node
		if derr := dec.Decode(&n); derr == io.EOF { // includes:documents
			break
		} else if derr != nil {
			return nil, false, fmt.Errorf("scope: .gitlab-ci.yml: %w", derr)
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) == 1 && n.Content[0].Kind == yaml.MappingNode {
			m := n.Content[0]
			for i := 0; i+1 < len(m.Content); i += 2 {
				if m.Content[i].Value == "include" && customTagged(m.Content[i+1], 0) {
					opaque = true
				}
			}
		}
		var doc map[string]any
		if derr := n.Decode(&doc); derr != nil {
			return nil, false, fmt.Errorf("scope: .gitlab-ci.yml: %w", derr)
		}
		docs = append(docs, doc)
	}
	var out []string
	add := func(s string) error {
		s = strings.TrimPrefix(s, "/")
		if err := ValidatePattern(s); err != nil {
			return fmt.Errorf("scope: .gitlab-ci.yml include: %w", err)
		}
		out = append(out, s)
		return nil
	}
	// A bare string is a local path unless it is a URL.
	str := func(s string) error {
		if strings.Contains(s, "://") {
			return nil
		}
		return add(s)
	}
	entry := func(v any) error {
		switch t := v.(type) {
		case string:
			return str(t)
		case map[string]any:
			if l, ok := t["local"].(string); ok {
				return add(l)
			}
		}
		return nil
	}
	for _, doc := range docs {
		switch t := doc["include"].(type) {
		case []any:
			for _, v := range t {
				if err := entry(v); err != nil {
					return nil, false, err
				}
			}
		default:
			if err := entry(t); err != nil {
				return nil, false, err
			}
		}
	}
	return out, opaque, nil
}
