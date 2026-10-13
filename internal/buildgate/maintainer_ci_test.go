package buildgate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/internal/exportguard"
)

// The maintainers' pipeline definition, and the ONE place that reads it
// (gonk-wqd3).
//
// gonk is built twice. The public CI is .github/workflows/. The maintainers
// also run a GitLab pipeline of their own, defined in the file named below:
// it builds the images they deploy, on runners, a registry and CI variables
// that exist only at their site. That file is on the internal-only list and
// is NOT in the published tree, so a published test may not simply open it.
//
// Every gate in this package whose subject is that pipeline reads it through
// maintainerCI. The rule it holds:
//
//   - the file is there: it is read, and the gate runs. This is every
//     internal tree, always.
//   - the file is absent AND this is an exported tree (EXPORT-SOURCE is
//     present, release/ is gone -- exportguard.DetectMode, the same switch
//     the tree guard and the internal-reads check use): there is no such
//     pipeline here to gate, and the caller skips, saying so.
//   - the file is absent in any other tree: an ERROR. In the internal tree a
//     missing pipeline definition is a failure of every gate that reads it,
//     never a skip. That is what keeps the skip from hiding anything: it is
//     decided by which tree this is, not by the file being missing.
//
// A gate that ALSO checks published files (the lint pin, the shuffle flag)
// keeps checking those in an exported tree; only its GitLab leg is dropped.
const maintainerCIFile = ".gitlab-ci.yml"

// readMaintainerCI is the rule above as a function of a tree. present is
// false only for an exported tree that does not carry the file.
func readMaintainerCI(root string) (data []byte, present bool, err error) {
	data, err = os.ReadFile(filepath.Join(root, maintainerCIFile))
	if err == nil {
		return data, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("read %s: %w", maintainerCIFile, err)
	}
	_, srcErr := os.Stat(filepath.Join(root, exportguard.ExportSourcePath))
	_, dataErr := os.Stat(filepath.Join(root, exportguard.SelfDir))
	mode, modeErr := exportguard.DetectMode(srcErr == nil, dataErr == nil)
	if modeErr != nil {
		return nil, false, fmt.Errorf("%s is missing and this tree cannot be told apart: %w", maintainerCIFile, modeErr)
	}
	if mode != exportguard.ModePublic {
		return nil, false, fmt.Errorf("%s is missing from an INTERNAL tree (%s is present): the maintainers' pipeline "+
			"definition is gone, and every gate that reads it would otherwise check nothing", maintainerCIFile, exportguard.SelfDir)
	}
	return nil, false, nil
}

// maintainerCI returns the maintainers' pipeline definition. ok is false only
// in an exported tree; anything else that keeps it from being read fails the
// test.
func maintainerCI(t *testing.T) (data []byte, ok bool) {
	t.Helper()
	data, ok, err := readMaintainerCI(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return data, ok
}

// skipWithoutMaintainerCI is what a gate on the maintainers' pipeline says
// when it runs in an exported tree.
const skipWithoutMaintainerCI = "exported tree: the maintainers' GitLab pipeline definition is not published, " +
	"so there is no such pipeline here to gate (the public CI is .github/workflows/)"

func TestMaintainerCIIsSkippedOnlyInAnExportedTree(t *testing.T) {
	tree := func(files map[string]string) string {
		root, _ := scratchTree(t, files)
		return root
	}
	const pipeline = "stages: [test]\n"

	// Internal tree, file present: read.
	data, present, err := readMaintainerCI(tree(map[string]string{
		exportguard.ListPath: scratchList, maintainerCIFile: pipeline}))
	if err != nil || !present || string(data) != pipeline {
		t.Errorf("internal tree with the file: data=%q present=%v err=%v", data, present, err)
	}

	// Internal tree, file missing: an error, never a skip.
	_, present, err = readMaintainerCI(tree(map[string]string{exportguard.ListPath: scratchList}))
	if err == nil || present || !strings.Contains(err.Error(), "INTERNAL tree") {
		t.Errorf("internal tree WITHOUT the file must be an error: present=%v err=%v", present, err)
	}

	// Exported tree, file missing: absent, no error -- the one skip.
	_, present, err = readMaintainerCI(tree(map[string]string{exportguard.ExportSourcePath: "generated\n"}))
	if err != nil || present {
		t.Errorf("exported tree without the file: present=%v err=%v; want absent and no error", present, err)
	}

	// A tree that is neither (no EXPORT-SOURCE, no release/): an error. A
	// copy of the sources that lost both must not read as "nothing to gate".
	_, present, err = readMaintainerCI(tree(map[string]string{"Makefile": "gate:\n"}))
	if err == nil || present || !strings.Contains(err.Error(), "neither") {
		t.Errorf("a tree that is neither must be an error: present=%v err=%v", present, err)
	}

	// A tree that is both: an error.
	_, present, err = readMaintainerCI(tree(map[string]string{
		exportguard.ListPath: scratchList, exportguard.ExportSourcePath: "generated\n"}))
	if err == nil || present || !strings.Contains(err.Error(), "both") {
		t.Errorf("a tree that is both must be an error: present=%v err=%v", present, err)
	}

	// The file present in an exported tree is read like anywhere else.
	data, present, err = readMaintainerCI(tree(map[string]string{
		exportguard.ExportSourcePath: "generated\n", maintainerCIFile: pipeline}))
	if err != nil || !present || string(data) != pipeline {
		t.Errorf("exported tree that carries the file: data=%q present=%v err=%v", data, present, err)
	}
}

// The real tree is one of the two, and says which: an internal tree HAS the
// pipeline definition. This is the assertion that fails if the file is
// deleted or renamed here while the gates on it quietly stop running.
func TestThisTreeHasTheMaintainerCIOrIsExported(t *testing.T) {
	data, ok := maintainerCI(t)
	if !ok {
		t.Skip(skipWithoutMaintainerCI)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty", maintainerCIFile)
	}
}
