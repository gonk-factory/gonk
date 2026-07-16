package meterapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// Unlimited must come out as null, both ways. This is PLAN.md's highest-value
// carry-forward: json.Marshal(math.Inf(1)) RETURNS AN ERROR, so an unlimited
// project would fail to serialize at all.
func TestUnlimitedBudgetSerializesAsNull(t *testing.T) {
	// Proof of the hazard first: assert json.Marshal of the raw +Inf float ERRORS.
	if _, err := json.Marshal(math.Inf(1)); err == nil {
		t.Fatal("json.Marshal(math.Inf(1)) did not error; the hazard Budget guards against no longer exists")
	}

	unlimited := gonkcfg.EffectiveBudget{
		MonthlyCostUSD: math.Inf(1),
		MonthlyTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
		PerTaskTokens:  gonkcfg.TokenQuantity(math.MaxInt64),
	}
	b, err := BudgetFrom(unlimited)
	if err != nil {
		t.Fatalf("BudgetFrom(unlimited): %v", err)
	}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal(Budget): %v", err)
	}
	want := `{"monthly_cost_usd":null,"monthly_tokens":null,"per_task_tokens":null}`
	if string(got) != want {
		t.Errorf("BudgetFrom(unlimited) marshaled to %s, want %s", got, want)
	}

	// And the inverse: null decodes back to the unlimited sentinels.
	var back Budget
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.Effective() != unlimited {
		t.Errorf("round trip: got %+v, want %+v", back.Effective(), unlimited)
	}
}

// ZeroBudget() marshals to `{"monthly_cost_usd":0,...}` -- NOT to nulls.
// An empty Budget{} means UNLIMITED, which for an invalid project is the exact
// opposite of fail-closed. This test is the guardrail.
func TestZeroBudgetIsNotAnEmptyBudget(t *testing.T) {
	zeroGot, err := json.Marshal(ZeroBudget())
	if err != nil {
		t.Fatalf("json.Marshal(ZeroBudget()): %v", err)
	}
	zeroWant := `{"monthly_cost_usd":0,"monthly_tokens":0,"per_task_tokens":0}`
	if string(zeroGot) != zeroWant {
		t.Errorf("ZeroBudget() marshaled to %s, want %s", zeroGot, zeroWant)
	}

	emptyGot, err := json.Marshal(Budget{})
	if err != nil {
		t.Fatalf("json.Marshal(Budget{}): %v", err)
	}
	emptyWant := `{"monthly_cost_usd":null,"monthly_tokens":null,"per_task_tokens":null}`
	if string(emptyGot) != emptyWant {
		t.Errorf("Budget{} marshaled to %s, want %s (an empty Budget must mean UNLIMITED)", emptyGot, emptyWant)
	}

	if string(zeroGot) == string(emptyGot) {
		t.Fatal("ZeroBudget() must not marshal the same as an empty Budget{} -- fail-closed vs. unlimited must be distinguishable on the wire")
	}
}

func TestBudgetFromRejectsNonFinite(t *testing.T) {
	for _, tc := range []struct {
		name string
		cost float64
	}{
		{"NaN", math.NaN()},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BudgetFrom(gonkcfg.EffectiveBudget{
				MonthlyCostUSD: tc.cost,
				MonthlyTokens:  1000,
				PerTaskTokens:  1000,
			})
			if err == nil {
				t.Fatalf("BudgetFrom(MonthlyCostUSD=%v): want error, got nil", tc.cost)
			}
		})
	}
}

func TestFiniteBudgetRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		eb   gonkcfg.EffectiveBudget
	}{
		{"typical", gonkcfg.EffectiveBudget{MonthlyCostUSD: 12.5, MonthlyTokens: 500000, PerTaskTokens: 10000}},
		{"explicit zero ceiling", gonkcfg.EffectiveBudget{MonthlyCostUSD: 0, MonthlyTokens: 0, PerTaskTokens: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := BudgetFrom(tc.eb)
			if err != nil {
				t.Fatalf("BudgetFrom: %v", err)
			}
			if b.MonthlyCostUSD == nil || b.MonthlyTokens == nil || b.PerTaskTokens == nil {
				t.Fatalf("a finite budget (including an explicit 0 ceiling) must not produce nil fields: %+v", b)
			}
			if got := b.Effective(); got != tc.eb {
				t.Errorf("round trip: got %+v, want %+v", got, tc.eb)
			}
		})
	}
}

// Marshal a DecideRequest and assert the JSON contains NO "attempt" key, and
// reflect over the struct asserting no field named Attempt. A caller-supplied
// attempt count is a forgery vector for climbing the ladder (Decision 2), and
// this test is what stops someone "helpfully" adding one.
func TestDecideRequestHasNoAttemptField(t *testing.T) {
	rt := reflect.TypeOf(DecideRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if strings.EqualFold(f.Name, "attempt") {
			t.Fatalf("DecideRequest must not have a field named Attempt (found %s) -- meter owns ladder state, not the caller", f.Name)
		}
		if tag := f.Tag.Get("json"); tag == "attempt" || strings.HasPrefix(tag, "attempt,") {
			t.Fatalf("DecideRequest field %s must not carry json tag %q", f.Name, tag)
		}
	}

	req := DecideRequest{
		Project:    "group/repo",
		Rig:        "gitlab",
		BeadID:     "gonk:42:issue:7",
		SessionKey: "sess-1",
		Trigger:    "issue-triage",
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("json.Marshal(DecideRequest): %v", err)
	}
	if bytes.Contains(got, []byte(`"attempt"`)) {
		t.Fatalf("DecideRequest JSON must not contain an \"attempt\" key: %s", got)
	}
}

// ProjectPath("group/repo") == "/v1/projects/group%2Frepo". An unescaped slash
// routes to a different handler, or to none.
func TestProjectPathEscapes(t *testing.T) {
	got := ProjectPath("group/repo")
	want := "/v1/projects/group%2Frepo"
	if got != want {
		t.Errorf("ProjectPath(%q) = %q, want %q", "group/repo", got, want)
	}

	if got := ProjectKeyRotatePath("group/repo"); got != "/v1/projects/group%2Frepo/key/rotate" {
		t.Errorf("ProjectKeyRotatePath(%q) = %q", "group/repo", got)
	}
	// ':' is a legal path-segment character per RFC 3986 and url.PathEscape
	// deliberately leaves it alone; only '/' (the segment delimiter) matters here.
	if got := CostBeadPath("gonk:1:issue:2"); got != "/v1/cost/bead/gonk:1:issue:2" {
		t.Errorf("CostBeadPath = %q", got)
	}
	if got := CostSessionPath("sess/1"); got != "/v1/cost/session/sess%2F1" {
		t.Errorf("CostSessionPath = %q", got)
	}
	if got := CostProjectPath("group/repo"); got != "/v1/cost/project/group%2Frepo" {
		t.Errorf("CostProjectPath = %q", got)
	}
}

// wireContractFixtures builds one fully-populated value of every
// request/response type in the contract, keyed by golden-file basename (no
// extension). Every JSON field name in the package should appear in at least
// one fixture.
func wireContractFixtures() map[string]any {
	t1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)

	budget := Budget{MonthlyCostUSD: f64p(100), MonthlyTokens: i64p(5_000_000), PerTaskTokens: i64p(50_000)}
	remaining := Budget{MonthlyCostUSD: f64p(80.5), MonthlyTokens: i64p(4_500_000), PerTaskTokens: i64p(49_000)}

	rungCost := RungCost{
		Rung:             "qwen-local",
		Kind:             "local",
		CostUSD:          0,
		SyntheticCostUSD: 0.5,
		CostSynthetic:    true,
		TotalTokens:      1500,
		Calls:            1,
	}
	triggerCost := TriggerCost{
		Trigger:          "issue-triage",
		CostUSD:          10,
		SyntheticCostUSD: 2,
		TotalTokens:      15000,
		Calls:            5,
	}

	effective := &Effective{
		Enabled:    true,
		Actions:    Actions{Triage: true, Pipelines: true, Features: false},
		Ladder:     []string{"qwen-local", "gpt-4o"},
		Continuity: "resume",
		Triage:     Triage{LabelPrefix: "gonk::", RespondToMentions: true},
		Provenance: Provenance{CommitTrailers: true, IncludeUsage: false},
		Schedule:   &Schedule{QuietHours: "22:00-06:00", Timezone: "UTC"},
	}

	projectCost := ProjectCostResponse{
		Project:          "group/repo",
		Window:           Window{Start: t1, End: t2},
		CostUSD:          10,
		SyntheticCostUSD: 2,
		PromptTokens:     10000,
		CompletionTokens: 5000,
		TotalTokens:      15000,
		ByRung:           []RungCost{rungCost},
		ByTrigger:        []TriggerCost{triggerCost},
		Budget:           budget,
		Remaining:        remaining,
		AsOf:             t1,
		Complete:         true,
		Stale:            false,
	}

	return map[string]any{
		"project_request": ProjectRequest{
			Project:         "group/repo",
			ProjectID:       42,
			Rig:             "gitlab",
			DefaultBranch:   "main",
			ConfigCommitSHA: "abc123",
			GonkYML:         "version: 1\n",
		},
		"project_response": ProjectResponse{
			Project:        "group/repo",
			Rig:            "gitlab",
			State:          StateActive,
			DisabledReason: "",
			Effective:      effective,
			Budget:         budget,
			KeyRef:         KeyRef{SecretName: "gonk-group-repo", SecretKey: "litellm-key"},
			ConfigHash:     "sha256:deadbeef",
			UpdatedAt:      t1,
		},
		"decide_request": DecideRequest{
			Project:    "group/repo",
			Rig:        "gitlab",
			BeadID:     "gonk:42:issue:7",
			SessionKey: "sess-1",
			Trigger:    "issue-triage",
		},
		"decide_response": DecideResponse{
			Decision:             DecisionRun,
			Rung:                 "qwen-local",
			Model:                "qwen2.5-coder-32b",
			Attempt:              1,
			Reason:               ReasonNotRegistered,
			Detail:               "ok",
			RetryAfter:           t1,
			Metadata:             map[string]string{"gonk_project": "group/repo", "gonk_trigger": "issue-triage"},
			KeyRef:               KeyRef{SecretName: "gonk-group-repo", SecretKey: "litellm-key"},
			ReservationID:        "resv-1",
			ReservationExpiresAt: t2,
			Budget:               budget,
			Remaining:            remaining,
			SpendAsOf:            t1,
		},
		"outcome_request": OutcomeRequest{
			Project:       "group/repo",
			BeadID:        "gonk:42:issue:7",
			SessionKey:    "sess-1",
			Attempt:       1,
			Rung:          "qwen-local",
			ReservationID: "resv-1",
			Outcome:       OutcomeSuccess,
		},
		"outcome_response": OutcomeResponse{
			OK:              true,
			RecordedAttempt: 1,
			Next:            "escalate",
			NextRung:        "gpt-4o",
		},
		"bead_cost_response": BeadCostResponse{
			BeadID:           "gonk:42:issue:7",
			Project:          "group/repo",
			CostUSD:          1.23,
			SyntheticCostUSD: 0.5,
			PromptTokens:     1000,
			CompletionTokens: 500,
			TotalTokens:      1500,
			ByRung:           []RungCost{rungCost},
			Attempts:         []AttemptView{{Attempt: 1, Rung: "qwen-local", Outcome: "success"}},
			AsOf:             t1,
			Complete:         true,
		},
		"session_cost_response": SessionCostResponse{
			SessionKey:       "sess-1",
			Project:          "group/repo",
			BeadID:           "gonk:42:issue:7",
			CostUSD:          1.23,
			SyntheticCostUSD: 0.5,
			PromptTokens:     1000,
			CompletionTokens: 500,
			TotalTokens:      1500,
			ByRung:           []RungCost{rungCost},
			AsOf:             t1,
			Complete:         true,
		},
		"project_cost_response": projectCost,
		"instance_cost_response": InstanceCostResponse{
			Window:           Window{Start: t1, End: t2},
			CostUSD:          100,
			SyntheticCostUSD: 20,
			TotalTokens:      150000,
			ByProject:        []ProjectCostResponse{projectCost},
			AsOf:             t1,
			Complete:         true,
		},
		"error_response": ErrorResponse{Error: "boom"},
		"spend_sync_response": SpendSyncResponse{
			SpendAsOf:    t1,
			RowsIngested: intp(14),
			Unattributed: intp(0),
			Synced:       true,
			Error:        "",
		},
	}
}

func intp(v int) *int { return &v }

func f64p(v float64) *float64 { return &v }
func i64p(v int64) *int64     { return &v }

// Marshal one fully-populated value of EVERY request/response type and compare
// BYTE FOR BYTE against testdata/golden/*.json. A field rename fails CI.
//
// Run with UPDATE_GOLDEN=1 to (re)write the golden files after a deliberate,
// reviewed contract change -- update testdata/contract.sha256 in the same commit.
func TestWireContractLiterals(t *testing.T) {
	update := os.Getenv("UPDATE_GOLDEN") == "1"
	for name, v := range wireContractFixtures() {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatalf("json.MarshalIndent(%s): %v", name, err)
			}
			got = append(got, '\n')

			path := filepath.Join("testdata", "golden", name+".json")
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
				t.Errorf("%s literal JSON mismatch (a field was renamed, retyped, added, or removed):\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

// TestContractIsFrozen fails on ANY edit to meterapi.go. That is the point: this
// file is a contract between three plans, and a change to it is a change to all
// three. Update the checksum in the SAME commit that updates the clients, and
// say so in the message.
func TestContractIsFrozen(t *testing.T) {
	src, err := os.ReadFile("meterapi.go")
	if err != nil {
		t.Fatalf("read meterapi.go: %v", err)
	}
	sum := sha256.Sum256(src)
	got := hex.EncodeToString(sum[:])

	wantRaw, err := os.ReadFile(filepath.Join("testdata", "contract.sha256"))
	if err != nil {
		t.Fatalf("read testdata/contract.sha256: %v", err)
	}
	want := strings.TrimSpace(string(wantRaw))

	if got != want {
		t.Errorf("meterapi.go sha256 = %s, want %s (testdata/contract.sha256 is stale -- if this edit is a deliberate, reviewed contract change, regenerate it with: sha256sum pkg/meterapi/meterapi.go | cut -d' ' -f1 > pkg/meterapi/testdata/contract.sha256)", got, want)
	}
}
