package glab

import (
	"context"
	"fmt"
)

// ListHooks, CreateHook, and EditHook all set admin: true so split-credential
// mode (AD-4) uses the admin token for hook management only.

func (c *Client) ListHooks(ctx context.Context, projectID int64) ([]Hook, error) {
	return paginate[Hook](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/hooks", projectID),
		admin:  true,
	})
}

func (c *Client) CreateHook(ctx context.Context, projectID int64, opts HookOptions) (*Hook, error) {
	var h Hook
	if err := c.getJSON(ctx, request{
		method: "POST",
		path:   fmt.Sprintf("/api/v4/projects/%d/hooks", projectID),
		body:   opts,
		admin:  true,
	}, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (c *Client) EditHook(ctx context.Context, projectID, hookID int64, opts HookOptions) (*Hook, error) {
	var h Hook
	if err := c.getJSON(ctx, request{
		method: "PUT",
		path:   fmt.Sprintf("/api/v4/projects/%d/hooks/%d", projectID, hookID),
		body:   opts,
		admin:  true,
	}, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
