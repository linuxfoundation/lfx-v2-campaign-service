// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The fetch budget is bounded even with NO caller deadline, which is the case the
// deadline-share alone could not cover: with context.Background() the share is never
// computed, and the only bound left is the walk's own — 25 images times the per-image
// timeout, over eight minutes of wall time chosen by whoever supplied the URLs.
//
// Each subtest names the half it pins, because the two can fail independently: deleting
// the ceiling leaves the share, and deleting the share leaves the ceiling, and either
// deletion keeps a test that only checked "the budget is positive" green.
func TestCreativeFetchBudget(t *testing.T) {
	t.Run("no deadline falls back to the absolute ceiling", func(t *testing.T) {
		if got := creativeFetchBudget(context.Background()); got != maxCreativeImageFetchWall {
			t.Errorf("budget = %v, want the %v ceiling — a caller with no deadline must still be bounded", got, maxCreativeImageFetchWall)
		}
	})

	t.Run("a tight deadline narrows below the ceiling", func(t *testing.T) {
		// Deliberately far under the ceiling, so the only way to produce a value in this
		// range is to have actually taken the caller's share.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		got := creativeFetchBudget(ctx)
		if got >= maxCreativeImageFetchWall {
			t.Fatalf("budget = %v, want well under the %v ceiling — the caller's share must win when it is smaller", got, maxCreativeImageFetchWall)
		}
		// Half of ten seconds, less whatever the call itself took.
		if got > 5*time.Second || got < 4*time.Second {
			t.Errorf("budget = %v, want ~5s (half of the caller's remaining 10s)", got)
		}
	})

	t.Run("a loose deadline does not widen past the ceiling", func(t *testing.T) {
		// An hour's share is thirty minutes; the ceiling must still win. This is the
		// direction a "just use the caller's share" simplification gets wrong.
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if got := creativeFetchBudget(ctx); got != maxCreativeImageFetchWall {
			t.Errorf("budget = %v, want the %v ceiling — a generous caller deadline must not widen the fetch phase", got, maxCreativeImageFetchWall)
		}
	})

	t.Run("an expired deadline yields a non-positive budget", func(t *testing.T) {
		// The caller leaves the context alone in this case, so the expiry is reported as
		// the caller's rather than as the fetch overrunning its own share.
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		if got := creativeFetchBudget(ctx); got > 0 {
			t.Errorf("budget = %v, want non-positive for an already-expired caller deadline", got)
		}
	})
}

// A creative the preflight called PRESENT that arrives with no images is an internal
// inconsistency, and reporting it as success wrote an audit step claiming an ad was
// created while naming no ad id. The refusal is what the caller can act on.
func TestCreateDemandGenAd_PresentCreativeWithNoImagesIsRefused(t *testing.T) {
	c := &Client{}
	assetIDs, adID, err := c.createDemandGenAd(context.Background(), "customers/1/adGroups/2", "2", "https://example.org", demandGenCreativePlan{}, nil)
	if err == nil {
		t.Fatal("returned success for a present creative with no images; the caller then appends an 'ad created' step naming an empty id")
	}
	if !strings.Contains(err.Error(), "no images") {
		t.Errorf("refusal %q does not say what was missing", err)
	}
	// Nothing was sent, so there is nothing to report alongside the error.
	if assetIDs != nil || adID != "" {
		t.Errorf("returned assetIDs=%v adID=%q alongside a pre-send refusal", assetIDs, adID)
	}
}

// The count bound runs BEFORE the per-element walk. Asserted by handing it a list that is
// both too long AND full of elements the walk would reject first: a count error proves the
// bound came first, and an "is empty" error proves the walk did the work anyway.
func TestValidateCreativeText_CountBoundPrecedesPerElementWork(t *testing.T) {
	const max = 5
	in := make([]string, 0, max*3)
	for i := 0; i < max*3; i++ {
		in = append(in, "   ") // trims to empty: the first thing the loop refuses
	}
	_, err := validateCreativeText("demand gen", "headline", in, 1, max, 30)
	if err == nil {
		t.Fatal("accepted a list three times the maximum")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("refusal %q is the per-element one; the count bound must be checked before the walk so an oversized list is not trimmed, measured and hashed first", err)
	}
}
