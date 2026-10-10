package effects

import (
	"fmt"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/gonk-factory/gonk/pkg/scope"
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
// TrajectorySection is the effect-shape.toml key carrying trajectory
// predicates. Declared here, beside the loader that must skip it, so the two
// cannot drift apart silently.
const TrajectorySection = "trajectory"

// ScopeSection is the effect-shape.toml key carrying the agent's workflow
// scope row (gonk-066 S3): which path classes it may write, what a dispatch
// may lift, and its size ceilings. It is read by pkg/scope.LoadRow -- from the
// same baked file -- and skipped here for the same reason as [trajectory].
const ScopeSection = scope.PackSection

func LoadShape(packDir, agent string) (Shape, error) {
	path := filepath.Join(packDir, "agents", agent, "effect-shape.toml")
	var raw map[string]tomlCard
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Shape{}, fmt.Errorf("effects: load shape %s: %w", path, err)
	}
	kinds := make(map[Kind]Card, len(raw))
	for k, c := range raw {
		// [trajectory] and [scope] are the TWO non-kind sections this file may
		// carry. [trajectory]: what the agent must have DONE to be allowed to say
		// what it says (gonk-hsb), read by pkg/trace. [scope]: which paths its
		// change may touch (gonk-066), read by pkg/scope. Each is skipped by
		// name rather than tolerated generally -- the "unknown kind" error below is what catches a typo'd
		// kind, and that must keep working.
		if k == TrajectorySection || k == ScopeSection {
			continue
		}
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
