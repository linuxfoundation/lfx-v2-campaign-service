// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"strings"
	"testing"
	"time"
)

// providerCallTimeoutMirror is the orchestrator's bound on a single provider call, duplicated
// here because internal/service imports this package (a real import would cycle).
const providerCallTimeoutMirror = 2 * time.Minute

// TestStaleClaimAgeExceedsProviderCallTimeout keeps the diagnostic from crying wolf. A claim
// held by a HEALTHY dispatch cannot outlive providerCallTimeout, so reporting anything younger
// than that would flag in-flight work as stuck and train operators to ignore the signal.
func TestStaleClaimAgeExceedsProviderCallTimeout(t *testing.T) {
	if staleClaimAge <= providerCallTimeoutMirror {
		t.Fatalf("staleClaimAge (%s) must exceed providerCallTimeout (%s), or a healthy in-flight dispatch is reported as stuck",
			staleClaimAge, providerCallTimeoutMirror)
	}
	if min := 2 * providerCallTimeoutMirror; staleClaimAge < min {
		t.Errorf("staleClaimAge (%s) should be at least 2x providerCallTimeout (%s) to absorb release time and replica clock skew",
			staleClaimAge, min)
	}
	// Passed to Postgres as an interval string; make sure it is a parseable one.
	if s := staleClaimAge.String(); !strings.HasSuffix(s, "m") && !strings.HasSuffix(s, "s") {
		t.Errorf("staleClaimAge.String() = %q, which Postgres may not parse as an interval", s)
	}
}

// TestDefaultStuckClaimLimitIsBounded guards the diagnostic against unbounded scans: a
// non-positive default would let a caller passing 0 pull the whole table.
func TestDefaultStuckClaimLimitIsBounded(t *testing.T) {
	if defaultStuckClaimLimit <= 0 {
		t.Fatalf("defaultStuckClaimLimit must be positive, got %d", defaultStuckClaimLimit)
	}
}
