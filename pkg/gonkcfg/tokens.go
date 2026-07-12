// Package gonkcfg is the contract for .gonk.yml: schema validation, typed
// loading, and instance -> group -> project precedence resolution.
package gonkcfg

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TokenQuantity is a token count. In YAML it is either a non-negative
// integer or a string matching ^[0-9]+[KMG]?$ (decimal multipliers 1e3,
// 1e6, 1e9). The string form exists so ".gonk.yml" authors can write "50M"
// without YAML numeric-parsing ambiguity (spec 5.4).
type TokenQuantity int64

var multipliers = map[byte]int64{'K': 1e3, 'M': 1e6, 'G': 1e9}

func ParseTokenQuantity(s string) (TokenQuantity, error) {
	if s == "" {
		return 0, fmt.Errorf("token quantity: empty")
	}
	mult := int64(1)
	digits := s
	if m, ok := multipliers[s[len(s)-1]]; ok {
		mult, digits = m, s[:len(s)-1]
	}
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, fmt.Errorf("token quantity %q: want ^[0-9]+[KMG]?$", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("token quantity %q: %w", s, err)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("token quantity %q: overflow", s)
	}
	return TokenQuantity(n * mult), nil
}

// UnmarshalYAML accepts integer or suffixed-string scalars, and null (which
// leaves the value unset at zero). It dispatches on the node's resolved tag
// rather than on decode success: yaml.v3 will happily decode a !!float into an
// int64 by truncating, so "1.5" would silently become a 1-token budget.
func (q *TokenQuantity) UnmarshalYAML(node *yaml.Node) error {
	switch tag := node.ShortTag(); tag {
	case "!!null":
		// An explicit null means "unset"; leave *q at its zero value.
		return nil
	case "!!int":
		var i int64
		if err := node.Decode(&i); err != nil {
			return fmt.Errorf("token quantity %q: %w", node.Value, err)
		}
		if i < 0 {
			return fmt.Errorf("token quantity %q: negative", node.Value)
		}
		*q = TokenQuantity(i)
		return nil
	case "!!str":
		var s string
		if err := node.Decode(&s); err != nil {
			return fmt.Errorf("token quantity %q: %w", node.Value, err)
		}
		v, err := ParseTokenQuantity(s)
		if err != nil {
			return err
		}
		*q = v
		return nil
	default:
		return fmt.Errorf("token quantity: want integer or string, got %s", tag)
	}
}
