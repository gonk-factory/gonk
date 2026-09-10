package intake

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

// MeterClient is intake's half of the pkg/meterapi seam. Plan 03 implements the
// server. Neither side re-derives the JSON shape.
type MeterClient struct {
	BaseURL string
	token   string // bearer, read from a FILE by main.go -- never an env value
	HTTP    *http.Client
}

func NewMeterClient(baseURL, token string, hc *http.Client) *MeterClient {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &MeterClient{BaseURL: strings.TrimSuffix(baseURL, "/"), token: token, HTTP: hc}
}

// ErrInvalidConfig is a 422: the PROJECT's .gonk.yml will not load. It is not an
// error in the HTTP sense and it is emphatically not intake's bug -- meter has
// successfully and idempotently recorded the project as invalid and deleted its
// virtual key. The Response is a full ProjectResponse carrying meter's error
// text, which the onboarding/comment flow shows to a human.
type ErrInvalidConfig struct {
	Response *meterapi.ProjectResponse
}

func (e *ErrInvalidConfig) Error() string {
	return "meter: project config is invalid: " + e.Response.Error
}

// Register pushes the RAW .gonk.yml. Meter validates, resolves, and provisions
// the key -- intake does none of those things (see "Division of responsibility").
//
// It is an idempotent upsert: the same bytes may be PUT every reconcile pass.
//
// Failure is NOT fatal to the pass: the caller records the project as `unsynced`,
// dispatches nothing for it, and retries next pass.
func (m *MeterClient) Register(ctx context.Context, req meterapi.ProjectRequest) (*meterapi.ProjectResponse, error) {
	var out meterapi.ProjectResponse
	code, err := m.do(ctx, http.MethodPut, meterapi.ProjectPath(req.Project), req, &out)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusUnprocessableEntity: // 422 -- the project's yaml is bad
		return nil, &ErrInvalidConfig{Response: &out}
	case code == http.StatusOK:
		return &out, nil
	}
	// Any other status (400, etc.) means OUR request was malformed. That is an
	// intake bug and it must be loud: retrying it will never help.
	return nil, fmt.Errorf("meter: PUT %s: unexpected status %d", req.Project, code)
}

// Get answers whether meter currently has PROJECT registered, WITHOUT
// mutating anything -- unlike Deregister's DELETE, it touches neither the
// Secret nor the DB row behind a registration. reconcileProject's
// archived-project branch uses it to decide whether a DELETE is even needed,
// rather than repeating an idempotent-but-not-free one every pass.
//
// found is false only on a clean 404 ("project not registered"); any other
// non-2xx status is a real error, same as Register and Deregister.
func (m *MeterClient) Get(ctx context.Context, project string) (*meterapi.ProjectResponse, bool, error) {
	var out meterapi.ProjectResponse
	code, err := m.do(ctx, http.MethodGet, meterapi.ProjectPath(project), nil, &out)
	switch {
	case err != nil:
		return nil, false, err
	case code == http.StatusNotFound:
		return nil, false, nil
	case code == http.StatusOK:
		return &out, true, nil
	}
	return nil, false, fmt.Errorf("meter: GET %s: unexpected status %d", project, code)
}

// Deregister de-onboards a project: meter disables it and deletes the virtual
// key. Idempotent -- deleting an unknown project is a 204, not a 404.
func (m *MeterClient) Deregister(ctx context.Context, project string) error {
	code, err := m.do(ctx, http.MethodDelete, meterapi.ProjectPath(project), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusOK {
		return fmt.Errorf("meter: DELETE %s: %d", project, code)
	}
	return nil
}

// Healthy checks GET /healthz. It carries no bearer token requirement (the
// health endpoints are unauthenticated) but do() always sends one; that is
// harmless.
func (m *MeterClient) Healthy(ctx context.Context) error {
	code, err := m.do(ctx, http.MethodGet, meterapi.HealthzPath, nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("meter: GET %s: %d", meterapi.HealthzPath, code)
	}
	return nil
}

// Decide is Gate 1: POST /v1/policy/decide. run/defer/deny are ALL HTTP 200 --
// normal answers, not errors. The request carries NO attempt count (meterapi.
// DecideRequest has no such field, deliberately: a caller-supplied attempt is a
// forgery vector for climbing the ladder). On a `run`, the response carries the
// rung/model/key_ref/reservation_id/attempt intake forwards as order vars; on a
// `defer`, a retry_after. Gate 2 (the pack) re-asks this on every pour.
func (m *MeterClient) Decide(ctx context.Context, req meterapi.DecideRequest) (*meterapi.DecideResponse, error) {
	var out meterapi.DecideResponse
	// Never hand-build the path (the plan's own rule): meterapi owns it.
	code, err := m.do(ctx, http.MethodPost, meterapi.DecidePath, req, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("meter: POST %s: %d", meterapi.DecidePath, code)
	}
	return &out, nil
}

// do sends one request with the bearer token, caps the response body, and
// decodes it. It NEVER puts the token in an error string: errors get logged.
func (m *MeterClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("meter: encode: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, m.BaseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	r.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.HTTP.Do(r)
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
			// A 5xx may not be JSON at all; that is fine, the status carries it.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == 422 {
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
