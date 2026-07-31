package gcapi

// The transcript read. GetSessionOutput's peek is a bounded PREVIEW WINDOW --
// it answers "is this session still running, and roughly what has it said" --
// whereas the batch the broker must extract is the session's output of record.
// Reading a fenced batch out of a preview is silently lossy: an agent that is
// chatty after the fence pushes it out of the window, the broker sees "no
// batch", and the bead is re-slung onto a more expensive rung with nothing to
// show for it (gonk-u1p.3).
//
// Upstream's transcript resolves closed sessions too
// (resolveSessionIDAllowClosedWithConfig), which matters because the natural
// moment to read a batch is once the session has ended.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TranscriptTurn is one entry of a conversation-format transcript. Field tags
// match gascity's outputTurn exactly.
type TranscriptTurn struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// SessionTranscript is the subset of gascity's sessionTranscriptGetResponse
// gonk reads. The transcript is STRUCTURED TURNS, not a flat string -- a
// convenient flat shape would be a fiction that only works against a fake.
//
// Pagination is carried so a caller can SEE whether more remains rather than
// silently reading a prefix; gonk asks for every segment (tail=0) and a triage
// session is small, but a truncated read must be visible, not assumed away.
type SessionTranscript struct {
	ID         string           `json:"id"`
	Template   string           `json:"template"`
	Provider   string           `json:"provider"`
	Format     string           `json:"format"`
	Turns      []TranscriptTurn `json:"turns,omitempty"`
	Pagination *struct {
		HasMore bool `json:"has_more,omitempty"`
	} `json:"pagination,omitempty"`
}

// Text joins every turn's text in order. The agent's fenced batch lands in an
// assistant turn, and extractBatch takes the LAST fence, so concatenating in
// transcript order preserves the semantics the sentinel scan relies on.
func (t *SessionTranscript) Text() string {
	if t == nil {
		return ""
	}
	parts := make([]string, 0, len(t.Turns))
	for _, turn := range t.Turns {
		if turn.Text != "" {
			parts = append(parts, turn.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// GetSessionTranscript reads a session's full transcript by id or alias.
//
// tail=0 is REQUIRED, not incidental: upstream documents "0 returns all
// segments" while omitting the parameter returns only the most recent one, so a
// caller that forgets it silently reads a fragment. It is an unsigned read,
// like GetSessionOutput; a 404 is an *APIError (IsNotFound == true).
func (c *Client) GetSessionTranscript(ctx context.Context, idOrAlias string) (*SessionTranscript, error) {
	if c.City == "" {
		return nil, errEmptyCity
	}
	if idOrAlias == "" {
		return nil, fmt.Errorf("gascity: GetSessionTranscript: id or alias is required")
	}
	path := fmt.Sprintf("/v0/city/%s/session/%s/transcript",
		url.PathEscape(c.City), url.PathEscape(idOrAlias))
	body, err := c.doRequest(ctx, http.MethodGet, path, "tail=0", nil)
	if err != nil {
		return nil, err
	}
	var out SessionTranscript
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: GET %s: decode: %w", path, err)
	}
	return &out, nil
}
