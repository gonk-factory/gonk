package gonkcfg

import (
	"bytes"
	_ "embed"
	"fmt"
	"math"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// SchemaVersion is the .gonk.yml schema major version this package speaks.
const SchemaVersion = 1

//go:embed gonk-config.v1.schema.json
var schemaJSON []byte

var compiled = mustCompile()

func mustCompile() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema unreadable: %v", err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("gonk-config.v1.schema.json", doc); err != nil {
		panic(err)
	}
	s, err := c.Compile("gonk-config.v1.schema.json")
	if err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema invalid: %v", err))
	}
	return s
}

// Validate checks raw .gonk.yml bytes against the v1 JSON Schema.
// It does not apply defaults or precedence; see Load and Resolve.
func Validate(raw []byte) error {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf(".gonk.yml: not valid YAML: %w", err)
	}
	// Guard the validator against non-finite floats before it sees them.
	// jsonschema/v6 (v6.0.2 validator.go:515-524) builds a big.Rat from a
	// number to compare against "minimum", gets nil back for NaN/Inf,
	// discards the ok, and dereferences it -- a SIGSEGV. .gonk.yml is
	// untrusted, project-authored repo content, so one line of YAML would
	// otherwise crash the process enforcing every project's budget.
	if err := rejectNonFinite(doc, ""); err != nil {
		return fmt.Errorf(".gonk.yml: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		return fmt.Errorf(".gonk.yml: %w", err)
	}
	return nil
}

// rejectNonFinite walks a decoded YAML document and rejects any non-finite
// float. JSON has no NaN or Infinity, so a non-finite float is never a valid
// document at any path -- reject generically rather than special-casing the
// one field whose "minimum" keyword we know reaches the crash today.
func rejectNonFinite(v any, path string) error {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return fmt.Errorf("%s: not a finite number", pathOrRoot(path))
		}
	case float32:
		f := float64(t)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%s: not a finite number", pathOrRoot(path))
		}
	case map[string]any:
		for k, val := range t {
			if err := rejectNonFinite(val, join(path, k)); err != nil {
				return err
			}
		}
	case map[any]any: // yaml.v3 can produce this for non-string keys
		for k, val := range t {
			if err := rejectNonFinite(val, join(path, fmt.Sprintf("%v", k))); err != nil {
				return err
			}
		}
	case []any:
		for i, val := range t {
			if err := rejectNonFinite(val, fmt.Sprintf("%s[%d]", pathOrRoot(path), i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func pathOrRoot(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}
