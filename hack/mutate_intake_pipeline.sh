#!/usr/bin/env bash
# mutate_intake_pipeline.sh -- "seen red" evidence for gonk-tios P3 (Pipeline
# Hook parsing, pipeline intake: gate, run identity, already-handled check,
# hook provisioning, backfill).
#
# Same contract as hack/mutate_pipelinectx.sh: apply ONE named break at a time,
# run the affected packages' tests, require them to FAIL, restore the file
# byte for byte. A break whose text is not found (VACUOUS) or that does not
# build (BROKEN) proves nothing and fails the run; one the tests do not notice
# SURVIVED and fails the run.
#
# Usage: hack/mutate_intake_pipeline.sh            (all mutations)
#        hack/mutate_intake_pipeline.sh <substring> (only names containing it)
# Exit: 0 iff every selected mutation applied and was killed.
#
# Edits files in the CURRENT worktree and restores them on exit (trap); run it
# in your own worktree, never in a shared checkout.
set -u -o pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-go}"
filter="${1:-}"
backup="$(mktemp -d)"
restore() {
	if [ -f "$backup/path" ]; then
		cp "$backup/orig" "$root/$(cat "$backup/path")"
	fi
	rm -rf "$backup"
}
trap restore EXIT

pkgs=(./pkg/intake/ ./pkg/ghook/ ./pkg/pipelinectx/ ./cmd/gonk-intake/)
killed=0
survived=0
vacuous=0

# mut NAME FILE FROM TO [FROM2 TO2 ...] -- replace the first literal FROM by
# TO. FILE is relative to the repo root. Several pairs make ONE mutant.
mut() {
	local name="$1" rel="$2"
	shift 2
	if [ -n "$filter" ] && [[ "$name" != *"$filter"* ]]; then
		return
	fi
	local f="$root/$rel"
	cp "$f" "$backup/orig"
	printf '%s' "$rel" >"$backup/path"
	local ok=1
	while [ $# -ge 2 ]; do
		FROM="$1" TO="$2" perl -0pi -e '$i = index($_, $ENV{FROM}); if ($i >= 0) { substr($_, $i, length($ENV{FROM})) = $ENV{TO}; $n++ } END { exit($n ? 0 : 3) }' "$f" || ok=0
		shift 2
	done
	if [ $ok -ne 1 ] || cmp -s "$f" "$backup/orig"; then
		echo "VACUOUS  $name  (text not found in $rel)"
		vacuous=$((vacuous + 1))
		cp "$backup/orig" "$f"
		rm -f "$backup/path"
		return
	fi
	local out
	out="$(cd "$root" && "$GO" test -count=1 "${pkgs[@]}" 2>&1)"
	local rc=$?
	cp "$backup/orig" "$f"
	rm -f "$backup/path"
	if printf '%s\n' "$out" | grep -q -e '\[build failed\]' -e '\[setup failed\]'; then
		echo "BROKEN   $name  (mutant does not build)"
		printf '%s\n' "$out" | grep -v '^ok' | head -5 | sed 's/^/           /'
		vacuous=$((vacuous + 1))
	elif [ $rc -eq 0 ]; then
		echo "SURVIVED $name"
		survived=$((survived + 1))
	else
		killed=$((killed + 1))
		echo "KILLED   $name"
		printf '%s\n' "$out" | grep -E -e '^\s*--- FAIL' -e '^panic:' | sed 's/^/           /' | head -8
	fi
}

I=pkg/intake
# --- hook provisioning (D4): the "renders fine and enforces nothing" class ---
mut "hook: pipeline_events enabled for EVERY project" $I/reconcile.go \
	'PipelineEvents:        wantPipelines,' 'PipelineEvents:        true,'
mut "hook: pipeline_events never enabled" $I/reconcile.go \
	'		hp = hookPipelinesOn' '		hp = hookPipelinesOff'
mut "hook: pipeline_events drift not compared (opt-out never edits it back)" $I/reconcile.go \
	'h.PipelineEvents == want.PipelineEvents &&' ''
mut "hook: wantPipelines from triage, not pipelines" $I/reconcile.go \
	'	case cls.MayDiagnosePipelines():' '	case cls.MayTriage():'
# verify report R10: a transient meter failure is "unknown", not "off".
mut "hook R10: transient meter failure turns pipeline_events off" $I/reconcile.go \
	'		hp = hookPipelinesKeep' '		hp = hookPipelinesOff'
mut "hook R10: keep ignores the existing hook's setting" $I/reconcile.go \
	'want.PipelineEvents = h.PipelineEvents' '_ = h'
mut "hook R10: registration failure not marked unknown" $I/reconcile.go \
	'					meterUnknown = true' ''

# --- gate (D5): per-trigger predicate ---
mut "gate: pipelines gated on MayTriage (hard-coded, as before)" $I/dispatch.go \
	'		return cls.MayDiagnosePipelines()' '		return cls.MayTriage()'
mut "gate: unknown trigger allowed" $I/dispatch.go \
	'		return cls.MayDiagnosePipelines()
	}
	return false' '		return cls.MayDiagnosePipelines()
	}
	return true'

# --- target rules (D1) ---
mut "target: non-failed status not dropped" $I/pipeline.go \
	'if p.Status != pipelineStatusFailed {' 'if false {'
mut "target: child pipeline not dropped" $I/pipeline.go \
	'if p.Source == sourceParentPipeline {' 'if false {'
mut "target: fork MR not dropped" $I/pipeline.go \
	'if p.MRSourceProjectID != ev.Project.ID {' 'if false {'
mut "target: MR pipeline with no MR not dropped" $I/pipeline.go \
	'if p.MRIID < 1 {' 'if false {'
mut "target: tag not dropped" $I/pipeline.go \
	'	if p.Tag {' '	if false {'
mut "target: more than one open MR accepted" $I/pipeline.go \
	'if len(open) == 1 {' 'if len(open) >= 1 {'
mut "target: closed MR counted as open" $I/pipeline.go \
	'mr.State == mrStateOpened && mr.SourceBranch' 'mr.SourceBranch'
mut "target: fork MR from a same-named branch counted" $I/pipeline.go \
	' && mr.SourceProjectID == ev.Project.ID {' ' {'
mut "target: any branch is a ref target" $I/pipeline.go \
	'if defaultBranch != "" && p.Ref == defaultBranch {' 'if true {'

# --- run identity (D2) ---
mut "identity: last success left out of the anchor" $I/pipeline.go \
	'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, f.FixEpoch)' 'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, 0, f.FixEpoch)'
mut "identity: pipeline id used as the fingerprint input" $I/pipeline.go \
	'fp := pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason)' 'fp := pipelinectx.Fingerprint(append(f.FailedJobs, pipelinectx.FailedJob{Name: fmt.Sprint(ev.Pipeline.ID)}), f.FailureReason)'
mut "facts: ref last success ignores status (server and client)" $I/pipeline.go \
	'ps, err = g.GL.ListPipelines(ctx, pid, t.Ref, pipelineStatusSuccess, lastSuccessPage)' 'ps, err = g.GL.ListPipelines(ctx, pid, t.Ref, "", lastSuccessPage)' \
	'pl.Status == pipelineStatusSuccess && ' ''
# verify report R3: a green CHILD pipeline is not a green of the target.
mut "facts R3: child pipeline counted as the last success" $I/pipeline.go \
	'pl.Source != sourceParentPipeline && ' ''
mut "facts: MR last success read from the default branch" $I/pipeline.go \
	'ps, err = g.GL.ListMRPipelines(ctx, pid, t.MRIID)' 'ps, err = g.GL.ListPipelines(ctx, pid, "main", pipelineStatusSuccess, 1)'
mut "facts: zero-job pipeline failure reason not read" $I/pipeline.go \
	'f.FailureReason = full.FailureReason' '_ = full'
mut "facts: jobs read error swallowed" $I/pipeline.go \
	'return nil, fmt.Errorf("pipeline facts: failed jobs of %d: %w", p.ID, err)' 'return f, nil'
mut "facts: BotUserID zero accepted" $I/pipeline.go \
	'if g.BotUserID <= 0 {' 'if false {'

# --- already-handled (D3) ---
mut "handled: landing read error proceeds (fail OPEN)" $I/pipeline.go \
	'if f.LandingErr != nil {' 'if false {'
mut "handled: unknown marker state proceeds (fail OPEN)" $I/pipeline.go \
	'	if unknown {
		d.Reason = ReasonLandingUnreadable' '	if unknown && false {
		d.Reason = ReasonLandingUnreadable'
mut "handled: handled states ignored" $I/pipeline.go \
	'if pipelinectx.RunHandled(st) {' 'if false {'
mut "handled: human-authored note counts" $I/pipeline.go \
	'if n.System || n.Author.ID != g.BotUserID {' 'if n.System {'
mut "handled: system note counts" $I/pipeline.go \
	'if n.System || n.Author.ID != g.BotUserID {' 'if n.Author.ID != g.BotUserID {'
mut "handled: human-authored issue counts" $I/pipeline.go \
	'State: "all", AuthorID: g.BotUserID}' 'State: "all"}' \
	'if is.Author.ID != g.BotUserID || !strings.Contains' 'if !strings.Contains'
mut "handled: only OPEN tracking issues read (a human close resets the run)" $I/pipeline.go \
	'State: "all", AuthorID: g.BotUserID}' 'State: "opened", AuthorID: g.BotUserID}'
mut "handled: issue for any ref counts" $I/pipeline.go \
	'!strings.Contains(is.Description, marker)' '(!strings.Contains(is.Description, marker) && false)'
mut "handled: issue listing error proceeds" $I/pipeline.go \
	'return nil, fmt.Errorf("bot-authored issues: %w", err)' 'return nil, nil'
# verify report B23: a tracking issue whose NOTES cannot be read fails closed.
mut "handled B23: tracking-issue notes read error proceeds (fail OPEN)" $I/pipeline.go \
	'return nil, fmt.Errorf("notes of issue #%d: %w", is.IID, err)' 'continue'
mut "handled: superseded not handled" pkg/pipelinectx/runmarker.go \
	'case RunStateDiagnosed, RunStateDenied, RunStateResolved, RunStateSuperseded:' 'case RunStateDiagnosed, RunStateDenied, RunStateResolved:'
mut "marker: anchor matched by prefix" pkg/pipelinectx/runmarker.go \
	'if m[1] == anchor {' 'if strings.HasPrefix(m[1], anchor[:len(anchor)-1]) {'
mut "marker: format drift (state: instead of state=)" pkg/pipelinectx/runmarker.go \
	'return runMarkerPrefix + anchor + " state=" + state + " -->", nil' 'return runMarkerPrefix + anchor + " state:" + state + " -->", nil'

# --- Handle: the nil deref, the order, Gate-1 deny ---
mut "handle: deny labels via ev.Issue on a pipeline (nil deref)" $I/dispatch.go \
	'if d.Labeler != nil && ev.Issue != nil {' 'if d.Labeler != nil {'
mut "handle: order reads ev.Issue.IID unconditionally (nil deref)" $I/dispatch.go \
	'IssueIID:      issueIID(ev),' 'IssueIID:      ev.Issue.IID,'
mut "handle: order drops the run identity" $I/dispatch.go \
	'			PipelineTarget:      dec.PipelineTarget,' ''
mut "order: pipeline_id not omitempty (issue orders change)" $I/dispatch.go \
	'`json:"pipeline_id,omitempty"`' '`json:"pipeline_id"`'
mut "handle: facts error does not fail closed" $I/dispatch.go \
	'			rec.fail(ReasonPipelineFactsError, err)
			return false, ""' '			_ = err'
mut "handle: sweep suppression ignored" $I/dispatch.go \
	'if suppressed != nil && suppressed(dec.BeadAnchor) {' 'if false {'
mut "decide: issue event without issue dereferenced" $I/dispatch.go \
	'		if ev.Issue == nil {
			return Decision{Reason: ReasonMalformedEvent}
		}
' ''

# --- backfill (D4) ---
mut "backfill: newest of ANY status taken" $I/pipeline_sweep.go \
	'if pipelinectx.TerminalForHealth(sorted[i].Status) {' 'if pipelinectx.TerminalForHealth(sorted[i].Status) || true {'
mut "backfill: child pipelines not passed over" $I/pipeline_sweep.go \
	'if sorted[i].Source == sourceParentPipeline {' 'if false {'
mut "backfill: oldest-first selection" $I/pipeline_sweep.go \
	'return sorted[i].ID > sorted[j].ID' 'return sorted[i].ID < sorted[j].ID'
mut "backfill: bot-triggered pipeline dispatched" $I/pipeline_sweep.go \
	'if full.User.ID == r.BotUserID {' 'if false {'
mut "backfill: not gated on the pipelines action" $I/pipeline_sweep.go \
	'if _, allowed := gate(entry, atags.TriggerPipelineDiagnosis); !allowed {' 'if _, allowed := gate(entry, atags.TriggerPipelineDiagnosis); !allowed && false {'
mut "backfill: no suppression memory" $I/pipeline_sweep.go \
	'			fired[anchor] = now' '			_ = anchor'
mut "backfill: suppression memory not carried across passes" $I/reconcile.go \
	'		entry.PipelineDispatchedAt = prev.PipelineDispatchedAt' ''
mut "backfill: MR recency bound gone (client and server)" $I/pipeline_sweep.go \
	'State: mrStateOpened, UpdatedAfter: cutoff})' 'State: mrStateOpened, UpdatedAfter: cutoff.Add(-1000 * time.Hour)})' \
	'mr.UpdatedAt == nil || mr.UpdatedAt.Before(cutoff)' 'mr.UpdatedAt == nil || mr.UpdatedAt.Before(cutoff.Add(-1000 * time.Hour))'
mut "backfill: not called from ReconcileOnce" $I/reconcile.go \
	'plSwept, plDispatched, perr := r.sweepPipelines(ctx, p)' 'plSwept, plDispatched, perr := 0, 0, error(nil)'

# --- ghook ---
mut "ghook: Pipeline Hook not handled by the receiver" pkg/ghook/receiver.go \
	'	"Pipeline Hook": true,' ''
mut "ghook: Pipeline Hook header not mapped" pkg/ghook/event.go \
	'	"Pipeline Hook":      KindPipeline,' ''
mut "ghook: dedupe key without status" pkg/ghook/dedupe.go \
	'id = fmt.Sprintf("pipeline:%d:%s", ev.Pipeline.ID, ev.Pipeline.Status)' 'id = fmt.Sprintf("pipeline:%d", ev.Pipeline.ID)'
mut "ghook: merge_request object not read" pkg/ghook/event.go \
	'pl.MRIID, pl.MRSourceProjectID = raw.MergeRequest.IID, raw.MergeRequest.SourceProjectID' '_ = raw'
mut "ghook: pipeline without id accepted" pkg/ghook/event.go \
	'return nil, fmt.Errorf("ghook: pipeline event has no id")' '_ = 0'

# --- wiring ---
mut "wiring: no pipeline fact source" cmd/gonk-intake/main.go \
	'dp.Pipelines = &intake.GitLabPipelineFacts{GL: gl, BotUserID: me.ID}' '_ = me'
mut "wiring: backfill not wired" cmd/gonk-intake/main.go \
	'		Pipelines:        dp,' ''

# --- fix epoch (gonk-2usw EPOCH, plan 3.3) ---
FE=$I/pipeline_fixepoch.go
mut "epoch: the decision ignores the fix epoch (the bug: same anchor after a merged fix)" $I/pipeline.go \
	'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, f.FixEpoch)' 'anchor, err := pipelinectx.Anchor(ev.Project.ID, t, fp, f.LastSuccess, 0)'
mut "epoch: the landing is read under the fix-less anchor (the old run's marker answers)" $I/pipeline.go \
	'pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason), f.LastSuccess, f.FixEpoch)' 'pipelinectx.Fingerprint(f.FailedJobs, f.FailureReason), f.LastSuccess, 0)'
mut "epoch: Gather never computes it" $I/pipeline.go \
	'f.FixEpoch, err = g.fixEpoch(ctx, pid, p, t, last)' 'f.FixEpoch, err = 0, error(nil)'
mut "epoch: an unreadable epoch reads as no fix (fail OPEN)" $I/pipeline.go \
	'return nil, fmt.Errorf("pipeline facts: fix epoch on %s: %w", t, err)' 'f.FixEpoch = 0'
mut "epoch: the decision does not carry it" $I/pipeline.go \
	'PipelineLastSuccess: f.LastSuccess, PipelineFixEpoch: f.FixEpoch,' 'PipelineLastSuccess: f.LastSuccess,'
mut "epoch: the order drops it" $I/dispatch.go \
	'			PipelineFixEpoch:    dec.PipelineFixEpoch,
' ''
mut "epoch: pipeline_fix_epoch not omitempty (orders without a fix change)" $I/dispatch.go \
	'`json:"pipeline_fix_epoch,omitempty"`' '`json:"pipeline_fix_epoch"`'
mut "epoch: the decision record names a zero epoch" $I/decision_record.go \
	'if d.PipelineFixEpoch != 0 {' 'if true {'
mut "epoch: the decision record never names it" $I/decision_record.go \
	'if d.PipelineFixEpoch != 0 {' 'if false {'
mut "fixes: a human-authored MR counts" $FE \
	'if mr.State != mrStateMerged || mr.Author.ID != g.BotUserID {' 'if mr.State != mrStateMerged {'
mut "fixes: an unmerged MR counts" $FE \
	'if mr.State != mrStateMerged || mr.Author.ID != g.BotUserID {' 'if mr.Author.ID != g.BotUserID {'
mut "fixes: any bot-authored branch counts" $FE \
	'if !pipelinectx.IsFixBranch(mr.SourceBranch) {' 'if false {'
mut "fixes: a fork MR counts" $FE \
	'if mr.SourceProjectID != projectID || mr.TargetProjectID != projectID {' 'if mr.TargetProjectID != projectID {'
mut "fixes: an MR into another project counts" $FE \
	'if mr.SourceProjectID != projectID || mr.TargetProjectID != projectID {' 'if mr.SourceProjectID != projectID {'
# The branch is checked by the source AND re-checked by the caller; both must
# go for a fix into another branch to count on a ref target.
mut "fixes: a fix merged into another branch counts (ref target)" $FE \
	'if targetBranch != "" && mr.TargetBranch != targetBranch {' 'if false {' \
	'if x.TargetBranch == branch {' 'if true {'
# On an MR-event pipeline the caller's re-check is the only one.
mut "fixes: a fix merged into another branch counts (MR-event target)" $FE \
	'if x.TargetBranch == branch {' 'if true {'
mut "fixes: a merged fix without merged_at is skipped (fail OPEN)" $FE \
	'return nil, fmt.Errorf("merged fixes: merged fix !%d carries no merged_at", mr.IID)' 'continue'
mut "fixes: a list error reads as no fixes (fail OPEN)" $FE \
	'return nil, fmt.Errorf("merged fixes: bot-authored merged merge requests: %w", err)' 'return nil, nil'
mut "fixes: BotUserID zero accepted" $FE \
	'if g.BotUserID <= 0 {' 'if false {'
mut "fixes: the merge commit is not a landed commit" $FE \
	'LandedSHAs: []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA},' 'LandedSHAs: []string{mr.SquashCommitSHA, mr.SHA},'
mut "fixes: the squash commit is not a landed commit" $FE \
	'LandedSHAs: []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA},' 'LandedSHAs: []string{mr.MergeCommitSHA, mr.SHA},'
mut "fixes: a fast-forwarded head is not a landed commit" $FE \
	'LandedSHAs: []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA},' 'LandedSHAs: []string{mr.MergeCommitSHA, mr.SquashCommitSHA},'
mut "epoch: a wired fix source is ignored" $FE \
	'	if src == nil {' '	if true {'
mut "epoch: an MR-event pipeline's ref is taken for its branch" $FE \
	'	case p.Source != sourceMergeRequest:' '	default:'
mut "epoch: the MR is read even when the project has no merged fix" $FE \
	'	if len(fixes) == 0 {
		return 0, nil
	}
	if branch == "" {' '	if branch == "" {'
mut "epoch: an unreadable MR reads as no fix (fail OPEN)" $FE \
	'return 0, fmt.Errorf("merge request !%d: %w", t.MRIID, err)' 'return 0, nil'
mut "epoch: an unreadable pipeline reads as no fix (fail OPEN)" $FE \
	'return 0, fmt.Errorf("pipeline %d: %w", p.ID, err)' 'return 0, nil'
mut "epoch: the pipeline's commit is not passed" $FE \
	'PipelineSHA: full.SHA, PipelineCreatedAt' 'PipelineSHA: "", PipelineCreatedAt'
mut "epoch: the last green's commit is not passed" $FE \
	'LastSuccessSHA: last.SHA,' 'LastSuccessSHA: "",'
mut "epoch: the last green is not passed at all (a green never ends the epoch)" $FE \
	'LastSuccess: last.ID, LastSuccessSHA' 'LastSuccess: 0, LastSuccessSHA'

echo
echo "killed=$killed survived=$survived vacuous=$vacuous"
[ "$survived" -eq 0 ] && [ "$vacuous" -eq 0 ]
