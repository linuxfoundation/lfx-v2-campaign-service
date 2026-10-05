// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// pendingCASPredicate is the compare-and-set that both collector writes must carry.
var pendingCASPredicate = regexp.MustCompile(`(?is)\bWHERE\b.*\bpending_report_id\s*=\s*\$5\b`)

// TestAccountReportRepo_CollectorWritesAreCompareAndSet pins the port's central promise:
// completing or failing a report applies only while that report is still the key's pending
// one. Without the predicate, a request that collected an OLDER report would overwrite the
// ready half with older numbers and erase a newer submission's pending marker -- and the
// statement would still succeed, so nothing else would notice.
func TestAccountReportRepo_CollectorWritesAreCompareAndSet(t *testing.T) {
	for name, q := range map[string]string{
		"CompleteAccountReport": completeAccountReportQuery,
		"FailAccountReport":     failAccountReportQuery,
	} {
		t.Run(name, func(t *testing.T) {
			require.Regexp(t, pendingCASPredicate, q,
				"%s must gate on `pending_report_id = $5` in its WHERE clause", name)
			require.Contains(t, normalizeWS(q),
				"WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND days = $4",
				"%s must address the row by its full primary key", name)
		})
	}
}

// TestAccountReportRepo_FailNeverTouchesReady pins that a failed refresh keeps serving the
// last good report.
func TestAccountReportRepo_FailNeverTouchesReady(t *testing.T) {
	assert.NotRegexp(t, regexp.MustCompile(`(?i)\bready_\w+\s*=`), failAccountReportQuery)
}

// TestAccountReportRepo_MarkPendingNeverAssignsReady pins that a new submission leaves the
// ready half alone: the conflict arm assigns only pending_* and updated_at.
func TestAccountReportRepo_MarkPendingNeverAssignsReady(t *testing.T) {
	q := markAccountReportPendingQuery
	m := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(\s*project_id\s*,\s*platform\s*,\s*account_id\s*,\s*days\s*\)\s*DO\s+UPDATE\s+SET(.*)$`).
		FindStringSubmatch(q)
	require.NotNil(t, m, "Mark must upsert ON CONFLICT on the full primary key:\n%s", q)
	setList := m[1]
	assert.NotContains(t, strings.ToLower(setList), "ready_",
		"Mark's conflict arm must never assign a ready_ column")
	assert.NotContains(t, strings.ToLower(setList), "last_failure",
		"Mark's conflict arm must not clear the operator's failure record")
	for _, col := range []string{"pending_report_id", "pending_window_start", "pending_window_end", "pending_submitted_at"} {
		assert.Regexp(t, regexp.MustCompile(`(?i)\b`+col+`\s*=\s*EXCLUDED\.`+col+`\b`), setList,
			"Mark's conflict arm must replace %s with the new submission's value", col)
	}
	// The INSERT column list must not name ready_ columns either.
	insertCols := q[:strings.Index(q, "VALUES")]
	assert.NotContains(t, insertCols, "ready_")
}

// TestAccountReportRepo_CompleteClearsWholePendingHalf pins that Complete nulls every
// pending_ column, not just the id: the table's all-or-nothing CHECK would reject a half-
// cleared row, turning every completion into an error.
func TestAccountReportRepo_CompleteClearsWholePendingHalf(t *testing.T) {
	for name, q := range map[string]string{
		"CompleteAccountReport": completeAccountReportQuery,
		"FailAccountReport":     failAccountReportQuery,
	} {
		for _, col := range []string{"pending_report_id", "pending_window_start", "pending_window_end", "pending_submitted_at"} {
			assert.Regexp(t, regexp.MustCompile(`(?i)\b`+col+`\s*=\s*NULL\b`), q, "%s must clear %s", name, col)
		}
	}
}

func TestClipFailureReason(t *testing.T) {
	long := strings.Repeat("é", maxAccountReportFailureRunes+50)
	got := clipFailureReason(long)
	assert.Equal(t, maxAccountReportFailureRunes, utf8.RuneCountInString(got))
	assert.True(t, utf8.ValidString(got))

	assert.Equal(t, "short", clipFailureReason("short"))
	// NUL and invalid UTF-8 are rejected by PostgreSQL TEXT; they must not reach the write.
	got = clipFailureReason("bad\x00byte\xff")
	assert.NotContains(t, got, "\x00")
	assert.True(t, utf8.ValidString(got))
}

func TestReportDate_NormalisesToUTCMidnight(t *testing.T) {
	// 23:30 on the 4th in UTC-5 is the 5th in UTC; the stored date is the UTC day.
	loc := time.FixedZone("UTC-5", -5*60*60)
	got := reportDate(time.Date(2026, 10, 4, 23, 30, 0, 0, loc))
	assert.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), got)
}

func TestValidateAccountReportKey(t *testing.T) {
	ok := model.AccountReportKey{ProjectID: "p", Platform: model.ProviderMicrosoftAds, AccountID: "a", Days: domain.MonitorDaysMin}
	require.NoError(t, validateAccountReportKey(ok))

	for name, mut := range map[string]func(*model.AccountReportKey){
		"empty project":  func(k *model.AccountReportKey) { k.ProjectID = "" },
		"empty platform": func(k *model.AccountReportKey) { k.Platform = "" },
		"empty account":  func(k *model.AccountReportKey) { k.AccountID = "" },
		"days too small": func(k *model.AccountReportKey) { k.Days = domain.MonitorDaysMin - 1 },
		"days too large": func(k *model.AccountReportKey) { k.Days = domain.MonitorDaysMax + 1 },
	} {
		k := ok
		mut(&k)
		assert.Error(t, validateAccountReportKey(k), name)
	}
	k := ok
	k.Days = 6
	assert.True(t, errors.Is(validateAccountReportKey(k), domain.ErrMonitorDaysInvalid))
}

func TestValidateReportWindow(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }
	assert.NoError(t, validateReportWindow(d(1), d(1)))
	assert.NoError(t, validateReportWindow(d(1), d(5)))
	assert.Error(t, validateReportWindow(d(5), d(1)))
	assert.Error(t, validateReportWindow(time.Time{}, d(1)))
	assert.Error(t, validateReportWindow(d(1), time.Time{}))
}
