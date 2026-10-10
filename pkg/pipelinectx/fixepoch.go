package pipelinectx

import (
	"fmt"
	"regexp"
	"time"
)

// The fix epoch (gonk-2usw plan section 3.3).
//
// Without it a run is identified by (target, fingerprint, last success), and
// the fingerprint reads only (stage, name, failure_reason) of the failed jobs.
// So when a gonk fix MR for a run MERGES and the same job then fails with a
// DIFFERENT error, nothing in the identity has moved: the new failure has the
// old run's anchor, the landing says that run is diagnosed, and the failure
// the fix revealed is never looked at. The owner's requirement R3 (a follow-up
// fix for a newly revealed failure) cannot be met from there.
//
// The fix epoch is the iid of the merged fix MR a failing pipeline comes
// AFTER. It is the anchor's optional ":f<iid>" suffix, so a merged fix ends
// the run it was written for and the next failing pipeline on the target is a
// new run, whatever its fingerprint.
//
// Everything here is PURE: the reads that feed it live in pkg/intake
// (MergedFixSource, GitLabMergedFixes).

// FixBranchPrefix starts the name of every branch gonk commits a pipeline fix
// to (plan 3.2: "gonk/pipefix-" + the first 12 hex of sha256(fix anchor)).
// The branch name, together with the bot being the MR's author, is how a
// merged fix MR is RECOGNISED -- both are facts a project member cannot
// change after the fact, unlike a label.
const FixBranchPrefix = "gonk/pipefix-"

var fixBranchRe = regexp.MustCompile(`^gonk/pipefix-[0-9a-f]{12}$`)

// IsFixBranch reports whether name is exactly a fix branch name.
func IsFixBranch(name string) bool { return fixBranchRe.MatchString(name) }

// MergedFix is one gonk pipeline-fix MR that has MERGED, as GitLab reports it.
type MergedFix struct {
	IID int64
	// TargetBranch is the branch the fix merged INTO.
	TargetBranch string
	// MergedAt is GitLab's merged_at. Never zero for a merged MR.
	MergedAt time.Time
	// LandedSHAs are the commits by which the fix can be on the target: the
	// MR's merge commit, its squash commit, and its head commit (which lands
	// itself on a fast-forward merge). Empty entries are ignored.
	LandedSHAs []string
}

// FixEpochFacts is what FixEpoch needs to know about the failing pipeline and
// about the target's last green one.
type FixEpochFacts struct {
	// The failing pipeline being identified.
	PipelineSHA       string
	PipelineCreatedAt time.Time
	// The target's newest green pipeline; LastSuccess == 0 means there is none
	// and the two fields after it are unused.
	LastSuccess          int64
	LastSuccessSHA       string
	LastSuccessCreatedAt time.Time
}

// FixEpoch is the fix epoch of one failing pipeline: the iid of the NEWEST
// fix in fixes that this pipeline comes after and the last green pipeline does
// not, or 0. Newest is by MergedAt, a tie by the larger iid. The caller passes
// only fixes merged into THIS target's branch.
//
// A fix counts when BOTH hold:
//
//   - the PIPELINE is after it: the pipeline ran on one of the fix's landed
//     commits, or it was created at or after MergedAt. Plan 3.3 words the
//     epoch without this bound; it is needed. Without it, the old run's own
//     failing pipelines -- re-delivered, or found by the backfill sweep in the
//     minutes before the merge commit's pipeline finishes -- would compute the
//     NEW anchor, be diagnosed a second time as the "new" run, and leave the
//     failure the fix actually revealed to be dropped as already handled.
//     It is also what keeps the anchor of a given pipeline STABLE when a later
//     fix merges.
//
//   - the LAST GREEN pipeline is not: it did not run on a landed commit, and
//     MergedAt is after its creation (plan 3.3). A green after the fix starts
//     a new run by itself (s<last_success> moves), so the epoch is dropped.
//
// The commit test exists because merged_at alone is not a safe clock: GitLab
// stamps it AFTER creating the merge commit (observed 3 s later on the
// synthetic project, section 17 of the plan), so the merge commit's own
// pipeline -- the one that matters most -- can be created before merged_at.
//
// Fail closed: a fix with no iid or no merged_at, and a pipeline or a last
// green one whose creation time is unknown while a fix exists, are errors, not
// "no epoch". With no fixes at all nothing is examined and the answer is 0:
// a target no fix has merged into keeps the identity it always had.
func FixEpoch(fixes []MergedFix, f FixEpochFacts) (int64, error) {
	if len(fixes) == 0 {
		return 0, nil
	}
	if f.PipelineCreatedAt.IsZero() {
		return 0, fmt.Errorf("pipelinectx: fix epoch: the pipeline's creation time is unknown")
	}
	if f.LastSuccess != 0 && f.LastSuccessCreatedAt.IsZero() {
		return 0, fmt.Errorf("pipelinectx: fix epoch: the creation time of last success %d is unknown", f.LastSuccess)
	}
	var best *MergedFix
	for i := range fixes {
		x := &fixes[i]
		if x.IID < 1 {
			return 0, fmt.Errorf("pipelinectx: fix epoch: merged fix with iid %d", x.IID)
		}
		if x.MergedAt.IsZero() {
			return 0, fmt.Errorf("pipelinectx: fix epoch: merged fix !%d has no merged_at", x.IID)
		}
		after := landedOn(x, f.PipelineSHA) || !f.PipelineCreatedAt.Before(x.MergedAt)
		if !after {
			continue
		}
		if f.LastSuccess != 0 && (landedOn(x, f.LastSuccessSHA) || !x.MergedAt.After(f.LastSuccessCreatedAt)) {
			continue
		}
		if best == nil || x.MergedAt.After(best.MergedAt) || (x.MergedAt.Equal(best.MergedAt) && x.IID > best.IID) {
			best = x
		}
	}
	if best == nil {
		return 0, nil
	}
	return best.IID, nil
}

// landedOn reports whether sha is one of the fix's landed commits. An empty
// sha matches nothing.
func landedOn(x *MergedFix, sha string) bool {
	if sha == "" {
		return false
	}
	for _, s := range x.LandedSHAs {
		if s != "" && s == sha {
			return true
		}
	}
	return false
}
