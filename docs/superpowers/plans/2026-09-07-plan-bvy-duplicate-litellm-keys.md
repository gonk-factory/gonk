# Plan v2: duplicate LiteLLM keys wedge a project forever (gonk-bvy, P0)

> **SUPERSEDED 2026-09-08 by `2026-09-08-plan-bvy-v3-litellm-teams.md`.**
> v2's mechanism works (a caller-supplied key value is a true idempotent
> no-op on repeat, measured 2026-09-08) but it is unnecessary: it exists
> only to make an *alias-shaped* identity atomic. v3 moves identity onto
> LiteLLM's `team_id`, which is a real primary key, and the derivation
> secret v2 required is no longer needed. Kept for the record.

**Supersedes v1**, which an adversarial review returned as *redesign*. v1's
mechanism was wrong (it resolved keys by a masked `key_name` join, which carries
only 20 bits and can silently collide), it required a `/key/info?key=<plaintext>`
call that **writes a live credential into LiteLLM's access log** — a leak I then
caused for real — and its fallback ("adopt the oldest") was a coin flip that
would have traded a visible wedge for an invisible unenforced ceiling.

## 1. The failure

Two meters raced, both created a key under alias
`gonk-agentic-gonk-e2e-1784441480` in the same second, and the project wedged
permanently on `virtual-key-missing`:

```
litellm: /key/list: 2 keys share alias, refusing to guess which to update
```

Refusing to guess is correct. The refusal being **terminal** is the bug: nothing
reconciles it away, so the only exit is a human deleting a key by hand. Reachable
from an ordinary node failure — orac04 went NotReady and the stranded meter was
force-deleted onto a healthy node, so two meters ran for a window.

## 2. Facts, measured against the live proxy (v1.92.0)

Everything below was verified, not assumed. This matters because v1 was built on
a guess that turned out to be both weaker and more dangerous than the truth.

| claim | result |
|---|---|
| `token` == `sha256_hex(plaintext)` | **true** — checked against a real key |
| `/key/generate` accepts a caller-supplied `key` | **true**, HTTP 200, echoed back |
| same key + same alias, twice | **HTTP 400**, alias uniqueness (the racing check) |
| same key + *different* alias, twice | **both HTTP 200 — but only ONE row exists** |
| `/key/info?key=<plaintext>` | logs the credential in the URL. **Never use it.** |

That fourth row is the whole design. LiteLLM's alias uniqueness is
application-level check-then-insert and races; `token` is the **primary key** and
does not. Supply the same key from two racers and the second insert is dropped by
the database, whichever alias it claims.

**And it reports success anyway.** The second call returned 200 with the alias it
asked for, for a row that does not exist. gonk must never treat a 200 from
`/key/generate` as proof the row is its own.

## 3. Design

### 3.1 Prevention: derive the key, do not let LiteLLM mint it

```
key = "sk-gonk-" + base64url(HMAC-SHA256(derivationSecret, project + ":" + epoch))
```

Two meters racing now compute the **same** key, so:

- the DB admits one row and silently drops the other — no duplicate, ever;
- **both racers hold the correct plaintext**, because they computed it. The
  split-brain that made v1's fallback a coin flip cannot arise;
- the meter can recompute the key at any time, so it never needs `KeySink.Get`,
  never needs `get` on Secrets in RBAC, and never needs to ask LiteLLM which key
  it holds — which is what removes the credential-in-a-URL leak entirely.

`epoch` is an integer per project, bumped to rotate. Rotation stays deliberate
and auditable instead of being a side effect of a retry.

### 3.2 Verify, never trust the 200

After any create, confirm by listing on `key_hash` (LiteLLM accepts a `key_hash`
filter) computed locally as `sha256(derived)`. If the row is absent or its alias
is not ours, that is an error — not a silent success. This is the direct
consequence of the measurement above.

### 3.3 Recovery: the wedge gets an exit

Duplicates already exist in the wild, and derivation does not retroactively
remove them. When `/key/list?key_alias=` returns more than one:

1. If one row's `token` equals `sha256(derived)`, adopt it. Exact, local, no
   round trip. This is v1's intent, done with the primary key instead of a
   20-bit masked string.
2. Otherwise the project predates derivation. Do **not** guess and do **not**
   adopt the oldest. Mint the derived key, adopt it, and leave the strays.
3. **Zero the strays' budget** (`/key/update {key: <hash>, max_budget: 0}`)
   rather than deleting them. Deleting destroys spend history; leaving them
   untouched leaves an N× hard ceiling, because `max_budget` is enforced per key
   row. Zeroing closes the hole and keeps the record.
4. Report it once, on transition, not once per tick.

### 3.4 What this plan does NOT do

- It does not stop two meters existing. `gonk-meter` is `replicas: 1,
  Recreate` with **no Lease** — verified, `kubectl get leases` is empty — so a
  force-delete off a NotReady node still yields two. Derivation makes that
  harmless rather than impossible. A Lease is worth its own bead.
- It does not fix `RotateKey`/`DeleteKey`, which post `key_aliases` and therefore
  delete **all** duplicates, destroying exactly the spend history §3.3 protects.
  Separate item, listed below.

## 4. Work items

1. `derive(project, epoch) (plaintext, tokenHash string)` — HMAC-SHA256, with
   the derivation secret read from a file mount like every other gonk secret.
2. The derivation secret itself: a new property in `eso/gonk/broker`, mounted
   into the meter, documented in the chart README as a required Secret. **This
   is a real new operational dependency and must not be hidden.**
3. `EnsureKey` supplies the derived `key` to `/key/generate`.
4. Post-create verification per §3.2, using a locally computed `key_hash`.
5. Duplicate resolution per §3.3, including zeroing strays.
6. Migration: existing projects hold random keys. On first ensure, the derived
   key differs from the stored one. Decide deliberately — mint the derived key
   and zero the old, or leave pre-existing projects on their current key until
   their epoch is bumped. **Do not let this happen by accident.**
7. Tests against httptest doubles (the `Fake` is keyed by alias and structurally
   cannot represent duplicates — say so rather than pretend):
   two rows where one matches the derived hash → that one adopted; no match →
   derived key minted, strays zeroed, nothing deleted; a create that returns 200
   with no row → error, not success; the duplicate case RESOLVES rather than
   erroring, which is the regression test for the wedge itself.
8. Negative controls for each.
9. Separate beads, filed not folded: a Lease for `gonk-meter`; `RotateKey`/
   `DeleteKey` deleting by alias; and the alias collision where `RigName` maps
   `a/b` and `a-b` to the same string, so two projects would share one key and
   one budget.

## 5. How this gets checked

Adversarial review of this document before code — v1 needed it and was wrong in
three places. Then a fresh agent checks the implementation against every item in
§4, with evidence per item.
