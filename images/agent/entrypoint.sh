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

# ---- Step 0: fetch this session's CHECKOUT, if one was granted --------------
# THE REPOSITORY IS NOT IN THIS IMAGE. /workspace is created empty and nothing
# used to put anything in it, while the scaffold prompt claimed a checkout
# existed -- so the agent read an empty directory and invented project context
# that gonk then committed (gonk-msz).
#
# gonk-gate grants a per-session checkout at the DECISION POINT (where the
# project, ref and event shape are all known) and gonk-intake serves the tree on
# its private listener. We fetch it HERE, before opencode starts, so a checkout
# is a PRECONDITION rather than a task the model can fail at. The pod holds no
# forge credentials: these bytes come from gonk, not from GitLab.
#
# THE POD COMPOSES ITS OWN URL, and that is the whole trick. A pooled session
# pod cannot be handed per-session env -- SessionCreateBody has no env field and
# resolved.Env is static agent/city config -- so a URL built controller-side
# could never reach it. But the two halves are separately available:
#
#   GONK_RIG_BASE_URL  per-INSTALL, injected into agent.toml's [env] by the
#                      chart's bootstrap-city initContainer, the same channel
#                      that already carries GONK_LITELLM_URL/GONK_MODEL.
#   GC_ALIAS           per-SESSION, put in every agent pod's env by Gas City
#                      itself (verified live with printenv; GC_SESSION_ID is
#                      there too).
#
# So the pod appends its own alias to a static base and fetches exactly the tree
# its own session was granted -- no per-session channel, and no dependency on
# the prompt-delivery redesign. GONK_RIG_URL still wins if set explicitly, which
# is what the e2e harness uses.
#
# NON-FATAL by design: a session with no checkout still runs, on a prompt that
# says so.
gonk_fetch_checkout() {
	_url="${GONK_RIG_URL:-}"
	if [ -z "${_url}" ] && [ -n "${GONK_RIG_BASE_URL:-}" ] && [ -n "${GC_ALIAS:-}" ]; then
		_url="${GONK_RIG_BASE_URL%/}/rig/${GC_ALIAS}.tar.gz"
	fi
	[ -n "${_url}" ] || return 0

	_tmp="${GONK_RUNTIME_DIR:-/tmp/gonk}/rig.tar.gz"
	mkdir -p "$(dirname "${_tmp}")" "${RIG_DIR}"
	if ! curl -fsS --max-time 120 -o "${_tmp}" "${_url}"; then
		log "WARNING: could not fetch the session checkout from ${_url}"
		log "WARNING: this session runs WITHOUT a working copy"
		return 0
	fi

	# GitLab's archive wraps everything in one <project>-<sha>/ directory;
	# --strip-components=1 lands the tree at RIG_DIR itself. Extraction is the
	# one place untrusted archive paths could escape, so refuse ".." rather than
	# trusting the forge's tar.
	#
	# There is deliberately NO --no-absolute-names here: THAT FLAG DOES NOT
	# EXIST (gonk-j9z). GNU tar spells the opt-OUT `-P/--absolute-names` and
	# strips leading "/" BY DEFAULT, so the guard it was meant to add is already
	# on and the invented flag made tar exit 64 every time. Because the fetch is
	# non-fatal by design, that surfaced only as a WARNING into a tmux pane
	# opencode immediately redrew -- so every session since the rig slice landed
	# ran with a SILENTLY empty working copy. Test: test/entrypoint.
	if ! tar -xzf "${_tmp}" -C "${RIG_DIR}" --strip-components=1 \
		--exclude='*/..*/*' 2>/dev/null; then
		log "WARNING: could not extract the session checkout; running without a working copy"
		rm -f "${_tmp}"
		return 0
	fi
	rm -f "${_tmp}"
	log "fetched session checkout into ${RIG_DIR}"
}
gonk_fetch_checkout

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

# THE SESSION IS RESIDENT, SO NONE OF THIS MAY BE FATAL AT STARTUP.
# Gas City's k8s provider launches the pod as a POOL session:
#     tmux new-session -d -s main "gonk-agent-entrypoint" && sleep infinity
# with NO prompt appended -- the prompt is delivered later, into the running
# tmux, when a bead is assigned. So at startup there are no markers, and an
# entrypoint that exits on a missing model takes the tmux session with it: the
# server dies, the reconciler sees runtime-missing, and the pod is reaped and
# respawned in a ~60s loop. (Observed exactly that.)
#
# Precedence, most specific first: prompt marker (per-session, when we were
# handed a prompt) > GC_WEBHOOK_ARG_* (any non-gascity caller that sets it) >
# GONK_MODEL, the STATIC per-install default the chart injects from the
# operator's default rung. Static is the honest v1 answer: this deployment has
# one local rung, and per-session model selection is what the v2 broker adds.
: "${GC_WEBHOOK_ARG_MODEL:=$(marker_value model)}"
: "${GC_WEBHOOK_ARG_METADATA_JSON:=$(marker_value meta)}"
: "${GC_WEBHOOK_ARG_MODEL:=${GONK_MODEL:-}}"

if [ -z "${GC_WEBHOOK_ARG_MODEL}" ]; then
	log "no model: no <!-- gonk:model:... --> prompt marker, no GC_WEBHOOK_ARG_MODEL, no GONK_MODEL."
	log "this is gonk-meter's rung decision and there is no built-in default -- refusing to start."
	exit 1
fi

# Attribution metadata is NOT fatal, deliberately. A resident pool session has no
# bead yet, so there is nothing per-bead to stamp until a prompt arrives; dying
# here would trade all attribution for no session at all. The header is simply
# omitted when empty, and that is LOUD in the log so a silent loss of the
# attribution seam (OD-7) cannot pass for normal.
if [ -z "${GC_WEBHOOK_ARG_METADATA_JSON}" ]; then
	log "WARNING: no attribution metadata (no <!-- gonk:meta:... --> marker, no GC_WEBHOOK_ARG_METADATA_JSON)"
	log "WARNING: spend rows for this session will NOT carry per-bead attribution"
fi

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

# NO forge credentials in the agent pod (broker design, spec 9/10). The agent
# never talks to GitLab: the broker reads the issue and posts the triage comment
# on its behalf. So there is deliberately no glab auth here, and the pod holds no
# GONK_BOT_TOKEN / GITLAB_HOST / GITLAB_TOKEN. Its only secret is the LiteLLM
# virtual key materialized above.

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
		# LAYER 1 of the fail-open fix (gonk-ob5). An ALLOWLIST, not a
		# denylist: opencode ships built-in providers that are present even
		# when this overlay loads correctly, so the exposure was never limited
		# to the config-missing case. Proven on opencode 1.18.3 in pod
		# s-go-8oj: without this, `opencode models` lists 7 built-ins
		# alongside gonk/; with it, exactly gonk/. It blocks USE, not merely
		# listing -- an explicit `-m opencode/big-pickle` is refused, which is
		# the real threat (a prompt-injected agent switching models, not just
		# a silent fallback).
		"enabled_providers": ["gonk"],
		"model": ("gonk/" + $model),
		"provider": {
			"gonk": {
				"npm": "@ai-sdk/openai-compatible",
				"name": "gonk (LiteLLM)",
				"options": {
					"baseURL": $baseurl,
					"apiKey": $keyfile,
					# OMIT the attribution header entirely when there is no
					# metadata, rather than sending an empty one: LiteLLM would
					# record an empty metadata field, which reads on the spend row
					# exactly like "attributed to nothing" and is indistinguishable
					# from a bug in the attribution chain. No header is honest.
					"headers": (if $metadata == "" then {} else
						{"x-litellm-spend-logs-metadata": $metadata} end)
				},
				"models": {
					($model): { "name": $model }
				}
			}
		}
	}' >"${OVERLAY_PATH}"

log "rendered ${OVERLAY_PATH} (model=${GC_WEBHOOK_ARG_MODEL})"

export OPENCODE_CONFIG="${OVERLAY_PATH}"

# Keep `opencode models` (and opencode itself) from fetching a remote model
# list: it is egress this pod should not make, and it is a way for providers to
# appear after an upgrade nobody reviewed. It also makes the assertion below
# answer from CONFIG ALONE, so a LiteLLM blip cannot turn this guard into a
# crash-loop.
export OPENCODE_DISABLE_MODELS_FETCH=1

# ---- LAYER 2 of the fail-open fix (gonk-ob5) --------------------------------
# The layer that closes the hole that actually fired. Layer 1 lives INSIDE the
# config; if OPENCODE_CONFIG does not resolve, layer 1 does not exist either --
# which is exactly how this was found (a tmux server did not inherit the export,
# opencode silently used a built-in cloud provider, answered correctly, exited
# 0, and gonk had no way to know a model call had happened off-meter).
#
# So do not TRUST that the export took. ASK opencode what it resolved and refuse
# to start if the answer is anything but gonk. This turns opencode's fail-open
# into gonk's fail-closed, matching what the entrypoint already does for a
# missing model and a missing LiteLLM key.
#
# Written deliberately verbosely rather than as a one-line grep: the obvious
# `opencode models | grep -qv "^gonk/"` passes when the command ERRORS and
# prints nothing, and this codebase's recurring bug is a discarded error
# reporting success (see the 2026-08-19 handoff). Every branch here fails
# closed, so exit status and emptiness are both checked explicitly.
_models=$(opencode models 2>/dev/null); _rc=$?
if [ "${_rc}" -ne 0 ]; then
	log "opencode models exited ${_rc} -- cannot confirm the provider is gonk; refusing to start unmetered"
	exit 1
fi
if [ -z "${_models}" ]; then
	log "opencode models returned nothing -- cannot confirm the provider is gonk; refusing to start unmetered"
	exit 1
fi
_foreign=$(printf '%s\n' "${_models}" | sed '/^[[:space:]]*$/d' | grep -v '^gonk/' || true)
if [ -n "${_foreign}" ]; then
	log "opencode resolved non-gonk provider(s) -- refusing to start unmetered:"
	printf '%s\n' "${_foreign}" | while IFS= read -r _line; do log "  ${_line}"; done
	exit 1
fi
log "provider check ok: opencode resolved only gonk/ models"
unset _models _rc _foreign

# ---- Step 3: exec opencode ---------------------------------------------------
# RUN NON-INTERACTIVELY (gonk-5k5). `opencode <flags>` starts the TUI, which does
# not exit when the turn is done -- so Gas City reported the session Running
# forever, the sweep never read the completed batch, and the bead waited out its
# reservation and was classified infra-failed. That is the entire reason gonk had
# never posted a triage comment. `opencode run` takes the message positionally
# and has -i/--interactive defaulting to false, so it answers and exits, which is
# what lets a session reach a terminal state at all.
#
# THIS IS NOT SUFFICIENT ON ITS OWN, and that is measured, not assumed: `opencode
# run` against an unreachable endpoint did NOT exit either -- it hung until it
# was killed. These model/harness pairs pause, prompt and hesitate. The
# authoritative completion signal is therefore a CLOSED GONK_BATCH fence, which
# the sweep now honours even while the provider still reports the session running
# (cmd/gonk-gate/broker_apply.go). Non-interactive mode is the happy path; the
# fence is the one that holds when the harness misbehaves.
#
# The gonk marker lines are stripped before the model ever sees the prompt: they
# are plumbing, and a model that reads "<!-- gonk:meta:{...} -->" may well echo
# it into the comment it posts.
if [ -n "${GONK_PROMPT}" ]; then
	_clean=$(printf '%s\n' "${GONK_PROMPT}" | sed '/^<!-- gonk:\(model\|meta\):.* -->[[:space:]]*$/d')

	# Gas City passes the prompt as `--prompt <text>` (pack agent.toml
	# prompt_mode="flag"). `run` takes it positionally instead, so that pair is
	# consumed here rather than forwarded. Anything ELSE on argv cannot be
	# forwarded blind -- TUI flags are not `run` flags -- so it is LOGGED rather
	# than dropped in silence. A silently discarded argument is this codebase's
	# signature failure, and a loud line in the pane is what makes the next
	# harness change visible instead of mysterious.
	_dropped=""
	_skip=0
	for _arg in "$@"; do
		if [ "${_skip}" = "1" ]; then _skip=0; continue; fi
		if [ "${_arg}" = "--prompt" ]; then _skip=1; continue; fi
		_dropped="${_dropped} ${_arg}"
	done
	[ -n "${_dropped}" ] && log "not forwarding to \`opencode run\`:${_dropped} (TUI flags are not run flags; see gonk-5k5)"
	unset _skip _arg _dropped

	# `--` so a prompt beginning with a dash is never parsed as a flag.
	log "starting opencode run (non-interactive)"
	exec opencode run -- "${_clean}"
fi

# No prompt: nothing to run non-interactively. Fall back to whatever we were
# given, unchanged -- the entrypoint refuses to start without a model anyway.
exec opencode "$@"
