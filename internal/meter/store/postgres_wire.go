package store

import (
	"encoding/json"
	"fmt"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/budget"
	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
)

// This file is the +Inf-safe persistence convention for Registration.Effective
// (see the package doc and Task 6's plan text: "pick ONE convention... apply
// the same null-for-unlimited convention meterapi.Budget uses"). It is pure
// (no I/O), so it is unit-tested without a live Postgres in postgres_wire_test.go
// and exercised end to end by storetest's UNLIMITED registration fixture
// against a real server in postgres_test.go.
//
// gonkcfg.Effective.Budget carries math.Inf(1) (cost) and math.MaxInt64
// (tokens) for an unlimited project (ADR-002). Neither survives a naive
// encoding/json.Marshal: +Inf makes Marshal return an error rather than a
// number, and a Postgres double precision column either rejects Infinity or
// silently stores NaN depending on configuration. pkg/budget's CostLimit and
// TokenLimit already encode exactly this "null means unlimited" convention
// (used on the meterapi wire contract) -- wireEffective embeds a budget.Budget
// instead of a raw gonkcfg.EffectiveBudget so that json.Marshal on the wire
// struct is always safe.

// wireEffective is the JSON shape stored in the registrations.effective
// column. It has no relation to any other wire contract (meterapi's); it
// exists purely so the Postgres store never marshals a raw
// gonkcfg.EffectiveBudget with its Inf/MaxInt64 sentinels.
type wireEffective struct {
	Enabled        bool                        `json:"enabled"`
	DisabledReason string                      `json:"disabled_reason"`
	Actions        gonkcfg.Actions             `json:"actions"`
	Ladder         []string                    `json:"ladder"`
	Continuity     string                      `json:"continuity"`
	Triage         gonkcfg.EffectiveTriage     `json:"triage"`
	Provenance     gonkcfg.EffectiveProvenance `json:"provenance"`
	Budget         budget.Budget               `json:"budget"`
}

func toWireEffective(e gonkcfg.Effective) wireEffective {
	return wireEffective{
		Enabled:        e.Enabled,
		DisabledReason: e.DisabledReason,
		Actions:        e.Actions,
		Ladder:         append([]string(nil), e.Ladder...),
		Continuity:     e.Continuity,
		Triage:         e.Triage,
		Provenance:     e.Provenance,
		Budget:         budget.FromEffective(e.Budget),
	}
}

func (w wireEffective) toEffective() gonkcfg.Effective {
	return gonkcfg.Effective{
		Enabled:        w.Enabled,
		DisabledReason: w.DisabledReason,
		Actions:        w.Actions,
		Ladder:         append([]string(nil), w.Ladder...),
		Continuity:     w.Continuity,
		Triage:         w.Triage,
		Provenance:     w.Provenance,
		Budget: gonkcfg.EffectiveBudget{
			MonthlyCostUSD: float64(w.Budget.MonthlyCostUSD),
			MonthlyTokens:  gonkcfg.TokenQuantity(w.Budget.MonthlyTokens),
			PerTaskTokens:  gonkcfg.TokenQuantity(w.Budget.PerTaskTokens),
		},
	}
}

// marshalEffective encodes e via wireEffective, so a +Inf/MaxInt64 "unlimited"
// project marshals as JSON null on the budget fields rather than failing.
func marshalEffective(e gonkcfg.Effective) ([]byte, error) {
	b, err := json.Marshal(toWireEffective(e))
	if err != nil {
		return nil, fmt.Errorf("store: marshal effective: %w", err)
	}
	return b, nil
}

func unmarshalEffective(raw []byte) (gonkcfg.Effective, error) {
	var w wireEffective
	if err := json.Unmarshal(raw, &w); err != nil {
		return gonkcfg.Effective{}, fmt.Errorf("store: unmarshal effective: %w", err)
	}
	return w.toEffective(), nil
}

// wireQuietHours is the JSON shape stored in the registrations.quiet_hours
// column. rung.QuietHours carries a *time.Location, which encoding/json
// cannot serialize (it has no exported fields) -- the IANA zone name is the
// only thing that round-trips through time.LoadLocation.
type wireQuietHours struct {
	StartNanos int64  `json:"start_ns"`
	EndNanos   int64  `json:"end_ns"`
	TZ         string `json:"tz"`
}

func marshalQuietHours(q *rung.QuietHours) ([]byte, error) {
	if q == nil {
		return nil, nil
	}
	loc := q.Loc
	if loc == nil {
		return nil, fmt.Errorf("store: quiet hours with a nil timezone")
	}
	b, err := json.Marshal(wireQuietHours{
		StartNanos: int64(q.Start),
		EndNanos:   int64(q.End),
		TZ:         loc.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("store: marshal quiet hours: %w", err)
	}
	return b, nil
}

func unmarshalQuietHours(raw []byte) (*rung.QuietHours, error) {
	if raw == nil {
		return nil, nil
	}
	var w wireQuietHours
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("store: unmarshal quiet hours: %w", err)
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil {
		return nil, fmt.Errorf("store: quiet hours timezone %q: %w", w.TZ, err)
	}
	return &rung.QuietHours{
		Start: time.Duration(w.StartNanos),
		End:   time.Duration(w.EndNanos),
		Loc:   loc,
	}, nil
}
