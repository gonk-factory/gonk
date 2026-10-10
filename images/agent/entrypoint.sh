#!/bin/sh
# gonk-agent-entrypoint: the agent pod's ENTRYPOINT (images/Dockerfile.agent).
#
# Does exactly two things, in order (Plan 04, Task 5, Step 3):
#
#   1. Renders overlay/opencode.json from THIS session's environment: the
#      LiteLLM base URL, the model gonk-meter chose (never a literal, never
#      chosen here), the virtual key (a FILE MOUNT, never an env value -- env
#      leaks into `ps`, /proc/<pid>/environ, crash dumps, and every child
#      process), and the attribution metadata as the static provider header
#      `provider.gonk.options.headers["x-litellm-spend-logs-metadata"]`,
#      stamped VERBATIM from gonk-meter's own DecideResponse.Metadata
#      (docs/environment.md, "VERIFIED: the attribution chain works"; Plan 04
#      OD-7).
#   2. execs opencode.
#
# There used to be a third step here, installing a prepare-commit-msg hook
# (Task 8) into the rig clone's .git/hooks/ for commit-provenance trailers.
# Deleted: the checkout this entrypoint fetches is a GitLab repository-archive
# tarball (pkg/rig, ArchiveFetcher.RepoArchive), never a git clone, so
# RIG_DIR/.git never existed in a real pod -- the hook install always took the
# "skip" branch, and with no hook installed, gonk-gate's own `trailers`
# subcommand was never invoked either. Both are gone now (spec §6.1's trailer
# paragraph is marked "not in v1").
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
#
# EXCEPT WHEN WE *ARE* PID 1 (gonk-t55, ADR-008 D10). Under a Kubernetes Job
# there is no tmux and no launcher: this script is the container's command, so
# /proc/1/fd/1 is our OWN stdout and stderr already IS the container log. The
# tee then wrote every line twice into the same pod log (observed in the T-55
# gate run). As PID 1, stderr alone is the container log, so write once.
log() {
	printf 'gonk-agent-entrypoint: %s\n' "$1" >&2
	if [ "$$" = "1" ]; then
		return 0
	fi
	printf 'gonk-agent-entrypoint: %s\n' "$1" >>/proc/1/fd/1 2>/dev/null || true
}

# ---- Which session runtime launched us (gonk-t55, ADR-008 D10) ---------------
# UNSET (or "gascity") is the Gas City path this script has always served: a
# tmux-wrapped pool session that fetches its prompt, model, metadata and key by
# alias, and holds after the run so tmux keeps the transcript readable. T-56
# deletes it; until then it must not change behaviour.
#
# "jobs" is the Kubernetes Jobs runtime (pkg/runtime/jobs). The Job always sets
# it, and it changes exactly three things: the prompt, the LiteLLM key and the
# attribution metadata are READ FROM MOUNTED FILES in GONK_SESSION_DIR (one
# Secret per Job, ADR-008 D3) instead of fetched; the model comes from the
# Job's own env (GONK_MODEL, the meter's per-session decision); and the script
# exits with opencode's rc instead of holding.
#
# Anything else is refused. A typo here would otherwise silently select the
# Gas City path, whose hold turns every clean Job into a DeadlineExceeded one.
# exit 7: 1-6 are the other deliberate refusal codes in this file.
case "${GONK_SESSION_RUNTIME:-gascity}" in
gascity | jobs) ;;
*)
	log "unknown GONK_SESSION_RUNTIME=${GONK_SESSION_RUNTIME} (expected jobs, gascity or unset) -- refusing to guess a delivery path"
	log "session end: refused (unknown session runtime)"
	exit 7
	;;
esac

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
if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
	# The Job name IS the session handle (ADR-008 D2) and is NOT a capability:
	# nothing fetches by it -- the prompt and key are mounted, not served by
	# name -- so it is logged in full.
	log "session start: runtime=jobs session=${GONK_SESSION_NAME:-?} agent=${GONK_AGENT:-?} attempt=${GONK_ATTEMPT:-?}"
else
	log "session start: alias=${GONK_ALIAS_PREFIX}... agent=${GC_AGENT:-triage} attempt=${GC_WEBHOOK_ARG_ATTEMPT:-?}"
fi

# ---- Jobs runtime: the SESSION CONTRACT handshake (gonk-t55 plan 3.3) --------
# gonk-gate (which builds this Job) and this image are deployed as SEPARATE
# images, and dev has run them at different versions for ~20 minutes at a time.
# Everything the two must agree on -- which env names are set, which files the
# session Secret holds and what they mean, what these exit codes say -- is
# versioned by one integer: pkg/runtime.SessionContract, which the runtime puts
# on every Job as GONK_SESSION_CONTRACT.
#
# GONK_SUPPORTED_SESSION_CONTRACTS is every version THIS image can serve,
# space-separated. Add a version here in the same commit that bumps the Go
# constant; test/entrypoint pins that this set contains it, so the two cannot
# drift silently. Keep it on ONE line of exactly this shape -- the test reads it.
GONK_SUPPORTED_SESSION_CONTRACTS="1"
#
# exit 8: DEDICATED to "unset or unsupported contract" (1-7 are this file's
# other codes). Checked BEFORE the session files are read, before the checkout
# is fetched and before opencode exists, so a skewed session spends nothing.
# gonk-sweep reads this exit code off the pod (runtime.ExitSessionContractMismatch)
# and treats the session as IMAGE SKEW: logged at ERROR, not charged, not an
# attempt, and parked to run again once the images agree. The
# "session contract mismatch:" line below is what the sweep quotes -- keep its
# shape (sent=... supported=[...]).
if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
	_contract_ok=""
	for _v in ${GONK_SUPPORTED_SESSION_CONTRACTS}; do
		if [ "${GONK_SESSION_CONTRACT:-}" = "${_v}" ]; then
			_contract_ok=1
		fi
	done
	if [ -z "${_contract_ok}" ]; then
		log "FATAL: session contract mismatch: sent=${GONK_SESSION_CONTRACT:-unset} supported=[${GONK_SUPPORTED_SESSION_CONTRACTS}] -- the controller that built this Job and this agent image disagree (image skew)"
		log "session end: refused (session contract mismatch)"
		exit 8
	fi
	unset _v _contract_ok
	log "session contract: ${GONK_SESSION_CONTRACT} (supported: ${GONK_SUPPORTED_SESSION_CONTRACTS})"
fi

# ---- Jobs runtime: the LOOP WATCHDOG's exit code and threshold (gonk-un7z) ---
# exit 9: DEDICATED to "the loop watchdog stopped opencode" (1-8 are this
# file's other codes; runtime.ExitLoopWatchdog on the Go side). In jobs mode
# opencode's output is passed through a line filter (gonk_run_opencode_watched,
# Step 3) that stops the session the moment the SAME tool call FAILS for the
# GONK_LOOP_WATCHDOG_THRESHOLD-th time, so the tokens a looping model would
# burn are never spent -- rather than spent and then classified. Observed on
# agentic/rom#1 attempt 1 (2026-10-04): the model Read two files that do not
# exist, the same pair three times, ~13k prompt tokens per call for no new
# information, and then ended without a batch.
#
# The sweep reads this code off the Failed Job together with the
# "loop watchdog: stopped opencode" line below (the code alone is not proof:
# opencode's own rc is passed through in this mode and could be 9) and logs the
# repeated call as the cause on its classification line. It is judged like any
# other finished session -- no batch, the model answered, gate-failed -- so the
# ladder escalates as it would for any bad answer.
#
# NOT A SESSION-CONTRACT CHANGE (runtime.SessionContract stays 1): no peer can
# misread it. An older controller sees a Failed Job with no batch and model
# spend, which it already judges gate-failed; a newer controller never sees 9
# from an older image. Bumping the contract would only park every session as
# image skew for the length of the rollout.
#
# THE DEFAULT THRESHOLD IS 2: the first time a call fails, the model has the
# error; an IDENTICAL call after that (same tool, same arguments) cannot return
# anything different -- the checkout does not change under a read-only agent --
# so the first repeat is already the loop, and every further one re-sends the
# whole context for nothing. 2 never stops a session for a single failure (a
# typo it then corrects is a DIFFERENT call), and successful calls never count,
# however often they repeat (rereading a file is legitimate). Overridable per
# install with GONK_LOOP_WATCHDOG_THRESHOLD (a positive integer).
GONK_EXIT_LOOP_WATCHDOG=9
GONK_LOOP_WATCHDOG_DEFAULT_THRESHOLD=2

# ---- Jobs runtime: this session's inputs are MOUNTED FILES (ADR-008 D3/D10) ---
# One Secret per Job, mounted read-only at GONK_SESSION_DIR, holding exactly:
#   prompt          the rendered prompt, verbatim
#   litellm-key     this project's virtual key (gonk-gate's fail-closed
#                   resolveLiteLLMKey, never an ambient key -- gonk-8gb)
#   metadata.json   gonk-meter's DecideResponse.Metadata, the compact JSON the
#                   prompt row's "metadata" field carried on the Gas City path
#
# ALL THREE ARE REQUIRED, and a missing or empty one is FATAL AT ONCE: never
# wait (the pod's volume is non-optional, so kubelet does not start us until
# the Secret exists -- a missing file is a bug, not a race), and never fall
# back to fetching (there is no alias to fetch by, and a fallback would be a
# second delivery path nobody tests). Checked BEFORE the checkout fetch, so a
# session that cannot run does not spend its rig grant first.
#
# exit 6: a distinct code for "the mounted session inputs are incomplete". 1-5
# are this file's other deliberate refusals (see the alias guard below), 7 is
# an unknown runtime, 8 a session contract mismatch (above). NOTE opencode's
# own rc is passed through at the end in this mode, so the code alone cannot
# distinguish this refusal from an opencode that happened to exit 6; the
# "session end:" line always does. (The same holds for 8, which is why the
# sweep requires the contract refusal LINE as well as the code.)
#
# THE OLD CHANNEL IS SHUT, not merely unused: any GC_WEBHOOK_ARG_* or GC_ALIAS
# that reached this pod's env is dropped here, so the mounted files (and the
# Job's own env for the model) are the ONLY sources in this mode.
if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
	unset GC_WEBHOOK_ARG_MODEL GC_WEBHOOK_ARG_METADATA_JSON GC_WEBHOOK_ARG_LITELLM_KEY GC_WEBHOOK_ARG_LITELLM_URL
	# And no alias: it would compose a rig URL (GONK_RIG_BASE_URL + alias) and
	# address a prompt row, both Gas City channels. The Job's checkout comes
	# only from the mounted rig-url (below).
	GC_ALIAS=""
	GONK_SESSION_DIR="${GONK_SESSION_DIR:-/run/gonk/session}"
	_incomplete=""
	for _f in prompt litellm-key metadata.json; do
		if [ ! -e "${GONK_SESSION_DIR}/${_f}" ]; then
			log "FATAL: session file ${GONK_SESSION_DIR}/${_f} is missing"
			_incomplete=1
		elif [ ! -r "${GONK_SESSION_DIR}/${_f}" ]; then
			log "FATAL: session file ${GONK_SESSION_DIR}/${_f} is not readable"
			_incomplete=1
		elif [ ! -s "${GONK_SESSION_DIR}/${_f}" ]; then
			log "FATAL: session file ${GONK_SESSION_DIR}/${_f} is empty"
			_incomplete=1
		fi
	done
	if [ -z "${_incomplete}" ]; then
		# Read NOW, before the checkout fetch, so a file that is non-empty but
		# blank (newlines only; $(...) strips them) is refused before anything
		# is spent too. $(cat ...) strips trailing newlines exactly as
		# $(jq -r ...) did on the Gas City prompt row.
		GONK_MOUNTED_TEXT=$(cat "${GONK_SESSION_DIR}/prompt")
		GONK_MOUNTED_METADATA=$(cat "${GONK_SESSION_DIR}/metadata.json")
		if [ -z "${GONK_MOUNTED_TEXT}" ] || [ -z "${GONK_MOUNTED_METADATA}" ]; then
			log "FATAL: the mounted prompt or metadata.json is blank (newlines only)"
			_incomplete=1
		fi
	fi
	if [ -n "${_incomplete}" ]; then
		log "session end: refused (session files incomplete in ${GONK_SESSION_DIR})"
		exit 6
	fi
	unset _f _incomplete
	log "session files present in ${GONK_SESSION_DIR}: prompt, litellm-key, metadata.json"

	# THE CHECKOUT URL IS MOUNTED TOO (plan 3.3, handshake item 4). It embeds
	# the rig grant's key, which pkg/rig serves the tree for until the grant's
	# TTL, so it is a capability: in env it was readable by anyone able to
	# `get` the Job. The session Secret carries it as `rig-url`, present ONLY
	# when a checkout was granted -- its absence means "no working copy", not
	# an error. Any GONK_RIG_URL in the env is IGNORED in this mode (unset,
	# so nothing below can use it), and the value read here is a plain shell
	# variable, NOT exported: opencode and its tools never see it.
	unset GONK_RIG_URL
	if [ -e "${GONK_SESSION_DIR}/rig-url" ]; then
		# Non-fatal like every checkout failure: an unreadable file is a
		# session without a working copy, said out loud (NOT the URL itself).
		if ! GONK_RIG_URL=$(cat "${GONK_SESSION_DIR}/rig-url" 2>/dev/null); then
			log "WARNING: ${GONK_SESSION_DIR}/rig-url is not readable; running without a working copy"
			unset GONK_RIG_URL
		elif [ -z "${GONK_RIG_URL}" ]; then
			log "WARNING: ${GONK_SESSION_DIR}/rig-url is empty; running without a working copy"
			unset GONK_RIG_URL
		fi
	fi
fi

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
if [ -z "${GC_ALIAS}" ] && [ "${GONK_SESSION_RUNTIME:-gascity}" != "jobs" ]; then
	log "no alias: GC_ALIAS is unset or empty -- refusing to start without a session identity"
	log "session end: refused (no alias)"
	exit 5
fi

if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
	# No alias in this mode, so only the mounted rig-url yields a checkout.
	log "expecting: checkout=$([ -n "${GONK_RIG_URL:-}" ] && echo yes || echo no) prompt=mounted"
else
	log "expecting: checkout=$([ -n "${GONK_RIG_BASE_URL:-}" ] && echo yes || echo no) prompt=$([ -n "${GONK_PROMPT_URL:-}" ] && echo yes || echo no)"
fi

# GONK_RIG_DIR defaults to the WORKDIR opencode is launched in (the rig
# clone).
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
	#
	# --no-same-owner --no-same-permissions (ADR-008 section 12 carry-forward):
	# the archive's members are uid 0 with the forge's modes, and the agent is
	# not root. GNU tar already implies both for a non-root user, so today these
	# change nothing; they are here so the checkout never depends on that
	# default -- an image that ever ran this as root would otherwise chown the
	# tree to whatever uid the archive names.
	if ! tar -xzf "${_tmp}" -C "${RIG_DIR}" --strip-components=1 \
		--no-same-owner --no-same-permissions \
		--exclude='*/..*/*' 2>/dev/null; then
		log "WARNING: could not extract the session checkout; running without a working copy"
		rm -f "${_tmp}"
		return 0
	fi
	rm -f "${_tmp}"
	log "fetched session checkout into ${RIG_DIR}"
}
gonk_fetch_checkout

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
# This USED TO be two HTML-comment marker lines a formula step stamped at the
# top of the rendered prompt -- one naming the model, one carrying the
# metadata JSON -- parsed out of our own --prompt argument by a
# marker_value() helper that used to live below. ADR-007 §3 deleted the whole
# formula layer that stamped them (including pack/formulas/gonk-triage.toml),
# and no live path hands this entrypoint a --prompt argument any more (broker
# sessions carry no launch-time Message at all -- see
# cmd/gonk-gate/dispatch_test.go's TestDispatchCreatesTriageSessionOnRun).
# marker_value() and the marker branch of the model/metadata precedence chain
# below have since been deleted along with it.
#
# argv is `--prompt <text>` (agent.toml prompt_mode=flag/prompt_flag=--prompt).
GONK_PROMPT=""
for _arg in "$@"; do
	case "${_prev_arg:-}" in
	--prompt) GONK_PROMPT="${_arg}" ;;
	esac
	_prev_arg="${_arg}"
done

# ---- Jobs runtime: take the prompt, metadata and key from the mounted files ---
# Fed into the SAME variables the Gas City fetch below fills from the prompt
# row, so the overlay render, the provider check and the run are one code path
# for both runtimes:
#   prompt         -> GONK_PROMPT                     (as .prompt did)
#   metadata.json  -> GC_WEBHOOK_ARG_METADATA_JSON    (as .metadata did; the
#                     x-litellm-spend-logs-metadata header, verbatim)
#   litellm-key    -> GONK_LITELLM_KEY_FILE           (the {file:...}
#                     indirection, pointed straight at the mount: the key bytes
#                     are never read by this shell, never copied to a second
#                     file, and never enter any process's environment)
#   GONK_MODEL     -> GC_WEBHOOK_ARG_MODEL            (as .model did; the Job
#                     env carries the meter's per-session decision)
# (Both files were read, and checked non-blank, in the session-file check at
# the top.) A mounted prompt WINS over any argv --prompt: the Job passes none,
# and a second source is how the two would drift apart unnoticed.
if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
	GONK_PROMPT="${GONK_MOUNTED_TEXT}"
	GC_WEBHOOK_ARG_METADATA_JSON="${GONK_MOUNTED_METADATA}"
	unset GONK_MOUNTED_TEXT GONK_MOUNTED_METADATA
	GONK_LITELLM_KEY_FILE="${GONK_SESSION_DIR}/litellm-key"
	if [ -n "${GONK_MODEL:-}" ]; then
		GC_WEBHOOK_ARG_MODEL="${GONK_MODEL}"
	fi
	log "prompt read from the session mount (${#GONK_PROMPT} bytes); using the mounted LiteLLM key file ${GONK_LITELLM_KEY_FILE}"
fi

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
if [ -z "${GONK_PROMPT}" ] && [ -n "${GONK_PROMPT_URL:-}" ] && [ -n "${GC_ALIAS:-}" ] &&
	[ "${GONK_SESSION_RUNTIME:-gascity}" != "jobs" ]; then
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

# THE SESSION IS RESIDENT, SO NONE OF THIS MAY BE FATAL AT STARTUP.
# Gas City's k8s provider launches the pod as a POOL session:
#     tmux new-session -d -s main "gonk-agent-entrypoint" && sleep infinity
# with NO prompt appended -- the prompt is delivered later, into the running
# tmux, when a bead is assigned. So at startup there is often no session-arg
# model yet, and an entrypoint that exits on a missing model takes the tmux
# session with it: the server dies, the reconciler sees runtime-missing, and
# the pod is reaped and respawned in a ~60s loop. (Observed exactly that.)
#
# Precedence, most specific first: GC_WEBHOOK_ARG_MODEL (per-session -- set by
# Step 1.5's prompt fetch above, or by any non-gascity caller) > GONK_MODEL,
# the STATIC per-install default the chart injects from the operator's
# default rung. Static is the honest v1 answer: this deployment has one local
# rung, and per-session model selection is what the v2 broker adds.
# WHICH SOURCE WON IS THE INTERESTING PART, not just the value: a session running
# the static per-install default rather than the meter's rung decision is a
# different situation, and it used to be indistinguishable in the log.
if [ -n "${GC_WEBHOOK_ARG_MODEL:-}" ]; then
	GONK_MODEL_SOURCE="session-arg"
	if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
		GONK_MODEL_SOURCE="session-env"
	fi
fi
: "${GC_WEBHOOK_ARG_MODEL:=${GONK_MODEL:-}}"
# Always defined from here on (possibly empty), same as GC_WEBHOOK_ARG_MODEL
# above -- the jq render below and the metadata-bytes log line both reference
# it unguarded under `set -u`.
: "${GC_WEBHOOK_ARG_METADATA_JSON:=}"
if [ -z "${GONK_MODEL_SOURCE:-}" ] && [ -n "${GC_WEBHOOK_ARG_MODEL}" ]; then
	GONK_MODEL_SOURCE="static-install-default"
	log "WARNING: model came from the static per-install default, NOT the meter's rung decision"
fi
if [ -n "${GC_WEBHOOK_ARG_METADATA_JSON:-}" ]; then
	log "attribution metadata present (${#GC_WEBHOOK_ARG_METADATA_JSON} bytes)"
fi

if [ -z "${GC_WEBHOOK_ARG_MODEL}" ]; then
	log "no model: no GC_WEBHOOK_ARG_MODEL, no GONK_MODEL."
	log "this is gonk-meter's rung decision and there is no built-in default -- refusing to start."
	log "session end: refused (no model)"
	exit 1
fi

# Attribution metadata is NOT fatal, deliberately. A resident pool session has no
# bead yet, so there is nothing per-bead to stamp until a prompt arrives; dying
# here would trade all attribution for no session at all. The header is simply
# omitted when empty, and that is LOUD in the log so a silent loss of the
# attribution seam (OD-7) cannot pass for normal.
if [ -z "${GC_WEBHOOK_ARG_METADATA_JSON:-}" ]; then
	log "WARNING: no attribution metadata (no GC_WEBHOOK_ARG_METADATA_JSON)"
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
	# config. GC_WEBHOOK_ARG_LITELLM_KEY is the per-session, per-project key
	# delivered with the prompt row (gonk-8gb): dispatch resolves the
	# project's virtual key and fails closed if it cannot, so this being
	# present means the meter vouched for it.
	#
	# THERE IS NO OTHER SOURCE. This used to fall back to a STATIC per-install
	# key env var (the one channel Gas City leaves open, since resolved.Env
	# comes from city/agent config) -- but that static value was the LiteLLM
	# proxy ADMIN key, so preferring it meant every session authenticated as
	# the admin and the per-project budget the meter had provisioned was
	# never consulted. Now that dispatch always resolves and fails closed on
	# the project key, the static fallback had no live caller left and is
	# deleted rather than kept as a silent downgrade path.
	_key="${GC_WEBHOOK_ARG_LITELLM_KEY:-}"
	if [ -n "${_key}" ]; then
		log "using this session's per-project LiteLLM key"
	else
		log "no LiteLLM key: set GONK_LITELLM_KEY_FILE or GC_WEBHOOK_ARG_LITELLM_KEY -- refusing to start unauthenticated"
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

# THE CHECKED-OUT REPOSITORY DOES NOT GET TO CONFIGURE THE AGENT (gonk-le3g).
# Step 0 above extracts an UNTRUSTED repository into RIG_DIR, and that is the
# directory opencode runs in (Dockerfile.agent's WORKDIR /workspace, and the
# exec at the bottom of this script). Without this flag, opencode walks up from
# its working directory and merges any opencode.json / opencode.jsonc it finds,
# plus every .opencode/ directory (config/paths.ts `directories`,
# config/config.ts, both guarded by exactly this flag at v1.18.3). Three things
# that buys a repository, all of them silent:
#
#   - agent.build.permission from the repo is merged AFTER the env-derived
#     ruleset (agent/agent.ts: `item.permission = Permission.merge(
#     item.permission, Permission.fromConfig(value.permission))`), and
#     evaluation is findLast -- so `{"agent":{"build":{"permission":
#     {"*":"allow"}}}}` re-enables every tool agent.toml's OPENCODE_PERMISSION
#     denied (gonk-7b79);
#   - a bare `permission` block re-orders ours even without overriding it,
#     because the env var is merged with `mergeDeep(result.permission, env)`
#     (config/config.ts) and mergeDeep keeps the DESTINATION's key positions --
#     a repo naming "bash" before "*" moves our catch-all after it;
#   - each discovered .opencode/ directory gets an `npm install` and its
#     plugins loaded, which is repo-controlled CODE, not just config.
#
# THIS DOES NOT TOUCH THE OVERLAY. OPENCODE_CONFIG is loaded on its own branch
# (config/config.ts `if (Flag.OPENCODE_CONFIG)`), unguarded by this flag, so the
# LiteLLM provider, the model and the attribution header rendered above are
# unaffected -- the provider assertion below still runs against them.
#
# WHAT IT DOES COST, stated so nobody rediscovers it as a bug: opencode also
# stops auto-loading the checkout's AGENTS.md / CLAUDE.md as ambient
# instructions (session/instruction.ts, core/instruction-context.ts, same flag).
# gonk does not rely on that. Nothing in this repo writes an opencode config
# into a checkout (no opencode.json, opencode.jsonc or .opencode/ exists
# anywhere in gonk, and nothing generates one), and the only agent that wants
# repository context gets it EXPLICITLY: cmd/gonk-gate/broker_inject.go probes a
# fixed file list that already includes CLAUDE.md and AGENTS.md and splices them
# into the prompt under maxRepoFileBytes/maxRepoContextBytes caps. Ambient
# discovery would have handed the same untrusted text to the model UNCAPPED and
# ahead of the system prompt, so losing it is a second, smaller win.
#
# ONE THING THIS FLAG DOES NOT COVER, named so it is a recorded gap and not an
# oversight: config/paths.ts ALSO scans "$HOME/.opencode", on a branch the flag
# does not guard, and Dockerfile.agent's own `ENV HOME=/workspace` is the same
# directory the checkout lands in. In production that is harmless -- every agent
# in pack/agents/ overrides HOME to /tmp/gonk-home in its [env] block, for the
# unrelated reason documented there -- but the two facts are only accidentally
# independent. If an agent ever ships without that override, the checkout's
# .opencode/ is back, flag or no flag. The HOME override is load-bearing for
# this guard too.
export OPENCODE_DISABLE_PROJECT_CONFIG=1

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

# ---- The loop watchdog (gonk-un7z; jobs mode only) ---------------------------
# The awk line filter opencode's output goes through. For EVERY line it first
# prints the line, unchanged, and flushes -- that is the pod log, and the
# GONK_BATCH fence the sweep reads out of it -- and only then looks at it.
#
# ONE printf PER LINE, not `print`: mawk -W interactive writes unbuffered, and
# `print` can reach stdout as two writes (the line, then the newline).
#
# THE SIGNATURE RULE. opencode `run` prints each tool call on one line, a glyph
# then the call, coloured with ANSI codes (opencode 1.18.3, as logged on rom#1):
#     ESC[0m<U+2731> ESC[0mGlob "**/*.go"ESC[90m 9 matchesESC[0m          (ran)
#     ESC[0m<U+2192> ESC[0mRead internal/widget/handlers.goESC[90m [limit=2000]ESC[0m
#     ESC[0m<U+2717> ESC[0mRead internal/widget/adapter.go failedESC[90m [limit=2000]ESC[0m
#     ESC[91mESC[1mError: ESC[0mFile not found: /workspace/internal/widget/adapter.go
# A FAILED call is a line that, with every ANSI CSI sequence removed and
# surrounding blanks trimmed, starts with U+2717 (the bytes \342\234\227) and a
# space. Its signature is the rest of that line with opencode's " failed"
# marker taken out: the tool name and its arguments, e.g.
#     Read internal/widget/adapter.go [limit=2000]
# Only those lines count -- a call that RAN (any other glyph) never does,
# however often it repeats, and neither does the "Error:" line. Counts are per
# signature for the whole session. The count that reaches `threshold` writes
# "<count>\n<signature>" to the file `fired` and SIGKILLs process group `pgid`
# (opencode's -- see below); after that the filter only passes lines through.
#
# THE "Invalid Tool" RULE (gonk-mf3v). A call whose arguments are not JSON
# never reaches a tool, so opencode has no arguments to print: EVERY such call
# is the same bare line, and what was called is on the line AFTER it (opencode
# 1.18.3, as logged by the k3d component test and on project 75, 2026-10-04):
#     ESC[0m<U+2717> ESC[0mInvalid Tool
#     The arguments provided to the tool are invalid: Invalid input for tool glob: JSON parsing failed: Text: {"pattern": "README*"}}.
#     Error message: JSON Parse error: Unable to parse JSON string
# Counted by the first line alone, two DIFFERENT malformed calls are one call
# failing twice -- which stopped a live session that had not looped. So a
# failed line whose signature is exactly "Invalid Tool" is not counted when it
# is seen: the NEXT line (ANSI stripped, trimmed) is its detail, and the
# signature is "Invalid Tool: <detail>" -- the tool and the argument text. The
# identical malformed call twice still has one signature and still stops the
# session. An "Invalid Tool" line with NO detail after it (an empty line, or
# another failed call straight away) cannot be told from any other and is NOT
# counted: this guard fails open, and the component test below is what notices
# if opencode ever stops printing the detail.
#
# PINNED TO THE REAL FORMAT by the k3d component tests
# (cmd/gonk-gate/jobs_component_test.go: the looping stub model, and the stub
# model whose tool-call arguments are malformed): an opencode upgrade that
# changes the glyph or the layout disarms this filter silently, and those
# tests are what go red when it does.
#
# Byte semantics (the caller sets LC_ALL=C) so mawk and gawk agree on the
# octal escapes; ASCII only in this file, and no apostrophe in the program
# (it is one single-quoted shell string).
GONK_LOOP_WATCHDOG_AWK='
function failed_call(sig,    n) {
	n = ++count[sig]
	if (n < threshold + 0) return
	stopped = 1
	printf "%d\n%s\n", n, substr(sig, 1, 400) > fired
	close(fired)
	system("kill -KILL " target " 2>/dev/null")
}
{
	printf "%s\n", $0
	fflush()
	if (stopped) next
	line = $0
	gsub(/\033\[[0-9;?]*[A-Za-z]/, "", line)
	sub(/^[ \t\r]+/, "", line)
	sub(/[ \t\r]+$/, "", line)
	if (pending) {
		pending = 0
		if (line != "" && line !~ /^\342\234\227 /) {
			failed_call("Invalid Tool: " line)
			next
		}
	}
	if (line !~ /^\342\234\227 /) next
	sig = line
	sub(/^\342\234\227 +/, "", sig)
	if (!sub(/ failed \[/, " [", sig)) sub(/ failed$/, "", sig)
	if (sig == "") next
	if (sig == "Invalid Tool") {
		pending = 1
		next
	}
	failed_call(sig)
}'

# gonk_run_opencode_watched runs `opencode run -- "$1"` under the loop
# watchdog and sets _rc to opencode's exit status. When the watchdog fired it
# also sets _loop_fired=1, _loop_count, _loop_call and _loop_threshold.
#
# WHY A FIFO AND NOT A PIPELINE. `opencode run | filter` would make the
# pipeline's status the FILTER's, and this image's /bin/sh is dash, which has
# no portable pipefail to get opencode's back. So opencode writes into a FIFO
# in the background, the filter reads it in the background, and this shell
# `wait`s on each by pid: opencode's rc is its own `wait` status, unchanged
# when the watchdog does not fire (stdout and stderr are merged into the one
# stream the pod log already was; the bytes are unchanged).
#
# WHY setsid. The kill has to reach opencode's CHILDREN (its tool processes),
# not just opencode. setsid makes opencode the leader of a new process group
# whose id is its own pid -- and since a background job of a non-interactive
# shell is never a group leader, setsid execs in place rather than forking, so
# $! IS that pid. One `kill -KILL -<pid>` then reaches the whole group.
#
# NO ZOMBIES, NO HANG. Both background jobs are reaped by `wait`. After
# opencode exits -- killed or not -- whatever it left in its group is killed
# too (as PID 1 exiting would have done before this filter existed), so no
# leftover child can hold the FIFO open and keep the filter, and with it the
# Job, waiting for an EOF that never comes.
#
# A watchdog that cannot be set up is LOUD and NOT fatal: opencode runs
# unwatched, as before gonk-un7z, with a WARNING saying a looping session will
# not be stopped. A guard must never be the reason a session fails to run.
gonk_run_opencode_watched() {
	_loop_threshold="${GONK_LOOP_WATCHDOG_THRESHOLD:-${GONK_LOOP_WATCHDOG_DEFAULT_THRESHOLD}}"
	case "${_loop_threshold}" in
	'' | *[!0-9]* | 0*)
		log "WARNING: GONK_LOOP_WATCHDOG_THRESHOLD=${_loop_threshold} is not a positive integer; using ${GONK_LOOP_WATCHDOG_DEFAULT_THRESHOLD}"
		_loop_threshold="${GONK_LOOP_WATCHDOG_DEFAULT_THRESHOLD}"
		;;
	esac
	# mawk block-buffers a pipe's INPUT unless -W interactive: it would not see
	# a failing call until 4 KiB more output arrived -- or opencode exited,
	# which is exactly too late. gawk reads lines as they come.
	if command -v mawk >/dev/null 2>&1; then
		_wd_awk="mawk -W interactive"
	else
		_wd_awk="awk"
	fi
	_wd_dir="${GONK_RUNTIME_DIR:-/tmp/gonk}/loop-watchdog"
	if ! { rm -rf "${_wd_dir}" && mkdir -p "${_wd_dir}" && mkfifo "${_wd_dir}/out"; } 2>/dev/null; then
		log "WARNING: loop watchdog: cannot create its FIFO in ${_wd_dir}; running opencode UNWATCHED -- a session looping on a failing tool call will NOT be stopped"
		opencode run -- "$1" || _rc=$?
		return 0
	fi
	log "loop watchdog: armed (stops the session when the same tool call fails ${_loop_threshold} times)"

	setsid opencode run -- "$1" </dev/null >"${_wd_dir}/out" 2>&1 &
	_wd_oc=$!
	# The kill target: opencode's whole process group (see WHY setsid).
	_wd_target="-${_wd_oc}"
	LC_ALL=C ${_wd_awk} -v threshold="${_loop_threshold}" -v target="${_wd_target}" \
		-v fired="${_wd_dir}/fired" "${GONK_LOOP_WATCHDOG_AWK}" <"${_wd_dir}/out" &
	_wd_filter=$!

	# 2>/dev/null: dash announces a signalled background job ("Killed") on
	# stderr. The rc below (137) says it, in the session-end line.
	wait "${_wd_oc}" 2>/dev/null || _rc=$?
	kill -KILL "${_wd_target}" 2>/dev/null || true
	wait "${_wd_filter}" || true

	if [ -s "${_wd_dir}/fired" ]; then
		_loop_fired=1
		_loop_count=$(sed -n 1p "${_wd_dir}/fired")
		_loop_call=$(sed -n 2p "${_wd_dir}/fired")
	fi
	rm -rf "${_wd_dir}"
	unset _wd_awk _wd_dir _wd_oc _wd_target _wd_filter
}

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
	#
	# Not under the Jobs runtime: there is no tmux, and the transcript is the
	# pod log, which does not wrap.
	if [ "${GONK_SESSION_RUNTIME:-gascity}" != "jobs" ]; then
		tmux set-option -g window-size manual 2>/dev/null || true
		tmux resize-window -t main -x 4000 -y 200 2>/dev/null || true
		log "pane width now: $(tmux display -p '#{pane_width}' 2>/dev/null || echo unknown)"
	fi

	log "starting opencode run (non-interactive)"
	# `|| _rc=$?` IS LOAD-BEARING, NOT STYLE. set -e is in force (top of file),
	# so a bare `opencode run` that exits non-zero kills this shell immediately:
	# the session-end line below never prints AND the tmux hold never runs, so
	# tmux dies and the transcript is unreadable -- reintroducing gonk-2tb on
	# exactly the failed turns an operator most needs to read.
	_rc=0
	_loop_fired=""
	if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
		# Jobs only: under the loop watchdog (gonk-un7z; sets _rc and, when it
		# fired, _loop_fired/_loop_count/_loop_call/_loop_threshold).
		gonk_run_opencode_watched "${_clean}"
	else
		opencode run -- "${_clean}" || _rc=$?
	fi

	# THE HOLD IS A GAS CITY REQUIREMENT, AND ONLY A GAS CITY REQUIREMENT.
	#
	# Under Gas City the transcript is read back out of a live tmux pane, so the
	# process had to stay alive after `opencode run` returned or the pane died
	# and the transcript became unreadable (gonk-2tb). Under the Jobs runtime
	# (ADR-008) the transcript is the POD LOG, which outlives the process, and
	# the hold is actively harmful: the container never exits, so the Job never
	# reaches status.succeeded and is eventually killed by activeDeadlineSeconds
	# and reported as DeadlineExceeded -- a clean session indistinguishable from
	# an overrun one. Observed directly in the T-55 gate run; see
	# docs/spikes/adr-008-t55-gate/README.md.
	#
	# So the hold is now conditional, and the DEFAULT IS STILL THE HOLD: the Gas
	# City path must not change behaviour until T-56 deletes it. The Jobs runtime
	# sets GONK_SESSION_RUNTIME=jobs and gets the exit code instead.
	log "session end: opencode run exited rc=${_rc}"
	if [ "${GONK_SESSION_RUNTIME:-gascity}" = "jobs" ]; then
		if [ -n "${_loop_fired}" ]; then
			# THE LINE THE SWEEP QUOTES as the cause (cmd/gonk-gate
			# sweep_loop.go): keep its shape -- "failed <n> times (threshold
			# <t>): <call>", with the call LAST so it can hold anything.
			log "loop watchdog: stopped opencode -- the same tool call failed ${_loop_count} times (threshold ${_loop_threshold}): ${_loop_call}"
			log "session end: runtime=jobs -- exiting ${GONK_EXIT_LOOP_WATCHDOG} (loop watchdog) so the Job reports a terminal status"
			exit "${GONK_EXIT_LOOP_WATCHDOG}"
		fi
		log "session end: runtime=jobs -- exiting ${_rc} so the Job reports a terminal status"
		exit "${_rc}"
	fi
	log "session end: runtime=gascity -- holding so tmux stays alive for the transcript read (gonk-2tb)"
	# `wait` would return immediately (no background jobs); sleep in a loop is
	# the portable hold. The sweep closes the session when it has judged.
	while :; do sleep 3600; done
fi

# No prompt: nothing to run non-interactively. Fall back to whatever we were
# given, unchanged -- the entrypoint refuses to start without a model anyway.
exec opencode "$@"
