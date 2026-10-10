package opercfg

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gonk-factory/gonk/pkg/gonkcfg"
)

// TestFoldPolicyHandlesEveryPolicyField is the structural guard behind
// gonk-a82p. foldPolicy builds its result field by field, so a section added to
// gonkcfg.Policy and not added there is silently dropped by GroupFor: the group
// layer validates at meter start and is then never applied. (Policy.Scope was
// dropped exactly this way.) This test fills EVERY leaf of Policy with a
// non-zero value by reflection, folds it onto a zero base, and fails naming any
// leaf the result lost. A new Policy field turns it red until foldPolicy
// handles it -- no list to remember to update.
//
// "Handled" is deliberately the weak, mechanical property (non-zero in, non-zero
// out, folded onto nothing). Each section's own semantics are tested separately
// (TestGroupFor*).
func TestFoldPolicyHandlesEveryPolicyField(t *testing.T) {
	var full gonkcfg.Policy
	fillNonZero(reflect.ValueOf(&full).Elem())

	// Sanity: the filler really reached every leaf, or the guard is vacuous.
	leaves := leafPaths(reflect.ValueOf(full), "Policy")
	if len(leaves) < 20 {
		t.Fatalf("filler reached only %d leaves of Policy (%v); the guard is not looking at the struct", len(leaves), leaves)
	}
	for _, l := range leaves {
		if l.zero {
			t.Fatalf("filler left %s zero; extend fillNonZero for its type", l.path)
		}
	}

	// The ONLY leaves a group fold may lose, each with why. An entry here is a
	// decision, not a convenience: the key must be refused at the operator layer
	// by validation, so no group can ever hold a value for it.
	projectOnly := map[string]string{
		// PipelineFixPolicy.Validate(scope.LayerOperator) refuses `paths`
		// ("paths is project-only"); TestLoadRejectsInvalidOperatorPipelineFix.
		"Policy.PipelineFix.Paths": "project-only; refused in an operator layer",
		// scope.Policy.Validate(scope.LayerOperator) refuses `profiles`
		// ("profiles is project-only"); TestLoadRejectsInvalidOperatorScope.
		// The operator's half, allowed_profiles, IS folded (intersection).
		"Policy.Scope.Profiles": "project-only; refused in an operator layer",
		// ChangePolicy.Validate(scope.LayerOperator) refuses `paths`
		// ("paths is project-only"); TestLoadRejectsInvalidOperatorChange.
		"Policy.Change.Paths": "project-only; refused in an operator layer",
	}

	got := foldPolicy(gonkcfg.Policy{}, full)
	for _, l := range leafPaths(reflect.ValueOf(got), "Policy") {
		if _, ok := projectOnly[l.path]; ok {
			if !l.zero {
				t.Errorf("%s is listed as project-only but the group fold carries it; remove it from projectOnly", l.path)
			}
			continue
		}
		if l.zero {
			t.Errorf("foldPolicy drops %s: a group's value for it never reaches GroupFor (gonk-a82p). Fold it in foldPolicy.", l.path)
		}
	}
	// And in the other argument position: a base-only value must survive a
	// silent child (the nested-group case).
	got = foldPolicy(full, gonkcfg.Policy{})
	for _, l := range leafPaths(reflect.ValueOf(got), "Policy") {
		if _, ok := projectOnly[l.path]; ok {
			continue
		}
		if l.zero {
			t.Errorf("foldPolicy drops %s from the coarser group when the nested group is silent", l.path)
		}
	}
}

type leaf struct {
	path string
	zero bool
}

// leafPaths lists every leaf under v, descending through structs and pointers
// to structs. A leaf is anything else: pointers to scalars, slices, scalars.
func leafPaths(v reflect.Value, path string) []leaf {
	switch v.Kind() {
	case reflect.Struct:
		var out []leaf
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			out = append(out, leafPaths(v.Field(i), path+"."+v.Type().Field(i).Name)...)
		}
		return out
	case reflect.Pointer:
		if !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			return leafPaths(v.Elem(), path)
		}
	}
	return []leaf{{path: path, zero: v.IsZero()}}
}

// fillNonZero sets every exported leaf under v to a non-zero value.
func fillNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZero(v.Field(i))
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(v.Elem())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0))
		v.Set(s)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.String:
		v.SetString("x")
	default:
		panic(fmt.Sprintf("fillNonZero: unsupported kind %s", v.Kind()))
	}
}
