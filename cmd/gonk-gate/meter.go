package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// meterAPI is gonk-gate's half of the pkg/meterapi seam: a thin wrapper
// over gonk-meter's HTTP contract, mirroring pkg/intake.MeterClient's client
// for Gate 1 but adding the endpoints Gate 2 needs that Gate 1 does not --
// POST /v1/policy/outcome (the sweeper's report) and the spend-sync/session-cost
// pair the sweeper polls before it will trust a token count (HB-2, HB-4).
//
// It is package-local on purpose: nothing outside cmd/gonk-gate needs it, and
// keeping it here means Task 3 adds no exported surface to pkg/meterapi beyond
// the wire types Plan 03 already shipped there.
type meterAPI struct {
	baseURL string
	token   string // bearer, read from a FILE by main.go -- never an env value
	http    *http.Client
}

// newMeterClient builds a client for baseURL. token may be empty in tests
// against a fake that does not check auth.
func newMeterAPI(baseURL, token string) *meterAPI {
	return &meterAPI{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// Decide is Gate 2: POST /v1/policy/decide. Called on EVERY dispatch --
// the first one, a re-sling, an unpark -- with no exceptions. See dispatch.go.
func (m *meterAPI) Decide(ctx context.Context, req meterapi.DecideRequest) (*meterapi.DecideResponse, error) {
	var out meterapi.DecideResponse
	code, err := m.do(ctx, http.MethodPost, meterapi.DecidePath, req, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: POST %s: %d", meterapi.DecidePath, code)
	}
	return &out, nil
}

// Outcome is the sweeper's report: POST /v1/policy/outcome, bound to the
// reservation_id meter minted at the matching /decide.
func (m *meterAPI) Outcome(ctx context.Context, req meterapi.OutcomeRequest) (*meterapi.OutcomeResponse, error) {
	var out meterapi.OutcomeResponse
	code, err := m.do(ctx, http.MethodPost, meterapi.OutcomePath, req, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: POST %s: %d", meterapi.OutcomePath, code)
	}
	return &out, nil
}

// SpendSync forces one spend-log poll (Plan 06 hand-back HB-2), so the
// sweeper has a predicate to wait on before it asks for a session's cost,
// instead of guessing at a sleep.
func (m *meterAPI) SpendSync(ctx context.Context) (*meterapi.SpendSyncResponse, error) {
	var out meterapi.SpendSyncResponse
	code, err := m.do(ctx, http.MethodPost, meterapi.AdminSpendSyncPath, nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: POST %s: %d", meterapi.AdminSpendSyncPath, code)
	}
	return &out, nil
}

// Project reads GET /v1/projects/{project} -- the `trailers` subcommand's
// ONLY source of a project's resolved provenance policy
// (commit_trailers/include_usage). gonk-gate never re-derives
// gonkcfg.Effective itself; per ADR-002, meter's Resolve is the only
// legitimate resolver, and this is that answer, read off the wire.
func (m *meterAPI) Project(ctx context.Context, project string) (*meterapi.ProjectResponse, error) {
	var out meterapi.ProjectResponse
	code, err := m.do(ctx, http.MethodGet, meterapi.ProjectPath(project), nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: GET %s: %d", meterapi.ProjectPath(project), code)
	}
	return &out, nil
}

// CostSession reads GET /v1/cost/session/{key} -- the sweeper polls this
// after SpendSync until SpendAsOf catches up past the session's end.
func (m *meterAPI) CostSession(ctx context.Context, sessionKey string) (*meterapi.SessionCostResponse, error) {
	var out meterapi.SessionCostResponse
	code, err := m.do(ctx, http.MethodGet, meterapi.CostSessionPath(sessionKey), nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: GET %s: %d", meterapi.CostSessionPath(sessionKey), code)
	}
	return &out, nil
}

// do sends one request with the bearer token, caps the response body, and
// decodes it. It never puts the token in an error string.
func (m *meterAPI) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("meter: encode: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	if m.token != "" {
		r.Header.Set("Authorization", "Bearer "+m.token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.http.Do(r)
	if err != nil {
		return 0, fmt.Errorf("meter: %s %s: %w", method, path, err) // no token in the error
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("meter: read: %w", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return resp.StatusCode, fmt.Errorf("meter: decode: %w", err)
			}
		}
	}
	if resp.StatusCode >= 500 {
		var er meterapi.ErrorResponse
		_ = json.Unmarshal(raw, &er)
		return resp.StatusCode, fmt.Errorf("meter: %s %s: %d: %s", method, path, resp.StatusCode, er.Error)
	}
	return resp.StatusCode, nil
}

// --- prompt-by-reference (gonk-mzd) -----------------------------------------

// PutPrompt stores the prompt the session will fetch. It is written BEFORE the
// create, because the pod can be up and asking before CreateSession returns.
func (m *meterAPI) PutPrompt(ctx context.Context, alias string, req meterapi.PromptRequest) error {
	path := meterapi.PromptPathPrefix + alias
	code, err := m.do(ctx, http.MethodPut, path, req, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return fmt.Errorf("meter: PUT %s: %d", path, code)
	}
	return nil
}

// PromptFetched reports whether the pod has taken its prompt.
//
// This is what replaced "we submitted it" as the evidence of delivery. It is
// strictly better -- the old keystroke path claimed success even when the
// composer was empty -- but it is NOT terminal success: it proves the entrypoint
// fetched, not that opencode accepted. Dispatch keeps its session-health checks.
func (m *meterAPI) PromptFetched(ctx context.Context, alias string) (bool, error) {
	var out meterapi.PromptStatusResponse
	path := meterapi.PromptPathPrefix + alias + "/status"
	code, err := m.do(ctx, http.MethodGet, path, nil, &out)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("meter: GET %s: %d", path, code)
	}
	return out.Fetched, nil
}
