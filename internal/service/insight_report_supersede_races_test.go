// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The supersede races (#294 review). Every case runs across both periodCases boundaries with a
// pinned clock, on a key whose pending report was requested for the previous period.

// scriptedAudienceStore lets a test script what a CONCURRENT request has done by the time this
// one fails the stale report (onFail) and what the re-read returns (getErr).
type scriptedAudienceStore struct {
	*fakeAudienceStore
	onFail  func(s *fakeAudienceStore, reportID string)
	failErr error
	getErr  error
	gets    int
}

func (s *scriptedAudienceStore) FailAudienceReport(ctx context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (bool, error) {
	if s.failErr != nil {
		return false, s.failErr
	}
	if s.onFail != nil {
		s.onFail(s.fakeAudienceStore, reportID)
	}
	return s.fakeAudienceStore.FailAudienceReport(ctx, key, reportID, reason, at)
}

func (s *scriptedAudienceStore) GetAudienceReport(ctx context.Context, key model.InsightReportKey) (*model.AudienceReportSnapshot, error) {
	s.gets++
	if s.gets > 1 && s.getErr != nil { // the first get is the read's own initial load
		return nil, s.getErr
	}
	return s.fakeAudienceStore.GetAudienceReport(ctx, key)
}

func stalePending(tc periodCase, id string) *model.PendingInsightReport {
	return &model.PendingInsightReport{ReportID: id, CampaignIDs: []string{"111"}, SubmittedAt: tc.asOf,
		WindowStart: tc.savedStart, WindowEnd: tc.savedEnd}
}

func readRacingAudience(t *testing.T, tc periodCase, store *scriptedAudienceStore) (*model.ReportedAudienceRead, *fakeAudienceReader, error) {
	t.Helper()
	r := &fakeAudienceReader{account: "123", submitID: "a-new", dates: utcWindowDates, submitNow: tc.after,
		check: &model.AudienceReportCheck{Status: model.AccountReportPending}}
	o := audienceOrch([]string{"111"}, r, store)
	o.SetInsightReportClock(func() time.Time { return tc.after })
	got, err := o.ReadReportedAudience(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window)
	return got, r, err
}

// Item 1: the lost CAS re-reads ANOTHER old-period report (recorded by a request that read before
// the date change); it is superseded in turn, and exactly one current-period report is submitted.
func TestSupersede_ReReadAnotherStaleReportIsSupersededToo(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			store := &scriptedAudienceStore{fakeAudienceStore: &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: stalePending(tc, "old-1")}}}
			store.onFail = func(s *fakeAudienceStore, id string) {
				if id == "old-1" {
					s.snap.Pending = stalePending(tc, "old-2") // a concurrent pre-midnight request won
				}
			}
			got, r, err := readRacingAudience(t, tc, store)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if r.submits != 1 || r.checks != 0 || !got.MetricsPending {
				t.Errorf("submits=%d checks=%d pending=%v, want one submission, no poll", r.submits, r.checks, got.MetricsPending)
			}
			if p := store.snap.Pending; p == nil || p.ReportID != "a-new" || !p.WindowStart.Equal(tc.wantStart) {
				t.Errorf("pending = %+v, want a-new for the current period (old-2 must not be adopted)", p)
			}
		})
	}
}

// Item 2: the winner's replacement already FINISHED by the re-read — the stored current-period
// report is adopted and served, and no second report is submitted.
func TestSupersede_AdoptsAFinishedReplacement(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			store := &scriptedAudienceStore{fakeAudienceStore: &fakeAudienceStore{snap: &model.AudienceReportSnapshot{
				Ready: &model.ReadyAudienceReport{ReportID: "prev", CampaignIDs: []string{"111"}, AsOf: tc.asOf.Add(-time.Hour),
					WindowStart: tc.savedStart, WindowEnd: tc.savedEnd, Rows: []model.AudienceReportRow{agRow("111", "18-24", "Male", 9)}},
				Pending: stalePending(tc, "old-1"),
			}}}
			store.onFail = func(s *fakeAudienceStore, _ string) {
				s.snap.Pending = nil
				s.snap.Ready = &model.ReadyAudienceReport{ReportID: "winner", CampaignIDs: []string{"111"}, AsOf: tc.after.Add(-time.Minute),
					WindowStart: tc.wantStart, WindowEnd: tc.wantEnd, Rows: []model.AudienceReportRow{agRow("111", "25-34", "Female", 5)}}
			}
			got, r, err := readRacingAudience(t, tc, store)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if r.submits != 0 || got.MetricsPending || got.MetricsAsOf == nil || len(got.Buckets) != 1 || got.Buckets[0].AgeGroup != "25-34" {
				t.Errorf("submits=%d pending=%v as_of=%v buckets=%+v, want the winner's finished report served and nothing submitted",
					r.submits, got.MetricsPending, got.MetricsAsOf, got.Buckets)
			}
		})
	}
}

// Item 3: an unresolved supersession fails the read — never polls the stale report, never
// submits, never answers metrics_pending.
func TestSupersede_UnresolvedFailsTheRead(t *testing.T) {
	storeDown := errors.New("db down")
	for _, tc := range periodCases {
		for name, mk := range map[string]func() *scriptedAudienceStore{
			"CAS error": func() *scriptedAudienceStore {
				return &scriptedAudienceStore{fakeAudienceStore: &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: stalePending(tc, "old-1")}}, failErr: storeDown}
			},
			"re-read error": func() *scriptedAudienceStore {
				s := &scriptedAudienceStore{fakeAudienceStore: &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: stalePending(tc, "old-1")}}, getErr: storeDown}
				s.onFail = func(f *fakeAudienceStore, _ string) { f.snap.Pending = stalePending(tc, "old-2") }
				return s
			},
			"bound exhausted": func() *scriptedAudienceStore {
				s := &scriptedAudienceStore{fakeAudienceStore: &fakeAudienceStore{snap: &model.AudienceReportSnapshot{Pending: stalePending(tc, "old-0")}}}
				n := 0
				s.onFail = func(f *fakeAudienceStore, _ string) {
					n++
					f.snap.Pending = stalePending(tc, "old-next-"+string(rune('a'+n)))
				}
				return s
			},
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				store := mk()
				got, r, err := readRacingAudience(t, tc, store)
				if err == nil {
					t.Fatalf("got %+v, want the read to fail", got)
				}
				if name != "bound exhausted" && !errors.Is(err, storeDown) {
					t.Errorf("err = %v, want the store failure wrapped", err)
				}
				if r.checks != 0 || r.submits != 0 {
					t.Errorf("checks=%d submits=%d, want the stale report never polled and nothing submitted", r.checks, r.submits)
				}
			})
		}
	}
}

// The keyword read shares the step: its unresolved supersession fails too, and a lost CAS that
// re-reads another stale report supersedes it.
func TestSupersede_KeywordReadSharesTheRules(t *testing.T) {
	for _, tc := range periodCases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &model.KeywordReportSnapshot{Pending: &model.PendingKeywordReport{ReportID: "old-1", CampaignIDs: []string{"111"},
				SubmittedAt: tc.asOf, WindowStart: tc.savedStart, WindowEnd: tc.savedEnd}}
			r := &fakeKeywordReader{account: "123", submitID: "k-new", dates: utcWindowDates, submitNow: tc.after}
			store := &fakeKeywordStore{snap: snap, getErr: nil}
			o := keywordOrch([]string{"111"}, r, &failingKeywordStore{fakeKeywordStore: store, err: errors.New("db down")})
			o.SetInsightReportClock(func() time.Time { return tc.after })
			if _, err := o.ReadReportedKeywordPerformance(context.Background(), "cncf", model.ProviderMicrosoftAds, tc.window); err == nil || r.checks+r.submits != 0 {
				t.Errorf("CAS error: err=%v checks=%d submits=%d, want a failed read with no upstream call", err, r.checks, r.submits)
			}
		})
	}
}

type failingKeywordStore struct {
	*fakeKeywordStore
	err error
}

func (s *failingKeywordStore) FailKeywordReport(context.Context, model.KeywordReportKey, string, string, time.Time) (bool, error) {
	return false, s.err
}
