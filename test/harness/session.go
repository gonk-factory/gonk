package harness

//
// SyntheticSession reproduces EXACTLY the sequence the pack's dispatch formula and
// gate step perform (spec 6.2.3, Plan 03's contract):
//
//  1. POST /v1/policy/decide            -> run | defer | deny
//  2. on `run`: read key_ref, and make N model calls THROUGH LiteLLM, stamping the
//     returned `metadata` VERBATIM (never a metadata the test made up -- the whole
//     attribution chain depends on the pack not inventing tags)
//  3. POST /v1/policy/outcome           with the outcome the SCRIPT dictates
//
// It exists so the entire money path -- decide, reserve, spend, attribute, settle,
// escalate -- is assertable at L1 and L2, where a real opencode session is
// impossible. L3 replaces it with the real pack and asserts the SAME ledger.
//
// It NEVER sends an attempt count: meterapi.DecideRequest has no such field, and
// adding one is a forgery vector for climbing the ladder (Plan 03 Decision 2).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// SyntheticSession drives one (bead, session) worth of the money path against a
// real meter (over real HTTP through the meterapi wire contract) and a real model
// endpoint (the stub directly at L1, LiteLLM in front of the stub at L2/L3).
type SyntheticSession struct {
	// HTTP is the client for both meter and model calls. Defaults to a client with
	// a 30s timeout. Give it a short timeout to exercise a hung-model scenario.
	HTTP *http.Client

	// MeterURL is the meter service base URL (e.g. http://127.0.0.1:9091); the
	// session appends meterapi.DecidePath / OutcomePath.
	MeterURL string
	// ModelURL is the base URL of the OpenAI-compatible endpoint the "pack" calls:
	// the stub directly at L1, or LiteLLM (which forwards to the stub) at L2/L3.
	// The session appends "/v1/chat/completions".
	ModelURL string

	// The attribution tuple. These are the DecideRequest identity fields; the
	// harness sends them verbatim and the ledger asserts on exactly them.
	Project    string
	Rig        string
	BeadID     string // the deterministic BeadAnchor, identical at both gates
	SessionKey string
	Trigger    string // an atags.Trigger* value

	// Key is the LiteLLM virtual key sent as Bearer on model calls. When
	// KeyResolver is set it is called after each `run` decision to fetch the key
	// named by DecideResponse.KeyRef; otherwise Key is used verbatim (a fake that
	// ignores auth is fine with "").
	Key         string
	KeyResolver func(meterapi.KeyRef) (string, error)

	// ModelOverride, when non-empty, replaces DecideResponse.Model on the wire.
	ModelOverride string

	last meterapi.DecideResponse
}

func (s *SyntheticSession) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Last is the most recent DecideResponse (after Decide/Run).
func (s *SyntheticSession) Last() meterapi.DecideResponse { return s.last }

// Decide runs the pack's Gate 2 /decide call and records the response. On a `run`
// decision it resolves the virtual key via KeyResolver (if set) so the following
// Call has a Bearer token.
func (s *SyntheticSession) Decide(t testing.TB) meterapi.DecideResponse {
	t.Helper()
	req := meterapi.DecideRequest{
		Project:    s.Project,
		Rig:        s.Rig,
		BeadID:     s.BeadID,
		SessionKey: s.SessionKey,
		Trigger:    s.Trigger,
	}
	var resp meterapi.DecideResponse
	s.postJSON(t, s.MeterURL+meterapi.DecidePath, req, &resp)
	s.last = resp
	if resp.Decision == meterapi.DecisionRun && s.KeyResolver != nil {
		key, err := s.KeyResolver(resp.KeyRef)
		if err != nil {
			t.Fatalf("SyntheticSession: resolve key_ref %+v: %v", resp.KeyRef, err)
		}
		s.Key = key
	}
	return resp
}

// Call makes n model calls at the decided rung, stamping the decided metadata
// VERBATIM -- both in the request body's `metadata` (what the stub records) and in
// the x-litellm-spend-logs-metadata provider header (what a real LiteLLM
// persists). It must follow a `run` Decide.
func (s *SyntheticSession) Call(t testing.TB, n int) {
	t.Helper()
	if s.last.Decision != meterapi.DecisionRun {
		t.Fatalf("SyntheticSession.Call: last decision was %q, not %q -- nothing may call the model", s.last.Decision, meterapi.DecisionRun)
	}
	model := s.ModelOverride
	if model == "" {
		model = s.last.Model
	}
	metaJSON, err := json.Marshal(s.last.Metadata)
	if err != nil {
		t.Fatalf("SyntheticSession.Call: marshal metadata: %v", err)
	}
	for i := 0; i < n; i++ {
		body := map[string]any{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "synthetic call"},
			},
			// The stub records the body's metadata directly (L1); LiteLLM reads the
			// header (L2/L3). Stamp both so the SAME session drives every layer.
			"metadata": s.last.Metadata,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("SyntheticSession.Call: marshal request: %v", err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			s.ModelURL+"/v1/chat/completions", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("SyntheticSession.Call: new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-litellm-spend-logs-metadata", string(metaJSON))
		if s.Key != "" {
			req.Header.Set("Authorization", "Bearer "+s.Key)
		}
		resp, err := s.httpClient().Do(req)
		if err != nil {
			t.Fatalf("SyntheticSession.Call: model call %d/%d: %v", i+1, n, err)
		}
		drained, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("SyntheticSession.Call: model call %d/%d returned %d: %s", i+1, n, resp.StatusCode, drained)
		}
	}
}

// Report runs the pack's /outcome call, binding the outcome to the reservation
// the matching Decide opened. outcome is one of the meterapi.Outcome* values.
func (s *SyntheticSession) Report(t testing.TB, outcome string) meterapi.OutcomeResponse {
	t.Helper()
	req := meterapi.OutcomeRequest{
		Project:       s.Project,
		BeadID:        s.BeadID,
		SessionKey:    s.SessionKey,
		Attempt:       s.last.Attempt,
		Rung:          s.last.Rung,
		ReservationID: s.last.ReservationID,
		Outcome:       outcome,
	}
	var resp meterapi.OutcomeResponse
	s.postJSON(t, s.MeterURL+meterapi.OutcomePath, req, &resp)
	return resp
}

// Run does all three steps: Decide, then -- only on a `run` decision -- `calls`
// model calls and a Report with outcome. On a defer/deny it makes no model call
// and reports nothing (there is no reservation to settle), returning the decision
// so the caller can assert the refusal. It returns the DecideResponse.
func (s *SyntheticSession) Run(t testing.TB, calls int, outcome string) meterapi.DecideResponse {
	t.Helper()
	d := s.Decide(t)
	if d.Decision != meterapi.DecisionRun {
		return d
	}
	s.Call(t, calls)
	s.Report(t, outcome)
	return d
}

func (s *SyntheticSession) postJSON(t testing.TB, url string, in, out any) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("SyntheticSession: marshal %T: %v", in, err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("SyntheticSession: new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		t.Fatalf("SyntheticSession: POST %s: %v", url, err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("SyntheticSession: POST %s returned %d: %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("SyntheticSession: decode %s response: %v (body: %s)", url, err, body)
	}
}
