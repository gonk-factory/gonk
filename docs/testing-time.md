# Time in tests

The project convention for any test that touches time, scheduling or a shared
OS resource. It exists because the same class of flake reached the gate three
times in eleven days, each time diagnosed by hand, fixed on the one test, and
the lesson left in a commit message. The measurements behind every rule here
are in `docs/reviews/2026-09-21-flaky-gate-review.md` (on branch
`docs/flaky-gate-review`).

The gate runs every test ONCE, with `-race`, alongside every other package, on
a box other worktrees are also loading. A test that is red 6% of the time is
green in 94% of gate runs. So "it passed" says almost nothing about a
wall-clock test. `make hunt` is how you find out what it actually does (see
the last section).

## The rules

### 1. Inject clocks and tickers

If the code under test reads the time, sleeps, or runs on a ticker, the test
supplies the clock or the ticker. The test then asserts on what the code DID
with it -- the interval it armed, the deadline it set, the tick it acted on --
not on how much real time went by.

- `pkg/intake`'s reconcile loop takes a ticker factory (`loopTicker`); the
  cadence tests read the value the loop passes to `Reset`
  (`reconcile_startup_test.go`, gonk-oxb8).
- `cmd/gonk-gate` dispatch takes `deps.Clock`; `broker_start_wait_test.go`
  drives it with a fake. Tests that leave `deps.Clock = nil` get the real
  clock and inherit every problem below.

Leaving the real adapter in use by SOME tests is fine and intended (it keeps
the adapter exercised), as long as those tests do not assert on timing.

### 2. Never count events in a wall-clock window

"Sleep 100ms, then expect at least 5 passes" measures the scheduler, not the
code. Under load the window contains fewer events, and the test goes red with
the code correct. Under a different load it can contain the "right" number
with the code broken, so it also fails to catch the bug it was written for.

Wait for a signal instead: a channel the fake closes, a hook in the fake that
runs at an exact point (`fakeMeter.onStatus` / `onDecide` in
`cmd/gonk-gate/dispatch_test.go`), or a poll-with-deadline on the state you
expect (`waitFor`).

### 3. A wall-time upper bound is a hang guard, never the assertion

A bound like `if time.Since(start) > X` may exist only to stop a test that
would otherwise hang. Then:

- make it generous: 5s at the very least, and far above anything the code
  should take under load (starvation spikes of 2.4s were measured on this box
  with nothing wrong);
- make its failure text say it is a hang guard, not "too slow";
- never let it be the thing that proves the behaviour. If another assertion
  already proves it (the call count, the returned error, the predicate having
  been satisfied), the bound adds nothing but a scheduler measurement --
  delete it.

If the property really IS "the code gives up by T", assert the deadline the
code SET (the context deadline it passed down, the timer it armed on the
injected clock), not the elapsed wall time.

### 4. Shared OS resources: hold, then check

A port, pid, lock file or anything else another process on the machine can
touch is asserted only in the direction the test controls. Hold the resource
for the whole check ("this port is busy while I hold it -- it must be
reported busy") and never assert the other direction ("the port I just
released is free") against the real kernel, and never infer "the code is
broken" from a second observation of the same shared resource. The logic
around the OS call (collect every failure, name them, sort them) is tested
with the call injected.

This box's ephemeral port range is 4,096 ports (44620-48715) against a Linux
default of 28,232, so collisions here are about 7x a normal host's.

**A dead address is HELD, never freed.** "Nothing is listening here" is the
other direction of the same rule, and it is the one that kept coming back
(gonk-th5r, gonk-myvu, gonk-d2kb, gonk-uv82): open a listener, close it, and
use its address as one nobody answers on. A freed port belongs to whoever
binds next. Starting your own servers first does not help against another
process, and neither does dialling the port to prove it refuses -- the proof
is true at the instant it is made, and the code under test probes later (in
gonk-uv82, a shell script and a subprocess later).

Use `internal/testnet.RefusedAddr(t)` / `RefusedURL(t)`. It binds a loopback
port and never calls `listen`, and keeps the socket until the test ends:

- bound, so the kernel gives the port to nobody else -- not to a `:0`
  listener, not to an explicit bind (the holder sets no `SO_REUSEADDR` /
  `SO_REUSEPORT`; `internal/testnet`'s tests assert `EADDRINUSE`);
- not listening, so a connection gets RST: a real `ECONNREFUSED`, the same
  thing the code under test sees in production.

Refused by construction, for as long as the test takes. (The price of not
setting `SO_REUSEADDR` is that the holder cannot take an ephemeral port that
still has a connection in TIME_WAIT, so after a burst of loopback connections
`bind` to port 0 can say "address already in use" with ports to spare. It then
binds explicit ports from the whole unprivileged space until one sticks; which
port is held does not matter.) What was considered
instead, and why not:

- *A listener that never accepts.* It does not refuse: the kernel completes
  handshakes into the backlog and then drops SYNs, so the caller hangs until
  its own timeout. That is a different branch of the code under test.
- *A fixed "nobody uses it" address* (`127.0.0.1:1`, port 0, a TEST-NET
  address). True by convention, not construction; port 0 and unroutable
  addresses behave differently per OS and per network (refused, unreachable,
  or a timeout); and every test shares the one address. Tests that already
  use `127.0.0.1:1` and accept ANY failure are fine and were left alone.
- *A port held in a private network namespace.* Needs root, and the script
  under test would have to run inside the namespace too.
- *A test-owned server that answers with a failure.* It was reached. "The
  meter answered 503" and "nothing answered" are deliberately different
  messages in the code these tests cover.

`internal/buildgate`'s `TestNoTestUsesAFreedPortAsADeadAddress` rejects the
freed-port idiom anywhere in the repo's Go: a listener or `httptest` server
closed and then its `.URL` / `.Addr()` (or a variable built from one) used, or
a helper that defers the close and returns the address. It is syntactic and
per-function, so it catches the obvious three lines and not a port laundered
through a struct; an exception needs `// freed-port-ok: <reason>` on the Close
line.

**A fake's address is the test's choice, not the kernel's.** The same full
range has a second effect, and it is not a port flake (gonk-ltep):
`httptest.NewServer` listens on `127.0.0.1:0` and, when that bind fails,
SILENTLY listens on `[::1]:0` instead. The bind fails with `EADDRINUSE` when
every ephemeral port has a client connection in TIME_WAIT on it -- nothing is
wrong with the test, and on a 4,096-port range a couple of neighbouring
`go test` runs get there (measured on the real loopback during this hunt: two
whole-package hunts and one `go test ./...` left 3,965 sockets in TIME_WAIT on
2,637 distinct ephemeral ports). For as long as the range stays full, every
fake started that way has the URL `http://[::1]:<port>`, and code that looks AT
the URL rather than only dialling it takes a different branch: gonk-gate links
a pipeline only when the project's web URL has no `[` in it, so every test
asserting on a link went red, and green again a moment later.

Use `internal/testnet.NewServer(t, handler)` (or `testnet.Listen(t)`) for any
fake whose URL the code under test reads, parses, validates or writes
somewhere. It is always `http://127.0.0.1:<port>`: port 0 first, then explicit
ports from the whole unprivileged space, and a `t.Fatalf` naming the errno if
neither can be had -- never another family. `pkg/glab/glabtest` is served that
way (its `TestTheFakeIsServedOnlyThroughTestnet` rejects any other start). The
other `httptest.NewServer` calls in the repo (about 80) were NOT converted and
were not checked one by one: forcing only the fake GitLab onto `[::1]` turns
twelve `cmd/gonk-gate` tests red and nothing else in `go test ./...`, which
says nothing about what the other fakes' URLs feed.

### 5. Subprocess bounds are safety nets

A test that runs a subprocess (a shell script, a helper binary, the program
under `-race`) needs a bound so a hang cannot wedge the suite. That bound is a
safety net: large, and its message says "safety net". The PASS condition is
what the subprocess printed or how it exited -- the marker line appearing, the
exit code, the call count -- never "it finished within N seconds". Kill timers
must not be read as "the code never reached step X".

### 6. `time.Sleep` needs a reason

`golangci-lint` (forbidigo, `.golangci.yml`) rejects `time.Sleep` in a
`_test.go` file unless the line carries `//nolint:forbidigo // <reason>`, and
nolintlint rejects a `//nolint` with no reason. Honest reasons: the poll
interval of a wait-until with a deadline; a helper process idling until the
test kills it; a negative wait ("nothing more may happen") that load can only
make pass vacuously, never fail; a server-side timer the test cannot inject.
If the honest reason is "so the other goroutine has had time to get there",
that is rule 2 -- use a signal.

The lint only catches sleeps. It does NOT catch rule 3 or rule 5 violations
(`time.Since(start) > const`, a real-clock budget handed to the code). Those
are for review.

## The incidents

| Date | Bead | Test | What happened |
|------|------|------|---------------|
| 2026-09-10 | gonk-654i | `test/harness TestCheckPortsFreePassesForAFreePort` | Asserted a just-released port was free against the real kernel. Fix (bde6048) added a 5-attempt retry plus a "confirm by binding it ourselves, else CheckPortsFree is broken" `Fatalf` -- a second observation of the same shared port, with the same race. Rule 4. |
| 2026-09-13 | gonk-alw | `cmd/gonk-gate write_budget_test.go` | Real-clock budget tests red 8/180 under 24-36 CPU burners with `-race`. The fix (cd69cfc) widened the budgets to seconds and left them on the real clock; the class is still open as gonk-te7x / gonk-zk25. Rules 1 and 3. |
| 2026-09-21 | gonk-oxb8 | `pkg/intake TestLoopSpeedsUpAfterAHealthyProjectGoesUnsynced` | Counted loop passes in a 100ms real sleep. 6/100 red under load; `make hunt` on the pre-fix commit reproduces it (5/100). Fixed (a8ec998) by injecting the ticker and asserting the re-arm value: 0/500 under the same load. Rules 1 and 2. |
| 2026-09-21 | gonk-th5r | `test/harness TestCheckPortsFreePassesForAFreePort` | The 09-10 fix's own "confirm" line fired: the port was taken by someone else between the two binds, and the test concluded the code was broken. A second-generation flake -- the fix for the first reintroduced the race it guarded against. Fixed (1279e3a, merged 5eafdf2) by testing CheckPortsFree's contract with the bind injected (`ports_internal_test.go`) and keeping only hold-then-check against the real kernel. Rule 4. |
| 2026-09-21 | gonk-5pyx | `cmd/gonk-gate TestASecondDispatchDuringTheDeliveryWindowCreatesNoSecondSession` | Found by the first `make hunt` sweep, never seen red in a gate: a `time.Sleep(1s)` that had to land after one dispatch's reservation and before its real-clock 3s budget ran out. 8/200 red under load. Replacing the sleep with a signal-and-hold hook still left 2/200 (the budget was still real time), and a fake clock with a 3s delivery budget still went red 1/300 in independent verification (my own 300-run hunt of that version had seen 0, which is what a 0.3% rate looks like at that sample size), because `bounded()` wraps each prompt-status read in a REAL timeout of the remaining budget -- around the read the test holds -- and both dispatches share one fake clock. With an hour of budget and an hour of per-read timeout, 0/500. Rules 1 and 2 -- and the reason to measure a fix rather than trust it, twice. |
| 2026-10-05 | gonk-uv82 | `internal/packtest TestMeterReachableCheckReportsAGenuinelyUnreachableMeter` | Red in a gate while three other agents' gates ran: "exit 0, want 2 ... answered HTTP 204". The address came from `testnet.RefusedAddr`, which (gonk-d2kb's fix for gonk-myvu's fix) freed a port and then dialled it to PROVE it refused -- and another process bound it before the script under test probed it. Third-generation: each fix narrowed the window and none closed it. Reproduced on demand with `make hunt CHURN=32` (17/100; CPU burners alone cannot bind a port). Fixed by holding the port bound and unlistened for the whole test: 0/500 under the same recipe. The same idiom was sitting unrouted in `pkg/intake` (`srv.Close() // closed before use`); a buildgate test now rejects it. Rule 4. |
| 2026-10-07 | gonk-ltep | `cmd/gonk-gate TestTheSameFailureAgainUpdatesTheLatestFailingLine`, `TestADifferentFailureSupersedesTheRun` | Red once, together, in a whole-package run while several agents' gates and mutation scripts ran; green in 1,200 isolated runs and, here, in 2,000 more under 36 CPU burners and 8 shuffled whole-package rounds. No clock, sleep or deadline is involved. The fake GitLab had moved to `[::1]`: `httptest.NewServer` falls back to IPv6 without saying so when it cannot bind an IPv4 port, the fake's URL is the project's `web_url`, and gonk-gate does not link a bracketed host. The twelve tests of the package that assert on a link all fail that way, and only those; the two seen are neighbours in one file with a test between and after them that asserts no link. Reproduced on demand with `make hunt CHURN=32` (189/1,350 on the branch, 188/1,350 on main, the same five rows) and deterministically by putting the fake on `[::1]`. Fixed in the fake (`testnet.NewServer`): 0/4,500 under the same recipe. Rule 4. |

More of the wall-clock class was filed on 09-21. Fixed and closed since: gonk-4f0v
(rule 5, seen red in a gate on 09-21 -- it was the worst of them, 48/60 under
load) and gonk-47l6 (rules 3 and 5, the prompt-fetch 410 test). Still open:
gonk-fzxb (rule 3, `TestWaitForSucceedsOnPredicate`), gonk-zk25 and gonk-te7x
(rules 1 and 3, `write_budget_test.go`; zk25 seen red in a gate on 09-21),
gonk-tj86 (rules 1 and 3). Their rates as measured by `make hunt` are in the
beads.

## Hunting: `make hunt`

`hack/flake_hunt.sh`, run as `make hunt`, is the load recipe that was worked
out by hand for each of these incidents (and reproduced gonk-oxb8 and the
09-13 budget flakes on demand; the port flakes never did under CPU load alone
-- see `CHURN` below), written down: the test binary is compiled once with `-race`,
then run with `-test.count=N` and `GOMAXPROCS=2` while 3 x nproc CPU-burner
PROCESSES run (burner goroutines inside one process reproduced nothing: 0/100
against 6/100). It prints each test's failure count, rate and p50/p99/max
duration. A max far above p50 is a bound waiting to fail even when the count
is 0.

    make hunt PKG=./pkg/intake RUN='^TestLoopSpeedsUp' COUNT=100
    make hunt PKG=./... ROUNDS=20 COUNT=1 SHUFFLE=on BURNERS=0   # order dependence

**Port flakes need a neighbour, not CPU load: `CHURN=N`.** A test that expects
nobody on a port it does not hold goes red when another process is handed that
port, and no number of CPU burners binds a port. `CHURN=N` starts
`hack/portchurn`: N loopback listeners on kernel-chosen ports, each answering
204 and re-bound somewhere else about once a second -- another gate's
`httptest` servers, on purpose.

    make hunt PKG=./internal/packtest RUN=Unreachable COUNT=200 CHURN=32

With `CHURN` the whole hunt runs in a private network namespace (`unshare -n`,
so it needs root) whose ephemeral range is narrowed to `CHURN_RANGE` ports
(default 16 x CHURN). Both are deliberate:

- the namespace keeps the churn off the real loopback, where it would hand
  its listeners to every other gate on the box and turn THEM red with the
  flake being hunted;
- the narrow range is what gets a usable rate from few listeners. `bind()` on
  this box takes about 11 ms (300 binds, 3.3 s), so the first attempt -- 1,024
  listeners rotating over the real 4,096-port range -- spent the run queued in
  `bind()`: 6 listeners up, 12 s per test run, 0 red in 30.

The rate it prints is the recipe's, not the gate's, and it swings (3, 17 and
61 per 100 on the same unfixed test). It answers "can this go red, and does
the fix take it to 0", not "how often". And a range that is too narrow runs
out of ports: `bind: address already in use` from a listen on `:0` is the
recipe failing, not the test -- widen `CHURN_RANGE` and do not count it.

**`CHURN` is also how a full port range is reproduced, and a full range does
more than hand ports to neighbours.** In the narrowed namespace `bind` to port
0 fails often, which is exactly the condition under which `httptest.NewServer`
moves a fake to `[::1]` (rule 4, gonk-ltep). So a group of tests that went red
TOGETHER in one whole-package run and never alone -- and that CPU burners and
`SHUFFLE=on` do not touch -- is worth one `CHURN` run over the whole file they
live in, not only over the tests that were seen red: which of their neighbours
fail with them, and which never do, is the diagnosis.

    make hunt PKG=./cmd/gonk-gate RUN='^(TestAGreenPipeline|TestTheSameFailure|TestADifferentFailure|TestRunMaintenanceLooks|TestOnlyTheNewest)' COUNT=150 CHURN=32 BURNERS=12

And keep the output: the run that found gonk-ltep had been piped through a
`grep` that kept the `--- FAIL` lines and dropped the assertion text, which
was the whole answer (a note with `pipeline 970` where a link was expected).

It is an instrument, not a gate: `make gate` stays one honest sample, and
nothing schedules the hunt. Use it:

- when fixing a flake: show it red on demand BEFORE the fix (a rate, not an
  anecdote) and 0 after, at a count where 0 means something (for a 5% flake,
  100 runs has a 0.6% chance of showing 0 by luck; use a few hundred);
- when writing or reviewing any test that touches time, a subprocess or a
  port, to see its duration spread under load;
- when a gate goes red on a test the branch did not touch (CLAUDE.md, "A flaky
  test is a red gate"): the rate goes into the P1 bead.

The gate's `go test` runs with `-shuffle=on`, so a failure that depends on
test order prints `-test.shuffle <seed>` at the top of that package's output;
rerun with `-shuffle=<seed>` to reproduce it.
