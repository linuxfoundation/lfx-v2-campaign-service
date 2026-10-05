// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

// The report bodies below are modelled on the same published-schema shapes metrics_test.go
// uses (see that file's header): they prove agreement with Reddit's PUBLISHED reporting
// schema, not with a live account. The campaign-list bodies and every "pagination.next_url"
// are not schema-verified at all — see paginationEnvelope's VERIFICATION LEVEL note.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	monitorCampaignsPath = "/api/v3/ad_accounts/t2_test/campaigns"
	monitorReportsPath   = "/api/v3/ad_accounts/t2_test/reports"
)

// tokenHandlerReturning is a tiny OAuth token-endpoint stub shared by the tests below.
func tokenHandlerReturning(accessToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken, "expires_in": 3600})
	}
}

// manyActiveCampaignsBody builds a bare-array campaign-list response with n ACTIVE campaigns,
// enough to exceed monitorReportConcurrency so a concurrency-bound regression has something to
// pin against.
func manyActiveCampaignsBody(n int) string {
	elements := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		elements = append(elements, map[string]any{
			"id":                fmt.Sprintf("camp%d", i),
			"name":              fmt.Sprintf("Campaign %d", i),
			"configured_status": StatusActive,
		})
	}
	b, err := json.Marshal(elements)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// reportRequest is the decoded body of one POST to the verified reports operation.
type reportRequest struct {
	Data struct {
		StartsAt   string   `json:"starts_at"`
		EndsAt     string   `json:"ends_at"`
		Fields     []string `json:"fields"`
		Filter     string   `json:"filter"`
		Breakdowns []string `json:"breakdowns"`
	} `json:"data"`
}

// campaignIDFromFilter extracts <id> from the filter "campaign:id==<id>", or "" when the
// filter is not that shape.
func campaignIDFromFilter(filter string) string {
	id, ok := strings.CutPrefix(filter, "campaign:id==")
	if !ok {
		return ""
	}
	return id
}

// echoReportRow is a healthy single-row report attributed to campaignID.
func echoReportRow(campaignID string) string {
	return `{"data":{"metrics":[{"campaign_id":"` + campaignID + `","impressions":100,"clicks":10,"spend":5000000}]},"pagination":{}}`
}

// monitorStub is an API stub serving the campaign list (listBody) and the verified reports
// operation (report, keyed by the campaign id in the request's filter). It records every
// request it sees. It never calls t.Fatal: a decode failure is reported with t.Errorf and
// answered with a 400.
type monitorStub struct {
	t        *testing.T
	listBody string
	report   func(campaignID string) (status int, body string)

	mu       sync.Mutex
	paths    []string
	reports  []reportRequest
	queries  []string
	inFlight atomic.Int64
	maxIn    atomic.Int64
	delay    time.Duration
}

func (s *monitorStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.queries = append(s.queries, r.URL.RawQuery)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == monitorCampaignsPath:
		_, _ = w.Write([]byte(s.listBody))
	case r.Method == http.MethodPost && r.URL.Path == monitorReportsPath:
		var req reportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.t.Errorf("decode report request body: %v", err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.reports = append(s.reports, req)
		s.mu.Unlock()

		cur := s.inFlight.Add(1)
		for {
			prev := s.maxIn.Load()
			if cur <= prev || s.maxIn.CompareAndSwap(prev, cur) {
				break
			}
		}
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		s.inFlight.Add(-1)

		status, body := s.report(campaignIDFromFilter(req.Data.Filter))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	default:
		// Any other path — the unverified nested /campaigns/{id}/reports included — is a 404,
		// so a regression onto it shows up as FetchFailed rows, not as passing data.
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *monitorStub) requestedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *monitorStub) reportRequests() []reportRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]reportRequest(nil), s.reports...)
}

// newMonitorClient wires a client to handler with the given clock.
func newMonitorClient(t *testing.T, handler http.Handler, now func() time.Time) *Client {
	t.Helper()
	apiSrv := httptest.NewServer(handler)
	t.Cleanup(apiSrv.Close)
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(now))
}

func healthyReport(campaignID string) (int, string) { return http.StatusOK, echoReportRow(campaignID) }

// TestListAccountCampaigns_BoundsReportConcurrency pins the round-19 review fix: the
// per-campaign report fan-out must run bounded-concurrently (monitorReportConcurrency, 5) rather
// than serially, so an account with several campaigns cannot exceed its caller's fixed read
// deadline even when each individual request is healthy. maxIn is the deterministic proof of
// boundedness; delay only holds each request open long enough for concurrent ones to pile up and
// is never compared against a wall-clock budget (round-19 review).
func TestListAccountCampaigns_BoundsReportConcurrency(t *testing.T) {
	const numCampaigns = 12
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(numCampaigns) + `}`, report: healthyReport, delay: 50 * time.Millisecond}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != numCampaigns {
		t.Fatalf("got %d rows, want %d", len(rows), numCampaigns)
	}
	for _, row := range rows {
		if row.FetchFailed {
			t.Errorf("row %+v: unexpected FetchFailed on a healthy report response", row)
		}
	}
	if got := stub.maxIn.Load(); got > int64(monitorReportConcurrency) {
		t.Errorf("max concurrent report requests = %d, want <= %d (monitorReportConcurrency)", got, monitorReportConcurrency)
	}
}

// TestListAccountCampaigns_ReportWindowIncludesToday pins the window rendering against a fixed
// clock. A days=N read covers today-(N-1) through today INCLUSIVE, rendered by reportRange — the
// same renderer GetCampaignMetrics uses — so ends_at is today's 23:00 hour. The ported BFF
// rendered ends_at as today's T00:00:00Z, which stops the range as today begins, so a days=7 read
// covered six days. A mid-day clock must render the same whole-day bounds.
func TestListAccountCampaigns_ReportWindowIncludesToday(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 15, 30, 0, 0, time.UTC),
		// Same UTC instant expressed in another zone: the window is UTC-anchored.
		time.Date(2026, 7, 1, 8, 30, 0, 0, time.FixedZone("PDT", -7*3600)),
	} {
		t.Run(now.String(), func(t *testing.T) {
			stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(1) + `}`, report: healthyReport}
			c := newMonitorClient(t, stub, func() time.Time { return now })

			if _, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7); err != nil {
				t.Fatalf("ListAccountCampaigns: %v", err)
			}
			reqs := stub.reportRequests()
			if len(reqs) != 1 {
				t.Fatalf("got %d report requests, want 1", len(reqs))
			}
			if got, want := reqs[0].Data.StartsAt, "2026-06-25T00:00:00Z"; got != want {
				t.Errorf("starts_at = %q, want %q (today-(days-1))", got, want)
			}
			if got, want := reqs[0].Data.EndsAt, "2026-07-01T23:00:00Z"; got != want {
				t.Errorf("ends_at = %q, want %q — today's last addressable hour, not the midnight that begins it", got, want)
			}
		})
	}
}

// TestListAccountCampaigns_UsesVerifiedReportOperation pins that the monitor reads through the
// ONE reporting operation this repository has verified against Reddit's published spec — POST
// /ad_accounts/{id}/reports — with GetCampaignMetrics' own body: UPPERCASE fields with
// CAMPAIGN_ID requested as a field, the campaign scoped by the filter DSL, and no breakdowns.
// The nested /campaigns/{id}/reports path it used to call is unverified and must not be reached.
func TestListAccountCampaigns_UsesVerifiedReportOperation(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(2) + `}`, report: healthyReport}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	for _, row := range rows {
		if row.FetchFailed || row.Impressions != 100 || row.Clicks != 10 || row.SpendUSD != 5 || row.Ctr != 10 {
			t.Errorf("row %+v: want the echoed report's 100 impressions, 10 clicks, $5, 10%% CTR", row)
		}
	}
	for _, p := range stub.requestedPaths() {
		if strings.Contains(p, "/campaigns/") {
			t.Errorf("requested %q — the unverified per-campaign nested report path", p)
		}
	}
	reqs := stub.reportRequests()
	if len(reqs) != 2 {
		t.Fatalf("got %d report requests, want one per campaign (2)", len(reqs))
	}
	filters := map[string]bool{}
	for _, r := range reqs {
		filters[r.Data.Filter] = true
		if got := strings.Join(r.Data.Fields, ","); got != "CAMPAIGN_ID,IMPRESSIONS,CLICKS,SPEND" {
			t.Errorf("fields = %q, want CAMPAIGN_ID,IMPRESSIONS,CLICKS,SPEND", got)
		}
		if r.Data.Breakdowns != nil {
			t.Errorf("breakdowns = %v, want omitted", r.Data.Breakdowns)
		}
	}
	if !filters["campaign:id==camp0"] || !filters["campaign:id==camp1"] {
		t.Errorf("filters = %v, want campaign:id==camp0 and campaign:id==camp1", filters)
	}
}

// TestListAccountCampaigns_ReportProvenance pins that every report row is attributed by its own
// CAMPAIGN_ID before it is counted, exactly as GetCampaignMetrics does. A row for another
// campaign (a filter Reddit parsed differently than intended) or a row with no campaign_id at all
// cannot be attributed, so that campaign is FetchFailed rather than credited with numbers that
// may not be its own. Siblings are unaffected.
func TestListAccountCampaigns_ReportProvenance(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"row for another campaign", `{"data":{"metrics":[{"campaign_id":"camp_other","impressions":100,"clicks":10,"spend":5000000}]}}`},
		{"row with no campaign_id", `{"data":{"metrics":[{"impressions":100,"clicks":10,"spend":5000000}]}}`},
		{"second row for another campaign", `{"data":{"metrics":[` +
			`{"campaign_id":"camp1","impressions":100,"clicks":10,"spend":5000000},` +
			`{"campaign_id":"camp_other","impressions":100,"clicks":10,"spend":5000000}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: func(id string) (int, string) {
				if id == "camp1" {
					return http.StatusOK, tt.body
				}
				return healthyReport(id)
			}}
			c := newMonitorClient(t, stub, fixedRedditClock())

			rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
			if err != nil {
				t.Fatalf("ListAccountCampaigns: %v", err)
			}
			for _, row := range rows {
				if row.CampaignID == "camp1" {
					if !row.FetchFailed || row.Impressions != 0 {
						t.Errorf("row %+v: want FetchFailed and no credited metrics for an unattributable report", row)
					}
					continue
				}
				if row.FetchFailed {
					t.Errorf("row %+v: a sibling's unattributable report must not fail this row", row)
				}
			}
		})
	}
}

// TestListAccountCampaigns_SumsMultipleReportRows pins that every row of a campaign's report is
// summed. The monitor used to read metrics[0] only, silently dropping the rest of a report Reddit
// split into several rows. CTR is derived from the totals, not averaged.
func TestListAccountCampaigns_SumsMultipleReportRows(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(1) + `}`, report: func(id string) (int, string) {
		return http.StatusOK, `{"data":{"metrics":[` +
			`{"campaign_id":"camp0","impressions":1000,"clicks":40,"spend":10000000},` +
			`{"campaign_id":"camp0","impressions":600,"clicks":20,"spend":5250000},` +
			`{"campaign_id":"camp0","impressions":400,"clicks":20,"spend":750000}]}}`
	}}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.FetchFailed || r.Impressions != 2000 || r.Clicks != 80 || r.SpendUSD != 16 || r.Ctr != 4 {
		t.Errorf("row = %+v; want 2000 impressions, 80 clicks, $16, 4%% CTR summed across all three rows", r)
	}
}

// TestListAccountCampaigns_EmptyReportIsZeroActivity pins that an explicit empty metrics array
// is real zero activity (as GetCampaignMetrics reads it), while a missing metrics array is a
// failure, never a fabricated zero.
func TestListAccountCampaigns_EmptyReportIsZeroActivity(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(2) + `}`, report: func(id string) (int, string) {
		if id == "camp0" {
			return http.StatusOK, `{"data":{"metrics":[]}}`
		}
		return http.StatusOK, `{"data":{}}`
	}}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	for _, row := range rows {
		switch row.CampaignID {
		case "camp0":
			if row.FetchFailed || row.Impressions != 0 {
				t.Errorf("row %+v: an explicit empty metrics array is zero activity, not a failure", row)
			}
		case "camp1":
			if !row.FetchFailed {
				t.Errorf("row %+v: a report with no metrics array must be FetchFailed, not a zero", row)
			}
		}
	}
}

// TestListAccountCampaigns_FollowsReportPagination pins that a report split across pages is
// read to its last page and summed, the next page re-POSTing the same body.
func TestListAccountCampaigns_FollowsReportPagination(t *testing.T) {
	var page atomic.Int64
	var base string
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(1) + `}`}
	stub.report = func(id string) (int, string) {
		if page.Add(1) == 1 {
			return http.StatusOK, `{"data":{"metrics":[{"campaign_id":"` + id + `","impressions":100,"clicks":10,"spend":1000000}]},` +
				`"pagination":{"next_url":"` + base + `/api/v3/ad_accounts/t2_test/reports?page.token=p2"}}`
		}
		return http.StatusOK, `{"data":{"metrics":[{"campaign_id":"` + id + `","impressions":50,"clicks":5,"spend":2000000}]},"pagination":{"next_url":null}}`
	}
	srv := httptest.NewUnstartedServer(stub)
	base = "http://" + srv.Listener.Addr().String()
	srv.Start()
	t.Cleanup(srv.Close)
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(base+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 || rows[0].FetchFailed || rows[0].Impressions != 150 || rows[0].SpendUSD != 3 {
		t.Fatalf("rows = %+v; want one row summing both pages (150 impressions, $3)", rows)
	}
	reqs := stub.reportRequests()
	if len(reqs) != 2 || reqs[1].Data.Filter != "campaign:id==camp0" {
		t.Errorf("report requests = %+v; want the same filtered body re-sent for page 2", reqs)
	}
	stub.mu.Lock()
	queries := append([]string(nil), stub.queries...)
	stub.mu.Unlock()
	if queries[len(queries)-1] != "page.token=p2" {
		t.Errorf("last request query = %q, want page.token=p2 from next_url", queries[len(queries)-1])
	}
}

// pagedListServer serves the campaign list as pages: page i (1-based, from the "page" query)
// returns pages[i-1], linking to page i+1 while one exists. Reports echo a healthy row.
func pagedListServer(t *testing.T, pages []string, nextFor func(base string, page int) string) (*Client, *atomic.Int64) {
	t.Helper()
	var listCalls atomic.Int64
	var base string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == monitorCampaignsPath:
			listCalls.Add(1)
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				_, _ = fmt.Sscanf(p, "%d", &page)
			}
			body := `[]`
			if page-1 < len(pages) {
				body = pages[page-1]
			}
			next := nextFor(base, page)
			pag := `{}`
			if next != "" {
				pag = `{"next_url":"` + next + `"}`
			}
			_, _ = w.Write([]byte(`{"data":` + body + `,"pagination":` + pag + `}`))
		case r.Method == http.MethodPost && r.URL.Path == monitorReportsPath:
			var req reportRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode report request body: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(echoReportRow(campaignIDFromFilter(req.Data.Filter))))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	srv := httptest.NewUnstartedServer(h)
	base = "http://" + srv.Listener.Addr().String()
	srv.Start()
	t.Cleanup(srv.Close)
	tokenSrv := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tokenSrv.Close)
	c := NewClient(testCreds, testAccount, WithBaseURL(base+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
	return c, &listCalls
}

// TestListAccountCampaigns_FollowsCampaignListPagination pins that every page of the campaign
// list is read. apiResponse used to drop the pagination envelope, so only the first page ever
// reached the monitor and the rest of the account's campaigns silently vanished from it.
func TestListAccountCampaigns_FollowsCampaignListPagination(t *testing.T) {
	pages := []string{
		`[{"id":"a1","name":"A1","configured_status":"ACTIVE"},{"id":"a2","name":"A2","configured_status":"ARCHIVED"}]`,
		`{"campaigns":[{"id":"b1","name":"B1","configured_status":"PAUSED"}]}`,
		`[{"id":"c1","name":"C1","configured_status":"ACTIVE"}]`,
	}
	c, calls := pagedListServer(t, pages, func(base string, page int) string {
		if page < 3 {
			return fmt.Sprintf("%s/api/v3/ad_accounts/t2_test/campaigns?page=%d", base, page+1)
		}
		return ""
	})

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	got := []string{}
	for _, r := range rows {
		got = append(got, r.CampaignID)
	}
	if strings.Join(got, ",") != "a1,b1,c1" {
		t.Errorf("campaigns = %v, want a1,b1,c1 (every page, ARCHIVED filtered out)", got)
	}
	if calls.Load() != 3 {
		t.Errorf("campaign-list calls = %d, want 3", calls.Load())
	}
}

// TestListAccountCampaigns_CampaignListPaginationFailsLoudly pins that a page walk that cannot
// finish is an ERROR, never a truncated list presented as complete: a walk still paging at
// monitorMaxPages, a next link that repeats an earlier page, a next link off the API origin
// (following it would send the bearer token there), and one campaign returned on two pages.
func TestListAccountCampaigns_CampaignListPaginationFailsLoudly(t *testing.T) {
	onePage := func(page int) string {
		return fmt.Sprintf(`[{"id":"p%d","name":"P","configured_status":"ACTIVE"}]`, page)
	}
	endless := make([]string, monitorMaxPages+5)
	for i := range endless {
		endless[i] = onePage(i + 1)
	}

	tests := []struct {
		name      string
		pages     []string
		next      func(base string, page int) string
		wantCalls int64
		wantErr   string
	}{
		{
			name:  "page cap reached with more pages pending",
			pages: endless,
			next: func(base string, page int) string {
				return fmt.Sprintf("%s/api/v3/ad_accounts/t2_test/campaigns?page=%d", base, page+1)
			},
			wantCalls: monitorMaxPages,
			wantErr:   "more pages",
		},
		{
			name:  "next link repeats an earlier page",
			pages: []string{onePage(1), onePage(2)},
			next: func(base string, page int) string {
				return base + "/api/v3/ad_accounts/t2_test/campaigns?page=2"
			},
			wantCalls: 2,
			wantErr:   "repeated an earlier page",
		},
		{
			name:  "next link off the API origin",
			pages: []string{onePage(1)},
			next: func(string, int) string {
				return "https://attacker.example/api/v3/ad_accounts/t2_test/campaigns?page=2"
			},
			wantCalls: 1,
			wantErr:   "off the API origin",
		},
		{
			name:  "same campaign on two pages",
			pages: []string{onePage(1), onePage(1)},
			next: func(base string, page int) string {
				if page == 1 {
					return base + "/api/v3/ad_accounts/t2_test/campaigns?page=2"
				}
				return ""
			},
			wantCalls: 2,
			wantErr:   "same campaign twice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := pagedListServer(t, tt.pages, tt.next)
			rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
			if err == nil {
				t.Fatalf("got rows=%+v, err=nil; want an error, not a truncated list", rows)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "t2_test") || strings.Contains(err.Error(), "attacker") {
				t.Errorf("err = %v leaks the account id or the upstream next_url", err)
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("campaign-list calls = %d, want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

// TestListAccountCampaigns_GoalTypeRoutesBudget pins the goal_type decode. goal_value is a
// lifetime budget only under LIFETIME_SPEND and a daily one under DAILY_SPEND; with any other
// goal_type, or none, this read cannot tell what the number is a budget FOR and sets neither, so
// the rule engine reports pacing as unknown instead of pacing against a guess. The BFF read every
// goal_value as a lifetime total, so a daily cap was prorated across the whole flight.
func TestListAccountCampaigns_GoalTypeRoutesBudget(t *testing.T) {
	list := `{"data":[
		{"id":"life","name":"L","configured_status":"ACTIVE","goal_type":"LIFETIME_SPEND","goal_value":100000000},
		{"id":"daily","name":"D","configured_status":"ACTIVE","goal_type":"DAILY_SPEND","goal_value":25000000},
		{"id":"absent","name":"A","configured_status":"ACTIVE","goal_value":100000000},
		{"id":"unknown","name":"U","configured_status":"ACTIVE","goal_type":"SOMETHING_NEW","goal_value":100000000},
		{"id":"novalue","name":"N","configured_status":"ACTIVE","goal_type":"DAILY_SPEND"}
	]}`
	stub := &monitorStub{t: t, listBody: list, report: healthyReport}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	want := map[string][2]float64{ // {TotalBudget, DailyBudget}
		"life":    {100, 0},
		"daily":   {0, 25},
		"absent":  {0, 0},
		"unknown": {0, 0},
		"novalue": {0, 0},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		w := want[r.CampaignID]
		if r.TotalBudget != w[0] || r.DailyBudget != w[1] {
			t.Errorf("%s: TotalBudget, DailyBudget = %v, %v; want %v, %v", r.CampaignID, r.TotalBudget, r.DailyBudget, w[0], w[1])
		}
	}
}

// TestListAccountCampaigns_PerCampaignReportFailure_MarksOnlyThatRowFailed pins that a single
// campaign's failed report call marks only that campaign's row FetchFailed and never cancels
// or degrades the other campaigns' in-flight reads — the reason ListAccountCampaigns's fan-out
// never propagates a per-item error out of errgroup.Go.
func TestListAccountCampaigns_PerCampaignReportFailure_MarksOnlyThatRowFailed(t *testing.T) {
	const failingCampaignID = "camp1"
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: func(id string) (int, string) {
		if id == failingCampaignID {
			return http.StatusInternalServerError, `{"error":"internal"}`
		}
		return healthyReport(id)
	}}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == failingCampaignID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for the campaign whose report call failed", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's report failure must not mark this healthy row FetchFailed", row)
		}
		if row.Impressions != 100 || row.Clicks != 10 {
			t.Errorf("row %+v: want the healthy metrics from its own report response", row)
		}
	}
}

// TestListAccountCampaigns_MalformedCampaignID_MarksFetchFailedWithoutRequest pins the round-23
// review fix: a Reddit-returned campaign id that fails accountIDRe must mark that row FetchFailed
// and never reach a report request. The id is interpolated into the report's filter DSL, where a
// comma would split one term into two and silently widen the report to another campaign.
func TestListAccountCampaigns_MalformedCampaignID_MarksFetchFailedWithoutRequest(t *testing.T) {
	const malformedID = "camp_1,campaign:id==camp_999"
	body := `{"data":[
		{"id":"` + malformedID + `","name":"Malformed","configured_status":"ACTIVE"},
		{"id":"campOK","name":"OK","configured_status":"ACTIVE"}
	]}`
	stub := &monitorStub{t: t, listBody: body, report: healthyReport}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == malformedID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for a campaign id that fails accountIDRe", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's malformed id must not mark this healthy row FetchFailed", row)
		}
	}
	for _, r := range stub.reportRequests() {
		if r.Data.Filter != "campaign:id==campOK" {
			t.Errorf("report requested with filter %q — the malformed id must never reach a request", r.Data.Filter)
		}
	}
}

// TestListAccountCampaigns_NoStartTime_LeavesStartDateEmpty pins the other half of the round-23
// review fix: a campaign whose start_time Reddit never reported must NOT have its StartDate
// seeded from the report window. An empty StartDate is EvaluateRedditMonitor's signal to set
// PacingUnknown instead of computing a pacing verdict against a flight the campaign never had.
func TestListAccountCampaigns_NoStartTime_LeavesStartDateEmpty(t *testing.T) {
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(1) + `}`, report: healthyReport}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].StartDate != "" {
		t.Errorf("StartDate = %q, want empty — manyActiveCampaignsBody sends no start_time, so it must not be seeded from the report window", rows[0].StartDate)
	}
	if rows[0].EndDate != "" {
		t.Errorf("EndDate = %q, want empty for the same reason", rows[0].EndDate)
	}
}

// TestListAccountCampaigns_MalformedReportJSON_MarksFetchFailed pins the round-21 review fix:
// a report response that isn't the expected report shape must mark that row FetchFailed rather
// than being read as a legitimate zero-delivery measurement, which is what the BFF's
// optional-chaining `?.metrics ?? []` did.
func TestListAccountCampaigns_MalformedReportJSON_MarksFetchFailed(t *testing.T) {
	const malformedCampaignID = "camp1"
	stub := &monitorStub{t: t, listBody: `{"data":` + manyActiveCampaignsBody(3) + `}`, report: func(id string) (int, string) {
		if id == malformedCampaignID {
			return http.StatusOK, `{"data":"not-the-expected-shape"}`
		}
		return healthyReport(id)
	}}
	c := newMonitorClient(t, stub, fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.CampaignID == malformedCampaignID {
			if !row.FetchFailed {
				t.Errorf("row %+v: want FetchFailed=true for a malformed report body, not a fabricated zero", row)
			}
			continue
		}
		if row.FetchFailed {
			t.Errorf("row %+v: a sibling campaign's malformed report must not mark this healthy row FetchFailed", row)
		}
	}
}

// TestListAccountCampaigns_MalformedCampaignListShape_ReturnsError pins the round-30+ review
// fix: a campaign-list body matching neither the bare-array nor the {"campaigns": [...]} shape
// must surface as an error, not as a fabricated empty account.
func TestListAccountCampaigns_MalformedCampaignListShape_ReturnsError(t *testing.T) {
	c := newMonitorClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":"not-an-array-or-a-campaigns-object"}`))
	}), fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err == nil {
		t.Fatalf("ListAccountCampaigns: got rows=%+v, err=nil; want an error for a malformed campaign-list shape, not a silent empty account", rows)
	}
}

// TestListAccountCampaigns_EmptyCampaignListBody_ReturnsNoRowsWithoutError confirms the
// legitimately-empty case (no body at all) is still NOT treated as malformed.
func TestListAccountCampaigns_EmptyCampaignListBody_ReturnsNoRowsWithoutError(t *testing.T) {
	c := newMonitorClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), fixedRedditClock())

	rows, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 7)
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 for a legitimately empty account", len(rows))
	}
}

// TestListAccountCampaigns_RejectsNonPositiveDays pins the client's own days guard: a direct
// caller that skips the dispatcher's validateMonitorDays must not render a range whose start is
// after its end.
func TestListAccountCampaigns_RejectsNonPositiveDays(t *testing.T) {
	var called atomic.Bool
	c := newMonitorClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}), fixedRedditClock())
	if _, err := c.ListAccountCampaigns(context.Background(), testAccount.AccountID, 0); err == nil {
		t.Error("days=0: got nil error")
	}
	if called.Load() {
		t.Error("days=0 reached the API")
	}
}

// TestNextPagePath pins nextPagePath's resolution and refusals directly.
func TestNextPagePath(t *testing.T) {
	c := NewClient(testCreds, testAccount, WithBaseURL("https://ads-api.reddit.com/api/v3"))
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"absent", ``, "", false},
		{"null", `null`, "", false},
		{"empty object", `{}`, "", false},
		{"null next_url", `{"next_url":null}`, "", false},
		{"absolute same origin", `{"next_url":"https://ads-api.reddit.com/api/v3/ad_accounts/a/campaigns?page.token=x"}`, "/ad_accounts/a/campaigns?page.token=x", false},
		{"relative", `{"next_url":"/api/v3/ad_accounts/a/campaigns?page.token=y"}`, "/ad_accounts/a/campaigns?page.token=y", false},
		{"other host", `{"next_url":"https://evil.example/api/v3/x"}`, "", true},
		{"downgraded scheme", `{"next_url":"http://ads-api.reddit.com/api/v3/x"}`, "", true},
		{"outside base path", `{"next_url":"https://ads-api.reddit.com/api/v2/x"}`, "", true},
		{"not an object", `[1]`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.nextPagePath(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("nextPagePath(%s) = %q, %v; want %q, err=%v", tt.raw, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
