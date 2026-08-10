// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// audienceColumnOrder is the exact column list audienceCols selects, in order.
//
// scanAudience matches destinations by POSITION, and created_by/updated_by are both JSONB
// while created_at/updated_at are both timestamps — so inserting a column without inserting
// its destination shifts the pair onto each other and NOTHING errors. The row just comes
// back attributed to the wrong person. Same reasoning as briefColumnOrder.
var audienceColumnOrder = []string{
	"id", "project_id", "brief_id", "platform", "platform_master_list_id",
	"suppression_list_ids", "inclusion_summary", "status", "version",
	"created_by", "updated_by", "created_at", "updated_at",
}

// fakeAudienceRow drives scanAudience with a fixed, positionally ordered result set.
// Comparing audienceCols against a hand-maintained list says nothing about scanAudience,
// whose destination order is a second hand-maintained list; only running it can catch a swap.
type fakeAudienceRow struct{ vals []any }

func (r fakeAudienceRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scanAudience requested %d destinations, row has %d columns: the "+
			"destination list and audienceCols have drifted apart", len(dest), len(r.vals))
	}
	for i, d := range dest {
		if r.vals[i] == nil {
			continue // leave the destination at its zero value, as a SQL NULL would
		}
		dv := reflect.ValueOf(d).Elem()
		sv := reflect.ValueOf(r.vals[i])
		if !sv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("column %d (%s): cannot scan %s into %s — the destination at this "+
				"position does not match the column audienceCols selects there",
				i, audienceColumnOrder[i], sv.Type(), dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

// strPtrPG models a nullable text column that is NOT null; scanAudience reads those into
// *string so it can tell an absent value from an empty one.
func strPtrPG(s string) *string { return &s }

// TestScanAudience_MapsEachActorColumnToItsField gives the two actors different usernames
// and the two timestamps different instants, because that is the only thing that makes a
// same-typed destination swap observable at all.
func TestScanAudience_MapsEachActorColumnToItsField(t *testing.T) {
	createdBy, err := json.Marshal(&model.Actor{Username: "ada"})
	require.NoError(t, err)
	updatedBy, err := json.Marshal(&model.Actor{Username: "grace"})
	require.NoError(t, err)
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)

	got, err := scanAudience(fakeAudienceRow{vals: []any{
		"aud-1", "cncf", "b1", "hubspot", strPtrPG("12345"),
		[]byte(`["s1"]`), strPtrPG("past attendees"), "built", int64(3),
		[]byte(createdBy), []byte(updatedBy), createdAt, updatedAt,
	}})
	require.NoError(t, err)

	require.JSONEq(t, string(createdBy), string(got.CreatedBy),
		"created_by did not land on CreatedBy; both actor columns are JSONB, so a swap is silent")
	require.JSONEq(t, string(updatedBy), string(got.UpdatedBy),
		"updated_by did not land on UpdatedBy — the row now names the creator as the last editor")
	require.Equal(t, createdAt, got.CreatedAt)
	require.Equal(t, updatedAt, got.UpdatedAt)
}

// TestScanAudience_NullUpdatedByIsNotRecorded pins that a never-edited row reads back nil
// rather than the JSONB literal `null`. Callers distinguish "not recorded" from a value.
func TestScanAudience_NullUpdatedByIsNotRecorded(t *testing.T) {
	got, err := scanAudience(fakeAudienceRow{vals: []any{
		"aud-1", "cncf", "b1", "hubspot", nil, nil, nil, "building", int64(1),
		nil, nil, time.Time{}, time.Time{},
	}})
	require.NoError(t, err)
	require.Nil(t, got.UpdatedBy, "a SQL NULL updated_by must scan to nil, not to a JSON null")
	require.Nil(t, got.CreatedBy)
}

// TestAudienceCols_ColumnOrderMatchesScanAudience pins the select list against the constant.
func TestAudienceCols_ColumnOrderMatchesScanAudience(t *testing.T) {
	got := make([]string, 0, len(audienceColumnOrder))
	for _, c := range strings.Split(audienceCols, ",") {
		got = append(got, strings.TrimSuffix(strings.TrimSpace(c), "::text"))
	}
	require.Equal(t, audienceColumnOrder, got,
		"audienceCols changed. scanAudience scans by POSITION, so update its destination list "+
			"in the same order before updating this test — a same-typed shift (the JSONB actors, "+
			"the two timestamps) will not error, it will just attribute rows to the wrong person.")
}

// TestAudienceWrites_StampTheActorInTheSameStatement asserts each write binds its actor
// column to the EXACT placeholder carrying the actor. The invariant is not "the column
// exists" — it is that the stamp happens in the SAME statement, since a follow-up UPDATE
// leaves a committed window where the row changed and the attribution had not.
//
// Cross-check when editing: createAudienceQuery's created_by is $8 (the 8th of the
// SELECT list), updateAudienceQuery's updated_by is $5.
func TestAudienceWrites_StampTheActorInTheSameStatement(t *testing.T) {
	// createAudienceQuery is INSERT ... SELECT, not INSERT ... VALUES, so bindingFor's
	// VALUES-tuple regexp does not apply; pair the column list against the SELECT list here.
	t.Run("CreateAudience", func(t *testing.T) {
		q := writeClause(t, createAudienceQuery)
		m := regexp.MustCompile(`(?is)^INSERT INTO \w+\s*\((.*?)\)\s*SELECT\s+([^\s]+)\s+WHERE`).
			FindStringSubmatch(q)
		require.NotNil(t, m, "createAudienceQuery is no longer INSERT ... SELECT ... WHERE:\n%s", q)
		cols, vals := strings.Split(m[1], ","), strings.Split(m[2], ",")
		require.Len(t, vals, len(cols), "column list and SELECT list differ in length:\n%s", q)
		bound := map[string]string{}
		for i, c := range cols {
			bound[strings.ToLower(strings.TrimSpace(c))] = strings.TrimSpace(vals[i])
		}
		// BOTH columns take the actor: created_by names the author forever, updated_by so a
		// fresh row already answers "who touched this last" without a second column read.
		require.Equal(t, map[string]string{"created_by": "$8", "updated_by": "$8"},
			map[string]string{"created_by": bound["created_by"], "updated_by": bound["updated_by"]},
			"createAudienceQuery binds the actor columns to %v, but the actor is passed as $8. Audiences "+
				"are built through SHARED system accounts, so the platform reports one identity for "+
				"everybody — if this statement does not capture the actor, nothing does. A "+
				"NEIGHBOURING placeholder is the quiet failure: the arguments around it are strings "+
				"too, so it type-checks, commits, and reads back as a plausible actor.", bound)
	})

	t.Run("CreateAudienceForApprovedBrief", func(t *testing.T) {
		q := writeClause(t, createAudienceForApprovedBriefQuery)
		require.Equal(t, "$8", bindingFor(t, q, "created_by"))
		require.Equal(t, "$8", bindingFor(t, q, "updated_by"),
			"createAudienceForApprovedBriefQuery must bind updated_by to $8, the placeholder "+
				"carrying the actor. BuildAudience runs under a human's request, so this row IS "+
				"attributable — dropping the stamp here loses the only record of who started a "+
				"build that spends money.")
	})

	t.Run("UpdateAudience", func(t *testing.T) {
		q := writeClause(t, updateAudienceQuery)
		require.Equal(t, "$5", bindingFor(t, q, "updated_by"),
			"updateAudienceQuery must bind updated_by to $5, the placeholder carrying the editor. "+
				"NULL erases the attribution; another column records the wrong person.")
	})
}

// TestAudienceUpdate_NeverTouchesCreatedBy pins the half of the invariant that is about NOT
// writing: created_by names the original author forever, so an UPDATE that assigned it would
// make every edit look like authorship.
func TestAudienceUpdate_NeverTouchesCreatedBy(t *testing.T) {
	// Only the SET clause — created_by also appears in RETURNING, where reading it is right.
	require.NotContains(t, setClause(t, normalizeWS(updateAudienceQuery)), "created_by",
		"updateAudienceQuery assigns created_by; that column is written once, at insert.")
}

// TestMigration000018_AddsAudienceUpdatedBy pins the DDL the statements above compile
// against. Only updated_by is added: campaign_audiences has carried created_by since 000005.
func TestMigration000018_AddsAudienceUpdatedBy(t *testing.T) {
	up, err := fs.ReadFile(migrations.FS, "000018_audience_actor_columns.up.sql")
	require.NoError(t, err)
	upSQL := normalizeWS(string(up))
	require.Contains(t, upSQL, "ALTER TABLE campaign_audiences")
	require.Regexp(t, regexp.MustCompile(`(?i)ADD COLUMN IF NOT EXISTS updated_by JSONB`), upSQL,
		"000018 must add updated_by as JSONB — marshalActor stores the actor as a JSONB "+
			"document, matching briefs and connections.")
	require.NotRegexp(t, regexp.MustCompile(`(?i)UPDATE campaign_audiences SET updated_by`), upSQL,
		"000018 backfills updated_by from existing data. NULL means \"not recorded\"; filling it "+
			"from created_by would assert an edit that never happened.")

	down, err := fs.ReadFile(migrations.FS, "000018_audience_actor_columns.down.sql")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`(?i)DROP COLUMN IF EXISTS updated_by`),
		normalizeWS(string(down)),
		"down migration leaves updated_by behind, so a down-then-up cycle hits an already-"+
			"present column")
}
