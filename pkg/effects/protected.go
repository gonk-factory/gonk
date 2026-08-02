package effects

// The protected-path denylist.
//
// WHY THIS EXISTS WHEN THE ALLOWLIST ALREADY COVERS IT. Today every file effect
// must sit under `.agent/` (see file.go), which already excludes everything
// here -- so this gate is, right now, redundant. It is here for the agent that
// does not exist yet.
//
// The first agent that legitimately edits source code needs a far wider
// allowlist than one prefix, and on the day someone writes it the incidental
// protection vanishes with no test failing and nothing to notice. A denylist
// refuses these paths ON THEIR OWN ACCOUNT, independently of whatever an
// agent's shape permits, so widening an allowlist cannot silently widen this.
//
// It is also the only DECIDABLE answer to a question the shape gate cannot
// otherwise touch: "did this run delete a CI job to turn a red pipeline green
// instead of fixing it?" That is a judgement about intent, and spec 6.3 forbids
// a model making it -- but "a non-CI agent may not edit CI definitions at all"
// is a fact a validator can check. See gonk-066 for the rest of that argument.

import (
	"fmt"
	"path"
	"strings"
)

// protectedExact are paths refused wherever they appear in the tree, matched on
// the final element. CODEOWNERS is meaningful in several locations, so it is
// matched by basename rather than by full path.
var protectedBase = map[string]bool{
	"CODEOWNERS": true, // changes who must approve the change
	".gonk.yml":  true, // gonk's own ladder, budget and permission to act
}

// protectedPrefixes are directory subtrees refused entirely.
var protectedPrefixes = []string{
	".gitlab/", // CI config, agent config, issue templates
	".github/", // the same, for a mirrored repo
	".git/",    // the repository's own machinery
}

// protectedExactPaths are refused only at these exact paths (a file with the
// same name deeper in the tree is ordinary content).
var protectedExactPaths = map[string]bool{
	".gitlab-ci.yml":  true,
	".gitlab-ci.yaml": true,
}

// ValidateProtectedPaths refuses file effects that touch repository control
// surfaces, regardless of the agent's shape or allowlist. It is a no-op for
// batches with no file effects.
func ValidateProtectedPaths(b Batch) error {
	for i, e := range b.Effects {
		if e.Kind != KindFile {
			continue
		}
		clean := path.Clean(e.Path)
		if protectedExactPaths[clean] {
			return protectedErr(i, e.Path, "a CI definition")
		}
		if protectedBase[path.Base(clean)] {
			return protectedErr(i, e.Path, "a repository control file")
		}
		for _, pfx := range protectedPrefixes {
			// Exact-prefix match on the trailing-slash form, so `.github/x` is
			// caught and `src/github/client.go` is not.
			if strings.HasPrefix(clean, pfx) {
				return protectedErr(i, e.Path, "a protected directory")
			}
		}
	}
	return nil
}

func protectedErr(i int, p, what string) error {
	return fmt.Errorf("effects: effect %d: path %q is %s and is protected -- no agent may propose changes to it", i, p, what)
}
