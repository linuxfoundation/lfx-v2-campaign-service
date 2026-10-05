// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeReportReader is a PlatformDispatcher that implements AccountReportReader with scripted
// answers, counting each call so a test can pin what one read did upstream.
type fakeReportReader struct {
	mu        sync.Mutex
	campaigns []model.AccountCampaignMetrics
	listErr   error
	submitID  string
	submitErr error
	check     *model.AccountReportCheck
	checkErr  error
	submits   int
	checks    int
	checkedID string
}

func (f *fakeReportReader) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (f *fakeReportReader) ListAccountCampaigns(context.Context, string, model.Provider, string) ([]model.AccountCampaignMetrics, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]model.AccountCampaignMetrics(nil), f.campaigns...), nil
}

func (f *fakeReportReader) SubmitAccountReport(context.Context, string, model.Provider, string, int) (*model.AccountReportSubmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	d := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	return &model.AccountReportSubmission{ReportID: f.submitID, WindowStart: d.AddDate(0, 0, -6), WindowEnd: d}, nil
}

func (f *fakeReportReader) CheckAccountReport(_ context.Context, _ string, _ model.Provider, _, reportID string) (*model.AccountReportCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	f.checkedID = reportID
	return f.check, f.checkErr
}

// fakeReportStore is an in-memory domain.AccountReportRepository with the real repository's
// compare-and-set semantics, plus switches to inject errors and a concurrent replacement.
type fakeReportStore struct {
	snap        *model.AccountReportSnapshot
	getErr      error
	completeErr error
	// replaceBeforeComplete simulates another read submitting a newer report while this one
	// was checking: the pending id changes just before Complete runs.
	replaceBeforeComplete string
	failures              []string
}

func (s *fakeReportStore) GetAccountReport(_ context.Context, key model.AccountReportKey) (*model.AccountReportSnapshot, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.snap == nil {
		return nil, domain.ErrNotFound
	}
	cp := *s.snap
	cp.Key = key
	return &cp, nil
}

func (s *fakeReportStore) MarkAccountReportPending(_ context.Context, key model.AccountReportKey, p model.PendingAccountReport) error {
	if s.snap == nil {
		s.snap = &model.AccountReportSnapshot{Key: key}
	}
	s.snap.Pending = &p
	return nil
}

func (s *fakeReportStore) CompleteAccountReport(_ context.Context, _ model.AccountReportKey, r model.ReadyAccountReport) (bool, error) {
	if s.completeErr != nil {
		return false, s.completeErr
	}
	if s.replaceBeforeComplete != "" {
		s.snap.Pending = &model.PendingAccountReport{ReportID: s.replaceBeforeComplete, SubmittedAt: time.Now()}
	}
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != r.ReportID {
		return false, nil
	}
	s.snap.Ready = &r
	s.snap.Pending = nil
	return true, nil
}

func (s *fakeReportStore) FailAccountReport(_ context.Context, _ model.AccountReportKey, reportID, reason string, _ time.Time) (bool, error) {
	if s.snap == nil || s.snap.Pending == nil || s.snap.Pending.ReportID != reportID {
		return false, nil
	}
	s.snap.Pending = nil
	s.failures = append(s.failures, reason)
	return true, nil
}

func reportOrch(reader *fakeReportReader, store domain.AccountReportRepository) *Orchestrator {
	o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: reader})
	o.SetAccountReportStore(store)
	return o
}

func twoCampaigns() []model.AccountCampaignMetrics {
	return []model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Name: "a", Status: "Active", BudgetDay: 10},
		{PlatformCampaignID: "2", Name: "b", Status: "Active", BudgetDay: 20},
	}
}

func readReported(t *testing.T, o *Orchestrator) *model.ReportedAccountRead {
	t.Helper()
	got, err := o.ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderMicrosoftAds, "123", 7)
	if err != nil {
		t.Fatalf("ReadReportedAccountCampaigns: %v", err)
	}
	return got
}

// The first read for an account has nothing saved: it submits a report, says one is building,
// and returns every live campaign with FetchFailed so no rule reads zero metrics as a zero.
func TestReadReported_FirstReadSubmitsAndMarksRowsUnavailable(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}
	store := &fakeReportStore{}
	got := readReported(t, reportOrch(reader, store))

	if reader.submits != 1 || reader.checks != 0 {
		t.Fatalf("submits=%d checks=%d, want one submit and no check", reader.submits, reader.checks)
	}
	if !got.MetricsPending || got.MetricsAsOf != nil {
		t.Errorf("pending=%v asOf=%v, want pending and no as-of", got.MetricsPending, got.MetricsAsOf)
	}
	if len(got.Rows) != 2 || !got.Rows[0].FetchFailed || !got.Rows[1].FetchFailed {
		t.Errorf("rows = %+v, want both campaigns with FetchFailed", got.Rows)
	}
	if store.snap.Pending == nil || store.snap.Pending.ReportID != "r1" {
		t.Errorf("saved pending = %+v, want r1", store.snap.Pending)
	}
}

// A pending report that has finished is collected, saved, and served on the same read. A
// campaign absent from the account-scoped report served nothing: zero delivery, conversions nil.
func TestReadReported_CollectsAFinishedReport(t *testing.T) {
	conv := 1.5
	reader := &fakeReportReader{campaigns: twoCampaigns(), check: &model.AccountReportCheck{
		Status: model.AccountReportReady, Partial: true,
		Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 7, Impressions: 200, Clicks: 4, Conversions: &conv}},
	}}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now().Add(-5 * time.Minute)}}}
	got := readReported(t, reportOrch(reader, store))

	if reader.checks != 1 || reader.checkedID != "r1" {
		t.Fatalf("checks=%d id=%q, want one check of r1", reader.checks, reader.checkedID)
	}
	if reader.submits != 0 {
		t.Errorf("submits=%d, want 0: the report just collected is fresh", reader.submits)
	}
	if got.MetricsPending || got.MetricsAsOf == nil {
		t.Errorf("pending=%v asOf=%v, want not pending with an as-of", got.MetricsPending, got.MetricsAsOf)
	}
	r1, r2 := got.Rows[0], got.Rows[1]
	if r1.FetchFailed || r1.Spend != 7 || r1.Impressions != 200 || r1.Clicks != 4 || r1.Conversions == nil || *r1.Conversions != 1.5 || r1.Ctr != 2 {
		t.Errorf("campaign 1 = %+v, want the report's metrics and Ctr 2%%", r1)
	}
	if r2.FetchFailed || r2.Spend != 0 || r2.Impressions != 0 || r2.Conversions != nil {
		t.Errorf("campaign 2 = %+v, want a measured zero with conversions unreported", r2)
	}
	if store.snap.Ready == nil || store.snap.Pending != nil || !store.snap.Ready.Partial {
		t.Errorf("saved snapshot = %+v, want ready (partial) and no pending", store.snap)
	}
}

// A fresh finished report is served as-is: no platform report call at all.
func TestReadReported_FreshReportMakesNoReportCalls(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns()}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{ReportID: "r0", CompletedAt: time.Now().Add(-time.Minute)}}}
	got := readReported(t, reportOrch(reader, store))
	if reader.submits != 0 || reader.checks != 0 {
		t.Errorf("submits=%d checks=%d, want none", reader.submits, reader.checks)
	}
	if got.MetricsPending || got.MetricsAsOf == nil {
		t.Errorf("pending=%v asOf=%v", got.MetricsPending, got.MetricsAsOf)
	}
}

// A stale finished report is still served while its replacement builds.
func TestReadReported_StaleReportIsServedWhileTheNextBuilds(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2"}
	completed := time.Now().Add(-2 * accountReportFreshFor)
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		ReportID: "r1", CompletedAt: completed,
		Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 3}},
	}}}
	got := readReported(t, reportOrch(reader, store))
	if reader.submits != 1 {
		t.Fatalf("submits=%d, want 1", reader.submits)
	}
	if !got.MetricsPending || got.MetricsAsOf == nil || !got.MetricsAsOf.Equal(completed) {
		t.Errorf("pending=%v asOf=%v, want pending with the stale report's completion time", got.MetricsPending, got.MetricsAsOf)
	}
	if got.Rows[0].Spend != 3 || got.Rows[0].FetchFailed {
		t.Errorf("row = %+v, want the stale report's metrics", got.Rows[0])
	}
}

// Still building: checked once, left pending, no second submission.
func TestReadReported_StillPendingIsNotResubmitted(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), check: &model.AccountReportCheck{Status: model.AccountReportPending}}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now()}}}
	got := readReported(t, reportOrch(reader, store))
	if reader.checks != 1 || reader.submits != 0 || !got.MetricsPending {
		t.Errorf("checks=%d submits=%d pending=%v", reader.checks, reader.submits, got.MetricsPending)
	}
}

// A report the platform failed is dropped and replaced in the same read.
func TestReadReported_FailedReportIsReplaced(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2", check: &model.AccountReportCheck{Status: model.AccountReportFailed}}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now()}}}
	readReported(t, reportOrch(reader, store))
	if len(store.failures) != 1 || reader.submits != 1 || store.snap.Pending.ReportID != "r2" {
		t.Errorf("failures=%v submits=%d pending=%+v, want r1 failed and r2 submitted", store.failures, reader.submits, store.snap.Pending)
	}
}

// A report pending past accountReportAbandonAfter is given up on without even checking it.
func TestReadReported_AbandonsAReportPendingTooLong(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2"}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now().Add(-2 * accountReportAbandonAfter)}}}
	readReported(t, reportOrch(reader, store))
	if reader.checks != 0 || len(store.failures) != 1 || reader.submits != 1 {
		t.Errorf("checks=%d failures=%v submits=%d, want no check, r1 abandoned, a new submit", reader.checks, store.failures, reader.submits)
	}
}

// A check error is transient: nothing changes and the read still answers.
func TestReadReported_CheckErrorKeepsPending(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), checkErr: errors.New("timeout")}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now()}}}
	got := readReported(t, reportOrch(reader, store))
	if !got.MetricsPending || store.snap.Pending.ReportID != "r1" || reader.submits != 0 {
		t.Errorf("pending=%v saved=%+v submits=%d", got.MetricsPending, store.snap.Pending, reader.submits)
	}
}

// A submit error never fails the read; the response says nothing is building.
func TestReadReported_SubmitErrorDoesNotFailTheRead(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitErr: errors.New("429")}
	got := readReported(t, reportOrch(reader, &fakeReportStore{}))
	if got.MetricsPending || len(got.Rows) != 2 {
		t.Errorf("pending=%v rows=%d, want not pending with both rows", got.MetricsPending, len(got.Rows))
	}
}

// Losing the compare-and-set to a concurrent submission still serves the rows this read
// collected, and reports the newer report as building.
func TestReadReported_LostRaceStillServesCollectedRows(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), check: &model.AccountReportCheck{
		Status: model.AccountReportReady, Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 9}},
	}}
	store := &fakeReportStore{
		snap:                  &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now()}},
		replaceBeforeComplete: "r2",
	}
	got := readReported(t, reportOrch(reader, store))
	if got.Rows[0].Spend != 9 || got.MetricsAsOf == nil {
		t.Errorf("row=%+v asOf=%v, want the collected rows served", got.Rows[0], got.MetricsAsOf)
	}
	if !got.MetricsPending || reader.submits != 0 {
		t.Errorf("pending=%v submits=%d, want r2 reported as building and no extra submit", got.MetricsPending, reader.submits)
	}
}

// The live list is the one step whose failure fails the read.
func TestReadReported_ListErrorFailsTheRead(t *testing.T) {
	want := domain.ErrAccountNotManagedByConnection
	o := reportOrch(&fakeReportReader{listErr: want}, &fakeReportStore{})
	if _, err := o.ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderMicrosoftAds, "123", 7); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// A store read failure fails the read rather than answering "no metrics yet" for an account
// that may have them.
func TestReadReported_StoreReadErrorFailsTheRead(t *testing.T) {
	o := reportOrch(&fakeReportReader{campaigns: twoCampaigns()}, &fakeReportStore{getErr: errors.New("db down")})
	if _, err := o.ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderMicrosoftAds, "123", 7); err == nil {
		t.Error("want an error")
	}
}

// No store wired, and a platform with no report capability, are both refused up front.
func TestReadReported_RefusesWithoutStoreOrCapability(t *testing.T) {
	o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderMicrosoftAds: &fakeReportReader{}})
	if _, err := o.ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderMicrosoftAds, "123", 7); err == nil || errors.Is(err, ErrAccountMetricsUnsupported) {
		t.Errorf("no store: err = %v, want a non-unsupported error (503)", err)
	}
	o2 := reportOrch(&fakeReportReader{}, &fakeReportStore{})
	if _, err := o2.ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderGoogleAds, "123", 7); !errors.Is(err, ErrAccountMetricsUnsupported) {
		t.Errorf("unregistered platform: err = %v, want ErrAccountMetricsUnsupported", err)
	}
}

// A campaign the dispatcher marked FetchFailed (unparseable budget) keeps the flag even with a
// finished report, while its trustworthy metrics are still filled in.
func TestReadReported_DispatcherFetchFailedSurvivesTheMerge(t *testing.T) {
	c := twoCampaigns()
	c[0].FetchFailed = true
	reader := &fakeReportReader{campaigns: c}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		CompletedAt: time.Now(), Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 5}},
	}}}
	got := readReported(t, reportOrch(reader, store))
	if !got.Rows[0].FetchFailed || got.Rows[0].Spend != 5 {
		t.Errorf("row = %+v, want FetchFailed kept and spend filled", got.Rows[0])
	}
}
