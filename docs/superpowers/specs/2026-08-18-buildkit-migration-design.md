# Design — buildkit migration (gonk pilot)

_Status: DESIGN ONLY, nothing built._

_Beads: `gonk-03f` (epic) → `gonk-sl1` (rule out the fork first) + `gonk-4by`
(Talos spike) → `gonk-zhe` (publish the template) → `gonk-d38` (migrate gonk and
score it). `gonk-c0y` is the owner decision in §4 and blocks nothing._

_Decided 2026-08-18: gonk is the pilot; the rest of the fleet migrates later on
evidence. Phase 0 of the local-parity roadmap remains the active work — this is
a CI cost/throughput change, not on the path to a merged MR._

## 1. What kaniko is and is not responsible for

The prompt for this design was "given all these build errors and memory pressure
from kaniko". The build errors are not kaniko's. Measured, not inferred:

| Failure | Pipelines | Actual cause |
|---|---|---|
| 8 image jobs fail at push | 2102, 2103, 2113, 2123, 2124, 2132 | `ENOSPC` — registry volume 100% full (`gonk-mzm`) |
| `controller-image-arm64` | 2177 | `GET https://gitlab.orac.local/jwt/auth?…&service=container_registry` → **500** |
| `meter-image-arm64` | 2177 | `runner_system_failure` |
| all image jobs "skipped" | 2187 | Correct behaviour — `10dda82`'s `changes:` rules on a docs-only commit |

Buildkit would have failed identically on every one. Two of these are worth
remembering independently: the registry's *"error checking push permissions"*
wrapper has now fronted for two unrelated root causes (ENOSPC and a GitLab 500),
and pipeline 2187 was **green with nothing built** — this project's signature
failure shape, in the CI layer.

**What kaniko IS responsible for**, ranked by how much it actually costs:

1. **Serialized builds.** `resource_group: gonk-images-$BUILD_ARCH` exists
   because parallel kaniko pods contend for memory on johnny, and
   `--compressed-caching=false` is a second memory workaround. Five images build
   one at a time per arch. This is the real, ongoing cost and the strongest
   argument for the migration — an argument about **wall-clock**, not
   correctness.
2. **Unbounded cache.** `--cache-repo="$INTERNAL_REGISTRY_REPO/cache"` reached
   **139 GB across 1296 tags** and was the single largest contributor to filling
   a 296 GB volume (`gonk-1t4`). kaniko has no cache GC; the remedy was a manual
   prune plus `10dda82` dropping caches that cannot hit. Buildkit's
   `gckeepstorage` bounds this structurally instead of retroactively.
3. **Upstream is dead.** GoogleContainerTools/kaniko was
   [archived 2025-06-03](https://github.com/GoogleContainerTools/kaniko) and is
   read-only; the maintainers retired and no CVE will be fixed there.
   [Chainguard forked it](https://www.chainguard.dev/unchained/fork-yeah-were-bringing-kaniko-back)
   with two of the original creators.

## 2. The cheaper option that must be ruled out first

Argument 3 — the one that sounds most alarming — is satisfiable by **changing an
image reference**. The Chainguard fork is a drop-in for
`gcr.io/kaniko-project/executor`. That is hours of work against weeks for a
buildkit migration, and it resolves the security-maintenance concern completely.

So the honest framing: **buildkit must be justified on parallelism and bounded
cache alone.** If those are not worth the migration, take the fork and stop.
This is `gonk-sl1`, and it is deliberately sequenced FIRST — it blocks
`gonk-zhe` — so the expensive path is only entered once the cheap one is known
to be insufficient.

## 3. Capacity plan (measured 2026-08-18)

| Node | Arch | CPU alloc | Mem alloc | Mem in use | Headroom |
|---|---|---|---|---|---|
| johnny | amd64 | 15950m | 29.2 Gi | 48% | **~15 Gi** |
| venus | amd64 | 3950m | 15.7 Gi | 60% | ~6 Gi |
| bailey | arm64 | 20 | **127 Gi** | 13% | **~110 Gi** |
| orac01 | arm64 | 3950m | 7.3 Gi | 52% | ~3.5 Gi |
| orac02 | arm64 | 3950m | 7.3 Gi | 60% | ~2.9 Gi |
| orac03 | arm64 | 3950m | 7.3 Gi | 62% | ~2.8 Gi |
| orac04 | arm64 | 3950m | 7.5 Gi | **85%** | ~1.1 Gi |

Current kaniko jobs request 2Gi and cap at 6Gi.

**amd64 is comfortable.** johnny alone absorbs a persistent buildkitd plus
parallel builds within its ~15 Gi headroom.

**arm64 is the whole problem, and it is a policy problem rather than a hardware
one.** orac01–04 cannot host a builder: three have under 3.5 Gi free and orac04
is at 85%. The only arm64 machine with room is bailey, with ~110 Gi idle.

## 4. The bailey constraint — the central open question

`homelab/ci-templates` `/kaniko.yml` already pins arm64 builds to bailey:

```yaml
KUBERNETES_NODE_SELECTOR_ARCH:     "kubernetes.io/arch=arm64"
KUBERNETES_NODE_SELECTOR_HOSTNAME: "kubernetes.io/hostname=bailey"
KUBERNETES_NODE_TOLERATIONS_GPU:   "orac.local/gpu-only=true:NoSchedule"
```

with the comment that orac01–04 are "~7Gi with cold image caches, which leaves"
too little. So arm64 CI builds are **already** an established exception to the
"bailey runs GPU workloads exclusively" rule.

That precedent does not automatically extend, and the difference is the point:

- today bailey hosts **ephemeral job pods** that tolerate the taint, appear for
  the length of a build, and leave;
- a persistent `buildkitd` is a **long-lived daemon holding a cache PVC**, which
  is a materially larger claim on a machine whose exclusivity is enforced by
  taint and label and is treated as load-bearing.

**This is the owner's call and the design does not presume it.** It is the
question that decides the architecture, so it is asked before anything is built:

> Does the bailey GPU-exclusivity policy permit a *persistent* arm64 buildkitd,
> or only the ephemeral job pods it already tolerates?

### The two architectures that answer falls out to

**(A) Persistent buildkitd per arch — if bailey may host a daemon.**
`Deployment` + `Service` per arch, cache on a PVC with `gckeepstorage` set below
the volume size. CI jobs become thin `buildctl` clients. Best cache hit rate,
biggest wall-clock win, largest policy ask, and it puts a CI dependency on the
GPU host's availability.

**(B) Ephemeral buildkitd inside the job — if it may not.**
Each job runs `buildkitd` in rootless mode alongside `buildctl`, exactly the
lifecycle kaniko has today, so **no policy change is needed at all**. Cache
moves to registry-backed (`--export-cache mode=max`) or a per-arch PVC mounted
into the job. Keeps parallelism (buildkit's memory profile is better-behaved
than kaniko's), loses warm-daemon cache locality.

**(B) is the recommended starting point** regardless of the answer: it is
strictly less invasive, it needs no decision from anyone before work can begin,
and it still delivers both of the benefits that justify the migration. (A)
becomes an optimization to measure later rather than a prerequisite.

## 5. Migration surface

There are **two independent kaniko implementations**, not one:

| Template project | Consumers |
|---|---|
| `homelab/ci-templates` (65) `/kaniko.yml` | `agentic/gonk-project` (69), `agentic/orac-gpu-scheduler` (68), `agentic/nagus` (67), `steve/glovebox` (7) |
| `aether-fm/platform/ci-templates` (86) `/kaniko.yml` | ~8 aether-fm projects (narrative-engine, ops-dashboard, website, streaming-service, audio-generator, content-repo, archival-pipeline, shared) |

About a dozen projects total. The pilot touches **only** `homelab` + gonk;
aether-fm is explicitly out of scope and migrates later, or never, on evidence.

Note `agentic/nagus` is also one of the three repos `gonk-4v8` picks as a first
real gonk target. Migrating its CI while gonk is learning to file MRs against it
changes two variables at once — sequence them apart.

## 6. Dual-mode, not a cutover

Per the standing preference for dual-mode over either/or migrations: publish
`/buildkit.yml` **alongside** `/kaniko.yml` in `homelab/ci-templates`. Both live
side by side; a consumer opts in by changing its `include:`; kaniko is deleted
only when the last consumer has moved. There is no flag day and every step is
revertible by reverting one `include:`.

What `/buildkit.yml` must replicate from the kaniko template (do not rediscover
these):

- `INTERNAL_REGISTRY_REPO` / `INTERNAL_REGISTRY_HOST` — in-cluster registry over
  plain HTTP, so buildkit needs the registry marked insecure in `buildkitd.toml`
  (kaniko's `--insecure-registry`).
- `ZOT_MIRROR` (`zot.registry-system.svc.cluster.local:5000`) — the pull-through
  cache, as a `[registry."docker.io"] mirrors` entry (kaniko's
  `--registry-mirror`). Missing this reintroduces the Docker Hub rate-limit and
  the uncached-multi-arch-pull flakiness.
- The `$CI_REGISTRY_USER:$CI_REGISTRY_PASSWORD` docker-config auth block, for
  both the external and internal registry hosts.
- Per-arch native legs plus a `crane` manifest merge — **keep this shape.**
  Buildkit can emit a multi-platform index directly, but only via QEMU
  emulation, which is far slower than two native legs on this cluster.
- The arch/hostname/toleration selector trio. Retarget arch pins, never delete
  them: a job with no `kubernetes.io/arch` selector gets the amd64 helper image
  and dies with `no match for platform in manifest` on an arm64 node.

## 7. Verification

Per image, not per pipeline:

1. `crane manifest` shows an OCI index with **both** `linux/amd64` and
   `linux/arm64` — the failure mode that produced `bed8792`'s half-built tags.
2. The binary actually runs on an arm64 node *and* an amd64 node. An index that
   exists is not an index that works.
3. Cache stays under its configured ceiling across ~10 consecutive builds. This
   is the claim that justifies the migration, so it gets measured, not assumed.
4. Wall-clock for a full five-image rebuild, against the serialized kaniko
   baseline. If it is not meaningfully faster, argument 1 has failed and the
   Chainguard fork was the right answer.
5. A build whose Dockerfile is unchanged hits cache; one with a changed early
   layer does not.

Gate: gonk runs on `/buildkit.yml` for **two weeks** with no image-related
incident before any second consumer is invited.

## 8. Risks

- **Rootless buildkitd on Talos.** Needs verifying that user namespaces /
  seccomp permit it without a node-config change. If it does, that change lives
  in the **hardware** repo, not gitops, and crosses a repo boundary — which
  would materially raise the cost of this whole plan. **Check this before
  writing any YAML;** it is the assumption most likely to invalidate the design.
- **A CI dependency on bailey** under architecture (A): if bailey is busy with
  GPU work or down, arm64 builds stop. Today that is already half-true for
  ephemeral jobs.
- **Two template repos drifting.** Migrating homelab's and not aether-fm's means
  two idioms coexist. That is acceptable *because they are already separate
  files with separate consumers* — but it is the same "two idioms" trap that
  produced the dead mention trigger in gonk, so it wants an explicit end state.
- **Doing this instead of Phase 0.** The roadmap's throughput-before-intake
  principle applies to gonk's own plumbing too: faster image builds do not move
  a single issue closer to a merged MR.

## 9. Open questions for the owner

1. **The bailey question in §4** — persistent daemon, or ephemeral only? Only
   this one blocks architecture choice, and (B) proceeds without an answer.
2. Is `aether-fm/platform/ci-templates` ever in scope, or is it independently
   owned and left alone permanently? Changes whether §6's end state is "one
   idiom" or "two, deliberately".
