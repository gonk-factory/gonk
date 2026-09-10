package harness

import "testing"

// TestHostNetworkForIsUnconditional pins hostNetworkFor's decision for both
// runtime kinds. StartPostgres and StartLiteLLM never publish a container
// port -- they always dial the container they just started on
// 127.0.0.1:<fixed port> -- so host networking is the only way either
// function's container is ever reachable, for EITHER runtime. A regression
// back to "Podman only" (the original bug: true only when kind == Podman)
// would leave a genuine Docker daemon -- e.g. a GitHub Actions runner --
// booting containers with no route back to them at all, and this test would
// catch it: it fails today if hostNetworkFor(Docker) is changed to false.
func TestHostNetworkForIsUnconditional(t *testing.T) {
	cases := []struct {
		kind RuntimeKind
		want bool
	}{
		{Podman, true},
		{Docker, true},
	}
	for _, tc := range cases {
		if got := hostNetworkFor(tc.kind); got != tc.want {
			t.Errorf("hostNetworkFor(%s) = %v, want %v -- StartPostgres/StartLiteLLM have no other way to reach a container they start under this runtime", tc.kind, got, tc.want)
		}
	}
}
