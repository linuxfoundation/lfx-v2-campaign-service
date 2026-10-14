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
	"sync"
	"testing"
	"time"
)

// monitorPageResponses serves ListAccountCampaigns' upstream reads (campaign list, account
// currency — always USD here — and insights) from canned per-path page bodies, one per request
// to that path, and records every request URI seen — mirroring adAccountsServer in
// accounts_test.go. A test that reaches the ad-set budget read uses monitorServer directly.
func monitorPageResponses(t *testing.T, campaignPages, insightsPages []string) (*httptest.Server, *recordedURIs) {
	t.Helper()
	return monitorServer(t, monitorFixture{campaigns: campaignPages, insights: insightsPages, currency: `{"currency":"USD","id":"act_123"}`})
}

// monitorFixture is every upstream body ListAccountCampaigns can read. currency is the raw body
// of GET /act_…?fields=currency; adSets is served one page per /adsets request, and adSetsFn,
// when set, answers every /adsets request instead (for walks longer than a canned list).
type monitorFixture struct {
	campaigns, insights, adSets []string
	currency                    string
	adSetsFn                    func(n int) string
	// adSetsStatus, when set, answers every /adsets request with that status and a Graph error.
	adSetsStatus int
}

func monitorServer(t *testing.T, f monitorFixture) (*httptest.Server, *recordedURIs) {
	t.Helper()
	rec := &recordedURIs{}
	var mu sync.Mutex
	var campaignN, insightsN, adSetsN int
	serve := func(w http.ResponseWriter, what string, pages []string, n *int) {
		if *n >= len(pages) {
			t.Errorf("%s asked for page %d but only %d were canned", what, *n+1, len(pages))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(pages[*n]))
		*n++
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.RequestURI())
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			serve(w, "campaign list", f.campaigns, &campaignN)
		case strings.HasSuffix(r.URL.Path, "/insights"):
			serve(w, "insights", f.insights, &insightsN)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			if f.adSetsStatus != 0 {
				w.WriteHeader(f.adSetsStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"Invalid parameter","type":"OAuthException","code":100}}`))
				return
			}
			if f.adSetsFn != nil {
				_, _ = w.Write([]byte(f.adSetsFn(adSetsN)))
				adSetsN++
				return
			}
			serve(w, "ad-set budgets", f.adSets, &adSetsN)
		case r.URL.Path == "/act_123" && r.URL.Query().Get("fields") == "currency":
			_, _ = w.Write([]byte(f.currency))
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

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	// The rows carry the bounds the read SENT, so the rules layer paces against exactly the days
	// the spend covers rather than re-deriving them from its own clock (PR #316 review).
	if len(rows) != 1 || rows[0].MetricsSince != "2026-09-11" || rows[0].MetricsUntil != "2026-09-17" {
		t.Errorf("rows = %+v, want MetricsSince/MetricsUntil 2026-09-11/2026-09-17, the time_range sent", rows)
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
	// The campaign carries a campaign-level budget, so no ad-set read is made.
	if rec.count() != 3 {
		t.Errorf("expected exactly one campaign-list, one account-currency and one insights request, got %d requests: %v", rec.count(), rec.all())
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
	// Two pages of campaigns + one currency read + two pages of insights = 5 requests. This
	// asserts the walk actually followed paging.cursors.after rather than stopping after page one.
	if rec.count() != 5 {
		t.Errorf("expected 5 requests (2 campaign pages + 1 currency + 2 insights pages), got %d: %v", rec.count(), rec.all())
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

// TestListAccountCampaigns_PausedCampaignFetchFailed_IsNotDropped pins a real defect found in
// round-15 review: the campaign-list filter ported from getMetaAnalytics
// (`impressions>0 || status==='ACTIVE'`) silently dropped a PAUSED campaign whose insights row
// failed to parse, since FetchFailed leaves Impressions at 0 and the campaign is not ACTIVE.
// That made the fetch failure invisible instead of surfacing it — the opposite of what
// FetchFailed exists to do. The filter now also admits a row with FetchFailed=true.
func TestListAccountCampaigns_PausedCampaignFetchFailed_IsNotDropped(t *testing.T) {
	malformedInsights := `{"data":[{"campaign_id":"111","impressions":"not-a-number","clicks":"50","spend":"12.50"}],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusPaused, false)},
		[]string{malformedInsights},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (a PAUSED campaign with a failed fetch must still surface): %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true", rows[0])
	}
}

// TestListAccountCampaigns_MalformedSpend_MarksFetchFailed pins a round-19 review fix:
// spend used to be parsed with its error discarded (`strconv.ParseFloat(row.Spend, 64)`
// followed by `_`), so a malformed spend value produced a fully-trusted $0 row instead of
// FetchFailed — exactly the false-zero failure mode this range's impressions/clicks gate
// already prevented for the sibling fields.
func TestListAccountCampaigns_MalformedSpend_MarksFetchFailed(t *testing.T) {
	malformedSpend := `{"data":[{"campaign_id":"111","impressions":"100","clicks":"50","spend":"not-a-number"}],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{malformedSpend},
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
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true for a campaign whose spend failed to parse", rows[0])
	}
	if rows[0].Impressions != 0 || rows[0].Clicks != 0 || rows[0].SpendUSD != 0 {
		t.Errorf("row for campaign 111 = %+v, want zero metrics (never fabricated) alongside FetchFailed", rows[0])
	}
}

// TestListAccountCampaigns_EmptySpend_IsLegitimateZero confirms an OMITTED spend field
// (Meta's convention for a zero-delivery campaign, decoded as "") is NOT treated as a parse
// failure — only a non-empty, unparseable value is.
func TestListAccountCampaigns_EmptySpend_IsLegitimateZero(t *testing.T) {
	emptySpend := `{"data":[{"campaign_id":"111","impressions":"0","clicks":"0","spend":""}],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{emptySpend},
	)
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=false for a legitimately empty spend field", rows[0])
	}
	if rows[0].SpendUSD != 0 {
		t.Errorf("row for campaign 111 = %+v, want SpendUSD=0", rows[0])
	}
}

// TestListAccountCampaigns_MalformedDailyBudget_MarksFetchFailed pins a round-31+ review fix
// mirroring TestListAccountCampaigns_MalformedSpend_MarksFetchFailed: minorUnitsToWhole used
// to swallow a parse error and return 0, converting an upstream-data failure (a non-empty,
// unparseable daily_budget) into a trusted zero budget instead of surfacing it as unreliable.
func TestListAccountCampaigns_MalformedDailyBudget_MarksFetchFailed(t *testing.T) {
	badBudget := `{"data":[{"id":"111","name":"Campaign One","status":"ACTIVE","daily_budget":"not-a-number","lifetime_budget":"","start_time":"2026-01-01T00:00:00-0800","stop_time":""}],"paging":{}}`
	srv, _ := monitorPageResponses(t, []string{badBudget}, []string{insightsPage("111", 1000, 50, false)})
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if !rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=true for a campaign whose daily_budget failed to parse", rows[0])
	}
	if rows[0].DailyBudget != 0 {
		t.Errorf("row for campaign 111 = %+v, want DailyBudget=0 (never fabricated) alongside FetchFailed", rows[0])
	}
	// A malformed budget must not mask a successful insights read.
	if rows[0].Impressions != 1000 || rows[0].Clicks != 50 {
		t.Errorf("row for campaign 111 = %+v, want the successfully-read insights metrics preserved alongside the budget failure", rows[0])
	}
}

// TestListAccountCampaigns_EmptyBudget_IsLegitimateZero confirms an omitted/empty budget
// string is NOT treated as a parse failure — only a non-empty, unparseable value is.
func TestListAccountCampaigns_EmptyBudget_IsLegitimateZero(t *testing.T) {
	emptyBudget := `{"data":[{"id":"111","name":"Campaign One","status":"ACTIVE","daily_budget":"","lifetime_budget":"","start_time":"2026-01-01T00:00:00-0800","stop_time":""}],"paging":{}}`
	// No campaign-level budget sends the read to the ad sets; this account has none.
	srv, _ := monitorServer(t, monitorFixture{
		campaigns: []string{emptyBudget}, insights: []string{insightsPage("111", 1000, 50, false)},
		currency: `{"currency":"USD"}`, adSets: []string{`{"data":[],"paging":{}}`},
	})
	c := newMonitorClient(srv)

	rows, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].FetchFailed {
		t.Errorf("row for campaign 111 = %+v, want FetchFailed=false for a legitimately empty budget field", rows[0])
	}
	if rows[0].DailyBudget != 0 {
		t.Errorf("row for campaign 111 = %+v, want DailyBudget=0", rows[0])
	}
}

// TestDateOnly pins the round-31+ review fix: dateOnly used to blindly slice the first 10
// characters without validating they form a real calendar date, so a malformed timestamp
// (e.g. all zeros) silently passed through as a fabricated date instead of yielding "".
func TestDateOnly(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"valid timestamp", "2026-01-15T00:00:00-0800", "2026-01-15"},
		{"too short", "2026-01", ""},
		{"empty", "", ""},
		{"not a real calendar date", "0000-00-00T00:00:00-0800", ""},
		{"malformed prefix", "not-a-date00", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dateOnly(tc.in); got != tc.want {
				t.Errorf("dateOnly(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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

// TestListAccountCampaigns_InsightsRowMissingCampaignID_IsRejected pins the round-31 review
// fix at monitor.go:284-291: an insights row with no campaign_id cannot be attributed to any
// specific campaign — there is no id to mark FetchFailed on individually, unlike the malformed
// impressions/clicks/spend cases above. Silently skipping the row instead of rejecting the
// whole read would let ListAccountCampaigns read that campaign's real activity as a legitimate
// zero, restoring the exact false-zero-delivery bug Copilot flagged on this range (PR #215
// post-merge review comment).
func TestListAccountCampaigns_InsightsRowMissingCampaignID_IsRejected(t *testing.T) {
	insightsMissingCampaignID := `{"data":[{"campaign_id":"","impressions":"1000","clicks":"50","spend":"12.50"}],"paging":{}}`
	srv, _ := monitorPageResponses(t,
		[]string{campaignPage("111", "Campaign One", StatusActive, false)},
		[]string{insightsMissingCampaignID},
	)
	c := newMonitorClient(srv)

	_, err := c.ListAccountCampaigns(context.Background(), "act_123", 30)
	if err == nil {
		t.Fatal("an insights row with no campaign_id was accepted, silently attributing its metrics to no campaign")
	}
	if !strings.Contains(err.Error(), "campaign_id") {
		t.Errorf("error = %v, want it to report the missing campaign_id", err)
	}
}
