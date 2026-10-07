// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The age/gender report reuses the account monitor's fake Microsoft (newMonitorServer, whose
// handlers hand captured bodies over under its mutex) and its pinned non-UTC clock
// (monitorClock: 2026-10-05 01:30 UTC+5 = 2026-10-04 20:30 UTC).

func TestSubmitAgeGenderReport_BodyScopeAndWindow(t *testing.T) {
	m := newMonitorServer(t)
	c := newMonitorClient(t, m)
	id, start, end, err := c.SubmitAgeGenderReport(context.Background(), model.MetricsWindowLast7Days, []string{"111", "222"})
	if err != nil {
		t.Fatalf("SubmitAgeGenderReport: %v", err)
	}
	if id != "rr-acct-1" {
		t.Errorf("report id = %q", id)
	}
	if got := start.Format("2006-01-02") + ".." + end.Format("2006-01-02"); got != "2026-09-28..2026-10-04" {
		t.Errorf("window = %s, want 2026-09-28..2026-10-04 (UTC dates)", got)
	}

	m.mu.Lock()
	raw := m.submitRaw
	m.mu.Unlock()
	var body struct {
		ReportRequest struct {
			Type                   string
			Format                 string
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
	if rr.Type != "AgeGenderAudienceReportRequest" || rr.Aggregation != "Summary" || rr.Format != "Csv" {
		t.Errorf("Type/Aggregation/Format = %q/%q/%q", rr.Type, rr.Aggregation, rr.Format)
	}
	if rr.ReturnOnlyCompleteData == nil || *rr.ReturnOnlyCompleteData {
		t.Errorf("ReturnOnlyCompleteData must be sent as false")
	}
	if rr.MaxRows != nil || rr.Filter != nil {
		t.Errorf("MaxRows/Filter must not be sent")
	}
	if got := strings.Join(rr.Columns, ","); got != "CampaignId,AgeGroup,Gender,Impressions,Clicks,Spend" {
		t.Errorf("Columns = %s", got)
	}
	// TimePeriod is not allowed with Summary; there is no device column to ask for.
	for _, c := range rr.Columns {
		if c == "TimePeriod" || strings.Contains(strings.ToLower(c), "device") {
			t.Errorf("column %s must not be requested", c)
		}
	}
	if len(rr.Scope) != 1 || rr.Scope["Campaigns"] == nil {
		t.Fatalf("Scope must carry ONLY Campaigns, got keys %v", keysOf(rr.Scope))
	}
	if got := string(rr.Scope["Campaigns"]); got != `[{"AccountId":"9999999","CampaignId":"111"},{"AccountId":"9999999","CampaignId":"222"}]` {
		t.Errorf("Campaigns = %s", got)
	}
	if rr.Time.CustomDateRangeEnd.Day != 4 || rr.Time.CustomDateRangeStart.Day != 28 || rr.Time.ReportTimeZone != "GreenwichMeanTimeDublinEdinburghLisbonLondon" {
		t.Errorf("Time = %+v", rr.Time)
	}
}

func TestSubmitAgeGenderReport_RefusesBadScopeWithoutCallingMicrosoft(t *testing.T) {
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
			_, _, _, err := newMonitorClient(t, m).SubmitAgeGenderReport(context.Background(), model.MetricsWindowLast30Days, ids)
			if !errors.Is(err, ErrAudienceReportScope) || errors.Is(err, ErrKeywordReportScope) {
				t.Fatalf("err = %v, want ErrAudienceReportScope only", err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.submitRaw != nil {
				t.Errorf("a refused scope must not reach Microsoft")
			}
		})
	}
	if ValidateAudienceReportCampaignID("123") != nil || ValidateAudienceReportCampaignID("x") == nil {
		t.Errorf("ValidateAudienceReportCampaignID disagrees with the scope rule")
	}
}

func TestSubmitAgeGenderReport_UnsupportedWindow(t *testing.T) {
	m := newMonitorServer(t)
	_, _, _, err := newMonitorClient(t, m).SubmitAgeGenderReport(context.Background(), model.MetricsWindowYesterday, []string{"111"})
	if !errors.Is(err, ErrUnsupportedWindow) {
		t.Fatalf("err = %v, want ErrUnsupportedWindow", err)
	}
}

func TestSubmitAgeGenderReport_2027IsTheRejectedSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.HandleFunc("/Reporting/v13/GenerateReport/Submit", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"Code":2027,"Message":"scope rejected"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(
		Credentials{ClientID: "cid", ClientSecret: "sec", DeveloperToken: "dev", RefreshToken: "ref"},
		AccountConfig{AccountID: "9999999", Label: "LF Events"},
		WithReportingBaseURL(srv.URL), WithTokenURL(srv.URL+"/token"), WithClock(monitorClock),
	)
	_, _, _, err := c.SubmitAgeGenderReport(context.Background(), model.MetricsWindowLast7Days, []string{"111"})
	if !errors.Is(err, ErrAudienceReportScopeRejected) || errors.Is(err, ErrKeywordReportScopeRejected) {
		t.Fatalf("err = %v, want ErrAudienceReportScopeRejected only", err)
	}
	if !strings.Contains(err.Error(), "age/gender") || !strings.Contains(err.Error(), "AccountIds") {
		t.Errorf("error must name the report and the not-widened scope: %v", err)
	}
}

func checkAgeGender(t *testing.T, m *monitorServer) (*AgeGenderReportResult, error) {
	t.Helper()
	res, err := newMonitorClient(t, m).CheckAgeGenderReport(context.Background(), "rr-acct-1")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pollCount != 1 {
		t.Errorf("Check issued %d Polls, want exactly 1", m.pollCount)
	}
	return res, err
}

func TestCheckAgeGenderReport_PendingErrorUnknownAndEmpty(t *testing.T) {
	for reply, want := range map[string]AccountReportStatus{
		`{"ReportRequestStatus":{"Status":"Pending"}}`: AccountReportStatusPending,
		`{"ReportRequestStatus":{"Status":"Error"}}`:   AccountReportStatusError,
	} {
		m := newMonitorServer(t)
		m.pollReply = reply
		res, err := checkAgeGender(t, m)
		if err != nil || res.Status != want || res.Rows != nil {
			t.Errorf("%s: got %+v, %v; want %s with no rows", reply, res, err, want)
		}
	}
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Building"}}`
	if _, err := checkAgeGender(t, m); err == nil {
		t.Errorf("an unrecognized status must be an error, not pending")
	}
	m = newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success"}}`
	res, err := checkAgeGender(t, m)
	if err != nil || res.Status != AccountReportStatusSuccess || res.Rows == nil || len(res.Rows) != 0 {
		t.Errorf("got %+v, %v; want Success with non-nil empty rows", res, err)
	}
}

func TestCheckAgeGenderReport_FoldsPerCampaignAgeAndGender(t *testing.T) {
	const csvBody = `"Report Name:","Age Gender Audience Report"
"Potential Incomplete Data: true"

"CampaignId","AgeGroup","Gender","Impressions","Clicks","Spend"
"111","25-34","Female","1000","50","25.50"
"111","25-34","Female","500","10","4.50"
"111","25-34","Male","300","9","4.00"
"222","25-34","Female","200","2","1.00"
"222","65+","Unknown","0","0","0"
"@2026 Microsoft Corporation. All rights reserved. "
`
	m := newMonitorServer(t)
	m.pollReply = `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"%URL%"}}`
	m.zipPayload = buildReportZip(t, csvBody)
	res, err := checkAgeGender(t, m)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Partial {
		t.Errorf("Partial must reflect the preamble flag")
	}
	want := []AgeGenderReportRow{
		{CampaignID: "111", AgeGroup: "25-34", Gender: "Female", Impressions: 1500, Clicks: 60, Spend: 30},
		{CampaignID: "111", AgeGroup: "25-34", Gender: "Male", Impressions: 300, Clicks: 9, Spend: 4},
		{CampaignID: "222", AgeGroup: "25-34", Gender: "Female", Impressions: 200, Clicks: 2, Spend: 1},
		{CampaignID: "222", AgeGroup: "65+", Gender: "Unknown"},
	}
	if len(res.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", res.Rows, want)
	}
	for i := range want {
		if res.Rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, res.Rows[i], want[i])
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downloadAuth != "" {
		t.Errorf("the pre-signed download must not carry our bearer token")
	}
}

func TestFoldAgeGenderReportRows_RefusesMalformed(t *testing.T) {
	const header = `"CampaignId","AgeGroup","Gender","Impressions","Clicks","Spend"`
	tooManyPairs := header
	for i := 0; i <= MaxAgeGenderBuckets; i++ {
		tooManyPairs += "\n" + fmt.Sprintf(`"1","age-%d","g","1","1","1"`, i)
	}
	for name, body := range map[string]string{
		"missing Gender column":   `"CampaignId","AgeGroup","Impressions","Clicks","Spend"` + "\n" + `"1","25-34","1","1","1"`,
		"missing Spend column":    `"CampaignId","AgeGroup","Gender","Impressions","Clicks"` + "\n" + `"1","25-34","Male","1","1"`,
		"repeated column":         `"CampaignId","AgeGroup","Gender","Gender","Impressions","Clicks","Spend"` + "\n" + `"1","25-34","Male","Female","1","1","1"`,
		"blank campaign id":       header + "\n" + `"","25-34","Male","1","1","1"`,
		"non-id campaign":         header + "\n" + `"abc","25-34","Male","1","1","1"`,
		"blank age group":         header + "\n" + `"1","","Male","1","1","1"`,
		"blank gender":            header + "\n" + `"1","25-34"," ","1","1","1"`,
		"invalid utf-8 gender":    header + "\n" + "\"1\",\"25-34\",\"Ma\xffle\",\"1\",\"1\",\"1\"",
		"control char in age":     header + "\n" + "\"1\",\"25\x0134\",\"Male\",\"1\",\"1\",\"1\"",
		"over-long label":         header + "\n" + `"1","` + strings.Repeat("a", maxAudienceLabelBytes+1) + `","Male","1","1","1"`,
		"negative impressions":    header + "\n" + `"1","25-34","Male","-1","1","1"`,
		"negative spend":          header + "\n" + `"1","25-34","Male","1","1","-0.5"`,
		"non-numeric clicks":      header + "\n" + `"1","25-34","Male","1","x","1"`,
		"short row":               header + "\n" + `"1","25-34"`,
		"too many distinct pairs": tooManyPairs,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := foldAgeGenderReportRows(csvRecords(t, body)); err == nil {
				t.Errorf("want an error")
			}
		})
	}
}

func TestFoldAgeGenderReportRows_HeaderOnlyIsEmptySuccess(t *testing.T) {
	res, err := foldAgeGenderReportRows(csvRecords(t, `"CampaignId","AgeGroup","Gender","Impressions","Clicks","Spend"`))
	if err != nil || res.Rows == nil || len(res.Rows) != 0 || res.Partial {
		t.Errorf("got %+v, %v; want an empty, complete Success", res, err)
	}
}
