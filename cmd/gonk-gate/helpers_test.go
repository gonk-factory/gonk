package main

import (
	"io"
	"log/slog"
	"net/http"
)

// testLogger writes structured logs into w so a test can assert that something
// was REPORTED, not merely handled. Used where the correct behaviour is "carry
// on, but loudly" -- a best-effort step whose failure must not be swallowed.
func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}

// meterClient builds a *meterAPI against baseURL with no bearer token -- every
// fake meter in this tree answers unauthenticated, so tests only need the URL.
func meterClient(baseURL string) *meterAPI {
	return newMeterAPI(baseURL, "")
}

// jsonReadAll reads and returns a request body whole -- test fakes are not on
// a trust boundary, so there is no size cap here (unlike pkg/glab/pkg/gcapi).
func jsonReadAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}
