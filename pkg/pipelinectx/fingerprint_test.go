package pipelinectx

import (
	"regexp"
	"testing"
)

func TestFingerprint(t *testing.T) {
	build := FailedJob{Stage: "build", Name: "compile", FailureReason: "script_failure"}
	test := FailedJob{Stage: "test", Name: "unit", FailureReason: "script_failure"}
	lint := FailedJob{Stage: "test", Name: "lint", FailureReason: "script_failure"}
	optional := FailedJob{Stage: "test", Name: "optional", FailureReason: "script_failure", AllowFailure: true}

	base := Fingerprint([]FailedJob{build, test, lint}, "")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(base) {
		t.Fatalf("shape %q", base)
	}
	// Pinned: the anchor of every open run is built from it.
	if want := "00f31badd47ed4ce"; base != want {
		t.Fatalf("fingerprint %s, pinned %s", base, want)
	}

	same := map[string][]FailedJob{
		"order independent (reversed)": {lint, test, build},
		"order independent (rotated)":  {test, lint, build},
		"allow_failure job excluded":   {build, optional, test, lint},
	}
	for name, jobs := range same {
		if got := Fingerprint(jobs, ""); got != base {
			t.Errorf("%s: %s != %s", name, got, base)
		}
	}
	differ := map[string][]FailedJob{
		"job missing":            {build, test},
		"failure reason differs": {build, test, {Stage: "test", Name: "lint", FailureReason: "runner_system_failure"}},
		"stage differs":          {build, test, {Stage: "verify", Name: "lint", FailureReason: "script_failure"}},
		"name differs":           {build, test, {Stage: "test", Name: "lint2", FailureReason: "script_failure"}},
		"allow_failure flipped":  {build, test, lint, {Stage: "test", Name: "optional", FailureReason: "script_failure"}},
	}
	for name, jobs := range differ {
		if got := Fingerprint(jobs, ""); got == base {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
	// Field boundaries matter: stage "a", name "bc" is not stage "ab", name "c".
	if Fingerprint([]FailedJob{{Stage: "a", Name: "bc"}}, "") == Fingerprint([]FailedJob{{Stage: "ab", Name: "c"}}, "") {
		t.Error("field boundary lost")
	}
}

func TestFingerprintNoJobs(t *testing.T) {
	none := Fingerprint(nil, "")
	if none != Fingerprint(nil, NoJobsReason) {
		t.Error("empty reason must mean no_jobs")
	}
	if none == Fingerprint(nil, "config_error") {
		t.Error("pipeline failure reason ignored")
	}
	// Only allow_failure jobs: same as no jobs.
	if none != Fingerprint([]FailedJob{{Stage: "s", Name: "n", AllowFailure: true}}, "") {
		t.Error("allow_failure-only set must fall back to the pipeline line")
	}
	// A job literally named like the fallback line is still a job line.
	if none == Fingerprint([]FailedJob{{Stage: "pipeline", Name: "no_jobs"}}, "") {
		t.Error("job line collided with the pipeline line")
	}
}
