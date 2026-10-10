package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/intake"
	"github.com/gonk-factory/gonk/pkg/meterapi"
)

// noticeForge is a STATEFUL issue thread: it is both the brokerApplier dispatch
// writes through and the gitlabQuerier it reads notes back from, so
// edit-in-place is observable (a create-only fake could not tell an edit from a
// second note). gonk-wnz6.
type noticeForge struct {
	*recordingApplier
	// It implements gitlabQuerier itself: ListIssueNotes here, GetIssue and
	// GetRawFile in status_note_lifecycle_test.go (the sweep reads the issue).

	mu        sync.Mutex
	thread    []glab.Note
	nextID    int64
	creates   int
	edits     int
	labelAdds []string
	failAll   error
	// failList fails every ListIssueNotes; failUpdate every UpdateIssueNote
	// (gonk-l900: each GitLab step must be able to fail on its own).
	failList   error
	failUpdate error
}

func newNoticeForge() *noticeForge {
	return &noticeForge{recordingApplier: &recordingApplier{}, nextID: 100}
}

func (f *noticeForge) CreateIssueNote(_ context.Context, _, _ int64, body string) (*glab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	f.nextID++
	f.creates++
	n := glab.Note{ID: f.nextID, Body: body, Author: glab.User{Username: "gonk-bot"}}
	f.thread = append(f.thread, n)
	return &n, nil
}

func (f *noticeForge) UpdateIssueNote(_ context.Context, _, _, id int64, body string) (*glab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	if f.failUpdate != nil {
		return nil, f.failUpdate
	}
	for i := range f.thread {
		if f.thread[i].ID == id {
			f.thread[i].Body = body
			f.edits++
			return &f.thread[i], nil
		}
	}
	return nil, errors.New("no such note")
}

func (f *noticeForge) AddIssueLabel(_ context.Context, _, _ int64, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	f.labelAdds = append(f.labelAdds, label)
	return nil
}

func (f *noticeForge) ListIssueNotes(context.Context, int64, int64) ([]glab.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failList != nil {
		return nil, f.failList
	}
	return append([]glab.Note(nil), f.thread...), nil
}

func (f *noticeForge) snapshot() (creates, edits int, labels []string, thread []glab.Note) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.edits, append([]string(nil), f.labelAdds...), append([]glab.Note(nil), f.thread...)
}

func denyMeter(reason, detail string) *fakeMeter {
	return &fakeMeter{resp: meterapi.DecideResponse{Decision: meterapi.DecisionDeny, Reason: reason, Detail: detail, Attempt: 3}}
}

func deferMeter() *fakeMeter {
	return &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionDefer, Reason: meterapi.ReasonQuietHours, Attempt: 1,
		RetryAfter: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}}
}

func noticeDeps(t *testing.T, fm *fakeMeter, store beadstore.Store, forge *noticeForge, log *bytes.Buffer) dispatchDeps {
	d := liveDispatchDeps(t, newTestRuntime(), fm, store, log)
	d.Apply, d.GL, d.BotUsername = forge, forge, "gonk-bot"
	return d
}

func TestDenyPostsOneNoteAndTheDenyLabel(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	var log bytes.Buffer
	d := noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "2 rungs, 2 unparseable batches"), store, forge, &log)
	if code := runDispatch(context.Background(), d); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	creates, edits, labels, thread := forge.snapshot()
	if creates != 1 || edits != 0 || len(thread) != 1 {
		t.Fatalf("creates=%d edits=%d thread=%d, want exactly one created note", creates, edits, len(thread))
	}
	if len(labels) != 1 || labels[0] != intake.DefaultDenyLabel {
		t.Fatalf("label adds = %v, want exactly [%s]", labels, intake.DefaultDenyLabel)
	}
	body := thread[0].Body
	for _, want := range []string{"need a human", "Every model rung", "2 rungs, 2 unparseable batches", statusMarker("gk-1a2b")} {
		if !strings.Contains(body, want) {
			t.Fatalf("note lacks %q:\n%s", want, body)
		}
	}
	rec, _, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if rec.State != beadstore.StateNeedsHuman {
		t.Fatalf("state = %q", rec.State)
	}
}

func TestSecondDenyEditsTheNoteAndTheLabelAddIsIdempotent(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	var log bytes.Buffer
	for _, detail := range []string{"first detail", "second detail"} {
		d := noticeDeps(t, denyMeter(meterapi.ReasonInfraRetriesExhausted, detail), store, forge, &log)
		if code := runDispatch(context.Background(), d); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log.String())
		}
	}
	creates, edits, labels, thread := forge.snapshot()
	if creates != 1 || edits != 1 || len(thread) != 1 {
		t.Fatalf("creates=%d edits=%d thread=%d, want ONE note created then edited", creates, edits, len(thread))
	}
	if !strings.Contains(thread[0].Body, "second detail") || strings.Contains(thread[0].Body, "first detail") {
		t.Fatalf("note not edited to the current detail:\n%s", thread[0].Body)
	}
	for _, l := range labels {
		if l != intake.DefaultDenyLabel {
			t.Fatalf("unexpected label %q", l)
		}
	}
}

func TestDeferThenDenyEditsTheSameNoteFromParkedToDenied(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	var log bytes.Buffer
	if code := runDispatch(context.Background(), noticeDeps(t, deferMeter(), store, forge, &log)); code != 0 {
		t.Fatalf("defer exit = %d\n%s", code, log.String())
	}
	_, _, _, thread := forge.snapshot()
	if len(thread) != 1 || !strings.Contains(thread[0].Body, "cannot answer yet") {
		t.Fatalf("defer left %+v", thread)
	}
	id := thread[0].ID
	if code := runDispatch(context.Background(), noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "d"), store, forge, &log)); code != 0 {
		t.Fatalf("deny exit = %d\n%s", code, log.String())
	}
	creates, edits, _, thread := forge.snapshot()
	if creates != 1 || edits != 1 || len(thread) != 1 || thread[0].ID != id {
		t.Fatalf("creates=%d edits=%d thread=%+v, want the SAME note edited", creates, edits, thread)
	}
	if strings.Contains(thread[0].Body, "cannot answer yet") || !strings.Contains(thread[0].Body, "need a human") {
		t.Fatalf("note not rewritten from parked to denied:\n%s", thread[0].Body)
	}
}

func TestDenyGitLabFailureWarnsAndKeepsTheRecord(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	forge.failAll = errors.New("gitlab 503")
	var log bytes.Buffer
	d := noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "x"), store, forge, &log)
	if code := runDispatch(context.Background(), d); code != 0 {
		t.Fatalf("exit = %d, want 0 -- a failed courtesy note must not change the outcome\n%s", code, log.String())
	}
	rec, found, _ := store.Get(context.Background(), "gonk:42:issue:3")
	if !found || rec.State != beadstore.StateNeedsHuman {
		t.Fatalf("record = %+v found=%v, want needs-human intact", rec, found)
	}
	out := log.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "gitlab 503") {
		t.Fatalf("no WARN carrying the error:\n%s", out)
	}
	if !strings.Contains(out, "deny label add failed") || !strings.Contains(out, "status note create failed") {
		t.Fatalf("both the label and the note failure should be reported:\n%s", out)
	}
}

// The notice is posted AFTER the record write: if the write is refused, the
// issue must not be told "a human is needed" about a bead that is not so marked.
func TestDenyNoticeIsNotPostedWhenTheRecordWriteFails(t *testing.T) {
	forge := newNoticeForge()
	var log bytes.Buffer
	d := noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "x"), denyWriteFailStore{beadstore.NewMemory()}, forge, &log)
	if code := runDispatch(context.Background(), d); code == 0 {
		t.Fatalf("exit = 0 with a failing record write\n%s", log.String())
	}
	creates, edits, labels, _ := forge.snapshot()
	if creates+edits+len(labels) != 0 {
		t.Fatalf("notice written before/without the record: creates=%d edits=%d labels=%v", creates, edits, labels)
	}
}

// A bead with no issue thread (scaffold, MR-anchored) is denied with the record
// only: nothing is posted anywhere.
func TestDenyWithNoIssueWritesOnlyTheRecord(t *testing.T) {
	store, forge := beadstore.NewMemory(), newNoticeForge()
	var log bytes.Buffer
	d := noticeDeps(t, denyMeter(meterapi.ReasonLadderExhausted, "x"), store, forge, &log)
	d.Args.IssueIID = 0
	if code := runDispatch(context.Background(), d); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log.String())
	}
	creates, edits, labels, _ := forge.snapshot()
	if creates+edits+len(labels) != 0 {
		t.Fatalf("wrote to GitLab with no issue: %d %d %v", creates, edits, labels)
	}
	if !strings.Contains(log.String(), "no issue to tell") {
		t.Fatalf("no log line saying why:\n%s", log.String())
	}
}

func TestDeniedBodyReasonText(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{meterapi.ReasonLadderExhausted, "Every model rung I am allowed to use has been tried"},
		{meterapi.ReasonInfraRetriesExhausted, "infrastructure failures"},
		{meterapi.ReasonActionNotAllowed, "does not allow me to take this action"},
		{"brand-new-code", "reason code: brand-new-code"},
		{"", "gave no reason code"},
	} {
		if got := deniedBody(tc.reason, "", "gonk-bot"); !strings.Contains(got, tc.want) {
			t.Errorf("reason %q: body lacks %q:\n%s", tc.reason, tc.want, got)
		}
	}
}

// denyWriteFailStore refuses every write, as a down record store would.
type denyWriteFailStore struct{ beadstore.Store }

func (denyWriteFailStore) Put(context.Context, beadstore.Record) error {
	return errors.New("store down")
}
func (denyWriteFailStore) PutIf(context.Context, beadstore.Record, string) (string, error) {
	return "", errors.New("store down")
}

// Both embedded halves declare ListMergeRequests; this path never calls it.
func (f *noticeForge) ListMergeRequests(context.Context, int64, glab.MRListOptions) ([]glab.MergeRequest, error) {
	return nil, nil
}
