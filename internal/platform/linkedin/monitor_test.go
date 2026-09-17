// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestListAccountCampaigns_RejectsMalformedAccountID pins the guard at monitor.go:60: a
// non-digits account id must be rejected before any request reaches LinkedIn, since this
// dispatcher method is reachable directly (not only through the Goa design layer's own
// Pattern(`^[0-9]+$`) check at the HTTP boundary).
func TestListAccountCampaigns_RejectsMalformedAccountID(t *testing.T) {
	c := newAccountsClient(t, "http://unused.invalid")
	_, err := c.ListAccountCampaigns(context.Background(), "urn:li:sponsoredAccount:5", 30)
	if err == nil {
		t.Fatal("a non-digits account id was accepted")
	}
	if !errors.Is(err, ErrInvalidAccountID) {
		t.Errorf("error = %v, want it to wrap ErrInvalidAccountID", err)
	}
}

// TestListAccountCampaigns_UsesInjectedClockNotWallClock pins the UTC window fix: the
// analytics pivot read's date range must come from the client's injected clock, not the bare
// wall clock, so it stays deterministic and testable — mirroring the equivalent Meta pinning
// test (internal/platform/meta/monitor_test.go).
func TestListAccountCampaigns_UsesInjectedClockNotWallClock(t *testing.T) {
	srv, rec := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"}],"metadata":{}}`,
		`{"elements":[{"pivotValue":"urn:li:sponsoredCampaign:111","impressions":1000,"clicks":50,"costInUsd":"12.50"}]}`,
	)
	// fixedNow's local calendar date (AEST, UTC+10) is 2026-09-18, one day ahead of its UTC
	// calendar date, 2026-09-17. restLiDate (metrics.go:477) renders day/month/year in the
	// value's own location, so this is exactly the case the client's `.UTC()` normalization
	// guards: without it, the request would be built against the 18th, not the 17th.
	fixedNow := time.Date(2026, 9, 17, 22, 0, 0, 0, time.FixedZone("AEST", 10*3600))
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL), WithClock(func() time.Time { return fixedNow }))

	rows, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 || rows[0].Impressions != 1000 || rows[0].Clicks != 50 {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	var analyticsURI string
	for _, u := range rec.all() {
		if strings.Contains(u, "/adAnalytics") {
			analyticsURI = u
		}
	}
	if analyticsURI == "" {
		t.Fatalf("no adAnalytics request recorded: %v", rec.all())
	}
	// days=7 ending on the injected clock's UTC calendar date, 2026-09-17 (inclusive), starts
	// 2026-09-11 — the same days-1 convention the Google/Reddit/Meta monitor dispatchers share.
	// If `.UTC()` were dropped, both dates would instead render as the 18th/12th (the clock's
	// AEST calendar date), so this pins the normalization rather than merely being consistent
	// with it.
	if !strings.Contains(analyticsURI, restLiDate(time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC))) {
		t.Errorf("adAnalytics request URI = %s, want it to contain the UTC-normalized start date", analyticsURI)
	}
	if !strings.Contains(analyticsURI, restLiDate(time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC))) {
		t.Errorf("adAnalytics request URI = %s, want it to contain the UTC-normalized end date", analyticsURI)
	}
	if strings.Contains(analyticsURI, restLiDate(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))) {
		t.Errorf("adAnalytics request URI = %s, contains the AEST calendar date (18th) — the UTC normalization is not being exercised", analyticsURI)
	}
}
