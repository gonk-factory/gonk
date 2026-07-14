// Package intake is gonk-intake: GitLab webhook receipt, reconciliation of the
// bot's project memberships and .gonk.yml state, the deterministic onboarding
// MR, and dispatch of work to the Gas City supervisor.
//
// Everything in this package is deterministic. No code path here calls a model.
//
// AND: no code path here calls gonkcfg.Resolve. Config resolution belongs to
// gonk-meter, which is the only component holding operator (instance/group)
// policy -- two independent resolvers would be two sources of truth for a budget
// ceiling. Intake pushes RAW .gonk.yml bytes to meter and reads the resolved
// answer back. See "Division of responsibility" in the plan.
package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// MaxConfigBytes caps .gonk.yml. It is project-authored, attacker-controlled
// content (ADR-002 "Untrusted input"); the real file is ~400 bytes.
const MaxConfigBytes int64 = 64 << 10

// ConfigPath is where a project declares itself tagged in (spec 5.1).
const ConfigPath = ".gonk.yml"

// AgentDir is the Navigator-style context directory (spec 5.3). Its absence is
// what makes a project `pending`.
const AgentDir = ".agent"

type State string

const (
	// Determined by GitLab alone.
	StateUnmanaged State = "unmanaged"
	StateAbsent    State = "absent"
	StateDeclined  State = "declined"

	// Determined by NEITHER: we have config bytes, and no answer from meter.
	StateUnsynced State = "unsynced"

	// Determined by METER. Intake records these; it cannot compute them.
	StateInvalid    State = "invalid"
	StateDisabled   State = "disabled"
	StateKeyMissing State = "key-missing"

	// The join: meter says active, and we looked in the repo.
	StatePending State = "pending"
	StateValid   State = "valid"
)

// AllStates is the metric label domain; keep it in sync with the constants.
var AllStates = []State{
	StateUnmanaged, StateAbsent, StateDeclined, StateUnsynced,
	StateInvalid, StateDisabled, StateKeyMissing, StatePending, StateValid,
}

// Observation is what intake can see IN GITLAB. Separating observation from
// classification is what makes the decision a pure function.
type Observation struct {
	Member          bool
	ConfigBytes     []byte // nil when .gonk.yml is absent
	ConfigTooLarge  bool   // fetch exceeded MaxConfigBytes
	ConfigCommitSHA string // the commit .gonk.yml was read at; "" if unknown
	AgentDirPresent bool
	// OnboardingDeclined: an onboarding MR was closed unmerged and the bot has
	// not been re-invited since (spec 5.3; see AD-3 for how that is derived).
	OnboardingDeclined bool
}

// Classification is the single source of truth for what intake may DO to a
// project. Consumers must ask the May* predicates rather than re-deriving
// permission from State.
type Classification struct {
	State  State
	Reason string // why, for the negative states; "" otherwise
	// Meter is meter's answer, verbatim. NIL means meter has not answered.
	// Effective policy is read from HERE and nowhere else.
	Meter      *meterapi.ProjectResponse
	ConfigHash string // sha256 of the raw bytes; "" when there is no config
	// LooksInvalid is intake's own gonkcfg.Load result. ADVISORY ONLY -- it
	// drives a metric and the onboarding flow. Meter's 422 is authoritative, and
	// where the two disagree, METER WINS (it has the operator config; we do not).
	LooksInvalid bool
	LoadError    string
}

// eff is meter's Effective, or a zero value if meter has not answered. Every
// May* predicate below gates on State first, and StateValid/StatePending are
// only reachable when Meter is non-nil with a non-nil Effective -- so this
// cannot hand out a permissive zero value.
func (c Classification) eff() meterapi.Effective {
	if c.Meter == nil || c.Meter.Effective == nil {
		return meterapi.Effective{}
	}
	return *c.Meter.Effective
}

// MayTriage. NOTE the Actions check is a PRE-FILTER, not enforcement: meter
// vetoes at /decide, the only chokepoint before a session spawns. We check it
// here purely to avoid firing an order that will certainly be denied. It must
// therefore be a SUBSET of meter's rules -- never a superset, or we silently
// drop work meter would have allowed.
func (c Classification) MayTriage() bool {
	return c.State == StateValid && c.eff().Actions.Triage
}

// MayScaffold: spec 5.3 -- the .agent/ scaffold MR is the one metered action
// permitted while a project is pending, authorized by the just-merged config.
func (c Classification) MayScaffold() bool { return c.State == StatePending }

// MayOnboard: the deterministic onboarding MR. Not gated on Actions (the project
// has no config yet to opt in with) and not a metered action.
func (c Classification) MayOnboard() bool { return c.State == StateAbsent }

// MayMentionReply follows triage (spec 5.5).
//
// triage.respond_to_mentions is NOT one of Effective.Actions, so METER DOES NOT
// CHECK IT. Intake is its only enforcement point. Drop this and the setting is
// enforced nowhere at all.
func (c Classification) MayMentionReply() bool {
	return c.MayTriage() && c.eff().Triage.RespondToMentions
}

// Classify is a pure function: same inputs, same answer, no IO, no clock, NO
// RESOLVER. `mr` is meter's response for this project, or nil if meter has not
// answered (unreachable, or we have not asked yet).
func Classify(obs Observation, mr *meterapi.ProjectResponse) Classification {
	if !obs.Member {
		return Classification{State: StateUnmanaged, Reason: "bot is not a member"}
	}

	// No config -> this is an onboarding question, and meter is not involved.
	if obs.ConfigBytes == nil && !obs.ConfigTooLarge {
		if obs.OnboardingDeclined {
			return Classification{State: StateDeclined, Reason: "onboarding merge request was closed unmerged"}
		}
		return Classification{State: StateAbsent, Reason: "no " + ConfigPath}
	}

	c := Classification{Meter: mr, ConfigHash: hashConfig(obs.ConfigBytes)}

	// Oversize: we never even fetched the bytes, so meter cannot have seen them.
	// This is the one validity call intake makes on its own authority, because it
	// is about the FETCH, not about the content.
	if obs.ConfigTooLarge {
		c.State = StateInvalid
		c.Reason = fmt.Sprintf("%s exceeds %d bytes", ConfigPath, MaxConfigBytes)
		c.LooksInvalid = true
		return c
	}

	// Our own read of the bytes. ADVISORY: it drives a metric and lets the
	// onboarding flow say something useful before meter has answered. It does NOT
	// decide the state when meter has spoken.
	if _, err := gonkcfg.Load(obs.ConfigBytes); err != nil {
		c.LooksInvalid, c.LoadError = true, err.Error()
	}

	// No answer from meter: we have a config and we do not know what it MEANS.
	// We have no operator policy and no resolver, so we cannot find out. Fail
	// closed and try again next pass.
	if mr == nil {
		c.State = StateUnsynced
		c.Reason = "gonk-meter has not resolved this project's config yet"
		if c.LooksInvalid {
			c.Reason = "gonk-meter has not resolved this project's config yet (and it does not appear to parse)"
		}
		return c
	}

	// Meter has spoken. Record its verdict.
	switch mr.State {
	case meterapi.StateInvalid:
		c.State, c.Reason = StateInvalid, mr.Error
		return c
	case meterapi.StateDisabled:
		// ADR-002's invariant, carried onto the wire: DisabledReason is non-empty
		// iff the project is disabled.
		c.State, c.Reason = StateDisabled, mr.DisabledReason
		return c
	case meterapi.StateKeyMissing:
		c.State = StateKeyMissing
		c.Reason = "gonk-meter has not provisioned this project's LiteLLM key yet"
		return c
	case meterapi.StateActive:
		// fall through
	default:
		// An unknown state from a newer meter. Fail closed rather than guess.
		c.State = StateUnsynced
		c.Reason = fmt.Sprintf("gonk-meter returned an unknown state %q", mr.State)
		return c
	}

	// Active. The remaining question is ours: is the repo scaffolded?
	if !obs.AgentDirPresent {
		c.State = StatePending
		c.Reason = "no " + AgentDir + "/ yet: scaffold merge request pending"
		return c
	}
	c.State = StateValid
	return c
}

// hashConfig identifies a config version for the order payload (so a session
// records which config authorized it) and for the reconcile short-circuit.
func hashConfig(raw []byte) string {
	if raw == nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
