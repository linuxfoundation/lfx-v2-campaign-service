// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ─── fakes ───

type fakeMetricsRepo struct {
	mu        sync.Mutex
	campaigns []*model.Campaign
	upserts   map[string][]*model.CampaignMetric
	listErr   error
	upsertErr error
	// listPlatforms records what the sweeper asked for.
	listPlatforms []model.Provider
	listCalls     atomic.Int32
	upsertCalls   atomic.Int32
}

func newFakeMetricsRepo(cs ...*model.Campaign) *fakeMetricsRepo {
	return &fakeMetricsRepo{campaigns: cs, upserts: map[string][]*model.CampaignMetric{}}
}

func (f *fakeMetricsRepo) UpsertMetrics(_ context.Context, campaignID string, rows []*model.CampaignMetric) (int, error) {
	// Record the CALL itself, separately from the rows. Counting only appended rows
	// cannot distinguish "never called" from "called with an empty slice" — appending
	// zero rows leaves no trace — and that distinction is exactly what the
	// empty-fetch test needs to assert.
	f.upsertCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return 0, f.upsertErr
	}
	f.upserts[campaignID] = append(f.upserts[campaignID], rows...)
	return len(rows), nil
}

func (f *fakeMetricsRepo) ListMetrics(context.Context, string, string, string, time.Time, time.Time) ([]*model.CampaignMetric, error) {
	return nil, nil
}

func (f *fakeMetricsRepo) ListCampaignsForMetricsSweep(_ context.Context, platforms []model.Provider, _ int) ([]*model.Campaign, error) {
	f.listCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listPlatforms = append([]model.Provider(nil), platforms...)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.campaigns, nil
}

func (f *fakeMetricsRepo) upsertedFor(id string) []*model.CampaignMetric {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts[id]
}

// fetchDispatcher is a PlatformDispatcher that ALSO implements MetricsFetcher.
type fetchDispatcher struct {
	rows  []model.CampaignMetric
	err   error
	calls atomic.Int32
	// gotFrom/gotTo record the window the sweeper requested.
	mu      sync.Mutex
	gotFrom time.Time
	gotTo   time.Time
	// block, when non-nil, holds the fetch until closed (for cancellation tests).
	block chan struct{}
}

func (f *fetchDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("not used")
}

func (f *fetchDispatcher) FetchMetrics(ctx context.Context, _ *model.Campaign, from, to time.Time) ([]model.CampaignMetric, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.gotFrom, f.gotTo = from, to
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// plainDispatcher implements ONLY PlatformDispatcher, never MetricsFetcher.
type plainDispatcher struct{}

func (plainDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("not used")
}

func sweepCampaign(id string, p model.Provider) *model.Campaign {
	return &model.Campaign{ID: id, ProjectID: "cncf", BriefID: "b1", Platform: p, PlatformCampaignID: "555"}
}

func metricRow(day string) model.CampaignMetric {
	d, _ := time.Parse("2006-01-02", day)
	return model.CampaignMetric{
		MetricDate: d, Impressions: 100, Clicks: 10,
		Spend: "1.500000", Conversions: "0.500000", Currency: "USD",
		AttributionBasis: model.AttributionGoogleAdsClickTime,
	}
}

// ─── tests ───

func TestSweepOncePersistsFetchedRows(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderGoogleAds))
	f := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01"), metricRow("2026-07-02")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f}, MetricsSweeperConfig{})

	n, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1", n)
	}
	if got := len(repo.upsertedFor("c1")); got != 2 {
		t.Errorf("persisted %d rows, want 2", got)
	}
}

// TestSweepOnceUsesRestatementWindow pins the restatement behaviour: the sweep must
// re-read a TRAILING WINDOW, not just yesterday, because platforms restate recent days
// as conversions mature.
func TestSweepOnceUsesRestatementWindow(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderGoogleAds))
	f := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f},
		MetricsSweeperConfig{Restatement: 72 * time.Hour})

	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	if _, err := s.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	f.mu.Lock()
	gotFrom, gotTo := f.gotFrom, f.gotTo
	f.mu.Unlock()

	if !gotTo.Equal(fixed) {
		t.Errorf("window end = %v, want %v", gotTo, fixed)
	}
	wantFrom := fixed.Add(-72 * time.Hour)
	if !gotFrom.Equal(wantFrom) {
		t.Errorf("window start = %v, want %v — the sweep must re-read a trailing window so restated days are corrected", gotFrom, wantFrom)
	}
	if gotTo.Sub(gotFrom) < 24*time.Hour {
		t.Error("window is under a day; restated conversions from earlier days would be frozen at provisional values")
	}
}

// TestSweepOnceSkipsPlatformsWithoutFetcher: a dispatcher that does not implement the
// capability must be skipped, not called.
func TestSweepOnceSkipsPlatformsWithoutFetcher(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderRedditAds))
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{
		model.ProviderRedditAds: plainDispatcher{},
	}, MetricsSweeperConfig{})

	n, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("updated = %d, want 0", n)
	}
	// A platform with no fetcher must not even be requested from the repo.
	repo.mu.Lock()
	asked := repo.listPlatforms
	repo.mu.Unlock()
	for _, p := range asked {
		if p == model.ProviderRedditAds {
			t.Error("a platform whose dispatcher has no MetricsFetcher was included in the sweep query")
		}
	}
}

// TestSweepOnceUnsupportedIsSkippedNotFatal: a deferred platform must not abort the
// batch, or one unimplemented platform would block every implemented one.
func TestSweepOnceUnsupportedIsSkippedNotFatal(t *testing.T) {
	repo := newFakeMetricsRepo(
		sweepCampaign("c-meta", model.ProviderMetaAds),
		sweepCampaign("c-ga", model.ProviderGoogleAds),
	)
	unsupported := &fetchDispatcher{err: domain.ErrMetricsUnsupported}
	good := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds:   unsupported,
		model.ProviderGoogleAds: good,
	}, MetricsSweeperConfig{})

	n, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("an unsupported platform aborted the sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1 (the supported platform must still be swept)", n)
	}
	if len(repo.upsertedFor("c-ga")) != 1 {
		t.Error("the supported campaign was not persisted; an unsupported sibling must not block it")
	}
	if len(repo.upsertedFor("c-meta")) != 0 {
		t.Error("an unsupported platform persisted rows")
	}
}

// TestSweepOnceOneFailureDoesNotAbortBatch: an expired credential on one project must
// not stop every other project's metrics from refreshing.
func TestSweepOnceOneFailureDoesNotAbortBatch(t *testing.T) {
	repo := newFakeMetricsRepo(
		sweepCampaign("c-bad", model.ProviderMetaAds),
		sweepCampaign("c-good", model.ProviderGoogleAds),
	)
	bad := &fetchDispatcher{err: errors.New("401 invalid credentials")}
	good := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{
		model.ProviderMetaAds:   bad,
		model.ProviderGoogleAds: good,
	}, MetricsSweeperConfig{})

	n, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("one failing campaign aborted the batch: %v", err)
	}
	if n != 1 {
		t.Errorf("updated = %d, want 1", n)
	}
	if len(repo.upsertedFor("c-good")) != 1 {
		t.Error("a healthy campaign was skipped because a sibling failed")
	}
}

func TestSweepOnceEmptyResultPersistsNothing(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderGoogleAds))
	f := &fetchDispatcher{rows: nil}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f}, MetricsSweeperConfig{})

	n, err := s.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("updated = %d, want 0", n)
	}
	if len(repo.upsertedFor("c1")) != 0 {
		t.Error("an empty fetch wrote rows; it must not overwrite stored history with nothing")
	}
	// The repo must not be called AT ALL for an empty fetch. Asserting only on rows
	// would pass even if UpsertMetrics were invoked with an empty slice, which is a
	// pointless write and, against a real DB, a pointless transaction.
	if got := repo.upsertCalls.Load(); got != 0 {
		t.Errorf("UpsertMetrics was called %d times for an empty fetch, want 0", got)
	}
}

func TestSweepOnceRepoErrorSurfaces(t *testing.T) {
	repo := newFakeMetricsRepo()
	repo.listErr = errors.New("db down")
	f := &fetchDispatcher{}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f}, MetricsSweeperConfig{})

	if _, err := s.SweepOnce(context.Background()); err == nil {
		t.Error("a repo failure must surface, not be swallowed as a clean sweep")
	}
}

// TestSweepOnceStopsOnCancellation: cancelling mid-batch must stop promptly rather
// than starting further platform calls.
func TestSweepOnceStopsOnCancellation(t *testing.T) {
	repo := newFakeMetricsRepo(
		sweepCampaign("c1", model.ProviderGoogleAds),
		sweepCampaign("c2", model.ProviderGoogleAds),
		sweepCampaign("c3", model.ProviderGoogleAds),
	)
	f := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f}, MetricsSweeperConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first campaign

	_, err := s.SweepOnce(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := f.calls.Load(); got != 0 {
		t.Errorf("made %d platform calls after cancellation, want 0", got)
	}
}

// TestShutdownStopsSweeperPromptly is the lifecycle contract: Shutdown must return
// quickly and the goroutine must be gone, even with a fetch in flight.
func TestShutdownStopsSweeperPromptly(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderGoogleAds))
	f := &fetchDispatcher{rows: []model.CampaignMetric{metricRow("2026-07-01")}}
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f},
		MetricsSweeperConfig{Interval: time.Millisecond})
	s.Start()

	// Let at least one tick land.
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Shutdown took %v; it must stop within the container close budget", elapsed)
	}
}

// TestShutdownIsIdempotentAndSafeWithoutStart: Shutdown must not panic when called
// twice or when Start was never called (the container may close before boot finishes).
func TestShutdownIsIdempotentAndSafeWithoutStart(t *testing.T) {
	s := NewMetricsSweeper(newFakeMetricsRepo(), nil, MetricsSweeperConfig{})
	ctx := context.Background()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown without Start: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestShutdownInterruptsBlockedFetch proves cancellation reaches INTO an in-flight
// platform call, rather than waiting for it to finish on its own.
func TestShutdownInterruptsBlockedFetch(t *testing.T) {
	repo := newFakeMetricsRepo(sweepCampaign("c1", model.ProviderGoogleAds))
	f := &fetchDispatcher{block: make(chan struct{})} // never closed: blocks until ctx dies
	s := NewMetricsSweeper(repo, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: f},
		MetricsSweeperConfig{Interval: time.Millisecond})
	s.Start()

	// Wait for the fetch to actually be in flight.
	deadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.calls.Load() == 0 {
		t.Fatal("no fetch started; the test cannot exercise interruption")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with a blocked fetch: %v", err)
	}
	// It must not have waited for metricsFetchTimeout (5s) to elapse.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Shutdown took %v; cancellation must interrupt the in-flight fetch, not wait it out", elapsed)
	}
}

// TestSweeperTimeoutsFitShutdownBudget mirrors the init() assertion as a visible test.
func TestSweeperTimeoutsFitShutdownBudget(t *testing.T) {
	if metricsSweepTimeout > CancelGracePeriod {
		t.Errorf("metricsSweepTimeout (%v) exceeds CancelGracePeriod (%v): a sweep could outlive the container close budget",
			metricsSweepTimeout, CancelGracePeriod)
	}
	if metricsFetchTimeout > metricsSweepTimeout {
		t.Errorf("metricsFetchTimeout (%v) exceeds metricsSweepTimeout (%v): one campaign could consume an entire sweep",
			metricsFetchTimeout, metricsSweepTimeout)
	}
}

func TestNewMetricsSweeperAppliesDefaults(t *testing.T) {
	s := NewMetricsSweeper(newFakeMetricsRepo(), nil, MetricsSweeperConfig{})
	if s.interval != DefaultMetricsSweepInterval {
		t.Errorf("interval = %v, want %v", s.interval, DefaultMetricsSweepInterval)
	}
	if s.restatement != DefaultMetricsRestatementWindow {
		t.Errorf("restatement = %v, want %v", s.restatement, DefaultMetricsRestatementWindow)
	}
	// The restatement default must span more than one day, or restated conversions
	// from earlier days would never be corrected.
	if s.restatement <= 24*time.Hour {
		t.Errorf("default restatement window %v is not longer than a day; platforms restate conversions for several days", s.restatement)
	}
}
