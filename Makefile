include images/versions.env

# GONK_TAG: NEVER `latest`. The image tag is the pinned GONK_VERSION plus the
# git SHA that built it -- reproducible, and traceable back to a commit
# (Task 5 Step 5 / Task 7 Step 4).
GONK_TAG ?= $(GONK_VERSION)-$(shell git rev-parse --short=12 HEAD)

# PODMAN, and --network=host: the CNI bridge is broken on this box
# (docs/environment.md). Do not "fix" it by removing the flag.
PODMAN := podman
BUILD  := $(PODMAN) build --network=host

.PHONY: images push pack-validate no-latest lint-pack

# images: only gonk-agent exists as of Plan 04 Task 5. Task 6 (controller) and
# Task 7 (intake, meter) each add their own `$(BUILD) -f images/Dockerfile.X
# ...` line here when their Dockerfiles land -- this target is NOT the final
# four-image list Task 7 formalizes, it is what Task 5 can honestly build
# today.
images: no-latest
	$(BUILD) \
	  --build-arg GONK_TAG=$(GONK_TAG) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg DEBIAN_BASE=$(DEBIAN_BASE) \
	  --build-arg OPENCODE_VERSION=$(OPENCODE_VERSION) \
	  --build-arg GLAB_VERSION=$(GLAB_VERSION) \
	  --build-arg BD_VERSION=$(BD_VERSION) \
	  -f images/Dockerfile.agent -t $(REGISTRY)/gonk-agent:$(GONK_TAG) .

# push: EVERY GITLAB RUNNER IS OFFLINE (docs/environment.md). CI has never
# executed for this repo, so the first images are pushed BY HAND from a box
# with LAN reach to registry.orac.local -- expected, not a workaround to be
# embarrassed about. Not run as part of Task 5 (no reach to the cluster LAN
# from this sandbox); Plan 05/CI formalizes it.
push: images
	@echo "pushing $(GONK_TAG) to $(REGISTRY)"
	$(PODMAN) push $(REGISTRY)/gonk-agent:$(GONK_TAG)

# pack-validate: Gas City's REAL loader, offline (Task 6, Plan 04). It needs
# images/Dockerfile.controller and the `gc` binary, neither of which exists in
# this worktree yet -- Task 4's internal/packtest is the interim, OFFLINE
# structural gate (an allow-list, not the loader) and is not a substitute.
# This target fails loud rather than silently claim a validation that has not
# happened.
pack-validate:
	@echo "pack-validate: NOT YET IMPLEMENTED -- Gas City's real loader is Task 6" >&2
	@echo "(needs images/Dockerfile.controller + the gc binary). Interim gate:" >&2
	@echo "  go test ./internal/packtest/ -race -count=1" >&2
	@exit 1

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
