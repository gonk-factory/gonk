package exportguard

import "fmt"

// Mode is which tree a scan is running in.
type Mode string

// The two modes.
const (
	// ModeInternal is the internal repository: the data directory is present
	// and the specific rules apply.
	ModeInternal Mode = "internal"
	// ModePublic is an exported tree: EXPORT-SOURCE is present, the data
	// directory is gone, and only the generic rules can apply.
	ModePublic Mode = "public"
)

// DetectMode decides the mode from two facts about a tree: whether the
// generated EXPORT-SOURCE file is present, and whether the data directory
// (SelfDir) is. Exactly one must hold. Both present is an export that kept
// its own deny data; both absent is a tree that is neither -- a scan there
// would run the generic rules only and call the result clean.
func DetectMode(exportSourcePresent, dataDirPresent bool) (Mode, error) {
	switch {
	case exportSourcePresent && dataDirPresent:
		return "", fmt.Errorf("both %s and %s are present: an exported tree may not carry the export data", ExportSourcePath, SelfDir)
	case !exportSourcePresent && !dataDirPresent:
		return "", fmt.Errorf("neither %s nor %s is present: cannot tell an internal tree from an exported one", ExportSourcePath, SelfDir)
	case exportSourcePresent:
		return ModePublic, nil
	}
	return ModeInternal, nil
}
