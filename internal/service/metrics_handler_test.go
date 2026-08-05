// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	metricsgen "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_metrics"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// listRecordingRepo captures the window ListMetrics was called with and returns a
// canned result.
type listRecordingRepo struct {
	rows     []*model.CampaignMetric
	err      error
	gotFrom  time.Time
	gotTo    time.Time
	numCalls int
}

func (r *listRecordingRepo) UpsertMetrics(context.Context, string, []*model.CampaignMetric) (int, error) {
	return 0, nil
}

func (r *listRecordingRepo) ListMetrics(_ context.Context, _, _, _ string, from, to time.Time) ([]*model.CampaignMetric, error) {
	r.numCalls++
	r.gotFrom, r.gotTo = from, to
	return r.rows, r.err
}

func (r *listRecordingRepo) ListCampaignsForMetricsSweep(context.Context, []model.Provider, int) ([]*model.Campaign, error) {
	return nil, nil
}

func strptr(s string) *string { return &s }

func metricsPayload() *metricsgen.GetCampaignMetricsPayload {
	return &metricsgen.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
	}
}

func fixedNowService(repo domain.MetricsRepository) *MetricsService {
	s := NewMetricsService(repo)
	s.now = func() time.Time { return time.Date(2026, 7, 20, 15, 30, 0, 0, time.UTC) }
	return s
}

// TestGetCampaignMetricsNilRepoIs503 pins the typed-503 mode: with no database the
// route must be mounted and answer 503, never 404 or 500.
func TestGetCampaignMetricsNilRepoIs503(t *testing.T) {
	s := NewMetricsService(nil)
	_, err := s.GetCampaignMetrics(context.Background(), metricsPayload())

	var unavail *metricsgen.ConnServiceUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("err = %T (%v), want ConnServiceUnavailableError", err, err)
	}
}

func TestGetCampaignMetricsDefaultsToTrailingWindow(t *testing.T) {
	repo := &listRecordingRepo{}
	s := fixedNowService(repo)

	if _, err := s.GetCampaignMetrics(context.Background(), metricsPayload()); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	// `to` defaults to today at midnight UTC; `from` to 30 days earlier.
	wantTo := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if !repo.gotTo.Equal(wantTo) {
		t.Errorf("default `to` = %v, want %v", repo.gotTo, wantTo)
	}
	if want := wantTo.Add(-defaultMetricsWindow); !repo.gotFrom.Equal(want) {
		t.Errorf("default `from` = %v, want %v", repo.gotFrom, want)
	}
}

func TestGetCampaignMetricsHonoursExplicitWindow(t *testing.T) {
	repo := &listRecordingRepo{}
	s := fixedNowService(repo)

	p := metricsPayload()
	p.From, p.To = strptr("2026-06-01"), strptr("2026-06-15")
	if _, err := s.GetCampaignMetrics(context.Background(), p); err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if got := repo.gotFrom.Format(metricsDateLayout); got != "2026-06-01" {
		t.Errorf("from = %s, want 2026-06-01", got)
	}
	if got := repo.gotTo.Format(metricsDateLayout); got != "2026-06-15" {
		t.Errorf("to = %s, want 2026-06-15", got)
	}
}

// TestGetCampaignMetricsRejectsInvertedWindow: an inverted range matches nothing in
// SQL, which a caller would read as "no activity" rather than "bad request".
func TestGetCampaignMetricsRejectsInvertedWindow(t *testing.T) {
	repo := &listRecordingRepo{}
	s := fixedNowService(repo)

	p := metricsPayload()
	p.From, p.To = strptr("2026-06-15"), strptr("2026-06-01")
	_, err := s.GetCampaignMetrics(context.Background(), p)

	var badReq *metricsgen.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("err = %T (%v), want BadRequestError", err, err)
	}
	if repo.numCalls != 0 {
		t.Error("an inverted window still hit the database; it must be rejected first")
	}
}

func TestGetCampaignMetricsRejectsUnparseableDates(t *testing.T) {
	repo := &listRecordingRepo{}
	s := fixedNowService(repo)

	for _, tc := range []struct{ name, from, to string }{
		{"bad from", "not-a-date", "2026-06-15"},
		{"bad to", "2026-06-01", "nonsense"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := metricsPayload()
			p.From, p.To = strptr(tc.from), strptr(tc.to)
			var badReq *metricsgen.BadRequestError
			if _, err := s.GetCampaignMetrics(context.Background(), p); !errors.As(err, &badReq) {
				t.Errorf("err = %T (%v), want BadRequestError", err, err)
			}
		})
	}
}

func TestGetCampaignMetricsRejectsOversizeWindow(t *testing.T) {
	repo := &listRecordingRepo{}
	s := fixedNowService(repo)

	p := metricsPayload()
	p.From, p.To = strptr("2000-01-01"), strptr("2026-06-15")
	var badReq *metricsgen.BadRequestError
	if _, err := s.GetCampaignMetrics(context.Background(), p); !errors.As(err, &badReq) {
		t.Errorf("err = %T (%v), want BadRequestError for an absurd window", err, err)
	}
}

func TestGetCampaignMetricsNotFoundMaps404(t *testing.T) {
	repo := &listRecordingRepo{err: domain.ErrNotFound}
	s := fixedNowService(repo)

	var nf *metricsgen.NotFoundError
	if _, err := s.GetCampaignMetrics(context.Background(), metricsPayload()); !errors.As(err, &nf) {
		t.Errorf("err = %T (%v), want NotFoundError", err, err)
	}
}

// TestSummaryOmitsSpendAcrossCurrencies is the API-level expression of the money
// caveat: the field must be ABSENT (nil), not zero and not a wrong sum.
func TestSummaryOmitsSpendAcrossCurrencies(t *testing.T) {
	repo := &listRecordingRepo{rows: []*model.CampaignMetric{
		{Spend: "10.000000", Conversions: "1.000000", Currency: "USD", AttributionBasis: model.AttributionGoogleAdsClickTime},
		{Spend: "20.000000", Conversions: "2.000000", Currency: "EUR", AttributionBasis: model.AttributionGoogleAdsClickTime},
	}}
	s := fixedNowService(repo)

	res, err := s.GetCampaignMetrics(context.Background(), metricsPayload())
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if res.Summary.CurrencyUniform {
		t.Error("currency_uniform = true for USD+EUR rows, want false")
	}
	if res.Summary.Spend != nil {
		t.Errorf("spend = %q for mixed currencies, want ABSENT: a consumer ignoring the caveat must not be able to read a wrong total", *res.Summary.Spend)
	}
	if res.Summary.Currency != nil {
		t.Errorf("currency = %q for mixed currencies, want absent", *res.Summary.Currency)
	}
	// Conversions share one basis, so they remain present — the caveats are independent.
	if res.Summary.Conversions == nil {
		t.Error("conversions was omitted despite a uniform attribution basis")
	}
}

// TestSummaryOmitsConversionsAcrossBases is the API-level expression of the
// attribution caveat.
func TestSummaryOmitsConversionsAcrossBases(t *testing.T) {
	repo := &listRecordingRepo{rows: []*model.CampaignMetric{
		{Spend: "10.000000", Conversions: "1.000000", Currency: "USD", AttributionBasis: model.AttributionGoogleAdsClickTime},
		{Spend: "20.000000", Conversions: "2.000000", Currency: "USD", AttributionBasis: model.AttributionBasis("meta:7d-click-1d-view")},
	}}
	s := fixedNowService(repo)

	res, err := s.GetCampaignMetrics(context.Background(), metricsPayload())
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if res.Summary.ConversionsComparable {
		t.Error("conversions_comparable = true across bases, want false")
	}
	if res.Summary.Conversions != nil {
		t.Errorf("conversions = %q across attribution bases, want ABSENT: summing them is wrong in a way that looks plausible", *res.Summary.Conversions)
	}
	if res.Summary.AttributionBasis != nil {
		t.Errorf("attribution_basis = %q across bases, want absent", *res.Summary.AttributionBasis)
	}
	// Spend shares one currency, so it stays present.
	if res.Summary.Spend == nil || *res.Summary.Spend != "30.000000" {
		t.Error("spend was omitted despite a uniform currency")
	}
}

func TestSummaryPresentWhenComparable(t *testing.T) {
	repo := &listRecordingRepo{rows: []*model.CampaignMetric{
		{Impressions: 100, Clicks: 10, Spend: "10.500000", Conversions: "1.250000", Currency: "USD", AttributionBasis: model.AttributionGoogleAdsClickTime},
		{Impressions: 200, Clicks: 20, Spend: "20.250000", Conversions: "2.750000", Currency: "USD", AttributionBasis: model.AttributionGoogleAdsClickTime},
	}}
	s := fixedNowService(repo)

	res, err := s.GetCampaignMetrics(context.Background(), metricsPayload())
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if res.Summary.Spend == nil || *res.Summary.Spend != "30.750000" {
		t.Errorf("spend = %v, want 30.750000", res.Summary.Spend)
	}
	if res.Summary.Conversions == nil || *res.Summary.Conversions != "4.000000" {
		t.Errorf("conversions = %v, want 4.000000", res.Summary.Conversions)
	}
	if res.Summary.Impressions != 300 || res.Summary.Clicks != 30 {
		t.Errorf("impressions/clicks = %d/%d, want 300/30", res.Summary.Impressions, res.Summary.Clicks)
	}
	if res.Summary.RowCount != 2 {
		t.Errorf("row_count = %d, want 2", res.Summary.RowCount)
	}
}

// TestMetricRowFormatting checks the wire encoding of one row, including that a
// missing attribution basis is omitted rather than sent as an empty string (an empty
// string would read as a real, known basis).
func TestMetricRowFormatting(t *testing.T) {
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 7, 2, 8, 30, 0, 0, time.UTC)
	repo := &listRecordingRepo{rows: []*model.CampaignMetric{
		{
			MetricDate: day, Platform: model.ProviderGoogleAds,
			Impressions: 5, Clicks: 1, Spend: "1.000000", Conversions: "0.000000",
			FetchedAt: fetched,
		},
	}}
	s := fixedNowService(repo)

	res, err := s.GetCampaignMetrics(context.Background(), metricsPayload())
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	r := res.Metrics[0]
	if r.MetricDate != "2026-07-01" {
		t.Errorf("metric_date = %q, want 2026-07-01", r.MetricDate)
	}
	if r.Platform != string(model.ProviderGoogleAds) {
		t.Errorf("platform = %q, want %q", r.Platform, model.ProviderGoogleAds)
	}
	if r.AttributionBasis != nil {
		t.Errorf("attribution_basis = %q for an unknown basis, want absent (an empty string would read as a real basis)", *r.AttributionBasis)
	}
	if r.FetchedAt == nil || *r.FetchedAt != "2026-07-02T08:30:00Z" {
		t.Errorf("fetched_at = %v, want 2026-07-02T08:30:00Z", r.FetchedAt)
	}
}

func TestGetCampaignMetricsEmptyIsNotAnError(t *testing.T) {
	repo := &listRecordingRepo{rows: nil}
	s := fixedNowService(repo)

	res, err := s.GetCampaignMetrics(context.Background(), metricsPayload())
	if err != nil {
		t.Fatalf("a campaign with no metrics must not be an error: %v", err)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("got %d rows, want 0", len(res.Metrics))
	}
	// An empty set is not comparable — see the model's rationale.
	if res.Summary.ConversionsComparable || res.Summary.CurrencyUniform {
		t.Error("an empty summary must not claim comparability")
	}
}

func TestMetricsJWTAuthRejectsEmptyToken(t *testing.T) {
	s := NewMetricsService(nil)
	var badReq *metricsgen.BadRequestError
	if _, err := s.JWTAuth(context.Background(), "", nil); !errors.As(err, &badReq) {
		t.Errorf("err = %T (%v), want BadRequestError", err, err)
	}
}

func TestMetricsSetBackendLateBinds(t *testing.T) {
	s := NewMetricsService(nil)
	// Before late-binding: 503.
	var unavail *metricsgen.ConnServiceUnavailableError
	if _, err := s.GetCampaignMetrics(context.Background(), metricsPayload()); !errors.As(err, &unavail) {
		t.Fatalf("expected 503 before SetBackend, got %T", err)
	}
	s.SetBackend(&listRecordingRepo{})
	if _, err := s.GetCampaignMetrics(context.Background(), metricsPayload()); err != nil {
		t.Errorf("after SetBackend the route must be live, got %v", err)
	}
}
