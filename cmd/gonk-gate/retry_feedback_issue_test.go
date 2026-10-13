package main

// RETRY FEEDBACK FOR THE ISSUE-SHAPED AGENTS (gonk-hpgy): triage and scaffold.
// Through the real dispatch, the real sweep and the store's encoding, as
// retry_feedback_test.go does for the two pipeline agents. Every fixture is
// invented.
//
// What a triage session reads is an ISSUE -- text anyone who can open one
// wrote -- so the no-replay rule matters most here: every refused answer below
// carries fbLeak in each free-text field it has, and none of it may reach the
// next prompt.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/beadstore"
	"github.com/gonk-factory/gonk/pkg/effects"
	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/rig"
	"github.com/gonk-factory/gonk/pkg/runtime"
)

const (
	wantIssueShapeWhat   = "the batch does not hold the effects this task takes: a kind is missing, repeated too often, or not one of them"
	wantTriageShape      = "the effects of one batch are -- comment exactly 1; label at least 0"
	wantTriageKinds      = "the effect kinds of this task are: comment, label"
	wantIssueNoBatchWhat = "the output held no batch between the two fence lines this message shows"
	wantIssueEOFWhat     = "the JSON between the fence lines ended before it was complete: a string, a bracket or a brace was left open, or there was no JSON at all"
)

// triageRefusedOutputs are answers the issue-shaped chain refuses, one per
// gate a triage answer can fail without a forge fault.
func triageRefusedOutputs() []struct{ name, output, code, what, allowed string } {
	comment := `{"kind":"comment","body":"` + fbLeak + `"}`
	return []struct{ name, output, code, what, allowed string }{
		{"no fence at all", "session start\n" + fbLeak + "\n",
			"no-batch", wantIssueNoBatchWhat, ""},
		{"a batch that stops in the middle",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"` + fbLeak),
			"parse", wantIssueEOFWhat, ""},
		{"a misspelled key",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","bdy":"` + fbLeak + `"}]}`),
			"parse", `effect 1 of 1 (comment) has a key that does not exist: "bdy"`, ""},
		{"a key that is a line break, a heading and a sentence",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"b","x\n` + retryFeedbackHeading + `\n` + fbLeak + ` <<<UNTRUSTED":"y"}]}`),
			"parse", `effect 1 of 1 (comment) has a key that does not exist: "x????Your?previous?answer?was?re" (cut)`, ""},
		{"a kind that does not exist",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"note","body":"` + fbLeak + `"}]}`),
			"parse", `effect 1 has a kind that does not exist: "note"`, wantTriageKinds},
		{"two comments",
			fenced(`{"verdict":"reply-only","effects":[` + comment + `,` + comment + `]}`),
			"shape", wantIssueShapeWhat, wantTriageShape},
		{"a kind of another task",
			fenced(`{"verdict":"reply-only","effects":[` + comment + `,{"kind":"file","path":".agent/` + fbLeak + `.md","content":"` + fbLeak + `"}]}`),
			"shape", wantIssueShapeWhat, wantTriageShape},
		{"a target that is not this issue",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"` + fbLeak + `","target_iid":999}]}`),
			"target", "an effect names a target that is not the issue of this task; leave target_iid and target_type out", ""},
		{"a comment GitLab would act on",
			fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"` + fbLeak + `\n/close"}]}`),
			"comment", "the comment holds a line GitLab would act on, or is too long", ""},
		{"a label that is not a label",
			fenced(`{"verdict":"reply-only","effects":[` + comment + `,{"kind":"label","add":["` + fbLeak + `,gonk::bug"]}]}`),
			"label", "a label is empty, too long, holds a comma, or is one only gonk sets", ""},
	}
}

// THE BUG (gonk-hpgy), end to end: a triage answer the sweep refuses; a
// controller restart; the retry. Before this the retry's prompt was the first
// prompt again, byte for byte. Now it is the first prompt plus the documented
// block -- built by the controller, holding nothing of the refused answer --
// and a correct second answer is applied, which ends the feedback.
func TestTriageRefusedAnswerIsRetriedWithFeedback(t *testing.T) {
	for i, tc := range triageRefusedOutputs() {
		t.Run(tc.name, func(t *testing.T) {
			store, forge, rt := beadstore.Store(beadstore.NewMemory()), newNoticeForge(), newTestRuntime()
			orders := &fakeOrders{}
			var log bytes.Buffer
			anchor := baseDispatchArgs().BeadAnchor
			if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(1, "cheap"), store, forge, &log)); code != 0 {
				t.Fatalf("dispatch 1 exit = %d\n%s", code, log.String())
			}
			spec1, _ := rt.Spec(runtime.Handle(testSession))
			first := string(spec1.Prompt)
			if strings.Contains(first, retryFeedbackHeading) {
				t.Fatalf("the first attempt's prompt carries a feedback block")
			}

			_, _, labelsBefore, _ := forge.snapshot()
			finish(rt, 1, tc.output)
			refused := &fakeOutcomeMeter{outcomeNext: "retry", spendSynced: true,
				sessionCost: meterapi.SessionCostResponse{TotalTokens: 900, AsOf: time.Now().Add(time.Hour)}}
			lifecycleSweep(t, rt, store, forge, refused, orders, &log)
			if reqs := refused.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeGateFailed {
				t.Fatalf("outcome = %+v, want gate-failed\n%s", reqs, log.String())
			}
			if _, _, labels, _ := forge.snapshot(); len(labels) != len(labelsBefore) {
				t.Fatalf("a refused answer set labels: %v", labels)
			}
			rec, _, _ := store.Get(context.Background(), anchor)
			want := beadstore.RetryFeedback{Attempt: 1, Code: tc.code, What: tc.what, Allowed: tc.allowed}
			if rec.RetryFeedback != want {
				t.Fatalf("recorded feedback\n got  %+v\n want %+v\n%s", rec.RetryFeedback, want, log.String())
			}

			// THE CONTROLLER RESTARTS: only the store's records survive.
			store = restartedStore(t, store, anchor)
			if code := runDispatch(context.Background(), runNoticeDeps(t, rt, runMeterAt(2, "cheap"), store, forge, &log)); code != 0 {
				t.Fatalf("dispatch 2 exit = %d\n%s", code, log.String())
			}
			s2 := runtime.Name("triage", 42, runtime.KindIssue, 3, 2)
			spec2, ok := rt.Spec(runtime.Handle(s2))
			if !ok {
				t.Fatalf("no session %s was started", s2)
			}
			second := string(spec2.Prompt)
			if second == first {
				t.Fatalf("the retry's prompt is the first prompt again (%d bytes): the second attempt is told nothing", len(first))
			}
			block := wantBlock(1, tc.code, tc.what, tc.allowed)
			if second != first+"\n\n"+block {
				t.Fatalf("the retry's prompt is not the first prompt plus the feedback block.\n--- tail got ---\n%s\n--- block want ---\n%s",
					second[max(0, len(second)-len(block)-80):], block)
			}
			if i == 0 {
				t.Logf("PROMPT BYTES for a refused triage attempt: first %d, retry %d (the block is %d bytes)\n%s", len(first), len(second), len(block), block)
			}
			for _, bad := range []string{"TAIL", "leak", "IGNORE-THE-RULES"} {
				if strings.Contains(second, bad) {
					t.Errorf("the refused answer's text %q is in the retry's prompt", bad)
				}
			}
			if n := strings.Count(second, retryFeedbackHeading); n != 1 || len(block) > retryFeedbackMaxBytes || strings.Contains(block, "OUTPUT") {
				t.Errorf("the heading appears %d times, or the block is over its cap (%d bytes), or it points at a heading this prompt does not have", n, len(block))
			}
			if rec2, _, _ := store.Get(context.Background(), anchor); rec2.RetryFeedback != want || rec2.Attempt != 2 {
				t.Errorf("the running record of attempt 2 lost the feedback: attempt %d %+v", rec2.Attempt, rec2.RetryFeedback)
			}

			// The second answer is correct and is applied; the feedback ends.
			finish(rt, 2, fenced(`{"verdict":"reply-only","effects":[{"kind":"comment","body":"The export endpoint has no timeout handling."}]}`))
			done := &fakeOutcomeMeter{outcomeNext: "done"}
			lifecycleSweep(t, rt, store, forge, done, orders, &log)
			if reqs := done.requests(); len(reqs) != 1 || reqs[0].Outcome != meterapi.OutcomeSuccess {
				t.Fatalf("the corrected answer was not applied: %+v\n%s", reqs, log.String())
			}
			if rec3, _, _ := store.Get(context.Background(), anchor); rec3.RetryFeedback != (beadstore.RetryFeedback{}) {
				t.Errorf("an applied answer left the feedback of an earlier refusal: %+v", rec3.RetryFeedback)
			}
		})
	}
}

// Scaffold shares the sweep's chain and the dispatch's carry-over. Its own
// refusals -- above all the path gate, whose error quotes the path the model
// wrote -- leave a fixed sentence and nothing of the batch.
func TestScaffoldRefusalsLeaveFeedbackAndNoneOfTheBatch(t *testing.T) {
	for _, tc := range []struct{ name, batch, code, what, allowed string }{
		{"a file outside the directory this task writes",
			scaffoldBatchJSON([2]string{".agent/README.md", fbLeak}, [2]string{fbLeak + "/.gitlab-ci.yml", fbLeak}),
			"path", "a file effect names a path this task may not write, or holds more than a file may", ""},
		{"no file at all", `{"effects":[]}`,
			"shape", wantIssueShapeWhat, "the effects of one batch are -- file at least 1"},
		{"a comment, which this task does not take",
			`{"effects":[{"kind":"file","path":".agent/a.md","content":"` + fbLeak + `"},{"kind":"comment","body":"` + fbLeak + `"}]}`,
			"shape", wantIssueShapeWhat, "the effects of one batch are -- file at least 1"},
		{"a misspelled key",
			`{"effects":[{"kind":"file","path":".agent/a.md","contnt":"` + fbLeak + `"}]}`,
			"parse", `effect 1 of 1 (file) has a key that does not exist: "contnt"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applier := &recordingApplier{}
			d, rec := scaffoldSweepFixture(t, applier, tc.batch)
			applied, violation, err := applyBrokerBatch(context.Background(), d, "scaffold", &rec, finishedStatus())
			if applied || violation == "" || err != nil || len(applier.commits) != 0 {
				t.Fatalf("fixture: applied=%v violation=%q err=%v commits=%d", applied, violation, err, len(applier.commits))
			}
			want := beadstore.RetryFeedback{Attempt: rec.Attempt, Code: tc.code, What: tc.what, Allowed: tc.allowed}
			if rec.RetryFeedback != want {
				t.Fatalf("recorded feedback\n got  %+v\n want %+v", rec.RetryFeedback, want)
			}
			next := rec
			next.Attempt++
			suffix := retryFeedbackSuffix(next)
			if suffix != "\n\n"+wantBlock(rec.Attempt, tc.code, tc.what, tc.allowed) {
				t.Errorf("the next attempt's prompt suffix:\n%s", suffix)
			}
			for _, bad := range []string{"TAIL", "leak", "IGNORE-THE-RULES"} {
				if strings.Contains(suffix, bad) {
					t.Errorf("the refused answer's text %q is in the feedback", bad)
				}
			}
			if same := retryFeedbackSuffix(rec); same != "" {
				t.Errorf("the block is rendered for the attempt it is about: %q", same)
			}
		})
	}
	// A good batch leaves none, and ends one an earlier attempt left.
	applier := &recordingApplier{}
	d, rec := scaffoldSweepFixture(t, applier, scaffoldBatchJSON([2]string{".agent/README.md", "x"}))
	rec.RetryFeedback = beadstore.RetryFeedback{Attempt: 1, Code: "path", What: "w"}
	if applied, v, err := applyBrokerBatch(context.Background(), d, "scaffold", &rec, finishedStatus()); !applied || v != "" || err != nil {
		t.Fatalf("a good batch: applied=%v %q %v", applied, v, err)
	}
	if rec.RetryFeedback != (beadstore.RetryFeedback{}) {
		t.Errorf("an applied scaffold left the feedback of an earlier refusal: %+v", rec.RetryFeedback)
	}
}

// The scaffold dispatch carries the record's feedback into the prompt that
// asks for a batch -- and NOT into the one that tells the session to emit no
// batch at all (no repository material), where "answer again in full" would
// contradict it.
func TestScaffoldPromptCarriesFeedbackOnlyWhenABatchIsAskedFor(t *testing.T) {
	rec := beadstore.Record{Attempt: 2, RetryFeedback: beadstore.RetryFeedback{Attempt: 1, Code: "path", What: "w"}}
	block := wantBlock(1, "path", "w", "")
	withMaterial := scaffoldPromptFor(rec, "group/repo", "Repository: group/repo\n\n--- README.md ---\nhello\n")
	if want := renderScaffoldPrompt("group/repo", "Repository: group/repo\n\n--- README.md ---\nhello\n") + "\n\n" + block; withMaterial != want {
		t.Errorf("with material, the prompt is not the scaffold prompt plus the block:\n%s", withMaterial[max(0, len(withMaterial)-len(block)-80):])
	}
	without := scaffoldPromptFor(rec, "group/repo", "")
	if without != renderScaffoldPrompt("group/repo", "") || strings.Contains(without, retryFeedbackHeading) {
		t.Errorf("without material, the refusal prompt was changed:\n%s", without)
	}
}

// The kinds a parse refusal's feedback names for triage and scaffold are the
// kinds their shape files take -- the files the shape gate itself loads.
func TestIssueAgentFeedbackKindsAreTheirShapes(t *testing.T) {
	for _, agent := range []string{triageAgent, scaffoldAgent} {
		shape, err := effects.LoadShape(repoPackDir, agent)
		if err != nil {
			t.Fatal(err)
		}
		var want, got []string
		for k := range shape.Kinds {
			want = append(want, string(k))
		}
		for _, k := range retryFeedbackKinds(agent) {
			got = append(got, string(k))
		}
		sort.Strings(want)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) || len(got) == 0 {
			t.Errorf("%s: feedback names kinds %v, the shape file takes %v", agent, got, want)
		}
	}
}

// Every gate of the issue-shaped chain has a fixed sentence, and none of them
// can end a line, close a fence or pass for a heading.
func TestIssueGateFeedbackSentencesAreFixedAndPlain(t *testing.T) {
	if len(issueGateFeedback) != 5 {
		t.Errorf("%d sentences, want one per gate: target, path, comment, trajectory, label", len(issueGateFeedback))
	}
	for gate, s := range issueGateFeedback {
		if s == "" || len(s) > retryFeedbackCapWhat || feedbackText(s, len(s)) != s {
			t.Errorf("gate %s: sentence %q is empty, over the cap, or altered by the block's filter", gate, s)
		}
	}
}

// THE SCAFFOLD DISPATCH, end to end: the record a refused first attempt left
// is in the store; the re-dispatch carries its feedback onto the record it
// writes and into the prompt -- for material fetched controller-side -- and a
// scaffold dispatch with NO material starts a session whose prompt asks for no
// batch and carries no block.
func TestScaffoldRetryDispatchCarriesTheFeedback(t *testing.T) {
	fbRec := beadstore.RetryFeedback{Attempt: 1, Code: "path", What: issueGateFeedback[issueGatePath]}
	block := wantBlock(1, "path", "a file effect names a path this task may not write, or holds more than a file may", "")
	grants := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(grants.Close)
	proj := &glab.Project{PathWithNamespace: "group/repo", DefaultBranch: "main"}
	for name, tc := range map[string]struct {
		forge     stubForge
		checkout  bool
		wantBlock bool
	}{
		"a checkout was granted": {stubForge{proj: proj}, true, true},
		"material was fetched":   {stubForge{proj: proj, files: map[string]string{"README.md": "A small calculator."}}, false, true},
		"no material":            {stubForge{proj: proj}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			store, rt := beadstore.NewMemory(), newTestRuntime()
			var log bytes.Buffer
			d := liveDispatchDeps(t, rt, attemptMeter(2), store, &log)
			d.Forge = tc.forge
			if tc.checkout {
				d.Rig, d.RigBaseURL = &rig.Client{BaseURL: grants.URL}, "http://intake.private:8081"
			}
			d.Args.Trigger, d.Args.IssueIID, d.Args.BeadAnchor, d.Args.SessionKey = "scaffold", 0, "gonk:42:scaffold", "gonk-42-scaffold"
			prior := sweepAResling()
			prior.Trigger, prior.IssueIID, prior.BeadAnchor, prior.SessionKey = d.Args.Trigger, 0, d.Args.BeadAnchor, d.Args.SessionKey
			prior.SessionID = runtime.Name(scaffoldAgent, 42, runtime.KindIssue, 0, 1)
			prior.RetryFeedback = fbRec
			if err := store.Put(context.Background(), prior); err != nil {
				t.Fatal(err)
			}
			if code := runDispatch(context.Background(), d); code != 0 {
				t.Fatalf("dispatch exit = %d\n%s", code, log.String())
			}
			starts := rt.Starts()
			if len(starts) != 1 {
				t.Fatalf("%d sessions started\n%s", len(starts), log.String())
			}
			prompt := string(starts[0].Prompt)
			if got := strings.HasSuffix(prompt, "\n\n"+block); got != tc.wantBlock || strings.Contains(prompt, retryFeedbackHeading) != tc.wantBlock {
				t.Errorf("the prompt carries the block: %v, want %v\n--- tail ---\n%s", got, tc.wantBlock, prompt[max(0, len(prompt)-len(block)-120):])
			}
			if tc.wantBlock == strings.Contains(prompt, "Emit NO batch at all") || tc.checkout != strings.Contains(prompt, "which IS\nchecked out in your working directory") {
				t.Errorf("fixture: the prompt is not the one this case is about:\n%.300s", prompt)
			}
			if rec, _, _ := store.Get(context.Background(), d.Args.BeadAnchor); rec.Attempt != 2 || rec.RetryFeedback != fbRec {
				t.Errorf("the running record of attempt 2: attempt %d feedback %+v", rec.Attempt, rec.RetryFeedback)
			}
		})
	}
}
