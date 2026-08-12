package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// stubForge is a minimal issueReader: it returns a canned issue (or error) so
// the broker's context-fetch can be exercised without a live GitLab.
type stubForge struct {
	iss *glab.Issue
	err error
	// proj/files/repoErr back the repoReader half (scaffold's controller-side
	// repository read). Zero values mean "no repository readable", which is the
	// case buildRepoContext must report as an error rather than as thin-but-ok
	// material -- see TestScaffoldPromptRefusesWithoutRepoContext.
	proj    *glab.Project
	files   map[string]string
	repoErr error
}

func (s stubForge) GetIssue(_ context.Context, _, _ int64) (*glab.Issue, error) {
	return s.iss, s.err
}

func (s stubForge) GetProject(_ context.Context, _ int64) (*glab.Project, error) {
	if s.repoErr != nil {
		return nil, s.repoErr
	}
	if s.proj == nil {
		return &glab.Project{PathWithNamespace: "acme/widget", DefaultBranch: "main"}, nil
	}
	return s.proj, nil
}

func (s stubForge) GetRawFile(_ context.Context, _ int64, path, _ string, _ int64) ([]byte, error) {
	if body, ok := s.files[path]; ok {
		return []byte(body), nil
	}
	return nil, fmt.Errorf("404 %s", path)
}

// With a forge configured, the broker fetches the issue controller-side and
// splices its title/labels/body into the prompt -- the agent pod cannot fetch
// it (no creds), so the context MUST come pre-fetched in the prompt.
func TestDispatchInjectsIssueContext(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{iss: &glab.Issue{
		IID: 3, Title: "Login button does nothing on Safari", State: "opened",
		Labels:      []string{"needs-triage"},
		Description: "Steps: click login in Safari 17. Nothing happens. Works in Chrome.",
	}}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: forge, Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one", gc.Created)
	}
	// The prompt rides the SUBMIT, not the create: k8s-backed sessions never
	// receive template_overrides.initial_message (gonk-u1p.1 / gonk-drf).
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one", gc.Submitted)
	}
	msg := gc.Submitted[0].Message
	for _, want := range []string{
		"fetched for you",
		"Login button does nothing on Safari", // title
		"needs-triage",                        // current label
		"Nothing happens. Works in Chrome.",   // body
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg)
		}
	}
}

// A forge fetch failure must NOT fail the dispatch: the session is still
// created (a re-sling can retry context), with the degraded reference-only
// prompt marker.
func TestDispatchProceedsWhenContextFetchFails(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Model: "m", Attempt: 1, ReservationID: "rsv-1",
	}}
	forge := stubForge{err: context.DeadlineExceeded}

	code := runDispatch(context.Background(), dispatchDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Forge: forge, Args: baseDispatchArgs(),
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (a context-fetch failure must not fail the run)", code)
	}
	if len(gc.Created) != 1 {
		t.Fatalf("created = %+v, want one even on a context miss", gc.Created)
	}
	if len(gc.Submitted) != 1 {
		t.Fatalf("submitted = %+v, want one even on a context miss", gc.Submitted)
	}
	if !strings.Contains(gc.Submitted[0].Message, "issue context unavailable") {
		t.Fatalf("prompt should carry the degraded marker:\n%s", gc.Submitted[0].Message)
	}
}

func TestCapBody(t *testing.T) {
	// Under cap: unchanged.
	if got := capBody("short", 100); got != "short" {
		t.Fatalf("under-cap body changed: %q", got)
	}
	// Over cap: truncated to <= max bytes of original + a visible marker.
	big := strings.Repeat("a", 50)
	got := capBody(big, 10)
	if !strings.Contains(got, "truncated by gonk") {
		t.Fatalf("over-cap body lacks truncation marker: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Fatalf("kept the wrong prefix: %q", got)
	}
	// Multibyte safety: cutting mid-rune must not leave an invalid trailing byte.
	multi := strings.Repeat("é", 20) // 2 bytes each
	if got := capBody(multi, 5); strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation split a rune: %q", got)
	}
}

// THE POINT OF gonk-msz. The old scaffold prompt asserted the repository "is
// checked out in your working directory" and encouraged reading it, while the
// pod's /workspace is empty and nothing ever clones into it. Combined with that
// prompt's "an honest gap is a SUCCESS", the model would find nothing, invent
// plausible context, and gonk-sweep would commit it and open an MR -- durable
// wrong .agent/ content that every later session reads as ground truth.
//
// So with no material the prompt must REFUSE, and it must not hand the model a
// batch template to fill in.
func TestScaffoldPromptRefusesWithoutRepoContext(t *testing.T) {
	got := renderScaffoldPrompt("acme/widget", "")

	if strings.Contains(got, batchStartSentinel) {
		t.Errorf("refusal prompt must NOT include the %s template; with nothing to\n"+
			"describe, offering the fence is an invitation to invent one:\n%s",
			batchStartSentinel, got)
	}
	if !strings.Contains(got, "Emit NO batch") {
		t.Errorf("refusal prompt must tell the agent to emit no batch, got:\n%s", got)
	}
	for _, banned := range []string{"checked out in your working directory", "Reading the working directory"} {
		if strings.Contains(got, banned) {
			t.Errorf("prompt still claims a checkout exists (%q); nothing clones one:\n%s", banned, got)
		}
	}
}

// With real material the prompt carries it verbatim, tells the agent it has no
// checkout, and only THEN offers the batch fence.
func TestScaffoldPromptCarriesRepoContext(t *testing.T) {
	got := renderScaffoldPrompt("acme/widget", "Repository: acme/widget\n\n--- go.mod ---\nmodule acme/widget\n")

	for _, want := range []string{
		"module acme/widget",             // the material itself
		"You do NOT have the repository", // the correction to the old lie
		"Base EVERY claim on the material above",
		batchStartSentinel,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scaffold prompt missing %q, got:\n%s", want, got)
		}
	}
}

// buildRepoContext must treat "project metadata resolved but not one file could
// be read" as a FAILURE. Returning the two header lines would look like success
// to the caller and license a fabricated .agent/ -- the exact outcome this
// change exists to prevent.
func TestBuildRepoContextFailsWhenNoFilesReadable(t *testing.T) {
	_, err := buildRepoContext(context.Background(), stubForge{}, 42)
	if err == nil {
		t.Fatal("expected an error when no probed file is readable; a repo we cannot read at all must not look like thin-but-valid material")
	}
}

// The happy path: files that exist are spliced in, files that 404 are simply
// absent (their absence is itself a fact about the project), and one unreadable
// file does not fail the whole fetch.
func TestBuildRepoContextSplicesFoundFilesOnly(t *testing.T) {
	f := stubForge{files: map[string]string{
		"README.md": "# Widget\nDoes widget things.",
		"go.mod":    "module acme/widget",
	}}
	got, err := buildRepoContext(context.Background(), f, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"acme/widget", "--- README.md ---", "Does widget things.", "--- go.mod ---"} {
		if !strings.Contains(got, want) {
			t.Errorf("repo context missing %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "package.json") {
		t.Errorf("repo context names a file that does not exist; absence must stay absent:\n%s", got)
	}
}

// THE OWNER'S POINT: the checkout decision belongs at the decision point, and it
// is per EVENT SHAPE. Most shapes need a working copy; some (an MR approval,
// which acts on forge state and reads no files) do not, and granting one there
// would be pure cost.
func TestCheckoutIsDecidedPerEventShape(t *testing.T) {
	if !needsCheckout["scaffold"] {
		t.Error("scaffold MUST get a checkout: describing a repository it cannot read is exactly the failure this exists to stop")
	}
	if !needsCheckout["triage"] {
		t.Error("triage needs a checkout too: .agent/ is what makes a judgement specific to THIS repo, and it is exactly what scaffold writes")
	}
	if needsCheckout["mr-approval"] {
		t.Error("an unknown/no-read shape must default to NO checkout")
	}
}

// grantCheckout must be silent and harmless for a shape that needs no checkout,
// and must never fail a dispatch when the rig is simply not configured.
func TestGrantCheckoutIsANoOpWhenNotNeededOrNotConfigured(t *testing.T) {
	d := dispatchDeps{Log: testLogger(io.Discard), Args: baseDispatchArgs(), Forge: stubForge{}}

	if url, err := grantCheckout(context.Background(), d, "triage", "alias-a"); url != "" || err != nil {
		t.Errorf("triage: got (%q, %v), want no checkout and no error", url, err)
	}
	// scaffold NEEDS one, but with no rig configured this must still be a clean
	// no-op that lets the caller fall back rather than failing the run.
	if url, err := grantCheckout(context.Background(), d, "scaffold", "alias-a"); url != "" || err != nil {
		t.Errorf("scaffold with no rig configured: got (%q, %v), want a clean no-op", url, err)
	}
}

// The checkout prompt must tell the agent the repository IS present -- the exact
// claim the no-checkout prompt is forbidden from making.
func TestScaffoldCheckoutPromptSaysTheRepoIsPresent(t *testing.T) {
	got := renderScaffoldCheckoutPrompt("acme/widget", "http://gonk-intake-internal.gonk.svc:9090/rig/a.tar.gz")
	for _, want := range []string{
		"checked out in your working directory",
		"you hold no credentials",
		batchStartSentinel,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checkout prompt missing %q:\n%s", want, got)
		}
	}
	// It must NOT tell the agent to go fetch anything itself.
	if strings.Contains(got, "git clone") {
		t.Errorf("the prompt tells the agent to clone; the entrypoint does that before opencode starts:\n%s", got)
	}
}

// The triage prompt may mention a working copy ONLY when one was granted.
// Claiming a checkout that is not there is the precise gonk-msz failure: the
// model goes looking, finds an empty directory, and fills the gap itself.
func TestTriagePromptMentionsTheRepoOnlyWhenGranted(t *testing.T) {
	without := renderTriagePrompt("acme/widget", 3, "Title: x", "")
	if strings.Contains(without, "checked out in your working directory") {
		t.Errorf("triage claims a checkout with none granted:\n%s", without)
	}
	if !strings.Contains(without, "fetched for you") {
		t.Error("the no-checkout prompt must still carry the injected issue")
	}

	with := renderTriagePrompt("acme/widget", 3, "Title: x", "http://intake:9090/rig/a.tar.gz")
	for _, want := range []string{"checked out in your working directory", ".agent/", "hold no credentials"} {
		if !strings.Contains(with, want) {
			t.Errorf("granted-checkout triage prompt missing %q:\n%s", want, with)
		}
	}
}
