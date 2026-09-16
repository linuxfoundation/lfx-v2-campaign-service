// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestListAccountCampaigns_SinglePage_ReturnsAllRows(t *testing.T) {
	srv, rec := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{insightsPage("111", 1000, 50, false)},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123")
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

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123")
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

func TestListAccountCampaigns_MissingCursor_IsAnError(t *testing.T) {
	// paging.next present but no cursors.after: an unusable shape, not a truncated list.
	badPage := `{"data":[{"id":"111","name":"Campaign One","status":"ACTIVE","daily_budget":"1000","lifetime_budget":"","start_time":"2026-01-01T00:00:00-0800","stop_time":""}],"paging":{"next":"https://graph.facebook.com/next"}}`
	srv, _ := monitorPageResponses(t, []string{badPage}, []string{insightsPage("111", 1000, 50, false)})
	c := newMonitorClient(srv)

	if _, err := c.ListAccountCampaigns(context.Background(), "act_123"); err == nil {
		t.Fatal("expected an error for a paging.next with no cursor, got nil")
	}
}
