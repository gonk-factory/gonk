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

# ALSO WRITE TO THE POD'S STDOUT, NOT JUST THIS PROCESS'S STDERR (gonk-dot).
#
# Gas City launches us as `tmux new-session -d ... && sleep infinity`, and tmux
# DETACHES -- so stderr here reaches the tmux pane and NOTHING reaches the
# container log. `kubectl logs` on a session pod is empty, always, and that
# emptiness reads as "nothing happened" rather than "you cannot see what
# happened". On 2026-09-07 an agent was refusing to start for a reason this
# function already printed -- no LiteLLM key -- and finding it took several
# rounds of racing capture scripts against pods that live under a minute.
#
# /proc/1/fd/1 is pid 1's stdout, which IS the container log, regardless of how
# many times tmux has re-parented us. Best effort: if it is not writable the
# message still goes to stderr, because a logging line must never be the reason
# an agent fails to start.
log() {
	printf 'gonk-agent-entrypoint: %s\n' "$1" >&2
	printf 'gonk-agent-entrypoint: %s\n' "$1" >>/proc/1/fd/1 2>/dev/null || true
}

# ---- Step 0: say who we are, before anything can fail -----------------------
# THE FIRST LINE IN THE CONTAINER LOG (gonk-dot). Everything below can refuse to
# start, and a refusal is only actionable if the reader knows WHICH session
# refused. Identity only -- no credential, no prompt text.
# THE ALIAS IS A CAPABILITY (128 bits; see the prompt-fetch comment below), so
# only a short PREFIX is logged -- enough to correlate a pod with a session in
# the controller log, never enough to replay a grant. The rig checkout grant is
# TTL-bounded and RE-FETCHABLE (pkg/rig: DefaultGrantTTL 30m, no consume-on-read),
# so a full alias in a log operators are told to read is a live credential.
# Computed OUT of the log line on purpose: the redaction test forbids GC_ALIAS
# appearing in a log call at all, and an allowlist exception for "but this one
# truncates it" is exactly the kind of hole that later hides a real leak.
# `${GC_ALIAS:-}` (NOT bare ${GC_ALIAS}): under `set -u` an unset GC_ALIAS
# would abort THIS expansion, before the log line below ever runs -- into a
# detached tmux pane, so the pod's container log showed nothing at all
# (R-06/R-37). Reassigning it here also makes every later `${GC_ALIAS:-}` in
# this file consistent with a variable that is now always at least defined.
GC_ALIAS="${GC_ALIAS:-}"
GONK_ALIAS_PREFIX="${GC_ALIAS%%"${GC_ALIAS#??????}"}"
log "session start: alias=${GONK_ALIAS_PREFIX}... agent=${GC_AGENT:-triage} attempt=${GC_WEBHOOK_ARG_ATTEMPT:-?}"

# A pool session with no alias at all has no session identity and nothing to
# fetch a checkout or a prompt for -- refuse now, LOUDLY, rather than limping
# into steps that all silently no-op on an empty GC_ALIAS.
#
# exit 5, NOT 2: dash (and every other shell tested) exits 2 for its OWN
# generic "parameter not set" abort under set -u -- which is precisely the
# accidental failure this guard replaces. Reusing 2 here would make a
# deliberate, logged refusal indistinguishable from an unguarded expansion
# blowing up somewhere else in this script, to any reader (a controller, a
# pod terminationMessage, `kubectl get pod -o jsonpath`) that only sees the
# exit status. 1, 3 and 4 are already the other deliberate refusal codes
# below (no model / prompt already consumed / no prompt), so 5 is the next
# free one.
if [ -z "${GC_ALIAS}" ]; then
	log "no alias: GC_ALIAS is unset or empty -- refusing to start without a session identity"
	log "session end: refused (no alias)"
	exit 5
fi

log "expecting: checkout=$([ -n "${GONK_RIG_BASE_URL:-}" ] && echo yes || echo no) prompt=$([ -n "${GONK_PROMPT_URL:-}" ] && echo yes || echo no)"

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
	if [ -z "${_url}" ]; then
		# Was silent. A session with no working copy behaves very differently and
		# the reader needs to know it was a CHOICE, not a failure.
		log "no checkout for this session (no rig URL); running without a working copy"
		return 0
	fi

	_tmp="${GONK_RUNTIME_DIR:-/tmp/gonk}/rig.tar.gz"
	mkdir -p "$(dirname "${_tmp}")" "${RIG_DIR}"
	if ! curl -fsS --max-time 120 -o "${_tmp}" "${_url}"; then
		# NOT ${_url}: it embeds GC_ALIAS, and the rig grant is re-fetchable.
		log "WARNING: could not fetch the session checkout (rig endpoint unreachable or refused)"
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
# NOT from GC_WEBHOOK_ARG_* as inherited env. That would be an EXEC-ORDER env
# overlay (internal/orderdispatch/dispatch.go at GASCITY_REF), and this pod is
# not started by an order at all -- gonk-dispatch creates the broker agent
# SESSION directly (cmd/gonk-gate/broker_inject.go's runBrokerDispatch). A
# session pod's environment is resolved.Env -- STATIC agent/provider/city
# config -- plus a fixed passthrough allow-list (internal/processenv/
# provider.go: PATH, HOME, USER, TZ, CLAUDE_*, locale) plus Dolt/city path
# projections. No per-session value can reach it that way, and no GONK_* env
# is inherited from the controller at all.
#
# THE PER-SESSION CHANNEL IS THE PROMPT, fetched by reference below (Step 1.5,
# gonk-mzd) and setting GC_WEBHOOK_ARG_MODEL/GC_WEBHOOK_ARG_METADATA_JSON
# directly from the fetched row's model/metadata fields once it lands.
#
# This USED TO be two marker lines a formula step stamped at the top of the
# rendered prompt --
#
#     <!-- gonk:model:<model> -->
#     <!-- gonk:meta:<metadata json> -->
#
# -- parsed out of our own --prompt argument by marker_value() below. ADR-007
# §3 deleted the whole formula layer that stamped them (including
# pack/formulas/gonk-triage.toml), and no live path hands this entrypoint a
# --prompt argument any more (broker sessions carry no launch-time Message at
# all -- see cmd/gonk-gate/dispatch_test.go's
# TestDispatchCreatesTriageSessionOnRun). marker_value() and its precedence
# chain below are dead code now, left in place as a harmless, no-op fallback
# rather than ripped out here -- that cleanup is outside a formula-layer
# deletion's scope.
#
# argv is `--prompt <text>` (agent.toml prompt_mode=flag/prompt_flag=--prompt).
GONK_PROMPT=""
for _arg in "$@"; do
	case "${_prev_arg:-}" in
	--prompt) GONK_PROMPT="${_arg}" ;;
	esac
	_prev_arg="${_arg}"
done

# ---- Step 1.5: fetch this session's prompt (gonk-mzd) ------------------------
# The pod PULLS its prompt instead of having it typed into the opencode TUI.
#
# WHY: keystroke delivery is a race against composer readiness, and it fails
# silently. Across five live runs one composer received text; the pod carried
# GC_STARTUP_PROMPT_DELIVERED=1 every time, including when the composer was
# visibly empty. A fetch either returns the prompt or fails loudly, which is the
# property that channel never had.
#
# NO CREDENTIAL. GC_ALIAS carries 128 bits of entropy and IS the capability, so
# there is nothing to mount here and the LiteLLM-key dance does not apply.
#
# TREAT THE ALIAS AS SECRET IN LOGS. This script logs freely and its pane is
# captured and read by sweep, so the URL is never echoed -- printing it would
# publish the capability into the transcript.
if [ -z "${GONK_PROMPT}" ] && [ -n "${GONK_PROMPT_URL:-}" ] && [ -n "${GC_ALIAS:-}" ]; then
	# The URL is NOT logged: it embeds GC_ALIAS, which is a capability.
	log "fetching prompt by reference (one-shot, by alias)"
	_purl="${GONK_PROMPT_URL%/}/v1/prompt/${GC_ALIAS}"
	_pfile="${GONK_RUNTIME_DIR:-/tmp/gonk}/prompt.json"
	mkdir -p "$(dirname "${_pfile}")"

	# Retry 404 AND transport failures: the pod can legitimately beat the
	# controller's PUT (404), and a refused/reset/timed-out connection (curl
	# exit != 0) is not evidence the prompt was ever consumed -- it is
	# evidence nothing was reached at all, which is exactly the case this
	# retry window exists for. Any REAL HTTP status other than 404 is still
	# terminal -- retrying a 410 would just re-confirm that someone already
	# took it.
	#
	# WHY THE "000"/malformed CHECK, NOT JUST A BARE nonzero-exit CHECK:
	# `curl -sS -o f -w '%{http_code}' ... || echo 000` runs `echo 000` only
	# when curl's own exit status is nonzero, but curl ALSO writes its -w
	# output ("000", the sentinel it uses when no HTTP response was ever
	# received) even on that same failure. Both land in the same command
	# substitution, so a single refused connection captures "000" (curl's own
	# -w) immediately followed by "000" (the `|| echo`) as ONE string:
	# "000000" -- which matched neither "200" nor "404" and fell into the old
	# catch-all `break`, so one refused connection exited 4 after zero
	# retries despite a 120s window (R-07). Treat anything that is not
	# exactly three digits (empty, "000000", ...) the same as a bare "000":
	# a transport failure, not a real status code, so keep waiting.
	_deadline=$(( $(date +%s) + ${GONK_PROMPT_WAIT_SECS:-120} ))
	_code=""
	while :; do
		_code=$(curl -sS -o "${_pfile}" -w '%{http_code}' --max-time 20 "${_purl}" 2>/dev/null || echo 000)
		case "${_code}" in
			200) break ;;
			404) : ;;                     # not stored yet -- keep waiting
			[1-9][0-9][0-9]) break ;;     # a REAL HTTP status (410, 5xx, ...) -- terminal
			*) : ;;                       # "000", "000000", empty, ... -- transport failure, keep waiting
		esac
		[ "$(date +%s)" -ge "${_deadline}" ] && break
		sleep 2
	done

	case "${_code}" in
		200)
			GONK_PROMPT=$(jq -r '.prompt // empty' <"${_pfile}")
			_pmodel=$(jq -r '.model // empty' <"${_pfile}")
			_pmeta=$(jq -r '.metadata // empty' <"${_pfile}")
			# THIS SESSION'S PROJECT KEY (gonk-8gb). It rides the prompt row
			# because that is the only per-session channel that reaches a pod:
			# order vars are env for the dispatch exec, and pod env comes from
			# resolved.Env, which Gas City builds from per-INSTALL agent config.
			# Never logged, and never echoed into the rendered opencode config --
			# it goes straight to the key file below.
			_pkey=$(jq -r '.litellm_key // empty' <"${_pfile}")
			# The model and metadata travel WITH the prompt so the overlay is
			# rendered per session. That is what restores per-bead attribution
			# (gonk-m6t) rather than attributing spend per install.
			[ -n "${_pmodel}" ] && GC_WEBHOOK_ARG_MODEL="${_pmodel}"
			[ -n "${_pmeta}" ] && GC_WEBHOOK_ARG_METADATA_JSON="${_pmeta}"
			[ -n "${_pkey}" ] && GC_WEBHOOK_ARG_LITELLM_KEY="${_pkey}"
			unset _pkey
			rm -f "${_pfile}"
			log "prompt fetched (${#GONK_PROMPT} bytes)"
			;;
		410)
			# ALREADY CONSUMED -- the opposite diagnosis from a timeout. Either
			# this pod was respawned past the fetch (Gas City relaunches a dead
			# agent into the warm pod and re-runs the start command), or someone
			# else took it. Exit distinctly so the logs say which, rather than
			# looking like a delivery bug.
			log "FATAL: prompt already consumed (410) -- respawn past the fetch, or theft"
			log "session end: refused (prompt already consumed)"
			exit 3
			;;
		*)
			# An agent that idles while looking healthy is the failure mode this
			# whole change exists to end. Exit non-zero and loudly.
			log "FATAL: no prompt after ${GONK_PROMPT_WAIT_SECS:-120}s (last status ${_code})"
			log "session end: refused (no prompt)"
			exit 4
			;;
	esac
	unset _purl _pfile _deadline _code _pmodel _pmeta
fi

marker_value() {
	# ALWAYS FINDS NOTHING since ADR-007 §3 (see the comment above Step 2).
	# GONK_PROMPT is not necessarily empty here -- Step 1.5's fetch populates
	# it with the broker's rendered prompt text for a real dispatched session
	# -- but no live path stamps a "<!-- gonk:model:...-->" / "<!--
	# gonk:meta:...-->" line into that text any more (the broker's own tests
	# assert its rendered prompts never carry them). So this greps real
	# content for a pattern nothing produces, rather than searching empty
	# input; either way the result is always empty. Left in place as a
	# harmless fallback rather than removed as part of a formula-layer
	# deletion.
	#
	# $1 = marker name. Prints the value, or nothing. `head -1` because only the
	# first occurrence is ours; a hostile issue body cannot forge an earlier one
	# (a formula step used to put these at the very top of the step it
	# rendered).
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
# WHICH SOURCE WON IS THE INTERESTING PART, not just the value: a session running
# the static per-install default rather than the meter's rung decision is a
# different situation, and it used to be indistinguishable in the log.
_model_before="${GC_WEBHOOK_ARG_MODEL:-}"
: "${GC_WEBHOOK_ARG_MODEL:=$(marker_value model)}"
: "${GC_WEBHOOK_ARG_METADATA_JSON:=$(marker_value meta)}"
if [ -n "${_model_before}" ]; then
	GONK_MODEL_SOURCE="session-arg"
elif [ -n "${GC_WEBHOOK_ARG_MODEL}" ]; then
	GONK_MODEL_SOURCE="prompt-marker"
fi
: "${GC_WEBHOOK_ARG_MODEL:=${GONK_MODEL:-}}"
if [ -z "${GONK_MODEL_SOURCE:-}" ] && [ -n "${GC_WEBHOOK_ARG_MODEL}" ]; then
	GONK_MODEL_SOURCE="static-install-default"
	log "WARNING: model came from the static per-install default, NOT the meter's rung decision"
fi
if [ -n "${GC_WEBHOOK_ARG_METADATA_JSON}" ]; then
	log "attribution metadata present (${#GC_WEBHOOK_ARG_METADATA_JSON} bytes)"
fi

if [ -z "${GC_WEBHOOK_ARG_MODEL}" ]; then
	log "no model: no <!-- gonk:model:... --> prompt marker, no GC_WEBHOOK_ARG_MODEL, no GONK_MODEL."
	log "this is gonk-meter's rung decision and there is no built-in default -- refusing to start."
	log "session end: refused (no model)"
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
	# PER-SESSION KEY WINS OVER THE STATIC ONE (gonk-8gb). GONK_LITELLM_KEY is a
	# PER-INSTALL channel written into agent.toml at bootstrap; the project's own
	# virtual key can only arrive per-session, because a project key is
	# per-project and agent.toml is not. Preferring the static value meant every
	# session used the install-wide key -- which was the proxy ADMIN key -- and
	# the per-project budget the meter had provisioned was never consulted.
	#
	# The order matters more than it looks: dispatch now resolves the project key
	# and fails closed if it cannot, so GC_WEBHOOK_ARG_LITELLM_KEY being present
	# means the meter vouched for it. The static value is the fallback for a
	# non-gascity caller that sets no per-session key at all.
	# The per-session key (delivered in the prompt row) wins. The static
	# per-install value remains only as a fallback for a caller that supplies no
	# prompt row at all, and it announces itself as unmetered when used --
	# because a per-install key cannot be a per-project budget (gonk-8gb).
	_key="${GC_WEBHOOK_ARG_LITELLM_KEY:-${GONK_LITELLM_KEY:-}}"
	if [ -n "${GC_WEBHOOK_ARG_LITELLM_KEY:-}" ]; then
		log "using this session's per-project LiteLLM key"
	elif [ -n "${GONK_LITELLM_KEY:-}" ]; then
		log "WARNING: no per-session LiteLLM key; falling back to the static per-install key, which is NOT metered per project (gonk-8gb)"
	fi
	if [ -z "${_key}" ]; then
		log "no LiteLLM key: set GONK_LITELLM_KEY_FILE, GONK_LITELLM_KEY, or GC_WEBHOOK_ARG_LITELLM_KEY -- refusing to start unauthenticated"
		log "session end: refused (no LiteLLM key)"
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
	log "session end: refused (key file missing)"
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

log "rendered ${OVERLAY_PATH} (model=${GC_WEBHOOK_ARG_MODEL}, source=${GONK_MODEL_SOURCE:-unknown})"

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
	log "session end: refused (provider check failed)"
	exit 1
fi
if [ -z "${_models}" ]; then
	log "opencode models returned nothing -- cannot confirm the provider is gonk; refusing to start unmetered"
	log "session end: refused (provider check empty)"
	exit 1
fi
_foreign=$(printf '%s\n' "${_models}" | sed '/^[[:space:]]*$/d' | grep -v '^gonk/' || true)
if [ -n "${_foreign}" ]; then
	log "opencode resolved non-gonk provider(s) -- refusing to start unmetered:"
	printf '%s\n' "${_foreign}" | while IFS= read -r _line; do log "  ${_line}"; done
	log "session end: refused (non-gonk provider resolved)"
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
	#
	# NOT `exec`, AND WE HOLD AFTERWARDS. THIS IS LOAD-BEARING (gonk-2tb).
	#
	# Gas City serves the session transcript by asking TMUX for it, and tmux is
	# only alive while the command it was given is still running: the k8s
	# provider launches us as `tmux new-session -d -s main "gonk-agent-entrypoint"`,
	# so the moment this script returns, the session ends and the tmux SERVER
	# exits. The pod itself survives on the provider's trailing `sleep infinity`,
	# which is what makes the failure so confusing -- the session is still
	# "there", it just has no tmux to read from.
	#
	# MEASURED, 2026-09-02, issue !42 attempt 1 on qwen3-14b: the agent emitted a
	# complete and fully valid batch (ParseBatch, shape, targets and paths all
	# pass -- replayed against the real broker path), the pod was still alive and
	# the session was not closed until a second AFTER judgement, and yet the
	# sweep recorded `violation: no GONK_BATCH_START/END fence in session
	# transcript`. `tmux list-sessions` inside a post-turn pod returns "no server
	# running on /tmp/tmux-65532/default". The work was done and then thrown away
	# for want of a reader.
	#
	# This was introduced by the move to non-interactive `run`: the TUI never
	# exited, so tmux never died and the pane was always readable. Holding here
	# restores that property without giving up non-interactive execution. It is
	# safe to hold precisely because the sweep now treats a CLOSED FENCE as the
	# completion signal rather than process exit (gonk-au1) -- the batch, not the
	# exit, is what ends the turn -- and the sweep closes the session itself once
	# it has judged, which is what finally reaps us.
	# WIDEN THE PANE BEFORE ANY OUTPUT IS PRODUCED (gonk-d6h).
	#
	# Gas City serves the transcript by CAPTURING THIS TMUX PANE, and it does not
	# join wrapped lines. A detached tmux session with no client attached sits at
	# the default 80x24, so a single-line JSON batch comes back with real
	# newlines inserted at every 80th column -- mid-word. Measured on issue !45,
	# the first successful end-to-end triage: the payload opens with 38
	# characters of JSON preamble followed by 42 characters of body, and the
	# break lands at exactly 80.
	#
	# Widening here is the fix for the CAUSE. The alternative -- joining newlines
	# when we parse -- cannot distinguish a wrap from a newline the model meant,
	# so it would silently corrupt any legitimately multi-line body.
	#
	# Best-effort: every one of these is non-fatal. A tmux that does not support
	# `window-size` or a launcher that does not use tmux at all must not stop the
	# agent from working -- a wrapped comment is bad, no comment is worse.
	tmux set-option -g window-size manual 2>/dev/null || true
	tmux resize-window -t main -x 4000 -y 200 2>/dev/null || true
	log "pane width now: $(tmux display -p '#{pane_width}' 2>/dev/null || echo unknown)"

	log "starting opencode run (non-interactive)"
	# `|| _rc=$?` IS LOAD-BEARING, NOT STYLE. set -e is in force (top of file),
	# so a bare `opencode run` that exits non-zero kills this shell immediately:
	# the session-end line below never prints AND the tmux hold never runs, so
	# tmux dies and the transcript is unreadable -- reintroducing gonk-2tb on
	# exactly the failed turns an operator most needs to read.
	_rc=0
	opencode run -- "${_clean}" || _rc=$?
	log "session end: opencode run exited rc=${_rc}; holding so tmux stays alive for the transcript read (gonk-2tb)"
	# `wait` would return immediately (no background jobs); sleep in a loop is
	# the portable hold. The sweep closes the session when it has judged.
	while :; do sleep 3600; done
fi

# No prompt: nothing to run non-interactively. Fall back to whatever we were
# given, unchanged -- the entrypoint refuses to start without a model anyway.
exec opencode "$@"
