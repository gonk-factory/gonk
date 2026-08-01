# DRAFT — gascity: session messages are delivered as keystrokes into a TUI

**Status: NOT POSTED. Draft for the owner to review, rewrite and file.**
Found by a Claude Code session, 2026-08-01. Bead `gonk-e9m`.
Verified against `4fda5a28445f42d6e789fc7f5751645ac4fecd19` and reproduced live.

---

## Suggested title

`Nudge/SendKeys deliver message text as terminal keystrokes, so agent input can trigger the TUI's own input modes`

## Suggested body

`Provider.Nudge` delivers a message by typing it into the session's terminal:

```go
// internal/runtime/carrier.go:74
func (c *tmuxCarrier) Nudge(ctx context.Context, name string, content []ContentBlock) error {
	message := FlattenText(content)
	...
	if _, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "-l", message); err != nil {
		return err
	}
	_, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "Enter")
	return err
}
```

`-l` prevents tmux from interpreting the text as *tmux key names*. It does not,
and cannot, prevent the **receiving application** from interpreting it. Whatever
occupies the pane decides what the bytes mean, and interactive agent TUIs
generally reserve characters for their own input modes.

Concretely, with opencode 1.18.3 in the pane, a `!` anywhere in the message puts
its composer into shell mode: the bang is consumed, a `$` prompt appears, and
the whole message is handed to `/bin/sh` instead of to the model.

**Reproduction** (k8s-backed agent session, message sent via
`POST /v0/city/{city}/session/{id}/submit`):

```
submitted: Hello there, see issue !42 and reply with exactly the word GOLF and nothing else.
rendered:  $ Hello there, see issue 42 and reply with exactly the word GOLF and nothing else.
produced:  /bin/sh: 1: Hello: not found
```

The API reported success. `request.result.session.submit` was emitted with
`queued=false`. The model never saw the message at all.

Multi-line text, backticks and JSON braces were each tested separately and pass
through correctly — the bang alone triggers it, at any position, not only the
first.

### Why this is worth more than "escape it at the sender"

In our deployment the message body embeds an untrusted GitLab issue title and
description. That makes this a **text-injection boundary**: text an outside
party controls reaches an interpreter chosen by whichever TUI happens to be
running. We can strip bangs, and have as a stopgap, but:

- the trigger set is the TUI's, not ours, and it changes when the agent CLI is
  upgraded — silently, with no signal at the transport layer;
- it is per-provider, so every caller has to know the input grammar of every
  agent binary anyone might configure;
- the failure is invisible. The session looks alive, the API says the message
  was delivered, and the agent sits on a spinner forever.

### What might help

Nothing here is a demand — you know the constraints better than we do. Options
that occur to us, roughly in order of how much they change:

1. **Document the boundary.** Say plainly in the `Carrier`/`Nudge` docs that
   message text is delivered as keystrokes and is subject to the receiving
   application's input grammar, so callers know it is not a neutral pipe.
2. **Bracketed paste.** Wrapping the payload in `\e[200~ … \e[201~` (or
   `tmux load-buffer` + `paste-buffer -p`) makes a paste-aware TUI treat it as
   literal content rather than typed input. It would not cover TUIs that ignore
   bracketed paste, but it addresses the common case at one place in the
   transport instead of N places in the callers.
3. **A non-keystroke path where a provider offers one** — a stdin/socket/ACP
   route advertised through `ProviderCapabilities`, letting callers ask whether
   a real message channel exists rather than assuming.

### Related

`Provider.Nudge` and `SendKeys` in `internal/runtime/k8s/provider.go:535,541`
discard the carrier's error and return `nil` unconditionally, so a caller cannot
detect a failed delivery either. That is a separate defect and we are not
claiming it caused anything above — in our testing the carrier itself worked
fine. Mentioned only because it means a delivery that *does* go wrong is
unreportable through this same path.

---

## Ledger

**Verified** (source at the pinned ref, plus live reproduction):

| claim | evidence |
|---|---|
| `Nudge` sends message text via `send-keys -l` then `Enter` | `internal/runtime/carrier.go:74-88` |
| the k8s provider routes `Nudge` through that carrier | `internal/runtime/k8s/provider.go:535`, `693` |
| a bang anywhere in the text triggers opencode's shell mode | live: `s-go-57b`, message logged above, `/bin/sh: 1: Hello: not found` in the pane |
| multi-line / backticks / JSON braces are unaffected | live: three separate control messages, each answered correctly by the model |
| the API reports success regardless | live: `request.result.session.submit`, `queued=false`, for the message that ran as a shell command |

**Inferred, not verified — soften or cut before posting**

- That bracketed paste would actually fix it for opencode. It is the standard
  mechanism and it is *likely*, but we have not tested opencode's handling of
  bracketed paste, and it should not be presented as a known-good fix.
- That other agent TUIs have the same class of trigger. Plausible (`/`, `@`, `!`
  are common leaders) but only opencode was tested.
- The characterisation of the omission as an oversight. Nothing documents intent
  either way.

**Do not include**: any offer to send a PR. That commits the owner's time.
