// Command doctor is the e2e preflight. It runs harness.Doctor and prints a
// verdict table; on any Required (or L3-only) failure it exits non-zero with a
// specific remedy, so a future executor is stopped BEFORE losing 25 minutes to a
// broken box. `make e2e-doctor` runs it.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/harness"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rep := harness.Doctor(ctx, harness.DoctorOptions{
		SkipL3: os.Getenv("GONK_E2E_KUBECONFIG") != "", // L3 against an existing cluster: no kind/bridge needed
	})
	fmt.Print(rep.String())
	if !rep.OK() {
		os.Exit(1)
	}
}
