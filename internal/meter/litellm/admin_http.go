package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// HTTPAdmin talks to LiteLLM's key-management API over HTTP.
//
// *** LIVE-INFRASTRUCTURE BOUNDARY ***: this adapter is written against
// LiteLLM's documented /key/generate, /key/update, /key/delete, and /key/info
// endpoints and is tested only against an httptest.Server standing in for
// LiteLLM. It is NOT verified against a real LiteLLM until Plan 06's e2e
// harness; the exact request/response shapes of the pinned LiteLLM version
// (ghcr.io/berriai/litellm-database:v1.92.0, docs/environment.md) must be
// confirmed there.
type HTTPAdmin struct {
	baseURL  string
	adminKey string
	client   *http.Client
}

func NewHTTPAdmin(baseURL, adminKey string, c *http.Client) *HTTPAdmin {
	return &HTTPAdmin{baseURL: strings.TrimRight(baseURL, "/"), adminKey: adminKey, client: c}
}

// keyRequestBody is the wire shape POSTed to /key/generate and /key/update.
// Key is set only for an update (it identifies which existing key record is
// being changed; LiteLLM's /key/update looks the record up by token, not by
// alias).
type keyRequestBody struct {
	Key            string            `json:"key,omitempty"`
	KeyAlias       string            `json:"key_alias,omitempty"`
	MaxBudget      *float64          `json:"max_budget,omitempty"`
	BudgetDuration string            `json:"budget_duration,omitempty"`
	Models         []string          `json:"models,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	TPMLimit       *int              `json:"tpm_limit,omitempty"`
	RPMLimit       *int              `json:"rpm_limit,omitempty"`
}

// keyResponseBody is the wire shape /key/generate, /key/update, and /key/info
// return. Key is the secret token -- it is read into KeyInfo.Token and must
// never be logged or echoed into an error.
type keyResponseBody struct {
	Key      string `json:"key"`
	KeyAlias string `json:"key_alias"`
}

func requestBodyFor(spec KeySpec) (keyRequestBody, error) {
	// spec.MaxBudgetUSD should only ever be nil or a finite value built by
	// MaxBudgetFor (never +Inf/NaN by hand -- see the KeySpec doc comment). This
	// is a defensive check, not the primary guard: encoding/json would itself
	// refuse to marshal +Inf, but failing here gives a clear error instead of a
	// generic marshal failure, and it is the same "never let a non-finite dollar
	// figure reach a persistence/wire boundary" discipline store/money.go applies.
	if spec.MaxBudgetUSD != nil && (math.IsNaN(*spec.MaxBudgetUSD) || math.IsInf(*spec.MaxBudgetUSD, 0)) {
		return keyRequestBody{}, fmt.Errorf("litellm: refusing to send non-finite max_budget %v", *spec.MaxBudgetUSD)
	}
	return keyRequestBody{
		KeyAlias:       spec.Alias,
		MaxBudget:      spec.MaxBudgetUSD,
		BudgetDuration: spec.BudgetDuration,
		Models:         spec.Models,
		Metadata:       spec.Metadata,
		TPMLimit:       spec.TPMLimit,
		RPMLimit:       spec.RPMLimit,
	}, nil
}

// do POSTs body as JSON to path and returns the decoded response. The admin
// key travels ONLY in the Authorization header, and any error returned is
// built from the status code and response body alone -- never from the
// request (which carries the admin key) or from a KeyInfo (which carries a
// project's token).
func (a *HTTPAdmin) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("litellm: encoding request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("litellm: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.adminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// err may embed the request URL but never the Authorization header, so
		// the admin key does not leak through this path either.
		return nil, 0, fmt.Errorf("litellm: request to %s failed: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("litellm: reading response from %s: %w", path, err)
	}
	return respBody, resp.StatusCode, nil
}

func decodeKeyResponse(path string, body []byte, status int) (KeyInfo, error) {
	if status >= 400 {
		return KeyInfo{}, fmt.Errorf("litellm: %s: status %d: %s", path, status, string(body))
	}
	var kr keyResponseBody
	if err := json.Unmarshal(body, &kr); err != nil {
		return KeyInfo{}, fmt.Errorf("litellm: %s: decoding response: %w", path, err)
	}
	return KeyInfo{Alias: kr.KeyAlias, Token: kr.Key}, nil
}

// alreadyExists is a heuristic over LiteLLM's error body: the exact wording is
// unverified against a live proxy (see the package-level live-infrastructure
// boundary note), so this is deliberately loose and case-insensitive.
func alreadyExists(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "already exist") || strings.Contains(s, "duplicate")
}

// EnsureKey creates the virtual key if it does not exist, or updates it in
// place if it does -- EnsureKey is idempotent BY ALIAS.
func (a *HTTPAdmin) EnsureKey(ctx context.Context, spec KeySpec) (KeyInfo, error) {
	reqBody, err := requestBodyFor(spec)
	if err != nil {
		return KeyInfo{}, err
	}
	body, status, err := a.do(ctx, http.MethodPost, "/key/generate", reqBody)
	if err != nil {
		return KeyInfo{}, err
	}
	if status >= 400 && alreadyExists(body) {
		return a.updateByAlias(ctx, spec, reqBody)
	}
	return decodeKeyResponse("/key/generate", body, status)
}

// updateByAlias looks the existing key up by alias (LiteLLM's /key/update
// identifies the record by token, not alias) and then updates it.
func (a *HTTPAdmin) updateByAlias(ctx context.Context, spec KeySpec, reqBody keyRequestBody) (KeyInfo, error) {
	infoBody, status, err := a.do(ctx, http.MethodGet, "/key/info?key_alias="+strings.TrimSpace(spec.Alias), nil)
	if err != nil {
		return KeyInfo{}, err
	}
	existing, err := decodeKeyResponse("/key/info", infoBody, status)
	if err != nil {
		return KeyInfo{}, err
	}
	reqBody.Key = existing.Token
	body, status, err := a.do(ctx, http.MethodPost, "/key/update", reqBody)
	if err != nil {
		return KeyInfo{}, err
	}
	info, err := decodeKeyResponse("/key/update", body, status)
	if err != nil {
		return KeyInfo{}, err
	}
	if info.Token == "" {
		// Some LiteLLM versions do not echo the token back from /key/update.
		// The token has not changed (update, not rotate), so the one we already
		// looked up is still correct.
		info.Token = existing.Token
	}
	return info, nil
}

// RotateKey invalidates the project's current token and issues a fresh one
// under the same alias: delete, then generate. A rotate that only generated
// (without deleting) would leave the old token live, which defeats the point
// of rotating.
func (a *HTTPAdmin) RotateKey(ctx context.Context, spec KeySpec) (KeyInfo, error) {
	if err := a.DeleteKey(ctx, spec.Alias); err != nil {
		return KeyInfo{}, err
	}
	reqBody, err := requestBodyFor(spec)
	if err != nil {
		return KeyInfo{}, err
	}
	body, status, err := a.do(ctx, http.MethodPost, "/key/generate", reqBody)
	if err != nil {
		return KeyInfo{}, err
	}
	return decodeKeyResponse("/key/generate", body, status)
}

// DeleteKey removes the virtual key. LiteLLM's /key/delete identifies keys by
// alias via key_aliases.
func (a *HTTPAdmin) DeleteKey(ctx context.Context, alias string) error {
	body, status, err := a.do(ctx, http.MethodPost, "/key/delete", struct {
		KeyAliases []string `json:"key_aliases"`
	}{KeyAliases: []string{alias}})
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("litellm: /key/delete: status %d: %s", status, string(body))
	}
	return nil
}

var _ Admin = (*HTTPAdmin)(nil)
