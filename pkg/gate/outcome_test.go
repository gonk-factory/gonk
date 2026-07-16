package gate

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// The classification table IS the contract. Every row is a real, reachable state.
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		in   Signals
		want string
	}{
		// --- aborted: a human or a config change stopped it. Neither retries nor escalates.
		{"aborted beats everything", Signals{Aborted: true, ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeAborted},
		{"aborted with no work done", Signals{Aborted: true}, meterapi.OutcomeAborted},

		// --- infra: never escalates, always retries the SAME rung (spec 6.3).
		{"reservation expired", Signals{ReservationExpired: true, ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeInfraFailed},
		{"gitlab unreachable -> we cannot even LOOK for the artifact", Signals{ArtifactUnknown: true, ModelTokens: 500}, meterapi.OutcomeInfraFailed},
		{"spend data stale -> we cannot prove the model answered", Signals{SpendStale: true}, meterapi.OutcomeInfraFailed},

		// --- THE INVARIANT: no tokens means no escalation, ever.
		// A session that never got a completion did not FAIL at the work; it never
		// got to do the work. Pod evicted before first token, LiteLLM 5xx on every
		// call, model cold-start timeout -- all land here, and all retry the same rung.
		{"no artifact, ZERO tokens", Signals{ModelTokens: 0}, meterapi.OutcomeInfraFailed},

		// --- gate-failed: the ONLY outcome that escalates. It must be EARNED:
		// the model answered (tokens > 0) and the work still is not there.
		{"no artifact, model answered", Signals{ModelTokens: 500}, meterapi.OutcomeGateFailed},
		{"no artifact, one token", Signals{ModelTokens: 1}, meterapi.OutcomeGateFailed},

		// --- success.
		{"artifact present", Signals{ArtifactPresent: true, ModelTokens: 500}, meterapi.OutcomeSuccess},
		// Success with zero tokens is bizarre (a cached/duplicate comment?) but it
		// is still success: the work is there. It costs nobody an escalation.
		{"artifact present, zero tokens", Signals{ArtifactPresent: true}, meterapi.OutcomeSuccess},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Fatalf("Classify(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The load-bearing property, asserted independently of the table above so that
// deleting a table row cannot silently delete the invariant.
//
// SPEC 6.3: "connection errors, LiteLLM 5xx, pod evictions -> retry same rung
// with backoff; only completed-but-failed-gate outcomes escalate."
func TestInfraFailureNeverEscalates(t *testing.T) {
	// Every combination of signals that includes ANY infra indicator must
	// classify as infra-failed, and infra-failed must never be the escalating
	// outcome.
	for _, art := range []bool{false, true} {
		for _, tok := range []int64{0, 1, 1_000_000} {
			for _, sig := range []Signals{
				{ReservationExpired: true, ArtifactPresent: art, ModelTokens: tok},
				{ArtifactUnknown: true, ArtifactPresent: art, ModelTokens: tok},
				{SpendStale: true, ArtifactPresent: art, ModelTokens: tok},
			} {
				got := Classify(sig)
				if got != meterapi.OutcomeInfraFailed {
					t.Fatalf("Classify(%+v) = %q, want infra-failed", sig, got)
				}
				if Escalates(got) {
					t.Fatalf("infra-failed must NEVER escalate a rung")
				}
			}
		}
	}
}

// Only ONE outcome buys an escalation. If this test ever needs updating, stop and
// think very hard about what you are about to make free.
func TestOnlyGateFailedEscalates(t *testing.T) {
	for _, o := range []string{meterapi.OutcomeSuccess, meterapi.OutcomeInfraFailed, meterapi.OutcomeAborted} {
		if Escalates(o) {
			t.Fatalf("%q must not escalate", o)
		}
	}
	if !Escalates(meterapi.OutcomeGateFailed) {
		t.Fatal("gate-failed must escalate, or the ladder never climbs and every test of it is vacuous")
	}
}

// A gate failure must be PAID FOR. This is the anti-forgery property: you cannot
// climb the ladder for free by having a session die instantly.
func TestEscalationRequiresSpentTokens(t *testing.T) {
	for _, tok := range []int64{0} {
		if o := Classify(Signals{ModelTokens: tok}); Escalates(o) {
			t.Fatalf("a session that burned %d tokens escalated (%q) -- a free ride up the ladder", tok, o)
		}
	}
	if o := Classify(Signals{ModelTokens: 1}); !Escalates(o) {
		t.Fatal("a session that got a real completion and produced nothing must escalate")
	}
}
