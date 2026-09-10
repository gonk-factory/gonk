package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/rig"
)

// issueReader is the sliver of pkg/glab the broker's inject needs: read the
// issue the controller is about to triage. Because the agent pod holds NO forge
// credentials (the broker's whole point), the agent cannot fetch the issue
// itself -- the CONTROLLER fetches it here (with its own PAT) and splices the
// context into the prompt. Satisfied by *glab.Client.
type issueReader interface {
	GetIssue(ctx context.Context, projectID, issueIID int64) (*glab.Issue, error)
}

// maxIssueBodyBytes caps the issue description spliced into the prompt. An issue
// body is untrusted, caller-controlled input of unbounded size; it must not be
// able to blow the model's context or the session-create request body. Over-cap
// bodies are truncated with an explicit marker (§11 OQ5).
const maxIssueBodyBytes = 8 << 10 // 8 KiB

// agentForTrigger maps a trigger to the broker AGENT that handles it, and --
// since ADR-007 §3 deleted the formula layer -- is the ONLY routing table
// left in gonk-gate. A trigger in this set is dispatched via the v2 broker:
// dispatch creates the agent session DIRECTLY (POST
// /v0/city/{city}/sessions), correlates it by a unique alias, and injects
// the rendered prompt as the session's initial message. The agent produces a
// proposed-effects batch and posts nothing; gonk-sweep validates the batch's
// shape and applies it under the controller's own bot PAT. The pod holds no
// forge creds.
//
// Triage was the first ported trigger; scaffold followed (see below).
// mention-reply is DELIBERATELY ABSENT: it used to pour a formula
// (gonk-mention) that provably could not deliver its prompt (upstream Gas
// City drops caller vars, gonk-6gs / #4668); ADR-007 §3 deleted that pour
// rather than port mention here too, so runDispatch now refuses the trigger
// outright until T-24 ports it for real.
var agentForTrigger = map[string]string{
	"issue-triage": "triage",
	// scaffold is ported for the same reason triage was, and it is what unblocks
	// ONBOARDING: intake gates triage until .agent/ exists (spec 5.3), .agent/ is
	// created by scaffold, and scaffold on the formula path could never deliver
	// its prompt (#4891) or even learn which repository it was for (#4668). A
	// newly onboarded project therefore sat at `pending` forever -- see gonk-bgx.
	"scaffold": "scaffold",
}

// brokerSessionAlias is the correlation key stamped on the created session and
// recorded on the bead (Record.SessionID). gonk-sweep reads the session back by
// this alias via GetSessionOutput.
//
// It carries 128 bits of crypto/rand as a base32 suffix, and THAT ENTROPY IS
// LOAD-BEARING (gonk-mzd): the alias is the capability the agent pod presents to
// fetch its own prompt, on the meter's one unauthenticated route. A deterministic
// alias -- which this used to be -- is reconstructible from a project and issue
// number by anyone who can reach the meter, which would make that route a public
// read of issue text. The meter refuses a nonce-free alias by shape for the same
// reason.
//
// The attempt segment stays: gascity rejects a create whose alias is taken, so a
// re-sling must not collide with the prior attempt's still-present session. The
// nonce makes that automatic, but keeping the segment keeps the alias legible in
// logs. session.ValidateAlias forbids colons (bead anchors use them) and caps
// length at 64; this form uses only [a-z0-9.A-Z2-7] and stays under the cap at
// ~49 characters.
//
// NOTHING IN PRODUCTION RECONSTRUCTS AN ALIAS. Sweep reads Record.SessionID;
// this is the one call site. Tests that used to recompute it must capture
// gc.Created[0].Alias instead -- a better assertion anyway, since it checks what
// was actually sent rather than re-running the generator and agreeing with
// itself.
func brokerSessionAlias(agent string, projectID, issueIID int64, attempt int) string {
	var b [16]byte // 128 bits
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not survivable here: a predictable alias is a
		// public read of the prompt, so refuse rather than degrade.
		panic("gonk-gate: crypto/rand unavailable, refusing to mint a guessable session alias: " + err.Error())
	}
	nonce := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	// Scaffold is PROJECT-scoped: it is dispatched with no issue, so an `.i0`
	// segment would be a lie about what the session is for. Two agents working
	// the same project must not collide, hence the agent in the alias.
	if issueIID == 0 {
		return fmt.Sprintf("gonk.%s.p%d.a%d.%s", agent, projectID, attempt, nonce)
	}
	return fmt.Sprintf("gonk.%s.p%d.i%d.a%d.%s", agent, projectID, issueIID, attempt, nonce)
}

// renderTriagePrompt builds the session's initial message.
//
// IT CARRIES NO <!-- gonk:model / gonk:meta --> MARKER LINES, and must not.
// Those were read by gonk-agent-entrypoint out of its --prompt ARGUMENT, and on
// this backend there is no such argument: the k8s provider never composes
// PromptSuffix onto the launch command, so the prompt arrives by submit LONG
// AFTER opencode has booted and chosen its model from static pod env. A marker
// delivered that way can never be consumed -- and, far worse, "<!--" contains a
// "!", which puts opencode's composer into shell mode (see
// sanitizeForKeystrokeDelivery). They were pure harm here.
//
// The per-bead attribution those markers carried is therefore NOT reaching the
// pod on this path. gonk-agent-entrypoint already says so out loud ("spend rows
// for this session will NOT carry per-bead attribution") rather than failing;
// closing that gap needs a real out-of-band channel, tracked separately.
//
// The body instructs the agent to emit a proposed-effects batch and to call NO
// external API: with the broker, the agent has no forge credentials, so it must
// not (and cannot) post anything itself.
//
// The issue's title/body/labels are injected as issueContext (built by
// buildIssueContext from a controller-side fetch) because the pod cannot fetch
// them itself. issueContext is empty only when the fetch was unavailable or
// failed -- a degraded, reference-only prompt. The exact emit wording may be
// tightened after C2's first live run confirms the fenced batch survives the
// GetSession(peek) read (C5).
func renderTriagePrompt(project string, issueIID int64, agentContext, issueContext, checkout string) string {
	context := issueContext
	if strings.TrimSpace(context) == "" {
		context = "(issue context unavailable -- triage from the issue reference alone)"
	}
	// THE PROJECT'S OWN CONTEXT COMES FIRST, ahead of the issue -- this is the
	// v1 thin loader (T-08). `.agent/` is what makes a triage judgement specific
	// to THIS repository instead of generic, and until now nothing on the
	// no-checkout path read it at all.
	//
	// It is a PREFIX rather than a section spliced into the body on purpose:
	// when there is no `.agent/`, this string is empty and the prompt is byte
	// for byte the one triage produced before. That property is asserted in
	// broker_inject_test.go, and it is what keeps "the directory is optional"
	// from quietly meaning "the prompt changed for everyone".
	agent := ""
	if strings.TrimSpace(agentContext) != "" {
		agent = "This project's own context, from its `.agent/` directory. IT OVERRIDES\n" +
			"anything you would otherwise assume about this repository, and you should\n" +
			"read it before the issue below. It may still be the unedited seed gonk\n" +
			"committed when the project was onboarded: any section marked NOT FILLED IN\n" +
			"YET is a section nobody has written, so treat it as absent rather than as a\n" +
			"statement about the project.\n\n" +
			agentContext + "\n\n" +
			"--- end of the project's context; the issue follows ---\n\n"
	}
	// Mentioned ONLY when a checkout was actually granted and fetched. Claiming a
	// working copy that is not there is the precise failure gonk-msz was: the
	// model goes looking, finds an empty directory, and fills the gap itself.
	repo := ""
	if strings.TrimSpace(checkout) != "" {
		// INVESTIGATE BEFORE ASKING (gonk-kta). On gonk's first successful triage
		// the agent read two files and then asked the reporter how sorting and
		// paging were implemented -- something it could have grepped for. The
		// reporter's answer was the correct critique: "it's hosted in this
		// project, you should be able to search the codebase yourself". Saying
		// "reading files is expected" was not enough, because the emit
		// instructions ask for questions and nothing pushed the other way.
		repo = "\nThe repository is checked out in your working directory. Read `.agent/` " +
			"first if it exists -- it is the project's own context and it overrides " +
			"anything you would otherwise assume. You still hold no credentials, so do " +
			"not try to reach GitLab.\n\n" +
			"LOOK AT WHAT IS ACTUALLY IN THE TREE BEFORE YOU SEARCH FOR IT. List the " +
			"directory first; do not guess at file extensions. On issue !49 a run " +
			"globbed five times for the wrong languages, concluded no relevant code " +
			"existed, and asked the reporter for help while the answer sat in a file " +
			"it never listed.\n\n" +
			"INVESTIGATE THE CODE BEFORE YOU ASK ANYTHING. Search for the behaviour the " +
			"issue describes and read the code that implements it. Ask the reporter only " +
			"for what the code CANNOT tell you -- their intent, their environment, exact " +
			"reproduction steps, which behaviour they expected. Anything answerable by " +
			"reading this repository you are expected to answer yourself, citing the " +
			"files you relied on. If you searched and genuinely found nothing relevant, " +
			"say that explicitly rather than asking a question you could have answered.\n"
	}
	return fmt.Sprintf(`%sTriage GitLab issue #%d in project `+"`%s`"+`. Here is the issue, already
fetched for you -- do NOT fetch anything yourself:

%s
%s
Decide the labels (each prefixed `+"`gonk::`"+`), one short triage comment, and a
VERDICT saying what kind of answer this is.

The comment is a brief analysis of what the issue asks for, grounded in the code
where you could find it. Ask the reporter only what the code cannot tell you.

The verdict must be EXACTLY ONE of:

  "reply-only"   No code change is needed, but the reporter needs an answer --
                 a question, a clarification, or "this works as designed, and
                 here is why".
  "code-change"  A genuine defect with an identifiable fix. Say in the comment
                 WHICH code is wrong and WHAT should change. Do not write the
                 fix here; that is a separate step.
  "close"        Terminal. No further discussion is useful -- a duplicate, an
                 obsolete report, something already fixed, or something you
                 established is not reproducible. The comment MUST say why.

Choose "close" only when you are confident, because it ENDS THE CONVERSATION.
When you are unsure between close and reply-only, choose reply-only and ask.

Do NOT post anything yourself. Do NOT run glab, git, bd, or any external API --
you hold no credentials and any such call will fail. Instead, emit your decision
as a single proposed-effects batch as the LAST thing in your output, fenced
EXACTLY like this:

GONK_BATCH_START
{"verdict":"<one of: reply-only, code-change, close>","effects":[{"kind":"comment","body":"<your comment>"},{"kind":"label","add":["gonk::<label>"]}]}
GONK_BATCH_END

Emit exactly one comment effect and zero or more label effects. Nothing after
GONK_BATCH_END.

The batch must be valid JSON on a SINGLE line. Keep the comment to one
paragraph, and if you must include a line break write it as \n inside the
string -- a real line break inside a JSON string is invalid and costs you the
whole batch.`, agent, issueIID, project, context, repo)
}

// renderScaffoldPrompt builds the scaffold session's initial message.
//
// IT IS A DIFFERENT SHAPE OF JOB from the v1 formula prompt it replaces
// (pack/agents/scaffold/prompt.template.md -- deleted with the rest of the
// formula layer, ADR-007 §3), and the difference is the whole point of the
// port: that prompt told the agent to create a branch and open a
// merge request ITSELF, which needs forge credentials the broker deliberately
// denies it. Here the agent only PROPOSES file content; gonk-sweep commits it
// onto gonk/scaffold and opens exactly one MR under the controller's own PAT.
//
// So there is no marker line to copy, no branch to create and no MR to open --
// every instruction about those was a way for the run to fail at something the
// controller now does deterministically.
//
// THE REPOSITORY IS NOT IN THE POD, and this prompt used to say it was.
//
// It previously opened "which is checked out in your working directory" and
// closed with "Reading the working directory is expected and encouraged". None
// of that is true: images/Dockerfile.agent creates /workspace EMPTY, the
// entrypoint only ASSUMES a clone (RIG_DIR=${GONK_RIG_DIR:-$PWD}) and treats a
// missing .git as non-fatal, and the controller's Gas City root has no rigs
// directory at all. Gas City's CreateSessionRequest has no rig field either, and
// pods are pooled and generic -- at pod start there is no project yet, so an
// entrypoint clone has nothing to clone (gonk-msz).
//
// Telling a model to read a directory that is empty is the worst possible
// framing, because this prompt also says an honest gap is a SUCCESS: the model
// finds nothing, invents plausible context, and gonk-sweep commits it and opens
// an MR. That produces CONFIDENT WRONG .agent/ files -- and .agent/ is durable
// context every later triage and mention session reads as ground truth.
//
// So the controller supplies the material instead, exactly as it already does
// for triage: runBrokerDispatch fetches with its own PAT and splices the result
// in (see buildIssueContext / buildRepoContext). Same reason as triage -- the
// pod holds no forge creds -- and it needs no clone channel at all.
//
// repoContext is empty only when every fetch failed. That is NOT survivable
// here the way a reference-only triage prompt is: with no material there is
// nothing to be accurate ABOUT, so the prompt instructs an explicit refusal
// rather than letting the model fill the vacuum.
func renderScaffoldPrompt(project, repoContext string) string {
	material := strings.TrimSpace(repoContext)
	if material == "" {
		return fmt.Sprintf(`Write durable project context for the repository `+"`%s`"+`.

The repository content could NOT be retrieved for this session, so there is
nothing to base a description on.

Do NOT guess, and do NOT describe this project from its name or from anything
you already believe about it. Emit NO batch at all: reply with a single line
saying the repository content was unavailable, and nothing else.`, project)
	}
	return fmt.Sprintf(`Write durable project context for the repository `+"`%s`"+`.

You do NOT have the repository checked out. Everything you know about it is the
material below, retrieved for you -- do NOT try to read a working directory, and
do NOT fetch anything yourself:

%s

From that material, write what this project IS, how it is built, how it is
tested, and any conventions a future automated triage or code session would
otherwise have to guess at. Prefer a few honest, specific files over one long
vague one.

Base EVERY claim on the material above. An honest gap is a SUCCESS, not a
failure: if something is not determinable from what you were given, write it
down as an open question instead of inventing an answer. A confident wrong claim
here will mislead every session that reads this directory afterwards, so
"unknown" is always the better answer than a plausible guess.

Do NOT run git, glab, bd, or any external API, and do NOT try to commit anything
or open a merge request: you hold no credentials, and gonk commits your proposal
and opens the merge request for you.

Emit your proposal as a single proposed-effects batch as the LAST thing in your
output, fenced EXACTLY like this:

GONK_BATCH_START
{"effects":[{"kind":"file","path":".agent/README.md","content":"<the whole file>"}]}
GONK_BATCH_END

Every path MUST begin with `+"`.agent/`"+` -- a batch touching anything else is
rejected in full. Emit one file effect per file, each carrying that file's
COMPLETE content (there are no partial edits). Nothing after GONK_BATCH_END.`, project, material)
}

// buildIssueContext fetches the issue and renders its title/labels/body into the
// prompt-embeddable block, with the body size-capped. It is best-effort: on any
// error (no reader configured, forge unreachable, issue gone) it returns "" and
// a non-nil err for the caller to log -- dispatch proceeds with a reference-only
// prompt rather than failing the whole run over a context fetch.
func buildIssueContext(ctx context.Context, r issueReader, projectID, issueIID int64) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no issue reader configured")
	}
	iss, err := r.GetIssue(ctx, projectID, issueIID)
	if err != nil {
		return "", err
	}
	body := capBody(iss.Description, maxIssueBodyBytes)
	labels := "(none)"
	if len(iss.Labels) > 0 {
		labels = strings.Join(iss.Labels, ", ")
	}
	return fmt.Sprintf("Title: %s\nState: %s\nCurrent labels: %s\n\n%s",
		iss.Title, iss.State, labels, body), nil
}

// grantCheckout registers a per-session checkout and returns the URL the pod
// fetches it from, or "" when this event shape does not need one.
//
// Called at the DECISION POINT: the project and ref are known, the controller
// holds the PAT, and the pod holds neither. The grant binds this one session
// alias to that one project at that one ref -- see pkg/rig for why registration
// is the authorization.
//
// Every failure here is NON-FATAL by design. A session without a checkout still
// runs, on a prompt that says so; failing the dispatch instead would turn a
// degraded run into no run at all.
func grantCheckout(ctx context.Context, d dispatchDeps, agent, alias string) (string, error) {
	if !needsCheckout[agent] {
		return "", nil
	}
	if d.Rig == nil || d.RigBaseURL == "" {
		return "", nil // not configured; the fallback tiers cover it
	}
	if d.Forge == nil {
		return "", fmt.Errorf("no forge reader to resolve the ref")
	}
	proj, err := d.Forge.GetProject(ctx, d.Args.ProjectID)
	if err != nil {
		return "", fmt.Errorf("resolve default branch: %w", err)
	}
	ref := proj.DefaultBranch
	if ref == "" {
		ref = "main"
	}
	// PIN THE REF at grant time. If the pod resolved "the default branch" for
	// itself, a push landing mid-session would change what it read, and the
	// batch it proposes would describe a tree nobody can reconstruct.
	if err := d.Rig.Grant(ctx, alias, d.Args.Project, d.Args.ProjectID, ref); err != nil {
		return "", fmt.Errorf("register grant: %w", err)
	}
	d.Log.Info("checkout granted for this session",
		"bead", d.Args.BeadAnchor, "alias", alias, "project", d.Args.Project, "ref", ref)
	return rig.FetchURL(d.RigBaseURL, alias), nil
}

// renderScaffoldCheckoutPrompt is the scaffold prompt when the agent HAS a real
// working copy. It is the good case: unlike renderScaffoldPrompt's probe-list
// material, the agent can read whatever it needs.
//
// The fetch itself is the entrypoint's job, done BEFORE opencode starts, so the
// checkout is a precondition rather than a task the model can fail. The URL is
// named here anyway so the transcript records where the tree came from.
func renderScaffoldCheckoutPrompt(project, url string) string {
	return fmt.Sprintf(`Write durable project context for the repository `+"`%s`"+`, which IS
checked out in your working directory (fetched for you from %s -- you do not need
to fetch anything, and you hold no credentials to fetch anything else).

Read enough of it to be accurate: what this project IS, how it is built, how it
is tested, and any conventions a future automated triage or code session would
otherwise have to guess at. Prefer a few honest, specific files over one long
vague one.

An honest gap is a SUCCESS, not a failure. If something cannot be determined
from the repository, write that down as an open question instead of inventing an
answer -- a confident wrong claim here will mislead every session that reads
this directory afterwards.

Do NOT run git, glab, bd, or any external API, and do NOT try to commit anything
or open a merge request: you hold no credentials, and gonk commits your proposal
and opens the merge request for you. Reading the working directory is expected
and encouraged.

Emit your proposal as a single proposed-effects batch as the LAST thing in your
output, fenced EXACTLY like this:

GONK_BATCH_START
{"effects":[{"kind":"file","path":".agent/README.md","content":"<the whole file>"}]}
GONK_BATCH_END

Every path MUST begin with `+"`.agent/`"+` -- a batch touching anything else is
rejected in full. Emit one file effect per file, each carrying that file's
COMPLETE content (there are no partial edits). Nothing after GONK_BATCH_END.`, project, url)
}

// scaffoldProbeFiles are the paths buildRepoContext asks the forge for, in
// order. They are the files that actually identify a project's language, build
// and test story -- which is precisely what .agent/ has to get right.
//
// A FIXED LIST, not a tree walk, because pkg/glab has no tree-listing call and
// this needs none: an absent file is itself a fact (no go.mod means it is not a
// Go project), so the hit/miss pattern carries most of the signal. Adding a path
// here is cheap; each miss is one 404 the controller absorbs.
var scaffoldProbeFiles = []string{
	"README.md", "README.rst", "README",
	"CONTRIBUTING.md", "CLAUDE.md", "AGENTS.md",
	"go.mod", "package.json", "pyproject.toml", "requirements.txt",
	"Cargo.toml", "pom.xml", "build.gradle", "Gemfile", "composer.json",
	"Makefile", "Justfile", "Taskfile.yml",
	"Dockerfile", "docker-compose.yml", "compose.yaml",
	".gitlab-ci.yml", ".github/workflows/ci.yml",
}

// maxRepoFileBytes caps EACH probed file. Repository content is caller-
// controlled and unbounded; a vendored lockfile or a generated README must not
// be able to blow the model's context. Deliberately smaller than
// maxIssueBodyBytes because scaffold splices MANY files where triage splices
// one body.
const maxRepoFileBytes = 4 << 10 // 4 KiB per file

// maxRepoContextBytes caps the WHOLE spliced block. Rung 1 (qwen3-14b) serves a
// 16384-token total window and opencode's own agent preamble already measures
// ~6.4K of it, so the material has to leave room for both the rest of the prompt
// and the model's output. This bound is the reason scaffold cannot simply be
// handed a repository.
const maxRepoContextBytes = 24 << 10 // 24 KiB total

// repoReader is the sliver of pkg/glab buildRepoContext needs. The agent pod
// holds NO forge credentials, so the CONTROLLER reads the repository with its
// own PAT and splices the result into the prompt -- the same division of labour
// as issueReader, and for the same reason. Satisfied by *glab.Client.
type repoReader interface {
	GetProject(ctx context.Context, projectID int64) (*glab.Project, error)
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
}

// needsCheckout says whether an agent's EVENT SHAPE requires a working copy of
// the repository.
//
// This is the owner's point, and it belongs exactly here: the decision point is
// already where gonk has read the project's config, parsed it and compared it
// against the event, so "does this shape need a checkout?" is one more property
// of the same decision rather than a separate lookup later. Most shapes do need
// one; some genuinely do not -- an MR approval acts on forge state and reads no
// files, and granting it a checkout would be pure cost.
//
// scaffold: YES. Its entire job is describing the repository, and without a
// checkout it either invents the description or refuses (gonk-msz).
//
// triage: YES. The issue is still injected -- that does not change -- but the
// project's own .agent/ context is the thing that makes a triage judgement
// specific to THIS repository rather than generic. The v1 formula told the agent
// to "Load .agent/ from the repository first" with no repository present, which
// is the same empty-directory failure as gonk-msz.
//
// Note the checkout is no longer the ONLY way that context arrives (T-08):
// buildAgentContext splices the seeded .agent/ files into the prompt
// controller-side, so a project gets its own context even when no working copy
// could be granted. The checkout still matters -- it is what lets the agent read
// the CODE the issue is about.
//
// A missing checkout stays non-fatal here. renderTriagePrompt only mentions the
// working copy when one was actually granted, so a fetch failure degrades to
// exactly the prompt triage used before.
var needsCheckout = map[string]bool{
	"scaffold": true,
	"triage":   true,
}

// brokerForgeReader is what dispatchDeps.Forge must satisfy: both halves of the
// controller-side read. Kept as one field because a single *glab.Client backs
// both, and splitting it would let a caller wire triage's reader without
// scaffold's and only discover it at dispatch time.
type brokerForgeReader interface {
	issueReader
	repoReader
}

// buildRepoContext assembles the repository material for a scaffold prompt.
//
// Best-effort per file: a probe that 404s is simply absent from the result, and
// absence is information (see scaffoldProbeFiles). An error is returned ONLY
// when nothing at all could be read, because that is the case the caller must
// not paper over -- renderScaffoldPrompt turns it into an explicit refusal
// rather than letting the model invent a project description (gonk-msz).
func buildRepoContext(ctx context.Context, r repoReader, projectID int64) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no repo reader configured")
	}
	proj, err := r.GetProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	ref := proj.DefaultBranch
	if ref == "" {
		ref = "main"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\nDefault branch: %s\n", proj.PathWithNamespace, ref)

	found := 0
	for _, path := range scaffoldProbeFiles {
		if b.Len() >= maxRepoContextBytes {
			fmt.Fprintf(&b, "\n[further files omitted: context budget reached]\n")
			break
		}
		raw, ferr := r.GetRawFile(ctx, projectID, path, ref, maxRepoFileBytes)
		if ferr != nil || len(raw) == 0 {
			continue
		}
		found++
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", path, capBody(string(raw), maxRepoFileBytes))
	}

	// Project metadata alone is NOT material. Without at least one real file
	// there is nothing to describe, and returning the two header lines would
	// look like success and license a fabricated .agent/.
	if found == 0 {
		return "", fmt.Errorf("no readable files among %d probed paths in %s",
			len(scaffoldProbeFiles), proj.PathWithNamespace)
	}
	return b.String(), nil
}

// agentContextFiles are the `.agent/` paths a triage prompt loads, in order.
//
// They are the paths gonk's own onboarding seed writes (pkg/intake's
// AgentSeedPaths), which is what makes a fixed list workable at all: pkg/glab
// has no tree-listing call, and asking for names gonk itself chose is not a
// guess. TestAgentContextFilesCoverTheOnboardingSeed fails if the seed grows a
// file this list does not read.
//
// A project may keep other files under `.agent/`; v1's loader is thin on
// purpose and does not read them. `.agent/README.md` says so, so a maintainer
// is not left wondering why a fifth file had no effect.
var agentContextFiles = []string{
	".agent/README.md",
	".agent/overview.md",
	".agent/build-and-test.md",
	".agent/conventions.md",
}

// maxAgentContextBytes caps the WHOLE `.agent/` block spliced into a triage
// prompt. Repository content is caller-controlled and unbounded, and rung 1
// serves a 16384-token total window that already carries opencode's preamble,
// the issue body (up to maxIssueBodyBytes) and the emit instructions. This is
// deliberately no larger than the issue's own cap: the project's context frames
// the answer, the issue IS the question.
const maxAgentContextBytes = 8 << 10 // 8 KiB total

// buildAgentContext reads the project's `.agent/` directory controller-side --
// the pod holds no forge credentials, the same division of labour as
// buildIssueContext and buildRepoContext.
//
// ABSENCE IS NOT AN ERROR, and that is the whole point of T-08: `.agent/` is
// optional context, not a precondition. A project with no such directory gets
// ("", nil) and a prompt identical to the one triage used before. An error is
// returned only when the forge could not be asked at all, so the caller can log
// something a person can act on.
func buildAgentContext(ctx context.Context, r repoReader, projectID int64) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no repo reader configured")
	}
	proj, err := r.GetProject(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	ref := proj.DefaultBranch
	if ref == "" {
		ref = "main"
	}

	var b strings.Builder
	for _, path := range agentContextFiles {
		if b.Len() >= maxAgentContextBytes {
			fmt.Fprintf(&b, "\n[further %s files omitted: context budget reached]\n", agentDirName)
			break
		}
		raw, ferr := r.GetRawFile(ctx, projectID, path, ref, maxRepoFileBytes)
		if ferr != nil || len(raw) == 0 {
			continue // absent, or unreadable: absence is information, not failure
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n\n", path, capBody(string(raw), maxRepoFileBytes))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// agentDirName is the directory those paths live in, named once so a message
// about it cannot disagree with the paths above.
const agentDirName = ".agent/"

// capBody truncates an untrusted issue body to at most max bytes, appending a
// visible marker so the agent (and a human reading the transcript) knows the
// body was cut. Truncation backs up to a rune boundary so the block stays valid
// UTF-8 (a multibyte rune may straddle max).
func capBody(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1] // a partial/continuation byte at the cut point
			continue
		}
		break
	}
	return fmt.Sprintf("%s\n\n[... issue body truncated by gonk: over %d bytes ...]", cut, max)
}

// runBrokerDispatch is the v2 broker's inject step: create the agent session,
// correlate by alias, deliver the rendered prompt. It mirrors the formula
// pour's post-decision bookkeeping (record the reservation + running state on
// the bead) but records the SESSION ALIAS as the correlation key sweep follows.
//
// Exit codes match runDispatch's contract: 0 acted, 1 infra, 2 misconfig.
func runBrokerDispatch(ctx context.Context, d dispatchDeps, agent string, dec meterapi.DecideResponse, base beadstore.Record) int {
	a := d.Args
	// The meter's attribution metadata NOW REACHES THE POD (gonk-m6t). It used
	// to be marshalled only to be logged, because the marker line it rode was
	// unreadable on the submit path -- so spend attributed per install instead
	// of per bead. It travels with the prompt row instead, and the entrypoint
	// stamps it into the opencode overlay's spend-logs header before the model
	// is ever called.
	md, err := json.Marshal(dec.Metadata)
	if err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
	}
	metadataJSON := string(md)
	// IDEMPOTENCY, AND IT IS NOT OPTIONAL (gonk-6n8). `base` is built fresh from
	// the webhook args in runDispatch, so it carries an empty SessionID even when
	// a session for this exact bead+attempt already exists. Nothing else looked,
	// so a second dispatch for one attempt created a SECOND session under the
	// SAME alias.
	//
	// That is not merely untidy. Gas City's create-time alias uniqueness only
	// considers ACTIVE sessions, while its alias RESOLUTION considers all of
	// them -- so once the first session ended, the duplicate create succeeded and
	// the alias permanently resolved to two sessions. Every read after that 409s
	// and never stops (gonk-u6p: gonk:75:issue:24 wedged for eight days). Session
	// TEARDOWN resolves by alias too, so an ambiguous alias also cannot be
	// reliably closed -- which is the gonk-xkm leak wearing a different hat.
	//
	// Measured shape of the failure: go-93gk at 23:11:07Z and go-s4ug at
	// 23:15:36Z, 4m29s apart, both `gonk.triage.p75.i24.a1`. Not a race -- a
	// re-dispatch, at an interval no lock would have covered. The meter is
	// idempotent here BY DESIGN (an open, unsettled reservation returns the same
	// attempt), so the duplicate has to be refused on this side.
	if prior, ok, perr := d.Store.Get(ctx, a.BeadAnchor); perr != nil {
		// Do NOT fail closed. An unreadable store used to mean a guaranteed
		// duplicate; since gonk-u6p a duplicate is recoverable (the sweep
		// classifies it infra-failed at the reservation deadline instead of
		// retrying forever), while refusing here would drop a legitimate
		// dispatch. Proceed, loudly.
		d.Log.Error("could not read the bead store before creating a session; "+
			"proceeding, but a duplicate session for this attempt cannot be ruled out",
			"err", perr, "bead", a.BeadAnchor)
	} else if ok && prior.SessionID != "" && prior.Attempt == dec.Attempt {
		d.Log.Warn("a session already exists for this bead and attempt; not creating a second",
			"bead", a.BeadAnchor, "attempt", dec.Attempt, "session", prior.SessionID)
		return 0
	}

	alias := brokerSessionAlias(agent, a.ProjectID, a.IssueIID, dec.Attempt)

	// The prompt is per-agent. Scaffold is project-scoped and reads the repo
	// from its own rig, so it needs no issue fetched for it -- and asking the
	// forge for issue 0 would be a pointless round trip that logs a warning.
	var prompt string
	switch agent {
	case "scaffold":
		// Read the repository CONTROLLER-SIDE, for the same reason triage's
		// issue is read here: the pod holds no forge creds, and (unlike what the
		// old prompt claimed) it holds no checkout either -- nothing clones one
		// and Gas City has no rig channel to supply one (gonk-msz).
		//
		// Unlike the triage fetch this failure is NOT degraded-but-continue. A
		// reference-only triage prompt still names a real issue the agent can
		// reason about; a scaffold prompt with no repository material has
		// nothing to be accurate about, and the agent's own instructions would
		// then reward it for writing something plausible. renderScaffoldPrompt
		// turns the empty case into an explicit refusal, and the WARN below is
		// what makes that visible rather than silent.
		// THREE TIERS, best first, each strictly safer than inventing content:
		//   1. a real CHECKOUT the pod fetches from gonk-intake (grantCheckout)
		//   2. the controller-side probe of identifying files (buildRepoContext)
		//   3. an explicit refusal (renderScaffoldPrompt with no material)
		if url, gerr := grantCheckout(ctx, d, agent, alias); gerr != nil {
			d.Log.Warn("could not grant a checkout; falling back to controller-side repository context",
				"err", gerr, "bead", a.BeadAnchor, "project", a.Project)
		} else if url != "" {
			prompt = renderScaffoldCheckoutPrompt(a.Project, url)
			break
		}
		repoContext, rerr := buildRepoContext(ctx, d.Forge, a.ProjectID)
		if rerr != nil {
			d.Log.Warn("scaffold repository fetch failed; injecting refusal prompt so the agent cannot invent .agent/ content",
				"err", rerr, "bead", a.BeadAnchor, "project", a.Project)
		}
		prompt = renderScaffoldPrompt(a.Project, repoContext)
	default:
		// Fetch the issue context controller-side (the pod has no forge creds).
		// Best-effort: a fetch failure degrades to a reference-only prompt rather
		// than failing the run -- a re-sling can try again, and the agent still
		// has the issue reference.
		issueContext, ferr := buildIssueContext(ctx, d.Forge, a.ProjectID, a.IssueIID)
		if ferr != nil {
			d.Log.Warn("triage context fetch failed; injecting reference-only prompt",
				"err", ferr, "bead", a.BeadAnchor, "issue", a.IssueIID)
		}
		// A checkout gives triage the project's own .agent/ context. Non-fatal:
		// on failure the prompt simply does not mention a working copy.
		checkout, cerr := grantCheckout(ctx, d, agent, alias)
		if cerr != nil {
			d.Log.Warn("could not grant a checkout for triage; prompting without a working copy",
				"err", cerr, "bead", a.BeadAnchor, "project", a.Project)
		}
		// The project's own `.agent/`, read controller-side for the same reason
		// the issue is: the pod holds no forge credentials. Best-effort and
		// silent when the directory is absent -- that is the common case and
		// not a problem, it just makes the prompt thinner.
		agentContext, aerr := buildAgentContext(ctx, d.Forge, a.ProjectID)
		if aerr != nil {
			d.Log.Warn("could not read the project's .agent/ context; prompting without it",
				"err", aerr, "bead", a.BeadAnchor, "project", a.Project)
		}
		prompt = renderTriagePrompt(a.Project, a.IssueIID, agentContext, issueContext, checkout)
	}
	// STORE THE PROMPT BEFORE CREATING THE SESSION (gonk-mzd). The pod can be
	// up and asking before CreateSession returns, so a prompt written after the
	// create is a race the pod loses -- it would 404 through its whole window
	// and exit.
	//
	// There is no sanitizeForKeystrokeDelivery call here any more, and its
	// deletion is the point rather than a side effect: the prompt is no longer
	// TYPED anywhere, so a `!` in an issue body is just text. Stripping bangs
	// was lossy protection for a channel that no longer exists.
	// The session's own project key rides the prompt row (gonk-8gb). It is the
	// only per-session channel that reaches a pod: order vars are env for the
	// dispatch exec, and a pod's env comes from resolved.Env, which Gas City
	// builds from per-INSTALL city/agent config. Resolved here and passed with
	// the prompt so a session that cannot be metered never gets one.
	litellmKey, kerr := resolveLiteLLMKey(ctx, d.Keys, dec.KeyRef.SecretName, dec.KeyRef.SecretKey)
	if kerr != nil {
		// FAIL CLOSED. A session that cannot be metered must not run, and the
		// old fallback -- the install-wide admin key -- is exactly what made
		// that failure invisible.
		d.Log.Error("refusing to inject: no per-project LiteLLM key",
			"alias", alias, "bead", base.BeadAnchor, "err", kerr)
		return 1
	}
	if err := d.Meter.PutPrompt(ctx, alias, meterapi.PromptRequest{
		Prompt:     prompt,
		Model:      dec.Model,
		Metadata:   metadataJSON,
		LiteLLMKey: litellmKey,
	}); err != nil {
		d.Log.Error("could not store the session prompt; refusing to create a session that would idle",
			"agent", agent, "alias", alias, "bead", a.BeadAnchor, "err", err)
		return 1
	}

	// NOTE the create carries NO Message. It used to, and that is exactly the
	// bug: `message` becomes template_overrides.initial_message, which Gas City
	// puts on runtime.Config.PromptSuffix for the PROVIDER to append to the
	// launch command -- and internal/runtime/k8s never does (tmux/acp/herdr/
	// t3bridge do). Every gonk session is a k8s pod, so the prompt was silently
	// dropped and the agent sat at opencode's idle splash forever, never
	// finishing and so never being swept. The prompt is delivered by the submit
	// below instead. Do NOT "restore" Message here once upstream (gonk-drf) is
	// fixed: that would deliver the prompt twice.
	created, err := d.GC.CreateSession(ctx, gcapi.CreateSessionRequest{
		Kind:  "agent",
		Name:  agent,
		Alias: alias,
		Async: true,
	})
	if err != nil {
		if gcapi.IsNotFound(err) {
			// A 404 on the sessions route is a wrong GONK_CITY (OD-1) or an
			// unloaded pack, same as the pour path -- a misconfiguration.
			d.Log.Error("supervisor 404 on create-session -- check GONK_CITY and that the pack loaded", "err", err)
			return 2
		}
		d.Log.Error("create-session failed", "agent", agent, "alias", alias, "err", err)
		return 1
	}
	// The create's 202 is no more a receipt than the submit's. Agent-kind create
	// is always-async: it validates and spawns AFTER answering, so a create that
	// fails outright -- a taken alias, an unknown agent -- is still a 202.
	// Observed live 2026-08-01: a colliding alias answered 202 and then emitted
	// request.failed error_code=create_failed "session alias already exists",
	// while dispatch logged "triage session created" and carried on to submit a
	// prompt into a session it had not created.
	ready, err := awaitCreate(ctx, d, alias, created)
	if err != nil {
		d.Log.Error("create-session did not succeed", "agent", agent, "alias", alias, "err", err)
		return 1
	}
	if ready {
		// The success event means the session is COMMANDABLE, so the prompt
		// should land on the first submit rather than after the retry loop
		// spends the pod-start window on resolve_failed.
		d.Log.Debug("session is commandable; delivering prompt", "alias", alias)
	}

	if err := awaitPromptFetched(ctx, d, alias); err != nil {
		// An undelivered prompt is a real failure, not a warning: the session
		// exists but will idle forever and never be swept. Fail as infra so the
		// existing re-sling decides again and retries with a fresh
		// attempt-suffixed alias.
		d.Log.Error("prompt was never fetched; session will idle -- re-sling will retry",
			"agent", agent, "alias", alias, "bead", a.BeadAnchor, "err", err)
		// ...and TEAR IT DOWN, because "will idle forever" is not a figure of
		// speech. The bead never reaches StateRunning on this path, so gonk-sweep
		// will never see it and will never close it -- this is the ONLY place that
		// can. Worse, the re-sling this comment promises creates a NEW session
		// under the next attempt's alias, so without this the retry that is
		// supposed to recover leaks a pod per rung while producing nothing.
		// Exactly what gonk-pev's scaffold attempt did.
		abandonSession(ctx, d, agent, alias)
		return 1
	}

	base.State = beadstore.StateRunning
	base.SessionID = alias
	base.Rung, base.Model, base.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	base.ReservationExpiresAt = dec.ReservationExpiresAt
	if err := d.Store.Put(ctx, base); err != nil {
		d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
		// The record is what makes a session sweepable: no record, no bead in
		// StateRunning, so gonk-sweep will never look at this alias and never
		// close it. A live agent with no record is an ORPHAN -- it will run,
		// spend tokens against a reservation nobody will reconcile, and hold its
		// pod forever. Tear it down here or nothing ever will.
		abandonSession(ctx, d, agent, alias)
		return 1
	}
	d.Log.Info("broker session created", "agent", agent, "alias", alias,
		"bead", a.BeadAnchor, "rung", dec.Rung, "attempt", dec.Attempt)
	return 0
}

// abandonSession tears down a session dispatch created but is walking away from.
//
// It exists because of an asymmetry that is easy to miss: gonk-sweep can only
// close sessions belonging to a bead that reached StateRunning. Every failure
// between "the session exists" and "the record is stored" produces a session
// NO SWEEP WILL EVER SEE. Those are invisible leaks -- not merely a pod held too
// long, but a pod nothing in the system is even aware of.
//
// Failure to close is logged and swallowed: dispatch is already failing, and the
// caller's exit code must reflect the dispatch failure that brought us here, not
// the teardown. Loud, though -- see closeSession.
func abandonSession(ctx context.Context, d dispatchDeps, agent, alias string) {
	if err := d.GC.CloseSession(ctx, alias); err != nil {
		if gcapi.IsNotFound(err) {
			return
		}
		d.Log.Error("could not close the abandoned session -- ITS POD IS LEAKED",
			"agent", agent, "alias", alias, "err", err)
		return
	}
	d.Log.Info("abandoned session closed", "agent", agent, "alias", alias)
}

// awaitCreate reads the create's terminal event off the city log. It returns
// ready=true once the session is confirmed COMMANDABLE, and an error only when
// the create is confirmed to have FAILED.
//
// The asymmetry is deliberate and comes from upstream's own control flow
// (GASCITY_REF internal/api/huma_handlers_sessions_command.go, verified):
//
//   - A create FAILURE (taken alias, bad config) is emitted immediately, before
//     any waiting -- it is plain validation. Observed live: sub-second.
//   - A create SUCCESS is emitted only after WaitForSessionCommandable, which
//     blocks up to 120s for the pod to start and its tmux to come up. Observed
//     live: NOT emitted within 45s.
//
// So a timeout here is NOT a failure and must not be treated as one -- it means
// the pod is still starting, which is the normal case. Delivery simply falls
// through to awaitPromptFetched's retry loop, built for exactly that window.
// Blocking on the success event instead would either exceed the order timeout or
// turn every slow-but-healthy pod start into a failed dispatch.
//
// A create failure is never retried: the alias is attempt-suffixed and
// deterministic, so every reason it could fail would fail identically on the
// next pass. The re-sling path decides again with a fresh attempt, which is the
// right level for that.
func awaitCreate(ctx context.Context, d dispatchDeps, alias string, ack *gcapi.CreateSessionResult) (bool, error) {
	if ack == nil || ack.RequestID == "" {
		return false, fmt.Errorf("create for session %q returned no request id: the outcome cannot be confirmed", alias)
	}
	timeout := d.CreateAwaitTimeout
	if timeout <= 0 {
		timeout = defaultCreateAwaitTimeout
	}
	outcome, err := d.GC.AwaitRequestOutcome(ctx, ack.RequestID, ack.EventCursor, gcapi.EventSessionCreateResult, timeout)
	if err != nil {
		// Unobserved within the window: the pod is still starting. Say so at
		// debug and let the delivery loop do the waiting.
		d.Log.Debug("create outcome not yet reported; the pod is still starting",
			"alias", alias, "err", err)
		return false, nil
	}
	if !outcome.OK {
		return false, fmt.Errorf("create of session %q %s", alias, outcome)
	}
	return true, nil
}

// awaitPromptFetched waits for the agent pod to TAKE its prompt (gonk-mzd).
//
// This replaced submit-and-correlate-an-event as the evidence that the agent got
// its work. The difference is what is being believed: the old path believed its
// own submit, and reported success even when the prompt landed on a splash
// screen that was not listening -- the pod carried GC_STARTUP_PROMPT_DELIVERED=1
// in exactly the runs where the composer stayed empty. fetched_at is the pod's
// own acknowledgement, recorded by the store when it consumed the row.
//
// It is NOT terminal success. It proves the entrypoint fetched, not that
// opencode accepted the prompt or that a model was reached, so dispatch keeps
// its session-health checks around it rather than treating this as proof of
// work in progress.
func awaitPromptFetched(ctx context.Context, d dispatchDeps, alias string) error {
	attempts, backoff := d.SubmitAttempts, d.SubmitBackoff
	if attempts <= 0 {
		attempts = defaultSubmitAttempts
	}
	if backoff == nil {
		backoff = defaultSubmitBackoff
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
		fetched, err := d.Meter.PromptFetched(ctx, alias)
		if err != nil {
			// The meter being briefly unreachable is not evidence the pod
			// failed; keep waiting within the window rather than tearing down a
			// session that may be about to fetch.
			lastErr = err
			continue
		}
		if fetched {
			d.Log.Info("prompt fetched by the agent pod", "alias", alias)
			return nil
		}
		lastErr = fmt.Errorf("prompt not yet fetched")
	}
	// SAY WHICH FAILURE THIS WAS (gonk-alw). "Prompt was never fetched" reads as
	// an AGENT fault, but the same message covers a case where no agent ever
	// existed: on issue !44 a controller that had been up 42 seconds accepted
	// POST /sessions with 202, created no pod, logged nothing, and the only
	// symptom was this timeout two minutes later. Those have completely
	// different causes and only one of them is the agent's fault, so ask Gas
	// City what it thinks the session is before blaming the pod.
	return fmt.Errorf("prompt was never fetched by the agent after %d checks (%s): %w",
		attempts, describeSessionRuntime(ctx, d, alias), lastErr)
}

// describeSessionRuntime reports what Gas City believes about a session, for
// the diagnosis above. Best effort by construction: it runs on a path that has
// ALREADY failed, so it must never mask the original error with one of its own.
func describeSessionRuntime(ctx context.Context, d dispatchDeps, alias string) string {
	if d.GC == nil {
		return "session runtime unknown"
	}
	view, err := d.GC.GetSessionOutput(ctx, alias, 1)
	if err != nil {
		return "session runtime unknown: " + err.Error()
	}
	if view == nil {
		return "session runtime unknown: no view"
	}
	if !view.Running && strings.TrimSpace(view.LastOutput) == "" {
		// Accepted, never ran, produced nothing: the controller-side failure,
		// not the agent's.
		return fmt.Sprintf("NO AGENT EVER RAN -- session state %q, not running, no output; "+
			"the session was accepted but nothing started it", view.State)
	}
	return fmt.Sprintf("session state %q running=%v -- a runtime existed, so the pod started and did not fetch",
		view.State, view.Running)
}
