#!/usr/bin/env python3
"""mutate_relexport.py -- "seen red" evidence for the release export
(gonk-cuwn parts GUARD and EXPORT): internal/exportguard, its gitexport
sub-package, hack/relexport and the real data in release/.

One mutation per deny-pattern rule and per export rule. Each one disables or
bends exactly one thing -- deletes a rule, widens a range check by one, drops
a boundary, empties the allow-list match, skips paths, skips the message
scan, removes one fence of publish -- runs ONLY the test that guards it (each
mutation names its package and -run pattern), and requires it to FAIL. Every
file is restored byte for byte afterwards (try/finally).

  KILLED     the named test went red                      (the only pass)
  SURVIVED   the named test did not notice                (failure)
  VACUOUS    the text to mutate was not found, or was found more than once
             and the mutation would be ambiguous          (failure)
  NOCOMPILE  the mutant did not build                     (failure)

Usage: hack/mutate_relexport.py            (all mutations)
       hack/mutate_relexport.py 3 7        (only these indexes)
       hack/mutate_relexport.py --list     (names and indexes, nothing run)
       hack/mutate_relexport.py --show 3   (also print the full `go test` output)
Exit:  0 iff every selected mutation applied and was killed.

It edits files in the CURRENT worktree; run it in your own worktree, never in
a shared checkout, and after an interruption run `git status` and restore
whatever it left mutated. Needs `go` on PATH (or GO=/path/to/go). Not run by
any gate; its output goes into the evidence log.
"""
import os
import subprocess
import sys

os.chdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
GO = os.environ.get('GO', 'go')

EG = './internal/exportguard/'
GE = './internal/exportguard/gitexport/'
CLI = './hack/relexport/'
REL = './release/'

GENERIC = 'internal/exportguard/generic.go'
GUARD = 'internal/exportguard/guard.go'
DATA = 'internal/exportguard/data.go'
LIST = 'internal/exportguard/list.go'
MODE = 'internal/exportguard/mode.go'
SOURCE = 'internal/exportguard/source.go'
GIT = 'internal/exportguard/gitexport/git.go'
TREE = 'internal/exportguard/gitexport/tree.go'
CHECK = 'internal/exportguard/gitexport/check.go'
COMMIT = 'internal/exportguard/gitexport/commit.go'
PUBLISH = 'internal/exportguard/gitexport/publish.go'
VERIFY = 'internal/exportguard/gitexport/verify.go'
IDENT = 'internal/exportguard/gitexport/identity.go'
MAIN = 'hack/relexport/main.go'
DENY = 'release/deny-internal.txt'
RLIST = 'release/internal-only.txt'
RECORD = 'internal/exportguard/gitexport/record.go'
TSCAN = 'internal/exportguard/treescan.go'
PTREE = 'internal/buildgate/public_tree_test.go'
BG = './internal/buildgate/'

RED = '^TestEveryRuleIsSeenRedInContentPathAndMessage$'
NEAR = '^TestNearMissesStayGreen$'
ALLOW = '^TestAllowListIsExact$'
ALLOW2 = '^TestAllowDomainAnywhereAndUpstream$'

muts = []


def m(name, pkg, run, *edits):
    muts.append((name, list(edits), pkg, run))


def off(f, cond):
    """`if COND {` -> `if (COND) && false {`: the condition is still evaluated
    (so nothing it declares goes unused and the mutant compiles) and never
    holds."""
    if '; ' in cond:
        init, c = cond.rsplit('; ', 1)
        return (f, 'if ' + cond + ' {', 'if ' + init + '; (' + c + ') && false {')
    return (f, 'if ' + cond + ' {', 'if (' + cond + ') && false {')


def case_off(f, cond):
    return (f, 'case ' + cond + ':', 'case false:')


def drop(f, text):
    return (f, text, '')


# ---------------------------------------------------------------- G1, G2 ----
m("G1: delete the 10/8 rule", EG, RED, (GENERIC, 'case a == 10:', 'case a == 10 && false:'))
m("G1: delete the 172.16/12 rule", EG, RED, (GENERIC, 'case a == 172 && b >= 16 && b <= 31:', 'case false:'))
m("G1: delete the 192.168/16 rule", EG, RED, (GENERIC, 'case a == 192 && b == 168:', 'case false:'))
m("G2: delete the CGNAT rule", EG, RED, (GENERIC, 'case a == 100 && b >= 64 && b <= 127:', 'case false:'))
m("G1: widen 10/8 down by one (9.x)", EG, NEAR, (GENERIC, 'case a == 10:', 'case a == 10 || a == 9:'))
m("G1: widen 10/8 up by one (11.x)", EG, NEAR, (GENERIC, 'case a == 10:', 'case a == 10 || a == 11:'))
m("G1: widen 172.16/12 down by one (172.15)", EG, NEAR, (GENERIC, 'b >= 16 && b <= 31', 'b >= 15 && b <= 31'))
m("G1: widen 172.16/12 up by one (172.32)", EG, NEAR, (GENERIC, 'b >= 16 && b <= 31', 'b >= 16 && b <= 32'))
m("G1: narrow 172.16/12 from below (172.16 passes)", EG, RED, (GENERIC, 'b >= 16 && b <= 31', 'b >= 17 && b <= 31'))
m("G1: narrow 172.16/12 from above (172.31 passes)", EG, RED, (GENERIC, 'b >= 16 && b <= 31', 'b >= 16 && b <= 30'))
m("G1: widen 172.x first octet (171, 173)", EG, NEAR, (GENERIC, 'case a == 172 && b >= 16', 'case a >= 171 && a <= 173 && b >= 16'))
m("G1: widen 192.168 second octet down (192.167)", EG, NEAR, (GENERIC, 'a == 192 && b == 168', 'a == 192 && b >= 167 && b <= 168'))
m("G1: widen 192.168 second octet up (192.169)", EG, NEAR, (GENERIC, 'a == 192 && b == 168', 'a == 192 && b >= 168 && b <= 169'))
m("G1: widen 192.168 first octet (191, 193)", EG, NEAR, (GENERIC, 'a == 192 && b == 168', 'a >= 191 && a <= 193 && b == 168'))
m("G2: widen CGNAT down by one (100.63)", EG, NEAR, (GENERIC, 'b >= 64 && b <= 127', 'b >= 63 && b <= 127'))
m("G2: widen CGNAT up by one (100.128)", EG, NEAR, (GENERIC, 'b >= 64 && b <= 127', 'b >= 64 && b <= 128'))
m("G2: narrow CGNAT from below (100.64 passes)", EG, RED, (GENERIC, 'b >= 64 && b <= 127', 'b >= 65 && b <= 127'))
m("G2: narrow CGNAT from above (100.127 passes)", EG, RED, (GENERIC, 'b >= 64 && b <= 127', 'b >= 64 && b <= 126'))
m("G2: widen CGNAT first octet (99, 101)", EG, NEAR, (GENERIC, 'case a == 100 && b >= 64', 'case a >= 99 && a <= 101 && b >= 64'))
m("IPv4: drop the right boundary (five groups count as an address)", EG, NEAR, (GENERIC, 'if count != 4 || !valid {', 'if count < 4 || !valid {'))
m("IPv4: drop the group count (three groups count as an address)", EG, NEAR, (GENERIC, 'if count != 4 || !valid {', 'if count > 4 || !valid {'))
m("IPv4: drop octet validation (a group over 255 counts)", EG, NEAR, (GENERIC, 'if count != 4 || !valid {', 'if count != 4 || (!valid && false) {'))
m("IPv4: drop the group length check (a four-digit group counts)", EG, NEAR, (GENERIC, 'if i-gs > 3 || v > 255 {', 'if v > 255 {'))

# -------------------------------------------------------------------- G3 ----
m("G3: delete the unique-local rule", EG, RED, (GENERIC, 'case prefixULA.Contains(addr):', 'case false && prefixULA.Contains(addr):'))
m("G3: delete the link-local rule", EG, RED, (GENERIC, 'case prefixLinkLocal.Contains(addr):', 'case false && prefixLinkLocal.Contains(addr):'))
m("G3: widen the unique-local prefix by one bit", EG, NEAR, (GENERIC, '[16]byte{0xfc}), 7)', '[16]byte{0xf8}), 6)'))
m("G3: narrow the unique-local prefix by one bit", EG, RED, (GENERIC, '[16]byte{0xfc}), 7)', '[16]byte{0xfc}), 8)'))
m("G3: widen the link-local prefix by one bit", EG, NEAR, (GENERIC, '[16]byte{0xfe, 0x80}), 10)', '[16]byte{0xfe, 0x80}), 9)'))
m("G3: narrow the link-local prefix by one bit", EG, RED, (GENERIC, '[16]byte{0xfe, 0x80}), 10)', '[16]byte{0xfe, 0x80}), 11)'))
m("G3: do not trim a surrounding colon", EG, '^TestIPv6CandidateIsTrimmedOfSurroundingColons$',
  off(GENERIC, "e-s > 2 && data[s] == ':' && data[s+1] != ':'"))
m("G3: do not trim a trailing colon", EG, '^TestIPv6CandidateIsTrimmedOfSurroundingColons$',
  off(GENERIC, "e-s > 2 && data[e-1] == ':' && data[e-2] != ':'"))

# -------------------------------------------------------------------- G4 ----
m("G4: delete the certificate rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G4, "pem-certificate", rePEMCertificate, data)\n'))
m("G4: delete the private-key rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G4, "pem-private-key", rePEMPrivateKey, data)\n'))
m("G4: delete the kubeconfig rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G4, "kubeconfig-data", reKubeconfigData, data)\n'))
m("G4: delete the fingerprint rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G4, "cert-fingerprint", reFingerprint, data)\n'))
m("G4: private key only when unqualified (RSA, OPENSSH pass)", EG, RED, (GENERIC, '`BEGIN[ ](?:[A-Z0-9]+[ ])*PRIVATE[ ]KEY`', '`BEGIN[ ]PRIVATE[ ]KEY`'))
m("G4: fingerprint one pair shorter (15 pairs match)", EG, NEAR, (GENERIC, '{2}:){15,}', '{2}:){14,}'))
m("G4: kubeconfig value of any length (a placeholder matches)", EG, NEAR, (GENERIC, '[A-Za-z0-9+/]{16,}`', '[A-Za-z0-9+/]{4,}`'))

# -------------------------------------------------------------------- G5 ----
m("G5: delete the URL-credentials rule", EG, RED, drop(GENERIC, '\tms = scanURLCredentials(data, ms)\n'))
m("G5: reserved hosts are not let through", EG, NEAR, off(GENERIC, 'reservedHost(host)'))
for block in ['192.0.2.0/24', '198.51.100.0/24', '203.0.113.0/24', '127.0.0.0/8', '2001:db8::/32', '::1/128']:
    m("reserved block %s removed" % block, EG, NEAR, drop(GENERIC, '\tnetip.MustParsePrefix("%s"),\n' % block))

# -------------------------------------------------------------------- G6 ----
m("G6: delete the e-mail rule", EG, RED, drop(GENERIC, '\tms = scanEmail(data, ms, domainAllowed)\n'))
m("G6: reserved domains are not let through", EG, NEAR, off(GENERIC, 'reservedName(domain)'))
m("G6: the allowed-domain list is ignored", EG, NEAR, off(GENERIC, 'domainAllowed != nil && domainAllowed(domain)'))
m("G6: a numeric last label counts as a domain (module@v1.2.3)", EG, NEAR, (GENERIC, r'\.)+[A-Za-z]{2,})`', r'\.)+[A-Za-z0-9]{1,})`'))
for zone in ['example.com', 'example.org', 'example.net', 'example.internal', 'example', 'test', 'invalid', 'localhost']:
    m("reserved zone %s removed" % zone, EG, NEAR, (GENERIC, '"%s",' % zone, ''))

# -------------------------------------------------------------------- G7 ----
m("G7: delete the /home rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G7, "home-path", reHomePath, data)\n'))
m("G7: delete the /mnt/<drive>/Users rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G7, "mnt-users-path", reMntUsersPath, data)\n'))
m("G7: delete the /Users rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G7, "users-path", reUsersPath, data)\n'))
m("G7: delete the worktree rule", EG, RED, drop(GENERIC, '\tms = appendAll(ms, G7, "root-worktree-path", reRootWorktree, data)\n'))
m("G7: worktree rule matches any /root/<name>/", EG, NEAR, (GENERIC, '`/root/[A-Za-z0-9._-]+-wt/`', '`/root/[A-Za-z0-9._-]+/`'))

# -------------------------------------------------------- the engine --------
m("engine: skip paths", EG, RED, (GUARD, 'return g.scan(KindPath, path, "", []byte(path))', 'return nil'))
m("engine: skip the message scan", EG, RED, (GUARD, 'return g.scan(KindMessage, name, allowAs, data)', 'return nil'))
m("engine: skip content", EG, RED, (GUARD, 'return g.scan(KindContent, path, path, data)', 'return nil'))
m("engine: skip the specific rules", EG, RED, (GUARD, '\tfor i := range g.specific {', '\tfor i := range g.specific[:0] {'))
m("engine: a scoped rule applies everywhere", EG, '^TestScopedRuleAppliesOnlyUnderItsScope$',
  off(GUARD, 'kind == KindMessage || !strings.HasPrefix(name, r.scope)'))
m("engine: a scoped rule applies to messages", EG, '^TestScopedRuleAppliesOnlyUnderItsScope$',
  (GUARD, 'if kind == KindMessage || !strings.HasPrefix(name, r.scope) {', 'if !strings.HasPrefix(name, r.scope) && kind != KindMessage {'))
m("engine: a contained match is reported as well", EG, '^TestALongerMatchSuppressesTheOneInsideIt$', (GUARD, 'if !contained {', 'if !contained || true {'))
m("engine: upstream switches the generic rules off everywhere", EG, RED, (GUARD, '\tupstream := false\n', '\tupstream := true\n'))
m("engine: upstream is ignored (generic rules run under it)", EG, ALLOW2, (GUARD, 'if !upstream {', 'if !upstream || true {'))
m("engine: a message can be upstream", EG, ALLOW2, (GUARD, 'if kind != KindMessage {', 'if true {'))
m("engine: a finding is positioned on the wrong line", EG, RED, (GUARD, "line = 1 + bytes.Count(data[:off], []byte{'\\n'})", 'line = 1'))
m("engine: public mode loads no rules -- and internal mode neither", EG, RED, (GUARD, '\t\tg.specific = rules\n', '\t\t_ = rules\n'))

# ------------------------------------------------------ the allow-list ------
m("allow-list: the match is emptied (nothing is ever excused)", EG, ALLOW, off(DATA, 's.start <= m.start && m.end <= s.end'))
m("allow-list: a path entry applies in every file", EG, ALLOW, off(DATA, 'allowPath == "" || e.path != allowPath'))
m("allow-list: everything in the file of an entry is excused", EG, ALLOW, (DATA, 'if s.start <= m.start && m.end <= s.end {', 'if true {'))
m("allow-list: a stale entry is not reported", EG, ALLOW, (DATA, '\t\tif e.used {\n', '\t\tif true {\n'))
m("allow-list: an unused entry counts as used", EG, ALLOW2, (DATA, '\t\tif domain == e.literal || strings.HasSuffix(domain, "."+e.literal) {\n\t\t\te.used = true\n\t\t\tok = true\n\t\t}', '\t\te.used = true\n\t\tif domain == e.literal || strings.HasSuffix(domain, "."+e.literal) {\n\t\t\tok = true\n\t\t}'))
m("allow-list: a domain entry matches any suffix (notcorp.zz)", EG, ALLOW2, (DATA, 'strings.HasSuffix(domain, "."+e.literal)', 'strings.HasSuffix(domain, e.literal)'))
m("allow-list: an entry needs no reason", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 'strings.TrimSpace(e.reason) == ""'))
m("allow-list: a glob path is accepted", EG, '^TestDataIsRefusedWhenMalformed$',
  (DATA, 'strings.HasSuffix(e.path, "/") || strings.ContainsAny(e.path, "*?[\\\\") {', 'strings.HasSuffix(e.path, "/") {'))
m("allow-list: a wildcard domain is accepted", EG, '^TestDataIsRefusedWhenMalformed$',
  (DATA, 'strings.ContainsAny(e.literal, "*?[\\\\@/ ") || !strings.Contains(e.literal, ".")', '!strings.Contains(e.literal, ".")'))
m("allow-list: an empty literal is accepted", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 'e.kind != "upstream" && e.kind != "scanner-config" && e.literal == ""'))
m("allow-list: a duplicate is accepted", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 'seen[key]'))

# -------------------------------------------------------- the deny data -----
m("deny data: a class may have no rule", EG, '^TestDataIsRefusedWhenMalformed$', (DATA, '\t\tif !have[c] {', '\t\tif false {'))
m("deny data: a rule may match the empty string", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 're.Match(nil)'))
m("deny data: a generic class may be redefined", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, '!isSpecific(class)'))
m("deny data: a duplicate rule name is accepted", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 'names[name]'))

# ------------------------------------------------- the internal-only list ---
m("LIST: the data directory need not be listed", EG, '^TestListRefusesWhatItCannotMeanExactly$', off(LIST, '!seen[SelfDir]'))
m("LIST: globs are accepted", EG, '^TestListRefusesWhatItCannotMeanExactly$', (LIST, '\tcase strings.ContainsAny(e, "*?[]\\\\"):', '\tcase false:'))
m("LIST: an entry needs no reason", EG, '^TestListRefusesWhatItCannotMeanExactly$', off(LIST, '!found || strings.TrimSpace(reason) == ""'))
m("LIST: a duplicate is accepted", EG, '^TestListRefusesWhatItCannotMeanExactly$', off(LIST, 'seen[entry]'))
m("LIST: a nested entry is accepted", EG, '^TestListRefusesWhatItCannotMeanExactly$', off(LIST, 'a != b && strings.HasSuffix(b, "/") && strings.HasPrefix(a, b)'))
m("LIST: a dotted path is accepted", EG, '^TestListRefusesWhatItCannotMeanExactly$', off(LIST, 'seg == "" || seg == "." || seg == ".."'))
m("LIST: a stale entry is accepted", EG, '^TestListStaleEntryIsRed$', off(LIST, 'removed[e] == 0'))
m("LIST: a directory entry removes by bare prefix (.beadsX goes too)", EG, '^TestListApplyRemovesExactlyTheListedPaths$',
  (LIST, '\t\treturn strings.HasPrefix(path, entry)\n', '\t\treturn strings.HasPrefix(path, strings.TrimSuffix(entry, "/"))\n'))
m("LIST: a file entry removes by prefix (CLAUDE.md.bak goes too)", EG, '^TestListApplyRemovesExactlyTheListedPaths$',
  (LIST, '\treturn path == entry\n', '\treturn strings.HasPrefix(path, entry)\n'))
m("LIST: an entry may overlap a hashed or copied path", EG, '^TestListMayNotOverlapAHashedOrCopiedPath$', off(LIST, 'overlaps(e, p)'))
m("LIST: a stage copy counts as a build-context source", EG, '^TestDockerfileCopySources$', (LIST, '\t\t\t\tfromStage = true\n', ''))

# ------------------------------------------------- mode, EXPORT-SOURCE ------
m("mode: both present is accepted", EG, '^TestModeSwitch$', case_off(MODE, 'exportSourcePresent && dataDirPresent'))
m("mode: both absent is accepted", EG, '^TestModeSwitch$', case_off(MODE, '!exportSourcePresent && !dataDirPresent'))
m("mode: the switch is inverted", EG, '^TestModeSwitch$', (MODE, '\t\treturn ModePublic, nil', '\t\treturn ModeInternal, nil'))
m("EXPORT-SOURCE: the version line is not written", EG, '^TestExportSourceRendersCanonicallyAndRoundTrips$', drop(SOURCE, '\t\tb.WriteString("version: " + s.Version + "\\n")\n'))
m("EXPORT-SOURCE: a short commit id is accepted", EG, '^TestExportSourceRendersCanonicallyAndRoundTrips$', off(SOURCE, '!ValidCommitID(s.SourceCommit)'))
m("EXPORT-SOURCE: a non-canonical file is parsed", EG, '^TestExportSourceRendersCanonicallyAndRoundTrips$', off(SOURCE, 'string(want) != string(data)'))

# ----------------------------------------------------------- the tree -------
T_TREE = '^TestTreeIsTheSourceMinusTheListPlusExportSource$'
T_REF = '^TestTreeRefusals$'
T_PT = '^TestInternalOnlyPathsNeverReachTheTreeAndAStrayOneIsRefused$'
T_DENY = '^TestTheDenyDataCanNeverBeExported$'
m("tree: the listed paths are not removed", GE, T_TREE, (TREE, '"rm", "--cached", "-r", "-q", "--", entry', '"ls-files", "--cached", "-z", "--", entry'))
m("tree: EXPORT-SOURCE is not added", GE, T_TREE, (TREE, '"update-index", "--add", "--cacheinfo", "100644,"+blob+","+exportguard.ExportSourcePath', '"ls-files", "--cached", "-z"'))
m("tree: the version is not recorded", GE, T_TREE, (TREE, 'exportguard.ExportSource{Version: version, SourceCommit: src', 'exportguard.ExportSource{SourceCommit: src'))
m("tree: the list is not checked against hashed and copied paths", GE, T_REF, (TREE, 'list.CheckProtected(protected)', 'list.CheckProtected(protected[:0])'))
m("tree: the sealed chart is not protected", GE, T_REF, (TREE, 'protected := append([]string{}, exportguard.HashedTrees...)', 'protected := []string{}'))
m("tree: Dockerfile copy sources are not protected", GE, T_REF, (TREE, '\t\tfor _, s := range exportguard.DockerfileCopySources(data) {', '\t\tfor _, s := range exportguard.DockerfileCopySources(data[:0]) {'))
m("tree: a committed EXPORT-SOURCE is accepted", GE, T_REF, off(TREE, 'e.Path == exportguard.ExportSourcePath || strings.HasPrefix(e.Path, exportguard.ExportSourcePath+"/")'))
m("tree: a source that is not a full id is accepted", GE, T_REF, off(GIT, '!validID(src)'))
m("tree: a source that is not on origin's main is accepted", GE, T_REF, (GIT, '\tif !ok {\n\t\treturn refuse("source %s is not on origin/%s"', '\tif !ok && false {\n\t\treturn refuse("source %s is not on origin/%s"'))
m("tree: a bad version is accepted", GE, T_REF, off(TREE, 'version != "" && !exportguard.ValidVersion(version)'))
m("public tree: an internal-only path in the output is accepted", GE, T_PT, (TREE, '\t\tif list.Covers(e.Path) {\n\t\t\treturn refuse("internal-only path', '\t\tif false {\n\t\t\treturn refuse("internal-only path'))
m("public tree: a copy of the deny data is accepted", GE, T_PT, (TREE, 'if p, ok := dataBlobs[e.ID]; ok {', 'if p, ok := dataBlobs[e.ID]; ok && false {'))
m("public tree: a copy of the deny data is accepted (end to end)", GE, T_DENY, (TREE, 'if p, ok := dataBlobs[e.ID]; ok {', 'if p, ok := dataBlobs[e.ID]; ok && false {'))
m("public tree: the data files' blob ids are not collected", GE, T_DENY, off(TREE, 'e.Path == exportguard.ListPath || e.Path == exportguard.DenyPath'))
m("public tree: a non-file entry is accepted", GE, T_PT, off(TREE, 'e.Type != "blob"'))
m("public tree: the file count is not compared", GE, T_PT, off(TREE, 'len(tree) != len(want)'))
m("public tree: modes and blobs are not compared", GE, T_PT, off(TREE, 'w, ok := want[e.Path]; !ok || w != e'))
m("public tree: a tree without EXPORT-SOURCE is accepted", GE, T_PT,
  (TREE, 'if mode, err := exportguard.DetectMode(hasSource, hasData); err != nil {', 'if mode, err := exportguard.DetectMode(hasSource, hasData); err != nil && false {'))

# ---------------------------------------------------------- the check -------
T_12 = '^TestEachOfTheTwelveClassesIsRefused$'
T_CHK = '^TestCheckRefusals$'
m("check: blob contents are not scanned", GE, T_12, drop(TSCAN, '\ts.Findings = append(s.Findings, s.Guard.Content(path, data)...)\n'))
m("check: paths are not scanned", GE, '^TestAPathIsRefusedLikeContent$', drop(TSCAN, '\ts.Findings = append(s.Findings, s.Guard.Path(path)...)\n'))
m("check: findings do not refuse", GE, T_12, (CHECK, 'res.OK = len(res.Findings) == 0 && len(res.Problems) == 0', 'res.OK = len(res.Problems) == 0'))
m("check: problems do not refuse", GE, T_CHK, (CHECK, 'res.OK = len(res.Findings) == 0 && len(res.Problems) == 0', 'res.OK = len(res.Findings) == 0'))
m("check: scanner findings do not refuse", GE, T_CHK, (CHECK, ' && len(res.ScanFindings) == 0 && res.ScannerRan', ' && res.ScannerRan'))
m("check: scanner findings are dropped", GE, T_CHK, (CHECK, '\t\tres.ScanFindings = append(res.ScanFindings, fs...)\n', '\t\t_ = fs\n'))
m("check: a failing scanner is a pass", GE, T_CHK, (CHECK, '\t\tif err != nil {\n\t\t\treturn nil, fmt.Errorf("secret scanner: %v", err)', '\t\tif false {\n\t\t\treturn nil, fmt.Errorf("secret scanner: %v", err)'))
m("check: a guard-only check does not say the scanner was skipped", GE, T_CHK, (CHECK, '\tif opt.SkipScanner {\n\t\tres.Problems = append(', '\tif opt.SkipScanner {\n\t\t_ = append('))
m("check: stale allow-list entries are not reported", GE, T_CHK, drop(TSCAN, '\ts.Problems = append(s.Problems, s.Guard.Stale()...)\n'))
m("check: UTF-16 text is passed unread", GE, T_CHK, off(TSCAN, 'HasUTF16BOM(data)'))
m("check: a scanner configuration in the tree needs no entry", GE, T_CHK, off(TSCAN, '!s.Guard.AllowsScannerConfig(path)'))
m("check: a scanner ignore file in the tree is accepted", GE, T_CHK, (TSCAN, '\tcase ScannerIgnorePath:\n', '\tcase "no such file":\n'))
m("allow-list: scanner-config may name any file", EG, '^TestDataIsRefusedWhenMalformed$', off(DATA, 'e.path != ScannerConfigPath'))
m("check: the allow-list is not read from the source", GE, T_CHK, (CHECK, 'guard, err := exportguard.New(deny, allow)', 'guard, err := exportguard.New(deny, allow[:0])'))
m("check: the deny data is not read (generic rules only)", GE, T_12, (CHECK, 'guard, err := exportguard.New(deny, allow)', 'guard, err := exportguard.New(nil, append(allow, deny[:0]...))'))
m("check: a missing deny file is accepted", GE, T_CHK, (CHECK, '\tif !ok {\n\t\treturn nil, refuse("%s does not exist at %s", exportguard.DenyPath', '\tif !ok && false {\n\t\treturn nil, refuse("%s does not exist at %s", exportguard.DenyPath'))
m("check: the scanner is handed the SOURCE tree, internal paths and all", GE, T_CHK, (CHECK, 'dir, err := r.materialise(tr.Tree)', 'dir, err := r.materialise(tr.Source)'))
m("check: the materialised tree is left behind", GE, T_CHK, drop(CHECK, '\t\t_ = os.RemoveAll(dir)\n'))
m("check: files that mention an internal-only path are not counted", GE, T_CHK, drop(TSCAN, '\t\t\t\ts.DanglingRefs++\n'))
m("check: the shared scan's findings are dropped", GE, T_12, drop(CHECK, '\tres.Findings = append(res.Findings, scan.Findings...)\n'))
m("check: the shared scan's problems are dropped", GE, T_CHK, drop(CHECK, '\tres.Problems = append(res.Problems, scan.Problems...)\n'))
m("check: the shared scan is never finished (stale exemptions unreported)", GE, T_CHK, drop(CHECK, '\tscan.Finish()\n'))
m("check: paths are not handed to the shared scan", GE, '^TestAPathIsRefusedLikeContent$', (CHECK, '\t\tscan.Path(e.Path)\n', '\t\t_ = e\n'))
m("scanner: any binary is accepted as gitleaks", CLI, '^TestThePinnedScannerRefusesAnyOtherBinary$',
  (CHECK, 'got != gitleaksSHA256LinuxX6 {', 'got != gitleaksSHA256LinuxX6 && false {'))

# ------------------------------ the tree guard of the gate (ENFORCE) --------
T_MERGE = '^TestMergeScansAnswersLikeOneScan$'
T_TG12 = '^TestTheTreeGuardRefusesEachOfTheTwelveClasses$'
T_TGP = '^TestTheTreeGuardScansPathsAndSkipsOnlyTheList$'
T_TGPROB = '^TestTheTreeGuardReportsProblems$'
T_TGMODE = '^TestTheTreeGuardCannotBeTalkedOutOfItsMode$'
T_TGGIT = '^TestTheTreeGuardReadsTrackedAndUntrackedButNotIgnored$'
m("merge: an exemption unused by ONE part is stale", EG, T_MERGE, (TSCAN, 'if staleIn[st] == len(parts) {', 'if staleIn[st] >= 1 {'))
m("merge: a stale exemption is never reported", EG, T_MERGE, (TSCAN, 'if staleIn[st] == len(parts) {', 'if staleIn[st] == len(parts) && false {'))
m("merge: the findings are not put in order", EG, T_MERGE, drop(TSCAN, '\tsortFindings(out.Findings)\n'))
m("merge: the findings of the parts are dropped", EG, T_MERGE, drop(TSCAN, '\t\tout.Findings = append(out.Findings, p.Findings...)\n'))
m("merge: the problems of the parts are dropped", EG, T_MERGE, drop(TSCAN, '\t\tout.Problems = append(out.Problems, p.Problems...)\n'))
m("merge: the files are not counted", EG, T_MERGE, drop(TSCAN, '\t\tout.Files += p.Files\n'))
m("tree guard: internal-only paths are scanned too", BG, T_TGP, (PTREE, '\t\t\tif !list.Covers(f) {', '\t\t\tif true {'))
m("tree guard: a rotted list entry is not reported", BG, T_TGPROB, (PTREE, 'if _, _, err := list.Apply(files); err != nil {', 'if _, _, err := list.Apply(files); err != nil && false {'))
m("tree guard: paths are not scanned", BG, T_TGP, drop(PTREE, '\t\t\t\tparts[w].Path(kept[i])\n'))
m("tree guard: contents are not scanned", BG, T_TG12, (PTREE, '\t\t\t\tparts[w].Blob(kept[i], data)\n', '\t\t\t\t_ = data\n'))
m("tree guard: only the first worker's share is scanned", BG, T_TG12, (PTREE, '\tfor w := range parts {\n\t\twg.Add(1)', '\tfor w := range parts[:1] {\n\t\twg.Add(1)'))
m("tree guard: the internal tree is judged by the generic rules only", BG, T_TG12, (PTREE, 'return exportguard.New(deny, allow) }', 'return exportguard.New(nil, append(allow, deny[:0]...)) }'))
m("tree guard: the internal allow-list is not read", BG, T_TGPROB, (PTREE, 'return exportguard.New(deny, allow) }', 'return exportguard.New(deny, allow[:0]) }'))
m("tree guard: the public allow-list is not used", BG, T_TGPROB, (PTREE, 'return exportguard.New(nil, publicAllow) }', 'return exportguard.New(nil, nil) }'))
m("tree guard: a missing data file is read as empty", BG, T_TGMODE, (PTREE, '\t\tif !ok {\n\t\t\treturn nil, fmt.Errorf("%s is missing', '\t\tif !ok && false {\n\t\t\treturn nil, fmt.Errorf("%s is missing'))
m("tree guard: a tree that is both internal and exported is scanned", BG, T_TGMODE,
  (PTREE, '\tmode, err := exportguard.DetectMode(hasSource, hasData)\n', '\tmode, err := exportguard.DetectMode(hasSource && !hasData, hasData)\n'))
m("tree guard: a tree that is neither is scanned as public", BG, T_TGMODE,
  (PTREE, '\tmode, err := exportguard.DetectMode(hasSource, hasData)\n', '\tmode, err := exportguard.DetectMode(hasSource || !hasData, hasData)\n'))
m("tree guard: a malformed EXPORT-SOURCE is accepted", BG, T_TGPROB, (PTREE, 'if _, err := exportguard.ParseExportSource(src); err != nil {', 'if _, err := exportguard.ParseExportSource(src); err != nil && false {'))
m("tree guard: untracked files are not read", BG, T_TGGIT, (PTREE, '"--cached", "--others", "--exclude-standard")', '"--cached")'))
m("tree guard: ignored files are read", BG, T_TGGIT, (PTREE, '"--cached", "--others", "--exclude-standard")', '"--cached", "--others")'))
m("tree guard: a hook's GIT_DIR redirects the listing", BG, T_TGGIT, (PTREE, '\t\t\tif !strings.HasPrefix(kv, "GIT_") {', '\t\t\tif true {'))
m("tree guard: a tracked file deleted from the working tree is an error", BG, T_TGGIT, (PTREE, '\tcase errors.Is(err, fs.ErrNotExist):\n\t\treturn nil, false, nil', '\tcase errors.Is(err, fs.ErrNotExist):\n\t\treturn nil, false, err'))
m("tree guard: the stale exemptions of the merge are dropped", BG, T_TGPROB, (PTREE, '\trep.Problems = append(rep.Problems, scan.Problems...)\n\treturn rep, nil', '\treturn rep, nil'))
m("tree guard: the public allow-list drifts from the internal one", BG, '^TestThePublicAllowListIsTheInternalOne$',
  (PTREE, '\t\t{"domain", "berri.ai", "an upstream security contact, in the upstream report under docs/upstream/"},\n', ''))
m("gate: the guard step is taken out of `make gate`", BG, '^TestTheGateRunsTheTreeGuard$', ('Makefile', 'gate: toolchain fmt vet guard test goldens lint', 'gate: toolchain fmt vet test goldens lint'))
m("gate: the guard step runs after the tests", BG, '^TestTheGateRunsTheTreeGuard$', ('Makefile', 'gate: toolchain fmt vet guard test goldens lint', 'gate: toolchain fmt vet test guard goldens lint'))
m("gate: the guard step is softened", BG, '^TestTheGateRunsTheTreeGuard$', ('Makefile', "-run '^TestPublicTreeHasNoInternalDetail$$' -count=1\n", "-run '^TestPublicTreeHasNoInternalDetail$$' -count=1 || true\n"))

# ------------------------------------------ the pre-push hook (ENFORCE) -----
HGUARD = '.beads/hooks/pre-push-public-guard'
HPUSH = '.beads/hooks/pre-push'
T_HURL = '^TestPrePushGuardRefusesTheInternalGraphOnThePublicHost$'
T_HSNAP = '^TestPrePushGuardAcceptsOnlySnapshots$'
T_HCHAIN = '^TestPrePushHookRunsTheGuardFirstAndPassesItsInputOn$'
m("hook: a subdomain of the public host is not recognised", REL, T_HURL, (HGUARD, '  "$PUBLIC_HOST" | *."$PUBLIC_HOST") ;;', '  "$PUBLIC_HOST") ;;'))
m("hook: the public host is matched by bare suffix", REL, T_HURL, (HGUARD, '  "$PUBLIC_HOST" | *."$PUBLIC_HOST") ;;', '  *"$PUBLIC_HOST") ;;'))
m("hook: the host comparison is case-sensitive", REL, T_HURL, (HGUARD, "lc=$(printf '%s' \"$url\" | tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz')", 'lc=$url'))
m("hook: a user name hides the host", REL, T_HURL, drop(HGUARD, 'host=${host##*@}\n'))
m("hook: a port hides the host", REL, T_HURL, drop(HGUARD, 'host=${host%%:*}\n'))
m("hook: a local path with a colon is taken for a host", REL, T_HURL, (HGUARD, ' case "$host" in */*) host="" ;; esac ;;', ' ;;'))
m("hook: the scp-like form is not recognised", REL, T_HURL, (HGUARD, '  *:*) host=${lc%%:*};', '  *:*) host="";'))
m("hook: a deletion on the public host is let through", REL, T_HSNAP, (HGUARD, '    *) refuse "$remote_ref would be DELETED on the public host" ;;', '    *) continue ;;'))
m("hook: any tree trailer will do", REL, T_HSNAP, (HGUARD, 'if [ "$named" != "$tree" ]; then', 'if [ -z "$named" ]; then'))
m("hook: only the tip is looked at", REL, T_HSNAP, (HGUARD, 'for c in $(git rev-list "$commit") ; do', 'for c in "$commit" ; do'))
m("hook: only the first ref is looked at", REL, T_HSNAP, (HGUARD, '  done\ndone\n', '  done\n  exit 0\ndone\n'))
m("hook: something that is not a commit is let through", REL, T_HSNAP, (HGUARD, '    refuse "$local_ref ($local_id) is not a commit"', '    continue'))
m("hook: the public host is never refused", REL, T_HURL, (HGUARD, '  echo >&2 "pre-push: nothing was pushed."\n  exit 1', '  echo >&2 "pre-push: nothing was pushed."\n  exit 0'))
m("hook chain: a refusing guard does not stop the push", REL, T_HCHAIN, (HPUSH, 'sh "$_gonk_guard" "$@" || exit 1', 'sh "$_gonk_guard" "$@" || true'))
m("hook chain: a missing guard lets every push through", REL, T_HCHAIN, (HPUSH, 'so the push is refused"\n  exit 1', 'so the push is refused"\n  exit 0'))
m("hook chain: the guard is not given git's arguments", REL, T_HCHAIN, (HPUSH, 'sh "$_gonk_guard" "$@" || exit 1', 'sh "$_gonk_guard" || exit 1'))
m("hook chain: git's input is not handed on", REL, T_HCHAIN, (HPUSH, 'exec <<_GONK_PRE_PUSH_INPUT_\n$_gonk_input\n_GONK_PRE_PUSH_INPUT_\n', 'exec </dev/null\n'))
m("hook chain: the guard does not get git's input", REL, T_HCHAIN, (HPUSH, 'printf \'%s\\n\' "$_gonk_input" | sh', "printf '' | sh"))

# --------------------------------------------------------- the commit -------
T_CR = '^TestCommitRefusals$'
T_CB = '^TestCommitBuildsTheSnapshotFromTheFixedTemplate$'
T_DET = '^TestTheExportIsDeterministic$'
T_SNAP = '^TestSnapshotAssertions$'
m("commit: a failed check does not stop the commit", GE, T_CR, off(COMMIT, '!chk.OK'))
m("commit: a guard-only check is accepted", GE, T_CR, off(COMMIT, 'opt.Check.SkipScanner'))
m("commit: the messages are not scanned", GE, T_CR, off(COMMIT, 'len(bad) > 0'))
m("commit: the commit message is not scanned (only the tag's)", GE, T_CR,
  drop(COMMIT, '\tbad = append(bad, chk.guard.Message("commit-message", []byte(res.Message), NotesPath(opt.Version))...)\n'))
m("commit: missing release notes are accepted", GE, T_CR, (COMMIT, '\tif !ok {\n\t\treturn res, refuse("%s does not exist at %s; a release needs its notes"', '\tif !ok && false {\n\t\treturn res, refuse("%s does not exist at %s; a release needs its notes"'))
m("commit: a short parent id is accepted", GE, T_CR, off(COMMIT, '!opt.Root && !validID(opt.Parent)'))
m("commit: a root may be given a parent (it is silently dropped)", GE, T_CR, off(COMMIT, 'opt.Root && opt.Parent != ""'))
m("commit: a missing parent makes a root without the declaration", GE, T_CR,
  (COMMIT, 'if !opt.Root && !validID(opt.Parent) {', 'if !opt.Root && opt.Parent != "" && !validID(opt.Parent) {'),
  (COMMIT, '\tif !opt.Root {\n\t\tcommitTree = append(commitTree, "-p", opt.Parent)', '\tif opt.Parent != "" {\n\t\tcommitTree = append(commitTree, "-p", opt.Parent)'),
  (COMMIT, '\tif !opt.Root {\n\t\tif opt.Mirror != "" {', '\tif opt.Parent != "" {\n\t\tif opt.Mirror != "" {'),
  (COMMIT, '\tif root {\n\t\treturn r.assertRootSnapshot(commit, tree)', '\tif root || parent == "" {\n\t\treturn r.assertRootSnapshot(commit, tree)'))
m("commit: the dates are not forced to UTC", GE, T_CB, (COMMIT, 'stamp := date + " +0000"', 'stamp := date + " +0200"'))
m("commit: the date is the author date, not the committer date", GE, T_DET, (COMMIT, '"--format=%ct"', '"--format=%at"'))
T_ROOTCHILD = '^TestARootAndAChildOfTheSameSourceDifferOnlyInTheCommit$'
T_ROOT = '^TestRootSnapshotAssertions$'
m("commit: a child snapshot has no parent", GE, T_ROOTCHILD,
  (COMMIT, '\tif !opt.Root {\n\t\tcommitTree = append(commitTree, "-p", opt.Parent)', '\tif false {\n\t\tcommitTree = append(commitTree, "-p", opt.Parent)'),
  (COMMIT, '\tif root {\n\t\treturn r.assertRootSnapshot(commit, tree)', '\tif true {\n\t\treturn r.assertRootSnapshot(commit, tree)'))
m("commit: a child snapshot is held to the root shape", GE, T_ROOTCHILD,
  (COMMIT, '\tif root {\n\t\treturn r.assertRootSnapshot(commit, tree)', '\tif true {\n\t\treturn r.assertRootSnapshot(commit, tree)'))
m("commit: a root snapshot is held to the child shape", GE, T_CB,
  (COMMIT, '\tif root {\n\t\treturn r.assertRootSnapshot(commit, tree)', '\tif false {\n\t\treturn r.assertRootSnapshot(commit, tree)'))
m("commit: a root snapshot is given the internal source as its parent", GE, T_CB,
  (COMMIT, '\tif !opt.Root {\n\t\tcommitTree = append(commitTree, "-p", opt.Parent)\n\t}', '\tcommitTree = append(commitTree, "-p", src)'),
  (COMMIT, '\tif root {\n\t\treturn r.assertRootSnapshot(commit, tree)', '\tif false {\n\t\treturn r.assertRootSnapshot(commit, tree)'),
  (COMMIT, 'if err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {', 'if err := r.assertShape(res.Commit, src, tr.Tree, opt.Root); err != nil {'))
m("commit: a root's bundle is thin against a parent it does not have", GE, T_CB,
  (COMMIT, '\t\tif !opt.Root {\n\t\t\tcreate = append(create, "^"+opt.Parent)', '\t\tif true {\n\t\t\tcreate = append(create, "^"+opt.Parent)'))
m("commit: the result does not say it is a root", GE, T_ROOTCHILD, (COMMIT, 'Parent: opt.Parent, Root: opt.Root}', 'Parent: opt.Parent}'))
m("root snapshot: the commit need not carry the checked tree", GE, T_ROOT, off(COMMIT, 'have != tree'))
m("root snapshot: a parent is accepted", GE, T_ROOT, off(COMMIT, 'f := strings.Fields(out); len(f) != 1 || f[0] != commit'))
m("root snapshot: the parent check is skipped by the snapshot assertion", GE, T_ROOT,
  (COMMIT, '\tif err := r.assertHasNoParent(commit); err != nil {\n\t\treturn err\n\t}\n\treturn r.assertHistoryIsOnlyItself(commit)', '\treturn nil'))
m("root snapshot: a history longer than itself is accepted", GE, T_ROOT, off(COMMIT, 'history != commit'))
m("commit: the identity is another one", GE, T_DET, (IDENT, 'IdentityName  = "Steven Wagner"', 'IdentityName  = "Somebody Else"'))
m("commit: the template loses the tree trailer", GE, T_CB, drop(COMMIT, '\tb.WriteString("Gonk-Export-Tree: " + tree + "\\n")\n'))
m("commit: contributors are not credited", GE, '^TestCommitCreditsContributorsFromTheNotes$', (COMMIT, '\tfor _, c := range coAuthors {', '\tfor _, c := range coAuthors[:0] {'))
m("determinism: plumbing sees the caller's environment and git configuration", GE, T_DET,
  (GIT, '\treturn append(env, extra...)\n', '\treturn append(append(env, os.Environ()...), extra...)\n'))
m("determinism: remote commands see the caller's GIT_DIR and GIT_INDEX_FILE", GE, T_DET,
  (GIT, '\t\tif drop[name] {', '\t\tif drop[name] && false {'))
m("determinism: the user's git configuration is read", GE, T_DET,
  (GIT, '\t\t"HOME=" + filepath.Join(r.root, "home"),\n\t\t"GIT_CONFIG_NOSYSTEM=1",\n\t\t"GIT_CONFIG_GLOBAL=" + os.DevNull,\n', '\t\t"HOME=" + os.Getenv("HOME"),\n'))
m("snapshot: the commit need not carry the checked tree", GE, T_SNAP, off(COMMIT, 'got != tree'))
m("snapshot: rev-list C --not PARENT need not be exactly C", GE, T_SNAP, off(COMMIT, 'out != commit'))
m("snapshot: the history need not be the parent's plus one", GE, T_SNAP, off(COMMIT, 'cn != pn+1'))
m("snapshot: the parent need not be an ancestor", GE, T_SNAP, (COMMIT, '\tif !ok {\n\t\treturn refuse("parent %s is not an ancestor', '\tif !ok && false {\n\t\treturn refuse("parent %s is not an ancestor'))

# -------------------------------------------------------- the publish -------
T_PUB = '^TestPublishIsFencedAndPushesExactlyOneObject$'
T_PR = '^TestPublishRefusals$'
m("publish: pushes without the confirmation", GE, T_PUB, off(PUBLISH, 'opt.Confirm != res.Object'))
m("publish: any non-empty confirmation will do", GE, T_PUB, (PUBLISH, 'if opt.Confirm != res.Object {', 'if opt.Confirm == "" {'))
m("publish: no mirror is accepted", GE, T_PR, off(PUBLISH, 'strings.TrimSpace(opt.Mirror) == ""'))
m("publish: a remote mirror needs no release variable", GE, T_PR, off(PUBLISH, '!isLocalDir(opt.Mirror) && os.Getenv(PublishEnv) != opt.Version'))
m("publish: any value of the release variable will do", GE, T_PR, (PUBLISH, 'os.Getenv(PublishEnv) != opt.Version', 'os.Getenv(PublishEnv) == ""'))
m("publish: every mirror counts as a local stand-in", GE, T_PR, (PUBLISH, '\treturn err == nil && st.IsDir()\n', '\treturn true || (err == nil && st.IsDir())\n'))
m("publish: a relative path counts as a local stand-in", GE, T_PR, (PUBLISH, '\tif !filepath.IsAbs(mirror) {', '\tif !filepath.IsAbs(mirror) && false {'))
m("publish: a tree other than the reviewed one is pushed", GE, T_PR, off(PUBLISH, 'cr.Tree != opt.ReviewedTree'))
m("publish: the reviewed tree is optional", GE, T_PR, off(PUBLISH, '!validID(opt.ReviewedTree)'))
m("publish: a mirror that moved is pushed to", GE, T_PR, off(PUBLISH, 'head != opt.Parent'))
m("publish: a root may name a parent", GE, T_PR, case_off(PUBLISH, 'opt.Root && opt.Parent != ""'))
m("publish: a root may name a previous source", GE, T_PR, case_off(PUBLISH, 'opt.Root && opt.PrevSource != ""'))
m("publish: a parent is trusted without a previous source (the retired baseline)", GE, T_PR, case_off(PUBLISH, 'opt.PrevSource == ""'))
m("publish: a later release is attempted on an empty mirror", GE, T_PR, off(PUBLISH, 'head == ""'))
m("publish: a root is pushed to a mirror that is not empty", GE, T_PR,
  (PUBLISH, 'if err := r.assertMirrorEmpty(opt.Mirror); err != nil {', 'if err := r.assertMirrorEmpty(opt.Mirror); err != nil && false {'))
m("publish: a mirror with no main counts as empty", GE, T_PR, (PUBLISH, '\tif len(refs) == 0 {\n\t\treturn nil\n\t}', '\tif main == "" {\n\t\treturn nil\n\t}'))
m("publish: only the mirror's main is looked at (other refs are not listed)", GE, T_PR,
  (PUBLISH, '\t\trefs = append(refs, f[1])\n\t\tif f[1] == "refs/heads/main" {\n\t\t\tmain = f[0]\n\t\t}', '\t\tif f[1] == "refs/heads/main" {\n\t\t\tmain = f[0]\n\t\t\trefs = append(refs, f[1])\n\t\t}'))
m("publish: a root does not say a previous export exists", GE, T_PUB, (PUBLISH, '\t\t} else if ok {\n\t\t\tfrom :=', '\t\t} else if ok && false {\n\t\t\tfrom :='))
m("publish: a root is not declared to the commit", GE, T_PUB, (PUBLISH, 'CommitOptions{Version: opt.Version, Root: opt.Root, Check: opt.Check}', 'CommitOptions{Version: opt.Version, Check: opt.Check}'))
m("publish: a failed check does not stop the publish", GE, T_PR, (PUBLISH, '\tif err != nil {\n\t\treturn res, err\n\t}\n\tif cr.Tree != opt.ReviewedTree {', '\tif err != nil && !IsRefused(err) {\n\t\treturn res, err\n\t}\n\tif cr.Tree != opt.ReviewedTree {'))
m("publish: an unknown ref is accepted", GE, T_PR, off(PUBLISH, 'opt.Ref != "main" && opt.Ref != "tag"'))
m("publish: the tag may be pushed before the commit", GE, T_PUB, off(PUBLISH, 'head != cr.Commit'))
m("publish: an existing tag is pushed over", GE, T_PUB, off(PUBLISH, 'existing != ""'))
m("publish: the previous source may be the source", GE, T_PUB, off(PUBLISH, 'opt.PrevSource == src'))
m("publish: releases may move backwards", GE, T_PUB, (PUBLISH, '\t\tif !ok {\n\t\t\treturn res, refuse("source %s does not descend', '\t\tif !ok && false {\n\t\t\treturn res, refuse("source %s does not descend'))
m("publish: the parent need not be the previous export", GE, T_PUB,
  (PUBLISH, 'if _, err := r.verifyCommit(opt.PrevSource, opt.Parent); err != nil {', 'if _, err := r.verifyCommit(opt.PrevSource, opt.Parent); err != nil && false {'))
m("publish: a later release needs no previous source", GE, T_PUB, case_off(PUBLISH, 'opt.PrevSource == ""'),
  (PUBLISH, '\tcase !validID(opt.PrevSource):', '\tcase opt.PrevSource != "" && !validID(opt.PrevSource):'))
m("publish: the tag ref receives the commit, not the tag object", GE, T_PUB, (PUBLISH, 'res.Object, target = cr.Tag, tagRef', 'res.Object, target = cr.Commit, tagRef'))
m("only publish pushes: a second push elsewhere", GE, '^TestOnlyPublishPushes$',
  (COMMIT, '\tif err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {\n\t\treturn res, err\n\t}\n',
   '\tif err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {\n\t\treturn res, err\n\t}\n\tif opt.Mirror != "" {\n\t\t_, _ = netGit(r.dir, "push", "--", opt.Mirror, res.Commit+":refs/heads/main")\n\t}\n'))
m("commit pushes to the mirror (behavioural: the mirror must not move)", GE, '^TestRecordAChildSnapshot$',
  (COMMIT, '\tif err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {\n\t\treturn res, err\n\t}\n',
   '\tif err := r.assertShape(res.Commit, opt.Parent, tr.Tree, opt.Root); err != nil {\n\t\treturn res, err\n\t}\n\tif opt.Mirror != "" {\n\t\t_, _ = netGit(r.dir, "push", "--", opt.Mirror, res.Commit+":refs/heads/main")\n\t}\n'))

# ------------------------------------------- write, record (origin) ---------
T_WT = '^TestWriteTreeWritesExactlyThePublicTree$'
T_WR = '^TestWriteTreeRefusals$'
T_REC = '^TestRecordPushesTheExportRefToOriginOnly$'
T_RPUB = '^TestRecordRefusesAPublicOrigin$'
m("write: a relative directory is accepted", GE, T_WR, off(TREE, '!filepath.IsAbs(dir)'))
m("write: a file is accepted as the directory", GE, T_WR, (TREE, '\tcase err == nil && !st.IsDir():', '\tcase err == nil && !st.IsDir() && false:'))
m("write: a directory that is not empty is written into", GE, T_WR, off(TREE, 'len(entries) != 0'))
m("write: the directory is not compared with the tree", GE, T_WR, off(TREE, 'got := strings.TrimSpace(string(out)); got != tree'))
m("write: ignored files are left out of the comparison", GE, T_WT, (TREE, '"add", "--all", "--force"); err != nil {', '"add", "--all"); err != nil {'))
m("write: the SOURCE tree is written, internal paths and all", GE, T_WT, (TREE, 'if _, err := r.gitWith(index, "read-tree", tr.Tree); err != nil {', 'if _, err := r.gitWith(index, "read-tree", tr.Source); err != nil {'),
  (TREE, '\treturn r.verifyWritten(dir, tr.Tree)\n', '\treturn nil\n'))
m("record: an origin on the public host is accepted", GE, T_RPUB, off(RECORD, 'isPublicHost(r.origin)'))
m("record: the public host is matched by bare suffix (notgithub.com)", GE, T_RPUB, (RECORD, 'strings.HasSuffix(host, "."+publicHost)', 'strings.HasSuffix(host, publicHost)'))
m("record: a subdomain of the public host is not recognised", GE, T_RPUB, (RECORD, 'host == publicHost || strings.HasSuffix(host, "."+publicHost)', 'host == publicHost'))
m("record: the host comparison is case-sensitive", GE, T_RPUB, (RECORD, '\thost = strings.ToLower(host)\n', ''))
m("record: a user name hides the host", GE, T_RPUB, (RECORD, '\tif i := strings.LastIndex(rest, "@"); i >= 0 {', '\tif i := strings.LastIndex(rest, "@"); i >= 0 && false {'))
m("record: pushes without the confirmation", GE, T_REC, off(RECORD, 'opt.Confirm != res.Object'))
m("record: any non-empty confirmation will do", GE, T_REC, (RECORD, 'if opt.Confirm != res.Object {', 'if opt.Confirm == "" {'))
m("record: an export ref is moved", GE, T_REC, off(RECORD, 'existing != ""'))
m("record: an existing identical ref is not recognised", GE, T_REC, off(RECORD, 'existing == res.Object'))
m("record: the ref is a branch", GE, T_REC, (RECORD, 'return "refs/export/" + version', 'return "refs/heads/export-" + version'))
m("record: a failed check does not stop the record", GE, T_REC, (RECORD, '\tif err != nil {\n\t\treturn res, err\n\t}\n\tres.Object = cr.Commit', '\tif err != nil && !IsRefused(err) {\n\t\treturn res, err\n\t}\n\tres.Object = cr.Commit'))
m("record: the root declaration is dropped", GE, T_REC, (RECORD, 'Parent: opt.Parent, Root: opt.Root, Mirror: opt.Mirror', 'Parent: opt.Parent, Mirror: opt.Mirror'))
m("record: pushes to the mirror instead of origin", GE, '^TestRecordAChildSnapshot$',
  (RECORD, '"push", "--quiet", "--", r.origin, res.Object+":"+res.Ref', '"push", "--quiet", "--", opt.Mirror, res.Object+":refs/heads/main"'))
m("only publish and record push: a second push in record", GE, '^TestOnlyPublishPushes$',
  (RECORD, '\tres.Pushed = true\n', '\t_, _ = netGit(r.dir, "push", "--", r.origin, res.Object+":refs/heads/main")\n\tres.Pushed = true\n'))

# --------------------------------------------------------- the verify -------
T_VER = '^TestVerifyRefusesWhatIsNotTheExport$'
m("verify: a different tree is called equal", GE, T_VER, off(VERIFY, '!res.Equal'))
m("verify: a commit exported from another source is accepted", GE, T_VER, off(VERIFY, 'es.SourceCommit != src'))
m("verify: a commit without EXPORT-SOURCE is accepted", GE, T_VER, (VERIFY, '\tif !ok {\n\t\treturn res, refuse("%s has no %s: it is not an export"', '\tif !ok && false {\n\t\treturn res, refuse("%s has no %s: it is not an export"'))
m("verify: the source trailer is not checked", GE, T_VER, off(VERIFY, 'got := trailer(msg, "Gonk-Source-Commit"); got != src'))
m("verify: the tree trailer is not checked", GE, T_VER, off(VERIFY, 'got := trailer(msg, "Gonk-Export-Tree"); got != res.PublicTree'))
m("verify: the recomputation ignores the recorded version", GE, T_PUB, (VERIFY, 'tr, err := r.Tree(src, es.Version)', 'tr, err := r.Tree(src, "")'))
m("verify: no mirror is accepted", GE, T_VER, off(VERIFY, 'mirror == ""'))

# -------------------------------------------------------- the command -------
T_E2E = '^TestCommitVerifyAndPublishEndToEnd$'
T_CLI = '^TestCheckExitStatusAndOutput$'
T_USE = '^TestUsageErrorsExitTwo$'
m("command: a snapshot may be neither root nor child, or both", CLI, T_USE, off(MAIN, 'root == (parent != "")'))
m("command: commit ignores --root", CLI, T_E2E, (MAIN, 'Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, OutDir: *c.out,', 'Version: *version, Parent: *parent, Mirror: *mirror, OutDir: *c.out,'))
m("command: publish ignores --root", CLI, T_E2E, (MAIN, 'Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, ReviewedTree: *reviewed,', 'Version: *version, Parent: *parent, Mirror: *mirror, ReviewedTree: *reviewed,'))
m("command: publish ignores --prev-src", CLI, T_E2E, (MAIN, 'PrevSource: *prev, Ref: *ref, Confirm: *confirm,', 'PrevSource: strings.Repeat(*prev, 0), Ref: *ref, Confirm: *confirm,'))
m("command: record ignores --root", CLI, '^TestRecordThroughTheCommand$', (MAIN, 'Version: *version, Parent: *parent, Root: *root, Mirror: *mirror, Confirm: *confirm,', 'Version: *version, Parent: *parent, Mirror: *mirror, Confirm: *confirm,'))
m("command: record ignores --confirm", CLI, '^TestRecordThroughTheCommand$', (MAIN, 'Root: *root, Mirror: *mirror, Confirm: *confirm,', 'Root: *root, Mirror: *mirror, Confirm: strings.Repeat(*confirm, 0),'))
m("command: a record dry run exits 0", CLI, '^TestRecordThroughTheCommand$',
  (MAIN, '\t\tsay(stdout, "would-push %s\\nto %s\\non %s\\n", nc.Object, nc.Target, nc.Mirror)\n\t\tsay(stderr, "%s: %v\\n", c.fs.Name(), err)\n\t\treturn exitDryRun\n\t}\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif !c.report(res, stdout, stderr) {\n\t\treturn exitRefused\n\t}\n\tif res.Already {',
   '\t\tsay(stdout, "would-push %s\\nto %s\\non %s\\n", nc.Object, nc.Target, nc.Mirror)\n\t\tsay(stderr, "%s: %v\\n", c.fs.Name(), err)\n\t\treturn exitOK\n\t}\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif !c.report(res, stdout, stderr) {\n\t\treturn exitRefused\n\t}\n\tif res.Already {'))
m("command: write does not need --dir", CLI, '^TestWriteWritesThePublicTreeIntoADirectory$', off(MAIN, '*dir == ""'))
m("command: write ignores --version", CLI, '^TestWriteWritesThePublicTreeIntoADirectory$', (MAIN, '\ttr, err := repo.Tree(*c.src, *version)\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif err := repo.WriteTree(tr, abs)', '\ttr, err := repo.Tree(*c.src, strings.Repeat(*version, 0))\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif err := repo.WriteTree(tr, abs)'))
m("command: a publish dry run exits 0", CLI, T_E2E,
  (MAIN, '\t\tsay(stderr, "%s: %v\\n", c.fs.Name(), err)\n\t\treturn exitDryRun\n\t}\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif !c.report(res, stdout, stderr) {\n\t\treturn exitRefused\n\t}\n\tsay(stdout, "pushed %s\\nto %s\\non %s\\n", res.Object, map',
   '\t\tsay(stderr, "%s: %v\\n", c.fs.Name(), err)\n\t\treturn exitOK\n\t}\n\tif err != nil {\n\t\treturn fail(c.fs.Name(), err, stderr)\n\t}\n\tif !c.report(res, stdout, stderr) {\n\t\treturn exitRefused\n\t}\n\tsay(stdout, "pushed %s\\nto %s\\non %s\\n", res.Object, map'))
m("command: a refused check exits 0", CLI, T_CLI, (MAIN, '\tsay(stderr, "check: REFUSED\\n")\n\treturn exitRefused', '\tsay(stderr, "check: REFUSED\\n")\n\treturn exitOK'))
m("command: guard-only exits 0 whatever the guard found", CLI, T_CLI, (MAIN, 'case *guardOnly && chk.GuardClean():', 'case *guardOnly:'))
m("command: the full check exits 0 when only the guard is clean", CLI, T_CLI, (MAIN, 'case *guardOnly && chk.GuardClean():', 'case chk.GuardClean():'))
m("command: a missing scanner is skipped silently", CLI, T_CLI, (MAIN, '\t\ts, err := c.scanner()\n\t\tif err != nil {\n\t\t\treturn fail(c.fs.Name(), err, stderr)\n\t\t}\n\t\topt.Scanner = s\n', '\t\ts, err := c.scanner()\n\t\tif err != nil {\n\t\t\topt.SkipScanner = true\n\t\t}\n\t\topt.Scanner = s\n'))
m("command: missing publish flags are not a usage error", CLI, '^TestUsageErrorsExitTwo$', off(MAIN, '*version == "" || *mirror == "" || *reviewed == ""'))

# ------------------------------------------------------ the real data -------
T_REAL = '^TestEachRealSpecificRuleIsSeenRed$'
T_RNEAR = '^TestRealRulesLeaveTheReservedValuesAndNearMissesGreen$'


# The deny data spells the internal names and this script is published, so
# the mutations below are DERIVED from the file, never written out: every rule
# line is deleted in turn; every rule with a word boundary loses it; every
# rule with an escaped dot loses the escape; every scoped rule loses its scope.
def deny_rules():
    out = []
    for line in open(DENY).read().split('\n'):
        f = line.split()
        if len(f) >= 4 and not line.startswith('#'):
            out.append((f[0], line))
    return out


for n, (cls, line) in enumerate(deny_rules()):
    m("real data: delete %s rule #%d" % (cls, n), REL, T_REAL, drop(DENY, line + '\n'))
    if '\\b' in line:
        m("real data: %s rule #%d loses its word boundaries" % (cls, n), REL, T_RNEAR, (DENY, line, line.replace('\\b', '')))
    if '\\.' in line:
        m("real data: %s rule #%d loses its escaped dot" % (cls, n), REL, T_RNEAR, (DENY, line, line.replace('\\.', '.')))
    if line.split()[2] != '*':
        scope = line.split()[2]
        m("real data: %s rule #%d applies outside its scope" % (cls, n), REL, T_RNEAR,
          (DENY, line, line.replace(' ' + scope + ' ', ' * ', 1)))
        m("real data: %s rule #%d matches prose, not only a value" % (cls, n), REL, T_RNEAR,
          (DENY, line, line.replace('(?m):[ \\t]*', '(?m)', 1).replace('[ \\t]*(#.*)?$', '', 1)))
m("real data: the list no longer removes the data directory", REL, '^TestTheRealListLoadsAndNamesOnlyWhatExists$',
  (RLIST, 'release/                                # this list', 'docs/adr/                               # this list'))
m("real data: Q2 (b) opens the whole zone", REL, '^TestQ2AlternativeIsADataChange$',
  ('release/release_test.go', '"anywhere\\tgonk.', '"anywhere\\t'))

if '--list' in sys.argv[1:]:
    for i, (name, _, pkg, run) in enumerate(muts):
        print(f"[{i}] {name}    {pkg} -run {run}")
    sys.exit(0)

show = '--show' in sys.argv[1:]
only = [a for a in sys.argv[1:] if a != '--show']
res = []
for i, (name, edits, pkg, run) in enumerate(muts):
    if only and str(i) not in only:
        continue
    originals = {}
    vacuous = False
    for f, _, _ in edits:
        if f not in originals:
            originals[f] = open(f).read()
    cur = dict(originals)
    for f, old, new in edits:
        if cur[f].count(old) != 1:
            vacuous = True
            break
        cur[f] = cur[f].replace(old, new, 1)
    if vacuous or cur == originals:
        res.append(f"VACUOUS  [{i}] {name}")
        print(res[-1], flush=True)
        continue
    out = ''
    try:
        for f, s in cur.items():
            open(f, 'w').write(s)
        r = subprocess.run([GO, "test", pkg, "-run", run, "-count=1"], capture_output=True, text=True)
        out = r.stdout + r.stderr
        fails = [l.strip() for l in out.splitlines() if l.strip().startswith("--- FAIL")]
        if r.returncode == 0:
            res.append(f"SURVIVED [{i}] {name}")
        elif not fails:
            res.append(f"NOCOMPILE [{i}] {name}\n{out[:800]}")
        else:
            res.append(f"KILLED   [{i}] {name}\n           " + "\n           ".join(fails[:4]))
    finally:
        for f, s in originals.items():
            open(f, 'w').write(s)
    print(res[-1], flush=True)
    if show:
        print(out, flush=True)
killed = len([r for r in res if r.startswith("KILLED")])
survived = len([r for r in res if r.startswith("SURVIVED")])
vac = len([r for r in res if r.startswith("VACUOUS")])
nocompile = len([r for r in res if r.startswith("NOCOMPILE")])
print(f"mutations={len(res)} killed={killed} survived={survived} vacuous={vac} nocompile={nocompile}")
sys.exit(0 if killed == len(res) and res else 1)
