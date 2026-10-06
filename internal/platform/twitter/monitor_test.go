// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// monitorRecorder captures every request a monitor stub served, under a mutex because the
// handler runs on the server's goroutines.
type monitorRecorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	query  url.Values
	auth   string
}

func (r *monitorRecorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedRequest{method: req.Method, path: req.URL.Path, query: req.URL.Query(), auth: req.Header.Get("Authorization")})
}

func (r *monitorRecorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

func (r *monitorRecorder) byPath(suffix string) []recordedRequest {
	var out []recordedRequest
	for _, q := range r.all() {
		if strings.HasSuffix(q.path, suffix) {
			out = append(out, q)
		}
	}
	return out
}

// monitorServer serves routes keyed by path; an unrouted path is a test error (never t.Fatal in
// a handler) answered 404.
func monitorServer(t *testing.T, routes map[string]http.HandlerFunc) (*httptest.Server, *monitorRecorder) {
	t.Helper()
	rec := &monitorRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func jsonBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func accountTZ(tz string) http.HandlerFunc {
	return jsonBody(fmt.Sprintf(`{"data":{"id":"account123","timezone":%q}}`, tz))
}

func monitorClient(baseURL string, now time.Time) *Client {
	return NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(baseURL), WithWriteDelay(0), WithClock(func() time.Time { return now }),
	)
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return b.Bytes()
}

func TestValidateMonitorAccountID(t *testing.T) {
	for id, ok := range map[string]bool{
		"18ce54d4x5t": true, "8r7gb": true, "ABC": true,
		"": false, " 8r7gb": false, "8r7gb ": false, "8r_7gb": false, "a/b": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	} {
		err := ValidateMonitorAccountID(id)
		if (err == nil) != ok {
			t.Errorf("ValidateMonitorAccountID(%q) = %v, want ok=%v", id, err, ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidMonitorAccountID) {
			t.Errorf("ValidateMonitorAccountID(%q) = %v, want ErrInvalidMonitorAccountID", id, err)
		}
	}
}

// Budgets come off the campaign (local micro / 1e6), the flight off its line items, converted to
// dates in the ACCOUNT's timezone. Pages are followed by cursor; the line-item read is filtered
// to the listed campaigns.
func TestListAccountCampaigns_BudgetsAndFlightsInAccountTimezone(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123": accountTZ("America/Los_Angeles"),
		"/12/accounts/account123/campaigns": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "" {
				jsonBody(`{"data":[
					{"id":"c1","name":" Daily ","entity_status":"ACTIVE","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null},
					{"id":"c2","name":"Total","entity_status":"PAUSED","total_budget_amount_local_micro":1000000000}
				],"next_cursor":"p2"}`)(w, r)
				return
			}
			jsonBody(`{"data":[
				{"id":"c3","name":"Bad budget","entity_status":"ACTIVE","daily_budget_amount_local_micro":"50000000"},
				{"id":"c4","name":"No line items","entity_status":"ACTIVE","daily_budget_amount_local_micro":10000000},
				{"id":"c5","name":"Gone","entity_status":"ACTIVE","deleted":true}
			],"next_cursor":null}`)(w, r)
		},
		"/12/accounts/account123/line_items": jsonBody(`{"data":[
			{"id":"l1","campaign_id":"c1","start_time":"2026-09-01T07:00:00Z","end_time":"2026-10-01T07:00:00Z"},
			{"id":"l2","campaign_id":"c1","start_time":"2026-09-10T07:00:00Z","end_time":"2026-10-15T19:00:00Z"},
			{"id":"l3","campaign_id":"c2","start_time":"2026-09-20T15:00:00Z","end_time":null},
			{"id":"l4","campaign_id":"c2","start_time":"2026-09-25T07:00:00Z","end_time":"2026-10-30T07:00:00Z"}
		],"next_cursor":null}`),
	})
	got, err := monitorClient(srv.URL, time.Now()).ListAccountCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	want := []AccountCampaign{
		{ID: "c1", Name: "Daily", Status: "ACTIVE", DailyBudget: 50, StartDate: "2026-09-01", EndDate: "2026-10-15",
			Flights: []FlightRange{{StartDate: "2026-09-01", EndDate: "2026-10-15"}}},
		{ID: "c2", Name: "Total", Status: "PAUSED", TotalBudget: 1000, StartDate: "2026-09-20", EndDate: "",
			Flights: []FlightRange{{StartDate: "2026-09-20"}}},
		{ID: "c3", Name: "Bad budget", Status: "ACTIVE", BudgetUnparseable: true},
		{ID: "c4", Name: "No line items", Status: "ACTIVE", DailyBudget: 10},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d campaigns %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("campaign %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	camps := rec.byPath("/campaigns")
	if len(camps) != 2 {
		t.Fatalf("%d campaign page reads, want 2", len(camps))
	}
	q := camps[0].query
	if q.Get("with_deleted") != "false" || q.Get("with_draft") != "false" || q.Get("count") != "1000" {
		t.Errorf("campaign query = %v, want with_deleted=false&with_draft=false&count=1000", q)
	}
	if camps[1].query.Get("cursor") != "p2" {
		t.Errorf("second page cursor = %q, want p2", camps[1].query.Get("cursor"))
	}
	lis := rec.byPath("/line_items")
	if len(lis) != 1 {
		t.Fatalf("%d line item reads, want 1", len(lis))
	}
	if got := lis[0].query.Get("campaign_ids"); got != "c1,c2,c3,c4" {
		t.Errorf("line_items campaign_ids = %q, want the listed campaigns", got)
	}
	if lis[0].query.Get("with_deleted") != "false" || lis[0].query.Get("count") != "1000" {
		t.Errorf("line_items query = %v", lis[0].query)
	}
	for _, r := range rec.all() {
		if !strings.HasPrefix(r.auth, "OAuth ") {
			t.Errorf("%s %s carried no OAuth header", r.method, r.path)
		}
	}
}

// The flight's scheduled days are the UNION of its line items, not the envelope: disjoint line
// items stay separate ranges (a window in the gap is not scheduled), while overlapping ones and
// ones that touch — the next starts the day after the previous ends, or the same instant — merge.
func TestFlightRanges(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	at := func(s string) time.Time {
		v, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			t.Fatalf("parse %q: %v", s, perr)
		}
		return v
	}
	cases := []struct {
		name  string
		spans []flightSpan
		want  []FlightRange
	}{
		{"disjoint stay separate", []flightSpan{
			{at("2026-10-01T07:00:00Z"), at("2026-10-06T07:00:00Z")},
			{at("2026-09-01T07:00:00Z"), at("2026-09-06T07:00:00Z")},
		}, []FlightRange{{"2026-09-01", "2026-09-05"}, {"2026-10-01", "2026-10-05"}}},
		{"overlapping merge", []flightSpan{
			{at("2026-09-01T07:00:00Z"), at("2026-09-11T07:00:00Z")},
			{at("2026-09-05T07:00:00Z"), at("2026-09-16T07:00:00Z")},
		}, []FlightRange{{"2026-09-01", "2026-09-15"}}},
		{"touching (next day) merge", []flightSpan{
			{at("2026-09-01T07:00:00Z"), at("2026-09-06T07:00:00Z")},
			{at("2026-09-06T07:00:00Z"), at("2026-09-11T07:00:00Z")},
		}, []FlightRange{{"2026-09-01", "2026-09-10"}}},
		{"one-day gap stays separate", []flightSpan{
			{at("2026-09-01T07:00:00Z"), at("2026-09-06T07:00:00Z")},
			{at("2026-09-07T07:00:00Z"), at("2026-09-11T07:00:00Z")},
		}, []FlightRange{{"2026-09-01", "2026-09-05"}, {"2026-09-07", "2026-09-10"}}},
		{"open-ended absorbs later", []flightSpan{
			{at("2026-09-01T07:00:00Z"), time.Time{}},
			{at("2026-10-01T07:00:00Z"), at("2026-10-06T07:00:00Z")},
		}, []FlightRange{{"2026-09-01", ""}}},
		{"open-ended after a gap", []flightSpan{
			{at("2026-09-01T07:00:00Z"), at("2026-09-06T07:00:00Z")},
			{at("2026-10-01T07:00:00Z"), time.Time{}},
		}, []FlightRange{{"2026-09-01", "2026-09-05"}, {"2026-10-01", ""}}},
		{"an hour-long interval schedules its day", []flightSpan{
			{at("2026-09-01T17:00:00Z"), at("2026-09-01T18:00:00Z")},
		}, []FlightRange{{"2026-09-01", "2026-09-01"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flightRanges(tc.spans, la); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("flightRanges = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A line item whose times cannot be read marks that campaign's flight unparseable rather than
// silently widening or dropping it.
func TestListAccountCampaigns_UnreadableFlightIsMarked(t *testing.T) {
	srv, _ := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123": accountTZ("UTC"),
		"/12/accounts/account123/campaigns": jsonBody(`{"data":[{"id":"c1","entity_status":"ACTIVE","daily_budget_amount_local_micro":1000000},
			{"id":"c2","entity_status":"ACTIVE","daily_budget_amount_local_micro":1000000}],"next_cursor":null}`),
		"/12/accounts/account123/line_items": jsonBody(`{"data":[{"id":"l1","campaign_id":"c1","start_time":"yesterday"},
			{"id":"l2","campaign_id":"c2","end_time":"2026-10-01T00:00:00Z"}],"next_cursor":null}`),
	})
	got, err := monitorClient(srv.URL, time.Now()).ListAccountCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	for _, c := range got {
		if !c.FlightUnparseable || c.StartDate != "" {
			t.Errorf("campaign %s = %+v, want FlightUnparseable with no dates", c.ID, c)
		}
	}
}

// A line item whose end_time is at or before its start_time — syntactically valid, but serving
// no instant — marks the flight unparseable like any other unreadable time: it neither widens
// the envelope nor puts a scheduled day into Flights. A well-formed sibling is unaffected.
func TestListAccountCampaigns_NonIncreasingFlightIsMarked(t *testing.T) {
	srv, _ := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123": accountTZ("UTC"),
		"/12/accounts/account123/campaigns": jsonBody(`{"data":[{"id":"c1","entity_status":"ACTIVE","daily_budget_amount_local_micro":1000000},
			{"id":"c2","entity_status":"ACTIVE","daily_budget_amount_local_micro":1000000},
			{"id":"c3","entity_status":"ACTIVE","daily_budget_amount_local_micro":1000000}],"next_cursor":null}`),
		"/12/accounts/account123/line_items": jsonBody(`{"data":[
			{"id":"l1","campaign_id":"c1","start_time":"2026-10-01T10:00:00Z","end_time":"2026-10-01T09:00:00Z"},
			{"id":"l2","campaign_id":"c2","start_time":"2026-10-01T10:00:00Z","end_time":"2026-10-01T10:00:00Z"},
			{"id":"l3","campaign_id":"c3","start_time":"2026-10-01T10:00:00Z","end_time":"2026-10-01T11:00:00Z"}],"next_cursor":null}`),
	})
	got, err := monitorClient(srv.URL, time.Now()).ListAccountCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d campaigns, want 3", len(got))
	}
	for _, c := range got {
		switch c.ID {
		case "c1", "c2": // inverted, zero-length
			if !c.FlightUnparseable || c.StartDate != "" || c.EndDate != "" || len(c.Flights) != 0 {
				t.Errorf("campaign %s = %+v, want FlightUnparseable with no dates or flights", c.ID, c)
			}
		case "c3":
			want := []FlightRange{{"2026-10-01", "2026-10-01"}}
			if c.FlightUnparseable || c.StartDate != "2026-10-01" || c.EndDate != "2026-10-01" ||
				!reflect.DeepEqual(c.Flights, want) {
				t.Errorf("campaign c3 = %+v, want a readable one-day flight", c)
			}
		default:
			t.Errorf("unexpected campaign %s", c.ID)
		}
	}
}

// Every way a list can end without X saying it ended is an error, never a short list.
func TestListAccountCampaigns_FailsClosed(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"absent cursor":   jsonBody(`{"data":[{"id":"c1","entity_status":"ACTIVE"}]}`),
		"empty cursor":    jsonBody(`{"data":[{"id":"c1","entity_status":"ACTIVE"}],"next_cursor":""}`),
		"null data":       jsonBody(`{"data":null,"next_cursor":null}`),
		"repeated cursor": jsonBody(`{"data":[],"next_cursor":"same"}`),
		"bad id":          jsonBody(`{"data":[{"id":"c/1","entity_status":"ACTIVE"}],"next_cursor":null}`),
		"server error":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := monitorServer(t, map[string]http.HandlerFunc{
				"/12/accounts/account123":           accountTZ("UTC"),
				"/12/accounts/account123/campaigns": h,
			})
			got, err := monitorClient(srv.URL, time.Now()).ListAccountCampaigns(context.Background())
			if err == nil {
				t.Errorf("got %+v, want an error", got)
			}
		})
	}
}

func TestAccountTimezone_FailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"absent":  `{"data":{"id":"account123"}}`,
		"unknown": `{"data":{"id":"account123","timezone":"Mars/Olympus_Mons"}}`,
		"no data": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := monitorServer(t, map[string]http.HandlerFunc{"/12/accounts/account123": jsonBody(body)})
			if _, err := monitorClient(srv.URL, time.Now()).AccountTimezone(context.Background()); err == nil {
				t.Error("want an error, not a guessed timezone")
			}
		})
	}
}

// The window is the account's own calendar days: [local midnight today-(days-1), the local
// midnight after today), sent as UTC instants — pinned in a non-UTC zone at an hour when the UTC
// date and the local date differ.
func TestSubmitAccountCampaignReport_WindowAndJobBodies(t *testing.T) {
	// 03:00Z on Oct 5 is 20:00 PDT on Oct 4: "today" for the account is Oct 4.
	now := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = fmt.Sprintf(`{"entity_id":"c%d","placements":["ALL_ON_TWITTER"]}`, i)
	}
	var jobN int
	var mu sync.Mutex
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123":                       accountTZ("America/Los_Angeles"),
		"/12/stats/accounts/account123/active_entities": jsonBody(`{"data":[` + strings.Join(ids, ",") + `,{"entity_id":"c0"}]}`),
		"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			jobN++
			n := jobN
			mu.Unlock()
			jsonBody(fmt.Sprintf(`{"data":{"id":%d,"id_str":"%d","status":"PROCESSING","url":null}}`, 1000+n, 1000+n))(w, r)
		},
	})
	reportID, first, last, err := monitorClient(srv.URL, now).SubmitAccountCampaignReport(context.Background(), 7)
	if err != nil {
		t.Fatalf("SubmitAccountCampaignReport: %v", err)
	}
	if reportID != "1001,1002" {
		t.Errorf("reportID = %q, want the two job ids comma-joined in order", reportID)
	}
	if want := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC); !first.Equal(want) {
		t.Errorf("window start = %v, want %v (the account's first local day)", first, want)
	}
	if want := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC); !last.Equal(want) {
		t.Errorf("window end = %v, want %v (the account's local today)", last, want)
	}

	ae := rec.byPath("/active_entities")
	if len(ae) != 1 {
		t.Fatalf("%d active_entities reads, want 1", len(ae))
	}
	wantAE := url.Values{"entity": {"CAMPAIGN"}, "start_time": {"2026-09-28T07:00:00Z"}, "end_time": {"2026-10-05T07:00:00Z"}}
	if ae[0].method != http.MethodGet || ae[0].query.Encode() != wantAE.Encode() {
		t.Errorf("active_entities = %s %v, want GET %v", ae[0].method, ae[0].query, wantAE)
	}

	jobs := rec.byPath("/stats/jobs/accounts/account123")
	if len(jobs) != 2 {
		t.Fatalf("%d job POSTs, want 2 (20 + 5 distinct active campaigns)", len(jobs))
	}
	var firstChunk []string
	for i := 0; i < 20; i++ {
		firstChunk = append(firstChunk, fmt.Sprintf("c%d", i))
	}
	wantJob := url.Values{
		"entity": {"CAMPAIGN"}, "entity_ids": {strings.Join(firstChunk, ",")},
		"start_time": {"2026-09-28T07:00:00Z"}, "end_time": {"2026-10-05T07:00:00Z"},
		"granularity": {"TOTAL"}, "placement": {"ALL_ON_TWITTER"}, "metric_groups": {"ENGAGEMENT,BILLING"},
	}
	if jobs[0].method != http.MethodPost || jobs[0].query.Encode() != wantJob.Encode() {
		t.Errorf("first job = %s %v, want POST %v", jobs[0].method, jobs[0].query, wantJob)
	}
	if got := jobs[1].query.Get("entity_ids"); got != "c20,c21,c22,c23,c24" {
		t.Errorf("second job entity_ids = %q, want the remaining five", got)
	}
	if !strings.HasPrefix(jobs[0].auth, "OAuth ") {
		t.Error("job POST was not OAuth-signed")
	}
}

// No active campaign: no job is created, and the sentinel id says so.
func TestSubmitAccountCampaignReport_NoActiveCampaigns(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123":                       accountTZ("UTC"),
		"/12/stats/accounts/account123/active_entities": jsonBody(`{"data":[]}`),
	})
	reportID, _, _, err := monitorClient(srv.URL, time.Now()).SubmitAccountCampaignReport(context.Background(), 30)
	if err != nil {
		t.Fatalf("SubmitAccountCampaignReport: %v", err)
	}
	if reportID != NoActiveCampaignsReportID {
		t.Errorf("reportID = %q, want the sentinel", reportID)
	}
	if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != 0 {
		t.Errorf("%d job POSTs, want none", n)
	}
}

// A body with no data is not "no active campaigns", and too many active campaigns is refused
// before any job is created.
func TestSubmitAccountCampaignReport_Refusals(t *testing.T) {
	many := make([]string, maxStatsJobsPerReport*statsJobMaxEntities+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"entity_id":"c%d"}`, i)
	}
	for name, body := range map[string]string{
		"no data":  `{}`,
		"bad id":   `{"data":[{"entity_id":"c 1"}]}`,
		"too many": `{"data":[` + strings.Join(many, ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, rec := monitorServer(t, map[string]http.HandlerFunc{
				"/12/accounts/account123":                       accountTZ("UTC"),
				"/12/stats/accounts/account123/active_entities": jsonBody(body),
			})
			_, _, _, err := monitorClient(srv.URL, time.Now()).SubmitAccountCampaignReport(context.Background(), 7)
			if err == nil {
				t.Error("want an error")
			}
			if tooMany := name == "too many"; errors.Is(err, ErrTooManyActiveCampaigns) != tooMany {
				t.Errorf("err = %v, want ErrTooManyActiveCampaigns=%v", err, tooMany)
			}
			if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != 0 {
				t.Errorf("%d job POSTs, want none", n)
			}
		})
	}
	if _, _, _, err := monitorClient("http://127.0.0.1:1", time.Now()).SubmitAccountCampaignReport(context.Background(), 91); err == nil {
		t.Error("days=91 accepted; X caps a job at 90 days")
	}
	t.Run("exactly the limit is accepted", func(t *testing.T) {
		ids := make([]string, MaxMonitorActiveCampaigns)
		for i := range ids {
			ids[i] = fmt.Sprintf(`{"entity_id":"c%d"}`, i)
		}
		var mu sync.Mutex
		jobN := 0
		srv, rec := monitorServer(t, map[string]http.HandlerFunc{
			"/12/accounts/account123":                       accountTZ("UTC"),
			"/12/stats/accounts/account123/active_entities": jsonBody(`{"data":[` + strings.Join(ids, ",") + `]}`),
			"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				jobN++
				n := jobN
				mu.Unlock()
				jsonBody(fmt.Sprintf(`{"data":{"id_str":"%d","status":"PROCESSING"}}`, 1000+n))(w, r)
			},
		})
		if _, _, _, err := monitorClient(srv.URL, time.Now()).SubmitAccountCampaignReport(context.Background(), 7); err != nil {
			t.Fatalf("SubmitAccountCampaignReport: %v", err)
		}
		if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != maxStatsJobsPerReport {
			t.Errorf("%d job POSTs, want %d for exactly %d active campaigns", n, maxStatsJobsPerReport, MaxMonitorActiveCampaigns)
		}
	})
}

// A timezone whose local midnight is not a whole UTC hour is refused before any request beyond
// the account read: X takes whole-hour bounds only, and a floored window would not be the days
// the report claims to cover.
func TestSubmitAccountCampaignReport_RefusesFractionalHourTimezone(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123": accountTZ("Asia/Kolkata"),
	})
	_, _, _, err := monitorClient(srv.URL, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)).SubmitAccountCampaignReport(context.Background(), 7)
	if !errors.Is(err, ErrReportWindowNotWholeHours) {
		t.Fatalf("err = %v, want ErrReportWindowNotWholeHours", err)
	}
	if n := len(rec.byPath("/active_entities")); n != 0 {
		t.Errorf("%d active_entities reads, want none", n)
	}
}

// The window queried is exactly the days reported: start/end are the local midnights of firstDay
// and of the day after lastDay, in every case — nothing is floored or trimmed by the hour.
func TestAccountReportWindow_Boundaries(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	la, _ := time.LoadLocation("America/Los_Angeles")
	scl, _ := time.LoadLocation("America/Santiago") // DST spring-forward skips 00:00 (2026-09-06)
	apia, _ := time.LoadLocation("Pacific/Apia")    // skipped 2011-12-30 entirely (-10 → +14)
	cases := []struct {
		name            string
		now             time.Time
		loc             *time.Location
		days            int
		start, end      string
		firstDay, lastD string
	}{
		{"utc", time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), time.UTC, 7,
			"2026-09-29T00:00:00Z", "2026-10-06T00:00:00Z", "2026-09-29", "2026-10-05"},
		// 90 days across the November fall-back is 90d+1h of wall time, over X's limit: the
		// earliest local day is dropped, and firstDay says so — 89 whole local days.
		{"dst fall-back at 90 days (NY)", time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC), ny, 90,
			"2026-08-24T04:00:00Z", "2026-11-21T05:00:00Z", "2026-08-24", "2026-11-20"},
		{"dst fall-back at 90 days (LA)", time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC), la, 90,
			"2026-08-24T07:00:00Z", "2026-11-21T08:00:00Z", "2026-08-24", "2026-11-20"},
		// Across the fall-back with fewer days the whole window fits: no day is dropped, and the
		// bounds sit on each side's own offset (PDT start, PST end).
		{"dst fall-back at 30 days (LA)", time.Date(2026, 11, 20, 20, 0, 0, 0, time.UTC), la, 30,
			"2026-10-22T07:00:00Z", "2026-11-21T08:00:00Z", "2026-10-22", "2026-11-20"},
		// Across the spring-forward a 90-day window is 90d-1h: it fits whole.
		{"dst spring-forward at 90 days (LA)", time.Date(2027, 4, 20, 20, 0, 0, 0, time.UTC), la, 90,
			"2027-01-21T08:00:00Z", "2027-04-21T07:00:00Z", "2027-01-21", "2027-04-20"},
		// Santiago skips local midnight on 2026-09-06: that day begins at 01:00 -03. time.Date
		// would normalize to 23:00 of 09-05, dating the window a day early. Today is the skipped day:
		{"skipped midnight is today (Santiago)", time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC), scl, 7,
			"2026-08-31T04:00:00Z", "2026-09-07T03:00:00Z", "2026-08-31", "2026-09-06"},
		// ...and the window STARTS on the skipped day.
		{"skipped midnight starts the window (Santiago)", time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC), scl, 7,
			"2026-09-06T04:00:00Z", "2026-09-13T03:00:00Z", "2026-09-06", "2026-09-12"},
		// A window whose first day was skipped ENTIRELY starts at the next real day, and firstDay
		// says so (6 reported days, never a day that did not exist or part of the day before).
		{"skipped day starts the window (Apia 2011-12-30)", time.Date(2012, 1, 4, 22, 0, 0, 0, time.UTC), apia, 7,
			"2011-12-30T10:00:00Z", "2012-01-05T10:00:00Z", "2011-12-31", "2012-01-05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e, first, last, err := accountReportWindow(tc.now, tc.loc, tc.days)
			if err != nil {
				t.Fatalf("accountReportWindow: %v", err)
			}
			if statsTime(s) != tc.start || statsTime(e) != tc.end {
				t.Errorf("window = [%s, %s), want [%s, %s)", statsTime(s), statsTime(e), tc.start, tc.end)
			}
			if first.Format(time.DateOnly) != tc.firstDay || last.Format(time.DateOnly) != tc.lastD {
				t.Errorf("days = %s..%s, want %s..%s", first.Format(time.DateOnly), last.Format(time.DateOnly), tc.firstDay, tc.lastD)
			}
			// Queried == reported: the bounds ARE the first instants of the reported days (local
			// midnight, or the instant after a skipped one).
			fl := localDayStart(first.Year(), first.Month(), first.Day(), tc.loc)
			ll := localDayStart(last.Year(), last.Month(), last.Day()+1, tc.loc)
			if localDate(s.In(tc.loc)) != first || localDate(e.Add(-time.Nanosecond).In(tc.loc)) != last {
				t.Errorf("bounds fall outside the reported days: [%v, %v) vs %s..%s", s.In(tc.loc), e.In(tc.loc), first.Format(time.DateOnly), last.Format(time.DateOnly))
			}
			if !s.Equal(fl) || !e.Equal(ll) {
				t.Errorf("queried [%v, %v) is not the reported days' local midnights [%v, %v)", s, e, fl, ll)
			}
			if e.Sub(s) > maxStatsWindow {
				t.Errorf("window %v exceeds X's 90 days", e.Sub(s))
			}
		})
	}
	// A fractional-hour zone cannot be queried on its own days: refused, never floored.
	for _, tz := range []string{"Asia/Kolkata", "Asia/Kathmandu", "America/St_Johns"} {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			t.Fatalf("load %s: %v", tz, err)
		}
		if _, _, _, _, werr := accountReportWindow(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), loc, 7); !errors.Is(werr, ErrReportWindowNotWholeHours) {
			t.Errorf("%s: err = %v, want ErrReportWindowNotWholeHours", tz, werr)
		}
	}
}

const statsFileBody = `{"data_type":"stats","time_series_length":1,"data":[
	{"id":"c1","id_data":[{"segment":null,"metrics":{"impressions":[1000],"clicks":[30],"billed_charge_local_micro":[12500000]}}]},
	{"id":"c2","id_data":[{"segment":null,"metrics":{"impressions":[50],"clicks":null,"billed_charge_local_micro":null}}]}
]}`

func TestCheckAccountCampaignReport_States(t *testing.T) {
	job := func(id, status, u string) string {
		if u == "" {
			return fmt.Sprintf(`{"id_str":%q,"status":%q,"url":null}`, id, status)
		}
		return fmt.Sprintf(`{"id_str":%q,"status":%q,"url":%q}`, id, status, u)
	}
	cases := []struct {
		name string
		jobs []string // given the server URL
		want AccountReportStatus
		err  bool
	}{
		{"processing", []string{job("11", "PROCESSING", ""), job("22", "SUCCESS", "/files/a")}, AccountReportStatusPending, false},
		{"queued", []string{job("11", "QUEUED", ""), job("22", "QUEUED", "")}, AccountReportStatusPending, false},
		{"job missing from the answer", []string{job("11", "SUCCESS", "/files/a")}, AccountReportStatusPending, false},
		{"failed", []string{job("11", "FAILED", ""), job("22", "SUCCESS", "/files/a")}, AccountReportStatusFailed, false},
		{"cancelled", []string{job("11", "CANCELLED", ""), job("22", "PROCESSING", "")}, AccountReportStatusFailed, false},
		{"success without a file", []string{job("11", "SUCCESS", ""), job("22", "SUCCESS", "/files/a")}, AccountReportStatusFailed, false},
		{"unknown status", []string{job("11", "EXPLODED", ""), job("22", "SUCCESS", "/files/a")}, "", true},
	}
	fileA := gz(t, statsFileBody)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srvURL string
			srv, rec := monitorServer(t, map[string]http.HandlerFunc{
				"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
					body := strings.ReplaceAll(`{"data":[`+strings.Join(tc.jobs, ",")+`],"next_cursor":null}`, `"/files/`, `"`+srvURL+`/files/`)
					jsonBody(body)(w, r)
				},
				"/files/a": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fileA) },
			})
			srvURL = srv.URL
			res, err := monitorClient(srv.URL, time.Now()).CheckAccountCampaignReport(context.Background(), "11,22")
			if tc.err {
				if err == nil {
					t.Errorf("got %+v, want an error", res)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckAccountCampaignReport: %v", err)
			}
			if res.Status != tc.want {
				t.Errorf("status = %q, want %q", res.Status, tc.want)
			}
			if n := len(rec.byPath("/files/a")); n != 0 {
				t.Errorf("%d downloads, want none until every job has succeeded", n)
			}
			st := rec.byPath("/stats/jobs/accounts/account123")
			if len(st) != 1 || st[0].query.Get("job_ids") != "11,22" || st[0].method != http.MethodGet {
				t.Errorf("status reads = %+v, want ONE GET with job_ids=11,22", st)
			}
		})
	}
}

// All jobs done: every file is downloaded WITHOUT credentials, decompressed, and folded per
// campaign — spend from billed_charge_local_micro, a null metric read as zero, a campaign present
// in two jobs' files summed.
func TestCheckAccountCampaignReport_SuccessDownloadsAndFolds(t *testing.T) {
	second := `{"data":[{"id":"c3","id_data":[{"metrics":{"impressions":[7],"clicks":[1],"billed_charge_local_micro":[2000000]}}]},
		{"id":"c1","id_data":[{"metrics":{"impressions":[1],"clicks":[0],"billed_charge_local_micro":[500000]}}]}]}`
	fileA := gz(t, statsFileBody)
	var srvURL string
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
			jsonBody(fmt.Sprintf(`{"data":[{"id_str":"11","status":"SUCCESS","url":"%s/files/a.json.gz"},{"id_str":"22","status":"SUCCESS","url":"%s/files/b.json.gz"}]}`, srvURL, srvURL))(w, r)
		},
		"/files/a.json.gz": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fileA) },
		// Served already decompressed, as a transport honouring Content-Encoding would hand it over.
		"/files/b.json.gz": jsonBody(second),
	})
	srvURL = srv.URL
	res, err := monitorClient(srv.URL, time.Now()).CheckAccountCampaignReport(context.Background(), "11,22")
	if err != nil {
		t.Fatalf("CheckAccountCampaignReport: %v", err)
	}
	if res.Status != AccountReportStatusSuccess || !res.Partial {
		t.Errorf("status=%q partial=%v, want success and partial (billing settles over days)", res.Status, res.Partial)
	}
	want := []AccountReportRow{
		{CampaignID: "c1", Impressions: 1001, Clicks: 30, SpendMicro: 13_000_000},
		{CampaignID: "c2", Impressions: 50},
		{CampaignID: "c3", Impressions: 7, Clicks: 1, SpendMicro: 2_000_000},
	}
	if len(res.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", res.Rows, want)
	}
	for i := range want {
		if res.Rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, res.Rows[i], want[i])
		}
	}
	for _, path := range []string{"/files/a.json.gz", "/files/b.json.gz"} {
		dl := rec.byPath(path)
		if len(dl) != 1 {
			t.Fatalf("%s downloaded %d times, want once", path, len(dl))
		}
		if dl[0].auth != "" {
			t.Errorf("%s was sent an Authorization header; the file url needs none and must not receive our credentials", path)
		}
	}
}

// Download failures never echo the file URL, and a non-https URL on a foreign host is refused
// before any request.
func TestCheckAccountCampaignReport_DownloadFailures(t *testing.T) {
	const secret = "sig=SECRET"
	cases := map[string]struct {
		url  func(base string) string
		file http.HandlerFunc
	}{
		"foreign http host":  {func(string) string { return "http://evil.example/f.json.gz?" + secret }, nil},
		"foreign https host": {func(string) string { return "https://evil.example/f.json.gz?" + secret }, nil},
		"twimg lookalike":    {func(string) string { return "https://ton.twimg.com.evil.example/f.json.gz?" + secret }, nil},
		"userinfo": {func(base string) string {
			return strings.Replace(base, "http://", "http://u:p@", 1) + "/files/f?" + secret
		}, nil},
		"404":            {func(base string) string { return base + "/files/f?" + secret }, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }},
		"corrupt gzip":   {func(base string) string { return base + "/files/f?" + secret }, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte{0x1f, 0x8b, 0, 1, 2}) }},
		"not stats json": {func(base string) string { return base + "/files/f?" + secret }, jsonBody(`[1,2]`)},
		"negative metric": {func(base string) string { return base + "/files/f?" + secret },
			jsonBody(`{"data":[{"id":"c1","id_data":[{"metrics":{"impressions":[-1]}}]}]}`)},
		"unattributable row": {func(base string) string { return base + "/files/f?" + secret },
			jsonBody(`{"data":[{"id":"","id_data":[{"metrics":{"impressions":[1]}}]}]}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var srvURL string
			routes := map[string]http.HandlerFunc{
				"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
					jsonBody(fmt.Sprintf(`{"data":[{"id_str":"11","status":"SUCCESS","url":%q}]}`, tc.url(srvURL)))(w, r)
				},
			}
			if tc.file != nil {
				routes["/files/f"] = tc.file
			}
			srv, rec := monitorServer(t, routes)
			srvURL = srv.URL
			res, err := monitorClient(srv.URL, time.Now()).CheckAccountCampaignReport(context.Background(), "11")
			if err == nil {
				t.Fatalf("got %+v, want an error", res)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/files/") || strings.Contains(err.Error(), "evil") {
				t.Errorf("error %q echoes the file url", err)
			}
			if tc.file == nil && len(rec.byPath("/files/f")) != 0 {
				t.Error("a refused url was fetched")
			}
		})
	}
}

// The sentinel and malformed ids are answered without a request.
func TestCheckAccountCampaignReport_NoRequestCases(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{})
	c := monitorClient(srv.URL, time.Now())
	res, err := c.CheckAccountCampaignReport(context.Background(), NoActiveCampaignsReportID)
	if err != nil || res.Status != AccountReportStatusSuccess || res.Rows == nil || len(res.Rows) != 0 {
		t.Errorf("sentinel = %+v, %v; want a finished, empty, non-nil report", res, err)
	}
	for _, bad := range []string{"", "11,", "abc", "11,11", "1,2,3,4,5,6,7,8,9,10,11", strings.Repeat("9", 21)} {
		if _, err := c.CheckAccountCampaignReport(context.Background(), bad); err == nil {
			t.Errorf("report id %q accepted", bad)
		}
	}
	if n := len(rec.all()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// The composite id round-trips: what Submit returns, Check parses back into the same jobs.
func TestStatsReportID_RoundTrip(t *testing.T) {
	ids := []string{"1120829647711653888", "1120829647711653889", "18446744073709551615"}
	got, err := parseStatsReportID(strings.Join(ids, ","))
	if err != nil {
		t.Fatalf("parseStatsReportID: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("round trip = %v, want %v", got, ids)
	}
}

// Job creation goes through the client's write pacer (one admission per job) and is never
// retried on a 429: a throttled create may have committed, and a retry would build a second job.
func TestSubmitAccountCampaignReport_PacedAndNotRetried(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123":                       accountTZ("UTC"),
		"/12/stats/accounts/account123/active_entities": jsonBody(`{"data":[{"entity_id":"c1"}]}`),
		"/12/stats/jobs/accounts/account123":            func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
	})
	c := NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(srv.URL), WithWriteDelay(time.Millisecond),
	)
	var admitted int
	var mu sync.Mutex
	c.onAdmit = func(context.Context, time.Time) {
		mu.Lock()
		admitted++
		mu.Unlock()
	}
	if _, _, _, err := c.SubmitAccountCampaignReport(context.Background(), 7); err == nil {
		t.Fatal("want the 429 surfaced")
	}
	mu.Lock()
	defer mu.Unlock()
	if admitted != 1 {
		t.Errorf("%d pacer admissions, want 1 (the job POST)", admitted)
	}
	if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != 1 {
		t.Errorf("%d job POSTs, want exactly 1 (no retry of a create)", n)
	}
}

// Only X's documented results-file host is admitted over https, matched exactly; the client's
// own API origin stays admitted (how most tests serve files). Everything else is refused.
func TestStatsFileURLAllowed(t *testing.T) {
	c := NewClient(Credentials{}, AccountConfig{AccountID: "account123"})
	for raw, want := range map[string]bool{
		"https://ton.twimg.com/advertiser-api-async-analytics/abc.json.gz": true,
		"https://TON.twimg.com/advertiser-api-async-analytics/abc.json.gz": true,
		"https://ads-api.x.com/files/abc.json.gz":                          true, // the API origin
		"https://evil.example/abc.json.gz":                                 false,
		"https://pbs.twimg.com/abc.json.gz":                                false,
		"https://ton.twimg.com.evil.example/abc.json.gz":                   false,
		"https://ton.twimg.com:8443/abc.json.gz":                           false,
		"http://ton.twimg.com/abc.json.gz":                                 false,
		"https://user:pw@ton.twimg.com/abc.json.gz":                        false,
		"https:ton.twimg.com/abc.json.gz":                                  false,
		"/relative/abc.json.gz":                                            false,
	} {
		if got := c.statsFileURLAllowed(raw); got != want {
			t.Errorf("statsFileURLAllowed(%q) = %v, want %v", raw, got, want)
		}
	}
}

// statsJobsServer serves one SUCCESS job whose file url is fileURL.
func statsJobsServer(t *testing.T, fileURL string, extra map[string]http.HandlerFunc) (*httptest.Server, *monitorRecorder) {
	t.Helper()
	routes := map[string]http.HandlerFunc{
		"/12/stats/jobs/accounts/account123": jsonBody(fmt.Sprintf(`{"data":[{"id_str":"11","status":"SUCCESS","url":%q}]}`, fileURL)),
	}
	for k, v := range extra {
		routes[k] = v
	}
	return monitorServer(t, routes)
}

// A results file on a second origin — a TLS stand-in for ton.twimg.com, admitted through the
// test-only host seam — is downloaded and folded, and is sent no Authorization header.
func TestCheckAccountCampaignReport_DownloadsFromTheFileHostWithoutCredentials(t *testing.T) {
	file := gz(t, statsFileBody)
	var auth []string
	var mu sync.Mutex
	files := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write(file)
	}))
	t.Cleanup(files.Close)
	fileHost := strings.TrimPrefix(files.URL, "https://")

	api, _ := statsJobsServer(t, files.URL+"/advertiser-api-async-analytics/a.json.gz", nil)
	c := NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(api.URL), WithWriteDelay(0), WithHTTPClient(files.Client()), withStatsFileHosts(fileHost),
	)
	res, err := c.CheckAccountCampaignReport(context.Background(), "11")
	if err != nil {
		t.Fatalf("CheckAccountCampaignReport: %v", err)
	}
	if res.Status != AccountReportStatusSuccess || len(res.Rows) != 2 {
		t.Errorf("result = %+v, want success with the file's two campaigns", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 1 || auth[0] != "" {
		t.Errorf("file host saw Authorization %q over %d requests, want one request with none", auth, len(auth))
	}
}

// A transport failure on an admitted host reports the failure without any part of the URL —
// it carries the file's access signature.
func TestCheckAccountCampaignReport_TransportFailureHidesTheURL(t *testing.T) {
	dead := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL, deadHost, client := dead.URL, strings.TrimPrefix(dead.URL, "https://"), dead.Client()
	dead.Close() // nothing listens any more: the download fails in transport

	const secret = "Signature=TOPSECRETSIG"
	api, _ := statsJobsServer(t, deadURL+"/advertiser-api-async-analytics/a.json.gz?"+secret, nil)
	c := NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(api.URL), WithWriteDelay(0), WithHTTPClient(client), withStatsFileHosts(deadHost),
	)
	_, err := c.CheckAccountCampaignReport(context.Background(), "11")
	if err == nil {
		t.Fatal("want a transport error")
	}
	for _, part := range []string{"TOPSECRETSIG", "Signature", "advertiser-api-async-analytics", deadHost, "127.0.0.1"} {
		if strings.Contains(err.Error(), part) {
			t.Errorf("error %q contains %q from the file url", err, part)
		}
	}
}

// Both download caps refuse one byte over: the compressed body, and the decompressed stream of a
// small, high-ratio gzip of zeros. At exactly the cap the size check passes (the bytes then fail
// to decode as stats, a different error).
func TestCheckAccountCampaignReport_DownloadCaps(t *testing.T) {
	const compressedCap, decompressedCap = 256, 4096
	zeros := func(n int) []byte { return gz(t, string(make([]byte, n))) }
	cases := []struct {
		name     string
		body     []byte
		exceeded bool
	}{
		{"compressed cap+1", bytes.Repeat([]byte("x"), compressedCap+1), true},
		{"compressed at cap", bytes.Repeat([]byte("x"), compressedCap), false},
		{"decompressed cap+1", zeros(decompressedCap + 1), true},
		{"decompressed at cap", zeros(decompressedCap), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "decompressed") && len(tc.body) > compressedCap {
				t.Fatalf("precondition: the gzip of zeros is %d bytes; it must fit the compressed cap", len(tc.body))
			}
			var srvURL string
			api, _ := monitorServer(t, map[string]http.HandlerFunc{
				"/12/stats/jobs/accounts/account123": func(w http.ResponseWriter, r *http.Request) {
					jsonBody(fmt.Sprintf(`{"data":[{"id_str":"11","status":"SUCCESS","url":"%s/files/f"}]}`, srvURL))(w, r)
				},
				"/files/f": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tc.body) },
			})
			srvURL = api.URL
			c := NewClient(
				Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
				AccountConfig{AccountID: "account123"},
				WithBaseURL(api.URL), WithWriteDelay(0), withStatsFileCaps(compressedCap, decompressedCap),
			)
			_, err := c.CheckAccountCampaignReport(context.Background(), "11")
			if err == nil {
				t.Fatal("want an error (over the cap, or not a stats document)")
			}
			if got := strings.Contains(err.Error(), "exceeds"); got != tc.exceeded {
				t.Errorf("err = %v; cap exceeded = %v, want %v", err, got, tc.exceeded)
			}
		})
	}
}

// A submission refuses BEFORE creating any job when the deadline cannot fit the paced POSTs —
// two jobs at 1s pacing plus the margin need about 4s — and proceeds when it can.
func TestSubmitAccountCampaignReport_RefusesWhenTheBudgetCannotFitTheJobs(t *testing.T) {
	ents := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		ents = append(ents, fmt.Sprintf(`{"entity_id":"c%d"}`, i))
	}
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123":                       accountTZ("UTC"),
		"/12/stats/accounts/account123/active_entities": jsonBody(`{"data":[` + strings.Join(ents, ",") + `]}`),
		"/12/stats/jobs/accounts/account123":            jsonBody(`{"data":{"id_str":"7","status":"QUEUED"}}`),
	})
	c := NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(srv.URL), WithWriteDelay(time.Second),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err := c.SubmitAccountCampaignReport(ctx, 7)
	if !errors.Is(err, ErrStatsJobBudget) {
		t.Fatalf("err = %v, want ErrStatsJobBudget", err)
	}
	if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != 0 {
		t.Errorf("%d job POSTs, want none: a declined submission creates nothing", n)
	}

	// With room for both jobs the same submission goes ahead (the second POST waits out the
	// pacer, ~1s).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, _, _, err := c.SubmitAccountCampaignReport(ctx2, 7); err != nil {
		t.Fatalf("with budget: %v", err)
	}
	if n := len(rec.byPath("/stats/jobs/accounts/account123")); n != 2 {
		t.Errorf("%d job POSTs, want 2", n)
	}
}

// One monitor read asks for the account timezone twice (list, then submit); the client reuses a
// fresh answer instead of reading the account again, and re-reads once it is stale.
func TestAccountTimezone_CachedBriefly(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{"/12/accounts/account123": accountTZ("America/Los_Angeles")})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c := NewClient(
		Credentials{ConsumerKey: "key", ConsumerSecret: "secret", AccessToken: "token", AccessTokenSecret: "token_secret"},
		AccountConfig{AccountID: "account123"},
		WithBaseURL(srv.URL), WithWriteDelay(0), WithClock(func() time.Time { return now }),
	)
	for i := 0; i < 2; i++ {
		if loc, err := c.AccountTimezone(context.Background()); err != nil || loc.String() != "America/Los_Angeles" {
			t.Fatalf("AccountTimezone #%d = %v, %v", i, loc, err)
		}
	}
	if n := len(rec.byPath("/accounts/account123")); n != 1 {
		t.Errorf("%d account reads, want 1 within the cache window", n)
	}
	now = now.Add(accountTimezoneCacheFor + time.Second)
	if _, err := c.AccountTimezone(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.byPath("/accounts/account123")); n != 2 {
		t.Errorf("%d account reads, want a re-read once the cached zone is stale", n)
	}
}
