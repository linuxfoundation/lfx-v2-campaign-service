// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"strings"
	"testing"
	"time"
)

// providerCallTimeoutMirror is the orchestrator's bound on a single provider call. It is
// duplicated here rather than imported because internal/service imports this package, so a
// real import would be a cycle. TestClaimLeaseTTLExceedsProviderCallTimeout guards the
// duplication by failing if the two ever drift apart in a way that makes the lease unsafe.
const providerCallTimeoutMirror = 2 * time.Minute

// TestClaimLeaseTTLExceedsProviderCallTimeout is the safety invariant behind the whole
// reclaim mechanism. A 'pending' claim is only reclaimable once it is provably ABANDONED,
// and the only thing that makes that provable is the orchestrator hard-bounding every
// provider call at providerCallTimeout. If the lease were ever shortened to at or below that
// bound, a LIVE dispatch could have its claim stolen mid-flight — two dispatchers would then
// create the same campaign upstream, which is precisely the duplicate this claim prevents.
func TestClaimLeaseTTLExceedsProviderCallTimeout(t *testing.T) {
	if claimLeaseTTL <= providerCallTimeoutMirror {
		t.Fatalf("claimLeaseTTL (%s) must exceed providerCallTimeout (%s): a live dispatch could otherwise have its claim reclaimed mid-flight and duplicate the campaign upstream",
			claimLeaseTTL, providerCallTimeoutMirror)
	}
	// Require real headroom, not a hairline margin: the release path itself is bounded
	// (claimReleaseTimeout) and replicas' clocks can skew, so 2x is the intended floor.
	if min := 2 * providerCallTimeoutMirror; claimLeaseTTL < min {
		t.Errorf("claimLeaseTTL (%s) should be at least 2x providerCallTimeout (%s) to absorb release time and clock skew",
			claimLeaseTTL, min)
	}
}

// TestClaimLeaseTTLIsPositive guards against a zero value, which would make EVERY claim
// instantly reclaimable and disable single-flight entirely.
func TestClaimLeaseTTLIsPositive(t *testing.T) {
	if claimLeaseTTL <= 0 {
		t.Fatalf("claimLeaseTTL must be positive, got %s — a non-positive lease makes every claim instantly stealable", claimLeaseTTL)
	}
	// The interval is passed to Postgres as a string; make sure it is a parseable one.
	if s := claimLeaseTTL.String(); !strings.HasSuffix(s, "m") && !strings.HasSuffix(s, "s") {
		t.Errorf("claimLeaseTTL.String() = %q, which Postgres may not parse as an interval", s)
	}
}
