// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The keyword report reuses the account monitor's fake Microsoft (newMonitorServer) and its
// fixed non-UTC clock (monitorClock: 2026-10-05 01:30 UTC+5 = 2026-10-04 20:30 UTC).

func TestSubmitKeywordReport_BodyScopeAndWindow(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)
	id, start, end, err := c.SubmitKeywordReport(context.Background(), model.MetricsWindowLast7Days, []string{"111", "222"})
	if err != nil {
		t.Fatalf("SubmitKeywordReport: %v", err)
	}
	if id != "rr-acct-1" {
		t.Errorf("report id = %q", id)
	}
	// UTC dates, not the clock's own zone: the window ends on the 4th.
	if got := start.Format("2006-01-02") + ".." + end.Format("2006-01-02"); got != "2026-09-28..2026-10-04" {
		t.Errorf("window = %s, want 2026-09-28..2026-10-04", got)
	}

	m.mu.Lock()
	raw := m.submitRaw
	m.mu.Unlock()
	var body struct {
		ReportRequest struct {
			Type                   string
			Aggregation            string
			ReturnOnlyCompleteData *bool
			Columns                []string
			Scope                  map[string]json.RawMessage
			Time                   struct {
				CustomDateRangeStart struct{ Day, Month, Year int }
				CustomDateRangeEnd   struct{ Day, Month, Year int }
				ReportTimeZone       string
			}
			MaxRows *int
			Filter  json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode submit body: %v\n%s", err, raw)
	}
	rr := body.ReportRequest
	if rr.Type != "KeywordPerformanceReportRequest" || rr.Aggregation != "Summary" {
		t.Errorf("Type/Aggregation = %q/%q", rr.Type, rr.Aggregation)
	}
	if rr.ReturnOnlyCompleteData == nil || *rr.ReturnOnlyCompleteData {
		t.Errorf("ReturnOnlyCompleteData must be sent as false")
	}
	if rr.MaxRows != nil || rr.Filter != nil {
		t.Errorf("MaxRows/Filter must not be sent: they would truncate before the fold")
	}
	want := map[string]bool{}
	for _, c := range rr.Columns {
		want[c] = true
	}
	for _, c := range []string{"CampaignId", "AdGroupId", "KeywordId", "Keyword", "BidMatchType", "KeywordStatus", "QualityScore", "Impressions", "Clicks", "Spend", "ConversionsQualified"} {
		if !want[c] {
			t.Errorf("column %s not requested: %v", c, rr.Columns)
		}
	}
	for _, banned := range []string{"Conversions", "TimePeriod", "DeliveredMatchType"} {
		if want[banned] {
			t.Errorf("column %s must not be requested", banned)
		}
	}
	// The scope is the UNION of its elements: AccountIds or AdGroups here would widen the read.
	if len(rr.Scope) != 1 || rr.Scope["Campaigns"] == nil {
		t.Fatalf("Scope must carry ONLY Campaigns, got keys %v", keysOf(rr.Scope))
	}
	if got := string(rr.Scope["Campaigns"]); got != `[{"AccountId":"9999999","CampaignId":"111"},{"AccountId":"9999999","CampaignId":"222"}]` {
		t.Errorf("Campaigns = %s (ids must be quoted strings)", got)
	}
	if rr.Time.CustomDateRangeEnd.Day != 4 || rr.Time.CustomDateRangeStart.Day != 28 || rr.Time.ReportTimeZone != "GreenwichMeanTimeDublinEdinburghLisbonLondon" {
		t.Errorf("Time = %+v", rr.Time)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSubmitKeywordReport_RefusesBadScopeWithoutCallingMicrosoft(t *testing.T) {
	tooMany := make([]string, MaxKeywordReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(i + 1)
	}
	for name, ids := range map[string][]string{
		"empty":       nil,
		"too many":    tooMany,
		"non-numeric": {"111", "abc"},
		"leading 0":   {"0111"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMonitorServer(t)
			_, _, _, err := newMonitorClient(t, m).SubmitKeywordReport(context.Background(), model.MetricsWindowLast30Days, ids)
			if !errors.Is(err, ErrKeywordReportScope) {
				t.Fatalf("err = %v, want ErrKeywordReportScope", err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.submitRaw != nil {
				t.Errorf("a refused scope must not reach Microsoft")
			}
		})
	}
}

func TestSubmitKeywordReport_UnsupportedWindow(t *testing.T) {
	m := newMonitorServer(t)
	_, _, _, err := newMonitorClient(t, m).SubmitKeywordReport(context.Background(), model.MetricsWindowYesterday, []string{"111"})
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("err = %v, want ErrUnsupportedWindow", err)
	}
	if ValidateKeywordReportWindow(model.MetricsWindowLast14Days) == nil || ValidateKeywordReportWindow(model.MetricsWindowLast30Days) != nil {
		t.Errorf("ValidateKeywordReportWindow disagrees with reportDateRange")
	}
}

func checkKeyword(t *testing.T, m *monitorServer) (*KeywordReportResult, error) {
	t.Helper()
	res, err := newMonitorClient(t, m).CheckKeywordReport(context.Background(), "rr-acct-1")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 1 {
		t.Errorf("Check issued %d Polls, want exactly 1", m.pollCount)
	}
	return res, err
}

func TestCheckKeywordReport_PendingErrorAndUnknown(t *testing.T) {
	for reply, want := range map[string]AccountReportStatus{
		`{"ReportRequestStatus":{"Status":"Pending"}}`: AccountReportStatusPending,
		`{"ReportRequestStatus":{"Status":"Error"}}`:   AccountReportStatusError,
	} {
		m := newMonitorServer(t)
		m.pollReply = reply
		res, err := checkKeyword(t, m)
		if err != nil || res.Status != want || res.Rows != nil {
			t.Errorf("%s: got %+v, %v; want %s with no rows", reply, res, err, want)
		}
	}
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Building"}}`
	if _, err := checkKeyword(t, m); err == nil {
		t.Errorf("an unrecognized status must be an error, not pending")
	}
}

func TestCheckKeywordReport_SuccessWithoutURLIsEmpty(t *testing.T) {
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success"}}`
	res, err := checkKeyword(t, m)
	if err != nil || res.Status != AccountReportStatusSuccess || res.Rows == nil || len(res.Rows) != 0 {
		t.Errorf("got %+v, %v; want Success with non-nil empty rows", res, err)
	}
}

func TestCheckKeywordReport_FoldsPerKeyword(t *testing.T) {
	const csvBody = `"Report Name:","Keyword Performance Report"
"Potential Incomplete Data: true"

"CampaignId","CampaignName","AdGroupId","AdGroupName","KeywordId","Keyword","BidMatchType","KeywordStatus","QualityScore","Impressions","Clicks","Spend","ConversionsQualified"
"111","KubeCon","501","Exact","9001","kubernetes training","Exact","Active","7","1000","50","25.50","2.5"
"111","KubeCon","501","Exact","9001","kubernetes training","Exact","Active","7","500","10","4.50","1"
"111","KubeCon","502","Broad","9002","cloud native","Broad","Paused","--","300","9","4.00",""
"111","KubeCon","502","Broad","9003","old term","Phrase","Deleted","5","999","99","99.00","9"
"@2026 Microsoft Corporation. All rights reserved. "
`
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zipPayload = buildReportZip(t, csvBody)
	res, err := checkKeyword(t, m)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Partial {
		t.Errorf("Partial must reflect the preamble flag")
	}
	if len(res.Rows) != 2 {
		t.Fatalf("got %d rows, want 2 (9001 summed, 9002; 9003 deleted): %+v", len(res.Rows), res.Rows)
	}
	a, b := res.Rows[0], res.Rows[1]
	if a.AdGroupID != "501" || a.KeywordID != "9001" || a.CampaignID != "111" || a.Impressions != 1500 || a.Clicks != 60 || a.Spend != 30 {
		t.Errorf("9001 = %+v", a)
	}
	if a.MatchType != "Exact" || a.Status != "Active" || a.Keyword != "kubernetes training" || a.CampaignName != "KubeCon" || a.AdGroupName != "Exact" {
		t.Errorf("9001 attributes = %+v", a)
	}
	if a.QualityScore == nil || *a.QualityScore != 7 || a.Conversions == nil || *a.Conversions != 3.5 {
		t.Errorf("9001 score/conversions = %v/%v", a.QualityScore, a.Conversions)
	}
	if b.QualityScore != nil {
		t.Errorf(`"--" quality score must be nil, got %d`, *b.QualityScore)
	}
	if b.Conversions != nil {
		t.Errorf("a blank conversion cell must be unknown (nil), got %v", *b.Conversions)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloadAuth != "" {
		t.Errorf("the pre-signed download must not carry our bearer token")
	}
}

func TestFoldKeywordReportRows_RefusesUnattributableOrIncomplete(t *testing.T) {
	const header = `"CampaignId","AdGroupId","KeywordId","Keyword","Impressions","Clicks","Spend"`
	for name, body := range map[string]string{
		"missing KeywordId column": `"CampaignId","AdGroupId","Keyword","Impressions","Clicks","Spend"` + "\n" + `"1","2","k","1","1","1"`,
		"blank keyword id":         header + "\n" + `"1","2","","k","1","1","1"`,
		"negative spend":           header + "\n" + `"1","2","3","k","1","1","-1"`,
		"two campaigns":            header + "\n" + `"1","2","3","k","1","1","1"` + "\n" + `"9","2","3","k","1","1","1"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := foldKeywordReportRows(csvRecords(t, body)); err == nil {
				t.Errorf("want an error")
			}
		})
	}
}

func csvRecords(t *testing.T, body string) [][]string {
	t.Helper()
	recs, err := readReportZipRecords(buildReportZip(t, body))
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return recs
}

func TestParseQualityScore(t *testing.T) {
	for in, want := range map[string]string{"7": "7", "10": "10", "1": "1", "--": "nil", "": "nil", "0": "nil", "11": "nil", "x": "nil"} {
		got := "nil"
		if v := parseQualityScore(in); v != nil {
			got = fmt.Sprint(*v)
		}
		if got != want {
			t.Errorf("parseQualityScore(%q) = %s, want %s", in, got, want)
		}
	}
}
