package cidef

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scalar is a YAML scalar this package keeps as written because it has no Go
// value worth comparing by (a timestamp, a binary). Two are equal when tag and
// text are.
type Scalar struct {
	Tag   string
	Value string
}

// maxNodes bounds how many nodes one document may expand to. Aliases make a
// small document arbitrarily large; past the bound it is unresolvable.
const maxNodes = 1 << 20

// converter turns a parsed YAML node tree into plain Go values, expanding
// aliases and merge keys itself. yaml.v3's own decoding into `any` cannot be
// used: it silently drops a custom tag such as `!reference`, which is exactly
// what must not be lost here.
type converter struct {
	active map[*yaml.Node]bool // anchored nodes being expanded, for cycles
	nodes  int
	// fatal: the DOCUMENT cannot be resolved (an alias cycle, too large).
	fatal []string
}

func newConverter() *converter {
	return &converter{active: map[*yaml.Node]bool{}}
}

func (c *converter) fail(format string, args ...any) Unresolved {
	msg := fmt.Sprintf(format, args...)
	c.fatal = append(c.fatal, msg)
	return Unresolved{Reason: msg}
}

// customTag returns the node's tag when it is not one of YAML's own.
func customTag(n *yaml.Node) (string, bool) {
	tag := n.ShortTag()
	if tag == "" || strings.HasPrefix(tag, "!!") {
		return "", false
	}
	return tag, true
}

func (c *converter) convert(n *yaml.Node) any {
	if n == nil {
		return nil
	}
	c.nodes++
	if c.nodes > maxNodes {
		if c.nodes == maxNodes+1 {
			return c.fail("the document expands to more than %d nodes", maxNodes)
		}
		return Unresolved{Reason: "the document is too large"}
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return c.fail("a YAML document with %d root nodes", len(n.Content))
		}
		return c.convert(n.Content[0])
	case yaml.AliasNode:
		target := n.Alias
		if target == nil {
			return c.fail("an alias (*%s) to no anchor", n.Value)
		}
		if c.active[target] {
			return c.fail("the anchor &%s contains itself", n.Value)
		}
		c.active[target] = true
		v := c.convert(target)
		delete(c.active, target)
		return v
	}
	if tag, custom := customTag(n); custom {
		// `!reference [.job, key]` and anything else GitLab resolves at a
		// stage this package does not reproduce. GitLab's merged YAML has
		// no tag left (it resolves `!reference` itself), so this is reached
		// only by a document that is not one.
		return Unresolved{Reason: fmt.Sprintf("a %s tag (line %d) is not resolved here", tag, n.Line)}
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return scalarValue(n)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, e := range n.Content {
			out = append(out, c.convert(e))
		}
		return out
	case yaml.MappingNode:
		return c.mapping(n)
	}
	return Unresolved{Reason: fmt.Sprintf("a YAML node of kind %d (line %d)", n.Kind, n.Line)}
}

func scalarValue(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!str":
		return n.Value
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return f
		}
	}
	return Scalar{Tag: n.ShortTag(), Value: n.Value}
}

// mapping converts a mapping node. Explicit keys first; then `<<` merges, which
// only ADD keys the mapping does not already have, the earlier merge winning.
// Anything else a mapping could mean (a duplicate key, a key that is not a
// scalar, a merge of something that is not a mapping) is Unresolved: which
// reading wins is a parser's choice and not one to make here.
func (c *converter) mapping(n *yaml.Node) any {
	out := make(map[string]any, len(n.Content)/2)
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.AliasNode && k.Alias != nil {
			k = k.Alias
		}
		if k.Kind != yaml.ScalarNode {
			return Unresolved{Reason: fmt.Sprintf("a mapping key that is not a scalar (line %d)", k.Line)}
		}
		if k.ShortTag() == "!!merge" {
			merges = append(merges, v)
			continue
		}
		if _, custom := customTag(k); custom {
			return Unresolved{Reason: fmt.Sprintf("a tagged mapping key %q (line %d)", k.Value, k.Line)}
		}
		if _, dup := out[k.Value]; dup {
			return Unresolved{Reason: fmt.Sprintf("the key %q appears twice in one mapping (line %d)", k.Value, k.Line)}
		}
		out[k.Value] = c.convert(v)
	}
	for _, m := range merges {
		sources := []*yaml.Node{m}
		if m.Kind == yaml.SequenceNode {
			sources = m.Content
		}
		for _, s := range sources {
			sv, ok := c.convert(s).(map[string]any)
			if !ok {
				return Unresolved{Reason: fmt.Sprintf("a merge key (<<) whose value is not a mapping (line %d)", s.Line)}
			}
			for k, v := range sv {
				if _, has := out[k]; !has {
					out[k] = v
				}
			}
		}
	}
	return out
}

// firstUnresolved returns the reason of an Unresolved marker anywhere in v.
// Map keys are visited in sorted order so the reason is stable.
func firstUnresolved(v any) (string, bool) {
	switch x := v.(type) {
	case Unresolved:
		return x.Reason, true
	case []any:
		for _, e := range x {
			if r, ok := firstUnresolved(e); ok {
				return r, true
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if r, ok := firstUnresolved(x[k]); ok {
				return r, true
			}
		}
	}
	return "", false
}

// deepMerge is GitLab's `extends` merge (Ruby's Hash#deep_merge): two hashes
// merge key by key, anything else -- an array included -- is replaced by over.
// Neither argument is modified.
func deepMerge(under, over map[string]any) map[string]any {
	out := make(map[string]any, len(under)+len(over))
	for k, v := range under {
		out[k] = v
	}
	for k, v := range over {
		um, uok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if uok && ook {
			out[k] = deepMerge(um, om)
			continue
		}
		out[k] = v
	}
	return out
}
