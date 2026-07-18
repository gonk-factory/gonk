# Task 0.5 — Gas City controller smoke findings (U1 / U2 / U3)

**Status:** DONE_WITH_CONCERNS. All three unknowns were exercised against real
tooling; two of them surfaced an image-completeness gap in the Plan 04
`gonk-controller` image that changes the answers and must be fixed before the
controller can actually run a city. The findings below are what a running
process *actually did*, plus the defensible chart defaults they imply.

- **Run date:** 2026-07-17
- **Image under test:** `registry.orac.local/agentic/gonk-controller:v0.1.0-f78a0df5262d`
  (built + pushed by Plan 04; **not** rebuilt here)
- **Where measured:** (a) locally in `podman --network=host` (cheap-first, settled
  U1 + U2 + the crash mode of U3), and (b) on the **real orac cluster** in a
  throwaway namespace `gonk-smoke-<epoch>` (confirmed U1 in-cluster, cross-pod
  reachability, and U3 pod-restart tolerance). Namespace + local containers torn
  down at the end.
- **Beads/Dolt store for the smoke:** a throwaway `dolthub/dolt-sql-server`
  (root / empty password / no TLS), locally on `127.0.0.1:3306` and in-cluster as
  a headless-Service Pod `dolt-smoke:3306`. The money-ledger (CNPG) was
  deliberately **not** wired — this spike is controller + Dolt only.

---

## TL;DR for Task 1 (`values.yaml`) and Task 6.5 (templates)

| Field | Smoke answer | Confidence |
|---|---|---|
| `gascity.supervisorPort` | **9443** for the Service / dispatch / probes (the city `[api]`, `bind=0.0.0.0`). **8372** is a *separate* loopback-only supervisor admin/dashboard API — do **not** expose it or point a Service/probe at it. | High on the port *numbers* and on 8372-is-loopback (measured); **the 9443 listener could not be brought up** because the city never finishes init on this image (see U2 concern). |
| `gascity.delivery` | **prebaked** — bake the gonk pack into the image (already at `/opt/gonk/pack/`), then at startup **copy it into a writable `/city` and run `gc init --preserve-existing --bootstrap-profile k8s-cell`**. A pure read-only ConfigMap mount at `/city` is **not viable** (init must write `.gc/`, `city.toml`, `.beads/` into that dir). | High. |
| `gascity.workload` | **Deployment, `replicas: 1`, `strategy: Recreate`** (or a `StatefulSet`). The supervisor process stays alive across city-init failure (retry loop, not crash) and a deleted pod is replaced cleanly with no Dolt-lock wedge. `Recreate`/STS is required to honor the **exactly-one-controller** rule (no leader election exists). | Medium-high (see U3 caveat: the Dolt advisory-lock path was never reached because init fails upstream of it). |

**Blocking concern for Plan 04 (must fix before the controller can run a city):**
the shipped image is missing the runtime binaries `gc` shells out to. `gc`'s beads
lifecycle refuses to initialize and the city never reaches ready, so the 9443 city
API never binds and no session/sentinel handshake can fire. Details in U2.

---

## U1 — Supervisor port: **it is two ports, and the docs vs research were both half-right**

The "9443 vs 8372" question dissolves once you see that the controller runs **two
HTTP servers**:

1. **Machine-wide supervisor API — `127.0.0.1:8372`, loopback only.**
   `gc supervisor run` prints `Supervisor API listening on http://127.0.0.1:8372`
   the instant it starts, *before* any city init. Measured three ways:
   - host `ss -ltnp` (podman `--network=host`): `LISTEN 127.0.0.1:8372 users:(("gc",...))`
   - in-cluster `/proc/net/tcp` (image has no `ss`/`wget`/`curl`): the **only**
     listening socket is `0100007F:20B4` = `127.0.0.1:8372`.
   - endpoints answer on 8372: `GET /health` →
     `{"status":"ok","version":"1.1.1",...,"startup":{"ready":false,"phase":"init_failed"}}`;
     `GET /v0/readiness` and `GET /v0/provider-readiness` return the provider
     readiness JSON; `GET /` serves the dashboard HTML.
   - **This bind is loopback and I could not move it.** Restarting the supervisor
     with `GC_SUPERVISOR_ADDR=0.0.0.0:8372`, `GC_SUPERVISOR_BIND`, `GC_API_ADDR`,
     and `GC_SUPERVISOR_HOST` **all** left it on `127.0.0.1:8372`. No CLI flag
     exists (`gc supervisor run --help` has only `-h`).
   - **Cross-pod proof:** from a second pod in the same namespace,
     `wget http://<controllerPodIP>:8372/health` → **Connection refused**. A
     Kubernetes `Service`, a sidecar, or an `httpGet` liveness/readiness probe
     against the pod IP therefore **cannot** reach 8372. Treat 8372 as an
     in-pod-localhost-only admin/dashboard port.

2. **Per-city API — `0.0.0.0:9443`, `allow_mutations=true`.** `gc init
   --bootstrap-profile k8s-cell` writes this into `city.toml`:
   ```toml
   [api]
   port = 9443
   bind = "0.0.0.0"
   allow_mutations = true
   ```
   This is the server intake/dispatch should POST orders to and that a Service /
   probe should target (it is the one bound to `0.0.0.0`). **It never came up in
   this smoke** because the city never reaches ready (U2). Its existence, port,
   and `0.0.0.0` bind are config-evidence, not a live-socket measurement.

**Recommendation:** `gascity.supervisorPort` (Service, probes, NetworkPolicy) =
**9443**. Do **not** expose 8372. Re-confirm the 9443 listener actually binds once
Plan 04 ships an image that can finish city init. Until then, flag the port as
config-derived, not socket-measured.

---

## U2 — Pack/city delivery + the `gc init` → `.gc-start` sentinel

### Delivery: prebaked + copy + `--preserve-existing` is the only viable path

- The gonk pack **is** baked into the image at `/opt/gonk/pack/` (`pack.toml`
  `name = "gonk"`, `gc lint /opt/gonk/pack` → `ok`). `/city` in the image is
  **empty** — the pack is *not* prebaked at `/city`.
- **`gc init --template gascity` is the wrong mechanism** and silently discards
  the gonk pack: it **overwrites** `pack.toml` with a generated template pack
  (`name = "city"`) and rewrites `city.toml` to pull **remote GitHub imports**
  (`github.com/gastownhall/gascity-packs...`, which it fetched into
  `~/.gc/cache/repos/...` — i.e. it needs egress and does not use our pack).
- **Correct mechanism:** copy the baked pack into a writable dir and preserve it:
  ```sh
  cp -r /opt/gonk/pack/. /city/
  gc init --preserve-existing --bootstrap-profile k8s-cell \
    --default-provider claude --skip-provider-readiness \
    --dolt-host <dolt> --dolt-port 3306 \
    --dolt-database bd_<id> --dolt-project-id <id> --no-start /city
  ```
  Result: `pack.toml` stays `name = "gonk"`, `gc lint /city` → `ok`, and
  `--bootstrap-profile k8s-cell` adds the `[api] bind=0.0.0.0 allow_mutations=true`
  block. `--preserve-existing` prints `Preserved existing pack.toml.`
- **A read-only ConfigMap mounted at `/city` is NOT viable.** `gc init` writes
  `.gc/`, `city.toml`, `.beads/`, `agents/…` etc. into that directory. Deliver the
  pack **prebaked in the image** and `cp` it into an `emptyDir` `/city` at startup
  (or an initContainer). ConfigMap is fine only for *overriding individual files*
  copied in afterwards, never as the `/city` root.
- `/city` and `/home/gonk` must be writable by uid **65532**. The image's passwd
  home for 65532 is `/` (read-only), so **set `HOME=/home/gonk`** and back it with
  a writable volume, or `gc` cannot write `~/.gc`.

### Sentinel `.gc-start`: NOT fired, and could not be fired on this image

- I could not observe a `.gc-start` sentinel handshake. No file named `.gc-start`
  is created and no such string is present in the `gc` binary. The handshake (and
  any session spawn) happens only **after** the city reaches ready, which never
  happened — see the concern below.
- Session spawn was **not exercised** and is **expected not to be**: the
  `gonk-agent` image is not built/pushed and there is no LiteLLM wiring in this
  spike. Provider readiness reports the agent CLIs absent, as expected
  (`claude … "status":"not_installed","detail":"claude executable not found"`).

### CONCERN (Plan 04 image-completeness) — why the city never reaches ready

`gc`'s beads lifecycle init fails **identically** locally and in-cluster:
```
gc-fatal: init: beads lifecycle: init city beads: exec beads init: gc-beads-bd:
  setting git config --global beads.role maintainer
  managed Dolt server unreachable while inspecting existing store 'bd_<id>';
  refusing to force-reinitialize (data-safety). retry once the Dolt server is reachable.
```
Root cause: the image ships only `bd`, `gc`, `gonk-gate` in `/usr/local/bin`.
**Missing**, and each is something `gc`/`gc-beads-bd` shells out to:
`dolt` (CLI), `beads`/`gc-beads-bd`, `tmux`, `jq`, `lsof`, `pgrep`; and the bundled
`bd` is **v1.0.3 while `gc init` wants v1.0.4+**. `gc init` prints exactly this
missing-deps list. The MySQL-protocol path is fine (`bd dolt test` → `✓ Connection
successful`; `bd ready` connects and returns results), so the Dolt server is
reachable — but `gc`'s init path drives a **managed/`dolt`-CLI** code path that the
absent `dolt` binary makes "unreachable", and it will not force-reinit over an
existing DB (data-safety).

Two consequences the chart must not paper over:
1. **Do not pre-create the beads database.** A pre-existing empty `bd_<id>` makes
   `bd`/`gc` treat it as "already initialized" and refuse to write the schema. Let
   the controller create it (once the image can). In the smoke I had to
   `bd init --force` by hand to get a schema, and `gc` init *still* failed on the
   missing `dolt`/`gc-beads-bd` binaries — so this is a real image gap, not just a
   provisioning-order nit.
2. **The 9443 listener and the sentinel are gated on fixing the image.** File a
   Plan 04 issue to add `dolt`, `gc-beads-bd`/`beads`, `tmux`, `jq`, `lsof`,
   `pgrep` and bump `bd` to ≥1.0.4, then re-run this smoke to socket-confirm 9443
   and observe the `.gc-start` handshake.

---

## U3 — Workload type: **Deployment (`replicas:1`, `strategy:Recreate`) is tolerated**

Run as a single-replica `Deployment` on the real cluster:

- **The supervisor process does not crash on city-init failure.** It stays
  `Running 1/1` and retries the city with backoff (`init failure #1 … #2 … #3`,
  10s → 20s → 40s). A `Deployment`'s `restartPolicy: Always` therefore does **not**
  induce a crash-loop from the (expected, image-gap) init failure. *(An earlier
  crash-loop I saw was self-inflicted: `gc register` auto-daemonizes a background
  supervisor, after which `exec gc supervisor run` exits with "supervisor already
  running" and kills PID 1. Fixed by skipping `gc register` and writing
  `~/.gc/cities.toml` directly, then `exec gc supervisor run` as PID 1. Chart note:
  the container command must make the foreground `gc supervisor run` PID 1 and must
  NOT call `gc register`, which daemonizes.)*
- **Pod delete → clean replacement.** Deleting the pod brought a replacement to
  `Running 1/1` in ~8s with **0 restarts**; it re-attached to the same external
  Dolt and resumed the same retry loop. **No duplicate-reconcile crash, no Dolt
  advisory-lock refusal, no wedged beads state.**
- **Caveat, stated honestly:** because init fails *before* beads acquires any Dolt
  advisory lock, the specific "two controllers fight over the Dolt lock" scenario
  was **never reached**, so I cannot claim the lock path is safe under a rolling
  update — only that nothing wedged here. Since **no leader election exists**, the
  chart must still guarantee exactly one controller: use `strategy: Recreate`
  (old pod terminates before new starts; I used this and it held) or a
  `StatefulSet` (`OrderedReady`). **Do not use the default `RollingUpdate`**, whose
  `maxSurge` can briefly run two controllers.

**Recommendation:** `gascity.workload: Deployment`, `replicas: 1`,
`strategy: { type: Recreate }`. Revisit vs `StatefulSet` after the image is fixed
and the Dolt-lock path can actually be exercised across a restart.

---

## RBAC — observed vs the documented Role

I created the namespaced Role from `docs/environment.md` for SA `gc-controller`:
`pods` [get,list,watch,create,update,patch,delete], `pods/exec` [create],
`pods/log` [get], `configmaps` [get,list,watch,create,update,patch,delete].

- **No `Forbidden` events** were emitted against the controller SA during the run.
- **But the k8s session provider was never fully exercised**: session/pod
  reconciliation happens only after the city is ready, which never occurred. So
  this run **neither confirms nor expands** the documented Role — it only shows
  nothing was denied up to the (early) init-failure point. Re-verify the session
  provider's real RBAC footprint once the image can init a city and spawn a
  session pod.

---

## Exact configuration used (for reproduction)

**Env on the controller container:**
`HOME=/home/gonk`, `GC_DOLT_HOST=<dolt host>`, `GC_DOLT_PORT=3306`,
`GC_SESSION_PROVIDER=k8s`. (The city `[api] bind`/`allow_mutations` come from
`--bootstrap-profile k8s-cell`, not env. No env was found that changes the 8372
supervisor bind.)

**Container command (foreground supervisor as PID 1):**
```sh
cp -r /opt/gonk/pack/. /city/
gc init --preserve-existing --bootstrap-profile k8s-cell \
  --default-provider claude --skip-provider-readiness \
  --dolt-host dolt-smoke --dolt-port 3306 \
  --dolt-database bd_gonk --dolt-project-id gonk --no-start /city
mkdir -p /home/gonk/.gc
printf '[[cities]]\n  path = "/city"\n  name = "gonk"\n' > /home/gonk/.gc/cities.toml
exec gc supervisor run     # do NOT use `gc register` — it daemonizes a second supervisor
```

**Pod securityContext (passed PodSecurity `restricted` for the controller):**
`runAsNonRoot: true`, `runAsUser/Group: 65532`, `fsGroup: 65532`,
`seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
`capabilities.drop: [ALL]`. `/city` and `/home/gonk` were `emptyDir` volumes.

**Dolt:** `dolthub/dolt-sql-server` with `--host=0.0.0.0 --port=3306 --user=root`
(empty password, no TLS), reached as `dolt-smoke:3306` via a headless Service.

---

## Open items handed to Task 1 / Task 6.5 / Plan 04

1. **Plan 04 image is incomplete** — add `dolt`, `gc-beads-bd`/`beads`, `tmux`,
   `jq`, `lsof`, `pgrep`; bump `bd` ≥ 1.0.4. Until then the controller cannot run
   a city. (Blocking for a *working* controller; not blocking for authoring the
   chart with the defaults above.)
2. **Re-run this smoke after (1)** to socket-confirm the 9443 `0.0.0.0` listener,
   observe the `.gc-start` sentinel, and exercise the Dolt advisory-lock path
   across a pod restart (the only U3 sub-question left open).
3. **Chart defaults to encode now:** `supervisorPort: 9443`; `delivery: prebaked`
   (copy `/opt/gonk/pack` → writable `/city`, `gc init --preserve-existing
   --bootstrap-profile k8s-cell`); `workload: Deployment` + `replicas: 1` +
   `strategy: Recreate`; container command runs `gc supervisor run` as PID 1 and
   never `gc register`; `HOME=/home/gonk` on a writable volume; do not pre-create
   the beads DB; do not expose 8372.

---

## gonk-fsl fix (re-smoke, local)

**Status:** the Plan 04 image-completeness gap (U2 concern above) is **FIXED and
proven locally**. `gc init` no longer fails on missing runtime binaries, the
supervisor reaches ready, and the beads store is created in Dolt end-to-end.

- **Run date:** 2026-07-17
- **Branch / image:** `fix-gonk-fsl-controller-image`, rebuilt with `make
  controller-image` →
  `registry.orac.local/agentic/gonk-project/gonk-controller:v0.1.0-171760c58f0f`
  (podman `--network=host`; **rebuilt**, not copied into — user rule).
- **Where measured:** locally in `podman --network=host` only — the SAME setup
  that reproduced the U2 failure. The in-cluster re-run (cross-pod 9443, the U3
  Dolt-advisory-lock-across-restart branch) is still owned by the cluster phase.
- **Throwaway Dolt:** `docker.io/dolthub/dolt-sql-server:2.1.7` (matched to the
  pinned dolt CLI), run with the image's own entrypoint defaults
  (`0.0.0.0:3306`, `root`, empty password, no TLS). NOTE: the smoke's
  `dolt-sql-server:v1.43.0` tag no longer exists on Docker Hub; `2.1.7` is the
  current tag matching our `DOLT_VERSION` pin. Torn down at the end (none linger).

### What was added to the image (all pinned + checksum-verified in `images/versions.env` / `images/Dockerfile.controller`)

Source of every version/step: gascity's own build chain at `GASCITY_REF`
(`contrib/k8s/Dockerfile.base` apt list + `deps.env` +
`.github/scripts/install-dolt-archive.sh`), PORTED into our runtime stage (we
build `gc` from source, we do not `FROM gc-agent`).

| Dep | Version | Source | Where it came from (upstream) |
|---|---|---|---|
| `dolt` (CLI) | **2.1.7** | fetch stage, sha256 `15983e81…477e28e7` (linux-amd64) | `deps.env DOLT_VERSION=2.1.7`; sha hard-coded in `install-dolt-archive.sh` (re-verified by download) |
| `bd` (beads) | **1.0.3 → 1.1.0** | fetch stage, sha256 `b0f3dd60…d491a34` | `deps.env BD_VERSION=v1.1.0` (min-supported is `v1.0.4`; `gc init` refused 1.0.3) |
| `jq` | apt (trixie) | runtime `apt-get` | `Dockerfile.base` apt list |
| `lsof` | apt (trixie) | runtime `apt-get` | `Dockerfile.base` ("runtime hard dep (gc doctor)") |
| `procps` (pgrep) | apt (trixie) | runtime `apt-get` | `Dockerfile.base` apt list |
| `tmux` | apt (trixie) | runtime `apt-get` | `Dockerfile.base` apt list |
| `util-linux` (flock) | apt (trixie) | runtime `apt-get` | `Dockerfile.base` ("flock(1) for the bd/Dolt beads lock — runtime hard dep") |
| `tini` | apt (trixie) | runtime `apt-get`; `ENTRYPOINT ["/usr/bin/tini","--"]` | `Dockerfile.base` apt + `Dockerfile.controller` ENTRYPOINT (gc doesn't reap SIGCHLD) |

**`gc-beads-bd` was NOT shipped separately — and does not need to be.** The
exec:beads provider script `gc-beads-bd.sh` is **embedded in the `gc` binary** and
materialized into the city at init (`.gc/system/packs/bd/assets/scripts/…`). Proof:
the U2 BEFORE error strings ("gc-beads-bd: setting git config --global beads.role
maintainer", "managed Dolt server unreachable … refusing to force-reinitialize
(data-safety)") are verbatim lines **from that script** (gascity
`examples/bd/assets/scripts/gc-beads-bd.sh`), which means the OLD image already ran
the script — it just could not reach Dolt because the `dolt` CLI and friends the
script shells out to were absent. The fix is those binaries, not the script.
`kubectl` / `gc-beads-k8s` / `gc-events-k8s` were **NOT** added: the `k8s-cell`
bootstrap profile only sets `[api]` (verified in gascity `cmd/gc/cmd_init.go`
`applyBootstrapProfile`); it does not wire the exec k8s providers, and the session
provider is native (`GC_SESSION=k8s`, compiled in). See "still needs in-cluster"
below.

### Proof — BEFORE vs AFTER (`gc init`, local, in-container)

- **BEFORE** (U2, old image): `gc-fatal: init: beads lifecycle: init city beads:
  exec beads init: gc-beads-bd: … managed Dolt server unreachable while inspecting
  existing store 'bd_gonk'; refusing to force-reinitialize (data-safety).`
- **AFTER** (this image): `gc init … --no-start /city` → **exit 0**, prints
  `Welcome to Gas City!` / `Preserved existing pack.toml.`; `pack.toml` stays
  `name = "gonk"`; `city.toml` has the `[api] port=9443 bind="0.0.0.0"
  allow_mutations=true` block and a `[dolt] host="127.0.0.1" port=3306`.

### Proof — supervisor + beads store creation (the U2 gate)

Ran the U2 container command (`cp` pack → `gc init` → write `~/.gc/cities.toml` →
`exec gc supervisor run`):

- **Supervisor reaches ready.** `GET http://127.0.0.1:8372/health` →
  `{"status":"ok","version":"1.1.1",…,"cities_running":1,"startup":{"ready":true,
  "phase":"running","phases_completed":["loading_config","starting_bead_store",
  "resolving_formulas","adopting_sessions","starting_agents"]}}`. The previously
  unreachable `starting_bead_store` phase now **completes** (took ~5.7s).
- **The beads store is CREATED in Dolt.** `SHOW DATABASES` on the throwaway Dolt
  now lists **`bd_gonk`** (absent before init), and `SHOW TABLES FROM bd_gonk`
  returns the full beads schema (`issues`, `dependencies`, `events`, `comments`,
  `labels`, `issue_counter`, …). This is the exact `gc-beads-bd` → `dolt` path
  that failed in U2, now working end-to-end.

### Observed bind ports (corrects U1 for supervisor mode)

- **8372 — binds, loopback only.** `ss -ltnp`: `LISTEN 127.0.0.1:8372
  users:(("gc",…))`. Same as U1.
- **9443 — does NOT bind under `gc supervisor run`.** New, measured finding: the
  supervisor log says verbatim **`city 'gonk' has [api] port=9443 which is ignored
  under supervisor mode`**. So the city `[api]` (9443) is *config* consumed by
  `gc start`, but `gc supervisor run` multiplexes through 8372 and does **not**
  open a 9443 socket in this topology. U1's "point the Service/probe at 9443"
  recommendation must be re-decided against this: either the chart runs the city
  via `gc start` (which does open 9443) rather than bare `gc supervisor run`, or
  the front door is fronted differently. **This is a chart/topology question the
  in-cluster re-run must settle — flagged, not resolved here.**

### `.gc-start` sentinel

- **Did not fire — and is not part of the `gc supervisor run` path.** `.gc-start`
  is the deploy handshake in gascity's *own* `Dockerfile.controller` CMD, which
  waits for `/city/.gc-start` and then `exec gc start --foreground`. gonk runs
  `gc supervisor run` (per U3's PID-1 finding), which starts the city directly and
  reaches `ready` with no `.gc-start` file (none appears in `/city`). If the chart
  moves to a `gc start`-based front door to get the 9443 socket (see above), the
  sentinel handshake comes back into scope; under supervisor mode it does not.

### What STILL needs the in-cluster re-run

1. **9443 cross-pod reachability** — decide `gc start` (opens 9443) vs `gc
   supervisor run` (ignores 9443), then socket-confirm 9443 on the pod IP from a
   second pod / the Service. Not answerable locally under supervisor mode.
2. **U3 Dolt advisory-lock across a pod restart** — still unreached: init now
   succeeds, so this path is finally *reachable*, but exercising two controllers
   contending for the Dolt lock across a `Recreate` rollout needs the cluster.
3. **Session/events RBAC footprint** — the k8s session provider (and any
   `gc-events-k8s`/`kubectl` need) only exercises once a session is actually
   spawned in-cluster; re-verify the Role and whether `kubectl`/`gc-events-k8s`
   must be added at that point.
