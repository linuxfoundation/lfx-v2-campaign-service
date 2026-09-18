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

// TestListAccountCampaigns_OversizedAnalyticsResponse_IsRejected pins a round-22 review fix:
// fetchAccountCampaignAnalyticsRaw used to read maxResponseBytes+1 (the boundary sentinel) but
// never check the result against the cap, so an oversized adAnalytics response was silently
// decoded from its truncated-but-valid-JSON prefix instead of rejected — matching the same
// defect client.go:1128, metrics.go:626 and token.go:412 already guard against.
func TestListAccountCampaigns_OversizedAnalyticsResponse_IsRejected(t *testing.T) {
	padding := strings.Repeat(" ", maxResponseBytes+64)
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"}],"metadata":{}}`,
		`{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInUsd":"12.50"}]`+padding+`}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	_, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err == nil {
		t.Fatal("an oversized adAnalytics response was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to report the size cap was exceeded", err)
	}
}

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
		`{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInUsd":"12.50"}]}`,
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

// TestListAccountCampaigns_CampaignAbsentFromAnalytics_IsZeroNotFailed pins a round-19 review
// fix: fetchAccountCampaignAnalytics is one account-wide pivot call that either returns the
// whole metrics map or a non-nil error — LinkedIn omits a campaign with no activity in the
// window from that response entirely, rather than reporting it at zero. A campaign missing
// from a SUCCESSFUL response used to be marked FetchFailed, which suppressed exactly the
// zero-delivery pacing/action checks this endpoint exists to report.
func TestListAccountCampaigns_CampaignAbsentFromAnalytics_IsZeroNotFailed(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"},{"id":222,"name":"Campaign Two","status":"ACTIVE"}],"metadata":{}}`,
		`{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInUsd":"12.50"}]}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	rows, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	var missing AccountCampaignRow
	for _, r := range rows {
		if r.CampaignID == "222" {
			missing = r
		}
	}
	if missing.FetchFailed {
		t.Errorf("row for campaign 222 = %+v, want FetchFailed=false — absent from a successful account-wide response means zero activity, not a failed fetch", missing)
	}
	if missing.Impressions != 0 || missing.Clicks != 0 || missing.SpendUSD != 0 {
		t.Errorf("row for campaign 222 = %+v, want zero metrics", missing)
	}
}

// TestListAccountCampaigns_MalformedCostInUsd_MarksFetchFailed pins a round-19 review fix: a
// non-empty, unparseable costInUsd used to be silently ignored (SpendUSD left at 0), converting
// an upstream-data failure into a trusted zero spend.
func TestListAccountCampaigns_MalformedCostInUsd_MarksFetchFailed(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"}],"metadata":{}}`,
		`{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInUsd":"not-a-number"}]}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	rows, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true for a campaign whose costInUsd failed to parse", rows[0])
	}
	if rows[0].SpendUSD != 0 {
		t.Errorf("row for campaign 111 = %+v, want SpendUSD=0 (never fabricated) alongside FetchFailed", rows[0])
	}
}

// TestListAccountCampaigns_MissingCampaignListMetadata_IsRejected pins a round-24 review fix:
// an absent metadata block on the adCampaigns page used to be treated the same
// as an empty NextPageToken — "no more pages" — so a malformed or truncated intermediate page
// silently returned a partial campaign list as a complete one. accounts.go's adAccount picker
// already rejects this exact state (accounts.go:202-211); the campaign list walk did not.
func TestListAccountCampaigns_MissingCampaignListMetadata_IsRejected(t *testing.T) {
	srv, _ := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"}]}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))

	_, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
	if err == nil {
		t.Fatal("a campaign-list page with no metadata block was accepted as a complete list")
	}
	if !strings.Contains(err.Error(), "no metadata") {
		t.Errorf("error = %v, want it to report the missing metadata block", err)
	}
}

// TestListAccountCampaigns_NullAnalyticsElements_IsRejected pins a round-24 review fix:
// fetchAccountCampaignAnalyticsRaw's elements field used to be value-typed, so
// `{}`, `"elements":null`, and a missing field all decoded as the same empty/nil slice with no
// error. ListAccountCampaigns would then read every listed campaign as measured zero activity
// instead of a failed analytics read, fabricating a false "no delivery" pacing/action-item
// verdict from data that was never actually fetched.
func TestListAccountCampaigns_NullAnalyticsElements_IsRejected(t *testing.T) {
	for name, analyticsBody := range map[string]string{
		"null elements":   `{"elements":null}`,
		"absent elements": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := adAccountsServer(t,
				`{"elements":[{"id":111,"name":"Campaign One","status":"ACTIVE"}],"metadata":{}}`,
				analyticsBody,
			)
			c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))

			_, err := c.ListAccountCampaigns(context.Background(), "512345678", 7)
			if err == nil {
				t.Fatalf("an analytics response with %s was accepted as a successful empty read", name)
			}
			if !strings.Contains(err.Error(), "no elements field") {
				t.Errorf("error = %v, want it to report the missing elements field", err)
			}
		})
	}
}
