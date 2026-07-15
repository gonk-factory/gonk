// Package service is gonk-meter's brain and its HTTP surface: it composes
// rung policy (pkg/rung), the reservation store (internal/meter/store), the
// LiteLLM seam (internal/meter/litellm), and tag minting
// (internal/meter/tagmint) into the wire contract (pkg/meterapi).
//
// This is the money path. POST /v1/policy/decide reserves budget; nothing in
// this package may hand back a `run` decision without a durable reservation
// backing it, and nothing may fail in a way that lets a caller spend without
// one. See service_test.go's fail-closed matrix.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/keysink"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/litellm"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/tagmint"
	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/opercfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// keyBudgetDuration is LiteLLM's key `budget_duration`, pinned to match
// meter's UTC calendar month (AD-9). Never derive this from anything else.
const keyBudgetDuration = "1mo"

// spendPollOverlap is how far before the last-seen cursor a spend poll
// re-reads. Overlap is safe (AddSpendRows dedupes by CallID); it exists so a
// row LiteLLM committed a moment after the previous poll's window closed is
// not missed.
const spendPollOverlap = 2 * time.Minute

// ErrUnknownReservation is returned by Outcome when reservation_id does not
// name a reservation meter minted for this exact (project, bead, attempt).
// The caller maps this to 400: no reservation, no attempt recorded.
var ErrUnknownReservation = errors.New("service: outcome: unknown or mismatched reservation")

// ErrBadOutcome is returned by Outcome for an outcome value outside
// rung.Outcome's bounded set.
var ErrBadOutcome = errors.New("service: outcome: invalid outcome value")

// Service is gonk-meter's whole brain. Every time-dependent behavior --
// staleness, quiet hours, reservation expiry, month rollover -- reads the
// clock through `now`, which is a field rather than time.Now() so every one
// of those behaviors is an ordinary table row in a test, not a sleep.
type Service struct {
	store store.Store
	admin litellm.Admin
	spend litellm.SpendSource
	keys  keysink.KeySink

	now func() time.Time

	locks keyedMutex // per-project serialization of decide+reserve (contention control, NOT the safety mechanism -- see store.ReserveIfFits)

	mu     sync.RWMutex
	cfg    *opercfg.OperatorConfig
	synced bool // has a spend sync ever succeeded?
	skewOK bool
}

// New builds a Service. now is the service's only clock.
func New(cfg *opercfg.OperatorConfig, st store.Store, admin litellm.Admin, spendSrc litellm.SpendSource, keys keysink.KeySink, now func() time.Time) *Service {
	return &Service{cfg: cfg, store: st, admin: admin, spend: spendSrc, keys: keys, now: now}
}

// Config returns the operator config currently in force. Safe for concurrent
// use with SetConfig (the reresolve loop's hot-reload).
func (s *Service) Config() *opercfg.OperatorConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetConfig hot-swaps the operator config. The caller (main's reresolve
// ticker) is responsible for re-reading the file and validating it first: an
// operator config that fails to validate must never reach here (keep the
// previous config and alert instead).
func (s *Service) SetConfig(cfg *opercfg.OperatorConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Ready reports whether meter trusts its own numbers enough to decide on
// them: a spend sync has completed at least once, and the clock agrees with
// the spend source's clock closely enough (spend.SkewOK). /readyz is 503
// otherwise, so Kubernetes takes meter out of service rather than letting it
// answer with numbers it does not trust.
func (s *Service) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.synced && s.skewOK
}

// keyedMutex serializes Decide per project WITHIN one process. It is a
// throughput/ordering optimization ONLY -- it cuts wasted work (two concurrent
// decides for one project both computing budget and one losing the race) and
// keeps Decide deterministic within a process. It is emphatically NOT a
// correctness mechanism: it does nothing across pods, and neither money-safety
// property depends on it.
//
// Both properties live in store.ReserveIfFits and hold ACROSS REPLICAS:
// no overspend (Postgres SELECT ... FOR UPDATE on the project lock row) and no
// double-reserve (partial UNIQUE index on the open (project, bead, session)
// key). Removing this mutex breaks neither -- the store's cross-replica race
// tests prove it by calling ReserveIfFits directly with no mutex in the way.
// This is why AD-10's "meter runs single-replica" constraint is LIFTED (see
// the store package doc and forthcoming ADR-004): meter is multi-replica-safe.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l, ok := k.m[key]
	if !ok {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	k.mu.Unlock()

	l.Lock()
	return l.Unlock
}

// newID returns a short unguessable id. crypto/rand, never math/rand: this
// value names a reservation on the money path.
func newID(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("service: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// configHash is meter's idempotency key for a project's raw .gonk.yml.
func configHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- registration

// Register is the whole onboarding path, and the resolved half of Conflict
// A: intake sends raw .gonk.yml bytes plus GitLab metadata; meter is the
// only component holding operator policy, so meter is the only component
// that resolves.
func (s *Service) Register(ctx context.Context, req meterapi.ProjectRequest) (meterapi.ProjectResponse, int, error) {
	return s.resolveProject(ctx, req.Project, req.Rig, []byte(req.GonkYML), s.now())
}

// Get returns a project's current registration, wire-shaped. ok is false if
// the project has never been registered.
func (s *Service) Get(ctx context.Context, project string) (meterapi.ProjectResponse, bool, error) {
	reg, ok, err := s.store.GetRegistration(ctx, project)
	if err != nil {
		return meterapi.ProjectResponse{}, false, err
	}
	if !ok {
		return meterapi.ProjectResponse{}, false, nil
	}
	resp, err := registrationResponse(reg)
	if err != nil {
		return meterapi.ProjectResponse{}, false, err
	}
	return resp, true, nil
}

// Delete de-onboards a project: disables it, deletes its virtual key (both
// the LiteLLM side and the keysink pointer), and drops the registration.
// Idempotent -- deleting an unknown project is a no-op, not an error: the
// caller (http.go) reports 204 either way, because intake retries.
func (s *Service) Delete(ctx context.Context, project string) error {
	if err := s.deleteKeyIfAny(ctx, project); err != nil {
		return err
	}
	return s.store.DeleteRegistration(ctx, project)
}

// deleteKeyIfAny removes a project's live LiteLLM virtual key AND its keysink
// pointer. Both, not just the pointer: a broken or disabled config "must not
// keep spending" means the credential a pod already holds has to stop
// working, not just become unreachable through the keysink.
func (s *Service) deleteKeyIfAny(ctx context.Context, project string) error {
	existing, ok, err := s.store.GetRegistration(ctx, project)
	if err != nil {
		return fmt.Errorf("service: delete key for %q: get registration: %w", project, err)
	}
	if ok && existing.KeyAlias != "" {
		if err := s.admin.DeleteKey(ctx, existing.KeyAlias); err != nil {
			return fmt.Errorf("service: delete key for %q: admin: %w", project, err)
		}
	}
	if err := s.keys.Delete(ctx, project); err != nil {
		return fmt.Errorf("service: delete key for %q: keysink: %w", project, err)
	}
	return nil
}

// resolveProject is the shared core of Register and the reresolve loop: load
// + validate the raw .gonk.yml, fold operator policy over it via the ONLY
// call to gonkcfg.Resolve in the system, and manage the project's key and
// registration accordingly. It never trusts a caller-supplied Effective --
// there is no such thing on the wire.
func (s *Service) resolveProject(ctx context.Context, project, rig string, raw []byte, now time.Time) (meterapi.ProjectResponse, int, error) {
	cfg := s.Config()

	invalid := func(detail string) (meterapi.ProjectResponse, int, error) {
		// A broken config must not keep spending: delete any existing key before
		// recording the project as invalid.
		if err := s.deleteKeyIfAny(ctx, project); err != nil {
			return meterapi.ProjectResponse{}, 0, err
		}
		reg := store.Registration{
			Project: project, Rig: rig, Raw: raw,
			State: store.StateInvalid, InvalidDetail: detail, UpdatedAt: now,
		}
		if err := s.store.PutRegistration(ctx, reg); err != nil {
			return meterapi.ProjectResponse{}, 0, fmt.Errorf("service: register %q: put invalid registration: %w", project, err)
		}
		resp, err := registrationResponse(reg)
		if err != nil {
			return meterapi.ProjectResponse{}, 0, err
		}
		return resp, http.StatusUnprocessableEntity, nil
	}

	pc, err := gonkcfg.Load(raw)
	if err != nil {
		return invalid(err.Error())
	}
	if cfg.Meter.EnforceLadderOrder {
		if err := opercfg.CheckLadderOrder(cfg.Instance.Ladder, pc.Ladder); err != nil {
			return invalid(err.Error())
		}
	}

	eff := gonkcfg.Resolve(cfg.Instance, cfg.GroupFor(project), *pc)

	var qh *rung.QuietHours
	if eff.Schedule != nil && eff.Schedule.QuietHours != "" {
		qh, err = rung.ParseQuietHours(eff.Schedule.QuietHours, eff.Schedule.Timezone)
		if err != nil {
			// A bad IANA timezone in the project's OWN .gonk.yml is exactly the
			// same class of problem as a schema-invalid or reordered-ladder config:
			// the project's config will not resolve. Route it through the same
			// 422 path for consistency (see the concern noted in the task report).
			return invalid(err.Error())
		}
	}

	if !eff.Enabled {
		if err := s.deleteKeyIfAny(ctx, project); err != nil {
			return meterapi.ProjectResponse{}, 0, err
		}
		reg := store.Registration{
			Project: project, Rig: rig, Raw: raw,
			State: store.StateDisabled, Effective: eff, QuietHours: qh, UpdatedAt: now,
		}
		if err := s.store.PutRegistration(ctx, reg); err != nil {
			return meterapi.ProjectResponse{}, 0, fmt.Errorf("service: register %q: put disabled registration: %w", project, err)
		}
		resp, err := registrationResponse(reg)
		if err != nil {
			return meterapi.ProjectResponse{}, 0, err
		}
		return resp, http.StatusOK, nil
	}

	// Enabled: provision or refresh the virtual key.
	alias := "gonk-" + rig
	models := make([]string, 0, len(eff.Ladder))
	for _, r := range eff.Ladder {
		models = append(models, cfg.Catalog[r].Model)
	}
	spec := litellm.KeySpec{
		Alias:          alias,
		MaxBudgetUSD:   litellm.MaxBudgetFor(eff.Budget, eff.Ladder, cfg.Catalog),
		BudgetDuration: keyBudgetDuration,
		Models:         models,
	}
	info, keyErr := s.admin.EnsureKey(ctx, spec)
	if keyErr != nil {
		// Fail closed, but not loud: onboarding is not at fault, and the
		// reconcile loop retries. /decide will defer with virtual-key-missing
		// until the key lands.
		reg := store.Registration{
			Project: project, Rig: rig, Raw: raw,
			State: store.StateKeyMissing, Effective: eff, QuietHours: qh,
			KeyAlias: alias, UpdatedAt: now,
		}
		if err := s.store.PutRegistration(ctx, reg); err != nil {
			return meterapi.ProjectResponse{}, 0, fmt.Errorf("service: register %q: put key-missing registration: %w", project, err)
		}
		resp, err := registrationResponse(reg)
		if err != nil {
			return meterapi.ProjectResponse{}, 0, err
		}
		return resp, http.StatusOK, nil
	}

	keyRef, err := s.keys.Put(ctx, project, info.Token)
	if err != nil {
		return meterapi.ProjectResponse{}, 0, fmt.Errorf("service: register %q: keysink put: %w", project, err)
	}
	reg := store.Registration{
		Project: project, Rig: rig, Raw: raw,
		State: store.StateActive, Effective: eff, QuietHours: qh,
		KeyRef: keyRef, KeyAlias: alias, UpdatedAt: now,
	}
	if err := s.store.PutRegistration(ctx, reg); err != nil {
		return meterapi.ProjectResponse{}, 0, fmt.Errorf("service: register %q: put active registration: %w", project, err)
	}
	resp, err := registrationResponse(reg)
	if err != nil {
		return meterapi.ProjectResponse{}, 0, err
	}
	return resp, http.StatusOK, nil
}

// registrationResponse projects a stored Registration onto the wire type,
// preserving every invariant ProjectResponse documents: Effective is nil iff
// invalid; DisabledReason is set iff disabled; KeyRef is populated iff
// active; Budget is ZeroBudget() iff invalid or disabled.
func registrationResponse(reg store.Registration) (meterapi.ProjectResponse, error) {
	resp := meterapi.ProjectResponse{
		Project:    reg.Project,
		Rig:        reg.Rig,
		State:      meterapi.State(reg.State),
		ConfigHash: configHash(reg.Raw),
		UpdatedAt:  reg.UpdatedAt,
	}
	switch reg.State {
	case store.StateInvalid:
		resp.Error = reg.InvalidDetail
		resp.Budget = meterapi.ZeroBudget()
		return resp, nil
	case store.StateDisabled:
		resp.DisabledReason = reg.Effective.DisabledReason
		weff := meterapi.EffectiveFrom(reg.Effective)
		resp.Effective = &weff
		resp.Budget = meterapi.ZeroBudget()
		return resp, nil
	default: // active, key-missing: resolved and enabled
		weff := meterapi.EffectiveFrom(reg.Effective)
		resp.Effective = &weff
		bud, err := meterapi.BudgetFrom(reg.Effective.Budget)
		if err != nil {
			return meterapi.ProjectResponse{}, fmt.Errorf("service: registration %q: %w", reg.Project, err)
		}
		resp.Budget = bud
		resp.KeyRef = meterapi.KeyRef{SecretName: reg.KeyRef.SecretName, SecretKey: reg.KeyRef.SecretKey}
		return resp, nil
	}
}

// ---------------------------------------------------------------- decide

// DecideExtras is everything a Run decision needs that is not policy: the
// minted attribution tags, a POINTER to the key (never the key), and the
// reservation. All three are zero for a defer or a deny -- a decision that is
// not going to run must not hand out an attribution identity or a route to a
// credential.
type DecideExtras struct {
	Metadata    map[string]string
	KeyRef      store.KeyRef
	Reservation store.Reservation
}

// Decide is the money path: given a project/bead/session/trigger, decide
// which ladder rung the next attempt runs at (or that it must wait, or that
// it must not run), and -- on a run -- reserve the budget atomically.
//
// It is idempotent on an OPEN RESERVATION (FIX-A): because the rung gate is
// checked in two places (intake Gate 1, the pack's Gate 2), a bead can be
// /decide'd twice for the same (project, bead_id, session_key) before any
// outcome is reported. The second call returns the SAME reservation rather
// than minting a second one -- otherwise one attempt would hold double the
// budget headroom.
func (s *Service) Decide(ctx context.Context, req meterapi.DecideRequest) (rung.Decision, DecideExtras, error) {
	unlock := s.locks.Lock(req.Project)
	defer unlock()

	now := s.now()
	cfg := s.Config()

	reg, ok, err := s.store.GetRegistration(ctx, req.Project)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: get registration: %w", req.Project, err)
	}

	in := rung.Input{
		Registered:      ok,
		Invalid:         ok && reg.State == store.StateInvalid,
		InvalidDetail:   reg.InvalidDetail,
		KeyReady:        ok && reg.State == store.StateActive,
		Effective:       reg.Effective,
		Catalog:         cfg.Catalog,
		Trigger:         req.Trigger,
		QuietHours:      reg.QuietHours,
		Now:             now,
		MaxSpendStale:   cfg.Meter.MaxSpendStaleness,
		KeyRetryBackoff: cfg.Meter.KeyRetryBackoff,
		MaxInfraRetries: cfg.Meter.MaxInfraRetries,
	}

	// A cold start or a skewed clock means we do not trust our own numbers.
	// Present that to Decide as maximally-stale spend, so there is exactly ONE
	// place in the codebase that decides what stale spend means.
	if s.Ready() {
		sa, err := s.store.SyncedAt(ctx)
		if err != nil {
			return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: synced at: %w", req.Project, err)
		}
		in.SpendAsOf = sa
	}

	w, err := s.store.Window(ctx)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: window: %w", req.Project, err)
	}
	in.Window = w

	prior, err := s.store.Attempts(ctx, req.Project, req.BeadID)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: attempts: %w", req.Project, err)
	}
	in.Prior = prior

	openRes, err := s.store.OpenReservations(ctx, req.Project, now)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: open reservations: %w", req.Project, err)
	}

	// *** IDEMPOTENCY (FIX-A) -- FAST PATH. *** An open (unsettled) reservation
	// already covering this exact (bead, session) means the work was already
	// decided and reserved. Hand it back verbatim: skip re-deciding (attempt and
	// rung are stable, since Prior has not changed) AND skip the budget gate --
	// which is the point, because that gate would otherwise see this work's OWN
	// reservation as consumed headroom and spuriously defer the second gate.
	//
	// This reads the STORE (the shared DB in the Postgres deployment), so it is
	// correct across replicas for the sequential Gate-1-then-Gate-2 case: a
	// second replica sees the first's committed reservation here. It is NOT the
	// correctness mechanism, though -- it is a fast path. The truly-concurrent
	// case (two replicas both reach here before either commits) falls through to
	// ReserveIfFits, whose partial unique index + FOR UPDATE lock is what
	// actually prevents a duplicate. Removing this fast path would cost a
	// spurious defer, never a double reservation. (Proven by the store's
	// TestReserveIsIdempotentAcrossReplicas, which bypasses this path entirely.)
	for _, r := range openRes {
		if r.BeadID == req.BeadID && r.SessionKey == req.SessionKey && !r.Settled {
			tags, err := tagmint.Mint(tagmint.Request{
				Project: req.Project, Rig: req.Rig, BeadID: req.BeadID,
				SessionKey: req.SessionKey, Rung: r.Rung, Attempt: r.Attempt, Trigger: req.Trigger,
			})
			if err != nil {
				return rung.Decision{}, DecideExtras{}, err
			}
			d := rung.Decision{Kind: rung.Run, Rung: r.Rung, Model: cfg.Catalog[r.Rung].Model, Attempt: r.Attempt}
			return d, DecideExtras{Metadata: tags.Metadata(), KeyRef: reg.KeyRef, Reservation: r}, nil
		}
	}

	rows, err := s.store.SpendRows(ctx, req.Project)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: spend rows: %w", req.Project, err)
	}
	// in.Spend (observed + already-open reservations) is what rung.Decide's
	// OWN pre-check reads, so its FIRST guess already accounts for concurrent
	// holds rather than always guessing "yes" and relying entirely on the
	// atomic re-check below.
	in.Spend = spendFor(rows, w, req.Project, req.BeadID, openRes)

	// The ceiling, needed again below for the atomic re-check at reserve time.
	bud := budget.FromEffective(reg.Effective.Budget)

	d := rung.Decide(in)

	if d.Kind != rung.Run {
		return d, DecideExtras{}, nil
	}

	spec, ok := cfg.Catalog[d.Rung]
	if !ok {
		// rung.Decide already checks catalog membership before returning Run;
		// unreachable in practice, but never trust that from outside the
		// package it was proven in.
		return rung.Decision{}, DecideExtras{}, fmt.Errorf("service: decide %q: rung %q is not in the catalog", req.Project, d.Rung)
	}
	cost, synthetic, tokens := rung.Reserve(spec)
	res := store.Reservation{
		ID: newID("rsv"), Project: req.Project, BeadID: req.BeadID,
		SessionKey: req.SessionKey, Rung: d.Rung, Attempt: d.Attempt,
		CostUSD: cost, SyntheticCostUSD: synthetic, Tokens: tokens,
		CreatedAt: now, ExpiresAt: now.Add(cfg.Meter.ReservationTTL),
	}

	// The reservation is written by an ATOMIC, IDEMPOTENT check-and-write IN THE
	// STORE. The per-project keyedMutex we hold is contention control only; it
	// does nothing across replicas and is NOT what makes this safe. ReserveIfFits
	// enforces BOTH money-safety properties across replicas: no overspend (FOR
	// UPDATE on the project lock) and no double-reserve (partial unique index on
	// the open (project, bead, session) key). So even if the fast path above
	// missed a concurrent sibling, the store returns that sibling's reservation
	// here rather than minting a duplicate.
	//
	// *** want carries OBSERVED spend ONLY -- never the Reserved* fields. ***
	// ReserveIfFits re-reads open reservations itself, atomically, inside its
	// own lock. Passing in.Spend here (which spendFor already folded open
	// reservations into, for rung.Decide's pre-check above) would COUNT EVERY
	// OPEN RESERVATION TWICE -- once from this call's pre-fold, once from the
	// store's own re-read -- silently halving effective headroom.
	observedOnly := budget.Spend{
		CostUSD: in.Spend.CostUSD, SyntheticCostUSD: in.Spend.SyntheticCostUSD,
		Tokens: in.Spend.Tokens, TaskTokens: in.Spend.TaskTokens,
	}
	result, err := s.store.ReserveIfFits(ctx, req.Project, bud, observedOnly, res)
	if err != nil {
		return rung.Decision{}, DecideExtras{}, err // fail closed: no reservation, no run
	}
	if !result.Fits {
		// We lost a race against a concurrent session. rung.Decide said yes on
		// a snapshot that is now stale. This is a DEFER, not an error and not a
		// deny: the budget is real, it is just spoken for right now.
		return rung.Decision{
			Kind: rung.Defer, Attempt: d.Attempt,
			Reason:     rung.ReasonMonthlyCostExhausted,
			Detail:     "lost a concurrent reservation race for the remaining budget",
			RetryAfter: now.Add(cfg.Meter.MaxSpendStaleness),
		}, DecideExtras{}, nil
	}

	// held is the reservation that actually holds budget for this work: the one
	// we just inserted, OR a pre-existing open one the store deduped us against
	// (result.Existing -- a concurrent gate/replica beat us here). Its rung and
	// attempt are authoritative; use them so both gates return the same run.
	held := result.Reservation
	d.Rung, d.Attempt = held.Rung, held.Attempt
	d.Model = cfg.Catalog[held.Rung].Model

	tags, err := tagmint.Mint(tagmint.Request{
		Project: req.Project, Rig: req.Rig, BeadID: req.BeadID,
		SessionKey: req.SessionKey, Rung: held.Rung, Attempt: held.Attempt, Trigger: req.Trigger,
	})
	if err != nil {
		// The session will never start, so drop the hold -- but ONLY if WE
		// created it. A pre-existing reservation belongs to a sibling gate/replica
		// that already minted its tags successfully; settling it here would kill
		// live work we did not create.
		if !result.Existing {
			_ = s.store.Settle(ctx, res.ID, now)
		}
		return rung.Decision{}, DecideExtras{}, err // 400: a hostile tag never leaves the building
	}

	return d, DecideExtras{Metadata: tags.Metadata(), KeyRef: reg.KeyRef, Reservation: held}, nil
}

// spendFor sums observed spend (windowed by project, lifetime by bead) and
// folds in open reservations into budget.Spend's Reserved* fields, splitting
// out the reservations that belong to THIS bead for ReservedTaskTokens.
func spendFor(rows []spend.Row, w spend.Window, project, beadID string, openRes []store.Reservation) budget.Spend {
	pt := spend.ProjectTotals(rows, w, project)
	bt := spend.BeadTotals(rows, project, beadID)
	sp := budget.Spend{
		CostUSD:          pt.CostUSD,
		SyntheticCostUSD: pt.SyntheticCostUSD,
		Tokens:           pt.TotalTokens(),
		TaskTokens:       bt.TotalTokens(),
	}
	for _, r := range openRes {
		sp.ReservedCostUSD += r.CostUSD
		sp.ReservedSyntheticCostUSD += r.SyntheticCostUSD
		sp.ReservedTokens += r.Tokens
		if r.BeadID == beadID {
			sp.ReservedTaskTokens += r.Tokens
		}
	}
	return sp
}

// BudgetSnapshot is a read-only reporting helper: the ceiling and the
// currently-remaining budget for a project/bead, for the budget/remaining
// fields every /decide response (run, defer, AND deny) carries. It plays no
// part in the decision itself.
func (s *Service) BudgetSnapshot(ctx context.Context, project, beadID string) (bud budget.Budget, rem budget.Remaining, spendAsOf time.Time, err error) {
	reg, ok, err := s.store.GetRegistration(ctx, project)
	if err != nil {
		return budget.Budget{}, budget.Remaining{}, time.Time{}, err
	}
	if !ok || reg.State == store.StateInvalid {
		return budget.Budget{}, budget.Remaining{}, time.Time{}, nil
	}
	bud = budget.FromEffective(reg.Effective.Budget)

	now := s.now()
	rows, err := s.store.SpendRows(ctx, project)
	if err != nil {
		return budget.Budget{}, budget.Remaining{}, time.Time{}, err
	}
	openRes, err := s.store.OpenReservations(ctx, project, now)
	if err != nil {
		return budget.Budget{}, budget.Remaining{}, time.Time{}, err
	}
	w, err := s.store.Window(ctx)
	if err != nil {
		return budget.Budget{}, budget.Remaining{}, time.Time{}, err
	}
	sp := spendFor(rows, w, project, beadID, openRes)
	rem = budget.Remain(bud, sp)

	if s.Ready() {
		spendAsOf, err = s.store.SyncedAt(ctx)
		if err != nil {
			return budget.Budget{}, budget.Remaining{}, time.Time{}, err
		}
	}
	return bud, rem, spendAsOf, nil
}

// ---------------------------------------------------------------- outcome

// Outcome reports how one attempt ended. It is a money endpoint too: the
// outcome is what buys an escalation, so it must bind to a reservation METER
// minted (a caller-supplied attempt is exactly as forgeable as a
// caller-supplied outcome -- Decision 2).
func (s *Service) Outcome(ctx context.Context, req meterapi.OutcomeRequest) (meterapi.OutcomeResponse, error) {
	res, ok, err := s.store.GetReservation(ctx, req.ReservationID)
	if err != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: get reservation: %w", err)
	}
	if !ok || res.Project != req.Project || res.BeadID != req.BeadID || res.Attempt != req.Attempt {
		return meterapi.OutcomeResponse{}, ErrUnknownReservation
	}

	outcome := rung.Outcome(req.Outcome)
	if !outcome.Valid() {
		return meterapi.OutcomeResponse{}, ErrBadOutcome
	}

	prior, err := s.store.Attempts(ctx, req.Project, req.BeadID)
	if err != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: attempts: %w", err)
	}

	// An outcome may overwrite a previously recorded attempt for this
	// reservation ONLY if the recorded one is infra-failed (the janitor's
	// guess, or a genuine infra failure -- either way "we do not have
	// confidence this attempt completed"). Otherwise a caller holding a valid
	// reservation_id could keep re-POSTing to flip its own already-recorded
	// outcome -- bounded (one rung per reservation) but still a vote it does
	// not get. Attempt numbers are unique per bead (assigned by
	// len(prior)+1), so matching on Attempt number finds the record tied to
	// this reservation.
	reg, _, regErr := s.store.GetRegistration(ctx, req.Project)
	if regErr != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: get registration: %w", regErr)
	}
	for _, a := range prior {
		if a.Attempt == res.Attempt && a.Outcome != rung.OutcomeInfraFailed && a.Outcome != outcome {
			next, nextRung := previewNext(a.Outcome, res.Rung, reg.Effective, prior)
			return meterapi.OutcomeResponse{OK: true, RecordedAttempt: a.Attempt, Next: next, NextRung: nextRung}, nil
		}
	}

	// SETTLE, do not release. The budget hold moves to now + max_spend_staleness,
	// because this session's spend rows have NOT landed yet -- see store.Settle.
	holdUntil := s.now().Add(s.Config().Meter.MaxSpendStaleness)
	if err := s.store.Settle(ctx, res.ID, holdUntil); err != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: settle: %w", err)
	}
	if err := s.store.RecordAttempt(ctx, req.Project, req.BeadID, res.ID, rung.Attempt{
		Attempt: res.Attempt, Rung: res.Rung, Outcome: outcome,
	}); err != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: record attempt: %w", err)
	}

	updated, err := s.store.Attempts(ctx, req.Project, req.BeadID)
	if err != nil {
		return meterapi.OutcomeResponse{}, fmt.Errorf("service: outcome: attempts: %w", err)
	}
	next, nextRung := previewNext(outcome, res.Rung, reg.Effective, updated)

	return meterapi.OutcomeResponse{OK: true, RecordedAttempt: res.Attempt, Next: next, NextRung: nextRung}, nil
}

// previewNext is ADVISORY ONLY (a preview for logs and dashboards); the
// authoritative answer is always the next /decide.
func previewNext(outcome rung.Outcome, curRung string, eff gonkcfg.Effective, prior []rung.Attempt) (next, nextRung string) {
	switch outcome {
	case rung.OutcomeSuccess:
		return "done", ""
	case rung.OutcomeInfraFailed:
		return "retry", curRung
	case rung.OutcomeGateFailed:
		idx := rung.Escalations(prior)
		if idx < len(eff.Ladder) {
			return "escalate", eff.Ladder[idx]
		}
		return "escalate", ""
	default: // aborted
		return "done", ""
	}
}

// ---------------------------------------------------------------- loops

// SyncSpend runs one spend-log poll: fetch rows since (cursor - overlap),
// dedupe-add them, advance the cursor and syncedAt, check clock skew, and
// roll the budget window if it is due. It never advances SyncedAt on a
// failed poll -- SyncedAt goes stale on its own, and that staleness is what
// Decide reads.
func (s *Service) SyncSpend(ctx context.Context) error {
	cfg := s.Config()
	now := s.now()

	cursor, err := s.store.SpendCursor(ctx)
	if err != nil {
		return fmt.Errorf("service: sync spend: cursor: %w", err)
	}
	since := cursor
	if !since.IsZero() {
		since = since.Add(-spendPollOverlap)
	}

	rows, sourceClock, err := s.spend.Since(ctx, since)
	if err != nil {
		return fmt.Errorf("service: sync spend: since: %w", err)
	}
	skewOK := spend.SkewOK(now, sourceClock, cfg.Meter.MaxClockSkew)

	if _, err := s.store.AddSpendRows(ctx, rows); err != nil {
		return fmt.Errorf("service: sync spend: add rows: %w", err)
	}

	newCursor := cursor
	for _, r := range rows {
		if r.At.After(newCursor) {
			newCursor = r.At
		}
	}
	if newCursor.After(cursor) {
		if err := s.store.SetSpendCursor(ctx, newCursor); err != nil {
			return fmt.Errorf("service: sync spend: set cursor: %w", err)
		}
	}
	if err := s.store.SetSyncedAt(ctx, now); err != nil {
		return fmt.Errorf("service: sync spend: set synced at: %w", err)
	}

	s.mu.Lock()
	s.synced = true
	s.skewOK = skewOK
	s.mu.Unlock()

	prevWindow, err := s.store.Window(ctx)
	if err != nil {
		return fmt.Errorf("service: sync spend: window: %w", err)
	}
	if nextWindow, advanced := spend.Advance(prevWindow, now); advanced {
		if err := s.store.SetWindow(ctx, nextWindow); err != nil {
			return fmt.Errorf("service: sync spend: set window: %w", err)
		}
	}
	return nil
}

// Janitor expires past-TTL, UNSETTLED reservations and records an
// infra-failed attempt for each -- a session that dies silently retries its
// rung instead of escalating. A session that reported (Settled == true) is
// not dead; ExpireReservations never returns it.
func (s *Service) Janitor(ctx context.Context) error {
	expired, err := s.store.ExpireReservations(ctx, s.now())
	if err != nil {
		return fmt.Errorf("service: janitor: expire reservations: %w", err)
	}
	for _, r := range expired {
		if err := s.store.RecordAttempt(ctx, r.Project, r.BeadID, r.ID, rung.Attempt{
			Attempt: r.Attempt, Rung: r.Rung, Outcome: rung.OutcomeInfraFailed,
		}); err != nil {
			return fmt.Errorf("service: janitor: record attempt: %w", err)
		}
	}
	return nil
}

// ReconcileKeys retries virtual-key provisioning for every registration
// stuck in key-missing.
func (s *Service) ReconcileKeys(ctx context.Context) error {
	cfg := s.Config()
	regs, err := s.store.ListRegistrations(ctx)
	if err != nil {
		return fmt.Errorf("service: reconcile keys: list registrations: %w", err)
	}
	for _, reg := range regs {
		if reg.State != store.StateKeyMissing {
			continue
		}
		alias := reg.KeyAlias
		if alias == "" {
			alias = "gonk-" + reg.Rig
		}
		models := make([]string, 0, len(reg.Effective.Ladder))
		for _, r := range reg.Effective.Ladder {
			models = append(models, cfg.Catalog[r].Model)
		}
		spec := litellm.KeySpec{
			Alias:          alias,
			MaxBudgetUSD:   litellm.MaxBudgetFor(reg.Effective.Budget, reg.Effective.Ladder, cfg.Catalog),
			BudgetDuration: keyBudgetDuration,
			Models:         models,
		}
		info, err := s.admin.EnsureKey(ctx, spec)
		if err != nil {
			continue // still missing; the next tick retries
		}
		keyRef, err := s.keys.Put(ctx, reg.Project, info.Token)
		if err != nil {
			return fmt.Errorf("service: reconcile keys: keysink put %q: %w", reg.Project, err)
		}
		reg.State = store.StateActive
		reg.KeyRef = keyRef
		reg.KeyAlias = alias
		reg.UpdatedAt = s.now()
		if err := s.store.PutRegistration(ctx, reg); err != nil {
			return fmt.Errorf("service: reconcile keys: put %q: %w", reg.Project, err)
		}
	}
	return nil
}

// Reresolve re-runs gonkcfg.Resolve for every registered project against the
// CURRENT operator config. This is why Registration.Raw exists: without it,
// the operator's instance kill switch (enabled: false, ADR-002) would have no
// effect on an already-registered project until intake happened to
// re-register it. It never aborts the whole pass because one project's
// resolution failed to persist; it returns the last error, if any, after
// attempting every project.
func (s *Service) Reresolve(ctx context.Context) error {
	regs, err := s.store.ListRegistrations(ctx)
	if err != nil {
		return fmt.Errorf("service: reresolve: list registrations: %w", err)
	}
	now := s.now()
	var lastErr error
	for _, reg := range regs {
		if _, _, err := s.resolveProject(ctx, reg.Project, reg.Rig, reg.Raw, now); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
