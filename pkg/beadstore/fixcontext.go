package beadstore

// FixContext is what ONE pipeline-fix session was shown (gonk-2usw plan 5.2;
// plan 5.3 gate A9), as controller-held facts: the commit its edits apply to,
// the failing jobs it was shown and the hash of each log excerpt, the landed
// diagnosis's hash, every cited file with the hash of the blob gonk read, and
// the limits the prompt stated. gonk-gate's preparePipefixDispatch builds it
// in the same pass that renders the prompt; the sweep re-derives it before it
// applies the session's answer (verifyPipefixContext), exactly as a
// diagnosis's excerpts are re-derived (gonk-tios D8 step 5).
//
// NOTHING HERE COMES FROM THE SESSION. Every field is read from GitLab with
// the bot's credential, or computed from the record, before the session
// starts.
//
// It is stored on the fix record as PipelineFix.Context (written by
// gonk-dispatch with the running record) and documented in
// docs/reference/gonk-state-record.md.
type FixContext struct {
	// BaseSHA is the full commit the session's checkout was pinned to and the
	// commit its `edit` effects are applied against. It is the commit the
	// excerpted pipeline ran on.
	BaseSHA string
	// PipelineID is the failing pipeline whose jobs were shown: the newest
	// failing pipeline of the run at dispatch (plan 3.2 E7), which is not
	// necessarily the pipeline the diagnosis was written from.
	PipelineID int64
	// DiagnosisSHA256 is the hash of the landed diagnosis as shown
	// (Record.PipelineDiagnosis of the diagnosed run).
	DiagnosisSHA256 string
	// Jobs are the jobs the session was asked to fix -- the target set T of
	// plan 3.2 -- AS THEY FAILED IN PipelineID: ID is the job's id in that
	// pipeline, and the excerpt and failure facts are that job's. In T order.
	Jobs []PipelineJob
	// ExcerptVersion is pipelinectx.ExcerptVersion and ExcerptMaxBytes the ONE
	// per-job byte cap every excerpt of this session was built with (the pair
	// Record.PipelineExcerptVersion / PipelineExcerptMaxBytes is for a
	// diagnosis).
	ExcerptVersion  int
	ExcerptMaxBytes int
	// Files are the diagnosis's cited files, in its order, each with what the
	// prompt said about it.
	Files []FixContextFile
	// Limits are the numbers the prompt stated. The gate enforces the
	// envelope's own; the two are compared at gate A9 so a session is never
	// refused against a limit it was not told.
	Limits FixLimits
	// PromptSHA256 is the hash of the whole rendered prompt.
	PromptSHA256 string
}

// FixContextFile is one cited file as a fix session was shown it.
type FixContextFile struct {
	Path string
	// Editable: the prompt listed the file as one the session may change. It
	// is the dispatch's envelope applied to this path AND the blob being one
	// an edit can apply to (it exists, is text, is within the file-size cap).
	Editable bool
	// Rule is the stable code of the predicate that makes the file not
	// editable (an effects rule code such as "SC-CLASS"), "" when Editable.
	Rule string
	// Bytes and SHA256 describe the blob gonk read at BaseSHA. Set for every
	// editable file; zero when the blob was not read.
	Bytes  int
	SHA256 string
	// Shown: the blob's content was rendered into the prompt, whole. False
	// when it was left out (too large for the prompt, or it holds something
	// gonk redacts); the session then reads the file in its checkout.
	Shown bool
}

// FixLimits are the size limits a fix session was told.
type FixLimits struct {
	Edits    int   // `edit` effects in one batch
	Files    int64 // files changed
	Lines    int64 // added + removed lines
	NewBytes int64 // bytes of `new` text across the batch
}

// Recorded reports whether a fix context is present.
func (c FixContext) Recorded() bool { return c.BaseSHA != "" }

// IsZero is what `omitzero` asks: a fix record whose session has not been
// prepared stores no Context key.
func (c FixContext) IsZero() bool {
	return c.BaseSHA == "" && c.PipelineID == 0 && c.DiagnosisSHA256 == "" && len(c.Jobs) == 0 &&
		c.ExcerptVersion == 0 && c.ExcerptMaxBytes == 0 && len(c.Files) == 0 && c.Limits == (FixLimits{}) &&
		c.PromptSHA256 == ""
}
