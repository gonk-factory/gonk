package main

import (
	"io"
	"net/http"
)

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
