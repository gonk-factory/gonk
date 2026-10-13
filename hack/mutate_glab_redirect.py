#!/usr/bin/env python3
"""mutate_glab_redirect.py -- "seen red" evidence for gonk-467e (gonk-37kg part D).

pkg/glab sends the bot's token in PRIVATE-TOKEN, a custom header Go's
http.Client copies onto every redirect. The client's redirect policy
(client.go, httpClient / redirectPolicy / sameOrigin) keeps the token on the
origin, follows a READ to another origin without it, and does not follow a
WRITE there at all.

Applies one named break at a time, runs pkg/glab's tests, and requires the
NAMED test to FAIL; then restores the file byte for byte (try/finally). A
mutation whose text is not found is VACUOUS, one the tests do not notice
SURVIVED, one that fails only other tests MISSED, and one that does not
compile NOCOMPILE -- each is a failure of this script.

Usage: hack/mutate_glab_redirect.py            (all mutations)
       hack/mutate_glab_redirect.py 3 7        (only these indexes)
Exit:  0 iff every selected mutation applied and was killed by its named test.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout. Needs `go` on PATH (or GO=/path/to/go). Not run by any gate.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')
C = 'pkg/glab/client.go'
CROSS = 'TestACrossOriginRedirectIsFollowedWithoutTheToken'
SLOTS = 'TestNoTokenSlotCrossesAnOrigin'
SAME = 'TestASameOriginRedirectKeepsTheToken'
BACK = 'TestTheTokenDoesNotComeBackAfterAChainLeftTheOrigin'
AWAY = 'TestTheTokenStaysOffWhileAChainStaysAway'
WRITE = 'TestAWriteIsNotRedirectedToAnotherOrigin'
ORIGIN = 'TestSameOrigin'
SWAP = 'TestTheRedirectPolicySurvivesAReplacedHTTPClient'
muts = [
    ("THE DEFECT: no redirect policy (c.HTTP is used as it is)", CROSS,
     "\t\tresp, err := c.httpClient().Do(req)\n", "\t\tresp, err := c.HTTP.Do(req)\n"),
    ("the token is not taken off a request that left the origin", CROSS,
     "\treq.Header.Del(tokenHeader) // redirect:strip\n", "\t// redirect:strip\n"),
    ("the admin and rotation slots: the header taken off is not the one the token is sent in", SLOTS,
     "\treq.Header.Del(tokenHeader) // redirect:strip\n", "\treq.Header.Del(\"Authorization\") // redirect:strip\n"),
    ("a read to another origin is refused, not followed (object storage stops working)", CROSS,
     "\tif first.Method != http.MethodGet && first.Method != http.MethodHead { // redirect:write",
     "\tif true { // redirect:write"),
    ("a write to another origin is followed (without the token)", WRITE,
     "\tif first.Method != http.MethodGet && first.Method != http.MethodHead { // redirect:write",
     "\tif false { // redirect:write"),
    ("a refused write's error is Go's, which prints the other host's Location", WRITE,
     "\t\tif errors.Is(err, ErrCrossOriginRedirect) {\n", "\t\tif false {\n"),
    ("every redirect is treated as leaving (a same-origin redirect loses the token)", SAME,
     "\tleft := !sameOrigin(first.URL, req.URL) // redirect:leaves", "\tleft := true // redirect:leaves"),
    ("only the NEXT hop is looked at: the token comes back when a chain returns to the origin", BACK,
     "\t\tif !sameOrigin(first.URL, v.URL) { // redirect:left-earlier", "\t\tif false && !sameOrigin(first.URL, v.URL) { // redirect:left-earlier"),
    ("the origin compared with is the previous hop, not the first request", AWAY,
     "\tleft := !sameOrigin(first.URL, req.URL) // redirect:leaves\n\tfor _, v := range via[1:] {\n\t\tif !sameOrigin(first.URL, v.URL) { // redirect:left-earlier",
     "\tleft := !sameOrigin(via[len(via)-1].URL, req.URL) // redirect:leaves\n\tfor _, v := range via[1:] {\n\t\tif false && !sameOrigin(first.URL, v.URL) { // redirect:left-earlier"),
    ("sameOrigin ignores the port", ORIGIN,
     "strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)", "strings.EqualFold(a.Hostname(), b.Hostname()) && (port(a) == port(b) || true)"),
    ("sameOrigin ignores the port (seen by the client: the test servers differ only in port)", CROSS,
     "strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)", "strings.EqualFold(a.Hostname(), b.Hostname()) && (port(a) == port(b) || true)"),
    ("sameOrigin ignores the scheme", ORIGIN,
     "return strings.EqualFold(a.Scheme, b.Scheme) && ", "return "),
    ("sameOrigin accepts a host that ends with the origin's name's letters (suffix match)", ORIGIN,
     "strings.EqualFold(a.Hostname(), b.Hostname())", "strings.HasPrefix(strings.ToLower(b.Hostname()), strings.ToLower(a.Hostname()))"),
    ("sameOrigin accepts a subdomain of the origin", ORIGIN,
     "strings.EqualFold(a.Hostname(), b.Hostname())", "strings.HasSuffix(strings.ToLower(b.Hostname()), strings.ToLower(a.Hostname()))"),
    ("sameOrigin compares host names by letter case (a legitimate redirect loses the token)", ORIGIN,
     "strings.EqualFold(a.Hostname(), b.Hostname())", "a.Hostname() == b.Hostname()"),
    ("sameOrigin does not default the port (https://h and https://h:443 differ)", ORIGIN,
     "\t\tcase \"https\":\n\t\t\treturn \"443\"\n", "\t\tcase \"https\":\n\t\t\treturn \"\"\n"),
    ("the caller's own CheckRedirect is dropped", SWAP,
     "\t\tif inner != nil {\n", "\t\tif false && inner != nil {\n"),
    ("no redirect limit when the caller set none (a loop never ends)", SWAP,
     "\t\tif len(via) >= maxRedirects {", "\t\tif false && len(via) >= maxRedirects {"),
]
only = sys.argv[1:]
res = []
for i, (name, expect, old, new) in enumerate(muts):
    if only and str(i) not in only:
        continue
    src = open(C).read()
    if old not in src or old == new:
        res.append(f"VACUOUS  {i} {name}")
        print(res[-1], flush=True)
        continue
    open(C, 'w').write(src.replace(old, new, 1))
    try:
        try:
            r = subprocess.run([GO, "test", "./pkg/glab/", "-count=1", "-timeout", "120s"], capture_output=True, text=True, timeout=300)
            out, code = r.stdout + r.stderr, r.returncode
        except subprocess.TimeoutExpired as e:
            out, code = (e.stdout or b"").decode() + "\n--- FAIL: (timeout) ", 1
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if "panic: test timed out" in out:
            fails.append(f"--- FAIL: {expect} (timed out: the test did not end)") if expect == SWAP else fails.append("--- FAIL: (timeout) ")
        if code == 0:
            res.append(f"SURVIVED {i} {name}")
        elif not fails:
            res.append(f"NOCOMPILE {i} {name}\n{out[:800]}")
        elif not any(f.startswith(f"--- FAIL: {expect} ") or f.startswith(f"--- FAIL: {expect}/") for f in fails):
            res.append(f"MISSED   {i} {name}\n           wanted {expect}, got:\n           " + "\n           ".join(fails))
        else:
            res.append(f"KILLED   {i} {name}\n           " + "\n           ".join(fails))
    finally:
        open(C, 'w').write(src)
    print(res[-1], flush=True)
bad = [r for r in res if not r.startswith("KILLED")]
print(f"killed={len(res)-len(bad)} not-killed={len(bad)} of {len(res)}")
sys.exit(1 if bad else 0)
