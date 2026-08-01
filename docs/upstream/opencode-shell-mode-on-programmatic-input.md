# DRAFT — opencode: a bang anywhere in delivered input switches the composer to shell mode

**Status: NOT POSTED. Draft for the owner to review, rewrite and file.**
Found by a Claude Code session, 2026-08-01. Bead `gonk-e9m`.
opencode 1.18.3 (pinned, `images/versions.env`). Reproduced live; **not** read
against opencode's source.

---

## Suggested title

`TUI: "!" anywhere in programmatically-delivered input switches to shell mode and executes the whole message`

## Suggested body

When text is delivered to a running opencode TUI by an orchestrator — in our
case `tmux send-keys -l` from a session supervisor, which is how agent
harnesses are commonly driven — a `!` **anywhere** in the text puts the composer
into shell mode. The bang is consumed, the composer shows a `$` prefix, and the
entire message is executed by `/bin/sh` rather than being sent to the model.

**Reproduction**

```
$ tmux send-keys -t main -l 'Hello there, see issue !42 and reply with exactly the word GOLF and nothing else.'
$ tmux send-keys -t main Enter
```

Pane afterwards:

```
┃  $ Hello there, see issue 42 and reply with exactly the word GOLF and nothing else.
┃
┃  /bin/sh: 1: Hello: not found
```

No model request appears in `opencode.log`; the session is created and then sits
on a spinner indefinitely.

Control cases, delivered identically, all behave correctly — the model answers
normally:

- multi-line text (three lines arrived as one message)
- backticks / inline code spans
- JSON braces, quotes and brackets

So the bang alone is the trigger, and its position does not matter. In the
example above it is the 24th character.

**What this looks like in practice.** Our prompts are generated and embed
third-party issue text, so they routinely contain `!` — GitLab merge-request
references (`!123`), HTML comments (`<!-- … -->`), and ordinary exclamation
marks written by users. Every one of those silently converts the prompt into a
shell command. The failure is quiet: opencode looks busy, the transport reports
success, and nothing indicates the message was never asked.

**What might help** (deferring to whatever fits opencode's input model):

- restrict the shell-mode trigger to a leading `!` on an otherwise-empty
  composer, rather than any occurrence;
- honour bracketed paste, so orchestrator-delivered text is treated as content
  rather than typed input;
- or offer a documented non-TUI ingress for automation — `opencode run` already
  exists and is what we intend to move to; the gap is only for the case where
  something drives the interactive UI.

---

## Ledger

**Verified** (live, opencode 1.18.3 in a k8s pod against a local model via LiteLLM):

| claim | evidence |
|---|---|
| a bang mid-string triggers shell mode | pane render above; `$` prefix, bang consumed |
| the message is executed by `/bin/sh` | `/bin/sh: 1: Hello: not found` |
| the model is never invoked | no request in `~/.local/share/opencode/log/opencode.log`; session created, no turn |
| multi-line text is fine | 3-line message answered `DELTA` |
| backticks are fine | message with two inline code spans answered `ECHO` |
| the whole message, not just the tail, is executed | 40-line prompt beginning `<!--` produced `/bin/sh: 2: Syntax error: newline unexpected` |

**Inferred, not verified — soften or cut before posting**

- **That this is unintended.** A leading-`!` shell escape is a normal TUI
  affordance; that it fires *mid-string* is what looks wrong, but we have not
  read opencode's input handling and cannot say which is the intended rule. The
  report should describe the behaviour and its consequence, not assert a bug.
- That bracketed paste would fix it — untested against opencode.
- That the behaviour is unchanged in versions after 1.18.3. We pin, and did not
  check `latest` before writing this. **Check before posting**; a report against
  a stale version wastes a maintainer's time and is the kind of thing that gets
  automated reports dismissed.

**Note on framing.** This one is a *behaviour report from an unusual integration*
rather than a defect we can prove. It is the weaker of the two drafts. If only
one gets filed, the gascity keystroke-boundary report is the one that carries
its own evidence.
