package glab

import (
	"context"
	"fmt"
	"net/url"
)

func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var u User
	if err := c.getJSON(ctx, request{method: "GET", path: "/api/v4/user"}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListMemberProjects returns every project the token's user is a member of
// (spec 5.2). This is the entry point of reconciliation.
func (c *Client) ListMemberProjects(ctx context.Context) ([]Project, error) {
	return paginate[Project](ctx, c, request{
		method: "GET",
		path:   "/api/v4/projects",
		query:  map[string]string{"membership": "true", "order_by": "id", "sort": "asc"},
	})
}

// GetProject fetches one project. The broker needs its DefaultBranch: a
// scaffold commit branches from it, and assuming "main" would silently target
// the wrong base on any repository that still uses "master" or anything else.
func (c *Client) GetProject(ctx context.Context, projectID int64) (*Project, error) {
	var p Project
	if err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d", projectID),
	}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetRawFile fetches a file's bytes at a ref. maxBytes is enforced by the
// client: the caller passes the .gonk.yml cap so a hostile 2 GiB file cannot be
// read into memory. Returns an error satisfying IsNotFound when absent.
func (c *Client) GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error) {
	p := fmt.Sprintf("/api/v4/projects/%d/repository/files/%s/raw", projectID, url.PathEscape(path))
	body, _, err := c.do(ctx, request{method: "GET", path: p, query: map[string]string{"ref": ref}, maxBytes: maxBytes})
	return body, err
}

// RepoArchive fetches the repository tree at ref as a GZIPPED TAR.
//
// This is how an agent pod gets a checkout without holding a forge credential:
// the CONTROLLER side calls this with its own PAT and serves the bytes on an
// in-cluster endpoint, and the pod fetches from gonk rather than from the forge
// (pkg/rig, gonk-msz).
//
// maxBytes is enforced by the client and matters more here than anywhere else in
// this package: a repository is unbounded, caller-controlled, and the response is
// read into memory. Pass a real cap, never 0.
func (c *Client) RepoArchive(ctx context.Context, projectID int64, ref string, maxBytes int64) ([]byte, error) {
	p := fmt.Sprintf("/api/v4/projects/%d/repository/archive.tar.gz", projectID)
	body, _, err := c.do(ctx, request{
		method:   "GET",
		path:     p,
		query:    map[string]string{"sha": ref},
		maxBytes: maxBytes,
	})
	return body, err
}

// DirExists reports whether a directory exists at ref (used for .agent/).
func (c *Client) DirExists(ctx context.Context, projectID int64, path, ref string) (bool, error) {
	var entries []struct {
		Name string `json:"name"`
	}
	err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/tree", projectID),
		query:  map[string]string{"path": path, "ref": ref, "per_page": "1"},
	}, &entries)
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func (c *Client) ListMembers(ctx context.Context, projectID int64) ([]Member, error) {
	return paginate[Member](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/members/all", projectID),
	})
}
