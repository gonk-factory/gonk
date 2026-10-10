package glab

import (
	"context"
	"fmt"
	"strings"
)

// Repository tree read WITH MODE (gonk-cuwn X-GLAB2, plan hypothesis H4). A GET.

// Git tree entry modes. A blob's content says nothing about its mode: a
// symlink's blob is its target path as text, perfectly editable UTF-8.
const (
	ModeRegular    = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	ModeSubmodule  = "160000"
	ModeTree       = "040000"
)

// TreeEntry is one entry of a repository tree.
type TreeEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is GitLab's: blob, tree or commit (a submodule).
	Type string `json:"type"`
	// Path is the full repository path.
	Path string `json:"path"`
	// Mode is the git mode as an octal string.
	Mode string `json:"mode"`
}

// IsRegularFile reports a blob whose mode is 100644 or 100755: the only kind of
// entry whose content an edit may be built on.
func (e TreeEntry) IsRegularFile() bool {
	return e.Type == "blob" && (e.Mode == ModeRegular || e.Mode == ModeExecutable)
}

// IsDir reports a tree (directory) entry.
func (e TreeEntry) IsDir() bool { return e.Type == "tree" && e.Mode == ModeTree }

// ListTree returns EVERY entry directly under dir ("" is the repository root;
// not recursive) at ref, walking all pages. A missing dir at a valid ref is a
// 404 (IsNotFound): the caller decides whether that means "absent", which it
// cannot tell apart from an unknown ref without a second read of the root.
//
// Each entry is asserted on: its type and mode must agree (blob with a file or
// symlink mode, tree with 040000, commit with 160000), its name must be
// non-empty, and its path must be dir/name. A 200 whose body is an error object
// fails to decode as the array it should be; an array of the wrong objects
// fails here. Neither can come back as an empty or partial tree.
func (c *Client) ListTree(ctx context.Context, projectID int64, ref, dir string) ([]TreeEntry, error) {
	if ref == "" {
		return nil, fmt.Errorf("gitlab: list tree: empty ref")
	}
	if strings.HasPrefix(dir, "/") || strings.HasSuffix(dir, "/") {
		return nil, fmt.Errorf("gitlab: list tree: dir %q must be clean and relative", dir)
	}
	q := map[string]string{"ref": ref}
	if dir != "" {
		q["path"] = dir
	}
	path := fmt.Sprintf("/api/v4/projects/%d/repository/tree", projectID)
	es, err := paginate[TreeEntry](ctx, c, request{method: "GET", path: path, query: q})
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if err := checkTreeEntry(e, dir); err != nil {
			return nil, fmt.Errorf("gitlab: GET %s ref=%s path=%q: %w", path, ref, dir, err)
		}
	}
	return es, nil
}

func checkTreeEntry(e TreeEntry, dir string) error {
	if e.Name == "" || e.Mode == "" || e.Type == "" {
		return fmt.Errorf("entry %+v lacks name, type or mode", e)
	}
	want := e.Name
	if dir != "" {
		want = dir + "/" + e.Name
	}
	if e.Path != want {
		return fmt.Errorf("entry %q has path %q, want %q", e.Name, e.Path, want)
	}
	switch e.Type {
	case "blob":
		if e.Mode != ModeRegular && e.Mode != ModeExecutable && e.Mode != ModeSymlink {
			return fmt.Errorf("blob %q has mode %q", e.Path, e.Mode)
		}
	case "tree":
		if e.Mode != ModeTree {
			return fmt.Errorf("tree %q has mode %q", e.Path, e.Mode)
		}
	case "commit":
		if e.Mode != ModeSubmodule {
			return fmt.Errorf("commit %q has mode %q", e.Path, e.Mode)
		}
	default:
		return fmt.Errorf("entry %q has type %q", e.Path, e.Type)
	}
	return nil
}
