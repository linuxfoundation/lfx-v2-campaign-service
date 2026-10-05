// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// submitUntilDeadline makes SubmitAccountReport block until its context is done and THEN
	// succeed — the platform accepted the last write just as the call budget ran out.
	submitUntilDeadline bool
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

func (f *fakeReportReader) SubmitAccountReport(ctx context.Context, _ string, _ model.Provider, _ string, _ int) (*model.AccountReportSubmission, error) {
	if f.submitUntilDeadline {
		<-ctx.Done()
	}
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
	// markedBeforeSubmit simulates a concurrent read marking its own submission between this
	// read's snapshot and its mark.
	markedBeforeSubmit string
	failures           []string
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

func (s *fakeReportStore) MarkAccountReportPending(_ context.Context, key model.AccountReportKey, p model.PendingAccountReport) (bool, error) {
	if s.markedBeforeSubmit != "" && s.snap != nil && s.snap.Pending == nil {
		// A concurrent read's submission lands between this read's snapshot and its mark.
		s.snap.Pending = &model.PendingAccountReport{ReportID: s.markedBeforeSubmit, SubmittedAt: time.Now()}
	}
	if s.snap == nil {
		s.snap = &model.AccountReportSnapshot{Key: key}
	}
	if s.snap.Pending != nil {
		return false, nil
	}
	s.snap.Pending = &p
	return true, nil
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
	submitted := time.Now().Add(-5 * time.Minute)
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: submitted}}}
	got := readReported(t, reportOrch(reader, store))

	if !store.snap.Ready.AsOf.Equal(submitted) {
		t.Errorf("saved AsOf = %v, want the submission time %v (not the collection time)", store.snap.Ready.AsOf, submitted)
	}
	if reader.checks != 1 || reader.checkedID != "r1" {
		t.Fatalf("checks=%d id=%q, want one check of r1", reader.checks, reader.checkedID)
	}
	if reader.submits != 0 {
		t.Errorf("submits=%d, want 0: the report just collected is fresh", reader.submits)
	}
	if got.MetricsPending || got.MetricsAsOf == nil || !got.MetricsAsOf.Equal(store.snap.Ready.AsOf) {
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
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{ReportID: "r0", AsOf: time.Now().Add(-time.Minute)}}}
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
		ReportID: "r1", AsOf: completed,
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

// An overdue report is CHECKED before it is abandoned. A finished one is collected, however
// late — abandoning on age alone meant an account viewed less than hourly never collected
// anything, because every visit threw away the report the previous visit had started.
func TestReadReported_OverdueButFinishedReportIsCollected(t *testing.T) {
	submitted := time.Now().Add(-2 * accountReportAbandonAfter)
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2", check: &model.AccountReportCheck{
		Status: model.AccountReportReady, Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 4}},
	}}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: submitted}}}
	got := readReported(t, reportOrch(reader, store))
	if reader.checks != 1 || len(store.failures) != 0 {
		t.Fatalf("checks=%d failures=%v, want r1 checked and collected, not abandoned", reader.checks, store.failures)
	}
	if got.Rows[0].Spend != 4 {
		t.Errorf("row = %+v, want the overdue report's metrics", got.Rows[0])
	}
	// Collected two hours after it was requested, so it describes data two hours old: already
	// stale, and the same read submits the next one.
	if got.MetricsAsOf == nil || !got.MetricsAsOf.Equal(submitted) || reader.submits != 1 {
		t.Errorf("asOf=%v submits=%d, want as-of = submission time and a refresh submitted", got.MetricsAsOf, reader.submits)
	}
}

// Only a report that is STILL pending past the cutoff is abandoned (and replaced).
func TestReadReported_AbandonsAReportStillPendingTooLong(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2", check: &model.AccountReportCheck{Status: model.AccountReportPending}}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now().Add(-2 * accountReportAbandonAfter)}}}
	readReported(t, reportOrch(reader, store))
	if reader.checks != 1 || len(store.failures) != 1 || reader.submits != 1 {
		t.Errorf("checks=%d failures=%v submits=%d, want r1 checked, abandoned, and replaced", reader.checks, store.failures, reader.submits)
	}
}

// A report that cannot be checked is also abandoned once overdue, so a permanently failing
// check cannot pin the account to one report forever.
func TestReadReported_AbandonsAnUncheckableOverdueReport(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r2", checkErr: errors.New("timeout")}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Pending: &model.PendingAccountReport{ReportID: "r1", SubmittedAt: time.Now().Add(-2 * accountReportAbandonAfter)}}}
	readReported(t, reportOrch(reader, store))
	if len(store.failures) != 1 || reader.submits != 1 {
		t.Errorf("failures=%v submits=%d, want r1 abandoned and replaced", store.failures, reader.submits)
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
		AsOf: time.Now(), Rows: []model.AccountReportRow{{PlatformCampaignID: "1", Spend: 5}},
	}}}
	got := readReported(t, reportOrch(reader, store))
	if !got.Rows[0].FetchFailed || got.Rows[0].Spend != 5 {
		t.Errorf("row = %+v, want FetchFailed kept and spend filled", got.Rows[0])
	}
}

// Two reads that both saw "nothing pending" both submit; the FIRST mark wins and the loser
// adopts it instead of overwriting it, so the winner's report is still collected later.
func TestReadReported_LosingTheMarkAdoptsTheWinnersReport(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "mine"}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{}, markedBeforeSubmit: "theirs"}
	got := readReported(t, reportOrch(reader, store))
	if store.snap.Pending == nil || store.snap.Pending.ReportID != "theirs" {
		t.Fatalf("saved pending = %+v, want the concurrent read's report kept", store.snap.Pending)
	}
	if !got.MetricsPending {
		t.Error("metrics_pending = false, want the winner's report reported as building")
	}
}

// markCtxStore records the context MarkAccountReportPending was handed.
type markCtxStore struct {
	fakeReportStore
	markErr      error
	markDeadline time.Time
	markHasDL    bool
}

func (s *markCtxStore) MarkAccountReportPending(ctx context.Context, key model.AccountReportKey, p model.PendingAccountReport) (bool, error) {
	s.markErr = ctx.Err()
	s.markDeadline, s.markHasDL = ctx.Deadline()
	if s.markErr != nil {
		return false, s.markErr
	}
	return s.fakeReportStore.MarkAccountReportPending(ctx, key, p)
}

// A submission that completes with the call budget spent must still be recorded: the report
// exists on the platform, and an unrecorded one is resubmitted on every read and never collected.
// The mark therefore runs on its own short budget, detached from the expired one.
func TestReadReported_MarkUsesItsOwnBudget(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1", submitUntilDeadline: true}
	store := &markCtxStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := reportOrch(reader, store).ReadReportedAccountCampaigns(ctx, "proj", model.ProviderMicrosoftAds, "123", 7)
	if err != nil {
		t.Fatalf("ReadReportedAccountCampaigns: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("precondition: the request budget should be spent by the time the mark runs")
	}
	if store.markErr != nil {
		t.Fatalf("mark ran on a dead context (%v); want its own live budget", store.markErr)
	}
	if !store.markHasDL || time.Until(store.markDeadline) > accountReportMarkTimeout {
		t.Errorf("mark deadline = %v (set=%v), want one bounded by accountReportMarkTimeout", store.markDeadline, store.markHasDL)
	}
	if store.snap == nil || store.snap.Pending == nil || store.snap.Pending.ReportID != "r1" || !got.MetricsPending {
		t.Errorf("saved pending = %+v, response pending = %v; want r1 recorded and reported as building", store.snap, got.MetricsPending)
	}
}

// A submission declined for lack of budget is a skip, not a failure: nothing is recorded, the
// read still serves its rows, and nothing is reported as building.
func TestReadReported_BudgetSkipIsNotRecorded(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitErr: fmt.Errorf("submit: %w", domain.ErrAccountReportBudgetTooShort)}
	store := &fakeReportStore{}
	got := readReported(t, reportOrch(reader, store))
	if got.MetricsPending || len(got.Rows) != 2 || (store.snap != nil && store.snap.Pending != nil) {
		t.Errorf("pending=%v rows=%d saved=%+v, want a skipped submission with nothing saved", got.MetricsPending, len(got.Rows), store.snap)
	}
}

// A PERMANENT refusal — too many active campaigns, or a timezone off the whole UTC hour — fails
// the read with its sentinel instead of being logged as a retry: no later read could submit, so
// serving the saved (or absent) metrics as if a refresh were merely delayed would hide it
// forever. Nothing is recorded as pending.
func TestReadReported_PermanentRefusalFailsTheRead(t *testing.T) {
	for _, sentinel := range []error{domain.ErrAccountTooManyActiveCampaigns, domain.ErrAccountTimezoneUnsupported} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			reader := &fakeReportReader{campaigns: twoCampaigns(), submitErr: fmt.Errorf("submit: %w", sentinel)}
			store := &fakeReportStore{}
			got, err := reportOrch(reader, store).ReadReportedAccountCampaigns(context.Background(), "proj", model.ProviderMicrosoftAds, "123", 7)
			if !errors.Is(err, sentinel) || got != nil {
				t.Fatalf("got %+v, err = %v; want the read to fail with %v", got, err, sentinel)
			}
			if store.snap != nil && store.snap.Pending != nil {
				t.Errorf("saved pending = %+v, want nothing recorded", store.snap.Pending)
			}
		})
	}
	// A fresh saved report means no submission is attempted, so it is still served.
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitErr: domain.ErrAccountTooManyActiveCampaigns}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{ReportID: "r0", AsOf: time.Now()}}}
	if got := readReported(t, reportOrch(reader, store)); len(got.Rows) != 2 || reader.submits != 0 {
		t.Errorf("rows=%d submits=%d, want the fresh report served with no submission", len(got.Rows), reader.submits)
	}
}

// The finished report's window travels with its metrics, so a rule engine that judges dates
// (X's) evaluates on the days the report covered.
func TestReadReported_CarriesTheReportWindow(t *testing.T) {
	ws := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	we := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	reader := &fakeReportReader{campaigns: twoCampaigns()}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		ReportID: "r0", AsOf: time.Now(), WindowStart: ws, WindowEnd: we,
	}}}
	got := readReported(t, reportOrch(reader, store))
	if got.MetricsWindowStart == nil || !got.MetricsWindowStart.Equal(ws) || got.MetricsWindowEnd == nil || !got.MetricsWindowEnd.Equal(we) {
		t.Errorf("window = %v..%v, want %v..%v", got.MetricsWindowStart, got.MetricsWindowEnd, ws, we)
	}

	none := readReported(t, reportOrch(&fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}, &fakeReportStore{}))
	if none.MetricsWindowStart != nil || none.MetricsWindowEnd != nil {
		t.Errorf("window = %v..%v, want nil with no finished report", none.MetricsWindowStart, none.MetricsWindowEnd)
	}
}
