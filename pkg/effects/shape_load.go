package effects

import (
	"fmt"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// tomlCard is the on-disk form of a Card. max = -1 means unbounded (N).
type tomlCard struct {
	Min int `toml:"min"`
	Max int `toml:"max"`
}

// LoadShape reads agents/<agent>/effect-shape.toml from the baked pack and
// yields a Shape. Each top-level table is an effect kind ({min,max}); a max of
// -1 maps to N (unbounded). An unknown kind key is an error -- the loader fails
// loud rather than silently forbidding a mistyped kind.
//
// IMPORTANT: the broker reads this from the baked pack (/opt/gonk/pack), never
// the /city copy -- reading from the baked pack sidesteps the `gc init`
// block-stripping that would strip broker-only data (spec §6.1).
func LoadShape(packDir, agent string) (Shape, error) {
	path := filepath.Join(packDir, "agents", agent, "effect-shape.toml")
	var raw map[string]tomlCard
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Shape{}, fmt.Errorf("effects: load shape %s: %w", path, err)
	}
	kinds := make(map[Kind]Card, len(raw))
	for k, c := range raw {
		kind := Kind(k)
		if !knownKinds[kind] {
			return Shape{}, fmt.Errorf("effects: shape %s has unknown kind %q", path, k)
		}
		max := c.Max
		if max == -1 {
			max = N
		}
		kinds[kind] = Card{Min: c.Min, Max: max}
	}
	return Shape{Kinds: kinds}, nil
}
