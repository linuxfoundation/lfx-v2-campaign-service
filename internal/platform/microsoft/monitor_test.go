// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// monitorServer stands in for every Microsoft surface the account monitor touches: OAuth,
// Campaign Management (QueryByAccountId), Reporting (Submit/Poll) and the pre-signed download.
// Handlers never call t.Fatal (they run off the test goroutine); they record and reply.
type monitorServer struct {
	srv *httptest.Server

	mu sync.Mutex
	// Campaign Management
	campaignsBody string
	queryRaw      []byte
	queryCount    int
	// Reporting
	submitRaw    []byte
	pollRaw      []byte
	pollCount    int
	pollReply    string // raw JSON; "%URL%" is replaced with the download URL
	zipPayload   []byte
	downloadAuth string
}

func newMonitorServer(t *testing.T) *monitorServer {
	t.Helper()
	m := &monitorServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/CampaignManagement/v13/Campaigns/QueryByAccountId", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.queryRaw = raw
		m.queryCount++
		body := m.campaignsBody
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.submitRaw = raw
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ReportRequestId":"rr-acct-1"}`)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Poll", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.pollRaw = raw
		m.pollCount++
		reply := m.pollReply
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.ReplaceAll(reply, "%URL%", m.srv.URL+"/download?sig=presigned"))
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.downloadAuth = r.Header.Get("Authorization")
		payload := m.zipPayload
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(payload)
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// monitorClock is pinned in a NON-UTC zone whose local calendar date differs from UTC's:
// 2026-10-05 01:30 at UTC+5 is 2026-10-04 20:30 UTC. A window computed in the local zone
// would end on the 5th; the house convention (UTC) ends on the 4th.
func monitorClock() time.Time {
	return time.Date(2026, 10, 5, 1, 30, 0, 0, time.FixedZone("UTC+5", 5*3600))
}

func newMonitorClient(t *testing.T, m *monitorServer) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
		WithBaseURL(m.srv.URL),
		WithReportingBaseURL(m.srv.URL),
		WithTokenURL(m.srv.URL+"/token"),
		WithClock(monitorClock),
	)
}

func TestValidateMonitorAccountID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"1", true},
		{"123456789", true},
		{"999999999999999999", true},
		{"", false},
		{" 1", false},
		{"1 ", false},
		{"01", false},
		{"0", false},
		{"1234567890123456789", false},
		{"12a", false},
		{"-1", false},
	} {
		err := ValidateMonitorAccountID(tc.id)
		if tc.want && err != nil {
			t.Errorf("ValidateMonitorAccountID(%q) = %v, want nil", tc.id, err)
		}
		if !tc.want {
			if err == nil {
				t.Errorf("ValidateMonitorAccountID(%q) = nil, want an error", tc.id)
			} else if !errors.Is(err, ErrInvalidMonitorAccountID) {
				t.Errorf("ValidateMonitorAccountID(%q) error does not wrap ErrInvalidMonitorAccountID: %v", tc.id, err)
			}
		}
	}
}

func TestSubmitAccountCampaignReport_BodyAndWindow(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)

	id, start, end, err := c.SubmitAccountCampaignReport(context.Background(), 7)
	if err != nil {
		t.Fatalf("SubmitAccountCampaignReport: %v", err)
	}
	if id != "rr-acct-1" {
		t.Errorf("reportID = %q, want rr-acct-1", id)
	}
	// UTC-normalised, today-inclusive: 7 days ending 2026-10-04 (UTC) start on 2026-09-28.
	if end.Location() != time.UTC || start.Location() != time.UTC {
		t.Errorf("window must be in UTC, got start=%v end=%v", start.Location(), end.Location())
	}
	if got := end.Format("2006-01-02"); got != "2026-10-04" {
		t.Errorf("windowEnd = %s, want 2026-10-04 (the UTC date, not the local 10-05)", got)
	}
	if got := start.Format("2006-01-02"); got != "2026-09-28" {
		t.Errorf("windowStart = %s, want 2026-09-28 (days-1 before end, inclusive)", got)
	}

	m.mu.Lock()
	raw := append([]byte(nil), m.submitRaw...)
	m.mu.Unlock()
	var body struct {
		ReportRequest struct {
			Type                   string          `json:"Type"`
			Format                 string          `json:"Format"`
			Aggregation            string          `json:"Aggregation"`
			ReturnOnlyCompleteData *bool           `json:"ReturnOnlyCompleteData"`
			Columns                []string        `json:"Columns"`
			Scope                  json.RawMessage `json:"Scope"`
			Time                   struct {
				CustomDateRangeStart msDate `json:"CustomDateRangeStart"`
				CustomDateRangeEnd   msDate `json:"CustomDateRangeEnd"`
				ReportTimeZone       string `json:"ReportTimeZone"`
			} `json:"Time"`
		} `json:"ReportRequest"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode submit body: %v (%s)", err, raw)
	}
	rr := body.ReportRequest
	if rr.Type != "CampaignPerformanceReportRequest" || rr.Format != "Csv" || rr.Aggregation != "Summary" {
		t.Errorf("report type/format/aggregation = %q/%q/%q", rr.Type, rr.Format, rr.Aggregation)
	}
	if rr.ReturnOnlyCompleteData == nil || *rr.ReturnOnlyCompleteData {
		t.Errorf("ReturnOnlyCompleteData must be present and false: %s", raw)
	}
	wantCols := []string{"CampaignId", "Impressions", "Clicks", "Spend", "ConversionsQualified"}
	if strings.Join(rr.Columns, ",") != strings.Join(wantCols, ",") {
		t.Errorf("Columns = %v, want %v", rr.Columns, wantCols)
	}
	// Scope is pinned on the WIRE: exactly the account id, quoted, and no Campaigns element.
	if got := string(bytes.TrimSpace(rr.Scope)); got != `{"AccountIds":["9999999"]}` {
		t.Errorf("Scope = %s, want {\"AccountIds\":[\"9999999\"]} with no Campaigns element", got)
	}
	if bytes.Contains(raw, []byte(`"Campaigns"`)) {
		t.Errorf("account-wide submit must not carry a Campaigns scope: %s", raw)
	}
	if got := rr.Time.CustomDateRangeStart; got != (msDate{Year: 2026, Month: 9, Day: 28}) {
		t.Errorf("CustomDateRangeStart = %+v, want 2026-09-28", got)
	}
	if got := rr.Time.CustomDateRangeEnd; got != (msDate{Year: 2026, Month: 10, Day: 4}) {
		t.Errorf("CustomDateRangeEnd = %+v, want 2026-10-04", got)
	}
	if rr.Time.ReportTimeZone != "GreenwichMeanTimeDublinEdinburghLisbonLondon" {
		t.Errorf("ReportTimeZone = %q", rr.Time.ReportTimeZone)
	}
}

func TestSubmitAccountCampaignReport_OneDayWindowIsToday(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)
	_, start, end, err := c.SubmitAccountCampaignReport(context.Background(), 1)
	if err != nil {
		t.Fatalf("SubmitAccountCampaignReport: %v", err)
	}
	if start.Format("2006-01-02") != "2026-10-04" || end.Format("2006-01-02") != "2026-10-04" {
		t.Errorf("days=1 window = %s..%s, want 2026-10-04..2026-10-04", start.Format("2006-01-02"), end.Format("2006-01-02"))
	}
}

func TestSubmitAccountCampaignReport_RejectsNonPositiveDays(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)
	for _, d := range []int{0, -1} {
		if _, _, _, err := c.SubmitAccountCampaignReport(context.Background(), d); err == nil {
			t.Errorf("days=%d: expected an error", d)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.submitRaw != nil {
		t.Errorf("a rejected window must not reach Microsoft: %s", m.submitRaw)
	}
}

func TestSubmitAccountCampaignReport_RejectsLooseAccountID(t *testing.T) {
	m := newMonitorServer(t)
	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "0099"},
		WithReportingBaseURL(m.srv.URL), WithTokenURL(m.srv.URL+"/token"), WithClock(monitorClock),
	)
	if _, _, _, err := c.SubmitAccountCampaignReport(context.Background(), 7); !errors.Is(err, ErrInvalidMonitorAccountID) {
		t.Errorf("want ErrInvalidMonitorAccountID, got %v", err)
	}
}

const monitorCampaignsJSON = `{"Campaigns":[
 {"Id":111,"Name":"Search A","Status":"Active","CampaignType":"Search","BudgetType":"DailyBudgetStandard","DailyBudget":50.5,"BudgetId":null},
 {"Id":"222","Name":"Shared B","Status":"Paused","CampaignType":"PerformanceMax","BudgetType":"DailyBudgetStandard","DailyBudget":20,"BudgetId":"98765"},
 {"Id":333,"Name":"Gone","Status":"Deleted","CampaignType":"Search","DailyBudget":10},
 {"Id":444,"Name":"No budget","Status":"BudgetPaused","CampaignType":"Audience","DailyBudget":null},
 {"Id":555,"Name":"Suspended","Status":"Suspended","CampaignType":"Shopping","DailyBudget":-3,"BudgetId":0},
 {"Id":666,"Name":"Shared null","Status":"BudgetAndManualPaused","CampaignType":"Search","DailyBudget":null,"BudgetId":4242},
 {"Id":777,"Name":"Future","Status":"SomethingNew","CampaignType":"Search","DailyBudget":"12.25"}
],"PartialErrors":null}`

func TestListAccountCampaigns_Decode(t *testing.T) {
	m := newMonitorServer(t)
	m.campaignsBody = monitorCampaignsJSON
	c := newMonitorClient(t, m)

	got, err := c.ListAccountCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	want := []AccountCampaign{
		{ID: "111", Name: "Search A", Status: "Active", CampaignType: "Search", DailyBudget: 50.5},
		{ID: "222", Name: "Shared B", Status: "Paused", CampaignType: "PerformanceMax", DailyBudget: 20, SharedBudget: true},
		{ID: "444", Name: "No budget", Status: "BudgetPaused", CampaignType: "Audience", BudgetUnparseable: true},
		{ID: "555", Name: "Suspended", Status: "Suspended", CampaignType: "Shopping", BudgetUnparseable: true},
		{ID: "666", Name: "Shared null", Status: "BudgetAndManualPaused", CampaignType: "Search", SharedBudget: true},
		{ID: "777", Name: "Future", Status: "SomethingNew", CampaignType: "Search", DailyBudget: 12.25},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d campaigns, want %d (Deleted dropped): %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("campaign[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	m.mu.Lock()
	raw := append([]byte(nil), m.queryRaw...)
	m.mu.Unlock()
	var req struct {
		AccountId    json.Number `json:"AccountId"`
		CampaignType string      `json:"CampaignType"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode query body: %v", err)
	}
	if req.AccountId.String() != "9999999" {
		t.Errorf("AccountId = %q, want 9999999", req.AccountId)
	}
	types := map[string]bool{}
	for _, f := range strings.Fields(req.CampaignType) {
		types[f] = true
	}
	for _, want := range []string{"Search", "Shopping", "DynamicSearchAds", "Audience", "PerformanceMax", "Hotel", "App", "ObjectiveBased"} {
		if !types[want] {
			t.Errorf("CampaignType %q lacks %s; the operation defaults to Search only and would hide every other type", req.CampaignType, want)
		}
	}
}

func TestListAccountCampaigns_EmptyAccountIsNonNilEmpty(t *testing.T) {
	m := newMonitorServer(t)
	m.campaignsBody = `{"Campaigns":[]}`
	c := newMonitorClient(t, m)
	got, err := c.ListAccountCampaigns(context.Background())
	if err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("want a non-nil empty slice, got %#v", got)
	}
}

func TestListAccountCampaigns_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing Campaigns field", `{"PartialErrors":null}`},
		{"null Campaigns field", `{"Campaigns":null}`},
		{"truncated array", `{"Campaigns":[{"Id":1,"Name":"a","Status":"Active"}`},
		{"truncated object", `{"Campaigns":[{"Id":1,"Name":"a","Status":"Active"}]`},
		{"truncated element", `{"Campaigns":[{"Id":1,"Name":"a`},
		{"trailing data", `{"Campaigns":[]}garbage`},
		{"element with no Id", `{"Campaigns":[{"Name":"a","Status":"Active","DailyBudget":5}]}`},
		{"element with zero Id", `{"Campaigns":[{"Id":0,"Name":"a","Status":"Active","DailyBudget":5}]}`},
		{"deleted element with no Id", `{"Campaigns":[{"Name":"a","Status":"Deleted"}]}`},
		{"malformed BudgetId", `{"Campaigns":[{"Id":1,"Status":"Active","DailyBudget":5,"BudgetId":-7}]}`},
		{"not an object", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitorServer(t)
			m.campaignsBody = tc.body
			c := newMonitorClient(t, m)
			got, err := c.ListAccountCampaigns(context.Background())
			if err == nil {
				t.Errorf("want an error, got %+v", got)
			}
		})
	}
}

// checkAndCountPolls runs one Check and asserts it issued exactly one Poll for the right id.
func checkAndCountPolls(t *testing.T, m *monitorServer, c *Client) (*AccountReportResult, error) {
	t.Helper()
	res, err := c.CheckAccountCampaignReport(context.Background(), "rr-acct-1")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 1 {
		t.Errorf("Check issued %d Poll requests, want exactly 1", m.pollCount)
	}
	if !bytes.Contains(m.pollRaw, []byte(`"ReportRequestId":"rr-acct-1"`)) {
		t.Errorf("Poll body does not name the report id: %s", m.pollRaw)
	}
	return res, err
}

func TestCheckAccountCampaignReport_Pending(t *testing.T) {
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Pending"}}`
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != AccountReportStatusPending || res.Rows != nil {
		t.Errorf("got %+v, want Pending with no rows", res)
	}
}

func TestCheckAccountCampaignReport_Error(t *testing.T) {
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Error"}}`
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != AccountReportStatusError {
		t.Errorf("got %+v, want Error", res)
	}
}

func TestCheckAccountCampaignReport_UnknownOrAbsentStatusIsAnError(t *testing.T) {
	for _, reply := range []string{
		`{"ReportRequestStatus":{"Status":"Building"}}`,
		`{"ReportRequestStatus":{}}`,
		`{}`,
	} {
		m := newMonitorServer(t)
		m.pollReply = reply
		res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
		if err == nil {
			t.Errorf("reply %s: want an error, got %+v", reply, res)
		}
	}
}

func TestCheckAccountCampaignReport_SuccessWithoutURLIsZeroRows(t *testing.T) {
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success"}}`
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != AccountReportStatusSuccess || res.Rows == nil || len(res.Rows) != 0 || res.Partial {
		t.Errorf("got %+v, want Success with a non-nil empty Rows", res)
	}
}

func TestCheckAccountCampaignReport_SuccessParsesPerCampaign(t *testing.T) {
	const csvBody = `"Report Name:","Campaign Performance Report"
"Report Time:","9/28/2026 - 10/4/2026"
"Potential Incomplete Data: true"

"CampaignId","Impressions","Clicks","Spend","ConversionsQualified"
"111","1000","50","25.50","2.5"
"222","300","9","4.00",""
"111","500","10","4.50","1"
"222","100","1","1.00","3"
"333","0","0","0.00","0"
"@2026 Microsoft Corporation. All rights reserved. "
`
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zipPayload = buildReportZip(t, csvBody)
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Status != AccountReportStatusSuccess {
		t.Errorf("Status = %q, want Success", res.Status)
	}
	if !res.Partial {
		t.Errorf("Partial = false; the preamble flags Potential Incomplete Data: true")
	}
	if len(res.Rows) != 3 {
		t.Fatalf("got %d rows, want 3 (111 summed, 222 summed, 333): %+v", len(res.Rows), res.Rows)
	}
	r111, r222, r333 := res.Rows[0], res.Rows[1], res.Rows[2]
	if r111.CampaignID != "111" || r111.Impressions != 1500 || r111.Clicks != 60 || r111.Spend != 30 {
		t.Errorf("campaign 111 = %+v, want duplicate rows summed to 1500/60/30", r111)
	}
	if r111.Conversions == nil || *r111.Conversions != 3.5 {
		t.Errorf("campaign 111 conversions = %v, want 3.5", r111.Conversions)
	}
	// One blank cell withdraws 222's conversions ONLY — not 111's or 333's.
	if r222.CampaignID != "222" || r222.Impressions != 400 || r222.Clicks != 10 || r222.Spend != 5 {
		t.Errorf("campaign 222 = %+v, want 400/10/5", r222)
	}
	if r222.Conversions != nil {
		t.Errorf("campaign 222 conversions = %v, want nil (a blank cell is unknown, not zero)", *r222.Conversions)
	}
	if r333.Conversions == nil || *r333.Conversions != 0 {
		t.Errorf("campaign 333 conversions = %v, want a measured 0", r333.Conversions)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloadAuth != "" {
		t.Errorf("the pre-signed download must not carry our bearer token, got %q", m.downloadAuth)
	}
}

func TestCheckAccountCampaignReport_CompleteReportIsNotPartial(t *testing.T) {
	const csvBody = `"Report Name:","Campaign Performance Report"
"Potential Incomplete Data: false"

"CampaignId","Impressions","Clicks","Spend"
"111","10","1","1.00"
`
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zipPayload = buildReportZip(t, csvBody)
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Partial {
		t.Errorf("Partial = true for a report flagged false")
	}
	if len(res.Rows) != 1 || res.Rows[0].Conversions != nil {
		t.Errorf("absent ConversionsQualified column must leave Conversions nil: %+v", res.Rows)
	}
}

func TestCheckAccountCampaignReport_HeaderOnlyIsZeroRows(t *testing.T) {
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zipPayload = buildReportZip(t, "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n\"©2026 Microsoft Corporation.\"\n")
	res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Rows == nil || len(res.Rows) != 0 {
		t.Errorf("header-only account report must be zero rows, got %+v", res.Rows)
	}
}

func TestCheckAccountCampaignReport_RefusesBadReports(t *testing.T) {
	const hdr = "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n"
	for _, tc := range []struct{ name, csv string }{
		{"empty CampaignId", hdr + "\"\",\"10\",\"1\",\"1.00\"\n"},
		{"non-numeric CampaignId", hdr + "\"abc\",\"10\",\"1\",\"1.00\"\n"},
		{"missing Spend column", "\"CampaignId\",\"Impressions\",\"Clicks\"\n\"1\",\"10\",\"1\"\n"},
		{"missing CampaignId column", "\"Impressions\",\"Clicks\",\"Spend\",\"CampaignName\"\n\"10\",\"1\",\"1.00\",\"x\"\n"},
		{"negative impressions", hdr + "\"1\",\"-10\",\"1\",\"1.00\"\n"},
		{"NaN spend", hdr + "\"1\",\"10\",\"1\",\"NaN\"\n"},
		{"negative conversions", "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\",\"ConversionsQualified\"\n\"1\",\"10\",\"1\",\"1.00\",\"-1\"\n"},
		{"impressions overflow", hdr + fmt.Sprintf("\"1\",\"%d\",\"0\",\"0\"\n\"1\",\"1\",\"0\",\"0\"\n", int64(^uint64(0)>>1))},
		{"short row", hdr + "\"1\",\"10\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitorServer(t)
			m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
			m.zipPayload = buildReportZip(t, tc.csv)
			res, err := checkAndCountPolls(t, m, newMonitorClient(t, m))
			if err == nil {
				t.Errorf("want an error, got %+v", res)
			}
		})
	}
}

func TestCheckAccountCampaignReport_RejectsEmptyReportID(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)
	if _, err := c.CheckAccountCampaignReport(context.Background(), " "); err == nil {
		t.Error("want an error for an empty report id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 0 {
		t.Errorf("an empty id must not be polled; got %d polls", m.pollCount)
	}
}
