//go:build !testclock

package main

import "time"

// Now is the service's only clock. Production: the wall clock, full stop.
func Now() time.Time { return time.Now().UTC() }
