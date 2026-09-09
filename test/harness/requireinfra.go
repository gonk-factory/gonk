package harness

import (
	"os"
	"testing"
)

// RequireInfra asserts that an external dependency a tagged suite needs
// (a running container, an image built locally, an env var pointing at a
// live proxy, a `sh` binary on PATH) is actually available before the test
// goes on to use it.
//
// The two environments this runs in want different behavior from the SAME
// missing dependency. On a dev box, "no `sh`" or "no litellm running" is
// routine -- the whole point of the tagged suites is that `go test ./...`
// does not need every one of them, so this is a skip. In CI (CI=true), a
// tagged job exists FOR THE SOLE PURPOSE of providing that dependency
// (T-15/T-16/T-17 bring up postgres, litellm, freshly built images); a
// missing one there is not "nothing to test", it is the job's own setup
// having silently failed, and a skip would report that job green. That is
// the exact failure T-14 exists to close (R-40, R-43, R-44): a t.Skip on
// missing infrastructure reads as green whether or not a runner ever
// provisioned the infrastructure at all. So under CI=true, missing infra
// is t.Fatal.
//
// name should identify the dependency AND, where useful, the remedy (e.g.
// "image gonk-agent:dev (run `make images` first)") -- it goes verbatim
// into both the skip and the fatal message.
func RequireInfra(t *testing.T, name string, ok bool) {
	t.Helper()
	switch requireInfraAction(ok, os.Getenv("CI")) {
	case infraActionOK:
		return
	case infraActionFatal:
		t.Fatalf("missing infrastructure dependency in CI: %s -- a CI job for this suite exists specifically to provide it; this is a job setup failure, not nothing-to-test", name)
	default:
		t.Skipf("missing infrastructure dependency: %s (skip, not fail -- CI=true would fail this instead)", name)
	}
}

// infraAction is what RequireInfra decides to do, factored out as a pure
// function of (ok, the CI env var) so it can be unit-tested directly --
// including the t.Fatal-under-CI branch, which cannot be exercised through a
// real *testing.T subtest without that subtest's failure cascading up and
// failing the whole `go test` run that is testing it.
type infraAction int

const (
	infraActionOK infraAction = iota
	infraActionSkip
	infraActionFatal
)

func requireInfraAction(ok bool, ciEnv string) infraAction {
	if ok {
		return infraActionOK
	}
	if ciEnv == "true" {
		return infraActionFatal
	}
	return infraActionSkip
}
