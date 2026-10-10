package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

// gonk-2usw part EPOCH (plan 3.3): the fix epoch is part of the run identity
// an order carries, and gonk-gate checks it like every other part.

// epochArgs is r's order for the run AFTER merged fix !epoch: the same
// project, target, fingerprint and last success, the anchor with ":f<epoch>".
func (r *plRun) epochArgs(t *testing.T, epoch int64) dispatchArgs {
	t.Helper()
	a := r.args()
	anchor, err := pipelinectx.Anchor(r.pid, r.target, a.PipelineFingerprint, a.PipelineLastSuccess, epoch)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pipelinectx.SessionKey(anchor)
	if err != nil {
		t.Fatal(err)
	}
	a.BeadAnchor, a.BeadID, a.SessionKey, a.PipelineFixEpoch = anchor, anchor, key, epoch
	return a
}

func TestCheckPipelineIdentityCoversTheFixEpoch(t *testing.T) {
	r := newPLRun(t, plOpts{})
	plain, with := r.args(), r.epochArgs(t, 12)
	if plain.BeadAnchor+":f12" != with.BeadAnchor {
		t.Fatalf("premise: %s / %s", plain.BeadAnchor, with.BeadAnchor)
	}
	cases := []struct {
		name string
		a    dispatchArgs
		ok   bool
	}{
		{"no fix, no epoch", plain, true},
		{"anchor :f12, epoch 12", with, true},
		{"anchor :f12, epoch var absent", func() dispatchArgs { a := with; a.PipelineFixEpoch = 0; return a }(), false},
		{"anchor :f12, another epoch", func() dispatchArgs { a := with; a.PipelineFixEpoch = 13; return a }(), false},
		{"plain anchor, an epoch var", func() dispatchArgs { a := plain; a.PipelineFixEpoch = 12; return a }(), false},
		{"negative epoch", func() dispatchArgs { a := plain; a.PipelineFixEpoch = -1; return a }(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkPipelineIdentity(c.a)
			if (err == nil) != c.ok {
				t.Fatalf("checkPipelineIdentity = %v, want ok=%v", err, c.ok)
			}
			if c.ok {
				return
			}
			// And through dispatch: refused before meter, nothing started.
			pd, code := r.dispatch(t, plRunDecision(), nil, c.a)
			if code != 2 || pd.fm.calls != 0 || len(pd.rt.Starts()) != 0 {
				t.Fatalf("exit=%d meter calls=%d starts=%d, want a refusal before meter", code, pd.fm.calls, len(pd.rt.Starts()))
			}
		})
	}
}

// The order var is read by its name, and an order without it means "no fix".
func TestDispatchArgsReadTheFixEpoch(t *testing.T) {
	vars := map[string]string{"pipeline_fix_epoch": "12"}
	if a := dispatchArgsFrom(func(k string) string { return vars[k] }); a.PipelineFixEpoch != 12 {
		t.Fatalf("PipelineFixEpoch = %d, want 12", a.PipelineFixEpoch)
	}
	if a := dispatchArgsFrom(func(string) string { return "" }); a.PipelineFixEpoch != 0 {
		t.Fatalf("PipelineFixEpoch = %d with no var, want 0", a.PipelineFixEpoch)
	}
}

// A run after a merged fix dispatches under ITS anchor: the record carries the
// epoch, the session is the pipeline's as always, and a re-fire of the parked
// record rebuilds the same identity. The fix-less run's re-fire carries no
// epoch var at all.
func TestPipelineFixEpochIsRecordedAndRoundTripsThroughRefire(t *testing.T) {
	for _, epoch := range []int64{0, 12} {
		r := newPLRun(t, plOpts{})
		args := r.epochArgs(t, epoch)
		store := beadstore.NewMemory()
		if _, code := r.dispatch(t, meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours}, store, args); code != 0 {
			t.Fatalf("epoch %d: defer exit = %d", epoch, code)
		}
		parked, ok, err := store.Get(context.Background(), args.BeadAnchor)
		if err != nil || !ok || parked.PipelineFixEpoch != epoch || parked.BeadAnchor != args.BeadAnchor {
			t.Fatalf("epoch %d: parked record %+v found=%v err=%v", epoch, parked, ok, err)
		}
		orders := &fakeOrders{}
		if !refire(context.Background(), sweepDeps{Orders: orders, Log: testLogger(io.Discard)}, parked) {
			t.Fatal("refire refused")
		}
		vars := orders.orders()[0].Vars
		if v, has := vars["pipeline_fix_epoch"]; (epoch == 0 && has) || (epoch != 0 && v != "12") {
			t.Fatalf("epoch %d: refired pipeline_fix_epoch = %q (present=%v)", epoch, v, has)
		}
		back := dispatchArgsFrom(func(k string) string { return vars[k] })
		if err := checkPipelineIdentity(back); err != nil {
			t.Fatalf("epoch %d: refired vars do not rebuild the anchor: %v (%v)", epoch, err, vars)
		}
		pd, code := r.dispatch(t, plRunDecision(), store, back)
		if code != 0 {
			t.Fatalf("epoch %d: exit = %d\n%s", epoch, code, pd.logs)
		}
		// The session name is keyed on the pipeline, not on the anchor: it is
		// what it always was, with or without a fix.
		want := runtime.Name("pipeline", r.pid, runtime.KindPipeline, 900, 1)
		if starts := pd.rt.Starts(); len(starts) != 1 || starts[0].Name != want {
			t.Fatalf("epoch %d: starts = %+v, want %s", epoch, starts, want)
		}
		rec, _, _ := store.Get(context.Background(), args.BeadAnchor)
		if rec.PipelineFixEpoch != epoch || rec.State != beadstore.StateRunning || rec.PipelineLastSuccess != 880 {
			t.Fatalf("epoch %d: running record = %+v", epoch, rec)
		}
	}
}

// The var must be declared where gc reads the order's params, under the name
// intake and the sweep send and dispatch reads -- and optional, because a run
// that follows no merged fix does not send it.
func TestGonkDispatchDeclaresTheFixEpochParam(t *testing.T) {
	var doc struct {
		Order struct {
			Params map[string]struct {
				Required bool `toml:"required"`
			} `toml:"params"`
		} `toml:"order"`
	}
	path := filepath.Join(repoPackDir, "orders", "gonk-dispatch.toml")
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	p, ok := doc.Order.Params["pipeline_fix_epoch"]
	if !ok {
		t.Fatalf("%s does not declare pipeline_fix_epoch", path)
	}
	if p.Required {
		t.Fatalf("%s declares pipeline_fix_epoch required; a run that follows no fix does not send it", path)
	}
}
