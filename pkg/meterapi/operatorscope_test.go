package meterapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gonk-factory/gonk/pkg/scope"
)

func fullScopePolicy() scope.Policy {
	return scope.Policy{
		Classes: []scope.Class{scope.ClassSource, scope.ClassDocs},
		Caps: scope.CapsPolicy{MaxFiles: i64p(2), MaxLines: i64p(30), MaxDeletes: i64p(0),
			MaxFileBytes: i64p(1000), MaxPatchBytes: i64p(2000)},
		ProtectedPaths: []string{"infra/**"}, CIPaths: []string{"ci/**"}, TestPaths: []string{"spec/**"},
		DocsPaths: []string{"handbook/**"}, SourcePaths: []string{"lib/**"},
		DenyLift: []scope.Class{scope.ClassCI}, Profiles: []string{"python"}, AllowedProfiles: []string{"python", "go"},
	}
}

// gonk-2usw DISPATCH: an operator layer survives the wire exactly. Every field
// of scope.Policy is set, so a field the projection forgot comes back zero and
// fails the comparison -- and a field ADDED to scope.Policy later fails the
// reflection check below until it is carried.
func TestScopePolicyRoundTripsEveryField(t *testing.T) {
	in := fullScopePolicy()
	v := reflect.ValueOf(in)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves scope.Policy.%s zero; a new field must be set here AND carried by ScopePolicyFrom/Policy",
				v.Type().Field(i).Name)
		}
	}
	raw, err := json.Marshal(ScopePolicyFrom(in))
	if err != nil {
		t.Fatal(err)
	}
	var wire ScopePolicy
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.Policy(); !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip:\n got %+v\nwant %+v\nwire %s", got, in, raw)
	}
}

// null and [] are different policies ("says nothing" vs "allows none") and
// must stay different across the wire: collapsing [] to null would turn an
// operator's "no class at all" into "no restriction".
func TestScopePolicyRoundTripsNilAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   scope.Policy
		want string
	}{
		{"says nothing", scope.Policy{}, `"classes":null`},
		{"allows no class", scope.Policy{Classes: []scope.Class{}}, `"classes":[]`},
		{"allows no profile", scope.Policy{AllowedProfiles: []string{}}, `"allowed_profiles":[]`},
		{"selects no profile", scope.Policy{Profiles: []string{}}, `"profiles":[]`},
	} {
		raw, err := json.Marshal(ScopePolicyFrom(tc.in))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), tc.want) {
			t.Errorf("%s: wire %s lacks %s", tc.name, raw, tc.want)
		}
		var wire ScopePolicy
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		got := wire.Policy()
		if !reflect.DeepEqual(got, tc.in) {
			t.Errorf("%s: round trip %+v, want %+v", tc.name, got, tc.in)
		}
		if got.IsZero() != tc.in.IsZero() {
			t.Errorf("%s: IsZero %v after the wire, %v before", tc.name, got.IsZero(), tc.in.IsZero())
		}
	}
}

// A meter that predates the field omits it; the reader must see "not stated"
// (nil), never an empty operator scope it could fold as "no restriction".
func TestAnAbsentOperatorScopeDecodesToNil(t *testing.T) {
	var resp ProjectResponse
	if err := json.Unmarshal([]byte(`{"project":"g/p","state":"active","effective":{"enabled":true}}`), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OperatorScope != nil {
		t.Fatalf("an absent operator_scope decoded to %+v, want nil", resp.OperatorScope)
	}
}

// The wire PipelineFix converts back to the gonkcfg type field for field.
func TestPipelineFixConvertsBackToEffective(t *testing.T) {
	w := wireContractFixtures()["project_response"].(ProjectResponse).Effective.PipelineFix
	e := w.EffectivePipelineFix()
	if !reflect.DeepEqual(e.Classes, w.Classes) || !reflect.DeepEqual(e.Targets, w.Targets) || e.AllowCI != w.AllowCI ||
		e.CIExecChanges != w.CIExecChanges || e.MaxAttempts != w.MaxAttempts || e.MaxChain != w.MaxChain ||
		e.VerifyTimeout != time.Duration(w.VerifyTimeoutSeconds)*time.Second || e.MaxFiles != w.MaxFiles ||
		e.MaxChangedLines != w.MaxChangedLines || !reflect.DeepEqual(e.Paths, w.Paths) ||
		e.AutoMerge.Mode != w.AutoMerge.Mode || !reflect.DeepEqual(e.AutoMerge.Classes, w.AutoMerge.Classes) {
		t.Fatalf("converted %+v from %+v", e, w)
	}
	if got := e.LiftOptIn(); len(got) != 1 || got[0] != scope.ClassCI {
		t.Fatalf("allow_ci: true must opt in to the ci lift, got %v", got)
	}
	w.AllowCI = false
	if got := w.EffectivePipelineFix().LiftOptIn(); len(got) != 0 {
		t.Fatalf("allow_ci: false opts in to %v", got)
	}
}
