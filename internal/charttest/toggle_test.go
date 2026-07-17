//go:build chart

// Package charttest -- Task 8.5: the component-toggle matrix.
//
// Toggleability is a first-class feature of this chart, not an afterthought:
// each bundled component (gascity, dolt, intake, meter) can be turned off when
// it is managed or replaced elsewhere. Every such combination must either
// render a coherent factory (no dangling references to the disabled
// component) or fail closed with a message the operator can act on. This file
// is the matrix assertion over the guards already written in _guards.tpl
// (Tasks 1-6.5); it does not invent new behavior, it proves the behavior that
// exists.
package charttest

import (
	"path/filepath"
	"strings"
	"testing"
)

// Each RENDERABLE combination templates coherently: the disabled component's
// objects are gone, and nothing that survives still points at it.
func TestToggleMatrixRenders(t *testing.T) {
	cases := []struct {
		name    string
		sets    []string
		present []string // kind/name that MUST appear
		absent  []string // kind/name that must NOT
		// coherent, if set, runs an extra assertion specific to this case: the
		// "no dangling references" check the plan calls for beyond simple
		// present/absent.
		coherent func(t *testing.T, out string)
	}{
		{
			// The whole factory. Minimum() already IS this profile (Task 1's
			// Minimum() comment: "the DEFAULT render is the WHOLE factory").
			name:    "all-bundled",
			sets:    nil,
			present: []string{"Deployment/gonk-controller", "StatefulSet/gonk-dolt", "Deployment/gonk-intake", "Deployment/gonk-meter"},
		},
		{
			// dolt.enabled=false: the bundled beads Dolt is gone. The CONTROLLER
			// (the only thing that ever talks to beads Dolt) points at the
			// external host instead. RECONCILED, Task 0: this has NO EFFECT on
			// the ledger -- meter never had a Dolt position, so meter's env must
			// carry no GC_DOLT_HOST/PORT reference at all (there is nothing
			// dangling to the disabled StatefulSet's in-cluster Service name
			// either).
			name:    "external-dolt",
			sets:    []string{"dolt.enabled=false", "dolt.external.host=dolt.databases.svc"},
			absent:  []string{"StatefulSet/gonk-dolt", "Service/gonk-dolt"},
			present: []string{"Deployment/gonk-controller", "Deployment/gonk-meter"},
			coherent: func(t *testing.T, out string) {
				ctrl := MustObject(t, out, "Deployment", "gonk-controller")
				if !strings.Contains(ctrl.Doc, "dolt.databases.svc") {
					t.Errorf("controller does not point at the external Dolt host:\n%s", ctrl.Doc)
				}
				if strings.Contains(ctrl.Doc, "gonk-dolt.gonk.svc") || strings.Contains(ctrl.Doc, "value: gonk-dolt\n") {
					t.Errorf("controller still references the disabled in-cluster Dolt service:\n%s", ctrl.Doc)
				}
				meter := MustObject(t, out, "Deployment", "gonk-meter")
				if strings.Contains(meter.Doc, "GC_DOLT_HOST") || strings.Contains(meter.Doc, "dolt.databases.svc") {
					t.Errorf("meter has a Dolt reference; it never talked to Dolt for the ledger (RECONCILED, Task 0):\n%s", meter.Doc)
				}
			},
		},
		{
			// gascity.enabled=false: no bundled controller, no RBAC for it.
			// intake dispatches to the external supervisor instead. Dolt (beads)
			// and meter are unaffected -- neither has any relationship to the
			// controller toggle.
			name:    "byo-gascity",
			sets:    []string{"gascity.enabled=false", "gascity.supervisorURL=http://gc.external.svc:8372"},
			absent:  []string{"Deployment/gonk-controller", "StatefulSet/gonk-controller", "ServiceAccount/gc-controller", "Role/gc-controller", "RoleBinding/gc-controller", "Service/gonk-controller"},
			present: []string{"StatefulSet/gonk-dolt", "Deployment/gonk-intake"},
			coherent: func(t *testing.T, out string) {
				intake := MustObject(t, out, "Deployment", "gonk-intake")
				if !strings.Contains(intake.Doc, "http://gc.external.svc:8372") {
					t.Errorf("intake does not dispatch to the external supervisor:\n%s", intake.Doc)
				}
			},
		},
		{
			// RECONCILED, Task 0: was "ledger.backend=postgres" +
			// "ledger.postgres.mode=cnpg" -- backend has no other value any
			// more, so only the mode changes. dolt.enabled stays true (beads is
			// an entirely separate component); a chart-managed CNPG Cluster
			// appears alongside it for the ledger.
			name:    "ledger-cnpg",
			sets:    []string{"ledger.postgres.mode=cnpg"},
			present: []string{"StatefulSet/gonk-dolt", "Cluster/gonk-ledger-postgres"},
			coherent: func(t *testing.T, out string) {
				meter := MustObject(t, out, "Deployment", "gonk-meter")
				if !strings.Contains(meter.Doc, "value: postgres") {
					t.Errorf("meter is not pointed at the postgres backend:\n%s", meter.Doc)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := Minimum()
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			out := Render(t, args...)
			index := map[string]bool{}
			for _, o := range Objects(t, out) {
				index[o.Kind+"/"+o.Metadata.Name] = true
			}
			for _, p := range tc.present {
				if !index[p] {
					t.Errorf("%s: expected %s to render", tc.name, p)
				}
			}
			for _, a := range tc.absent {
				if index[a] {
					t.Errorf("%s: %s rendered and must not have", tc.name, a)
				}
			}
			if tc.coherent != nil {
				tc.coherent(t, out)
			}
			// Every combination that renders must be kubeconform-clean. All four
			// cases are checked against the vendored CRD schemas (harmless for
			// the cases with no CRD in the render -- Kubeconform's -schema-location
			// "default" still covers every core/apps/networking/rbac kind).
			Kubeconform(t, out, filepath.Join(ChartDir(), "tests", "crd-schemas"))
		})
	}
}

// The four toggle profiles ALSO exist as ci/ values files (a peer of the
// --set lists above), so `helm lint --strict` covers them independently of
// however internal/charttest happens to construct the same combination. Task
// 9's golden/profile test will point kubeconform and `helm lint` at the same
// files; this is the Task 8.5 evidence that they are internally coherent
// today.
func TestToggleProfilesLintClean(t *testing.T) {
	for _, name := range []string{"values-all-bundled", "values-external-dolt", "values-byo-gascity", "values-ledger-cnpg"} {
		t.Run(name, func(t *testing.T) {
			Lint(t, Profile(name)...)
		})
	}
}

// The four toggle profiles render on their own (no Minimum() --set flags),
// proving each ci/ file is self-sufficient, and are kubeconform-clean.
func TestToggleProfilesRenderAndValidate(t *testing.T) {
	for _, name := range []string{"values-all-bundled", "values-external-dolt", "values-byo-gascity", "values-ledger-cnpg"} {
		t.Run(name, func(t *testing.T) {
			out := Render(t, Profile(name)...)
			Kubeconform(t, out, filepath.Join(ChartDir(), "tests", "crd-schemas"))
		})
	}
}

// Each fail-closed combination refuses to render, with a message that names
// the missing external replacement (or, for the ledger.backend case, the
// schema's own rejection -- see the comment on that case below). A toggle
// that turns a component off WITHOUT its replacement must never render a
// silently-broken factory.
func TestToggleMatrixFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		sets []string
		want string
	}{
		{"dolt-off-no-external", []string{"dolt.enabled=false"}, "dolt.external.host"},
		{"gascity-off-no-url", []string{"gascity.enabled=false"}, "gascity.supervisorURL"},
		{"meter-off-intake-on", []string{"meter.enabled=false"}, "dispatch unmetered work"},
		// RECONCILED, Task 0 -- NEW: there is no dolt-backed ledger any more.
		// ledger.backend is a single-value enum (["postgres"]) in
		// values.schema.json, so setting it to "dolt" is caught by layer A
		// (the JSON Schema) before _guards.tpl's layer B ever runs -- there is
		// no "G15" fail() in _guards.tpl, and none is needed: the schema
		// already fails closed. This is the same belt-and-braces relationship
		// G3/G4 and G19 document elsewhere (see guards_test.go's
		// TestGuardLedgerNeedsDSNSecret and its comment). The plan's draft
		// asserted the dot-path "ledger.backend"; the real message uses the
		// schema's slash path ("/ledger/backend"), verified empirically
		// against this helm/schema version -- asserted here, not guessed.
		{"ledger-backend-dolt", []string{"ledger.backend=dolt"}, "ledger/backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := Minimum()
			for _, s := range tc.sets {
				args = append(args, "--set", s)
			}
			out, err := RenderErr(t, args...)
			if err == nil {
				t.Fatalf("%s rendered; it must fail closed", tc.name)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("%s failed with the wrong message.\nwant: %q\ngot:\n%s", tc.name, tc.want, out)
			}
		})
	}
}
