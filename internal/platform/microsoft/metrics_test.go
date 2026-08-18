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
//
// The trailer here uses "@", which is what Microsoft's own published sample report and its
// ExcludeReportFooter description both show — "@2020 Microsoft Corporation. All rights
// reserved. ", including the trailing space reproduced below. The "©" spelling is exercised
// by headerOnlyCSV and TestDropTrailerRows_IdentifiesTrailerPositively instead.
//
// Splitting the two markers across fixtures is deliberate and is the point of this comment.
// Every fixture in this file once used "©" — the same character dropTrailerRows matched on —
// so the suite could only ever prove the parser agreed with itself. A single-marker
// regression passed every test while failing every live report, because the real footer
// would survive the filter, reach foldReportRows as a data row, and fail parseReportInt.
// With the markers split, reverting dropTrailerRows to either one alone fails a test.
const realisticCSV = `"Report Name:","Campaign Performance Report"
"Report Time:","8/1/2026 - 8/14/2026"
"Account:","LF Events"

"CampaignId","Impressions","Clicks","Spend"
"1234567","10000","250","125.50"
"1234567","5000","100","74.50"
"@2026 Microsoft Corporation. All rights reserved. "
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

// headerOnlyCSV is the OTHER shape of "the report completed with no data": Microsoft ships
// the file, with its metadata preamble, its full header naming every metric column, and its
// copyright trailer — but not one data row between them.
const headerOnlyCSV = `"Report Name:","Campaign Performance Report"
"Report Time:","8/1/2026 - 8/14/2026"
"Account:","LF Events"

"CampaignId","Impressions","Clicks","Spend"
"©2026 Microsoft Corporation. All rights reserved."
`

// (This fixture deliberately keeps the "©" spelling while realisticCSV uses "@" — see the
// note on realisticCSV for why the two markers are split across fixtures.)

// TestGetCampaignMetrics_HeaderOnlyReportIsNotAZero is the download-side twin of
// TestGetCampaignMetrics_SuccessWithNoURLIsNotAZero. That test covers the door where
// Microsoft omits the file; this one covers the door where it ships a file containing only
// a header. Both mean "the report completed carrying no data", and the adapter can no more
// distinguish "the campaign served nothing" from "no such campaign in this account's scope"
// in one than in the other — so both must answer ErrNoRowsInReport, never a zero.
//
// The missing-columns guard does NOT cover this: a header-only file NAMES all three metric
// columns, so the lookups succeed and the fold runs over an empty row set, which without the
// explicit guard returns a zero-valued CampaignMetrics and a nil error.
func TestGetCampaignMetrics_HeaderOnlyReportIsNotAZero(t *testing.T) {
	m := newMSMetricsServer(t, buildReportZip(t, headerOnlyCSV), 0)
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
	if !errors.Is(err, ErrNoRowsInReport) {
		t.Fatalf("want ErrNoRowsInReport for a header-only report, got err=%v", err)
	}
	if got != nil {
		t.Errorf("a header-only report must not render as metrics, got %+v", got)
	}
}

// TestFoldReportRows_HeaderOnlyIsNotAZero pins the guard at the unit it lives in, so a
// refactor that moves the ZIP/CSV plumbing cannot quietly drop it. Asserting the zero-value
// shape explicitly is the point: a nil error here would hand every consumer a measurement of
// zero impressions, zero clicks and zero spend, synthesized from a file that measured
// nothing at all.
func TestFoldReportRows_HeaderOnlyIsNotAZero(t *testing.T) {
	records := [][]string{
		{"Report Name:", "Campaign Performance Report"},
		{"CampaignId", "Impressions", "Clicks", "Spend"},
		{"©2026 Microsoft Corporation. All rights reserved."},
	}
	got, err := foldReportRows(records, "1234567", model.MetricsWindowLast7Days)
	if !errors.Is(err, ErrNoRowsInReport) {
		t.Fatalf("want ErrNoRowsInReport, got err=%v metrics=%+v", err, got)
	}
	if got != nil {
		t.Errorf("want no metrics alongside the sentinel, got %+v", got)
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
	// Assert the WIRE form, not the decoded value: decoding into `any` turns both a quoted
	// string and a bare number into the same Go value, so the map cannot tell them apart.
	//
	// Reporting v13 wants these ids QUOTED. Microsoft's JSON reference for
	// CampaignReportScope renders "AccountId"/"CampaignId" as "LongValueHere" — quoted —
	// while ReportTime's Day/Month/Year on the same page render as IntValueHere, unquoted.
	// That `long`-quoted / `int`-bare contrast within one document is the type signal. This
	// deliberately differs from campaign.go, which sends bare numbers to Campaign
	// Management v13 — a DIFFERENT API, and not a precedent for this one. Quoting is also
	// the precision-safe direction: a 64-bit id exceeds what a JSON number holds exactly.
	if bytes.Contains(m.submitRaw, []byte(`"CampaignId":1234567`)) {
		t.Errorf("CampaignId went out as a BARE NUMBER; Reporting v13 quotes long ids, and a "+
			"bare number risks precision loss on a large id: %s", m.submitRaw)
	}
	if !bytes.Contains(m.submitRaw, []byte(`"CampaignId":"1234567"`)) {
		t.Errorf("CampaignId is not a quoted string on the wire: %s", m.submitRaw)
	}
	if !bytes.Contains(m.submitRaw, []byte(`"AccountId":"9999999"`)) {
		t.Errorf("AccountId is not a quoted string on the wire: %s", m.submitRaw)
	}
	// Scope must be the CAMPAIGN alone. AccountThroughCampaignReportScope UNIONs AccountIds
	// with Campaigns, so an AccountIds element alongside Campaigns widens this read to every
	// campaign in the account and foldReportRows sums them into an account-wide total
	// reported as one campaign's metrics — a valid request that returns a wrong number.
	//
	// Asserted on the RAW BYTES as well as the decoded map, and the wire check is the
	// load-bearing one — for the same reason the id-typing assertions above read raw bytes:
	// encoding/json collapses distinct wire forms onto the same decoded value, so the map
	// cannot pin what Microsoft actually receives. (A comma-ok key test does catch a JSON
	// null, which creates the key; it is kept as a second, independent statement of the same
	// requirement rather than as the primary evidence.)
	if bytes.Contains(m.submitRaw, []byte(`"AccountIds"`)) {
		t.Errorf("Scope.AccountIds is present on the wire; the scope is a UNION, so this "+
			"returns ACCOUNT-WIDE totals for a campaign-scoped read: %s", m.submitRaw)
	}
	if _, ok := scope["AccountIds"]; ok {
		t.Errorf("Scope.AccountIds decoded from the submit body: %+v", scope)
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

// TestSubmitReport_Error2027NamesTheScopeTradeoff pins the legibility half of the scope fix.
// Dropping Scope.AccountIds is correct per Microsoft's documented UNION semantics, but
// community reports claim error 2027 when it is omitted and no credentials exist to settle
// that. If 2027 DOES occur live, the error must name the cause in one read rather than
// leaving someone to rediscover the tradeoff. Both spellings are exercised because Microsoft
// returns a numeric Code on some services and a string ErrorCode on others.
func TestSubmitReport_Error2027NamesTheScopeTradeoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"numeric Code", `{"Code":2027,"Message":"scope rejected"}`},
		{"string ErrorCode", `{"ErrorCode":"InvalidAccountThruCampaignReportScope"}`},
		{"nested Errors array", `{"Errors":[{"Code":2027}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			})
			mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			c := NewClient(
				Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
				AccountConfig{AccountID: "9999999", Label: "LF Events"},
				WithReportingBaseURL(srv.URL),
				WithTokenURL(srv.URL+"/token"),
				WithClock(func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }),
			)

			_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
			if err == nil {
				t.Fatal("expected an error when submit is rejected")
			}
			msg := err.Error()
			// The point of this test is the DIAGNOSIS, not merely that an error happened.
			// A bare "microsoft-ads POST ... -> 400" is what this fix exists to replace.
			for _, want := range []string{"campaign-only report scope was REJECTED", "AccountIds", msErrCodeInvalidScope, msErrNameInvalidScope} {
				if !strings.Contains(msg, want) {
					t.Errorf("error must name %q so the cause reads in one pass; got: %s", want, msg)
				}
			}
		})
	}
}

// TestSubmitReport_UnrelatedErrorDoesNotClaimTheScopeCause guards the other direction: the
// 2027 arm must not annotate every submit failure with a scope diagnosis it has no evidence
// for. A misleading cause is worse than a bare status.
func TestSubmitReport_UnrelatedErrorDoesNotClaimTheScopeCause(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Code":1001,"Message":"something else"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
		WithReportingBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithClock(func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }),
	)

	_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatal("expected an error when submit is rejected")
	}
	if strings.Contains(err.Error(), "campaign-only report scope was REJECTED") {
		t.Errorf("an unrelated submit failure must NOT be reported as the scope tradeoff: %s", err)
	}
}

// TestErrReportNotReady_DoesNotPromiseAResumableReport pins the honest half of defect 2.
// The pending ReportRequestId is discarded with this sentinel and the synchronous
// MetricsReader contract has nowhere to return it, so a retry submits a NEW report rather
// than collecting the pending one. The sentinel's TEXT is what a caller reads in a log, so
// the disclaimer lives there and not only in a doc comment a reader may never open.
func TestErrReportNotReady_DoesNotPromiseAResumableReport(t *testing.T) {
	msg := ErrReportNotReady.Error()
	for _, want := range []string{"NEW report", "does not resume"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrReportNotReady must state that a retry restarts the report (missing %q): %s", want, msg)
		}
	}
}

// TestGetCampaignMetrics_RetryAfterNotReadySubmitsAFreshReport proves the claim above is a
// description of real behavior rather than a pessimistic comment: two calls that both time
// out issue two DISTINCT submits, so the second never polls the first's pending report. If
// resumption is ever built, this test is the one that must change.
func TestGetCampaignMetrics_RetryAfterNotReadySubmitsAFreshReport(t *testing.T) {
	// These captures are written from httptest's handler goroutine and read from the test
	// goroutine, so they are guarded — the same discipline msMetricsServer.mu applies to
	// every other capture in this file.
	//
	// -race does NOT currently flag the unguarded version, and that was measured rather
	// than assumed: removing the polledIDs lock and running -race -count=3 passes clean.
	// The reason is that net/http serves a connection from a single goroutine and this
	// client issues its requests strictly sequentially, so the handlers never actually
	// overlap (a probe counting distinct handler goroutines across 20 sequential httptest
	// requests reported exactly 1 — httptest spawns per CONNECTION, not per request).
	// The lock stays because that safety rests on an invariant nothing here states or
	// enforces: connection reuse plus a caller that never issues overlapping requests.
	// A t.Parallel() sub-test, a concurrent second call, or disabled keep-alives would
	// each break it and surface as an intermittent CI flake. See
	// docs/knowledge/log/2026-08-18-LFXV2-3260-header-only-and-test-race.md.
	var mu sync.Mutex
	var submits int
	var polledIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		submits++
		n := submits
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ReportRequestId":"rr-%d"}`, n)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Poll", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReportRequestID string `json:"ReportRequestId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		polledIDs = append(polledIDs, body.ReportRequestID)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ReportRequestStatus":{"Status":"Pending"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	newCall := func() error {
		c := NewClient(
			Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
			AccountConfig{AccountID: "9999999", Label: "LF Events"},
			WithReportingBaseURL(srv.URL),
			WithTokenURL(srv.URL+"/token"),
			WithClock(advancingClock(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC), reportPollInterval)),
		)
		_, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
		return err
	}

	if err := newCall(); !errors.Is(err, ErrReportNotReady) {
		t.Fatalf("first call: want ErrReportNotReady, got %v", err)
	}
	if err := newCall(); !errors.Is(err, ErrReportNotReady) {
		t.Fatalf("retry: want ErrReportNotReady, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if submits != 2 {
		t.Errorf("submits = %d, want 2 — each retry starts a NEW report", submits)
	}
	// The retry must NEVER have polled rr-1, the report the first call left building.
	// This is the defect stated as an assertion: resumption does not happen.
	var polledFirstAfterRetry bool
	for _, id := range polledIDs[len(polledIDs)/2:] {
		if id == "rr-1" {
			polledFirstAfterRetry = true
		}
	}
	if polledFirstAfterRetry {
		t.Error("the retry polled rr-1; if resumption now works, ErrReportNotReady's text and the dispatcher message must be updated")
	}
	if len(polledIDs) == 0 || polledIDs[0] != "rr-1" {
		t.Errorf("first call polled %v, want it to poll its own rr-1", polledIDs)
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
"@2026 Microsoft Corporation. All rights reserved. "
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
// (so it can reach the column check) while still removing the blank and copyright trailer
// lines — under BOTH documented spellings of the marker.
//
// Both spellings appear here on purpose. Microsoft's published sample and its
// ExcludeReportFooter description show "@"; earlier fixtures in this repo assumed "©".
// Recognising only one is not a cosmetic miss: the unrecognised footer survives the filter,
// reaches foldReportRows as a data row, and fails parseReportInt — so every otherwise
// successful live report fails to parse. Asserting both here means dropping either marker
// from dropTrailerRows fails this test.
func TestDropTrailerRows_IdentifiesTrailerPositively(t *testing.T) {
	in := [][]string{
		{"1234567", "10000", "250", "125.50"},
		{"1234567", "5000", "100"}, // short DATA row — must SURVIVE
		{"", "", "", ""},           // blank — must be dropped
		{},                         // empty record — must be dropped
		// Both markers, and the trailing space Microsoft's own sample carries.
		{"©2026 Microsoft Corporation. All rights reserved."},
		{"@2026 Microsoft Corporation. All rights reserved. "},
	}
	out := dropTrailerRows(in)
	if len(out) != 2 {
		t.Fatalf("kept %d rows, want 2 (both data rows): %v", len(out), out)
	}
	if len(out[1]) != 3 {
		t.Errorf("the short data row must survive so parseReportInt can report it, got %v", out[1])
	}
}

// TestDropTrailerRows_BothCopyrightMarkersAreRecognised pins each marker independently.
//
// The combined test above would still pass if one marker were handled only by the
// single-cell rule rather than by the marker match, so each spelling is asserted here as a
// TWO-cell row — a shape the single-cell rule cannot catch. That keeps the marker list
// itself load-bearing: delete either entry from reportTrailerMarkers and this fails.
func TestDropTrailerRows_BothCopyrightMarkersAreRecognised(t *testing.T) {
	for _, marker := range []string{"©", "@"} {
		t.Run(marker, func(t *testing.T) {
			in := [][]string{
				{"1234567", "10000", "250", "125.50"},
				// Two cells, so only the marker match can reject it.
				{marker + "2026 Microsoft Corporation. All rights reserved. ", "trailing"},
			}
			out := dropTrailerRows(in)
			if len(out) != 1 {
				t.Fatalf("marker %q: kept %d rows, want 1 (the data row only): %v", marker, len(out), out)
			}
			if out[0][0] != "1234567" {
				t.Errorf("marker %q: wrong row survived: %v", marker, out[0])
			}
		})
	}
}

// TestFoldReportRows_UnrecognisedTrailerWouldBreakEveryReport states the consequence the
// marker list exists to prevent, at the level a consumer sees it.
//
// A footer the filter does not recognise is not dropped quietly — it is folded in as data,
// where parseReportInt refuses its text. That turns EVERY successful report into a parse
// error, which is why this guard is worth a test of its own rather than only a unit
// assertion on dropTrailerRows.
func TestFoldReportRows_UnrecognisedTrailerWouldBreakEveryReport(t *testing.T) {
	for _, marker := range []string{"©", "@"} {
		t.Run(marker, func(t *testing.T) {
			records := [][]string{
				{"Report Name:", "Campaign Performance Report"},
				{"CampaignId", "Impressions", "Clicks", "Spend"},
				{"1234567", "10000", "250", "125.50"},
				{marker + "2026 Microsoft Corporation. All rights reserved. "},
			}
			got, err := foldReportRows(records, "1234567", model.MetricsWindowLast7Days)
			if err != nil {
				t.Fatalf("marker %q: the trailer was read as data: %v", marker, err)
			}
			if got.Impressions != 10000 || got.Clicks != 250 {
				t.Errorf("marker %q: totals = imp %d clicks %d, want 10000/250", marker, got.Impressions, got.Clicks)
			}
		})
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

// incompleteDataCSV reproduces Microsoft's documented header block for a report whose last
// day is still aggregating. The label and its value are one quoted cell, which is exactly how
// Microsoft's published sample renders every header line ("Key: Value" inside one pair of
// quotes).
//
// The rows below it are deliberately VALID and non-zero: the point is that the parser refuses
// on the flag rather than on anything wrong with the numbers. Without the check these totals
// would be returned as a complete measurement of 15000/350, indistinguishable from a campaign
// that really did serve exactly that.
const incompleteDataCSV = `"Report Name: Campaign Performance Report"
"Report Time: 8/1/2026 - 8/14/2026"
"Report Aggregation: Summary"
"Report Filter: "
"Potential Incomplete Data: true"
"Rows: 2"

"CampaignId","Impressions","Clicks","Spend"
"1234567","10000","250","125.50"
"1234567","5000","100","74.50"
"@2026 Microsoft Corporation. All rights reserved. "
`

// TestGetCampaignMetrics_IncompleteDataIsRefusedNotReported proves a report Microsoft flags as
// partial does not reach a consumer as a measurement.
//
// submitReport sends ReturnOnlyCompleteData=false — it must, because true fails outright with
// NoCompleteDataAvaliable (2004) on any window including today. The cost is that the totals may
// be an under-count, and Microsoft says so ONLY in the report's header block, never in the HTTP
// status. A partial total is indistinguishable from a complete measurement of a smaller number,
// so it is refused.
func TestGetCampaignMetrics_IncompleteDataIsRefusedNotReported(t *testing.T) {
	m := newMSMetricsServer(t, buildReportZip(t, incompleteDataCSV), 0)
	c := newMetricsClient(t, m)

	got, err := c.GetCampaignMetrics(context.Background(), "1234567", model.MetricsWindowLast7Days)
	if !errors.Is(err, ErrReportDataIncomplete) {
		t.Fatalf("want ErrReportDataIncomplete for a flagged report, got err=%v metrics=%+v", err, got)
	}
	if got != nil {
		t.Errorf("an incomplete report must not render as metrics, got %+v", got)
	}
}

// TestFoldReportRows_IncompleteDataFlagVariants pins WHICH values refuse and which do not.
//
// The negative cases carry the same weight as the positive ones. A complete report emits the
// same label with "false", and the whole header block disappears when ExcludeReportHeader is
// true — Microsoft states that with ReturnOnlyCompleteData=false "there is no indication as to
// whether the data is complete". So absence is not evidence of incompleteness, and refusing on
// it would fail every report rather than the partial ones. This guard refuses what Microsoft
// explicitly flags; it does not claim to prove completeness.
func TestFoldReportRows_IncompleteDataFlagVariants(t *testing.T) {
	dataRows := [][]string{
		{"CampaignId", "Impressions", "Clicks", "Spend"},
		{"1234567", "10000", "250", "125.50"},
	}
	cases := []struct {
		name     string
		preamble [][]string
		refuse   bool
	}{
		{"single cell true", [][]string{{"Potential Incomplete Data: true"}}, true},
		{"split cells true", [][]string{{"Potential Incomplete Data:", "true"}}, true},
		{"mixed case", [][]string{{"POTENTIAL INCOMPLETE DATA: TRUE"}}, true},
		{"yes", [][]string{{"Potential Incomplete Data: yes"}}, true},
		{"one", [][]string{{"Potential Incomplete Data: 1"}}, true},
		// Negatives: a complete report says so, and must NOT be refused.
		{"explicit false", [][]string{{"Potential Incomplete Data: false"}}, false},
		{"split cells false", [][]string{{"Potential Incomplete Data:", "false"}}, false},
		{"empty value", [][]string{{"Potential Incomplete Data: "}}, false},
		// Header block suppressed entirely (ExcludeReportHeader=true).
		{"label absent", [][]string{{"Report Name: Campaign Performance Report"}}, false},
		{"no preamble at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := append(append([][]string{}, tc.preamble...), dataRows...)
			got, err := foldReportRows(records, "1234567", model.MetricsWindowLast7Days)
			if tc.refuse {
				if !errors.Is(err, ErrReportDataIncomplete) {
					t.Fatalf("want ErrReportDataIncomplete, got err=%v metrics=%+v", err, got)
				}
				if got != nil {
					t.Errorf("want no metrics alongside the sentinel, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("must not refuse this report: %v", err)
			}
			if got.Impressions != 10000 {
				t.Errorf("impressions = %d, want 10000", got.Impressions)
			}
		})
	}
}

// TestFoldReportRows_IncompleteFlagIsCheckedBeforeTheColumns proves the flag is refused on its
// own terms rather than only when the rows happen to be readable.
//
// The header below names none of the metric columns, so the column guard would fire first if
// the order were reversed — reporting a shape problem and hiding the more important fact that
// the numbers were never complete. Whichever way the file is broken, "Microsoft told us this
// is partial" is the answer the caller needs.
func TestFoldReportRows_IncompleteFlagIsCheckedBeforeTheColumns(t *testing.T) {
	records := [][]string{
		{"Potential Incomplete Data: true"},
		{"CampaignId", "SomethingElse"},
		{"1234567", "42"},
	}
	if _, err := foldReportRows(records, "1234567", model.MetricsWindowLast7Days); !errors.Is(err, ErrReportDataIncomplete) {
		t.Fatalf("want ErrReportDataIncomplete ahead of the column guard, got %v", err)
	}
}

// TestDropTrailerRows_SingleCellTrailerWithoutAMarkerIsDropped pins the generic rule, which
// the marker assertions cannot reach.
//
// Every trailer fixture elsewhere in this file starts with "©" or "@", so the marker match
// alone would satisfy them and the single-cell rule could be deleted unnoticed. The row below
// carries NEITHER marker, so only the generic rule can reject it. That rule is what covers a
// footer whose wording or symbol this file has not anticipated — the whole point of not
// enumerating trailer text on an UNVERIFIED contract.
//
// It stays safe because a one-cell row could not have been a measurement anyway: foldReportRows
// resolves three named columns to read a row, so a single cell can never satisfy it.
func TestDropTrailerRows_SingleCellTrailerWithoutAMarkerIsDropped(t *testing.T) {
	in := [][]string{
		{"1234567", "10000", "250", "125.50"},
		{"Copyright 2026 Microsoft Corporation. All rights reserved."}, // no ©, no @
	}
	out := dropTrailerRows(in)
	if len(out) != 1 {
		t.Fatalf("kept %d rows, want 1 (the data row only): %v", len(out), out)
	}
	if out[0][0] != "1234567" {
		t.Errorf("wrong row survived: %v", out[0])
	}
}

// TestDropTrailerRows_MultiCellDataRowsAlwaysSurvive is the counterweight to the rule above:
// it proves the filter does NOT over-reject.
//
// A wrong rejection is a silent under-count — the failure class this file exists to prevent —
// and it is the risk any "looks like a trailer" heuristic carries. Every row below has two or
// more cells and no marker, so all of them must reach the fold, including the two-cell row
// that is far narrower than the header. Narrowness alone must never drop a row.
func TestDropTrailerRows_MultiCellDataRowsAlwaysSurvive(t *testing.T) {
	in := [][]string{
		{"1234567", "10000", "250", "125.50"},
		{"1234567", "5000", "100"},
		{"1234567", "1"},
	}
	out := dropTrailerRows(in)
	if len(out) != len(in) {
		t.Fatalf("kept %d rows, want all %d — a data row was dropped and its metrics would vanish silently: %v", len(out), len(in), out)
	}
}

// TestFoldReportRows_IncompleteFlagIsScopedToThePreamble proves the flag is read from the
// header block ONLY, not from anywhere in the file.
//
// Microsoft documents "Potential Incomplete Data" as a header-block line, above the column
// names. Scanning the whole file for it instead would let text in a DATA row refuse a report
// that Microsoft never flagged — a wrong rejection, which on this path means a healthy
// campaign reporting an error instead of its real numbers. That is the mirror of the
// under-count this guard exists to prevent, and it is just as dishonest, so the search is
// bounded to the rows above the header.
//
// The campaign-id cell below carries the label text while the row stays otherwise VALID, so
// the only thing that can fail this test is the guard reading past the header. (A row that
// were also malformed would fail on the parse instead, proving nothing about scope.)
func TestFoldReportRows_IncompleteFlagIsScopedToThePreamble(t *testing.T) {
	records := [][]string{
		{"Report Name:", "Campaign Performance Report"},
		{"CampaignId", "Impressions", "Clicks", "Spend"},
		// Below the header, so NOT the header-block flag. Every metric cell is valid, so
		// this row must fold normally rather than refuse the report.
		{"Potential Incomplete Data: true", "10000", "250", "125.50"},
	}
	got, err := foldReportRows(records, "1234567", model.MetricsWindowLast7Days)
	if errors.Is(err, ErrReportDataIncomplete) {
		t.Fatal("a data row must not be read as the header-block incomplete-data flag")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Impressions != 10000 {
		t.Errorf("impressions = %d, want 10000", got.Impressions)
	}
}
