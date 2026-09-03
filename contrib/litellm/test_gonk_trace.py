"""Tests for the trajectory collector (gonk-p8j).

These cover the two properties that matter most and are easiest to lose in a
later edit: that no free text can reach the ledger through the target field, and
that non-gonk traffic on this shared proxy is ignored entirely.
"""

import json

import pytest

from gonk_trace import (
    build_report,
    extract_calls,
    gonk_identity,
    normalise_target,
)


class Obj:
    """Minimal attribute-shaped stand-in for a LiteLLM response object."""

    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


def _md(**kw):
    base = {"gonk_session_key": "gonk-75-issue-44", "gonk_attempt": "1"}
    base.update(kw)
    return {"litellm_params": {"metadata": {"spend_logs_metadata": base}}}


# --- normalise_target: the redaction boundary ------------------------------


def test_extracts_a_path_from_json_string_arguments():
    assert normalise_target('{"file_path": "internal/paging/paging.go"}') == "internal/paging/paging.go"


def test_extracts_a_path_from_dict_arguments():
    assert normalise_target({"path": "README.md"}) == "README.md"


@pytest.mark.parametrize(
    "arguments",
    [
        # A grep pattern lifted from an issue body is free text, not a path, and
        # must never be stored. Whitespace is the signal.
        {"path": "the user said hello"},
        {"file_path": "line one\nline two"},
        # Unknown keys are dropped rather than stored: the key list is closed.
        {"pattern": "TODO"},
        {"query": "why is this broken"},
        # Over-long values are refused outright.
        {"path": "a/" + "b" * 300},
        # Non-dict, unparseable, and wrong-typed arguments yield nothing.
        "not json at all",
        "[1,2,3]",
        None,
        {"path": 42},
        {},
    ],
)
def test_refuses_anything_that_is_not_clearly_a_path(arguments):
    assert normalise_target(arguments) == ""


def test_prefers_the_most_specific_key():
    assert normalise_target({"path": "b.go", "file_path": "a.go"}) == "a.go"


# --- extract_calls ---------------------------------------------------------


def test_extracts_tool_names_and_targets():
    resp = Obj(choices=[Obj(message=Obj(tool_calls=[
        Obj(function=Obj(name="read", arguments='{"file_path": "a.go"}')),
        Obj(function=Obj(name="grep", arguments='{"pattern": "TODO"}')),
    ]))])
    assert extract_calls(resp) == [
        {"tool": "read", "target": "a.go"},
        # The grep pattern is deliberately NOT stored.
        {"tool": "grep", "target": ""},
    ]


def test_handles_dict_shaped_responses_too():
    resp = {"choices": [{"message": {"tool_calls": [
        {"function": {"name": "read", "arguments": {"path": "x.go"}}}
    ]}}]}
    assert extract_calls(resp) == [{"tool": "read", "target": "x.go"}]


def test_a_call_with_no_name_is_not_evidence():
    resp = Obj(choices=[Obj(message=Obj(tool_calls=[
        Obj(function=Obj(name="", arguments="{}")),
        Obj(function=Obj(name=None, arguments="{}")),
    ]))])
    assert extract_calls(resp) == []


@pytest.mark.parametrize("resp", [None, Obj(), Obj(choices=None), Obj(choices=[]), {}, "nonsense"])
def test_unrecognised_shapes_yield_no_calls_rather_than_raising(resp):
    assert extract_calls(resp) == []


# --- identity: this is a SHARED proxy --------------------------------------


def test_ignores_traffic_that_is_not_gonk():
    assert gonk_identity({}) is None
    assert gonk_identity({"litellm_params": {"metadata": {}}}) is None
    assert gonk_identity({"litellm_params": {"metadata": {"spend_logs_metadata": {"other": "x"}}}}) is None


def test_declines_a_request_with_no_usable_attempt():
    # Attempt is part of the trace identity; without one we would merge evidence
    # across runs that must stay separate, so we decline rather than guess.
    for bad in ("0", "-1", "not-a-number", None):
        md = _md(gonk_attempt=bad)
        assert gonk_identity(md) is None


def test_reads_metadata_from_either_location():
    top_level = {"metadata": {"spend_logs_metadata": {"gonk_session_key": "s", "gonk_attempt": "2"}}}
    assert gonk_identity(top_level) == ("s", 2, "", "")


# --- build_report ----------------------------------------------------------


def test_builds_a_report_with_completeness_and_one_turn():
    resp = Obj(choices=[Obj(message=Obj(tool_calls=[
        Obj(function=Obj(name="read", arguments='{"path": "a.go"}'))
    ]))])
    report = build_report(_md(gonk_bead_id="gonk:75:issue:44", gonk_project="g/p"), resp)
    assert report == {
        "session_key": "gonk-75-issue-44",
        "attempt": 1,
        "bead_id": "gonk:75:issue:44",
        "project": "g/p",
        "completeness": "complete",
        "calls": [{"tool": "read", "target": "a.go"}],
        "turns": 1,
    }


def test_no_report_for_non_gonk_traffic():
    assert build_report({}, Obj(choices=[])) is None


def test_a_turn_with_no_tool_calls_is_still_a_report():
    # "The model called nothing this turn" is an observation and must be
    # recorded. Dropping it would make an observed-idle turn look unobserved.
    report = build_report(_md(), Obj(choices=[Obj(message=Obj(tool_calls=None))]))
    assert report is not None
    assert report["calls"] == []
    assert report["turns"] == 1


def test_report_body_is_json_serialisable():
    report = build_report(_md(), Obj(choices=[Obj(message=Obj(tool_calls=[
        Obj(function=Obj(name="read", arguments='{"path": "a.go"}'))
    ]))]))
    json.dumps(report)
