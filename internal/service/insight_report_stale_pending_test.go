// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A report still BUILDING for the previous period when the UTC date changes must not block a
// report for the current period (#292 review). periodCases (insight_report_period_test.go) pins
// a month boundary (this_month) and a day boundary (today).

func TestReadKeywords_StalePendingIsSupersededByOneSubmission(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeKeywordReader{account: "123", submitID: "k2", dates: utcWindowDates, submitNow: tc.after,
				check: &model.KeywordReportCheck{Status: model.AccountReportPending}}
			store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{
				// The previous period's finished report, and its successor still building.
				Ready: &model.ReadyKeywordReport{ReportID: "k0", CampaignIDs: []string{"111"}, AsOf: tc.asOf.Add(-time.Hour),
					WindowStart: tc.savedStart, WindowEnd: tc.savedEnd, Rows: []model.KeywordReportRow{kwRow("111", "1", 5)}},
				Pending: &model.PendingKeywordReport{ReportID: "k1", CampaignIDs: []string{"111"}, SubmittedAt: tc.asOf,
					WindowStart: tc.savedStart, WindowEnd: tc.savedEnd},
			}}
			o := keywordOrch([]string{"111"}, r, store)
			o.SetInsightReportClock(func() time.Time { return tc.after })
			got, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got.Rows) != 0 || got.MetricsAsOf != nil || !got.MetricsPending {
				t.Errorf("rows=%d as_of=%v pending=%v, want nothing served and a report pending", len(got.Rows), got.MetricsAsOf, got.MetricsPending)
			}
			if r.submits != 1 || r.checks != 0 {
				t.Errorf("submits=%d checks=%d, want exactly one submission and the stale report not polled", r.submits, r.checks)
			}
			p := store.snap.Pending
			if p == nil || p.ReportID != "k2" || !p.WindowStart.Equal(tc.wantStart) || !p.WindowEnd.Equal(tc.wantEnd) {
				t.Errorf("pending = %+v, want k2 for %s..%s", p, tc.wantStart.Format("2006-01-02"), tc.wantEnd.Format("2006-01-02"))
			}
			if len(store.failures) != 1 || !strings.HasPrefix(store.failures[0], "superseded") {
				t.Errorf("failures = %v, want the stale report recorded as superseded", store.failures)
			}

			// A late collection of the old report (a request still holding the old snapshot) can
			// no longer complete it...
			applied, _ := store.CompleteKeywordReport(context.Background(), model.KeywordReportKey{}, model.ReadyKeywordReport{
				ReportID: "k1", CampaignIDs: []string{"111"}, WindowStart: tc.savedStart, WindowEnd: tc.savedEnd, AsOf: tc.asOf,
				Rows: []model.KeywordReportRow{kwRow("111", "1", 50)}})
			if applied {
				t.Errorf("a late collection of the superseded report must not complete")
			}
			// ...and the next read, with k2 still building, serves nothing and submits nothing.
			r2 := &fakeKeywordReader{account: "123", dates: utcWindowDates, check: &model.KeywordReportCheck{Status: model.AccountReportPending}}
			o2 := keywordOrch([]string{"111"}, r2, store)
			o2.SetInsightReportClock(func() time.Time { return tc.after.Add(time.Minute) })
			got, err = o2.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
			if err != nil || len(got.Rows) != 0 || !got.MetricsPending || r2.submits != 0 {
				t.Errorf("next read: err=%v rows=%d pending=%v submits=%d", err, len(got.Rows), got.MetricsPending, r2.submits)
			}
		})
	}
}

// racingAudienceStore is a fakeAudienceStore on which a CONCURRENT request supersedes the stale
// report first: this request's fail loses the compare-and-set, and the winner's replacement is
// already pending.
type racingAudienceStore struct {
	*fakeAudienceStore
	winner model.PendingInsightReport
}

func (s *racingAudienceStore) FailAudienceReport(ctx context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (bool, error) {
	s.snap.Pending = &s.winner
	return s.fakeAudienceStore.FailAudienceReport(ctx, key, reportID, reason, at)
}

func TestReadAudience_StalePendingIsSupersededByOneSubmission(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			stale := func() *fakeAudienceStore {
				return &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: &model.PendingInsightReport{
					ReportID: "a1", CampaignIDs: []string{"111"}, SubmittedAt: tc.asOf, WindowStart: tc.savedStart, WindowEnd: tc.savedEnd,
				}}}
			}
			read := func(store interface {
				GetAudienceReport(context.Context, model.InsightReportKey) (*model.AudienceReportSnapshot, error)
				MarkAudienceReportPending(context.Context, model.InsightReportKey, model.PendingInsightReport) (bool, error)
				CompleteAudienceReport(context.Context, model.InsightReportKey, model.ReadyAudienceReport) (bool, error)
				FailAudienceReport(context.Context, model.InsightReportKey, string, string, time.Time) (bool, error)
			}) (*model.ReportedAudienceRead, *fakeAudienceReader) {
				r := &fakeAudienceReader{account: "123", submitID: "a2", dates: utcWindowDates, submitNow: tc.after,
					check: &model.AudienceReportCheck{Status: model.AccountReportPending}}
				o := audienceOrch([]string{"111"}, r, store)
				o.SetInsightReportClock(func() time.Time { return tc.after })
				got, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				return got, r
			}

			store := stale()
			got, r := read(store)
			if len(got.Buckets) != 0 || got.MetricsAsOf != nil || !got.MetricsPending || r.submits != 1 || r.checks != 0 {
				t.Errorf("buckets=%d as_of=%v pending=%v submits=%d checks=%d, want one submission and pending",
					len(got.Buckets), got.MetricsAsOf, got.MetricsPending, r.submits, r.checks)
			}
			if p := store.snap.Pending; p == nil || p.ReportID != "a2" || !p.WindowStart.Equal(tc.wantStart) || !p.WindowEnd.Equal(tc.wantEnd) {
				t.Errorf("pending = %+v, want a2 for the current period", p)
			}
			if applied, _ := store.CompleteAudienceReport(context.Background(), model.InsightReportKey{}, model.ReadyAudienceReport{ReportID: "a1"}); applied {
				t.Errorf("a late collection of the superseded report must not complete")
			}

			// Lost race: another request superseded a1 and recorded its own replacement first;
			// this one adopts it and submits nothing.
			winner := model.PendingInsightReport{ReportID: "a-winner", CampaignIDs: []string{"111"}, SubmittedAt: tc.after,
				WindowStart: tc.wantStart, WindowEnd: tc.wantEnd}
			racing := &racingAudienceStore{fakeAudienceStore: stale(), winner: winner}
			got, r = read(racing)
			if r.submits != 0 || !got.MetricsPending || racing.snap.Pending.ReportID != "a-winner" {
				t.Errorf("lost race: submits=%d pending=%v stored=%+v, want the winner's report adopted", r.submits, got.MetricsPending, racing.snap.Pending)
			}
		})
	}
}
