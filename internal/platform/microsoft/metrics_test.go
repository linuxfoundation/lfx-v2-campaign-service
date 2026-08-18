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
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	submitRaw    []byte
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
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.submitBody = body
		m.submitRaw = raw
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

// TestGetCampaignMetrics_SuccessWithNoURLIsNotAZero covers Microsoft's "report built, no
// downloadable file" answer. An earlier revision returned zeroes here, reasoning that
// Microsoft omits the file rather than shipping a header-only CSV — but that is a SHAPE
// claim about a contract this client declares unverified, and the adapter cannot tell "the
// campaign served nothing" from "no such campaign in this account's scope". It must answer
// the sentinel so the dispatcher can map it to domain.ErrNoMetricsInWindow, never a number.
func TestGetCampaignMetrics_SuccessWithNoURLIsNotAZero(t *testing.T) {
	m := newMSMetricsServer(t, nil, 0)
	m.omitURL = true
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast30Days)
	if !errors.Is(err, ErrNoRowsInReport) {
		t.Errorf("want ErrNoRowsInReport, got err=%v", err)
	}
	if got != nil {
		t.Errorf("a no-rows answer must not render as metrics, got %+v", got)
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
	if first["CampaignId"] == nil {
		t.Errorf("Scope.Campaigns[0] has no CampaignId: %+v", first)
	}
	// Assert the WIRE form, not the decoded value: Microsoft types these ids as `long`,
	// and decoding into `any` turns both a quoted string and a bare number into float64,
	// so the map cannot tell them apart. A quoted id is rejected by the live API.
	if bytes.Contains(m.submitRaw, []byte(`"CampaignId":"`)) {
		t.Errorf("CampaignId went out QUOTED; Microsoft types it as long: %s", m.submitRaw)
	}
	if !bytes.Contains(m.submitRaw, []byte(`"CampaignId":1234567`)) {
		t.Errorf("CampaignId is not a bare number on the wire: %s", m.submitRaw)
	}
	if bytes.Contains(m.submitRaw, []byte(`"AccountIds":["`)) {
		t.Errorf("AccountIds went out QUOTED: %s", m.submitRaw)
	}
	if !bytes.Contains(m.submitRaw, []byte(`"ReportTimeZone"`)) {
		t.Errorf("ReportTimeZone missing; Microsoft defaults to Pacific: %s", m.submitRaw)
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

// advancingClock returns a clock that moves forward by step on every call, so the poll
// budget can actually expire. The suite's default clock is a CONSTANT, which makes
// `c.now().Add(reportPollInterval).Before(deadline)` permanently true — the budget check
// can never fire and ErrReportNotReady is unreachable from any test using it.
func advancingClock(start time.Time, step time.Duration) func() time.Time {
	n := 0
	return func() time.Time {
		t := start.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// TestGetCampaignMetrics_BudgetExpiryReturnsNotReady drives a server that never leaves
// Pending and asserts the budget produces ErrReportNotReady rather than leaking the
// caller's context error. Without an advancing clock this branch cannot be reached.
func TestGetCampaignMetrics_BudgetExpiryReturnsNotReady(t *testing.T) {
	// pollsBefore far beyond the budget: the server stays Pending forever.
	m := newMSMetricsServer(t, buildReportZip(t, realisticCSV), 100000)
	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
		WithReportingBaseURL(m.srv.URL),
		WithTokenURL(m.srv.URL+"/token"),
		WithClock(advancingClock(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC), reportPollInterval)),
	)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatal("expected an error when the report never leaves Pending")
	}
	if !errors.Is(err, ErrReportNotReady) {
		t.Errorf("want ErrReportNotReady, got %v", err)
	}
}

// TestReportPollBudgetStaysUnderTheMetricsCallTimeout pins the relationship the budget
// depends on. Orchestrator.ReadCampaignMetrics bounds every metrics call at 20s; if the
// poll budget ever reaches that, the caller's context cancels first and ErrReportNotReady
// becomes dead code in production while every test still passes.
func TestReportPollBudgetStaysUnderTheMetricsCallTimeout(t *testing.T) {
	// Mirrors internal/service/orchestrator.go's metricsCallTimeout. Duplicated rather
	// than imported: internal/platform must not depend on internal/service.
	const metricsCallTimeout = 20 * time.Second
	if reportPollBudget >= metricsCallTimeout {
		t.Fatalf("reportPollBudget (%s) must be < metricsCallTimeout (%s), or the caller's "+
			"context cancels before the budget and ErrReportNotReady is unreachable",
			reportPollBudget, metricsCallTimeout)
	}
}

// TestDownloadReportErrorsOmitThePresignedURL pins the redaction. The download URL's
// query string IS the credential, and net/http's *url.Error renders the whole URL —
// so wrapping the transport cause leaks sig= into any log that prints the error.
func TestDownloadReportErrorsOmitThePresignedURL(t *testing.T) {
	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
	)
	const secret = "SECRETSIGNATUREVALUE"
	u := "https://nonexistent.invalid.example/report.zip?skoid=ACCT&sig=" + secret

	_, err := c.downloadReport(context.Background(), u, "1234567", model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatal("expected a transport error against an unroutable host")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("pre-signed signature leaked into the error: %s", err.Error())
	}
	if strings.Contains(err.Error(), "nonexistent.invalid.example") {
		t.Errorf("download URL leaked into the error: %s", err.Error())
	}
}

// TestDownloadReportPreservesContextSentinels pins that a cancelled or timed-out download
// still matches errors.Is. An earlier revision wrapped context.Cause(ctx), which is nil
// when http.Client.Timeout fires while the caller's context is still live — that renders
// %!w(<nil>) and silently stops matching the sentinels a caller branches on.
func TestDownloadReportPreservesContextSentinels(t *testing.T) {
	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.downloadReport(ctx, "https://nonexistent.invalid.example/r.zip?sig=SECRET",
		"1234567", model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled download must still match context.Canceled, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "%!w") {
		t.Errorf("error is malformed or leaks the URL: %s", err.Error())
	}
}

// TestParseReportZip_OversizeCSVIsRefusedNotTruncated proves an oversized DECOMPRESSED CSV
// is an error rather than a partial total.
//
// The payload is aligned so that byte reportDownloadCap+1 falls exactly on a row boundary.
// That matters: io.LimitReader signals EOF at its limit rather than erroring, so a prefix cut
// on a boundary is SYNTACTICALLY COMPLETE and csv.ReadAll accepts it without complaint. The
// pre-fix code therefore returned a clean-looking total that was short by every row past the
// limit — a wrong number, not a failure. Only the size check catches this; a parse error
// would not have been raised.
func TestParseReportZip_OversizeCSVIsRefusedNotTruncated(t *testing.T) {
	header := "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n"
	row := "\"1234567\",\"1\",\"0\",\"0.00\"\n"
	// Pad a preamble line so (preamble+header) + k*len(row) lands exactly on cap+1.
	pad := 0
	for (reportDownloadCap+1-len(header)-pad)%len(row) != 0 {
		pad++
	}
	var b strings.Builder
	if pad > 0 {
		b.WriteString(strings.Repeat("#", pad-1) + "\n")
	}
	b.WriteString(header)
	rowsWithinCap := (reportDownloadCap + 1 - b.Len()) / len(row)
	totalRows := rowsWithinCap + 5000
	for i := 0; i < totalRows; i++ {
		b.WriteString(row)
	}

	got, err := parseReportZip(buildReportZip(t, b.String()), "1234567", model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatalf("expected an oversize error; got Impressions=%d from a CSV of %d rows — %d rows were silently dropped",
			got.Impressions, totalRows, int64(totalRows)-got.Impressions)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should report the size cap, got: %v", err)
	}
}

// TestParseReportZip_AtCapIsAccepted pins the boundary the +1 exists to draw: a CSV exactly
// AT the cap is valid, so the guard above rejects only what genuinely exceeds it.
func TestParseReportZip_AtCapIsAccepted(t *testing.T) {
	header := "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n"
	row := "\"1234567\",\"1\",\"0\",\"0.00\"\n"
	pad := 0
	for (reportDownloadCap-len(header)-pad)%len(row) != 0 {
		pad++
	}
	var b strings.Builder
	if pad > 0 {
		b.WriteString(strings.Repeat("#", pad-1) + "\n")
	}
	b.WriteString(header)
	rows := (reportDownloadCap - b.Len()) / len(row)
	for i := 0; i < rows; i++ {
		b.WriteString(row)
	}
	if b.Len() != reportDownloadCap {
		t.Fatalf("test setup: csv is %d bytes, wanted exactly %d", b.Len(), reportDownloadCap)
	}

	got, err := parseReportZip(buildReportZip(t, b.String()), "1234567", model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("a CSV exactly at the cap must be accepted: %v", err)
	}
	if got.Impressions != int64(rows) {
		t.Errorf("impressions = %d, want %d — every row at the cap must be counted", got.Impressions, rows)
	}
}

// TestGetCampaignMetrics_ShortDataRowIsAnErrorNotADrop proves a data row with a missing
// trailing field is REFUSED rather than silently discarded.
//
// The trailer filter used to drop any row narrower than the header, on the stated assumption
// that "a DATA row always carries the full column set" — an assumption about a contract this
// file declares UNVERIFIED. A short row's impressions/clicks/spend then vanished into a total
// that still looked clean. This is the same class the missing-column guard refuses by name.
func TestGetCampaignMetrics_ShortDataRowIsAnErrorNotADrop(t *testing.T) {
	const shortRow = `"Report Name:","Campaign Performance Report"

"CampaignId","Impressions","Clicks","Spend"
"1234567","10000","250","125.50"
"1234567","5000","100"
"©2026 Microsoft Corporation. All rights reserved."
`
	m := newMSMetricsServer(t, buildReportZip(t, shortRow), 0)
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowToday)
	if err == nil {
		t.Fatalf("expected an error for a truncated data row; got impressions=%d clicks=%d cost=%d — the short row was dropped silently",
			got.Impressions, got.Clicks, got.CostMicros)
	}
	if !strings.Contains(err.Error(), "wanted column") {
		t.Errorf("error should come from the short-row column check, got: %v", err)
	}
}

// TestDropTrailerRows_IdentifiesTrailerPositively proves the filter keeps a short DATA row
// (so it can reach the column check) while still removing the blank and © trailer lines.
func TestDropTrailerRows_IdentifiesTrailerPositively(t *testing.T) {
	in := [][]string{
		{"1234567", "10000", "250", "125.50"},
		{"1234567", "5000", "100"}, // short DATA row — must SURVIVE
		{"", "", "", ""},           // blank — must be dropped
		{},                         // empty record — must be dropped
		{"©2026 Microsoft Corporation. All rights reserved."}, // trailer — must be dropped
	}
	out := dropTrailerRows(in)
	if len(out) != 2 {
		t.Fatalf("kept %d rows, want 2 (both data rows): %v", len(out), out)
	}
	if len(out[1]) != 3 {
		t.Errorf("the short data row must survive so parseReportInt can report it, got %v", out[1])
	}
}

// TestFoldReportRows_TotalsAreOverflowChecked proves the running TOTALS are guarded, not just
// the per-row values.
//
// Each row below is individually valid — the per-row guards accept it. Only the accumulation
// wraps. Note every case needs TWO rows: the total starts at zero, so a single row can never
// trip a guard on the running total.
func TestFoldReportRows_TotalsAreOverflowChecked(t *testing.T) {
	maxI64 := strconv.FormatInt(math.MaxInt64, 10)
	// A spend whose micros are ~60% of MaxInt64: valid alone, overflowing when doubled.
	bigSpend := strconv.FormatFloat(float64(math.MaxInt64)/1e6*0.6, 'f', 2, 64)

	cases := []struct {
		name    string
		csv     string
		wantErr string
	}{
		{
			name:    "impressions",
			csv:     "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n\"1\",\"" + maxI64 + "\",\"0\",\"0.00\"\n\"1\",\"" + maxI64 + "\",\"0\",\"0.00\"\n",
			wantErr: "impressions: total would overflow",
		},
		{
			name:    "clicks",
			csv:     "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n\"1\",\"0\",\"" + maxI64 + "\",\"0.00\"\n\"1\",\"0\",\"" + maxI64 + "\",\"0.00\"\n",
			wantErr: "clicks: total would overflow",
		},
		{
			name:    "cost",
			csv:     "\"CampaignId\",\"Impressions\",\"Clicks\",\"Spend\"\n\"1\",\"0\",\"0\",\"" + bigSpend + "\"\n\"1\",\"0\",\"0\",\"" + bigSpend + "\"\n",
			wantErr: "spend: cost total would overflow",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReportZip(buildReportZip(t, tc.csv), "1", model.MetricsWindowToday)
			if err == nil {
				t.Fatalf("expected an overflow error; got impressions=%d clicks=%d cost=%d (a wrapped total renders as a measurement)",
					got.Impressions, got.Clicks, got.CostMicros)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestFoldReportRows_MultiRowTotalsStillSum guards the overflow checks against being written
// so tightly they reject ordinary multi-row reports. The realistic CSV has two data rows.
func TestFoldReportRows_MultiRowTotalsStillSum(t *testing.T) {
	got, err := parseReportZip(buildReportZip(t, realisticCSV), "1234567", model.MetricsWindowToday)
	if err != nil {
		t.Fatalf("parseReportZip: %v", err)
	}
	if got.Impressions != 15000 || got.Clicks != 350 || got.CostMicros != 200_000_000 {
		t.Errorf("impressions=%d clicks=%d cost=%d, want 15000/350/200000000",
			got.Impressions, got.Clicks, got.CostMicros)
	}
}
