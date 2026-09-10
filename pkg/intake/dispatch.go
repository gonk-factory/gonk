// Dispatch is the money path: the point where intake decides to spend by
// firing an order. It is Gate 1 of the two-gate budget system (spec 4.3, 10.2):
// intake asks gonk-meter's POST /v1/policy/decide and fires the order IF AND
// ONLY IF the decision is `run`. On `defer` it records retry_after and fires
// nothing (quiet hours land here -- Conflict B, settled: every wait-vs-spend
// decision belongs to meter). On `deny` it labels and fires nothing. Gate 2 (the
// pack's exec order, Plan 04) re-decides on every re-sling; that is not this
// file's concern.
package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// OrderRequest is what intake asks the Gas City supervisor to do (spec 4.3
// step 2). OD-A is RESOLVED by Plan 04, Task 2: the real contract is
// `POST /v0/city/{cityName}/order/gonk-dispatch/run` with body `{"vars":{...}}`.
// The route is grant-gated: when a write-auth key is configured the request
// carries a fresh ed25519 X-GC-City-Write grant plus the X-GC-Request CSRF
// header (minted by `pkg/gcapi`); without a key it sends no grant, the legacy
// loopback/network-position path. `pkg/gcapi` marshals this struct into that
// `vars` map; intake fires it only on a Gate-1 `run`.
//
// NOTE what is NOT here: there is no `not_before`. Quiet hours -- and every other
// wait-vs-spend decision -- belong to gonk-meter, which answers `defer` with a
// `retry_after`. Intake asks at Gate 1 and the PACK re-asks at Gate 2 (every
// pour); intake does not schedule work, it only fires the first `run`. On a
// `defer` intake records `retry_after` and fires nothing. (Conflict B.)
type OrderRequest struct {
	Trigger      string `json:"trigger"` // atags.Trigger*
	Project      string `json:"project"` // path_with_namespace
	ProjectID    int64  `json:"project_id"`
	Rig          string `json:"rig"`
	IssueIID     int64  `json:"issue_iid,omitempty"`
	NoteID       int64  `json:"note_id,omitempty"`
	DiscussionID string `json:"discussion_id,omitempty"`
	MRIID        int64  `json:"mr_iid,omitempty"`

	// SessionKey and BeadAnchor are DETERMINISTIC functions of the artifact. The
	// controller MUST treat BeadAnchor as an idempotency key: firing the same
	// order twice (a duplicate delivery, an intake restart mid-flight) must not
	// create a second bead or a second session.
	SessionKey string `json:"session_key"`
	BeadAnchor string `json:"bead_anchor"`

	// BeadID is the marker/record id the gonk-dispatch order requires (required
	// param `bead_id`). At Gate 1 the Gas City-internal bead does not exist yet
	// (the poured formula creates it), so intake sends the deterministic
	// BeadAnchor here: it is a stable, re-sling-safe identifier for the comment
	// marker (`<!-- gonk:bead:... -->`) and the bead-store record. gonk-gate
	// (Gate 2) reads it as GC_WEBHOOK_ARG_BEAD_ID; it is NEVER sent to meter
	// (meter keys on bead_anchor). Without this field orderVars omits `bead_id`
	// and every dispatch is rejected 422 "missing required param(s): bead_id".
	BeadID string `json:"bead_id"`

	// ConfigHash names the .gonk.yml that authorized this work.
	ConfigHash string `json:"config_hash"`

	// These are carried ONLY on a Gate-1 `run`: they are meter's decision, passed
	// through verbatim as order vars for the session. They are NOT trusted by
	// Gate 2 -- the pack re-decides at pour time and derives its own values.
	//
	// ATTEMPT IS CARRIED IN EXACTLY ONE PLACE: inside MetadataJSON. Meter mints the
	// atags (the seven `gonk_*` keys, `gonk_attempt` among them) as
	// `map[string]string` and returns them in `DecideResponse.Metadata`; intake
	// stamps that JSON here verbatim. There is deliberately NO separate
	// `Attempt` field on OrderRequest: an earlier draft carried it BOTH here as an
	// `int64` AND inside MetadataJSON, which is (a) redundant and (b) a needless
	// int/int64 width crossing (`DecideResponse.Attempt` is `int`, `atags.Tags.Attempt`
	// is `int`; nothing on this path needs an `int64`). Keeping it only inside the
	// verbatim metadata removes both the duplication and the crossing. Intake never
	// computes or sends an attempt as an INPUT to /decide (that would be a
	// ladder-climb forgery vector); the value here is meter's, echoed forward for
	// attribution, never a decision input.
	Rung          string          `json:"rung,omitempty"`
	Model         string          `json:"model,omitempty"`
	MetadataJSON  json.RawMessage `json:"metadata_json,omitempty"`
	KeyRef        meterapi.KeyRef `json:"key_ref,omitempty"`
	ReservationID string          `json:"reservation_id,omitempty"`
}

// Dispatcher is the seam to Gas City. Plan 04 supplies the real implementation.
type Dispatcher interface {
	FireOrder(ctx context.Context, o OrderRequest) error
}

// Decision is the pure gate's verdict. It has no clock and no scheduling: intake
// decides WHETHER work exists, never WHEN it runs.
type Decision struct {
	Dispatch     bool
	Trigger      string
	Reason       string // why it was dropped, when Dispatch is false
	SessionKey   string
	BeadAnchor   string
	DiscussionID string
}

// Decide is a pure function: entry + event -> verdict. No IO, no clock.
//
// This is the GitLab-STATE pre-filter, not the rung/budget gate. It exists to
// avoid even asking meter about work meter would certainly refuse (which would
// churn a bead and a session slot for nothing). The rung/budget gate is the
// /policy/decide call, made twice: Gate 1 in Handle (before the first dispatch)
// and Gate 2 in the pack (every pour, the enforcement point). So every rule here
// must be a SUBSET of meter's: dropping something meter would have allowed is a
// silent bug, and a much harder one to see than the reverse.
func Decide(e Entry, ev *ghook.Event, botUsername string) Decision {
	cls := e.Classification

	// The blocklist is checked FIRST, ahead of every other rule, and separately
	// from the reconciler's check (gonk-jn5). Belt and braces on purpose: the
	// reconciler stops a blocked project entering the cache, and this stops one
	// being acted on if it ever gets there another way -- a stale cache entry, a
	// direct Handle call, a future code path nobody has written yet. A guard
	// that exists at only one layer is a guard that a refactor can move around.
	if e.Blocked {
		return Decision{Reason: "blocked_project"}
	}

	switch ev.Kind {
	case ghook.KindMergeRequest:
		// MR events never dispatch work; they are reconcile signals (see Handle).
		return Decision{Reason: "mr_event"}

	case ghook.KindIssue:
		if ev.Issue.Action != "open" && ev.Issue.Action != "reopen" {
			// Deliberately narrow: dispatching on every "update" would re-triage an
			// issue every time somebody fixes a typo in it, and each re-triage costs
			// tokens.
			return Decision{Reason: "not_a_trigger"}
		}
		if d, ok := gate(e, atags.TriggerIssueTriage); !ok {
			return d
		}
		return dispatchDecision(atags.TriggerIssueTriage, ev.Project.ID, ev.Issue.IID, "")

	case ghook.KindNote:
		if ev.Note.NoteableType != "Issue" || ev.Issue == nil {
			return Decision{Reason: "not_a_trigger"}
		}
		if !Mentions(ev.Note.Body, botUsername) {
			return Decision{Reason: "no_mention"}
		}
		if d, ok := gate(e, atags.TriggerMentionReply); !ok {
			return d
		}
		// respond_to_mentions is NOT an Action, so meter never checks it. Intake is
		// its only enforcement point.
		if !cls.MayMentionReply() {
			return Decision{Reason: "mentions_disabled"}
		}
		return dispatchDecision(atags.TriggerMentionReply, ev.Project.ID, ev.Issue.IID, ev.Note.DiscussionID)
	}
	return Decision{Reason: "unhandled_kind"}
}

// gate applies the state/permission rules shared by every trigger.
func gate(e Entry, trigger string) (Decision, bool) {
	cls := e.Classification

	// No unmetered work, ever. `valid` and `pending` are the only states in which
	// meter has resolved the policy AND provisioned the key. Everything else --
	// unsynced, invalid, disabled, key-missing, declined, absent, unmanaged --
	// dispatches nothing. Note we do not enumerate the negatives: a state added
	// later is non-dispatchable by default, which is the safe direction.
	if !e.Dispatchable() {
		return Decision{Reason: "state_" + string(cls.State)}, false
	}
	// THERE IS NO `state_pending` DROP HERE ANY MORE (T-08). `pending` used to
	// mean "triage waits for .agent/", and it cost every project a metered
	// scaffold session before it could get a single issue triaged. The
	// onboarding merge request now ships the `.agent/` seed itself, so a
	// missing `.agent/` is a project that removed one -- a reason for a
	// thinner prompt, not for refusing the work. The state itself survives,
	// classified and counted; see Classify and AllStates.
	if !cls.MayTriage() {
		return Decision{Reason: "action_disabled"}, false
	}
	return Decision{}, true
}

func dispatchDecision(trigger string, projectID, issueIID int64, discussionID string) Decision {
	return Decision{
		Dispatch:     true,
		Trigger:      trigger,
		SessionKey:   SessionKey(projectID, issueIID),
		BeadAnchor:   BeadAnchor(projectID, issueIID),
		DiscussionID: discussionID,
	}
}

// SessionKey is stable across triage and every follow-up on the same issue, so a
// mention resumes the session that did the triage (spec 4.2, 5.5). Shape is
// constrained: it becomes part of a k8s object name.
func SessionKey(projectID, issueIID int64) string {
	return fmt.Sprintf("gonk-%d-issue-%d", projectID, issueIID)
}

// BeadAnchor identifies the work item. The controller MUST dedupe on it.
func BeadAnchor(projectID, issueIID int64) string {
	return fmt.Sprintf("gonk:%d:issue:%d", projectID, issueIID)
}

// mentionRe captures the FULL GitLab username token following an `@` that is at
// start-of-line or preceded by whitespace. A GitLab username starts with an
// alphanumeric or `_`, may contain `.`/`-`/`_` internally, and MUST end with an
// alphanumeric or `_` (it can never end in `.` or `-`) -- so the trailing
// `[a-z0-9_]` makes the capture stop before sentence punctuation.
//
// This exact-token capture is why Mentions cannot be a bare `@gonk\b` regex.
// RE2 has no lookahead, and `.`/`-` are themselves word boundaries, so `\b`
// cannot tell `@gonk.bot` (a DIFFERENT GitLab account) or `@gonk-city` (another
// account) apart from `@gonk.` (the bot, then a full stop). Capturing the whole
// token and comparing it EXACTLY to the bot username resolves every case:
// `@gonk.bot` -> "gonk.bot" != "gonk" (miss), `@gonk.` -> "gonk" (hit).
var mentionRe = regexp.MustCompile(`(?i)(?:^|\s)@([a-z0-9_](?:[a-z0-9_.-]*[a-z0-9_])?)`)

// Mentions reports whether the note addresses the bot. Quoted lines (markdown
// blockquotes, `>`-prefixed) are stripped first: a human quoting gonk's own
// comment is not a new request, and treating it as one is how a bot ends up
// talking to itself.
//
// It captures the full username token after each qualifying `@` and compares it
// case-insensitively and EXACTLY to botUsername. This is the money path: a near
// miss like `@gonk-staging` or `@gonk.bot` is a different account, and letting
// it through would reach /policy/decide and possibly fire a mention-reply for a
// comment that never addressed gonk.
func Mentions(body, botUsername string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue // a quoted line is not a fresh mention
		}
		for _, m := range mentionRe.FindAllStringSubmatch(line, -1) {
			if strings.EqualFold(m[1], botUsername) {
				return true
			}
		}
	}
	return false
}

// DecideClient is the Gate-1 seam. *MeterClient satisfies it; tests use a fake.
// It is deliberately narrower than MeterClient: Handle needs only /policy/decide.
type DecideClient interface {
	Decide(ctx context.Context, req meterapi.DecideRequest) (*meterapi.DecideResponse, error)
}

// DenyLabeler applies the ONE GitLab label intake writes on the dispatch path:
// the narrow Gate-1 `deny` marker (default `gonk::denied`), so a human can see
// meter refused the work. The SESSION posts every other label and comment (spec
// 4.3 step 5); intake writes nothing else here. Implemented over pkg/glab in
// cmd/gonk-intake; tests use a recorder. A nil Labeler is tolerated (Handle logs
// the deny instead of labelling), so a LogDispatcher-only wiring still runs.
type DenyLabeler interface {
	ApplyDenyLabel(ctx context.Context, projectID, issueIID int64, reason string) error
}

// DefaultStalenessWindow is the bound dispatch applies when StalenessWindow is
// left at its zero value (owner-approved addition, not in Plan 02's Task 9
// text). The reconciler keeps a project's last-known-good cache Entry even when
// a later GitLab fetch fails -- LastReconcile is bumped ONLY on a SUCCESSFUL
// reconcile -- so a short GitLab blip must not stop work. But sustained
// uncertainty about a project's policy must fail closed: dispatch refuses to
// fire for a project whose Entry has not been refreshed within this window. A
// zero StalenessWindow field therefore means "use this safe default", never
// "unbounded" -- fail closed by construction, the same direction as every other
// zero-value default in this package (see Dispatchable's whitelist).
const DefaultStalenessWindow = 30 * time.Minute

// Dispatch is the webhook-side consumer: cache lookup, gate, GATE 1, fire.
//
// GATE 1 ITSELF has no clock and no scheduling (`Now func() time.Time` is gone
// with quiet hours -- Conflict B). The one clock seam it carries, Now, backs
// ONLY the staleness cutoff below; it is nil-safe (nil means time.Now) so every
// existing caller that builds a Dispatch without setting it keeps working
// unchanged.
type Dispatch struct {
	Dispatcher    Dispatcher
	Meter         DecideClient // Gate 1: POST /v1/policy/decide before the first dispatch
	Labeler       DenyLabeler  // applies the deny label; nil -> Handle logs the deny
	Cache         *Cache
	BotUsername   string
	Obs           Observer
	Log           *slog.Logger
	KickReconcile func() // request an out-of-band reconcile pass (coalesced)

	// StalenessWindow bounds how old a cache Entry's LastReconcile may be before
	// dispatch refuses to fire for that project. Zero means DefaultStalenessWindow,
	// not unbounded (see the constant's doc comment).
	StalenessWindow time.Duration
	// Now is the clock seam for the staleness check; nil means time.Now. Tests
	// inject a fixed clock so the cutoff is provable without a sleep.
	Now func() time.Time
}

func (d *Dispatch) stalenessWindow() time.Duration {
	if d.StalenessWindow > 0 {
		return d.StalenessWindow
	}
	return DefaultStalenessWindow
}

func (d *Dispatch) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// stale reports whether e's cache is too old to trust for a dispatch decision.
// LastReconcile is bumped ONLY on a successful reconcile (a failed GitLab fetch
// returns before the Entry is rebuilt), so an Entry that has gone unrefreshed
// beyond the staleness window reflects SUSTAINED uncertainty about the
// project's policy, not a transient blip -- and dispatch must fail closed
// rather than fire on possibly-stale state. This is a dispatch-time gate on top
// of the classification, not a new State: Entry.State() is unaffected, and a
// reconcile that succeeds again immediately un-stales the project.
func (d *Dispatch) stale(e Entry) bool {
	return d.now().Sub(e.LastReconcile) > d.stalenessWindow()
}

// Handle is intake's webhook-side entry point and the ONE place it calls
// /policy/decide (Gate 1). The flow is fixed and total:
//
//  1. A merge-request event carries no work -- it is a reconcile signal (an
//     onboarding MR merging/closing changes derived state). Kick a reconcile,
//     fire nothing.
//  2. An unknown project has never been reconciled, so it is not registered with
//     meter and nothing is dispatchable for it. Kick a reconcile so the next
//     delivery finds it; fire nothing now.
//  3. STALENESS CUTOFF (owner-approved addition): if the cached Entry has not
//     been refreshed within StalenessWindow, treat the project as NOT
//     dispatchable -- do not even ask meter. Fire nothing.
//  4. Run the PURE GitLab-state gate (Decide). If it drops, record why and stop.
//  5. GATE 1: ask meter. On `run` fire the order ONCE, carrying meter's answer
//     VERBATIM (rung, model, metadata_json, key_ref, reservation_id); on `defer`
//     record retry_after and fire nothing; on `deny` apply the deny label and
//     fire nothing. All three decisions are HTTP 200 -- normal answers.
//
// It never sends an attempt to meter (DecideRequest has no such field) and never
// evaluates quiet hours (they arrive as a `defer` it simply handles).
//
// It reports whether it actually fired an order -- true only on the path that
// reaches d.Obs.Dispatched below -- not merely whether it was called. The issue
// sweep (reconcile.go, IssueSweeper) needs that to tell an attempt from a real
// dispatch.
func (d *Dispatch) Handle(ctx context.Context, ev *ghook.Event) bool {
	if ev.Kind == ghook.KindMergeRequest {
		d.KickReconcile()
		return false
	}

	entry, ok := d.Cache.Get(ev.Project.ID)
	if !ok {
		d.Obs.DispatchDropped("unknown_project")
		d.KickReconcile()
		return false
	}

	if d.stale(entry) {
		// Fail closed on sustained uncertainty: do not even ask meter. A blip is
		// tolerated (LastReconcile only moves forward on success); this is the
		// bound on how long "last known good" may be trusted.
		d.Obs.DispatchDropped("stale-config")
		d.Log.Warn("cache entry stale beyond the staleness window; firing nothing",
			"project", entry.Project.PathWithNamespace,
			"last_reconcile", entry.LastReconcile, "window", d.stalenessWindow())
		return false
	}

	dec := Decide(entry, ev, d.BotUsername)
	if !dec.Dispatch {
		d.Obs.DispatchDropped(dec.Reason)
		return false
	}

	project := entry.Project.PathWithNamespace
	rig := RigName(project)

	// ---- GATE 1 -----------------------------------------------------------
	// bead_id IS THE DETERMINISTIC BeadAnchor -- the SAME identifier Gate 2 (the
	// pack's gonk-dispatch exec order) will send for this work item, so meter
	// reuses its one open reservation instead of minting a second (the cross-plan
	// BeadAnchor contract). It is NOT a Gas City bead id: that bead does not exist
	// yet -- the order this fires is what creates it. And there is NO attempt field
	// (a caller-supplied attempt is a ladder-climb forgery vector).
	resp, err := d.Meter.Decide(ctx, meterapi.DecideRequest{
		Project:    project,
		Rig:        rig,
		BeadID:     dec.BeadAnchor, // bead_id == BeadAnchor, always
		SessionKey: dec.SessionKey,
		Trigger:    dec.Trigger,
	})
	if err != nil {
		// FAIL CLOSED. An unreachable budget enforcer is not permission to spend;
		// the next reconcile/delivery retries.
		d.Obs.DispatchDropped("decide_error")
		d.Log.Error("meter /decide failed; firing nothing", "err", err, "bead", dec.BeadAnchor)
		return false
	}

	if MayFire(resp.Decision) { // "run"
		md, err := json.Marshal(resp.Metadata) // meter's atags, stamped verbatim
		if err != nil {
			d.Obs.DispatchDropped("metadata_encode_error")
			d.Log.Error("could not marshal meter metadata; firing nothing", "err", err, "bead", dec.BeadAnchor)
			return false
		}
		o := OrderRequest{
			Trigger:       dec.Trigger,
			Project:       project,
			ProjectID:     entry.Project.ID,
			Rig:           rig,
			IssueIID:      ev.Issue.IID,
			DiscussionID:  dec.DiscussionID,
			SessionKey:    dec.SessionKey,
			BeadAnchor:    dec.BeadAnchor,
			BeadID:        dec.BeadAnchor, // Gate 1: no internal bead yet; use the anchor
			ConfigHash:    entry.Classification.ConfigHash,
			Rung:          resp.Rung,
			Model:         resp.Model,
			MetadataJSON:  md,
			KeyRef:        resp.KeyRef,
			ReservationID: resp.ReservationID,
		}
		if err := attributionSafeOrder(o); err != nil {
			d.Obs.DispatchDropped("attribution_unsafe")
			d.Log.Error("refusing an attribution-unsafe order", "err", err, "bead", dec.BeadAnchor)
			return false
		}
		if err := d.Dispatcher.FireOrder(ctx, o); err != nil {
			d.Obs.DispatchDropped("fire_error")
			d.Log.Error("FireOrder failed", "err", err, "bead", dec.BeadAnchor)
			return false
		}
		d.Obs.Dispatched(dec.Trigger)
		return true
	}

	switch resp.Decision {
	case "defer":
		// A NORMAL answer. Quiet hours and out-of-budget both land here. Record it
		// (metric + log with retry_after) and FIRE NOTHING; the pack unparks at
		// retry_after by re-deciding at Gate 2.
		d.Obs.DispatchDropped("decide_defer")
		d.Log.Info("meter deferred; firing nothing", "bead", dec.BeadAnchor,
			"reason", resp.Reason, "retry_after", resp.RetryAfter)
	case "deny":
		// Also HTTP 200. Apply the deny label so a human sees it, and FIRE NOTHING.
		d.Obs.DispatchDropped("decide_deny")
		if d.Labeler != nil {
			if err := d.Labeler.ApplyDenyLabel(ctx, entry.Project.ID, ev.Issue.IID, resp.Reason); err != nil {
				d.Log.Error("could not apply deny label", "err", err, "bead", dec.BeadAnchor)
			}
		}
		d.Log.Info("meter denied; firing nothing", "bead", dec.BeadAnchor, "reason", resp.Reason)
	default:
		// A decision kind this binary does not know. Fail closed.
		d.Obs.DispatchDropped("decide_unknown")
		d.Log.Error("meter returned an unknown decision; firing nothing", "decision", resp.Decision)
	}
	return false
}

// MayFire is the Gate-1 verdict->action rule, and it lives here so Plan 04's
// Task 3 Step 8 shared-semantics test can compare it against Gate 2 (the pack's
// gonk-dispatch exec order) and detect drift MECHANICALLY: intake fires the order
// on exactly the decisions the pack pours on. Keep it three lines and literal.
// (The shared decision table itself is defined in Plan 04.)
func MayFire(decision string) bool {
	return decision == "run"
}

// FireScaffold is called by the reconciler for a project with no `.agent/` that
// has opted into the metered scaffold (spec 5.3; MayScaffold owns that rule). It
// is dormant under the default config, which sets `actions.scaffold: false`. It runs
// the SAME Gate-1 path as Handle -- scaffold is a broker agent (Gate 2 creates its
// session directly; see cmd/gonk-gate/broker_inject.go's agentForTrigger), so it
// needs a rung and a reservation exactly like triage does -- but
// the work item is the PROJECT, not an issue, so its BeadAnchor/SessionKey are
// project-scoped. As in Handle, bead_id is that BeadAnchor and no attempt is sent.
//
// It applies the same staleness cutoff as Handle before firing: the reconciler
// passes the Entry it just built (LastReconcile == now), so this is normally a
// no-op guard, but it keeps the two order-firing paths symmetric.
func (d *Dispatch) FireScaffold(ctx context.Context, e Entry) error {
	if d.stale(e) {
		d.Obs.DispatchDropped("stale-config")
		return nil // fail closed; retry next reconcile pass
	}

	project := e.Project.PathWithNamespace
	rig := RigName(project)
	anchor := fmt.Sprintf("gonk:%d:scaffold", e.Project.ID)
	session := fmt.Sprintf("gonk-%d-scaffold", e.Project.ID)

	resp, err := d.Meter.Decide(ctx, meterapi.DecideRequest{
		Project: project, Rig: rig, BeadID: anchor, SessionKey: session,
		Trigger: atags.TriggerScaffold,
	})
	if err != nil {
		return fmt.Errorf("scaffold decide: %w", err) // fail closed
	}
	if !MayFire(resp.Decision) {
		d.Obs.DispatchDropped("scaffold_" + resp.Decision)
		return nil // defer/deny: fire nothing, retry next pass
	}
	md, err := json.Marshal(resp.Metadata)
	if err != nil {
		return fmt.Errorf("scaffold metadata: %w", err)
	}
	o := OrderRequest{
		Trigger: atags.TriggerScaffold, Project: project, ProjectID: e.Project.ID, Rig: rig,
		SessionKey: session, BeadAnchor: anchor, BeadID: anchor, ConfigHash: e.Classification.ConfigHash,
		Rung: resp.Rung, Model: resp.Model, MetadataJSON: md,
		KeyRef: resp.KeyRef, ReservationID: resp.ReservationID,
	}
	if err := attributionSafeOrder(o); err != nil {
		return err
	}
	if err := d.Dispatcher.FireOrder(ctx, o); err != nil {
		return err
	}
	d.Obs.Dispatched(atags.TriggerScaffold)
	return nil
}

// attributionSafeOrder is the boundary check for the fields that become atags /
// ledger columns downstream (PLAN.md carry-forward: pkg/atags accepts any string,
// so the caller enforces the charset). A newline or comma in Project or Rig would
// corrupt a CSV row, a log line, or a header.
func attributionSafeOrder(o OrderRequest) error {
	for name, v := range map[string]string{"project": o.Project, "rig": o.Rig} {
		if v == "" {
			return fmt.Errorf("order %s is empty", name)
		}
		if strings.ContainsAny(v, "\n\r,") {
			return fmt.Errorf("order %s contains attribution-unsafe characters", name)
		}
	}
	return nil
}

// ---------------------------------------------------------------- Dispatcher implementations (Task 10)

// LogDispatcher is the default Dispatcher, wired when GONK_SUPERVISOR_URL is
// unset. It never contacts Gas City: it logs every order it would have fired,
// which is safe (the log line names no secret -- OrderRequest has none) and
// keeps intake runnable with no supervisor at all (local dev, or a
// Plan-04-less environment).
type LogDispatcher struct {
	Log *slog.Logger
}

func NewLogDispatcher(log *slog.Logger) *LogDispatcher { return &LogDispatcher{Log: log} }

func (d *LogDispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *LogDispatcher) FireOrder(_ context.Context, o OrderRequest) error {
	d.log().Info("dispatch: order (no GONK_SUPERVISOR_URL configured; logging only, not firing)",
		"trigger", o.Trigger, "project", o.Project, "rig", o.Rig, "rung", o.Rung,
		"bead_anchor", o.BeadAnchor, "session_key", o.SessionKey)
	return nil
}

// defaultCityName is the one Gas City instance this whole deployment talks to
// (spec: "the Gas City supervisor", singular -- there is no multi-city concept
// in gonk today).
const defaultCityName = "gonk"

// dispatchOrderName is the single order intake ever fires: the Gate-2 budget
// gate (pack/orders/gonk-dispatch.toml). Every trigger routes through it.
const dispatchOrderName = "gonk-dispatch"

// HTTPDispatcher fires the gonk-dispatch order at the Gas City supervisor by
// delegating to *gcapi.Client -- the ONE typed client that mints the ed25519
// X-GC-City-Write grant (and the always-required X-GC-Request CSRF header) on
// every mutating POST. The order-run route is grant-gated (write-auth spec):
// when the client carries a Signer (a write-auth key was file-mounted -- see
// cmd/gonk-intake) the request is authenticated; when it does not, no grant
// headers are sent, exactly the legacy loopback / network-position path.
// Routing intake through gcapi keeps ALL request signing in one place instead
// of a second hand-rolled, unauthenticated HTTP client.
//
// vars is OrderRequest marshaled through its own JSON tags -- the single source
// of truth for the shape stays OrderRequest, not a second hand-maintained map.
type HTTPDispatcher struct {
	GC *gcapi.Client
}

// NewHTTPDispatcher builds a dispatcher over a *gcapi.Client aimed at baseURL's
// gonk city. signer, when non-nil, authenticates every order-run POST with a
// fresh grant; nil leaves the client on the unsigned loopback path (a
// grant-gated controller then rejects it -- a deploy responsibility). hc
// overrides the client's HTTP client (nil keeps gcapi's default).
func NewHTTPDispatcher(baseURL string, signer *gcapi.Signer, hc *http.Client) *HTTPDispatcher {
	c := gcapi.New(baseURL, defaultCityName)
	c.UserAgent = "gonk-intake"
	c.Signer = signer
	if hc != nil {
		c.HTTP = hc
	}
	return &HTTPDispatcher{GC: c}
}

func (d *HTTPDispatcher) FireOrder(ctx context.Context, o OrderRequest) error {
	vars, err := orderVars(o)
	if err != nil {
		return err
	}
	// gcapi.RunOrder attaches the write-auth grant (when a Signer is set) and
	// returns an error carrying only the RESPONSE body -- never the request
	// vars, so a key_ref (a Secret NAME, never key material) cannot leak into a
	// log line. Its APIError also disambiguates a write-auth rejection from a
	// missing order (gcapi's authHint), which the old hand-rolled client could
	// not.
	if _, err := d.GC.RunOrder(ctx, dispatchOrderName, vars); err != nil {
		return err
	}
	return nil
}

// orderVars turns an OrderRequest into the string-valued vars map the order-run
// route expects (Gas City namespaces each var into the exec's GC_WEBHOOK_ARG_*
// environment, which is string-typed). OrderRequest stays the single source of
// truth for the field set: it is marshaled through its own JSON tags, then each
// value is rendered as a string -- a JSON string value unquoted, everything
// else (numbers, the metadata_json object, the key_ref object) as its compact
// JSON text, the same way gonk-gate builds its own pour vars.
func orderVars(o OrderRequest) (map[string]string, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("dispatch: encode order: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("dispatch: encode order: %w", err)
	}
	vars := make(map[string]string, len(fields))
	for k, v := range fields {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			vars[k] = s // a JSON string: use its unquoted value
		} else {
			vars[k] = string(v) // number/object/array: its compact JSON text
		}
	}
	return vars, nil
}

// ---------------------------------------------------------------- DenyLabeler implementation (Task 10)

// DefaultDenyLabel is what GitLabDenyLabeler applies on a Gate-1 deny, absent
// an override. It matches the project's usual `gonk::` label prefix so a human
// scanning labels sees it grouped with gonk's other labels.
const DefaultDenyLabel = "gonk::denied"

// DenyGitLab is the narrow API slice NewDenyLabeler needs.
type DenyGitLab interface {
	// AddIssueLabel idempotently adds a label to an issue. GitLab's add_labels
	// parameter is itself idempotent (re-adding a present label is a no-op), so
	// no read-before-write is required here.
	AddIssueLabel(ctx context.Context, projectID, issueIID int64, label string) error
}

// GitLabDenyLabeler is the thin glab-backed DenyLabeler Task 10 wires into
// Dispatch.Labeler: on a Gate-1 deny it applies Label to the issue. It is the
// ONLY GitLab write intake makes on the dispatch path (spec 4.3 step 5 --
// every other label/comment is the session's, not intake's).
type GitLabDenyLabeler struct {
	GL    DenyGitLab
	Label string // "" -> DefaultDenyLabel
}

// NewDenyLabeler builds a GitLabDenyLabeler with the default label.
func NewDenyLabeler(gl DenyGitLab) *GitLabDenyLabeler {
	return &GitLabDenyLabeler{GL: gl}
}

func (l *GitLabDenyLabeler) ApplyDenyLabel(ctx context.Context, projectID, issueIID int64, _ string) error {
	label := l.Label
	if label == "" {
		label = DefaultDenyLabel
	}
	return l.GL.AddIssueLabel(ctx, projectID, issueIID, label)
}
