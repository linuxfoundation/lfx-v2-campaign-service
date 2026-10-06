// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestLiveKeywordReportRoundTrip drives KeywordReportRepo through the orchestrator's lifecycle
// against migration 000038: submit, a losing concurrent mark, a stale collector, collect, and a
// failed refresh that keeps the ready half.
func TestLiveKeywordReportRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewKeywordReportRepo(&postgres.Pool{Pool: pool})
	key := model.KeywordReportKey{
		ProjectID: dbtest.UniqueID(t, "proj"),
		Platform:  model.ProviderMicrosoftAds,
		AccountID: dbtest.UniqueID(t, "acct"),
		Window:    model.MetricsWindowLast30Days,
	}

	_, err := repo.GetKeywordReport(ctx, key)
	require.ErrorIs(t, err, domain.ErrNotFound)

	submitted := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p1 := model.PendingKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111", "222"},
		WindowStart: reportDay(2026, 9, 6), WindowEnd: reportDay(2026, 10, 5), SubmittedAt: submitted,
	}
	requireMarked(t)(repo.MarkKeywordReportPending(ctx, key, p1))

	// A concurrent read's mark loses and leaves k1 pending.
	applied, err := repo.MarkKeywordReportPending(ctx, key, model.PendingKeywordReport{
		ReportID: "k-loser", CampaignIDs: []string{"111"}, WindowStart: p1.WindowStart, WindowEnd: p1.WindowEnd, SubmittedAt: submitted,
	})
	require.NoError(t, err)
	assert.False(t, applied)

	// A stale collector holding another id writes nothing.
	applied, err = repo.CompleteKeywordReport(ctx, key, model.ReadyKeywordReport{
		ReportID: "k-stale", CampaignIDs: []string{"111"}, WindowStart: p1.WindowStart, WindowEnd: p1.WindowEnd, AsOf: submitted,
	})
	require.NoError(t, err)
	assert.False(t, applied)

	qs := int64(7)
	conv := 2.5
	ready := model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: p1.CampaignIDs, Partial: true,
		WindowStart: p1.WindowStart, WindowEnd: p1.WindowEnd, AsOf: submitted,
		Rows: []model.KeywordReportRow{
			{CampaignID: "111", AdGroupID: "501", KeywordID: "9001", Text: "kubernetes", MatchType: "Exact", Status: "Active", QualityScore: &qs, Impressions: 10, Clicks: 2, Spend: 1.25, Conversions: &conv},
			{CampaignID: "222", AdGroupID: "502", KeywordID: "9002", Text: "cloud", MatchType: "Broad", Status: "Paused", Impressions: 5},
		},
	}
	applied, err = repo.CompleteKeywordReport(ctx, key, ready)
	require.NoError(t, err)
	require.True(t, applied)

	snap, err := repo.GetKeywordReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Pending)
	require.NotNil(t, snap.Ready)
	assert.Equal(t, []string{"111", "222"}, snap.Ready.CampaignIDs)
	assert.Equal(t, ready.Rows, snap.Ready.Rows)
	assert.True(t, snap.Ready.Partial)
	assert.True(t, snap.Ready.AsOf.Equal(submitted))

	// A refresh that fails keeps serving k1.
	p2 := p1
	p2.ReportID = "k2"
	requireMarked(t)(repo.MarkKeywordReportPending(ctx, key, p2))
	applied, err = repo.FailKeywordReport(ctx, key, "k2", "the platform reported the report as failed", submitted.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, applied)
	snap, err = repo.GetKeywordReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Pending)
	require.NotNil(t, snap.Ready)
	assert.Equal(t, "k1", snap.Ready.ReportID)
	assert.Equal(t, "the platform reported the report as failed", snap.LastFailure)

	// An empty finished report is stored as [] and reads back as a non-nil empty slice.
	p3 := p1
	p3.ReportID = "k3"
	requireMarked(t)(repo.MarkKeywordReportPending(ctx, key, p3))
	empty := ready
	empty.ReportID, empty.Rows = "k3", nil
	applied, err = repo.CompleteKeywordReport(ctx, key, empty)
	require.NoError(t, err)
	require.True(t, applied)
	snap, err = repo.GetKeywordReport(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, snap.Ready.Rows)
	assert.Empty(t, snap.Ready.Rows)
}
