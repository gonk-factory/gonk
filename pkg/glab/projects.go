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

// GetRawFile fetches a file's bytes at a ref. maxBytes is enforced by the
// client: the caller passes the .gonk.yml cap so a hostile 2 GiB file cannot be
// read into memory. Returns an error satisfying IsNotFound when absent.
func (c *Client) GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error) {
	p := fmt.Sprintf("/api/v4/projects/%d/repository/files/%s/raw", projectID, url.PathEscape(path))
	body, _, err := c.do(ctx, request{method: "GET", path: p, query: map[string]string{"ref": ref}, maxBytes: maxBytes})
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
