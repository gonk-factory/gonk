package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// GitLab is the slice of the API the reconciler uses. *glab.Client satisfies it;
// tests drive the real client against pkg/glab/glabtest.
type GitLab interface {
	ListMemberProjects(ctx context.Context) ([]glab.Project, error)
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
	DirExists(ctx context.Context, projectID int64, path, ref string) (bool, error)
	ListHooks(ctx context.Context, projectID int64) ([]glab.Hook, error)
	CreateHook(ctx context.Context, projectID int64, o glab.HookOptions) (*glab.Hook, error)
	EditHook(ctx context.Context, projectID, hookID int64, o glab.HookOptions) (*glab.Hook, error)
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
	ListMembers(ctx context.Context, projectID int64) ([]glab.Member, error)
	ListIssues(ctx context.Context, projectID int64, o glab.IssueListOptions) ([]glab.Issue, error)
}

// IssueSweeper is the one method the issue sweep needs from *Dispatch. Nil is
// legal: the reconciler simply does not sweep.
//
// It is deliberately the SAME entry point the webhook path uses, fed a
// synthesized event, rather than a second dispatch route. A parallel path would
// be a second place for the staleness window, the Decide rules and the
// classification gate to drift apart, and those are the rules that stop gonk
// spending money it should not.
type IssueSweeper interface {
	Handle(ctx context.Context, ev *ghook.Event)
}

// Onboarder opens the deterministic onboarding MR (Task 8). Nil is legal: the
// reconciler simply does not onboard.
type Onboarder interface {
	Onboard(ctx context.Context, p glab.Project) error
	// Declined reports whether an onboarding MR was closed unmerged and the bot
	// has not been re-invited since (spec 5.3, AD-3).
	Declined(ctx context.Context, p glab.Project) (bool, error)
}

// Scaffolder fires the metered .agent/ scaffold order for a project that has no
// `.agent/` AND has opted in with `actions.scaffold: true` (spec 5.3; see
// Classification.MayScaffold, which is the only place that rule lives). It is
// satisfied by *Dispatch (pkg/intake/dispatch.go, Task 9); the reconciler is
// declared against the narrow interface it actually calls, rather than the
// concrete type, so this package does not need Task 9's file to exist to build
// or test the reconciler on its own. Nil is legal: the reconciler simply does
// not scaffold.
type Scaffolder interface {
	FireScaffold(ctx context.Context, e Entry) error
}

// Observer is intake's metric surface (Prometheus impl in Task 10).
type Observer interface {
	WebhookOutcome(event string, o ghook.Outcome) // also satisfies ghook.Observer
	ReconcileResult(result string, d time.Duration)
	ProjectStates(counts map[State]int)
	// MeterPush result is one of: ok | invalid | error.
	//   ok      -- 200, meter resolved the config
	//   invalid -- 422, the PROJECT's yaml is bad (recorded; not our bug)
	//   error   -- 5xx / unreachable / 400 (400 IS our bug; log it loudly)
	MeterPush(result string)
	OnboardingResult(result string)
	Dispatched(trigger string)
	DispatchDropped(reason string)
}

type NopObserver struct{}

func (NopObserver) WebhookOutcome(string, ghook.Outcome)  {}
func (NopObserver) ReconcileResult(string, time.Duration) {}
func (NopObserver) ProjectStates(map[State]int)           {}
func (NopObserver) MeterPush(string)                      {}
func (NopObserver) OnboardingResult(string)               {}
func (NopObserver) Dispatched(string)                     {}
func (NopObserver) DispatchDropped(string)                {}

type Summary struct {
	Projects int
	Errors   int
	Duration time.Duration
	// MeterPushes and Dispatched are the counts Task 10's ReconcileSummary (the
	// `?wait=true` wire type, server.go) reports; they are gathered here rather
	// than re-derived from the Obs metric calls, which have no query API.
	MeterPushes int
	Dispatched  int
	// Blocked counts projects skipped because they are on the blocklist. It is
	// normally a small constant (gonk's own repository); a sudden rise means
	// membership widened somewhere.
	Blocked int
	// IssuesSwept counts open issues the sweep handed to Dispatch this pass. It
	// is a count of ATTEMPTS, not of dispatches: Handle re-applies the staleness
	// window and the Decide rules and may drop any of them.
	IssuesSwept int
	// ErrorMsgs is one line per failed project, "path: err". Never a token, never
	// a secret: pkg/glab's APIError and MeterClient.do both refuse to put
	// credential material in an error string, so echoing these here (they end up
	// in ReconcileSummary.Errors, a test/operator surface) is safe.
	ErrorMsgs []string
}

type Reconciler struct {
	GL        GitLab
	Meter     *MeterClient
	Cache     *Cache
	Onboarder Onboarder
	Dispatch  Scaffolder
	// Issues drives the issue sweep (spec 5.2). Nil disables it.
	Issues IssueSweeper
	// IssueLabelPrefix is the namespace the broker applies to labels it adds
	// (`gonk::` by default). An open issue already carrying one has been triaged,
	// so the sweep skips it.
	//
	// THIS IS AN OPTIMISATION, NOT THE SAFETY PROPERTY, and the distinction
	// matters: the prefix is per-project configurable in the broker, so this can
	// be wrong, and an issue dispatched seconds ago has no label yet either way.
	// What makes a re-fire harmless is the deterministic bead anchor -- the meter
	// keys /decide idempotency, ladder state and the reservation on it
	// (cmd/gonk-gate/dispatch.go, dispatch_idempotency_test.go), so a second
	// order for the same issue rejoins the first reservation instead of spending
	// twice. Exactly the tradeoff Entry.ScaffoldFiredAt already documents.
	IssueLabelPrefix string
	// IssueSweepLimit caps how many issues one pass may hand to Dispatch, per
	// project. Zero means DefaultIssueSweepLimit.
	//
	// THIS IS A SPEND GUARD, not a performance one. Onboard a project with a
	// hundred open issues and an uncapped sweep fires a hundred triage orders in
	// a single pass -- each one a real model call against the project's budget.
	// The meter's ceiling would eventually refuse them, but "eventually" is after
	// the money is gone, and a backlog is not urgent. Capped, a backlog drains a
	// few per pass and an operator has time to notice.
	IssueSweepLimit int
	// Blocked names projects gonk must never work on, however it came to see
	// them (gonk-jn5). Enforced HERE as well as at dispatch: a blocked project
	// must never be registered with meter, never have a webhook provisioned,
	// and never enter the cache -- not merely be refused later.
	Blocked Blocklist
	// IssueSweepMaxAge bounds how far back the sweep looks. Zero means
	// DefaultIssueSweepMaxAge.
	//
	// THIS IS WHAT MAKES THE SWEEP A SAFETY NET RATHER THAN A BACKFILL. A LOST
	// EVENT IS RECENT BY DEFINITION -- it was delivered, ACKed and dropped
	// minutes ago. An issue nobody has touched in two months is not a lost
	// event, it is backlog, and triaging it is a decision an operator should
	// make deliberately rather than something a bug fix does on their behalf.
	//
	// Measured on project 75, 2026-09-07: unbounded, the first pass would have
	// dispatched 57 orders against stale e2e issues dating to July -- 11.4M est
	// tokens, 22.8% of that project's monthly budget, in under two hours.
	// Bounded to 24h it dispatches 8, which are the ones that were actually
	// lost. It also collapses the listing: `updated_after` is applied
	// server-side, so a project with thousands of open issues is a page or two
	// rather than fifty per pass.
	IssueSweepMaxAge time.Duration
	Obs              Observer
	Log              *slog.Logger

	BotUserID int64
	HookURL   string // public webhook URL, WITHOUT the gen parameter
	HookToken string // current secret (rotation slot 1)
	TokenGen  string // bumped on rotation; embedded in the hook URL (ADR-003)
	SSLVerify bool

	// MeterResyncInterval re-registers a project with meter even when its config
	// hash has not changed (default 1h). This is NOT belt-and-braces: meter's
	// answer can change WITHOUT the project's .gonk.yml changing -- an operator
	// flipping the instance kill switch, or tightening a group ceiling, changes
	// Effective for a project whose file never moved. Without this, the
	// config-hash short-circuit would pin intake's copy of the policy forever.
	// Zero (the test default) disables periodic forcing: only the config-hash
	// change and the "meter has never answered" case trigger a re-PUT.
	MeterResyncInterval time.Duration

	// NOTE what is NOT here: `Instance gonkcfg.Policy` and
	// `GroupPolicy func(string) gonkcfg.Policy`. Intake does not hold operator
	// policy and does not resolve. Meter does. (Conflict A.)

	// StartupLadder is how long to wait between reconcile attempts at BOOT,
	// before settling into the periodic interval. Nil means defaultStartupLadder.
	// See runStartupLadder for why it exists; tests set tiny values.
	StartupLadder []time.Duration

	// UnsettledRetryInterval is how soon to reconcile again after a pass that
	// left a project UNSYNCED, instead of waiting the full interval. Zero means
	// defaultUnsettledRetry.
	//
	// This is the non-boot half of gonk-fan and it is easy to miss: a meter 5xx
	// or timeout during ANY pass -- not just the first -- downgrades a perfectly
	// healthy project to unsynced (see reconcileProject: "Classify will make it
	// `unsynced`, and nothing will be dispatched"). Without this, one transient
	// blip during a routine pass silently stops dispatching for up to a full
	// interval, with no restart to correlate it against.
	UnsettledRetryInterval time.Duration

	// The fields below back Loop/Kick/WaitForNextPass (Task 10, HB-1). They are
	// zero-value-safe: every existing test that builds a Reconciler by literal
	// keeps working, and initPass lazily wires them on first use.
	loopInit sync.Once
	kickCh   chan struct{}

	passMu       sync.Mutex
	startSeq     uint64 // seq of the pass currently running or most recently started
	completedSeq uint64 // seq of the most recently COMPLETED pass
	passDone     chan struct{}
	lastSummary  ReconcileSummary
	haveSummary  bool
}

// initPass lazily wires the Loop/Kick/WaitForNextPass machinery. Safe to call
// from any of those methods, any number of times, from any goroutine.
func (r *Reconciler) initPass() {
	r.loopInit.Do(func() {
		r.kickCh = make(chan struct{}, 1)
		r.passDone = make(chan struct{})
	})
}

// Kick requests an out-of-band reconcile pass. Concurrent kicks coalesce: the
// channel is buffered to exactly 1, so no matter how many callers kick while a
// pass is in flight (or already queued), at most ONE extra pass runs. This is
// what makes /admin/reconcile (unauthenticated, private-listener-only, spec
// 5.2) a bounded amount of free work rather than an amplifier.
func (r *Reconciler) Kick() {
	r.initPass()
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

// Loop runs ReconcileOnce every interval (a zero or negative interval uses the
// documented default of 10 minutes) and also whenever Kick is called, until ctx
// is done. It is meant to run in its own goroutine for the lifetime of the
// process.
func (r *Reconciler) Loop(ctx context.Context, interval time.Duration) {
	r.initPass()
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	// BEFORE the ticker, not after it. This is gonk-fan: without it the first
	// reconcile happens a full interval after boot, and for those ten minutes
	// every webhook is answered 200 and dropped as state_unsynced.
	r.runStartupLadder(ctx)
	t := time.NewTicker(r.nextInterval(interval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runPass(ctx)
		case <-r.kickCh:
			r.runPass(ctx)
		}
		// Re-arm from the state the pass just produced: fast while anything is
		// unsynced, normal cadence once it settles.
		t.Reset(r.nextInterval(interval))
	}
}

// nextInterval is how long until the next pass should be due: the normal
// cadence when everything is resolved, the much shorter retry when something is
// still unsynced. See UnsettledRetryInterval for why the distinction matters.
func (r *Reconciler) nextInterval(interval time.Duration) time.Duration {
	if r.startupSettled() {
		return interval
	}
	retry := r.UnsettledRetryInterval
	if retry <= 0 {
		retry = defaultUnsettledRetry
	}
	// Never SLOWER than the normal cadence: a caller running a 5s interval must
	// not be pushed out to 30s by this.
	if retry > interval {
		return interval
	}
	return retry
}

// defaultStartupLadder is the boot-time retry schedule: reconcile immediately,
// then at 5s, 15s and 60s if anything is still unresolved, then hand over to the
// periodic interval.
//
// The first rung is what closes gonk-fan's ten-minute window. The REST of the
// ladder exists because one pass is demonstrably not enough: intake and meter
// start together and intake usually wins, so the first registration fails with
// "connect: connection refused" (observed twice on 2026-07-31 and again on the
// 2026-08-04 deploy). Without the retries, a boot race puts the project right
// back to unsynced-until-the-next-tick, which is the whole window again.
//
// The rungs are spaced to cover a pod start, not to hammer: a meter that is
// still not answering after 80 seconds has a problem no amount of retrying from
// here will fix, and the periodic loop takes it from there.
var defaultStartupLadder = []time.Duration{0, 5 * time.Second, 15 * time.Second, 60 * time.Second}

// defaultUnsettledRetry is how soon to try again after a pass that left
// something unsynced, once the startup ladder is over. It is far shorter than
// the 10m cadence and far longer than a hot loop: a meter that is down stays
// down for a while, and hammering it does not help.
const defaultUnsettledRetry = 30 * time.Second

// runStartupLadder reconciles at boot until nothing is left unsynced, or the
// ladder is exhausted, or ctx is done.
//
// WHAT "SETTLED" MEANS HERE, and it is deliberately narrow: no project is in
// StateUnsynced. Unsynced is the ONE state that means "meter has not answered",
// which is the condition this ladder can actually do something about. Every
// other non-dispatchable state -- invalid, disabled, key-missing, declined --
// is a resolved answer, and retrying it would be a boot-time loop over
// something a human has to change. Zero projects is settled too: an instance
// with nothing onboarded has nothing to wait for.
//
// It does NOT lower the bar for dispatching. Nothing here makes an unsynced
// project dispatchable; it only shortens how long the project stays unsynced.
func (r *Reconciler) runStartupLadder(ctx context.Context) {
	ladder := r.StartupLadder
	if ladder == nil {
		ladder = defaultStartupLadder
	}
	for _, wait := range ladder {
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		r.runPass(ctx)
		if r.startupSettled() {
			return
		}
	}
}

// startupSettled reports whether the ladder has nothing left to wait for.
func (r *Reconciler) startupSettled() bool {
	if r.Cache == nil {
		return true
	}
	return r.Cache.CountByState()[StateUnsynced] == 0
}

// runPass runs one ReconcileOnce and records its ReconcileSummary under
// passMu, bumping completedSeq and broadcasting passDone so WaitForNextPass
// wakes up. Every pass gets a monotonically increasing start sequence number,
// assigned BEFORE ReconcileOnce runs -- that is what lets WaitForNextPass tell
// "a pass that started before my request" apart from "the pass I asked for".
func (r *Reconciler) runPass(ctx context.Context) ReconcileSummary {
	r.initPass()

	r.passMu.Lock()
	r.startSeq++
	mySeq := r.startSeq
	r.passMu.Unlock()

	started := time.Now()
	sum, err := r.ReconcileOnce(ctx)
	finished := time.Now()

	rs := ReconcileSummary{
		StartedAt:   started,
		FinishedAt:  finished,
		Projects:    sum.Projects,
		States:      stateCountsToWire(r.Cache.CountByState()),
		MeterPushes: sum.MeterPushes,
		Dispatched:  sum.Dispatched,
		IssuesSwept: sum.IssuesSwept,
		Errors:      sum.ErrorMsgs,
		Result:      "ok",
	}
	switch {
	case err != nil:
		rs.Result = "error"
		rs.Errors = append(rs.Errors, err.Error())
	case sum.Errors > 0:
		rs.Result = "partial"
	}

	r.passMu.Lock()
	r.completedSeq = mySeq
	r.lastSummary = rs
	r.haveSummary = true
	done := r.passDone
	r.passDone = make(chan struct{})
	r.passMu.Unlock()
	close(done)

	return rs
}

// WaitForNextPass kicks a pass and blocks until a pass that STARTED AT OR AFTER
// this call has COMPLETED, then returns its summary. This is HB-1 (Plan 06):
// the guarantee that makes `?wait=true` an "ask", not a "sleep and hope" --
// without it, a pass already in flight when the request arrived could satisfy
// the wait while reflecting a pre-request world.
//
// It requires Loop to be running (nothing else drains kickCh); if it is not,
// this blocks until ctx is done. The caller (server.go) bounds ctx with a
// server-side timeout, per spec.
func (r *Reconciler) WaitForNextPass(ctx context.Context) (ReconcileSummary, error) {
	r.initPass()

	r.passMu.Lock()
	baselineSeq := r.startSeq // any pass with seq <= this already started strictly before this call
	ch := r.passDone
	r.passMu.Unlock()

	r.Kick()

	for {
		select {
		case <-ctx.Done():
			return ReconcileSummary{}, ctx.Err()
		case <-ch:
		}
		r.passMu.Lock()
		completed, rs, next := r.completedSeq, r.lastSummary, r.passDone
		r.passMu.Unlock()
		if completed > baselineSeq {
			return rs, nil
		}
		ch = next
	}
}

// LastSummary returns the most recently completed pass's summary, and whether
// any pass has completed yet. Used for readiness (main.go): "first reconcile
// completed" is exactly `ok == true` here.
func (r *Reconciler) LastSummary() (ReconcileSummary, bool) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	return r.lastSummary, r.haveSummary
}

// stateCountsToWire converts the State-keyed gauge map to the plain
// map[string]int the wire ReconcileSummary carries.
func stateCountsToWire(counts map[State]int) map[string]int {
	out := make(map[string]int, len(counts))
	for s, n := range counts {
		out[string(s)] = n
	}
	return out
}

// log returns a non-nil logger: callers that build a Reconciler by literal (as
// every test in this package does) commonly leave Log unset, and log/slog's
// *Logger panics on a nil receiver.
func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// RigName derives the Gas City rig name from a project path (spec 4.2).
//
// It must be stable: it is an attribution tag value in every spend row, and it is
// what meter and the ledger join on. Note it flattens the WHOLE path -- the rig
// for `group/repo` is `group-repo`, NOT `repo`. Two projects named `repo` in
// different groups would otherwise share a rig, and their spend would merge.
func RigName(path string) string {
	return strings.ReplaceAll(path, "/", "-")
}

// ReconcileOnce rebuilds gonk's entire view of the world from GitLab and
// gonk-meter. It is the correctness path (spec 5.2): it must never abort the
// whole pass because one project misbehaved.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (Summary, error) {
	start := time.Now()
	projects, err := r.GL.ListMemberProjects(ctx)
	if err != nil {
		r.Obs.ReconcileResult("error", time.Since(start))
		return Summary{}, fmt.Errorf("reconcile: list memberships: %w", err)
	}

	seen := make(map[int64]bool, len(projects))
	sum := Summary{Projects: len(projects)}
	for _, p := range projects {
		// Blocked projects are dropped BEFORE anything observes them. Not
		// "reconciled and then refused": no meter registration, no webhook
		// provisioning, no cache entry, so nothing downstream can act on one
		// even by mistake. `seen` is deliberately NOT set, so a project that
		// becomes blocked while already cached is de-registered by the
		// vanished-membership sweep below, exactly as if the bot had been
		// removed from it.
		if r.Blocked.Blocked(p) {
			sum.Blocked++
			r.log().Warn("project is on the blocklist; not reconciling it",
				"project", p.PathWithNamespace, "project_id", p.ID)
			continue
		}
		seen[p.ID] = true
		out, err := r.reconcileProject(ctx, p)
		if out.meterPushed {
			sum.MeterPushes++
		}
		if out.scaffoldFired {
			sum.Dispatched++
		}
		if n, serr := r.sweepIssues(ctx, p); serr != nil {
			sum.Errors++
			sum.ErrorMsgs = append(sum.ErrorMsgs, fmt.Sprintf("%s: sweep issues: %s", p.PathWithNamespace, serr))
			r.log().Error("issue sweep failed", "project", p.PathWithNamespace, "err", serr)
		} else {
			sum.IssuesSwept += n
		}
		if err != nil {
			sum.Errors++
			sum.ErrorMsgs = append(sum.ErrorMsgs, fmt.Sprintf("%s: %s", p.PathWithNamespace, err))
			r.log().Error("reconcile project failed", "project", p.PathWithNamespace, "err", err)
		}
	}

	// Projects that vanished from the membership list were de-onboarded (spec
	// 5.1: removing the bot de-onboards). DELETE the meter registration -- that
	// is what disables the project and deletes its LiteLLM key -- then drop them.
	for _, id := range r.Cache.IDs() {
		if seen[id] {
			continue
		}
		if e, ok := r.Cache.Get(id); ok {
			if err := r.Meter.Deregister(ctx, e.Project.PathWithNamespace); err != nil {
				r.Obs.MeterPush("error")
				r.log().Warn("failed to deregister on de-onboard", "project", e.Project.PathWithNamespace, "err", err)
				continue // keep it cached and retry next pass; do NOT forget a live key
			}
			r.Obs.MeterPush("ok")
		}
		r.Cache.Delete(id)
	}

	r.Obs.ProjectStates(r.Cache.CountByState())
	sum.Duration = time.Since(start)
	result := "ok"
	if sum.Errors > 0 {
		result = "partial"
	}
	r.Obs.ReconcileResult(result, sum.Duration)
	return sum, nil
}

// observe gathers the facts Classify needs FROM GITLAB. Note the ordering: the
// config is fetched with an explicit byte cap, because these bytes are
// attacker-controlled (ADR-002 "Untrusted input").
//
// Member is always true here: reconcileProject is only called for projects
// ListMemberProjects returned, and that call is itself scoped to the bot's
// memberships. StateUnmanaged is reachable only via Classify's other callers
// (e.g. a webhook for a project the bot has since left).
func (r *Reconciler) observe(ctx context.Context, p glab.Project) (Observation, error) {
	obs := Observation{Member: true}
	ref := p.DefaultBranch
	if ref == "" {
		ref = "main"
	}

	raw, err := r.GL.GetRawFile(ctx, p.ID, ConfigPath, ref, MaxConfigBytes)
	switch {
	case err == nil:
		obs.ConfigBytes = raw
	case glab.IsNotFound(err):
		// absent: onboarding candidate
	case strings.Contains(err.Error(), "too large"):
		obs.ConfigTooLarge = true
	default:
		return Observation{}, fmt.Errorf("fetch %s: %w", ConfigPath, err)
	}

	if obs.ConfigBytes == nil && !obs.ConfigTooLarge {
		if r.Onboarder != nil {
			declined, err := r.Onboarder.Declined(ctx, p)
			if err != nil {
				return Observation{}, fmt.Errorf("check decline: %w", err)
			}
			obs.OnboardingDeclined = declined
		}
		return obs, nil
	}

	present, err := r.GL.DirExists(ctx, p.ID, AgentDir, ref)
	if err != nil {
		return Observation{}, fmt.Errorf("check %s/: %w", AgentDir, err)
	}
	obs.AgentDirPresent = present
	return obs, nil
}

// hookURL embeds the secret generation. GitLab never returns a hook's token, so
// there is no way to compare the configured token with the live one; the
// generation marker in the URL is the observable proxy for "this hook was
// provisioned with the current secret" (ADR-003).
func (r *Reconciler) hookURL() string {
	sep := "?"
	if strings.Contains(r.HookURL, "?") {
		sep = "&"
	}
	return r.HookURL + sep + "gen=" + url.QueryEscape(r.TokenGen)
}

func (r *Reconciler) hookOptions() glab.HookOptions {
	return glab.HookOptions{
		URL:                   r.hookURL(),
		Token:                 r.HookToken,
		IssuesEvents:          true,
		NoteEvents:            true,
		MergeRequestsEvents:   true,
		PushEvents:            false,
		EnableSSLVerification: r.SSLVerify,
	}
}

// ensureHook is idempotent: it creates the hook if missing, repairs it if its URL
// (generation), event flags, or SSL setting drifted, and does nothing otherwise.
// "Ours" is decided by URL prefix, so a project's own unrelated hooks are never
// touched.
func (r *Reconciler) ensureHook(ctx context.Context, p glab.Project) error {
	hooks, err := r.GL.ListHooks(ctx, p.ID)
	if err != nil {
		return err
	}
	want := r.hookOptions()
	base := strings.SplitN(r.HookURL, "?", 2)[0]
	for _, h := range hooks {
		if !strings.HasPrefix(h.URL, base) {
			continue
		}
		if h.URL == want.URL &&
			h.IssuesEvents == want.IssuesEvents &&
			h.NoteEvents == want.NoteEvents &&
			h.MergeRequestsEvents == want.MergeRequestsEvents &&
			h.PushEvents == want.PushEvents &&
			h.EnableSSLVerification == want.EnableSSLVerification {
			return nil // already correct
		}
		_, err := r.GL.EditHook(ctx, p.ID, h.ID, want)
		return err
	}
	_, err = r.GL.CreateHook(ctx, p.ID, want)
	return err
}

// register sends the RAW bytes. Note there is no BudgetFrom, no Resolve, and no
// Effective anywhere in here -- that is the point.
func (r *Reconciler) register(ctx context.Context, p glab.Project, obs Observation) (*meterapi.ProjectResponse, error) {
	return r.Meter.Register(ctx, meterapi.ProjectRequest{
		Project:         p.PathWithNamespace,
		ProjectID:       p.ID,
		Rig:             RigName(p.PathWithNamespace),
		DefaultBranch:   p.DefaultBranch,
		ConfigCommitSHA: obs.ConfigCommitSHA,
		GonkYML:         string(obs.ConfigBytes),
	})
}

// projectOutcome is what one reconcileProject call actually DID, so
// ReconcileOnce can build a ReconcileSummary without re-deriving counts from
// the Obs metric calls (which have no query API -- Prometheus counters are
// write-only from this package's side).
type projectOutcome struct {
	meterPushed   bool // a real PUT to meter succeeded (the config-hash short-circuit does not count)
	scaffoldFired bool
}

func (r *Reconciler) reconcileProject(ctx context.Context, p glab.Project) (projectOutcome, error) {
	var out projectOutcome
	if p.Archived {
		// ReconcileOnce already set seen[p.ID] = true before calling us (it has
		// to: an archived project is still a membership, not a vanished one), so
		// the vanished-membership sweep below will never see this project as
		// gone and will never DELETE its meter registration. This is the only
		// place that observes Archived, so deregistering has to happen here, or
		// the LiteLLM key outlives the project's archival.
		//
		// Deregister UNCONDITIONALLY, off `p` itself, not gated on a cache hit.
		// The cache is derived-only and rebuilds empty on every restart
		// (cache.go): a project that is already archived when a fresh replica
		// (or the first pass after a restart/rollout) sees it was never
		// Cache.Put in the first place, so a cache-gated deregister would skip
		// it forever. MeterClient.Deregister documents deleting an unknown
		// project as a 204, not a 404, so the extra idempotent DELETE this
		// causes on an already-deregistered project is free.
		if err := r.Meter.Deregister(ctx, p.PathWithNamespace); err != nil {
			r.Obs.MeterPush("error")
			r.log().Warn("failed to deregister archived project", "project", p.PathWithNamespace, "err", err)
			return out, err // keep cached; retry deregistration next pass
		}
		r.Obs.MeterPush("ok")
		r.Cache.Delete(p.ID)
		return out, nil
	}
	obs, err := r.observe(ctx, p) // GitLab: membership, .gonk.yml bytes, .agent/, decline
	if err != nil {
		return out, err
	}

	// The webhook is the latency path; failing to provision it is logged and
	// retried, never fatal (reconciliation still works without it).
	if err := r.ensureHook(ctx, p); err != nil {
		r.log().Warn("hook provisioning failed", "project", p.PathWithNamespace, "err", err)
	}

	prev, hadPrev := r.Cache.Get(p.ID)

	// REGISTER WITH METER BEFORE CLASSIFYING. `invalid`, `disabled` and
	// `key-missing` are meter's answers -- intake cannot compute them.
	var mr *meterapi.ProjectResponse
	lastSync := prev.LastMeterSync
	if obs.ConfigBytes != nil {
		hash := hashConfig(obs.ConfigBytes)

		// The config-hash short-circuit: skip the PUT when the bytes have not
		// changed, meter has already answered, AND we are not overdue for a
		// periodic resync (meter's answer can change without the config moving).
		resyncDue := r.MeterResyncInterval > 0 && time.Since(prev.LastMeterSync) >= r.MeterResyncInterval
		if hadPrev && prev.Classification.ConfigHash == hash && prev.Classification.Meter != nil && !resyncDue {
			mr = prev.Classification.Meter
		} else {
			mr, err = r.register(ctx, p, obs)
			if err != nil {
				var inv *ErrInvalidConfig
				if errors.As(err, &inv) {
					mr = inv.Response // a 422 IS an answer: state=invalid, key deleted
					r.Obs.MeterPush("invalid")
					lastSync = time.Now()
				} else {
					// Unreachable / 5xx / our bug. We do not know this project's policy.
					// Classify will make it `unsynced`, and nothing will be dispatched.
					// Leave lastSync where it was: mr stays nil, so the ConfigHash
					// short-circuit's `prev.Classification.Meter != nil` guard will not
					// fire next pass either, and we retry unconditionally.
					r.log().Warn("meter registration failed", "project", p.PathWithNamespace, "err", err)
					r.Obs.MeterPush("error")
				}
			} else {
				r.Obs.MeterPush("ok")
				lastSync = time.Now()
				out.meterPushed = true
			}
		}
	}

	cls := Classify(obs, mr)

	entry := Entry{Project: p, Classification: cls, LastReconcile: time.Now(), LastMeterSync: lastSync}
	if hadPrev {
		entry.ScaffoldFiredAt = prev.ScaffoldFiredAt
	}

	if cls.MayOnboard() && r.Onboarder != nil {
		if err := r.Onboarder.Onboard(ctx, p); err != nil {
			r.Obs.OnboardingResult("error")
			r.log().Warn("onboarding failed", "project", p.PathWithNamespace, "err", err)
		}
	}

	r.Cache.Put(p.ID, entry)

	// spec 5.3: the metered .agent/ scaffold order. OPT-IN since T-08 -- the
	// onboarding merge request seeds .agent/ deterministically, so MayScaffold
	// now also requires `actions.scaffold: true` and this is dormant by default.
	// Triage does NOT depend on it and never waits for it.
	if entry.Classification.MayScaffold() && r.Dispatch != nil &&
		time.Since(entry.ScaffoldFiredAt) > time.Hour {
		if err := r.Dispatch.FireScaffold(ctx, entry); err != nil {
			r.log().Warn("scaffold dispatch failed", "project", p.PathWithNamespace, "err", err)
		} else {
			entry.ScaffoldFiredAt = time.Now()
			r.Cache.Put(p.ID, entry)
			out.scaffoldFired = true
		}
	}
	return out, nil
}

// DefaultIssueLabelPrefix matches the broker's default label namespace
// (cmd/gonk-gate/broker_label.go). See Reconciler.IssueLabelPrefix for why a
// mismatch here costs an extra order rather than correctness.
const DefaultIssueLabelPrefix = "gonk::"

// DefaultIssueSweepLimit is deliberately small. The sweep is a SAFETY NET for
// events that were lost, not a backfill tool: in steady state the webhook has
// already handled everything and this finds nothing. A number this size drains
// a surprise backlog over several passes instead of spending a project's month
// in one.
const DefaultIssueSweepLimit = 5

// DefaultIssueSweepMaxAge is the recency bound. A day is comfortably longer
// than any plausible outage-plus-restart window and comfortably shorter than
// "the backlog".
const DefaultIssueSweepMaxAge = 24 * time.Hour

// sweepIssues is the ISSUE half of spec 5.2, and it exists because the spec's
// central claim was only half true:
//
//	"Reconciliation is the correctness path; webhooks are the latency
//	 optimization."
//
// Reconciliation was implemented for PROJECTS -- memberships, .gonk.yml, hook
// provisioning, rig registration -- and for nothing else. For issue events the
// webhook was the ONLY path, which made it the correctness path, which is
// exactly what the spec says it must not be.
//
// MEASURED CONSEQUENCE (gonk-vrf, 2026-09-07): issues 65 and 67 on project 75
// were delivered, ACKed 200, accepted into an in-memory sink, and lost when the
// pod restarted seconds later. GitLab does not retry, nothing reconciled them,
// and they sat untouched until a human noticed. This closes that: an issue that
// should have been triaged and was not gets picked up on the next pass.
//
// It returns the number of issues handed to Dispatch, which is a count of
// ATTEMPTS. Handle re-applies the staleness window and the full Decide rules,
// so it may drop every one of them.
func (r *Reconciler) sweepIssues(ctx context.Context, p glab.Project) (int, error) {
	if r.Issues == nil {
		return 0, nil
	}
	entry, ok := r.Cache.Get(p.ID)
	if !ok {
		return 0, nil
	}
	// Ask the REAL rule whether this project could dispatch a triage at all,
	// rather than re-deriving it. A project that cannot is one whose issues we
	// must not even list -- listing costs an API call per pass forever, and for
	// an unmanaged or denied project the answer can never change to yes without
	// a reconcile that would update the cache first anyway.
	if _, allowed := gate(entry, atags.TriggerIssueTriage); !allowed {
		return 0, nil
	}

	maxAge := r.IssueSweepMaxAge
	if maxAge <= 0 {
		maxAge = DefaultIssueSweepMaxAge
	}
	cutoff := time.Now().Add(-maxAge)

	issues, err := r.GL.ListIssues(ctx, p.ID, glab.IssueListOptions{
		State:        "opened",
		UpdatedAfter: cutoff,
	})
	if err != nil {
		return 0, err
	}

	prefix := r.IssueLabelPrefix
	if prefix == "" {
		prefix = DefaultIssueLabelPrefix
	}

	limit := r.IssueSweepLimit
	if limit <= 0 {
		limit = DefaultIssueSweepLimit
	}

	swept := 0
	for _, is := range issues {
		if swept >= limit {
			r.log().Warn("issue sweep hit its per-pass limit; the rest wait for the next pass",
				"project", p.PathWithNamespace, "limit", limit, "open_issues", len(issues))
			break
		}
		// The loop guard, which the webhook path gets from the event payload and
		// a swept issue would otherwise get from nowhere. An issue the bot opened
		// must never be triaged by the bot.
		if r.BotUserID != 0 && is.Author.ID == r.BotUserID {
			continue
		}
		if hasLabelPrefix(is.Labels, prefix) {
			continue
		}
		// Belt and braces on the server-side bound. If `updated_after` is ever
		// dropped -- an older GitLab, a proxy that strips query params -- the
		// bound would silently vanish and the sweep would become the backfill it
		// must not be. A filter that only works when the server cooperates is
		// not a spend guard.
		if !is.UpdatedAt.IsZero() && is.UpdatedAt.Before(cutoff) {
			continue
		}
		r.Issues.Handle(ctx, &ghook.Event{
			Kind: ghook.KindIssue,
			Project: ghook.Project{
				ID:                entry.Project.ID,
				PathWithNamespace: entry.Project.PathWithNamespace,
				DefaultBranch:     entry.Project.DefaultBranch,
				WebURL:            entry.Project.WebURL,
			},
			// "open" because that is the only action Decide dispatches on, and a
			// swept issue IS an open issue nobody triaged. The synthesized event
			// carries no User: Decide does not read one for KindIssue, and the
			// bot-author check above is the guard that would have needed it.
			Issue: &ghook.Issue{
				IID:         is.IID,
				Action:      "open",
				Title:       is.Title,
				Description: is.Description,
			},
		})
		swept++
	}
	if swept > 0 {
		// Worth an INFO, not a debug: in steady state this never fires, because
		// the webhook got there first. When it does fire, events were being lost
		// and that is the thing an operator wants to see.
		r.log().Info("issue sweep dispatched issues the webhook path never handled",
			"project", p.PathWithNamespace, "count", swept, "open_issues", len(issues))
	}
	return swept, nil
}

// hasLabelPrefix reports whether any label sits in the broker's namespace.
// Case-insensitive, because a GitLab label is case-preserving but matched
// case-insensitively, so `Gonk::bug` and `gonk::bug` are one label.
func hasLabelPrefix(labels []string, prefix string) bool {
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}
