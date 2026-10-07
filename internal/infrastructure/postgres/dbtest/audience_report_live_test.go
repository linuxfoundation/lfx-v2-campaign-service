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

// TestLiveAudienceReportRoundTripAndKindSeparation drives the age/gender kind (migration 000041)
// through the orchestrator's lifecycle on the SAME key as a keyword report, proving the two
// kinds never see, block, complete or fail each other.
func TestLiveAudienceReportRoundTripAndKindSeparation(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewKeywordReportRepo(&postgres.Pool{Pool: pool})
	key := model.InsightReportKey{
		ProjectID: dbtest.UniqueID(t, "proj"),
		Platform:  model.ProviderMicrosoftAds,
		AccountID: dbtest.UniqueID(t, "acct"),
		Window:    model.MetricsWindowLast30Days,
	}
	_, err := repo.GetAudienceReport(ctx, key)
	require.ErrorIs(t, err, domain.ErrNotFound)

	submitted := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	window := func(id string) model.PendingInsightReport {
		return model.PendingInsightReport{ReportID: id, CampaignIDs: []string{"111", "222"},
			WindowStart: reportDay(2026, 9, 8), WindowEnd: reportDay(2026, 10, 7), SubmittedAt: submitted}
	}

	// A keyword report pending and finished on the key.
	requireMarked(t)(repo.MarkKeywordReportPending(ctx, key, window("k1")))
	applied, err := repo.CompleteKeywordReport(ctx, key, model.ReadyKeywordReport{
		ReportID: "k1", CampaignIDs: []string{"111"}, WindowStart: reportDay(2026, 9, 8), WindowEnd: reportDay(2026, 10, 7), AsOf: submitted,
		Rows: []model.KeywordReportRow{{CampaignID: "111", AdGroupID: "501", KeywordID: "9001", Text: "kubernetes", Impressions: 10}},
	})
	require.NoError(t, err)
	require.True(t, applied)
	requireMarked(t)(repo.MarkKeywordReportPending(ctx, key, window("k2")))

	// The row exists, but no age/gender report is saved on it.
	snap, err := repo.GetAudienceReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Ready, "a keyword report must never read back as an audience report")
	assert.Nil(t, snap.Pending, "a pending keyword report must not read as a pending audience report")

	// The keyword kind's pending k2 does not block the audience mark, and the keyword ids are
	// not this kind's: completing or failing "k2" here touches nothing.
	requireMarked(t)(repo.MarkAudienceReportPending(ctx, key, window("a1")))
	applied, err = repo.CompleteAudienceReport(ctx, key, model.ReadyAudienceReport{
		ReportID: "k2", CampaignIDs: []string{"111"}, WindowStart: reportDay(2026, 9, 8), WindowEnd: reportDay(2026, 10, 7), AsOf: submitted,
	})
	require.NoError(t, err)
	assert.False(t, applied, "the keyword kind's pending id must not complete an audience report")
	applied, err = repo.FailAudienceReport(ctx, key, "k2", "x", submitted)
	require.NoError(t, err)
	assert.False(t, applied)

	// A losing concurrent audience mark leaves a1 pending.
	applied, err = repo.MarkAudienceReportPending(ctx, key, window("a-loser"))
	require.NoError(t, err)
	assert.False(t, applied)

	ready := model.ReadyAudienceReport{
		ReportID: "a1", CampaignIDs: []string{"111", "222"}, Partial: true,
		WindowStart: reportDay(2026, 9, 8), WindowEnd: reportDay(2026, 10, 7), AsOf: submitted,
		Rows: []model.AudienceReportRow{
			{CampaignID: "111", AgeGroup: "25-34", Gender: "Female", Impressions: 100, Clicks: 5, Spend: 2.5},
			{CampaignID: "222", AgeGroup: "65+", Gender: "Unknown"},
		},
	}
	applied, err = repo.CompleteAudienceReport(ctx, key, ready)
	require.NoError(t, err)
	require.True(t, applied)

	snap, err = repo.GetAudienceReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Pending)
	require.NotNil(t, snap.Ready)
	assert.Equal(t, ready.Rows, snap.Ready.Rows)
	assert.Equal(t, []string{"111", "222"}, snap.Ready.CampaignIDs)
	assert.True(t, snap.Ready.Partial)
	assert.True(t, snap.Ready.AsOf.Equal(submitted))

	// The keyword kind is untouched: k1 still ready, k2 still pending, no failure recorded.
	kw, err := repo.GetKeywordReport(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, kw.Ready)
	assert.Equal(t, "k1", kw.Ready.ReportID)
	require.NotNil(t, kw.Pending)
	assert.Equal(t, "k2", kw.Pending.ReportID)
	assert.Empty(t, kw.LastFailure)

	// An audience failure keeps the audience ready half and records its own failure only.
	requireMarked(t)(repo.MarkAudienceReportPending(ctx, key, window("a2")))
	applied, err = repo.FailAudienceReport(ctx, key, "a2", "the platform reported the report as failed", submitted.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, applied)
	snap, err = repo.GetAudienceReport(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "a1", snap.Ready.ReportID)
	assert.Equal(t, "the platform reported the report as failed", snap.LastFailure)
	kw, err = repo.GetKeywordReport(ctx, key)
	require.NoError(t, err)
	assert.Empty(t, kw.LastFailure, "an audience failure must not be recorded against the keyword kind")

	// An empty finished audience report is stored as [] and reads back non-nil.
	requireMarked(t)(repo.MarkAudienceReportPending(ctx, key, window("a3")))
	empty := ready
	empty.ReportID, empty.Rows = "a3", nil
	applied, err = repo.CompleteAudienceReport(ctx, key, empty)
	require.NoError(t, err)
	require.True(t, applied)
	snap, err = repo.GetAudienceReport(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, snap.Ready.Rows)
	assert.Empty(t, snap.Ready.Rows)

	// A row created by the AUDIENCE kind first reads back to the keyword kind as nothing saved.
	key2 := key
	key2.Window = model.MetricsWindowLast7Days
	requireMarked(t)(repo.MarkAudienceReportPending(ctx, key2, window("a9")))
	kw, err = repo.GetKeywordReport(ctx, key2)
	require.NoError(t, err)
	assert.Nil(t, kw.Ready)
	assert.Nil(t, kw.Pending)
}

// TestLiveMigration000041_HalvesAreAllOrNothing proves the new CHECK constraints refuse a
// half-written age/gender half, as 000038's do for the keyword halves.
func TestLiveMigration000041_HalvesAreAllOrNothing(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	pid := dbtest.UniqueID(t, "proj")
	_, err := pool.Exec(ctx, `INSERT INTO keyword_insight_reports (project_id, platform, account_id, report_window, age_gender_pending_report_id)
		VALUES ($1, 'microsoft-ads', 'a', 'last_30_days', 'x')`, pid)
	require.Error(t, err, "a pending half with only its id must be refused")
	_, err = pool.Exec(ctx, `INSERT INTO keyword_insight_reports (project_id, platform, account_id, report_window, age_gender_ready_report_id)
		VALUES ($1, 'microsoft-ads', 'a', 'last_30_days', 'x')`, pid)
	require.Error(t, err, "a ready half with only its id must be refused")
}
