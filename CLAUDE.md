# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Implementing a Plan

**Write the plan down first, then have a DIFFERENT agent implement each part, then
check the result against the plan.** Not the session that wrote it.

The failure this exists to stop is real and repeated: a plan gets written, most of
it gets built, and the session reports it done because the session is the same one
that decided what "done" meant. It marks its own homework, and it does so holding
every assumption that produced the gaps. Nothing catches the missing third.

1. **Write the plan to a markdown file in the repo** before implementing. This is
   the precondition for everything below -- a plan that exists only in a session's
   context cannot be checked against, because there is nothing to check.
2. **Dispatch a subagent per distinct part.** Independent parts can run in
   parallel; give each one the plan file and the specific slice it owns.
3. **Verify against the WHOLE plan, with a fresh agent.** Not "did the tests
   pass" -- tests only assert what someone thought to assert. Ask: for each
   numbered item in the plan, what is the evidence it was built, and where does
   that evidence live? An item with no evidence is not done, whatever the diff
   shows.
4. **Report gaps as gaps.** Partial implementation is a normal outcome and must
   be stated plainly, per-item, not averaged into "mostly done".

**BE STRICT ON YOUR OWN WORK — grade it the way you would grade someone else's.**
The default failure is not dishonesty, it is generosity: the author knows what
each line was *meant* to do, so a line that looks right reads as working. That is
precisely the judgement a verifier does not have and why it must not be the
author who makes it.

This is measured, not theoretical. On 2026-09-07 this session implemented a
five-item plan, reviewed it, and reported it complete. A fresh agent checking the
same plan found **four of the five items PARTIAL**, including a `set -e` bug that
silently reintroduced a defect fixed hours earlier, and a session capability
being written into a log the same commit told operators to read. Every one was
visible in the diff.

So: when tempted to write "done", ask what a reviewer who distrusts you would
demand as evidence — and go get that instead.

**Verification means observing the thing, not the proxy for it.** A green test
run is not evidence a feature works in the deployment; a deployed image is not
evidence the code path executes; a code path executing is not evidence it changed
the outcome. Prefer the measurement closest to the claim -- and never truncate
the output you are verifying from (`head -N` on a test run has hidden real
failures here more than once).

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - `make gate` (fmt, vet, test, lint)

   **Run it BEFORE pushing, not after.** gonk commits go direct to main, so a
   gate that only runs in CI is post-hoc detection, not prevention -- by the
   time it goes red the commit is already on main. That matters most for the
   chart seal (gonk-sjb): a chart edit that skips the version bump is one Flux
   will never deploy, and the only thing that says so is `go test ./...`.
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
