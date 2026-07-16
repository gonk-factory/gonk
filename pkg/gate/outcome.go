// Package gate is gonk's deterministic outcome classifier: the pure function
// that decides how one session attempt ended.
//
// THERE IS NO LLM IN THIS PACKAGE, AND THERE NEVER WILL BE (spec 6.3: "no LLM
// judges"). Everything here is a total function of objective signals gathered
// from services gonk owns -- GitLab (did the artifact land?) and gonk-meter (did
// the model actually answer?). The agent is NEVER asked how it did: a session
// that can classify itself can hallucinate `gate-failed` and walk its project up
// to its most expensive rung for free.
//
// The one invariant to defend with your life (spec 6.3, and Plan 06's HB-4):
//
//	INFRA FAILURES NEVER ESCALATE A RUNG. ONLY A COMPLETED-BUT-FAILED GATE DOES.
//
// and its corollary, which is what makes the first one enforceable:
//
//	AN ESCALATION MUST BE PAID FOR. If we cannot PROVE the model answered
//	(ModelTokens > 0), the outcome is infra-failed, not gate-failed.
package gate

import "gitlab.orac.local/agentic/gonk-project/pkg/meterapi"

// Signals are the objective inputs to a classification. Every field is gathered
// by gonk-gate from GitLab or gonk-meter. None of them comes from the agent.
type Signals struct {
	// Aborted: a human closed the bead, or the project's config changed under the
	// session (spec: "cancelled by a human or by a config change"). Neither
	// escalates nor retries.
	Aborted bool

	// ReservationExpired: meter's reservation TTL elapsed before the session
	// reported. The session may or may not have done work; either way, the
	// reservation it held is gone and the attempt is void. INFRA.
	ReservationExpired bool

	// ArtifactUnknown: we could not ASK GitLab whether the artifact landed
	// (GitLab 5xx, network error, auth failure). Absence of evidence is not
	// evidence of a gate failure -- classifying it as one would buy an escalation
	// out of our own outage. INFRA.
	ArtifactUnknown bool

	// SpendStale: meter's view of spend has not caught up past the end of this
	// session, so ModelTokens is not trustworthy. We CANNOT PROVE the model
	// answered, therefore we do not escalate. INFRA. (gonk-gate forces a spend
	// sync and waits on a predicate before setting this -- see cmd/gonk-gate/sweep.go.)
	SpendStale bool

	// ArtifactPresent: the bot's comment carrying this bead's marker is on the
	// GitLab issue. This is the GATE, and it is a fact, not a judgement.
	ArtifactPresent bool

	// ModelTokens: total tokens this session consumed, per meter's ledger. The
	// PROOF that the model answered. Zero means it never did.
	ModelTokens int64
}

// Classify is total: every Signals value maps to exactly one outcome. The order
// of the checks IS the priority order, and it is deliberate.
func Classify(s Signals) string {
	// 1. Abort wins outright. A human said stop; nothing about the work matters.
	if s.Aborted {
		return meterapi.OutcomeAborted
	}
	// 2. Any infra indicator wins over any judgement about the work. We refuse to
	//    convert our own outage into somebody's escalation.
	if s.ReservationExpired || s.ArtifactUnknown || s.SpendStale {
		return meterapi.OutcomeInfraFailed
	}
	// 3. The artifact is there. Done. (Even at zero tokens -- the work exists, and
	//    success costs nobody a rung.)
	if s.ArtifactPresent {
		return meterapi.OutcomeSuccess
	}
	// 4. No artifact, and we cannot prove the model ever answered. It did not FAIL
	//    at the work; it never got to do the work. Retry the same rung.
	//    THIS IS THE LINE THAT MAKES "infra failures never escalate" TRUE for pod
	//    evictions, cold-start timeouts and LiteLLM 5xx, none of which announce
	//    themselves any other way.
	if s.ModelTokens <= 0 {
		return meterapi.OutcomeInfraFailed
	}
	// 5. The model answered, and the work is not there. THIS, and only this, is a
	//    gate failure, and only this buys a rung.
	return meterapi.OutcomeGateFailed
}

// Escalates reports whether an outcome buys the next rung on the ladder. It is a
// one-line function guarding the entire cost model. Meter enforces the same rule
// independently (Plan 03's rung.Outcome.Escalates) -- two agreeing enforcers, on
// purpose.
func Escalates(outcome string) bool { return outcome == meterapi.OutcomeGateFailed }

// Known false positive -- record it, do not hide it.
//
// A pod evicted AFTER at least one successful completion but BEFORE posting its
// comment classifies as gate-failed and buys ONE unearned escalation. It is
// bounded (one rung, one attempt) and the escalated attempt still passes
// /decide, so it cannot exceed budget. Closing it properly needs a
// pod-termination signal from Gas City's session provider, which is not in the
// facts available to this plan.
//
// Hand-off: Plan 06 should measure how often it fires (K-series kill tests
// already evict pods); if it is common, it becomes an upstream ask. This
// paragraph is also recorded in ADR-004 verbatim.
