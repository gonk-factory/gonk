package effects

// The `file` effect and its path gate.
//
// Scaffold is the second trigger ported to the broker, and it is the first one
// whose artifact is repository CONTENT rather than a comment. The agent holds
// no forge credentials -- that is the whole premise -- so it cannot create a
// branch or open a merge request itself. It proposes files; the broker commits
// them onto `gonk/scaffold` and opens exactly one MR.
//
// THAT MAKES THIS FILE A SECURITY BOUNDARY. The paths and the bytes are model
// output derived from an untrusted repository, and the broker writes them into
// a real branch under the controller's own credentials. The prompt asks for
// `.agent/`; this is what ENFORCES it. Everything here is deterministic and
// pure, so the decision is reproducible in tests and no model judges it.

import (
	"fmt"
	"path"
	"strings"
)

// AgentDirPrefix is the ONLY writable prefix for a file effect. Deliberately a
// constant and not configuration: a knob that widens where an agent-authored
// commit may land is a knob that will one day be set to "".
//
// Note what this keeps out beyond the obvious: `.gonk.yml` (a run could
// otherwise rewrite its own budget and ladder) and any CI definition
// (`.gitlab-ci.yml`, `.github/`), which is arbitrary code execution wearing a
// commit.
const AgentDirPrefix = ".agent/"

const (
	// MaxFileBytes caps one proposed file. Generous for prose context, far
	// below anything that would bloat a repository.
	MaxFileBytes = 64 << 10 // 64 KiB
	// MaxBatchBytes caps the whole batch. The per-file cap alone does not bound
	// a batch -- a thousand small files is the same problem as one huge one.
	MaxBatchBytes = 256 << 10 // 256 KiB
)

// ValidatePaths enforces the file-effect rules on a batch. It is a no-op for
// batches with no file effects, so the broker can call it unconditionally.
//
// It is separate from Validate (cardinality) because they answer different
// questions: Validate says "is this the right SHAPE of batch for this agent",
// and this says "are these writes allowed at all". A batch must pass both.
func ValidatePaths(b Batch) error {
	// The denylist first, and unconditionally. It is redundant against today's
	// .agent/-only allowlist by design -- see protected.go for why it must not
	// depend on that allowlist staying narrow.
	if err := ValidateProtectedPaths(b); err != nil {
		return err
	}

	seen := make(map[string]bool)
	total := 0

	for i, e := range b.Effects {
		if e.Kind != KindFile {
			continue
		}
		if strings.TrimSpace(e.Path) == "" {
			return fmt.Errorf("effects: effect %d: file effect has no path", i)
		}

		// path.Clean resolves `.` and `..` BEFORE the prefix check, so
		// ".agent/../.gitlab-ci.yml" is judged as what it actually writes.
		// Checking the raw string first would pass it.
		clean := path.Clean(e.Path)
		if clean != e.Path {
			return fmt.Errorf("effects: effect %d: path %q is not in canonical form (cleans to %q)", i, e.Path, clean)
		}
		if path.IsAbs(clean) {
			return fmt.Errorf("effects: effect %d: path %q is absolute", i, e.Path)
		}
		// HasPrefix on the trailing-slash form, so ".agentx/..." cannot pass by
		// sharing a prefix with ".agent".
		if !strings.HasPrefix(clean, AgentDirPrefix) {
			return fmt.Errorf("effects: effect %d: path %q is outside %s -- a scaffold may only write there", i, e.Path, AgentDirPrefix)
		}
		if seen[clean] {
			return fmt.Errorf("effects: effect %d: duplicate path %q -- the batch disagrees with itself", i, e.Path)
		}
		seen[clean] = true

		if len(e.Content) > MaxFileBytes {
			return fmt.Errorf("effects: effect %d: %q is %d bytes, over the %d-byte per-file cap", i, e.Path, len(e.Content), MaxFileBytes)
		}
		total += len(e.Content)
		if total > MaxBatchBytes {
			return fmt.Errorf("effects: batch content is over the %d-byte cap", MaxBatchBytes)
		}
	}
	return nil
}
