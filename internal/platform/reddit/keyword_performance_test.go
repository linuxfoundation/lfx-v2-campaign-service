// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

func healthyKeywords(campaignID, breakdown string) (int, string) {
	if breakdown != "KEYWORD" {
		return http.StatusBadRequest, `{"error":{"message":"unexpected breakdown"}}`
	}
	return http.StatusOK, audienceReport(
		audienceRow(campaignID, "keyword", `"service mesh"`, 900, 45, 4_500_000),
		audienceRow(campaignID, "keyword", `"kubernetes"`, 300, 3, 600_000),
		audienceRow(campaignID, "keyword", `null`, 300, 6, 300_000))
}

func TestGetKeywordPerformance_OneReportPerCampaignRowsPerCampaignKeyword(t *testing.T) {
	s := &audienceStub{answer: healthyKeywords}
	kp, err := newAudienceClient(t, s).GetKeywordPerformance(context.Background(), model.MetricsWindowLast30Days, []string{"t3_b", "t3_a", "t3_b"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if kp.Window != model.MetricsWindowLast30Days || kp.Truncated {
		t.Errorf("window/truncated = %q/%v", kp.Window, kp.Truncated)
	}
	want := []KeywordRow{
		{CampaignID: "t3_a", Text: "service mesh", Impressions: 900, Clicks: 45, CostMicros: 4_500_000, Ctr: 0.05},
		{CampaignID: "t3_b", Text: "service mesh", Impressions: 900, Clicks: 45, CostMicros: 4_500_000, Ctr: 0.05},
		{CampaignID: "t3_a", Text: "kubernetes", Impressions: 300, Clicks: 3, CostMicros: 600_000, Ctr: 0.01},
		{CampaignID: "t3_a", Text: "", Impressions: 300, Clicks: 6, CostMicros: 300_000, Ctr: 0.02},
		{CampaignID: "t3_b", Text: "kubernetes", Impressions: 300, Clicks: 3, CostMicros: 600_000, Ctr: 0.01},
		{CampaignID: "t3_b", Text: "", Impressions: 300, Clicks: 6, CostMicros: 300_000, Ctr: 0.02},
	}
	if len(kp.Rows) != len(want) {
		t.Fatalf("rows = %+v", kp.Rows)
	}
	for i := range want {
		if kp.Rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, kp.Rows[i], want[i])
		}
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want one per distinct campaign", len(reqs))
	}
	for _, r := range reqs {
		if strings.Join(r.Data.Breakdowns, ",") != "KEYWORD" || strings.Join(r.Data.Fields, ",") != "CAMPAIGN_ID,IMPRESSIONS,CLICKS,SPEND,KEYWORD" {
			t.Errorf("breakdowns/fields = %v / %v", r.Data.Breakdowns, r.Data.Fields)
		}
	}
	for _, p := range s.paths {
		if p != "POST "+monitorReportsPath {
			t.Errorf("request %q, want only the bound account's reports operation", p)
		}
	}
}

// More than MaxKeywordRows rows: the top ones by impressions, flagged truncated.
func TestGetKeywordPerformance_CapsAtFiftyAndFlagsTruncated(t *testing.T) {
	s := &audienceStub{answer: func(id, _ string) (int, string) {
		rows := make([]string, 0, 30)
		for i := 0; i < 30; i++ {
			rows = append(rows, audienceRow(id, "keyword", fmt.Sprintf(`"kw %02d"`, i), int64(1000-i), 1, 10))
		}
		return http.StatusOK, audienceReport(rows...)
	}}
	kp, err := newAudienceClient(t, s).GetKeywordPerformance(context.Background(), model.MetricsWindowToday, []string{"t3_a", "t3_b"})
	if err != nil {
		t.Fatalf("GetKeywordPerformance: %v", err)
	}
	if len(kp.Rows) != MaxKeywordRows || !kp.Truncated {
		t.Fatalf("rows = %d, truncated = %v; want %d and true", len(kp.Rows), kp.Truncated, MaxKeywordRows)
	}
	for i := 1; i < len(kp.Rows); i++ {
		if kp.Rows[i].Impressions > kp.Rows[i-1].Impressions {
			t.Fatalf("rows not ranked by impressions at %d", i)
		}
	}
	if kp.Rows[MaxKeywordRows-1].Impressions != 976 {
		t.Errorf("last kept row impressions = %d, want 976 (the 25th per campaign)", kp.Rows[MaxKeywordRows-1].Impressions)
	}
}

// The keyword read refuses what the shared read refuses, plus keyword text that is not text.
func TestGetKeywordPerformance_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(id string) string
	}{
		{"duplicate top-level data key", func(id string) string {
			return `{"data":{"metrics":[` + audienceRow(id, "keyword", `"k"`, 1, 0, 1) + `]},"data":{"metrics":[]},"pagination":{}}`
		}},
		{"explicit null counter", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":5,"clicks":null,"spend":0,"keyword":"k"}`)
		}},
		{"row for another campaign", func(string) string { return audienceReport(audienceRow("t3_CANARY", "keyword", `"k"`, 1, 0, 1)) }},
		{"keyword key missing", func(id string) string {
			return audienceReport(`{"campaign_id":"` + id + `","impressions":5,"clicks":0,"spend":0}`)
		}},
		{"markup in keyword", func(id string) string { return audienceReport(audienceRow(id, "keyword", `"<script>CANARY"`, 1, 0, 1)) }},
		{"control character in keyword", func(id string) string { return audienceReport(audienceRow(id, "keyword", `"CANARY\u0007"`, 1, 0, 1)) }},
		{"blank keyword", func(id string) string { return audienceReport(audienceRow(id, "keyword", `"   "`, 1, 0, 1)) }},
		{"overlong keyword", func(id string) string {
			return audienceReport(audienceRow(id, "keyword", `"`+strings.Repeat("a", maxKeywordTextLen+1)+`"`, 1, 0, 1))
		}},
		{"duplicate keyword row", func(id string) string {
			r := audienceRow(id, "keyword", `"k"`, 1, 0, 1)
			return audienceReport(r, r)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{answer: func(id, _ string) (int, string) { return http.StatusOK, tc.body(id) }}
			kp, err := newAudienceClient(t, s).GetKeywordPerformance(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
			if err == nil || kp != nil {
				t.Fatalf("got (%+v, %v), want a refusal", kp, err)
			}
			var te *transportError
			if !errors.As(err, &te) {
				t.Errorf("error = %T (%v), want a transportError (→ 503)", err, err)
			}
			if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "t2_test") {
				t.Errorf("error echoes upstream content or the account id: %v", err)
			}
		})
	}
}

// Non-ASCII and punctuated keywords are text, returned verbatim.
func TestGetKeywordPerformance_KeepsPhrasesVerbatim(t *testing.T) {
	for _, kw := range []string{"service mesh", "c++", "kubernetes-native", "o'reilly", "café"} {
		if err := keywordTextErr(kw); err != nil {
			t.Errorf("%q refused: %v", kw, err)
		}
	}
}

func TestGetKeywordPerformance_RefusedBeforeAnyRequest(t *testing.T) {
	tooMany := make([]string, MaxReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = "t3_" + strings.Repeat("k", i+1)
	}
	for _, tc := range []struct {
		name   string
		window model.MetricsWindow
		scope  []string
		want   error
	}{
		{"empty scope", model.MetricsWindowLast30Days, nil, ErrReportScopeInvalid},
		{"invalid id", model.MetricsWindowLast30Days, []string{"t3_a,campaign:id==t3_x"}, ErrReportScopeInvalid},
		{"too many", model.MetricsWindowLast30Days, tooMany, ErrReportScopeTooLarge},
		{"unmapped window", model.MetricsWindowLast14Days, []string{"t3_a"}, ErrUnsupportedWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &audienceStub{answer: healthyKeywords}
			kp, err := newAudienceClient(t, s).GetKeywordPerformance(context.Background(), tc.window, tc.scope)
			if !errors.Is(err, tc.want) || kp != nil {
				t.Errorf("got (%+v, %v), want %v", kp, err, tc.want)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// An absent breakdown key is reported as such — the report was not grouped by the breakdown — not
// as some downstream symptom (a "not a string" decode of nothing), so a responder checks the
// request shape rather than Reddit's values.
func TestGetKeywordPerformance_AbsentKeywordKeyNamesTheCause(t *testing.T) {
	s := &audienceStub{answer: func(id, _ string) (int, string) {
		return http.StatusOK, audienceReport(`{"campaign_id":"` + id + `","impressions":5,"clicks":0,"spend":0}`)
	}}
	_, err := newAudienceClient(t, s).GetKeywordPerformance(context.Background(), model.MetricsWindowLast30Days, []string{"t3_a"})
	if err == nil || !strings.Contains(err.Error(), `breakdown "keyword" is missing from the row`) {
		t.Fatalf("error = %v, want the missing-breakdown cause", err)
	}
}
