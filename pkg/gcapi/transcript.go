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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// transcriptMaxResponseBytes is GetSessionTranscript's OWN cap, distinct from
// the generic maxResponseBytes (64 KiB) every other route is held to.
//
// A triage transcript is not a small supervisor control-plane reply: the
// triage prompt now tells the model to list directories and read code before
// answering, and a real session's transcript -- every turn, tool call and
// tool result, concatenated -- routinely clears 64 KiB. Judging that session
// against the generic cap does not fail closed in any useful sense: it turns
// a real, complete batch into a transport error, the sweep re-slings the bead
// onto a pricier rung, and the agent's finished work goes unread (R-10).
//
// 4 MiB is generous headroom over an observed real transcript without being
// unbounded -- GetSessionTranscript still refuses to read an arbitrarily
// large body into memory whole; IsTranscriptTooLarge names the refusal.
//
// Buffering, not streaming: like every other gcapi read, readCapped reads the
// whole body into one []byte before returning (io.ReadAll over a LimitReader),
// and json.Unmarshal then copies every turn's text again into the decoded
// SessionTranscript. The broker's own read of it (broker_apply.go) calls
// Text() TWICE -- once for isUnreadableTranscript, once for extractBatch --
// and Text() rebuilds a fresh joined copy of the whole transcript each time
// (strings.Join over every turn) rather than caching it. So a single sweep's
// judgment of one session can transiently hold up to FOUR near-cap-sized
// copies of the same text live at once (the raw HTTP body, the decoded turn
// strings, and two independent Text() joins) -- worst case on the order of
// 16 MiB for one session at the 4 MiB cap, not the ~4 MiB the cap number
// alone suggests. gonk-gate's sweep judges sessions one at a time (no
// per-tick fan-out today), so this is a per-sweep-tick peak, not something
// that multiplies across sessions within one process -- but it is a real
// number an operator sizing the sweep pod should know, and it is not changed
// by this task (T-55's Jobs move replaces the transcript source with pod
// logs; that is the point to reconsider streaming, not here).
const transcriptMaxResponseBytes = 4 << 20 // 4 MiB

// IsTranscriptTooLarge reports whether err is GetSessionTranscript refusing a
// transcript that exceeded transcriptMaxResponseBytes -- a NAMED refusal
// (errors.Is against the shared size-cap sentinel), not merely "some error
// came back", so a caller can tell "this session produced too much text to
// read" apart from a network or decode failure and react accordingly (retry
// makes no sense here; the transcript will not get smaller).
func IsTranscriptTooLarge(err error) bool {
	return errors.Is(err, errResponseTooLarge)
}

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
	body, err := c.doRequestCapped(ctx, http.MethodGet, path, "tail=0", nil, transcriptMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	var out SessionTranscript
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gascity: GET %s: decode: %w", path, err)
	}
	return &out, nil
}
