# gonk-bvy v3: stop using `key_alias` as identity; make the LiteLLM *team* the project

> **SUPERSEDED 2026-09-08 (same day) by `2026-09-08-plan-bvy-v4-litellm-teams.md`.**
> An adversarial review returned this as *needs revision*: its §3.1 required
> returning a plaintext LiteLLM reveals only once, its "adopt the oldest" rule
> could not verify what it adopted, its operator-authored org ceiling would have
> wedged every project on the shipped defaults, and its F3 -- labelled "the fact
> the whole plan rests on" -- measured one key, not the two-keys-one-counter
> claim gonk-bvy actually needs. v4 measures that claim and reverses the
> group/instance mapping. Kept for the record.

Status: PROPOSED, 2026-09-08
Supersedes: `2026-09-07-plan-bvy-duplicate-litellm-keys.md` (v1 and v2)
Related: gonk-uom2 (RigName not injective), gonk-ay87 (three-tier budget draw)

---

## 0. Why there is a v3

v1 and v2 both accepted the premise that gonk identifies a project's LiteLLM
credential **by `key_alias`**, and tried to make creation-by-alias atomic. v2 got
as far as deriving the key value deterministically so that LiteLLM's primary key
(`token = sha256(plaintext)`) would reject the second writer.

That is a workaround for a design mistake, not a fix. `key_alias` is a *label*
whose global uniqueness LiteLLM enforces in application code as check-then-insert
— which is precisely the operation that races. Every symptom in gonk-bvy descends
from choosing it as the identity:

| symptom | root |
|---|---|
| two meters both create a key | check-then-insert on alias |
| `2 keys share alias, refusing to guess` — terminal | alias is not a key, so it cannot disambiguate |
| gonk-uom2: `a/b` and `a-b` collide | alias derived from a non-injective `RigName` |
| v2 needs an HMAC secret | only to make an alias-shaped identity atomic |

LiteLLM already models exactly what gonk needs. The fix is to use it.

---

## 1. Measured facts (all verified against the live proxy, 2026-09-08)

Probes ran **inside** the `litellm` pod (`kubectl -n litellm exec -i deploy/litellm
-- python3 -`) so the master key never left the pod and never entered a URL. All
probe orgs/teams/keys were deleted; `/team/list` returned to `['OpenClaw']` and
`/organization/list` to `0` after every run.

**F1. `team_id` is caller-supplied and honoured.**
`POST /team/new {"team_id": "gonk-probe-team-a-…"}` → 200, and the team is
retrievable at exactly that id. It is a real primary key, not a label.

**F2. A repeated `team_id` fails with a distinguishable 400.**
```
400 {"error": "Team id = gonk-probe-3tier-… already exists. Please use a different team id."}
```
This is the idempotency signal: *already exists* == *success* for a reconciler.
There is no window in which two callers both succeed.

**F3. Team budgets ENFORCE, and bind a key that has no budget of its own.**
Team ceiling `5e-07`, key created with `max_budget: None`:
```
req 1 -> 200   team spend=0.0
req 2 -> 429   {"message":"Budget has been exceeded! Team=gonk-probe-team-…
                Current cost: 6.25e-06, Max budget: 5e-07",
                "type":"budget_exceeded","code":"429"}
```
**This is the fact the whole plan rests on.** The ceiling lives on the team, so
duplicate keys inside one team do **not** multiply the ceiling.

**F4. Organization budgets ENFORCE.**
Org ceiling `5e-07`, team inside it with no budget of its own:
```
req 3 -> 429   {"message":"Budget has been exceeded! Organization=e02f65a8-…
                Current cost: 6.25e-06, Max budget: 5e-07"}
```

**F5. LiteLLM validates team ≤ org at write time.**
```
400 Team max_budget (100.0) exceeds organization's max_budget (5e-07).
```
This is gonk's own spec 5.4 ("ceilings only tighten downward") enforced by the
proxy for free. Note the asymmetry: **key ≤ team is NOT validated** (a key with
`max_budget 99` inside a team with `0.001` was accepted, 200) — but the team
ceiling still binds at request time. Validation is org→team only; *enforcement*
is all three tiers.

**F6. Organizations do NOT nest.** `parent_organization_id` and `parent_id` are
both silently ignored (echoed `None`); `organization_id` returns 500. Depth is
exactly **org → team → key**: three tiers, no more.

**F7. A key can be moved between teams without reissue.**
`POST /key/update {"key": …, "team_id": B}` → 200; `/key/list?team_id=B` returns
it; the same plaintext key still authenticates. (This answers gonk-ay87's
explicit "check before committing" question.)

**F8. `/key/list` filters on first-class indexed fields** — `team_id`, `user_id`,
`organization_id`, `key_hash` — so lookup needs no alias join.

**F9. Org budgets are stored in a nested object, and the top-level field lies.**
`/organization/info` reports `max_budget: None` while the real ceiling sits at
`litellm_budget_table.max_budget`. Any gonk code reading an org budget MUST read
the nested field. This is a live trap.

**F10. `/key/generate` accepts a caller-supplied `key` and is a true idempotent
no-op on repeat.** Second create with the same value: 200, same value returned,
**still one row**, spend `6.25e-06` preserved, alias unchanged. Recorded because
it kills v2 on the merits: v2's mechanism *works*, it is simply unnecessary once
identity moves off the alias — and it would have required an HMAC secret to keep
the key unguessable, since the key value is the credential.

**F11. Team budgets carry a reset cadence.** `budget_duration: "30d"` yields
`budget_reset_at: 2026-10-01T00:00:00Z` — calendar-monthly, matching gonk's
`monthly_cost_usd`.

**F12. Teams are already in production use on this proxy.** `OpenClaw`:
`max_budget 20.0`, `spend 1.859155`, `budget_duration 30d`. Team spend accrual is
not theoretical here.

---

## 2. The mapping

```
GitLab instance   ->  LiteLLM ORGANIZATION   (one org)      instance ceiling, native
GitLab project    ->  LiteLLM TEAM           (one per prj)  project ceiling,  native
agent credential  ->  LiteLLM KEY            (n per team)   disposable, no budget
GitLab group      ->  gonk                                  group ceiling,   gonk code
```

**Project → team, not project → key.** This is the whole plan. It is what makes
duplicates harmless: the ceiling is on the team, so a race that produces two keys
produces two credentials against *one* budget, and either one is correct.

`team_id = "gonk-" + <instance-slug> + "-p" + <numeric GitLab project id>`

Numeric project ids are unique per GitLab instance and contain no separators, so
the mapping is **injective by construction** — `a/b` and `a-b` have different ids.
**gonk-uom2 dissolves**; it is not fixed, it stops being reachable.

### Why the group tier is the one gonk keeps

Only three LiteLLM tiers exist (F6) and GitLab has four levels (instance, nested
groups, project, credential), so exactly one tier must be gonk's. Group is the
right one to give up:

- **Instance → org is exact.** There is one instance and one org. Mapping
  *group* → org would be lossy the moment a subgroup exists, because GitLab
  groups nest and orgs do not (F6).
- **F5 then does real work**: with org = instance, LiteLLM itself refuses to let
  any project ceiling exceed the instance ceiling, at write time.
- **gonk already owns group semantics.** `gonkcfg.Resolve` resolves group policy
  today, and how nested subgroups aggregate is a gonk decision either way.

### What gonk must still build for the group tier

Note what `gonkcfg.Resolve` does and does not give us. `EffectiveBudget` is the
**minimum across instance/group/project layers** — it *tightens each project's
ceiling* but does not create a *shared bucket*. Ten projects under a group with a
$50 group ceiling can each draw $50, for $500. That gap is gonk-ay87 and this
plan does not close it; it closes it for the instance tier (via the org) and
leaves the group tier as gonk-ay87's remaining scope, now much smaller: sum the
spend of the teams whose projects are in the group, and refuse dispatch above the
group ceiling.

---

## 3. What changes in the code

### 3.1 `internal/meter/litellm` — identity moves to the team

Replace `EnsureKey(ctx, KeySpec{Alias})`'s alias semantics. New shape:

```go
// EnsureProject makes the project's LiteLLM team exist with the resolved
// ceiling, and returns a usable credential inside it.
//
// Idempotent by TEAM ID, which is LiteLLM's primary key -- not by key_alias,
// whose uniqueness is application-level check-then-insert and therefore races
// (gonk-bvy). Concurrent callers cannot both create: the loser gets 400
// "already exists", which is success.
EnsureProject(ctx context.Context, spec ProjectSpec) (KeyInfo, error)
```

Algorithm:

1. `POST /team/new` with the deterministic `team_id`, the resolved
   `max_budget`, `budget_duration: "30d"`, and `organization_id` = the instance
   org.
   - 200 → created.
   - 400 **and** the body matches *already exists* → treat as success, then
     `POST /team/update` to converge `max_budget` onto the resolved ceiling.
   - 400 for any other reason (notably F5's *exceeds organization's max_budget*)
     → a real error; surface it as a distinct condition, do not retry blindly.
2. `GET /key/list?team_id=<id>` (F8).
   - **exactly 1 key** → use it.
   - **0 keys** → `POST /key/generate {"team_id": <id>}` with **no
     `max_budget`** (F3: the team is the ceiling) and **no `key_alias`** — see
     3.2. Race here is benign; it lands in case 3.
   - **≥2 keys** → **not an error.** Adopt the OLDEST (`created_at`), log at
     WARN with the count, and increment a counter. This is bvy requirement #2 and
     it is now *safe*, because F3 means the extras cannot inflate the ceiling and
     cannot be billed to anyone else. The old "refusing to guess" was correct
     when keys could differ in budget; under this plan they cannot.
3. Return the plaintext for the keysink.

`resolveTokenIDByAlias` and its **false doc comment** (it claims
"keysink.Slug's hash suffix guarantees alias uniqueness", which was never true
of the alias — gonk-uom2) are deleted.

### 3.2 `key_alias` becomes decoration, and must carry a nonce

`key_alias` is globally unique in LiteLLM, so if gonk ever creates a second key
for a project, a fixed alias would 400. Either omit it, or set
`"gonk-p<id>-<8 random hex>"`. **Nothing may look a key up by alias.** A
buildgate-style test should assert no `key_alias` query parameter survives in
`internal/meter/litellm`.

### 3.3 `RigName` stays, but is no longer an identity

`RigName` remains the Gas City rig name and the spend-row attribution tag — that
is a separate contract and this plan does not touch it. What changes is that it
is no longer what gonk joins on for keys. Per-project spend now comes from
`GET /team/info?team_id=` directly, which is stronger than summing tagged rows.
Its doc comment must stop implying uniqueness guarantees it does not provide.

### 3.4 Org bootstrap belongs in gitops, not in a POST from gonk

Per the standing objection to non-idempotent HTTP provisioning: gonk **must not**
create the organization. The instance org (and its `max_budget`) is a gitops
object, created by the same Job pattern as `job-gonk-meter-scoped-key.yaml`, and
gonk is *given* the `organization_id` by config. gonk creates teams (its own
tenancy) but never the org (the operator's ceiling).

Reading the org's ceiling back for display must use
`litellm_budget_table.max_budget` (F9).

### 3.5 Scoped credential

The `gonk-meter` scoped key currently allows `/key/*`. It now needs `/team/new`,
`/team/update`, `/team/info`, plus the existing `/key/list`, `/key/generate`,
`/key/delete`. It must **not** get `/organization/*` write routes — gonk does not
own the instance ceiling. Update the gitops Job's `allowed_routes` and the
verification step that asserts the scope (the Job already proves `/key/list` is
200 and `/chat/completions` is 403; extend that assertion set).

---

## 4. Migration

Existing keys (`gonk-agentic-gonk-e2e-1784441480`,
`gonk-homelab-talos-toolkit`) have no team and carry their own `max_budget`.

F7 says a key can be moved into a team without reissue, so migration does not
interrupt any running agent:

1. Create the org (gitops) and, per live project, the team at the resolved ceiling.
2. `POST /key/update {"key_hash": …, "team_id": …}` to adopt each existing key.
   Use the hash, never the plaintext, and **never in a URL**.
3. Clear the key's own `max_budget` so the team is unambiguously the ceiling
   (F5's validation gap means a stale key budget would otherwise be a second,
   lower ceiling that silently binds first).
4. Verify: `/team/info` shows the adopted spend, and the agent keeps working.

Spend history on the key is preserved by the move (F10 showed spend survives an
idempotent rewrite; step 4 verifies it survives the team move specifically —
**this is the one migration property not yet measured and it must be measured
before step 2 runs on a real project**).

---

## 5. What this closes

| issue | outcome |
|---|---|
| **gonk-bvy** | the race cannot produce a wedge: creation is idempotent by primary key (F2), and duplicate credentials are harmless (F3) |
| **gonk-uom2** | dissolved — numeric project ids are injective |
| **gonk-ay87** | instance tier native (F4) and validated (F5); group tier remains, now scoped to one shared bucket |
| v2's HMAC secret | not needed |

## 6. Verification (fresh agent, against the plan — not the diff)

1. Two `EnsureProject` calls racing on one project produce **one team** and a
   usable credential from both — asserted against a fake that reproduces F2's
   400 body verbatim.
2. A team at its ceiling returns **429 `budget_exceeded`**, and gonk surfaces it
   as a budget condition, not a generic upstream error.
3. `≥2 keys` in a team logs WARN, adopts the oldest, and **dispatches** — a live
   assertion that the terminal park is gone.
4. `grep` proves no `key_alias` lookup path remains.
5. Org ceiling read back equals what gitops set (guards F9).
6. A project whose resolved ceiling exceeds the org's surfaces F5's 400 as a
   distinct operator-visible condition, not an infinite retry.
