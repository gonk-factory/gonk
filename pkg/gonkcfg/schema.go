package gonkcfg

import (
	"bytes"
	_ "embed"
	"encoding/json"
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
	id := schemaID()
	c := jsonschema.NewCompiler()
	if err := c.AddResource(id, doc); err != nil {
		panic(err)
	}
	s, err := c.Compile(id)
	if err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema invalid: %v", err))
	}
	return s
}

// schemaID reads the schema's own "$id" and uses it as the resource name for
// AddResource/Compile. Without this, jsonschema/v6 resolves a bare filename
// against the process's working directory, and every validation error ends
// up citing a local, CWD-dependent filesystem path
// (file:///.../pkg/gonkcfg/gonk-config.v1.schema.json#) instead of the
// schema's published identity. Plan 02 echoes these errors into GitLab
// MR/issue comments, where a local path is both meaningless to the reader
// and a filesystem-layout leak.
func schemaID() string {
	var meta struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(schemaJSON, &meta); err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema unreadable: %v", err))
	}
	if meta.ID == "" {
		panic("gonkcfg: embedded schema has no $id")
	}
	return meta.ID
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
// float found as a scalar or as a map/slice VALUE. JSON has no NaN or
// Infinity, so a non-finite float is never a valid document -- reject
// generically rather than special-casing the one field whose "minimum"
// keyword we know reaches the crash today.
//
// This does not descend into map KEYS. That is not a gap in practice: a
// non-string key (which is what a non-finite float key would be, since
// yaml.v3 only produces map[any]any for those) makes the whole enclosing
// map non-JSON, and jsonschema/v6 rejects it wholesale ("invalid jsonType")
// at the top of every recursive validate() call -- before any keyword,
// including the crashing "minimum" comparison, runs on it or anything
// nested under it. Verified against jsonschema/v6 v6.0.2: typeOf falls
// through map[any]any to invalidType, and validator.validate() checks
// typeOf(v) == invalidType before evaluating any keyword.
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
