// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The keyword report statements carry the account report store's guarantees (see
// account_report_repo_test.go); these pin the same properties on the sibling table.

func TestKeywordReportRepo_CollectorWritesAreCompareAndSet(t *testing.T) {
	for name, q := range map[string]string{
		"CompleteKeywordReport": completeKeywordReportQuery,
		"FailKeywordReport":     failKeywordReportQuery,
	} {
		require.Regexp(t, pendingCASPredicate, q, "%s must gate on `pending_report_id = $5`", name)
		require.Contains(t, normalizeWS(q),
			"WHERE project_id = $1 AND platform = $2 AND account_id = $3 AND report_window = $4",
			"%s must address the row by its full primary key", name)
		for _, col := range []string{"pending_report_id", "pending_campaign_ids", "pending_window_start", "pending_window_end", "pending_submitted_at"} {
			assert.Regexp(t, regexp.MustCompile(`(?i)\b`+col+`\s*=\s*NULL\b`), q, "%s must clear %s", name, col)
		}
	}
	assert.NotRegexp(t, regexp.MustCompile(`(?i)\bready_\w+\s*=`), failKeywordReportQuery,
		"a failed refresh must keep serving the last good report")
}

func TestKeywordReportRepo_MarkPendingNeverAssignsReady(t *testing.T) {
	q := markKeywordReportPendingQuery
	m := regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(\s*project_id\s*,\s*platform\s*,\s*account_id\s*,\s*report_window\s*\)\s*DO\s+UPDATE\s+SET(.*)$`).
		FindStringSubmatch(q)
	require.NotNil(t, m, "Mark must upsert ON CONFLICT on the full primary key")
	assert.NotContains(t, strings.ToLower(m[1]), "ready_")
	assert.NotContains(t, strings.ToLower(m[1]), "last_failure")
	assert.Regexp(t, regexp.MustCompile(`(?i)WHERE\s+keyword_insight_reports\.pending_report_id\s+IS\s+NULL`), m[1],
		"the first mark must win: a pending report is never replaced by a mark")
	assert.NotContains(t, q[:strings.Index(q, "VALUES")], "ready_")
}

func TestValidateKeywordReportKey(t *testing.T) {
	ok := model.KeywordReportKey{ProjectID: "p", Platform: model.ProviderMicrosoftAds, AccountID: "a", Window: model.MetricsWindowLast30Days}
	require.NoError(t, validateKeywordReportKey(ok))
	for name, mut := range map[string]func(*model.KeywordReportKey){
		"empty project":  func(k *model.KeywordReportKey) { k.ProjectID = "" },
		"empty platform": func(k *model.KeywordReportKey) { k.Platform = "" },
		"empty account":  func(k *model.KeywordReportKey) { k.AccountID = "" },
		"bad window":     func(k *model.KeywordReportKey) { k.Window = "last_90_days" },
	} {
		k := ok
		mut(&k)
		assert.Error(t, validateKeywordReportKey(k), name)
	}
}
