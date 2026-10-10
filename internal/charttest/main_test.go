//go:build chart

package charttest

import (
	"os"
	"testing"
)

// The version-normalized chart copy lives in a temp dir for the life of the
// test binary (see ChartDir). Remove it on the way out rather than leaving one
// behind per run.
func TestMain(m *testing.M) {
	code := m.Run()
	cleanupNormalizedChart()
	os.Exit(code)
}
