package gcapi

// The triage broker's C2 surface: two calls over the same signed API as
// RunOrder, against gascity's city-scoped session routes (GASCITY_REF
// internal/api/supervisor_city_routes.go):
//
//   - CreateSession -> POST /v0/city/{city}/sessions  (mutating; signed)
//     For kind:"agent" this is ALWAYS async: it returns 202 {status,
//     request_id, event_cursor} and spawns the session in the background. It
//     accepts a unique `alias` (the correlation marker; rejected if taken) and
//     a `message` (stored as template_overrides.initial_message -- that IS the
//     prompt inject). One call thus creates + correlates + injects.
//
//   - GetSessionOutput -> GET /v0/city/{city}/session/{id}?peek=true&peekLines=N
//     (read; UNSIGNED). Resolves by id OR alias, so the alias we stamped at
//     create is a durable read handle. Returns the SessionView, whose
//     LastOutput carries the agent's sentinel-fenced proposed-effects batch.
//
// The pod holds no forge creds and the return is the session's own output --
// there is no bd write-back (S0 proved the pod has no working bd).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// CreateSessionRequest is the body of POST /v0/city/{city}/sessions. Field json
// tags MUST match gascity's sessionCreateBody exactly -- an unmatched key is
// silently ignored by the supervisor (a dropped alias => no correlation handle;
// a dropped message => no inject).
type CreateSessionRequest struct {
	Kind    string `json:"kind"`              // "agent" or "provider"
	Name    string `json:"name"`              // agent/provider name (e.g. "triage")
	Alias   string `json:"alias,omitempty"`   // unique correlation marker; server rejects a taken alias
	Message string `json:"message,omitempty"` // becomes template_overrides.initial_message (the inject)
	Async   bool   `json:"async,omitempty"`   // agent-kind creation is async-only upstream
}

// CreateSessionResult is the 202 async-accepted body. The session id is NOT
// here (it materializes on the city event stream keyed by RequestID); the alias
// we sent is the correlation handle, so the id is not needed to read back.
type CreateSessionResult struct {
	Status      string `json:"status"`
	RequestID   string `json:"request_id"`
	EventCursor string `json:"event_cursor"`
}

// SessionView is the subset of gascity's SessionView gonk reads. LastOutput is
// the last-output preview returned when peek=true -- where the agent's
// sentinel-fenced batch lands.
type SessionView struct {
	ID         string `json:"id"`
	Template   string `json:"template"`
	State      string `json:"state"`
	Alias      string `json:"alias"`
	Running    bool   `json:"running"`
	LastOutput string `json:"last_output"`
}

// CreateSession creates an agent (or provider) session and, for agents, injects
// the initial prompt via Message. It is a mutation and is signed exactly like
// RunOrder when a Signer is configured.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (*CreateSessionResult, error) {
	if c.City == "" {
		return nil, errEmptyCity
	}
	if req.Kind == "" {
		return nil, fmt.Errorf("gascity: CreateSession: kind is required (\"agent\" or \"provider\")")
	}
	if req.Name == "" {
		return nil, fmt.Errorf("gascity: CreateSession: name is required")
	}
	path := fmt.Sprintf("/v0/city/%s/sessions", url.PathEscape(c.City))
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gascity: CreateSession: encode body: %w", err)
	}
	body, err := c.doRequest(ctx, http.MethodPost, path, "", payload)
	if err != nil {
		return nil, err
	}
	var out CreateSessionResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: POST %s: decode: %w", path, err)
	}
	return &out, nil
}

// CloseSession tears a session down by id or alias: POST
// /v0/city/{city}/session/{id}/close. It is a mutation and is signed exactly
// like RunOrder and CreateSession.
//
// THIS IS THE ONLY CALL THAT RETURNS THE POD. A session that stopped, crashed or
// was judged and marked done still holds its pod, its 500m CPU and its 1Gi of
// memory, indefinitely. Nothing in gonk called this before 2026-08-03 and
// thirteen leaked pods took the cluster to the point where no new agent pod
// could be SCHEDULED at all (gonk-xkm, and the cause of gonk-pev).
//
// UNLIKE create and submit, this route is SYNCHRONOUS. Upstream's
// humaHandleSessionClose (GASCITY_REF internal/api/huma_handlers_sessions_command.go)
// calls handle.CloseDetailed INLINE and only then answers, so a 200 here is a
// real receipt rather than the 202 "we will get to it" that this tree has been
// burned by three times. That is why there is no event correlation on this path
// and there should not be one.
//
// A 404 is returned as an *APIError (IsNotFound == true) and callers should
// treat it as SUCCESS: a session that is already gone is the desired state, and
// close is idempotent by intent.
func (c *Client) CloseSession(ctx context.Context, idOrAlias string) error {
	if c.City == "" {
		return errEmptyCity
	}
	if idOrAlias == "" {
		return fmt.Errorf("gascity: CloseSession: id/alias is empty")
	}
	path := fmt.Sprintf("/v0/city/%s/session/%s/close", url.PathEscape(c.City), url.PathEscape(idOrAlias))
	if _, err := c.doRequest(ctx, http.MethodPost, path, "", nil); err != nil {
		return err
	}
	return nil
}

// GetSessionOutput reads one session by id or alias, asking the supervisor to
// include the last-output preview. peekLines <= 0 means "use the server
// default" (the param is omitted). It is an unsigned read; a 404 is an
// *APIError (IsNotFound == true) so a caller can distinguish "no session for
// this alias" from a transport failure.
func (c *Client) GetSessionOutput(ctx context.Context, idOrAlias string, peekLines int) (*SessionView, error) {
	if c.City == "" {
		return nil, errEmptyCity
	}
	if idOrAlias == "" {
		return nil, fmt.Errorf("gascity: GetSessionOutput: id/alias is empty")
	}
	path := fmt.Sprintf("/v0/city/%s/session/%s", url.PathEscape(c.City), url.PathEscape(idOrAlias))
	q := url.Values{}
	q.Set("peek", "true")
	if peekLines > 0 {
		q.Set("peekLines", strconv.Itoa(peekLines))
	}
	body, err := c.doRequest(ctx, http.MethodGet, path, q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out SessionView
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: GET %s: decode: %w", path, err)
	}
	return &out, nil
}
