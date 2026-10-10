package pipefix

import (
	"fmt"

	"github.com/gonk-factory/gonk/pkg/pipelinectx"
)

// RecordedJob is one failed job of the diagnosed pipeline AS RECORDED at the
// diagnosis dispatch (beadstore.PipelineJob), reduced to the failure facts.
// The caller copies it from the record; nothing here comes from a batch.
type RecordedJob struct {
	Name           string
	Stage          string
	FailureReason  string
	ErrorSignature string
	AllowFailure   bool
	// Generic is beadstore.PipelineJob.GenericFailure(): the signature cannot
	// tell this failure from another failure of the same job.
	Generic bool
	// FactsRecorded is beadstore.PipelineJob.FailureFactsRecorded(). False on
	// a job recorded before the failure facts existed: its Stage and
	// FailureReason are "" because nobody wrote them, not because GitLab
	// said so, and it cannot be judged.
	FactsRecorded bool
}

// Target is one job of the target set T (plan 3.2): a failed,
// non-allow_failure job of the diagnosed pipeline that the diagnosis's
// evidence names, with its recorded job failure key.
type Target struct {
	Key pipelinectx.JobFailureKey
	// Generic: the recorded signature is "" or was built from the runner's
	// own job-failed summary. Judge then treats EVERY later failure of the
	// job as the same failure (plan 6.3 J3, the conservative rule).
	Generic bool
}

// Name is the job's name: the one field of the key every comparison is
// keyed on.
func (t Target) Name() string { return t.Key.Name }

// TargetSet computes T from the record (plan 3.2): for each DISTINCT job name
// the landed diagnosis's evidence names, in first-cited order, the recorded
// failed job of that name -- unless it was allowed to fail, in which case it
// is not in T by definition.
//
// problems lists what makes T unusable: an evidence name with no recorded job,
// a name recorded more than once, a job whose failure facts were never
// recorded. T is the set a fix is later judged against, so a caller must not
// dispatch on a T with problems (Eligible refuses it, row E5); it is returned
// anyway so the problem can be shown.
//
// The fix session cannot change T: both arguments come from the record.
func TargetSet(jobs []RecordedJob, evidenceJobs []string) (targets []Target, problems []string) {
	seen := map[string]bool{}
	for _, name := range evidenceJobs {
		if seen[name] {
			continue
		}
		seen[name] = true
		var found []RecordedJob
		for _, j := range jobs {
			if j.Name == name {
				found = append(found, j)
			}
		}
		switch {
		case len(found) == 0:
			problems = append(problems, fmt.Sprintf("the diagnosis cites job %q, which is not a recorded failed job of the run", name))
		case len(found) > 1:
			problems = append(problems, fmt.Sprintf("job %q is recorded %d times; which failure the diagnosis means cannot be told", name, len(found)))
		case found[0].AllowFailure:
			// Not in T: a job that was allowed to fail did not fail the pipeline.
		case !found[0].FactsRecorded:
			problems = append(problems, fmt.Sprintf("job %q has no recorded failure facts (a record older than the facts)", name))
		default:
			j := found[0]
			targets = append(targets, Target{
				Key: pipelinectx.JobFailureKey{
					Stage:          j.Stage,
					Name:           j.Name,
					FailureReason:  j.FailureReason,
					ErrorSignature: j.ErrorSignature,
				},
				Generic: j.Generic || j.ErrorSignature == "",
			})
		}
	}
	return targets, problems
}

// CauseKey is the cause key of a target set (plan 3.2): pipelinectx.CauseKey
// over the targets' job failure keys. "" for an empty set.
func CauseKey(targets []Target) string {
	keys := make([]pipelinectx.JobFailureKey, 0, len(targets))
	for _, t := range targets {
		keys = append(keys, t.Key)
	}
	return pipelinectx.CauseKey(keys)
}
