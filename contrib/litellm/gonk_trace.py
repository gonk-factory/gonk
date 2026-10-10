"""Trajectory-evidence collector for the LiteLLM proxy (gonk-p8j).

WHAT THIS IS FOR
gonk's triage agent can reach a verdict -- including "close this issue" -- whose
entire gate is four syntactic checks over the emitted batch. None of them read
the work. An agent that greps nothing and confabulates a plausible diagnosis
passes all four, and because a reply-only verdict produces no artifact, no
downstream experiment can catch it either. This collector records what a session
ACTUALLY ASKED THE MODEL TO DO, so a later predicate can require evidence of a
read before such a verdict is honoured.

WHY IT LIVES IN THE PROXY AND NOT IN THE POD
The agent pod's session storage sits under a writable path and issue text can
obtain a shell there (gonk-e9m). A trace the agent can edit is not evidence, it
is a field the agent fills in. The proxy is on the other side of that boundary:
the pod cannot reach it, cannot see it, and cannot alter what it recorded.

ITS HONEST LIMIT, which belongs here and not in a footnote: the proxy observes
what the MODEL ASKED FOR, not what the HARNESS DID. A tool call opencode
refused, retried or truncated looks identical here to one that ran. For the
predicate this is built for -- catching a model that never even asked to read --
that is sufficient. A predicate that assumes execution would be wrong about a
denied call, and must not be written on top of this without saying so.

WHAT IT MUST NEVER DO
- Never store prompt or response text. Tool names and a normalised argument
  shape only: bodies carry untrusted issue content and, on cloud rungs, left our
  premises to begin with.
- Never raise into the proxy. This runs on a shared service; an exception here
  must never cost somebody else's completion. Every entry point swallows.
- Never touch non-gonk traffic. Requests without gonk metadata are ignored
  entirely.
"""

from __future__ import annotations

import json
from typing import Any, Dict, Iterable, List, Optional, Tuple

# Argument keys worth normalising into a Target, in preference order. The list is
# deliberately SHORT and closed: anything not named here is dropped rather than
# stored, which is what keeps this from becoming a transcript store by accident.
_TARGET_KEYS = ("file_path", "filePath", "path", "notebook_path", "target_file")

# A normalised target is a path, and a path is not prose. These bounds exist so
# that a tool argument carrying free text -- a grep pattern taken from issue
# body, say -- cannot be smuggled into the ledger through this field.
_MAX_TARGET_LEN = 256


def normalise_target(arguments: Any) -> str:
    """Reduce a tool's arguments to a single normalised path, or "".

    Returns "" whenever there is no clearly path-shaped value, which is the safe
    outcome: a missing target costs a predicate some precision, while a stored
    blob costs us the property that the ledger holds no free text.
    """
    args = arguments
    if isinstance(args, str):
        try:
            args = json.loads(args)
        except (ValueError, TypeError):
            return ""
    if not isinstance(args, dict):
        return ""

    for key in _TARGET_KEYS:
        value = args.get(key)
        if not isinstance(value, str):
            continue
        cleaned = value.strip()
        if not cleaned or len(cleaned) > _MAX_TARGET_LEN:
            continue
        # Reject anything that reads as prose rather than a path. Newlines and
        # internal whitespace are the giveaway, and both appear in free-text
        # arguments far more often than in real paths.
        if any(ch.isspace() for ch in cleaned):
            continue
        return cleaned
    return ""


def extract_calls(response_obj: Any) -> List[Dict[str, str]]:
    """Pull {tool, target} pairs out of a completion response.

    Tolerates both object-shaped and dict-shaped responses because LiteLLM hands
    back a ModelResponse for non-streaming and an assembled object for streaming,
    and the exact type has changed across versions. Anything unrecognised yields
    no calls rather than an exception.
    """
    calls: List[Dict[str, str]] = []
    for choice in _iter_choices(response_obj):
        message = _get(choice, "message") or _get(choice, "delta")
        if message is None:
            continue
        for tc in _as_iter(_get(message, "tool_calls")):
            fn = _get(tc, "function")
            if fn is None:
                continue
            name = _get(fn, "name")
            if not isinstance(name, str) or not name:
                # A call with no name is not evidence of anything, and counting
                # it would inflate the totals a predicate reasons over.
                continue
            calls.append({"tool": name, "target": normalise_target(_get(fn, "arguments"))})
    return calls


def gonk_identity(kwargs: Dict[str, Any]) -> Optional[Tuple[str, int, str, str]]:
    """Return (session_key, attempt, bead_id, project) for a gonk request.

    Returns None for anything that is not gonk traffic. This proxy is shared, and
    silently ignoring everybody else's requests is a requirement, not a nicety.
    """
    md = _spend_logs_metadata(kwargs)
    if not md:
        return None
    session_key = md.get("gonk_session_key")
    if not isinstance(session_key, str) or not session_key:
        return None
    try:
        attempt = int(md.get("gonk_attempt", 0))
    except (TypeError, ValueError):
        attempt = 0
    if attempt <= 0:
        # Attempt is part of the trace identity. Without a usable one we would
        # have to merge evidence across runs that must stay separate, so decline.
        return None
    return (
        session_key,
        attempt,
        str(md.get("gonk_bead_id") or ""),
        str(md.get("gonk_project") or ""),
    )


def build_report(kwargs: Dict[str, Any], response_obj: Any) -> Optional[Dict[str, Any]]:
    """Build one POST body for the meter, or None if this is not gonk traffic.

    Completeness is reported per-report and is always "complete" here, meaning
    "this turn was observed in full". It is NOT a claim about the whole session:
    the meter folds reports together and degrades, so a session with a dropped
    turn ends up partial without this function needing to know that.
    """
    identity = gonk_identity(kwargs)
    if identity is None:
        return None
    session_key, attempt, bead_id, project = identity
    return {
        "session_key": session_key,
        "attempt": attempt,
        "bead_id": bead_id,
        "project": project,
        "completeness": "complete",
        "calls": extract_calls(response_obj),
        "turns": 1,
    }


def _spend_logs_metadata(kwargs: Dict[str, Any]) -> Dict[str, Any]:
    """Dig out the metadata gonk set via x-litellm-spend-logs-metadata.

    Checks both litellm_params.metadata and a top-level metadata, because which
    one carries it has moved between LiteLLM versions and a collector that only
    knew one would silently stop collecting after an upgrade.
    """
    for container in (
        (kwargs.get("litellm_params") or {}).get("metadata"),
        kwargs.get("metadata"),
    ):
        if not isinstance(container, dict):
            continue
        md = container.get("spend_logs_metadata")
        if isinstance(md, dict) and md:
            return md
    return {}


def _iter_choices(response_obj: Any) -> Iterable[Any]:
    choices = _get(response_obj, "choices")
    return _as_iter(choices)


def _as_iter(v: Any) -> Iterable[Any]:
    if v is None:
        return ()
    if isinstance(v, (list, tuple)):
        return v
    return ()


def _get(obj: Any, name: str) -> Any:
    """Attribute or key lookup, whichever the object supports."""
    if obj is None:
        return None
    if isinstance(obj, dict):
        return obj.get(name)
    return getattr(obj, name, None)


# --------------------------------------------------------------------------
# The proxy-side shipper.
#
# Kept BELOW the pure functions above and deliberately separate from them: the
# logic is unit-tested without a proxy, a network or LiteLLM installed, and this
# part is the thin, untestable-in-CI edge. Keeping the boundary sharp is what
# lets the interesting half be covered at all.
#
# REGISTERED AS ITS OWN CALLBACK, not folded into the existing
# ToolCallFinishReasonFixer. That hook repairs streamed tool calls for every
# consumer of this proxy; a defect in trajectory collection must not be able to
# take it down with it.
# --------------------------------------------------------------------------

import os  # noqa: E402  (import placement is deliberate: edge code, below the core)

_METER_URL_ENV = "GONK_METER_URL"
_METER_TOKEN_ENV = "GONK_METER_TOKEN"  # noqa: S105 -- an env var NAME, not a secret
_TRACE_PATH = "/v1/trace"

# Short by design. This runs after a completion has already been served, so the
# user is not waiting on it -- but the proxy's event loop is shared, and a
# collector that hangs is a collector that degrades everybody's throughput.
_TIMEOUT_SECONDS = 5.0


# LiteLLM registers callbacks as CustomLogger instances, and the existing
# finish_reason repair on this proxy subclasses it. A plain class risks being
# ignored or rejected at registration, which would leave this collector silently
# dead -- the worst outcome, because the evidence table would simply stay empty
# and look like agents that never call tools.
#
# The fallback keeps the module importable WITHOUT litellm installed, which is
# what lets the pure logic above be unit-tested in CI at all.
try:  # pragma: no cover - exercised only inside the proxy image
    from litellm.integrations.custom_logger import CustomLogger as _CustomLogger
except Exception:  # pragma: no cover - the standalone/test path

    class _CustomLogger:  # type: ignore[no-redef]
        pass


class GonkTrajectoryCollector(_CustomLogger):
    """Reports observed tool calls for gonk sessions to the gonk meter.

    Silent by construction. If the meter is unset, unreachable, or refuses the
    report, the completion has already been served and nothing about it changes;
    the evidence is simply missing, and the meter's completeness folding is what
    turns missing evidence into a trace a predicate will not act on. That is the
    correct failure mode: LOSING EVIDENCE MUST DEGRADE THE VERDICT, NEVER THE
    SERVICE.
    """

    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):  # noqa: D102
        try:
            report = build_report(kwargs or {}, response_obj)
            if report is None:
                return  # not gonk traffic; this proxy is shared
            base = os.environ.get(_METER_URL_ENV, "").rstrip("/")
            token = os.environ.get(_METER_TOKEN_ENV, "")
            if not base or not token:
                return
            if not report["calls"] and report["turns"] == 0:
                return
            import httpx  # LiteLLM already depends on it; imported late so an

            # environment without it cannot break proxy startup.
            async with httpx.AsyncClient(timeout=_TIMEOUT_SECONDS) as client:
                await client.post(
                    base + _TRACE_PATH,
                    json=report,
                    headers={"Authorization": "Bearer " + token},
                )
        except Exception:
            # NEVER raise into the proxy. This is a shared service and an
            # exception here would cost somebody else's request for the sake of
            # our telemetry.
            pass


proxy_handler_instance = GonkTrajectoryCollector()
