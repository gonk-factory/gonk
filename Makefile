.PHONY: lint-pack

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
