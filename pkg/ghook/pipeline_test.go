package ghook

import (
	"testing"
)

// gonk-tios P3: the Pipeline Hook. Every fixture under testdata/pipeline-*.json
// is SYNTHETIC, written from GitLab's documented Pipeline Hook schema for a
// made-up project (group/repo, id 42). None is copied from a real delivery.

func TestParsePipelineFixtures(t *testing.T) {
	for _, tc := range []struct {
		fixture             string
		id                  int64
		ref, status, source string
		tag                 bool
		mrIID, mrSourceProj int64
		userID              int64
		sha                 string
	}{
		{"pipeline-failed-default", 5001, "main", "failed", "push", false, 0, 0, 9, "1111111111111111111111111111111111111111"},
		{"pipeline-failed-mr-event", 5002, "refs/merge-requests/8/head", "failed", "merge_request_event", false, 8, 42, 9, "2222222222222222222222222222222222222222"},
		{"pipeline-failed-branch", 5003, "feature-y", "failed", "push", false, 0, 0, 9, "3333333333333333333333333333333333333333"},
		{"pipeline-success", 5004, "main", "success", "push", false, 0, 0, 9, "1111111111111111111111111111111111111111"},
		{"pipeline-running", 5005, "main", "running", "push", false, 0, 0, 9, "1111111111111111111111111111111111111111"},
		{"pipeline-child", 5006, "main", "failed", "parent_pipeline", false, 0, 0, 9, "1111111111111111111111111111111111111111"},
		{"pipeline-tag", 5007, "v1.2.3", "failed", "push", true, 0, 0, 9, "1111111111111111111111111111111111111111"},
		{"pipeline-bot-triggered", 5008, "gonk/scaffold", "failed", "push", false, 0, 0, 7, "4444444444444444444444444444444444444444"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			ev, err := ParseEvent("Pipeline Hook", fixture(t, tc.fixture))
			if err != nil {
				t.Fatalf("ParseEvent = %v", err)
			}
			if ev.Kind != KindPipeline || ev.Project.ID != 42 || ev.Project.DefaultBranch != "main" {
				t.Fatalf("event = %+v", ev)
			}
			if ev.Issue != nil || ev.Note != nil || ev.MergeRequest != nil {
				t.Fatalf("a pipeline event must carry no issue/note/MR object (callers nil-check them): %+v", ev)
			}
			p := ev.Pipeline
			if p == nil {
				t.Fatal("Pipeline is nil")
			}
			if p.ID != tc.id || p.Ref != tc.ref || p.Status != tc.status || p.Source != tc.source ||
				p.Tag != tc.tag || p.MRIID != tc.mrIID || p.MRSourceProjectID != tc.mrSourceProj || p.SHA != tc.sha {
				t.Fatalf("pipeline = %+v, want %+v", *p, tc)
			}
			if ev.User.ID != tc.userID {
				t.Fatalf("user = %d, want %d (the bot loop guard reads this)", ev.User.ID, tc.userID)
			}
		})
	}
}

func TestParsePipelineRejects(t *testing.T) {
	cases := map[string]struct{ header, body string }{
		// header/object_kind mismatch, both directions
		"pipeline header, issue body": {"Pipeline Hook", `{"object_kind":"issue","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"iid":1,"action":"open"}}`},
		"issue header, pipeline body": {"Issue Hook", `{"object_kind":"pipeline","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"id":5,"ref":"main","status":"failed"}}`},
		"job header, build body":      {"Job Hook", `{"object_kind":"build","project":{"id":1,"path_with_namespace":"a/b"}}`},
		"pipeline without id":         {"Pipeline Hook", `{"object_kind":"pipeline","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"ref":"main","status":"failed"}}`},
		"pipeline without ref":        {"Pipeline Hook", `{"object_kind":"pipeline","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"id":5,"status":"failed"}}`},
		"pipeline without status":     {"Pipeline Hook", `{"object_kind":"pipeline","project":{"id":1,"path_with_namespace":"a/b"},"object_attributes":{"id":5,"ref":"main"}}`},
	}
	for name, c := range cases {
		if _, err := ParseEvent(c.header, []byte(c.body)); err == nil {
			t.Errorf("%s: ParseEvent accepted %q", name, c.body)
		}
	}
}

// A pipeline fires one hook per status change. Without the status in the
// derived key, `running` then `failed` would dedupe into one, and the FAILED
// delivery -- the only one that is work -- would be the one dropped.
func TestPipelineDedupeKeyIncludesStatus(t *testing.T) {
	failed, err := ParseEvent("Pipeline Hook", fixture(t, "pipeline-failed-default"))
	if err != nil {
		t.Fatal(err)
	}
	running := *failed
	pl := *failed.Pipeline
	pl.Status = "running"
	running.Pipeline = &pl
	if DedupeKey("", failed) == DedupeKey("", &running) {
		t.Fatal("running and failed deliveries of one pipeline share a derived dedupe key")
	}
	other := *failed
	pl2 := *failed.Pipeline
	pl2.ID++
	other.Pipeline = &pl2
	if DedupeKey("", failed) == DedupeKey("", &other) {
		t.Fatal("two different pipelines share a derived dedupe key")
	}
	if k1, k2 := DedupeKey("", failed), DedupeKey("", failed); k1 != k2 {
		t.Fatal("derived key is not stable")
	}
}

// The receiver now ACCEPTS a Pipeline Hook (it used to be unhandled_event).
func TestReceiverAcceptsPipelineHook(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Pipeline Hook", secretA, "application/json", string(fixture(t, "pipeline-failed-default")))
	if w.Code != 200 || len(c.events) != 1 || c.outcomes[0] != OutcomeAccepted {
		t.Fatalf("code=%d events=%d outcomes=%v", w.Code, len(c.events), c.outcomes)
	}
	if c.events[0].Pipeline == nil || c.events[0].Pipeline.ID != 5001 {
		t.Fatalf("event = %+v", c.events[0])
	}
	if c.labels[0] != "Pipeline Hook" {
		t.Fatalf("metric label = %q, want the handled-event label", c.labels[0])
	}
}

// D1: a pipeline the BOT triggered (its own gonk/onboard, gonk/scaffold
// pushes) keeps the existing loop guard and never reaches the sink.
func TestReceiverDropsBotTriggeredPipeline(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Pipeline Hook", secretA, "application/json", string(fixture(t, "pipeline-bot-triggered")))
	if w.Code != 200 || len(c.events) != 0 || c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("code=%d events=%d outcomes=%v", w.Code, len(c.events), c.outcomes)
	}
}

// D9: the default-branch landing is a BOT-AUTHORED issue labelled
// gonk::pipeline-failing. Its Issue Hook must be dropped bot_authored, or
// triage would start on gonk's own pipeline tracker.
func TestReceiverDropsBotAuthoredPipelineFailingIssue(t *testing.T) {
	c := &capture{}
	w := post(t, newHandler(t, c), "Issue Hook", secretA, "application/json", string(fixture(t, "issue-pipeline-failing-by-bot")))
	if w.Code != 200 || len(c.events) != 0 || c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("code=%d events=%d outcomes=%v", w.Code, len(c.events), c.outcomes)
	}
}
