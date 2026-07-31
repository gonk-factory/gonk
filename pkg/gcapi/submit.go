package gcapi

// SubmitSession is gonk's prompt-delivery path, and it exists because the
// create-time inject does not work on this backend.
//
// Gas City routes both the rendered template prompt and
// template_overrides.initial_message onto runtime.Config.PromptSuffix
// (cmd/gc/session_lifecycle_parallel.go), which runtime.go documents as "the
// shell-quoted prompt text appended to Command when starting the session". The
// PROVIDER owes that composition: tmux, acp, herdr and t3bridge all do it;
// internal/runtime/k8s does NOT -- agentCommandB64 base64s cfg.Command verbatim
// and never references PromptSuffix/PromptFlag. gonk runs every session as a
// k8s pod, so a session created with Message set boots with no prompt at all
// and sits at opencode's idle splash forever.
//
// Upstream's own conformance matrix never caught this: every Phase-2 worker
// profile is <family>/tmux-cli (there is no k8s profile), and WC-INPUT-001
// asserts initial_message reached runtime.Config -- not that the provider
// delivered it. Reported as gonk-drf; tracked here as gonk-u1p.1.
//
// Delivering as a second signed call also sidesteps a size trap. tmux switches
// to a prompt FILE above maxInlinePromptLen=1024 precisely because an oversized
// argv is ENAMETOOLONG/exit-126 pane death, and gonk's injected prompt carries
// up to 8 KiB of issue context. A message body has no argv limit.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// SubmitIntent is the semantic delivery choice for a submitted message. Values
// mirror gascity's session.SubmitIntent enum exactly -- the route validates
// against enum:"default,follow_up,interrupt_now", so an invented value is a 422.
type SubmitIntent string

const (
	// SubmitIntentDefault delivers the message on the session's normal turn
	// boundary. This is what a first prompt wants.
	SubmitIntentDefault SubmitIntent = "default"
	// SubmitIntentFollowUp queues the message behind work already in flight.
	SubmitIntentFollowUp SubmitIntent = "follow_up"
	// SubmitIntentInterruptNow preempts the current turn.
	SubmitIntentInterruptNow SubmitIntent = "interrupt_now"
)

// submitSessionRequest is the body of POST /v0/city/{city}/session/{id}/submit.
// Field json tags MUST match gascity's SessionSubmitInput.Body exactly.
type submitSessionRequest struct {
	Message string       `json:"message"`
	Intent  SubmitIntent `json:"intent,omitempty"`
}

// SubmitSession delivers a message to an existing session. idOrAlias may be a
// session id, an alias, or a runtime session_name -- upstream resolves all
// three -- so the unique alias stamped at CreateSession is a sufficient handle
// and the async create's missing session id is not needed.
//
// It is a mutation and is signed exactly like RunOrder and CreateSession.
//
// A 404 is returned as an *APIError (IsNotFound == true) rather than being
// flattened: create is async, so the session legitimately may not exist yet on
// the first attempt and the caller is expected to retry rather than fail.
func (c *Client) SubmitSession(ctx context.Context, idOrAlias, message string, intent SubmitIntent) error {
	if c.City == "" {
		return errEmptyCity
	}
	if idOrAlias == "" {
		return fmt.Errorf("gascity: SubmitSession: id or alias is required")
	}
	// Upstream validates minLength:1 + pattern \S. Reject a blank prompt here
	// rather than spend a signed round-trip to be told the same thing.
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("gascity: SubmitSession: message must contain a non-whitespace character")
	}
	if intent == "" {
		intent = SubmitIntentDefault
	}
	path := fmt.Sprintf("/v0/city/%s/session/%s/submit",
		url.PathEscape(c.City), url.PathEscape(idOrAlias))
	payload, err := json.Marshal(submitSessionRequest{Message: message, Intent: intent})
	if err != nil {
		return fmt.Errorf("gascity: SubmitSession: encode body: %w", err)
	}
	if _, err := c.doRequest(ctx, http.MethodPost, path, "", payload); err != nil {
		return err
	}
	return nil
}
