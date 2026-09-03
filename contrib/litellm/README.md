# LiteLLM proxy callbacks

## `gonk_trace.py` — trajectory-evidence collector (gonk-p8j)

Records which tools a gonk session asked the model to call, and reports them to
gonk-meter. Slice 1 of trajectory evaluation: **evidence collection only**,
nothing gates on it until `gonk-hsb`.

### Why the source lives here and not only in the ConfigMap

The deployed copy is a ConfigMap in the gitops repo
(`clusters/orac/apps/litellm/configmap-litellm-gonk-trace.yaml`), because that is
how the proxy mounts a callback. But logic that only exists inside a YAML string
cannot be tested, so the source of truth is here and the ConfigMap embeds it.

**The two copies must be kept in sync by hand.** Verify with:

```sh
make -C ../.. litellm-callback-check   # or the diff below
```

### Testing

```sh
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
python -m pytest -q
```

The pure functions — argument normalisation, call extraction, identity — are
covered without LiteLLM, a proxy or a network. `GonkTrajectoryCollector` is the
deliberately thin edge that is not: keeping that boundary sharp is what makes
the interesting half testable at all.

### What it must never do

- **Store prompt or response text.** Tool names and a normalised path only.
  Bodies carry untrusted issue content and, on cloud rungs, left our premises to
  begin with. `normalise_target` refuses anything containing whitespace, which is
  what stops a grep pattern lifted from an issue body reaching the ledger.
- **Raise into the proxy.** This is a shared service; losing evidence must
  degrade the verdict, never the service.
- **Touch non-gonk traffic.** Requests without gonk metadata are ignored.

### Configuration

| env | meaning |
|---|---|
| `GONK_METER_URL` | gonk-meter base URL, e.g. `http://gonk-meter.gonk.svc:8080` |
| `GONK_METER_TOKEN` | bearer token for `POST /v1/trace` |

Unset either and the collector is inert — which is also the rollback.
