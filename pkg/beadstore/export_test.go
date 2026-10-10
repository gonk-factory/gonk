package beadstore

import "time"

// SetBdReadBackoff shortens the cutover's bd read backoff for a test and
// returns the restore func.
func SetBdReadBackoff(d time.Duration) func() {
	old := bdReadBackoff
	bdReadBackoff = d
	return func() { bdReadBackoff = old }
}

// SetBdReadTimeout shortens the per-try bd read timeout for a test.
func SetBdReadTimeout(d time.Duration) func() {
	old := bdReadTimeout
	bdReadTimeout = d
	return func() { bdReadTimeout = old }
}

// PipelineRunOpenPredicate is ListOpenPipelineRuns' WHERE clause and the
// partial index's predicate.
const PipelineRunOpenPredicate = pipelineRunOpenPredicate
