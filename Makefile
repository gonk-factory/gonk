include images/versions.env

# GONK_TAG: NEVER `latest`. The image tag is the pinned GONK_VERSION plus the
# git SHA that built it -- reproducible, and traceable back to a commit
# (Task 5 Step 5 / Task 7 Step 4).
GONK_TAG ?= $(GONK_VERSION)-$(shell git rev-parse --short=12 HEAD)

# PODMAN, and --network=host: the CNI bridge is broken on this box
# (docs/environment.md). Do not "fix" it by removing the flag.
PODMAN := podman
BUILD  := $(PODMAN) build --network=host

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
	  --build-arg GLAB_VERSION=$(GLAB_VERSION) \
	  --build-arg BD_VERSION=$(BD_VERSION) \
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

# push: EVERY GITLAB RUNNER IS OFFLINE (docs/environment.md). CI has never
# executed for this repo, so the first images are pushed BY HAND from a box
# with LAN reach to registry.orac.local -- expected, not a workaround to be
# embarrassed about. Not run as part of Task 5 (no reach to the cluster LAN
# from this sandbox); Plan 05/CI formalizes it.
push: images
	@echo "pushing $(GONK_TAG) to $(REGISTRY)"
	for i in gonk-agent gonk-controller gonk-intake gonk-meter; do \
	  $(PODMAN) push $(REGISTRY)/$$i:$(GONK_TAG); done
	$(PODMAN) push $(REGISTRY)/gonk-meter:$(GONK_TAG)-testclock

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
no-latest:
	@paths="images Makefile test"; \
	  [ -d chart ] && paths="$$paths chart"; \
	  ! grep -rn --exclude-dir=crd-schemas '$(BANNED_TAG)' $$paths 2>/dev/null || \
	  (echo "FAIL: a floating image tag was found. Pin an exact tag (docs/environment.md)." && exit 1)

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
