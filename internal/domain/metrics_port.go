// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MetricsRepository persists per-campaign, per-day performance metrics read back
// from the ad platforms. All reads are tenant-scoped by projectID.
type MetricsRepository interface {
	// UpsertMetrics writes rows for one campaign, keyed (campaign_id, metric_date).
	// An existing row for the same day is UPDATED in place rather than duplicated,
	// which is what makes a re-fetch idempotent: platforms restate recent days as
	// conversions mature, so the sweeper deliberately re-fetches a trailing window
	// and overwrites. Returns the number of rows written.
	UpsertMetrics(ctx context.Context, campaignID string, rows []*model.CampaignMetric) (int, error)

	// ListMetrics returns one campaign's daily rows within [from, to] inclusive,
	// ascending by date, scoped to (project, brief, campaign). It returns
	// ErrNotFound when the campaign itself does not exist in that scope — as
	// distinct from a campaign that exists but has no metrics yet, which is an
	// empty slice and no error. Collapsing those two into one response would make
	// "wrong id" and "no data yet" indistinguishable to the caller.
	ListMetrics(ctx context.Context, projectID, briefID, campaignID string, from, to time.Time) ([]*model.CampaignMetric, error)

	// ListCampaignsForMetricsSweep returns campaigns eligible for a metrics refresh:
	// those with an upstream platform campaign id, on a platform whose fetcher is
	// wired. limit bounds one sweep's working set so a large estate degrades into
	// several passes rather than one unbounded query.
	ListCampaignsForMetricsSweep(ctx context.Context, platforms []model.Provider, limit int) ([]*model.Campaign, error)
}
