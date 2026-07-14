# LiteLLM: unbounded `GET /spend/logs` can OOM the proxy (DoS)

**Status:** ready to file — owner will submit to the appropriate audience.
**Decide before filing:** public GitHub issue vs. `security@berri.ai` — hinges on
the one UNVERIFIED question in the Security section below.

## Summary

The still-live, default `GET /spend/logs` endpoint has no `limit`, no
pagination, and optional date filters. With no `start_date`, it fetches the
entire `LiteLLM_SpendLogs` table into memory and serializes it. On an instance
with months of history this exhausts the container's memory and the proxy is
OOMKilled, taking it down for all tenants until it restarts.

A bounded replacement already exists (`/spend/logs/v2`, aka `/spend/logs/ui`),
so this is a one-line hardening of the legacy endpoint, not a new feature.

## Environment (observed)

- LiteLLM `v1.92.0` (`ghcr.io/berriai/litellm-database:v1.92.0`), proxy mode,
  Postgres-backed, running in Kubernetes with a 2Gi container memory limit.
- Spend-log table with months of accumulated rows.

## Steps to reproduce

1. Have a LiteLLM proxy with a large `LiteLLM_SpendLogs` table (e.g. an instance
   that has been serving traffic for weeks/months).
2. Issue a single authenticated request with **no date bounds**:

   ```bash
   curl -s "https://<proxy>/spend/logs" -H "Authorization: Bearer <master-key>"
   ```

3. Observe the proxy container's memory spike and the process get OOMKilled
   (exit 137).

### Observed (on our instance)

- The proxy pod was OOMKilled (exit 137) against its 2Gi limit, restarted twice,
  and was unavailable for ~90 seconds. Every tenant sharing that proxy was
  affected during the outage.

## Root cause (inferred from source)

`litellm/proxy/spend_tracking/spend_management_endpoints.py`, `view_spend_logs`
(≈ line 2290 on `v1.92.0`; unchanged on `main`):

- Query params `limit`/`page`/`page_size` do not exist; `start_date`/`end_date`
  are optional.
- With no scoping filter, the handler executes
  `prisma_client.get_data(table_name="spend", query_type="find_all")` and returns
  the full result set — no cap, no streaming.
- On `main` the docstring now says *"[DEPRECATED] This endpoint is not paginated
  and can cause performance issues. Please use `/spend/logs/v2` instead"* — a
  documentation change only; the `find_all` path is unchanged and the endpoint is
  still served.

The sibling `/spend/logs/ui` (= `/spend/logs/v2`) *is* bounded: `page`
(default 1), `page_size` (default 50, max 100), **mandatory** date range, and a
`SPEND_LOGS_PAGINATION_COUNT_CAP` of 10,000 rows. The project clearly knows how
to bound this query; the legacy endpoint was never retrofitted.

## Impact

- **We observed:** an admin/master-key caller can OOM a shared proxy with one
  accidental request (no date filter). This is a realistic operational footgun —
  e.g. an automated poller that forgets a date bound (this is exactly how we hit
  it: a metering service that polls `/spend/logs`).
- **We infer:** severity depends on who can reach the unscoped path — see below.

## Security — the question that decides where this gets filed (UNVERIFIED)

We did **not** confirm this and are not asserting it. It must be tested before
the report is framed as a security vulnerability:

- Route guard is `dependencies=[Depends(user_api_key_auth)]` — authenticates,
  does not require the master key.
- `/spend/logs` is in `spend_tracking_routes` ⊂ `internal_user_routes`; docs say
  a non-admin key with `permissions={"get_spend_routes": true}` can reach
  `/spend/*`.
- **However**, for `INTERNAL_USER` / `INTERNAL_USER_VIEW_ONLY` roles the handler
  forces `user_id = user_api_key_dict.user_id`, scoping the query to that user's
  own rows — which would **not** hit the full-table `find_all`.
- The unscoped `find_all` (the OOM path) is reached by callers **not** forced
  into that scoping — confirmed for master/admin. Whether a plain virtual key
  with `get_spend_routes` but no internal-user role can reach it, and pass the
  route-check gate, is **unverified**.

**If a low-privilege tenant key can reach the unscoped path, this is a
privilege-limited DoS** (any tenant can OOM the shared proxy) and should go to
`security@berri.ai` privately rather than a public issue. **If only master/admin
can, it is an operational hardening bug** and a public issue is appropriate.

A safe way to check (auth gate only, no unbounded query fired): mint an
internal-user key with `get_spend_routes`, then call `/spend/logs` with a
**bounded** query (`?request_id=...` or a 1-day window). If it returns 200 and
is not silently scoped to the key's own `user_id`, the unscoped path is
reachable by that role.

## Suggested fix

Apply the same bound the v2 endpoint already uses to the legacy endpoint:
a default `limit` (or the existing `SPEND_LOGS_PAGINATION_COUNT_CAP`), or make a
date range mandatory when no single-row filter (`request_id`) is present. A
one-line cap closes the DoS with no behavior change for small tables.

## Prior art

- Issue **#14218** (Sept 2025) requested pagination for `/spend/logs`; closed
  **"not planned"** — but it was framed as a *feature request*, not as a DoS on a
  still-live deprecated endpoint. This report should be filed as distinct from
  it, or as a comment reframing it.
- PR **#17742** (merged 2025-12-09) reduced *background* spend-log memory use but
  does not touch this endpoint.
- PR **#16603** added pagination to `/spend/logs/session/ui` (a different route).

## References

- `github.com/BerriAI/litellm/blob/v1.92.0/litellm/proxy/spend_tracking/spend_management_endpoints.py`
- `github.com/BerriAI/litellm/issues/14218`
- `github.com/BerriAI/litellm/pull/17742`, `.../pull/16603`
