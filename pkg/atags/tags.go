// Package atags is the attribution-tags contract: the metadata attached to
// every LiteLLM request so gonk-meter can join spend to beads, sessions,
// and GitLab artifacts (spec 6.1). Key names are part of the public
// contract; changing them is a breaking change to the ledger.
package atags

import (
	"fmt"
	"strconv"
)

const (
	TriggerIssueTriage  = "issue-triage"
	TriggerOnboarding   = "onboarding"
	TriggerScaffold     = "scaffold"
	TriggerMentionReply = "mention-reply"
)

var validTriggers = map[string]struct{}{
	TriggerIssueTriage: {}, TriggerOnboarding: {},
	TriggerScaffold: {}, TriggerMentionReply: {},
}

// Metadata key names as they appear in LiteLLM spend logs.
const (
	KeyProject    = "gonk_project"
	KeyRig        = "gonk_rig"
	KeyBeadID     = "gonk_bead_id"
	KeySessionKey = "gonk_session_key"
	KeyRung       = "gonk_rung"
	KeyAttempt    = "gonk_attempt"
	KeyTrigger    = "gonk_trigger"
)

type Tags struct {
	Project    string // GitLab path_with_namespace
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int // 1-based
	Trigger    string
}

func (t Tags) Validate() error {
	for name, v := range map[string]string{
		"project": t.Project, "rig": t.Rig, "bead_id": t.BeadID,
		"session_key": t.SessionKey, "rung": t.Rung,
	} {
		if v == "" {
			return fmt.Errorf("atags: %s empty", name)
		}
	}
	if t.Attempt < 1 {
		return fmt.Errorf("atags: attempt %d < 1", t.Attempt)
	}
	if _, ok := validTriggers[t.Trigger]; !ok {
		return fmt.Errorf("atags: unknown trigger %q", t.Trigger)
	}
	return nil
}

func (t Tags) Metadata() map[string]string {
	return map[string]string{
		KeyProject: t.Project, KeyRig: t.Rig, KeyBeadID: t.BeadID,
		KeySessionKey: t.SessionKey, KeyRung: t.Rung,
		KeyAttempt: strconv.Itoa(t.Attempt), KeyTrigger: t.Trigger,
	}
}

func FromMetadata(m map[string]string) (Tags, error) {
	attempt, err := strconv.Atoi(m[KeyAttempt])
	if err != nil {
		return Tags{}, fmt.Errorf("atags: attempt: %w", err)
	}
	t := Tags{
		Project: m[KeyProject], Rig: m[KeyRig], BeadID: m[KeyBeadID],
		SessionKey: m[KeySessionKey], Rung: m[KeyRung],
		Attempt: attempt, Trigger: m[KeyTrigger],
	}
	return t, t.Validate()
}
