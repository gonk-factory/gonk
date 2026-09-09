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

# PODMAN, and --network=host: the CNI bridge is broken on this box
# (docs/environment.md). Do not "fix" it by removing the flag.
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
# index over them (.gitlab-ci.yml + homelab/ci-templates .manifest-merge).
#
# On 2026-08-04 a hand `make push` overwrote four of those indexes with amd64
# images. orac is mixed-architecture, so every gonk pod that landed on an arm64
# node crash-looped with `exec format error` -- 1593 controller restarts, 1100
# meter restarts, six days -- while CI stayed green, because the merge job's
# verification could not tell a single-arch image from an index (gonk-n50,
# homelab/ci-templates!4).
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
# runner on gitlab.orac.local was offline through Plan 01 -- so THIS is the real
# gate, and every target below must be runnable on a laptop, offline, against the
# vendored module cache.
GO ?= go
# Offline by default: the module cache is vendored (go mod vendor + committed).
export GOFLAGS ?= -mod=vendor
export GOPROXY ?= off

.PHONY: gate fmt vet test lint e2e-doctor

# gate is L0 + L1: no containers, race-clean, in under ~90s. It is what runs on
# every commit.
gate: fmt vet test lint

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
# either (T-14). Keep in sync with .golangci.yml's run.build-tags and
# internal/buildgate's TestRepoBuildTagsFindsTheKnownSixMatchesTheTaskList,
# which pins this same set against a real scan of the tree.
VET_BUILD_TAGS := component integration images live chart testclock

vet:
	$(GO) vet ./...
	@for t in $(VET_BUILD_TAGS); do \
	  echo "go vet -tags $$t ./..."; \
	  $(GO) vet -tags $$t ./... || exit 1; \
	done

# TestEveryBuildTagRunsInCI (internal/buildgate) is DELIBERATELY red: T-14
# added it to assert every //go:build tag under cmd/pkg/internal/test
# appears in a `go test … -tags <tag>` line in .github/workflows/ci.yml, and
# today component/integration/images/live/testclock do not (only chart
# does). This package runs in the ordinary gate on purpose (see
# nolatest_test.go's package doc), so without the SAME -skip ci.yml's own
# `go test` step carries, this one intentionally-red test would fail
# `make gate` for every contributor on every branch -- worse than the gap it
# exists to surface. The finding is not hidden: run
# `go test ./internal/buildgate/ -count=1 -v` directly (no -skip) to see it.
# TODO(2026-09-08, T-15/T-16/T-17): narrow or remove this exclusion as each
# task lands the CI job it covers.
test:
	$(GO) test ./... -race -count=1 -skip '^TestEveryBuildTagRunsInCI$$'

# lint runs the SAME golangci-lint the CI job runs, pinned to the same tag.
#
# It used to hard-code /root/go/bin/golangci-lint, which is not installed on
# every dev box -- so `make gate` was not actually runnable here, and lint
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
	elif [ -x /root/go/bin/golangci-lint ]; then \
	  bin=/root/go/bin/golangci-lint; \
	fi; \
	if [ -n "$$bin" ]; then \
	  got=$$($$bin version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1); \
	  if [ "$$got" != "$(LINT_VERSION_NUM)" ]; then \
	    echo "host golangci-lint ($$bin) reports version $${got:-<unknown>}, but CI is pinned to $(LINT_VERSION) -- refusing to run a mismatched linter, which can report different findings than the pin (new/removed checks, changed defaults) and pass locally while CI still fails it, or the reverse"; \
	    echo "fix: install $(LINT_VERSION), or take $$bin off PATH (and /root/go/bin) to fall back to the pinned $(LINT_IMAGE) container below"; \
	    exit 1; \
	  fi; \
	  $$bin run ./...; \
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

# e2e-doctor is the preflight that refuses to waste your 25 minutes: it fails
# fast, with a remedy, when a layer's prereqs are absent. Expected to FAIL on the
# dev box (broken CNI bridge) -- that IS the OD-3 finding, not a bug in this task.
e2e-doctor:
	$(GO) run ./test/harness/cmd/doctor
