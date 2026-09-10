// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
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
	"id", "project_id", "program_type", "event_slug", "delivery_type", "stage", "url", "platforms", "event_details",
	"copy", "keywords", "targeting", "status", "version", "approved_by", "approved_at",
	"created_by", "updated_by", "created_at", "updated_at",
}

// fakeRow is a pgx.Row that hands scanBrief a fixed, positionally ordered result set.
//
// Comparing briefCols against a hand-maintained list proves the SELECT is what we think it
// is; it proves NOTHING about scanBrief, whose destination list is a second hand-maintained
// order that could be permuted independently and stay green. Driving the real function is
// the only way a swapped destination fails.
type fakeRow struct{ vals []any }

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scanBrief requested %d destinations, row has %d columns: the "+
			"destination list and briefCols have drifted apart", len(dest), len(r.vals))
	}
	for i, d := range dest {
		if r.vals[i] == nil {
			continue // leave the destination at its zero value, as a SQL NULL would
		}
		dv := reflect.ValueOf(d).Elem()
		sv := reflect.ValueOf(r.vals[i])
		if !sv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("column %d (%s): cannot scan %s into %s — the destination at "+
				"this position does not match the column briefCols selects there",
				i, briefColumnOrder[i], sv.Type(), dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

// TestScanBrief_MapsEachColumnToItsField drives scanBrief with distinct values per column and
// asserts each lands on the right field.
//
// The three actor columns are all JSONB and the two timestamps are both time.Time, so a
// destination-order swap inside scanBrief cannot fail at the type level. It would simply
// return the approver as the author, or the creation time as the last edit — silently, and
// forever. Giving every actor a different username is what makes such a swap observable.
func TestScanBrief_MapsEachColumnToItsField(t *testing.T) {
	url := "https://events.example/kubecon"
	approvedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	actorJSON := func(username string) []byte {
		return []byte(`{"name":"N","email":"e@lf.dev","username":"` + username + `"}`)
	}

	b, err := scanBrief(fakeRow{vals: []any{
		"b-1", "cncf", "events", "kubecon-2026", "email", "Registration Push", &url,
		json.RawMessage(`["google_ads"]`), json.RawMessage(`{"venue":"London"}`),
		json.RawMessage(`{"headline":"h"}`), json.RawMessage(`["k8s"]`), json.RawMessage(`{"geo":"EU"}`),
		"approved", int64(7),
		actorJSON("approver"), &approvedAt,
		actorJSON("author"), actorJSON("editor"),
		createdAt, updatedAt,
	}})
	require.NoError(t, err)

	require.Equal(t, "b-1", b.ID)
	require.Equal(t, "cncf", b.ProjectID)
	require.Equal(t, model.ProgramType("events"), b.ProgramType)
	// The identity columns are asserted like every other mapped field. Feeding them into `fakeRow`
	// without checking them would let a scan regression that leaves either unset pass a test whose
	// stated purpose is that every column maps.
	require.Equal(t, model.DeliveryEmail, b.DeliveryType)
	require.Equal(t, "Registration Push", b.Stage)
	require.Equal(t, "kubecon-2026", b.EventSlug)
	require.Equal(t, url, b.URL)
	require.Equal(t, model.BriefStatus("approved"), b.Status)
	require.Equal(t, int64(7), b.Version)

	// The assertions that matter: three same-typed actor columns, each on its own field.
	require.Equal(t, "approver", b.ApprovedBy.Username, "approved_by landed on the wrong field")
	require.Equal(t, "author", b.CreatedBy.Username,
		"created_by landed on the wrong field — the row now names the wrong person as author")
	require.Equal(t, "editor", b.UpdatedBy.Username, "updated_by landed on the wrong field")
	require.Equal(t, approvedAt, *b.ApprovedAt)
	require.Equal(t, createdAt, b.CreatedAt)
	require.Equal(t, updatedAt, b.UpdatedAt, "created_at and updated_at are interchangeable by type")
}

// TestScanBrief_NullActorsAreNotRecorded covers every row written before migration 000015:
// both actor columns are NULL, and that must read back as nil rather than erroring or
// producing an all-empty Actor indistinguishable from a real one.
func TestScanBrief_NullActorsAreNotRecorded(t *testing.T) {
	b, err := scanBrief(fakeRow{vals: []any{
		"b-1", "cncf", "events", "kubecon-2026", "paid-marketing", "", nil,
		nil, nil, nil, nil, nil,
		"draft", int64(1),
		nil, nil,
		nil, nil,
		time.Time{}, time.Time{},
	}})
	require.NoError(t, err, "a pre-000015 row must still be readable")
	require.Nil(t, b.CreatedBy, "NULL created_by must be nil, not an all-empty Actor")
	require.Nil(t, b.UpdatedBy, "NULL updated_by must be nil, not an all-empty Actor")
}

// TestScanBrief_CorruptActorFails pins that bad actor JSON surfaces as an error rather than a
// silent nil, which would look exactly like "not recorded" and hide the corruption.
func TestScanBrief_CorruptActorFails(t *testing.T) {
	for _, col := range []string{"created_by", "updated_by"} {
		t.Run(col, func(t *testing.T) {
			vals := []any{
				"b-1", "cncf", "events", "kubecon-2026", "paid-marketing", "", nil,
				nil, nil, nil, nil, nil,
				"draft", int64(1),
				nil, nil,
				nil, nil,
				time.Time{}, time.Time{},
			}
			// Derived from briefColumnOrder rather than hardcoded. The literal 14/15 this replaces
			// silently pointed at the wrong slot the moment a column was inserted ahead of them --
			// which is exactly what 000030 did, moving created_by from 14 to 16.
			i := slices.Index(briefColumnOrder, col)
			require.GreaterOrEqual(t, i, 0, "%s is not in briefColumnOrder", col)
			vals[i] = []byte(`{"name":`)
			_, err := scanBrief(fakeRow{vals: vals})
			require.ErrorContains(t, err, "unmarshal "+col,
				"corrupt %s returned no error, so the caller cannot tell corruption from "+
					"an unattributed row", col)
		})
	}
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
// CreateBrief lists BOTH columns: createBriefQuery binds one placeholder to each, so a
// freshly inserted row already answers "who touched this last" without also reading
// created_by. Requiring only created_by here would let the updated_by half be deleted.
//
// Each column names the EXACT placeholder it must bind, not merely "some placeholder".
// "Some placeholder" is satisfied by binding the wrong one: createBriefQuery's actor is
// $11, and $10 is approvedBy — a one-character edit that persists the approver as both the
// author and the last editor, with every positional-only assertion still green. The index
// is the only thing tying the column to the argument the repo method actually passes, so
// it is what gets asserted. Cross-check when editing: $11 is createdBy
// (brief_repo.go:144-148), $9 is updatedBy (:181-185), $1 is approvedBy (:225), $3 is
// archivedBy (:288). Changing an argument list without changing the query here is exactly
// the regression this pins.
var actorStampedWrites = map[string]struct {
	query string
	// cols maps each actor column to the placeholder that must carry it.
	cols map[string]string
}{
	"CreateBrief":  {createBriefQuery, map[string]string{"created_by": "$13", "updated_by": "$13"}},
	"ReplaceBrief": {replaceBriefQuery, map[string]string{"updated_by": "$9"}},
	"Approve":      {approveBriefQuery, map[string]string{"updated_by": "$1"}},
	"ArchiveBrief": {archiveBriefQuery, map[string]string{"updated_by": "$3"}},
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

// insertParts splits an INSERT's column list and its VALUES tuple, both comma-separated and
// positionally paired. Returns nil, nil for a statement that is not an INSERT.
var insertParts = regexp.MustCompile(`(?is)^INSERT INTO \w+\s*\((.*?)\)\s*VALUES\s*\((.*?)\)\s*$`)

// bindingFor returns the expression an INSERT or UPDATE binds to col — `$11` for a stamped
// column, `NULL` for one explicitly erased, "" for one the statement never writes.
//
// Matching the column NAME alone is not enough. `updated_by=created_by` mentions the column
// and records nothing; an INSERT can list the column and supply NULL positionally, which no
// amount of `col=NULL` matching catches because the INSERT never uses assignment syntax. The
// invariant is that the column is bound to a PLACEHOLDER, so that is what gets extracted.
func bindingFor(t *testing.T, q, col string) string {
	t.Helper()
	if m := insertParts.FindStringSubmatch(q); m != nil {
		cols, vals := strings.Split(m[1], ","), strings.Split(m[2], ",")
		require.Len(t, vals, len(cols),
			"INSERT column list and VALUES tuple differ in length, so no column is reliably "+
				"paired with its value:\n%s", q)
		for i, c := range cols {
			if strings.EqualFold(strings.TrimSpace(c), col) {
				return strings.TrimSpace(vals[i])
			}
		}
		return ""
	}
	// UPDATE: the assignment lives between SET and WHERE.
	set := setClause(t, q)
	m := regexp.MustCompile(`(?i)\b` + col + `\s*=\s*([^,]+)`).FindStringSubmatch(set)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// setClause returns the span between SET and WHERE — the assignments an UPDATE performs.
func setClause(t *testing.T, q string) string {
	t.Helper()
	m := regexp.MustCompile(`(?is)\bSET\b(.*?)\bWHERE\b`).FindStringSubmatch(q)
	require.NotNil(t, m,
		"no SET ... WHERE found; if the statement shape changed, update this test deliberately:\n%s", q)
	return m[1]
}

// TestBriefWrites_StampTheActorInTheSameStatement asserts each write binds its actor column
// to the EXACT placeholder carrying the actor — not to NULL, not to another column, not to
// a neighbouring argument, and not by omission.
func TestBriefWrites_StampTheActorInTheSameStatement(t *testing.T) {
	for name, tc := range actorStampedWrites {
		t.Run(name, func(t *testing.T) {
			// Scope to the WRITE portion. Every one of these statements ends in
			// `RETURNING ` + briefCols, which names created_by and updated_by for the
			// read-back — so a match against the whole string succeeds even for a
			// statement that writes neither. (Verified: deleting the actor columns from
			// the INSERT left this test green until the RETURNING clause was stripped.)
			q := writeClause(t, tc.query)
			for col, want := range tc.cols {
				bound := bindingFor(t, q, col)
				require.NotEmpty(t, bound,
					"%s never writes %s, so the write commits with no record of who made it. "+
						"Campaigns run under SHARED system accounts: the ad platform reports one "+
						"identity for every person, so if this statement does not capture the "+
						"actor the information exists nowhere.", name, col)
				require.Equal(t, want, bound,
					"%s binds %s to %q, but the actor this method resolves is passed as %s. "+
						"NULL erases the attribution; a literal or another column records the "+
						"wrong person; and a NEIGHBOURING placeholder is the quiet failure — "+
						"%s's arguments are all strings, so binding the approver or the project "+
						"id here type-checks, commits, and reads back as a plausible actor.",
					name, col, bound, want, name)
			}
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
			require.NotContains(t, setClause(t, normalizeWS(q)), "created_by",
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
