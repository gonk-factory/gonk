package intake

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

type recordingDispatcher struct{ orders []OrderRequest }

func (d *recordingDispatcher) FireOrder(_ context.Context, o OrderRequest) error {
	d.orders = append(d.orders, o)
	return nil
}

// fakeDecide is the Gate-1 seam under test: it records every DecideRequest and
// returns a canned response. The POINT of the Handle tests is what intake DOES
// with an answer, not how meter computes one (that is Plan 03's table).
type fakeDecide struct {
	resp *meterapi.DecideResponse
	err  error
	reqs []meterapi.DecideRequest
}

func (f *fakeDecide) Decide(_ context.Context, req meterapi.DecideRequest) (*meterapi.DecideResponse, error) {
	f.reqs = append(f.reqs, req)
	return f.resp, f.err
}

// recordingLabeler captures the single deny-path GitLab write.
type recordingLabeler struct {
	calls   int
	lastPID int64
	lastIID int64
	reason  string
}

func (l *recordingLabeler) ApplyDenyLabel(_ context.Context, pid, iid int64, reason string) error {
	l.calls++
	l.lastPID, l.lastIID, l.reason = pid, iid, reason
	return nil
}

// countingObserver records the last DispatchDropped reason, so the staleness
// cutoff tests can assert on WHY nothing fired, not merely THAT nothing fired.
type countingObserver struct {
	NopObserver
	lastDropped string
	dropCount   int
}

func (o *countingObserver) DispatchDropped(reason string) {
	o.lastDropped = reason
	o.dropCount++
}

// validEntry: meter says active, the repo has .agent/. Note we build meter's
// RESPONSE, not a gonkcfg.Effective -- intake has no resolver. LastReconcile is
// set to "now" (owner-approved staleness-cutoff addition): every dispatch
// decision now depends on how recently the cache was refreshed, and a project
// this test calls "valid" must also read as freshly reconciled, not merely
// correctly classified.
func validEntry() Entry {
	return Entry{
		Project:        glab.Project{ID: 42, PathWithNamespace: "group/repo"},
		Classification: Classify(obs(nil), active(nil)),
		LastReconcile:  time.Now(),
	}
}

// dispatchFor builds a Dispatch whose cache already holds validEntry() at id 42,
// wired to the given Gate-1 client, order dispatcher, and (optional) labeler.
func dispatchFor(m DecideClient, disp Dispatcher, lbl DenyLabeler) (*Dispatch, *int) {
	cache := NewCache()
	cache.Put(42, validEntry())
	kicks := 0
	return &Dispatch{
		Dispatcher: disp, Meter: m, Labeler: lbl, Cache: cache,
		BotUsername: "gonk", Obs: NopObserver{}, Log: slog.New(slog.DiscardHandler),
		KickReconcile: func() { kicks++ },
	}, &kicks
}

func issueEvent(action string) *ghook.Event {
	return &ghook.Event{
		Kind:    ghook.KindIssue,
		Project: ghook.Project{ID: 42, PathWithNamespace: "group/repo", DefaultBranch: "main"},
		User:    ghook.User{ID: 9, Username: "human"},
		Issue:   &ghook.Issue{IID: 3, Action: action, Title: "t"},
	}
}

func noteEvent(body string) *ghook.Event {
	return &ghook.Event{
		Kind:    ghook.KindNote,
		Project: ghook.Project{ID: 42, PathWithNamespace: "group/repo", DefaultBranch: "main"},
		User:    ghook.User{ID: 9, Username: "human"},
		Issue:   &ghook.Issue{IID: 3, Action: "update"},
		Note:    &ghook.Note{ID: 5, Body: body, NoteableType: "Issue", DiscussionID: "d1"},
	}
}

func mrEvent() *ghook.Event {
	return &ghook.Event{
		Kind:    ghook.KindMergeRequest,
		Project: ghook.Project{ID: 42, PathWithNamespace: "group/repo"},
		User:    ghook.User{ID: 9, Username: "human"},
	}
}

func TestDecideTriageOnNewIssue(t *testing.T) {
	d := Decide(validEntry(), issueEvent("open"), "gonk")
	if !d.Dispatch || d.Trigger != atags.TriggerIssueTriage {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideDrops(t *testing.T) {
	pending := validEntry()
	pending.Classification = Classify(obs(func(o *Observation) { o.AgentDirPresent = false }), active(nil))

	unsynced := validEntry()
	unsynced.Classification = Classify(obs(nil), nil) // meter has not answered

	keyMissing := validEntry()
	keyMissing.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.State = meterapi.StateKeyMissing
	}))

	noTriage := validEntry()
	noTriage.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions.Triage = false // vetoed at some layer (ADR-002)
	}))

	cases := map[string]struct {
		entry  Entry
		ev     *ghook.Event
		reason string
	}{
		"issue update is not a trigger": {validEntry(), issueEvent("update"), "not_a_trigger"},
		"issue close is not a trigger":  {validEntry(), issueEvent("close"), "not_a_trigger"},
		"pending project":               {pending, issueEvent("open"), "state_pending"},
		"policy not resolved":           {unsynced, issueEvent("open"), "state_unsynced"},
		"no virtual key yet":            {keyMissing, issueEvent("open"), "state_key-missing"},
		"triage not enabled":            {noTriage, issueEvent("open"), "action_disabled"},
		"comment without a mention":     {validEntry(), noteEvent("looks fine to me"), "no_mention"},
	}
	for name, c := range cases {
		d := Decide(c.entry, c.ev, "gonk")
		if d.Dispatch {
			t.Errorf("%s: dispatched, want drop", name)
		}
		if d.Reason != c.reason {
			t.Errorf("%s: reason = %q, want %q", name, d.Reason, c.reason)
		}
	}
}

// *** CONFLICT B'S REGRESSION GUARD. ***
// The PURE GATE never evaluates quiet hours: a project with quiet hours set still
// returns Dispatch=true here. Quiet hours are meter's, seen as a `defer` at the
// /policy/decide call -- Gate 1 in intake's Handle, Gate 2 in the pack -- NOT
// computed in this gate. If this test ever fails because someone "helpfully"
// re-added quiet-hours evaluation to the gate, we are back to two components
// owning one decision. (Gate 1's handling of the defer is tested in Handle.)
func TestQuietHoursAreNotIntakesProblem(t *testing.T) {
	e := validEntry()
	e.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Schedule = &meterapi.Schedule{QuietHours: "00:00-23:59", Timezone: "UTC"}
	}))
	d := Decide(e, issueEvent("open"), "gonk")
	if !d.Dispatch {
		t.Fatal("intake must dispatch regardless of quiet hours: METER defers (Conflict B), and the pack parks the bead")
	}
	// And the order carries no scheduling hint of any kind.
	if _, hasNotBefore := any(d).(interface{ NotBefore() }); hasNotBefore {
		t.Fatal("Decision must not carry a NotBefore; scheduling is meter's, via retry_after")
	}
}

func TestDecideMentionReply(t *testing.T) {
	d := Decide(validEntry(), noteEvent("hey @gonk can you re-triage this?"), "gonk")
	if !d.Dispatch || d.Trigger != atags.TriggerMentionReply {
		t.Fatalf("decision = %+v", d)
	}
	if d.DiscussionID != "d1" {
		t.Fatalf("the reply must be routed to the thread it came from: %+v", d)
	}
}

// respond_to_mentions is NOT one of Effective.Actions, so meter does not check
// it. Intake is its ONLY enforcement point.
func TestMentionRepliesCanBeTurnedOff(t *testing.T) {
	e := validEntry()
	e.Classification = Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Triage.RespondToMentions = false
	}))
	d := Decide(e, noteEvent("@gonk hello"), "gonk")
	if d.Dispatch {
		t.Fatal("respond_to_mentions: false must suppress the reply -- nothing downstream checks it")
	}
	if d.Reason != "mentions_disabled" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestMentions(t *testing.T) {
	cases := map[string]bool{
		// Genuine addresses to the bot.
		"@gonk":                          true,
		"@gonk ":                         true,
		"@gonk.":                         true, // end of sentence: the "." is not part of a username
		"@gonk,":                         true,
		"hey @gonk please":               true,
		"hey @gonk look at this":         true,
		"@GONK":                          true, // case-insensitive
		"GONK is case-insensitive @GONK": true,

		// DIFFERENT GitLab accounts -- usernames may contain "-", ".", "_". A bare
		// `@gonk\b` matcher forged a trigger on every one of these; the exact-token
		// compare rejects them.
		"@gonk-city please help":      false,
		"ping @gonk.bot about this":   false,
		"@gonk-staging is down":       false,
		"@gonkbot is a different bot": false,
		"@gonk_x is someone else":     false,

		// Not a mention at all.
		"mail me at x@gonk.example": false, // the @ is not preceded by start/whitespace
		"email@gonk":                false, // ditto: "email" abuts the @
		"no mention here":           false,
		"> a human quoting @gonk":   false, // blockquote is stripped
	}
	for body, want := range cases {
		if got := Mentions(body, "gonk"); got != want {
			t.Errorf("Mentions(%q) = %v, want %v", body, got, want)
		}
	}
}

// Session keys must be stable: a follow-up comment has to reach the same session
// the triage ran in (spec 4.2, 5.5).
func TestSessionKeyIsDeterministicAndK8sSafe(t *testing.T) {
	a := Decide(validEntry(), issueEvent("open"), "gonk")
	b := Decide(validEntry(), noteEvent("@gonk again"), "gonk")
	if a.SessionKey != b.SessionKey {
		t.Fatalf("triage and its follow-up must share a session key: %q vs %q", a.SessionKey, b.SessionKey)
	}
	if a.SessionKey != "gonk-42-issue-3" || a.BeadAnchor != "gonk:42:issue:3" {
		t.Fatalf("session key = %q, bead anchor = %q", a.SessionKey, a.BeadAnchor)
	}
}

// PLAN.md carry-forward: attribution values must be safe for the ledger. A
// newline or comma in the project or rig would corrupt a downstream row.
func TestDispatchRefusesAttributionUnsafeValues(t *testing.T) {
	if err := attributionSafeOrder(OrderRequest{Project: "group/repo", Rig: "group-repo"}); err != nil {
		t.Fatalf("a clean order was rejected: %v", err)
	}
	for _, bad := range []OrderRequest{
		{Project: "grp\nrepo", Rig: "r"},
		{Project: "grp,repo", Rig: "r"},
		{Project: "group/repo", Rig: "r\ri"},
		{Project: "", Rig: "r"},
	} {
		if err := attributionSafeOrder(bad); err == nil {
			t.Errorf("attribution-unsafe order accepted: %+v", bad)
		}
	}
}

// GATE 1, the `run` path. The pure gate passes, meter says run, and intake fires
// exactly one order carrying meter's answer VERBATIM. The DecideRequest it sent
// keys bead_id on the BeadAnchor and carries NO attempt field.
func TestHandleRun(t *testing.T) {
	fm := &fakeDecide{resp: &meterapi.DecideResponse{
		Decision: "run", Rung: "qwen-local", Model: "qwen3-14b", Attempt: 1,
		ReservationID: "rsv-1",
		Metadata:      map[string]string{"gonk_project": "group/repo", "gonk_rung": "qwen-local", "gonk_attempt": "1"},
		KeyRef:        meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}
	disp := &recordingDispatcher{}
	d, _ := dispatchFor(fm, disp, &recordingLabeler{})

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 1 {
		t.Fatalf("fired %d orders, want exactly 1 on a run", len(disp.orders))
	}
	o := disp.orders[0]
	// The order carries meter's decision verbatim.
	if o.Rung != "qwen-local" || o.Model != "qwen3-14b" || o.ReservationID != "rsv-1" {
		t.Fatalf("order did not carry meter's answer: %+v", o)
	}
	if o.KeyRef.SecretName != "gonk-key-abc" || o.KeyRef.SecretKey != "LITELLM_API_KEY" || o.BeadAnchor != "gonk:42:issue:3" {
		t.Fatalf("order = %+v", o)
	}
	// ConfigHash records WHICH .gonk.yml authorized the spend; it must reach the
	// order from the cache entry (validEntry's config resolves to a sha256 hash).
	if o.ConfigHash == "" || o.ConfigHash != validEntry().Classification.ConfigHash {
		t.Fatalf("order ConfigHash = %q, want the entry's %q", o.ConfigHash, validEntry().Classification.ConfigHash)
	}
	var md map[string]string
	if err := json.Unmarshal(o.MetadataJSON, &md); err != nil || md["gonk_attempt"] != "1" {
		t.Fatalf("metadata not stamped verbatim (attempt travels only here): %q", o.MetadataJSON)
	}
	// The DecideRequest keyed bead_id on the BeadAnchor and sent no attempt.
	if len(fm.reqs) != 1 || fm.reqs[0].BeadID != "gonk:42:issue:3" {
		t.Fatalf("Gate-1 request = %+v, want bead_id == BeadAnchor", fm.reqs)
	}
	raw, _ := json.Marshal(fm.reqs[0])
	if m := map[string]any{}; json.Unmarshal(raw, &m) == nil {
		if _, ok := m["attempt"]; ok {
			t.Fatal("intake sent an attempt to /decide -- a ladder-climb forgery vector")
		}
	}
}

// GATE 1, the `defer` path. A defer is a NORMAL answer: fire nothing, and do NOT
// treat it as an error. Quiet hours and out-of-budget both arrive this way.
func TestHandleDefer(t *testing.T) {
	fm := &fakeDecide{resp: &meterapi.DecideResponse{
		Decision: "defer", Reason: "quiet-hours",
	}}
	disp := &recordingDispatcher{}
	lbl := &recordingLabeler{}
	d, _ := dispatchFor(fm, disp, lbl)

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 0 {
		t.Fatal("a defer must fire NOTHING")
	}
	if lbl.calls != 0 {
		t.Fatal("a defer must not label -- only a deny does")
	}
	if len(fm.reqs) != 1 {
		t.Fatalf("meter /decide called %d times, want exactly 1", len(fm.reqs))
	}
}

// GATE 1, the `deny` path. Fire nothing, and apply the deny label so a human sees
// meter refused.
func TestHandleDeny(t *testing.T) {
	fm := &fakeDecide{resp: &meterapi.DecideResponse{
		Decision: "deny", Reason: "ladder-exhausted",
	}}
	disp := &recordingDispatcher{}
	lbl := &recordingLabeler{}
	d, _ := dispatchFor(fm, disp, lbl)

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 0 {
		t.Fatal("a deny must fire NOTHING")
	}
	if lbl.calls != 1 || lbl.lastPID != 42 || lbl.lastIID != 3 || lbl.reason != "ladder-exhausted" {
		t.Fatalf("deny label = %+v, want one call on (42, 3) with the meter reason", lbl)
	}
}

// GATE 1 FAIL-CLOSED. A decision that is not LITERALLY "run" must fire nothing.
// Meter answers with a bounded vocabulary (run/defer/deny); anything else -- an
// empty string, a typo, the right word in the wrong case, a trailing space -- is
// a malformed answer, and the only safe reading of "I could not understand the
// budget enforcer" is "do not spend". This table is the regression guard: it
// goes RED the moment someone widens MayFire or the switch to fire on anything
// that is merely not defer/deny.
func TestHandleFailsClosedOnUnknownDecision(t *testing.T) {
	for _, decision := range []string{"", "garbage", "RUN", "run "} {
		fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: decision, Rung: "qwen-local"}}
		disp := &recordingDispatcher{}
		obs := &countingObserver{}
		d, _ := dispatchFor(fm, disp, &recordingLabeler{})
		d.Obs = obs

		d.Handle(context.Background(), issueEvent("open"))

		if len(disp.orders) != 0 {
			t.Errorf("decision %q: fired an order; only a literal \"run\" may spend", decision)
		}
		if obs.dropCount == 0 {
			t.Errorf("decision %q: must record a drop", decision)
		}
	}
}

// GATE 1 FAIL-CLOSED on an unreachable meter. An error from /decide is NOT
// permission to spend: fire nothing, record the decide-error drop, and let the
// next delivery retry.
func TestHandleFailsClosedWhenMeterErrors(t *testing.T) {
	fm := &fakeDecide{err: errors.New("meter unreachable")}
	disp := &recordingDispatcher{}
	obs := &countingObserver{}
	d, _ := dispatchFor(fm, disp, &recordingLabeler{})
	d.Obs = obs

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 0 {
		t.Fatal("an unreachable meter must fire NOTHING -- it is not permission to spend")
	}
	if obs.lastDropped != "decide_error" {
		t.Fatalf("dropped reason = %q, want %q", obs.lastDropped, "decide_error")
	}
}

// A merge-request delivery is a reconcile signal, never work: it kicks a reconcile
// and fires nothing (an onboarding MR merging is what flips a project to pending).
func TestOnboardingMergeKicksReconcile(t *testing.T) {
	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run"}}
	disp := &recordingDispatcher{}
	d, kicks := dispatchFor(fm, disp, &recordingLabeler{})

	d.Handle(context.Background(), mrEvent())

	if len(disp.orders) != 0 || len(fm.reqs) != 0 {
		t.Fatal("an MR event must not call meter or fire an order")
	}
	if *kicks != 1 {
		t.Fatalf("kicks = %d, want 1 (an MR event is a reconcile signal)", *kicks)
	}
}

// An event for a project the cache has never seen kicks a reconcile (which will
// PUT it to meter) and fires nothing now -- nothing is metered yet.
func TestUnknownProjectTriggersReconcile(t *testing.T) {
	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run"}}
	disp := &recordingDispatcher{}
	d, kicks := dispatchFor(fm, disp, &recordingLabeler{})
	d.Cache.Delete(42) // make the project unknown

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 0 || len(fm.reqs) != 0 {
		t.Fatal("an unknown project must not call meter or fire an order")
	}
	if *kicks != 1 {
		t.Fatalf("kicks = %d, want 1", *kicks)
	}
}

// ---------------------------------------------------------------------------
// OWNER-APPROVED ADDITION: the bounded staleness cutoff (not in Plan 02's Task
// 9 text). The reconciler keeps a project's last-known-good cache Entry even
// when a later GitLab fetch fails -- LastReconcile is bumped ONLY on a
// successful reconcile -- so dispatch must refuse to fire once that Entry has
// gone stale beyond a bounded window: resilient to a short blip, fail-closed on
// sustained uncertainty. Every test below uses an INJECTED clock (Dispatch.Now)
// and never a real sleep.
// ---------------------------------------------------------------------------

// A fresh cache entry (well inside the staleness window) fires exactly like
// TestHandleRun, proving the cutoff is not a blanket "never fire": it only acts
// on genuinely stale entries.
func TestHandleFiresWhenCacheEntryIsFresh(t *testing.T) {
	fixedNow := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	e := validEntry()
	e.LastReconcile = fixedNow.Add(-5 * time.Minute) // well inside a 30m window

	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}}
	disp := &recordingDispatcher{}
	cache := NewCache()
	cache.Put(42, e)
	d := &Dispatch{
		Dispatcher: disp, Meter: fm, Labeler: &recordingLabeler{}, Cache: cache,
		BotUsername: "gonk", Obs: NopObserver{}, Log: slog.New(slog.DiscardHandler),
		KickReconcile:   func() {},
		StalenessWindow: 30 * time.Minute,
		Now:             func() time.Time { return fixedNow },
	}

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 1 {
		t.Fatalf("fresh entry: fired %d orders, want exactly 1", len(disp.orders))
	}
	if len(fm.reqs) != 1 {
		t.Fatalf("fresh entry: meter /decide called %d times, want exactly 1", len(fm.reqs))
	}
}

// The heart of the addition: an entry whose LastReconcile is older than
// StalenessWindow must not fire, must not even CALL meter, and must record a
// clear reason. This test goes RED if the cutoff is removed (Handle would call
// meter, see "run", and fire).
func TestHandleRefusesStaleCacheEntry(t *testing.T) {
	fixedNow := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	e := validEntry()
	e.LastReconcile = fixedNow.Add(-31 * time.Minute) // just beyond a 30m window

	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}}
	disp := &recordingDispatcher{}
	obs := &countingObserver{}
	cache := NewCache()
	cache.Put(42, e)
	d := &Dispatch{
		Dispatcher: disp, Meter: fm, Labeler: &recordingLabeler{}, Cache: cache,
		BotUsername: "gonk", Obs: obs, Log: slog.New(slog.DiscardHandler),
		KickReconcile:   func() {},
		StalenessWindow: 30 * time.Minute,
		Now:             func() time.Time { return fixedNow },
	}

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 0 {
		t.Fatal("a stale cache entry must fire NOTHING, even though meter would say run")
	}
	if len(fm.reqs) != 0 {
		t.Fatal("a stale cache entry must not even call meter -- Gate 1 must never run")
	}
	if obs.lastDropped != "stale-config" {
		t.Fatalf("dropped reason = %q, want %q", obs.lastDropped, "stale-config")
	}
}

// A zero StalenessWindow must mean "use the safe default", never "unbounded".
// The fresh entry (10m old) still fires; the stale one (45m old) does not, even
// though neither Dispatch sets StalenessWindow explicitly.
func TestStalenessWindowZeroMeansDefaultNotUnbounded(t *testing.T) {
	fixedNow := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	cases := map[string]struct {
		age      time.Duration
		wantFire bool
	}{
		"10m old, inside DefaultStalenessWindow": {10 * time.Minute, true},
		"45m old, beyond DefaultStalenessWindow": {45 * time.Minute, false},
	}
	for name, c := range cases {
		e := validEntry()
		e.LastReconcile = fixedNow.Add(-c.age)

		fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}}
		disp := &recordingDispatcher{}
		cache := NewCache()
		cache.Put(42, e)
		d := &Dispatch{ // StalenessWindow deliberately left at its zero value
			Dispatcher: disp, Meter: fm, Labeler: &recordingLabeler{}, Cache: cache,
			BotUsername: "gonk", Obs: NopObserver{}, Log: slog.New(slog.DiscardHandler),
			KickReconcile: func() {},
			Now:           func() time.Time { return fixedNow },
		}

		d.Handle(context.Background(), issueEvent("open"))

		gotFire := len(disp.orders) == 1
		if gotFire != c.wantFire {
			t.Errorf("%s (StalenessWindow=0): fired=%v, want %v", name, gotFire, c.wantFire)
		}
	}
}

// The cutoff is `>` window, not `>=`: an entry aged EXACTLY the window is still
// fresh enough to fire. Pins the boundary so a future refactor to `>=` (which
// would refuse a just-barely-in-window entry) is caught.
func TestStalenessBoundaryIsInclusive(t *testing.T) {
	fixedNow := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	e := validEntry()
	e.LastReconcile = fixedNow.Add(-30 * time.Minute) // age == StalenessWindow exactly

	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}}
	disp := &recordingDispatcher{}
	cache := NewCache()
	cache.Put(42, e)
	d := &Dispatch{
		Dispatcher: disp, Meter: fm, Labeler: &recordingLabeler{}, Cache: cache,
		BotUsername: "gonk", Obs: NopObserver{}, Log: slog.New(slog.DiscardHandler),
		KickReconcile:   func() {},
		StalenessWindow: 30 * time.Minute,
		Now:             func() time.Time { return fixedNow },
	}

	d.Handle(context.Background(), issueEvent("open"))

	if len(disp.orders) != 1 {
		t.Fatalf("age == StalenessWindow must still fire (cutoff is strictly >): fired %d", len(disp.orders))
	}
}

// FireScaffold applies the same staleness cutoff as Handle: symmetric
// protection on the scaffold order path.
func TestFireScaffoldRefusesStaleCacheEntry(t *testing.T) {
	fixedNow := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	e := validEntry()
	e.LastReconcile = fixedNow.Add(-31 * time.Minute)

	fm := &fakeDecide{resp: &meterapi.DecideResponse{Decision: "run", Rung: "qwen-local"}}
	disp := &recordingDispatcher{}
	d := &Dispatch{
		Dispatcher: disp, Meter: fm, Labeler: &recordingLabeler{}, Cache: NewCache(),
		BotUsername: "gonk", Obs: NopObserver{}, Log: slog.New(slog.DiscardHandler),
		KickReconcile:   func() {},
		StalenessWindow: 30 * time.Minute,
		Now:             func() time.Time { return fixedNow },
	}

	if err := d.FireScaffold(context.Background(), e); err != nil {
		t.Fatalf("FireScaffold must fail closed quietly on a stale entry, not error: %v", err)
	}
	if len(disp.orders) != 0 || len(fm.reqs) != 0 {
		t.Fatal("a stale entry must not fire a scaffold order or call meter")
	}
}
