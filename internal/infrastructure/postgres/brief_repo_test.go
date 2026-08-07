// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// briefColumnOrder is the exact column list briefCols selects, in order.
//
// scanBrief passes a positional destination per column, and pgx matches them by POSITION,
// not by name. Inserting a column into briefCols without inserting the matching destination
// shifts every later column onto the wrong field — and where the shifted pair share a type
// (approved_by/created_by/updated_by are all JSONB; created_at/updated_at both timestamps)
// nothing errors at runtime either. The row simply comes back attributed to the wrong person.
// Pinning the order here makes that edit fail a test instead of silently corrupting an audit
// trail; the fix is to change BOTH lists and then this constant, deliberately.
var briefColumnOrder = []string{
	"id", "project_id", "program_type", "event_slug", "url", "platforms", "event_details",
	"copy", "keywords", "targeting", "status", "version", "approved_by", "approved_at",
	"created_by", "updated_by", "created_at", "updated_at",
}

// TestBriefCols_ColumnOrderMatchesScanBrief pins the select list against briefColumnOrder.
func TestBriefCols_ColumnOrderMatchesScanBrief(t *testing.T) {
	got := make([]string, 0, len(briefColumnOrder))
	for _, c := range strings.Split(briefCols, ",") {
		// briefCols casts the two uuid columns with ::text so they scan into string.
		got = append(got, strings.TrimSuffix(strings.TrimSpace(c), "::text"))
	}
	require.Equal(t, briefColumnOrder, got,
		"briefCols changed. scanBrief scans by POSITION, so update its destination list in the "+
			"same order before updating this test — a same-typed shift (JSONB actors, timestamps) "+
			"will not error, it will just attribute rows to the wrong person.")
}

// actorStampedWrites names, per write statement, the actor column that statement MUST set.
//
// The invariant is not merely "the column exists" — it is that the stamp happens in the SAME
// statement as the write. A follow-up UPDATE compiles, passes every other test, and leaves a
// committed window in which the row changed and the attribution had not; a crash inside that
// window loses the actor for a change that stuck. Since these statements are the only place
// that can be checked without a live database, they are checked here.
//
// Approve appears with updated_by rather than approved_by deliberately: approved_by was
// already stamped before actor attribution existed, and ReplaceBrief CLEARS it. Only
// updated_by survives an edit to answer "who touched this row last".
var actorStampedWrites = map[string]struct {
	query string
	col   string
}{
	"CreateBrief":  {createBriefQuery, "created_by"},
	"ReplaceBrief": {replaceBriefQuery, "updated_by"},
	"Approve":      {approveBriefQuery, "updated_by"},
	"ArchiveBrief": {archiveBriefQuery, "updated_by"},
}

// writeClause returns the statement with its RETURNING clause removed, so assertions about
// what a statement WRITES cannot be satisfied by what it READS BACK.
func writeClause(t *testing.T, q string) string {
	t.Helper()
	n := normalizeWS(q)
	i := strings.Index(strings.ToUpper(n), " RETURNING ")
	require.NotEqual(t, -1, i,
		"statement has no RETURNING clause; every brief write returns its committed row so the "+
			"outbox payload is exactly what committed:\n%s", q)
	return n[:i]
}

// TestBriefWrites_StampTheActorInTheSameStatement asserts each write binds its actor column
// to a placeholder — not to NULL, and not by omission.
func TestBriefWrites_StampTheActorInTheSameStatement(t *testing.T) {
	for name, tc := range actorStampedWrites {
		t.Run(name, func(t *testing.T) {
			// Scope to the WRITE portion. Every one of these statements ends in
			// `RETURNING ` + briefCols, which names created_by and updated_by for the
			// read-back — so a match against the whole string succeeds even for a
			// statement that writes neither. (Verified: deleting the actor columns from
			// the INSERT left this test green until the RETURNING clause was stripped.)
			q := writeClause(t, tc.query)
			// An UPDATE assigns `col=$n`; the INSERT names the column in its list and supplies
			// a placeholder positionally. Both forms must reference the column by name.
			require.Regexp(t, regexp.MustCompile(`(?i)\b`+tc.col+`\b`), q,
				"%s never mentions %s, so the write commits with no record of who made it. "+
					"Campaigns run under SHARED system accounts: the ad platform reports one "+
					"identity for every person, so if this statement does not capture the actor "+
					"the information exists nowhere.", name, tc.col)
			require.NotRegexp(t, regexp.MustCompile(`(?i)\b`+tc.col+`\s*=\s*NULL\b`), q,
				"%s sets %s to NULL, which erases the attribution it is supposed to record.", name, tc.col)
		})
	}
}

// TestBriefWrites_UpdatesNeverTouchCreatedBy pins the half of the invariant that is about
// NOT writing: created_by names the original author forever. An UPDATE that assigned it
// would rewrite history on every edit, and the row would then claim the last editor wrote it.
func TestBriefWrites_UpdatesNeverTouchCreatedBy(t *testing.T) {
	for name, q := range map[string]string{
		"ReplaceBrief": replaceBriefQuery,
		"Approve":      approveBriefQuery,
		"ArchiveBrief": archiveBriefQuery,
	} {
		t.Run(name, func(t *testing.T) {
			// Only the SET clause matters — created_by also appears in the RETURNING list,
			// where reading it is exactly right.
			m := regexp.MustCompile(`(?is)\bSET\b(.*?)\bWHERE\b`).FindStringSubmatch(normalizeWS(q))
			require.NotNil(t, m, "no SET ... WHERE found; if the statement shape changed, update this test deliberately:\n%s", q)
			require.NotContains(t, m[1], "created_by",
				"%s assigns created_by. That column is written once, at insert, and must keep "+
					"naming the original author; an UPDATE makes every edit look like authorship.", name)
		})
	}
}

// TestMigration000015_AddsBriefActorColumns pins the DDL that backs all of the above. The
// statements compile against columns this migration is the only thing that creates, and a
// migration edited to add just one of the pair fails at runtime on the other.
func TestMigration000015_AddsBriefActorColumns(t *testing.T) {
	up, err := fs.ReadFile(migrations.FS, "000015_brief_actor_columns.up.sql")
	require.NoError(t, err)
	upSQL := normalizeWS(string(up))

	require.Contains(t, upSQL, "ALTER TABLE campaign_briefs",
		"migration 000015 must alter campaign_briefs; campaigns gets its own migration")
	for _, col := range []string{"created_by", "updated_by"} {
		require.Regexp(t, regexp.MustCompile(`(?i)ADD COLUMN IF NOT EXISTS `+col+` JSONB`), upSQL,
			"000015 does not add %s as JSONB. The actor is stored as a JSONB document by "+
				"marshalActor, matching connections; a text column would scan into []byte and "+
				"unmarshal, but would lose the ability to query into the actor.", col)
	}

	down, err := fs.ReadFile(migrations.FS, "000015_brief_actor_columns.down.sql")
	require.NoError(t, err)
	downSQL := normalizeWS(string(down))
	for _, col := range []string{"created_by", "updated_by"} {
		require.Regexp(t, regexp.MustCompile(`(?i)DROP COLUMN IF EXISTS `+col), downSQL,
			"down migration leaves %s behind, so a down-then-up cycle hits an already-present "+
				"column and the pair drift apart", col)
	}
}
