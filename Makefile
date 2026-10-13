include images/versions.env

# GONK_TAG: NEVER `latest`. The image tag is the pinned GONK_VERSION plus the
# git SHA that built it -- reproducible, and traceable back to a commit
# (Task 5 Step 5 / Task 7 Step 4).
GONK_TAG ?= $(GONK_VERSION)-$(shell git rev-parse --short=12 HEAD)

# TARGETARCH: the Dockerfiles fail closed if this is unset rather than guess
# which architecture to fetch prebuilt binaries for (opencode/glab/bd/dolt).
# podman does auto-populate it, but pass it explicitly so a cross-build is a
# visible flag rather than an accident of which machine you ran make on.
# Defaults to THIS host's arch, which is what a local `make images` wants.
TARGETARCH ?= $(shell go env GOARCH)

# PODMAN, and --network=host: the build must not depend on a working container
# bridge network (docs/environment.md, "Building and testing this repository").
# Do not "fix" it by removing the flag.
PODMAN := podman
BUILD  := $(PODMAN) build --network=host --build-arg TARGETARCH=$(TARGETARCH)

.PHONY: images agent-image controller-image intake-image meter-image meter-testclock-image push pack-validate no-latest lint-pack scan

# images: the full four-image list Task 7 formalizes -- gonk-agent (Task 5),
# gonk-controller (Task 6), gonk-intake and gonk-meter (Task 7) -- plus the
# meter's separate testclock variant (e2e-only, never pushed as
# gonk-meter:$(GONK_TAG)).
images: no-latest agent-image controller-image intake-image meter-image meter-testclock-image

agent-image:
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DEBIAN_BASE=$(DEBIAN_BASE) \
	  --build-arg OPENCODE_VERSION=$(OPENCODE_VERSION) \
	  -f images/Dockerfile.agent -t $(REGISTRY)/gonk-agent:$(GONK_TAG) .

# controller-image: gc (MIT gascity, pinned GASCITY_REF) + gonk-gate + bd +
# the pack (Task 6). Kept as its own target (not folded only into `images`)
# so `pack-validate` below can build just this one, without also needing
# network reach to opencode's/glab's release servers that agent-image's fetch
# stage requires.
controller-image:
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DEBIAN_BASE=$(DEBIAN_BASE) \
	  --build-arg BD_VERSION=$(BD_VERSION) \
	  --build-arg DOLT_VERSION=$(DOLT_VERSION) \
	  --build-arg GASCITY_REF=$(GASCITY_REF) \
	  -f images/Dockerfile.controller -t $(REGISTRY)/gonk-controller:$(GONK_TAG) .

# intake-image / meter-image: the two Plan 02/03 service binaries (Task 7).
# Both are pure vendored Go builds (no fetch stage, no apt packages at
# runtime, unlike agent/controller) onto a distroless nonroot base.
intake-image:
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DISTROLESS_BASE=$(DISTROLESS_BASE) \
	  -f images/Dockerfile.intake -t $(REGISTRY)/gonk-intake:$(GONK_TAG) .

meter-image:
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DISTROLESS_BASE=$(DISTROLESS_BASE) \
	  -f images/Dockerfile.meter -t $(REGISTRY)/gonk-meter:$(GONK_TAG) .

# meter-testclock-image: the e2e-only meter whose clock can be MOVED BY A
# FILE (cmd/gonk-meter/clock_testclock.go). Tagged $(GONK_TAG)-testclock, NEVER
# $(GONK_TAG) -- a clock that can be moved by a file is a budget window that
# can be reset by a file, and that must never ship as the production tag.
# test/images/servers_smoke_test.go's TestProductionMeterImageHasNoTestClock /
# TestTestclockMeterImageHasTheSeam assert the two images differ exactly this
# way.
meter-testclock-image:
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DISTROLESS_BASE=$(DISTROLESS_BASE) \
	  --build-arg BUILD_TAGS=testclock \
	  -f images/Dockerfile.meter -t $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock .

# push: a HAND build, and a hand build is SINGLE-ARCH -- it is whatever
# architecture the box running make happens to be ($(TARGETARCH)).
#
# This target used to push $(GONK_TAG) itself, from the era when every runner
# was offline and images HAD to be built by hand. That era is over: CI builds
# all five images on both architectures and publishes $(GONK_TAG) as an OCI
# index over them.
#
# A hand `make push` once overwrote four of those indexes with single-arch
# images. On a mixed-architecture cluster every pod that lands on a node of
# the other architecture then crash-loops with `exec format error`, while CI
# stays green if its verification cannot tell a single-arch image from an
# index (gonk-n50).
#
# So a hand push can no longer write a DEPLOYABLE tag at all. It writes only
# arch-suffixed tags, which is exactly the convention CI's own legs use: these
# stay usable as index inputs and can never themselves be deployed by accident.
# If you need a deployable tag, push a commit and let CI build it.
#
# internal/buildgate asserts this property so a future edit cannot quietly
# reintroduce the unsuffixed push.
push: images
	@echo "pushing SINGLE-ARCH $(GONK_TAG)-$(TARGETARCH) to $(REGISTRY)"
	@echo "NOTE: $(GONK_TAG) itself is published by CI as a multi-arch index and is NOT written here (gonk-9ub)"
	for i in gonk-agent gonk-controller gonk-intake gonk-meter; do \
	  $(PODMAN) tag $(REGISTRY)/$$i:$(GONK_TAG) $(REGISTRY)/$$i:$(GONK_TAG)-$(TARGETARCH); \
	  $(PODMAN) push $(REGISTRY)/$$i:$(GONK_TAG)-$(TARGETARCH); done
	$(PODMAN) tag $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock-$(TARGETARCH)
	$(PODMAN) push $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock-$(TARGETARCH)

# scan: Trivy every built image (spec 10.3: "image builds smoke-tested and
# trivy-scanned"). --ignore-unfixed is deliberate: failing the build on a CVE
# with no fix available teaches people to pass --skip, and then the scan is
# worthless. Not run as part of this task (no trivy binary on this sandbox,
# and this task's own scope is the images + the no-latest gate) -- Plan 05/CI
# formalizes actually running it.
scan:
	for i in gonk-agent gonk-controller gonk-intake gonk-meter; do \
	  trivy image --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed \
	    $(REGISTRY)/$$i:$(GONK_TAG) || exit 1; done

# pack-validate: Gas City's REAL loader (internal/config's pack parser, via
# `gc lint`), offline, in the controller image -- no cluster, no deployed
# city (Task 6, Plan 04). This is NOT a substitute for
# `go test ./internal/packtest/` (Task 4's OFFLINE structural allow-list) --
# it is the complementary check: the actual code that will reject an unknown
# pack.toml key in production, run against our pack now. See
# test/images/packvalidate_test.go for the fuller picture, including the
# negative controls and the order-level semantic checks `gc lint` does NOT
# cover (documented there and in ADR-004).
pack-validate: controller-image
	@echo "pack-validate: gc lint (Gas City's real loader) against the gonk pack, offline, in the controller image"
	$(PODMAN) run --rm --network=host \
	  $(REGISTRY)/gonk-controller:$(GONK_TAG) gc lint /opt/gonk/pack

# no-latest: docs/environment.md is unambiguous -- "Pin exact tags -- never
# latest." Renovate autodiscovers and bumps PINNED tags; it cannot bump a
# floating one, and a floating one cannot be rolled back or reasoned about.
#
# BANNED_TAG is assembled with $(empty) so THIS FILE never contains the
# banned substring as one contiguous token -- otherwise this very recipe
# (which necessarily talks about the substring) would trip its own check the
# moment `grep -rn ... Makefile` reached this line. Make expands $(empty) to
# nothing before the shell ever sees it, so the search still works at runtime.
empty :=
BANNED_TAG := :late$(empty)st

# REAL BUG FOUND AND FIXED HERE (Task 7): `chart/` does not exist yet (it is
# Plan 05's own deliverable). GNU grep exits 2 -- not "no match" (1), an ERROR
# -- when ANY path argument is missing, REGARDLESS of what it found in the
# paths that DO exist. `! grep ... || (echo FAIL; exit 1)` therefore turned a
# REAL planted `:late$(empty)st` in images/Dockerfile.intake into a silent
# PASS: `!` only distinguishes zero from nonzero, so exit 2 (error) and exit 1
# (no match) both flip to 0. Verified two ways: (1) `grep -rn ... chart/`
# alone, piped through `echo $?`, prints the match AND reports exit 2; (2) the
# same grep with `chart/` dropped from the argument list correctly reports
# exit 0 (match found). The fix: only pass paths that exist to grep, so a
# missing Plan-05 directory can never again mask a real hit.
#
# SECOND FALSE-POSITIVE SOURCE FIXED (Plan 05, Task 9): once `chart/` exists it
# carries VENDORED UPSTREAM CRD schemas under chart/gonk/tests/crd-schemas/
# (CNPG, prometheus-operator). Those JSON files legitimately contain the banned
# token inside Kubernetes' own imagePullPolicy documentation (the sentence about
# the default pull policy when that tag is specified). That is upstream API
# prose, not one of OUR image tags, so the scan excludes that one vendored
# directory by name -- every file we actually author (Dockerfiles, values.yaml,
# ci/ profiles) is still scanned. Uses GNU grep's --exclude-dir (present in the
# alpine CI image via `apk add grep`, and locally). NOTE: like BANNED_TAG's own
# $(empty) trick, this comment must never spell the token as one contiguous
# string, or `grep -rn ... Makefile` would trip on the comment itself.
# THIRD FALSE-POSITIVE SOURCE FIXED (T-04): `! grep ... || (echo FAIL; exit
# 1)` only distinguishes zero exit from nonzero exit -- it cannot tell "grep
# found nothing" (exit 1, the pass case) apart from "grep itself errored"
# (exit >=2: a bad flag, an unreadable file, anything). Both looked like
# "no match" and both flipped `!` to a silent pass. Capture grep's actual
# exit code instead: 0 is a real hit (fail, banned tag found), 1 is a clean
# scan (pass), and anything else is a grep error (fail, with grep's own
# stderr shown so the error is visible instead of swallowed).
no-latest:
	@paths="images Makefile test"; \
	  [ -d chart ] && paths="$$paths chart"; \
	  out=$$(grep -rn --exclude-dir=crd-schemas '$(BANNED_TAG)' $$paths 2>&1); \
	  code=$$?; \
	  if [ $$code -eq 0 ]; then \
	    echo "$$out"; \
	    echo "FAIL: a floating image tag was found. Pin an exact tag (docs/environment.md)."; \
	    exit 1; \
	  elif [ $$code -eq 1 ]; then \
	    exit 0; \
	  else \
	    echo "$$out" >&2; \
	    echo "FAIL: grep exited $$code scanning for a floating image tag -- that is a scan error, not a clean pass." >&2; \
	    exit 1; \
	  fi

# lint-pack runs the pack's anti-drift greps (Plan 04, Task 4, Step 7). The
# same three properties are ALSO asserted as permanent Go tests in
# internal/packtest (TestNoModelNameAppearsAnywhereInThePack,
# TestNoCredentialShapedStringInThePack) so `go test ./...` alone catches a
# regression -- this target exists because the plan calls for it by name and
# because a one-line shell grep is a useful sanity check independent of the
# Go toolchain.
lint-pack:
	@echo "AD-1: the pack names no model (rungs map to models in the operator's catalog, never here)"
	@if grep -rniE "qwen|llama|gpt-|claude|sonnet|opus|haiku|glm|mistral|gemini|ollama|vllm" pack/; then \
		echo "FAIL: the pack names a model"; exit 1; \
	else \
		echo "ok"; \
	fi
	@echo "the pack holds no credential and no bare secret value"
	@if grep -rniE "sk-|glpat-|password|secret:|token:" pack/ \
		| grep -v "key_secret_name\|key_secret_key\|shared-secret"; then \
		echo "FAIL: credential-shaped string in the pack"; exit 1; \
	else \
		echo "ok"; \
	fi
	@echo "no LLM on the gate path -- every exec order/check script is a thin wrapper over gonk-gate"
	@n=$$(grep -rlE "^exec gonk-gate " pack/scripts/*.sh | wc -l); \
	total=$$(ls pack/scripts/*.sh | wc -l); \
	if [ "$$n" -eq "$$total" ]; then \
		echo "ok ($$n/$$total scripts end in a bare 'exec gonk-gate <subcommand>')"; \
	else \
		echo "FAIL: only $$n/$$total scripts under pack/scripts/ end in a bare exec gonk-gate call"; \
		exit 1; \
	fi

# ---------------------------------------------------------------------------
# The standing gate (Plan 06, Task 2). CI has NEVER RUN on this repo -- every
# runner on the maintainers' GitLab was offline through Plan 01 -- so THIS is the real
# gate, and every target below must be runnable on a laptop, offline, against the
# vendored module cache.
GO ?= go
# Offline by default: the module cache is vendored (go mod vendor + committed).
export GOFLAGS ?= -mod=vendor
export GOPROXY ?= off

.PHONY: gate toolchain fmt vet guard test goldens lint e2e-doctor

# gate is L0 + L1: no containers, race-clean, in under ~90s. It is what runs on
# every commit.
gate: toolchain fmt vet guard test goldens lint

# guard: the tree guard, as its own step, before the long test run.
#
# What is published is this tree minus a list of internal-only paths, and the
# export refuses a tree that holds internal detail: a private address, a
# certificate or key, a credential in a URL, a personal path, an e-mail address
# outside a short reserved list -- and, in the maintainers' tree, their own
# internal names. Commits go straight to main, so a check that ran only at
# export would find the commit weeks after it was made. This step applies the
# same rules to the tree being gated: every tracked file and every untracked
# file git does not ignore. It needs no network, no clone and no scanner
# binary (the secret scanner runs in CI and at export). A refusal prints one
# line per finding: CLASS rule kind path:line:col.
#
# The same test runs again inside `test`; it is a step of its own so that the
# refusal is the first thing a red gate says, not line 4000 of the test output.
# Through require_nonzero_go_tests.sh, so a renamed test cannot turn this step
# into a filter that matches nothing and passes. internal/buildgate
# (TestTheGateRunsTheTreeGuard) holds this recipe and its place in `gate`.
guard:
	@hack/require_nonzero_go_tests.sh ./internal/buildgate/ -run '^TestPublicTreeHasNoInternalDetail$$' -count=1

# toolchain: refuse to run the gate on a Go the CI pin does not match.
#
# WHY THIS IS A GATE STEP AND NOT A NOTE. On 2026-09-15 snap auto-refreshed
# go 1.26.7 -> 1.27.1 on the dev box. golangci-lint $(LINT_VERSION) is built
# with go1.26 and PANICS in go/types on a 1.27 stdlib ("file requires newer Go
# version go1.27"), so `make gate` exited 2 on every branch, for days, for a
# reason that had nothing to do with the code (gonk-e2nt). A red gate is never
# acceptable, and a red gate whose message is a linter stack trace is worse:
# it reads as a code fault. Assert the version instead, and say what to do.
#
# It checks the `go` on PATH, not just $(GO): golangci-lint invokes whatever
# PATH gives it, so pinning $(GO) alone would still panic.
#
# Major.minor only -- CI pins the patch (GO_VERSION in images/versions.env,
# with its digest), dev boxes drift within it, and 1.26.7 vs 1.26.5 is not a
# skew this gate cares about.
GO_VERSION_NUM := $(word 1,$(subst @, ,$(GO_VERSION)))
GO_SERIES := $(shell echo '$(GO_VERSION_NUM)' | cut -d. -f1,2)
toolchain:
	@got=$$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | sed 's/^go//'); \
	series=$$(echo "$$got" | cut -d. -f1,2); \
	if [ -z "$$got" ]; then \
	  echo "no go on PATH; the gate needs go $(GO_SERIES).x (CI pins $(GO_VERSION_NUM))"; \
	  exit 1; \
	fi; \
	if [ "$$series" != "$(GO_SERIES)" ]; then \
	  echo "WRONG GO TOOLCHAIN, NOT A CODE FAULT: go on PATH is $$got, but this repo's gate is go $(GO_SERIES).x (CI pins $(GO_VERSION_NUM), images/versions.env)."; \
	  echo "golangci-lint $(LINT_VERSION) is built with go1.26 and panics in go/types against a newer stdlib, so the failure you would see next is a linter stack trace, not a finding (gonk-e2nt)."; \
	  echo "fix: put a matching toolchain first on PATH, e.g."; \
	  for d in /snap/go/*/bin; do \
	    [ -x "$$d/go" ] || continue; \
	    v=$$("$$d/go" version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | sed 's/^go//'); \
	    [ "$$(echo "$$v" | cut -d. -f1,2)" = "$(GO_SERIES)" ] && echo "         PATH=$$d:\$$PATH make gate   (go $$v, already installed)"; \
	  done; \
	  echo "         or hold the snap so it cannot drift again: snap refresh --hold go"; \
	  exit 1; \
	fi

# goldens RENDERS THE CHART (gonk-xsk3 review, D3). Without this, `make gate`
# only type-checks the chart tests (`go vet -tags chart`) and never runs one, so
# a template edit that skips `make chart-goldens` is green in the pre-push gate
# CLAUDE.md mandates and is caught only by the GitHub chart job -- after it is on
# main. It also feeds the image-contract check, which reads the goldens: stale
# goldens would make that check assert against yesterday's chart.
#
# It needs real helm, and fails loudly without it rather than skipping.
#
# HELM IS PINNED TO A MAJOR, NOT JUST "on PATH" (gonk-3zcs). The goldens are a
# function of the renderer: helm 4.3.0 emits 27 extra blank lines per profile
# against the helm 3.19.0 they were made with, so on a box with both (this one
# has /usr/bin/helm 3.19.0 and /snap/bin/helm 4.3.0) PATH order alone decided
# whether this gate was red on a clean tree -- and the golden failure told the
# reader to regenerate, which would have committed helm 4's shape. charttest
# asserts the version before it renders anything; set HELM to choose the binary:
#
#	make gate HELM=/usr/bin/helm
#
# THE IMAGE FLOOR TESTS RUN HERE TOO (gonk-jth4). A floor change is a chart
# change like any other: before this, moving a floor passed `make gate` and was
# caught only by the GitHub chart job, after the commit was on main -- the
# "post-hoc detection, not prevention" case CLAUDE.md names. They cost 2.0s of
# the gate's ~40s. They read the floors out of Chart.yaml, so moving a floor
# EXERCISES them instead of breaking them.
#
# THE IN-CLUSTER REACHABILITY TESTS RUN HERE TOO (gonk-zjk7). They are the only
# thing that catches a pod configured to call a Service its own NetworkPolicy
# does not permit -- on a cluster whose CNI enforces no NetworkPolicy there is
# no runtime signal at all, and the defect is invisible until the day one does.
# Leaving them in the GitHub chart job (which is gated on a $GONK_CHART_TOOLS_IMAGE
# that does not exist, so it never runs) would have been writing the test and
# never running it. They need only helm, like the goldens, and cost ~2s.
#
# The REST of the chart suite deliberately stays in the GitHub chart job: it
# needs kubeconform, a second unpinned external binary in the mandatory gate --
# the fault this target just fixed for helm -- and it costs 45s rather than 2s.
#
# THE POD REAPER'S CHART TESTS RUN HERE TOO (gonk-md3v). podReap.enabled is the
# switch on the one feature in gonk that DELETES a Kubernetes object, and its
# chart assertions -- the env that arms it, the namespaced Role, the binding
# subject, the verb set -- were reachable only from the GitHub chart job, i.e.
# after the commit was already on main. That is the same post-hoc-detection
# fault the image floors were moved here for, on a feature where the cost of
# being wrong is a deleted pod rather than a stale tag.
#
# internal/buildgate/podreap_gate_test.go asserts that the -run pattern below
# covers EVERY test in internal/charttest/podreap_test.go, so a new one cannot
# be added outside the gate by accident.
#
# THE JOBS SESSION RUNTIME'S CHART TESTS RUN HERE TOO (gonk-t55 P5). Role
# gonk-gate-jobs is the grant that lets gonk-gate create Jobs and create, read
# and DELETE Secrets, and its env is what tells the runtime which image and
# namespace to use. The GitHub chart job never runs (see above), so a chart test
# outside this pattern runs nowhere. internal/buildgate/jobs_gate_test.go holds
# ^TestJobsRuntime against every test in internal/charttest/jobs_rbac_test.go.
#
# THE RELEASE-TAG CHART TESTS RUN HERE TOO (release-unit, 2026-09-26). They are
# what holds every gonk image -- including the agent image the controller hands
# to session pods -- to ONE tag, and what refuses an empty agent tag. A skew
# there is a controller<->agent contract violation, so like the pod reaper it is
# checked before push, not after. internal/buildgate/release_tag_gate_test.go
# pins ^TestReleaseTag in the pattern against internal/charttest/release_tag_test.go.
#
# THE NOTES TESTS RUN HERE TOO (gonk-tomt). NOTES.txt is what a first-time
# installer reads to learn which Secrets to create; ^TestNotes compares that
# list with the Secrets the render mounts. It needs only helm.
#
# THE POLICY-PAIRING, API-SERVER AND RBAC-DOCUMENT TESTS RUN HERE TOO
# (gonk-nax7, gonk-vxel, gonk-6xzj, gonk-6by9; netpol_pairs_test.go). They hold
# that every same-namespace NetworkPolicy rule has its other half, that the
# API-server allowance reaches the pods that call the API and no others, and
# that values.yaml and the chart README describe the Roles that render. Dev
# does not enforce NetworkPolicy, so nothing at run time says when one of these
# breaks. They need only helm.
#
# The REST of the chart suite deliberately stays in the GitHub chart job, and
# the numbers are why (measured 2026-09-23, one development machine, helm 3.19.0, warm build
# cache; two sets, the second with a parallel gate loading the box, which is
# why the range and not a single figure):
#
#	goldens + floors only         2.3s   3.3s
#	the same plus ^TestPodReap    2.9s   3.7s   <- this target
#	the whole charttest package  46.0s  58.4s
#
# 46s on a gate whose whole budget is ~90s, to run a suite that also hard-FATALs
# without `kubeconform` on PATH (internal/charttest/charttest.go: it refuses to
# skip) -- that would put a second unpinned external binary in the mandatory
# pre-push gate, which is the exact fault gonk-3zcs fixed for helm. So: widen
# the pattern, do not widen the package.
HELM ?= helm
export HELM
goldens:
	$(GO) test -tags chart ./internal/charttest/ -run '^TestGoldenManifests$$|^TestTheHelm|^TestImageFloor|^TestInClusterDials|^TestEveryDialExemption|^TestPodReap|^TestJobsRuntime|^TestReleaseTag|^TestNotes|^TestEveryInNamespaceRule|^TestEveryPairExemption|^TestIntakePrivateListener|^TestIntakeIngressFollows|^TestEveryDNSEgress|^TestKubeAPI|^TestGuardKubeAPI|^TestValuesDoNotClaim|^TestREADMEDescribesEveryRole' -count=1

# Seal the chart: append `<version> <sha256 of chart/gonk>` to chart/CHART-SEAL.
#
# Run this AFTER bumping `version:` in chart/gonk/Chart.yaml. Under the
# HelmRelease's reconcileStrategy: ChartVersion, Flux only deploys when that
# version changes -- a chart edit that keeps the old version sits in git and
# never reaches the cluster, with CI green (gonk-sjb). The ledger is append-only
# and this target REFUSES to rewrite an existing entry, because rewriting is the
# bug: Flux will not repackage a name+version it has already built.
#
# Pure Go on purpose -- no helm, no kubeconform. It must run in CI's golang
# image and on a dev box that has neither.
.PHONY: chart-seal chart-goldens
chart-seal:
	$(GO) test ./internal/buildgate/ -run TestChartSealMatchesTheChart -reseal -count=1

# Regenerate the golden manifests. Separate from chart-seal because it needs
# helm, and because a combined target that failed here would leave CHART-SEAL
# already written -- a tree that PASSES the seal gate with stale goldens.
#
# A version bump alone does NOT need this: renders are normalized to
# charttest.GoldenChartVersion, so bumping changes zero golden lines.
chart-goldens:
	$(GO) test -tags chart ./internal/charttest/ -run Golden -update -count=1

# Scan the REPO's own Go files, not everything under the tree. `gofmt -l .`
# walks into git worktrees under .worktrees/ and .claude/worktrees/, each of
# which carries its own vendor/ -- so a single leftover worktree made `make
# gate` fail on thousands of vendored files it does not own, and a gate that
# cannot run is the same as no gate (gonk-n50's lesson, and the --network=host
# lint break on 2026-09-07). Excluding by path prefix keeps working when a
# worktree is nested deeper than the old '^vendor/' filter could see.
fmt:
	@out="$$(gofmt -l . | grep -v -e '^vendor/' -e '/vendor/' \
	    -e '^\.worktrees/' -e '^\.claude/worktrees/' || true)"; \
	  if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# VET_BUILD_TAGS: every custom //go:build tag under cmd/ pkg/ internal/ test/
# that the default (untagged) `go vet ./...` never compiles, so it never
# type-checks the component/integration/images/live/chart/testclock suites
# either (T-14). Keep in sync with .golangci.yml's run.build-tags, with the
# `go vet (every custom build tag)` step in .github/workflows/ci.yml and
# .gitlab-ci.yml, and with internal/buildgate's
# TestRepoBuildTagsAndKindsMatchTheKnownSix, which pins this same set -- and
# each tag's KIND -- against a real scan of the tree. For `testclock` the
# tagged vet is not just a nicety: it guards no _test.go file, so a vet is
# the only coverage it can honestly have (gonk-0bvc).
VET_BUILD_TAGS := component integration images live chart testclock

vet:
	$(GO) vet ./...
	@for t in $(VET_BUILD_TAGS); do \
	  echo "go vet -tags $$t ./..."; \
	  $(GO) vet -tags $$t ./... || exit 1; \
	done

# NO -skip. This target used to exclude TestEveryBuildTagRunsInCI
# (internal/buildgate), which T-14 added deliberately red while the `live`
# and `testclock` tags had no CI coverage -- and .gitlab-ci.yml, which runs a
# bare `go test ./...`, was red for hours because nobody added the same
# exclusion there. That is what a tolerated red gate costs. Both tags are
# covered now (`live` by .github/workflows/live.yml, `testclock` by the
# tagged `go vet` this Makefile and ci.yml both run), so the test passes for
# real and every runner -- local, Hub and Lab -- runs the identical command.
#
# -shuffle=on (gonk-5pyx): the test order within each package is randomized
# every run, so a test that only passes because an earlier one left state
# behind goes red instead of hiding. Turned on only after a 15-round shuffled
# hunt over ./... (540 distinct package seeds, 24,855 test runs) came back 0
# failures -- shuffling on top of known red would only add red. A shuffled
# failure prints `-test.shuffle <seed>` at the top of its package's output;
# rerun that package with -shuffle=<seed> to reproduce the order exactly.
# internal/buildgate/flake_tooling_test.go keeps this line and CI's identical.
test:
	$(GO) test ./... -race -count=1 -shuffle=on

# lint runs the SAME golangci-lint the CI job runs, pinned to the same tag.
#
# It used to hard-code one path to golangci-lint, which is not installed on
# every machine -- so `make gate` was not actually runnable everywhere, and lint
# findings were discovered in CI instead. That happened (gonk-mzd: nine findings,
# one pipeline). The container fallback means there is no excuse and no version
# skew: if the binary is present it is used, otherwise the pinned image is.
#
# LINT_VERSION_NUM has no leading "v" because `golangci-lint version` prints
# the bare number ("golangci-lint has version 2.12.2 built with go1.26.2
# from ..."); LINT_VERSION adds it back for the image tag, matching the SAME
# release .gitlab-ci.yml and .github/workflows/ci.yml pin.
LINT_VERSION_NUM := 2.12.2
LINT_VERSION := v$(LINT_VERSION_NUM)
LINT_IMAGE ?= golangci/golangci-lint:$(LINT_VERSION)
lint:
	@bin=""; \
	if command -v golangci-lint >/dev/null 2>&1; then \
	  bin=golangci-lint; \
	elif [ -x "$$($(GO) env GOPATH)/bin/golangci-lint" ]; then \
	  bin="$$($(GO) env GOPATH)/bin/golangci-lint"; \
	fi; \
	if [ -n "$$bin" ]; then \
	  got=$$($$bin version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1); \
	  if [ "$$got" != "$(LINT_VERSION_NUM)" ]; then \
	    echo "host golangci-lint ($$bin) reports version $${got:-<unknown>}, but CI is pinned to $(LINT_VERSION) -- refusing to run a mismatched linter, which can report different findings than the pin (new/removed checks, changed defaults) and pass locally while CI still fails it, or the reverse"; \
	    echo "fix: install $(LINT_VERSION), or take $$bin off PATH (and out of GOPATH/bin) to fall back to the pinned $(LINT_IMAGE) container below"; \
	    exit 1; \
	  fi; \
	  : "--allow-serial-runners: WAIT for another run's lock, do not fail." ; \
	  : " golangci-lint takes a per-user lock, so two concurrent gates on one" ; \
	  : " box -- routine here, with several agents in worktrees -- made the" ; \
	  : " second exit 2 with 'parallel golangci-lint is running'. That turned" ; \
	  : " a clean gate red for a reason unrelated to the code (2026-09-21)." ; \
	  : " Serial, not --allow-parallel-runners: parallel runs share a cache" ; \
	  : " and can race on it; serialising only costs wall time." ; \
	  : "A CACHE PER CHECKOUT (gonk-nn71; hack/lint_cache_dir.sh says why)." ; \
	  : " With the per-user default, a second checkout of the same commit" ; \
	  : " replays the first one's cached issues at the FIRST one's paths;" ; \
	  : " once that checkout is deleted the nolint lines cannot be read and" ; \
	  : " lint exits 1 naming files in a directory that is gone." ; \
	  cache="$$(hack/lint_cache_dir.sh)" || exit 1; \
	  GOLANGCI_LINT_CACHE="$$cache" $$bin run --allow-serial-runners ./... || { \
	    rc=$$?; \
	    echo "lint: exit $$rc with GOLANGCI_LINT_CACHE=$$cache"; \
	    echo "lint: every finding should be at a path inside this checkout ($$PWD). One at a path OUTSIDE it is a cache entry replayed from another checkout (gonk-nn71), which a per-checkout cache is meant to make impossible: it is not a fault in this tree. Delete the cache directory above, rerun, and report it."; \
	    exit $$rc; \
	  }; \
	else \
	  echo "golangci-lint not installed; running $(LINT_IMAGE) in a container"; \
	  : "--network=none, NOT host: the lint is fully offline (vendored deps," ; \
	  : " GOPROXY=off), and on a WSL box podman's CNI bridge fails with" ; \
	  : " 'table nat is incompatible, use nft' -- so --network=host made this" ; \
	  : " fallback ERROR OUT rather than lint. That is how three errcheck" ; \
	  : " violations reached main and left CI red for eight commits" ; \
	  : " (gonk-vrf review, 2026-09-07): go vet and go test were clean, and" ; \
	  : " the one gate that would have caught it could not run here." ; \
	  $(PODMAN) run --rm --network=none -v "$$PWD":/w -w /w \
	    -e GOFLAGS=-mod=vendor -e GOPROXY=off $(LINT_IMAGE) golangci-lint run ./...; \
	fi

# hunt is the flake INSTRUMENT (gonk-5pyx): it runs tests many times under the
# load that makes wall-clock tests fail -- CPU burner PROCESSES (not
# goroutines: 6/100 vs 0/100 on gonk-oxb8's test), GOMAXPROCS=2, -race,
# -test.count=N -- and prints each test's failure rate and p50/p99/max
# duration. A person runs it; it is NOT part of `gate` and must never become
# one (internal/buildgate/flake_tooling_test.go fails if it does), and it is
# not scheduled. Every knob is a make variable passed straight through; see
# hack/flake_hunt.sh for the full list. docs/testing-time.md says when to run
# it: any test fix for a flake is shown red on demand with this BEFORE the fix
# and 0 after.
#
#   make hunt PKG=./pkg/intake RUN='^TestLoopSpeedsUp' COUNT=100
#   make hunt PKG=./... ROUNDS=20 COUNT=1 SHUFFLE=on
#   make hunt PKG=./internal/packtest RUN=Unreachable COUNT=200 CHURN=32   # port flakes
.PHONY: hunt
hunt:
	GO=$(GO) hack/flake_hunt.sh

# e2e-doctor is the preflight that refuses to waste your 25 minutes: it fails
# fast, with a remedy, when a layer's prereqs are absent. Expected to FAIL on the
# dev box (broken CNI bridge) -- that IS the OD-3 finding, not a bug in this task.
e2e-doctor:
	$(GO) run ./test/harness/cmd/doctor

# refresh-cassette runs test/stubmodel's -record mode (AD-2, cmd/gonk-stubmodel):
# it starts gonk-stubmodel as a PROXY in front of a real upstream model API,
# capturing every {request, response} pair it forwards into a scrubbed cassette
# under test/stubmodel/cassettes/. It is run by a HUMAN, OUT OF BAND -- never by
# a test or by CI, and never with a shared/production credential (never commit
# an unencrypted secret; the credential lives only in the requests
# YOU send it, and is redacted before the cassette is written).
#
# Required: GONK_STUBMODEL_UPSTREAM, the base URL of a real OpenAI-compatible
# endpoint (e.g. https://model.example.internal/v1). This target refuses to guess one --
# there is no default and no `latest` (repo rule) -- and fails with this message
# instead of silently recording against nothing.
#
# Optional: GONK_STUBMODEL_CASSETTE (default: recorded) names the cassette file
# written to test/stubmodel/cassettes/<name>.json. Pass triage to refresh the
# committed fixture (but see the note in test/stubmodel/record.go about what
# request must be sent for that recording to be worth committing).
#
# Usage: in one terminal, `make refresh-cassette GONK_STUBMODEL_UPSTREAM=...`;
# in another, POST the exact request you want captured to
# http://127.0.0.1:8081/v1/chat/completions, carrying ITS OWN real credential
# (Authorization header or api_key field -- the proxy forwards whatever you
# send, and scrubs it back out before saving). Then Ctrl-C the first terminal:
# SIGINT now triggers a clean shutdown-then-save (fixed here -- the previous
# `defer rec.Save()` never ran under a human's Ctrl-C, so this path had never
# actually produced a cassette).
GONK_STUBMODEL_CASSETTE ?= recorded
.PHONY: refresh-cassette
refresh-cassette:
	@if [ -z "$(GONK_STUBMODEL_UPSTREAM)" ]; then \
	  echo "refresh-cassette: GONK_STUBMODEL_UPSTREAM is not set." >&2; \
	  echo "  Set it to a real OpenAI-compatible base URL, e.g.:" >&2; \
	  echo "    make refresh-cassette GONK_STUBMODEL_UPSTREAM=https://model.example.internal/v1" >&2; \
	  echo "  The credential itself is NEVER passed to make -- send it in the" >&2; \
	  echo "  request you POST to the running proxy; it is scrubbed before the" >&2; \
	  echo "  cassette is written." >&2; \
	  exit 1; \
	fi
	$(GO) run ./cmd/gonk-stubmodel \
	  -record "$(GONK_STUBMODEL_UPSTREAM)" \
	  -cassette "$(GONK_STUBMODEL_CASSETTE)" \
	  -cassette-dir test/stubmodel/cassettes \
	  -addr :8081

# component-jobs: the Jobs session runtime on a REAL cluster with the REAL
# agent image (gonk-t55 P6; cmd/gonk-gate/jobs_component_test.go, README in
# cmd/gonk-gate/README-component-jobs.md). NOT part of `make gate`: it needs a
# disposable k3d cluster, which CI does not have.
#
#   1. builds images/Dockerfile.agent with EXACTLY agent-image's build args,
#      tagged localhost/gonk-agent:$(GONK_TAG) (GONK_TAG carries the commit);
#   2. imports it into the k3d node the T-45 way (`k3d image import` wants a
#      docker socket): podman save -> podman cp -> ctr -n k8s.io images import;
#   3. runs the component tests against COMPONENT_KUBE_CONTEXT, with the stub
#      model listening on the k3d network's gateway, which pods can reach.
#
# The context is always passed explicitly (never the kubectl default), and the
# test refuses any context that is not k3d-* or kind-*.
#
#   make component-jobs
#   make component-jobs-test COMPONENT_AGENT_IMAGE=localhost/gonk-agent:t55-gate2   # a different, already-imported image
COMPONENT_KUBE_CONTEXT ?= k3d-gonk-t45
COMPONENT_K3D_NODE     ?= k3d-gonk-t45-server-0
COMPONENT_AGENT_IMAGE  ?= localhost/gonk-agent:$(GONK_TAG)
# The image-skew variant (plan 3.3, handshake item 5): the SAME agent image
# with its one GONK_SUPPORTED_SESSION_CONTRACTS line rewritten to "999", i.e.
# an agent that speaks no contract this controller sends. Built FROM the
# image above, so the entrypoint's contract line is the only difference.
COMPONENT_SKEW_IMAGE   ?= localhost/gonk-agent:$(GONK_TAG)-skew999
COMPONENT_STUB_LISTEN  ?= $(shell $(PODMAN) inspect $(COMPONENT_K3D_NODE) --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' 2>/dev/null):0
COMPONENT_RUN          ?= ^TestComponentJobs
.PHONY: component-jobs component-jobs-image component-jobs-test
component-jobs: component-jobs-image component-jobs-test

component-jobs-image:
	$(MAKE) agent-image REGISTRY=localhost GONK_TAG=$(GONK_TAG)
	@ctx=$$(mktemp -d) && \
	printf '%s\n' 'ARG BASE' 'FROM $${BASE}' 'USER 0' \
	  "RUN grep -qx 'GONK_SUPPORTED_SESSION_CONTRACTS=\"1\"' /usr/local/bin/gonk-agent-entrypoint && sed -i 's/^GONK_SUPPORTED_SESSION_CONTRACTS=\"1\"\$$/GONK_SUPPORTED_SESSION_CONTRACTS=\"999\"/' /usr/local/bin/gonk-agent-entrypoint && grep -qx 'GONK_SUPPORTED_SESSION_CONTRACTS=\"999\"' /usr/local/bin/gonk-agent-entrypoint" \
	  'USER 65532:65532' \
	  | $(PODMAN) build --network=host --build-arg BASE=$(COMPONENT_AGENT_IMAGE) -f - -t $(COMPONENT_SKEW_IMAGE) $$ctx; \
	  rc=$$?; rmdir $$ctx; exit $$rc
	@for img in $(COMPONENT_AGENT_IMAGE) $(COMPONENT_SKEW_IMAGE); do \
	  tmp=$$(mktemp /tmp/gonk-agent-component.XXXXXX.tar) && \
	  $(PODMAN) save --format docker-archive -o $$tmp $$img && \
	  $(PODMAN) cp $$tmp $(COMPONENT_K3D_NODE):/tmp/gonk-agent-component.tar && \
	  $(PODMAN) exec $(COMPONENT_K3D_NODE) ctr -n k8s.io images import /tmp/gonk-agent-component.tar; \
	  rc=$$?; rm -f $$tmp; $(PODMAN) exec $(COMPONENT_K3D_NODE) rm -f /tmp/gonk-agent-component.tar; \
	  [ $$rc = 0 ] || exit $$rc; \
	  $(PODMAN) exec $(COMPONENT_K3D_NODE) crictl inspecti $$img >/dev/null || exit 1; \
	done

component-jobs-test:
	GONK_COMPONENT_KUBE_CONTEXT=$(COMPONENT_KUBE_CONTEXT) \
	GONK_COMPONENT_AGENT_IMAGE=$(COMPONENT_AGENT_IMAGE) \
	GONK_COMPONENT_SKEW_IMAGE=$(COMPONENT_SKEW_IMAGE) \
	GONK_COMPONENT_STUB_LISTEN=$(COMPONENT_STUB_LISTEN) \
	  $(GO) test -tags component ./cmd/gonk-gate/ -run '$(COMPONENT_RUN)' -race -count=1 -v -timeout 30m

# --- release export (gonk-cuwn) ---------------------------------------------
# What the public mirror may receive is decided by hack/relexport: the tree of
# EXPORT_SRC minus release/internal-only.txt (as committed THERE), plus
# EXPORT-SOURCE, refused unless it passes the deny-pattern guard and the
# pinned secret scanner. Neither target publishes anything, and neither can.
#
# `make gate` runs the deny-pattern guard on the WORKING TREE (the `guard`
# target above); these two run the export's own check on a COMMIT, from a fresh
# clone, and are what CI's export-check job runs on every pipeline.
#
#   export-guard  the deny-pattern guard alone (no scanner binary needed);
#                 it says so, and a pass is NOT clearance to export.
#   export-check  guard + gitleaks 8.30.1 (EXPORT_GITLEAKS=/path/to/gitleaks,
#                 or RELEXPORT_GITLEAKS, or on PATH; pinned by sha256).
#
# EXPORT_SRC must be a commit on EXPORT_ORIGIN's main; by default that is
# origin's main, read from a fresh clone, never from this working tree.
.PHONY: export-guard export-check
EXPORT_ORIGIN ?= $(shell git config --get remote.origin.url)
EXPORT_SRC ?= $(shell git ls-remote $(EXPORT_ORIGIN) refs/heads/main | cut -f1)
EXPORT_GITLEAKS ?=
EXPORT_ARGS ?=

export-guard:
	$(GO) run ./hack/relexport check --guard-only --origin $(EXPORT_ORIGIN) --src $(EXPORT_SRC) $(EXPORT_ARGS)

export-check:
	$(GO) run ./hack/relexport check --origin $(EXPORT_ORIGIN) --src $(EXPORT_SRC) \
	  $(if $(EXPORT_GITLEAKS),--gitleaks $(EXPORT_GITLEAKS)) $(EXPORT_ARGS)
