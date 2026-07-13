package gonkcfg

import (
	"bytes"
	_ "embed"
	"fmt"

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
	if err := compiled.Validate(doc); err != nil {
		return fmt.Errorf(".gonk.yml: %w", err)
	}
	return nil
}
