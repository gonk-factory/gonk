package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// `gonk-gate trailers` renders the commit-provenance trailer block (spec 6.1). It
// is invoked by a prepare-commit-msg git hook installed into the rig clone at
// session start (AD-4) -- NOT by asking the agent to write trailers, because an
// agent asked to write a trailer will one day write a plausible-looking wrong one.
//
// TWO RULES, both of which are about not lying in permanent history:
//
//  1. include_usage defaults OFF. Cost in public git history is a per-project
//     choice (spec 6.1), not ours.
//  2. When meter says `complete: false` -- which at commit time it USUALLY does,
//     because the session is still open and its own last calls have not landed --
//     WRITE `Gonk-Usage: pending`, NEVER A NUMBER. A wrong cost in a commit
//     trailer is permanent, uncorrectable, and will be quoted back at you.
//
// And a rule about not losing work: a trailer lookup must NEVER fail a commit. If
// meter is unreachable, we write `pending` and move on. Losing the agent's actual
// work over a metadata footer is a terrible trade.

// Trailer key names. Part of the contract every downstream tool (git,
// GitLab, a future dashboard) reads -- do not rename casually.
const (
	TrailerGeneratedBy = "Generated-By"
	TrailerBead        = "Gonk-Bead"
	TrailerSession     = "Gonk-Session"
	TrailerRung        = "Gonk-Rung"
	TrailerAttempt     = "Gonk-Attempt"
	TrailerUsage       = "Gonk-Usage"
	TrailerTokens      = "Gonk-Tokens"
	TrailerCostUSD     = "Gonk-Cost-USD"
)

// defaultProvenance is gonkcfg's OWN shipped default (pkg/gonkcfg/resolve.go:
// EffectiveProvenance{CommitTrailers: true, IncludeUsage: false}), duplicated
// here on purpose: it is what `trailers` falls back to when it cannot reach
// meter to ask, so that "meter is down" degrades to the same answer almost
// every project already has, rather than to silence.
var defaultProvenance = meterapi.Provenance{CommitTrailers: true, IncludeUsage: false}

// trailerInput is every value renderTrailers needs to render the block. It
// carries NO secret material anywhere -- there is no field for a virtual key
// or any other credential, so there is no path by which one could leak into
// permanent git history through this type.
type trailerInput struct {
	Provenance      meterapi.Provenance
	GonkVersion     string
	OpencodeVersion string
	Model           string
	BeadID          string
	SessionKey      string
	Rung            string
	Attempt         int
	// Cost is nil when include_usage is off, or when the lookup at commit
	// time failed, or simply was never attempted. Either way `renderTrailers`
	// treats a nil Cost identically to a Cost with Complete == false: pending.
	Cost *meterapi.SessionCostResponse
}

func renderTrailers(in trailerInput) string {
	if !in.Provenance.CommitTrailers {
		return ""
	}

	type kv struct{ key, val string }
	vals := []kv{
		{TrailerGeneratedBy, fmt.Sprintf("gonk/%s (opencode %s; %s via litellm)",
			in.GonkVersion, in.OpencodeVersion, in.Model)},
		{TrailerBead, in.BeadID},
		{TrailerSession, in.SessionKey},
		{TrailerRung, in.Rung},
		{TrailerAttempt, strconv.Itoa(in.Attempt)},
	}

	if in.Provenance.IncludeUsage {
		if in.Cost != nil && in.Cost.Complete {
			// A COMPLETE cost is meter's own final word for this session --
			// safe to publish as a number.
			vals = append(vals,
				kv{TrailerTokens, strconv.FormatInt(in.Cost.TotalTokens, 10)},
				kv{TrailerCostUSD, strconv.FormatFloat(in.Cost.CostUSD, 'f', -1, 64)},
			)
		} else {
			// complete == false (the USUAL case at commit time -- the session
			// is still open) OR the lookup failed entirely (Cost == nil).
			// Both mean the same thing: we do not know the final cost, and a
			// wrong number here is PERMANENT and UNCORRECTABLE. Never guess.
			vals = append(vals, kv{TrailerUsage, "pending"})
		}
	}

	// Refuse rather than sanitize: a value with an embedded newline (or CR)
	// could forge an extra trailer line, or break the block out of its own
	// paragraph. A silently-mangled bead id is worse than a missing one
	// (Plan 01 carry-forward) -- so ANY unsafe value voids the WHOLE block,
	// rather than emitting a partially-trusted one.
	for _, v := range vals {
		if strings.ContainsAny(v.val, "\r\n") {
			return ""
		}
	}

	var b strings.Builder
	for _, v := range vals {
		fmt.Fprintf(&b, "%s: %s\n", v.key, v.val)
	}
	return b.String()
}

// ---------------------------------------------------------------- splice

var trailerLineRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*: .+$`)

// isTrailerParagraph reports whether every non-blank line of p looks like a
// git trailer (`Key: value`). An empty paragraph is not one.
func isTrailerParagraph(p string) bool {
	lines := strings.Split(strings.TrimRight(p, "\n"), "\n")
	found := false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !trailerLineRE.MatchString(l) {
			return false
		}
		found = true
	}
	return found
}

// lastParagraph returns the text after the LAST blank-line break in msg, and
// false if msg contains no such break at all -- a bare one-line subject (e.g.
// "feat: add a widget") has no paragraph break, and must NEVER be mistaken
// for an existing trailer footer just because its subject happens to contain
// a colon-space.
func lastParagraph(msg string) (para string, ok bool) {
	i := strings.LastIndex(msg, "\n\n")
	if i < 0 {
		return "", false
	}
	return msg[i+2:], true
}

// appendTrailerBlock splices block onto the end of a commit message,
// producing well-formed git trailers (a contiguous `Key: value` paragraph at
// the very end, separated from any prose by exactly one blank line) --
// UNLESS the message already ends in a trailer paragraph, in which case block
// extends that SAME paragraph, because git's own reader only recognises the
// LAST contiguous run of trailer-shaped lines: a second blank line here would
// silently split an existing trailer (e.g. Co-Authored-By) out of the block
// every downstream tool reads.
//
// It is idempotent: if block is already present verbatim, msg is returned
// unchanged. A prepare-commit-msg hook CAN be invoked more than once for the
// same commit (git re-runs it on `commit --amend`), and re-adding an
// identical block would duplicate it.
func appendTrailerBlock(msg, block string) string {
	if block == "" {
		return msg
	}
	if strings.Contains(msg, block) {
		return msg
	}

	trimmedBlock := strings.TrimRight(block, "\n")
	trimmed := strings.TrimRight(msg, "\n")
	if trimmed == "" {
		return trimmedBlock + "\n"
	}
	if p, ok := lastParagraph(trimmed); ok && isTrailerParagraph(p) {
		return trimmed + "\n" + trimmedBlock + "\n"
	}
	return trimmed + "\n\n" + trimmedBlock + "\n"
}

// ---------------------------------------------------------------- subcommand wiring

// trailersArgs is what main.go hands runTrailers: the git hook's own
// argument (the commit message file path) plus the two GC_WEBHOOK_ARG_*
// values the agent pod's session already carries for the OTHER attribution
// seam (entrypoint.sh's overlay/opencode.json render, OD-7) -- `trailers`
// reuses the SAME verbatim metadata blob rather than re-deriving bead/
// session/rung/attempt a second way.
type trailersArgs struct {
	CommitMsgFile string
	Model         string // GC_WEBHOOK_ARG_MODEL
	MetadataJSON  string // GC_WEBHOOK_ARG_METADATA_JSON, atags.Tags.Metadata(), verbatim
}

type trailersDeps struct {
	Meter   *meterAPI
	Log     *slog.Logger
	Version string // gonk version (main.version, i.e. GONK_TAG at build time)
}

// trailersCommitMsgFile parses `--commit-msg-file <path>` out of a
// subcommand's raw args. It never errors loudly (ContinueOnError, output
// discarded): an unparsed flag means an empty path, which runTrailers already
// treats as "nothing to do" -- consistent with "a trailer lookup must never
// fail a commit".
func trailersCommitMsgFile(args []string) string {
	fs := flag.NewFlagSet("trailers", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("commit-msg-file", "", "path to the git commit message file (git's own $1)")
	_ = fs.Parse(args)
	return *path
}

// opencodeVersion shells out to the opencode binary this agent image ships
// (images/Dockerfile.agent). "unknown" on any failure -- Generated-By is a
// human-readable footer, not a machine contract field, so a missing version
// string is a cosmetic gap, never a reason to withhold the rest of the block.
func opencodeVersion() string {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// parseTrailerTags decodes GC_WEBHOOK_ARG_METADATA_JSON (verbatim
// atags.Tags.Metadata()) back into atags.Tags, reusing atags.FromMetadata --
// the SAME validated round-trip the wire contract already guarantees,
// instead of inventing a second parse of the same JSON shape.
func parseTrailerTags(metadataJSON string) (atags.Tags, error) {
	if metadataJSON == "" {
		return atags.Tags{}, fmt.Errorf("trailers: GC_WEBHOOK_ARG_METADATA_JSON is empty")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(metadataJSON), &m); err != nil {
		return atags.Tags{}, fmt.Errorf("trailers: decode metadata json: %w", err)
	}
	return atags.FromMetadata(m)
}

// runTrailers is the `gonk-gate trailers` subcommand body: read this
// session's attribution identity, ask meter what this project's provenance
// policy is (and, if asked, its session cost), render the block, and splice
// it into the commit message file -- IDEMPOTENTLY, and WITHOUT EVER FAILING.
//
// Every failure mode here degrades gracefully instead of returning non-zero:
//   - no/invalid attribution metadata -> leave the message untouched (there is
//     no identity to stamp; guessing one would be worse than omitting it).
//   - meter unreachable for the project's provenance policy -> fall back to
//     the shipped default (commit_trailers on, include_usage off) -- the
//     answer almost every project already has.
//   - meter unreachable (or the cost is not yet complete) for session cost ->
//     renderTrailers already turns that into `Gonk-Usage: pending` (AD-5).
//   - the commit message file cannot be read or written -> log and return 0
//     anyway. A hook that exits non-zero can abort the commit; a missing
//     footer never should.
func runTrailers(ctx context.Context, d trailersDeps, args trailersArgs) int {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if args.CommitMsgFile == "" {
		d.Log.Warn("trailers: no --commit-msg-file given; nothing to do")
		return 0
	}

	tags, err := parseTrailerTags(args.MetadataJSON)
	if err != nil {
		d.Log.Warn("trailers: no attribution identity for this session; leaving the commit message untouched", "err", err)
		return 0
	}

	prov := defaultProvenance
	if d.Meter != nil {
		if resp, err := d.Meter.Project(ctx, tags.Project); err != nil {
			d.Log.Warn("trailers: could not reach meter for this project's provenance policy; "+
				"falling back to the shipped default (commit_trailers=on, include_usage=off)", "err", err)
		} else if resp.Effective != nil {
			prov = resp.Effective.Provenance
		}
	}

	var cost *meterapi.SessionCostResponse
	if prov.IncludeUsage && d.Meter != nil {
		if c, err := d.Meter.CostSession(ctx, tags.SessionKey); err != nil {
			d.Log.Warn("trailers: could not reach meter for session cost; writing Gonk-Usage: pending", "err", err)
		} else {
			cost = c
		}
	}

	block := renderTrailers(trailerInput{
		Provenance:      prov,
		GonkVersion:     d.Version,
		OpencodeVersion: opencodeVersion(),
		Model:           args.Model,
		BeadID:          tags.BeadID,
		SessionKey:      tags.SessionKey,
		Rung:            tags.Rung,
		Attempt:         tags.Attempt,
		Cost:            cost,
	})
	if block == "" {
		return 0
	}

	raw, err := os.ReadFile(args.CommitMsgFile)
	if err != nil {
		d.Log.Warn("trailers: could not read commit message file", "path", args.CommitMsgFile, "err", err)
		return 0
	}
	updated := appendTrailerBlock(string(raw), block)
	if updated == string(raw) {
		return 0 // already applied -- idempotent no-op
	}
	if err := os.WriteFile(args.CommitMsgFile, []byte(updated), 0o644); err != nil {
		d.Log.Warn("trailers: could not write commit message file", "path", args.CommitMsgFile, "err", err)
	}
	return 0
}
