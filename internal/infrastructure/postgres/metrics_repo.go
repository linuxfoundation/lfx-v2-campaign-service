// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetricsRepo is a pgx-backed implementation of domain.MetricsRepository.
type MetricsRepo struct {
	db *Pool
}

// NewMetricsRepo returns a MetricsRepo backed by pool.
func NewMetricsRepo(pool *Pool) *MetricsRepo { return &MetricsRepo{db: pool} }

var _ domain.MetricsRepository = (*MetricsRepo)(nil)

// maxMetricsPerList caps one range read. A campaign accrues one row per day, so this
// is roughly a 5-year window — far beyond any dashboard query, but it bounds the
// response for a caller that asks for an absurd range instead of streaming the whole
// table into memory.
const maxMetricsPerList = 2000

// metricsCols is the SELECT list for a metrics row.
//
// spend and conversions are cast to ::text deliberately. They are NUMERIC(18,6), and
// scanning them through a float64 would reintroduce exactly the representation error
// the NUMERIC column exists to prevent (see model.addDecimal for the measured
// failure). ::text hands back the database's own exact decimal literal, which the
// model carries as a string end to end.
const metricsCols = `m.id::text, m.campaign_id::text, m.metric_date,
	m.impressions, m.clicks, m.spend::text, m.conversions::text,
	m.currency, m.attribution_basis, m.raw, m.fetched_at`

// UpsertMetrics writes daily rows for one campaign.
//
// The ON CONFLICT clause is what makes a re-fetch idempotent. Platforms RESTATE
// recent days as conversions mature, so the sweeper deliberately re-reads a trailing
// window on every pass; without the upsert each pass would insert duplicate days and
// every downstream sum would silently inflate.
//
// All rows are written in ONE transaction: a partially-applied day range would leave
// the campaign with an inconsistent history that looks complete.
func (r *MetricsRepo) UpsertMetrics(ctx context.Context, campaignID string, rows []*model.CampaignMetric) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin metrics upsert: %w", err)
	}
	// Rollback is a no-op once the tx is committed; this guarantees no leaked tx on
	// any error path below.
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `INSERT INTO campaign_metrics
		(campaign_id, metric_date, impressions, clicks, spend, conversions,
		 currency, attribution_basis, raw, fetched_at)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, $9, now())
		ON CONFLICT (campaign_id, metric_date) DO UPDATE SET
			impressions       = EXCLUDED.impressions,
			clicks            = EXCLUDED.clicks,
			spend             = EXCLUDED.spend,
			conversions       = EXCLUDED.conversions,
			currency          = EXCLUDED.currency,
			attribution_basis = EXCLUDED.attribution_basis,
			raw               = EXCLUDED.raw,
			fetched_at        = now(),
			updated_at        = now()`

	written := 0
	for _, m := range rows {
		// Pass the decimals as STRINGS cast to ::numeric in SQL, so Postgres parses
		// the exact literal. Binding a Go float64 here would defeat the whole design.
		tag, execErr := tx.Exec(ctx, q,
			campaignID,
			m.MetricDate,
			m.Impressions,
			m.Clicks,
			defaultDecimal(m.Spend),
			defaultDecimal(m.Conversions),
			nullableText(m.Currency),
			nullableText(string(m.AttributionBasis)),
			nullableJSON(m.Raw),
		)
		if execErr != nil {
			return 0, fmt.Errorf("upsert metrics for campaign %s on %s: %w",
				campaignID, m.MetricDate.Format("2006-01-02"), execErr)
		}
		written += int(tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit metrics upsert: %w", err)
	}
	return written, nil
}

// ListMetrics returns one campaign's rows in [from, to] inclusive, ascending by date.
func (r *MetricsRepo) ListMetrics(ctx context.Context, projectID, briefID, campaignID string, from, to time.Time) ([]*model.CampaignMetric, error) {
	// Verify the campaign exists in THIS (project, brief) scope first. Without this,
	// a wrong/cross-tenant campaign id would return 200 with an empty array — making
	// "this id does not exist here" indistinguishable from "no metrics collected
	// yet", which are very different answers for an operator debugging a dashboard.
	var platform string
	err := r.db.QueryRow(ctx,
		`SELECT platform FROM campaigns WHERE id=$1 AND project_id=$2 AND brief_id=$3`,
		campaignID, projectID, briefID,
	).Scan(&platform)
	if err != nil {
		// errors.Is, not ==: a wrapped ErrNoRows would otherwise fall through to the
		// generic branch and surface as a 500 instead of the 404 the endpoint declares.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("verify parent campaign: %w", err)
	}

	q := `SELECT ` + metricsCols + ` FROM campaign_metrics m
		WHERE m.campaign_id=$1 AND m.metric_date BETWEEN $2 AND $3
		ORDER BY m.metric_date ASC
		LIMIT $4`
	rows, err := r.db.Query(ctx, q, campaignID, from, to, maxMetricsPerList)
	if err != nil {
		return nil, fmt.Errorf("list metrics: %w", err)
	}
	defer rows.Close()

	var out []*model.CampaignMetric
	for rows.Next() {
		m, sErr := scanMetric(rows)
		if sErr != nil {
			return nil, fmt.Errorf("scan metric row: %w", sErr)
		}
		// Platform is denormalised onto each row from the owning campaign: a caller
		// comparing rows needs to know who reported them, and the metrics table does
		// not repeat it.
		m.Platform = model.Provider(platform)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate metric rows: %w", err)
	}
	return out, nil
}

// ListCampaignsForMetricsSweep returns campaigns eligible for a metrics refresh.
func (r *MetricsRepo) ListCampaignsForMetricsSweep(ctx context.Context, platforms []model.Provider, limit int) ([]*model.Campaign, error) {
	if len(platforms) == 0 {
		// No platform has a wired fetcher: there is nothing to sweep. Returning early
		// avoids emitting `IN ()`, which is a syntax error.
		return nil, nil
	}
	if limit <= 0 {
		limit = 1
	}

	names := make([]string, 0, len(platforms))
	for _, p := range platforms {
		names = append(names, string(p))
	}

	// Only campaigns that actually exist upstream can report anything. A pending or
	// id-less row would produce a guaranteed-failing platform call every sweep.
	q := `SELECT id::text, project_id::text, brief_id::text, platform, platform_campaign_id,
			campaign_name, status, start_date, end_date
		FROM campaigns
		WHERE platform = ANY($1)
		  AND platform_campaign_id IS NOT NULL
		  AND platform_campaign_id <> ''
		  AND status <> 'pending'
		ORDER BY updated_at DESC
		LIMIT $2`
	rows, err := r.db.Query(ctx, q, names, limit)
	if err != nil {
		return nil, fmt.Errorf("list campaigns for metrics sweep: %w", err)
	}
	defer rows.Close()

	var out []*model.Campaign
	for rows.Next() {
		var (
			c        model.Campaign
			platform string
			upstream *string
		)
		if sErr := rows.Scan(&c.ID, &c.ProjectID, &c.BriefID, &platform, &upstream,
			&c.CampaignName, &c.Status, &c.StartDate, &c.EndDate); sErr != nil {
			return nil, fmt.Errorf("scan sweep campaign row: %w", sErr)
		}
		c.Platform = model.Provider(platform)
		if upstream != nil {
			c.PlatformCampaignID = *upstream
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sweep campaign rows: %w", err)
	}
	return out, nil
}

// scanMetric reads one campaign_metrics row in metricsCols order.
func scanMetric(row pgx.Row) (*model.CampaignMetric, error) {
	var (
		m        model.CampaignMetric
		spend    *string
		convs    *string
		currency *string
		basis    *string
		raw      []byte
	)
	if err := row.Scan(
		&m.ID, &m.CampaignID, &m.MetricDate,
		&m.Impressions, &m.Clicks, &spend, &convs,
		&currency, &basis, &raw, &m.FetchedAt,
	); err != nil {
		return nil, err
	}
	// A NULL decimal becomes the zero literal rather than "", so a consumer always
	// receives a well-formed number.
	m.Spend = derefDecimal(spend)
	m.Conversions = derefDecimal(convs)
	if currency != nil {
		m.Currency = *currency
	}
	if basis != nil {
		m.AttributionBasis = model.AttributionBasis(*basis)
	}
	m.Raw = raw
	return &m, nil
}

// defaultDecimal returns a decimal literal safe to bind to a ::numeric parameter,
// substituting zero for an empty value (an unreported metric is zero, not NULL).
func defaultDecimal(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// derefDecimal renders a scanned NUMERIC as a decimal string, mapping NULL to zero.
func derefDecimal(s *string) string {
	if s == nil {
		return "0.000000"
	}
	return *s
}

// nullableText maps an empty string to a SQL NULL, so an absent value is stored as
// "unknown" rather than as an empty-string value that would compare equal to itself
// and read as a real, known-empty datum.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableJSON maps an empty payload to a SQL NULL so the JSONB column never holds a
// zero-length value that would fail to parse on read.
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
