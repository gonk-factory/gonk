// Package meterapi is the wire contract between gonk-meter (the server),
// gitlab-intake (the config client), and the Gas City pack (the policy client).
// All three import it; NONE of them re-derives the JSON shape.
//
// Ownership: gonk-meter (Plan 03) owns this contract. gitlab-intake conforms to
// it. If a client needs a field that is not here, that is a change to the METER
// contract and must be made here first, with the sha256 drift gate updated in
// the same commit.
//
// Endpoints (see docs/api/gonk-meter-v1.md):
//
//	PUT    /v1/projects/{project}            ProjectRequest  -> ProjectResponse   (intake)
//	GET    /v1/projects/{project}                            -> ProjectResponse   (intake)
//	DELETE /v1/projects/{project}                            -> 204               (intake)
//	POST   /v1/projects/{project}/key/rotate                 -> ProjectResponse   (operator)
//	POST   /v1/policy/decide                 DecideRequest   -> DecideResponse    (intake Gate 1 + pack Gate 2; idempotent on an open reservation)
//	POST   /v1/policy/outcome                OutcomeRequest  -> OutcomeResponse    (pack)
//	GET    /v1/cost/bead/{bead_id}                           -> BeadCostResponse
//	GET    /v1/cost/session/{session_key}                    -> SessionCostResponse
//	GET    /v1/cost/project/{project}                        -> ProjectCostResponse
//	GET    /v1/cost/instance                                 -> InstanceCostResponse
//	GET    /healthz | /readyz | /metrics                     (unauthenticated)
//
// Everything except the health/metrics endpoints requires
// `Authorization: Bearer <token>`.
package meterapi

import (
	"fmt"
	"math"
	"net/url"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// ---------------------------------------------------------------- paths

// ProjectPath builds the project path. {project} is the GitLab
// path_with_namespace, URL-path-escaped: "group/repo" -> "group%2Frepo".
// Never hand-build this: an unescaped "/" silently routes to a different (or no)
// handler.
func ProjectPath(project string) string {
	return "/v1/projects/" + url.PathEscape(project)
}

func ProjectKeyRotatePath(project string) string { return ProjectPath(project) + "/key/rotate" }

func CostBeadPath(beadID string) string { return "/v1/cost/bead/" + url.PathEscape(beadID) }
func CostSessionPath(sessionKey string) string {
	return "/v1/cost/session/" + url.PathEscape(sessionKey)
}
func CostProjectPath(project string) string { return "/v1/cost/project/" + url.PathEscape(project) }

const (
	DecidePath       = "/v1/policy/decide"
	OutcomePath      = "/v1/policy/outcome"
	CostInstancePath = "/v1/cost/instance"
	HealthzPath      = "/healthz"
	ReadyzPath       = "/readyz"
	MetricsPath      = "/metrics"
)

// ---------------------------------------------------------------- budget

// Budget is an EffectiveBudget, wire-shaped. NULL MEANS UNLIMITED.
//
// gonkcfg encodes "unlimited" as math.Inf(1) (cost) and math.MaxInt64 (tokens)
// -- ADR-002. Neither survives contact with JSON: encoding/json REFUSES to
// marshal +Inf (it returns an error, not a number), so an unlimited project
// would fail to serialize AT ALL; and MaxInt64 loses precision in any
// float64-based parser, which is every JavaScript client, so a "limit" silently
// becomes a DIFFERENT limit. `null` is unambiguous, symmetric across both types,
// and round-trips.
//
// NOBODY may marshal gonkcfg.EffectiveBudget directly. This type is the only
// correct way across the wire.
type Budget struct {
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
	MonthlyTokens  *int64   `json:"monthly_tokens"`
	PerTaskTokens  *int64   `json:"per_task_tokens"`
}

const unlimitedTokens = gonkcfg.TokenQuantity(math.MaxInt64)

// BudgetFrom converts a resolved budget for the wire. It errors on a non-finite
// cost ceiling: Resolve pins those to 0 and disables the project, so this is
// unreachable in practice -- but the seam does not trust that, because an
// unvalidated operator policy is exactly the gap ADR-002 leaves open.
func BudgetFrom(b gonkcfg.EffectiveBudget) (Budget, error) {
	var out Budget
	switch {
	case math.IsInf(b.MonthlyCostUSD, 1):
		// unlimited -> null
	case math.IsNaN(b.MonthlyCostUSD) || math.IsInf(b.MonthlyCostUSD, -1):
		return Budget{}, fmt.Errorf("meterapi: monthly_cost_usd is not a finite number (%v)", b.MonthlyCostUSD)
	default:
		v := b.MonthlyCostUSD
		out.MonthlyCostUSD = &v
	}
	if b.MonthlyTokens != unlimitedTokens {
		v := int64(b.MonthlyTokens)
		out.MonthlyTokens = &v
	}
	if b.PerTaskTokens != unlimitedTokens {
		v := int64(b.PerTaskTokens)
		out.PerTaskTokens = &v
	}
	return out, nil
}

// Effective is the inverse: null becomes the unlimited sentinels again.
func (b Budget) Effective() gonkcfg.EffectiveBudget {
	out := gonkcfg.EffectiveBudget{
		MonthlyCostUSD: math.Inf(1),
		MonthlyTokens:  unlimitedTokens,
		PerTaskTokens:  unlimitedTokens,
	}
	if b.MonthlyCostUSD != nil {
		out.MonthlyCostUSD = *b.MonthlyCostUSD
	}
	if b.MonthlyTokens != nil {
		out.MonthlyTokens = gonkcfg.TokenQuantity(*b.MonthlyTokens)
	}
	if b.PerTaskTokens != nil {
		out.PerTaskTokens = gonkcfg.TokenQuantity(*b.PerTaskTokens)
	}
	return out
}

// ZeroBudget is the fail-closed budget: nothing is affordable. It is what an
// INVALID project gets. Never send an empty Budget{} for a project you are
// disabling -- an empty Budget{} is all-nil, and nil means UNLIMITED.
func ZeroBudget() Budget {
	zero, zeroTokens := 0.0, int64(0)
	return Budget{MonthlyCostUSD: &zero, MonthlyTokens: &zeroTokens, PerTaskTokens: &zeroTokens}
}

// ---------------------------------------------------------------- project state

// State is a project's registration state AS DETERMINED BY METER. Intake
// records it; intake cannot compute it (only meter resolves config).
type State string

const (
	StateActive     State = "active"      // resolved, enabled, virtual key provisioned
	StateDisabled   State = "disabled"    // resolved, Effective.Enabled == false
	StateInvalid    State = "invalid"     // .gonk.yml would not load; there is NO Effective
	StateKeyMissing State = "key-missing" // resolved and enabled, but LiteLLM key not provisioned yet
)

// KeyRef points at WHERE a LiteLLM virtual key lives. It NEVER contains the key
// itself: a token in an API response ends up in the event bus and in every log
// line that echoes it.
type KeyRef struct {
	SecretName string `json:"secret_name"`
	SecretKey  string `json:"secret_key"`
}

type Actions struct {
	Triage    bool `json:"triage"`
	Pipelines bool `json:"pipelines"`
	Features  bool `json:"features"`
}

type Triage struct {
	LabelPrefix       string `json:"label_prefix"`
	RespondToMentions bool   `json:"respond_to_mentions"`
}

type Provenance struct {
	CommitTrailers bool `json:"commit_trailers"`
	IncludeUsage   bool `json:"include_usage"`
}

type Schedule struct {
	QuietHours string `json:"quiet_hours"`
	Timezone   string `json:"timezone"`
}

// Effective is gonkcfg.Effective, wire-shaped. It is produced by exactly ONE
// call site in the entire system: gonk-meter's service.Register, which calls
// gonkcfg.Resolve. Per ADR-002, Resolve's return value is the only legitimate
// way to produce an Effective -- so intake reads this off the wire and NEVER
// computes it.
//
// Note the ABSENCE of Budget: the budget lives on ProjectResponse, because a
// disabled or invalid project has a budget (zero) but no meaningful Effective.
type Effective struct {
	Enabled    bool       `json:"enabled"`
	Actions    Actions    `json:"actions"`
	Ladder     []string   `json:"ladder"`
	Continuity string     `json:"continuity"`
	Triage     Triage     `json:"triage"`
	Provenance Provenance `json:"provenance"`
	Schedule   *Schedule  `json:"schedule"`
}

// EffectiveFrom is meter-side only: it projects a resolved gonkcfg.Effective
// onto the wire type.
func EffectiveFrom(e gonkcfg.Effective) Effective {
	out := Effective{
		Enabled:    e.Enabled,
		Actions:    Actions{Triage: e.Actions.Triage, Pipelines: e.Actions.Pipelines, Features: e.Actions.Features},
		Ladder:     append([]string(nil), e.Ladder...),
		Continuity: e.Continuity,
		Triage:     Triage{LabelPrefix: e.Triage.LabelPrefix, RespondToMentions: e.Triage.RespondToMentions},
		Provenance: Provenance{CommitTrailers: e.Provenance.CommitTrailers, IncludeUsage: e.Provenance.IncludeUsage},
	}
	if e.Schedule != nil {
		out.Schedule = &Schedule{QuietHours: e.Schedule.QuietHours, Timezone: e.Schedule.Timezone}
	}
	return out
}

// ---------------------------------------------------------------- PUT/GET/DELETE /v1/projects/{project}

// ProjectRequest is an idempotent upsert of one project, FROM ITS RAW .gonk.yml.
//
// Intake sends the BYTES IT READ FROM GITLAB, verbatim. It does NOT resolve them
// -- meter is the only component that holds operator (instance/group) policy, and
// two independent resolvers would mean two sources of truth for a budget ceiling.
// Meter validates, folds the nested-group-aware group policy over the instance
// policy, calls gonkcfg.Resolve, and owns the resulting Effective.
type ProjectRequest struct {
	Project         string `json:"project"` // GitLab path_with_namespace
	ProjectID       int64  `json:"project_id"`
	Rig             string `json:"rig"`
	DefaultBranch   string `json:"default_branch"`
	ConfigCommitSHA string `json:"config_commit_sha"` // commit .gonk.yml was read at; "" if unknown
	GonkYML         string `json:"gonk_yml"`          // RAW .gonk.yml bytes. Meter is the validator.
}

// ProjectResponse is meter's answer, and the ONLY legitimate source of a
// project's effective policy for any other component.
//
// Invariants (asserted by the golden tests, and relied on by intake):
//   - Effective is nil IF AND ONLY IF State == StateInvalid. An invalid config
//     has no Effective; per ADR-002 a zero-value Effective must never be treated
//     as a resolved one.
//   - Error is non-empty IF AND ONLY IF State == StateInvalid.
//   - DisabledReason is non-empty IF AND ONLY IF State == StateDisabled
//     (ADR-002's invariant, carried onto the wire).
//   - KeyRef is populated IF AND ONLY IF State == StateActive.
//   - Budget is ZeroBudget() when State is Invalid or Disabled. NEVER an empty
//     Budget{} -- that is all-nil, and nil means UNLIMITED.
type ProjectResponse struct {
	Project        string     `json:"project"`
	Rig            string     `json:"rig"`
	State          State      `json:"state"`
	DisabledReason string     `json:"disabled_reason"`
	Error          string     `json:"error,omitempty"`
	Effective      *Effective `json:"effective"`
	Budget         Budget     `json:"budget"`
	KeyRef         KeyRef     `json:"key_ref"`
	ConfigHash     string     `json:"config_hash"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ---------------------------------------------------------------- POST /v1/policy/decide

// Decision kinds. defer and deny are HTTP 200: they are normal answers.
const (
	DecisionRun   = "run"
	DecisionDefer = "defer"
	DecisionDeny  = "deny"
)

// Machine-readable decision reasons. A BOUNDED set on purpose: they are
// Prometheus label values and part of this contract.
const (
	ReasonNotRegistered          = "project-not-registered"
	ReasonDisabled               = "disabled"
	ReasonInvalidConfig          = "invalid-config"
	ReasonActionNotAllowed       = "action-not-allowed"
	ReasonKeyMissing             = "virtual-key-missing"
	ReasonQuietHours             = "quiet-hours"
	ReasonSpendStale             = "spend-data-stale"
	ReasonLadderExhausted        = "ladder-exhausted"
	ReasonInfraRetriesExhausted  = "infra-retries-exhausted"
	ReasonPerTaskTokensExhausted = "per-task-tokens-exhausted"
	ReasonMonthlyTokensExhausted = "monthly-tokens-exhausted"
	ReasonMonthlyCostExhausted   = "monthly-cost-exhausted"
)

// DecideRequest asks meter which rung the next attempt runs at.
//
// THERE IS DELIBERATELY NO ATTEMPT COUNT AND NO PRIOR-OUTCOME LIST, and there
// never will be. Meter owns ladder state, reading it from its own store. A
// caller-supplied attempt number is a forgery vector: raise it and you skip
// straight to the most expensive rung. Any client that tries to send one is
// wrong; any server that reads one is a bug.
type DecideRequest struct {
	Project string `json:"project"`
	Rig     string `json:"rig"`
	// BeadID is the deterministic BeadAnchor -- the stable identifier derived from
	// the GitLab artifact (`gonk:{project_id}:issue:{iid}`), IDENTICAL at intake's
	// Gate 1 and the pack's Gate 2 and stable across every re-sling. It is NOT the
	// Gas City internal bead id: that id does not even exist at Gate 1 (the first
	// dispatch is what creates it), and it is an execution detail, not a budget
	// key. /decide IDEMPOTENCY, ladder state, and reservation binding all key on
	// this value -- so both gates MUST send the same one, or meter mints two
	// reservations for one attempt (double headroom, a leaked reservation). If
	// per-session tracing ever needs the Gas City bead id, that is a separate
	// future field (YAGNI -- do not add it now).
	BeadID     string `json:"bead_id"`
	SessionKey string `json:"session_key"`
	Trigger    string `json:"trigger"` // an atags.Trigger* value
}

// DecideResponse. On a defer or a deny, Metadata is empty, KeyRef is zero, and
// ReservationID is empty: a decision that is not going to run hands out neither
// an attribution identity nor a route to a credential.
type DecideResponse struct {
	Decision string `json:"decision"` // run | defer | deny
	Rung     string `json:"rung"`
	Model    string `json:"model,omitempty"`
	Attempt  int    `json:"attempt"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
	// RetryAfter is set on EVERY defer. A defer with no retry_after is an
	// infinite park.
	RetryAfter time.Time `json:"retry_after,omitzero"`
	// Metadata is atags.Metadata(). METER mints it, so meter is the single
	// boundary where tag values are charset-validated. The pack stamps it
	// verbatim onto every LiteLLM request.
	Metadata             map[string]string `json:"metadata"`
	KeyRef               KeyRef            `json:"key_ref"`
	ReservationID        string            `json:"reservation_id,omitempty"`
	ReservationExpiresAt time.Time         `json:"reservation_expires_at,omitzero"`
	Budget               Budget            `json:"budget"`
	// Remaining is REAL dollars and real tokens. Synthetic local-model dollars
	// are not spend and never appear here.
	Remaining Budget    `json:"remaining"`
	SpendAsOf time.Time `json:"spend_as_of"`
}

// ---------------------------------------------------------------- POST /v1/policy/outcome

// Outcome values. Only OutcomeGateFailed escalates the ladder.
const (
	OutcomeSuccess     = "success"
	OutcomeGateFailed  = "gate-failed"
	OutcomeInfraFailed = "infra-failed"
	OutcomeAborted     = "aborted"
)

// OutcomeRequest reports how one attempt ended. ReservationID binds the outcome
// to a reservation METER minted: without it, `gate-failed` is a forgery vector
// (repeat it and a project walks itself up to its most expensive rung).
type OutcomeRequest struct {
	Project string `json:"project"`
	// BeadID is the SAME deterministic BeadAnchor the two /decide gates sent (see
	// DecideRequest.BeadID) -- NOT the Gas City bead id. The outcome must settle the
	// reservation that the matching /decide opened, so it has to key on the same
	// identifier or the reservation it names does not exist.
	BeadID        string `json:"bead_id"`
	SessionKey    string `json:"session_key"`
	Attempt       int    `json:"attempt"`
	Rung          string `json:"rung"`
	ReservationID string `json:"reservation_id"`
	Outcome       string `json:"outcome"`
}

// OutcomeResponse. Next/NextRung are ADVISORY -- a preview for logs and
// dashboards. The authoritative answer is always the next /decide.
type OutcomeResponse struct {
	OK              bool   `json:"ok"`
	RecordedAttempt int    `json:"recorded_attempt"`
	Next            string `json:"next"` // escalate | retry | done
	NextRung        string `json:"next_rung"`
}

// ---------------------------------------------------------------- cost

// RungCost is one rung's slice of a total.
//
// CostUSD and SyntheticCostUSD are DIFFERENT CURRENCIES and a client must never
// add them. Local rungs cost no real money; they are priced synthetically in
// LiteLLM so that the USD virtual-key ceiling is a hard door for tokens too. A
// synthetic dollar is an accounting unit, NOT SPEND. Present it labelled or not
// at all.
type RungCost struct {
	Rung             string  `json:"rung"`
	Kind             string  `json:"kind"` // local | cloud
	CostUSD          float64 `json:"cost_usd"`
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"`
	CostSynthetic    bool    `json:"cost_synthetic"` // true iff Kind == "local"
	TotalTokens      int64   `json:"total_tokens"`
	Calls            int     `json:"calls"`
}

type TriggerCost struct {
	Trigger          string  `json:"trigger"`
	CostUSD          float64 `json:"cost_usd"`
	SyntheticCostUSD float64 `json:"synthetic_cost_usd"`
	TotalTokens      int64   `json:"total_tokens"`
	Calls            int     `json:"calls"`
}

type AttemptView struct {
	Attempt int    `json:"attempt"`
	Rung    string `json:"rung"`
	Outcome string `json:"outcome"`
}

type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// BeadCostResponse: lifetime, not windowed. The per-task ceiling is a property
// of the WORK ITEM, so a bead that straddles a month boundary keeps the same
// per-task budget.
type BeadCostResponse struct {
	BeadID           string        `json:"bead_id"`
	Project          string        `json:"project"`
	CostUSD          float64       `json:"cost_usd"`
	SyntheticCostUSD float64       `json:"synthetic_cost_usd"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	ByRung           []RungCost    `json:"by_rung"`
	Attempts         []AttemptView `json:"attempts"`
	AsOf             time.Time     `json:"as_of"`
	// Complete is false iff an open reservation exists for this scope: spend
	// rows for it may not have landed. A client that publishes a cost (a commit
	// trailer!) MUST NOT publish one when Complete is false.
	Complete bool `json:"complete"`
}

type SessionCostResponse struct {
	SessionKey       string     `json:"session_key"`
	Project          string     `json:"project"`
	BeadID           string     `json:"bead_id"`
	CostUSD          float64    `json:"cost_usd"`
	SyntheticCostUSD float64    `json:"synthetic_cost_usd"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	TotalTokens      int64      `json:"total_tokens"`
	ByRung           []RungCost `json:"by_rung"`
	AsOf             time.Time  `json:"as_of"`
	Complete         bool       `json:"complete"`
}

type ProjectCostResponse struct {
	Project          string        `json:"project"`
	Window           Window        `json:"window"`
	CostUSD          float64       `json:"cost_usd"`
	SyntheticCostUSD float64       `json:"synthetic_cost_usd"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	ByRung           []RungCost    `json:"by_rung"`
	ByTrigger        []TriggerCost `json:"by_trigger"`
	Budget           Budget        `json:"budget"`
	Remaining        Budget        `json:"remaining"`
	AsOf             time.Time     `json:"as_of"`
	Complete         bool          `json:"complete"`
	Stale            bool          `json:"stale"`
}

type InstanceCostResponse struct {
	Window           Window                `json:"window"`
	CostUSD          float64               `json:"cost_usd"`
	SyntheticCostUSD float64               `json:"synthetic_cost_usd"`
	TotalTokens      int64                 `json:"total_tokens"`
	ByProject        []ProjectCostResponse `json:"by_project"`
	AsOf             time.Time             `json:"as_of"`
	Complete         bool                  `json:"complete"`
}

// ---------------------------------------------------------------- errors

// ErrorResponse is the body of a 400 (a malformed REQUEST) and of a 500.
//
// It is NOT the body of a 422: a 422 means the project's .gonk.yml would not
// load, which is a successful, idempotent registration of an invalid config, and
// it returns a full ProjectResponse with State == StateInvalid and Error set.
type ErrorResponse struct {
	Error string `json:"error"`
}
