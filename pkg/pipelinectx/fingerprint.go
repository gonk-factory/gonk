package pipelinectx

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// FailedJob is the part of a failed GitLab job the fingerprint reads. Callers
// pass the LATEST attempt of each failed job (include_retried=false).
type FailedJob struct {
	Stage         string
	Name          string
	FailureReason string // GitLab's job failure_reason, e.g. "script_failure"
	AllowFailure  bool
}

// NoJobsReason stands in for an empty pipeline failure_reason when a pipeline
// failed without any failed job (a config/YAML error).
const NoJobsReason = "no_jobs"

// Fingerprint identifies a pipeline's failing-job SET (plan D2): the first 16
// hex of sha256 over the sorted lines "stage\x00name\x00failure_reason" of the
// failed jobs with AllowFailure == false, each line terminated by "\n". Order of
// jobs does not matter; allow_failure jobs do not count. When no job counts,
// the single line is "pipeline\x00<pipelineFailureReason or no_jobs>".
func Fingerprint(jobs []FailedJob, pipelineFailureReason string) string {
	var lines []string
	for _, j := range jobs {
		if j.AllowFailure {
			continue
		}
		lines = append(lines, j.Stage+"\x00"+j.Name+"\x00"+j.FailureReason)
	}
	if len(lines) == 0 {
		r := pipelineFailureReason
		if r == "" {
			r = NoJobsReason
		}
		lines = []string{"pipeline\x00" + r}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])[:16]
}
