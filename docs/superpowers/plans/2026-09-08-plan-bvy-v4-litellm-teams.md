# gonk-bvy v4: project identity is the LiteLLM *team*; credential identity is a stored *hash*

Status: PROPOSED, 2026-09-08
Supersedes: v3 (`2026-09-08-plan-bvy-v3-litellm-teams.md`), which an adversarial
review returned as **needs revision, not a green light**.
Related: gonk-uom2, gonk-ay87, gonk-jn5

---

## 0. What v3 got wrong

v3's direction (identity on a real primary key, not on a label) survives review.
Three of its specifics did not, and one of its "measured facts" was an inference
wearing a measurement's label:

| v3 defect | why it was fatal |
|---|---|
| §3.1 step 3 "return the plaintext for the keysink" | **impossible.** `admin_http.go:19-21`, VERIFIED against v1.92.0: *"A key's PLAINTEXT secret is revealed exactly once, in the /key/generate response. No later call recovers it."* Both adopt paths have no plaintext to return. |
| "adopt the OLDEST" | **decorative.** `KeySink` has only `Put`/`Delete`, no `Get`, so gonk could not tell whether it held the plaintext of the key it "adopted". It would record adopting A while the agent kept using B. |
| org ceiling authored by an operator (§3.4) | **wedges every project.** The ceiling is `MaxBudgetFor` — `cost + tokens x maxPrice`, a *synthetic* fold — which on shipped defaults is **$1000.00**. Any human-authored dollar figure is smaller, so F5 rejects every `/team/new`, instance-wide, permanently. |
| F3 "the fact the whole plan rests on" | measured **one** key with `max_budget: None`, not **two keys sharing a counter**. The claim gonk-bvy actually needs was never tested. |

The last one is the important one. v3's §0 exists to condemn exactly that move.
It has now been measured (M3 below) and the claim holds — but it held by luck of
being right, not by having been checked.

---

## 1. Measured facts

All probes run **inside** the `litellm` pod (`kubectl -n litellm exec -i
deploy/litellm -- python3 -`), so the master key never left the pod and never
entered a URL. Every probe cleaned up; `/team/list` returned to `['OpenClaw']`
and `/organization/list` to `0` after each run.

### Identity and idempotency

- **M1.** `team_id` is a caller-supplied primary key. A repeat returns
  `400 "Team id = X already exists. Please use a different team id."` — a
  distinguishable, usable idempotency signal.
- **M2.** `organization_id` is **also** caller-supplied and honoured, and is
  unique-constrained (a repeat creates nothing). **But its conflict signal is
  `500 Internal server error`**, not a clean 400. A 500 is indistinguishable
  from a genuine fault, so a conflict MUST be confirmed by a follow-up
  `GET /organization/info?organization_id=`, never assumed.
- **M3.** `/organization/new` is **not** idempotent by `organization_alias`:
  two calls with the same alias produced two orgs with different UUIDs. Alias is
  as unusable for orgs as it is for keys.
- **M4.** A caller-supplied key value makes `/key/generate` a true idempotent
  no-op: repeat returns 200, the same value, **one** row, spend preserved
  (`6.25e-06`), alias unchanged. *(Recorded because it kills v2 on the merits —
  it works, it is simply unnecessary, and it needed an HMAC secret.)*

### Budget enforcement

- **M5. TWO budget-less keys in ONE team share ONE counter.** Ceiling `2e-05`,
  keys K1/K2 both `max_budget: None`, requests alternating between them:
  ```
  req 3 -> 200  team spend=1.25e-05
  req 5 -> 429  Budget has been exceeded! Team=gonk-h2-... Current cost: 2.5e-05, Max budget: 2e-05
  ```
  Blocked near `1x` the ceiling, not `2x`. **This is the fact the plan rests on,
  and this time it is the fact that was actually measured.**
- **M6.** An ORG ceiling enforces the same way
  (`429 ... Organization=... 6.25e-06 vs 5e-07`).
- **M7.** LiteLLM validates `team.max_budget <= org.max_budget` at write time
  (`400 Team max_budget (100.0) exceeds organization's max_budget (5e-07)`).
  It does **not** validate key <= team.
- **M8. A key's OWN `max_budget` binds BEFORE its team's** — 429 naming
  `Key=`, not `Team=`. Any migrated key that keeps its old budget keeps a
  second, lower, silently-binding ceiling.
- **M9.** `POST /key/update {"max_budget": null}` **clears** the key budget:
  `5e-07 -> None`, and the key that was 429ing then returns 200. Clearing is
  real and verifiable.
- **M10.** `budget_duration` `"30d"` and `"1mo"` both yield
  `budget_reset_at: 2026-10-01T00:00:00Z`. They are indistinguishable *today*;
  one observation cannot prove they stay so. **Use the pinned
  `service.keyBudgetDuration = "1mo"`** — its comment says *"Never derive this
  from anything else"* (AD-9).

### Shape and lifecycle

- **M11. Orgs do NOT nest.** `parent_id` / `parent_organization_id` are silently
  ignored. Depth is exactly **org > team > key** — three tiers for GitLab's four
  levels.
- **M12.** A key moves between teams via `/key/update {"team_id": B}` without
  reissue; the same plaintext still authenticates. **This plan deliberately does
  not use it** — see §2.1. Recorded only so a future reader knows the capability
  exists and that declining it was a choice.
- **M13.** `/key/list?team_id=X&return_full_object=true` returns full objects
  carrying **`token`** (the hash) and **`created_at` at millisecond precision**
  (two keys 14 ms apart were distinguishable). Note the original gonk-bvy
  duplicates were recorded as same-*second*; ms precision helps but does not
  remove the need for a deterministic tie-break.
- **M14. `/key/generate` with a nonexistent `team_id` returns 200** and mints a
  key with `max_budget: None` — *but the key does not work*: using it returns
  `404 "Team doesn't exist in db"`. **Fail-open at creation, fail-closed at
  use.** Not a budget bypass; it is a *new wedge shape*, and it must be made
  unreachable (§3.1).
- **M15.** `/organization/info` reports top-level `max_budget: None` while the
  real ceiling lives at `litellm_budget_table.max_budget`. Read the nested field.

### Repo constraints (verified in-tree, not assumed)

- `admin_http.go:19-21` — plaintext revealed exactly once; `/key/list` and
  `/key/info` return only the hash; the hash as a Bearer token is 401.
- `keysink.KeySink` — `Put` and `Delete` only, deliberately no `Get`.
- `litellm.MaxBudgetFor` — `cost + tokens x maxPrice`; synthetic dollars, and
  **nil** when either ceiling is unlimited (meaning `max_budget` is omitted —
  *no hard door at all*).
- `service.keyBudgetDuration = "1mo"`, pinned to meter's UTC calendar month.
- `store.Registration` has **no `ProjectID`**; it carries `KeyAlias`.
- `litellm.Fake.EnsureKey` returns a **plaintext token on the idempotent-hit
  path** — a fiction real LiteLLM does not provide. Every test in the package
  currently passes against it.

---

## 2. The mapping

```
GitLab top-level group -> LiteLLM ORGANIZATION   group ceiling,   native + shared
GitLab project         -> LiteLLM TEAM           project ceiling, native + shared
agent credential       -> LiteLLM KEY            no budget of its own
GitLab instance        -> gonk                   instance ceiling
```

```
organization_id = "gonk-" + <instance-slug> + "-g" + <numeric top-level group id>
team_id         = "gonk-" + <instance-slug> + "-g" + <group id> + "-p" + <project id>
```

Both are deterministic, public, and **injective** — numeric GitLab ids are unique
per instance and contain no separators, so `a/b` and `a-b` no longer collide.
**gonk-uom2 becomes unreachable**, not merely fixed. Because both ids are
caller-supplied (M1, M2), there is no server-generated UUID to hand back into
gitops, and no chicken-and-egg on first install.

### 2.1 A project that changes groups gets a NEW team and a NEW key

The team id carries the **group** as well as the project, so transferring a
project from group A to group B **changes the computed team id**. That is
deliberate, and it is what makes the billing boundary honest:

> Group A paid for the work up to the transfer; group B pays after it. There is
> no migration of spend, and no key that outlives the change of ownership.

So a transfer is an **onboarding**, not a move: mint a fresh key billing to
group B's team, and **invalidate the group A key**. Concretely, gonk never calls
`/key/update {"team_id": …}` (M12) and never re-parents a team with
`/team/update {"organization_id": …}`.

This is strictly simpler than the alternative and it is also the only correct
one. Keying the team on the project alone would have forced a re-parent on
transfer, which drags the team's accumulated spend into group B's organization
counter — making group B pay for work group A already paid for, and corrupting
both groups' ceilings in the same stroke.

The old team is **tombstoned, not deleted** (§3.5): its spend is the durable
record of what group A paid, and deleting it would discard exactly the evidence
the billing boundary is meant to establish.

Two consequences, both intended:

- A transferred project starts the new group's period at **zero LiteLLM spend**.
  gonk's own ledger (`pkg/spend`) is unaffected and still holds the full history.
- The credential the agent holds **changes** at transfer. That is the
  "invalidate" half of the rule, and it is the same code path as rotation.

### Why group -> org, reversing v3

v3 spent the single org tier on the *instance*. The review attacked that and it
does not survive:

- **The instance has cardinality one.** `pkg/opercfg` carries exactly one
  `Instance` policy, so instance→org is an identity map — a shared bucket with
  one logical member.
- **The hard problem is the group tier.** v3 conceded it: *"ten projects under a
  $50 group ceiling can each draw $50."* That starvation is a **group** problem.
  The instance total is one `/team/list` and a sum — trivial in gonk code. v3
  assigned the native shared-bucket primitive to the tier that needed it least.
- **The nesting objection was already absorbed.** v3 rejected group→org because
  GitLab groups nest and orgs do not (M11). But `opercfg.GroupFor` *already*
  folds a nested hierarchy into one `gonkcfg.Policy` layer — *"gonkcfg.Resolve
  takes exactly ONE group layer, and a nested group hierarchy has several."*
  gonk's group model is flat by the time it matters, so top-level-namespace→org
  is exact **for the model gonk actually has**.
- **gonk-jn5 makes it worse for v3.** Watching every project on an instance
  multiplies *groups*, not instances.
- **M7 still does real work**, just at a more useful level: it now enforces
  project <= group at write time, which is spec 5.4 where the contention is.

What gonk loses is instance-tier *hard backstop*. That costs less than it looks:
gonk-ay87 already records that instance admission must happen **before a pod is
spawned**, and *"LiteLLM can only refuse a request already in flight, by which
point the pod and the checkout are paid for."* An org-as-instance was never going
to implement instance admission anyway.

---

## 3. Design

### 3.1 `EnsureProject` — the credential contract, stated honestly

The plaintext exists only in the `/key/generate` response. So gonk must be able
to decide *"is the key I already stored still the live one?"* **without** the
plaintext and **without** reading the Secret back.

**Store the key's hash in the registration.** `/key/list?...&return_full_object=true`
returns each key's `token`, which *is* `sha256(plaintext)` (M13). Comparing a
stored hash against that list needs no plaintext, no `KeySink.Get`, and no `get`
verb on Secrets in RBAC — preserving the property v2 chose deliberately.

`store.Registration.KeyAlias` is replaced by `KeyHash`, in the same migration
that adds `ProjectID` (§3.3).

```
EnsureProject(ctx, spec) (KeyInfo, error)

0. TRANSFER CHECK. Compute the expected team_id from (group id, project id).
   If the registration holds a DIFFERENT team_id, the project changed groups
   (§2.1). Retire the old tenancy first, and only then continue at step 1:
     - delete every key in the OLD team (this is the "invalidate" half of the
       rule -- the credential the agent holds must stop working, not merely
       stop being referenced)
     - tombstone the OLD team at max_budget 0; do NOT delete it, and do NOT
       re-parent it
   The new team is then created from scratch by step 2, at zero spend, under
   group B's org. Emit gonk_meter_litellm_project_transferred_total{from,to}.

1. Ensure the ORG (group tier), then the TEAM (project tier), then the key.
   Each tier is idempotent by its caller-supplied primary key.

2. TEAM: POST /team/new {team_id, organization_id, max_budget, budget_duration:"1mo"}
     200                                  -> created
     400 AND body matches "already exists" -> exists; converge with /team/update:
                                              max_budget AND organization_id (L7:
                                              a team stranded in a stale org must
                                              be re-parented, or it sits outside
                                              the group ceiling silently)
     any other 400/5xx                     -> RETURN THE ERROR. Do not proceed.
                                              This is what makes M14 unreachable:
                                              step 3 is never entered without a team.

3. KEYS: GET /key/list?team_id=<id>&return_full_object=true
     a) stored KeyHash is present in the list
          -> that key is live. Delete every OTHER key in the team, return
             KeyInfo{Token: ""} meaning "credential unchanged; keep what you hold".
     b) stored KeyHash absent, or nothing stored
          -> delete EVERY key in the team, then POST /key/generate {team_id}
             with NO max_budget (M5: the team is the ceiling).
             Assert the response's team_id equals the expected one; if not,
             delete the key and fail. Store the new hash; Put the plaintext.

4. Duplicates are never a terminal state. Case (a) resolves them silently, case
   (b) resolves them by replacement. Emit gonk_meter_litellm_duplicate_keys_total
   {project} and log WARN with the count either way.
```

**Why "delete every other key" is safe now and was not before.** The spend lives
on the *team* (M5), so deleting a key destroys no budget history and cannot
change any ceiling. Under today's design — budget on the key — the same deletion
would have destroyed a ledger, which is exactly why the original gonk-bvy note
said refusing to guess was correct. That premise is now gone.

**Deterministic tie-break** (M13): where two keys must be ordered, order by
`created_at` then by `token` ascending, so two racing meters cannot pick
different keys and flap the Secret under a running pod.

### 3.2 Org bootstrap stays in gitops, with read-verify

gonk does **not** author the group ceiling. A gitops Job (the
`job-gonk-meter-scoped-key.yaml` pattern) ensures each org, using the
deterministic `organization_id` so there is no UUID handoff. Because a conflict
surfaces as `500` (M2), the Job must be **find-or-create with read-verify**:
`POST`; on non-200, `GET /organization/info?organization_id=` and treat existence
as success; only a genuine absence is an error. Never treat a bare 500 as
"already exists".

The org's ceiling is **computed, not authored**: it must be
`MaxBudgetFor(group effective budget, group ladder, catalog)` in the same
synthetic currency as the team ceilings, or M7 rejects every `/team/new` beneath
it. A hand-written dollar figure is a wedge (v3's C4). Where `MaxBudgetFor`
returns nil (either ceiling unlimited), the org gets **no** `max_budget` —
matching today's key behaviour and ADR-004's recorded limitation.

### 3.3 Schema and plumbing

`registrations` gains `team_id TEXT` and `key_hash TEXT` (replacing `key_alias`),
plus `project_id BIGINT` for provenance. `Service.resolveProject` currently
**drops** `req.ProjectID`; it must carry it into `store.Registration`.

`team_id` is stored rather than only recomputed because it is the **previous**
tenancy: §3.1 step 0 detects a group transfer precisely by comparing the stored
team id against the freshly computed one. Without it, a transfer is invisible and
the project would keep billing to its old group indefinitely.

**Backfill matters more than it looks.** `Service.ReconcileKeys` — the loop that
unsticks `key-missing`, i.e. the loop gonk-bvy's recovery requirement is about —
rebuilds the `KeySpec` from the stored `Registration` alone. Rows written before
the migration have no `project_id`, so they cannot derive a `team_id`. Such rows
must be marked as needing re-registration and skipped with a distinct, counted
condition rather than failing silently.

### 3.4 `key_alias` becomes decoration; nothing may look a key up by it

Alias is globally unique across the proxy, so a fixed per-project alias 400s the
moment a second key is minted. Set `"gonk-p<id>-<8 hex>"` or omit it. An alias
400 on create must be **retryable**, not terminal. A buildgate-style test asserts
no `key_alias` query parameter survives in `internal/meter/litellm`.

### 3.5 Delete, rotate, de-onboard — all become team-scoped

v3 dropped these silently; under nonce aliases they get *worse*, not better.
`DeleteKey` posts `key_aliases`, so with a nonce alias it deletes **nothing** —
and `Service.deleteKeyIfAny` is the *"a broken or disabled config must not keep
spending"* path, whose own comment insists the credential a pod holds *"has to
stop working, not just become unreachable."* Silently no-opping it is a security
regression.

- `DeleteKey(project)` -> list by `team_id`, delete each hash.
- `RotateKey(project)` -> delete all in team, generate one. (Team preserves the
  spend, so rotation no longer loses history.)
- De-onboarding -> **tombstone the team at `max_budget: 0`**, do not delete it;
  deleting discards the spend record. Requires `/team/update`, not `/team/delete`.
- Group transfer (§2.1, §3.1 step 0) -> the same tombstone, applied to the old
  team, plus deletion of its keys. A tombstoned team at `max_budget: 0` cannot
  serve a request even if a credential for it were somehow retained, so the
  invalidation is enforced at two layers rather than one.

Because nothing re-parents a team and nothing moves a key between teams, the
scoped credential needs **no** `/team/delete` and gonk holds no capability to
merge two groups' spend. That is a property worth keeping: the billing boundary
is enforced by what gonk *cannot* do, not only by what it declines to do.

### 3.6 Scoped credential

`gonk-meter`'s `allowed_routes` gains `/team/new`, `/team/update`, `/team/info`,
`/team/list`, and keeps `/key/list`, `/key/generate`, `/key/delete`.

**`/key/update` is dropped.** Once keys carry no budget and are never moved
between teams (§2.1, §4), nothing gonk does needs to mutate an existing key —
every change is expressed as delete-and-mint. Removing the route means a
compromised meter credential cannot silently re-point an existing key at another
team or raise its ceiling. Likewise **`/team/delete` is not granted**: retirement
is a tombstone, so the spend record cannot be destroyed by anything gonk holds.

It gets `/organization/info` (read, for M15's nested ceiling) but
**no `/organization/*` write route** — gonk does not own the group ceiling. The
gitops Job's existing scope assertion (proves `/key/list` 200, `/chat/completions`
403) is extended to prove `/organization/new` is 403.

### 3.7 Explicitly unchanged: the spend ledger

v3 claimed team spend was *"stronger than summing tagged rows."* That was wrong
and is withdrawn. `pkg/spend` keeps `cost_synthetic` because *"LiteLLM has one
`spend` column and does not know the difference. This flag is the ONLY thing that
keeps synthetic dollars out of the cost gate."* `/team/info` returns that one
blended column, has no per-bead/session/rung granularity, and follows
`budget_duration` rather than `spend.Window`. **The tag-based ingest is
unchanged.** Team spend is an independent cross-check on the ceiling, nothing
more.

### 3.8 Models / TPM / RPM

`KeySpec.Models` restricts a project to its resolved ladder. It stays **on the
key**, unchanged, because it is per-project policy and the team is per-project
too. It must not be silently dropped — that would be a policy regression. Whether
an empty team `models` list means "all" and whether key-models must be a subset
of team-models are **unmeasured**; the team is therefore created with no model
restriction, and that is recorded as a deliberate choice, not an oversight.

---

## 4. Cutover — a re-onboarding, not a migration

The same rule as §2.1 applies to the one-time move onto this scheme: **do not
carry credentials across the boundary.** Per project, one at a time:

1. Ensure the org and the team at the computed ceiling.
2. `POST /key/generate {team_id}` with **no** `max_budget` — a fresh credential
   billing to the new team.
3. `keysink.Put` the new plaintext; store the new `team_id` and `key_hash`.
4. Delete the OLD key by its hash. The old credential must stop working, not
   merely stop being referenced.

This deletes three problems the adopt-the-old-key version had:

- **No `/key/update {"team_id"}`**, so M12 is not relied on and neither of v4's
  two "unmeasured and gating" questions — does spend survive a team move, and is
  a move accepted when the key's budget exceeds the target team's — needs
  answering at all. They are now moot rather than outstanding.
- **No `max_budget: null` clear**, so M9 and the `*float64`/`omitempty` encoding
  trap (nil is *omitted*, meaning "no change", not "clear") never arise on this
  path. M8's stale-key-budget hazard cannot occur, because the new key never had
  a budget.
- **No one-way door.** Rollback is: revert the code and let the ordinary
  provisioning path re-mint per-key budgets from `MaxBudgetFor`. Nothing needs
  restoring by hand.

**The one real consequence, and it is intended.** A project's new team starts at
**zero** LiteLLM spend even though the project has already spent against its old
key this period. So in the cutover period a project can draw up to *(already
spent) + (full team ceiling)*. This is bounded, one-time, and visible.

gonk's own accounting is unaffected — `pkg/spend`'s tag-based ledger keeps the
full history and the soft reservation gate still sees true month-to-date, so the
looseness is confined to LiteLLM's hard backstop. Mitigate by either cutting over
at a period boundary, or seeding the team's first-period `max_budget` at
*(ceiling − month-to-date from gonk's ledger)* and letting it reset naturally.
Pick one deliberately; do not leave it unstated.

---

## 5. Scope split

This plan is gonk-bvy proper: §3.1, §3.4, §3.5, §3.6, and §4. The rest is filed
separately rather than smuggled in: the schema migration and `ProjectID`
plumbing, the group-tier shared bucket (gonk-ay87, now smaller), the instance
admission gate, and the test-double rewrite.

## 6. Verification (fresh agent, against the plan, not the diff)

1. **The Fake must lie less first.** `litellm.Fake` returns plaintext on the
   idempotent-hit path; real LiteLLM does not. Until it returns `Token: ""` on
   adoption, no test in this package proves anything about §3.1. Same for
   `test/harness/litellm.go`, which is keyed by alias.
2. Two `EnsureProject` calls racing produce **one team**, **one key**, and a
   working credential from both — against a fake reproducing M1's 400 body
   verbatim.
3. A stored `KeyHash` that is still present is adopted with `Token: ""` and the
   stored `KeyRef` is returned unchanged; a stored hash that is **absent** causes
   full replacement and a `Put`.
4. `≥2` keys logs WARN, increments the counter, and **dispatches** — the terminal
   park is gone.
5. M14 is unreachable: a `/team/new` failure returns before any `/key/generate`.
   Assert with a fake that 400s on team creation and fails the test if a key is
   ever generated.
6. A key is **never** returned without a bounding ceiling at some tier
   (negative control).
7. `grep` proves no `key_alias` lookup path remains, and that `DeleteKey` no
   longer posts `key_aliases`.
8. `org.max_budget >= max over projects of MaxBudgetFor(...)`, asserted before
   any team is created, so M7 cannot wedge the instance.
9. Pre-cutover rows without `team_id` surface a distinct counted condition, not
   a silent skip.
10. **Group transfer (§2.1).** A project whose group id changes produces a NEW
    team under the new group's org, a NEW credential in the keysink, and an OLD
    team that is tombstoned at `max_budget: 0` with its spend **intact**. Assert
    all four, and assert the old key is gone — a transfer that leaves the
    previous credential working has not invalidated anything.
11. `grep` proves gonk never calls `/key/update` with a `team_id`, and never
    calls `/team/update` with an `organization_id`. Both would silently merge two
    groups' billing, and neither is reachable through any supported flow.
