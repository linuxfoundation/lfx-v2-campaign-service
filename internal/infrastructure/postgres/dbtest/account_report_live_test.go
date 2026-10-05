// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

func accountReportKey(t *testing.T, days int) model.AccountReportKey {
	return model.AccountReportKey{
		ProjectID: dbtest.UniqueID(t, "proj"),
		Platform:  model.ProviderMicrosoftAds,
		AccountID: dbtest.UniqueID(t, "acct"),
		Days:      days,
	}
}

func reportDay(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// TestLiveAccountReportRoundTrip drives every method of AccountReportRepo through the
// lifecycle the orchestrator runs: submit, collect, resubmit while serving, race a stale
// collector, fail. Each step asserts both halves, because the defect this store exists to
// prevent is one half's write disturbing the other.
func TestLiveAccountReportRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewAccountReportRepo(&postgres.Pool{Pool: pool})
	key := accountReportKey(t, 30)

	// Nothing saved yet.
	_, err := repo.GetAccountReport(ctx, key)
	require.ErrorIs(t, err, domain.ErrNotFound)

	// Submit R1 -> pending only. The window start carries a non-UTC zone late in the day to
	// pin that the stored DATE is the UTC calendar day.
	submitted := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	p1 := model.PendingAccountReport{
		ReportID:    "r1",
		WindowStart: time.Date(2026, 9, 5, 22, 0, 0, 0, time.FixedZone("UTC-5", -5*3600)), // 2026-09-06 UTC
		WindowEnd:   reportDay(2026, 10, 5),
		SubmittedAt: submitted,
	}
	require.NoError(t, repo.MarkAccountReportPending(ctx, key, p1))
	snap, err := repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, key, snap.Key)
	assert.Nil(t, snap.Ready)
	require.NotNil(t, snap.Pending)
	assert.Equal(t, "r1", snap.Pending.ReportID)
	assert.Equal(t, reportDay(2026, 9, 6), snap.Pending.WindowStart)
	assert.Equal(t, time.UTC, snap.Pending.WindowStart.Location())
	assert.Equal(t, reportDay(2026, 10, 5), snap.Pending.WindowEnd)
	assert.True(t, snap.Pending.SubmittedAt.Equal(submitted))
	assert.Empty(t, snap.LastFailure)
	assert.Nil(t, snap.LastFailureAt)

	// Complete R1 -> ready populated, pending cleared.
	half := 0.5
	completed := submitted.Add(5 * time.Minute)
	r1 := model.ReadyAccountReport{
		ReportID: "r1",
		Rows: []model.AccountReportRow{
			{PlatformCampaignID: "c-nil", Spend: 12.5, Impressions: 1000, Clicks: 10, Conversions: nil},
			{PlatformCampaignID: "c-half", Spend: 3.25, Impressions: 200, Clicks: 2, Conversions: &half},
		},
		Partial:     true,
		WindowStart: reportDay(2026, 9, 6),
		WindowEnd:   reportDay(2026, 10, 5),
		CompletedAt: completed,
	}
	applied, err := repo.CompleteAccountReport(ctx, key, r1)
	require.NoError(t, err)
	require.True(t, applied, "completing the current pending report must apply")
	snap, err = repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Pending, "Complete must clear the pending half")
	require.NotNil(t, snap.Ready)
	assert.Equal(t, "r1", snap.Ready.ReportID)
	assert.True(t, snap.Ready.Partial)
	assert.Equal(t, reportDay(2026, 9, 6), snap.Ready.WindowStart)
	assert.Equal(t, reportDay(2026, 10, 5), snap.Ready.WindowEnd)
	assert.True(t, snap.Ready.CompletedAt.Equal(completed))
	require.Len(t, snap.Ready.Rows, 2)
	assert.Equal(t, "c-nil", snap.Ready.Rows[0].PlatformCampaignID)
	assert.Nil(t, snap.Ready.Rows[0].Conversions, "an unreported conversion count must read back nil, not 0")
	assert.Equal(t, int64(1000), snap.Ready.Rows[0].Impressions)
	require.NotNil(t, snap.Ready.Rows[1].Conversions)
	assert.InDelta(t, 0.5, *snap.Ready.Rows[1].Conversions, 0)
	assert.InDelta(t, 3.25, snap.Ready.Rows[1].Spend, 0)

	// Complete with a stale id -> not applied, nothing changed.
	before := snap
	applied, err = repo.CompleteAccountReport(ctx, key, model.ReadyAccountReport{
		ReportID: "r0-stale", WindowStart: reportDay(2026, 9, 1), WindowEnd: reportDay(2026, 9, 30), CompletedAt: completed,
	})
	require.NoError(t, err)
	assert.False(t, applied, "a stale collector must not apply")
	snap, err = repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, before, snap, "a stale Complete must change nothing")

	// Submit R2 while R1 is ready -> ready preserved, pending R2.
	p2 := model.PendingAccountReport{ReportID: "r2", WindowStart: reportDay(2026, 9, 7), WindowEnd: reportDay(2026, 10, 6), SubmittedAt: submitted.Add(time.Hour)}
	require.NoError(t, repo.MarkAccountReportPending(ctx, key, p2))
	snap, err = repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, before.Ready, snap.Ready, "a new submission must leave the ready half untouched")
	require.NotNil(t, snap.Pending)
	assert.Equal(t, "r2", snap.Pending.ReportID)

	// A late collector of R1 must not clear R2's marker either.
	applied, err = repo.CompleteAccountReport(ctx, key, r1)
	require.NoError(t, err)
	assert.False(t, applied, "re-completing an already-collected report must not apply")

	// Fail with a stale id -> not applied.
	failedAt := submitted.Add(2 * time.Hour)
	applied, err = repo.FailAccountReport(ctx, key, "r1", "stale", failedAt)
	require.NoError(t, err)
	assert.False(t, applied)
	snap, err = repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, snap.Pending)
	assert.Equal(t, "r2", snap.Pending.ReportID)
	assert.Empty(t, snap.LastFailure)

	// Fail with the current id -> pending cleared, ready preserved, failure recorded.
	applied, err = repo.FailAccountReport(ctx, key, "r2", "platform reported Error", failedAt)
	require.NoError(t, err)
	assert.True(t, applied)
	snap, err = repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, snap.Pending)
	assert.Equal(t, before.Ready, snap.Ready, "a failed refresh must keep serving the last good report")
	assert.Equal(t, "platform reported Error", snap.LastFailure)
	require.NotNil(t, snap.LastFailureAt)
	assert.True(t, snap.LastFailureAt.Equal(failedAt))

	// Complete on a row with no pending report -> not applied.
	applied, err = repo.CompleteAccountReport(ctx, key, r1)
	require.NoError(t, err)
	assert.False(t, applied)
}

// TestLiveAccountReportZeroRowsStoresEmptyArray pins that a finished report with no rows is
// stored as '[]' and reads back as an EMPTY, NON-NIL slice under a non-nil Ready: "nothing
// served" is a real answer, distinct from "no report has finished" (Ready == nil).
func TestLiveAccountReportZeroRowsStoresEmptyArray(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewAccountReportRepo(&postgres.Pool{Pool: pool})
	key := accountReportKey(t, 7)

	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	require.NoError(t, repo.MarkAccountReportPending(ctx, key, model.PendingAccountReport{
		ReportID: "z1", WindowStart: reportDay(2026, 9, 29), WindowEnd: reportDay(2026, 10, 5), SubmittedAt: at,
	}))
	applied, err := repo.CompleteAccountReport(ctx, key, model.ReadyAccountReport{
		ReportID: "z1", Rows: nil, WindowStart: reportDay(2026, 9, 29), WindowEnd: reportDay(2026, 10, 5), CompletedAt: at,
	})
	require.NoError(t, err)
	require.True(t, applied)

	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT ready_rows::text FROM account_monitor_reports
		 WHERE project_id=$1 AND platform=$2 AND account_id=$3 AND days=$4`,
		key.ProjectID, string(key.Platform), key.AccountID, key.Days).Scan(&stored))
	assert.Equal(t, "[]", stored, "a nil Rows slice must be stored as an empty array, not JSON null")

	snap, err := repo.GetAccountReport(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, snap.Ready)
	assert.NotNil(t, snap.Ready.Rows, "an empty report reads back as an empty non-nil slice")
	assert.Empty(t, snap.Ready.Rows)
	assert.False(t, snap.Ready.Partial)
}

// TestLiveAccountReportChecksRejectMalformedRows proves the table's CHECKs hold for a writer
// that is not this repository.
func TestLiveAccountReportChecksRejectMalformedRows(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)

	insert := func(t *testing.T, days int, extraCols, extraVals string, args ...any) error {
		t.Helper()
		base := []any{dbtest.UniqueID(t, "proj"), "microsoft-ads", dbtest.UniqueID(t, "acct"), days}
		_, err := pool.Exec(ctx, `INSERT INTO account_monitor_reports (project_id, platform, account_id, days`+extraCols+`)
			VALUES ($1, $2, $3, $4`+extraVals+`)`, append(base, args...)...)
		return err
	}
	requireCheckViolation := func(t *testing.T, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr), "expected a PostgreSQL error, got %v", err)
		assert.Equal(t, pgerrcodeCheckViolation, pgErr.Code, "expected a CHECK violation, got %s: %s", pgErr.Code, pgErr.Message)
	}

	t.Run("days below the monitor minimum", func(t *testing.T) {
		requireCheckViolation(t, insert(t, 6, "", ""))
	})
	t.Run("days above the monitor maximum", func(t *testing.T) {
		requireCheckViolation(t, insert(t, 91, "", ""))
	})
	t.Run("days at the bounds are accepted", func(t *testing.T) {
		require.NoError(t, insert(t, domain.MonitorDaysMin, "", ""))
		require.NoError(t, insert(t, domain.MonitorDaysMax, "", ""))
	})
	t.Run("half-filled pending", func(t *testing.T) {
		requireCheckViolation(t, insert(t, 30, ", pending_report_id", ", $5", "only-the-id"))
	})
	t.Run("half-filled ready", func(t *testing.T) {
		requireCheckViolation(t, insert(t, 30, ", ready_report_id, ready_rows", ", $5, '[]'::jsonb", "id-and-rows"))
	})
	t.Run("ready_rows not an array", func(t *testing.T) {
		requireCheckViolation(t, insert(t, 30,
			", ready_report_id, ready_rows, ready_partial, ready_window_start, ready_window_end, ready_completed_at",
			", $5, '{}'::jsonb, false, DATE '2026-09-01', DATE '2026-09-30', now()", "obj-rows"))
	})
}

// TestLiveAccountReportRejectsEmptyReportID pins the cheap input guard: an empty report id
// could never be completed (the CAS compares against it), so it is refused before the write.
func TestLiveAccountReportRejectsEmptyReportID(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := postgres.NewAccountReportRepo(&postgres.Pool{Pool: pool})
	key := accountReportKey(t, 14)
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

	require.Error(t, repo.MarkAccountReportPending(ctx, key, model.PendingAccountReport{
		WindowStart: reportDay(2026, 9, 22), WindowEnd: reportDay(2026, 10, 5), SubmittedAt: at,
	}))
	_, err := repo.CompleteAccountReport(ctx, key, model.ReadyAccountReport{
		WindowStart: reportDay(2026, 9, 22), WindowEnd: reportDay(2026, 10, 5), CompletedAt: at,
	})
	require.Error(t, err)
	_, err = repo.FailAccountReport(ctx, key, "", "x", at)
	require.Error(t, err)
	_, err = repo.GetAccountReport(ctx, key)
	require.ErrorIs(t, err, domain.ErrNotFound, "rejected writes must not have created a row")
}
