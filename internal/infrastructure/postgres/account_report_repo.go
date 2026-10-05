// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// AccountReportRepo is a pgx-backed implementation of domain.AccountReportRepository over
// account_monitor_reports (migration 000035).
type AccountReportRepo struct {
	db *Pool
}

// NewAccountReportRepo returns an AccountReportRepo backed by pool.
func NewAccountReportRepo(pool *Pool) *AccountReportRepo { return &AccountReportRepo{db: pool} }

var _ domain.AccountReportRepository = (*AccountReportRepo)(nil)

// maxAccountReportFailureRunes bounds last_failure. The reason is usually a platform error
// string, and those can embed a whole response body; the column is an operator hint, not a
// log, so a bounded prefix is all it should hold.
const maxAccountReportFailureRunes = 500

// Every statement addresses one row by the table's full primary key, in the same parameter
// order ($1..$4 = project_id, platform, account_id, days), so a key bound in the wrong slot
// would have to be wrong in all four at once.
const (
	getAccountReportQuery = `SELECT
		ready_report_id, ready_rows, ready_partial, ready_window_start, ready_window_end,
		ready_as_of,
		pending_report_id, pending_window_start, pending_window_end, pending_submitted_at,
		last_failure, last_failure_at
		FROM account_monitor_reports
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND days = $4`

	// Upsert of the PENDING half only. The conflict arm deliberately names no ready_ column:
	// a newer submission must leave the last finished report in place, because serving
	// slightly stale numbers while the next report builds is the whole point of keeping it.
	// The conflict arm applies ONLY when nothing is pending (`WHERE ... pending_report_id IS
	// NULL`). Two requests that both read "nothing pending" can both submit; an unconditional
	// overwrite let the later one silently replace the earlier, which was then never polled or
	// collected -- a Microsoft report build thrown away per extra concurrent viewer. Now the
	// first mark wins and the loser learns it (applied=false) and adopts the winner's report.
	// A pending report is only ever cleared by Complete/Fail (including the abandon path), so
	// this guard never blocks a legitimate replacement.
	markAccountReportPendingQuery = `INSERT INTO account_monitor_reports
		(project_id, platform, account_id, days,
		 pending_report_id, pending_window_start, pending_window_end, pending_submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (project_id, platform, account_id, days) DO UPDATE SET
			pending_report_id    = EXCLUDED.pending_report_id,
			pending_window_start = EXCLUDED.pending_window_start,
			pending_window_end   = EXCLUDED.pending_window_end,
			pending_submitted_at = EXCLUDED.pending_submitted_at,
			updated_at           = now()
		WHERE account_monitor_reports.pending_report_id IS NULL`

	// Promote a collected report to the ready half and clear the pending half in ONE
	// statement, gated by `pending_report_id = $5`. That predicate is the compare-and-set the
	// port promises: two requests can race to collect, and one may hold an OLDER report id
	// than the row's current pending one. Without the predicate its write would replace a
	// newer ready report with older numbers and erase the newer submission's pending marker,
	// so that report would never be collected. Zero rows affected is the "a newer submission
	// replaced it" answer, not an error.
	completeAccountReportQuery = `UPDATE account_monitor_reports SET
		ready_report_id      = $5,
		ready_rows           = $6,
		ready_partial        = $7,
		ready_window_start   = $8,
		ready_window_end     = $9,
		ready_as_of          = $10,
		pending_report_id    = NULL,
		pending_window_start = NULL,
		pending_window_end   = NULL,
		pending_submitted_at = NULL,
		updated_at           = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND days = $4
		  AND pending_report_id = $5`

	// Same compare-and-set as completeAccountReportQuery, for the same reason: a stale
	// collector reporting its old report's failure must not clear a newer pending report.
	// The ready half is not named -- a failed refresh keeps serving the last good report.
	failAccountReportQuery = `UPDATE account_monitor_reports SET
		pending_report_id    = NULL,
		pending_window_start = NULL,
		pending_window_end   = NULL,
		pending_submitted_at = NULL,
		last_failure         = $6,
		last_failure_at      = $7,
		updated_at           = now()
		WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND days = $4
		  AND pending_report_id = $5`
)

// GetAccountReport implements domain.AccountReportRepository.
func (r *AccountReportRepo) GetAccountReport(ctx context.Context, key model.AccountReportKey) (*model.AccountReportSnapshot, error) {
	if err := validateAccountReportKey(key); err != nil {
		return nil, fmt.Errorf("get account report: %w", err)
	}
	snap, err := scanAccountReport(key, r.db.QueryRow(ctx, getAccountReportQuery, accountReportKeyArgs(key)...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get account report: %w", err)
	}
	return snap, nil
}

// MarkAccountReportPending implements domain.AccountReportRepository.
func (r *AccountReportRepo) MarkAccountReportPending(ctx context.Context, key model.AccountReportKey, p model.PendingAccountReport) (bool, error) {
	if err := validateAccountReportKey(key); err != nil {
		return false, fmt.Errorf("mark account report pending: %w", err)
	}
	if p.ReportID == "" {
		return false, errors.New("mark account report pending: empty report id")
	}
	if err := validateReportWindow(p.WindowStart, p.WindowEnd); err != nil {
		return false, fmt.Errorf("mark account report pending: %w", err)
	}
	if p.SubmittedAt.IsZero() {
		return false, errors.New("mark account report pending: zero submitted-at")
	}
	args := append(accountReportKeyArgs(key), p.ReportID, reportDate(p.WindowStart), reportDate(p.WindowEnd), p.SubmittedAt)
	tag, err := r.db.Exec(ctx, markAccountReportPendingQuery, args...)
	if err != nil {
		return false, fmt.Errorf("mark account report pending: %w", err)
	}
	// 1 for a fresh insert or a conflict arm that applied; 0 when another report was already
	// pending and the guarded DO UPDATE declined.
	return tag.RowsAffected() == 1, nil
}

// CompleteAccountReport implements domain.AccountReportRepository.
func (r *AccountReportRepo) CompleteAccountReport(ctx context.Context, key model.AccountReportKey, rep model.ReadyAccountReport) (bool, error) {
	if err := validateAccountReportKey(key); err != nil {
		return false, fmt.Errorf("complete account report: %w", err)
	}
	if rep.ReportID == "" {
		return false, errors.New("complete account report: empty report id")
	}
	if err := validateReportWindow(rep.WindowStart, rep.WindowEnd); err != nil {
		return false, fmt.Errorf("complete account report: %w", err)
	}
	if rep.AsOf.IsZero() {
		return false, errors.New("complete account report: zero completed-at")
	}
	// A nil slice is stored as '[]', never as JSON null (which json.Marshal would produce).
	// A finished report with no rows is a real answer -- no campaign on the account served in
	// the window -- and it must stay distinct from "no report has finished", which is a NULL
	// ready half. The table's CHECKs refuse a null or non-array ready_rows anyway, so this is
	// also what keeps an empty report writable at all.
	rows := rep.Rows
	if rows == nil {
		rows = []model.AccountReportRow{}
	}
	rowsJSON, err := json.Marshal(rows)
	if err != nil {
		return false, fmt.Errorf("complete account report: marshal rows: %w", err)
	}
	args := append(accountReportKeyArgs(key), rep.ReportID, rowsJSON, rep.Partial,
		reportDate(rep.WindowStart), reportDate(rep.WindowEnd), rep.AsOf)
	tag, err := r.db.Exec(ctx, completeAccountReportQuery, args...)
	if err != nil {
		return false, fmt.Errorf("complete account report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailAccountReport implements domain.AccountReportRepository.
func (r *AccountReportRepo) FailAccountReport(ctx context.Context, key model.AccountReportKey, reportID, reason string, at time.Time) (bool, error) {
	if err := validateAccountReportKey(key); err != nil {
		return false, fmt.Errorf("fail account report: %w", err)
	}
	if reportID == "" {
		return false, errors.New("fail account report: empty report id")
	}
	if at.IsZero() {
		return false, errors.New("fail account report: zero failed-at")
	}
	args := append(accountReportKeyArgs(key), reportID, clipFailureReason(reason), at)
	tag, err := r.db.Exec(ctx, failAccountReportQuery, args...)
	if err != nil {
		return false, fmt.Errorf("fail account report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// accountReportKeyArgs returns the primary key as $1..$4, in the order every query above expects.
func accountReportKeyArgs(key model.AccountReportKey) []any {
	return []any{key.ProjectID, string(key.Platform), key.AccountID, key.Days}
}

// validateAccountReportKey refuses a key no row could legitimately have, before a round
// trip. The days range is enforced by the table's CHECK as well; checking it here returns
// the domain's own error rather than a constraint violation.
func validateAccountReportKey(key model.AccountReportKey) error {
	switch {
	case key.ProjectID == "":
		return errors.New("empty project id")
	case key.Platform == "":
		return errors.New("empty platform")
	case key.AccountID == "":
		return errors.New("empty account id")
	case key.Days < domain.MonitorDaysMin || key.Days > domain.MonitorDaysMax:
		return domain.ErrMonitorDaysInvalid
	}
	return nil
}

// validateReportWindow refuses a window the CHECKs would accept but no report can have: a
// zero date stores as 0001-01-01 rather than NULL, and a reversed window describes nothing.
func validateReportWindow(start, end time.Time) error {
	if start.IsZero() || end.IsZero() {
		return errors.New("zero report window bound")
	}
	if reportDate(end).Before(reportDate(start)) {
		return fmt.Errorf("report window ends (%s) before it starts (%s)",
			end.UTC().Format(time.DateOnly), start.UTC().Format(time.DateOnly))
	}
	return nil
}

// reportDate reduces t to midnight UTC of its UTC calendar day. Report windows are UTC dates
// (model.AccountReportSubmission); pgx encodes a DATE parameter from the time's OWN location,
// so a window bound carrying a non-UTC zone would otherwise land on the neighbouring day.
func reportDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// clipFailureReason makes reason storable and bounded. PostgreSQL TEXT rejects NUL bytes and
// invalid UTF-8, and a platform error string can carry either; a rejected write here would
// leave the failed report as the key's pending one, so it is sanitised rather than refused.
func clipFailureReason(reason string) string {
	s := strings.ToValidUTF8(reason, "�")
	s = strings.ReplaceAll(s, "\x00", "")
	if utf8.RuneCountInString(s) <= maxAccountReportFailureRunes {
		return s
	}
	return string([]rune(s)[:maxAccountReportFailureRunes])
}

// scanAccountReport reads getAccountReportQuery's row. Each half is reconstructed from its
// report id alone: the table's all-or-nothing CHECKs guarantee the rest of a half is present
// exactly when its id is.
func scanAccountReport(key model.AccountReportKey, row pgx.Row) (*model.AccountReportSnapshot, error) {
	var (
		readyID, pendingID, lastFailure    *string
		readyRows                          []byte
		readyPartial                       *bool
		readyStart, readyEnd, readyDone    *time.Time
		pendingStart, pendingEnd, pendSubm *time.Time
		lastFailureAt                      *time.Time
	)
	if err := row.Scan(
		&readyID, &readyRows, &readyPartial, &readyStart, &readyEnd, &readyDone,
		&pendingID, &pendingStart, &pendingEnd, &pendSubm,
		&lastFailure, &lastFailureAt,
	); err != nil {
		return nil, err
	}

	snap := &model.AccountReportSnapshot{Key: key, LastFailure: derefStr(lastFailure)}
	if lastFailureAt != nil {
		t := *lastFailureAt
		snap.LastFailureAt = &t
	}
	if readyID != nil {
		// A rows payload that does not decode is an error, not an empty report: an empty
		// slice would claim "no campaign served", and the monitor would act on that claim.
		var rows []model.AccountReportRow
		if err := json.Unmarshal(readyRows, &rows); err != nil {
			return nil, fmt.Errorf("decode ready_rows: %w", err)
		}
		snap.Ready = &model.ReadyAccountReport{
			ReportID:    *readyID,
			Rows:        rows,
			Partial:     derefAccountReportBool(readyPartial),
			WindowStart: reportDate(derefAccountReportTime(readyStart)),
			WindowEnd:   reportDate(derefAccountReportTime(readyEnd)),
			AsOf:        derefAccountReportTime(readyDone),
		}
	}
	if pendingID != nil {
		snap.Pending = &model.PendingAccountReport{
			ReportID:    *pendingID,
			WindowStart: reportDate(derefAccountReportTime(pendingStart)),
			WindowEnd:   reportDate(derefAccountReportTime(pendingEnd)),
			SubmittedAt: derefAccountReportTime(pendSubm),
		}
	}
	return snap, nil
}

func derefAccountReportBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefAccountReportTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
