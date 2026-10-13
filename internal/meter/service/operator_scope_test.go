package service

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/meterapi"
	"github.com/gonk-factory/gonk/pkg/opercfg"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// gonk-2usw DISPATCH: the project response carries the OPERATOR half of the
// scope fold -- the instance layer and the project's own group layer, each as
// the operator wrote it -- so gonk-gate can fold a dispatch's envelope without
// a second copy of the operator config. Asserted on the named fields, for a
// project inside the group and one outside it.
func TestProjectResponseCarriesTheOperatorScopeLayers(t *testing.T) {
	yaml := strings.Replace(testOperatorYAML, "instance:\n  enabled: true\n",
		"instance:\n  enabled: true\n  scope: { classes: [source, test, docs], deny_lift: [ci], caps: { max_files: 2 } }\n", 1)
	yaml = strings.Replace(yaml, "  kubernetes:\n    budget: { monthly_cost_usd: 50 }\n",
		"  kubernetes:\n    budget: { monthly_cost_usd: 50 }\n    scope: { classes: [source], protected_paths: [\"infra/**\"] }\n", 1)
	cfg, err := opercfg.Load([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instance.Scope.IsZero() || cfg.GroupFor("kubernetes/calc").Scope.IsZero() {
		t.Fatal("the fixture edit did not apply: the operator config carries no scope")
	}
	f := newTestServiceWithConfig(t, yaml)

	in := f.register("kubernetes/calc", "kubernetes-calc", simpleYAML("qwen-local", 5))
	out := f.register("elsewhere/calc", "elsewhere-calc", simpleYAML("qwen-local", 5))
	for _, tc := range []struct {
		name string
		resp meterapi.ProjectResponse
		// wantGroup: the group layer that applies to the project.
		wantGroup scope.Policy
	}{
		{"inside the group", in, cfg.GroupFor("kubernetes/calc").Scope},
		{"outside every group", out, scope.Policy{}},
	} {
		if tc.resp.OperatorScope == nil {
			t.Fatalf("%s: no operator_scope on an active project's response", tc.name)
		}
		if got := tc.resp.OperatorScope.Instance.Policy(); !reflect.DeepEqual(got, cfg.Instance.Scope) {
			t.Errorf("%s: instance layer %+v, want %+v", tc.name, got, cfg.Instance.Scope)
		}
		if got := tc.resp.OperatorScope.Group.Policy(); !reflect.DeepEqual(got, tc.wantGroup) {
			t.Errorf("%s: group layer %+v, want %+v", tc.name, got, tc.wantGroup)
		}
	}
	if got := in.OperatorScope.Group.Policy(); len(got.Classes) != 1 || got.Classes[0] != scope.ClassSource ||
		len(got.ProtectedPaths) != 1 || got.ProtectedPaths[0] != "infra/**" {
		t.Errorf("the group layer of kubernetes/calc is %+v, want classes [source] and protected_paths [infra/**]", got)
	}

	// GET answers the same, and it is the LIVE operator config: an operator
	// who tightens scope is obeyed by the next read, without a re-registration.
	got, ok, err := f.svc.Get(context.Background(), "kubernetes/calc")
	if err != nil || !ok || got.OperatorScope == nil {
		t.Fatalf("Get: %+v ok=%v err=%v", got.OperatorScope, ok, err)
	}
	tightened, err := opercfg.Load([]byte(strings.Replace(yaml, "classes: [source, test, docs]", "classes: [docs]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.SetConfig(tightened)
	got, _, err = f.svc.Get(context.Background(), "kubernetes/calc")
	if err != nil || got.OperatorScope == nil {
		t.Fatal(err)
	}
	if c := got.OperatorScope.Instance.Classes; len(c) != 1 || c[0] != "docs" {
		t.Errorf("after the operator tightened the instance scope, GET says classes %v, want [docs]", c)
	}
}

// An invalid project has no fold: operator_scope is nil exactly when effective
// is, so a reader cannot fold a scope for a config that did not load.
func TestAnInvalidProjectCarriesNoOperatorScope(t *testing.T) {
	f := newTestService(t)
	resp, status, err := f.svc.Register(context.Background(), meterapi.ProjectRequest{
		Project: "group/repo", ProjectID: 1, Rig: "group-repo", GonkYML: "not: [valid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 422 || resp.State != meterapi.StateInvalid || resp.Effective != nil {
		t.Fatalf("setup: status %d state %q effective %v", status, resp.State, resp.Effective)
	}
	if resp.OperatorScope != nil {
		t.Fatalf("an invalid project carries an operator scope: %+v", resp.OperatorScope)
	}
}
