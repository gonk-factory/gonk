package trace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// LoadPolicy reads an agent's [trajectory] predicates from the BAKED pack.
//
// THE PATH IS THE SECURITY PROPERTY. It reads /opt/gonk/pack/agents/<agent>/,
// exactly as the effect shape does, and never anything the agent can write.
// Predicates loaded from a path the session controls would let a run declare
// its own requirements away -- the same self-widening loophole pkg/verify
// closes by reading test declarations from the base commit.
//
// A MISSING FILE OR A MISSING SECTION IS NOT AN ERROR: it means this agent
// declares no trajectory requirements, which Classify reports as no-policy.
// Erroring would make adding trajectory to one agent break every other.
func LoadPolicy(packDir, agent string) (Policy, error) {
	path := filepath.Join(packDir, "agents", agent, "effect-shape.toml")
	var doc struct {
		Trajectory struct {
			RequiredTools        []string       `toml:"required_tools"`
			ForbiddenTools       []string       `toml:"forbidden_tools"`
			MinCalls             map[string]int `toml:"min_calls"`
			RequireTargetReadFor []string       `toml:"require_target_read_for"`
			RequireAnyReadFor    []string       `toml:"require_any_read_for"`
			ReadTools            []string       `toml:"read_tools"`
		} `toml:"trajectory"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		if os.IsNotExist(err) {
			return Policy{}, nil
		}
		return Policy{}, fmt.Errorf("trace: load trajectory policy %s: %w", path, err)
	}
	t := doc.Trajectory
	return Policy{
		RequiredTools:        t.RequiredTools,
		ForbiddenTools:       t.ForbiddenTools,
		MinCalls:             t.MinCalls,
		RequireTargetReadFor: t.RequireTargetReadFor,
		RequireAnyReadFor:    t.RequireAnyReadFor,
		ReadTools:            t.ReadTools,
	}, nil
}
