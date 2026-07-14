# ADR-003: intake's trust boundary and seams

Status: accepted 2026-07-14

This ADR records the decisions Plan 02 (`pkg/glab`, `pkg/ghook`, `pkg/intake`,
`cmd/gonk-intake`) locks in, so later plans cannot silently undo them. It is the
GitLab-side counterpart to ADR-002 (config precedence) and cross-references
ADR-004 (Plan 03), which records the other half of the split.

1. **Verify before parse.** `ghook.Handler.ServeHTTP` checks `X-Gitlab-Token`
   before the body is read at all (`pkg/ghook/receiver.go`). Consequence: the
   webhook secret is instance-wide, not per-project (AD-1) -- a per-project
   secret would require parsing untrusted input to decide which key to check,
   which is exactly the ordering this rule forbids.

2. **The public listener serves exactly one path.** `intake.Server.Public()`
   (`pkg/intake/server.go`) serves `POST /hook/gitlab` and nothing else --
   every other path 404s. `/metrics`, `/healthz`, `/readyz`, and
   `POST /admin/reconcile` live on `Server.Private()`, a second,
   cluster-internal listener. `cmd/gonk-intake` binds them to two different
   addresses (`GONK_LISTEN_ADDR` default `:8080`, `GONK_PRIVATE_ADDR` default
   `:9090`); only the first may ever sit behind an Ingress.

3. **Reconciliation is the correctness path.** Webhook loss (a full event
   queue, a misconfigured hook, GitLab down) degrades latency, never
   correctness -- the next reconcile pass repairs the hook and rebuilds the
   cache regardless. Therefore `ghook.Handler` answers GitLab with `200` for
   anything it accepts-and-drops (unhandled event, duplicate, bot-authored,
   queue full): `4xx`/`5xx` would get the hook auto-disabled by GitLab, which
   *would* be a correctness problem. Only a bad/missing token (`401`), a bad
   method (`405`), a bad content type (`415`), or an oversize body (`413`) are
   non-200.

4. **No persistent state.** `intake.Cache` is derived, in-memory only
   (`pkg/intake/cache.go`). A restart rebuilds it from GitLab **and
   gonk-meter** on the first reconcile pass; `cmd/gonk-intake` kicks that pass
   immediately at startup rather than waiting a full
   `GONK_RECONCILE_INTERVAL`.

5. **Hook token freshness via a generation marker.** GitLab never returns a
   hook's token once set, so there is no way to compare the configured secret
   against the live one. `Reconciler.hookURL()` embeds `?gen=N`
   (`GONK_WEBHOOK_TOKEN_GEN`) as the observable proxy: bump the generation on
   a webhook-secret rotation, and `ensureHook` repairs every hook whose URL
   does not carry the current generation.

6. **INTAKE DOES NOT RESOLVE CONFIG.** `gonkcfg.Resolve` is not called
   anywhere in `pkg/intake` or `cmd/gonk-intake` outside of one test
   (`render_test.go`'s `TestDefaultConfigIsValidAndEnabled`, which asserts a
   property of the onboarding *template* -- that the `.gonk.yml` it commits
   resolves to enabled/triage-only/$0-cloud-budget -- not a second resolver in
   the running system). `pkg/intake/reconcile.go`'s `register` sends
   gonk-meter the **raw `.gonk.yml` bytes**; gonk-meter holds operator
   (instance/group) policy and is the only place that *can* resolve. Two
   resolvers would be two sources of truth for a budget ceiling. Intake may
   call `gonkcfg.Load` to ask "do these bytes parse?" (`Classification.LooksInvalid`),
   but that answer is **advisory** -- it drives a metric and the onboarding
   flow's messaging, nothing else. Where intake and meter disagree, **meter
   wins**: `invalid`, `disabled`, and `key-missing` are meter's answers,
   recorded verbatim in `Classification.Meter`. This ADR does not modify
   ADR-002; it narrows *who applies it*.

7. **INTAKE DOES NOT EVALUATE QUIET HOURS.** `schedule.quiet_hours` is a
   wait-vs-spend decision, and every wait-vs-spend decision belongs to meter,
   computed only inside `POST /v1/policy/decide`. `Dispatch.Handle` (Gate 1,
   `pkg/intake/dispatch.go`) asks meter and gets quiet hours back as a
   `defer` + `retry_after`, which it records (`Obs.DispatchDropped("decide_defer")`)
   and fires nothing; the pack's Gate 2 (every pour, Plan 04) is where the
   bead actually waits out the window. `OrderRequest` carries no
   `not_before` field, and neither `Decision` (the pure gate's verdict) nor
   `Dispatch` itself holds a clock for scheduling -- the one clock seam,
   `Dispatch.Now`, backs only the staleness cutoff (decision 10), never
   quiet hours.

8. **Fail closed.** `Entry.Dispatchable()` (`pkg/intake/cache.go`) is a
   **whitelist** -- `valid` and `pending` only. Unknown project, unresolved
   policy (`unsynced`), invalid config, disabled config, missing virtual key,
   an unmanaged/absent/declined project, or an unreachable meter (`decide_error`)
   all mean **no dispatch**. A state added to `pkg/intake/state.go` later is
   non-dispatchable by default, which is the safe direction, not an oversight
   to fix.

9. **The meter seam is `pkg/meterapi`, owned by Plan 03.** Intake imports it
   and restates nothing: `MeterClient` (`pkg/intake/meterclient.go`) and
   `Dispatch.Meter` (`DecideClient`) both speak `meterapi.ProjectRequest` /
   `meterapi.ProjectResponse` / `meterapi.DecideRequest` /
   `meterapi.DecideResponse` verbatim. `+Inf` / `MaxInt64` become `null` on
   that wire (ADR-002); an empty `meterapi.Budget{}` means *unlimited*, never
   *zero* -- `meterapi.ZeroBudget()` is the explicit fail-closed value.

10. **Intake's GitLab-state gate is a pre-filter, never enforcement.** `Decide`
    (`pkg/intake/dispatch.go`) exists to avoid asking meter about work meter
    would certainly refuse -- it must be a **subset** of meter's rules, never a
    superset: dropping work meter would have allowed is a silent bug, harder to
    see than the reverse. The rung/budget gate is `POST /v1/policy/decide`,
    called at **Gate 1** (`Dispatch.Handle`, before the first dispatch) and
    **Gate 2** (the pack, every pour -- the actual enforcement chokepoint,
    Plan 04). The one exception: `triage.respond_to_mentions`
    (`Classification.MayMentionReply`) is *not* one of `meterapi.Effective.Actions`,
    so meter never checks it -- intake is its only enforcement point for that
    setting.

    A related, narrower fail-closed rule lives at the *dispatch* layer, not the
    state layer: **the staleness cutoff.** `Dispatch.stale` refuses to fire (or
    even ask meter) for a project whose cached `Entry.LastReconcile` has not
    been refreshed within `Dispatch.StalenessWindow` (default
    `DefaultStalenessWindow = 30m`, `GONK_DISPATCH_STALENESS_WINDOW` to
    override). `LastReconcile` only advances on a *successful* reconcile pass,
    so a transient GitLab blip is tolerated (the last-known-good `Entry`
    survives), but sustained uncertainty about a project's policy must not let
    dispatch keep firing on stale trust. This is owner-approved scope beyond
    Plan 02's original text, not a Plan 04 concern.

11. **The order seam is `intake.Dispatcher`; OD-A is RESOLVED by Plan 04,
    Task 2**: `POST /v0/city/{cityName}/order/gonk-dispatch/run`, body
    `{"vars": {...}}`, no per-route auth (admission is by network position).
    `intake.HTTPDispatcher` (`pkg/intake/dispatch.go`) is a minimal client for
    that contract -- it marshals `OrderRequest` through its own JSON tags into
    `vars`, so the shape has exactly one source of truth. It exists so Task 10
    does not block on Plan 04's `pkg/gcapi`, which a later caller with access
    to it should prefer. The Gas City controller MUST treat
    `OrderRequest.BeadAnchor` as an idempotency key: intake can fire the same
    order twice across a restart (a duplicate webhook delivery, a crash
    mid-flight), and a duplicate bead means duplicate spend.

12. **Secrets are file mounts with rotation slots, never env values.** Every
    `GONK_*_FILE` env var in `cmd/gonk-intake/main.go`'s `Config` carries a
    *path*; `readSecretFile` is the only thing that reads the material, and it
    refuses an empty file (an empty webhook secret would accept every
    request). Two rotation slots exist for the GitLab bot PAT
    (`GONK_GITLAB_TOKEN_FILE` / `GONK_GITLAB_TOKEN_PREVIOUS_FILE`, AD-4b) and
    the webhook secret (`GONK_WEBHOOK_SECRET_FILE` /
    `GONK_WEBHOOK_SECRET_PREVIOUS_FILE`) -- a credential intake *verifies*
    accepts either slot (`ghook.NewVerifier`); a credential intake *presents*
    (the GitLab PAT) tries slot 1 and retries ONCE on a 401/403 with slot 2
    (`glab.Client`'s `AuthFallback`, wired to
    `gonk_intake_gitlab_auth_fallback_total` -- nonzero means a rotation is
    half-done). The meter bearer token is presented by intake and *verified by
    meter*, so intake carries slot 1 only; `GONK_METER_TOKEN_PREVIOUS_FILE` is
    meter's env var (Plan 03), not intake's.

13. **TLS to GitLab trusts the private CA; verification is never disabled.**
    `glab.Client`'s `http.Client` carries no `TLSClientConfig` at all --
    `gitlab.orac.local`'s private CA is trusted through `SSL_CERT_FILE`, which
    the chart sets from the `trust-manager` `trust-bundle` ConfigMap
    (`docs/environment.md`). There is no config value anywhere in `pkg/glab`
    or `cmd/gonk-intake` that lets a caller set `InsecureSkipVerify`.

14. **`POST /admin/reconcile` is unauthenticated, private-listener-only, and
    bounded.** It is not a spend surface -- it fires no order that `/decide`
    would not gate -- but it is free work on an unauthenticated endpoint, so
    concurrent kicks must not amplify: `Reconciler.Kick()` coalesces into at
    most one extra pass via a buffered `chan struct{}` of size 1
    (`TestKickCoalesces`). `?wait=true` (Plan 06's hand-back HB-1) blocks until
    a pass that **started at or after the request** completes, then returns a
    `ReconcileSummary` -- never a pass already in flight when the request
    arrived, which would silently observe a pre-request world. This is what
    lets Plan 06's e2e suite assert on reconciliation without sleeping.
    `Reconciler` tracks this with a monotonic start-sequence number assigned
    before each pass runs and a completion sequence assigned after
    (`pkg/intake/reconcile.go`); `WaitForNextPass` records the sequence in
    flight (or most recently completed) *before* kicking, then waits for a
    strictly later one to complete. The endpoint is bounded server-side by
    `ServerConfig.AdminWaitTimeout` (default 30s) composed with the request
    context.

15. **Nothing in intake calls a model.** `cmd/gonk-intake/main.go`'s package
    doc says so explicitly. No HTTP client in this component talks to
    anything but GitLab, gonk-meter, or the Gas City supervisor order API.

Cross-reference `ADR-004` (Plan 03), which records the other half of the
split: gonk-meter owns `pkg/meterapi`, validates and resolves the raw
`.gonk.yml`, and owns quiet hours as a `defer`.
