package main

import (
	"context"
	"strings"
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi/gcapitest"
	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

// THE SECOND HALF OF THE gonk-pop3 ACCEPTANCE TEST. The first half
// (pkg/intake: TestAcceptanceMentionOn75Issue71IsFullyAccountedForInLogs)
// proves intake now says, per delivery, whether it dispatched the !71 mention
// and under which bead anchor. This proves the NEXT hop's refusal is joinable
// to it.
//
// Before this, runDispatch logged `unknown trigger; dispatching nothing` with
// the trigger and nothing else: no bead anchor, no project, no issue. So even
// with both halves in hand a reader could not prove the two lines were about
// the same comment -- which is exactly why gonk-ecn's root cause was still
// only a HYPOTHESIS after a live forensic.
//
// It also PRINTS the record (go test -v), because the deliverable here is the
// observed output.
func TestMentionReplyRefusalIsLoggedWithTheBeadAnchor(t *testing.T) {
	gc := gcapitest.New(t)
	fm := &fakeMeter{resp: meterapi.DecideResponse{
		Decision: meterapi.DecisionRun, Rung: "cheap", Attempt: 1,
		KeyRef: meterapi.KeyRef{SecretName: "gonk-key-abc", SecretKey: "LITELLM_API_KEY"},
	}}

	// The real !71 work item, as intake would have sent it.
	args := baseDispatchArgs()
	args.Trigger = "mention-reply"
	args.Project, args.ProjectID, args.IssueIID = "group/sortlib", 75, 71
	args.BeadAnchor, args.SessionKey = "gonk:75:issue:71", "gonk-75-issue-71"

	var logged strings.Builder
	code := runDispatch(context.Background(), dispatchDeps{
		Keys:  readerWith(secret("gonk-key-abc", "LITELLM_API_KEY", "sk-project-abc")),
		Meter: meterClient(fm.server(t)), GC: gc.Client("gonk-city"), Store: beadstore.NewMemory(),
		Args: args,
		Log:  testLogger(&logged),
	})

	t.Logf("\n--- what `kubectl logs deploy/gonk-controller` now shows for this order ---\n%s", logged.String())

	if code != 2 {
		t.Fatalf("exit = %d, want 2 (an unrouted trigger)", code)
	}
	// The join key. Without it this line cannot be tied to intake's record.
	for _, want := range []string{
		`bead=gonk:75:issue:71`,
		`trigger=mention-reply`,
		`project=group/sortlib`,
		`issue_iid=71`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("refusal record is missing %s:\n%s", want, logged.String())
		}
	}
	// And it must say what the alternative was, so the line diagnoses itself
	// instead of sending a reader to go read a map in broker_inject.go.
	if !strings.Contains(logged.String(), "routable_triggers=") {
		t.Errorf("refusal record does not name the triggers that DO route:\n%s", logged.String())
	}
	for _, want := range []string{"issue-triage", "scaffold"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("routable_triggers does not list %q:\n%s", want, logged.String())
		}
	}
}

// routableTriggers is what the refusal line reads from. It must be sorted, so
// the log line is stable across runs (a map range order would make two
// identical refusals look different).
func TestRoutableTriggersIsStableAndComplete(t *testing.T) {
	got := routableTriggers()
	if len(got) != len(agentForTrigger) {
		t.Fatalf("routableTriggers() = %v, but agentForTrigger has %d entries", got, len(agentForTrigger))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("routableTriggers() is not sorted: %v", got)
		}
	}
	for _, k := range got {
		if _, ok := agentForTrigger[k]; !ok {
			t.Fatalf("routableTriggers() returned %q, which agentForTrigger does not route", k)
		}
	}
}
