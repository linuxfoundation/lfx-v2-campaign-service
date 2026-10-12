// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// utcWindowDates is the UTC-day rule the Microsoft client resolves a window by
// (microsoft.ReportWindowDates), restated for the windows these tests use.
func utcWindowDates(w model.MetricsWindow, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch w {
	case model.MetricsWindowToday:
		return day, day, nil
	case model.MetricsWindowThisMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), day, nil
	default:
		return day.AddDate(0, 0, -29), day, nil
	}
}

func utcDay(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// periodCase is one rollover: a report requested shortly before a calendar boundary (so it is
// minutes old, and fresh by age) read just after it.
type periodCase struct {
	name                 string
	window               model.MetricsWindow
	asOf, before, after  time.Time
	savedStart, savedEnd time.Time
	wantStart, wantEnd   time.Time
}

var periodCases = []periodCase{
	{
		name: "this_month across a month boundary", window: model.MetricsWindowThisMonth,
		asOf:       time.Date(2026, 10, 31, 23, 50, 0, 0, time.UTC),
		before:     time.Date(2026, 10, 31, 23, 55, 0, 0, time.UTC),
		after:      time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC),
		savedStart: utcDay(2026, 10, 1), savedEnd: utcDay(2026, 10, 31),
		wantStart: utcDay(2026, 11, 1), wantEnd: utcDay(2026, 11, 1),
	},
	{
		name: "today across a day boundary", window: model.MetricsWindowToday,
		asOf:       time.Date(2026, 10, 15, 23, 50, 0, 0, time.UTC),
		before:     time.Date(2026, 10, 15, 23, 59, 0, 0, time.UTC),
		after:      time.Date(2026, 10, 16, 0, 5, 0, 0, time.UTC),
		savedStart: utcDay(2026, 10, 15), savedEnd: utcDay(2026, 10, 15),
		wantStart: utcDay(2026, 10, 16), wantEnd: utcDay(2026, 10, 16),
	},
}

// TestReadKeywords_ServesASavedReportOnlyForItsOwnPeriod: a fresh, covering keyword report is
// served while its dates are the window's, and NOT once the window has rolled over — then the
// read answers like the no-report case (nothing served, metrics_as_of absent, pending) and submits
// a replacement for the new dates.
func TestReadKeywords_ServesASavedReportOnlyForItsOwnPeriod(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			read := func(now time.Time) (*model.ReportedKeywordRead, *fakeKeywordReader, *fakeKeywordStore) {
				r := &fakeKeywordReader{account: "123", submitID: "k2", dates: utcWindowDates, submitNow: now}
				store := &fakeKeywordStore{snap: &model.KeywordReportSnapshot{Ready: &model.ReadyKeywordReport{
					ReportID: "k1", CampaignIDs: []string{"111"}, AsOf: tc.asOf,
					WindowStart: tc.savedStart, WindowEnd: tc.savedEnd,
					Rows: []model.KeywordReportRow{kwRow("111", "1", 5)},
				}}}
				o := keywordOrch([]string{"111"}, r, store)
				o.SetInsightReportClock(func() time.Time { return now })
				got, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				return got, r, store
			}

			got, r, _ := read(tc.before)
			if len(got.Rows) != 1 || got.MetricsAsOf == nil || got.MetricsPending || r.submits != 0 {
				t.Errorf("same period: rows=%d as_of=%v pending=%v submits=%d, want the saved report served as is",
					len(got.Rows), got.MetricsAsOf, got.MetricsPending, r.submits)
			}

			got, r, store := read(tc.after)
			if len(got.Rows) != 0 || got.MetricsAsOf != nil || !got.MetricsPending {
				t.Errorf("rolled over: rows=%d as_of=%v pending=%v, want nothing served and a report pending",
					len(got.Rows), got.MetricsAsOf, got.MetricsPending)
			}
			if r.submits != 1 || store.snap.Pending == nil ||
				!store.snap.Pending.WindowStart.Equal(tc.wantStart) || !store.snap.Pending.WindowEnd.Equal(tc.wantEnd) {
				t.Errorf("rolled over: submits=%d pending=%+v, want one replacement for %s..%s",
					r.submits, store.snap.Pending, tc.wantStart.Format("2006-01-02"), tc.wantEnd.Format("2006-01-02"))
			}
		})
	}
}

// TestReadAudience_ServesASavedReportOnlyForItsOwnPeriod is the audience kind's twin, plus a
// report PENDING across the boundary: it is superseded before the collect step — never polled,
// stored or served for the new period — and its replacement is submitted on the same read.
func TestReadAudience_ServesASavedReportOnlyForItsOwnPeriod(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			read := func(now time.Time, snap *model.AudienceReportSnapshot, check *model.AudienceReportCheck) (*model.ReportedAudienceRead, *fakeAudienceReader, *fakeAudienceStore) {
				r := &fakeAudienceReader{account: "123", submitID: "a2", dates: utcWindowDates, submitNow: now, check: check}
				store := &fakeAudienceStore{snap: snap}
				o := audienceOrch([]string{"111"}, r, store)
				o.SetInsightReportClock(func() time.Time { return now })
				got, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				return got, r, store
			}
			ready := func() *model.AudienceReportSnapshot {
				return &model.AudienceReportSnapshot{Ready: &model.ReadyAudienceReport{
					ReportID: "a1", CampaignIDs: []string{"111"}, AsOf: tc.asOf,
					WindowStart: tc.savedStart, WindowEnd: tc.savedEnd,
					Rows: []model.AudienceReportRow{agRow("111", "25-34", "Female", 5)},
				}}
			}

			got, r, _ := read(tc.before, ready(), nil)
			if len(got.Buckets) != 1 || got.MetricsAsOf == nil || got.MetricsPending || r.submits != 0 {
				t.Errorf("same period: buckets=%d as_of=%v pending=%v submits=%d", len(got.Buckets), got.MetricsAsOf, got.MetricsPending, r.submits)
			}

			got, r, store := read(tc.after, ready(), nil)
			if len(got.Buckets) != 0 || got.MetricsAsOf != nil || !got.MetricsPending || r.submits != 1 ||
				!store.snap.Pending.WindowStart.Equal(tc.wantStart) || !store.snap.Pending.WindowEnd.Equal(tc.wantEnd) {
				t.Errorf("rolled over: buckets=%d as_of=%v pending=%v submits=%d pending=%+v",
					len(got.Buckets), got.MetricsAsOf, got.MetricsPending, r.submits, store.snap.Pending)
			}

			// Pending across the boundary (even one Microsoft has finished): superseded without
			// being polled, never stored or served as the new period, and replaced at once.
			pending := &model.AudienceReportSnapshot{Pending: &model.PendingInsightReport{
				ReportID: "a1", CampaignIDs: []string{"111"}, SubmittedAt: tc.asOf,
				WindowStart: tc.savedStart, WindowEnd: tc.savedEnd,
			}}
			check := &model.AudienceReportCheck{Status: model.AccountReportReady, Rows: []model.AudienceReportRow{agRow("111", "25-34", "Female", 5)}}
			got, r, store = read(tc.after, pending, check)
			if store.snap.Ready != nil || r.checks != 0 {
				t.Errorf("a report pending for another period must not be polled or stored: ready=%+v checks=%d", store.snap.Ready, r.checks)
			}
			if len(got.Buckets) != 0 || got.MetricsAsOf != nil || !got.MetricsPending || r.submits != 1 {
				t.Errorf("pending across the boundary: buckets=%d as_of=%v pending=%v submits=%d, want nothing served and a replacement",
					len(got.Buckets), got.MetricsAsOf, got.MetricsPending, r.submits)
			}
		})
	}
}
