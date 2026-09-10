package intake

import (
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// Entry is gonk's derived view of one project. DERIVED CACHE ONLY (spec goal 6):
// nothing here is persisted, and a restart rebuilds all of it from GitLab and
// gonk-meter in one reconcile pass. The project -- not gonk -- is the source of
// truth for config; METER is the source of truth for what that config MEANS.
type Entry struct {
	Project        glab.Project
	Classification Classification
	LastReconcile  time.Time
	// LastMeterSync is the last time this project was actually PUT to meter (not
	// merely reconciled -- a pass that hits the config-hash short-circuit does
	// not update this). It backs Reconciler.MeterResyncInterval: the reconciler
	// skips re-registering an unchanged config, but must still resync
	// periodically, because meter's answer can change WITHOUT the project's
	// .gonk.yml moving (an operator flipping the instance kill switch, or
	// tightening a group ceiling).
	LastMeterSync time.Time
	// ScaffoldFiredAt suppresses re-firing the .agent/ scaffold order every
	// reconcile pass while the first one is still in flight. Best-effort only:
	// the real idempotency guarantee is the deterministic bead anchor, which the
	// Gas City controller must honour (see "Cross-plan contracts").
	ScaffoldFiredAt time.Time
	// TriageDispatchedAt is ScaffoldFiredAt's counterpart for the issue sweep
	// (sweepIssues): issue IID -> when the sweep last handed it to Dispatch and
	// got back a real fire. It exists for the same reason ScaffoldFiredAt does --
	// the sweep's OTHER guard, the broker's `gonk::` label, is best-effort and
	// asynchronous (IssueLabelPrefix's doc calls this out explicitly): the label
	// can take a reconcile pass or two to land, and every pass in between would
	// otherwise re-fire a triage order for the same issue. Best-effort only,
	// like ScaffoldFiredAt: the real idempotency guarantee is still the
	// deterministic bead anchor at Gate 1, so a duplicate handed to Dispatch
	// rejoins the existing reservation rather than double-spending -- this only
	// stops the noise of asking.
	TriageDispatchedAt map[int64]time.Time
	// Blocked marks an entry whose project is on the blocklist. The reconciler
	// normally prevents such an entry existing at all; this carries the fact to
	// Decide so the dispatch layer refuses independently rather than trusting
	// that prevention worked (gonk-jn5).
	Blocked bool
}

func (e Entry) State() State { return e.Classification.State }

// Dispatchable: work is dispatched only for a project whose policy METER HAS
// RESOLVED and whose virtual key EXISTS -- i.e. state `valid` or `pending`.
// Unmetered work is the one thing this system must never do.
//
// Note this is deliberately a whitelist, not a blacklist. A new state added
// later defaults to NOT dispatchable, which is the safe direction.
func (e Entry) Dispatchable() bool {
	switch e.Classification.State {
	case StateValid, StatePending:
		return true
	}
	return false
}

type Cache struct {
	mu sync.RWMutex
	m  map[int64]Entry
	// deregistered tombstones a project ID once reconcileProject's
	// archived-project branch has CONFIRMED, this process lifetime, that
	// meter holds no registration for it -- either this process DELETEd it,
	// or a MeterClient.Get found it already absent. Later passes consult it
	// to skip the meter call entirely for an archived project already known
	// gone, rather than repeating an idempotent-but-not-free DELETE every
	// pass forever.
	//
	// Deliberately NOT a field on Entry: an archived-and-deregistered
	// project has no classification, must never be Dispatchable, and must
	// never contribute a label to CountByState's metric domain (AllStates).
	// Keeping it out of m guarantees that structurally instead of relying on
	// every Entry consumer to remember to check a flag.
	//
	// DERIVED ONLY, same as m: it rebuilds empty on every restart,
	// reschedule, rollout or fresh replica. See reconcileProject's archived
	// branch for how the resulting cold start is handled -- it asks meter
	// directly (MeterClient.Get) rather than trusting an empty tombstone; an
	// empty tombstone must never by itself be read as "nothing to
	// deregister", or this reintroduces the hole T-12 closed.
	deregistered map[int64]struct{}
}

func NewCache() *Cache {
	return &Cache{m: make(map[int64]Entry), deregistered: make(map[int64]struct{})}
}

func (c *Cache) Get(id int64) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[id]
	return e, ok
}

// Put stores id's Entry. A project with a live Entry is, by definition, not a
// confirmed-deregistered archived project (the non-archived reconcile path is
// the only caller), so Put also clears any stale deregistered tombstone --
// otherwise a project that unarchives, re-registers, and later archives again
// would be skipped forever on the strength of a tombstone from its PREVIOUS
// archival.
func (c *Cache) Put(id int64, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = e
	delete(c.deregistered, id)
}

func (c *Cache) Delete(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
	delete(c.deregistered, id)
}

// WasDeregistered reports whether id's archived-project meter deregistration
// has already been confirmed this process lifetime.
func (c *Cache) WasDeregistered(id int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.deregistered[id]
	return ok
}

// MarkDeregistered tombstones id (see the deregistered field doc above) and
// drops any Entry for it -- an archived, deregistered project has no
// classification left to hold.
func (c *Cache) MarkDeregistered(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
	c.deregistered[id] = struct{}{}
}

// IDs is a snapshot of every cached project ID, used to find projects that
// vanished from the latest membership list (de-onboarding).
func (c *Cache) IDs() []int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := make([]int64, 0, len(c.m))
	for id := range c.m {
		ids = append(ids, id)
	}
	return ids
}

// CountByState powers the project-state gauge (spec 8). Pre-seed every state to
// zero so a dashboard shows 0 rather than nothing.
func (c *Cache) CountByState() map[State]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	counts := make(map[State]int, len(AllStates))
	for _, s := range AllStates {
		counts[s] = 0
	}
	for _, e := range c.m {
		counts[e.Classification.State]++
	}
	return counts
}
