// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
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
	if !strings.Contains(err.Error(), "invalid LinkedIn ad account id") {
		t.Errorf("error = %v, want it to name the invalid account id", err)
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
	fixedNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
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
	// days=7 ending 2026-09-17 (inclusive) starts 2026-09-11, the same days-1 convention the
	// Google/Reddit/Meta monitor dispatchers share — not whatever the wall clock happens to be
	// when the test runs.
	if !strings.Contains(analyticsURI, restLiDate(time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC))) {
		t.Errorf("adAnalytics request URI = %s, want it to contain the start date derived from the injected clock", analyticsURI)
	}
	if !strings.Contains(analyticsURI, restLiDate(fixedNow)) {
		t.Errorf("adAnalytics request URI = %s, want it to contain the end date derived from the injected clock", analyticsURI)
	}
}
