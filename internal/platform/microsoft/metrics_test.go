// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"archive/zip"
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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// buildReportZip produces a real ZIP containing a real CSV in the shape Microsoft's
// Reporting service returns: metadata preamble, header row, data rows, copyright trailer.
// Building the archive for real (rather than stubbing the parser) is what makes these tests
// exercise the zip + csv path the live service would drive.
func buildReportZip(t *testing.T, csvBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("CampaignPerformanceReport.csv")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := io.WriteString(w, csvBody); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// realisticCSV is the shape Microsoft documents: a metadata preamble BEFORE the header, and
// a copyright line after the data. A parser that assumed records[0] was the header would
// read "Report Name" as a column name and find none of the metric columns.
const realisticCSV = `"Report Name:","Campaign Performance Report"
"Report Time:","8/1/2026 - 8/14/2026"
"Account:","LF Events"

"CampaignId","Impressions","Clicks","Spend"
"1234567","10000","250","125.50"
"1234567","5000","100","74.50"
"©2026 Microsoft Corporation. All rights reserved."
`

// msMetricsServer stands in for the three Microsoft surfaces this pipeline touches: the
// OAuth token endpoint, the Reporting service, and the pre-signed storage download.
type msMetricsServer struct {
	srv *httptest.Server

	mu           sync.Mutex
	submitBody   map[string]any
	pollCount    int
	pollsBefore  int // number of Pending polls before Success
	reportStatus string
	zipPayload   []byte
	downloadAuth string // Authorization header seen on the download request
	omitURL      bool
}

func newMSMetricsServer(t *testing.T, zipPayload []byte, pollsBefore int) *msMetricsServer {
	t.Helper()
	m := &msMetricsServer{pollsBefore: pollsBefore, reportStatus: "Success", zipPayload: zipPayload}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})

	mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.submitBody = body
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ReportRequestId":"rr-1"}`)
	})

	mux.HandleFunc("/Reporting/v13/GenerateReport/Poll", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.pollCount++
		n := m.pollCount
		status := m.reportStatus
		omit := m.omitURL
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n <= m.pollsBefore {
			_, _ = io.WriteString(w, `{"ReportRequestStatus":{"Status":"Pending"}}`)
			return
		}
		if status != "Success" {
			_, _ = fmt.Fprintf(w, `{"ReportRequestStatus":{"Status":%q}}`, status)
			return
		}
		if omit {
			// Success with NO download URL — Microsoft's "report built, zero rows".
			_, _ = io.WriteString(w, `{"ReportRequestStatus":{"Status":"Success"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":%q}}`,
			m.srv.URL+"/download?sig=presigned")
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

func newMetricsClient(t *testing.T, m *msMetricsServer) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
		WithReportingBaseURL(m.srv.URL),
		WithTokenURL(m.srv.URL+"/token"),
		WithClock(func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }),
	)
}

// TestGetCampaignMetrics_HappyPath drives the whole pipeline — submit, poll through a
// Pending, download a real ZIP, parse a realistic CSV — and asserts the totals.
func TestGetCampaignMetrics_HappyPath(t *testing.T) {
	m := newMSMetricsServer(t, buildReportZip(t, realisticCSV), 1)
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	// Both data rows must be summed; a parser that stopped at the first row would report
	// 10000/250 and look entirely plausible.
	if got.Impressions != 15000 {
		t.Errorf("impressions = %d, want 15000 (both rows summed)", got.Impressions)
	}
	if got.Clicks != 350 {
		t.Errorf("clicks = %d, want 350 (both rows summed)", got.Clicks)
	}
	// 125.50 + 74.50 = 200.00 -> 200_000_000 micros.
	if got.CostMicros != 200_000_000 {
		t.Errorf("costMicros = %d, want 200000000", got.CostMicros)
	}
	// Ctr is derived, not read: 350/15000.
	if want := 350.0 / 15000.0; got.Ctr != want {
		t.Errorf("ctr = %v, want %v", got.Ctr, want)
	}
	if got.CampaignID != "1234567" {
		t.Errorf("campaignID = %q, want 1234567", got.CampaignID)
	}
	if got.Window != model.MetricsWindowLast7Days {
		t.Errorf("window = %q, want last_7_days", got.Window)
	}
}

// TestGetCampaignMetrics_DownloadOmitsBearerToken pins a security property: the download URL
// is a PRE-SIGNED storage URL, not an API endpoint, so our OAuth bearer token must not be
// attached to it. Sending it would disclose an API credential to a storage host.
func TestGetCampaignMetrics_DownloadOmitsBearerToken(t *testing.T) {
	m := newMSMetricsServer(t, buildReportZip(t, realisticCSV), 0)
	c := newMetricsClient(t, m)

	if _, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloadAuth != "" {
		t.Errorf("download request carried an Authorization header (%q) — the pre-signed URL must not receive our bearer token", m.downloadAuth)
	}
}

// TestGetCampaignMetrics_SuccessWithNoURLIsAConfirmedZero covers Microsoft's "report built,
// no rows" answer. That is the ONE path where zeroes are the truthful result, precisely
// because the pipeline reported Success.
func TestGetCampaignMetrics_SuccessWithNoURLIsAConfirmedZero(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	m.omitURL = true
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if got.Impressions != 0 || got.Clicks != 0 || got.CostMicros != 0 {
		t.Errorf("want a zeroed result, got %+v", got)
	}
}

// TestGetCampaignMetrics_ReportErrorFailsFast proves a terminal Error status does not burn
// the whole poll budget: retrying cannot help, so it must fail on the first Error.
func TestGetCampaignMetrics_ReportErrorFailsFast(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	m.reportStatus = "Error"
	c := newMetricsClient(t, m)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error for a failed report build")
	}
	if errors.Is(err, ErrReportNotReady) {
		t.Errorf("a terminal Error must not be reported as not-ready: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 1 {
		t.Errorf("polled %d times; a terminal Error must stop after the first poll", m.pollCount)
	}
}

// TestGetCampaignMetrics_UnrecognizedStatusIsNotPending pins that an unknown status fails
// loudly. Treating it as Pending would spend the entire budget and then report a timeout,
// hiding the contract change that actually occurred.
func TestGetCampaignMetrics_UnrecognizedStatusIsNotPending(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	m.reportStatus = "Frobnicating"
	c := newMetricsClient(t, m)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error for an unrecognized status")
	}
	if !strings.Contains(err.Error(), "Frobnicating") {
		t.Errorf("error should name the unrecognized status, got: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 1 {
		t.Errorf("polled %d times; an unrecognized status must not loop", m.pollCount)
	}
}

// TestGetCampaignMetrics_MissingColumnsRefused proves a CSV without the metric columns is an
// ERROR, not a zeroed result. A zero from an absent column is indistinguishable, to every
// consumer, from a measured zero — the failure-as-measurement class.
func TestGetCampaignMetrics_MissingColumnsRefused(t *testing.T) {
	const noMetrics = `"CampaignId","SomethingElse"
"1234567","x"
`
	m := newMSMetricsServer(t, buildReportZip(t, noMetrics), 0)
	c := newMetricsClient(t, m)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error when the report has no metric columns")
	}
	if !strings.Contains(err.Error(), "missing required columns") {
		t.Errorf("error should name the missing columns, got: %v", err)
	}
}

// TestGetCampaignMetrics_ColumnsResolvedByName proves the parser reads columns by HEADER
// NAME, not by position. Microsoft's report writer chooses its own column order; a
// positional read would swap Clicks and Spend and produce plausible, wrong numbers.
func TestGetCampaignMetrics_ColumnsResolvedByName(t *testing.T) {
	// Deliberately reordered relative to the request: Spend before Clicks.
	const reordered = `"CampaignId","Impressions","Spend","Clicks"
"1234567","1000","50.00","25"
`
	m := newMSMetricsServer(t, buildReportZip(t, reordered), 0)
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if got.Clicks != 25 {
		t.Errorf("clicks = %d, want 25 — columns must resolve by name, not position", got.Clicks)
	}
	if got.CostMicros != 50_000_000 {
		t.Errorf("costMicros = %d, want 50000000 — columns must resolve by name, not position", got.CostMicros)
	}
}

// TestGetCampaignMetrics_SubmitBodyShape asserts the request the live service would receive:
// the campaign scope, the account, and the {Month,Day,Year} date objects (Microsoft rejects
// an ISO-8601 string for these).
func TestGetCampaignMetrics_SubmitBodyShape(t *testing.T) {
	m := newMSMetricsServer(t, buildReportZip(t, realisticCSV), 0)
	c := newMetricsClient(t, m)

	if _, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	req, _ := m.submitBody["ReportRequest"].(map[string]any)
	if req == nil {
		t.Fatalf("submit body has no ReportRequest: %+v", m.submitBody)
	}
	if req["Format"] != "Csv" {
		t.Errorf("Format = %v, want Csv", req["Format"])
	}
	if req["Aggregation"] != "Summary" {
		t.Errorf("Aggregation = %v, want Summary", req["Aggregation"])
	}
	scope, _ := req["Scope"].(map[string]any)
	if scope == nil {
		t.Fatalf("ReportRequest has no Scope: %+v", req)
	}
	camps, _ := scope["Campaigns"].([]any)
	if len(camps) != 1 {
		t.Fatalf("Scope.Campaigns = %v, want exactly the one campaign asked about", scope["Campaigns"])
	}
	first, _ := camps[0].(map[string]any)
	if first["CampaignId"] != "1234567" {
		t.Errorf("Scope.Campaigns[0].CampaignId = %v, want 1234567", first["CampaignId"])
	}
	// The date must be the object form. A string here would be accepted by our own fake
	// but rejected by Microsoft — exactly the class of bug a fake cannot catch, so the
	// shape is asserted explicitly.
	tm, _ := req["Time"].(map[string]any)
	start, _ := tm["CustomDateRangeStart"].(map[string]any)
	if start == nil {
		t.Fatalf("CustomDateRangeStart must be a {Month,Day,Year} object, got %T: %v", tm["CustomDateRangeStart"], tm["CustomDateRangeStart"])
	}
	// Clock is pinned to 2026-08-14; last_7_days starts 6 days earlier, on the 8th.
	if start["Year"] != float64(2026) || start["Month"] != float64(8) || start["Day"] != float64(8) {
		t.Errorf("CustomDateRangeStart = %v, want 2026-08-08 for last_7_days at the pinned clock", start)
	}
}

// TestGetCampaignMetrics_UnsupportedWindow pins the sentinel the dispatcher classifies on.
func TestGetCampaignMetrics_UnsupportedWindow(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	c := newMetricsClient(t, m)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindow("last_decade"))
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("want ErrUnsupportedWindow, got %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 0 || m.submitBody != nil {
		t.Error("an unsupported window must be refused BEFORE any upstream request")
	}
}

// TestGetCampaignMetrics_RejectsNonNumericCampaignID keeps a caller-supplied value that
// reaches a request body under the same discipline as the account ids.
func TestGetCampaignMetrics_RejectsNonNumericCampaignID(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	c := newMetricsClient(t, m)

	for _, bad := range []string{"", "  ", "abc", "123abc", "12 34"} {
		if _, err := c.GetCampaignMetrics(context.Background(), bad, model.MetricsWindowToday); err == nil {
			t.Errorf("campaign id %q was accepted; must be digits only", bad)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.submitBody != nil {
		t.Error("an invalid campaign id must be refused BEFORE any upstream request")
	}
}

// TestGetCampaignMetrics_ContextCancelStopsPolling proves the poll loop honours the caller's
// context rather than sleeping out its full budget after the caller has given up.
func TestGetCampaignMetrics_ContextCancelStopsPolling(t *testing.T) {
	m := newMSMetricsServer(t, nil, 1000) // never reaches Success
	c := newMetricsClient(t, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetCampaignMetrics(ctx, "1234567", model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
