include images/versions.env

# GONK_TAG: NEVER `latest`. The image tag is the pinned GONK_VERSION plus the
# git SHA that built it -- reproducible, and traceable back to a commit
# (Task 5 Step 5 / Task 7 Step 4).
GONK_TAG ?= $(GONK_VERSION)-$(shell git rev-parse --short=12 HEAD)

# PODMAN, and --network=host: the CNI bridge is broken on this box
# (docs/environment.md). Do not "fix" it by removing the flag.
PODMAN := podman
BUILD  := $(PODMAN) build --network=host

.PHONY: images agent-image controller-image push pack-validate no-latest lint-pack

# images: gonk-agent (Task 5) and gonk-controller (Task 6). Task 7 (intake,
# meter) adds its own two lines here when their Dockerfiles land -- this
# target is NOT the final four-image list Task 7 formalizes, it is what
# Plan 04 can honestly build today.
images: no-latest agent-image controller-image

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
	  --build-arg GASCITY_REF=$(GASCITY_REF) \
	  -f images/Dockerfile.controller -t $(REGISTRY)/gonk-controller:$(GONK_TAG) .

# push: EVERY GITLAB RUNNER IS OFFLINE (docs/environment.md). CI has never
# executed for this repo, so the first images are pushed BY HAND from a box
# with LAN reach to registry.orac.local -- expected, not a workaround to be
# embarrassed about. Not run as part of Task 5 (no reach to the cluster LAN
# from this sandbox); Plan 05/CI formalizes it.
push: images
	@echo "pushing $(GONK_TAG) to $(REGISTRY)"
	$(PODMAN) push $(REGISTRY)/gonk-agent:$(GONK_TAG)
	$(PODMAN) push $(REGISTRY)/gonk-controller:$(GONK_TAG)

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

no-latest:
	@! grep -rn '$(BANNED_TAG)' images/ Makefile chart/ 2>/dev/null || \
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
