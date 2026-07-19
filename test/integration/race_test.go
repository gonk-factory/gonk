package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/test/ledger"
)

// decideRaw posts one /decide over real HTTP and returns the response WITHOUT
// touching *testing.T -- so it is safe to call from the racing goroutines below
// (t.Fatalf from a non-test goroutine is illegal). It is the same wire the
// SyntheticSession drives, minus the fatal-on-error behaviour.
func (w *World) decideRaw(project, bead, sess, trigger string) (meterapi.DecideResponse, error) {
	body, err := json.Marshal(meterapi.DecideRequest{
		Project: project, Rig: rigFor(project), BeadID: bead, SessionKey: sess, Trigger: trigger,
	})
	if err != nil {
		return meterapi.DecideResponse{}, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		w.meterURL+meterapi.DecidePath, bytes.NewReader(body))
	if err != nil {
		return meterapi.DecideResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return meterapi.DecideResponse{}, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return meterapi.DecideResponse{}, fmt.Errorf("decide: status %d: %s", resp.StatusCode, raw)
	}
	var out meterapi.DecideResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return meterapi.DecideResponse{}, err
	}
	return out, nil
}

// TestConcurrentSessionsCannotOverspendACeiling is the crown jewel: TWO SESSIONS
// RACING THE LAST OF A BUDGET, EXACTLY ONE MAY WIN -- or here, headroom for
// exactly two glm attempts and thirty-two racers, of which exactly two win. It
// proves the SERVICE has no check-then-act hole ABOVE the store, end to end
// through the real /decide, which is the only thing a user ever touches.
func TestConcurrentSessionsCannotOverspendACeiling(t *testing.T) {
	w := NewWorld(t)
	// Ceiling with headroom for EXACTLY TWO glm attempts (est_cost_usd 0.40 each).
	onboard(t, w, "acme/widget", withLadder("glm"), withBudget(1.00))

	const N = 32
	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := w.decideRaw("acme/widget", fmt.Sprintf("gk-%d", i), fmt.Sprintf("s-%d", i), atags.TriggerIssueTriage)
			if err != nil {
				t.Errorf("goroutine %d: decide: %v", i, err)
				return
			}
			switch d.Decision {
			case "run":
				wins.Add(1)
			case "defer":
				if d.Reason != meterapi.ReasonMonthlyCostExhausted {
					t.Errorf("loser deferred for the wrong reason: %q", d.Reason)
				}
			default:
				t.Errorf("unexpected decision %+v", d)
			}
		}(i)
	}
	wg.Wait()

	if got := wins.Load(); got != 2 {
		t.Fatalf("%d of %d sessions won against headroom for 2 -- A BUDGET CEILING IS RACEABLE", got, N)
	}
	// The invariant, not the winner: the PERSISTED reservations never exceed the ceiling.
	if total := w.openReservationCostUSD(t, "acme/widget"); total > 1.00+ledger.USDEpsilon {
		t.Fatalf("persisted reservations total $%.4f against a $1.00 ceiling: OVERSPEND", total)
	}
	// And the losers spent NOTHING. A defer that still burns tokens is not a defer.
	w.SyncSpend(t)
	if calls := len(w.Model.Log().Calls()); calls != 0 {
		t.Fatalf("%d model calls from sessions that never ran", calls)
	}
}
