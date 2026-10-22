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

// TestMigration000047_IsExpandOnly pins 000047's DDL the way TestMigration000041_IsExpandOnly pins
// 000041's, and for the same reason: the expand/contract rule in the migrations README is a
// property of the SQL TEXT, so it is the one thing about a migration a source-text assertion can
// genuinely prove, and it is the property a later edit is most likely to break by reflex — adding
// NOT NULL to a column that is finally being backfilled, or reaching for a DEFAULT.
//
// What is pinned: the added columns are nullable with no default (so an N-1 binary's INSERT, which
// names neither, keeps working), the migration touches creative_assets only, and the down drops
// only what the up added and never the table.
func TestMigration000047_IsExpandOnly(t *testing.T) {
	up, err := fs.ReadFile(migrations.FS, "000047_creative_assets_dimensions.up.sql")
	require.NoError(t, err)
	stmts := stripSQLComments(string(up))

	// NOT NULL / DEFAULT anywhere in the statements would make this a narrowing an N-1 writer
	// cannot satisfy. "IS NOT NULL" is stripped first: it appears inside the CHECK's whole-pair
	// predicate and is a test for a value, not a column constraint.
	assert.NotRegexp(t, regexp.MustCompile(`(?i)NOT\s+NULL|DEFAULT`),
		regexp.MustCompile(`(?i)IS\s+NOT\s+NULL`).ReplaceAllString(stmts, ""),
		"width/height must stay nullable with no default — a pre-000047 row's dimensions are unknown, and no default can be correct")

	for _, m := range regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+(\w+)`).FindAllStringSubmatch(stmts, -1) {
		assert.Contains(t, []string{"width", "height"}, m[1], "000047 adds %s, which is not a dimension column", m[1])
	}
	for _, m := range regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)`).FindAllStringSubmatch(stmts, -1) {
		assert.Equal(t, "creative_assets", m[1], "000047 must touch creative_assets only, not %s", m[1])
	}
	// No index: 000028 already settled that UNIQUE (brief_id, checksum) serves list-by-brief, and
	// neither dimension is ever a search predicate — a slot rule is a ratio with a tolerance plus
	// two minimums, which no btree on one column answers.
	assert.NotRegexp(t, regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX`), stmts,
		"000047 must not add an index; see 000028's indexing note")

	down, err := fs.ReadFile(migrations.FS, "000047_creative_assets_dimensions.down.sql")
	require.NoError(t, err)
	downStmts := stripSQLComments(string(down))
	for _, m := range regexp.MustCompile(`(?i)DROP\s+COLUMN\s+IF\s+EXISTS\s+(\w+)`).FindAllStringSubmatch(downStmts, -1) {
		assert.Contains(t, []string{"width", "height"}, m[1], "the down must drop only the dimension columns, not %s", m[1])
	}
	assert.NotRegexp(t, regexp.MustCompile(`(?i)DROP\s+TABLE`), downStmts,
		"the down must not drop creative_assets — the bytes exist nowhere else in this service")
}

// TestCreativeAssetColumnsCarryDimensions pins the two places the new columns have to agree, both
// of which fail SILENTLY rather than loudly when they drift.
//
// The scanner order is positional: creativeAssetCols lists the columns and the scanners bind
// fields in that order, so a column inserted anywhere but the matching scanner position assigns
// the wrong value to the wrong field — and width/height sit between checksum and created_by,
// where a mismatch lands two strings and a timestamp one slot out and surfaces as a scan type
// error at runtime, not at compile time.
//
// The NULLIF on the insert is the other half. The model's zero pair means "unknown" and the
// column's spelling of that is NULL; a bare $n would write a literal 0, which 000047's CHECK
// refuses outright — turning a legitimate dimensionless write into an insert failure.
func TestCreativeAssetColumnsCarryDimensions(t *testing.T) {
	assert.Contains(t, creativeAssetCols, "COALESCE(width, 0)")
	assert.Contains(t, creativeAssetCols, "COALESCE(height, 0)")
	// Position matters as much as presence: they must follow checksum and precede created_by,
	// which is the order both scanners bind.
	cols := creativeAssetCols
	assert.Less(t, strings.Index(cols, "checksum"), strings.Index(cols, "COALESCE(width, 0)"))
	assert.Less(t, strings.Index(cols, "COALESCE(height, 0)"), strings.Index(cols, "created_by"))
	// bytes stays last, appended by creativeAssetColsWithBytes, so the bytes scanner's trailing
	// field is still the trailing column.
	assert.True(t, strings.HasSuffix(creativeAssetColsWithBytes, ", bytes"))

	assert.Contains(t, createCreativeAssetQuery, "NULLIF($6::int, 0), NULLIF($7::int, 0)",
		"a zero dimension must be written as NULL, not as a literal 0 the CHECK refuses")
}
