package main

import (
	"context"
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

// agentForTrigger maps a trigger to the broker AGENT that handles it. A trigger
// in this set is dispatched via the v2 broker: dispatch creates the agent
// session DIRECTLY (POST /v0/city/{city}/sessions), correlates it by a unique
// alias, and injects the rendered prompt as the session's initial message --
// instead of pouring a formula order. The agent produces a proposed-effects
// batch and posts nothing; gonk-sweep validates the batch's shape and applies
// it under the controller's own bot PAT. The pod holds no forge creds.
//
// Triage was the first ported trigger; scaffold followed (see below). Only
// mention still pours its formula in runDispatch until it is ported (Phase 6).
// An entry here takes precedence over orderForTrigger.
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
// It is deterministic and re-sling-stable EXCEPT for the attempt suffix, which
// is deliberate: gascity rejects a create whose alias is already taken, so a
// re-sling (same project+issue, next attempt) must get a fresh alias rather than
// collide with the prior attempt's still-present session. session.ValidateAlias
// forbids colons (bead anchors use them) and caps length at 64; this form uses
// only [a-z0-9.] and stays well under the cap.
func brokerSessionAlias(agent string, projectID, issueIID int64, attempt int) string {
	// Scaffold is PROJECT-scoped: it is dispatched with no issue, so an `.i0`
	// segment would be a lie about what the session is for. Two agents working
	// the same project must not collide, hence the agent in the alias.
	if issueIID == 0 {
		return fmt.Sprintf("gonk.%s.p%d.a%d", agent, projectID, attempt)
	}
	return fmt.Sprintf("gonk.%s.p%d.i%d.a%d", agent, projectID, issueIID, attempt)
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
func renderTriagePrompt(project string, issueIID int64, issueContext string) string {
	context := issueContext
	if strings.TrimSpace(context) == "" {
		context = "(issue context unavailable -- triage from the issue reference alone)"
	}
	return fmt.Sprintf(`Triage GitLab issue #%d in project `+"`%s`"+`. Here is the issue, already
fetched for you -- do NOT fetch anything yourself:

%s

Decide the labels (each prefixed `+"`gonk::`"+`) and one short triage comment: a
brief analysis of what the issue asks for, with anything genuinely ambiguous
phrased as a direct question to the reporter.

Do NOT post anything yourself. Do NOT run glab, git, bd, or any external API --
you hold no credentials and any such call will fail. Instead, emit your decision
as a single proposed-effects batch as the LAST thing in your output, fenced
EXACTLY like this:

GONK_BATCH_START
{"effects":[{"kind":"comment","body":"<your comment>"},{"kind":"label","add":["gonk::<label>"]}]}
GONK_BATCH_END

Emit exactly one comment effect and zero or more label effects. Nothing after
GONK_BATCH_END.`, issueIID, project, context)
}

// renderScaffoldPrompt builds the scaffold session's initial message.
//
// IT IS A DIFFERENT SHAPE OF JOB from the v1 formula prompt it replaces
// (pack/agents/scaffold/prompt.template.md), and the difference is the whole
// point of the port: that prompt told the agent to create a branch and open a
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

// sanitizeForKeystrokeDelivery makes a prompt safe to TYPE into opencode's TUI.
//
// The prompt is delivered by the supervisor as tmux `send-keys -l`, i.e. as
// KEYSTROKES into a running terminal UI -- and opencode's composer treats "!"
// as its shell-mode trigger. PROVEN LIVE 2026-08-01: sending
//
//	Hello there, see issue !42 and reply with exactly the word GOLF
//
// rendered as "$ Hello there, see issue 42 ..." (bang eaten, shell prompt shown)
// and produced "/bin/sh: 1: Hello: not found". The model never saw the message
// AT ALL. Every wedged triage session was this: the whole prompt executed as a
// shell command instead of being asked.
//
// THIS IS ALSO AN INJECTION BOUNDARY, which is why it is a hard strip rather
// than a tidy-up of gonk's own wording. The prompt embeds an UNTRUSTED GitLab
// issue title and body; a body containing a "!" followed by shell syntax would
// otherwise run in the agent pod. Sanitizing only the parts gonk writes would
// leave the half an attacker controls.
//
// Stripping is lossy -- exclamation marks and GitLab "!123" MR references do
// not survive -- and that is the right trade against executing issue text.
// The durable fix is to stop delivering prompts as keystrokes at all.
func sanitizeForKeystrokeDelivery(prompt string) string {
	return strings.ReplaceAll(prompt, "!", "")
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
// triage: NO, for now. The controller already injects the issue, which is what
// triage reasons about. Reading .agent/ would make it better and is the obvious
// next shape to flip, but flipping it changes every triage prompt, so it is a
// deliberate follow-up rather than a side effect of landing the mechanism.
var needsCheckout = map[string]bool{
	"scaffold": true,
	"triage":   false,
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
	// The meter's attribution metadata is deliberately NOT put in the prompt any
	// more (see renderTriagePrompt): it rode a marker line the pod cannot read on
	// this delivery path. It is still marshalled and logged so the value that
	// SHOULD be reaching the pod is visible at the point it is lost, rather than
	// quietly disappearing from the code.
	if md, err := json.Marshal(dec.Metadata); err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
	} else {
		d.Log.Debug("attribution metadata is not deliverable to the pod on the submit path",
			"bead", a.BeadAnchor, "metadata", string(md))
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
		prompt = renderTriagePrompt(a.Project, a.IssueIID, issueContext)
	}
	prompt = sanitizeForKeystrokeDelivery(prompt)

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

	if err := deliverPrompt(ctx, d, alias, prompt); err != nil {
		// An undelivered prompt is a real failure, not a warning: the session
		// exists but will idle forever and never be swept. Fail as infra so the
		// existing re-sling decides again and retries with a fresh
		// attempt-suffixed alias.
		d.Log.Error("prompt delivery failed; session will idle -- re-sling will retry",
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
// through to deliverPrompt's retry loop, which is built for exactly that window.
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

// sessionIsRunning reports whether the session's runtime is live yet. A 404 is
// "not there yet" -- agent create is async, so the alias legitimately does not
// resolve for the first seconds -- not an error.
func sessionIsRunning(ctx context.Context, d dispatchDeps, alias string) (bool, error) {
	// peekLines=1: this is a state check, not a read of the agent's output, and
	// the whole transcript would be pulled otherwise.
	view, err := d.GC.GetSessionOutput(ctx, alias, 1)
	if err != nil {
		if gcapi.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return view.Running, nil
}

// deliverPrompt submits the rendered prompt to the freshly-created session and
// then CONFIRMS, from the city event log, that it was actually delivered.
//
// THE 202 IS NOT A RECEIPT. POST .../session/{id}/submit resolves the session
// and delivers the message in a goroutine AFTER answering, so a submit against
// a session that does not exist yet is answered 202 exactly like one that
// lands, and the real outcome exists only as a terminal event keyed by the
// request id (gcapi.AwaitRequestOutcome). An earlier version of this function
// retried on 404 -- a status this route never returns -- so the first submit
// always "succeeded", dispatch recorded StateRunning, and the prompt was
// dropped whenever the async create had not materialized the session yet. Every
// agent then sat at opencode's idle splash forever. That was gonk-u1p.7; do not
// reintroduce a success path that does not read the outcome.
//
// The retry is not defensive padding: agent-kind create is ALWAYS-async
// upstream (202 with no session id), so the session genuinely does not exist
// for the first attempts -- a live run took ~31s from create to session start.
// Only a RETRYABLE outcome is retried (the session is not there / not live
// yet); a hard rejection is terminal, because retrying it just burns the
// order's timeout budget.
//
// The bound must stay comfortably inside gonk-dispatch's own 120s order timeout
// (pack/orders/gonk-dispatch.toml) -- overshooting it turns a recoverable
// delivery failure into a killed order with no bead update.
func deliverPrompt(ctx context.Context, d dispatchDeps, alias, prompt string) error {
	attempts, backoff := d.SubmitAttempts, d.SubmitBackoff
	if attempts <= 0 {
		attempts = defaultSubmitAttempts
	}
	if backoff == nil {
		backoff = defaultSubmitBackoff
	}
	awaitTimeout := d.SubmitAwaitTimeout
	if awaitTimeout <= 0 {
		awaitTimeout = defaultSubmitAwaitTimeout
	}
	budget := d.SubmitDeadline
	if budget <= 0 {
		budget = defaultSubmitDeadline
	}
	// The budget bounds the CONTEXT, not just the loop arithmetic, so it caps the
	// in-flight HTTP calls too. Without this a single slow round-trip started
	// just inside the deadline could run the whole delivery well past it -- a
	// live run overshot to 3m14s against a 90s budget exactly that way, which
	// would be a killed order rather than a legible failure.
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	deadline := time.Now().Add(budget)

	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(i)):
			}
		}
		// The hard cap on the whole loop. Checked before spending an attempt so
		// the budget bounds real work, not just the sleeps between it.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// DO NOT SUBMIT INTO A SESSION THAT IS NOT RUNNING YET.
		//
		// Manager.submit parks a default-intent message on the nudge queue --
		// outcome.Queued -- when the session is still start_pending/creating
		// (GASCITY_REF internal/session/submit.go). Delivery then depends on a
		// separate poller process, and PROVEN LIVE 2026-08-01 it never arrived:
		// session go-57b took a queued prompt, its pod came up healthy, and
		// opencode still sat at the idle splash minutes later. A queued prompt is
		// a dropped prompt on this backend.
		//
		// Waiting for running=true avoids the queue entirely rather than trying
		// to recover from it, which also sidesteps the one thing a retry cannot
		// undo: a parked copy landing later and prompting the agent twice.
		running, err := sessionIsRunning(ctx, d, alias)
		if err != nil {
			// A state check that could not be answered is not a verdict. The
			// supervisor is single-threaded behind a session mutation lock and a
			// live run saw this GET exceed its client timeout while another
			// session was mid-turn -- failing the dispatch on that would throw
			// away a perfectly good session over a slow read. Retry within the
			// budget instead; if it never answers, the loop exhausts and fails.
			last = err
			d.Log.Debug("could not read session state; will retry", "alias", alias, "attempt", i+1, "err", err)
			continue
		}
		if !running {
			last = fmt.Errorf("session is not running yet")
			d.Log.Debug("session not running yet; holding the prompt back", "alias", alias, "attempt", i+1)
			continue
		}

		ack, err := d.GC.SubmitSession(ctx, alias, prompt, gcapi.SubmitIntentDefault)
		if err != nil {
			// A transport-level rejection really is synchronous (no grant,
			// wrong city, unrouted path) and never reaches the event log.
			return err
		}
		if ack == nil || ack.RequestID == "" {
			// No correlation handle means no way to confirm delivery, and an
			// unconfirmable prompt is exactly the failure being fixed here.
			return fmt.Errorf("submit for session %q returned no request id: delivery cannot be confirmed", alias)
		}

		// NOTE the await is NOT retried on timeout, and the prompt is NOT
		// resubmitted: a timeout means the outcome is unobserved, not that it
		// failed, and resubmitting would risk delivering the prompt twice while
		// still not knowing. An unknown outcome fails the dispatch, loudly.
		outcome, err := d.GC.AwaitRequestOutcome(ctx, ack.RequestID, ack.EventCursor, gcapi.EventSessionSubmitResult, min(awaitTimeout, remaining))
		if err != nil {
			return fmt.Errorf("confirming prompt delivery to session %q: %w", alias, err)
		}
		if outcome.OK && !outcome.Queued {
			return nil // typed into the live runtime, and observed to be
		}
		if outcome.OK {
			// Accepted but PARKED, not delivered -- see the running-check above
			// for why that is a dropped prompt here. The running gate should
			// make this unreachable, so reaching it is worth a warning, not a
			// silent retry.
			last = fmt.Errorf("prompt was queued rather than delivered live")
			d.Log.Warn("prompt accepted but QUEUED, not delivered live -- retrying",
				"alias", alias, "session", outcome.SessionID)
			continue
		}
		last = fmt.Errorf("%s", outcome.String())
		if !outcome.Retryable() {
			return fmt.Errorf("prompt delivery to session %q rejected: %w", alias, last)
		}
		d.Log.Debug("prompt not delivered yet; session still materializing",
			"alias", alias, "attempt", i+1, "outcome", outcome.String())
	}
	return fmt.Errorf("session %q never accepted its prompt after %d attempts: %w", alias, attempts, last)
}
