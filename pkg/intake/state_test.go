package intake

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/pkg/meterapi"
)

const goodConfig = `
version: 1
enabled: true
actions: { triage: true }
ladder: [qwen-local]
`

func obs(mut func(*Observation)) Observation {
	o := Observation{
		Member:          true,
		ConfigBytes:     []byte(goodConfig),
		AgentDirPresent: true,
	}
	if mut != nil {
		mut(&o)
	}
	return o
}

// active is what meter returns for a healthy project. NOTE we build a
// meterapi.Effective here, NOT a gonkcfg.Effective -- intake never holds the
// latter, because intake never calls Resolve (ADR-002 says Resolve's return
// value is the only legitimate way to produce one, and intake has no business
// producing one).
func active(mut func(*meterapi.ProjectResponse)) *meterapi.ProjectResponse {
	r := &meterapi.ProjectResponse{
		Project: "group/repo", Rig: "group-repo",
		State: meterapi.StateActive,
		Effective: &meterapi.Effective{
			Enabled: true,
			Actions: meterapi.Actions{Triage: true},
			Ladder:  []string{"qwen-local"},
			Triage:  meterapi.Triage{LabelPrefix: "gonk::", RespondToMentions: true},
		},
		KeyRef:     meterapi.KeyRef{SecretName: "gonk-key-x", SecretKey: "LITELLM_API_KEY"},
		ConfigHash: "sha256:abc",
	}
	if mut != nil {
		mut(r)
	}
	return r
}

func TestClassifyValid(t *testing.T) {
	c := Classify(obs(nil), active(nil))
	if c.State != StateValid {
		t.Fatalf("state = %q (%s)", c.State, c.Reason)
	}
	if !c.MayTriage() || c.MayScaffold() {
		t.Fatalf("valid project: MayTriage=%v MayScaffold=%v", c.MayTriage(), c.MayScaffold())
	}
	if c.ConfigHash == "" {
		t.Fatal("valid config must have a hash (the order records which config authorized it)")
	}
}

func TestClassifyPendingUntilAgentDirExists(t *testing.T) {
	c := Classify(obs(func(o *Observation) { o.AgentDirPresent = false }), active(nil))
	if c.State != StatePending {
		t.Fatalf("state = %q", c.State)
	}
	// spec 5.3: no LLM actions while pending EXCEPT the scaffold MR itself.
	if c.MayTriage() {
		t.Fatal("triage must not run while a project is pending")
	}
	if !c.MayScaffold() {
		t.Fatal("pending is exactly the state where the scaffold MR is authorized")
	}
}

func TestClassifyAbsentAndDeclined(t *testing.T) {
	// No config at all: meter was never called, so there is no response. This is
	// NOT unsynced -- there is nothing to sync.
	c := Classify(obs(func(o *Observation) { o.ConfigBytes = nil }), nil)
	if c.State != StateAbsent {
		t.Fatalf("state = %q, want absent (onboarding candidate)", c.State)
	}
	if !c.MayOnboard() {
		t.Fatal("absent is exactly the onboarding-candidate state")
	}
	c = Classify(obs(func(o *Observation) {
		o.ConfigBytes = nil
		o.OnboardingDeclined = true
	}), nil)
	if c.State != StateDeclined || c.MayOnboard() {
		t.Fatalf("a declined project must not get another onboarding MR (spec 5.3): %+v", c)
	}
}

func TestClassifyUnmanaged(t *testing.T) {
	c := Classify(obs(func(o *Observation) { o.Member = false }), active(nil))
	if c.State != StateUnmanaged || c.MayTriage() || c.MayOnboard() {
		t.Fatalf("non-member must be inert even if meter still has a registration: %+v", c)
	}
}

// *** THE FAIL-CLOSED RULE, AND THE HEART OF CONFLICT A. ***
// We have a config. We do NOT have meter's answer -- meter is down, or this is
// the first pass. We do not know whether this project is enabled, what its
// ladder is, or whether its key exists. WE CANNOT GUESS, because we no longer
// have a resolver, and that is deliberate: guessing is what two resolvers
// disagreeing looks like.
func TestClassifyUnsyncedWhenMeterHasNotAnswered(t *testing.T) {
	c := Classify(obs(nil), nil)
	if c.State != StateUnsynced {
		t.Fatalf("state = %q, want unsynced", c.State)
	}
	if c.MayTriage() || c.MayScaffold() || c.MayOnboard() {
		t.Fatal("a project whose policy we have not resolved must authorize NOTHING (no unmetered work, ever)")
	}
	if c.Reason == "" {
		t.Fatal("unsynced must explain itself")
	}
}

// Meter's answers, recorded verbatim. Intake CANNOT compute any of these three.
func TestClassifyRecordsMetersVerdict(t *testing.T) {
	cases := map[string]struct {
		resp  *meterapi.ProjectResponse
		state State
	}{
		"invalid (422: the yaml will not load)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateInvalid
				r.Effective = nil // there is NO Effective for an invalid config (ADR-002)
				r.Error = ".gonk.yml: at '/budget/monthly_tokens': got string, want integer"
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateInvalid,
		},
		"disabled (the instance kill switch, an empty ladder, ...)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateDisabled
				r.Effective = &meterapi.Effective{Enabled: false}
				r.DisabledReason = "disabled by instance policy"
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateDisabled,
		},
		"key-missing (LiteLLM was unreachable at provisioning time)": {
			active(func(r *meterapi.ProjectResponse) {
				r.State = meterapi.StateKeyMissing
				r.KeyRef = meterapi.KeyRef{}
			}),
			StateKeyMissing,
		},
	}
	for name, c := range cases {
		got := Classify(obs(nil), c.resp)
		if got.State != c.state {
			t.Errorf("%s: state = %q, want %q", name, got.State, c.state)
		}
		if got.MayTriage() || got.MayScaffold() || got.MayOnboard() {
			t.Errorf("%s: must authorize nothing", name)
		}
		if got.Reason == "" {
			t.Errorf("%s: must explain itself (it goes in a metric label and a log line)", name)
		}
	}
}

// Intake's action check is a PRE-FILTER, not the enforcement point: meter vetoes
// at /decide, which is the only chokepoint before a session spawns. But we still
// must not fire an order we know will be denied.
func TestClassifyRespectsMetersActionVeto(t *testing.T) {
	c := Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Actions.Triage = false // a coarser layer vetoed it (ADR-002)
	}))
	if c.State != StateValid {
		t.Fatalf("state = %q; an action veto disables the ACTION, not the project (ADR-002)", c.State)
	}
	if c.MayTriage() {
		t.Fatal("triage is vetoed; do not fire an order meter will deny")
	}
}

func TestClassifyRespectsRespondToMentions(t *testing.T) {
	// respond_to_mentions is NOT an Action, so meter does not check it. Intake is
	// its only enforcement point. If we drop it here, it is enforced NOWHERE.
	c := Classify(obs(nil), active(func(r *meterapi.ProjectResponse) {
		r.Effective.Triage.RespondToMentions = false
	}))
	if !c.MayTriage() {
		t.Fatal("triage itself is still on")
	}
	if c.MayMentionReply() {
		t.Fatal("respond_to_mentions: false must suppress mention replies -- intake is the ONLY thing that checks it")
	}
}

// Intake may still ask "do these bytes parse?" for its own metrics and for the
// onboarding flow -- but it is ADVISORY. Meter's 422 is authoritative, and where
// the two disagree, METER WINS.
func TestLooksInvalidIsAdvisoryOnly(t *testing.T) {
	garbage := obs(func(o *Observation) { o.ConfigBytes = []byte("{{{{") })
	// Intake thinks it is garbage; meter (hypothetically) said active. Meter wins:
	// we do not have the operator config, so we do not get a vote.
	c := Classify(garbage, active(nil))
	if c.State != StateValid {
		t.Fatalf("state = %q; meter is the authority on validity, not intake", c.State)
	}
	// But with no answer from meter, our own read is what populates the metric.
	c = Classify(garbage, nil)
	if c.State != StateUnsynced || !c.LooksInvalid {
		t.Fatalf("unsynced + LooksInvalid expected, got %+v", c)
	}
}
