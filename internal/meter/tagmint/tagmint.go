// Package tagmint is the boundary where gonk's attribution tags are created.
//
// pkg/atags is a data CONTRACT: it checks that fields are present and that the
// trigger is known, and it deliberately accepts any string for project and
// rung. That is safe inside the package -- and unsafe the moment the values
// leave it. Tag values end up in LiteLLM request metadata, spend-log rows, Loki
// labels, Prometheus label values, JSON responses, and CSV exports. A newline
// in a project name is log injection; a comma is a shifted CSV column.
//
// gonk-meter is the ONLY thing that mints tags (POST /v1/policy/decide returns
// the metadata map and the pack stamps it verbatim), so this is the one place
// the charset gate has to exist. Plan 01 carry-forward: "enforce at the
// boundary, not in the contract package."
package tagmint

import (
	"fmt"
	"regexp"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
)

// maxLen bounds every tag value. Unbounded label values are a memory and
// cardinality problem in every downstream system that stores them.
const maxLen = 200

var (
	// GitLab path_with_namespace: segments of alnum/._- joined by /.
	reProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
	// Rung names match the .gonk.yml schema's ladder item pattern exactly.
	reRung = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// Bead IDs, session keys, rig names: printable, no separators that any
	// downstream serializer treats as structure.
	reID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Request is a tag-minting request. Attempt and Rung come from a rung.Decision;
// the rest come from the caller.
type Request struct {
	Project    string
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int
	Trigger    string
}

// Mint validates every field against a strict charset and returns tags that are
// safe to serialize anywhere. It ALSO runs atags.Validate, so the contract's own
// rules (non-empty, known trigger, attempt >= 1) still apply -- this is an
// additional gate, never a replacement.
func Mint(r Request) (atags.Tags, error) {
	for _, f := range []struct {
		name, value string
		pattern     *regexp.Regexp
	}{
		{"project", r.Project, reProject},
		{"rig", r.Rig, reID},
		{"bead_id", r.BeadID, reID},
		{"session_key", r.SessionKey, reID},
		{"rung", r.Rung, reRung},
	} {
		if err := check(f.name, f.value, f.pattern); err != nil {
			return atags.Tags{}, err
		}
	}
	t := atags.Tags{
		Project: r.Project, Rig: r.Rig, BeadID: r.BeadID,
		SessionKey: r.SessionKey, Rung: r.Rung, Attempt: r.Attempt,
		Trigger: r.Trigger,
	}
	if err := t.Validate(); err != nil {
		return atags.Tags{}, err
	}
	return t, nil
}

func check(name, value string, pattern *regexp.Regexp) error {
	if value == "" {
		return fmt.Errorf("tagmint: %s is empty", name)
	}
	if len(value) > maxLen {
		return fmt.Errorf("tagmint: %s is %d bytes, max %d", name, len(value), maxLen)
	}
	// Reject control characters and non-ASCII outright before the pattern, so
	// the error names the real problem (an invisible byte) rather than "does not
	// match ^[A-Za-z0-9]...". U+2028/U+2029 and the bidi overrides are exactly
	// the characters that survive a naive regexp and then break a log parser.
	for i, r := range value {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			return fmt.Errorf("tagmint: %s contains a control or non-ASCII character at byte %d (%U); "+
				"tag values are serialized into log lines, metadata headers, and CSV", name, i, r)
		}
	}
	if !pattern.MatchString(value) {
		return fmt.Errorf("tagmint: %s %q does not match %s", name, value, pattern)
	}
	return nil
}
