package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// reservationIDPattern matches service.newID("rsv")'s output: a 4-byte
// crypto/rand value, hex-encoded. It is the ONE field in this contract that
// is deliberately unguessable (a reservation id must not be predictable),
// so it cannot be pinned byte-for-byte like everything else here. Every
// other field on every response in this file -- including every other id,
// hash, and timestamp -- comes from the fixture's fixed clock and
// deterministic inputs and IS pinned.
var reservationIDPattern = regexp.MustCompile(`"reservation_id":"rsv-[0-9a-f]{8}"`)

func redactReservationID(body []byte) []byte {
	return reservationIDPattern.ReplaceAll(body, []byte(`"reservation_id":"rsv-REDACTED"`))
}

// wireContractCase is one (name, HTTP round trip) pair. Cases run in slice
// order, not map order: several build on state a prior case created (the
// same registered project, an open reservation to report an outcome
// against), so execution order is part of the fixture, not incidental.
type wireContractCase struct {
	name string
	run  func(t *testing.T, h *httpFixture) []byte
}

// wireContractCases drives the REAL mux (the same NewMux cmd/gonk-meter
// serves) through one representative round trip per response shape in
// docs/api/gonk-meter-v1.md, and returns the raw bytes it puts on the wire.
func wireContractCases() []wireContractCase {
	return []wireContractCase{
		{"project_response_active", func(t *testing.T, h *httpFixture) []byte {
			_, body := h.do(http.MethodPut, meterapi.ProjectPath("wire/active"), h.token, meterapi.ProjectRequest{
				Project: "wire/active", ProjectID: 101, Rig: "wire-active",
				GonkYML: simpleYAML("qwen-local, glm", 10),
			})
			return body
		}},
		{"project_response_invalid", func(t *testing.T, h *httpFixture) []byte {
			_, body := h.do(http.MethodPut, meterapi.ProjectPath("wire/invalid"), h.token, meterapi.ProjectRequest{
				Project: "wire/invalid", ProjectID: 102, Rig: "wire-invalid",
				GonkYML: "not valid [",
			})
			return body
		}},
		{"decide_response_run", func(t *testing.T, h *httpFixture) []byte {
			h.syncOnce()
			_, body := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
				Project: "wire/active", Rig: "wire-active", BeadID: "gk-wire-1", SessionKey: "sess-wire-1", Trigger: trigger,
			})
			return redactReservationID(body)
		}},
		{"decide_response_defer", func(t *testing.T, h *httpFixture) []byte {
			// A cloud-only ladder whose sole rung costs more than the project's
			// entire monthly ceiling: the FIRST decision is a deterministic
			// monthly-cost-exhausted defer, no prior attempts required.
			h.do(http.MethodPut, meterapi.ProjectPath("wire/defer"), h.token, meterapi.ProjectRequest{
				Project: "wire/defer", ProjectID: 103, Rig: "wire-defer",
				GonkYML: simpleYAML("glm", 0.30),
			})
			h.syncOnce()
			_, body := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
				Project: "wire/defer", Rig: "wire-defer", BeadID: "gk-wire-2", SessionKey: "sess-wire-2", Trigger: trigger,
			})
			return body
		}},
		{"decide_response_deny", func(t *testing.T, h *httpFixture) []byte {
			_, body := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
				Project: "wire/never-registered", Rig: "x", BeadID: "gk-wire-3", SessionKey: "sess-wire-3", Trigger: trigger,
			})
			return body
		}},
		{"outcome_response", func(t *testing.T, h *httpFixture) []byte {
			_, decideBody := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
				Project: "wire/active", Rig: "wire-active", BeadID: "gk-wire-4", SessionKey: "sess-wire-4", Trigger: trigger,
			})
			var dr meterapi.DecideResponse
			if err := json.Unmarshal(decideBody, &dr); err != nil {
				t.Fatalf("decode decide_response_run for outcome fixture: %v; body=%s", err, decideBody)
			}
			_, body := h.do(http.MethodPost, meterapi.OutcomePath, h.token, meterapi.OutcomeRequest{
				Project: "wire/active", BeadID: "gk-wire-4", SessionKey: "sess-wire-4",
				Attempt: dr.Attempt, Rung: dr.Rung, ReservationID: dr.ReservationID, Outcome: meterapi.OutcomeSuccess,
			})
			return body
		}},
		{"bead_cost_response", func(t *testing.T, h *httpFixture) []byte {
			// gk-wire-4 has exactly one, successful attempt from the
			// outcome_response case above.
			_, body := h.do(http.MethodGet, meterapi.CostBeadPath("gk-wire-4"), h.token, nil)
			return body
		}},
		{"error_response", func(t *testing.T, h *httpFixture) []byte {
			// A malformed /decide request (no bead_id): the ErrorResponse shape.
			_, body := h.do(http.MethodPost, meterapi.DecidePath, h.token, meterapi.DecideRequest{
				Project: "wire/active", Rig: "wire-active", SessionKey: "sess-wire-5", Trigger: trigger,
			})
			return body
		}},
		{"spend_sync_response", func(t *testing.T, h *httpFixture) []byte {
			_, body := h.do(http.MethodPost, meterapi.AdminSpendSyncPath, h.token, nil)
			return body
		}},
	}
}

// TestWireContractLiterals is the intake<->meter<->pack contract, frozen at
// the layer that actually matters: bytes leaving the real HTTP mux
// (internal/meter/service.NewMux, wired exactly as cmd/gonk-meter wires it),
// not a struct marshaled in isolation.
//
// pkg/meterapi/meterapi_test.go's own TestWireContractLiterals already
// freezes the Go *types'* JSON encoding. This test freezes the separate
// claim the DoD makes: "internal/meter/service/http.go declares no wire
// types of its own." A handler that quietly wrapped, renamed, or
// re-encoded a field before calling writeJSON would pass every test in
// this package that round-trips through meterapi's own struct (decoding
// back into the same Go type tolerates a field rename or an added
// wrapper), but it would fail HERE, because the comparison is against the
// literal bytes on the wire.
//
// Run with UPDATE_GOLDEN=1 to (re)write the golden files after a
// deliberate, reviewed contract change.
func TestWireContractLiterals(t *testing.T) {
	update := os.Getenv("UPDATE_GOLDEN") == "1"
	h := newHTTPFixture(t)

	for _, c := range wireContractCases() {
		t.Run(c.name, func(t *testing.T) {
			got := c.run(t, h)

			path := filepath.Join("testdata", "golden", c.name+".json")
			if update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", path, err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with UPDATE_GOLDEN=1 to create it)", path, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s literal wire JSON mismatch (a field was renamed, retyped, added, or removed -- this is a breaking change to the intake<->meter<->pack contract):\n--- got ---\n%s\n--- want ---\n%s", c.name, got, want)
			}
		})
	}
}
