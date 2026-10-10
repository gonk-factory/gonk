package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
)

// HTTPAdmin talks to LiteLLM's key-management API over HTTP.
//
// VERIFIED against real LiteLLM v1.92.0 (gonk-huy live harness). The contract
// that matters for re-provisioning, and which a Plan 03 bug got wrong:
//   - A key's PLAINTEXT secret is revealed exactly once, in the /key/generate
//     response. No later call recovers it: /key/list and /key/info return only
//     the hashed token id, and presenting that hash as a Bearer token is 401.
//   - There is therefore no lookup-by-alias that yields a usable token. To
//     UPDATE an existing key by alias we resolve its hashed id via
//     /key/list?key_alias=<alias> and pass that id to /key/update (which
//     accepts it) -- but we cannot, and do not, return a fresh usable token
//     from the update path. updateByAlias returns an empty KeyInfo.Token
//     meaning "budget updated in place; the plaintext is unchanged". The
//     caller must preserve the token it already stored.
//   - /key/info?key_alias= does NOT 404-cleanly: it ignores the param and
//     returns the CALLER's own key, which is why alias lookups must never go
//     through /key/info.
//   - Admin routes require a proxy_admin-role key; a plain key is 401. That is
//     a deployment concern (mint the right key); the adapter just carries it.
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

// keyResponseBody is the FLAT wire shape /key/generate and /key/update return
// (VERIFIED against real v1.92.0: top-level key + key_alias). Key is the secret
// token -- it is read into KeyInfo.Token and must never be logged or echoed
// into an error. NOTE the asymmetry: /key/generate's Key is the usable
// plaintext secret; /key/update's Key is only the hashed token id (the
// plaintext is never returned again), so the update path does NOT decode a
// usable token out of this shape.
type keyResponseBody struct {
	Key      string `json:"key"`
	KeyAlias string `json:"key_alias"`
}

// keyListResponse is /key/list's shape. With no return_full_object it lists the
// HASHED token ids under "keys" (VERIFIED against real v1.92.0). This is the
// only endpoint that filters by alias and thus the only way to resolve an
// existing key from its alias -- the id it yields identifies the record for
// /key/update, but is NOT a usable Bearer token.
type keyListResponse struct {
	Keys       []string `json:"keys"`
	TotalCount int      `json:"total_count"`
	TotalPages int      `json:"total_pages"`
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

// updateByAlias updates an existing key identified only by its alias. It
// resolves the key's hashed token id via /key/list?key_alias= (the only alias
// filter LiteLLM offers), hands that id to /key/update (which accepts it), and
// returns a KeyInfo with an EMPTY Token.
//
// The empty Token is the crux, not an oversight: real v1.92.0 reveals a key's
// plaintext secret only once, at /key/generate. /key/list and /key/update
// return only the hashed id, which is NOT usable as a Bearer token (verified:
// presenting it is 401). An update does not rotate, so the plaintext is
// unchanged -- the caller must keep the token it stored at create time. The
// meter service honours this by preserving its stored KeyRef when Token == "".
func (a *HTTPAdmin) updateByAlias(ctx context.Context, spec KeySpec, reqBody keyRequestBody) (KeyInfo, error) {
	tokenID, err := a.resolveTokenIDByAlias(ctx, spec.Alias)
	if err != nil {
		return KeyInfo{}, err
	}
	reqBody.Key = tokenID
	// The alias MUST NOT be re-sent. LiteLLM's alias-uniqueness check on
	// /key/update does not exclude the key being updated, so re-asserting a
	// key's OWN alias is rejected as a duplicate of itself:
	//
	//   400 Key with alias 'gonk-agentic-gonk-e2e-1784441480' already exists.
	//       Unique key aliases across all keys are required.
	//
	// Verified against the live LiteLLM (v1.92.0) on 2026-08-10: the identical
	// /key/update with key_alias omitted returns 200. The field is redundant
	// here anyway -- tokenID already identifies the record, and its alias is by
	// construction the one we looked it up by.
	//
	// This was not theoretical. It made EnsureKey fail for every project whose
	// key already existed, so meter recorded key-missing, and intake answered
	// every webhook 200 and dropped it as state_key-missing. Triage could not
	// dispatch at all (gonk-zp3).
	reqBody.KeyAlias = ""
	body, status, err := a.do(ctx, http.MethodPost, "/key/update", reqBody)
	if err != nil {
		return KeyInfo{}, err
	}
	if status >= 400 {
		return KeyInfo{}, fmt.Errorf("litellm: /key/update: status %d: %s", status, string(body))
	}
	// Deliberately no usable Token: see the doc comment. Budget is now updated.
	return KeyInfo{Alias: spec.Alias, Token: ""}, nil
}

// resolveTokenIDByAlias returns the hashed token id of the single key bearing
// the given alias. gonk mints exactly one key per alias (keysink.Slug's hash
// suffix guarantees alias uniqueness), so a well-formed result has exactly one
// id; anything else is an error rather than a guess about which key to update.
func (a *HTTPAdmin) resolveTokenIDByAlias(ctx context.Context, alias string) (string, error) {
	q := url.Values{}
	q.Set("key_alias", strings.TrimSpace(alias))
	body, status, err := a.do(ctx, http.MethodGet, "/key/list?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("litellm: /key/list: status %d: %s", status, string(body))
	}
	var lr keyListResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", fmt.Errorf("litellm: /key/list: decoding response: %w", err)
	}
	if len(lr.Keys) == 0 {
		return "", fmt.Errorf("litellm: /key/list: no key found for alias")
	}
	if len(lr.Keys) > 1 {
		return "", fmt.Errorf("litellm: /key/list: %d keys share alias, refusing to guess which to update", len(lr.Keys))
	}
	return lr.Keys[0], nil
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
