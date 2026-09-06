"""Repair finish_reason on streamed tool calls (openclaw-e3mb).

THE DEFECT
LiteLLM's ollama_chat streaming path emits a well-formed tool_calls delta and
then closes the stream with finish_reason="stop" instead of "tool_calls".
Clients that gate tool execution on finish_reason -- OpenClaw does -- discard
the accumulated call and end the turn with whatever text preceded it. The
model looks like it "narrates instead of calling tools" when in fact it
emitted a correct call that never arrived.

VERIFIED ON THIS PROXY, 2026-07-31. Identical request, only `stream` varied:
    stream=false -> finish_reason="tool_calls", tool_calls=[correct call]
    stream=true  -> chunk0: delta.tool_calls=[{name:..., arguments:...}]
                    chunk2: finish_reason="stop", delta={}
Reproduced at both a 288-token and a 49,913-token prompt, so it is not
prompt-size dependent. Sibling upstream report: BerriAI/litellm#34692 (that
one is the anthropic-messages variant; this is the openai-completions path,
which the earlier transport switch was believed to avoid -- it does not).

WHY A HOOK AND NOT A CONFIG FLAG
This LiteLLM build has no generic per-model fake_stream/supports_native_
streaming knob (the only hits are Azure/Responses transforms and an internal
agentic-loop helper), and OpenClaw sends stream:true regardless of its own
model-scope `streaming: false` setting -- confirmed by capturing the inbound
request. So the repair has to happen here.

SAFETY
Deliberately conservative. It rewrites "stop" -> "tool_calls" ONLY on a choice
index where a tool_calls delta was actually observed earlier in the same
stream, so ordinary text completions are untouched. It never raises: any
unexpected chunk shape passes through unmodified, because an exception in this
hook would break every streamed completion on the proxy.

REMOVE once the upstream defect is fixed and this LiteLLM is upgraded past it.
Rollback is dropping the `callbacks` line from litellm_settings.
"""

from typing import Any, AsyncGenerator, Set

from litellm.integrations.custom_logger import CustomLogger


class ToolCallFinishReasonFixer(CustomLogger):
    # NOTE: this method must be defined directly on the class, not inherited.
    # The proxy gates the hook on `"async_post_call_streaming_iterator_hook"
    # in type(cb).__dict__` (proxy/common_request_processing.py), so an
    # inherited implementation would be silently skipped.
    async def async_post_call_streaming_iterator_hook(
        self,
        user_api_key_dict: Any,
        response: Any,
        request_data: dict,
    ) -> AsyncGenerator[Any, None]:
        saw_tool_call: Set[int] = set()
        # SECOND DEFECT, SAME STREAM (gonk-217). LiteLLM drops function.name
        # from CONTINUATION tool-call deltas: the upstream emits it on every
        # chunk and only the first survives the proxy. Measured 2026-09-06,
        # identical model and request, from inside this pod:
        #     direct to vLLM  -> 4 of 4 deltas carry name
        #     through LiteLLM -> 1 of 5 carries name
        # Clients that validate each delta against the OpenAI tool-call schema
        # then abort the turn: opencode's @ai-sdk/openai-compatible throws
        # "Expected 'function.name' to be a string", and gonk's agent dies a few
        # file reads in.
        #
        # Fixed upstream but not in any tagged release yet -- v1.99.1 is the
        # newest published and still exhibits it -- so it is repaired here,
        # beside the finish_reason repair, being the same class of bug in the
        # same hook.
        #
        # SELF-DEACTIVATING: it only FILLS IN a name that is MISSING. Once the
        # upstream fix ships, every delta already carries one and this becomes a
        # no-op, so retiring it needs no second change.
        names: dict = {}
        async for chunk in response:
            try:
                for choice in getattr(chunk, "choices", None) or []:
                    index = getattr(choice, "index", 0) or 0
                    delta = getattr(choice, "delta", None)
                    if delta is not None and getattr(delta, "tool_calls", None):
                        saw_tool_call.add(index)
                        drop = []
                        for tc in delta.tool_calls or []:
                            fn = getattr(tc, "function", None)
                            if fn is None:
                                continue
                            # Keyed on (choice, tool index) so parallel tool
                            # calls in one stream cannot borrow each other's
                            # names.
                            key = (index, getattr(tc, "index", 0) or 0)
                            name = getattr(fn, "name", None)
                            if name:
                                names[key] = name
                            elif names.get(key):
                                fn.name = names[key]
                            elif not getattr(fn, "arguments", None):
                                # THE SECOND SHAPE, and remembering cannot fix
                                # it: a delta for a tool index whose OPENING was
                                # never emitted, so there is no name to restore.
                                # Measured on this proxy with five tools in the
                                # request -- indices 0 and 2 arrived with names,
                                # index 1 arrived with NEITHER a name NOR any
                                # arguments.
                                #
                                # An entry with no name and no arguments carries
                                # no information at all: it cannot be executed,
                                # accumulated, or even identified. Its only
                                # effect is to fail a client that validates each
                                # delta, which is what aborts the agent's turn.
                                # Dropping it invents nothing, which is the line
                                # this repair must not cross.
                                drop.append(tc)
                        if drop:
                            kept = [t for t in delta.tool_calls if t not in drop]
                            # Never hand back an EMPTY tool_calls list: some
                            # clients read its presence as "a tool call starts
                            # here". None is the honest shape for "this delta
                            # carries no tool call".
                            delta.tool_calls = kept or None
                    if (
                        index in saw_tool_call
                        and getattr(choice, "finish_reason", None) == "stop"
                    ):
                        choice.finish_reason = "tool_calls"
            except Exception:
                # Never break the stream over a repair.
                pass
            yield chunk


proxy_handler_instance = ToolCallFinishReasonFixer()
