package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/atags"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

func TestTrailersDefaultOn(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance:  meterapi.Provenance{CommitTrailers: true, IncludeUsage: false},
		GonkVersion: "0.1.0", OpencodeVersion: "1.2.3", Model: "some-model",
		BeadID: "gk-1a2b", SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
	})
	want := []string{
		"Generated-By: gonk/0.1.0 (opencode 1.2.3; some-model via litellm)",
		"Gonk-Bead: gk-1a2b",
		"Gonk-Session: gonk-42-issue-3",
		"Gonk-Rung: cheap",
		"Gonk-Attempt: 2",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("trailers missing %q:\n%s", w, got)
		}
	}
	// include_usage is OFF by default: cost in public history is a per-project choice.
	if strings.Contains(got, "Gonk-Cost-USD") || strings.Contains(got, "Gonk-Tokens") {
		t.Fatalf("usage trailers leaked with include_usage=false:\n%s", got)
	}
}

func TestNoTrailersWhenDisabled(t *testing.T) {
	got := renderTrailers(trailerInput{Provenance: meterapi.Provenance{CommitTrailers: false}})
	if strings.TrimSpace(got) != "" {
		t.Fatalf("commit_trailers=false must produce NOTHING, got:\n%s", got)
	}
}

func TestUsageTrailersWhenComplete(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost:       &meterapi.SessionCostResponse{TotalTokens: 128000, CostUSD: 0.42, Complete: true},
	})
	if !strings.Contains(got, "Gonk-Tokens: 128000") || !strings.Contains(got, "Gonk-Cost-USD: 0.42") {
		t.Fatalf("usage trailers missing:\n%s", got)
	}
	if strings.Contains(got, "pending") {
		t.Fatalf("a COMPLETE cost must be written as a number:\n%s", got)
	}
}

// *** AD-5 / Plan 03's AD-7. THE ONE THAT MATTERS. ***
//
// At commit time the session is still open, so meter usually has not seen the
// spend rows for its own last calls. Writing the number anyway publishes a WRONG
// COST INTO PERMANENT GIT HISTORY, where nobody can correct it and everybody will
// quote it.
func TestPendingUsageWhenCostIsIncomplete(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost:       &meterapi.SessionCostResponse{TotalTokens: 90000, CostUSD: 0.30, Complete: false},
	})
	if strings.Contains(got, "Gonk-Cost-USD") || strings.Contains(got, "Gonk-Tokens") {
		t.Fatalf("A COST WAS WRITTEN INTO GIT HISTORY THAT METER DOES NOT YET KNOW.\n"+
			"complete=false means the spend rows have not landed. The number in this\n"+
			"trailer is WRONG and it is PERMANENT:\n%s", got)
	}
	if !strings.Contains(got, "Gonk-Usage: pending") {
		t.Fatalf("want `Gonk-Usage: pending`, got:\n%s", got)
	}
}

// Meter unreachable at commit time is the same situation: we do not know the cost.
func TestPendingUsageWhenMeterIsUnreachable(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		Cost:       nil, // the lookup failed
	})
	if !strings.Contains(got, "Gonk-Usage: pending") {
		t.Fatalf("want pending, got:\n%s", got)
	}
	// And the commit still happens. A trailer lookup must NEVER block a commit --
	// losing the agent's work over a metadata footer is a terrible trade.
	if strings.Contains(got, "error") {
		t.Fatalf("an error must not end up in git history:\n%s", got)
	}
}

// A trailer is `Key: value` on its own line, in a trailer block at the END of the
// message, separated by a blank line -- or `git interpret-trailers` (and GitLab,
// and every tool that reads them) will not see them.
//
// This test checks the BLOCK ITSELF is well-formed (a contiguous run of `Key:
// value` lines, no blank lines inside it). It does not shell out to `git` --
// that end-to-end proof, against git's OWN reader, is
// TestHookAttachesTrailersToARealCommit (build tag `images`, test/images/),
// deliberately kept out of the fast day-to-day gate.
func TestTrailerBlockIsWellFormed(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance:  meterapi.Provenance{CommitTrailers: true, IncludeUsage: true},
		GonkVersion: "0.1.0", OpencodeVersion: "1.2.3", Model: "some-model",
		BeadID: "gk-1a2b", SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
		Cost: &meterapi.SessionCostResponse{TotalTokens: 128000, CostUSD: 0.42, Complete: true},
	})
	if got == "" {
		t.Fatal("expected a non-empty trailer block")
	}
	if strings.Contains(got, "\n\n") {
		t.Fatalf("trailer block must be a CONTIGUOUS paragraph -- no blank lines inside it:\n%q", got)
	}
	if strings.HasPrefix(got, "\n") {
		t.Fatalf("trailer block must not start with a blank line:\n%q", got)
	}
	trailerLine := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*: .+$`)
	trimmed := strings.TrimRight(got, "\n")
	if trimmed != got && strings.Count(got, "\n")-strings.Count(trimmed, "\n") > 1 {
		t.Fatalf("trailer block has more than one trailing newline:\n%q", got)
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 {
		t.Fatal("no trailer lines produced")
	}
	for _, l := range lines {
		if !trailerLine.MatchString(l) {
			t.Fatalf("line %q is not a well-formed git trailer (`Key: value`)", l)
		}
	}
}

// Attribution safety (Plan 01 carry-forward): a value with a newline would forge a
// trailer. Refuse rather than sanitize -- a silently-mangled bead id is worse than
// a missing one.
func TestValuesWithNewlinesAreRefused(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance:  meterapi.Provenance{CommitTrailers: true, IncludeUsage: false},
		GonkVersion: "0.1.0", OpencodeVersion: "1.2.3", Model: "some-model",
		BeadID:     "gk-1a2b\nEvil-Trailer: forged",
		SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
	})
	if strings.Contains(got, "Evil-Trailer") {
		t.Fatalf("a newline in a value forged an extra trailer line:\n%q", got)
	}
	// Refuse the WHOLE block, not just the bad field -- a silently-mangled bead
	// id is worse than a missing one.
	if got != "" {
		t.Fatalf("a value with an embedded newline must void the whole trailer block, got:\n%q", got)
	}
}

func TestValuesWithCarriageReturnAreRefused(t *testing.T) {
	got := renderTrailers(trailerInput{
		Provenance:  meterapi.Provenance{CommitTrailers: true, IncludeUsage: false},
		GonkVersion: "0.1.0", OpencodeVersion: "1.2.3", Model: "some-model",
		BeadID:     "gk-1a2b",
		SessionKey: "gonk-42-issue-3\r\nEvil-Trailer: forged", Rung: "cheap", Attempt: 2,
	})
	if got != "" {
		t.Fatalf("a value with an embedded CR must void the whole trailer block, got:\n%q", got)
	}
}

// ---------------------------------------------------------------------------
// appendTrailerBlock: the idempotent, well-formed splice into a real commit
// message file. renderTrailers only produces the block; this is what makes
// sure applying it twice (a prepare-commit-msg hook CAN run more than once --
// git re-invokes it on `commit --amend`) never duplicates it.
// ---------------------------------------------------------------------------

func TestAppendTrailerBlockAddsABlankLineSeparator(t *testing.T) {
	msg := "feat: add a widget\n"
	block := "Generated-By: gonk/0.1.0 (opencode 1.2.3; some-model via litellm)\nGonk-Bead: gk-1a2b\n"
	got := appendTrailerBlock(msg, block)
	want := "feat: add a widget\n\n" + block
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestAppendTrailerBlockIsIdempotent(t *testing.T) {
	msg := "feat: add a widget\n"
	block := "Generated-By: gonk/0.1.0 (opencode 1.2.3; some-model via litellm)\nGonk-Bead: gk-1a2b\n"
	once := appendTrailerBlock(msg, block)
	twice := appendTrailerBlock(once, block)
	if once != twice {
		t.Fatalf("applying the same block twice must be a no-op:\nonce:\n%q\ntwice:\n%q", once, twice)
	}
	if strings.Count(twice, "Generated-By:") != 1 {
		t.Fatalf("trailer duplicated:\n%s", twice)
	}
}

func TestAppendTrailerBlockExtendsAnExistingTrailerParagraph(t *testing.T) {
	// A message that already ends in a trailer paragraph (e.g.
	// `Co-Authored-By:`) must gain OUR block as part of the SAME paragraph --
	// git's own reader only recognises the LAST contiguous paragraph of
	// `Key: value` lines as the trailer block, so a second blank line would
	// silently split it into two and hide the earlier trailer from every tool
	// that reads them.
	msg := "feat: add a widget\n\nCo-Authored-By: Someone <someone@example.com>\n"
	block := "Generated-By: gonk/0.1.0 (opencode 1.2.3; some-model via litellm)\nGonk-Bead: gk-1a2b\n"
	got := appendTrailerBlock(msg, block)
	want := "feat: add a widget\n\nCo-Authored-By: Someone <someone@example.com>\n" + block
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestAppendTrailerBlockNoOpWhenBlockIsEmpty(t *testing.T) {
	msg := "feat: add a widget\n"
	if got := appendTrailerBlock(msg, ""); got != msg {
		t.Fatalf("commit_trailers=false (empty block) must leave the message untouched, got:\n%q", got)
	}
}

// ---------------------------------------------------------------------------
// runTrailers: the subcommand's wiring. It reads gonk-meter for the project's
// resolved Provenance policy and (if include_usage) the session's cost, then
// splices renderTrailers' block into the commit message file -- and it must
// NEVER fail the commit: an unreachable meter degrades to the shipped default
// (commit_trailers on, include_usage off), never a process error.
// ---------------------------------------------------------------------------

func fakeTrailersMeter(t *testing.T, project meterapi.ProjectResponse, cost meterapi.SessionCostResponse) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(project)
	})
	mux.HandleFunc("/v1/cost/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cost)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeCommitMsgFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func baseTrailerTags() atags.Tags {
	return atags.Tags{
		Project: "group/repo", Rig: "repo", BeadID: "gk-1a2b",
		SessionKey: "gonk-42-issue-3", Rung: "cheap", Attempt: 2,
		Trigger: atags.TriggerIssueTriage,
	}
}

func trailersArgsFor(t *testing.T, tags atags.Tags, model string) trailersArgs {
	t.Helper()
	md, err := json.Marshal(tags.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	return trailersArgs{Model: model, MetadataJSON: string(md)}
}

func TestRunTrailersReadsProvenanceFromMeter(t *testing.T) {
	project := meterapi.ProjectResponse{
		Project: "group/repo", State: meterapi.StateActive,
		Effective: &meterapi.Effective{Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: false}},
	}
	url := fakeTrailersMeter(t, project, meterapi.SessionCostResponse{})
	path := writeCommitMsgFile(t, "feat: add a widget\n")

	args := trailersArgsFor(t, baseTrailerTags(), "some-model")
	args.CommitMsgFile = path

	code := runTrailers(context.Background(), trailersDeps{
		Meter: meterClient(url), Version: "0.1.0",
	}, args)
	if code != 0 {
		t.Fatalf("runTrailers exit = %d, want 0", code)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Gonk-Bead: gk-1a2b") {
		t.Fatalf("commit message file missing trailers:\n%s", got)
	}
	if strings.Contains(string(got), "Gonk-Tokens") {
		t.Fatalf("include_usage=false but usage leaked:\n%s", got)
	}
}

func TestRunTrailersWritesNothingWhenProjectDisablesTrailers(t *testing.T) {
	project := meterapi.ProjectResponse{
		Project: "group/repo", State: meterapi.StateActive,
		Effective: &meterapi.Effective{Provenance: meterapi.Provenance{CommitTrailers: false}},
	}
	url := fakeTrailersMeter(t, project, meterapi.SessionCostResponse{})
	path := writeCommitMsgFile(t, "feat: add a widget\n")

	args := trailersArgsFor(t, baseTrailerTags(), "some-model")
	args.CommitMsgFile = path

	code := runTrailers(context.Background(), trailersDeps{
		Meter: meterClient(url), Version: "0.1.0",
	}, args)
	if code != 0 {
		t.Fatalf("runTrailers exit = %d, want 0", code)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "feat: add a widget\n" {
		t.Fatalf("commit_trailers=false: message must be untouched, got:\n%q", got)
	}
}

func TestRunTrailersFallsBackToDefaultWhenMeterIsUnreachable(t *testing.T) {
	path := writeCommitMsgFile(t, "feat: add a widget\n")
	args := trailersArgsFor(t, baseTrailerTags(), "some-model")
	args.CommitMsgFile = path

	// No listener at all. meter.Project() will time out (this box's WSL2
	// networking does not refuse connections to an unbound loopback port
	// promptly) -- a short client timeout keeps this test fast without
	// changing what it proves: runTrailers must still degrade to the shipped
	// default (commit_trailers=on, include_usage=off) rather than exiting
	// non-zero.
	meter := meterClient("http://127.0.0.1:1")
	meter.http.Timeout = 200 * time.Millisecond
	code := runTrailers(context.Background(), trailersDeps{
		Meter: meter, Version: "0.1.0",
	}, args)
	if code != 0 {
		t.Fatalf("runTrailers must never fail a commit over an unreachable meter; exit = %d", code)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Gonk-Bead: gk-1a2b") {
		t.Fatalf("expected the shipped default (commit_trailers=on) even with meter unreachable:\n%s", got)
	}
}

func TestRunTrailersIsIdempotentAcrossTwoInvocations(t *testing.T) {
	project := meterapi.ProjectResponse{
		Project: "group/repo", State: meterapi.StateActive,
		Effective: &meterapi.Effective{Provenance: meterapi.Provenance{CommitTrailers: true, IncludeUsage: false}},
	}
	url := fakeTrailersMeter(t, project, meterapi.SessionCostResponse{})
	path := writeCommitMsgFile(t, "feat: add a widget\n")

	args := trailersArgsFor(t, baseTrailerTags(), "some-model")
	args.CommitMsgFile = path

	deps := trailersDeps{Meter: meterClient(url), Version: "0.1.0"}
	if code := runTrailers(context.Background(), deps, args); code != 0 {
		t.Fatalf("first run: exit = %d", code)
	}
	if code := runTrailers(context.Background(), deps, args); code != 0 {
		t.Fatalf("second run: exit = %d", code)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "Generated-By:"); n != 1 {
		t.Fatalf("running the hook twice duplicated the trailer block (%d Generated-By lines):\n%s", n, got)
	}
}

func TestRunTrailersNoMetadataLeavesMessageUntouched(t *testing.T) {
	path := writeCommitMsgFile(t, "feat: add a widget\n")
	code := runTrailers(context.Background(), trailersDeps{
		Meter: meterClient("http://127.0.0.1:1"), Version: "0.1.0",
	}, trailersArgs{CommitMsgFile: path, MetadataJSON: ""})
	if code != 0 {
		t.Fatalf("missing metadata must not fail the commit; exit = %d", code)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "feat: add a widget\n" {
		t.Fatalf("no attribution metadata: message must be untouched, got:\n%q", got)
	}
}
