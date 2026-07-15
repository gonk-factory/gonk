//go:build testclock

package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// *** THIS FILE MUST NEVER BE IN A PRODUCTION IMAGE. ***
//
// A gonk-meter that reads its clock from a file is a gonk-meter whose BUDGET
// WINDOW CAN BE MOVED BY ANYONE WHO CAN WRITE THAT FILE. Moving the window
// forward resets every project's spend to zero. That is not a test seam in
// production; it is a budget bypass.
//
// It is therefore behind a build tag, shipped ONLY in the e2e image, and
// Plan 06's Task 9 Step 2 asserts the production binary contains neither the
// `testclock` symbol nor the GONK_TESTCLOCK_FILE literal.
//
// The file holds a signed offset in seconds, applied to the wall clock. An
// offset (not an absolute time) keeps the clock MONOTONE, which spend.Advance
// requires: a backwards jump must never reset a window.
func Now() time.Time {
	path := os.Getenv("GONK_TESTCLOCK_FILE")
	if path == "" {
		return time.Now().UTC()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Now().UTC()
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Now().UTC().Add(time.Duration(secs) * time.Second)
}
