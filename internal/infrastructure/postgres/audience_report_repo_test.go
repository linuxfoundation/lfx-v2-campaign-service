// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// The age/gender kind's statements carry the keyword kind's guarantees (keyword_report_repo_test.go)
// over its own columns, and — the point of the kind split — never name the other kind's.

func TestAudienceReportRepo_CollectorWritesAreCompareAndSet(t *testing.T) {
	for name, q := range map[string]string{
		"CompleteAudienceReport": completeAudienceReportQuery,
		"FailAudienceReport":     failAudienceReportQuery,
	} {
		assert.Contains(t, normalizeWS(q), "AND age_gender_pending_report_id = $5", "%s must gate on its own pending id", name)
		require.Contains(t, normalizeWS(q),
			"WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4",
			"%s must address the row by its full primary key", name)
		for _, col := range []string{"pending_report_id", "pending_campaign_ids", "pending_window_start", "pending_window_end", "pending_submitted_at"} {
			assert.Regexp(t, regexp.MustCompile(`(?i)\bage_gender_`+col+`\s*=\s*NULL\b`), q, "%s must clear age_gender_%s", name, col)
		}
	}
	assert.NotRegexp(t, regexp.MustCompile(`(?i)\bage_gender_ready_\w+\s*=`), failAudienceReportQuery,
		"a failed refresh must keep serving the last good report")
}

func TestAudienceReportRepo_MarkPendingNeverAssignsReady(t *testing.T) {
	q := markAudienceReportPendingQuery
	m := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(\s*project_id\s*,\s*platform\s*,\s*account_id\s*,\s*report_window\s*\)\s*DO\s+UPDATE\s+SET(.*)$`).
		FindStringSubmatch(q)
	require.NotNil(t, m, "Mark must upsert ON CONFLICT on the full primary key")
	assert.NotContains(t, strings.ToLower(m[1]), "ready_")
	assert.NotContains(t, strings.ToLower(m[1]), "last_failure")
	assert.Regexp(t, regexp.MustCompile(`(?i)WHERE\s+keyword_insight_reports\.age_gender_pending_report_id\s+IS\s+NULL`), m[1],
		"the first mark wins, and only THIS kind's pending report blocks it")
}

// unprefixedColumn matches a keyword-kind column name that is NOT part of an age_gender_ one.
var unprefixedColumn = regexp.MustCompile(`(?i)(^|[^_a-z])(ready_|pending_|last_failure)`)

// TestInsightReportKindsNeverShareColumns is the structural kind separation: every age/gender
// statement names only age_gender_ columns, and no keyword statement names any of them, so one
// kind can never be read, completed, failed or served as the other.
func TestInsightReportKindsNeverShareColumns(t *testing.T) {
	for name, q := range map[string]string{
		"get": getAudienceReportQuery, "mark": markAudienceReportPendingQuery,
		"complete": completeAudienceReportQuery, "fail": failAudienceReportQuery,
	} {
		// keyword_insight_reports.age_gender_pending_report_id is the conflict guard's qualified name.
		stripped := strings.ReplaceAll(q, "keyword_insight_reports.", "")
		assert.NotRegexp(t, unprefixedColumn, stripped, "audience %s names a keyword-kind column", name)
	}
	for name, q := range map[string]string{
		"get": getKeywordReportQuery, "mark": markKeywordReportPendingQuery,
		"complete": completeKeywordReportQuery, "fail": failKeywordReportQuery,
	} {
		assert.NotContains(t, q, "age_gender_", "keyword %s names an age/gender column", name)
	}
	assert.Equal(t, "keyword report", keywordReportStatements.label())
	assert.Equal(t, "age_gender report", audienceReportStatements.label())
}

// TestMigration000041_IsExpandOnly pins the DDL: nullable columns, no default, no change to the
// primary key or to any 000038 column/constraint, and a down that drops only what it added.
func TestMigration000041_IsExpandOnly(t *testing.T) {
	up, err := fs.ReadFile(migrations.FS, "000041_keyword_insight_reports_age_gender_kind.up.sql")
	require.NoError(t, err)
	stmts := stripSQLComments(string(up))
	assert.NotRegexp(t, regexp.MustCompile(`(?i)PRIMARY\s+KEY`), stmts, "000041 must not touch the primary key the N-1 upsert infers")
	assert.NotRegexp(t, regexp.MustCompile(`(?i)NOT\s+NULL|DEFAULT`), regexp.MustCompile(`(?i)IS\s+NOT\s+NULL`).ReplaceAllString(stmts, ""),
		"added columns must be nullable with no default (metadata-only)")
	for _, m := range regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+(\w+)`).FindAllStringSubmatch(stmts, -1) {
		assert.True(t, strings.HasPrefix(m[1], "age_gender_"), "000041 adds %s, outside the age_gender kind", m[1])
	}
	for _, m := range regexp.MustCompile(`(?i)DROP\s+CONSTRAINT\s+IF\s+EXISTS\s+(\w+)`).FindAllStringSubmatch(stmts, -1) {
		assert.Contains(t, m[1], "_age_gender_", "000041 may only (re)create its own constraints, not %s", m[1])
	}
	down, err := fs.ReadFile(migrations.FS, "000041_keyword_insight_reports_age_gender_kind.down.sql")
	require.NoError(t, err)
	for _, m := range regexp.MustCompile(`(?i)DROP\s+(?:COLUMN|CONSTRAINT)\s+IF\s+EXISTS\s+(\w+)`).FindAllStringSubmatch(stripSQLComments(string(down)), -1) {
		assert.Contains(t, m[1], "age_gender_", "the down must drop only the age_gender kind, not %s", m[1])
	}
	assert.NotRegexp(t, regexp.MustCompile(`(?i)DROP\s+TABLE`), string(down))
}
