#!/usr/bin/env python3
"""mutate_loop_watchdog.py -- "seen red" evidence for the loop watchdog's
signature rule in images/agent/entrypoint.sh (gonk-un7z, gonk-mf3v).

Applies one named break at a time to the awk filter, runs test/entrypoint's
loop-watchdog tests, and requires them to FAIL; then restores the file byte for
byte (try/finally). A mutation whose text is not found is VACUOUS and one the
tests do not notice SURVIVED -- each is a failure of this script.

Usage: hack/mutate_loop_watchdog.py          (all mutations)
       hack/mutate_loop_watchdog.py 0 3      (only these indexes)
Exit:  0 iff every selected mutation applied and was killed.

It edits a file in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate
(gonk-5ed1). The same rule is pinned against REAL opencode by
`make component-jobs`, which this script does not run.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
E = 'images/agent/entrypoint.sh'
PENDING = '\tif (sig == "Invalid Tool") {\n\t\tpending = 1\n\t\tnext\n\t}\n'
muts = [
    # gonk-mf3v, the live defect: every malformed call is the bare line.
    ("mf3v: Invalid Tool counted by its bare line (two DIFFERENT malformed calls are one loop)",
     PENDING, ''),
    ("mf3v: the detail line is not part of the signature",
     'failed_call("Invalid Tool: " line)', 'failed_call("Invalid Tool")'),
    ("mf3v: a malformed call is never counted (the same one twice is not a loop)",
     '\t\t\tfailed_call("Invalid Tool: " line)\n', ''),
    ("mf3v: an Invalid Tool line with no detail is counted (empty line)",
     'if (line != "" && line !~ /^\\342\\234\\227 /) {', 'if (line !~ /^\\342\\234\\227 /) {'),
    ("mf3v: a failed call right after Invalid Tool is taken for its detail",
     'if (line != "" && line !~ /^\\342\\234\\227 /) {', 'if (line != "") {'),
    ("mf3v: ANSI not stripped from the detail line (pending checked before cleaning)",
     '\tif (pending) {\n\t\tpending = 0\n\t\tif (line != ""', '\tif (pending) {\n\t\tpending = 0\n\t\tline = $0\n\t\tif (line != ""'),
    # gonk-un7z, the original rule.
    ("un7z: a call that RAN counts as a failure",
     '\tif (line !~ /^\\342\\234\\227 /) next\n', '\tif (line !~ /^\\342/) next\n'),
    ("un7z: threshold ignored (first failure stops the session)",
     'if (n < threshold + 0) return', 'if (n < 1) return'),
    ("un7z: never fires", 'if (n < threshold + 0) return', 'return'),
    ("un7z: all failed calls share one signature",
     '\tn = ++count[sig]\n', '\tn = ++count["x"]\n'),
    ("un7z: output not passed through", '\tprintf "%s\\n", $0\n\tfflush()\n', '\tfflush()\n'),
]
only = sys.argv[1:]
res = []
for i, (name, old, new) in enumerate(muts):
    if only and str(i) not in only:
        continue
    src = open(E).read()
    if old not in src:
        res.append(f"VACUOUS  {name}")
        continue
    open(E, 'w').write(src.replace(old, new, 1))
    try:
        r = subprocess.run([GO, "test", "./test/entrypoint/", "-run", "LoopWatchdog", "-count=1"],
                           capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED {name}")
        elif not fails:
            res.append(f"NOCOMPILE {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   {name}\n           " + "\n           ".join(fails))
    finally:
        open(E, 'w').write(src)
print("\n".join(res))
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)}")
sys.exit(1 if bad else 0)
