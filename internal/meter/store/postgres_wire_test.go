package store

import (
	"math"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/rung"
)

// TestMarshalEffectiveIsInfSafe is the pure-logic proof behind the
// Postgres store's persistence convention: json.Marshal(gonkcfg.Effective{})
// directly panics-the-error on the unlimited sentinels, but marshalEffective
// (which routes Budget through pkg/budget's null-for-unlimited types) must
// not. This runs with no Postgres and is part of the normal gate.
func TestMarshalEffectiveIsInfSafe(t *testing.T) {
	e := gonkcfg.Effective{
		Enabled: true,
		Ladder:  []string{"qwen-local"},
		Budget: gonkcfg.EffectiveBudget{
			MonthlyCostUSD: math.Inf(1),
			MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
			PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
		},
	}
	raw, err := marshalEffective(e)
	if err != nil {
		t.Fatalf("marshalEffective(unlimited) = %v, want no error", err)
	}
	got, err := unmarshalEffective(raw)
	if err != nil {
		t.Fatalf("unmarshalEffective: %v", err)
	}
	if !math.IsInf(got.Budget.MonthlyCostUSD, 1) {
		t.Fatalf("MonthlyCostUSD = %v, want +Inf", got.Budget.MonthlyCostUSD)
	}
	if got.Budget.MonthlyTokens != gonkcfg.TokenQuantity(math.MaxInt64) {
		t.Fatalf("MonthlyTokens = %v, want MaxInt64", got.Budget.MonthlyTokens)
	}
	if got.Budget.PerTaskTokens != gonkcfg.TokenQuantity(math.MaxInt64) {
		t.Fatalf("PerTaskTokens = %v, want MaxInt64", got.Budget.PerTaskTokens)
	}
	if !got.Enabled || len(got.Ladder) != 1 || got.Ladder[0] != "qwen-local" {
		t.Fatalf("round trip lost non-budget fields: %+v", got)
	}
}

// A finite budget must round-trip exactly, not just the unlimited sentinels.
func TestMarshalEffectiveFiniteBudgetRoundTrips(t *testing.T) {
	e := gonkcfg.Effective{
		Enabled:        true,
		DisabledReason: "",
		Actions:        gonkcfg.Actions{Triage: true},
		Ladder:         []string{"qwen-local", "glm"},
		Continuity:     "resume",
		Triage:         gonkcfg.EffectiveTriage{LabelPrefix: "gonk::", RespondToMentions: true},
		Provenance:     gonkcfg.EffectiveProvenance{CommitTrailers: true},
		Budget: gonkcfg.EffectiveBudget{
			MonthlyCostUSD: 12.5,
			MonthlyTokens:  gonkcfg.TokenQuantity(1_000_000),
			PerTaskTokens:  gonkcfg.TokenQuantity(50_000),
		},
	}
	raw, err := marshalEffective(e)
	if err != nil {
		t.Fatalf("marshalEffective: %v", err)
	}
	got, err := unmarshalEffective(raw)
	if err != nil {
		t.Fatalf("unmarshalEffective: %v", err)
	}
	if got.Budget.MonthlyCostUSD != 12.5 || got.Budget.MonthlyTokens != 1_000_000 || got.Budget.PerTaskTokens != 50_000 {
		t.Fatalf("finite budget did not round-trip: %+v", got.Budget)
	}
	if got.Continuity != "resume" || !got.Actions.Triage || !got.Triage.RespondToMentions {
		t.Fatalf("non-budget fields did not round-trip: %+v", got)
	}
}

// TestMarshalEffectiveScheduleRoundTrips is R-23: Effective.Schedule (the
// resolved schedule.quiet_hours/timezone STRINGS, as distinct from
// Registration.QuietHours -- the already-parsed rung.QuietHours the policy
// engine reads) must survive marshalEffective/unmarshalEffective. An earlier
// wireEffective had no Schedule field at all, so it silently dropped it: a
// live meter enforced quiet hours correctly (from the separately-persisted
// parsed form) while GET /v1/projects/{p} reported `schedule: null` after
// every restart -- a real config, invisibly hidden from every operator who
// asked to see it.
func TestMarshalEffectiveScheduleRoundTrips(t *testing.T) {
	sc := gonkcfg.Schedule{QuietHours: "22:00-07:00", Timezone: "America/New_York"}
	e := gonkcfg.Effective{Enabled: true, Ladder: []string{"qwen-local"}, Schedule: &sc}
	raw, err := marshalEffective(e)
	if err != nil {
		t.Fatalf("marshalEffective: %v", err)
	}
	got, err := unmarshalEffective(raw)
	if err != nil {
		t.Fatalf("unmarshalEffective: %v", err)
	}
	if got.Schedule == nil {
		t.Fatal("Schedule did not round-trip: got nil, want a schedule")
	}
	if *got.Schedule != sc {
		t.Fatalf("Schedule did not round-trip: got %+v, want %+v", *got.Schedule, sc)
	}
}

// A nil Schedule (no layer ever set schedule.quiet_hours/timezone) must stay
// nil, not turn into a zero-value Schedule{} -- which would read back as
// "quiet_hours: \"\", timezone: \"\"" rather than "no schedule configured".
func TestMarshalEffectiveNilScheduleRoundTrips(t *testing.T) {
	e := gonkcfg.Effective{Enabled: true, Ladder: []string{"qwen-local"}}
	raw, err := marshalEffective(e)
	if err != nil {
		t.Fatalf("marshalEffective: %v", err)
	}
	got, err := unmarshalEffective(raw)
	if err != nil {
		t.Fatalf("unmarshalEffective: %v", err)
	}
	if got.Schedule != nil {
		t.Fatalf("Schedule = %+v, want nil", got.Schedule)
	}
}

// marshalEffective must fail closed on a NaN/-Inf cost ceiling (a corrupted
// or unvalidated operator Policy -- see ADR-002 "Known gap") rather than
// silently persisting garbage that would corrupt every budget check against
// this project.
func TestMarshalEffectiveRejectsNonFiniteBudget(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(-1)} {
		e := gonkcfg.Effective{Budget: gonkcfg.EffectiveBudget{MonthlyCostUSD: bad}}
		if _, err := marshalEffective(e); err == nil {
			t.Fatalf("marshalEffective(MonthlyCostUSD=%v) = nil error, want a rejection", bad)
		}
	}
}

func TestQuietHoursRoundTrips(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	q := &rung.QuietHours{Start: 22 * time.Hour, End: 7 * time.Hour, Loc: loc}
	raw, err := marshalQuietHours(q)
	if err != nil {
		t.Fatalf("marshalQuietHours: %v", err)
	}
	got, err := unmarshalQuietHours(raw)
	if err != nil {
		t.Fatalf("unmarshalQuietHours: %v", err)
	}
	if got.Start != q.Start || got.End != q.End || got.Loc.String() != loc.String() {
		t.Fatalf("quiet hours did not round-trip: %+v", got)
	}
}

func TestQuietHoursNilRoundTrips(t *testing.T) {
	raw, err := marshalQuietHours(nil)
	if err != nil || raw != nil {
		t.Fatalf("marshalQuietHours(nil) = %v %v, want nil, nil", raw, err)
	}
	got, err := unmarshalQuietHours(nil)
	if err != nil || got != nil {
		t.Fatalf("unmarshalQuietHours(nil) = %v %v, want nil, nil", got, err)
	}
}
