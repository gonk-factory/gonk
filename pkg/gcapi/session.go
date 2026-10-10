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
//   - GetSessionOutput -> GET /v0/city/{city}/session/{id}?peek=true&peek_lines=N
//     (read; UNSIGNED). Resolves by id OR alias, so the alias we stamped at
//     create is a durable read handle. Returns the SessionView, whose
//     LastOutput carries the agent's sentinel-fenced proposed-effects batch --
//     but ONLY while the session is Running (GASCITY_REF
//     internal/api/handler_sessions.go:604).
//
//     peek_lines is SNAKE_CASE and N is rows of SCROLLBACK, on top of the whole
//     visible pane, which the capture always includes. Both halves of that
//     sentence were wrong in this header until gonk-lv3y, and copying the route
//     out of it is how the bug would come back -- see GetSessionOutput.
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

// SessionSummary is the subset of one row of GET /v0/city/{city}/sessions that
// the orphan reaper needs. Deliberately small: this is a hostile-ish read used
// to decide what to DESTROY, so it takes only the fields the decision rests on.
type SessionSummary struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
	State string `json:"state"`
	// CreatedAt is RFC3339. It is what the reaper's grace window is measured
	// against, so an unparseable value must be treated as "too young to touch"
	// rather than "epoch, therefore ancient".
	CreatedAt string `json:"created_at"`
}

// SessionList is one page of the session list, in gascity's standard list
// envelope (internal/api/huma_types.go, ListBody).
type SessionList struct {
	Items      []SessionSummary `json:"items"`
	Total      int              `json:"total"`
	NextCursor string           `json:"next_cursor"`
	// Partial reports that one or more backends failed and the page is
	// INCOMPLETE. A caller that reaps must not read an incomplete list as
	// "these are all the sessions that exist".
	Partial bool `json:"partial"`
}

// ListSessions reads every OPEN-BEAD session in the city, FOLLOWING PAGINATION
// TO THE END.
//
// "Open-bead" is the load-bearing qualifier and it is not this client's choice:
// upstream builds the route from the session read model with IncludeClosed unset
// (GASCITY_REF internal/api/huma_handlers_sessions_query.go,
// humaHandleSessionList -> session.Store.ListAllWithResponses), so a session
// whose bead has been closed is absent from every page -- even when its pod is
// still running, which gascity's own create-rollback path can produce. Measured
// on the live city 2026-09-21: this route answered {"items":[],"total":0} while
// GET /v0/city/gonk/beads?type=session&all=true answered 832 rows.
//
// A CALLER THAT DESTROYS BY ABSENCE MUST NOT READ THIS AS "EVERY SESSION THAT
// EXISTS". It is every session that still has an open bead. gonk-wmb7 is what
// the difference costs.
//
// The pagination is not incidental. The route is keyset-paginated with a server
// cap, so a caller that reads one page and stops sees an arbitrary prefix -- and
// for the orphan reaper that is a silent under-reap: it would report success
// having missed most of what it was built to find. Exactly the failure shape
// this codebase keeps producing, so the loop lives HERE rather than being left
// to each caller to remember.
//
// partial reports whether ANY page came back incomplete, so a caller can decide
// what an incomplete answer means for it. It is returned rather than treated as
// an error because "some backends failed" is still useful to a reaper (it can
// only ever act on sessions it positively identified) and useless to hide.
//
// It is an unsigned read.
func (c *Client) ListSessions(ctx context.Context) (sessions []SessionSummary, partial bool, err error) {
	if c.City == "" {
		return nil, false, errEmptyCity
	}
	path := fmt.Sprintf("/v0/city/%s/sessions", url.PathEscape(c.City))
	cursor := ""
	// A hard bound on pages: a server that keeps handing back a cursor must not
	// spin this loop forever inside a 30s cooldown order.
	const maxPages = 50
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, rerr := c.doRequest(ctx, http.MethodGet, path, q.Encode(), nil)
		if rerr != nil {
			return nil, partial, rerr
		}
		var out SessionList
		if uerr := json.Unmarshal(body, &out); uerr != nil {
			return nil, partial, fmt.Errorf("gascity: GET %s: decode: %w", path, uerr)
		}
		sessions = append(sessions, out.Items...)
		partial = partial || out.Partial
		if out.NextCursor == "" {
			return sessions, partial, nil
		}
		cursor = out.NextCursor
	}
	// Ran out of pages. Report what we have AND say the list is incomplete --
	// silently truncating here would be the same bug as not paginating at all.
	return sessions, true, nil
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
// A 404 is returned as an *APIError (IsNotFound == true). IT DOES NOT MEAN THE
// POD IS GONE, and callers must not treat it as success. Upstream resolves the
// target with allowClosed=false (humaHandleSessionClose ->
// resolveSessionIDWithConfig), so a 404 means "no OPEN session bead answers to
// this name" -- which is also true of a session whose bead was closed while its
// runtime kept running. Such a pod can never be closed through this route, and
// ListSessions cannot see it either. gonk-wmb7 left one running for half an hour
// on the strength of the old reading of this comment.
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

// The peek read's size cap (gonk-lv3y).
//
// WHAT ACTUALLY GOVERNS THE SIZE, established against GASCITY_REF 4fda5a28
// (the SHA images/versions.env pins) rather than assumed:
//
//   - The supervisor serves the peek by capturing the agent's tmux pane:
//     `capture-pane -p -t <session> -S -<lines>`
//     (internal/runtime/tmux/tmux.go:2548). There is NO -E, so -S -N starts N
//     rows back in scrollback and captures THROUGH THE BOTTOM OF THE VISIBLE
//     PANE. The reply is therefore N + pane_height rows, not N. Measured on
//     the live agent pod: `capture-pane -p -S -400` on a 200-row pane returned
//     exactly 600 lines.
//   - Omitted or zero, the server uses defaultSessionPeekLines = 5
//     (internal/api/handler_sessions.go:540), and it only fills last_output at
//     all when the session is Running (handler_sessions.go:604).
//   - Both pane dimensions are constants of GONK'S OWN agent image
//     (images/agent/entrypoint.sh: `tmux resize-window -t main -x 4000 -y 200`,
//     gonk-d6h -- so that a one-line JSON batch is not broken mid-word by
//     wrapping).
//
// So the bound is (scrollback rows + pane rows) x pane columns, and every term
// in it is something gonk controls. That is what the cap is derived from. It
// is NOT derived from peekLines alone: on 2026-09-21 that would have been a
// bound computed from a number with no effect on anything (see the wire-name
// note on GetSessionOutput), which happened to be large enough to work.
//
// MEASURED, project 75 issue 79 attempt 1, 2026-09-21. 5 + 200 = 205 captured
// rows at the ~316 bytes/row a real exploration turn produces is ~65 KB --
// sitting directly on the 64 KiB generic cap. That is why the read succeeded
// for sixteen minutes and then failed on every tick afterwards: the pane's
// tail crossed a line it was always going to sit near. The supervisor answered
// 200 every time; this client refused the body, the sweep could not tell that
// refusal from an unreachable supervisor, and a finished triage run was denied
// with its verdict unread.
//
// The trust-boundary reason for maxResponseBytes (spec 9) is preserved: this
// is still a hard bound, still refused past it, and a misbehaving supervisor
// cannot grow it -- it is fixed by gonk's own pane geometry, not by anything
// the far side chooses. What changed is that the bound is now computed from
// the quantities that actually determine the payload.
const (
	// AgentPaneCols and AgentPaneRows are the tmux pane geometry
	// images/agent/entrypoint.sh configures. They are duplicated here because
	// this cap is meaningless without them --
	// TestPeekCapIsDerivedFromTheAgentPaneGeometry reads the entrypoint and
	// fails if they ever drift apart again, which is the precise way gonk-d6h
	// broke this read silently.
	//
	// THIS IS THE ONE COPY (gonk-z61b). They are exported so every model of
	// the pane -- gcapitest's fake capture, cmd/gonk-gate's peek-window
	// tests -- derives from this drift-guarded pair instead of restating it.
	// A copy that restates it is a copy a resize leaves silently stale.
	AgentPaneCols = 4000
	AgentPaneRows = 200
	// GascityDefaultPeekLines is the scrollback the supervisor uses when
	// peek_lines is omitted or zero (GASCITY_REF handler_sessions.go:540).
	GascityDefaultPeekLines = 5
	// GascityTranscriptPeekLines is the scrollback gascity's TRANSCRIPT read
	// captures when the provider resolves no session log for the session:
	// `handle.Peek(ctx, 100)` (GASCITY_REF 4fda5a28
	// internal/api/handler_session_transcript.go:140), served as format "text"
	// with one role "output" turn. That is the deployed agent's case -- every
	// transcript read in the live controller logs is roles "output=1", 1.5-2.5
	// KB -- so in production "the transcript" is a pane capture of this many
	// rows of scrollback plus the whole pane, NOT the session's whole output
	// (gonk-xflt).
	GascityTranscriptPeekLines = 100
	// AgentPaneHistoryLimit is how many rows of scrollback the agent's tmux
	// pane keeps: tmux's own default history-limit, because
	// images/agent/entrypoint.sh does not set one
	// (TestAgentPaneHistoryLimitIsTmuxDefault holds it to that). No capture of
	// any depth reaches further back than this plus the visible pane -- it is
	// the hard floor under every read that looks for a fence.
	AgentPaneHistoryLimit = 2000
	// AgentPaneHistoryGuaranteed is the least scrollback the pane holds once
	// its history has filled: tmux's grid_collect_history drops a TENTH of the
	// limit when it is reached, so a full history saws between
	// limit-limit/10+1 (1801) and limit (2000) rows depending on how much has
	// been printed. Coverage claims are made against THIS, the worst phase,
	// not the limit (gonk-xflt NEW-1; measured on tmux 3.2a, see gcapitest's
	// historyKept).
	AgentPaneHistoryGuaranteed = AgentPaneHistoryLimit - AgentPaneHistoryLimit/10 + 1
	// peekRowOverheadBytes covers the newline and JSON escaping per captured
	// row. Pane content is overwhelmingly printable ASCII, which escapes 1:1.
	//
	// It is an ASSUMPTION, and it can be wrong: 2000 double-width CJK glyphs
	// fill a 4000-column row and cost 6000 bytes, so a pane full of them
	// exceeds this budget by a comfortable margin. That is survivable rather
	// than alarming, and the reason is the shape of the fix rather than the
	// size of the number: an over-budget peek is REFUSED, the refusal is
	// named, brokerSessionView re-reads without the preview, and the fence is
	// recovered from the transcript. The proportionate cap and the fallback
	// defend each other -- the cap makes the fallback rare, and the fallback
	// makes the cap safe to be wrong about. Neither is load-bearing alone,
	// which is why both are tested alone (see the mutation matrix in
	// docs/plans/gonk-lv3y-session-response-too-large.md).
	peekRowOverheadBytes = 96
	// sessionViewEnvelopeBytes covers every field of the view other than
	// last_output. Measured live against gonk's own supervisor: the complete
	// peek-free view is 451 bytes, its largest field 62. 8 KiB is 18x that.
	sessionViewEnvelopeBytes = 8 << 10
)

// capturedPeekRows is how many rows the supervisor's capture-pane will return
// for a peek asking for peekLines rows of scrollback.
func capturedPeekRows(peekLines int) int {
	if peekLines < GascityDefaultPeekLines {
		peekLines = GascityDefaultPeekLines
	}
	return AgentPaneRows + peekLines
}

// SessionPeekMaxResponseBytes is the cap for a peek read.
//
// It is ~848 KiB for the default scrollback. That is large next to a
// control-plane reply and small next to what this same sweep tick already
// accepts one call later: GetSessionTranscript's 4 MiB. It is the honest worst
// case for a 4000x200 pane, and the typical case is ~65 KB.
func SessionPeekMaxResponseBytes(peekLines int) int64 {
	return sessionViewEnvelopeBytes + int64(capturedPeekRows(peekLines))*(AgentPaneCols+peekRowOverheadBytes)
}

// GetSessionOutput reads one session by id or alias, asking the supervisor to
// include the last-output preview. peekLines is rows of SCROLLBACK, on top of
// the whole visible pane, which the capture always includes; <= 0 means "use
// the server default" (the param is omitted, and the server uses 5). It is an
// unsigned read; a 404 is an *APIError (IsNotFound == true) so a caller can
// distinguish "no session for this alias" from a transport failure.
//
// THE QUERY PARAMETER IS peek_lines, NOT peekLines (gonk-lv3y). gonk sent
// camelCase from the day this was written; upstream binds snake_case
// (GASCITY_REF internal/api/huma_types_sessions.go:43,
// `query:"peek_lines" ... Defaults to 5.`) and `grep -rn '"peekLines"'` over
// the pinned clone has no hits at all. huma ignores unknown query parameters,
// so the supervisor answered 200 and quietly used its own default every single
// time -- which is why the bug looked like a 200 with a body nobody asked for.
// The spelling is fixed here; brokerPeekLines is set accordingly, since
// correcting it makes a previously inert number live.
//
// Its size cap is SessionPeekMaxResponseBytes(peekLines), not the generic one;
// a refusal satisfies IsResponseTooLarge, and GetSessionState is the read to
// fall back to, since nothing but last_output can be responsible for it.
func (c *Client) GetSessionOutput(ctx context.Context, idOrAlias string, peekLines int) (*SessionView, error) {
	return c.getSession(ctx, "GetSessionOutput", idOrAlias, true, peekLines)
}

// GetSessionState reads one session WITHOUT asking for the terminal preview:
// no peek, no last_output, just the control-plane view -- id, alias, state,
// running, template.
//
// It exists so that "the preview was bigger than we budgeted for" cannot cost
// a verdict (gonk-lv3y). Everything the sweep's finished/still-working
// decision rests on is in this view, and the batch it will judge comes from
// the transcript, not from the preview -- so a caller that cannot read the
// preview has lost an optimisation, not the answer.
//
// It is held to the generic maxResponseBytes, and that is deliberate: measured
// live, this view is 451 bytes, so 64 KiB is 145x headroom. A body that
// exceeds it here is not a peek that grew, it is a supervisor saying something
// unexpected, and that is exactly what the trust-boundary cap is for.
func (c *Client) GetSessionState(ctx context.Context, idOrAlias string) (*SessionView, error) {
	return c.getSession(ctx, "GetSessionState", idOrAlias, false, 0)
}

func (c *Client) getSession(ctx context.Context, who, idOrAlias string, peek bool, peekLines int) (*SessionView, error) {
	if c.City == "" {
		return nil, errEmptyCity
	}
	if idOrAlias == "" {
		return nil, fmt.Errorf("gascity: %s: id/alias is empty", who)
	}
	path := fmt.Sprintf("/v0/city/%s/session/%s", url.PathEscape(c.City), url.PathEscape(idOrAlias))
	q := url.Values{}
	limit := int64(maxResponseBytes)
	if peek {
		q.Set("peek", "true")
		if peekLines > 0 {
			q.Set("peek_lines", strconv.Itoa(peekLines))
		}
		limit = SessionPeekMaxResponseBytes(peekLines)
	}
	body, err := c.doRequestCapped(ctx, http.MethodGet, path, q.Encode(), nil, limit)
	if err != nil {
		return nil, err
	}
	var out SessionView
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: GET %s: decode: %w", path, err)
	}
	return &out, nil
}
