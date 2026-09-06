"""Tests for the streamed tool-call repairs (openclaw-e3mb, gonk-217).

This hook runs on EVERY streamed completion on a shared proxy, so the tests
concentrate on what must not change: ordinary text must pass through untouched,
and a repair must never invent something that was not there.
"""

import asyncio
import importlib.util
import os

import pytest

spec = importlib.util.spec_from_file_location(
    "custom_callbacks", os.path.join(os.path.dirname(__file__), "custom_callbacks.py")
)
cc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cc)


class O:
    def __init__(self, **kw):
        for k, v in kw.items():
            setattr(self, k, v)


def fn(name=None, arguments=""):
    f = O(arguments=arguments)
    if name is not None:
        f.name = name
    return f


def chunk(*tool_calls, finish=None, index=0):
    delta = O(tool_calls=list(tool_calls) if tool_calls else None)
    return O(choices=[O(index=index, delta=delta, finish_reason=finish)])


def run(chunks):
    async def gen():
        for c in chunks:
            yield c

    async def collect():
        return [c async for c in cc.proxy_handler_instance.async_post_call_streaming_iterator_hook(
            None, gen(), {}
        )]

    return asyncio.run(collect())


# --- gonk-217: the dropped name -------------------------------------------


def test_name_is_restored_on_continuation_deltas():
    out = run([
        chunk(O(index=0, function=fn("list_files", ""))),
        chunk(O(index=0, function=fn(None, '{"path": '))),
        chunk(O(index=0, function=fn(None, '"."}'))),
    ])
    names = [c.choices[0].delta.tool_calls[0].function.name for c in out]
    assert names == ["list_files", "list_files", "list_files"], names


def test_parallel_tool_calls_do_not_borrow_each_others_names():
    out = run([
        chunk(O(index=0, function=fn("alpha", "")), O(index=1, function=fn("beta", ""))),
        chunk(O(index=0, function=fn(None, "a")), O(index=1, function=fn(None, "b"))),
    ])
    second = out[1].choices[0].delta.tool_calls
    assert second[0].function.name == "alpha"
    assert second[1].function.name == "beta"


def test_a_name_that_is_present_is_never_overwritten():
    # Self-deactivating: once upstream ships the fix, every delta carries a
    # name and this repair must leave all of them exactly as they are.
    out = run([
        chunk(O(index=0, function=fn("first", ""))),
        chunk(O(index=0, function=fn("first", "args"))),
    ])
    assert [c.choices[0].delta.tool_calls[0].function.name for c in out] == ["first", "first"]


def test_no_name_is_invented_when_none_was_ever_seen():
    # If the FIRST delta had no name either, there is nothing to restore, and
    # guessing would be worse than passing the defect through.
    out = run([chunk(O(index=0, function=fn(None, "args")))])
    assert getattr(out[0].choices[0].delta.tool_calls[0].function, "name", None) is None


# --- openclaw-e3mb: the existing repair must still work --------------------


def test_finish_reason_is_still_repaired_after_a_tool_call():
    out = run([
        chunk(O(index=0, function=fn("list_files", ""))),
        chunk(finish="stop"),
    ])
    assert out[1].choices[0].finish_reason == "tool_calls"


def test_ordinary_text_completions_are_untouched():
    out = run([chunk(finish=None), chunk(finish="stop")])
    assert out[1].choices[0].finish_reason == "stop"


# --- it must never break the stream ---------------------------------------


@pytest.mark.parametrize("weird", [
    O(),
    O(choices=None),
    O(choices=[]),
    O(choices=[O()]),
    O(choices=[O(index=0, delta=None, finish_reason=None)]),
    O(choices=[O(index=0, delta=O(tool_calls=[O()]), finish_reason=None)]),
])
def test_unexpected_chunk_shapes_pass_through_rather_than_raising(weird):
    out = run([weird])
    assert out == [weird]
