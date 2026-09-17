// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// monitorPageResponses serves ListAccountCampaigns' two upstream reads (campaign list and
// insights) from canned per-path page bodies, one per request to that path, and records
// every request URI seen — mirroring adAccountsServer in accounts_test.go.
func monitorPageResponses(t *testing.T, campaignPages, insightsPages []string) (*httptest.Server, *recordedURIs) {
	t.Helper()
	rec := &recordedURIs{}
	var campaignN, insightsN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			if campaignN >= len(campaignPages) {
				t.Errorf("campaign list asked for page %d but only %d were canned", campaignN+1, len(campaignPages))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(campaignPages[campaignN]))
			campaignN++
		case strings.HasSuffix(r.URL.Path, "/insights"):
			if insightsN >= len(insightsPages) {
				t.Errorf("insights asked for page %d but only %d were canned", insightsN+1, len(insightsPages))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(insightsPages[insightsN]))
			insightsN++
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newMonitorClient(srv *httptest.Server) *Client {
	return NewClient(Credentials{AccessToken: "tok-secret-abc"}, AccountConfig{}, WithBaseURL(srv.URL))
}

func campaignPage(id, name, status string, next bool) string {
	paging := `{}`
	if next {
		paging = fmt.Sprintf(`{"cursors":{"after":"cursor-after-%s"},"next":"https://graph.facebook.com/next?after=cursor-after-%s"}`, id, id)
	}
	return fmt.Sprintf(`{"data":[{"id":"%s","name":"%s","status":"%s","daily_budget":"1000","lifetime_budget":"","start_time":"2026-01-01T00:00:00-0800","stop_time":""}],"paging":%s}`, id, name, status, paging)
}

func insightsPage(campaignID string, impressions, clicks int, next bool) string {
	paging := `{}`
	if next {
		paging = fmt.Sprintf(`{"cursors":{"after":"cursor-after-%s"},"next":"https://graph.facebook.com/next?after=cursor-after-%s"}`, campaignID, campaignID)
	}
	return fmt.Sprintf(`{"data":[{"campaign_id":"%s","impressions":"%d","clicks":"%d","spend":"12.50"}],"paging":%s}`, campaignID, impressions, clicks, paging)
}

// TestListAccountCampaigns_InsightsWindow_UsesCallerDaysNotHardcoded30 pins the fix for a
// real defect: the ported BFF logic (getMetaAnalytics) hardcoded date_preset=last_30d,
// ignoring its own caller-supplied days window, so a days=7 request silently got 30 days of
// spend. fetchAccountCampaignInsights now renders an explicit time_range from days and the
// client's injected clock (not the wall clock) — this test pins that with WithClock so it
// cannot pass or fail by when it happens to run.
func TestListAccountCampaigns_InsightsWindow_UsesCallerDaysNotHardcoded30(t *testing.T) {
	srv, rec := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{insightsPage("111", 1000, 50, false)},
	)
	fixedNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, AccountConfig{}, WithBaseURL(srv.URL), WithClock(func() time.Time { return fixedNow }))

	if _, err := c.ListAccountCampaigns(context.Background(), "act_123", 7); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}

	var insightsURI string
	for _, u := range rec.all() {
		if strings.Contains(u, "/insights") {
			insightsURI = u
		}
	}
	if insightsURI == "" {
		t.Fatalf("no insights request recorded: %v", rec.all())
	}
	if strings.Contains(insightsURI, "date_preset") {
		t.Errorf("insights request still uses date_preset instead of an explicit time_range: %s", insightsURI)
	}
	// days=7 ending 2026-09-17 (inclusive) starts 2026-09-11 (the days-1 convention shared
	// with Google/Reddit's monitor dispatchers), not the BFF's hardcoded 30-day window.
	wantRange := url.QueryEscape(`{"since":"2026-09-11","until":"2026-09-17"}`)
	if !strings.Contains(insightsURI, "time_range="+wantRange) {
		t.Errorf("insights request URI = %s, want it to contain time_range=%s", insightsURI, wantRange)
	}
}

func TestListAccountCampaigns_SinglePage_ReturnsAllRows(t *testing.T) {
	srv, rec := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{insightsPage("111", 1000, 50, false)},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].CampaignID != "111" || rows[0].Impressions != 1000 || rows[0].Clicks != 50 {
		t.Errorf("unexpected row: %+v", rows[0])
	}
	if rec.count() != 2 {
		t.Errorf("expected exactly one campaign-list request and one insights request, got %d requests: %v", rec.count(), rec.all())
	}
}

// TestListAccountCampaigns_MultiPage_ConcatenatesEveryPage is the pagination-fix regression
// test (LFXV2-2519 Part 3): before this fix, ListAccountCampaigns read only the first page of
// both the campaign list and insights, silently dropping rows past 500 (or effectively past
// whatever the mock's first page holds here). A ported single-page read would return only
// the first campaign's row; the fixed version must return both.
func TestListAccountCampaigns_MultiPage_ConcatenatesEveryPage(t *testing.T) {
	srv, rec := monitorPageResponses(t,
		[]string{
			campaignPage("111", "Campaign One", StatusActive, true),
			campaignPage("222", "Campaign Two", StatusActive, false),
		},
		[]string{
			insightsPage("111", 1000, 50, true),
			insightsPage("222", 2000, 75, false),
		},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (one per page): %+v", len(rows), rows)
	}
	byID := map[string]AccountCampaignRow{rows[0].CampaignID: rows[0], rows[1].CampaignID: rows[1]}
	if r, ok := byID["111"]; !ok || r.Impressions != 1000 || r.Clicks != 50 {
		t.Errorf("missing or wrong row for campaign 111: %+v (ok=%v)", r, ok)
	}
	if r, ok := byID["222"]; !ok || r.Impressions != 2000 || r.Clicks != 75 {
		t.Errorf("missing or wrong row for campaign 222: %+v (ok=%v)", r, ok)
	}
	// Two pages of campaigns + two pages of insights = 4 requests. This asserts the walk
	// actually followed paging.cursors.after rather than stopping after page one.
	if rec.count() != 4 {
		t.Errorf("expected 4 requests (2 campaign pages + 2 insights pages), got %d: %v", rec.count(), rec.all())
	}
	for _, uri := range rec.all() {
		if strings.Contains(uri, "tok-secret-abc") {
			t.Errorf("request URI leaked the access token: %s", uri)
		}
	}
}

// TestListAccountCampaigns_MalformedInsightsRow_MarksFetchFailed pins a real defect found in
// round-14 review: a campaign whose insights row could not be parsed (impressions/clicks not
// numeric) used to end up indistinguishable from a campaign that simply had zero delivery in
// the window — both left FetchFailed at its zero value, so the rule-engine's "underspending"
// action item fired on a measurement failure. fetchAccountCampaignInsights now records the
// campaign id of any row it drops for a parse failure, and ListAccountCampaigns marks that
// row's FetchFailed true instead of leaving it silently zeroed.
func TestListAccountCampaigns_MalformedInsightsRow_MarksFetchFailed(t *testing.T) {
	malformedInsights := `{"data":[{"campaign_id":"111","impressions":"not-a-number","clicks":"50","spend":"12.50"}],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{malformedInsights},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true for a campaign whose insights row failed to parse", rows[0])
	}
	if rows[0].Impressions != 0 || rows[0].Clicks != 0 {
		t.Errorf("row for campaign 111 = %+v, want zero metrics (never fabricated) alongside FetchFailed", rows[0])
	}
}

func TestListAccountCampaigns_MissingCursor_IsAnError(t *testing.T) {
	// paging.next present but no cursors.after: an unusable shape, not a truncated list.
	badPage := `{"data":[{"id":"111","name":"Campaign One","status":"ACTIVE","daily_budget":"1000","lifetime_budget":"","start_time":"2026-01-01T00:00:00-0800","stop_time":""}],"paging":{"next":"https://graph.facebook.com/next"}}`
	srv, _ := monitorPageResponses(t, []string{badPage}, []string{insightsPage("111", 1000, 50, false)})
	c := newMonitorClient(srv)

	if _, err := c.ListAccountCampaigns(context.Background(), "act_123", 30); err == nil {
		t.Fatal("expected an error for a paging.next with no cursor, got nil")
	}
}
