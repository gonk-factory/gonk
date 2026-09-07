package intake

import (
	"fmt"
	"strconv"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// Blocklist names projects gonk must never work on, however it came to see them.
//
// WHY THIS IS THE GUARD, and not membership scope (gonk-jn5). It is tempting to
// say "gonk only sees projects somebody added it to", but that caps the design
// at the project tier: an instance-level budget ceiling can only be enforced by
// something that sees instance-level draw, so gonk may legitimately need to
// watch every project on an instance. Broad visibility is a REQUIREMENT, which
// makes narrow visibility the wrong safety property.
//
// It is also a property gonk does not hold. Membership is granted in GitLab's
// UI. `membership=true` includes projects inherited through GROUP membership,
// and gonk-project lives in the same `agentic` group as the repos gonk is meant
// to work on -- so adding the bot to that group would make gonk watch its own
// broker, gate and effect-shape.toml, arriving as a side effect of onboarding
// rather than as a decision. That is exactly the scenario gonk-066's workflow
// scope gate exists to prevent.
//
// So the blocklist is enforced where gonk DOES hold the property: in its own
// process, at both the reconcile and dispatch layers.
type Blocklist struct {
	paths map[string]struct{}
	ids   map[int64]struct{}
	raw   []string
}

// ParseBlocklist reads entries that are either a full path with namespace
// ("agentic/gonk-project") or a numeric project id ("69").
//
// FAILS CLOSED BY CONSTRUCTION: it returns an error rather than a partial list,
// and callers must treat that as fatal. An entry nobody can parse is an entry
// that would silently stop blocking, and "the list was malformed so we watched
// everything" is the outcome this whole bead exists to prevent.
func ParseBlocklist(entries []string) (Blocklist, error) {
	b := Blocklist{paths: map[string]struct{}{}, ids: map[int64]struct{}{}}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		b.raw = append(b.raw, e)
		if id, err := strconv.ParseInt(e, 10, 64); err == nil {
			if id <= 0 {
				return Blocklist{}, fmt.Errorf("blocklist: %q is not a positive project id", e)
			}
			b.ids[id] = struct{}{}
			continue
		}
		// A path must look like a path. Accepting a bare word would silently
		// match nothing -- a typo'd "gonk-project" instead of
		// "agentic/gonk-project" would parse fine and block nothing, which is
		// the failure this rejects.
		if !strings.Contains(e, "/") {
			return Blocklist{}, fmt.Errorf(
				"blocklist: %q is neither a numeric project id nor a path with namespace "+
					"(want e.g. \"agentic/gonk-project\"); a bare name would match nothing and "+
					"block nothing", e)
		}
		b.paths[strings.ToLower(e)] = struct{}{}
	}
	return b, nil
}

// Blocked reports whether this project is on the list. Matching is by path
// (case-insensitively, because GitLab paths are case-preserving but compared
// case-insensitively) or by numeric id, so a rename does not silently unblock a
// project that was listed by id, and a re-created project does not silently
// unblock one listed by path.
func (b Blocklist) Blocked(p glab.Project) bool {
	if _, ok := b.ids[p.ID]; ok {
		return true
	}
	_, ok := b.paths[strings.ToLower(p.PathWithNamespace)]
	return ok
}

// Entries returns the list as configured, for logging at startup. An operator
// should be able to see what is blocked without reading the config back out of
// a pod.
func (b Blocklist) Entries() []string { return append([]string(nil), b.raw...) }

// Empty reports whether nothing is blocked. Callers log a warning: an empty
// blocklist is legal but is almost never what someone meant on an instance
// where gonk can see its own repository.
func (b Blocklist) Empty() bool { return len(b.paths) == 0 && len(b.ids) == 0 }
