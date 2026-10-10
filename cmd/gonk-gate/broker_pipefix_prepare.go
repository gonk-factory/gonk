package main

// The one function a pipeline-fix dispatch calls to prepare its session
// (gonk-2usw plan 5.2, part AGENT): context, checkout, prompt, and the facts
// to record. Part DISPATCH owns everything around it -- eligibility, the fix
// record, the order, the `proposing` note -- and part APPLY owns what is done
// with the answer; this file starts nothing and writes nothing.

import (
	"context"
	"errors"
	"fmt"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
)

// pipefixDispatch is what preparePipefixDispatch built for one session.
type pipefixDispatch struct {
	prompt      string
	checkoutURL string // "" when no checkout was granted
	checkoutSHA string // == the request's BaseSHA when one was

	// context is what the session was shown. RECORD IT on the fix record
	// before the session's answer is judged: verifyPipefixContext (plan 5.3
	// A9) re-derives every hash in it.
	context beadstore.FixContext
	// editable are the files the prompt told the session it may change, in
	// cited order. For the log line and the `proposing` note.
	editable []string
}

// pipefixMaxEdits is the `edit` cap for this pack: the pipefix shape's
// maximum, never above effects.MaxFixEdits (the shape may only tighten it).
// The prompt states it and effects.Validate enforces it, from the same file.
func pipefixMaxEdits(packDir string) (int, error) {
	shape, err := effects.LoadShape(packDir, pipefixAgent)
	if err != nil {
		return 0, err
	}
	card, ok := shape.Kinds[effects.KindEdit]
	if !ok || card.Max < 1 {
		return 0, fmt.Errorf("the %s effect shape allows no %s effect", pipefixAgent, effects.KindEdit)
	}
	return min(card.Max, effects.MaxFixEdits), nil
}

// preparePipefixDispatch reads the fix context, grants a checkout at the fix
// base commit and renders the prompt within its cap.
//
// An error means NO SESSION MAY BE STARTED and nothing was spent:
//
//   - errors.Is(err, errPipefixNothingEditable): no cited file may be edited
//     under req.Envelope at the base commit. The answer is needs-scope, and
//     the controller gives it itself (plan 3.2 E6, 4.4 way 1).
//   - any other error (errPipefixContext): the context could not be built --
//     no landed diagnosis, an unusable target set, the diagnosed job not
//     failing in the pipeline, the pipeline not on the base commit, a failed
//     read, a prompt that cannot fit. Re-evaluate on the next look.
//
// A checkout that could not be granted is an error only when the session
// would need it: when some editable file's content is not in the prompt.
// Otherwise the prompt says there is no working copy, as triage's does.
func preparePipefixDispatch(ctx context.Context, d dispatchDeps, session string, req pipefixRequest) (pipefixDispatch, error) {
	var out pipefixDispatch
	if d.Pipelines == nil {
		return out, fmt.Errorf("%w: no pipeline forge configured", errPipefixContext)
	}
	maxEdits, err := pipefixMaxEdits(d.packDir())
	if err != nil {
		return out, fmt.Errorf("%w: %v", errPipefixContext, err)
	}
	pc, err := buildPipefixContext(ctx, d.Pipelines, req, maxEdits)
	if err != nil {
		return out, err
	}
	for _, u := range pc.Unavailable {
		// Logged, never rendered: the prompt says only "(unavailable)".
		d.Log.Warn("pipefix context block unavailable; the prompt says so", "project", req.Project, "pipeline_id", req.PipelineID, "block", u)
	}
	url, sha, gerr := grantCheckoutAt(ctx, d, pipefixAgent, session, req.BaseSHA)
	if gerr != nil {
		d.Log.Warn("could not grant a checkout at the fix base commit", "err", gerr, "project", req.Project, "base_sha", req.BaseSHA)
		url, sha = "", ""
	}
	out.checkoutURL, out.checkoutSHA = url, sha
	prompt, err := fitPipefixPrompt(pc, sha)
	if err != nil {
		return pipefixDispatch{}, errors.Join(errPipefixContext, err)
	}
	if sha == "" {
		for _, f := range pc.editable() {
			if !f.Shown {
				return pipefixDispatch{}, fmt.Errorf("%w: no checkout could be granted and the content of %q is not in the prompt (%s): "+
					"the session could not read the file it must edit", errPipefixContext, f.Path, f.NotShown)
			}
		}
	}
	out.prompt = prompt
	out.context = pc.Facts(prompt)
	for _, f := range pc.editable() {
		out.editable = append(out.editable, f.Path)
	}
	pc.release()
	return out, nil
}
