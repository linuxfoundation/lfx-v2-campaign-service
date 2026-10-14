// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// unmeasuredLogMessage is the one summary line reportUnmeasuredRows writes per request.
const unmeasuredLogMessage = "account monitor: some campaigns could not be measured; totals may understate the account"

// captureMonitorLogs routes the default logger to a JSON buffer for the test and returns the
// records carrying unmeasuredLogMessage.
func captureMonitorLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("log line is not JSON: %q: %v", line, err)
			}
			if rec["msg"] == unmeasuredLogMessage {
				out = append(out, rec)
			}
		}
		return out
	}
}

func sortedUnmeasured(recs []unmeasuredRecord) []unmeasuredRecord {
	sort.Slice(recs, func(i, j int) bool { return recs[i].class < recs[j].class })
	return recs
}

// monitorTotals counts every FetchFailed row and nothing else: a PacingUnknown row whose
// delivery was read is not unmeasured.
func TestMonitorTotals_CountsFetchFailedRowsOnly(t *testing.T) {
	rows := []model.AccountCampaignMetrics{
		{Spend: 10, Impressions: 100, Clicks: 3},
		{Spend: 5, PacingUnknown: true},
		{FetchFailed: true, PacingUnknown: true, FailureClass: model.MonitorFailureThrottle},
		// A budget-only failure: metrics read and summed, still counted (an upper bound).
		{Spend: 7, Impressions: 70, FetchFailed: true, FailureClass: model.MonitorFailureDecode},
	}
	got := monitorTotals(rows)
	if got.UnmeasuredCampaigns != 2 {
		t.Errorf("UnmeasuredCampaigns = %d, want 2 (the two FetchFailed rows; not the PacingUnknown one)", got.UnmeasuredCampaigns)
	}
	if got.CampaignCount != 4 || got.Spend != 22 {
		t.Errorf("totals = %+v, want campaign_count 4 and spend 22 (every row still summed)", got)
	}
	if conv := toConnAccountMonitorTotals(got); conv.UnmeasuredCampaigns != 2 {
		t.Errorf("response unmeasured_campaigns = %d, want 2", conv.UnmeasuredCampaigns)
	}
	if got := monitorTotals(nil); got.UnmeasuredCampaigns != 0 {
		t.Errorf("empty account UnmeasuredCampaigns = %d, want 0", got.UnmeasuredCampaigns)
	}
}

func redditMonitorService(rows []model.AccountCampaignMetrics, rec *recordingMetrics) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: &mockAccountMetricsReaderDispatcher{rows: rows},
	}}
	o.SetMetrics(rec)
	svc.SetOrchestrator(o)
	return svc
}

// One monitor request with failed rows of several classes: the response counts them, ONE Warn
// carries per-class counts with no upstream text, and the metric is one Add per class present.
func TestMonitorAccount_UnmeasuredRowsLoggedAndCountedOncePerRequest(t *testing.T) {
	logs := captureMonitorLogs(t)
	rec := &recordingMetrics{}
	svc := redditMonitorService([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "ok", Name: "ok", Status: "ACTIVE", Spend: 40, Impressions: 400, Clicks: 8},
		{PlatformCampaignID: "a1", Name: "a1", Status: "ACTIVE", FetchFailed: true, FailureClass: model.MonitorFailureAuth},
		{PlatformCampaignID: "a2", Name: "a2", Status: "ACTIVE", FetchFailed: true, FailureClass: model.MonitorFailureAuth},
		{PlatformCampaignID: "t1", Name: "t1", Status: "ACTIVE", FetchFailed: true, FailureClass: model.MonitorFailureThrottle},
		{PlatformCampaignID: "d1", Name: "d1", Status: "PAUSED", FetchFailed: true, FailureClass: model.MonitorFailureDecode},
		// Unclassified (the Google case) and an out-of-set value both become other.
		{PlatformCampaignID: "u1", Name: "u1", Status: "ACTIVE", FetchFailed: true},
		{PlatformCampaignID: "u2", Name: "u2", Status: "ACTIVE", FetchFailed: true, FailureClass: "token abc123 revoked"},
	}, rec)

	got, err := svc.MonitorRedditAdsAccount(context.Background(),
		&conn.MonitorRedditAdsAccountPayload{ProjectID: "proj-1", AccountID: "t2_acct", Days: 30})
	if err != nil {
		t.Fatalf("MonitorRedditAdsAccount: %v", err)
	}
	if got.Totals.UnmeasuredCampaigns != 6 || got.Totals.CampaignCount != 7 {
		t.Errorf("totals = %+v, want unmeasured_campaigns 6 of campaign_count 7", got.Totals)
	}

	records := logs()
	if len(records) != 1 {
		t.Fatalf("got %d summary log records, want exactly 1: %v", len(records), records)
	}
	r := records[0]
	if r["level"] != "WARN" || r["project_id"] != "proj-1" || r["platform"] != string(model.ProviderRedditAds) {
		t.Errorf("log record = %v, want WARN with project_id and platform", r)
	}
	if r["unmeasured_campaigns"] != float64(6) || r["campaign_count"] != float64(7) {
		t.Errorf("log counts = %v/%v, want 6/7", r["unmeasured_campaigns"], r["campaign_count"])
	}
	wantByClass := map[string]float64{"auth": 2, "throttle": 1, "upstream_5xx": 0, "decode": 1, "no_report": 0, "other": 2}
	byClass, _ := r["unmeasured_by_class"].(map[string]any)
	if len(byClass) != len(wantByClass) {
		t.Errorf("unmeasured_by_class = %v, want exactly the %d classes", byClass, len(wantByClass))
	}
	for c, n := range wantByClass {
		if byClass[c] != n {
			t.Errorf("unmeasured_by_class[%s] = %v, want %v", c, byClass[c], n)
		}
	}
	line, _ := json.Marshal(r)
	for _, leak := range []string{"abc123", "t2_acct"} {
		if strings.Contains(string(line), leak) {
			t.Errorf("summary log carries %q: %s", leak, line)
		}
	}

	want := []unmeasuredRecord{
		{platform: model.ProviderRedditAds, class: "auth", n: 2},
		{platform: model.ProviderRedditAds, class: "decode", n: 1},
		{platform: model.ProviderRedditAds, class: "other", n: 2},
		{platform: model.ProviderRedditAds, class: "throttle", n: 1},
	}
	gotRecs := sortedUnmeasured(rec.unmeasuredRecords())
	if len(gotRecs) != len(want) {
		t.Fatalf("metric records = %+v, want %+v", gotRecs, want)
	}
	for i := range want {
		if gotRecs[i] != want[i] {
			t.Errorf("metric record %d = %+v, want %+v", i, gotRecs[i], want[i])
		}
	}
}

// A fully measured account says unmeasured_campaigns 0 and emits neither the log nor the metric.
func TestMonitorAccount_FullyMeasuredAccountIsSilent(t *testing.T) {
	logs := captureMonitorLogs(t)
	rec := &recordingMetrics{}
	svc := redditMonitorService([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "a", Status: "ACTIVE", Spend: 40, Impressions: 400, Clicks: 8},
		{PlatformCampaignID: "2", Name: "b", Status: "ACTIVE", PacingUnknown: true},
	}, rec)
	got, err := svc.MonitorRedditAdsAccount(context.Background(),
		&conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: "t2_a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorRedditAdsAccount: %v", err)
	}
	if got.Totals.UnmeasuredCampaigns != 0 {
		t.Errorf("unmeasured_campaigns = %d, want 0", got.Totals.UnmeasuredCampaigns)
	}
	if n := len(logs()); n != 0 {
		t.Errorf("got %d summary log records, want none", n)
	}
	if recs := rec.unmeasuredRecords(); len(recs) != 0 {
		t.Errorf("metric records = %+v, want none", recs)
	}
}

// Google's client sets no class: its failed rows still count in the totals and as other.
func TestMonitorGoogleAdsAccount_UnclassifiedRowsCountAsOther(t *testing.T) {
	logs := captureMonitorLogs(t)
	rec := &recordingMetrics{}
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{rows: []model.AccountCampaignMetrics{
			{PlatformCampaignID: "1", Name: "c", Status: "ENABLED", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5},
			{PlatformCampaignID: "2", Name: "d", Status: "ENABLED", FetchFailed: true},
		}},
	}}
	o.SetMetrics(rec)
	svc.SetOrchestrator(o)
	got, err := svc.MonitorGoogleAdsAccount(context.Background(),
		&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "1234567890", Days: 30})
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount: %v", err)
	}
	if got.Totals.UnmeasuredCampaigns != 1 {
		t.Errorf("unmeasured_campaigns = %d, want 1", got.Totals.UnmeasuredCampaigns)
	}
	if n := len(logs()); n != 1 {
		t.Errorf("got %d summary log records, want 1", n)
	}
	recs := rec.unmeasuredRecords()
	if len(recs) != 1 || recs[0] != (unmeasuredRecord{platform: model.ProviderGoogleAds, class: "other", n: 1}) {
		t.Errorf("metric records = %+v, want one other=1 for google-ads", recs)
	}
}

// A report-backed first read: every row is no_report, counted in totals and the metric, but not
// warned about — it is the expected state before any report finishes.
func TestMonitorMicrosoftAdsAccount_NoReportRowsAreCountedNotWarned(t *testing.T) {
	logs := captureMonitorLogs(t)
	rec := &recordingMetrics{}
	o := reportOrch(&fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}, &fakeReportStore{})
	o.SetMetrics(rec)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(o)
	got, err := svc.MonitorMicrosoftAdsAccount(context.Background(),
		&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
	if err != nil {
		t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
	}
	if got.Totals.UnmeasuredCampaigns != 2 {
		t.Errorf("unmeasured_campaigns = %d, want 2", got.Totals.UnmeasuredCampaigns)
	}
	if n := len(logs()); n != 0 {
		t.Errorf("got %d summary log records, want none for a first read", n)
	}
	recs := rec.unmeasuredRecords()
	if len(recs) != 1 || recs[0] != (unmeasuredRecord{platform: model.ProviderMicrosoftAds, class: "no_report", n: 2}) {
		t.Errorf("metric records = %+v, want one no_report=2", recs)
	}
}

// A report-backed read with a finished report and one unreadable budget: the dispatcher's
// decode class survives the merge and is warned about.
func TestMonitorMicrosoftAdsAccount_DispatcherDecodeClassIsWarned(t *testing.T) {
	logs := captureMonitorLogs(t)
	rec := &recordingMetrics{}
	campaigns := twoCampaigns()
	campaigns[1].FetchFailed = true
	campaigns[1].FailureClass = model.MonitorFailureDecode
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		AsOf: time.Now(),
		Rows: []model.AccountReportRow{
			{PlatformCampaignID: "1", Spend: 70, Impressions: 1000, Clicks: 30},
			{PlatformCampaignID: "2", Spend: 10, Impressions: 500, Clicks: 5},
		},
	}}}
	o := reportOrch(&fakeReportReader{campaigns: campaigns, submitID: "r2"}, store)
	o.SetMetrics(rec)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(o)
	got, err := svc.MonitorMicrosoftAdsAccount(context.Background(),
		&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
	if err != nil {
		t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
	}
	if got.Totals.UnmeasuredCampaigns != 1 || got.Totals.Spend != 80 {
		t.Errorf("totals = %+v, want unmeasured 1 with the budget-only row's spend still summed (80)", got.Totals)
	}
	records := logs()
	if len(records) != 1 {
		t.Fatalf("got %d summary log records, want 1", len(records))
	}
	if byClass, _ := records[0]["unmeasured_by_class"].(map[string]any); byClass["decode"] != float64(1) {
		t.Errorf("unmeasured_by_class = %v, want decode=1", records[0]["unmeasured_by_class"])
	}
	recs := rec.unmeasuredRecords()
	if len(recs) != 1 || recs[0] != (unmeasuredRecord{platform: model.ProviderMicrosoftAds, class: "decode", n: 1}) {
		t.Errorf("metric records = %+v, want one decode=1", recs)
	}
}

// Every published ad-monitor totals example is a response the service could produce:
// unmeasured_campaigns is present, non-negative, and no more than campaign_count — the rows it
// counts are a subset of the rows campaign_count counts. Where the totals sit beside the campaigns
// array they sum (every account-monitor response example), campaign_count must also equal that
// array's length and unmeasured_campaigns the number of its rows with fetch_failed=true: bounds
// alone let the examples publish counts that no row beside them backs. Whether each example is a
// response its own OPERATION could return is TestPublishedAccountMonitorExamplesMatchTheirOperation.
func TestPublishedAccountMonitorTotalsExamplesArePossible(t *testing.T) {
	for _, spec := range hubspotMonitorSpecs {
		t.Run(spec, func(t *testing.T) {
			seen, enclosed := 0, 0
			for path, ex := range hubspotMonitorSpecExamples(t, spec) {
				hubspotMonitorVisit(ex, func(v any) {
					n, ok := v.(map[string]any)
					if !ok {
						return
					}
					if totals, ok := n["totals"].(map[string]any); ok {
						if rows, ok := n["campaigns"].([]any); ok {
							enclosed++
							checkUnmeasuredAgainstCampaigns(t, path, totals, rows)
						}
					}
					cc, hasCC := n["campaign_count"].(float64)
					if !hasCC {
						return
					}
					seen++
					u, hasU := n["unmeasured_campaigns"].(float64)
					if !hasU {
						t.Errorf("%s: totals example without unmeasured_campaigns: %v", path, n)
						return
					}
					if u < 0 || u > cc || u != float64(int(u)) {
						t.Errorf("%s: unmeasured_campaigns %v beside campaign_count %v", path, u, cc)
					}
				})
			}
			if seen == 0 {
				t.Fatalf("no account-monitor totals example found in %s; the scan is vacuous", spec)
			}
			// OpenAPI 2 carries no per-operation response example, and the shared schemas carry
			// no account-level one (see TestPublishedAccountMonitorExamplesMatchTheirOperation),
			// so only the OpenAPI 3 documents have totals beside campaigns.
			if enclosed == 0 && strings.HasSuffix(spec, "openapi3.json") {
				t.Fatalf("no account-monitor example with totals beside campaigns found in %s; the scan is vacuous", spec)
			}
		})
	}
}

// checkUnmeasuredAgainstCampaigns pins one response example's totals to the campaigns array it
// sits beside: campaign_count is the array's length, and unmeasured_campaigns counts exactly the
// rows that say fetch_failed=true.
func checkUnmeasuredAgainstCampaigns(t *testing.T, path string, totals map[string]any, rows []any) {
	t.Helper()
	if cc, ok := totals["campaign_count"].(float64); !ok || cc != float64(len(rows)) {
		t.Errorf("%s: totals.campaign_count = %v, but the campaigns array beside it has %d rows",
			path, totals["campaign_count"], len(rows))
	}
	failed := 0
	for _, r := range rows {
		if row, ok := r.(map[string]any); ok && row["fetch_failed"] == true {
			failed++
		}
	}
	u, ok := totals["unmeasured_campaigns"].(float64)
	if !ok {
		t.Errorf("%s: totals beside campaigns without unmeasured_campaigns: %v", path, totals)
		return
	}
	if u != float64(failed) {
		t.Errorf("%s: totals.unmeasured_campaigns = %v, but %d of the %d campaign rows beside it have fetch_failed=true",
			path, u, failed, len(rows))
	}
}
