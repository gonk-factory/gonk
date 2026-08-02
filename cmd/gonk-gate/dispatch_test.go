package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// fakeMeter serves /v1/policy/decide with a canned answer and records what it was
// asked. It is deliberately dumb: the POINT of these tests is what gonk-gate DOES
// with an answer, not how meter computes one (that is Plan 03's exhaustive table).
type fakeMeter struct {
	resp     meterapi.DecideResponse
	mu       sync.Mutex
	lastBody []byte
	calls    int
}

func (f *fakeMeter) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := jsonReadAll(r)
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

// The formula pour path is still live for triggers not yet ported to the v2
// broker (scaffold, mention). This asserts it hands the formula EXACTLY what
// meter said. (Triage is ported -- see TestDispatchCreatesTriageSessionOnRun.)
func TestDispatchPoursOnRun(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model-from-the-catalog",
		Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	args := baseDispatchArgs()
	// mention-reply is the remaining non-ported trigger: it still pours its
	// formula. (scaffold used to be the example here and is now on the broker.)
	args.Trigger = "mention-reply"
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Args: args,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	// It poured the TRIGGER'S formula-order, exactly once -- and created no session.
	if len(gc.Created) != 0 {
		t.Fatalf("a non-ported trigger must not create a broker session: %+v", gc.Created)
	}
	if len(gc.Poured) != 1 || gc.Poured[0].Order != "gonk-mention" {
		t.Fatalf("poured = %+v, want one gonk-mention", gc.Poured)
	}
	// And it handed the formula EXACTLY what meter said -- the rung, the model, the
	// reservation, the key_ref, and atags VERBATIM. Meter mints the metadata; the
	// pack stamps it. The pack must never construct a tag itself.
	v := gc.Poured[0].Vars
	if v["rung"] != "cheap" || v["model"] != "some-model-from-the-catalog" || v["reservation_id"] != "rsv-1" {
		t.Fatalf("vars = %+v", v)
	}
	if v["key_secret_name"] != "gonk-key-abc" || v["key_secret_key"] != "LITELLM_API_KEY" {
		t.Fatalf("key_ref not passed through: %+v", v)
	}
	var md map[string]string
	if err := json.Unmarshal([]byte(v["metadata_json"]), &md); err != nil || md["gonk_rung"] != "cheap" {
		t.Fatalf("metadata not stamped verbatim: %q", v["metadata_json"])
	}
	// The key_ref is a POINTER. If the key MATERIAL is anywhere in these vars, it is
	// now in the event bus and in every log line that echoes an order.
	for k, val := range v {
		if val == "sk-secret" {
			t.Fatalf("key material leaked into order var %q", k)
		}
	}

	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateRunning || rec.Rung != "cheap" || rec.Attempt != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

// C2 (the triage broker): on a `run`, the triage trigger no longer pours the
// gonk-triage formula -- it creates the triage AGENT session directly, stamps a
// unique, re-sling-stable alias (the correlation key recorded on the bead), and
// injects the rendered prompt as the session's initial message. The prompt
// carries meter's model + attribution as marker lines (the entrypoint parses
// them), references the issue, instructs the fenced proposed-effects batch, and
// forbids any external API call (the pod holds no forge creds). No formula, so
// #4668's formula-var drop is moot.
func TestDispatchCreatesTriageSessionOnRun(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model-from-the-catalog",
		Attempt: 1, ReservationID: "rsv-1",
		Metadata: map[string]string{"gonk_project": "group/repo", "gonk_rung": "cheap"},
		KeyRef:   meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}

	// It created exactly one triage agent session, and poured no formula.
	if len(gc.Poured) != 0 {
		t.Fatalf("the broker path must NOT pour a formula: %+v", gc.Poured)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want exactly one triage session", gc.Created)
	}
	cs := gc.Created[0]
	if cs.Kind != "agent" || cs.Name != "triage" || !cs.Async {
		t.Fatalf("created session = %+v, want kind=agent name=triage async=true", cs)
	}
	// The alias is the correlation marker: deterministic, colon-free (bead
	// anchors have colons; aliases forbid them), attempt-suffixed so a re-sling
	// never collides with the prior attempt's session.
	const wantAlias = "gonk.triage.p42.i3.a1"
	if cs.Alias != wantAlias {
		t.Fatalf("alias = %q, want %q", cs.Alias, wantAlias)
	}
	// The prompt is delivered by the submit (the create-time inject is dropped
	// by the k8s provider -- gonk-u1p.1).
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one", gc.Submitted)
	}
	cs.Message = gc.Submitted[0].Message
	// IT MUST CARRY NO "!". opencode's composer treats a bang as its shell-mode
	// trigger, and a prompt typed in as keystrokes then runs as a shell command
	// instead of reaching the model -- which is what wedged every live triage
	// session. See sanitizeForKeystrokeDelivery.
	if strings.Contains(cs.Message, "!") {
		t.Fatalf("prompt contains a bang, which opencode reads as shell mode:\n%s", cs.Message)
	}
	// The marker lines are gone with it: they began "<!--", and the pod cannot
	// read them on this delivery path anyway (they were parsed from --prompt,
	// which the k8s provider never composes).
	if strings.Contains(cs.Message, "gonk:model:") || strings.Contains(cs.Message, "gonk:meta:") {
		t.Fatalf("prompt still carries entrypoint marker lines:\n%s", cs.Message)
	}
	// It references the issue, instructs the fenced batch, forbids external calls.
	if !strings.Contains(cs.Message, "#3") || !strings.Contains(cs.Message, "group/repo") {
		t.Fatalf("prompt missing issue reference:\n%s", cs.Message)
	}
	if !strings.Contains(cs.Message, "GONK_BATCH_START") || !strings.Contains(cs.Message, "GONK_BATCH_END") {
		t.Fatalf("prompt missing batch sentinels:\n%s", cs.Message)
	}

	// The alias is recorded on the bead so sweep can read the session back.
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateRunning || rec.SessionID != wantAlias || rec.Rung != "cheap" || rec.Attempt != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

// Every pour targets a formula order (gonk-triage/gonk-scaffold/gonk-mention).
// Gas City's graphv2.PrepareInvocation hard-rejects a caller-supplied vars map
// that contains the key "bead_id" (or "convoy_id", or the deprecated alias
// "issue") with "formulas v2 reserved variable ... cannot be supplied by the
// caller" -- REGARDLESS of whether the formula itself declares that var. If
// this map ever regains a "bead_id" key (it carries the Gas City bead id
// under "city_bead_id" instead -- see the comment beside its construction),
// every single dispatch fails the instant it reaches the real loader. See
// pack/formulas/gonk-triage.toml's matching comment.
func TestDispatchNeverSendsAReservedFormulaVarName(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "some-model", Attempt: 1,
		ReservationID: "rsv-1",
	}}

	args := baseDispatchArgs()
	args.Trigger = "mention-reply" // guards the still-live formula pour path
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Args: args,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Poured) != 1 {
		t.Fatalf("poured = %+v, want exactly one", gc.Poured)
	}
	v := gc.Poured[0].Vars
	for _, reserved := range []string{"bead_id", "convoy_id", "issue"} {
		if _, ok := v[reserved]; ok {
			t.Fatalf("vars carried reserved formulas v2 key %q -- Gas City's real loader "+
				"rejects this pour outright: %+v", reserved, v)
		}
	}
	if v["city_bead_id"] != "gk-1a2b" {
		t.Fatalf("city_bead_id = %q, want the Gas City bead id", v["city_bead_id"])
	}
}

// A `defer` is a NORMAL ANSWER. It parks the bead and exits 0. If it exited
// non-zero, every project's quiet hours would light up the dashboards as an
// outage, and people would learn to ignore red.
func TestDispatchParksOnDefer(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	retry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours,
		Detail: "quiet hours until 07:00", RetryAfter: retry, Attempt: 1,
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 -- a defer is a normal answer, not a failure", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a deferred bead must NOT pour a formula -- that is the whole point of the gate")
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateParked || !rec.RetryAfter.Equal(retry) {
		t.Fatalf("record = %+v, want parked with retry_after", rec)
	}
}

func TestDispatchStopsOnDeny(t *testing.T) {
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDeny, Reason: meterapi.ReasonLadderExhausted,
		Detail: "2 rungs, 2 gate failures", Attempt: 3,
	}}
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store,
		Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("a denied bead must not pour")
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
	gc := gcapitest.New(t)
	store := beadstore.NewMemory()
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "expensive", Model: "m2", Attempt: 2, ReservationID: "rsv-2",
		Metadata: map[string]string{"gonk_rung": "expensive"},
	}}
	args := baseDispatchArgs()   // issue-triage -> the broker path
	args.Rung = "cheap"          // intake's hint, from a PREVIOUS decision
	args.ReservationID = "rsv-1" // stale
	args.Model = "m1"            // stale

	code := runDispatch(context.Background(), dispatchDeps{Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: store, Args: args})
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
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one", gc.Created)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one", gc.Submitted)
	}
	if gc.Created[0].Alias != "gonk.triage.p42.i3.a2" { // meter's Attempt=2, not a caller value
		t.Fatalf("alias = %q, want attempt 2 from meter", gc.Created[0].Alias)
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
	fm := &fakeMeter{resp: meterapi.DecideResponse{Decision: meterapi.DecisionDefer, Attempt: 1, RetryAfter: time.Now().Add(time.Hour)}}
	_ = runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gcapitest.New(t).Client("gonk-city"),
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
	gc := gcapitest.New(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)

	code := runDispatch(context.Background(), dispatchDeps{Meter: meterClient(down.URL), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(), Args: baseDispatchArgs()})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (infra error)", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("METER WAS DOWN AND WE POURED A FORMULA ANYWAY. That is unmetered spend.")
	}
}

// OD-1: no default city. A missing GONK_CITY is a misconfiguration, exit 2, and it
// must never be papered over with a guess.
func TestDispatchRefusesWithoutCity(t *testing.T) {
	gc := gcapitest.New(t)
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(gc.URL()), GC: gc.Client(""), Store: beadstore.NewMemory(), Args: baseDispatchArgs(),
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (misconfiguration)", code)
	}
}

// An unknown trigger pours nothing -- orderForTrigger is a closed set.
func TestDispatchRefusesUnknownTrigger(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{Decision: meterapi.DecisionRun, Rung: "cheap", Attempt: 1}}
	args := baseDispatchArgs()
	args.Trigger = "something-new"
	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(), Args: args,
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if len(gc.Poured) != 0 {
		t.Fatal("an unknown trigger must not pour")
	}
}
