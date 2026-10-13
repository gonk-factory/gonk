package main

// THE ENVELOPE OF A FIX, FOLDED AGAIN FROM ITS RECORD (gonk-2usw plan 4.2,
// 5.3 A3; the integration of parts AGENT, DISPATCH and APPLY).
//
// The sweep's look folds the scope once (sweep_pipefix.go readScope) and
// records what it decided as NAMES: the class names of the envelope, the
// names of the lifted classes, the caps, the cited files. A scope.Effective
// and a scope.Lift cannot be stored -- a Lift's decoders refuse on purpose --
// so the two later holders of the envelope cannot read it back:
//
//   - gonk-dispatch, which must hand part AGENT's preparer an
//     effects.FixEnvelope to build the session's prompt from, and
//   - the apply, which must hand effects.PlanFix the envelope the session's
//     answer is judged against.
//
// Both FOLD AGAIN, here, from the same sources the look used: the pack row,
// the operator layers and the project's pipeline_fix section as gonk-meter
// holds them NOW, and the project's `scope:` section read at the commit the
// look pinned (PipelineFix.ConfigSHA -- never a branch name). The lift is
// MINTED AGAIN by scope.Effective.GrantLift (envelopeFor), which re-checks the
// pack row, the project's allow_ci and the operator veto; nothing on the
// record can supply one.
//
// THE RECORD CAN ONLY BE AGREED WITH. The result must name exactly the
// classes, lift and caps the record names. If it does not -- an operator or
// the project changed something after the look -- that is an error, OURS
// (errPipefixEnvelopeChanged): the session was told one envelope and would be
// judged by another. Nothing is started, nothing is applied.
//
// AND THE PROJECT MUST STILL BE OPTED IN (plan 3.2 E1): a project whose
// effective configuration no longer has actions.pipeline_fixes (and
// pipelines) gets no fix session and no fix write, whatever a record or an
// order says (errPipefixNotOptedIn).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/gonkcfg"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/scope"
)

var (
	// errPipefixNotOptedIn: the project's effective configuration does not
	// enable pipeline fixes. No session, no write.
	errPipefixNotOptedIn = errors.New("the project has not opted in to pipeline fixes (actions.pipeline_fixes)")
	// errPipefixEnvelopeChanged: the envelope folded now is not the one the
	// fix record names. Ours, never the agent's.
	errPipefixEnvelopeChanged = errors.New("the scope envelope folded now is not the one recorded for this fix")
	// errPipefixEnvelope wraps every other failure to fold (a read, a pack
	// row, a config that does not load). Ours.
	errPipefixEnvelope = errors.New("pipefix envelope")
)

// rawFileReader is the one forge read the refold needs. Satisfied by
// *glab.Client and pipelineForge.
type rawFileReader interface {
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
}

// refoldPipefixEnvelope folds the envelope of the fix record `fix` again and
// returns it only if it agrees with the record. cfg is gonk-meter's answer
// for the project (GET /v1/projects/{project}).
func refoldPipefixEnvelope(ctx context.Context, r rawFileReader, cfg *meterapi.ProjectResponse, packDir string,
	fix beadstore.Record, log *slog.Logger) (effects.FixEnvelope, error) {
	var none effects.FixEnvelope
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", errPipefixEnvelope, fmt.Sprintf(format, args...))
	}
	pf := fix.PipelineFix
	if !pf.Recorded() {
		return none, bad("the record is not a fix record")
	}
	if r == nil {
		return none, bad("no forge reader configured")
	}
	if cfg == nil {
		return none, bad("no project configuration was read")
	}
	// E1, again, at the point of use.
	if cfg.Effective == nil || !cfg.Effective.Actions.PipelineFixes || !cfg.Effective.Actions.Pipelines {
		return none, errPipefixNotOptedIn
	}
	if cfg.OperatorScope == nil {
		return none, bad("the meter's answer carries no operator scope layers")
	}
	if !glab.IsCommitSHA(pf.ConfigSHA) || !glab.IsCommitSHA(pf.BaseSHA) {
		return none, bad("the record pins no commit (config %q, base %q)", pf.ConfigSHA, pf.BaseSHA)
	}
	row, err := scope.LoadRow(packDir, pipefixAgent)
	if err != nil {
		return none, bad("the pack's scope row for the fix agent does not load: %v", err)
	}
	if len(row.Classes)+len(row.Liftable) == 0 {
		return none, bad("agents/%s/effect-shape.toml has no [scope] table", pipefixAgent)
	}
	raw, err := r.GetRawFile(ctx, fix.ProjectID, intake.ConfigPath, pf.ConfigSHA, intake.MaxConfigBytes)
	if err != nil {
		return none, bad("read %s at %s: %v", intake.ConfigPath, pf.ConfigSHA, err)
	}
	pc, err := gonkcfg.Load(raw)
	if err != nil {
		return none, bad("%s at %s does not load: %v", intake.ConfigPath, pf.ConfigSHA, err)
	}
	fixCfg := cfg.Effective.PipelineFix.EffectivePipelineFix()
	eff, err := scope.Fold(scope.Input{
		Row:      row,
		Instance: cfg.OperatorScope.Instance.Policy(),
		Group:    cfg.OperatorScope.Group.Policy(),
		Project:  fixCfg.TightenScope(pc.Scope),
		BaseSHA:  pf.ConfigSHA,
		Enabled:  true, // checked above: both actions are on
		// The project's opt-in to the ci lift, read from meter NOW: the record
		// names the lift but cannot grant it.
		LiftOptIn: fixCfg.LiftOptIn(),
	})
	if err != nil {
		return none, bad("the scope does not fold: %v", err)
	}
	if eff, err = eff.WithBead(fixCfg.Paths); err != nil {
		return none, bad("pipeline_fix.paths does not validate: %v", err)
	}
	// The class row, and the lift MINTED AGAIN (GrantLift, inside envelopeFor).
	env, err := envelopeFor(eff, pf.Class)
	if err != nil {
		return none, bad("%v", err)
	}

	// Agree with the record, or stop.
	if !slices.Equal(env.classes, pf.Envelope) { // refold:classes
		return none, fmt.Errorf("%w: classes %v, recorded %v", errPipefixEnvelopeChanged, env.classes, pf.Envelope)
	}
	if !slices.Equal(env.liftNames(), pf.Lift) { // refold:lift
		return none, fmt.Errorf("%w: lift %v, recorded %v", errPipefixEnvelopeChanged, env.liftNames(), pf.Lift)
	}
	if c := eff.Caps; c.MaxFiles != pf.MaxFiles || c.MaxLines != pf.MaxLines || // refold:caps
		c.MaxFileBytes != pf.MaxFileBytes || c.MaxPatchBytes != pf.MaxPatchBytes {
		return none, fmt.Errorf("%w: caps %+v, recorded files=%d lines=%d file_bytes=%d patch_bytes=%d",
			errPipefixEnvelopeChanged, c, pf.MaxFiles, pf.MaxLines, pf.MaxFileBytes, pf.MaxPatchBytes)
	}
	for _, f := range pf.Files { // refold:paths
		if !eff.InBead(f) {
			return none, fmt.Errorf("%w: %q is recorded as a file this fix may touch and is outside pipeline_fix.paths now",
				errPipefixEnvelopeChanged, f)
		}
	}

	// Base facts at the commit the fix is written against: the pipeline
	// definition's local includes (they are `ci` class wherever they live).
	base := scope.BaseFacts{Generated: map[string]bool{}}
	inc, cerr, ierr := pipefixCIIncludes(ctx, r, fix.ProjectID, pf.BaseSHA, intake.MaxConfigBytes*16)
	switch {
	case ierr != nil: // refold:includes
		return none, bad("the pipeline definition's includes cannot be read: %v", ierr)
	case cerr != nil:
		return none, bad("read %s at %s: %v", pipefixCIFile, pf.BaseSHA, cerr)
	}
	base.CILocalIncludes = inc.Paths
	return effects.FixEnvelope{
		Scope: env.scope, Base: base, Lift: env.lift,
		// The cited files, from the RECORD (already inside pipeline_fix.paths).
		Cited: append([]string(nil), pf.Files...),
		Log:   log,
	}, nil
}
