import asyncio, importlib.util, sys
spec = importlib.util.spec_from_file_location("cc", "/work/custom_callbacks.py")
cc = importlib.util.module_from_spec(spec); spec.loader.exec_module(cc)
from litellm.integrations.custom_logger import CustomLogger
assert isinstance(cc.proxy_handler_instance, CustomLogger), "handler is not a CustomLogger"
assert "async_post_call_streaming_iterator_hook" in type(cc.proxy_handler_instance).__dict__, \
    "hook must be defined ON the class or the proxy silently skips it"

class O:
    def __init__(self, **kw):
        for k, v in kw.items(): setattr(self, k, v)
def fn(name=None, arguments=""):
    f = O(arguments=arguments)
    if name is not None: f.name = name
    return f
def chunk(*tcs, finish=None, index=0):
    return O(choices=[O(index=index, delta=O(tool_calls=list(tcs) or None), finish_reason=finish)])
def run(chunks):
    async def gen():
        for c in chunks: yield c
    async def collect():
        return [c async for c in cc.proxy_handler_instance.async_post_call_streaming_iterator_hook(None, gen(), {})]
    return asyncio.run(collect())

fails = []
def check(label, cond):
    print(("PASS  " if cond else "FAIL  ") + label)
    if not cond: fails.append(label)

out = run([chunk(O(index=0, function=fn("list_files", ""))),
           chunk(O(index=0, function=fn(None, '{"path": '))),
           chunk(O(index=0, function=fn(None, '"."}')))])
check("name restored on continuation deltas",
      [c.choices[0].delta.tool_calls[0].function.name for c in out] == ["list_files"]*3)

out = run([chunk(O(index=0, function=fn("alpha","")), O(index=1, function=fn("beta",""))),
           chunk(O(index=0, function=fn(None,"a")), O(index=1, function=fn(None,"b")))])
tcs = out[1].choices[0].delta.tool_calls
check("parallel calls keep their own names",
      tcs[0].function.name == "alpha" and tcs[1].function.name == "beta")

out = run([chunk(O(index=0, function=fn("first",""))), chunk(O(index=0, function=fn("first","a")))])
check("a present name is never overwritten (self-deactivating)",
      [c.choices[0].delta.tool_calls[0].function.name for c in out] == ["first","first"])

out = run([chunk(O(index=0, function=fn(None,"args")))])
check("no name is invented when none was ever seen",
      getattr(out[0].choices[0].delta.tool_calls[0].function, "name", None) is None)

out = run([chunk(O(index=0, function=fn("list_files",""))), chunk(finish="stop")])
check("finish_reason repair still works (openclaw-e3mb)",
      out[1].choices[0].finish_reason == "tool_calls")

out = run([chunk(finish=None), chunk(finish="stop")])
check("ordinary text completions untouched", out[1].choices[0].finish_reason == "stop")

weird = [O(), O(choices=None), O(choices=[]), O(choices=[O()]),
         O(choices=[O(index=0, delta=None, finish_reason=None)]),
         O(choices=[O(index=0, delta=O(tool_calls=[O()]), finish_reason=None)])]
ok = True
for w in weird:
    try:
        if run([w]) != [w]: ok = False
    except Exception as e:
        ok = False; print("   raised on", w, e)
check("unexpected chunk shapes pass through without raising", ok)

print("\nRESULT:", "ALL PASS" if not fails else ("FAILURES: " + ", ".join(fails)))
sys.exit(1 if fails else 0)
