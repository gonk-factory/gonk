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
# --- WHERE THE PER-SESSION VALUES COME FROM (read this before "fixing" it) ----
# NOT from GC_WEBHOOK_ARG_*. That is an EXEC-ORDER env overlay
# (internal/orderdispatch/dispatch.go at GASCITY_REF), and the thing that starts
# this pod is a FORMULA order. A session pod's environment is resolved.Env --
# STATIC agent/provider/city config -- plus a fixed passthrough allow-list
# (internal/processenv/provider.go: PATH, HOME, USER, TZ, CLAUDE_*, locale) plus
# Dolt/city path projections. No per-session value can reach it, and no GONK_*
# env is inherited from the controller at all.
#
# The ONE per-session channel is the PROMPT. pack/formulas/gonk-triage.toml
# stamps two marker lines at the top of the rendered step:
#
#     <!-- gonk:model:<model> -->
#     <!-- gonk:meta:<metadata json> -->
#
# We parse them out of our own --prompt argument, strip them, and hand the rest
# to opencode. Secrets are NOT in the prompt (they are static pod env, below):
# a key in the prompt is a key in the model's context and in every transcript.
#
# argv is `--prompt <text>` (agent.toml prompt_mode=flag/prompt_flag=--prompt).
GONK_PROMPT=""
for _arg in "$@"; do
	case "${_prev_arg:-}" in
	--prompt) GONK_PROMPT="${_arg}" ;;
	esac
	_prev_arg="${_arg}"
done

marker_value() {
	# $1 = marker name. Prints the value, or nothing. `head -1` because only the
	# first occurrence is ours; a hostile issue body cannot forge an earlier one
	# (the formula puts these at the very top of the step it renders).
	printf '%s\n' "${GONK_PROMPT}" \
		| sed -n "s/^<!-- gonk:$1:\(.*\) -->[[:space:]]*$/\1/p" \
		| head -1
}

: "${GC_WEBHOOK_ARG_MODEL:=$(marker_value model)}"
: "${GC_WEBHOOK_ARG_METADATA_JSON:=$(marker_value meta)}"

: "${GC_WEBHOOK_ARG_MODEL:?no model: neither GC_WEBHOOK_ARG_MODEL nor a <!-- gonk:model:... --> prompt marker. This is gonk-meter s rung decision, never a literal, and there is no default}"
: "${GC_WEBHOOK_ARG_METADATA_JSON:?no attribution metadata: neither GC_WEBHOOK_ARG_METADATA_JSON nor a <!-- gonk:meta:... --> prompt marker -- the attribution seam (OD-7) has nothing to stamp}"

# --- v1-minimal session-config delivery (gonk-aql / minimal opencode leg) ------
# Gas City's k8s session provider mounts no gonk secrets and controller env does
# not flow to sessions (allow_env_override is inert at GASCITY_REF), so the
# LiteLLM URL/key and the bot token are threaded in as ORDER ARGS
# (GC_WEBHOOK_ARG_*, set by cmd/gonk-gate/dispatch.go from controller env). This
# is a DELIBERATE v1 compromise: two of these are secrets transiting an order var,
# which the v2 broker removes by keeping all creds out of the pod. The overlay
# still consumes the key only as opencode's {file:...} syntax -- the key bytes
# are written to a 0600 file here and never enter the rendered JSON or a shell
# var that a child process inherits.
: "${GONK_LITELLM_URL:=${GC_WEBHOOK_ARG_LITELLM_URL:-}}"
: "${GONK_LITELLM_URL:?GONK_LITELLM_URL is unset (no GC_WEBHOOK_ARG_LITELLM_URL either)}"

if [ -z "${GONK_LITELLM_KEY_FILE:-}" ]; then
	# Materialize the virtual key into a private file so opencode's {file:...}
	# apiKey syntax can read it without the key ever appearing in the rendered
	# config. GONK_LITELLM_KEY is STATIC pod env (per-install, not per-session):
	# it is the one channel Gas City leaves open, since resolved.Env comes from
	# city/agent config. GC_WEBHOOK_ARG_LITELLM_KEY is accepted as a fallback for
	# any non-gascity caller that does set it.
	_key="${GONK_LITELLM_KEY:-${GC_WEBHOOK_ARG_LITELLM_KEY:-}}"
	if [ -z "${_key}" ]; then
		log "no LiteLLM key: set GONK_LITELLM_KEY_FILE, GONK_LITELLM_KEY, or GC_WEBHOOK_ARG_LITELLM_KEY -- refusing to start unauthenticated"
		exit 1
	fi
	GONK_LITELLM_KEY_FILE="${GONK_RUNTIME_DIR:-/tmp/gonk}/llkey"
	mkdir -p "$(dirname "${GONK_LITELLM_KEY_FILE}")"
	( umask 077; printf '%s' "${_key}" > "${GONK_LITELLM_KEY_FILE}" )
	unset _key
	log "materialized LiteLLM key file at ${GONK_LITELLM_KEY_FILE}"
fi

if [ ! -f "${GONK_LITELLM_KEY_FILE}" ]; then
	log "GONK_LITELLM_KEY_FILE=${GONK_LITELLM_KEY_FILE} does not exist -- refusing to start unattributed/unauthenticated"
	exit 1
fi

# glab auth for the direct-post path: the triage prompt has opencode read the
# issue and post the comment via glab. glab honours GITLAB_HOST + GITLAB_TOKEN.
# (v2: the broker posts and the agent holds no GitLab token.)
# GONK_BOT_TOKEN is STATIC pod env, same reasoning as the LiteLLM key.
_bot="${GONK_BOT_TOKEN:-${GC_WEBHOOK_ARG_BOT_TOKEN:-}}"
if [ -n "${_bot}" ]; then
	export GITLAB_HOST="${GITLAB_HOST:-${GONK_GITLAB_HOST:-gitlab.orac.local}}"
	export GITLAB_TOKEN="${GITLAB_TOKEN:-${_bot}}"
	log "configured glab for ${GITLAB_HOST}"
else
	log "no bot token -- glab is unauthenticated; the triage comment will not post"
fi
unset _bot

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
# Strip the gonk marker lines from the prompt before the model ever sees them:
# they are plumbing, and a model that reads "<!-- gonk:meta:{...} -->" may well
# echo it into the comment it posts. Everything else is passed through verbatim,
# argument boundaries intact.
if [ -n "${GONK_PROMPT}" ]; then
	# Rebuild argv positionally, swapping ONLY the value that follows --prompt.
	# Any other flag opencode was given survives untouched, in order.
	_clean=$(printf '%s\n' "${GONK_PROMPT}" | sed '/^<!-- gonk:\(model\|meta\):.* -->[[:space:]]*$/d')
	_orig_argc=$#
	_take_next=0
	for _arg in "$@"; do
		if [ "${_take_next}" = "1" ]; then
			set -- "$@" "${_clean}"
			_take_next=0
		else
			set -- "$@" "${_arg}"
			[ "${_arg}" = "--prompt" ] && _take_next=1
		fi
	done
	# Drop the original argv, keeping only the rebuilt copy appended above.
	shift "${_orig_argc}"
	unset _clean _orig_argc _take_next _arg
fi

exec opencode "$@"
