# Plan 02: gitlab-intake (webhooks, reconciliation, onboarding MR)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `cmd/gonk-intake`: the GitLab trust boundary and the deterministic
onboarding path — verified webhook receipt, duplicate/bot-loop suppression,
membership+`.gonk.yml` reconciliation into a derived cache, the Renovate-style
onboarding MR, and dispatch of issue-triage / mention-reply / scaffold work.

**Architecture:** Reconciliation is the correctness path; webhooks are a latency
optimization (spec 5.2). Intake holds **no persistent state**: every project's
status is re-derived from GitLab each pass into an in-memory cache (spec goal 6).
Everything on the onboarding and reconciliation paths is deterministic — **zero
model calls anywhere in this plan**. The only agent work intake *causes* is an
order it fires at the Gas City supervisor (triage / mention-reply / `.agent/`
scaffold); intake never talks to an LLM and never posts triage comments itself
(the session does, spec 4.3 step 5).

**Tech Stack:** Go 1.26, stdlib `net/http` (no GitLab SDK — see "Dependency
decision" below), `github.com/prometheus/client_golang` (new), existing
`pkg/gonkcfg` + `pkg/atags` from Plan 01.

**Spec:** `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` sections 4.1,
4.3, 4.4, 5.1–5.5, 6.2, 8, 9, 10.2. **The spec is authoritative.**

**Contracts consumed (do not redefine them):**

- `gonkcfg.Load(raw []byte) (*ProjectConfig, error)` — schema-validates, then decodes.
  Intake calls this to answer one question, for its own onboarding UX and metrics:
  *do these bytes parse?* That is all.
- **`gonkcfg.Resolve` — intake does NOT call it.** See "Division of responsibility"
  below. **gonk-meter resolves config**, because meter is the only component that
  holds operator (instance/group) policy, and two independent resolvers would mean
  two sources of truth for a budget ceiling. Intake pushes the **raw `.gonk.yml`
  bytes** to meter and reads the resolved answer back.
- **`pkg/meterapi` — the intake↔meter wire contract. Owned by Plan 03 (Task 0).**
  Intake **imports** it. Intake does not restate a single field of it, and does not
  add one: a field intake needs that is not there is a change to *Plan 03's*
  contract, made *there*. `meterapi.Effective` (read off the wire, from meter) is
  where intake gets `.Enabled`, `.Actions`, `.Ladder`, `.Triage`, `.Provenance`,
  `.Schedule` — **never** from a local `Resolve`.
- `atags.Tags{...}`, `atags.TriggerIssueTriage|TriggerOnboarding|TriggerScaffold|TriggerMentionReply`.
  Intake does not *mint* atags (**meter does**, at `/policy/decide`), but the order it
  fires carries the fields the pack turns into atags — so intake enforces the
  boundary rules on them (PLAN.md carry-forward: no newline/comma in `Project`/`Rig`).

---

## Threat model (read before Task 1)

The webhook endpoint is the only thing gonk exposes to the ingress (spec 9), and
it fronts the component that decides whether a project may spend money. Two
inputs are hostile:

1. **The HTTP request.** Anything that can route to the ingress can POST to it.
   Verification happens **before the body is read**, so a forged request never
   causes an allocation. Bodies are size-capped. No secret material ever reaches
   a log line or an error string.
2. **`.gonk.yml`.** It is project-authored content: any user with push access to
   any project the bot is invited to controls those bytes. Plan 01 shipped a
   one-line remote crash (`monthly_cost_usd: .nan` SIGSEGV'd the validator, see
   ADR-002 "Untrusted input") precisely because this was underweighted. Intake
   therefore: size-caps the fetch **before** parsing, passes the bytes only to
   `gonkcfg.Load` (never to a bespoke parser), and treats every error as
   `invalid` (fail closed, no work dispatched) rather than as a reason to fall
   back to a default.

Fail-closed is the rule everywhere in this plan: unknown project, unsynced
budget key, unreachable meter, invalid config, disabled config, empty ladder →
**no dispatch**.

---

## Division of responsibility with gonk-meter (settled — do not re-open)

Plans 02 and 03 were written in parallel by authors who could not see each other, and **both** claimed config resolution and quiet hours. The controller settled both. This table is the contract.

| Responsibility | Owner |
|---|---|
| Fetching `.gonk.yml` from GitLab (size-capped; the bytes are hostile) | **intake** |
| Deciding "do these bytes parse?" (`gonkcfg.Load`) — for onboarding UX and metrics | **intake** |
| **Validating + resolving `.gonk.yml` into an `Effective` (`gonkcfg.Resolve`)** | **meter** |
| Operator instance/group policy (`pkg/opercfg`); folding **nested** GitLab groups | **meter** |
| Is this project enabled? What is its ladder? Its budget? Its actions? | **meter** (intake reads the answer) |
| **Quiet hours** (`schedule.quiet_hours`) | **meter** (as a `defer`) |
| Rung choice (`/policy/decide`) | **meter**, called by the **pack** |
| Budget enforcement (soft) | **meter** |
| Budget enforcement (hard) | **LiteLLM's virtual key**, provisioned by meter |
| Virtual-key provisioning | **meter** |
| Ladder / attempt state | **meter** |
| Attribution-tag minting | **meter** |
| GitLab state: member? `.gonk.yml` present? `.agent/` present? declined? | **intake** |
| Webhook receipt, verification, dedupe, bot-loop suppression | **intake** |
| The deterministic onboarding MR | **intake** |
| **Firing the order** at the Gas City supervisor | **intake** |
| Parking a **deferred** bead and retrying at `retry_after` | **pack** (Plan 04) |

**Why resolution moved to meter.** Meter is the only component that holds operator policy. If intake also resolved, a project's budget ceiling would have two independent derivations that could disagree — and the one that decides whether money may be spent is meter's. One resolver, one truth.

**Consequence: intake never sees a `defer`.** Intake does not call `/policy/decide` — the pack does, immediately before the session spawns (spec 6.2.3). So intake **fires the order unconditionally** (subject to its own GitLab-state gates), and if the project is inside its quiet hours, or out of budget, **meter defers the order downstream and the pack parks the bead.** Intake has no quiet-hours code, no `not_before`, and no deferral logic. That is not a gap — it is the whole point of putting the wait-vs-spend decision at the last possible moment, next to the spend.

---

## Open questions

Two of the original nine are **settled** by the controller (quiet hours → meter; secrets → file mounts with two rotation slots). Of the rest, each is either an **assumed default** the implementer must build, or a genuine **owner decision** needing a fact only the owner has.

### Assumed defaults (build these; revisit if wrong)

**AD-1 — One instance-wide webhook token, with two rotation slots.** Not per-project.
A per-project token forces us to **parse the untrusted body** to learn which project is claimed *before* we can pick a secret to verify against — auth-after-parse, on the one endpoint exposed to the ingress. The instance-wide token verifies **from a header alone, before a single byte of body is read** (Task 3, `TestBodyIsNotReadBeforeVerification`).
*Blast radius if wrong:* a project Maintainer who can read their own hook's config learns the token for **every** project's hook, and can forge webhooks for any of them. That is bounded by what a forged webhook can *do*: it is a latency optimization only (reconciliation is the correctness path, spec 5.2), and any work it triggers still has to pass meter's `/decide`. It cannot mint spend. If the owner rules otherwise, Task 3 changes shape and verification moves after the parse — accept that cost knowingly.

**AD-2 — A previously-valid project whose `.gonk.yml` becomes malformed: fail closed, silently, with a metric.**
The project goes `invalid`, no work is dispatched, and meter (which now owns validation) disables the LiteLLM key on the 422 — a broken config cannot keep spending. There is **no last-known-good fallback**: that would let an attacker's bad edit silently preserve a permissive budget.
The *notification* half is the actual question: (a) stay silent and move a metric, (b) open one idempotent `gonk::config-error` issue, or (c) comment on the MR/commit that broke it.
*Assumed default: (a), silent + metric* — (b) and (c) are bot **writes**, and each needs its own idempotency design or it becomes a spam generator.
*Blast radius if wrong:* **a project goes dark and nobody notices.** This is the assumed default I am least comfortable with. Mitigation, which Plan 05 must ship: an alert rule on `gonk_intake_projects{state="invalid"} > 0`. If the owner wants (b), it is a small task on top of the existing `requestAccess` idempotency pattern (Task 8) — that pattern already exists and works.

**AD-3 — Decline and re-invite are derived from GitLab, never persisted.**
A closed-unmerged onboarding MR is a decline (spec 5.3). A re-invite is `bot member.created_at > onboarding MR closed_at` — both read from GitLab, so decline stays derived state with nothing stored on gonk's side (spec goal 6).
*Blast radius if wrong:* **this depends on GitLab CE's member payload actually carrying `created_at`, which is unverified** (a **Plan 06** item). If it does not, decline becomes permanent until a human deletes the closed MR or pushes a `.gonk.yml`. That is a degraded but safe fallback, and it is what we ship if Plan 06 says the field is absent.

**AD-4 — Split-credential mode: the seam exists; `bot-does-everything` is the default.**
`GONK_GITLAB_TOKEN_FILE` plus an optional `GONK_GITLAB_ADMIN_TOKEN_FILE` used **only** for `/hooks` calls (spec 5.1). Unset → the bot token does everything (the homelab default).
*Blast radius if wrong:* none to the code — the seam is already there (`request.admin` in `pkg/glab`). If split-credential must be the *default*, it is a chart change (Plan 05), not a Go change.

**AD-5 — Intake's pre-dispatch gate uses meter's `Effective.Actions`, but is NOT the enforcement point.**
Meter enforces the action veto at `/decide` (it is the only chokepoint before a session spawns). Intake checks `Actions.Triage` too — purely to **avoid firing an order that will certainly be denied**, which would churn a bead for nothing. `triage.respond_to_mentions` is different: **meter does not check it**, so intake is its only enforcement point.
*Blast radius if wrong:* if intake's pre-filter is too strict, work is silently dropped that meter would have allowed — so the pre-filter must be a **subset** of meter's rules, never a superset. Task 9 asserts this. If it is too loose, the only cost is a denied bead.

### Owner decision needed (a fact only you have)

**OD-A — The Gas City supervisor order API.** Spec 4.1 says the controller exposes a "supervisor REST API + SSE event bus"; nothing in the spec gives the shape for *firing an order*.
*Built (Task 9):* a `Dispatcher` interface with (i) a `LogDispatcher` that structured-logs the order — so intake runs and is fully testable today — and (ii) an `HTTPDispatcher` that POSTs our `OrderRequest` JSON to a configurable URL.
*Needed:* the real endpoint and payload. **Plan 04 must reconcile `HTTPDispatcher` with Gas City's actual contract.** Until then this is a placeholder and the plan says so.

**OD-B — The canonical local rung name.** The onboarding template ships `ladder: [qwen-local]` (spec 5.4's example). That string must **exactly** match a LiteLLM model name **and** appear in the instance ladder, or ADR-002's empty-ladder rule disables **every freshly-onboarded project**. Is `qwen-local` the real name?
(Same question as Plan 03's OD-D — one answer serves both. Carry-forward to **Plan 05**: the chart's default instance ladder **must contain it**; `opercfg.Load` now refuses to start with an empty instance ladder, which closes the fail-open PLAN.md flagged.)

---

## The gonk-meter seam

**The contract is `pkg/meterapi`. Plan 03 owns it. This plan imports it and restates nothing.**

The normative Go source is **Plan 03, Task 0** (`docs/superpowers/plans/2026-07-13-plan-03-gonk-meter.md`). Read it before writing Task 6 or Task 7. A sha256 drift gate guards it: **if you find you need a field that is not there, that is a change to Plan 03's contract and it gets made there, not here.**

> **Sequencing.** Plan 02 executes *before* Plan 03, so this plan physically lands `pkg/meterapi` first, by copying Plan 03 Task 0's source **verbatim** (Task 6). That does not transfer ownership. Plan 03's Task 0 then verifies the shipped package is byte-identical.

### What intake calls, and what it does not

Intake calls **exactly four** endpoints:

```
PUT    /v1/projects/{project}     push RAW .gonk.yml; meter validates + resolves + provisions the key
GET    /v1/projects/{project}     read back state / effective / budget          (readiness, admin)
DELETE /v1/projects/{project}     de-onboard: disable the project, delete the key
GET    /healthz                   readiness gate
```

`{project}` is the GitLab `path_with_namespace`, URL-path-escaped. Use `meterapi.ProjectPath(project)`; never hand-build it.

**Intake does NOT call `/v1/policy/decide`.** Spec 6.2.3 puts the rung decision in the *dispatch formula* (the pack, Plan 04), immediately before the session spawns. Intake never touches it, never sends an attempt count (`meterapi.DecideRequest` has no such field, deliberately — it is a forgery vector for climbing the ladder), and **never sees a `defer`**.

### Intake pushes RAW config. Meter resolves.

This is the settled half of **Conflict A**. `PUT /v1/projects/{project}` carries `meterapi.ProjectRequest`: the `.gonk.yml` **bytes intake read from GitLab, verbatim**, plus the GitLab metadata meter cannot know (project path, project id, rig, default branch, config commit sha).

Meter validates them, folds instance + (nested-group-folded) group policy over them, calls `gonkcfg.Resolve` — **the only call to `Resolve` in the entire system** — and returns the resolved `meterapi.ProjectResponse`.

**Three response codes, and intake must handle each distinctly:**

| Code | Meaning | Intake's reaction |
|---|---|---|
| **200** | Resolved. `state` ∈ `active` \| `disabled` \| `key-missing`; `effective` is populated. | Record the state and the `Effective`. **Dispatchable only when `state == active`.** |
| **422** | The **project's** `.gonk.yml` will not load. A *successful, idempotent registration of an invalid config*: meter recorded `state: invalid` and **deleted the virtual key**. `error` carries `gonkcfg.Load`'s message verbatim. | Record `invalid`. Dispatch nothing. Echo `error` into the MR/issue comment. **This is not intake's bug.** |
| **400** | The **request** is malformed — missing `project_id`, an attribution-unsafe path, an oversized body. Nothing recorded. | **This IS intake's bug.** Log loudly, metric it, do not retry blindly. |

Collapsing 422 into 400 is the mistake to avoid: it makes intake unable to tell its own bug from a project's bad YAML.

### Hard rules on this seam

- **Intake never computes an `Effective`.** Enabled? Ladder? Budget? Actions? All of it comes off the wire in `ProjectResponse.Effective`. ADR-002 is explicit that `Resolve`'s return value is the only legitimate way to produce an `Effective`, and intake no longer calls `Resolve`.
- **`effective` is `null` iff `state == invalid`.** An invalid config has *no* `Effective` — not a zero one. Intake must not synthesize one.
- **`null` in a budget means UNLIMITED.** `meterapi.Budget` uses `*float64`/`*int64`. Never send or interpret an empty `Budget{}` for a project you are disabling — all-nil means *unlimited*, the exact opposite of fail-closed. (`+Inf` is not JSON-serializable at all; `json.Marshal` **returns an error**. That is PLAN.md's highest-value carry-forward and `meterapi` is where it is solved, once.)
- **`key_ref` is a pointer, never key material.** Intake logs this struct.
- **Meter is idempotent on `config_hash`.** Intake may PUT the same bytes every reconcile pass (every 10 minutes, per project); meter no-ops.
- **Failure semantics — no unmetered work, ever.** If the PUT fails, or meter is unreachable, or the response is anything but `200 / state: active`, intake marks the project **not dispatchable** and **dispatches nothing for it**, retrying next pass.
- **De-onboarding is a `DELETE`**, not a `PUT` with `enabled: false`. It is idempotent; deleting an unknown project is `204`.
- **Auth:** `Authorization: Bearer <token>`, read from a **file** (`GONK_METER_TOKEN_FILE`, plus an optional previous-slot file). Never an env value.

---

## Dependency decision: no GitLab SDK

We write a ~300-line typed client (`pkg/glab`) over `net/http` instead of pulling
in a GitLab SDK. Reasons: intake uses ~14 endpoints; an SDK is a large
attack/compat surface for a component on the trust boundary; and a hand-rolled
client lets us own the two things that actually matter here — response size caps
and retry/backoff semantics. The fake GitLab (`pkg/glab/glabtest`) is what makes
everything else in this plan unit-testable with zero infrastructure.

---

## File structure

```
pkg/glab/
  client.go          HTTP plumbing: auth header, do(), pagination, retry, size caps, APIError
  types.go           User, Project, Hook, Branch, MergeRequest, Issue, Member, Commit*
  projects.go        CurrentUser, ListMemberProjects, GetRawFile, DirExists, ListMembers
  hooks.go           ListHooks, CreateHook, EditHook
  write.go           CreateBranch, CreateCommit, CreateMergeRequest, ListMergeRequests,
                     ListIssues, CreateIssue
  *_test.go
pkg/glab/glabtest/
  server.go          in-memory GitLab (httptest) — the test substrate for the whole plan
  server_test.go
pkg/ghook/
  verify.go          X-Gitlab-Token verification: rotation slots, constant time
  receiver.go        hardened HTTP handler: method/content-type/size/allow-list, Observer
  event.go           minimal typed event parse + validation
  dedupe.go          bounded TTL dedupe cache
  testdata/*.json    golden webhook payloads (REFRESH FROM REAL GITLAB IN PLAN 06)
  *_test.go
pkg/meterapi/
  meterapi.go        *** NOT DEFINED HERE. Landed verbatim from Plan 03 Task 0. ***
  meterapi_test.go   Plan 03 owns this contract; Task 6 copies it and does not edit it.
pkg/intake/
  state.go           (Observation, meter's ProjectResponse) -> Classification. PURE. No Resolve.
  cache.go           derived-only project cache (no persistence)
  reconcile.go       memberships -> observe -> PUT raw config to meter -> classify -> hooks
  onboard.go         onboarding MR: branch, commit, MR, idempotency, decline, role fallback
  render.go          deterministic .gonk.yml default + MR body (golden + anti-drift gate)
  dispatch.go        gating (pure Decide) + Dispatcher seam + mentions
                     NO QUIET HOURS -- meter owns them, as a defer (Conflict B).
  meterclient.go     PUT/GET/DELETE /v1/projects/{project}, bearer auth from a file
  server.go          public listener (hook only) + private listener (health/metrics/admin)
  metrics.go         prometheus Observer impls
  testdata/onboarding-mr.golden.md
  *_test.go
cmd/gonk-intake/
  main.go            env config (secrets from files), wiring, reconcile loop, shutdown
docs/adr/ADR-003-intake-trust-boundary-and-seams.md
```

---

### Task 1: `pkg/glab` — typed GitLab REST client

**Files:** Create `pkg/glab/client.go`, `pkg/glab/types.go`, `pkg/glab/projects.go`,
`pkg/glab/hooks.go`, `pkg/glab/write.go`, `pkg/glab/client_test.go`.

- [ ] **Step 1: Write the failing test** — `pkg/glab/client_test.go`

```go
package glab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "s3cret")
	c.RetryBackoff = func(int) time.Duration { return 0 } // no sleeping in tests
	return c
}

func TestSendsPrivateToken(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "s3cret" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
	}))
	u, err := c.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser = %v", err)
	}
	if u.ID != 7 || u.Username != "gonk" {
		t.Fatalf("user = %+v", u)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
	}))
	_, err := c.GetRawFile(context.Background(), 1, ".gonk.yml", "main", 1024)
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want IsNotFound", err)
	}
}

// An error must never carry the token: errors get logged.
func TestErrorDoesNotLeakToken(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	c.MaxRetries = 0
	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"id":7,"username":"gonk"}`)
	}))
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatalf("CurrentUser = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	if _, err := c.CurrentUser(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (4xx must not retry)", calls)
	}
}

func TestPaginatesMemberProjects(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"id":1,"path_with_namespace":"a/b","default_branch":"main"}]`)
		default:
			w.Header().Set("X-Next-Page", "")
			fmt.Fprint(w, `[{"id":2,"path_with_namespace":"c/d","default_branch":"main"}]`)
		}
	}))
	ps, err := c.ListMemberProjects(context.Background())
	if err != nil {
		t.Fatalf("ListMemberProjects = %v", err)
	}
	if len(ps) != 2 || ps[1].ID != 2 {
		t.Fatalf("projects = %+v", ps)
	}
}

// .gonk.yml is attacker-controlled: a project can commit a 2 GiB file. The
// client must refuse to read past the cap rather than OOM the budget enforcer.
func TestGetRawFileRefusesOversizeBody(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 100 {
			fmt.Fprint(w, strings.Repeat("x", 1024))
		}
	}))
	_, err := c.GetRawFile(context.Background(), 1, ".gonk.yml", "main", 4096)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want 'too large'", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/glab/ -v`
Expected: FAIL — `no Go files` / undefined `New`, `Client`, `CurrentUser`, …

- [ ] **Step 3: Implement `pkg/glab/client.go`**

```go
// Package glab is a minimal typed client for the subset of the GitLab REST API
// that gonk-intake uses. It is deliberately not a full SDK: the client is on
// the trust boundary, so it owns its own response size caps, retry policy, and
// error type, and it never logs or embeds the private token.
package glab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxBytes caps any single response body. GitLab is semi-trusted, but a
// project's repository content is not.
const DefaultMaxBytes int64 = 1 << 20

type Client struct {
	BaseURL string // e.g. https://gitlab.orac.local
	token   string
	HTTP    *http.Client
	// AdminToken, when non-empty, is used ONLY for webhook management
	// (split-credential mode, spec 5.1). Empty means bot-does-everything.
	AdminToken   string
	MaxBytes     int64
	MaxRetries   int
	RetryBackoff func(attempt int) time.Duration
	UserAgent    string
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:      strings.TrimSuffix(baseURL, "/"),
		token:        token,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
		MaxBytes:     DefaultMaxBytes,
		MaxRetries:   3,
		RetryBackoff: func(a int) time.Duration { return time.Duration(1<<a) * 250 * time.Millisecond },
		UserAgent:    "gonk-intake",
	}
}

// APIError is any non-2xx response. It carries the status and a truncated body
// for diagnosis, and never the token.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized)
}

type request struct {
	method   string
	path     string // begins with /api/v4
	query    map[string]string
	body     any    // JSON-encoded when non-nil
	admin    bool   // use AdminToken when set (hook management)
	maxBytes int64  // 0 -> Client.MaxBytes
}

// do sends one request with retries on 429 and 5xx. It returns the raw body and
// the response headers (pagination lives in X-Next-Page).
func (c *Client) do(ctx context.Context, rq request) ([]byte, http.Header, error) {
	limit := rq.maxBytes
	if limit == 0 {
		limit = c.MaxBytes
	}
	var payload []byte
	if rq.body != nil {
		var err error
		if payload, err = json.Marshal(rq.body); err != nil {
			return nil, nil, fmt.Errorf("gitlab: encode %s %s: %w", rq.method, rq.path, err)
		}
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, rq.method, c.BaseURL+rq.path, bytes.NewReader(payload))
		if err != nil {
			return nil, nil, err
		}
		if len(rq.query) > 0 {
			q := req.URL.Query()
			for k, v := range rq.query {
				q.Set(k, v)
			}
			req.URL.RawQuery = q.Encode()
		}
		tok := c.token
		if rq.admin && c.AdminToken != "" {
			tok = c.AdminToken
		}
		req.Header.Set("PRIVATE-TOKEN", tok)
		req.Header.Set("User-Agent", c.UserAgent)
		if rq.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, err)
		} else {
			body, rerr := readCapped(resp.Body, limit)
			_ = resp.Body.Close()
			switch {
			case rerr != nil:
				return nil, nil, fmt.Errorf("gitlab: %s %s: %w", rq.method, rq.path, rerr)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return body, resp.Header, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				lastErr = &APIError{Status: resp.StatusCode, Method: rq.method, Path: rq.path, Body: truncate(body)}
			default:
				// 4xx other than 429: retrying cannot help.
				return nil, nil, &APIError{Status: resp.StatusCode, Method: rq.method, Path: rq.path, Body: truncate(body)}
			}
		}
		if attempt >= c.MaxRetries {
			return nil, nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(c.RetryBackoff(attempt)):
		}
	}
}

// readCapped reads at most limit bytes and errors if the body is longer, rather
// than silently truncating (a truncated .gonk.yml could parse as a *different,
// valid* config).
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response too large (> %d bytes)", limit)
	}
	return b, nil
}

func truncate(b []byte) string {
	const n = 256
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *Client) getJSON(ctx context.Context, rq request, out any) error {
	body, _, err := c.do(ctx, rq)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gitlab: %s %s: decode: %w", rq.method, rq.path, err)
	}
	return nil
}

// paginate walks X-Next-Page, appending each page's decoded items.
func paginate[T any](ctx context.Context, c *Client, rq request) ([]T, error) {
	var all []T
	page := "1"
	for {
		q := map[string]string{"per_page": "100", "page": page}
		for k, v := range rq.query {
			q[k] = v
		}
		body, hdr, err := c.do(ctx, request{method: rq.method, path: rq.path, query: q, admin: rq.admin})
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("gitlab: %s %s: decode: %w", rq.method, rq.path, err)
		}
		all = append(all, items...)
		next := hdr.Get("X-Next-Page")
		if next == "" || next == page {
			return all, nil
		}
		if _, err := strconv.Atoi(next); err != nil {
			return all, nil
		}
		page = next
	}
}
```

- [ ] **Step 4: Implement `pkg/glab/types.go`**

```go
package glab

import "time"

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type Project struct {
	ID                int64       `json:"id"`
	PathWithNamespace string      `json:"path_with_namespace"`
	DefaultBranch     string      `json:"default_branch"`
	WebURL            string      `json:"web_url"`
	Archived          bool        `json:"archived"`
	Permissions       Permissions `json:"permissions"`
}

type Permissions struct {
	ProjectAccess *Access `json:"project_access"`
	GroupAccess   *Access `json:"group_access"`
}

type Access struct {
	AccessLevel int `json:"access_level"`
}

// GitLab access levels.
const (
	AccessGuest      = 10
	AccessReporter   = 20
	AccessDeveloper  = 30
	AccessMaintainer = 40
	AccessOwner      = 50
)

// EffectiveAccess is the higher of the bot's direct project access and its
// inherited group access. A bot invited at the group level has no
// project_access at all, so reading only project_access would misreport it as
// having no rights and open a spurious "I need Developer" issue.
func (p Project) EffectiveAccess() int {
	lvl := 0
	if p.Permissions.ProjectAccess != nil {
		lvl = p.Permissions.ProjectAccess.AccessLevel
	}
	if g := p.Permissions.GroupAccess; g != nil && g.AccessLevel > lvl {
		lvl = g.AccessLevel
	}
	return lvl
}

type Hook struct {
	ID                    int64  `json:"id"`
	URL                   string `json:"url"`
	IssuesEvents          bool   `json:"issues_events"`
	NoteEvents            bool   `json:"note_events"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	PushEvents            bool   `json:"push_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

// HookOptions is the create/edit payload. Token is write-only: GitLab never
// returns it, which is why hook token freshness is tracked by a generation
// marker in the URL instead (see ADR-003).
type HookOptions struct {
	URL                   string `json:"url"`
	Token                 string `json:"token,omitempty"`
	IssuesEvents          bool   `json:"issues_events"`
	NoteEvents            bool   `json:"note_events"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	PushEvents            bool   `json:"push_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

type Member struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	AccessLevel int        `json:"access_level"`
	CreatedAt   *time.Time `json:"created_at"`
}

type MergeRequest struct {
	IID          int64      `json:"iid"`
	Title        string     `json:"title"`
	State        string     `json:"state"` // opened | closed | merged | locked
	SourceBranch string     `json:"source_branch"`
	TargetBranch string     `json:"target_branch"`
	WebURL       string     `json:"web_url"`
	UpdatedAt    *time.Time `json:"updated_at"`
	MergedAt     *time.Time `json:"merged_at"`
	ClosedAt     *time.Time `json:"closed_at"`
}

type MRListOptions struct {
	SourceBranch string
	State        string // opened | closed | merged | all
}

type MROptions struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	AssigneeID         int64  `json:"assignee_id,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
}

type Issue struct {
	IID    int64    `json:"iid"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	WebURL string   `json:"web_url"`
}

type IssueOptions struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Labels      string `json:"labels,omitempty"` // comma-separated, per the API
}

type IssueListOptions struct {
	State  string
	Labels string
}

type CommitAction struct {
	Action   string `json:"action"` // create | update
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type CommitOptions struct {
	Branch        string         `json:"branch"`
	StartBranch   string         `json:"start_branch,omitempty"`
	CommitMessage string         `json:"commit_message"`
	Actions       []CommitAction `json:"actions"`
}

type Commit struct {
	ID string `json:"id"`
}

type Branch struct {
	Name string `json:"name"`
}
```

- [ ] **Step 5: Implement the endpoint methods**

`pkg/glab/projects.go`:

```go
package glab

import (
	"context"
	"fmt"
	"net/url"
)

func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var u User
	if err := c.getJSON(ctx, request{method: "GET", path: "/api/v4/user"}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListMemberProjects returns every project the token's user is a member of
// (spec 5.2). This is the entry point of reconciliation.
func (c *Client) ListMemberProjects(ctx context.Context) ([]Project, error) {
	return paginate[Project](ctx, c, request{
		method: "GET",
		path:   "/api/v4/projects",
		query:  map[string]string{"membership": "true", "order_by": "id", "sort": "asc"},
	})
}

// GetRawFile fetches a file's bytes at a ref. maxBytes is enforced by the
// client: the caller passes the .gonk.yml cap so a hostile 2 GiB file cannot be
// read into memory. Returns an error satisfying IsNotFound when absent.
func (c *Client) GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error) {
	p := fmt.Sprintf("/api/v4/projects/%d/repository/files/%s/raw", projectID, url.PathEscape(path))
	body, _, err := c.do(ctx, request{method: "GET", path: p, query: map[string]string{"ref": ref}, maxBytes: maxBytes})
	return body, err
}

// DirExists reports whether a directory exists at ref (used for .agent/).
func (c *Client) DirExists(ctx context.Context, projectID int64, path, ref string) (bool, error) {
	var entries []struct {
		Name string `json:"name"`
	}
	err := c.getJSON(ctx, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/repository/tree", projectID),
		query:  map[string]string{"path": path, "ref": ref, "per_page": "1"},
	}, &entries)
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func (c *Client) ListMembers(ctx context.Context, projectID int64) ([]Member, error) {
	return paginate[Member](ctx, c, request{
		method: "GET",
		path:   fmt.Sprintf("/api/v4/projects/%d/members/all", projectID),
	})
}
```

Implementer note on `url.PathEscape`: `.gonk.yml` contains no `/`, so this is a
plain escape. If a future caller passes a nested path, Go sends the `%2F` form
via `URL.RawPath`, which is what GitLab wants — `TestGetRawFileEscapesNestedPath`
in `glabtest` (Task 2) proves it. Do not "simplify" this to string concatenation.

`pkg/glab/hooks.go` — `ListHooks` (GET `/api/v4/projects/{id}/hooks`),
`CreateHook` (POST same path, body `HookOptions`), `EditHook` (PUT
`/api/v4/projects/{id}/hooks/{hook_id}`, body `HookOptions`). All three set
`admin: true` on the `request` so split-credential mode (AD-4) uses the admin
token for hook management only.

`pkg/glab/write.go` — the same one-line-per-method shape:

| Method | HTTP | Path | Body / query |
|---|---|---|---|
| `CreateBranch(ctx, id, branch, ref) (*Branch, error)` | POST | `/api/v4/projects/{id}/repository/branches` | query `branch`, `ref` |
| `CreateCommit(ctx, id, CommitOptions) (*Commit, error)` | POST | `/api/v4/projects/{id}/repository/commits` | JSON body |
| `ListMergeRequests(ctx, id, MRListOptions) ([]MergeRequest, error)` | GET | `/api/v4/projects/{id}/merge_requests` | query `source_branch`, `state` (paginated) |
| `CreateMergeRequest(ctx, id, MROptions) (*MergeRequest, error)` | POST | `/api/v4/projects/{id}/merge_requests` | JSON body |
| `ListIssues(ctx, id, IssueListOptions) ([]Issue, error)` | GET | `/api/v4/projects/{id}/issues` | query `state`, `labels` (paginated) |
| `CreateIssue(ctx, id, IssueOptions) (*Issue, error)` | POST | `/api/v4/projects/{id}/issues` | JSON body |

Each is `getJSON` (or `paginate`) over `request{...}` exactly like `CurrentUser`
/ `ListMemberProjects` above. No retries beyond the shared policy; no logging.

- [ ] **Step 6: Run the tests**

Run: `go test ./pkg/glab/ -race -count=1 -v`
Expected: PASS (7 tests).

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./pkg/glab/ -race -count=1
git add pkg/glab && git commit -m "feat(glab): typed GitLab REST client with size caps, retries, typed errors"
```

---

### Task 2: `pkg/glab/glabtest` — the in-memory fake GitLab

This is the substrate the rest of the plan is tested against. **Nothing in Plans
02 requires a live GitLab.** (What does is listed under "Plan 06 e2e items" at
the end of this document.)

**Files:** Create `pkg/glab/glabtest/server.go`, `pkg/glab/glabtest/server_test.go`.

- [ ] **Step 1: Write the failing test** — `pkg/glab/glabtest/server_test.go`

```go
package glabtest_test

import (
	"context"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func TestFakeDrivesTheRealClient(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\n"))

	c := gl.Client()
	ctx := context.Background()

	if u, err := c.CurrentUser(ctx); err != nil || u.Username != "gonk" {
		t.Fatalf("CurrentUser = %+v, %v", u, err)
	}
	ps, err := c.ListMemberProjects(ctx)
	if err != nil || len(ps) != 1 || ps[0].EffectiveAccess() != glab.AccessMaintainer {
		t.Fatalf("ListMemberProjects = %+v, %v", ps, err)
	}
	raw, err := c.GetRawFile(ctx, ps[0].ID, ".gonk.yml", "main", 65536)
	if err != nil || string(raw) != "version: 1\nenabled: true\n" {
		t.Fatalf("GetRawFile = %q, %v", raw, err)
	}
	if _, err := c.GetRawFile(ctx, ps[0].ID, "nope.yml", "main", 65536); !glab.IsNotFound(err) {
		t.Fatalf("missing file err = %v, want IsNotFound", err)
	}
}

func TestFakeRejectsBadToken(t *testing.T) {
	gl := glabtest.New(t)
	c := glab.New(gl.URL(), "wrong-token")
	if _, err := c.CurrentUser(context.Background()); !glab.IsForbidden(err) {
		t.Fatalf("err = %v, want 401/403", err)
	}
}

func TestFakeHookLifecycle(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	c, ctx := gl.Client(), context.Background()

	h, err := c.CreateHook(ctx, p.ID, glab.HookOptions{URL: "https://gonk/hook/gitlab?gen=1", Token: "t", IssuesEvents: true})
	if err != nil {
		t.Fatalf("CreateHook = %v", err)
	}
	hooks, err := c.ListHooks(ctx, p.ID)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("ListHooks = %+v, %v", hooks, err)
	}
	if hooks[0].URL == "" || hooks[0].ID != h.ID {
		t.Fatalf("hook = %+v", hooks[0])
	}
	// GitLab never returns the token. The fake must not either, or a test could
	// pass against a behaviour production does not have.
	if got := gl.HookToken(p.ID, h.ID); got != "t" {
		t.Fatalf("stored token = %q", got)
	}
	if _, err := c.EditHook(ctx, p.ID, h.ID, glab.HookOptions{URL: "https://gonk/hook/gitlab?gen=2", Token: "t2", IssuesEvents: true}); err != nil {
		t.Fatalf("EditHook = %v", err)
	}
	if got := gl.HookToken(p.ID, h.ID); got != "t2" {
		t.Fatalf("token after edit = %q", got)
	}
}

func TestFakeBranchCommitMR(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	c, ctx := gl.Client(), context.Background()

	if _, err := c.CreateBranch(ctx, p.ID, "gonk/onboard", "main"); err != nil {
		t.Fatalf("CreateBranch = %v", err)
	}
	if _, err := c.CreateBranch(ctx, p.ID, "gonk/onboard", "main"); err == nil {
		t.Fatal("second CreateBranch must fail (branch exists)")
	}
	if _, err := c.CreateCommit(ctx, p.ID, glab.CommitOptions{
		Branch:        "gonk/onboard",
		CommitMessage: "chore: add .gonk.yml",
		Actions:       []glab.CommitAction{{Action: "create", FilePath: ".gonk.yml", Content: "version: 1\n"}},
	}); err != nil {
		t.Fatalf("CreateCommit = %v", err)
	}
	mr, err := c.CreateMergeRequest(ctx, p.ID, glab.MROptions{
		SourceBranch: "gonk/onboard", TargetBranch: "main", Title: "gonk onboarding",
	})
	if err != nil {
		t.Fatalf("CreateMergeRequest = %v", err)
	}
	mrs, err := c.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: "gonk/onboard", State: "all"})
	if err != nil || len(mrs) != 1 || mrs[0].IID != mr.IID || mrs[0].State != "opened" {
		t.Fatalf("ListMergeRequests = %+v, %v", mrs, err)
	}
	// Branch content is visible on the branch, not on main: the onboarding MR
	// must not make the project look already-onboarded.
	if _, err := c.GetRawFile(ctx, p.ID, ".gonk.yml", "main", 4096); !glab.IsNotFound(err) {
		t.Fatalf("main must not have .gonk.yml yet: %v", err)
	}
	if _, err := c.GetRawFile(ctx, p.ID, ".gonk.yml", "gonk/onboard", 4096); err != nil {
		t.Fatalf("branch must have .gonk.yml: %v", err)
	}
}

func TestFakeCanInjectFailures(t *testing.T) {
	gl := glabtest.New(t)
	gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.FailNext("GET", "/api/v4/projects", 500, 2) // fail twice, then succeed
	c := gl.Client()
	if _, err := c.ListMemberProjects(context.Background()); err != nil {
		t.Fatalf("client should have retried through the 500s: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/glab/glabtest/ -v`
Expected: FAIL — package `glabtest` does not exist.

- [ ] **Step 3: Implement `pkg/glab/glabtest/server.go`**

Behaviour contract (the tests above are the spec; this is the shape):

```go
// Package glabtest is an in-memory GitLab REST API: enough of it to drive
// gonk-intake's tests with no network, no containers, and no live GitLab.
// Fidelity notes that matter:
//   - the private token is required on every request (401 otherwise);
//   - hook tokens are stored but never returned (GitLab does not return them);
//   - files are per-branch, so an onboarding MR does not make main look onboarded;
//   - CreateBranch on an existing branch is a 400, like the real API.
package glabtest

type Server struct {
	Me glab.User

	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	token    string
	projects map[int64]*Project
	nextID   int64
	fails    []failure // FailNext queue
}

type Project struct {
	glab.Project
	Files    map[string]map[string][]byte // branch -> path -> content
	Branches map[string]bool
	Hooks    []glab.Hook
	MRs      []glab.MergeRequest
	Issues   []glab.Issue
	Members  []glab.Member

	hookTokens map[int64]string
}

func New(t *testing.T) *Server                                   // starts httptest.Server, t.Cleanup closes it
func (s *Server) URL() string
func (s *Server) Client() *glab.Client                           // real glab.Client, zero retry backoff
func (s *Server) AddProject(path string, access int) *Project    // default_branch "main", bot member at access
func (p *Project) PutFile(path string, content []byte)           // on the default branch
func (p *Project) PutFileOn(branch, path string, content []byte)
func (s *Server) HookToken(projectID, hookID int64) string       // test-only introspection
func (s *Server) FailNext(method, pathPrefix string, status, times int)
func (s *Server) AddMember(projectID int64, m glab.Member)
func (s *Server) SetMRState(projectID, iid int64, state string, at time.Time) // merged / closed
func (s *Server) Requests() []string                             // "METHOD /path" log, for asserting idempotency
```

Router: one `http.ServeMux`-free handler that (1) checks `PRIVATE-TOKEN` → 401,
(2) consults the `FailNext` queue → injected status, (3) matches
`METHOD /api/v4/...` with `strings.Split` on the path, and (4) writes JSON. Keep
it a single `switch` — clarity over cleverness. Pagination: honour `page`, set
`X-Next-Page` when more remain (`ListMemberProjects` must exercise this: give
`AddProject` an id sequence and page at 100).

`Requests()` is not decoration: several later tasks assert *idempotency* by
running reconcile twice and requiring zero writes on the second pass.

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/glab/... -race -count=1 -v`
Expected: PASS (all `glabtest` tests and Task 1's client tests).

- [ ] **Step 5: Add the nested-path escape test** (proves the `%2F` handling)

```go
func TestGetRawFileEscapesNestedPath(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".agent/context.md", []byte("hi"))
	b, err := gl.Client().GetRawFile(context.Background(), p.ID, ".agent/context.md", "main", 4096)
	if err != nil || string(b) != "hi" {
		t.Fatalf("GetRawFile = %q, %v", b, err)
	}
}
```

Run: `go test ./pkg/glab/glabtest/ -run Escapes -v` → PASS. If it fails with a
404 whose path shows a literal `/`, the client must build the URL with
`u.Opaque` instead — fix in `glab`, not here.

- [ ] **Step 6: Commit**

```bash
gofmt -l . && go test ./pkg/glab/... -race -count=1
git add pkg/glab/glabtest && git commit -m "test(glab): in-memory fake GitLab server with failure injection"
```

---

### Task 3: `pkg/ghook` — token verification and the hardened receiver

**Files:** Create `pkg/ghook/verify.go`, `pkg/ghook/receiver.go`,
`pkg/ghook/verify_test.go`, `pkg/ghook/receiver_test.go`.

Spec 4.4.1: GitLab authenticates webhooks with a **constant `X-Gitlab-Token`
header**, not an HMAC signature. There is no body-derived MAC to check, so the
token *is* the entire trust boundary: it must be long, compared in constant time,
and never logged.

- [ ] **Step 1: Write the failing verifier test** — `pkg/ghook/verify_test.go`

```go
package ghook

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	secretA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 36 chars
	secretB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func req(token string) *httptest.ResponseRecorder { panic("unused") } // placeholder removed below

func TestVerifierAcceptsEitherRotationSlot(t *testing.T) {
	v, err := NewVerifier(secretA, secretB)
	if err != nil {
		t.Fatalf("NewVerifier = %v", err)
	}
	for _, tok := range []string{secretA, secretB} {
		r := httptest.NewRequest("POST", "/hook/gitlab", nil)
		r.Header.Set("X-Gitlab-Token", tok)
		if err := v.Verify(r); err != nil {
			t.Errorf("Verify(%q…) = %v, want nil", tok[:4], err)
		}
	}
}

func TestVerifierRejects(t *testing.T) {
	v, _ := NewVerifier(secretA)
	cases := map[string]string{
		"missing":      "",
		"wrong":        strings.Repeat("c", 36),
		"prefix":       secretA[:35],
		"suffix":       secretA + "x",
		"empty-ish":    " ",
	}
	for name, tok := range cases {
		r := httptest.NewRequest("POST", "/hook/gitlab", nil)
		if tok != "" {
			r.Header.Set("X-Gitlab-Token", tok)
		}
		if err := v.Verify(r); err == nil {
			t.Errorf("%s: Verify accepted", name)
		}
	}
}

func TestNewVerifierRefusesWeakConfig(t *testing.T) {
	if _, err := NewVerifier(); err == nil {
		t.Error("no secrets configured must be an error, not an open door")
	}
	if _, err := NewVerifier(""); err == nil {
		t.Error("empty-only secret must be an error")
	}
	if _, err := NewVerifier("short"); err == nil {
		t.Error("short secret must be rejected")
	}
	// An unset *second* slot is fine: rotation is usually one-slot-populated.
	if _, err := NewVerifier(secretA, ""); err != nil {
		t.Errorf("empty second slot should be allowed: %v", err)
	}
}

// Errors and logs are the classic leak path for a shared secret.
func TestErrorsDoNotContainSecrets(t *testing.T) {
	v, _ := NewVerifier(secretA)
	r := httptest.NewRequest("POST", "/hook/gitlab", nil)
	r.Header.Set("X-Gitlab-Token", secretB)
	err := v.Verify(r)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secretA) || strings.Contains(err.Error(), secretB) {
		t.Fatalf("error leaks secret material: %v", err)
	}
}
```

(Delete the `req` placeholder line before running — it is there only to remind
you the tests construct requests inline.)

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/ghook/ -run Verifier -v`
Expected: FAIL — undefined `NewVerifier`.

- [ ] **Step 3: Implement `pkg/ghook/verify.go`**

```go
// Package ghook is gonk's GitLab webhook trust boundary: token verification,
// hardened receipt, event parsing, and duplicate suppression. Every input here
// is treated as hostile — the endpoint is, by design, reachable from anything
// that can route to the ingress.
package ghook

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
)

// MinSecretLen is the shortest webhook token gonk accepts. GitLab enforces no
// length at all, and this token is the only thing standing between the internet
// and the component that decides what may spend money.
const MinSecretLen = 32

var (
	ErrMissingToken = errors.New("ghook: missing X-Gitlab-Token header")
	ErrBadToken     = errors.New("ghook: X-Gitlab-Token matches no configured secret")
)

// Verifier checks X-Gitlab-Token against one or more accepted secrets. Multiple
// secrets are the rotation slots of spec 9: during a rotation both the new and
// the previous secret verify, so hooks can be re-provisioned without dropping
// events.
type Verifier struct {
	secrets [][]byte
}

// NewVerifier rejects a configuration that would silently disable verification.
// An empty slot is skipped (an unused rotation slot is normal); zero usable
// secrets, or a secret shorter than MinSecretLen, is a fatal config error.
func NewVerifier(secrets ...string) (*Verifier, error) {
	v := &Verifier{}
	for i, s := range secrets {
		if s == "" {
			continue
		}
		if len(s) < MinSecretLen {
			return nil, fmt.Errorf("ghook: webhook secret in slot %d is %d bytes, want >= %d", i, len(s), MinSecretLen)
		}
		v.secrets = append(v.secrets, []byte(s))
	}
	if len(v.secrets) == 0 {
		return nil, errors.New("ghook: no webhook secret configured")
	}
	return v, nil
}

// Verify compares against every slot and ORs the results: it does not
// short-circuit on the first match, so timing reveals neither which slot matched
// nor how many are configured. Errors never contain secret material.
func (v *Verifier) Verify(r *http.Request) error {
	got := []byte(r.Header.Get("X-Gitlab-Token"))
	if len(got) == 0 {
		return ErrMissingToken
	}
	var ok int
	for _, s := range v.secrets {
		ok |= subtle.ConstantTimeCompare(got, s)
	}
	if ok != 1 {
		return ErrBadToken
	}
	return nil
}
```

- [ ] **Step 4: Write the failing receiver test** — `pkg/ghook/receiver_test.go`

```go
package ghook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capture struct {
	events   []*Event
	outcomes []Outcome
	full     bool
}

func (c *capture) WebhookOutcome(_ string, o Outcome) { c.outcomes = append(c.outcomes, o) }
func (c *capture) sink(e *Event) bool {
	if c.full {
		return false
	}
	c.events = append(c.events, e)
	return true
}

func newHandler(t *testing.T, c *capture) *Handler {
	t.Helper()
	v, err := NewVerifier(secretA)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{Verifier: v, Deduper: NewDeduper(time.Hour, 1024), BotUserID: 7, Sink: c.sink, Obs: c}
}

func post(t *testing.T, h *Handler, event, token, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/hook/gitlab", strings.NewReader(body))
	if token != "" {
		r.Header.Set("X-Gitlab-Token", token)
	}
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const issueOpen = `{"object_kind":"issue","project":{"id":42,"path_with_namespace":"g/r","default_branch":"main"},"user":{"id":9,"username":"human"},"object_attributes":{"iid":3,"action":"open","title":"t"}}`

func TestAcceptsGoodRequest(t *testing.T) {
	c := &capture{}
	if got := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen).Code; got != 200 {
		t.Fatalf("code = %d", got)
	}
	if len(c.events) != 1 || c.events[0].Issue.IID != 3 {
		t.Fatalf("events = %+v", c.events)
	}
}

func TestRejectsBadToken(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Issue Hook", "wrong-but-long-enough-token-aaaaaaaa", "application/json", issueOpen)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if len(c.events) != 0 {
		t.Fatal("forged request reached the sink")
	}
	if c.outcomes[0] != OutcomeBadToken {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}

// Verification must precede reading the body: a forged request must not be able
// to make us allocate a megabyte.
func TestBodyIsNotReadBeforeVerification(t *testing.T) {
	c := &capture{}
	r := httptest.NewRequest("POST", "/hook/gitlab", &explodingReader{t: t})
	r.Header.Set("X-Gitlab-Event", "Issue Hook")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newHandler(t, c).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

type explodingReader struct{ t *testing.T }

func (e *explodingReader) Read([]byte) (int, error) {
	e.t.Fatal("body was read before the token was verified")
	return 0, nil
}

func TestRejectsMethodAndContentType(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	r := httptest.NewRequest("GET", "/hook/gitlab", nil)
	r.Header.Set("X-Gitlab-Token", secretA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d", w.Code)
	}
	if got := post(t, h, "Issue Hook", secretA, "text/plain", issueOpen).Code; got != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type code = %d", got)
	}
	// charset suffixes are legal
	if got := post(t, h, "Issue Hook", secretA, "application/json; charset=utf-8", issueOpen).Code; got != 200 {
		t.Fatalf("charset code = %d", got)
	}
}

func TestOversizeBodyRejected(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	big := `{"object_kind":"issue","x":"` + strings.Repeat("a", MaxBodyBytes+1) + `"}`
	if got := post(t, h, "Issue Hook", secretA, "application/json", big).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", got)
	}
	if len(c.events) != 0 {
		t.Fatal("oversize body reached the sink")
	}
}

// An event we do not handle is not an error: 4xx makes GitLab disable the hook
// after repeated failures, which would permanently break the latency path.
func TestUnhandledEventIsDroppedWith200(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Pipeline Hook", secretA, "application/json", `{"object_kind":"pipeline"}`)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if len(c.events) != 0 || c.outcomes[0] != OutcomeUnhandledEvent {
		t.Fatalf("events=%d outcome=%q", len(c.events), c.outcomes[0])
	}
}

func TestBotAuthoredEventDropped(t *testing.T) {
	c := &capture{}
	body := strings.Replace(issueOpen, `"user":{"id":9`, `"user":{"id":7`, 1) // bot user id
	post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", body)
	if len(c.events) != 0 {
		t.Fatal("bot-authored event was not suppressed (this is the infinite-loop guard)")
	}
	if c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}

func TestDuplicateEventDropped(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c)
	for range 2 {
		r := httptest.NewRequest("POST", "/hook/gitlab", strings.NewReader(issueOpen))
		r.Header.Set("X-Gitlab-Token", secretA)
		r.Header.Set("X-Gitlab-Event", "Issue Hook")
		r.Header.Set("X-Gitlab-Event-UUID", "same-uuid")
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(c.events) != 1 {
		t.Fatalf("events = %d, want 1 (second delivery is a duplicate)", len(c.events))
	}
}

// Queue full is a drop, not a 5xx: GitLab does not retry webhooks, and a 5xx
// only gets the hook disabled. Reconciliation is the correctness path (spec 5.2).
func TestQueueFullDropsWith200(t *testing.T) {
	c := &capture{full: true}
	w := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", issueOpen)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if c.outcomes[0] != OutcomeQueueFull {
		t.Fatalf("outcome = %q", c.outcomes[0])
	}
}
```

- [ ] **Step 5: Implement `pkg/ghook/receiver.go`**

```go
package ghook

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

// MaxBodyBytes caps a webhook payload. Issue/note/MR payloads are a few KiB;
// 1 MiB is generous and still bounds what an unauthenticated flood can make us
// allocate (bodies are only read after the token verifies).
const MaxBodyBytes = 1 << 20

// Outcome is why a delivery ended the way it did. These strings are metric label
// values and dashboard keys: keep them stable.
type Outcome string

const (
	OutcomeAccepted       Outcome = "accepted"
	OutcomeMissingToken   Outcome = "missing_token"
	OutcomeBadToken       Outcome = "bad_token"
	OutcomeBadMethod      Outcome = "bad_method"
	OutcomeBadContentType Outcome = "bad_content_type"
	OutcomeTooLarge       Outcome = "too_large"
	OutcomeMalformed      Outcome = "malformed"
	OutcomeUnhandledEvent Outcome = "unhandled_event"
	OutcomeDuplicate      Outcome = "duplicate"
	OutcomeBotAuthored    Outcome = "bot_authored"
	OutcomeQueueFull      Outcome = "queue_full"
)

// Observer receives one call per delivery. The Prometheus implementation lives
// in pkg/intake (Task 10); ghook stays dependency-free.
type Observer interface {
	WebhookOutcome(event string, o Outcome)
}

type NopObserver struct{}

func (NopObserver) WebhookOutcome(string, Outcome) {}

// handledEvents is the X-Gitlab-Event allow-list. Anything else is dropped with
// a 200 (see TestUnhandledEventIsDroppedWith200).
var handledEvents = map[string]bool{
	"Issue Hook":         true,
	"Note Hook":          true,
	"Merge Request Hook": true,
}

// Handler is the /hook/gitlab endpoint: the only path gonk exposes to the
// ingress (spec 9).
type Handler struct {
	Verifier  *Verifier
	Deduper   *Deduper
	BotUserID int64
	// Sink hands the event to the dispatcher. It must not block or do IO; it
	// returns false when its queue is full, which is a drop, not an error.
	Sink func(*Event) bool
	Obs  Observer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-Gitlab-Event")

	if r.Method != http.MethodPost {
		h.finish(w, event, OutcomeBadMethod, http.StatusMethodNotAllowed)
		return
	}
	// Verify BEFORE touching the body. A forged request must cost us nothing.
	if err := h.Verifier.Verify(r); err != nil {
		out := OutcomeBadToken
		if errors.Is(err, ErrMissingToken) {
			out = OutcomeMissingToken
		}
		h.finish(w, event, out, http.StatusUnauthorized)
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		h.finish(w, event, OutcomeBadContentType, http.StatusUnsupportedMediaType)
		return
	}
	if !handledEvents[event] {
		h.finish(w, event, OutcomeUnhandledEvent, http.StatusOK)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.finish(w, event, OutcomeTooLarge, http.StatusRequestEntityTooLarge)
			return
		}
		h.finish(w, event, OutcomeMalformed, http.StatusBadRequest)
		return
	}

	ev, err := ParseEvent(event, body)
	if err != nil {
		h.finish(w, event, OutcomeMalformed, http.StatusBadRequest)
		return
	}
	if h.Deduper.Seen(DedupeKey(r.Header.Get("X-Gitlab-Event-UUID"), ev)) {
		h.finish(w, event, OutcomeDuplicate, http.StatusOK)
		return
	}
	// Loop guard (spec 4.3 step 2): the bot's own comments must never trigger
	// the bot.
	if ev.User.ID == h.BotUserID {
		h.finish(w, event, OutcomeBotAuthored, http.StatusOK)
		return
	}
	if !h.Sink(ev) {
		h.finish(w, event, OutcomeQueueFull, http.StatusOK)
		return
	}
	h.finish(w, event, OutcomeAccepted, http.StatusOK)
}

func (h *Handler) finish(w http.ResponseWriter, event string, o Outcome, code int) {
	if h.Obs != nil {
		h.Obs.WebhookOutcome(event, o)
	}
	w.WriteHeader(code)
	// Body is deliberately terse: it is an error channel to an attacker.
	_, _ = io.WriteString(w, string(o)+"\n")
}

func isJSON(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(mt) == "application/json"
}
```

- [ ] **Step 6: Run the tests** (they need `Deduper`, `Event`, `ParseEvent`,
  `DedupeKey` from Task 4 — write Task 4 first if you prefer, or stub them and
  let Task 4 replace the stubs. Recommended: **do Task 4's Step 3 implementation
  now**, then run both test files together.)

Run: `go test ./pkg/ghook/ -race -count=1 -v`
Expected: PASS (verifier + receiver).

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./pkg/ghook/ -race -count=1
git add pkg/ghook && git commit -m "feat(ghook): constant-time token verification and hardened webhook receiver"
```

---
### Task 4: `pkg/ghook` — event parsing, golden fixtures, bounded dedupe

**Files:** Create `pkg/ghook/event.go`, `pkg/ghook/dedupe.go`,
`pkg/ghook/event_test.go`, `pkg/ghook/dedupe_test.go`,
`pkg/ghook/testdata/{issue-open,issue-update,note-mention-issue,note-by-bot,mr-onboard-merged,mr-onboard-closed}.json`.

> **Fixture provenance (spec 12.6).** These fixtures are hand-built from the
> GitLab webhook documentation, not captured from a live instance. **Plan 06 must
> replace them with payloads captured from the real gitlab.orac.local**, and
> refresh them on every GitLab upgrade. Until then, treat "the fixture says so"
> as weaker evidence than "GitLab says so". Every field this package reads is
> listed in `event.go`'s doc comment so the refresh has a checklist.

- [ ] **Step 1: Write the fixtures**

`pkg/ghook/testdata/issue-open.json` (trimmed to the fields we consume plus a
few we deliberately ignore, so the "unknown fields are tolerated" property is
actually exercised):

```json
{
  "object_kind": "issue",
  "event_type": "issue",
  "user": { "id": 9, "username": "human", "name": "A Human" },
  "project": {
    "id": 42,
    "name": "repo",
    "path_with_namespace": "group/repo",
    "default_branch": "main",
    "web_url": "https://gitlab.orac.local/group/repo"
  },
  "object_attributes": {
    "id": 1001,
    "iid": 3,
    "title": "Login button does nothing",
    "description": "Clicking it does nothing.",
    "state": "opened",
    "action": "open",
    "updated_at": "2026-07-13 10:00:00 UTC"
  },
  "labels": [],
  "changes": {}
}
```

`note-mention-issue.json`: `object_kind: "note"`, `user.id: 9`,
`object_attributes: { id: 2001, note: "@gonk please re-triage this", noteable_type: "Issue", discussion_id: "d1" }`,
plus a sibling `issue: { id: 1001, iid: 3 }` object and the same `project`.

`note-by-bot.json`: identical to the above but `user.id: 7` (the bot).

`mr-onboard-merged.json`: `object_kind: "merge_request"`, `user.id: 9`,
`object_attributes: { iid: 5, source_branch: "gonk/onboard", target_branch: "main", state: "merged", action: "merge" }`.

`mr-onboard-closed.json`: same with `state: "closed"`, `action: "close"`.

`issue-update.json`: `action: "update"` (used to prove `update` does not trigger
triage).

- [ ] **Step 2: Write the failing test** — `pkg/ghook/event_test.go`

```go
package ghook

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

func TestParseIssueOpen(t *testing.T) {
	ev, err := ParseEvent("Issue Hook", fixture(t, "issue-open"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.Kind != KindIssue || ev.Project.ID != 42 || ev.Project.PathWithNamespace != "group/repo" {
		t.Fatalf("project = %+v", ev.Project)
	}
	if ev.Project.DefaultBranch != "main" || ev.User.ID != 9 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Issue == nil || ev.Issue.IID != 3 || ev.Issue.Action != "open" {
		t.Fatalf("issue = %+v", ev.Issue)
	}
}

func TestParseNoteOnIssue(t *testing.T) {
	ev, err := ParseEvent("Note Hook", fixture(t, "note-mention-issue"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.Note == nil || ev.Note.NoteableType != "Issue" || !strings.Contains(ev.Note.Body, "@gonk") {
		t.Fatalf("note = %+v", ev.Note)
	}
	if ev.Issue == nil || ev.Issue.IID != 3 {
		t.Fatalf("note event must carry the issue iid it hangs off: %+v", ev.Issue)
	}
	if ev.Note.DiscussionID != "d1" {
		t.Fatalf("discussion id = %q (needed to route the reply to the right thread)", ev.Note.DiscussionID)
	}
}

func TestParseMergeRequest(t *testing.T) {
	ev, err := ParseEvent("Merge Request Hook", fixture(t, "mr-onboard-merged"))
	if err != nil {
		t.Fatalf("ParseEvent = %v", err)
	}
	if ev.MergeRequest == nil || ev.MergeRequest.SourceBranch != "gonk/onboard" || ev.MergeRequest.Action != "merge" {
		t.Fatalf("mr = %+v", ev.MergeRequest)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ header, body string }{
		"not json":              {"Issue Hook", "{{{"},
		"empty":                 {"Issue Hook", ""},
		"header/kind mismatch":  {"Issue Hook", `{"object_kind":"note","project":{"id":1,"path_with_namespace":"a/b"}}`},
		"unknown kind":          {"Issue Hook", `{"object_kind":"wiki_page"}`},
		"no project id":         {"Issue Hook", `{"object_kind":"issue","project":{"path_with_namespace":"a/b"}}`},
		"no project path":       {"Issue Hook", `{"object_kind":"issue","project":{"id":1}}`},
		"issue without iid":     {"Issue Hook", `{"object_kind":"issue","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"action":"open"}}`},
		"note on a wiki":        {"Note Hook", `{"object_kind":"note","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"id":1,"noteable_type":"Snippet"}}`},
	}
	for name, c := range cases {
		if _, err := ParseEvent(c.header, []byte(c.body)); err == nil {
			t.Errorf("%s: ParseEvent accepted %q", name, c.body)
		}
	}
}

// PLAN.md carry-forward: atags accepts any string for Project/Rig, so a value
// with a newline or comma could shift a column in the ledger. GitLab paths
// cannot contain those — enforce it at the boundary anyway, which is here.
func TestParseRejectsAttributionUnsafePath(t *testing.T) {
	for _, bad := range []string{"a/b\nc", "a/b,c", "a/b\rc"} {
		body := `{"object_kind":"issue","project":{"id":1,"path_with_namespace":` +
			strconvQuote(bad) + `},"object_attributes":{"iid":1,"action":"open"}}`
		if _, err := ParseEvent("Issue Hook", []byte(body)); err == nil {
			t.Errorf("accepted attribution-unsafe path %q", bad)
		}
	}
}
```

(`strconvQuote` is `strconv.Quote`; import it.)

- [ ] **Step 3: Implement `pkg/ghook/event.go`**

```go
package ghook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event is the subset of a GitLab webhook payload gonk consumes. It is
// deliberately small: every field here is a field the golden fixtures must keep
// carrying across GitLab upgrades (spec 12.6). Fields consumed:
//
//	object_kind
//	project.id, project.path_with_namespace, project.default_branch, project.web_url
//	user.id, user.username
//	object_attributes.iid / .action / .title / .description        (issue)
//	object_attributes.id / .note / .noteable_type / .discussion_id (note)
//	issue.iid                                                       (note on an issue)
//	object_attributes.iid / .source_branch / .target_branch
//	  / .state / .action                                            (merge_request)
type Event struct {
	Kind         Kind
	Project      Project
	User         User
	Issue        *Issue
	Note         *Note
	MergeRequest *MergeRequest
}

type Kind string

const (
	KindIssue        Kind = "issue"
	KindNote         Kind = "note"
	KindMergeRequest Kind = "merge_request"
)

type Project struct {
	ID                int64
	PathWithNamespace string
	DefaultBranch     string
	WebURL            string
}

type User struct {
	ID       int64
	Username string
}

type Issue struct {
	IID         int64
	Action      string // open | reopen | update | close
	Title       string
	Description string
}

type Note struct {
	ID           int64
	Body         string
	NoteableType string // Issue | MergeRequest
	DiscussionID string
}

type MergeRequest struct {
	IID          int64
	Action       string // open | merge | close | update
	State        string
	SourceBranch string
	TargetBranch string
}

// headerKind maps X-Gitlab-Event to the object_kind we require in the body.
var headerKind = map[string]Kind{
	"Issue Hook":         KindIssue,
	"Note Hook":          KindNote,
	"Merge Request Hook": KindMergeRequest,
}

type rawEvent struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	User struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Attrs struct {
		ID           int64  `json:"id"`
		IID          int64  `json:"iid"`
		Action       string `json:"action"`
		State        string `json:"state"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		DiscussionID string `json:"discussion_id"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
	} `json:"object_attributes"`
	Issue struct {
		IID int64 `json:"iid"`
	} `json:"issue"`
}

// ParseEvent decodes and validates one delivery. The X-Gitlab-Event header and
// the body's object_kind must agree: a mismatch means either a GitLab payload
// change (which must not be silently absorbed — spec 12.6) or a caller confusion,
// and both deserve a loud failure rather than a guess.
func ParseEvent(header string, body []byte) (*Event, error) {
	want, ok := headerKind[header]
	if !ok {
		return nil, fmt.Errorf("ghook: unhandled event header %q", header)
	}
	var raw rawEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("ghook: malformed payload: %w", err)
	}
	if Kind(raw.ObjectKind) != want {
		return nil, fmt.Errorf("ghook: header %q but object_kind %q", header, raw.ObjectKind)
	}
	if raw.Project.ID <= 0 {
		return nil, fmt.Errorf("ghook: payload has no project.id")
	}
	if err := attributionSafe(raw.Project.PathWithNamespace); err != nil {
		return nil, err
	}

	ev := &Event{
		Kind: want,
		Project: Project{
			ID:                raw.Project.ID,
			PathWithNamespace: raw.Project.PathWithNamespace,
			DefaultBranch:     raw.Project.DefaultBranch,
			WebURL:            raw.Project.WebURL,
		},
		User: User{ID: raw.User.ID, Username: raw.User.Username},
	}

	switch want {
	case KindIssue:
		if raw.Attrs.IID <= 0 {
			return nil, fmt.Errorf("ghook: issue event has no iid")
		}
		ev.Issue = &Issue{
			IID: raw.Attrs.IID, Action: raw.Attrs.Action,
			Title: raw.Attrs.Title, Description: raw.Attrs.Description,
		}
	case KindNote:
		switch raw.Attrs.NoteableType {
		case "Issue":
			if raw.Issue.IID <= 0 {
				return nil, fmt.Errorf("ghook: note on an issue with no issue.iid")
			}
			ev.Issue = &Issue{IID: raw.Issue.IID}
		case "MergeRequest":
			// carried for v2 (pipeline/MR conversations); v1 dispatch ignores it.
		default:
			return nil, fmt.Errorf("ghook: note on unsupported noteable_type %q", raw.Attrs.NoteableType)
		}
		ev.Note = &Note{
			ID: raw.Attrs.ID, Body: raw.Attrs.Note,
			NoteableType: raw.Attrs.NoteableType, DiscussionID: raw.Attrs.DiscussionID,
		}
	case KindMergeRequest:
		if raw.Attrs.IID <= 0 {
			return nil, fmt.Errorf("ghook: merge_request event has no iid")
		}
		ev.MergeRequest = &MergeRequest{
			IID: raw.Attrs.IID, Action: raw.Attrs.Action, State: raw.Attrs.State,
			SourceBranch: raw.Attrs.SourceBranch, TargetBranch: raw.Attrs.TargetBranch,
		}
	}
	return ev, nil
}

// attributionSafe rejects values that would corrupt a downstream ledger row.
// pkg/atags accepts any string by contract (PLAN.md carry-forward); the boundary
// is where it gets enforced.
func attributionSafe(path string) error {
	if path == "" {
		return fmt.Errorf("ghook: payload has no project.path_with_namespace")
	}
	if strings.ContainsAny(path, "\n\r,") {
		return fmt.Errorf("ghook: project path contains attribution-unsafe characters")
	}
	return nil
}
```

- [ ] **Step 4: Write the failing dedupe test** — `pkg/ghook/dedupe_test.go`

```go
package ghook

import (
	"sync"
	"testing"
	"time"
)

func TestDeduperSuppressesRepeats(t *testing.T) {
	d := NewDeduper(time.Hour, 10)
	if d.Seen("k") {
		t.Fatal("first sighting must be new")
	}
	if !d.Seen("k") {
		t.Fatal("second sighting must be a duplicate")
	}
	if d.Seen("other") {
		t.Fatal("a different key must be new")
	}
}

func TestDeduperExpires(t *testing.T) {
	now := time.Unix(0, 0)
	d := NewDeduper(time.Minute, 10)
	d.now = func() time.Time { return now }
	d.Seen("k")
	now = now.Add(2 * time.Minute)
	if d.Seen("k") {
		t.Fatal("entry should have expired")
	}
}

// Unbounded memory here is a remote DoS: every distinct event id would be
// retained forever.
func TestDeduperIsBounded(t *testing.T) {
	d := NewDeduper(time.Hour, 4)
	for i := range 100 {
		d.Seen(string(rune('a' + i%26)) + string(rune('0'+i/26)))
	}
	if got := d.Len(); got > 4 {
		t.Fatalf("cache holds %d entries, cap is 4", got)
	}
}

func TestDeduperIsConcurrencySafe(t *testing.T) {
	d := NewDeduper(time.Hour, 128)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Seen("key")
			d.Seen(string(rune(i)))
		}()
	}
	wg.Wait()
}

func TestDedupeKeyPrefersEventUUID(t *testing.T) {
	ev, err := ParseEvent("Issue Hook", fixture(t, "issue-open"))
	if err != nil {
		t.Fatal(err)
	}
	if got := DedupeKey("abc-123", ev); got != "uuid:abc-123" {
		t.Fatalf("key = %q", got)
	}
	// No UUID header (older GitLab, or a replay stripped of it): derive one.
	k1 := DedupeKey("", ev)
	k2 := DedupeKey("", ev)
	if k1 == "" || k1 != k2 {
		t.Fatalf("derived key must be stable and non-empty: %q %q", k1, k2)
	}
}
```

- [ ] **Step 5: Implement `pkg/ghook/dedupe.go`**

```go
package ghook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Deduper suppresses repeat deliveries. GitLab can deliver the same event twice
// (retries after a slow response, an operator hitting "Test"), and a duplicate
// triage order costs real tokens. Bounded in both time and size: an unbounded
// map keyed on attacker-influenced ids is a memory DoS.
type Deduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	max  int
	now  func() time.Time
}

func NewDeduper(ttl time.Duration, max int) *Deduper {
	if max < 1 {
		max = 1
	}
	return &Deduper{seen: make(map[string]time.Time), ttl: ttl, max: max, now: time.Now}
}

// Seen reports whether key was already recorded, and records it if not.
func (d *Deduper) Seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	if at, ok := d.seen[key]; ok {
		if now.Sub(at) < d.ttl {
			return true
		}
		delete(d.seen, key) // expired
	}
	d.evict(now)
	d.seen[key] = now
	return false
}

func (d *Deduper) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

// evict drops expired entries, then, if still at capacity, the oldest ones.
func (d *Deduper) evict(now time.Time) {
	for k, at := range d.seen {
		if now.Sub(at) >= d.ttl {
			delete(d.seen, k)
		}
	}
	for len(d.seen) >= d.max {
		oldestKey, oldestAt := "", time.Time{}
		for k, at := range d.seen {
			if oldestAt.IsZero() || at.Before(oldestAt) {
				oldestKey, oldestAt = k, at
			}
		}
		delete(d.seen, oldestKey)
	}
}

// DedupeKey prefers GitLab's X-Gitlab-Event-UUID, which is exactly this. When
// absent, derive a stable key from the identifying fields of the event.
func DedupeKey(eventUUID string, ev *Event) string {
	if eventUUID != "" {
		return "uuid:" + eventUUID
	}
	var id string
	switch {
	case ev.Note != nil:
		id = fmt.Sprintf("note:%d", ev.Note.ID)
	case ev.MergeRequest != nil:
		id = fmt.Sprintf("mr:%d:%s", ev.MergeRequest.IID, ev.MergeRequest.Action)
	case ev.Issue != nil:
		id = fmt.Sprintf("issue:%d:%s", ev.Issue.IID, ev.Issue.Action)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", ev.Kind, ev.Project.ID, id)))
	return "derived:" + hex.EncodeToString(sum[:8])
}
```

- [ ] **Step 6: Run everything in the package**

Run: `go test ./pkg/ghook/ -race -count=1 -v`
Expected: PASS (verifier, receiver, event, dedupe).

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./pkg/ghook/ -race -count=1
git add pkg/ghook && git commit -m "feat(ghook): event parsing with golden fixtures and bounded dedupe cache"
```

---

### Task 5: `pkg/intake` — the project state machine

**Files:** Create `pkg/intake/state.go`, `pkg/intake/state_test.go`.

**One pure function** decides what gonk thinks of a project. No IO, no clock, no GitLab, **no resolver** — so it can be table-tested exhaustively, and so there is exactly one place where "may this project be given work" is decided.

**This is the task Conflict A reshaped.** Intake used to call `gonkcfg.Resolve` here and compute enabled-ness and budgets itself. It no longer does. `Classify` now **joins two inputs**:

1. an `Observation` — what intake sees in **GitLab** (member? `.gonk.yml` bytes? `.agent/`? onboarding declined?), and
2. meter's `*meterapi.ProjectResponse` — the **resolved** answer (valid? disabled? what actions? what ladder?).

The states split into three groups, and knowing which group a state is in tells you who can compute it:

| State | Determined by | Meaning | May dispatch |
|---|---|---|---|
| `unmanaged` | **GitLab** | bot is not a member (de-onboarded, or never invited) | nothing |
| `absent` | **GitLab** | member, no `.gonk.yml` on the default branch | onboarding MR (deterministic, no LLM) |
| `declined` | **GitLab** | member, no `.gonk.yml`, onboarding MR closed unmerged (spec 5.3) | nothing |
| `unsynced` | **neither** | `.gonk.yml` exists, but meter has not answered yet (unreachable, or first pass) | **nothing** — no unmetered work, ever |
| `invalid` | **METER** (422) | `.gonk.yml` present but will not load. Meter deleted the key. | nothing |
| `disabled` | **METER** (200) | resolves, but `Effective.Enabled == false` (kill switch, empty ladder, non-finite budget — ADR-002) | nothing |
| `key-missing` | **METER** (200) | resolves and is enabled, but the LiteLLM virtual key is not provisioned yet | nothing (meter would `defer` anyway) |
| `pending` | **the join** | meter says `active`, and `.agent/` is not in the repo yet (spec 5.3) | **only** the scaffold MR order |
| `valid` | **the join** | meter says `active`, and `.agent/` is present | triage, mention replies |

**Say it plainly, because it is the point of the split:** `invalid`, `disabled`, and `key-missing` **cannot be computed by intake**. They are meter's answers. Intake *records* them. If meter has not answered, the project is `unsynced` and **nothing runs** — which is the same fail-closed rule as before, just honest about where the knowledge lives.

Note `unsynced` replaces the old `KeySynced bool`. It is a *state*, not a flag, because it is genuinely a distinct thing to be: we have a config, and we do not yet know what it means.

- [ ] **Step 1: Write the failing test** — `pkg/intake/state_test.go`

```go
package intake

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

const goodConfig = `
version: 1
enabled: true
actions: { triage: true }
ladder: [qwen-local]
`

func obs(mut func(*Observation)) Observation {
	o := Observation{
		Member:          true,
		ConfigBytes:     []byte(goodConfig),
		AgentDirPresent: true,
	}
	if mut != nil {
		mut(&o)
	}
	return o
}

// active is what meter returns for a healthy project. NOTE we build a
// meterapi.Effective here, NOT a gonkcfg.Effective -- intake never holds the
// latter, because intake never calls Resolve (ADR-002 says Resolve's return
// value is the only legitimate way to produce one, and intake has no business
// producing one).
func active(mut func(*meterapi.ProjectResponse)) *meterapi.ProjectResponse {
	r := &meterapi.ProjectResponse{
		Project: "group/repo", Rig: "group-repo",
		State: meterapi.StateActive,
		Effective: &meterapi.Effective{
			Enabled: true,
			Actions: meterapi.Actions{Triage: true},
			Ladder:  []string{"qwen-local"},
			Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
		},
		KeyRef:     meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		ConfigHash: "sha256:abc",
	}
	if mut != nil {
		mut(r)
	}
	return r
}

func TestClassifyValid(t *testing.T) {
	c := Classify(obs(nil), active(nil))
	if c.State != StateValid {
		t.Fatalf("state = %q (%s)", c.State, c.Reason)
	}
	if !c.MayTriage() || c.MayScaffold() {
		t.Fatalf("valid project: MayTriage=%v MayScaffold=%v", c.MayTriage(), c.MayScaffold())
	}
	if c.ConfigHash == "" {
		t.Fatal("valid config must have a hash (the order records which config authorized it)")
	}
}

func TestClassifyPendingUntilAgentDirExists(t *testing.T) {
	c := Classify(obs(func(o *Observation) { o.AgentDirPresent = false }), active(nil))
	if c.State != StatePending {
		t.Fatalf("state = %q", c.State)
	}
	// spec 5.3: no LLM actions while pending EXCEPT the scaffold MR itself.
	if c.MayTriage() {
		t.Fatal("triage must not run while a project is pending")
	}
	if !c.MayScaffold() {
		t.Fatal("pending is exactly the state where the scaffold MR is authorized")
	}
}

func TestClassifyAbsentAndDeclined(t *testing.T) {
	// No config at all: meter was never called, so there is no response. This is
	// NOT unsynced -- there is nothing to sync.
	c := Classify(obs(func(o *Observation) { o.ConfigBytes = nil }), nil)
	if c.State != StateAbsent {
		t.Fatalf("state = %q, want absent (onboarding candidate)", c.State)
	}
	if !c.MayOnboard() {
		t.Fatal("absent is exactly the onboarding-candidate state")
	}
	c = Classify(obs(func(o *Observation) {
		o.ConfigBytes = nil
		o.OnboardingDeclined = true
	}), nil)
	if c.State != StateDeclined || c.MayOnboard() {
		t.Fatalf("a declined project must not get another onboarding MR (spec 5.3): %+v", c)
	}
}

func TestClassifyUnmanaged(t *testing.T) {
	c := Classify(obs(func(o *Observation) { o.Member = false }), active(nil))
	if c.State != StateUnmanaged || c.MayTriage() || c.MayOnboard() {
		t.Fatalf("non-member must be inert even if meter still has a registration: %+v", c)
	}
}

// *** THE FAIL-CLOSED RULE, AND THE HEART OF CONFLICT A. ***
// We have a config. We do NOT have meter's answer -- meter is down, or this is
// the first pass. We do not know whether this project is enabled, what its
// ladder is, or whether its key exists. WE CANNOT GUESS, because we no longer
// have a resolver, and that is deliberate: guessing is what two resolvers
// disagreeing looks like.
func TestClassifyUnsyncedWhenMeterHasNotAnswered(t *testing.T) {
	c := Classify(obs(nil), nil)
	if c.State != StateUnsynced {
		t.Fatalf("state = %q, want unsynced", c.State)
	}
	if c.MayTriage() || c.MayScaffold() || c.MayOnboard() {
		t.Fatal("a project whose policy we have not resolved must authorize NOTHING (no unmetered work, ever)")
	}
	if c.Reason == "" {
		t.Fatal("unsynced must explain itself")
	}
}

// Meter's answers, recorded verbatim. Intake CANNOT compute any of these three.
func TestClassifyRecordsMetersVerdict(t *testing.T) {
	cases := map[string]struct {
		resp  *meterapi.ProjectResponse
		state State
	}{
		"invalid (422: the yaml will not load)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateInvalid
				r.Effective = nil // there is NO Effective for an invalid config (ADR-002)
				r.Error = ".gonk.yml: at '/budget/monthly_tokens': got string, want integer"
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateInvalid,
		},
		"disabled (the instance kill switch, an empty ladder, ...)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateDisabled
				r.Effective = &meterapi.Effective{Enabled: false}
				r.DisabledReason = "disabled by instance policy"
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateDisabled,
		},
		"key-missing (LiteLLM was unreachable at provisioning time)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateKeyMissing
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateKeyMissing,
		},
	}
	for name, c := range cases {
		got := Classify(obs(nil), c.resp)
		if got.State != c.state {
			t.Errorf("%s: state = %q, want %q", name, got.State, c.state)
		}
		if got.MayTriage() || got.MayScaffold() || got.MayOnboard() {
			t.Errorf("%s: must authorize nothing", name)
		}
		if got.Reason == "" {
			t.Errorf("%s: must explain itself (it goes in a metric label and a log line)", name)
		}
	}
}

// Intake's action check is a PRE-FILTER, not the enforcement point: meter vetoes
// at /decide, which is the only chokepoint before a session spawns. But we still
// must not fire an order we know will be denied.
func TestClassifyRespectsMetersActionVeto(t *testing.T) {
	c := Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions.Triage = false // a coarser layer vetoed it (ADR-002)
	}))
	if c.State != StateValid {
		t.Fatalf("state = %q; an action veto disables the ACTION, not the project (ADR-002)", c.State)
	}
	if c.MayTriage() {
		t.Fatal("triage is vetoed; do not fire an order meter will deny")
	}
}

func TestClassifyRespectsRespondToMentions(t *testing.T) {
	// respond_to_mentions is NOT an Action, so meter does not check it. Intake is
	// its only enforcement point. If we drop it here, it is enforced NOWHERE.
	c := Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Triage.RespondToMentions = false
	}))
	if !c.MayTriage() {
		t.Fatal("triage itself is still on")
	}
	if c.MayMentionReply() {
		t.Fatal("respond_to_mentions: false must suppress mention replies -- intake is the ONLY thing that checks it")
	}
}

// Intake may still ask "do these bytes parse?" for its own metrics and for the
// onboarding flow -- but it is ADVISORY. Meter's 422 is authoritative, and where
// the two disagree, METER WINS.
func TestLooksInvalidIsAdvisoryOnly(t *testing.T) {
	garbage := obs(func(o *Observation) { o.ConfigBytes = []byte("{{{{") })
	// Intake thinks it is garbage; meter (hypothetically) said active. Meter wins:
	// we do not have the operator config, so we do not get a vote.
	c := Classify(garbage, active(nil))
	if c.State != StateValid {
		t.Fatalf("state = %q; meter is the authority on validity, not intake", c.State)
	}
	// But with no answer from meter, our own read is what populates the metric.
	c = Classify(garbage, nil)
	if c.State != StateUnsynced || !c.LooksInvalid {
		t.Fatalf("unsynced + LooksInvalid expected, got %+v", c)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/intake/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement `pkg/intake/state.go`**

```go
// Package intake is gonk-intake: GitLab webhook receipt, reconciliation of the
// bot's project memberships and .gonk.yml state, the deterministic onboarding
// MR, and dispatch of work to the Gas City supervisor.
//
// Everything in this package is deterministic. No code path here calls a model.
//
// AND: no code path here calls gonkcfg.Resolve. Config resolution belongs to
// gonk-meter, which is the only component holding operator (instance/group)
// policy -- two independent resolvers would be two sources of truth for a budget
// ceiling. Intake pushes RAW .gonk.yml bytes to meter and reads the resolved
// answer back. See "Division of responsibility" in the plan.
package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// MaxConfigBytes caps .gonk.yml. It is project-authored, attacker-controlled
// content (ADR-002 "Untrusted input"); the real file is ~400 bytes.
const MaxConfigBytes int64 = 64 << 10

// ConfigPath is where a project declares itself tagged in (spec 5.1).
const ConfigPath = ".gonk.yml"

// AgentDir is the Navigator-style context directory (spec 5.3). Its absence is
// what makes a project `pending`.
const AgentDir = ".agent"

type State string

const (
	// Determined by GitLab alone.
	StateUnmanaged State = "unmanaged"
	StateAbsent    State = "absent"
	StateDeclined  State = "declined"

	// Determined by NEITHER: we have config bytes, and no answer from meter.
	StateUnsynced State = "unsynced"

	// Determined by METER. Intake records these; it cannot compute them.
	StateInvalid    State = "invalid"
	StateDisabled   State = "disabled"
	StateKeyMissing State = "key-missing"

	// The join: meter says active, and we looked in the repo.
	StatePending State = "pending"
	StateValid   State = "valid"
)

// AllStates is the metric label domain; keep it in sync with the constants.
var AllStates = []State{
	StateUnmanaged, StateAbsent, StateDeclined, StateUnsynced,
	StateInvalid, StateDisabled, StateKeyMissing, StatePending, StateValid,
}

// Observation is what intake can see IN GITLAB. Separating observation from
// classification is what makes the decision a pure function.
type Observation struct {
	Member          bool
	ConfigBytes     []byte // nil when .gonk.yml is absent
	ConfigTooLarge  bool   // fetch exceeded MaxConfigBytes
	ConfigCommitSHA string // the commit .gonk.yml was read at; "" if unknown
	AgentDirPresent bool
	// OnboardingDeclined: an onboarding MR was closed unmerged and the bot has
	// not been re-invited since (spec 5.3; see AD-3 for how that is derived).
	OnboardingDeclined bool
}

// Classification is the single source of truth for what intake may DO to a
// project. Consumers must ask the May* predicates rather than re-deriving
// permission from State.
type Classification struct {
	State  State
	Reason string // why, for the negative states; "" otherwise
	// Meter is meter's answer, verbatim. NIL means meter has not answered.
	// Effective policy is read from HERE and nowhere else.
	Meter      *meterapi.ProjectResponse
	ConfigHash string // sha256 of the raw bytes; "" when there is no config
	// LooksInvalid is intake's own gonkcfg.Load result. ADVISORY ONLY -- it
	// drives a metric and the onboarding flow. Meter's 422 is authoritative, and
	// where the two disagree, METER WINS (it has the operator config; we do not).
	LooksInvalid bool
	LoadError    string
}

// eff is meter's Effective, or a zero value if meter has not answered. Every
// May* predicate below gates on State first, and StateValid/StatePending are
// only reachable when Meter is non-nil with a non-nil Effective -- so this
// cannot hand out a permissive zero value.
func (c Classification) eff() meterapi.Effective {
	if c.Meter == nil || c.Meter.Effective == nil {
		return meterapi.Effective{}
	}
	return *c.Meter.Effective
}

// MayTriage. NOTE the Actions check is a PRE-FILTER, not enforcement: meter
// vetoes at /decide, the only chokepoint before a session spawns. We check it
// here purely to avoid firing an order that will certainly be denied. It must
// therefore be a SUBSET of meter's rules -- never a superset, or we silently
// drop work meter would have allowed.
func (c Classification) MayTriage() bool {
	return c.State == StateValid && c.eff().Actions.Triage
}

// MayScaffold: spec 5.3 -- the .agent/ scaffold MR is the one metered action
// permitted while a project is pending, authorized by the just-merged config.
func (c Classification) MayScaffold() bool { return c.State == StatePending }

// MayOnboard: the deterministic onboarding MR. Not gated on Actions (the project
// has no config yet to opt in with) and not a metered action.
func (c Classification) MayOnboard() bool { return c.State == StateAbsent }

// MayMentionReply follows triage (spec 5.5).
//
// triage.respond_to_mentions is NOT one of Effective.Actions, so METER DOES NOT
// CHECK IT. Intake is its only enforcement point. Drop this and the setting is
// enforced nowhere at all.
func (c Classification) MayMentionReply() bool {
	return c.MayTriage() && c.eff().Triage.RespondToMentions
}

// Classify is a pure function: same inputs, same answer, no IO, no clock, NO
// RESOLVER. `mr` is meter's response for this project, or nil if meter has not
// answered (unreachable, or we have not asked yet).
func Classify(obs Observation, mr *meterapi.ProjectResponse) Classification {
	if !obs.Member {
		return Classification{State: StateUnmanaged, Reason: "bot is not a member"}
	}

	// No config -> this is an onboarding question, and meter is not involved.
	if obs.ConfigBytes == nil && !obs.ConfigTooLarge {
		if obs.OnboardingDeclined {
			return Classification{State: StateDeclined, Reason: "onboarding merge request was closed unmerged"}
		}
		return Classification{State: StateAbsent, Reason: "no " + ConfigPath}
	}

	c := Classification{Meter: mr, ConfigHash: hashConfig(obs.ConfigBytes)}

	// Oversize: we never even fetched the bytes, so meter cannot have seen them.
	// This is the one validity call intake makes on its own authority, because it
	// is about the FETCH, not about the content.
	if obs.ConfigTooLarge {
		c.State = StateInvalid
		c.Reason = fmt.Sprintf("%s exceeds %d bytes", ConfigPath, MaxConfigBytes)
		c.LooksInvalid = true
		return c
	}

	// Our own read of the bytes. ADVISORY: it drives a metric and lets the
	// onboarding flow say something useful before meter has answered. It does NOT
	// decide the state when meter has spoken.
	if _, err := gonkcfg.Load(obs.ConfigBytes); err != nil {
		c.LooksInvalid, c.LoadError = true, err.Error()
	}

	// No answer from meter: we have a config and we do not know what it MEANS.
	// We have no operator policy and no resolver, so we cannot find out. Fail
	// closed and try again next pass.
	if mr == nil {
		c.State = StateUnsynced
		c.Reason = "gonk-meter has not resolved this project's config yet"
		if c.LooksInvalid {
			c.Reason = "gonk-meter has not resolved this project's config yet (and it does not appear to parse)"
		}
		return c
	}

	// Meter has spoken. Record its verdict.
	switch mr.State {
	case meterapi.StateInvalid:
		c.State, c.Reason = StateInvalid, mr.Error
		return c
	case meterapi.StateDisabled:
		// ADR-002's invariant, carried onto the wire: DisabledReason is non-empty
		// iff the project is disabled.
		c.State, c.Reason = StateDisabled, mr.DisabledReason
		return c
	case meterapi.StateKeyMissing:
		c.State = StateKeyMissing
		c.Reason = "gonk-meter has not provisioned this project's LiteLLM key yet"
		return c
	case meterapi.StateActive:
		// fall through
	default:
		// An unknown state from a newer meter. Fail closed rather than guess.
		c.State = StateUnsynced
		c.Reason = fmt.Sprintf("gonk-meter returned an unknown state %q", mr.State)
		return c
	}

	// Active. The remaining question is ours: is the repo scaffolded?
	if !obs.AgentDirPresent {
		c.State = StatePending
		c.Reason = "no " + AgentDir + "/ yet: scaffold merge request pending"
		return c
	}
	c.State = StateValid
	return c
}

// hashConfig identifies a config version for the order payload (so a session
// records which config authorized it) and for the reconcile short-circuit.
func hashConfig(raw []byte) string {
	if raw == nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./pkg/intake/ -race -count=1 -v`
Expected: PASS.

Note what is **no longer** here, and why that is correct: intake has no budget test, no ladder test, and no `.nan` crash test. Those moved to meter with the resolver. Intake's remaining job on hostile bytes is to **cap the fetch** and **hand them to meter without interpreting them** — which is a smaller, sharper trust boundary than it had before.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && go vet ./... && go test ./pkg/intake/ -race -count=1
git add pkg/intake && git commit -m "feat(intake): project state machine (pure; meter owns resolution, intake records the verdict)"
```

---

### Task 6: `pkg/meterapi` — land the shared contract (DO NOT AUTHOR IT)

**Files:** Create `pkg/meterapi/meterapi.go`, `pkg/meterapi/meterapi_test.go`, `pkg/meterapi/testdata/contract.sha256` — **all copied verbatim from Plan 03, Task 0.**

**This package is not defined here.** `pkg/meterapi` is the wire contract between gonk-intake, gonk-meter, and the pack, and **Plan 03 owns it** — meter is the server; the clients conform. Plan 02 simply executes first, so Plan 02 is what physically lands the files.

> **Read `docs/superpowers/plans/2026-07-13-plan-03-gonk-meter.md`, Task 0.** The Go source there is normative. Copy it. Do not improve it, rename anything in it, or add a field to it.
>
> **If you find you need a field that is not there, stop.** That is a change to *meter's* contract. Make it in Plan 03's Task 0 first (updating the sha256 gate in the same commit), then come back. This is not bureaucracy: the last time these two plans each derived their own types, they produced two incompatible seams, which is the conflict this task exists to have already resolved.

- [ ] **Step 1: Copy the normative source** from Plan 03 Task 0 into `pkg/meterapi/meterapi.go`.

- [ ] **Step 2: Copy the tests** from Plan 03 Task 0 into `pkg/meterapi/meterapi_test.go`.

The ones that matter most to *this* plan:

- `TestUnlimitedBudgetSerializesAsNull` — PLAN.md's highest-value carry-forward. `json.Marshal(math.Inf(1))` **returns an error**, so an unlimited project would fail to serialize *at all*. `meterapi.Budget` (`*float64`/`*int64`, `nil` == unlimited) is where that is solved, once, for both sides.
- `TestZeroBudgetIsNotAnEmptyBudget` — an empty `Budget{}` is all-nil, and **nil means UNLIMITED**. For a project you are *disabling*, that is the exact opposite of fail-closed. Use `meterapi.ZeroBudget()`.
- `TestDecideRequestHasNoAttemptField` — intake never calls `/decide` at all, but this test protects the seam from a well-meaning future edit: a caller-supplied attempt count is a forgery vector for climbing the ladder straight to the most expensive rung.
- `TestProjectPathEscapes` — `ProjectPath("group/repo")` is `/v1/projects/group%2Frepo`. Intake **must** use it; an unescaped slash routes to a different handler, or to none.

- [ ] **Step 3: Arm the drift gate**

```bash
mkdir -p pkg/meterapi/testdata
sha256sum pkg/meterapi/meterapi.go | cut -d' ' -f1 > pkg/meterapi/testdata/contract.sha256
```

`TestContractIsFrozen` then fails on **any** edit to `meterapi.go`. That is intentional. It is a contract between three plans, and a change to it is a change to all three.

- [ ] **Step 4: Run and commit**

```bash
go test ./pkg/meterapi/ -race -count=1 -v
gofmt -l . && go vet ./... && go test ./pkg/meterapi/ -race -count=1
git add pkg/meterapi && git commit -m "feat(meterapi): land the intake<->meter wire contract (normative source: plan 03 task 0)"
```

---

### Task 7: `pkg/intake` — reconciler, derived cache, meter registration

**Files:** Create `pkg/intake/cache.go`, `pkg/intake/meterclient.go`,
`pkg/intake/reconcile.go`, `pkg/intake/reconcile_test.go`,
`pkg/intake/meterclient_test.go`.

Spec 5.2: **reconciliation is the correctness path; webhooks are the latency optimization.** Every N minutes (and on demand) intake lists the bot's memberships, fetches `.gonk.yml`, **registers it with meter (raw)**, idempotently provisions or repairs the project webhook, and classifies the result.

Spec goal 6 / 4.2: the cache is **derived only**. Nothing is persisted; after a restart, one reconcile pass rebuilds the entire world from GitLab + meter. If you find yourself wanting a database, you have taken a wrong turn.

**The per-project order of operations changed with Conflict A, and the order matters:**

```
observe GitLab  ->  PUT raw .gonk.yml to meter  ->  classify(observation, meter's response)  ->  cache
                    ^^^^^^^^^^^^^^^^^^^^^^^^^^
                    meter validates + resolves + provisions the key.
                    Intake cannot classify BEFORE this, because `invalid`,
                    `disabled` and `key-missing` ARE meter's answers.
```

If the meter call fails, the project is `unsynced` and **nothing is dispatched for it**. That is the same fail-closed rule as before; it is just that the thing we are missing is now the *policy*, not merely a *key*.

- [ ] **Step 1: Write the failing test** — `pkg/intake/reconcile_test.go`

```go
package intake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// fakeMeter is the SERVER side of pkg/meterapi. Plan 03 implements the real one
// against the same types, so this fake and that server cannot drift apart on the
// wire shape -- only on behaviour, which is what Plan 06 is for.
type fakeMeter struct {
	srv      *httptest.Server
	puts     []meterapi.ProjectRequest
	deletes  []string
	fail     bool   // 500 on everything
	invalid  bool   // 422: the project's yaml will not load
	state    meterapi.State
	authSeen string
}

func newFakeMeter(t *testing.T) *fakeMeter {
	m := &fakeMeter{state: meterapi.StateActive}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.authSeen = r.Header.Get("Authorization")
		if r.URL.Path == meterapi.HealthzPath {
			w.WriteHeader(200)
			return
		}
		if m.fail {
			http.Error(w, `{"error":"nope"}`, 500)
			return
		}
		if r.Method == http.MethodDelete {
			m.deletes = append(m.deletes, r.URL.Path)
			w.WriteHeader(204)
			return
		}
		var req meterapi.ProjectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad"}`, 400)
			return
		}
		m.puts = append(m.puts, req)

		resp := meterapi.ProjectResponse{
			Project: req.Project, Rig: req.Rig, State: m.state,
			ConfigHash: "sha256:fake",
			Effective: &meterapi.Effective{
				Enabled: true,
				Actions: meterapi.Actions{Triage: true},
				Ladder:  []string{"qwen-local"},
				Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
			},
			KeyRef: meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		}
		code := 200
		if m.invalid {
			code = 422
			resp.State = meterapi.StateInvalid
			resp.Effective = nil
			resp.Error = ".gonk.yml: unknown field 'banana'"
			resp.KeyRef = meterapi.KeyRef{}
			resp.Budget = meterapi.ZeroBudget()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func newReconciler(t *testing.T, gl *glabtest.Server, m *fakeMeter) *Reconciler {
	t.Helper()
	return &Reconciler{
		GL:        gl.Client(),
		Meter:     NewMeterClient(m.srv.URL, "meter-token", nil),
		Cache:     NewCache(),
		BotUserID: 7,
		HookURL:   "https://gonk.orac.local/hook/gitlab",
		HookToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TokenGen:  "1",
		Obs:       NopObserver{},
	}
}

// The registration must carry the RAW yaml, byte for byte. This is Conflict A on
// the wire: if intake ever starts sending a resolved policy instead, meter has
// lost its single-resolver property and two components can disagree about a
// budget ceiling.
func TestReconcileRegistersRAWConfigWithMeter(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 7, Username: "gonk"}
	raw := "version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte(raw))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)

	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if sum.Projects != 1 || sum.Errors != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(m.puts) != 1 {
		t.Fatalf("meter puts = %d, want 1", len(m.puts))
	}
	got := m.puts[0]
	if got.GonkYML != raw {
		t.Fatalf("meter got %q, want the RAW bytes %q -- intake must not transform the config", got.GonkYML, raw)
	}
	if got.Project != "group/repo" || got.ProjectID != p.ID || got.Rig != "group-repo" || got.DefaultBranch != "main" {
		t.Fatalf("GitLab metadata missing from the registration: %+v", got)
	}
	if m.authSeen != "Bearer meter-token" {
		t.Fatalf("auth = %q; the meter API is not unauthenticated", m.authSeen)
	}
	e, ok := r.Cache.Get(p.ID)
	if !ok || e.State() != StateValid || !e.Dispatchable() {
		t.Fatalf("cache entry = %+v ok=%v", e, ok)
	}
	// The Effective came off the wire, not out of a local resolver.
	if e.Classification.Meter == nil || !e.Classification.MayTriage() {
		t.Fatalf("effective policy must come from meter's response: %+v", e.Classification)
	}

	hooks, _ := gl.Client().ListHooks(context.Background(), p.ID)
	if len(hooks) != 1 || !hooks[0].IssuesEvents || !hooks[0].NoteEvents || !hooks[0].MergeRequestsEvents || hooks[0].PushEvents {
		t.Fatalf("hook = %+v", hooks)
	}
}

// *** THE REGRESSION GUARD FOR CONFLICT A. ***
// Intake must not import a resolver, and must not ship one.
func TestIntakeDoesNotResolve(t *testing.T) {
	// A compile-time-ish assertion, enforced by the DoD grep as well:
	//   grep -rn "gonkcfg.Resolve" pkg/intake/  -> MUST BE EMPTY
	// This test documents the rule where an implementer will actually read it.
	// If you are here because you "just need the ladder", the ladder is in
	// Classification.Meter.Effective.Ladder. Use it.
	t.Log("intake resolves nothing; see the DoD grep")
}

// Idempotency is the whole game: reconcile runs every 10 minutes forever.
func TestReconcileIsIdempotent(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(gl.Requests())
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, req := range gl.Requests()[before:] {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "PUT") {
			t.Errorf("second pass wrote to GitLab: %s", req)
		}
	}
	if len(m.puts) != 1 {
		t.Errorf("meter registered %d times for an unchanged config hash, want 1", len(m.puts))
	}
}

// A hook provisioned with an old secret generation must be repaired, or a secret
// rotation silently deafens gonk (GitLab never returns hook tokens, so the
// generation marker in the URL is how staleness is detected -- ADR-003).
func TestReconcileRepairsStaleHook(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	_, _ = gl.Client().CreateHook(context.Background(), p.ID, glab.HookOptions{
		URL: "https://gonk.orac.local/hook/gitlab?gen=0", Token: "old", IssuesEvents: true,
	})
	r := newReconciler(t, gl, newFakeMeter(t))
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooks, _ := gl.Client().ListHooks(context.Background(), p.ID)
	if len(hooks) != 1 {
		t.Fatalf("hook was duplicated instead of repaired: %+v", hooks)
	}
	if !strings.Contains(hooks[0].URL, "gen=1") || !hooks[0].NoteEvents {
		t.Fatalf("hook not repaired: %+v", hooks[0])
	}
	if got := gl.HookToken(p.ID, hooks[0].ID); got == "old" {
		t.Fatal("hook token was not re-set on rotation")
	}
}

// A 422 is the PROJECT's fault, not ours. Meter has already deleted the key.
// Intake records `invalid`, dispatches nothing, and keeps the error text so the
// onboarding/comment flow can show it to a human.
func TestReconcileInvalidConfigIsRecordedNotFatal(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nbanana: true\n"))
	m := newFakeMeter(t)
	m.invalid = true
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("a 422 must not fail the pass: %v", err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateInvalid {
		t.Fatalf("state = %q, want invalid", e.State())
	}
	if e.Dispatchable() {
		t.Fatal("an invalid project must not be dispatchable")
	}
	if !strings.Contains(e.Classification.Reason, "banana") {
		t.Fatalf("meter's error text must survive for the human: %q", e.Classification.Reason)
	}
}

// No unmetered work: if meter is unreachable, we do not know the project's
// policy, so nothing runs. We must NOT fall back to a local resolve.
func TestReconcileMeterFailureBlocksDispatch(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.fail = true
	r := newReconciler(t, gl, m)

	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("a meter failure must not abort the whole pass: %v", err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateUnsynced {
		t.Fatalf("state = %q, want unsynced", e.State())
	}
	if e.Dispatchable() {
		t.Fatal("a project whose policy we could not resolve must not be dispatchable")
	}
}

// key-missing: meter resolved the config but LiteLLM was down, so there is no
// virtual key. Meter would defer anyway; do not churn a bead for it.
func TestReconcileKeyMissingIsNotDispatchable(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nactions: {triage: true}\nladder: [qwen-local]\n"))
	p.PutFile(".agent/context.md", []byte("hi"))
	m := newFakeMeter(t)
	m.state = meterapi.StateKeyMissing
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := r.Cache.Get(p.ID)
	if e.State() != StateKeyMissing || e.Dispatchable() {
		t.Fatalf("entry = %+v", e)
	}
}

// De-onboarding: the bot is removed from the project (spec 5.1). Intake DELETEs
// the registration; meter disables the project and deletes the key.
func TestReconcileDeletesProjectsTheBotLeft(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	p.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	m := newFakeMeter(t)
	r := newReconciler(t, gl, m)
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	gl.RemoveProject(p.ID) // no longer a membership
	if _, err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Cache.Get(p.ID); ok {
		t.Fatal("de-onboarded project must leave the cache")
	}
	if len(m.deletes) != 1 || !strings.Contains(m.deletes[0], "group%2Frepo") {
		t.Fatalf("de-onboarding must DELETE the meter registration (URL-escaped): %v", m.deletes)
	}
}

// One bad project must not stop the others.
func TestReconcileContinuesPastOneProjectError(t *testing.T) {
	gl := glabtest.New(t)
	p1 := gl.AddProject("group/a", glab.AccessMaintainer)
	p2 := gl.AddProject("group/b", glab.AccessMaintainer)
	p2.PutFile(".gonk.yml", []byte("version: 1\nenabled: true\nladder: [qwen-local]\n"))
	gl.FailAlways("GET", fmt.Sprintf("/api/v4/projects/%d/repository/files", p1.ID), 500)
	r := newReconciler(t, gl, newFakeMeter(t))
	sum, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce = %v", err)
	}
	if sum.Errors != 1 || sum.Projects != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	if _, ok := r.Cache.Get(p2.ID); !ok {
		t.Fatal("the healthy project must still have been reconciled")
	}
}
```

(Add `glabtest.RemoveProject(id)` and `FailAlways(method, pathPrefix, status)` to the fake — small additions to Task 2's file.)

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/intake/ -run Reconcile -v` — FAIL: undefined `Reconciler`, `NewCache`, `NewMeterClient`.

- [ ] **Step 3: Implement `pkg/intake/cache.go`**

```go
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
	// ScaffoldFiredAt suppresses re-firing the .agent/ scaffold order every
	// reconcile pass while the first one is still in flight. Best-effort only:
	// the real idempotency guarantee is the deterministic bead anchor, which the
	// Gas City controller must honour (see "Cross-plan contracts").
	ScaffoldFiredAt time.Time
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
}

func NewCache() *Cache { return &Cache{m: make(map[int64]Entry)} }

func (c *Cache) Get(id int64) (Entry, bool) { /* RLock */ }
func (c *Cache) Put(id int64, e Entry)      { /* Lock */ }
func (c *Cache) Delete(id int64)            { /* Lock */ }
func (c *Cache) IDs() []int64               { /* snapshot, to find projects that vanished */ }

// CountByState powers the project-state gauge (spec 8). Pre-seed every state to
// zero so a dashboard shows 0 rather than nothing.
func (c *Cache) CountByState() map[State]int { /* ... */ }
```

- [ ] **Step 4: Implement `pkg/intake/meterclient.go`**

```go
package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// MeterClient is intake's half of the pkg/meterapi seam. Plan 03 implements the
// server. Neither side re-derives the JSON shape.
type MeterClient struct {
	BaseURL string
	token   string // bearer, read from a FILE by main.go -- never an env value
	HTTP    *http.Client
}

func NewMeterClient(baseURL, token string, hc *http.Client) *MeterClient {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &MeterClient{BaseURL: strings.TrimSuffix(baseURL, "/"), token: token, HTTP: hc}
}

// ErrInvalidConfig is a 422: the PROJECT's .gonk.yml will not load. It is not an
// error in the HTTP sense and it is emphatically not intake's bug -- meter has
// successfully and idempotently recorded the project as invalid and deleted its
// virtual key. The Response is a full ProjectResponse carrying meter's error
// text, which the onboarding/comment flow shows to a human.
type ErrInvalidConfig struct {
	Response *meterapi.ProjectResponse
}

func (e *ErrInvalidConfig) Error() string {
	return "meter: project config is invalid: " + e.Response.Error
}

// Register pushes the RAW .gonk.yml. Meter validates, resolves, and provisions
// the key -- intake does none of those things (see "Division of responsibility").
//
// It is an idempotent upsert: the same bytes may be PUT every reconcile pass.
//
// Failure is NOT fatal to the pass: the caller records the project as `unsynced`,
// dispatches nothing for it, and retries next pass.
func (m *MeterClient) Register(ctx context.Context, req meterapi.ProjectRequest) (*meterapi.ProjectResponse, error) {
	var out meterapi.ProjectResponse
	code, err := m.do(ctx, http.MethodPut, meterapi.ProjectPath(req.Project), req, &out)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusUnprocessableEntity: // 422 -- the project's yaml is bad
		return nil, &ErrInvalidConfig{Response: &out}
	case code == http.StatusOK:
		return &out, nil
	}
	// 400 means OUR request was malformed. That is an intake bug and it must be
	// loud: retrying it will never help.
	return nil, fmt.Errorf("meter: PUT %s: unexpected status %d", req.Project, code)
}

// Deregister de-onboards a project: meter disables it and deletes the virtual
// key. Idempotent -- deleting an unknown project is a 204, not a 404.
func (m *MeterClient) Deregister(ctx context.Context, project string) error {
	code, err := m.do(ctx, http.MethodDelete, meterapi.ProjectPath(project), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusOK {
		return fmt.Errorf("meter: DELETE %s: %d", project, code)
	}
	return nil
}

func (m *MeterClient) Healthy(ctx context.Context) error { /* GET /healthz -> 200 */ }

// do sends one request with the bearer token, caps the response body, and
// decodes it. It NEVER puts the token in an error string: errors get logged.
func (m *MeterClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("meter: encode: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, m.BaseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	r.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.HTTP.Do(r)
	if err != nil {
		return 0, fmt.Errorf("meter: %s %s: %w", method, path, err) // no token in the error
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("meter: read: %w", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			// A 5xx may not be JSON at all; that is fine, the status carries it.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == 422 {
				return resp.StatusCode, fmt.Errorf("meter: decode: %w", err)
			}
		}
	}
	if resp.StatusCode >= 500 {
		var er meterapi.ErrorResponse
		_ = json.Unmarshal(raw, &er)
		return resp.StatusCode, fmt.Errorf("meter: %s %s: %d: %s", method, path, resp.StatusCode, er.Error)
	}
	return resp.StatusCode, nil
}
```

`meterclient_test.go` must cover: the bearer header is sent; **the token never appears in any error string**; a 422 returns `*ErrInvalidConfig` with the message intact; a 500 is a plain error; `Register` uses `meterapi.ProjectPath` so `group/repo` is escaped to `group%2Frepo`; and an oversized response body is capped, not read into the heap.

- [ ] **Step 5: Implement `pkg/intake/reconcile.go`**

```go
package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

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
}

// Onboarder opens the deterministic onboarding MR (Task 8). Nil is legal: the
// reconciler simply does not onboard.
type Onboarder interface {
	Onboard(ctx context.Context, p glab.Project) error
	// Declined reports whether an onboarding MR was closed unmerged and the bot
	// has not been re-invited since (spec 5.3, AD-3).
	Declined(ctx context.Context, p glab.Project) (bool, error)
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
		seen[p.ID] = true
		if err := r.reconcileProject(ctx, p); err != nil {
			sum.Errors++
			r.Log.Error("reconcile project failed", "project", p.PathWithNamespace, "err", err)
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
				r.Log.Warn("failed to deregister on de-onboard", "project", e.Project.PathWithNamespace, "err", err)
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

type Reconciler struct {
	GL        GitLab
	Meter     *MeterClient
	Cache     *Cache
	Onboarder Onboarder
	Dispatch  *Dispatch
	Obs       Observer
	Log       *slog.Logger

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
	MeterResyncInterval time.Duration

	// NOTE what is NOT here any more: `Instance gonkcfg.Policy` and
	// `GroupPolicy func(string) gonkcfg.Policy`. Intake does not hold operator
	// policy and does not resolve. Meter does. (Conflict A.)
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

func (r *Reconciler) reconcileProject(ctx context.Context, p glab.Project) error {
	if p.Archived {
		r.Cache.Delete(p.ID)
		return nil
	}
	obs, err := r.observe(ctx, p)   // GitLab: membership, .gonk.yml bytes, .agent/, decline
	if err != nil {
		return err
	}

	// The webhook is the latency path; failing to provision it is logged and
	// retried, never fatal (reconciliation still works without it).
	if err := r.ensureHook(ctx, p); err != nil {
		r.Log.Warn("hook provisioning failed", "project", p.PathWithNamespace, "err", err)
	}

	// REGISTER WITH METER BEFORE CLASSIFYING. `invalid`, `disabled` and
	// `key-missing` are meter's answers -- intake cannot compute them.
	var mr *meterapi.ProjectResponse
	if obs.ConfigBytes != nil {
		mr, err = r.register(ctx, p, obs)
		if err != nil {
			var inv *ErrInvalidConfig
			if errors.As(err, &inv) {
				mr = inv.Response // a 422 IS an answer: state=invalid, key deleted
				r.Obs.MeterPush("invalid")
			} else {
				// Unreachable / 5xx / our bug. We do not know this project's policy.
				// Classify will make it `unsynced`, and nothing will be dispatched.
				r.Log.Warn("meter registration failed", "project", p.PathWithNamespace, "err", err)
				r.Obs.MeterPush("error")
			}
		} else {
			r.Obs.MeterPush("ok")
		}
	}

	cls := Classify(obs, mr)

	prev, hadPrev := r.Cache.Get(p.ID)
	entry := Entry{Project: p, Classification: cls, LastReconcile: time.Now()}
	if hadPrev {
		entry.ScaffoldFiredAt = prev.ScaffoldFiredAt
	}

	if cls.MayOnboard() && r.Onboarder != nil {
		if err := r.Onboarder.Onboard(ctx, p); err != nil {
			r.Obs.OnboardingResult("error")
			r.Log.Warn("onboarding failed", "project", p.PathWithNamespace, "err", err)
		}
	}

	r.Cache.Put(p.ID, entry)

	// spec 5.3: a `pending` project gets the .agent/ scaffold order -- the one
	// metered action allowed while pending.
	if entry.Classification.MayScaffold() && r.Dispatch != nil &&
		time.Since(entry.ScaffoldFiredAt) > time.Hour {
		if err := r.Dispatch.FireScaffold(ctx, entry); err != nil {
			r.Log.Warn("scaffold dispatch failed", "project", p.PathWithNamespace, "err", err)
		} else {
			entry.ScaffoldFiredAt = time.Now()
			r.Cache.Put(p.ID, entry)
		}
	}
	return nil
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
```

Implementer notes:

- **ConfigHash short-circuit:** before calling `r.Meter.Register`, skip the call when `hadPrev && prev.Classification.ConfigHash == cls-to-be's hash && prev.Classification.Meter != nil` — carry the previous `Meter` response forward. That is what makes `TestReconcileIsIdempotent` pass. (Meter is idempotent on `config_hash` anyway; this just saves the round trip.)
  **But re-register at least every `MeterResyncInterval` (default 1h) even when the hash is unchanged**, because meter's answer can change *without the project's config changing* — an operator flipping the instance kill switch, or tightening a group ceiling, changes `Effective` for a project whose `.gonk.yml` never moved. Meter's own reresolve loop handles its side; this is what refreshes *intake's* copy.
- **De-onboarding** (a project that vanished from the membership list) calls `r.Meter.Deregister(ctx, path)` and then drops the cache entry.
- `Observer` gains `MeterPush(result)` with `result` ∈ `ok | invalid | error`.

- [ ] **Step 6: Run the tests**

Run: `go test ./pkg/intake/ -race -count=1 -v` → PASS (state machine + all reconcile tests).

- [ ] **Step 7: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1
git add pkg/intake pkg/glab && git commit -m "feat(intake): reconciler, derived cache, and raw-config registration with gonk-meter"
```

---

### Task 8: `pkg/intake` — the deterministic onboarding MR

**Files:** Create `pkg/intake/render.go`, `pkg/intake/onboard.go`,
`pkg/intake/testdata/onboarding-mr.golden.md`, `pkg/intake/render_test.go`,
`pkg/intake/onboard_test.go`.

Spec 5.3. **Zero tokens. No model call. No agent session.** If any part of this
task reaches for an LLM, it is wrong: the whole point of the Renovate model is
that a maintainer can read the MR and know exactly what merging it authorizes,
and that the same invite always produces the same MR.

The MR carries (a) a conservative default `.gonk.yml` — triage only, local rungs
only, **zero cloud budget** — and (b) an explanation *re-rendered from the actual
config values*, so the prose cannot drift from the settings.

- [ ] **Step 1: Write the failing render test** — `pkg/intake/render_test.go`

```go
package intake

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/gonkcfg"
)

// The template must be a valid, enabled config — an onboarding MR that lands a
// config gonk then rejects would be a spectacular own goal.
func TestDefaultConfigIsValidAndEnabled(t *testing.T) {
	cfg, err := gonkcfg.Load(DefaultConfigYAML)
	if err != nil {
		t.Fatalf("the config we ask projects to merge does not validate: %v", err)
	}
	eff := gonkcfg.Resolve(gonkcfg.Policy{}, gonkcfg.Policy{}, *cfg)
	if !eff.Enabled {
		t.Fatalf("merging the default config would leave the project disabled: %s", eff.DisabledReason)
	}
	if !eff.Actions.Triage || eff.Actions.Pipelines || eff.Actions.Features {
		t.Fatalf("default must be triage-only: %+v", eff.Actions)
	}
	if eff.Budget.MonthlyCostUSD != 0 {
		t.Fatalf("default cloud budget must be 0, got %v", eff.Budget.MonthlyCostUSD)
	}
	if len(eff.Ladder) != 1 || eff.Ladder[0] != "qwen-local" {
		t.Fatalf("default ladder must be local-only: %v", eff.Ladder)
	}
}

// Determinism: same input, same bytes, every time. This is what makes the
// onboarding MR reviewable and reproducible.
func TestRenderIsDeterministic(t *testing.T) {
	a, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		b, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatal("render is not deterministic")
		}
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	got, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/onboarding-mr.golden.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("MR body drifted from the golden file.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Spec 5.3: the MR must explain "every configurable key". This gate makes that
// enforceable rather than aspirational: it reads the PUBLISHED schema and fails
// if a key exists that the MR body never mentions. Adding a key to .gonk.yml
// without documenting it here fails CI.
func TestMRBodyDocumentsEveryConfigKey(t *testing.T) {
	raw, err := os.ReadFile("../../docs/schemas/gonk-config.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
	if err != nil {
		t.Fatal(err)
	}
	for top, sub := range schema.Properties {
		if top == "version" {
			continue // schema plumbing, not a user-facing knob
		}
		if !strings.Contains(body, top) {
			t.Errorf("MR body never mentions config key %q (spec 5.3: explain every configurable key)", top)
		}
		for k := range sub.Properties {
			if !strings.Contains(body, k) {
				t.Errorf("MR body never mentions config key %q.%q", top, k)
			}
		}
	}
}

// The prose must be generated from the values, not typed alongside them.
func TestRenderedBodyQuotesTheActualValues(t *testing.T) {
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, string(DefaultConfigYAML)) {
		t.Fatal("the MR body must embed the exact .gonk.yml it commits, byte for byte")
	}
	for _, want := range []string{"$0", "qwen-local", "@gonk", "group/repo"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q", want)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./pkg/intake/ -run 'Render|DefaultConfig|MRBody' -v`
Expected: FAIL — undefined `DefaultConfigYAML`, `RenderOnboardingMR`.

- [ ] **Step 3: Implement `pkg/intake/render.go`**

```go
package intake

import (
	"bytes"
	"fmt"
	"text/template"
)

// OnboardBranch is the branch the onboarding MR is opened from (spec, Appendix A).
const OnboardBranch = "gonk/onboard"

// DefaultConfigYAML is what the onboarding MR commits: the most conservative
// configuration that still does something (spec 5.3) — triage only, local rungs
// only, zero cloud budget. A maintainer who merges this without reading it has
// authorized nothing that costs money.
//
// It is a byte-exact constant, not a marshaled struct: the comments ARE the
// product, and a YAML marshaler would strip them. TestDefaultConfigIsValidAndEnabled
// keeps it honest against the schema and the resolver.
var DefaultConfigYAML = []byte(`# gonk configuration. https://gitlab.orac.local/agentic/gonk-project
# Schema: docs/schemas/gonk-config.v1.schema.json
#
# Removing this file, or removing the gonk bot from this project, de-onboards it.
version: 1

# Master switch for this project.
enabled: true

# What gonk is allowed to do here. Only triage is implemented today.
actions:
  triage: true
  pipelines: false
  features: false

# Hard ceilings. These only ever tighten: the instance and group may impose a
# lower limit, never a higher one.
budget:
  monthly_cost_usd: 0      # paid-model spend. 0 = no paid models, ever.
  monthly_tokens: "50M"
  per_task_tokens: "2M"

# Models gonk may use here, cheapest first. This is an allow-list: a model that
# is not listed cannot be used, whatever the budget says. Cloud rungs must be
# added deliberately (and need a non-zero monthly_cost_usd to be reachable).
ladder:
  - qwen-local

# resume: continue an interrupted session where it left off. fresh: start over.
continuity: resume

triage:
  label_prefix: "gonk::"
  respond_to_mentions: true

# Bot-authored commits carry git trailers naming what generated them.
provenance:
  commit_trailers: true
  include_usage: false     # true also records token/cost in commit trailers
`)

// OnboardingContext is everything the MR body varies on. Keep it small: every
// field here is a way for two renders to differ.
type OnboardingContext struct {
	Project     string // path_with_namespace
	BotUsername string
	Version     string // gonk version, for the provenance trailer
}

// RenderOnboardingMR produces the MR description. It is a pure function of its
// input and the default config — DETERMINISTIC, NO MODEL CALL (spec 5.3). The
// consequences it lists are rendered from the config values themselves, so the
// text cannot drift from the settings.
func RenderOnboardingMR(c OnboardingContext) (string, error) {
	var buf bytes.Buffer
	if err := onboardingTmpl.Execute(&buf, struct {
		OnboardingContext
		Config string
	}{c, string(DefaultConfigYAML)}); err != nil {
		return "", fmt.Errorf("intake: render onboarding MR: %w", err)
	}
	return buf.String(), nil
}

var onboardingTmpl = template.Must(template.New("onboarding").Parse(
	`# Enable gonk on {{.Project}}

gonk is an automation bot. It was invited to **{{.Project}}**, and this merge
request is how it asks permission to actually do anything. Nothing happens until
this is merged. Closing this merge request declines gonk; it will not ask again
unless it is re-invited.

**No language model was used to produce this merge request.** It is rendered from
a template, and the explanation below is generated from the exact configuration
values committed here, so the two cannot disagree.

## What merging this authorizes

| Key | Value | What it means |
|---|---|---|
| ` + "`enabled`" + ` | ` + "`true`" + ` | gonk acts on this project. Set to ` + "`false`" + ` to switch it off without removing the file. |
| ` + "`actions.triage`" + ` | ` + "`true`" + ` | On a new issue, gonk reads it, applies labels, and posts one analysis comment. |
| ` + "`actions.pipelines`" + ` | ` + "`false`" + ` | gonk will not touch failing pipelines. (Not implemented yet.) |
| ` + "`actions.features`" + ` | ` + "`false`" + ` | gonk will not decompose designs into work items. (Not implemented yet.) |
| ` + "`budget.monthly_cost_usd`" + ` | ` + "`0`" + ` | **$0.** No paid model can be used on this project. Raising this is the only way to spend money here. |
| ` + "`budget.monthly_tokens`" + ` | ` + "`50M`" + ` | Ceiling on tokens per calendar month across all of gonk's work here. |
| ` + "`budget.per_task_tokens`" + ` | ` + "`2M`" + ` | Ceiling for a single work item, so one runaway task cannot eat the month. |
| ` + "`ladder`" + ` | ` + "`[qwen-local]`" + ` | The only model gonk may use here: the local one. It costs no money. Cloud models must be listed explicitly to be usable. |
| ` + "`continuity`" + ` | ` + "`resume`" + ` | An interrupted session resumes rather than starting over. |
| ` + "`triage.label_prefix`" + ` | ` + "`gonk::`" + ` | Every label gonk creates starts with this, so its labels are always distinguishable from yours. |
| ` + "`triage.respond_to_mentions`" + ` | ` + "`true`" + ` | Mentioning ` + "`@{{.BotUsername}}`" + ` in an issue comment gets a reply in that thread. |
| ` + "`provenance.commit_trailers`" + ` | ` + "`true`" + ` | Commits gonk authors carry a trailer naming what generated them. |
| ` + "`provenance.include_usage`" + ` | ` + "`false`" + ` | Token counts and cost are NOT written into commit trailers. Set to ` + "`true`" + ` to publish them in git history. |
| ` + "`schedule.quiet_hours`" + ` | unset | Optional. Set (with ` + "`schedule.timezone`" + `) to hold gonk's work outside a window, e.g. ` + "`\"22:00-07:00\"`" + `. |
| ` + "`schedule.timezone`" + ` | unset | IANA zone name for ` + "`quiet_hours`" + `, e.g. ` + "`America/New_York`" + `. |
| ` + "`version`" + ` | ` + "`1`" + ` | Config schema version. |

## What happens after you merge

1. gonk opens a second merge request adding a ` + "`.agent/`" + ` directory: a short,
   written-by-reading-this-repo description of what the project is and how it is
   built. That is the first thing gonk does that uses a model, and it is
   authorized by the budget you just merged.
2. Until that lands, gonk does nothing else. Triage starts once ` + "`.agent/`" + ` exists.
3. gonk never merges anything itself, ever, and never pushes outside ` + "`gonk/*`" + `
   branches.

## The file this adds

` + "```yaml\n{{.Config}}```" + `

## Turning it off

Remove this file, or remove ` + "`@{{.BotUsername}}`" + ` from the project's members. Either alone
de-onboards gonk. Setting ` + "`enabled: false`" + ` keeps the config but stops all activity.

---
Generated-By: gonk/{{.Version}} (deterministic onboarding; no model was used)
`))
```

- [ ] **Step 4: Generate the golden file, then eyeball it**

```bash
go test ./pkg/intake/ -run TestRenderMatchesGolden -v 2>&1 | head -60
```

Expected: FAIL (no golden file). Write it from the render output:

```bash
cat > /tmp/gen_golden.go <<'EOF'
//go:build ignore
package main
EOF
```

Simpler and reviewable: add a one-shot test helper instead of a script —

```go
// render_golden_test.go — run with: go test ./pkg/intake/ -run TestWriteGolden -args -update
func TestWriteGolden(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") == "" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate")
	}
	body, err := RenderOnboardingMR(OnboardingContext{Project: "group/repo", BotUsername: "gonk", Version: "v0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/onboarding-mr.golden.md", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

Run: `mkdir -p pkg/intake/testdata && UPDATE_GOLDEN=1 go test ./pkg/intake/ -run TestWriteGolden`

**Then read `pkg/intake/testdata/onboarding-mr.golden.md` end to end.** This text
is the product: it is what a maintainer of someone else's repo reads before
deciding whether to let a bot into it. If it is not clear, honest, and complete,
fix the template, not the golden file.

- [ ] **Step 5: Run the render tests**

Run: `go test ./pkg/intake/ -run 'Render|DefaultConfig|MRBody' -race -v`
Expected: PASS (5 tests, including the schema-coverage gate).

- [ ] **Step 6: Write the failing onboarder test** — `pkg/intake/onboard_test.go`

```go
package intake

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func newOnboarder(gl *glabtest.Server) *GitLabOnboarder {
	return &GitLabOnboarder{
		GL: gl.Client(), BotUserID: 7, BotUsername: "gonk", Version: "v0", Obs: NopObserver{},
	}
}

func TestOnboardOpensMRWithTheConfig(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()

	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 || mrs[0].TargetBranch != "main" {
		t.Fatalf("mrs = %+v", mrs)
	}
	if !strings.Contains(mrs[0].Description, "monthly_cost_usd") {
		t.Fatal("MR description is not the rendered explanation")
	}
	raw, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, OnboardBranch, 65536)
	if err != nil || string(raw) != string(DefaultConfigYAML) {
		t.Fatalf("branch does not carry the exact default config: %v", err)
	}
	// The commit must not land on the default branch. Only a human merging can
	// do that.
	if _, err := gl.Client().GetRawFile(ctx, p.ID, ConfigPath, "main", 65536); !glab.IsNotFound(err) {
		t.Fatal("onboarding must not write to the default branch")
	}
}

func TestOnboardIsIdempotent(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatalf("second Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("opened %d MRs; onboarding must never nag", len(mrs))
	}
}

// Spec 5.3: closing the MR unmerged is a decline. Do not re-open it.
func TestDeclinedIsNotReopened(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	gl.SetMRState(p.ID, 1, "closed", time.Now())

	declined, err := o.Declined(ctx, p.Project)
	if err != nil || !declined {
		t.Fatalf("Declined = %v, %v; want true", declined, err)
	}
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatal("a declined project was pestered with a second onboarding MR")
	}
}

// AD-3: re-inviting the bot (a fresh membership, created after the decline)
// re-opens the question.
func TestReinviteClearsDecline(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	o := newOnboarder(gl)
	ctx := context.Background()
	if err := o.Onboard(ctx, p.Project); err != nil {
		t.Fatal(err)
	}
	closedAt := time.Now().Add(-time.Hour)
	gl.SetMRState(p.ID, 1, "closed", closedAt)
	joined := time.Now()
	gl.AddMember(p.ID, glab.Member{ID: 7, Username: "gonk", AccessLevel: glab.AccessDeveloper, CreatedAt: &joined})

	declined, err := o.Declined(ctx, p.Project)
	if err != nil {
		t.Fatal(err)
	}
	if declined {
		t.Fatal("a re-invite after a decline must re-open onboarding (spec 5.3)")
	}
}

// Spec 5.3: without Developer, the bot cannot push a branch. It says so in an
// issue instead of failing silently — and it says so exactly once.
func TestInsufficientRoleOpensOneIssue(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessReporter)
	o := newOnboarder(gl)
	ctx := context.Background()

	for range 3 {
		if err := o.Onboard(ctx, p.Project); err != nil {
			t.Fatalf("Onboard = %v", err)
		}
	}
	issues, _ := gl.Client().ListIssues(ctx, p.ID, glab.IssueListOptions{State: "opened", Labels: OnboardingIssueLabel})
	if len(issues) != 1 {
		t.Fatalf("opened %d issues, want exactly 1", len(issues))
	}
	if !strings.Contains(issues[0].Title, "Developer") {
		t.Fatalf("issue must name the role it needs: %q", issues[0].Title)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 0 {
		t.Fatal("must not attempt an MR without push rights")
	}
}

// A leftover branch from a crashed run must not wedge onboarding forever.
func TestOnboardRecoversFromOrphanBranch(t *testing.T) {
	gl := glabtest.New(t)
	p := gl.AddProject("group/repo", glab.AccessDeveloper)
	ctx := context.Background()
	if _, err := gl.Client().CreateBranch(ctx, p.ID, OnboardBranch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := newOnboarder(gl).Onboard(ctx, p.Project); err != nil {
		t.Fatalf("Onboard = %v", err)
	}
	mrs, _ := gl.Client().ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if len(mrs) != 1 {
		t.Fatalf("mrs = %+v", mrs)
	}
}
```

- [ ] **Step 7: Implement `pkg/intake/onboard.go`**

```go
package intake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
)

// OnboardingIssueLabel marks the "I need Developer" issue so the bot can find it
// again and not open a second one.
const OnboardingIssueLabel = "gonk::onboarding"

// GitLabOnboarder opens the deterministic onboarding MR (spec 5.3).
//
// DETERMINISTIC. NO MODEL CALL. Every byte it writes comes from render.go.
type GitLabOnboarder struct {
	GL          OnboardGitLab
	BotUserID   int64
	BotUsername string
	Version     string
	Obs         Observer
}

// OnboardGitLab is the API slice onboarding needs.
type OnboardGitLab interface {
	ListMergeRequests(ctx context.Context, projectID int64, o glab.MRListOptions) ([]glab.MergeRequest, error)
	CreateMergeRequest(ctx context.Context, projectID int64, o glab.MROptions) (*glab.MergeRequest, error)
	CreateBranch(ctx context.Context, projectID int64, branch, ref string) (*glab.Branch, error)
	CreateCommit(ctx context.Context, projectID int64, o glab.CommitOptions) (*glab.Commit, error)
	ListMembers(ctx context.Context, projectID int64) ([]glab.Member, error)
	ListIssues(ctx context.Context, projectID int64, o glab.IssueListOptions) ([]glab.Issue, error)
	CreateIssue(ctx context.Context, projectID int64, o glab.IssueOptions) (*glab.Issue, error)
}

func (o *GitLabOnboarder) Onboard(ctx context.Context, p glab.Project) error {
	mrs, err := o.GL.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "all"})
	if err != nil {
		return fmt.Errorf("onboard: list MRs: %w", err)
	}
	if len(mrs) > 0 {
		// Open: the ball is in the maintainer's court. Closed: declined (and
		// Declined() decides whether a re-invite reopens it). Merged: the project
		// has a config and is not our problem. In no case do we open a second MR.
		return nil
	}

	// Spec 5.3: no Developer, no branch. Say what is needed instead of failing.
	if p.EffectiveAccess() < glab.AccessDeveloper {
		return o.requestAccess(ctx, p)
	}

	target := p.DefaultBranch
	if target == "" {
		target = "main"
	}

	// A branch may survive a crashed earlier attempt; that is not an error.
	if _, err := o.GL.CreateBranch(ctx, p.ID, OnboardBranch, target); err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("onboard: create branch: %w", err)
	}

	action := "create"
	if _, err := o.GL.CreateCommit(ctx, p.ID, glab.CommitOptions{
		Branch:        OnboardBranch,
		CommitMessage: o.commitMessage(),
		Actions: []glab.CommitAction{{
			Action: action, FilePath: ConfigPath, Content: string(DefaultConfigYAML),
		}},
	}); err != nil {
		// The file may already exist on a leftover branch: retry as an update.
		if _, uerr := o.GL.CreateCommit(ctx, p.ID, glab.CommitOptions{
			Branch:        OnboardBranch,
			CommitMessage: o.commitMessage(),
			Actions: []glab.CommitAction{{
				Action: "update", FilePath: ConfigPath, Content: string(DefaultConfigYAML),
			}},
		}); uerr != nil {
			return fmt.Errorf("onboard: commit: %w", err)
		}
	}

	body, err := RenderOnboardingMR(OnboardingContext{
		Project: p.PathWithNamespace, BotUsername: o.BotUsername, Version: o.Version,
	})
	if err != nil {
		return err
	}

	if _, err := o.GL.CreateMergeRequest(ctx, p.ID, glab.MROptions{
		SourceBranch:       OnboardBranch,
		TargetBranch:       target,
		Title:              "gonk: enable automated issue triage",
		Description:        body,
		AssigneeID:         o.pickAssignee(ctx, p),
		RemoveSourceBranch: true,
	}); err != nil {
		return fmt.Errorf("onboard: create MR: %w", err)
	}
	o.Obs.OnboardingResult("mr_opened")
	return nil
}

// commitMessage carries a provenance trailer (spec 6.1). This commit had no
// model in it at all, and says so.
func (o *GitLabOnboarder) commitMessage() string {
	return "chore: add .gonk.yml (gonk onboarding)\n\n" +
		"Generated-By: gonk/" + o.Version + " (deterministic onboarding; no model)\n"
}

// pickAssignee: the lowest-numbered Maintainer/Owner, deterministically. GitLab
// CE supports a single assignee (multiple assignees is a paid feature), so the
// choice must be stable rather than arbitrary — two reconcile passes must not
// disagree. 0 means "leave unassigned", which is legal.
func (o *GitLabOnboarder) pickAssignee(ctx context.Context, p glab.Project) int64 {
	members, err := o.GL.ListMembers(ctx, p.ID)
	if err != nil {
		return 0
	}
	var ids []int64
	for _, m := range members {
		if m.ID != o.BotUserID && m.AccessLevel >= glab.AccessMaintainer {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids[0]
}

// requestAccess opens exactly one issue naming the role gonk needs (spec 5.3).
// Idempotent via the label: no repeat nagging every reconcile pass.
func (o *GitLabOnboarder) requestAccess(ctx context.Context, p glab.Project) error {
	existing, err := o.GL.ListIssues(ctx, p.ID, glab.IssueListOptions{State: "opened", Labels: OnboardingIssueLabel})
	if err != nil {
		return fmt.Errorf("onboard: list issues: %w", err)
	}
	if len(existing) > 0 {
		return nil
	}
	_, err = o.GL.CreateIssue(ctx, p.ID, glab.IssueOptions{
		Title: "gonk needs the Developer role to open its onboarding merge request",
		Description: fmt.Sprintf(
			"`@%s` was invited to this project, but with a role below Developer, so it "+
				"cannot push the `%s` branch that carries its onboarding merge request.\n\n"+
				"Grant `@%s` the **Developer** role and it will open the merge request on its "+
				"next reconciliation pass (within ten minutes). Nothing else will happen until "+
				"a human merges that request.\n\n"+
				"If gonk was invited by mistake, remove it from the project and close this issue.",
			o.BotUsername, OnboardBranch, o.BotUsername),
		Labels: OnboardingIssueLabel,
	})
	if err != nil {
		return fmt.Errorf("onboard: create access issue: %w", err)
	}
	o.Obs.OnboardingResult("access_requested")
	return nil
}

// Declined: an onboarding MR was closed without merging, and the bot has not
// been re-invited since (spec 5.3, AD-3). Both facts are read from GitLab --
// nothing about a decline is persisted on gonk's side.
func (o *GitLabOnboarder) Declined(ctx context.Context, p glab.Project) (bool, error) {
	mrs, err := o.GL.ListMergeRequests(ctx, p.ID, glab.MRListOptions{SourceBranch: OnboardBranch, State: "closed"})
	if err != nil {
		return false, err
	}
	var closedAt time.Time
	for _, mr := range mrs {
		at := mr.ClosedAt
		if at == nil {
			at = mr.UpdatedAt
		}
		if at != nil && at.After(closedAt) {
			closedAt = *at
		}
	}
	if closedAt.IsZero() {
		return false, nil
	}
	// Re-invited after the decline? Then the question is open again.
	members, err := o.GL.ListMembers(ctx, p.ID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m.ID == o.BotUserID && m.CreatedAt != nil && m.CreatedAt.After(closedAt) {
			return false, nil
		}
	}
	return true, nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists")
}
```

- [ ] **Step 8: Run the whole package**

Run: `go test ./pkg/intake/ -race -count=1 -v`
Expected: PASS. Wire `Onboarder: newOnboarder(...)` into the reconciler in
`main.go` (Task 10).

- [ ] **Step 9: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1
git add pkg/intake && git commit -m "feat(intake): deterministic Renovate-style onboarding MR (zero model calls)"
```

---
### Task 9: `pkg/intake` — dispatch: gating, mentions, the order seam

**Files:** Create `pkg/intake/dispatch.go`, `pkg/intake/dispatch_test.go`.

**There is no `quiet.go`, and there is no quiet-hours code in this plan.** That is **Conflict B**, settled: `schedule.quiet_hours` is a **wait-vs-spend decision**, and every wait-vs-spend decision belongs to meter, which already speaks `defer` and already returns `retry_after`. Intake dispatches; **meter defers**; the pack parks the bead and retries. Intake never even sees it.

The chain, end to end:

```
issue webhook -> intake.Decide (GitLab state + meter's Effective) -> fire order
                                                                      |
                                          Gas City controller --------+
                                                                      |
                              pack dispatch formula -> POST /v1/policy/decide
                                                                      |
                                              meter: "defer, quiet-hours, retry_after=07:00"
                                                                      |
                                          pack: park the bead, retry at 07:00
```

Nothing is lost and intake stays stateless — which is exactly what the old design was straining to achieve by inventing a `not_before` field on the order and making it a **Plan 04 dependency**. That field is gone; so is the dependency.

The gate is still a **pure function** — `Decide(entry, event, botUsername) Decision` — for the same reason meter's rung decision is (spec 10.2): "may this event cause work?" must be answerable exhaustively in a table test with zero infrastructure. Note it **no longer takes a clock**: with quiet hours gone, nothing in intake's gate is time-dependent.

- [ ] **Step 1: Write the failing test** — `pkg/intake/dispatch_test.go`

```go
package intake

import (
	"context"
	"testing"

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

// validEntry: meter says active, the repo has .agent/. Note we build meter's
// RESPONSE, not a gonkcfg.Effective -- intake has no resolver.
func validEntry() Entry {
	return Entry{
		Project:        glab.Project{ID: 42, PathWithNamespace: "group/repo"},
		Classification: Classify(obs(nil), active(nil)),
	}
}

func issueEvent(action string) *ghook.Event { /* ... as before ... */ }
func noteEvent(body string) *ghook.Event    { /* ... as before ... */ }

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
// A project with quiet hours set DISPATCHES NORMALLY from intake. The deferral
// happens downstream, at meter's /policy/decide, where the pack asks. If this
// test ever starts failing because someone "helpfully" re-added quiet-hours
// handling here, we are back to two components owning one decision.
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

func TestMentions(t *testing.T) { /* ... unchanged: @gonk word-boundary matching ... */ }

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

// PLAN.md carry-forward: attribution values must be safe for the ledger.
func TestDispatchRefusesAttributionUnsafeValues(t *testing.T) { /* ... unchanged ... */ }
func TestHandleFiresOrder(t *testing.T)                       { /* ... unchanged ... */ }
func TestUnknownProjectTriggersReconcile(t *testing.T)        { /* ... unchanged ... */ }
func TestOnboardingMergeKicksReconcile(t *testing.T)          { /* ... unchanged ... */ }
```

- [ ] **Step 2: Implement `pkg/intake/dispatch.go`**

```go
// OrderRequest is what intake asks the Gas City supervisor to do (spec 4.3
// step 2). NOTE (OD-A): the real supervisor endpoint and payload are not in the
// spec. This is gonk's internal shape; Plan 04 maps it onto Gas City's actual
// order API.
//
// NOTE what is NOT here: there is no `not_before`. Quiet hours -- and every other
// wait-vs-spend decision -- belong to gonk-meter, which the PACK asks at
// /v1/policy/decide immediately before the session spawns, and which answers
// `defer` with a `retry_after`. Intake does not schedule work; it only says work
// exists. (Conflict B.)
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

	// ConfigHash names the .gonk.yml that authorized this work.
	ConfigHash string `json:"config_hash"`
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
// This gate is a PRE-FILTER. It exists to avoid firing orders that meter will
// certainly deny (which would churn a bead and a session slot for nothing). It
// is NOT the enforcement point -- meter's /policy/decide is, and it is the only
// chokepoint before a session spawns. So every rule here must be a SUBSET of
// meter's: dropping something meter would have allowed is a silent bug, and a
// much harder one to see than the reverse.
func Decide(e Entry, ev *ghook.Event, botUsername string) Decision {
	cls := e.Classification

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
	if cls.State == StatePending {
		return Decision{Reason: "state_pending"}, false // spec 5.3: triage waits for .agent/
	}
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

// Mentions reports whether the note addresses the bot. Quoted lines (markdown
// blockquotes) are stripped first: a human quoting gonk's own comment is not a
// new request, and treating it as one is how a bot ends up talking to itself.
func Mentions(body, botUsername string) bool { /* ... unchanged ... */ }

// Dispatch is the webhook-side consumer: cache lookup, gate, fire.
// It has no clock. (`Now func() time.Time` is gone with quiet hours.)
type Dispatch struct {
	Dispatcher    Dispatcher
	Cache         *Cache
	BotUsername   string
	Obs           Observer
	Log           *slog.Logger
	KickReconcile func() // request an out-of-band reconcile pass (coalesced)
}

func (d *Dispatch) Handle(ctx context.Context, ev *ghook.Event) { /* ... as before, minus the clock ... */ }

// FireScaffold is called by the reconciler for a pending project (spec 5.3: the
// .agent/ scaffold MR is the one metered action permitted while pending).
func (d *Dispatch) FireScaffold(ctx context.Context, e Entry) error { /* ... as before ... */ }

func attributionSafeOrder(o OrderRequest) error { /* ... unchanged ... */ }
```

- [ ] **Step 3: Run the package**

Run: `go test ./pkg/intake/ -race -count=1 -v` → PASS.

Then prove the negative:

```bash
grep -rn "quiet\|QuietHours\|NotBefore\|not_before" pkg/intake/ && echo "FAIL: quiet hours leaked back into intake" || echo "ok"
```

- [ ] **Step 4: Commit**

```bash
gofmt -l . && go vet ./... && go test ./... -race -count=1
git add pkg/intake && git commit -m "feat(intake): pure dispatch gate, mention routing, order seam (quiet hours are meter's)"
```

---

### Task 10: `cmd/gonk-intake` — metrics, servers, wiring, ADR

**Files:** Create `pkg/intake/metrics.go`, `pkg/intake/server.go`,
`pkg/intake/metrics_test.go`, `pkg/intake/server_test.go`,
`cmd/gonk-intake/main.go`, `docs/adr/ADR-003-intake-trust-boundary-and-seams.md`.
Modify: `PLAN.md` (status + carry-forwards), `README.md` (layout line).

- [ ] **Step 1: Add the Prometheus dependency**

```bash
go get github.com/prometheus/client_golang@latest
```

- [ ] **Step 2: Write the failing metrics test** — `pkg/intake/metrics_test.go`

```go
package intake

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

func TestMetricsRecordOutcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.WebhookOutcome("Issue Hook", ghook.OutcomeAccepted)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeBadToken)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeBadToken)
	m.ReconcileResult("ok", 2*time.Second)
	m.ProjectStates(map[State]int{StateValid: 3, StatePending: 1})
	m.Dispatched("issue-triage")
	m.DispatchDropped("state_pending")

	if got := testutil.ToFloat64(m.webhook.WithLabelValues("Issue Hook", "bad_token")); got != 2 {
		t.Errorf("bad_token count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.projects.WithLabelValues("valid")); got != 3 {
		t.Errorf("valid gauge = %v, want 3", got)
	}
	out, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out {
		t.Errorf("metric lint: %s: %s", p.Metric, p.Text)
	}
}

// Spec 8 names the series intake must export. Missing one is a broken dashboard.
func TestRequiredSeriesExist(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.WebhookOutcome("Issue Hook", ghook.OutcomeAccepted)
	m.ReconcileResult("ok", time.Second)
	m.ProjectStates(map[State]int{StateValid: 1})
	m.MeterPush("ok")
	m.OnboardingResult("mr_opened")
	m.Dispatched("issue-triage")

	got, err := testutil.CollectAndCount(reg)
	if err != nil || got == 0 {
		t.Fatalf("no metrics collected: %v", err)
	}
	dump, err := testutil.GatherAndCompare(reg, strings.NewReader(""))
	_ = dump
	_ = err
	for _, want := range []string{
		"gonk_intake_webhook_total",
		"gonk_intake_reconcile_total",
		"gonk_intake_reconcile_duration_seconds",
		"gonk_intake_projects",
		"gonk_intake_meter_push_total",
		"gonk_intake_onboarding_total",
		"gonk_intake_dispatched_total",
		"gonk_intake_dispatch_dropped_total",
	} {
		if n, err := testutil.GatherAndCount(reg, want); err != nil || n == 0 {
			t.Errorf("series %s is not exported (spec 8)", want)
		}
	}
}
```

(`testutil.GatherAndCount` exists in recent client_golang; if the version you get
lacks it, gather with `reg.Gather()` and check the metric family names — the
assertion, not the helper, is the point.)

- [ ] **Step 3: Implement `pkg/intake/metrics.go`**

```go
package intake

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gitlab.orac.local/agentic/gonk-project/pkg/ghook"
)

// Metrics implements Observer (and ghook.Observer). Series names and label
// values are a contract with the shipped Grafana dashboards (spec 8): renaming
// one silently blanks a panel.
type Metrics struct {
	webhook    *prometheus.CounterVec
	reconcile  *prometheus.CounterVec
	reconDur   prometheus.Histogram
	projects   *prometheus.GaugeVec
	meterPush  *prometheus.CounterVec
	onboarding *prometheus.CounterVec
	dispatched *prometheus.CounterVec
	dropped    *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		webhook: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_webhook_total",
			Help: "GitLab webhook deliveries by event and outcome (accepted, bad_token, duplicate, bot_authored, ...).",
		}, []string{"event", "outcome"}),
		reconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_reconcile_total",
			Help: "Reconciliation passes by result (ok, partial, error).",
		}, []string{"result"}),
		reconDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "gonk_intake_reconcile_duration_seconds",
			Help:    "Duration of a full reconciliation pass.",
			Buckets: prometheus.DefBuckets,
		}),
		projects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gonk_intake_projects",
			Help: "Projects by state. GitLab-determined: unmanaged, absent, declined. Meter-determined: invalid, disabled, key-missing. Neither: unsynced. The join: pending, valid.",
		}, []string{"state"}),
		meterPush: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_meter_push_total",
			Help: "Raw-config registrations with gonk-meter by result (ok, invalid, error).",
		}, []string{"result"}),
		onboarding: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_onboarding_total",
			Help: "Onboarding outcomes (mr_opened, access_requested, error).",
		}, []string{"result"}),
		dispatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_dispatched_total",
			Help: "Orders fired by trigger.",
		}, []string{"trigger"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gonk_intake_dispatch_dropped_total",
			Help: "Events that did not become orders, by reason.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.webhook, m.reconcile, m.reconDur, m.projects, m.meterPush, m.onboarding, m.dispatched, m.dropped)
	// Pre-create the state gauges so a dashboard shows 0 rather than nothing.
	for _, s := range AllStates {
		m.projects.WithLabelValues(string(s)).Set(0)
	}
	return m
}

func (m *Metrics) WebhookOutcome(event string, o ghook.Outcome) {
	m.webhook.WithLabelValues(event, string(o)).Inc()
}

func (m *Metrics) ReconcileResult(result string, d time.Duration) {
	m.reconcile.WithLabelValues(result).Inc()
	m.reconDur.Observe(d.Seconds())
}

func (m *Metrics) ProjectStates(counts map[State]int) {
	for _, s := range AllStates {
		m.projects.WithLabelValues(string(s)).Set(float64(counts[s]))
	}
}

func (m *Metrics) MeterPush(result string)        { m.meterPush.WithLabelValues(result).Inc() }
func (m *Metrics) OnboardingResult(result string) { m.onboarding.WithLabelValues(result).Inc() }
func (m *Metrics) Dispatched(trigger string)      { m.dispatched.WithLabelValues(trigger).Inc() }
func (m *Metrics) DispatchDropped(reason string)  { m.dropped.WithLabelValues(reason).Inc() }
```

- [ ] **Step 4: Write the failing server test** — `pkg/intake/server_test.go`

The security property to prove: **the public listener serves exactly one path.**

```go
func TestPublicListenerExposesOnlyTheHook(t *testing.T) {
	srv := NewServer(ServerConfig{Hook: &ghook.Handler{ /* … */ }})
	for _, path := range []string{"/", "/metrics", "/healthz", "/admin/reconcile", "/debug/pprof/"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("public listener serves %s (code %d); it must serve /hook/gitlab and nothing else", path, w.Code)
		}
	}
}

func TestPrivateListenerServesOpsEndpoints(t *testing.T) {
	// /healthz, /readyz, /metrics -> 200; POST /admin/reconcile -> 202 and kicks
	// a reconcile; GET /admin/reconcile -> 405.
}
```

- [ ] **Step 5: Implement `pkg/intake/server.go`**

Two `http.ServeMux`es on two ports:

- `Public()` — `POST /hook/gitlab` only. Everything else 404. This is the only
  thing behind the Ingress (spec 9).
- `Private()` — `GET /healthz` (process alive), `GET /readyz` (bot user resolved,
  first reconcile completed, meter `/healthz` OK), `GET /metrics`
  (`promhttp.HandlerFor(reg, …)`), `POST /admin/reconcile` (on-demand pass, spec
  5.2 — coalesced: a request while one is running just returns 202).

Both wrapped in `http.Server` with `ReadHeaderTimeout: 5 * time.Second` (an
unauthenticated slowloris on the ingress port is otherwise free).

- [ ] **Step 6: Implement `cmd/gonk-intake/main.go`**

```go
// Command gonk-intake is the GitLab side of gonk: it receives and verifies
// webhooks, reconciles the bot's project memberships and .gonk.yml state,
// opens the deterministic onboarding merge request, and dispatches work to the
// Gas City supervisor.
//
// It calls no language model. Ever.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // quiet_hours timezones are resolved in-process; the image has no system tzdata

	"github.com/prometheus/client_golang/prometheus"
	// … gonk packages
)

// Config comes from the environment. SECRETS ARE READ FROM FILES, never from an
// env VALUE and never from a flag.
//
// The chain is Vault/1Password -> ExternalSecret -> k8s Secret -> projected FILE
// MOUNT. Env leaks into `ps`, into /proc/<pid>/environ, into crash dumps, and
// into every child process; a flag is visible in `ps` to anything on the node.
// The env vars below therefore carry PATHS, never material.
//
// TWO ROTATION SLOTS for every credential (the GitLab bot token, the webhook
// secret, the meter bearer token), so a rotation is not an outage: write the new
// secret to slot 1, the old to slot 2, roll, then drop slot 2.
//
// NO SECRET IS EVER COMMITTED. House rule; not negotiable.
type Config struct {
	GitLabURL             string // GONK_GITLAB_URL
	GitLabTokenFile       string // GONK_GITLAB_TOKEN_FILE      (bot PAT)
	GitLabAdminTokenFile  string // GONK_GITLAB_ADMIN_TOKEN_FILE (optional; split-credential, AD-4)
	WebhookSecretFile     string // GONK_WEBHOOK_SECRET_FILE          (rotation slot 1)
	WebhookPrevSecretFile string // GONK_WEBHOOK_SECRET_PREVIOUS_FILE (rotation slot 2, optional)
	WebhookTokenGen       string // GONK_WEBHOOK_TOKEN_GEN            (bump on rotation)
	WebhookPublicURL      string // GONK_WEBHOOK_PUBLIC_URL  e.g. https://gonk.orac.local/hook/gitlab
	HookSSLVerify         bool   // GONK_WEBHOOK_SSL_VERIFY (default true)
	BotUsername           string // GONK_BOT_USERNAME (default "gonk")
	MeterURL              string // GONK_METER_URL
	MeterTokenFile        string // GONK_METER_TOKEN_FILE          (bearer, rotation slot 1)
	MeterTokenPrevFile    string // GONK_METER_TOKEN_PREVIOUS_FILE (rotation slot 2, optional)
	SupervisorURL         string // GONK_SUPERVISOR_URL ("" -> LogDispatcher, OD-A)
	ReconcileInterval     time.Duration // GONK_RECONCILE_INTERVAL (default 10m)
	ListenAddr            string // GONK_LISTEN_ADDR  (default :8080, public: hook only)
	PrivateAddr           string // GONK_PRIVATE_ADDR (default :9090, cluster-internal)
	Version               string // GONK_VERSION (build stamp)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err // fail fast: a missing secret must not degrade into "no verification"
	}
	botToken, err := readSecretFile(cfg.GitLabTokenFile)
	if err != nil {
		return err
	}
	hookSecret, err := readSecretFile(cfg.WebhookSecretFile)
	if err != nil {
		return err
	}
	prevSecret, _ := readSecretFile(cfg.WebhookPrevSecretFile) // optional

	verifier, err := ghook.NewVerifier(hookSecret, prevSecret)
	if err != nil {
		return err
	}

	gl := glab.New(cfg.GitLabURL, botToken)
	if cfg.GitLabAdminTokenFile != "" {
		if adm, err := readSecretFile(cfg.GitLabAdminTokenFile); err == nil {
			gl.AdminToken = adm
		} else {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Resolve the bot's own user id. This is the loop guard: without it, gonk
	// would react to its own comments. Refuse to start without it, and refuse to
	// start if the token belongs to someone other than the configured bot.
	me, err := gl.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("resolve bot identity: %w", err)
	}
	if me.Username != cfg.BotUsername {
		return fmt.Errorf("token belongs to %q but GONK_BOT_USERNAME is %q; refusing to start",
			me.Username, cfg.BotUsername)
	}
	log.Info("bot identity", "username", me.Username, "id", me.ID)

	reg := prometheus.NewRegistry()
	metrics := intake.NewMetrics(reg)
	cache := intake.NewCache()

	dispatcher := intake.Dispatcher(intake.NewLogDispatcher(log))
	if cfg.SupervisorURL != "" {
		dispatcher = intake.NewHTTPDispatcher(cfg.SupervisorURL, nil) // OD-A: shape pending Plan 04
	}
	dp := &intake.Dispatch{Dispatcher: dispatcher, Cache: cache, BotUsername: cfg.BotUsername, Obs: metrics, Log: log}

	meterToken, err := readSecretFile(cfg.MeterTokenFile)
	if err != nil {
		return err
	}

	rec := &intake.Reconciler{
		GL: gl, Meter: intake.NewMeterClient(cfg.MeterURL, meterToken, nil), Cache: cache, Obs: metrics, Log: log,
		Onboarder: &intake.GitLabOnboarder{GL: gl, BotUserID: me.ID, BotUsername: cfg.BotUsername, Version: cfg.Version, Obs: metrics},
		Dispatch:  dp,
		BotUserID: me.ID, HookURL: cfg.WebhookPublicURL, HookToken: hookSecret,
		TokenGen: cfg.WebhookTokenGen, SSLVerify: cfg.HookSSLVerify,
		// NOTE: no Instance policy and no GroupPolicy. Intake does not hold
		// operator config and does not resolve -- gonk-meter does (Conflict A).
	}
	dp.KickReconcile = rec.Kick // coalesced out-of-band pass

	// events is bounded: a webhook flood must not grow the heap. A full queue
	// drops (with a metric) rather than blocking the handler — reconciliation
	// remains the correctness path (spec 5.2).
	events := make(chan *ghook.Event, 256)
	hook := &ghook.Handler{
		Verifier: verifier, Deduper: ghook.NewDeduper(time.Hour, 8192),
		BotUserID: me.ID, Obs: metrics,
		Sink: func(e *ghook.Event) bool {
			select {
			case events <- e:
				return true
			default:
				return false
			}
		},
	}
	go func() {
		for ev := range events {
			dp.Handle(ctx, ev)
		}
	}()

	// … start public + private http.Servers, run rec.Loop(ctx, cfg.ReconcileInterval),
	// wait on ctx.Done(), then Shutdown both servers with a 15s grace period.
	_ = hook
	_ = reg
	return nil
}

// readSecretFile reads a mounted secret. It trims exactly one trailing newline
// (kubectl create secret --from-literal adds none; a shell heredoc adds one) and
// refuses an empty file: an empty webhook secret would accept every request.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", path, err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return s, nil
}
```

Implementer notes:

- `Reconciler.Loop(ctx, interval)` and `Reconciler.Kick()` are small additions to
  `reconcile.go`: a ticker plus a buffered `chan struct{}` of size 1 so
  concurrent kicks coalesce into one extra pass. Add
  `TestKickCoalesces`.
- `LogDispatcher` / `HTTPDispatcher` live in `dispatch.go`.
- **Never log a token, a secret, or `HookOptions`.** Add a test that
  `slog`-formatting an `OrderRequest` cannot contain the hook token (it has no
  such field — keep it that way).

- [ ] **Step 7: Write `docs/adr/ADR-003-intake-trust-boundary-and-seams.md`**

Content (the decisions this plan locks, so later plans cannot silently undo them):

1. **Verify before parse.** The `X-Gitlab-Token` header is checked before the
   body is read. Consequence: the webhook secret is instance-wide, not
   per-project (AD-1) — a per-project secret would require parsing untrusted
   input to decide which key to check.
2. **The public listener serves exactly one path.** `/metrics`, `/healthz`,
   `/admin/reconcile` are on a second, cluster-internal listener.
3. **Reconciliation is the correctness path.** Webhook loss (queue full, hook
   misconfigured, GitLab down) degrades latency, never correctness. Therefore
   intake answers GitLab with 200 for anything it accepts-and-drops: 4xx/5xx
   would get the hook auto-disabled, which *would* be a correctness problem.
4. **No persistent state.** The project cache is derived; restart rebuilds it
   from GitLab **and gonk-meter**.
5. **Hook token freshness via a generation marker.** GitLab never returns a
   hook's token, so `?gen=N` in the hook URL is the observable proxy: bump
   `GONK_WEBHOOK_TOKEN_GEN` on rotation and reconcile repairs every hook.
6. **INTAKE DOES NOT RESOLVE CONFIG.** `gonkcfg.Resolve` is not called anywhere
   in this component. Intake pushes **raw `.gonk.yml` bytes** to gonk-meter,
   which holds operator (instance/group) policy and is therefore the only place
   that *can* resolve. Two resolvers would be two sources of truth for a budget
   ceiling. **`invalid`, `disabled` and `key-missing` are meter's answers, which
   intake records.** Intake may call `gonkcfg.Load` to ask "do these bytes
   parse?", but that answer is **advisory**: where intake and meter disagree,
   **meter wins.** (This ADR does not modify ADR-002; it *narrows who applies it*.)
7. **INTAKE DOES NOT ENFORCE QUIET HOURS.** `schedule.quiet_hours` is a
   wait-vs-spend decision, and every wait-vs-spend decision is meter's. Intake
   dispatches; meter answers the pack's `/policy/decide` with `defer` +
   `retry_after`; the pack parks the bead. There is no `not_before` on an order,
   and intake has no clock in its gate.
8. **Fail closed.** Unknown project, unresolved policy (`unsynced`), invalid
   config, disabled config, missing virtual key, unreachable meter → **no
   dispatch**. Dispatchability is a *whitelist* (`valid`, `pending`), so a state
   added later is non-dispatchable by default.
9. **The meter seam is `pkg/meterapi`, owned by Plan 03.** Intake imports it and
   restates nothing. `+Inf`/`MaxInt64` become `null` there (ADR-002), and an
   empty `Budget{}` means *unlimited*, never *zero*.
10. **Intake's action check is a pre-filter, not enforcement.** Meter's
    `/policy/decide` is the only chokepoint before a session spawns. Intake's
    gate must be a **subset** of meter's rules, never a superset — dropping work
    meter would have allowed is a silent bug. The exception is
    `triage.respond_to_mentions`, which is *not* one of `Effective.Actions`, so
    **meter never checks it and intake is its only enforcement point.**
11. **The order seam is `intake.Dispatcher`**, shape provisional until Plan 04
    (OD-A). The controller must dedupe on `BeadAnchor`.
12. **Secrets are file mounts with two rotation slots**, never env values.
13. **Nothing in intake calls a model.**

Cross-reference `ADR-004` (Plan 03), which records the other half of the split.

- [ ] **Step 8: Update `PLAN.md`**

- Set plan 02 Status to `done`.
- Add to "Contracts published by plan 02": `pkg/glab` (+ `glabtest`), `pkg/ghook`,
  `pkg/meterapi` (**Plan 03 must import it, not re-derive the JSON**),
  `pkg/intake` (`Classify`, `Decide`, `RigName`, `SessionKey`, `BeadAnchor`).
- Add to "Carried into later plans" (see "Cross-plan contracts" below).

- [ ] **Step 9: Full gate + commit**

```bash
gofmt -l .
go vet ./...
go test ./... -race -count=1
golangci-lint run ./...
git add -A && git commit -m "feat(intake): metrics, servers, and the gonk-intake binary (ADR-003)"
```

---

## Cross-plan contracts this plan creates

Add these to `PLAN.md`'s "Carried into later plans" in Task 10:

- **Plan 03 (meter) — ALREADY RECONCILED; this is the summary:** meter **owns
  `pkg/meterapi`** (its Task 0 is the normative source; this plan merely lands the
  files first). Meter implements `PUT`/`GET`/`DELETE /v1/projects/{project}` +
  `GET /healthz`. **Meter validates and resolves the raw `.gonk.yml`** intake
  sends — `gonkcfg.Resolve` is called in exactly one place in the repo, and it is
  there. Meter **owns quiet hours**, as a `defer`. A 422 means *the project's yaml
  is bad* (recorded, key deleted, error echoed to intake); a 400 means *intake's
  request is bad*. `null` budget means *unlimited*. `key_ref` is never key
  material. Meter no-ops on an unchanged `config_hash`. **Intake never calls
  `/policy/decide`** — that is the dispatch formula's call (spec 6.2.3).
- **Plan 04 (pack):** (a) the Gas City order API shape (OD-A) — `HTTPDispatcher`
  is a placeholder; (b) **the controller must treat `OrderRequest.BeadAnchor` as
  an idempotency key** — intake can fire the same order twice across a restart,
  and a duplicate bead means duplicate spend; (c) **the pack calls
  `POST /v1/policy/decide` before every session spawn, and a `defer` is a NORMAL
  answer: park the bead in `waiting-for-capacity` and retry at `retry_after`.
  This is where quiet hours land, and it is the only place a deferral is
  handled** — intake no longer carries a `not_before`; (d) the pack must **never**
  send an attempt count (meter owns ladder state; a caller-supplied attempt is a
  forgery vector); (e) the session is what posts labels and comments — intake does
  not.
- **Plan 05 (chart):** the instance ladder must be non-empty and must contain the
  onboarding template's rung (`qwen-local`, OD-B), or every newly-onboarded
  project resolves to `disabled` (ADR-002). (`opercfg.Load` now refuses to start
  with an empty instance ladder, which closes the fail-open PLAN.md flagged.)
  Secrets are **file mounts from existingSecret refs, with two rotation slots**:
  `GONK_GITLAB_TOKEN_FILE`, `GONK_GITLAB_ADMIN_TOKEN_FILE` (optional),
  `GONK_WEBHOOK_SECRET_FILE`, `GONK_WEBHOOK_SECRET_PREVIOUS_FILE`,
  `GONK_METER_TOKEN_FILE`, `GONK_METER_TOKEN_PREVIOUS_FILE` — **never env
  values**. Ship an **alert rule on `gonk_intake_projects{state="invalid"} > 0`**
  (AD-2: an invalid project otherwise goes dark silently). Two listeners: only
  `:8080` (hook) goes behind the Ingress; `:9090` (metrics/health/admin) must not.
  The image needs no system tzdata (intake no longer resolves timezones at all —
  quiet hours moved to meter), but it *does* need the GitLab CA if
  `GONK_WEBHOOK_SSL_VERIFY` is on.
- **Plan 06 (e2e):** the list below.

## Plan 06 e2e verification items (things a fake GitLab cannot prove)

1. **Golden payload refresh.** `pkg/ghook/testdata/*.json` are hand-built from
   docs. Capture the real payloads from gitlab.orac.local and diff. Every field
   `event.go` reads is listed in its doc comment — that is the checklist. Repeat on
   every GitLab upgrade (spec 12.6).
2. **Does the member payload carry `created_at`?** AD-3's re-invite rule depends
   on it. If not, decline becomes permanent-until-manual-reset.
3. **Hook provisioning against real GitLab CE**, including whether GitLab accepts
   a hook URL with a query parameter (the `?gen=N` marker) and returns it
   unchanged.
4. **MR assignee on CE:** a single `assignee_id` (multiple assignees is a paid
   feature). Confirm the MR is assigned and visible to maintainers.
5. **Webhook TLS:** whether GitLab trusts the ingress certificate with
   `enable_ssl_verification: true`.
6. **Hook auto-disable:** GitLab disables hooks that fail repeatedly. Confirm the
   200-on-drop policy (ADR-003.3) actually prevents it.
7. **The full onboarding scenario** (spec 11.2): invite bot → onboarding MR
   appears → merge → `.gonk.yml` on default branch → project goes `pending` →
   scaffold MR → `valid` → file an issue → triage comment lands.
8. **Kill test** (spec 11.6): restart intake mid-flow. No duplicate onboarding
   MRs, no duplicate triage comments — which is to say, confirm the controller
   really does dedupe on `BeadAnchor`.
9. **The intake↔meter seam against a real gonk-meter.** Every test in this plan
   runs against a `fakeMeter`. Confirm against the real one: a raw `.gonk.yml`
   round-trips; a bad one returns **422** (not 400) with the loader's message; a
   de-onboard **DELETE** actually deletes the LiteLLM key; and an operator
   flipping the instance kill switch turns a project `disabled` in intake's cache
   **within one `MeterResyncInterval`, without the project's config changing**.
   That last one is the failure mode the hash short-circuit could hide.
10. **Quiet hours end to end** (Conflict B): file an issue inside a project's
   quiet window and confirm intake **fires the order**, meter **defers** it, and
   the pack **runs it when the window ends** — nothing lost, nothing run early.

---

## Definition of done

- `gofmt -l .` is empty; `go vet ./...`, `go test ./... -race -count=1`, and
  `golangci-lint run ./...` (v2.12.2) are green locally. **These are the standing
  gate** — every runner on gitlab.orac.local was offline during Plan 01, so CI
  green may still be unavailable (PLAN.md carry-forward). If a runner exists, the
  `lint` and `test` jobs must be green too.
- `cmd/gonk-intake` builds and starts against a fake GitLab; a forged webhook is
  rejected 401; a valid issue webhook produces one order.
- **Conflict A holds — intake does not resolve.** These greps, over **non-test**
  files, must all be empty:
  ```bash
  g() { grep -rn "$1" pkg/intake/ cmd/gonk-intake/ --include='*.go' | grep -v '_test\.go'; }
  g "gonkcfg.Resolve"      # meter resolves, not us
  g "gonkcfg.Effective"    # we hold meterapi.Effective, off the wire
  g "gonkcfg.Policy"       # we hold no operator policy at all
  ```
  **There is exactly one legitimate `gonkcfg.Resolve` in this package, and it is in
  a test:** `render_test.go`'s `TestDefaultConfigIsValidAndEnabled`, which asserts
  that the `.gonk.yml` the onboarding MR *ships* would resolve to an enabled,
  triage-only, $0-cloud-budget project. That is a property of the **template**, and
  the template lives here — an onboarding MR that lands a config gonk then rejects
  would be a spectacular own goal, and it must be caught at the template, not three
  components downstream. It creates no second resolver in the running system, which
  is the thing Conflict A actually forbids. ADR-002 explicitly permits `Resolve` in
  tests. Do not "fix" it by deleting the test.
- **Conflict B holds — intake has no quiet hours.** This grep must be empty:
  ```bash
  grep -rni "quiet\|not_before\|NotBefore" pkg/intake/ cmd/gonk-intake/
  ```
- **`pkg/meterapi` is byte-identical to Plan 03 Task 0's normative source**, and
  its sha256 drift gate is armed. Intake defines **no** meter wire types of its own.
- **No secret is read from an env value or a flag.** `grep -rn "os.Getenv"
  cmd/gonk-intake/` shows only *paths*, never material.
- **No test in this plan requires a live GitLab, a live meter, a Kubernetes
  cluster, or a model.** If one does, it belongs in Plan 06.
- **No code path in this plan calls a language model.** Grep the diff: any HTTP
  client pointed at anything but GitLab, gonk-meter, or the Gas City supervisor is
  a bug.
- `PLAN.md` updated: plan 02 `done`, contracts published, carry-forwards recorded.
- Every remaining open question is either an **"Assumed default"** the code
  actually implements, or an **"Owner decision needed"** block still flagged in
  `PLAN.md`. Nothing has been silently decided.



