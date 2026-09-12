package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab"
	"gitlab.orac.local/agentic/gonk-project/pkg/glab/glabtest"
)

func transcriptFixture() *gcapi.SessionTranscript {
	return &gcapi.SessionTranscript{
		ID: "s-abc", Format: "conversation",
		Turns: []gcapi.TranscriptTurn{
			{Role: "user", Text: "triage this issue"},
			{Role: "assistant", Text: "looking"},
			{Role: "assistant", Text: "GONK_BATCH_START\n{}\nGONK_BATCH_END"},
		},
	}
}

func recFixture() beadstore.Record {
	return beadstore.Record{
		BeadAnchor: "gonk:75:issue:71", SessionID: "gonk-75-issue-71.a1.mfqz",
		Project: "group/sortlib", IssueIID: 71, Trigger: "issue-triage", Attempt: 1, Rung: "cheap",
	}
}

// The always-on record must describe the transcript's SHAPE without carrying a
// byte of its content: a transcript is untrusted model output over untrusted
// user input.
func TestTranscriptRecordDescribesShapeAndCarriesNoContent(t *testing.T) {
	tr := transcriptFixture()
	got := describeTranscript(recFixture(), tr, tr.Text())

	if got.Turns != 3 {
		t.Errorf("Turns = %d, want 3", got.Turns)
	}
	if got.Roles != "assistant=2,user=1" {
		t.Errorf("Roles = %q, want assistant=2,user=1", got.Roles)
	}
	if !got.Fence {
		t.Error("the batch fence is present in the transcript but the record says otherwise")
	}
	if got.Empty {
		t.Error("a transcript with three turns must not read as empty")
	}
	if got.SHA256 == "" || len(got.SHA256) != 16 {
		t.Errorf("SHA256 = %q, want a 16-char prefix", got.SHA256)
	}

	var b strings.Builder
	got.log(testLogger(&b))
	for _, leaked := range []string{"triage this issue", "looking"} {
		if strings.Contains(b.String(), leaked) {
			t.Errorf("transcript content reached the log:\n%s", b.String())
		}
	}
	if !strings.Contains(b.String(), "turns=3") {
		t.Errorf("record does not carry the turn count:\n%s", b.String())
	}
}

// The two expensive-to-misread cases must be ERRORs, not Info: an empty read is
// a failed read (gonk-2tb) and a paginated read is a fragment. Both cost an
// attempt and a rung escalation if judged as a verdict.
func TestUnreadableAndPaginatedTranscriptsAreErrors(t *testing.T) {
	cases := map[string]*gcapi.SessionTranscript{
		"empty": {ID: "s-1"},
		"paginated": {ID: "s-2", Turns: []gcapi.TranscriptTurn{{Role: "assistant", Text: "partial"}},
			Pagination: &struct {
				HasMore bool `json:"has_more,omitempty"`
			}{HasMore: true}},
	}
	for name, tr := range cases {
		var b strings.Builder
		describeTranscript(recFixture(), tr, tr.Text()).log(testLogger(&b))
		if !strings.Contains(b.String(), "level=ERROR") {
			t.Errorf("%s transcript did not produce an ERROR:\n%s", name, b.String())
		}
	}
}

// THE ARCHIVE IS OFF BY DEFAULT. An unset GONK_TRANSCRIPT_DIR must write
// nothing anywhere -- the archive holds untrusted text and is dev-only.
func TestTranscriptArchiveIsOffUnlessNamed(t *testing.T) {
	var b strings.Builder
	if got := archiveTranscript(testLogger(&b), "", recFixture(), transcriptFixture()); got != "" {
		t.Errorf("archiveTranscript with no directory returned %q, want \"\"", got)
	}
	if b.String() != "" {
		t.Errorf("a disabled archive logged something:\n%s", b.String())
	}
}

// With the switch on, the transcript is written 0600, atomically, with the
// metadata a reader needs to know what they are looking at.
func TestTranscriptArchiveWritesTheTranscriptWithItsContext(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	path := archiveTranscript(testLogger(&b), dir, recFixture(), transcriptFixture())
	if path == "" {
		t.Fatalf("archive wrote nothing: %s", b.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("archived transcript is mode %o, want 0600 (it holds untrusted text)", perm)
	}

	var out struct {
		BeadAnchor string                   `json:"bead_anchor"`
		SessionID  string                   `json:"session_id"`
		Project    string                   `json:"project"`
		IssueIID   int64                    `json:"issue_iid"`
		Transcript *gcapi.SessionTranscript `json:"transcript"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.BeadAnchor != "gonk:75:issue:71" || out.IssueIID != 71 || out.Project != "group/sortlib" {
		t.Errorf("archive lost its context: %+v", out)
	}
	if len(out.Transcript.Turns) != 3 {
		t.Errorf("archive lost turns: %+v", out.Transcript)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("a temp file survived the write: %s", e.Name())
		}
	}
}

// A bead anchor carries colons and a session alias is upstream-supplied. Neither
// may be able to escape the archive directory or name a path outside it.
func TestTranscriptFileNameCannotEscapeTheDirectory(t *testing.T) {
	for _, rec := range []beadstore.Record{
		{BeadAnchor: "../../etc/passwd", SessionID: "x"},
		{BeadAnchor: "gonk:1:issue:2", SessionID: "../../../root/.ssh/authorized_keys"},
		{BeadAnchor: "", SessionID: ""},
		{BeadAnchor: "..", SessionID: ".."},
	} {
		name := transcriptFileName(rec)
		if strings.ContainsAny(name, "/\\") {
			t.Errorf("transcriptFileName(%+v) = %q, which contains a path separator", rec, name)
		}
		if filepath.Clean(filepath.Join("/archive", name)) != "/archive/"+name {
			t.Errorf("transcriptFileName(%+v) = %q escapes its directory", rec, name)
		}
	}
}

// The archive is bounded. An emptyDir that fills is a node eviction, and this
// namespace has already lost pods that way.
func TestTranscriptArchivePrunesToABoundedCount(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	log := testLogger(&b)

	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, "old-"+string(rune('a'+i))+".json")
		if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Distinct mtimes so "oldest first" is well defined.
		old := time.Now().Add(time.Duration(-10+i) * time.Minute)
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pruneTranscripts(log, dir, 2)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("after pruning to 2 the directory holds %d files: %v", len(entries), names)
	}
	// The NEWEST two survive.
	for _, e := range entries {
		if e.Name() != "old-d.json" && e.Name() != "old-e.json" {
			t.Errorf("pruning kept %s, which is not one of the two newest", e.Name())
		}
	}
}

// An archive that cannot be written must degrade to metadata-only, never fail
// the sweep: a sweep that died on a full disk would be gonk-p7qh again.
func TestTranscriptArchiveFailureIsNeverFatal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dir, []byte("i am a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if got := archiveTranscript(testLogger(&b), dir, recFixture(), transcriptFixture()); got != "" {
		t.Errorf("archive claimed to write %q into a path that is a file", got)
	}
	if !strings.Contains(b.String(), "transcript archive") {
		t.Errorf("the failure was not reported:\n%s", b.String())
	}
}

// THE FULL-PATH CHECK (gonk-pop3 item 2). The tests above prove
// describeTranscript and archiveTranscript behave; this proves the SWEEP calls
// them -- which is a different claim, and the one that decides whether anything
// survives the reaper in the cluster. It drives runSweep end to end, exactly as
// the cooldown order does, with the dev switch on.
func TestTheSweepItselfRecordsAndArchivesTheTranscript(t *testing.T) {
	gl := glabtest.New(t)
	gl.Me = glab.User{ID: 1, Username: "gonk"}
	p := gl.AddProject("group/repo", glab.AccessMaintainer)
	gl.AddIssue(p.ID, 3, "opened")

	gc := gcapitest.New(t)
	gc.FinishSession("gonk.triage.p42.i3.a1", "thinking...\n"+
		"GONK_BATCH_START\n"+
		`{"effects":[{"kind":"comment","body":"Looks like a Safari-only CSS bug."}]}`+
		"\nGONK_BATCH_END\n")

	store := beadstore.NewMemory()
	rec := brokerRunningRecord(p.ID)
	_ = store.Put(context.Background(), rec)
	fm := &fakeOutcomeMeter{outcomeNext: "done"}

	archive := t.TempDir()
	var logged strings.Builder
	code := runSweep(context.Background(), sweepDeps{
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), GL: gl.Client(),
		Apply: &recordingApplier{}, Store: store, BotUsername: "gonk", PackDir: repoPackDir,
		Log: testLogger(&logged), TranscriptDir: archive,
	})
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, logged.String())
	}

	t.Logf("\n--- what `kubectl logs deploy/gonk-controller` now shows for this session ---\n%s", logged.String())

	if !strings.Contains(logged.String(), `msg="transcript read"`) {
		t.Fatalf("the sweep never recorded the transcript it judged from:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "batch_fence=true") {
		t.Errorf("the record does not say the batch fence was found:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "archived=") {
		t.Errorf("the record does not name the archived copy:\n%s", logged.String())
	}

	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the archive holds %d files, want 1 -- the transcript did not survive the sweep", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(archive, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	// The evidence that was being lost: the agent's own words, after the
	// session that produced them is classified and its pod is due for reaping.
	if !strings.Contains(string(body), "Safari-only CSS bug") {
		t.Errorf("the archived transcript does not contain the agent's output:\n%s", body)
	}
}
