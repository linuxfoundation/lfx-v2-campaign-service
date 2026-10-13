// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// The report bodies below follow the response schema of POST /ad_accounts/{ad_account_id}/reports
// in Reddit's published Ads API v3 OpenAPI document (Metric properties `campaign_id`,
// `impressions`, `clicks`, `spend`, `gender`, `country`, `community`). They prove agreement with
// that schema, not with a live account.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// audienceStub answers the reports operation by (campaign, breakdown) and records every request.
type audienceStub struct {
	t      *testing.T
	answer func(campaignID, breakdown string) (int, string)

	mu       sync.Mutex
	reqs     []reportRequest
	paths    []string
	inFlight atomic.Int64
	maxIn    atomic.Int64
	delay    time.Duration
}

func (s *audienceStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req reportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.t.Errorf("decode report request body: %v", err)
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	cur := s.inFlight.Add(1)
	for {
		m := s.maxIn.Load()
		if cur <= m || s.maxIn.CompareAndSwap(m, cur) {
			break
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.inFlight.Add(-1)
	breakdown := ""
	if len(req.Data.Breakdowns) == 1 {
		breakdown = req.Data.Breakdowns[0]
	}
	status, body := s.answer(campaignIDFromFilter(req.Data.Filter), breakdown)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (s *audienceStub) requests() []reportRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reportRequest(nil), s.reqs...)
}

func newAudienceClient(t *testing.T, s *audienceStub) *Client {
	t.Helper()
	s.t = t
	return newMonitorClient(t, s, fixedRedditClock())
}

// audienceRow renders one report row for campaignID with the breakdown value raw JSON (a quoted
// string or null) under key.
func audienceRow(campaignID, key, rawValue string, impressions, clicks, spend int64) string {
	b, _ := json.Marshal(map[string]any{"campaign_id": campaignID, "impressions": impressions, "clicks": clicks, "spend": spend})
	return strings.TrimSuffix(string(b), "}") + `,"` + key + `":` + rawValue + `}`
}

func audienceReport(rows ...string) string {
	return `{"data":{"metrics":[` + strings.Join(rows, ",") + `]},"pagination":{}}`
}

// healthyAudience answers every (campaign, breakdown) with one or two rows.
func healthyAudience(campaignID, breakdown string) (int, string) {
	switch breakdown {
	case "GENDER":
		return http.StatusOK, audienceReport(
			audienceRow(campaignID, "gender", `"female"`, 600, 30, 3_000_000),
			audienceRow(campaignID, "gender", `null`, 400, 10, 1_000_000))
	case "COUNTRY":
		return http.StatusOK, audienceReport(audienceRow(campaignID, "country", `"US"`, 1000, 40, 4_000_000))
	case "COMMUNITY":
		return http.StatusOK, audienceReport(
			audienceRow(campaignID, "community", `"kubernetes"`, 700, 35, 3_500_000),
			audienceRow(campaignID, "community", `"golang"`, 300, 5, 500_000))
	}
	return http.StatusBadRequest, `{"error":{"message":"unexpected breakdown"}}`
}

func TestGetAudienceInsights_RequestsOneReportPerCampaignPerBreakdownAndSums(t *testing.T) {
	s := &audienceStub{answer: healthyAudience}
	c := newAudienceClient(t, s)

	ai, err := c.GetAudienceInsights(context.Background(), model.MetricsWindowLast7Days, []string{"t3_a", "t3_b", "t3_a"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowLast7Days {
		t.Errorf("window = %q", ai.Window)
	}
	want := []AudienceBucket{
		{Dimension: "gender", Value: "female", Impressions: 1200, Clicks: 60, CostMicros: 6_000_000, Ctr: 0.05},
		{Dimension: "gender", Value: "", Impressions: 800, Clicks: 20, CostMicros: 2_000_000, Ctr: 0.025},
		{Dimension: "country", Value: "US", Impressions: 2000, Clicks: 80, CostMicros: 8_000_000, Ctr: 0.04},
		{Dimension: "community", Value: "kubernetes", Impressions: 1400, Clicks: 70, CostMicros: 7_000_000, Ctr: 0.05},
		{Dimension: "community", Value: "golang", Impressions: 600, Clicks: 10, CostMicros: 1_000_000, Ctr: 10.0 / 600},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i := range want {
		if ai.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], want[i])
		}
	}

	reqs := s.requests()
	// The duplicate scope entry collapses: 2 campaigns × 3 breakdowns.
	if len(reqs) != 6 {
		t.Fatalf("requests = %d, want 6", len(reqs))
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		if len(r.Data.Breakdowns) != 1 {
			t.Fatalf("breakdowns = %v, want exactly one", r.Data.Breakdowns)
		}
		b := r.Data.Breakdowns[0]
		wantFields := []string{"CAMPAIGN_ID", "IMPRESSIONS", "CLICKS", "SPEND", b}
		if strings.Join(r.Data.Fields, ",") != strings.Join(wantFields, ",") {
			t.Errorf("fields = %v, want %v", r.Data.Fields, wantFields)
		}
		id := campaignIDFromFilter(r.Data.Filter)
		if id != "t3_a" && id != "t3_b" {
			t.Errorf("filter = %q, want one owned campaign", r.Data.Filter)
		}
		// The metrics read's range for last_7_days on the fixed clock (2026-07-01).
		if r.Data.StartsAt != "2026-06-25T00:00:00Z" || r.Data.EndsAt != "2026-07-01T23:00:00Z" {
			t.Errorf("range = %s..%s", r.Data.StartsAt, r.Data.EndsAt)
		}
		seen[id+"/"+b] = true
	}
	if len(seen) != 6 {
		t.Errorf("distinct (campaign, breakdown) requests = %v", seen)
	}
	for _, p := range s.paths {
		if p != "POST "+monitorReportsPath {
			t.Errorf("request %q, want only the bound account's reports operation", p)
		}
	}
}

// An explicit empty metrics array is real zero delivery: no buckets, no error.
func TestGetAudienceInsights_EmptyReportIsZeroDelivery(t *testing.T) {
	s := &audienceStub{answer: func(string, string) (int, string) { return http.StatusOK, audienceReport() }}
	ai, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
	if err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if ai.Buckets == nil || len(ai.Buckets) != 0 {
		t.Errorf("buckets = %+v, want empty", ai.Buckets)
	}
}

// Every untrustworthy response fails the WHOLE read: no partial buckets, and no upstream value
// (CANARY) or account id in the error.
func TestGetAudienceInsights_FailsClosed(t *testing.T) {
	good := func(id string) string { return audienceRow(id, "country", `"US"`, 10, 1, 100) }
	for _, tc := range []struct {
		name string
		body func(id string) string
	}{
		{"duplicate top-level data key (empty second)", func(id string) string {
			return `{"data":{"metrics":[` + good(id) + `]},"data":{"metrics":[]},"pagination":{}}`
		}},
		{"case-folded duplicate data key", func(id string) string {
			return `{"data":{"metrics":[` + good(id) + `]},"Data":{"metrics":[]},"pagination":{}}`
		}},
		{"duplicate key inside a row", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":10,"impressions":99999,"clicks":1,"spend":100,"country":"US"}`)
		}},
		{"explicit null counter", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":null,"clicks":0,"spend":0,"country":"US"}`)
		}},
		{"absent counter", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":10,"spend":100,"country":"US"}`)
		}},
		{"non-integer counter", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":10.5,"clicks":1,"spend":100,"country":"US"}`)
		}},
		{"negative counter", func(id string) string { return audienceReport(audienceRow(id, "country", `"US"`, 10, 1, -5)) }},
		{"clicks without impressions", func(id string) string { return audienceReport(audienceRow(id, "country", `"US"`, 0, 3, 5)) }},
		{"row for another campaign", func(string) string { return audienceReport(good("t3_CANARY")) }},
		{"row with no campaign id", func(string) string {
			return audienceReport(`{"impressions":10,"clicks":1,"spend":100,"country":"US"}`)
		}},
		{"breakdown key missing", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":10,"clicks":1,"spend":100}`)
		}},
		{"breakdown value outside the charset", func(id string) string {
			return audienceReport(audienceRow(id, "country", `"<b>CANARY</b>"`, 10, 1, 100))
		}},
		{"breakdown value not a string", func(id string) string { return audienceReport(audienceRow(id, "country", `42`, 10, 1, 100)) }},
		{"duplicate row for one value", func(id string) string { return audienceReport(good(id), good(id)) }},
		{"null data", func(string) string { return `{"data":null,"pagination":{}}` }},
		{"missing metrics", func(string) string { return `{"data":{},"pagination":{}}` }},
		{"null metrics", func(string) string { return `{"data":{"metrics":null},"pagination":{}}` }},
		{"metrics not an array", func(string) string { return `{"data":{"metrics":{"CANARY":1}},"pagination":{}}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{answer: func(id, b string) (int, string) {
				if b == "COUNTRY" {
					return http.StatusOK, tc.body(id)
				}
				return healthyAudience(id, b)
			}}
			ai, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
			if err == nil {
				t.Fatalf("read succeeded with %+v, want a refusal", ai)
			}
			if ai != nil {
				t.Errorf("a refused read returned %+v", ai)
			}
			var te *transportError
			if !errors.As(err, &te) {
				t.Errorf("error = %T (%v), want a transportError (→ 503)", err, err)
			}
			if msg := err.Error(); strings.Contains(msg, "CANARY") || strings.Contains(msg, "t2_test") || strings.Contains(msg, "99999") {
				t.Errorf("error echoes upstream content or the account id: %v", err)
			}
		})
	}
}

// An upstream refusal fails the read too, with the account id redacted from the request path.
func TestGetAudienceInsights_UpstreamFailureRedactsAccount(t *testing.T) {
	s := &audienceStub{answer: func(id, b string) (int, string) {
		if b == "COMMUNITY" {
			return http.StatusForbidden, `{"error":{"message":"CANARY"}}`
		}
		return healthyAudience(id, b)
	}}
	ai, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
	if err == nil || ai != nil {
		t.Fatalf("got (%+v, %v), want a refusal", ai, err)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusForbidden {
		t.Errorf("error = %v, want the 403 apiError in the chain", err)
	}
	if strings.Contains(err.Error(), "t2_test") || strings.Contains(err.Error(), "CANARY") {
		t.Errorf("error leaks the account id or upstream text: %v", err)
	}
}

// A report still paging after breakdownMaxPages is refused rather than truncated.
func TestGetAudienceInsights_PageCapRefuses(t *testing.T) {
	page := 0
	var mu sync.Mutex
	s := &audienceStub{answer: func(id, b string) (int, string) {
		if b != "COUNTRY" {
			return healthyAudience(id, b)
		}
		mu.Lock()
		page++
		n := page
		mu.Unlock()
		next := "/api/v3/ad_accounts/t2_test/reports?page.token=p" + strings.Repeat("x", n)
		return http.StatusOK, `{"data":{"metrics":[]},"pagination":{"next_url":"` + next + `"}}`
	}}
	_, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
	if !errors.Is(err, errBreakdownPageCap) {
		t.Fatalf("error = %v, want the page cap refusal", err)
	}
}

// Refusals decided before Reddit is contacted.
func TestGetAudienceInsights_RefusedBeforeAnyRequest(t *testing.T) {
	tooMany := make([]string, MaxReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = "t3_" + strings.Repeat("a", i+1)
	}
	for _, tc := range []struct {
		name    string
		window  model.MetricsWindow
		account string
		scope   []string
		want    error
	}{
		{"empty scope never widens", model.MetricsWindowLast30Days, "t2_test", nil, ErrReportScopeInvalid},
		{"malformed campaign id", model.MetricsWindowLast30Days, "t2_test", []string{"t3_a", "t3_a,campaign:id==t3_x"}, ErrReportScopeInvalid},
		{"scope too large", model.MetricsWindowLast30Days, "t2_test", tooMany, ErrReportScopeTooLarge},
		{"unsupported window", model.MetricsWindowYesterday, "t2_test", []string{"t3_a"}, ErrUnsupportedWindow},
		{"unaddressable bound account", model.MetricsWindowLast30Days, "t2/../x", []string{"t3_a"}, ErrInvalidAccountID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{answer: healthyAudience}
			c := newAudienceClient(t, s)
			c.account.AccountID = tc.account
			ai, err := c.GetAudienceInsights(context.Background(), tc.window, tc.scope)
			if !errors.Is(err, tc.want) || ai != nil {
				t.Errorf("got (%+v, %v), want %v", ai, err, tc.want)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// The fan-out is bounded: never more than reportBreakdownConcurrency reports in flight.
func TestGetAudienceInsights_BoundsConcurrency(t *testing.T) {
	ids := make([]string, MaxReportCampaigns)
	for i := range ids {
		ids[i] = "t3_" + strings.Repeat("c", i+1)
	}
	s := &audienceStub{answer: healthyAudience, delay: 20 * time.Millisecond}
	if _, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, ids); err != nil {
		t.Fatalf("GetAudienceInsights: %v", err)
	}
	if got := s.maxIn.Load(); got > reportBreakdownConcurrency || got < 2 {
		t.Errorf("max in flight = %d, want 2..%d", got, reportBreakdownConcurrency)
	}
	if n := len(s.requests()); n != MaxReportCampaigns*len(audienceBreakdowns) {
		t.Errorf("requests = %d", n)
	}
}

// Every requested breakdown is in the spec's `breakdowns` AND `fields` enums (checked by hand
// against the published document); AGE is not a breakdown there, so it must never be requested.
func TestAudienceBreakdowns_NoAge(t *testing.T) {
	for _, b := range audienceBreakdowns {
		if b.breakdown == "AGE" || b.dimension == "age" {
			t.Errorf("breakdown %+v: Reddit's report breakdowns enum has no AGE", b)
		}
	}
}

// Two campaigns whose counters for one dimension value are each valid but together exceed int64
// refuse the WHOLE read rather than wrapping — for each counter.
func TestGetAudienceInsights_CrossCampaignOverflowRefused(t *testing.T) {
	const big = int64(math.MaxInt64/2 + 1)
	for name, row := range map[string]func(id string) string{
		"impressions": func(id string) string { return audienceRow(id, "country", `"US"`, big, 1, 1) },
		"clicks":      func(id string) string { return audienceRow(id, "country", `"US"`, 1, big, 1) },
		"spend":       func(id string) string { return audienceRow(id, "country", `"US"`, 1, 1, big) },
	} {
		t.Run(name, func(t *testing.T) {
			s := &audienceStub{answer: func(id, b string) (int, string) {
				if b == "COUNTRY" {
					return http.StatusOK, audienceReport(row(id))
				}
				return healthyAudience(id, b)
			}}
			ai, err := newAudienceClient(t, s).GetAudienceInsights(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a", "t3_b"})
			if err == nil || ai != nil {
				t.Fatalf("got (%+v, %v), want the whole read refused", ai, err)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("error = %v, want the overflow refusal", err)
			}
		})
	}
}
