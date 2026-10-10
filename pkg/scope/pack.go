package scope

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// PackSection is the effect-shape.toml table holding an agent's pack row.
// pkg/effects.LoadShape skips it (as it skips [trajectory]) by this name.
const PackSection = "scope"

// packRow is the on-disk [scope] table. Caps are pointers so that "absent" is
// distinguishable from 0, which is a real cap (max_deletes = 0).
type packRow struct {
	Classes       []Class `toml:"classes"`
	Liftable      []Class `toml:"liftable"`
	MaxFiles      *int64  `toml:"max_files"`
	MaxLines      *int64  `toml:"max_lines"`
	MaxDeletes    *int64  `toml:"max_deletes"`
	MaxFileBytes  *int64  `toml:"max_file_bytes"`
	MaxPatchBytes *int64  `toml:"max_patch_bytes"`
}

// LoadRow reads an agent's pack row: the [scope] table of
// <packDir>/agents/<agent>/effect-shape.toml.
//
// THE PATH IS THE SECURITY PROPERTY, as for LoadShape and trace.LoadPolicy:
// the broker passes the BAKED pack (/opt/gonk/pack), never the /city copy and
// never anything a session can write. This is the only file this package
// opens.
//
// A missing file or a missing [scope] table is not an error: the agent has no
// row, and the zero Row allows nothing -- which is every agent that exists
// today. A table that IS there must be complete and exact: an unknown key, an
// unknown or protected class, a non-liftable `liftable`, or a row that allows
// something without stating all five caps is an error, so a typo cannot
// quietly become "no limit" or "limit 0".
func LoadRow(packDir, agent string) (Row, error) {
	path := filepath.Join(packDir, "agents", agent, "effect-shape.toml")
	var doc struct {
		Scope *packRow `toml:"scope"`
	}
	md, err := toml.DecodeFile(path, &doc)
	if err != nil {
		if os.IsNotExist(err) {
			return Row{}, nil
		}
		return Row{}, fmt.Errorf("scope: load pack row %s: %w", path, err)
	}
	if doc.Scope == nil {
		return Row{}, nil
	}
	for _, k := range md.Undecoded() {
		if len(k) > 0 && k[0] == PackSection {
			return Row{}, fmt.Errorf("scope: load pack row %s: unknown key %q", path, k.String())
		}
	}
	r := doc.Scope
	row := Row{Classes: r.Classes, Liftable: r.Liftable}
	for _, c := range []struct {
		key string
		v   *int64
		dst *int64
	}{{"max_files", r.MaxFiles, &row.Caps.MaxFiles}, {"max_lines", r.MaxLines, &row.Caps.MaxLines}, {"max_deletes", r.MaxDeletes, &row.Caps.MaxDeletes},
		{"max_file_bytes", r.MaxFileBytes, &row.Caps.MaxFileBytes}, {"max_patch_bytes", r.MaxPatchBytes, &row.Caps.MaxPatchBytes}} {
		if c.v == nil {
			if len(r.Classes)+len(r.Liftable) > 0 {
				return Row{}, fmt.Errorf("scope: load pack row %s: %s is not set; a row that allows a class must state every cap", path, c.key)
			}
			continue
		}
		*c.dst = *c.v
	}
	if err := row.validate(); err != nil {
		return Row{}, fmt.Errorf("scope: load pack row %s: %w", path, err)
	}
	return row, nil
}
