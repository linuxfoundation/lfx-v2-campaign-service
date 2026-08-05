// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"sync"
	"time"

	metricsgen "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_metrics"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"

	"goa.design/goa/v3/security"
)

// metricsDateLayout is the wire format for the from/to query parameters.
const metricsDateLayout = "2006-01-02"

// defaultMetricsWindow is how far back the endpoint reads when the caller supplies no
// `from`. Thirty days covers the usual dashboard view without returning an unbounded
// history by default.
const defaultMetricsWindow = 30 * 24 * time.Hour

// maxMetricsWindow caps the requested range. Without it a caller could ask for a
// decade and force a large scan; the repo also caps rows, but rejecting an absurd
// window is a clearer error than a silently truncated result.
const maxMetricsWindow = 400 * 24 * time.Hour

// MetricsService implements the generated metrics service, delegating to the metrics
// repository.
type MetricsService struct {
	mu   sync.RWMutex
	repo domain.MetricsRepository
	// now is injectable so tests can pin the default window.
	now func() time.Time
}

var (
	_ metricsgen.Service = (*MetricsService)(nil)
	_ metricsgen.Auther  = (*MetricsService)(nil)
)

// NewMetricsService constructs a MetricsService. A nil repo mounts the routes in the
// typed-503 (unavailable) mode, matching the brief/audience services.
func NewMetricsService(repo domain.MetricsRepository) *MetricsService {
	return &MetricsService{repo: repo, now: time.Now}
}

// SetBackend late-binds the repo after a cold-start DB retry (guarded by the RWMutex;
// handlers snapshot via ready() so a mid-request swap cannot race).
func (s *MetricsService) SetBackend(repo domain.MetricsRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo = repo
}

// ready returns the repo or a typed 503 when the database is not wired yet.
func (s *MetricsService) ready() (domain.MetricsRepository, error) {
	s.mu.RLock()
	repo := s.repo
	s.mu.RUnlock()
	if repo == nil {
		return nil, &metricsgen.ConnServiceUnavailableError{Code: "503", Message: "metrics storage is unavailable"}
	}
	return repo, nil
}

// JWTAuth records the authenticated actor (validated by Heimdall at the gateway) into
// the context, mirroring the sibling services.
func (s *MetricsService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, &metricsgen.BadRequestError{Code: "400", Message: "missing bearer token"}
	}
	if a := actorFromToken(token); a != nil {
		ctx = context.WithValue(ctx, actorCtxKey{}, a)
	}
	return ctx, nil
}

// GetCampaignMetrics returns a campaign's stored daily rows plus a derived summary.
func (s *MetricsService) GetCampaignMetrics(ctx context.Context, p *metricsgen.GetCampaignMetricsPayload) (*metricsgen.GetCampaignMetricsResult, error) {
	repo, err := s.ready()
	if err != nil {
		return nil, err
	}

	from, to, werr := s.resolveWindow(p.From, p.To)
	if werr != nil {
		return nil, werr
	}

	rows, lerr := repo.ListMetrics(ctx, p.ProjectID, p.BriefID, p.CampaignID, from, to)
	if lerr != nil {
		return nil, mapMetricsErr(lerr)
	}

	out := make([]*metricsgen.CampaignMetric, 0, len(rows))
	for _, m := range rows {
		out = append(out, metricResult(m))
	}
	return &metricsgen.GetCampaignMetricsResult{
		Metrics: out,
		Summary: summaryResult(model.SummariseMetrics(rows)),
	}, nil
}

// resolveWindow parses the optional from/to parameters and applies defaults.
func (s *MetricsService) resolveWindow(fromStr, toStr *string) (time.Time, time.Time, error) {
	now := s.now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if toStr != nil {
		t, err := time.Parse(metricsDateLayout, *toStr)
		if err != nil {
			return time.Time{}, time.Time{}, &metricsgen.BadRequestError{
				Code: "400", Message: "`to` must be a YYYY-MM-DD date",
			}
		}
		to = t
	}

	from := to.Add(-defaultMetricsWindow)
	if fromStr != nil {
		f, err := time.Parse(metricsDateLayout, *fromStr)
		if err != nil {
			return time.Time{}, time.Time{}, &metricsgen.BadRequestError{
				Code: "400", Message: "`from` must be a YYYY-MM-DD date",
			}
		}
		from = f
	}

	// An inverted range must be rejected, not silently returned as empty: BETWEEN
	// with the bounds reversed matches nothing, which a caller would read as "this
	// campaign had no activity" rather than "your request was malformed".
	if from.After(to) {
		return time.Time{}, time.Time{}, &metricsgen.BadRequestError{
			Code: "400", Message: "`from` must not be after `to`",
		}
	}
	if to.Sub(from) > maxMetricsWindow {
		return time.Time{}, time.Time{}, &metricsgen.BadRequestError{
			Code: "400", Message: "the requested window is too large; narrow `from`/`to`",
		}
	}
	return from, to, nil
}

// metricResult maps a stored row to the wire type.
func metricResult(m *model.CampaignMetric) *metricsgen.CampaignMetric {
	r := &metricsgen.CampaignMetric{
		MetricDate:  m.MetricDate.UTC().Format(metricsDateLayout),
		Platform:    string(m.Platform),
		Impressions: m.Impressions,
		Clicks:      m.Clicks,
		Spend:       m.Spend,
		Conversions: m.Conversions,
	}
	if m.Currency != "" {
		c := m.Currency
		r.Currency = &c
	}
	if m.AttributionBasis != model.AttributionUnknown {
		b := string(m.AttributionBasis)
		r.AttributionBasis = &b
	}
	if !m.FetchedAt.IsZero() {
		f := m.FetchedAt.UTC().Format(time.RFC3339)
		r.FetchedAt = &f
	}
	return r
}

// summaryResult maps the derived summary to the wire type.
//
// The omissions here are the whole point of the type. When a total is not comparable
// its field is left NIL, so it is absent from the JSON entirely rather than present
// with a caveat a consumer can ignore. A dashboard cannot accidentally render a
// cross-currency spend total or a cross-basis conversion total, because the number
// simply is not there.
func summaryResult(s model.MetricsSummary) *metricsgen.MetricsSummary {
	out := &metricsgen.MetricsSummary{
		Impressions:           s.Impressions,
		Clicks:                s.Clicks,
		CurrencyUniform:       s.CurrencyUniform,
		ConversionsComparable: s.ConversionsComparable,
		RowCount:              s.RowCount,
	}
	if s.CurrencyUniform && s.Spend != "" {
		spend := s.Spend
		out.Spend = &spend
		if s.Currency != "" {
			cur := s.Currency
			out.Currency = &cur
		}
	}
	if s.ConversionsComparable && s.Conversions != "" {
		conv := s.Conversions
		out.Conversions = &conv
		if s.AttributionBasis != model.AttributionUnknown {
			basis := string(s.AttributionBasis)
			out.AttributionBasis = &basis
		}
	}
	return out
}

// mapMetricsErr maps domain errors to the generated typed errors.
func mapMetricsErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return &metricsgen.NotFoundError{Code: "404", Message: "the campaign or its parent brief was not found"}
	case errors.Is(err, domain.ErrMetricsUnsupported):
		return &metricsgen.BadRequestError{Code: "400", Message: "metrics are not supported for this campaign's platform yet"}
	}
	// Pass an already-typed error through unchanged. Without this block a typed 503
	// raised deeper in the stack would be flattened into a generic 500, losing the
	// distinction between "the database is unavailable" and "something broke".
	var (
		unavail  *metricsgen.ConnServiceUnavailableError
		badReq   *metricsgen.BadRequestError
		notFound *metricsgen.NotFoundError
		conflict *metricsgen.ConflictError
	)
	switch {
	case errors.As(err, &unavail), errors.As(err, &badReq),
		errors.As(err, &notFound), errors.As(err, &conflict):
		return err
	default:
		return &metricsgen.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
}
