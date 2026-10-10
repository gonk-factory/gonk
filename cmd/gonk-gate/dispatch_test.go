package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// fakeMeter serves /v1/policy/decide with a canned answer and records what it was
// asked. It is deliberately dumb: the POINT of these tests is what gonk-gate DOES
// with an answer, not how meter computes one (that is Plan 03's exhaustive table).
//
// (It used to serve the prompt-by-reference routes too, gonk-mzd. Dispatch no
// longer calls them: under the SessionRuntime the prompt is mounted, gonk-t55.)
type fakeMeter struct {
	resp     meterapi.DecideResponse
	mu       sync.Mutex
	lastBody []byte
	calls    int

	// onDecide, if set, runs at the start of every /decide, before the answer
	// is written and without the lock held: a test can hold a dispatch INSIDE
	// meter's decision (gonk-b2jk).
	onDecide func()
}

func (f *fakeMeter) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := jsonReadAll(r)
		f.mu.Lock()
		onDecide := f.onDecide
		f.mu.Unlock()
		if onDecide != nil {
			onDecide()
		}
		f.mu.Lock()
		f.calls++
		f.lastBody = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func baseDispatchArgs() dispatchArgs {
	return dispatchArgs{
		Project: "group/repo", ProjectID: 42, Rig: "group-repo", IssueIID: 3,
		BeadAnchor: "gonk:42:issue:3", BeadID: "gk-1a2b",
		SessionKey: "gonk-42-issue-3", Trigger: "issue-triage",
	}
}

// ADR-007 §3 deleted the formula layer entirely: formulas, [steps.check], the
// `check` subcommand, and the vars map runDispatch used to build for a
// formula pour (which carried litellm_key/bot_token as plain map entries --
// see TestDispatchSourceNeverBuildsLegacyFormulaCredentialVars below). mention
// -reply was the one trigger still on that path (it "still pours its formula"
// used to be the comment here); it is not on agentForTrigger either, so it is
// now a plain unrouted trigger like any other -- refused loudly, exactly like
// TestDispatchRefusesUnknownTrigger, rather than left to idle against a
// pour that provably could not deliver its prompt (gonk-6gs / #4668). T-24
// ports it onto the broker for real.
func TestDispatchRefusesMentionReplyUntilItIsPortedToTheBroker(t *testing.T) {
	rt := newTestRuntime()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model-from-the-catalog",
		Attempt: 1, ReservationID: "rsv-1",
		KeyRef: meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	args := baseDispatchArgs()
	args.Trigger = "mention-reply"
	code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(),
		Args: args,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (misconfiguration -- an unrouted trigger)", code)
	}
	if fm.calls != 0 {
		t.Fatalf("meter /decide was called %d times, want 0 -- an unrouted trigger must not even ask", fm.calls)
	}
	if starts := rt.Starts(); len(starts) != 0 {
		t.Fatalf("started = %+v, want none -- mention is not on agentForTrigger until T-24", starts)
	}
}

// Exit criterion: dispatch.go must have no code path that sends litellm_key
// or bot_token. Those were plain entries in the formula-pour's vars map (a
// credential in an order var is a credential in the event bus and in every
// log line that echoes an order) -- a v1-only compromise ADR-007 §3 deletes
// along with the rest of the formula layer. The broker path this repo runs
// exclusively now delivers the project's key over the one-shot prompt row
// (meterapi.PromptRequest.LiteLLMKey, set in runBrokerDispatch) instead, and
// carries no bot token at all -- the pod holds no forge creds to use one
// with.
//
// There is no live vars map left in dispatch.go to assert against (that
// absence IS the fix), so this reads dispatch.go's own source. Proven to
// fail: temporarily re-adding either literal to dispatch.go turns this test
// red (checked by hand while writing it, then reverted).
func TestDispatchSourceNeverBuildsLegacyFormulaCredentialVars(t *testing.T) {
	src, err := os.ReadFile("dispatch.go")
	if err != nil {
		t.Fatalf("reading dispatch.go: %v", err)
	}
	for _, forbidden := range []string{`"litellm_key"`, `"bot_token"`} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("dispatch.go contains %s -- the formula-pour vars map (ADR-007 §3) must not "+
				"come back; the broker path delivers the key over the prompt row instead", forbidden)
		}
	}
}

// On a `run`, the triage trigger starts the triage AGENT session directly
// through the SessionRuntime (gonk-t55), under the deterministic session name
// that is recorded on the bead as the correlation key sweep follows, with the
// rendered prompt MOUNTED into it. The prompt references the issue, instructs
// the fenced proposed-effects batch, and forbids any external API call (the
// pod holds no forge creds).
func TestDispatchStartsTriageSessionOnRun(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model-from-the-catalog",
		Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	// It started exactly one triage agent session.
	starts := rt.Starts()
	if len(starts) != 1 {
		t.Fatalf("starts = %+v, want exactly one triage session", starts)
	}
	spec := starts[0]
	if spec.Agent != "triage" {
		t.Fatalf("started agent %q, want triage", spec.Agent)
	}
	// The name is the correlation marker and the idempotency key: deterministic
	// in project, issue and METER's attempt, DNS-1123 (plan D2).
	if spec.Name != "gonk-triage-p42-i3-a1" {
		t.Fatalf("name = %q, want gonk-triage-p42-i3-a1", spec.Name)
	}
	prompt := string(spec.Prompt)
	// The marker lines are gone: they began "<!--", and nothing reads them.
	if strings.Contains(prompt, "gonk:model:") || strings.Contains(prompt, "gonk:meta:") {
		t.Fatalf("prompt still carries entrypoint marker lines:\n%s", prompt)
	}
	// It references the issue, instructs the fenced batch, forbids external calls.
	if !strings.Contains(prompt, "#3") || !strings.Contains(prompt, "group/repo") {
		t.Fatalf("prompt missing issue reference:\n%s", prompt)
	}
	if !strings.Contains(prompt, "GONK_BATCH_START") || !strings.Contains(prompt, "GONK_BATCH_END") {
		t.Fatalf("prompt missing batch sentinels:\n%s", prompt)
	}

	// The name is recorded on the bead so sweep can find the session again.
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateRunning || rec.SessionID != spec.Name || rec.Rung != "cheap" || rec.Attempt != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

// A `defer` is a NORMAL ANSWER. It parks the bead and exits 0. If it exited
// non-zero, every project's quiet hours would light up the dashboards as an
// outage, and people would learn to ignore red.
func TestDispatchParksOnDefer(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	retry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours,
		Detail: "quiet hours until 07:00", RetryAfter: retry, Attempt: 1,
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 -- a defer is a normal answer, not a failure", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("a deferred bead started %d sessions -- that is the whole point of the gate", n)
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateParked || !rec.RetryAfter.Equal(retry) {
		t.Fatalf("record = %+v, want parked with retry_after", rec)
	}
}

func TestDispatchStopsOnDeny(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionDeny, Reason: meterapi.ReasonLadderExhausted,
		Detail: "2 rungs, 2 gate failures", Attempt: 3,
	}}
	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatalf("a denied bead started %d sessions", n)
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateNeedsHuman {
		t.Fatalf("state = %q, want needs-human (a human must look at a ladder-exhausted bead)", rec.State)
	}
}

// *** THE TEST THAT PREVENTS THE CATASTROPHIC DRIFT. DO NOT DELETE IT. ***
//
// intake already called /decide and passed a rung in the order vars. The exec
// order must IGNORE that and ask meter AGAIN. If it ever trusts the vars, then a
// controller-initiated re-sling (which never passes intake at all) runs at the
// OLD rung with the OLD reservation and NO budget check -- and escalations spend
// unmetered. See "The two call sites MUST agree" at the top of this plan.
func TestDispatchAlwaysDecidesEvenWhenVarsCarryARung(t *testing.T) {
	rt := newTestRuntime()
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
		Decision: meterapi.DecisionRun, Rung: "expensive", Model: "m2", Attempt: 2, ReservationID: "rsv-2",
		Metadata: map[string]string{"gonk_rung": "expensive"},
	}}
	args := baseDispatchArgs()   // issue-triage -> the broker path
	args.Rung = "cheap"          // intake's hint, from a PREVIOUS decision
	args.ReservationID = "rsv-1" // stale
	args.Model = "m1"            // stale

	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys: readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")), Meter: meterClient(fm.server(t)), Runtime: rt, Store: store, Args: args})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if fm.calls != 1 {
		t.Fatalf("meter /decide called %d times, want exactly 1 -- THE GATE MUST ALWAYS ASK", fm.calls)
	}
	// The broker path must use meter's LIVE answer, never the stale order-var
	// hints: the bead's recorded rung/model/reservation are meter's, and the
	// alias carries meter's attempt.
	//
	// The recorded model is the load-bearing assertion now that the prompt no
	// longer carries a model marker. It is also the better one: the RECORD is
	// what the ladder, the reservation and sweep all read, whereas the marker was
	// only ever advisory text the pod could not consume on this path.
	starts := rt.Starts()
	if len(starts) != 1 {
		t.Fatalf("starts = %+v, want one", starts)
	}
	// The attempt in the name comes from METER's answer, not a caller value.
	if starts[0].Name != "gonk-triage-p42-i3-a2" {
		t.Fatalf("name = %q, want attempt 2 from meter", starts[0].Name)
	}
	if starts[0].Env[envModel] != "m2" {
		t.Fatalf("session GONK_MODEL = %q, want meter's live m2, not the stale order var", starts[0].Env[envModel])
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.Rung != "expensive" || rec.ReservationID != "rsv-2" || rec.Model != "m2" {
		t.Fatalf("the gate recorded the STALE order vars instead of meter's live answer: %+v\n"+
			"This is the bug that makes ladder escalations spend unmetered.", rec)
	}
}

// Meter owns ladder state (Plan 03, Decision 2). A caller-supplied attempt is a
// forgery vector: raise it and you skip straight to the most expensive rung.
func TestDispatchNeverSendsAnAttempt(t *testing.T) {
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef: meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"}, Decision: meterapi.DecisionDefer, Attempt: 1, RetryAfter: time.Now().Add(time.Hour)}}
	_ = runDispatch(context.Background(), dispatchDeps{
		// The session's project key rides the prompt row, and dispatch fails
		// closed without it (gonk-8gb).
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: newTestRuntime(),
		Store: beadstore.NewMemory(), Args: baseDispatchArgs(),
	})

	var raw map[string]any
	if err := json.Unmarshal(fm.lastBody, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, forbidden := range []string{"attempt", "outcome", "rung", "prior_outcomes"} {
		if _, ok := raw[forbidden]; ok {
			t.Fatalf("DecideRequest carried %q -- meter owns ladder state; sending it is a forgery vector for climbing the ladder", forbidden)
		}
	}
}

// If meter is unreachable we do NOT pour. No unmetered work, ever -- the same
// fail-closed rule intake follows.
func TestDispatchFailsClosedWhenMeterIsDown(t *testing.T) {
	rt := newTestRuntime()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)

	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys: readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")), Meter: meterClient(down.URL), Runtime: rt, Store: beadstore.NewMemory(), Args: baseDispatchArgs()})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (infra error)", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatal("METER WAS DOWN AND WE STARTED A SESSION ANYWAY. That is unmetered spend.")
	}
}

// No session runtime is a misconfiguration, exit 2 -- never a guess, and never
// a decision taken for a session that could not be started. (It replaces the
// dispatch-side GONK_CITY check: dispatch no longer talks to Gas City at all;
// main.go still refuses an unset GONK_CITY for the order route.)
func TestDispatchRefusesWithoutARuntime(t *testing.T) {
	fm := &fakeMeter{resp: runDecision("m")}
	code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Store: beadstore.NewMemory(), Args: baseDispatchArgs(),
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (misconfiguration)", code)
	}
	if fm.calls != 0 {
		t.Fatalf("meter /decide called %d times with no runtime, want 0", fm.calls)
	}
}

// An unknown trigger pours nothing -- orderForTrigger is a closed set.
func TestDispatchRefusesUnknownTrigger(t *testing.T) {
	rt := newTestRuntime()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		KeyRef: meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"}, Decision: meterapi.DecisionRun, Rung: "cheap", Attempt: 1}}
	args := baseDispatchArgs()
	args.Trigger = "something-new"
	code := runDispatch(context.Background(), dispatchDeps{
		// The agent must receive THIS project's key, not the controller's
		// (gonk-8gb); dispatch fails closed without it.
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), Runtime: rt, Store: beadstore.NewMemory(), Args: args,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if n := len(rt.Starts()); n != 0 {
		t.Fatal("an unknown trigger must not start a session")
	}
}
