#!/usr/bin/env bash
# mutate_pipelinectx.sh -- "seen red" evidence for pkg/pipelinectx (gonk-tios P1).
#
# pkg/pipelinectx holds two security controls: the evidence-grounding
# (hallucination) check and secret redaction. A test of a control is only
# evidence if it goes RED when the control is broken. This script applies one
# named break at a time to the package source, runs the package tests, and
# requires them to FAIL; then restores the file byte for byte.
#
# A mutation whose text is not found is itself a failure (a vacuous mutation
# proves nothing), as is a mutation the tests do not notice ("SURVIVED").
#
# Usage: hack/mutate_pipelinectx.sh            (all mutations)
#        hack/mutate_pipelinectx.sh <substring> (only names containing it)
# Exit: 0 iff every selected mutation applied and was killed.
#
# It edits files in the CURRENT worktree and restores them on exit (trap);
# run it in your own worktree, never in a shared checkout.
set -u -o pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
pkg="$root/pkg/pipelinectx"
GO="${GO:-go}"
filter="${1:-}"
backup="$(mktemp -d)"
trap 'for f in "$backup"/*.go; do [ -e "$f" ] && cp "$f" "$pkg/$(basename "$f")"; done; rm -rf "$backup"' EXIT

killed=0
survived=0
vacuous=0

# mut NAME FILE FROM TO [FROM2 TO2 ...] -- replace each literal FROM (first
# occurrence) by its TO. Several pairs make ONE mutant (for a break that needs
# two edits to be observable at all).
mut() {
	local name="$1" file="$2"
	shift 2
	if [ -n "$filter" ] && [[ "$name" != *"$filter"* ]]; then
		return
	fi
	local f="$pkg/$file"
	cp "$f" "$backup/$file"
	local ok=1
	while [ $# -ge 2 ]; do
		FROM="$1" TO="$2" perl -0pi -e '$i = index($_, $ENV{FROM}); if ($i >= 0) { substr($_, $i, length($ENV{FROM})) = $ENV{TO}; $n++ } END { exit($n ? 0 : 3) }' "$f" || ok=0
		shift 2
	done
	if [ $ok -ne 1 ] || cmp -s "$f" "$backup/$file"; then
		echo "VACUOUS  $name  (text not found in $file)"
		vacuous=$((vacuous + 1))
		cp "$backup/$file" "$f"
		return
	fi
	local out
	out="$(cd "$root" && "$GO" test -count=1 ./pkg/pipelinectx/ 2>&1)"
	local rc=$?
	cp "$backup/$file" "$f"
	rm -f "$backup/$file"
	if printf '%s\n' "$out" | grep -q -e '\[build failed\]' -e '\[setup failed\]'; then
		# A mutant that does not compile proves nothing about the tests.
		echo "BROKEN   $name  (mutant does not build)"
		printf '%s\n' "$out" | head -5 | sed 's/^/           /'
		vacuous=$((vacuous + 1))
	elif [ $rc -eq 0 ]; then
		echo "SURVIVED $name"
		survived=$((survived + 1))
	else
		killed=$((killed + 1))
		echo "KILLED   $name"
		# The failing tests, so the evidence names what went red.
		printf '%s\n' "$out" | grep -E -e '^\s*--- FAIL' -e '^# ' -e 'build failed' | sed 's/^/           /' | head -8
	fi
}

# --- grounding (the hallucination check) ---
mut "grounding: accepts a line spanning two excerpt lines" grounded.go \
	'for _, l := range strings.Split(excerpt, "\n") {' \
	'for _, l := range []string{strings.ReplaceAll(excerpt, "\n", " ")} {'
mut "grounding: accepts fragments under 12 runes" grounded.go \
	'short := utf8.RuneCountInString(q) < MinGroundedRunes' \
	'short := utf8.RuneCountInString(q) < 0'
mut "grounding: refuses a whole short line" grounded.go \
	'if n == q {' 'if false {'
mut "grounding: accepts a multi-line quote" grounded.go \
	'if strings.ContainsAny(line, "\n\r") {' 'if false {'
mut "grounding: accepts gonk marker lines" grounded.go \
	'if isMarker(l) {' 'if false {'
# verify report B3: punctuation is part of the line.
mut "grounding B3: punctuation ignored (accepts a punctuation-only paraphrase)" grounded.go \
	'return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")' 'return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) }), " ")'
mut "grounding: case-insensitive (accepts a re-cased paraphrase)" grounded.go \
	'if strings.Contains(n, q) {' 'if strings.Contains(strings.ToLower(n), strings.ToLower(q)) {'
mut "grounding: no whitespace normalisation of the excerpt" grounded.go \
	'n := Norm(strings.TrimSuffix(l, LineTruncatedSuffix))' \
	'n := strings.TrimSuffix(l, LineTruncatedSuffix)'
mut "grounding: truncation suffix quotable" grounded.go \
	'n := Norm(strings.TrimSuffix(l, LineTruncatedSuffix))' 'n := Norm(l)'
mut "grounding: accepts an empty quote" grounded.go \
	'if q == "" {
		return false
	}' ''

# --- redaction ---
mut "redact: misses glpat- tokens" redact.go 'glpat|' ''
mut "redact: misses AKIA keys" redact.go '(?:AKIA|ASIA)' 'ASIA'
mut "redact: sk- narrowed to the plan regex (misses sk-proj-/sk-ant-)" redact.go \
	'\bsk-[A-Za-z0-9_\-]{20,}' '\bsk-[A-Za-z0-9]{20,}'
mut "redact: misses JWTs" redact.go '\beyJ[A-Za-z0-9_\-]{8,}' '\beyX[A-Za-z0-9_\-]{8,}'
mut "redact: misses URL userinfo" redact.go '://([^/\s:@]+:[^/\s@]+)@' '://([^/\s:@]+:[^/\s@]+)@@'
mut "redact: misses PEM private keys" redact.go '-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(' '-----BEGIN [A-Z0-9 ]*PRIVATE KEYS-----('
mut "redact: unterminated PEM block leaks" redact.go '|[\s\S]*$)' ')'
mut "redact: bearer token survives (scheme redacted instead)" redact.go \
	'(?:(?:bearer|basic|token)[ \t]+)?' ''
mut "redact: CI_JOB_TOKEN= style prefixed keys missed" redact.go \
	'(?i)[A-Za-z0-9_\-]*(?:authorization' '(?i)\b(?:authorization'
mut "redact: password key missed" redact.go '|password|' '|'
mut "redact: not idempotent (placeholder re-redacted)" redact.go \
	'if alreadyRedacted(secret) {' 'if false {'
# verify report R7: only an EXACT placeholder is not a secret.
mut "redact R7: anything starting with the placeholder prefix passes" redact.go \
	'return ok && redactionKinds[kind]' 'return true || ok && redactionKinds[kind]'
mut "redact R7: a placeholder of an unknown kind passes" redact.go \
	'return ok && redactionKinds[kind]' 'return ok'
mut "redact: hint drops a matching line" redact.go \
	'containsAny("AKIA", "ASIA")' 'containsAny("AKIA ")'
# Spanning lines needs BOTH the whole-text pass and \s* around the separator;
# either alone is an equivalent mutant.
mut "redact: credential rule spans lines" redact.go \
	'["'"'"']?[ \t]*[:=][ \t]*' '["'"'"']?\s*[:=]\s*' \
	'lines := strings.Split(s, "\n")' 'lines := []string{s}'

# --- normalisation ---
mut "normalize: ANSI CSI not stripped" normalize.go '`\x1b\[[0-?]*[ -/]*[@-~]|' '`'
mut "normalize: OSC not stripped" normalize.go '|\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)?' ''
mut "normalize: section markers kept" normalize.go 'b = sectionRe.ReplaceAll(b, nil)' ''
mut "normalize: CR overwrite not resolved" normalize.go \
	'if i := strings.LastIndexByte(p, '"'"'\r'"'"'); i >= 0 {' 'if i := -1; i >= 0 {'
mut "normalize: CRLF treated as overwrite" normalize.go 'p = strings.TrimRight(p, "\r")' ''
mut "normalize: control bytes kept" normalize.go 'p = dropControls(p)' ''
mut "normalize: invalid utf-8 kept" normalize.go 's := strings.ToValidUTF8(string(b), "\uFFFD")' 's := string(b)'
mut "normalize: trailing whitespace kept" normalize.go 'p = strings.TrimRightFunc(p, unicode.IsSpace)' ''
mut "normalize: repeats not collapsed" normalize.go 'if run >= MinRepeatCollapse {' 'if false {'
mut "normalize: runs of 3 collapsed" normalize.go 'if run >= MinRepeatCollapse {' 'if run >= 3 {'
# gonk-mf3v: GitLab's timestamped-trace line prefix (the real trace in testdata).
mut "normalize mf3v: trace prefix kept (the live defect: '$ exit 3' ungrounded)" normalize.go 'b := stripTracePrefix(raw)' 'b := raw'
mut "normalize mf3v: continuation emitted as its own line" normalize.go \
	"if m[len(m)-1] == '+' && len(out) > 0 && out[len(out)-1] == '\\n' {" 'if false {'
mut "normalize mf3v: every prefixed line joined to the one before" normalize.go \
	"if m[len(m)-1] == '+' && len(out) > 0 && out[len(out)-1] == '\\n' {" "if len(out) > 0 && out[len(out)-1] == '\\n' {"
mut "normalize mf3v: stderr-stream prefix kept" normalize.go 'Z [0-9a-fA-F]{2}[OE][ +]`' 'Z [0-9a-fA-F]{2}O[ +]`'
mut "normalize mf3v: continuation prefix kept" normalize.go 'Z [0-9a-fA-F]{2}[OE][ +]`' 'Z [0-9a-fA-F]{2}[OE] `'
mut "normalize mf3v: hex stream number not a prefix" normalize.go 'Z [0-9a-fA-F]{2}[OE][ +]`' 'Z [0-9]{2}[OE][ +]`'
mut "normalize mf3v: prefix without fractional seconds kept" normalize.go '(?:\.[0-9]{1,9})?Z ' '\.[0-9]{1,9}Z '
mut "normalize mf3v: nanosecond prefix kept" normalize.go '(?:\.[0-9]{1,9})?Z ' '(?:\.[0-9]{1,6})?Z '
mut "normalize mf3v: prefix stripped mid-line (eats the text before it)" normalize.go \
	'regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T' 'regexp.MustCompile(`^.*?[0-9]{4}-[0-9]{2}-[0-9]{2}T'
mut "normalize mf3v: any line starting with a timestamp loses it" normalize.go 'Z [0-9a-fA-F]{2}[OE][ +]`' 'Z (?:[0-9a-fA-F]{2}[OE][ +])?`'
mut "normalize mf3v: stream marker needs no separator" normalize.go 'Z [0-9a-fA-F]{2}[OE][ +]`' 'Z [0-9a-fA-F]{2}[OE][ +]?`'

# --- excerpt ---
mut "excerpt mf3v: ExcerptVersion not bumped" excerpt.go 'const ExcerptVersion = 2' 'const ExcerptVersion = 1'
mut "excerpt mf3v: verify passes an OLDER recorded version" excerpt.go 'if version != ExcerptVersion {' 'if version > ExcerptVersion {'
mut "excerpt: not deterministic across runs" excerpt.go \
	'e.Text = strings.Join(out, "\n") + "\n"' \
	'e.Text = strings.Join(out, "\n") + "\n"
		for i := 0; i < 4; i++ {
			for k := range map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7, 8: 8} {
				e.Text += strconv.Itoa(k)
				break
			}
		}'
mut "excerpt: not redacted" excerpt.go 'redacted, _ := Redact(strings.Join(norm, "\n"))' 'redacted := strings.Join(norm, "\n")'
mut "excerpt: lines cut before redaction (secret prefix leaks)" excerpt.go \
	'norm := Normalize(trace)' \
	'norm := Normalize(trace)
	for i := range norm {
		norm[i] = truncateLine(norm[i], MaxLineBytes)
	}'
mut "excerpt: no first-error window" excerpt.go 'if fe >= 0 && fe < s.ts {' 'if false {'
mut "excerpt: line cap ignored" excerpt.go 'if len(out) <= MaxJobExcerptLines && ' 'if '
mut "excerpt: byte cap ignored" excerpt.go '&& textLen(out) <= maxBytes {' '{'
mut "excerpt: MaxBytes option not clamped" excerpt.go '|| maxBytes > MaxJobExcerptBytes ' ''
mut "excerpt: head truncation not marked" excerpt.go 'trace = trace[len(trace)-MaxTraceBytes:]
		head = true' 'trace = trace[len(trace)-MaxTraceBytes:]'
mut "excerpt: whole trace read past MaxTraceBytes" excerpt.go 'if len(trace) > MaxTraceBytes {' 'if false {'
mut "excerpt: partial first line kept after head truncation" excerpt.go 'trace = trace[i+1:]' '_ = i'
mut "excerpt: tail-truncated not marked" excerpt.go 'if s.tailTrunc {' 'if false {'
mut "excerpt: window shrinks before the tail" excerpt.go 'case tail > minTailLines:' 'case tail > 1000:'
mut "excerpt: verify ignores the hash" excerpt.go 'if e.SHA256 != strings.ToLower(wantSHA256) {' 'if false {'
# verify report B7: the whole hash is compared.
mut "excerpt B7: verify compares the first 8 hex only" excerpt.go \
	'if e.SHA256 != strings.ToLower(wantSHA256) {' 'if len(wantSHA256) < 8 || e.SHA256[:8] != strings.ToLower(wantSHA256)[:8] {'
mut "excerpt: verify ignores the version" excerpt.go 'if version != ExcerptVersion {' 'if false {'
mut "excerpt: error regex loses word boundaries" excerpt.go '`(?i)\b(error|fatal|failed|failure|panic|exception)\b`' '`(?i)(error|fatal|failed|failure|panic|exception)`'

# --- fingerprint ---
mut "fingerprint: order-dependent" fingerprint.go 'sort.Strings(lines)' 'sort.Strings(lines[:0])'
mut "fingerprint: allow_failure jobs counted" fingerprint.go 'if j.AllowFailure {
			continue
		}' ''
mut "fingerprint: field boundary lost" fingerprint.go 'j.Stage+"\x00"+j.Name' 'j.Stage+j.Name'
mut "fingerprint: pipeline failure reason ignored" fingerprint.go 'lines = []string{"pipeline\x00" + r}' 'lines = []string{"pipeline\x00"}'

# --- anchor / session key ---
mut "anchor: ref used raw in the key" anchor.go 'return "ref-" + hex.EncodeToString(sum[:])[:8]' '_ = sum
	return "ref-" + t.Ref'
mut "anchor: fingerprint not validated" anchor.go '`^[0-9a-f]{16}$`' '`^.*$`'
mut "anchor: ref with whitespace accepted" anchor.go 'if unicode.IsSpace(r) || unicode.IsControl(r) {' 'if r < 0 && unicode.IsSpace(r) {'
mut "sessionkey: longer than a DNS label" anchor.go 'hex.EncodeToString(sum[:])[:12], nil' 'hex.EncodeToString(sum[:]) + hex.EncodeToString(sum[:]), nil'
mut "sessionkey: accepts a non-anchor" anchor.go 'if m == nil {' 'if m == nil && anchor == "" {'
mut "terminal-for-health: canceled counts" anchor.go 'return status == "success" || status == "failed"' 'return status == "success" || status == "failed" || status == "canceled"'

# --- error signature ---
mut "signature: build paths not canonicalised" signature.go '{re: regexp.MustCompile(`/builds/' '{re: regexp.MustCompile(`/nobuilds/'
mut "signature: hex ids not canonicalised" signature.go '{re: regexp.MustCompile(`\b[0-9a-fA-F]{7,}\b`), fn: hexWithDigit},' ''
mut "signature: timestamps not canonicalised" signature.go '`\b\d{4}-\d{2}-\d{2}(' '`\bX\d{4}-\d{2}-\d{2}('
mut "signature: IPs not canonicalised" signature.go '`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`' '`\bX\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`'
mut "signature: durations not canonicalised" signature.go '(?:ns|us|\x{00B5}s|ms|s|m|h)(?:\d+' '(?:ns)(?:\d+'
mut "signature: exit status canonicalised away" signature.go 'kept = append(kept, m)
		return keep' 'return m'
mut "signature: every dotted name treated as a host" signature.go '(^|[\s(@'"'"'"=])[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?){2,}:(\d{1,5})\b`), repl: "${1}<host>:${2}"' '(^|[\s(@'"'"'"=])[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?){2,}`), repl: "${1}<host>"'
mut "signature: letters-only hex words canonicalised" signature.go 'if strings.ContainsAny(m, "0123456789") {' 'if true {'
mut "signature: unversioned" signature.go 'return signaturePrefix + hex' 'return hex'

# --- job failure key, cause key, generic lines (gonk-2usw FACTS) ---
mut "2usw generic: the runner's exit-code summary is not flagged" failurekey.go \
	'`^ERROR: Job failed: exit (?:code|status) \d+$`' '`^XERROR: Job failed: exit (?:code|status) \d+$`'
mut "2usw generic: exit status spelling not flagged" failurekey.go 'exit (?:code|status) \d+$`' 'exit code \d+$`'
mut "2usw generic: the kubernetes executor's summary is not flagged" failurekey.go \
	'`^ERROR: Job failed: command terminated with exit code \d+$`' '`^XERROR: Job failed: command terminated with exit code \d+$`'
mut "2usw generic: a line that merely CONTAINS the summary is generic" failurekey.go \
	'`^ERROR: Job failed: exit (?:code|status) \d+$`' '`ERROR: Job failed: exit (?:code|status) \d+$`'
mut "2usw generic: a summary with a cause appended is generic" failurekey.go \
	'`^ERROR: Job failed: exit (?:code|status) \d+$`' '`^ERROR: Job failed: exit (?:code|status) \d+`'
mut "2usw generic: case-insensitive" failurekey.go \
	'`^ERROR: Job failed: exit (?:code|status) \d+$`' '`(?i)^ERROR: Job failed: exit (?:code|status) \d+$`'
mut "2usw generic: matched on the raw line, not the canonical one" failurekey.go \
	'c := CanonicalError(line)' 'c := line'
mut "2usw generic: a line with no signature is not a generic first error" failurekey.go \
	'return ErrorSignature(line) == "" || GenericErrorLine(line)' 'return GenericErrorLine(line)'
mut "2usw failurekey: the signature is not part of the key string" failurekey.go \
	'strconv.QuoteToASCII(k.FailureReason) + " " + strconv.QuoteToASCII(k.ErrorSignature)' 'strconv.QuoteToASCII(k.FailureReason)'
mut "2usw failurekey: failure_reason is not part of the key string" failurekey.go \
	'strconv.QuoteToASCII(k.FailureReason) + " " + strconv.QuoteToASCII(k.ErrorSignature)' 'strconv.QuoteToASCII(k.ErrorSignature)'
mut "2usw failurekey: stage is not part of the key string" failurekey.go \
	'return strconv.QuoteToASCII(k.Stage) + " " + strconv.QuoteToASCII(k.Name) + " " +' 'return strconv.QuoteToASCII(k.Name) + " " +'
mut "2usw failurekey: fields not quoted (a separator in a name moves the boundary)" failurekey.go \
	'return strconv.QuoteToASCII(k.Stage) + " " + strconv.QuoteToASCII(k.Name) + " " +' 'return k.Stage + " " + k.Name + " " +'
mut "2usw causekey: changes with job order" failurekey.go \
	'	sort.Strings(lines)
	var b strings.Builder' '	sort.Strings(lines[:0])
	var b strings.Builder'
mut "2usw causekey: a repeated key counts twice" failurekey.go 'if i > 0 && l == lines[i-1] {' 'if i < 0 && l == lines[i-1] {'
mut "2usw causekey: an empty set has a key" failurekey.go 'if len(keys) == 0 {' 'if false {'
mut "2usw causekey: sorts the caller's slice in place" failurekey.go \
	'	sort.Strings(lines)
	var b strings.Builder' '	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	sort.Strings(lines)
	var b strings.Builder'
mut "2usw causekey: no domain prefix (bytes change)" failurekey.go 'b.WriteString("pipelinectx-cause-key\x00")' ''
mut "2usw causekey: keys not line-terminated" failurekey.go '		b.WriteString(l)
		b.WriteString("\n")' '		b.WriteString(l)'

# --- fix epoch (gonk-2usw EPOCH, plan 3.3) ---
# The bug the epoch exists for: after a merged fix the anchor does not move.
mut "epoch: Anchor ignores the fix epoch (the bug: same anchor after a merged fix)" anchor.go \
	'if fixEpoch != 0 {' 'if false {'
# And its converse: a run that follows NO fix must keep its bytes.
mut "epoch: an anchor without a fix gains one byte (trailing colon)" anchor.go \
	'a := fmt.Sprintf("gonk:%d:pipeline:%s:%s:s%d", projectID' 'a := fmt.Sprintf("gonk:%d:pipeline:%s:%s:s%d:", projectID'
mut "epoch: an anchor without a fix gains :f0" anchor.go \
	'if fixEpoch != 0 {' 'if true {'
mut "epoch: negative fix epoch accepted" anchor.go \
	'if fixEpoch < 0 {' 'if false {'
mut "epoch: anchorRe accepts :f0 (two spellings of no fix)" anchor.go \
	'(?::f([1-9][0-9]*))?$`' '(?::f([0-9]+))?$`'
mut "epoch: anchorRe refuses an epoch anchor (no session key, no marker)" anchor.go \
	'(?::f([1-9][0-9]*))?$`' '(?::g([1-9][0-9]*))?$`'
mut "epoch: anchorRe accepts a repeated epoch" anchor.go \
	'(?::f([1-9][0-9]*))?$`' '(?::f([1-9][0-9]*))*$`'
mut "epoch: AnchorFixEpoch always answers none" anchor.go \
	'if m[2] == "" {' 'if true {'
mut "epoch: a marker answers for any anchor it is a prefix of" runmarker.go \
	'if m[1] == anchor {' 'if strings.HasPrefix(m[1], anchor) {'
mut "epoch: the old run's pipelines join the new run (no upper bound)" fixepoch.go \
	'after := landedOn(x, f.PipelineSHA) || !f.PipelineCreatedAt.Before(x.MergedAt)' 'after := true'
mut "epoch: merged_at is the only clock (the merge commit's own pipeline is missed)" fixepoch.go \
	'after := landedOn(x, f.PipelineSHA) || !f.PipelineCreatedAt.Before(x.MergedAt)' 'after := !f.PipelineCreatedAt.Before(x.MergedAt)'
mut "epoch: a pipeline created at merged_at is before the fix" fixepoch.go \
	'|| !f.PipelineCreatedAt.Before(x.MergedAt)' '|| f.PipelineCreatedAt.After(x.MergedAt)'
mut "epoch: a green after the fix does not end its epoch" fixepoch.go \
	'if f.LastSuccess != 0 && (landedOn(x, f.LastSuccessSHA) || !x.MergedAt.After(f.LastSuccessCreatedAt)) {' 'if false {'
mut "epoch: a green that ran ON the fix does not end its epoch" fixepoch.go \
	'(landedOn(x, f.LastSuccessSHA) || !x.MergedAt.After(f.LastSuccessCreatedAt))' '(!x.MergedAt.After(f.LastSuccessCreatedAt))'
mut "epoch: a fix merged at the last green's creation instant counts" fixepoch.go \
	'|| !x.MergedAt.After(f.LastSuccessCreatedAt))' '|| x.MergedAt.Before(f.LastSuccessCreatedAt))'
mut "epoch: no last success still applies the lower bound" fixepoch.go \
	'if f.LastSuccess != 0 && (landedOn' 'if (landedOn'
mut "epoch: newest by iid, not by merged_at" fixepoch.go \
	'if best == nil || x.MergedAt.After(best.MergedAt) ||' 'if best == nil || x.IID > best.IID ||'
mut "epoch: oldest merged fix wins" fixepoch.go \
	'if best == nil || x.MergedAt.After(best.MergedAt) ||' 'if best == nil || x.MergedAt.Before(best.MergedAt) ||'
mut "epoch: a tie on merged_at is not broken by iid" fixepoch.go \
	'(x.MergedAt.Equal(best.MergedAt) && x.IID > best.IID)' 'false'
mut "epoch: a fix with no merged_at is tolerated (fail OPEN)" fixepoch.go \
	'if x.MergedAt.IsZero() {' 'if false {'
mut "epoch: a fix with no iid is tolerated" fixepoch.go \
	'if x.IID < 1 {' 'if false {'
mut "epoch: unknown pipeline creation time reads as no fix (fail OPEN)" fixepoch.go \
	'if f.PipelineCreatedAt.IsZero() {' 'if false {'
mut "epoch: unknown last-success creation time is tolerated (fail OPEN)" fixepoch.go \
	'if f.LastSuccess != 0 && f.LastSuccessCreatedAt.IsZero() {' 'if false {'
# Both guards must go for an empty SHA to match: either alone is equivalent.
mut "epoch: an empty SHA matches an empty landed SHA" fixepoch.go \
	'if sha == "" {
		return false
	}' '' \
	'if s != "" && s == sha {' 'if s == sha {'
mut "epoch: any branch starting gonk/pipefix- is a fix branch" fixepoch.go \
	'`^gonk/pipefix-[0-9a-f]{12}$`' '`^gonk/pipefix-`'
mut "epoch: fix branch not anchored at the start" fixepoch.go \
	'`^gonk/pipefix-[0-9a-f]{12}$`' '`gonk/pipefix-[0-9a-f]{12}$`'
mut "epoch: fix branch not anchored at the end" fixepoch.go \
	'`^gonk/pipefix-[0-9a-f]{12}$`' '`^gonk/pipefix-[0-9a-f]{12}`'

echo
echo "killed=$killed survived=$survived vacuous=$vacuous"
[ "$survived" -eq 0 ] && [ "$vacuous" -eq 0 ] && [ "$killed" -gt 0 ]
