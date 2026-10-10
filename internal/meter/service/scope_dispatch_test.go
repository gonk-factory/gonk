package service

import (
	"context"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/rung"
)

// gonk-066 S3, the test that must be seen red: "invalid .gonk.yml scope denies
// at dispatch". 066 invariant 5 -- our (the project's) misconfiguration is
// refused BEFORE any spend, never discovered at judge time as the agent's
// failure.
//
// The scope section fails for a reason only pkg/scope's lattice knows
// (`docs_paths` aimed at the deploy class): the JSON Schema accepts the
// document. Registration must mark the project invalid, drop its key, and the
// next decision must be a deny with no reservation.
func TestInvalidScopeDeniesAtDispatch(t *testing.T) {
	f := newTestService(t)
	// A good config first, so there is a live key and a possible run to lose.
	f.register("group/repo", "group-repo", simpleYAML("qwen-local", 5))
	f.syncOnce()
	if d, _, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-ok", "sess-ok")); err != nil || d.Kind != rung.Run {
		t.Fatalf("setup: decide on the valid config = %+v, %v; want run", d, err)
	}

	bad := simpleYAML("qwen-local", 5) + "scope:\n  docs_paths: [\"deploy/**\"]\n"
	resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: bad,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if status != 422 || resp.State != meterapi.StateInvalid {
		t.Fatalf("status %d state %q, want 422 invalid", status, resp.State)
	}
	if !strings.Contains(resp.Error, "scope") || !strings.Contains(resp.Error, "deploy") {
		t.Errorf("Error = %q, want it to name the scope section and the deploy class", resp.Error)
	}
	if resp.Effective != nil {
		t.Error("an invalid scope left an Effective config")
	}
	if _, ok := f.admin.Keys["gonk-group-repo"]; ok {
		t.Error("the virtual key survived an invalid scope -- the project can still spend")
	}

	d, extras, err := f.svc.Decide(context.Background(), decideReq("group/repo", "gk-new", "sess-new"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonInvalidConfig {
		t.Fatalf("decision = %+v, want deny/%s", d, rung.ReasonInvalidConfig)
	}
	if extras.Reservation.ID != "" {
		t.Error("a project with an invalid scope got a reservation")
	}
}

// And a VALID scope section changes nothing about how a project registers.
func TestValidScopeRegistersAsBefore(t *testing.T) {
	f := newTestService(t)
	plain := f.register("a/plain", "a-plain", simpleYAML("qwen-local", 5))
	scoped := f.register("a/scoped", "a-scoped", simpleYAML("qwen-local", 5)+
		"scope:\n  classes: [source]\n  caps: { max_files: 1 }\n  test_paths: [\"internal/testutil/**\"]\n")
	if scoped.State != meterapi.StateActive || plain.State != meterapi.StateActive {
		t.Fatalf("states = %q / %q, want both active", plain.State, scoped.State)
	}
	if scoped.Effective == nil || plain.Effective == nil {
		t.Fatal("no Effective")
	}
	if a, b := *plain.Effective, *scoped.Effective; a.Enabled != b.Enabled || a.Actions != b.Actions || a.Continuity != b.Continuity ||
		strings.Join(a.Ladder, ",") != strings.Join(b.Ladder, ",") {
		t.Errorf("Effective differs because of a scope section:\n%+v\n%+v", a, b)
	}
}

// Ecosystem profiles (gonk-wj14). An unknown profile is refused by the project
// layer alone; a profile outside the operator's allowed_profiles takes both
// layers, and the meter -- the one place that holds both at registration --
// refuses it the same way: invalid, key deleted, every decision denied.
func TestProfileSelectionIsCheckedAtDispatch(t *testing.T) {
	restricted := strings.Replace(testOperatorYAML, "  agentic:\n", "  agentic:\n    scope: { allowed_profiles: [go, docs-md] }\n", 1)
	if restricted == testOperatorYAML {
		t.Fatal("the operator fixture has no agentic group to restrict")
	}
	f := newTestServiceWithConfig(t, restricted)
	ctx := context.Background()
	reg := func(project, scopeBody string) (meterapi.ProjectResponse, int) {
		t.Helper()
		resp, status, err := f.svc.Register(ctx, meterapi.ProjectRequest{
			Project: project, ProjectID: 1, Rig: strings.ReplaceAll(project, "/", "-"), GonkYML: simpleYAML("qwen-local", 5) + scopeBody,
		})
		if err != nil {
			t.Fatalf("Register(%s): %v", project, err)
		}
		return resp, status
	}

	// Inside the menu, and no key at all: both register.
	for project, body := range map[string]string{
		"agentic/go":      "scope:\n  profiles: [go]\n",
		"agentic/default": "",
		"agentic/none":    "scope:\n  profiles: []\n",
		// The restriction is the agentic group's; another group is not bound by it.
		"elsewhere/py": "scope:\n  profiles: [python, docs-rst]\n",
	} {
		if resp, status := reg(project, body); status != 200 || resp.State != meterapi.StateActive {
			t.Errorf("%s: status %d state %q error %q; want 200 active", project, status, resp.State, resp.Error)
		}
	}

	// Outside the menu: refused, naming the key that refused it.
	resp, status := reg("agentic/py", "scope:\n  profiles: [python]\n")
	if status != 422 || resp.State != meterapi.StateInvalid {
		t.Fatalf("a profile the operator does not allow: status %d state %q, want 422 invalid", status, resp.State)
	}
	if !strings.Contains(resp.Error, "allowed_profiles") || !strings.Contains(resp.Error, "python") || !strings.HasPrefix(resp.Error, ".gonk.yml: ") {
		t.Errorf("Error = %q; want a .gonk.yml error naming python and allowed_profiles", resp.Error)
	}
	if resp.Effective != nil {
		t.Error("a disallowed profile left an Effective config")
	}
	if _, ok := f.admin.Keys["gonk-agentic-py"]; ok {
		t.Error("the virtual key exists for a project with a disallowed profile -- it can spend")
	}
	f.syncOnce()
	d, extras, err := f.svc.Decide(ctx, decideReq("agentic/py", "gk-py", "sess-py"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonInvalidConfig || extras.Reservation.ID != "" {
		t.Fatalf("decision = %+v (reservation %q), want deny/%s with no reservation", d, extras.Reservation.ID, rung.ReasonInvalidConfig)
	}

	// An unknown profile is refused whatever the operator says.
	if resp, status := reg("elsewhere/rust", "scope:\n  profiles: [rust]\n"); status != 422 || resp.State != meterapi.StateInvalid || !strings.Contains(resp.Error, "scope") {
		t.Errorf("an unknown profile: status %d state %q error %q; want 422 invalid", status, resp.State, resp.Error)
	}
}

// The operator narrowing the menu reaches a project that is ALREADY registered:
// the reresolve loop re-checks every project against the current operator
// config, so "valid when it registered" is not a way around the restriction.
func TestNarrowingAllowedProfilesInvalidatesOnReresolve(t *testing.T) {
	f := newTestService(t)
	ctx := context.Background()
	resp := f.register("agentic/py", "agentic-py", simpleYAML("qwen-local", 5)+"scope:\n  profiles: [python]\n")
	if resp.State != meterapi.StateActive {
		t.Fatalf("setup: state %q, want active", resp.State)
	}
	if _, ok := f.admin.Keys["gonk-agentic-py"]; !ok {
		t.Fatalf("setup: no virtual key gonk-agentic-py (have %d keys); the assertion below would be vacuous", len(f.admin.Keys))
	}
	narrowed, err := opercfg.Load([]byte(strings.Replace(testOperatorYAML, "instance:\n  enabled: true\n", "instance:\n  enabled: true\n  scope: { allowed_profiles: [go, docs-md] }\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.Instance.Scope.AllowedProfiles == nil {
		t.Fatal("the narrowed operator config carries no allowed_profiles; the fixture edit did not apply")
	}
	f.svc.SetConfig(narrowed)
	if err := f.svc.Reresolve(ctx); err != nil {
		t.Fatalf("Reresolve: %v", err)
	}
	f.syncOnce()
	d, _, err := f.svc.Decide(ctx, decideReq("agentic/py", "gk-py", "sess-py"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != rung.Deny || d.Reason != rung.ReasonInvalidConfig {
		t.Fatalf("after the operator narrowed the menu: decision = %+v, want deny/%s", d, rung.ReasonInvalidConfig)
	}
	if _, ok := f.admin.Keys["gonk-agentic-py"]; ok {
		t.Error("the virtual key survived the operator narrowing the menu")
	}
}
