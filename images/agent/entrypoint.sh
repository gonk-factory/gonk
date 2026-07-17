#!/bin/sh
# gonk-agent-entrypoint: the agent pod's ENTRYPOINT (images/Dockerfile.agent).
#
# Does exactly three things, in order (Plan 04, Task 5, Step 3):
#
#   1. Installs the prepare-commit-msg hook into the rig clone's .git/hooks/
#      (Task 8's hook is not cloned with the repo, so the session installs it
#      itself, every time -- if Gas City's session provider ever wipes
#      .git/hooks on pod recreation, this line is what re-installs it).
#   2. Renders overlay/opencode.json from THIS session's environment: the
#      LiteLLM base URL, the model gonk-meter chose (never a literal, never
#      chosen here), the virtual key (a FILE MOUNT, never an env value -- env
#      leaks into `ps`, /proc/<pid>/environ, crash dumps, and every child
#      process), and the attribution metadata as the static provider header
#      `provider.gonk.options.headers["x-litellm-spend-logs-metadata"]`,
#      stamped VERBATIM from gonk-meter's own DecideResponse.Metadata
#      (docs/environment.md, "VERIFIED: the attribution chain works"; Plan 04
#      OD-7).
#   3. execs opencode.
#
# *** OPENCODE-VERSION-SENSITIVE, FLAGGED FOR PLAN 06 ***
# The mechanism (provider.<id>.options.headers, the @ai-sdk/openai-compatible
# npm shape, {file:...} substitution) is confirmed against opencode's own
# source at the EXACT pinned tag (images/versions.env's OPENCODE_VERSION) --
# see packages/web/src/content/docs/providers.mdx and
# packages/opencode/src/config/variable.ts in github.com/anomalyco/opencode.
# It is NOT confirmed against a LIVE LiteLLM with THIS opencode binary in a
# real pod: docs/environment.md's smoke test used a hand-built HTTP request,
# not opencode itself. Plan 06 must do that live verification before this
# seam is trusted in production; a future opencode bump could rename or move
# the config key, and Task 5 Step 4 says to re-confirm at every bump.

set -eu

log() { printf 'gonk-agent-entrypoint: %s\n' "$1" >&2; }

# ---- Step 1: install the commit-provenance hook -----------------------------
# GONK_RIG_DIR defaults to the WORKDIR opencode is launched in (the rig
# clone). A missing .git (no clone yet, or a non-git rig) is not fatal here --
# scaffold-only sessions and dry runs still need to start.
RIG_DIR="${GONK_RIG_DIR:-$PWD}"
if [ -d "${RIG_DIR}/.git" ]; then
	mkdir -p "${RIG_DIR}/.git/hooks"
	cp /usr/local/share/gonk/prepare-commit-msg "${RIG_DIR}/.git/hooks/prepare-commit-msg"
	chmod +x "${RIG_DIR}/.git/hooks/prepare-commit-msg"
	log "installed prepare-commit-msg into ${RIG_DIR}/.git/hooks"
else
	log "no .git at ${RIG_DIR}; skipping prepare-commit-msg install"
fi

# ---- Step 2: render overlay/opencode.json -----------------------------------
# Every one of these is REQUIRED. There is no default rung, no default model,
# and no ambient virtual key -- an agent pod with any of these unset is a pod
# that would either talk to nothing or (worse) talk to LiteLLM unattributed,
# and spec goal 4 (attribution at every granularity) does not tolerate that.
: "${GC_WEBHOOK_ARG_MODEL:?GC_WEBHOOK_ARG_MODEL is unset -- this is gonk-meter's rung decision, never a literal, and there is no default}"
: "${GC_WEBHOOK_ARG_METADATA_JSON:?GC_WEBHOOK_ARG_METADATA_JSON is unset -- the attribution seam (OD-7) has nothing to stamp}"
: "${GONK_LITELLM_URL:?GONK_LITELLM_URL is unset}"
: "${GONK_LITELLM_KEY_FILE:?GONK_LITELLM_KEY_FILE is unset -- the virtual key is a FILE MOUNT, never an env value}"

if [ ! -f "${GONK_LITELLM_KEY_FILE}" ]; then
	log "GONK_LITELLM_KEY_FILE=${GONK_LITELLM_KEY_FILE} does not exist -- refusing to start unattributed/unauthenticated"
	exit 1
fi

OVERLAY_PATH="${GONK_OPENCODE_OVERLAY:-/etc/gonk/overlay/opencode.json}"
mkdir -p "$(dirname "${OVERLAY_PATH}")"

# jq builds the JSON, not string concatenation: GC_WEBHOOK_ARG_METADATA_JSON is
# itself a JSON blob (gonk-meter's verbatim DecideResponse.Metadata,
# marshaled in cmd/gonk-gate/dispatch.go) that must land as the STRING VALUE
# of one header -- jq's --arg escapes it correctly (embedded quotes and all),
# where naive `sed`/string-substitution would silently corrupt the config the
# moment a value contained a double quote.
#
# apiKey is opencode's own `{file:...}` variable syntax (NOT expanded here --
# confirmed in opencode's source, packages/opencode/src/config/variable.ts:
# it resolves {file:<absolute path>} at config-load time and JSON-escapes the
# file's content). That means this script never reads the key into its own
# environment or a shell variable at all -- only opencode's own process ever
# touches the key bytes, and only at the moment it needs them.
jq -n \
	--arg model "${GC_WEBHOOK_ARG_MODEL}" \
	--arg baseurl "${GONK_LITELLM_URL%/}/v1" \
	--arg keyfile "{file:${GONK_LITELLM_KEY_FILE}}" \
	--arg metadata "${GC_WEBHOOK_ARG_METADATA_JSON}" \
	'{
		"$schema": "https://opencode.ai/config.json",
		"model": ("gonk/" + $model),
		"provider": {
			"gonk": {
				"npm": "@ai-sdk/openai-compatible",
				"name": "gonk (LiteLLM)",
				"options": {
					"baseURL": $baseurl,
					"apiKey": $keyfile,
					"headers": {
						"x-litellm-spend-logs-metadata": $metadata
					}
				},
				"models": {
					($model): { "name": $model }
				}
			}
		}
	}' >"${OVERLAY_PATH}"

log "rendered ${OVERLAY_PATH} (model=${GC_WEBHOOK_ARG_MODEL})"

export OPENCODE_CONFIG="${OVERLAY_PATH}"

# ---- Step 3: exec opencode ---------------------------------------------------
exec opencode "$@"
