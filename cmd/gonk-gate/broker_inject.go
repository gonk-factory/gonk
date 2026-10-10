package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/pipelinectx"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
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
// dispatch starts the agent session DIRECTLY through the SessionRuntime
// (gonk-t55), under a deterministic name recorded on the bead, with the
// rendered prompt mounted into it. The agent produces a
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
	// gonk-tios: a failed pipeline gets a grounded diagnosis. The agent reads a
	// checkout pinned to the pipeline's commit and proposes ONE typed
	// `diagnosis` effect; where it lands is the controller's choice (D9).
	"pipeline-diagnosis": pipelineAgent,
	// gonk-2usw: a diagnosed failure gets a proposed fix. The agent reads a
	// checkout pinned to the fix base commit and proposes `edit` effects; the
	// controller gates them, commits them and opens the merge request
	// (broker_pipefix_apply.go). runDispatch additionally requires a preparer
	// (dispatchDeps.PrepareFix) before it dispatches this trigger.
	"pipeline-fix": pipefixAgent,
}

// routableTriggers lists, in a stable order, the triggers agentForTrigger will
// actually dispatch. It exists so the refusal in runDispatch can say what the
// alternative WAS, rather than leaving a reader to go find this map.
func routableTriggers() []string {
	out := make([]string, 0, len(agentForTrigger))
	for k := range agentForTrigger {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// searchIncompleteMarker is the literal token a triage comment must OPEN with
// when the agent could not finish its search. It exists to split two outcomes
// that today collapse into the same reply-only comment and read identically to
// a human and to the sweep:
//
//	"I looked everywhere I said I looked and found nothing relevant"  -- an ANSWER
//	"I ran out of road before I finished looking"                     -- a NON-ANSWER
//
// The verdict vocabulary is closed (reply-only / code-change / close, see
// pkg/effects) and is NOT widened here: widening it would break every consumer
// of the label vocabulary at once. The distinction is carried in the comment
// instead, as a fixed leading token, so it is greppable by a human on the issue
// and available to the outcome classifier when gonk-2xev teaches the sweep to
// escalate a non-answer onto the next rung. Nothing in this package reads it
// yet, deliberately -- the classifier is that bead's, not this one's.
const searchIncompleteMarker = "SEARCH INCOMPLETE:"

// searchIncompleteLabel is proposed by the AGENT alongside the marker, so the
// same fact is visible on the board and not only in prose. It is deliberately
// NOT in ReservedLabels (broker_label.go): a reserved label proposed by an agent
// refuses the WHOLE batch, so reserving this one would turn an honest "I could
// not finish" into a dropped run -- the exact inversion of what it is for.
// broker_inject_test.go asserts that against ReservedLabels itself rather than
// restating the list.
const searchIncompleteLabel = "gonk::search-incomplete"

// searchProcedure is the anti-FALSE-ABSENCE half of the triage prompt, and it is
// spliced in ONLY when a checkout was actually granted -- there is nothing to
// enumerate otherwise, and telling a model to list a tree that is not there is
// the gonk-msz failure verbatim.
//
// WHY IT IS A PROCEDURE AND NOT ADVICE. The previous text already said "LOOK AT
// WHAT IS ACTUALLY IN THE TREE BEFORE YOU SEARCH FOR IT", added after issue !49.
// It did not hold. On 2026-09-12, project 75 -- a six-file repository whose
// internal/paging/sort.go ignores the desc flag in its SortByDate case, and says
// so in its own doc comment -- was triaged twice from the same prompt at the same
// rung with opposite results: !71 took nine model calls and named the file, the
// case, the comparison and the fix; !70 took six, and answered "I cannot find any
// relevant code files in the repository". All six files were present in
// /workspace on the !70 run, verified by exec'ing the pod before it was reaped,
// and five tool round-trips had actually executed. The model looked, stopped, and
// reported ABSENCE rather than UNCERTAINTY. A sentence of advice cannot stop
// that; a precondition on the negative conclusion can, because it makes the
// unsupported negative impossible to write without visibly omitting the evidence
// it is required to carry.
//
// WHAT IT DOES NOT DO -- and this is the property to defend on every future edit.
// Every clause here is pressure to LOOK, never pressure to FIND. A prompt that
// leans on a small model to produce a finding will get one invented, and a
// confident wrong bug report costs the reporter far more than a question does. So
// the procedure is paired with an explicit statement that finding no defect is a
// correct outcome, and the code-change verdict in the body carries a citation
// precondition (name the file and the symbol): the checklist raises the cost of
// giving up, and the citation raises the cost of making something up. Neither
// works without the other. The narrative balance is part of that: the two war
// stories in the text both end with the code having been there, so a small model
// pattern-matching on story shape would see two examples of "the last run's
// mistake was not finding it" and none of the opposite. The mirror-image failure
// is therefore spelled out too, and labelled as having no incident behind it
// yet -- inventing an anecdote to balance the tone would be the same sin the
// paragraph is warning about.
//
// THE QUICK-ACTION INTERLOCK. Requiring the comment to NAME the directories it
// searched created a new way to lose the whole run, and it had to be closed in
// the same breath: pkg/effects.ValidateComments REFUSES an entire batch if any
// comment line begins with a slash and a letter, because GitLab would read it as
// a quick action (/close, /assign). The obvious way to answer "which directories
// did you search" is a list of absolute paths, one per line -- which is exactly
// that shape. So the procedure says to write paths without a leading slash and
// never to start a line with one. Without that clause this change would have
// converted the most honest possible report into a silently dropped run, which
// is the same inversion searchIncompleteLabel is kept out of ReservedLabels to
// avoid. broker_inject_absence_test.go proves the hazard is real by running
// effects.ValidateComments against the shape, rather than only asserting the
// warning exists -- so if pkg/effects ever stops refusing, the stale warning is
// caught instead of quietly outliving its reason.
//
// ON LARGE REPOSITORIES. "Enumerate the tree" is NOT written as an unconditional
// instruction, because on a monorepo it is either impossible or it spends the
// entire context window on ls output. Step 2 is bounded and breadth-first on
// purpose. The load-bearing requirement is not exhaustiveness but
// ACCOUNTABILITY: a negative conclusion must state which directories were listed
// and which terms were searched. That costs the same on six files as on sixty
// thousand, and it degrades honestly with scale -- on a huge repository the
// comment becomes "I searched these four directories for these terms and did not
// find it", which is true and useful, instead of "there is no relevant code",
// which is false.
const searchProcedure = "SEARCH PROCEDURE. Do these IN ORDER, and do not stop early because a guess\n" +
	"missed:\n" +
	"  1. List the top level of your working directory. Read what is actually\n" +
	"     there. Do not guess at languages, file extensions or layout.\n" +
	"  2. List the subdirectories that could plausibly hold the behaviour the\n" +
	"     issue describes. Go breadth-first; on a large repository you are not\n" +
	"     required to enumerate every file, but you ARE required to know, and to\n" +
	"     be able to say, which directories you looked in.\n" +
	"  3. Search the tree for the issue's OWN words -- the identifiers, function\n" +
	"     names, flags, error messages and nouns the reporter used, plus their\n" +
	"     obvious variants. A search that returns nothing is evidence that your\n" +
	"     guess was wrong. It is NOT evidence that the code is absent.\n" +
	"  4. Open and READ the most likely file, end to end. Do not judge from a\n" +
	"     grep hit or a file name alone.\n" +
	"  5. Only now decide.\n\n" +
	"YOU MAY NOT REPORT THAT THE RELEVANT CODE DOES NOT EXIST UNLESS YOU\n" +
	"COMPLETED STEPS 1 TO 4, AND YOUR COMMENT NAMES THE DIRECTORIES YOU LISTED\n" +
	"AND THE TERMS YOU SEARCHED FOR. A negative conclusion without that evidence\n" +
	"is not an answer. On issue !49 a run globbed five times for the wrong\n" +
	"languages, concluded no relevant code existed, and asked the reporter for\n" +
	"help while the answer sat in a file it never listed. On issue !70 a run\n" +
	"stopped after five tool calls and said \"I cannot find any relevant code\n" +
	"files in the repository\" -- the file was there, and another run on the SAME\n" +
	"repository found it. \"I could not find it\" is a statement about your\n" +
	"search, not about the repository, and it must be written that way.\n\n" +
	"IF YOU COULD NOT FINISH THE SEARCH, SAY THAT INSTEAD -- IT IS A DIFFERENT\n" +
	"ANSWER, NOT A WORSE ONE. Running out of turns, a tool that kept failing, or\n" +
	"a tree too large to cover are NOT the same as having looked and found\n" +
	"nothing, and must not be reported as if they were. In that case: begin your\n" +
	"comment with the exact text \"" + searchIncompleteMarker + "\", add the label\n" +
	searchIncompleteLabel + ", say what you did get through and where you stopped,\n" +
	"and do NOT state that the code does not exist. The verdict for that is\n" +
	"reply-only -- never close, which would end a conversation you did not\n" +
	"finish having.\n\n" +
	"WRITE PATHS WITHOUT A LEADING SLASH, and NEVER begin a line of your comment\n" +
	"with a slash. Write internal/paging/sort.go, not /internal/paging/sort.go.\n" +
	"GitLab reads a line that starts with a slash and a letter as a QUICK ACTION,\n" +
	"and gonk refuses the entire batch rather than post one -- so a tidy list of\n" +
	"the directories you searched, one absolute path per line, would cost you the\n" +
	"whole run and leave the reporter with nothing. Name them inline instead.\n\n" +
	"FINDING NO DEFECT IS A CORRECT OUTCOME. This procedure is a requirement\n" +
	"about how hard you LOOK, not about what you must CONCLUDE. Having done it,\n" +
	"\"I read these files and this behaves as designed, and here is why\" is a\n" +
	"complete and welcome answer, and so is an honest question. Never assert a\n" +
	"fault you have not actually read in the code.\n\n" +
	"THE MIRROR-IMAGE FAILURE IS WORSE, and it has no war story above only\n" +
	"because nobody has caught one yet: a run that cannot find the code, guesses\n" +
	"at a plausible file and function, and reports a defect that does not exist.\n" +
	"That comment is confidently wrong, a human has to go and disprove it, and it\n" +
	"misleads every later session that reads the issue. Searching harder is the\n" +
	"point of the procedure above; inventing a finding is not a way to satisfy\n" +
	"it. If you are guessing, say you are guessing.\n\n" +
	"INVESTIGATE THE CODE BEFORE YOU ASK ANYTHING. Ask the reporter only for what\n" +
	"the code CANNOT tell you -- their intent, their environment, exact\n" +
	"reproduction steps, which behaviour they expected. Anything answerable by\n" +
	"reading this repository you are expected to answer yourself, citing the\n" +
	"files you relied on.\n"

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
func renderTriagePrompt(project string, issueIID int64, agentContext, issueContext, checkout, checkoutSHA string) string {
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
		// The commit is named so the agent (and the transcript) can say exactly
		// which tree it read: the checkout is pinned to it (gonk-qdr4), and a
		// later push is NOT in it.
		at := ""
		if strings.TrimSpace(checkoutSHA) != "" {
			at = "It is the tree at commit " + checkoutSHA + ", the head of the default " +
				"branch when this session was dispatched; anything pushed since is not in it. " +
				"Cite that commit if you refer to a version of the code.\n\n"
		}
		repo = "\nThe repository is checked out in your working directory. " + at + "Read `.agent/` " +
			"first if it exists -- it is the project's own context and it overrides " +
			"anything you would otherwise assume. You still hold no credentials, so do " +
			"not try to reach GitLab.\n\n" +
			searchProcedure
	}
	return fmt.Sprintf(`%sTriage GitLab issue #%d in project `+"`%s`"+`. Here is the issue, already
fetched for you -- do NOT fetch anything yourself:

%s
%s
Decide the labels (each prefixed `+"`gonk::`"+`), one triage comment, and a
VERDICT saying what kind of answer this is.

The comment is a brief analysis of what the issue asks for, grounded in the code
where you could find it. Ask the reporter only what the code cannot tell you.

The verdict must be EXACTLY ONE of:

  "reply-only"   No code change is needed, but the reporter needs an answer --
                 a question, a clarification, or "this works as designed, and
                 here is why".
  "code-change"  A genuine defect you have SEEN in the code, with an
                 identifiable fix. NAME THE FILE AND THE FUNCTION OR SYMBOL
                 whose behaviour is wrong, and say what should change. If you
                 cannot point at the code, it is NOT a code-change: choose
                 reply-only and say what you suspect. Do not write the fix
                 here; that is a separate step.
  "close"        Terminal. No further discussion is useful -- a duplicate, an
                 obsolete report, something already fixed, or something you
                 established is not reproducible. The comment MUST say why.

Choose "close" only when you are confident, because it ENDS THE CONVERSATION.
When you are unsure between close and reply-only, choose reply-only and ask.

DO NOT MANUFACTURE A DEFECT. Concluding that there is no defect is a correct
outcome, not a failed run, and so is asking an honest question. A wrong bug
report costs the reporter more than a question does.

Do NOT post anything yourself. Do NOT run glab, git, bd, or any external API --
you hold no credentials and any such call will fail. Instead, emit your decision
as a single proposed-effects batch as the LAST thing in your output, fenced
EXACTLY like this:

GONK_BATCH_START
{"verdict":"<one of: reply-only, code-change, close>","effects":[{"kind":"comment","body":"<your comment>"},{"kind":"label","add":["gonk::<label>"]}]}
GONK_BATCH_END

Emit exactly one comment effect and zero or more label effects. Nothing after
GONK_BATCH_END.

The batch must be valid JSON on a SINGLE line. Keep the comment brief -- but
never at the cost of the evidence this prompt requires of you -- and if you
must include a line break write it as \n inside the string: a real line break
inside a JSON string is invalid and costs you the whole batch.`, agent, issueIID, project, context, repo)
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
// fetches it from and the commit SHA it is pinned to, or "", "" when this event
// shape does not need one.
//
// THE GRANT IS PINNED TO A COMMIT, NOT A BRANCH (gonk-qdr4). The default branch
// is resolved to its head SHA HERE, at the decision point, and the grant carries
// the SHA; intake fetches the archive by that SHA whenever the pod asks. A push
// landing between the grant and the pod's fetch therefore cannot change the tree
// the agent reads, which matters because every later claim -- the batch the agent
// proposes, a diff that must apply, a scope predicate -- is about THAT commit. A
// ref that cannot be resolved fails the grant (an error naming the ref and the
// forge's reason); the callers treat that as non-fatal and log it, so triage
// proceeds with no checkout, as it always did when a grant failed.
//
// Called at the DECISION POINT: the project and ref are known, the controller
// holds the PAT, and the pod holds neither. The grant binds one unguessable
// key to that one project at that one ref -- see pkg/rig for why registration
// is the authorization.
//
// THE GRANT KEY IS NOT THE SESSION NAME (gonk-t55). pkg/rig serves a granted
// tree to anyone who presents its key on gonk-intake's private listener, which
// every agent pod can reach -- "an unguessable alias is load-bearing, not
// cosmetic" (pkg/rig/http.go). The Gas City alias carried 128 bits of nonce and
// was the key. The session name is DETERMINISTIC (plan D2) -- gonk-triage-p42-
// i3-a1 is reconstructible from a project and an issue number -- so keying the
// grant by it would let one project's agent fetch another project's checkout.
// So the key is the name plus a fresh 128-bit nonce (rigGrantKey), and the
// only copy of it outside gonk-intake is the URL handed to the session.
//
// Every failure here is NON-FATAL by design. A session without a checkout still
// runs, on a prompt that says so; failing the dispatch instead would turn a
// degraded run into no run at all.
func grantCheckout(ctx context.Context, d dispatchDeps, agent, session string) (url, sha string, err error) {
	if !needsCheckout[agent] {
		return "", "", nil
	}
	if d.Rig == nil || d.RigBaseURL == "" {
		return "", "", nil // not configured; the fallback tiers cover it
	}
	if d.Forge == nil {
		return "", "", fmt.Errorf("no forge reader to resolve the ref")
	}
	proj, err := d.Forge.GetProject(ctx, d.Args.ProjectID)
	if err != nil {
		return "", "", fmt.Errorf("resolve default branch: %w", err)
	}
	ref := proj.DefaultBranch
	if ref == "" {
		ref = "main"
	}
	// PIN THE COMMIT at grant time. If the pod (or intake, later) resolved "the
	// default branch" for itself, a push landing mid-session would change what
	// it read, and the batch it proposes would describe a tree nobody can
	// reconstruct. ResolveRef asserts a full hex SHA came back.
	sha, err = d.Forge.ResolveRef(ctx, d.Args.ProjectID, ref)
	if err != nil {
		return "", "", fmt.Errorf("pin checkout: cannot resolve ref %q to a commit: %w", ref, err)
	}
	return registerCheckout(ctx, d, session, ref, sha)
}

// grantCheckoutAt is grantCheckout for a shape whose commit is already known:
// the pipeline agent's checkout is pinned to the FAILED PIPELINE'S commit
// (gonk-tios D6), never to the default branch head, which may have moved on
// since the pipeline ran. The SHA is asserted to be a full commit id. Same key,
// same non-fatal failure rule as grantCheckout.
func grantCheckoutAt(ctx context.Context, d dispatchDeps, agent, session, sha string) (url, pinned string, err error) {
	if !needsCheckout[agent] {
		return "", "", nil
	}
	if d.Rig == nil || d.RigBaseURL == "" {
		return "", "", nil
	}
	if !glab.IsCommitSHA(sha) {
		return "", "", fmt.Errorf("pin checkout: %q is not a full commit SHA", sha)
	}
	return registerCheckout(ctx, d, session, "", sha)
}

// registerCheckout registers the grant for one session at one commit. ref is
// only for the log line ("" when the commit was given, not resolved).
func registerCheckout(ctx context.Context, d dispatchDeps, session, ref, sha string) (url, pinned string, err error) {
	key, err := rigGrantKey(session)
	if err != nil {
		return "", "", err
	}
	if err := d.Rig.Grant(ctx, key, d.Args.Project, d.Args.ProjectID, sha); err != nil {
		return "", "", fmt.Errorf("register grant: %w", err)
	}
	// The session name only: the key is a capability, and this is a log line
	// operators are told to read.
	d.Log.Info("checkout granted for this session",
		"bead", d.Args.BeadAnchor, "session", session, "project", d.Args.Project, "ref", ref, "sha", sha)
	return rig.FetchURL(d.RigBaseURL, key), sha, nil
}

// rigGrantKey is the session name plus 128 bits of crypto/rand, base32. It
// only ever names a rig grant (see grantCheckout). The result is a valid
// rig.ValidAlias: a DNS-1123 name, a dot, and [A-Z2-7], well under 128 bytes.
func rigGrantKey(session string) (string, error) {
	var b [16]byte // 128 bits
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable key is a public read of the checkout: refuse rather
		// than degrade. grantCheckout's caller treats this as "no checkout".
		return "", fmt.Errorf("crypto/rand unavailable, refusing to mint a guessable checkout key: %w", err)
	}
	return session + "." + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
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
//
// pipeline: YES, at the PIPELINE'S commit (grantCheckoutAt), not the default
// branch head -- the diagnosis is about the tree that failed, and every file it
// cites is checked to exist at that commit.
//
// pipefix: YES, at the FIX BASE commit (gonk-2usw plan 5.2): the session reads
// the files it proposes edits to, and the edits are applied to that commit.
var needsCheckout = map[string]bool{
	"scaffold":    true,
	"triage":      true,
	pipelineAgent: true,
	pipefixAgent:  true,
}

// brokerForgeReader is what dispatchDeps.Forge must satisfy: both halves of the
// controller-side read. Kept as one field because a single *glab.Client backs
// both, and splitting it would let a caller wire triage's reader without
// scaffold's and only discover it at dispatch time.
type brokerForgeReader interface {
	issueReader
	repoReader
	refResolver
}

// refResolver turns a branch/tag name into the full commit SHA it names right
// now. The checkout grant is pinned to that SHA, never to the name (gonk-qdr4).
// Satisfied by *glab.Client.
type refResolver interface {
	ResolveRef(ctx context.Context, projectID int64, ref string) (string, error)
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

// runBrokerDispatch is the v2 broker's start step: render the prompt, start
// the agent session through the SessionRuntime under its deterministic name,
// and record that name on the bead as the correlation key sweep follows.
//
// Exit codes match runDispatch's contract: 0 acted, 1 infra, 2 misconfig.
//
// WHAT IS NOT HERE ANY MORE (gonk-t55). Under Gas City this function created a
// session (a 202 that promised nothing, gonk-alw), read the create's outcome
// off an event stream, stored the prompt on the meter for the pod to fetch by
// alias, and then waited up to 210s for the pod to prove it had fetched it
// (prompt_wait.go). Under the Jobs runtime none of that exists: Start returns
// once the API server has ADMITTED the session or refused it, and the prompt,
// the key and the attribution metadata are mounted into the pod rather than
// fetched. So "the session exists" is known at Start, and there is no delivery
// wait to budget.
func runBrokerDispatch(ctx context.Context, d dispatchDeps, agent string, dec meterapi.DecideResponse, base beadstore.Record) (code int) {
	a := d.Args
	// The order budget (order_budget.go): measured from PROCESS START against
	// the order timeout gonk-dispatch still runs under until T-57.
	clk := d.clock()
	budget := d.Budget.withDefaults()
	started := d.StartedAt
	if started.IsZero() {
		started = clk.Now()
	}
	// The meter's attribution metadata reaches the pod (gonk-m6t): the
	// entrypoint stamps it into the opencode overlay's spend-logs header
	// before the model is ever called. It is MOUNTED (runtime.Spec.Metadata).
	md, err := json.Marshal(dec.Metadata)
	if err != nil {
		d.Log.Error("could not marshal meter metadata", "err", err, "bead", a.BeadAnchor)
		return 1
	}
	// THE SESSION'S ENVIRONMENT COMES FROM THE PACK (plan D5), read BEFORE
	// anything is written: a pack that cannot be read is a misconfiguration
	// of this controller image, and no reservation should be taken for a
	// session that can never start with its permissions (gonk-blxu).
	packEnv, err := packAgentEnv(d.packDir(), agent)
	if err != nil {
		d.Log.Error("cannot build the session environment from the pack; dispatching nothing",
			"agent", agent, "bead", a.BeadAnchor, "err", err)
		return 2
	}
	// THE NAME IS THE IDEMPOTENCY KEY (plan D2). It is deterministic in
	// (agent, project, issue, attempt), so a second Start for the same bead and
	// attempt reaches the SAME session -- the API server answers AlreadyExists
	// and the runtime hands back the existing one -- rather than a second pod.
	// That is what the Gas City alias could not do: its uniqueness only
	// covered ACTIVE sessions, and a re-dispatch after the first ended minted
	// a duplicate that wedged every later read (gonk-u6p).
	//
	// It is no longer a capability. The 128-bit alias nonce existed because
	// the pod presented its alias to fetch its own prompt on the meter's one
	// unauthenticated route (gonk-mzd); with the prompt mounted, nothing
	// fetches anything by this name.
	name := runtime.Name(agent, a.ProjectID, runtime.KindIssue, a.IssueIID, dec.Attempt)
	if agent == pipelineAgent {
		// Named for the pipeline it diagnoses -- the RECORDED one, reused by
		// every attempt of the run (setPipelineIdentity) -- e.g.
		// gonk-pipeline-p84-pl3200-a1 (gonk-tios D6).
		name = runtime.Name(agent, a.ProjectID, runtime.KindPipeline, base.PipelineID, dec.Attempt)
	}
	if agent == pipefixAgent {
		// Named for the pipeline the fix was decided against (the fix record's,
		// never the order's), e.g. gonk-pipefix-p84-pl3200-a1. The agent name
		// keeps it apart from the diagnosis session of the same pipeline.
		name = runtime.Name(agent, a.ProjectID, runtime.KindPipeline, base.PipelineFix.BasePipelineID, dec.Attempt)
	}

	// IDEMPOTENCY ON THE RECORD SIDE (gonk-6n8), and the pending-prompt
	// reservation, are kept as belt-and-braces (plan D2): the reservation is
	// entangled with the sweep's reclaim pass and with the park/deny
	// compare-and-swap, and it still refuses a second dispatch for an attempt
	// that already has a session before that dispatch spends a Start or a
	// prompt render on it. The read lives in reservePendingPrompt, next to the
	// reservation write it guards, because the two are one compare-and-swap
	// (gonk-y4m7).
	//
	// The release RESTORES THE PRIOR RECORD (T-19), and only over this
	// dispatch's own reservation (gonk-y4m7) -- see releasePendingPrompt. A
	// released pending-prompt record left in that state was invisible to the
	// sweep; a parked bead downgraded to pending-prompt by a failed re-dispatch
	// stopped being unparked. Putting back exactly what was there fixes both.
	//
	// NOTHING STAMPS UpdatedAt HERE: the store owns the write time
	// (pkg/beadstore stampWriteTime), and runSweep reclaims a pending-prompt
	// record by how long it has sat there.
	// fixNeedsScope: the fix preparer found nothing editable. Registered
	// BEFORE the release defer below, so it runs AFTER it (LIFO): the record
	// is first put back as the sweep wrote it, then made terminal.
	fixNeedsScope := false
	defer func() {
		if fixNeedsScope && !terminated(ctx) {
			settleFixNeedsScope(ctx, d, base, dec)
		}
	}()
	pending := base
	pending.State = beadstore.StatePendingPrompt
	pending.SessionID = name
	pending.Rung, pending.Model, pending.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	pending.ReservationExpiresAt = dec.ReservationExpiresAt
	res, rcode, proceed := reservePendingPrompt(ctx, d, base, pending)
	if !proceed {
		return rcode
	}
	defer func() {
		if code == 0 {
			// The happy path below overwrote this record with StateRunning.
			// Nothing to release.
			return
		}
		if res.kind == reservationNone {
			return // nothing was written, so there is nothing of ours to undo
		}
		if terminated(ctx) {
			// NOT STARTING A WRITE WE CANNOT FINISH (gonk-alw). Gas City's
			// order-timeout SIGTERM gives the process group 2s before SIGKILL,
			// and a Put is up to five bd commands: cut off mid-way it leaves a
			// record no sweep List ever matches. The pending-prompt reservation
			// as written is a state gonk-sweep reclaims (reclaimPendingPrompt).
			d.Log.Error("dispatch terminated: NOT releasing the pending-prompt reservation; "+
				"gonk-sweep reclaims it after pendingPromptGrace",
				"bead", a.BeadAnchor, "session", name, "cause", context.Cause(ctx))
			return
		}
		// Capped ONLY at the hard stop, never at the write reserve: a timeout
		// on the write itself kills the bd child mid-Put (exec.CommandContext)
		// -- the very torn write the budget exists to prevent.
		rctx, cancel := context.WithTimeout(ctx, timeLeft(clk.Now(), budget.hardStop(started)))
		defer cancel()
		releasePendingPrompt(rctx, d, name, res)
	}()

	// SAY SO NOW (gonk-l900 item 1). The attempt is reserved -- the record
	// write this note reports has landed -- and everything between here and a
	// session the reporter could see takes seconds to minutes. On rom#1
	// (2026-10-04) the issue showed nothing at all for 5+ minutes across two
	// attempts. A reservation this dispatch could not confirm is reported
	// after the running record lands instead (below), never before a write.
	runNoted := res.kind == reservationHeld
	if runNoted {
		d.postRunningNotice(ctx, agent, dec, budget.startDeadline(started).Add(-statusNoteHeadroom))
	}

	// The prompt is per-agent. Scaffold is project-scoped and reads the repo
	// from its own rig, so it needs no issue fetched for it -- and asking the
	// forge for issue 0 would be a pointless round trip that logs a warning.
	var prompt, checkoutURL, checkoutSHA string
	switch agent {
	case pipelineAgent:
		// The failed pipeline's context, read controller-side (the pod holds
		// no forge credential), and a checkout at the pipeline's own commit.
		// NOT degraded-but-continue: without the pipeline and its jobs there is
		// nothing to diagnose and nothing to check an answer against, so no
		// session is started (exit 1; the reservation is released).
		pp, perr := preparePipelineDispatch(ctx, d, name, base)
		if perr != nil {
			d.Log.Error("cannot build the pipeline's context; starting no session",
				"bead", a.BeadAnchor, "pipeline_id", base.PipelineID, "err", perr)
			return 1
		}
		prompt, checkoutURL, checkoutSHA = pp.prompt, pp.checkoutURL, pp.checkoutSHA
		base = pp.apply(base)
	case pipefixAgent:
		// The fix session's context and checkout come through the seam to
		// part AGENT. NOT degraded-but-continue, as for the diagnosis: with
		// no context there is nothing to fix, so no session is started.
		if d.PrepareFix == nil {
			d.Log.Error("no fix session preparer is configured; starting no session", "bead", a.BeadAnchor)
			return 2
		}
		fs, perr := d.PrepareFix(ctx, d, pipefixPrepareInput{Session: name, Fix: base, Run: d.fixRun, Rung: dec.Rung})
		if errors.Is(perr, errPipefixNothingEditable) {
			// Needs-scope, decided at dispatch for zero tokens (plan 4.4 way
			// 1). Landed AFTER the reservation's release (the defer above runs
			// first): see the defer registered before it.
			d.Log.Warn("no cited file may be edited at the fix base commit; starting no session",
				"bead", a.BeadAnchor, "base_pipeline", base.PipelineFix.BasePipelineID, "err", perr)
			fixNeedsScope = true
			return 1
		}
		if perr != nil {
			d.Log.Error("cannot prepare the fix session; starting no session",
				"bead", a.BeadAnchor, "base_pipeline", base.PipelineFix.BasePipelineID, "err", perr)
			return 1
		}
		prompt, checkoutURL, checkoutSHA = fs.Prompt, fs.CheckoutURL, fs.CheckoutSHA
		base.PipelineFix.ContextSHA256 = fs.ContextSHA256
		// What the session was shown, for the apply's gate A9. Written in the
		// write that makes the record `running`, and never again.
		base.PipelineFix.Context = fs.Context
	case "scaffold":
		// Read the repository CONTROLLER-SIDE, for the same reason triage's
		// issue is read here: the pod holds no forge creds.
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
		if url, sha, gerr := grantCheckout(ctx, d, agent, name); gerr != nil {
			d.Log.Warn("could not grant a checkout; falling back to controller-side repository context",
				"err", gerr, "bead", a.BeadAnchor, "project", a.Project)
		} else if url != "" {
			prompt, checkoutURL, checkoutSHA = renderScaffoldCheckoutPrompt(a.Project, url), url, sha
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
		checkout, sha, cerr := grantCheckout(ctx, d, agent, name)
		if cerr != nil {
			d.Log.Warn("could not grant a checkout for triage; prompting without a working copy",
				"err", cerr, "bead", a.BeadAnchor, "project", a.Project)
		}
		checkoutURL, checkoutSHA = checkout, sha
		// The project's own `.agent/`, read controller-side for the same reason
		// the issue is: the pod holds no forge credentials. Best-effort and
		// silent when the directory is absent -- that is the common case and
		// not a problem, it just makes the prompt thinner.
		agentContext, aerr := buildAgentContext(ctx, d.Forge, a.ProjectID)
		if aerr != nil {
			d.Log.Warn("could not read the project's .agent/ context; prompting without it",
				"err", aerr, "bead", a.BeadAnchor, "project", a.Project)
		}
		prompt = renderTriagePrompt(a.Project, a.IssueIID, agentContext, issueContext, checkout, sha)
	}
	// The session's own project key (gonk-8gb), resolved FAIL-CLOSED: a
	// session that cannot be metered must not run, and the old fallback -- the
	// install-wide admin key -- is exactly what made that failure invisible.
	// It is MOUNTED into the session, never put in its environment.
	litellmKey, kerr := resolveLiteLLMKey(ctx, d.Keys, dec.KeyRef.SecretName, dec.KeyRef.SecretKey)
	if kerr != nil {
		d.Log.Error("refusing to start a session: no per-project LiteLLM key",
			"session", name, "bead", base.BeadAnchor, "err", kerr)
		return 1
	}

	// THE SESSION'S DEADLINE IS THE RESERVATION'S (plan D6): the runtime turns
	// it into activeDeadlineSeconds, so a session that is still running when
	// meter would expire its reservation is killed and reads Failed /
	// DeadlineExceeded -- which the sweep books infra-failed. A decision with
	// no deadline (meter's wire field is omitzero) gets the one the sweep will
	// judge it by anyway: runningDeadline's default TTL from now.
	deadline := dec.ReservationExpiresAt
	if deadline.IsZero() {
		deadline = clk.Now().Add(opercfg.DefaultReservationTTL)
		d.Log.Warn("meter's run decision carries no reservation deadline; bounding the session by the default reservation TTL",
			"bead", a.BeadAnchor, "session", name, "deadline", deadline)
	}
	spec := runtime.Spec{
		Name:  name,
		Agent: agent,
		// Image left empty: the runtime runs its configured agent image
		// (jobs.Config.Image), the one the chart renders beside Gas City's.
		Env: buildSessionEnv(packEnv, sessionEnvInputs{
			Attempt: dec.Attempt, Model: dec.Model,
			LiteLLMURL: d.LiteLLMURL, RigBaseURL: d.RigBaseURL,
		}),
		// The checkout URL is a capability (it embeds the rig grant's key):
		// MOUNTED as the session's rig-url file, never env (plan 3.3 item 4).
		RigURL:     checkoutURL,
		Prompt:     []byte(prompt),
		LiteLLMKey: []byte(litellmKey),
		Metadata:   md,
		Deadline:   deadline,
	}

	// DO NOT START A SESSION THERE IS NO TIME TO RECORD (gonk-alw). Start only
	// admits the session, but the running-record write -- and, if that fails,
	// the Stop that must follow it -- still have to fit inside the order
	// timeout. A dispatch whose pre-start work already ate that time would
	// start a pod only to leave it with no record.
	if left := budget.startDeadline(started).Sub(clk.Now()); left <= 0 {
		d.Log.Error("not starting a session: too little of the order timeout is left to record it",
			"agent", agent, "session", name, "bead", a.BeadAnchor, "late_by_s", int(-left.Seconds()))
		return 1
	}
	h, created, err := d.Runtime.Start(ctx, spec)
	if err != nil {
		// NEVER SILENT (gonk-alw's surviving requirement). A refused start is a
		// dispatch failure: exit 1, the deferred release puts the record back,
		// and the re-sling decides again.
		d.Log.Error("session start failed; dispatching nothing",
			"agent", agent, "session", name, "bead", a.BeadAnchor, "err", err)
		return 1
	}

	if terminated(ctx) {
		// Admitted, but told to stop before the StateRunning write: do not start
		// a write that cannot finish. The pending-prompt record already carries
		// this session's name, so the sweep's reclaim promotes it to running and
		// judges the session.
		d.Log.Error("dispatch terminated after the session started: NOT writing the running record; "+
			"gonk-sweep reclaims the reservation", "agent", agent, "session", name, "bead", a.BeadAnchor,
			"cause", context.Cause(ctx))
		return 1
	}
	base.State = beadstore.StateRunning
	base.SessionID = string(h)
	base.CheckoutSHA = checkoutSHA
	base.Rung, base.Model, base.ReservationID = dec.Rung, dec.Model, dec.ReservationID
	base.ReservationExpiresAt = dec.ReservationExpiresAt
	// Capped ONLY where a Stop must still fit before the hard stop, never at
	// the write reserve.
	//
	// UNCONDITIONAL, DELIBERATELY, unlike the reservation and the release
	// (gonk-y4m7). The session is ADMITTED: a running record that does not land
	// leaves a live agent no sweep will ever judge. Everything that could
	// legitimately have replaced this dispatch's reservation is already
	// excluded upstream of here -- a dispatch for the same attempt sees the
	// reservation and backs off (or loses its compare-and-swap), every sweep
	// write is conditional on a revision read before the reservation, the
	// reclaim waits out pendingPromptGrace, and meter hands out no other
	// attempt, defer or deny while this attempt's reservation is open -- and a
	// defer or deny DECIDED before the reservation is a PutIf on a revision read
	// before that decision, so it cannot land over it either (gonk-b2jk).
	pctx, pcancel := context.WithTimeout(ctx, timeLeft(clk.Now(), budget.hardStop(started).Add(-budget.Close)))
	err = d.Store.Put(pctx, base)
	pcancel()
	if err != nil {
		d.Log.Error("bead store Put failed", "err", err, "bead", a.BeadAnchor)
		// The record is what makes a session sweepable: no record, no bead in
		// StateRunning, so gonk-sweep will never look at this session and never
		// stop it. Stop it here. (The Job would still die at its deadline and be
		// removed by its TTL, and the sweep's stale-session pass would catch it
		// after that -- but that is an hour of a pod spending tokens nobody
		// will read.)
		//
		// ONLY IF THIS DISPATCH CREATED IT (plan 3.4 C2). The name is
		// deterministic, so Start can hand back a session ANOTHER dispatch
		// created for the same bead and attempt -- reachable when two
		// dispatches both get past reservePendingPrompt, which its unreadable-
		// store path deliberately allows (gonk-u6p). That session is the other
		// dispatch's: its record names it, and stopping it here would make the
		// sweep find its recorded session absent and CHARGE the attempt. So an
		// adopted session is left to its owner. If the owner failed too, its
		// own abandon stops it (it created it), or, failing that, the Job's
		// deadline and TTL and the sweep's stale-session pass do.
		if !created {
			d.Log.Warn("not stopping the session after the failed record write: this dispatch ADOPTED it -- "+
				"another dispatch created it and owns it", "agent", agent, "session", name, "bead", a.BeadAnchor)
			return 1
		}
		abandonSession(ctx, d, agent, h, phaseTimeout(clk.Now(), budget.hardStop(started), budget.Close))
		return 1
	}
	d.Log.Info("broker session started", "agent", agent, "session", name,
		"bead", a.BeadAnchor, "rung", dec.Rung, "attempt", dec.Attempt, "deadline", deadline)
	if agent == pipelineAgent {
		// The run note says "diagnosing" only now, AFTER the running record
		// landed (D9, the wnz6/l900 ordering rule) -- and it is the run note on
		// the pipeline's landing, never an issue status note.
		d.postPipelineRunNote(ctx, base, runNote{State: pipelinectx.RunStateDiagnosing,
			Status: diagnosingStatus(base, dec.Attempt, dec.Rung)})
		return 0
	}
	if agent == pipefixAgent {
		// The fix note says "proposing" only now, AFTER the running record
		// landed (plan 7.1: `proposing` is "dispatch wrote the running
		// record"; 8.1) -- on the DIAGNOSIS landing, beside the run note.
		d.postFixNote(ctx, base, d.fixRun, fixProposing(base, dec.Attempt, dec.Rung), false)
		return 0
	}
	if !runNoted {
		d.postRunningNotice(ctx, agent, dec, budget.hardStop(started))
	}
	return 0
}

// statusNoteHeadroom is how much of the start budget the RUN note may never
// touch: the note goes out before the prompt is built and the session started,
// and a slow GitLab must not be what makes this dispatch miss startDeadline.
const statusNoteHeadroom = 30 * time.Second

// postRunningNotice posts or edits the issue's one status note to say gonk is
// triaging it now: which attempt, on which model (gonk-l900 item 1). An account
// of the previous attempt left by the sweep's re-sling note is carried forward.
//
// BEST EFFORT, like postDeferredNotice: bounded by statusNoteTimeout and by
// notAfter, logged at WARN on failure, and never able to change the exit code.
// Only triage of an issue has a thread to write to; scaffold (project-scoped)
// and anything without an issue iid gets nothing.
func (d dispatchDeps) postRunningNotice(ctx context.Context, agent string, dec meterapi.DecideResponse, notAfter time.Time) {
	a := d.Args
	if d.Apply == nil || agent != "triage" || a.ProjectID == 0 || a.IssueIID == 0 || terminated(ctx) {
		return
	}
	timeout := phaseTimeout(d.clock().Now(), notAfter, statusNoteTimeout)
	if timeout <= 0 {
		d.Log.Warn("not posting the triaging-now status note: no time is left for it in this dispatch's budget",
			"bead", a.BeadAnchor, "attempt", dec.Attempt)
		return
	}
	nctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sd := sweepDeps{Apply: d.Apply, GL: d.GL, BotUsername: d.BotUsername, Log: d.Log}
	upsertStatusNoteWith(nctx, sd, a.ProjectID, a.IssueIID, a.BeadID, func(existing *glab.Note) string {
		return runningBody(dec.Attempt, dec.Rung, carriedPrevious(existing, dec.Attempt-1), d.BotUsername)
	})
}

// reservationKind is what a dispatch knows about its own pending-prompt write.
type reservationKind int

const (
	// reservationNone: nothing was written, so a release has nothing to undo.
	reservationNone reservationKind = iota
	// reservationHeld: the write landed, at reservation.revision.
	reservationHeld
	// reservationUnknown: the write reported an error. It may still have
	// landed -- BdCLI.PutIf can fail after its comment is appended -- so its
	// revision, if any, is unknown.
	reservationUnknown
)

// reservation is exactly what runBrokerDispatch's release is allowed to undo.
type reservation struct {
	kind reservationKind
	// revision is the reservation's own write (reservationHeld only).
	revision string
	// release is the record the reservation replaced: the record the guard
	// read, or base with no session id when there was none or it was unreadable.
	release beadstore.Record
	// claim is the reservation record this dispatch WROTE (reservationUnknown
	// only): what the release compares the store's record against when it
	// cannot compare revisions.
	claim beadstore.Record
}

// maxReserveTries bounds reservePendingPrompt's read-and-reserve loop. One
// retry covers the ordinary case -- gonk-sweep's fire-time write landing
// between the fired dispatch's read and its reservation -- and a record that
// changes under three consecutive reads is being actively written by someone
// else, which is no time to create a session for it.
const maxReserveTries = 3

// reservePendingPrompt is the dedupe guard and the reservation write as ONE
// compare-and-swap (gonk-y4m7): read the record, refuse if it already holds a
// session for this attempt, and write the reservation only if the record is
// still the one just read -- PutIf against its revision, "" when there was none.
//
// It returns proceed=false with the exit code when dispatch must stop here.
//
// AN ErrStale IS NOT "SOMEONE ELSE RESERVED". It means only that the record
// moved. Besides another dispatch's reservation, the writer can be gonk-sweep
// re-stamping the running record with a new re-sling (its window ended while
// this dispatch -- from intake, or an earlier fire -- sat between its read and
// its reservation). Backing off there would silently drop this dispatch. So a stale reservation
// reads the record AGAIN and re-runs the guard: a session for this attempt
// now means back off (exit 0, exactly as the guard does); anything else means
// reserve against the new revision, and release to THAT record.
//
// An unreadable store still does not fail closed (the gonk-u6p bias: a
// duplicate is recoverable, a dropped dispatch is not) -- but it no longer
// writes over what it could not read either. PutIf against "" lands only when
// there is no record at all; when one exists, dispatch proceeds WITHOUT a
// reservation rather than overwrite a record it never saw.
//
// A reservation write that fails for any other reason proceeds, as it always
// has; the release then has to find out whether that write landed.
func reservePendingPrompt(ctx context.Context, d dispatchDeps, base, pending beadstore.Record) (res reservation, code int, proceed bool) {
	a := d.Args
	nothingDispatched := base
	nothingDispatched.SessionID = ""
	couldNotReserve := func(err error) {
		// Same bias as an unreadable store: an unwritable one must not block a
		// legitimate dispatch, only weaken this particular guard for it.
		d.Log.Error("could not reserve the pending-prompt record before creating a session; "+
			"proceeding, but a duplicate session for this attempt cannot be ruled out",
			"err", err, "bead", a.BeadAnchor, "session", pending.SessionID)
	}
	for try := 1; ; try++ {
		prior, priorFound, perr := d.Store.Get(ctx, a.BeadAnchor)
		if perr != nil {
			d.Log.Error("could not read the bead store before creating a session; "+
				"proceeding, but a duplicate session for this attempt cannot be ruled out",
				"err", perr, "bead", a.BeadAnchor)
			rev, err := d.Store.PutIf(ctx, pending, "")
			switch {
			case err == nil:
				return reservation{kind: reservationHeld, revision: rev, release: nothingDispatched}, 0, true
			case errors.Is(err, beadstore.ErrStale):
				d.Log.Error("not reserving the pending-prompt record: the bead store could not be read and a "+
					"record already exists, which this dispatch will not write over unseen; proceeding "+
					"without a reservation", "bead", a.BeadAnchor, "session", pending.SessionID)
				return reservation{kind: reservationNone}, 0, true
			default:
				couldNotReserve(err)
				return reservation{kind: reservationUnknown, release: nothingDispatched, claim: pending}, 0, true
			}
		}
		if priorFound && prior.SessionID != "" && prior.Attempt == pending.Attempt {
			d.Log.Warn("a session already exists for this bead and attempt; not creating a second",
				"bead", a.BeadAnchor, "attempt", pending.Attempt, "session", prior.SessionID)
			return reservation{}, 0, false
		}
		release, ifRevision := nothingDispatched, ""
		if priorFound {
			release, ifRevision = prior, prior.Revision
		}
		rev, err := d.Store.PutIf(ctx, pending, ifRevision)
		switch {
		case err == nil:
			return reservation{kind: reservationHeld, revision: rev, release: release}, 0, true
		case errors.Is(err, beadstore.ErrStale):
			if try >= maxReserveTries {
				d.Log.Error("not creating a session: the bead record kept changing between this dispatch's "+
					"read and its pending-prompt reservation, so another writer is active on it",
					"bead", a.BeadAnchor, "session", pending.SessionID, "tries", try, "err", err)
				return reservation{}, 1, false
			}
			d.Log.Info("the bead record changed between this dispatch's read and its reservation; reading it again",
				"bead", a.BeadAnchor, "session", pending.SessionID, "try", try)
		default:
			couldNotReserve(err)
			return reservation{kind: reservationUnknown, release: release, claim: pending}, 0, true
		}
	}
}

// releasePendingPrompt puts back the record a failed dispatch's reservation
// replaced -- but ONLY over that reservation (gonk-y4m7). It is a PutIf against
// the reservation's own revision, so a record anyone has written since is left
// exactly where it is. That is the 15:52:31 write in go-3tbq's log: MPKO's
// release, unconditional then, wrote its minutes-old prior over the reservation
// FYKO had written after MPKO's was erased.
//
// When the reservation write reported an error its revision is unknown, so the
// record is read first: it is this dispatch's reservation only if it is
// CONTENT-EQUAL to the reservation this dispatch wrote (sameRecordContent:
// every field but the store's Revision and UpdatedAt). Under Gas City the
// alias carried 128 bits of nonce and matching it was enough; the session
// name is deterministic (plan D2), so a record naming this session can be
// another dispatch's reservation for the same attempt. In production that one
// carries the same meter answer and can be content-equal too -- but such a
// dispatch passed the guard only if this reservation did NOT land, and then
// the worst a release over its reservation can do is let a third dispatch
// through the guard, whose Start reaches the SAME session by name rather than
// a second pod. Anything else is left alone. A read that fails
// releases nothing; if the reservation did land, gonk-sweep reclaims it after
// pendingPromptGrace.
//
// ErrStale IS NOT A FAILURE HERE. It is the release correctly declining to
// overwrite a newer writer, and it is logged as that. Nothing on this path
// closes a session: any close already happened before the release runs.
func releasePendingPrompt(ctx context.Context, d dispatchDeps, session string, res reservation) {
	a := d.Args
	ifRevision := res.revision
	if res.kind == reservationUnknown {
		cur, found, err := d.Store.Get(ctx, a.BeadAnchor)
		if err != nil {
			d.Log.Error("could not release the pending-prompt reservation after a failed dispatch: its write "+
				"reported an error and the record cannot be read to tell whether it landed; if it did, "+
				"gonk-sweep reclaims it after pendingPromptGrace",
				"err", err, "bead", a.BeadAnchor, "session", session)
			return
		}
		if !found || cur.State != beadstore.StatePendingPrompt || !sameRecordContent(cur, res.claim) {
			d.Log.Info("the pending-prompt reservation this dispatch could not confirm is not in the store; "+
				"nothing to release", "bead", a.BeadAnchor, "session", session,
				"state", string(cur.State), "session", cur.SessionID)
			return
		}
		ifRevision = cur.Revision
	}
	_, err := d.Store.PutIf(ctx, res.release, ifRevision)
	switch {
	case errors.Is(err, beadstore.ErrStale):
		d.Log.Warn("not releasing the pending-prompt reservation: the record changed after this dispatch "+
			"reserved it, so the newer record is left in place",
			"bead", a.BeadAnchor, "session", session, "err", err)
	case err != nil:
		d.Log.Error("could not release the pending-prompt reservation after a failed dispatch; "+
			"a retry may wrongly see this attempt as already dispatched",
			"err", err, "bead", a.BeadAnchor, "session", session)
	}
}

// abandonSession stops a session dispatch started but is walking away from.
//
// It exists because of an asymmetry that is easy to miss: gonk-sweep only
// judges -- and so only stops -- sessions belonging to a bead that reached
// StateRunning. A session started for a record that then failed to land is
// one no sweep pass will look at.
//
// Failure to stop is logged and swallowed: dispatch is already failing, and
// the caller's exit code must reflect the dispatch failure that brought us
// here, not the teardown.
//
// A NOT-FOUND IS "ALREADY GONE", AND UNDER JOBS THAT READING IS EARNED
// (gonk-wmb7 polarity, plan D9). Under Gas City a 404 on close meant only
// "no OPEN session bead answers to this alias" while a pod could run behind
// it, so it was logged as a possible leak. A Job is deleted with its pod
// (Foreground propagation), so the runtime not knowing the session means the
// pod is gone -- INFO, not ERROR.
//
// The stop runs under its own timeout, handed in from the order budget. A
// dispatch that has been told to stop does not start it at all.
func abandonSession(ctx context.Context, d dispatchDeps, agent string, h runtime.Handle, timeout time.Duration) {
	if terminated(ctx) {
		d.Log.Error("dispatch terminated: NOT stopping the abandoned session; it ends at its deadline",
			"agent", agent, "session", string(h), "cause", context.Cause(ctx))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := d.Runtime.Stop(cctx, h); err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			d.Log.Info("abandoned session already gone", "agent", agent, "session", string(h))
			return
		}
		d.Log.Error("could not stop the abandoned session -- it runs until its deadline",
			"agent", agent, "session", string(h), "err", err)
		return
	}
	d.Log.Info("abandoned session stopped", "agent", agent, "session", string(h))
}
