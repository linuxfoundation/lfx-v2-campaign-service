// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"testing"
	"time"
)

// providerCallTimeoutMirror is the orchestrator's bound on a single provider call, duplicated
// here because internal/service imports this package (a real import would cycle).
//
// KEEP IN SYNC with service.providerCallTimeout. Nothing enforces that automatically: raising
// the real timeout without updating this mirror leaves the test passing against a stale value,
// silently ending the guard at the moment it would matter.
const providerCallTimeoutMirror = 2 * time.Minute

// TestStuckClaimReportAgeExceedsProviderCallTimeout keeps the diagnostic from crying wolf. A claim
// held by a HEALTHY dispatch cannot outlive providerCallTimeout, so reporting anything younger
// than that would flag in-flight work as stuck and train operators to ignore the signal.
func TestStuckClaimReportAgeExceedsProviderCallTimeout(t *testing.T) {
	if stuckClaimReportAge <= providerCallTimeoutMirror {
		t.Fatalf("stuckClaimReportAge (%s) must exceed providerCallTimeout (%s), or a healthy in-flight dispatch is reported as stuck",
			stuckClaimReportAge, providerCallTimeoutMirror)
	}
	if min := 2 * providerCallTimeoutMirror; stuckClaimReportAge < min {
		t.Errorf("stuckClaimReportAge (%s) should be at least 2x providerCallTimeout (%s) to absorb release time and replica clock skew",
			stuckClaimReportAge, min)
	}
	// The value reaches Postgres as NUMERIC SECONDS via make_interval, not as a duration
	// string. That matters: Go renders this duration as "4m0s", which Postgres REJECTS as
	// interval input — an earlier version bound it as $1::interval and would have errored on
	// every scan. Assert a positive, whole-second value, which make_interval always accepts.
	if secs := stuckClaimReportAge.Seconds(); secs <= 0 || secs != float64(int64(secs)) {
		t.Errorf("stuckClaimReportAge.Seconds() = %v; make_interval(secs =>) wants a positive whole number", secs)
	}
}

// TestDefaultStuckClaimLimitIsBounded guards the diagnostic against unbounded scans: a
// non-positive default would let a caller passing 0 pull the whole table.
func TestDefaultStuckClaimLimitIsBounded(t *testing.T) {
	if DefaultStuckClaimLimit <= 0 {
		t.Fatalf("DefaultStuckClaimLimit must be positive, got %d", DefaultStuckClaimLimit)
	}
}
