// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// wizardSessionColumnOrder is the exact column list wizardSessionCols selects, in order.
//
// Same hazard as briefColumnOrder, and worse here: five of these columns are JSONB
// (plan_result, reference_variant, stage_variant, sections, chat_history) and two more are
// the JSONB actors, so a positional shift inside scanWizardSession cannot fail at the type
// level. It would return the reference variant's copy as the stage variant's — two bodies of
// generated marketing text swapped, each valid JSON, with nothing erroring. The A/B variants
// are then cloned into HubSpot under each other's labels.
var wizardSessionColumnOrder = []string{
	"id", "project_id", "brief_id", "phase", "progress_token",
	"plan_result", "reference_variant", "stage_variant", "sections", "email_id", "draft_url",
	"chat_history", "version", "created_by", "updated_by", "created_at", "updated_at",
}

// wizardFakeRow is a pgx.Row handing scanWizardSession a fixed, positionally ordered result.
// It drives the REAL function, which is the only way a swapped destination fails: comparing
// the SELECT list against a hand-maintained slice proves nothing about the scan's own
// hand-maintained order.
type wizardFakeRow struct{ vals []any }

func (r wizardFakeRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scanWizardSession requested %d destinations, row has %d columns: "+
			"the destination list and wizardSessionCols have drifted apart", len(dest), len(r.vals))
	}
	for i, d := range dest {
		if r.vals[i] == nil {
			continue // leave the destination at its zero value, as a SQL NULL would
		}
		dv := reflect.ValueOf(d).Elem()
		sv := reflect.ValueOf(r.vals[i])
		if !sv.Type().AssignableTo(dv.Type()) {
			return fmt.Errorf("column %d (%s): cannot scan %s into %s — the destination at this "+
				"position does not match the column wizardSessionCols selects there",
				i, wizardSessionColumnOrder[i], sv.Type(), dv.Type())
		}
		dv.Set(sv)
	}
	return nil
}

func wizardRowVals() []any {
	str := func(s string) *string { return &s }
	return []any{
		"s-1", "cncf", "b-1", "content", str("tok-1"),
		[]byte(`{"phase":"plan"}`), []byte(`{"subject":"reference"}`), []byte(`{"subject":"stage"}`),
		[]byte(`[{"id":"hero"}]`), str("email-9"), str("https://app.hubspot.com/draft/9"),
		[]byte(`[{"role":"user"}]`), int64(4),
		[]byte(`{"name":"N","email":"e@lf.dev","username":"author"}`),
		[]byte(`{"name":"N","email":"e@lf.dev","username":"editor"}`),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC),
	}
}

// TestScanWizardSession_MapsEachColumnToItsField gives every same-typed column a DISTINCT
// value, so a destination-order swap is observable rather than merely type-compatible.
func TestScanWizardSession_MapsEachColumnToItsField(t *testing.T) {
	s, err := scanWizardSession(wizardFakeRow{vals: wizardRowVals()})
	require.NoError(t, err)

	require.Equal(t, "s-1", s.ID)
	require.Equal(t, "cncf", s.ProjectID)
	require.Equal(t, "b-1", s.BriefID)
	require.Equal(t, model.WizardPhaseContent, s.Phase)
	require.Equal(t, "tok-1", s.ProgressToken)
	require.Equal(t, "email-9", s.EmailID)
	require.Equal(t, "https://app.hubspot.com/draft/9", s.DraftURL)
	require.Equal(t, int64(4), s.Version)

	// The five JSONB payload columns are the assertion that matters: all one type, and two
	// of them hold the A and B variants of the same email.
	require.JSONEq(t, `{"phase":"plan"}`, string(s.PlanResult))
	require.JSONEq(t, `{"subject":"reference"}`, string(s.ReferenceVariant),
		"reference_variant landed on the wrong field — the two variants are interchangeable by type")
	require.JSONEq(t, `{"subject":"stage"}`, string(s.StageVariant))
	require.JSONEq(t, `[{"id":"hero"}]`, string(s.Sections))
	require.JSONEq(t, `[{"role":"user"}]`, string(s.ChatHistory))

	require.Equal(t, "author", s.CreatedBy.Username, "created_by landed on the wrong field")
	require.Equal(t, "editor", s.UpdatedBy.Username, "updated_by landed on the wrong field")
	require.Equal(t, 2026, s.CreatedAt.Year())
	require.Equal(t, time.February, s.UpdatedAt.Month(),
		"created_at and updated_at are interchangeable by type")
}

// TestScanWizardSession_NullsAreZeroValues covers a session that has not reached the clone
// phase: every optional column is still NULL and must read back as Go's zero value rather
// than erroring. rawJSON maps an empty blob to nil so the round trip through nullJSON is
// stable.
func TestScanWizardSession_NullsAreZeroValues(t *testing.T) {
	vals := wizardRowVals()
	for _, col := range []string{
		"progress_token", "plan_result", "reference_variant", "stage_variant", "sections",
		"email_id", "draft_url", "chat_history", "created_by", "updated_by",
	} {
		vals[slices.Index(wizardSessionColumnOrder, col)] = nil
	}
	s, err := scanWizardSession(wizardFakeRow{vals: vals})
	require.NoError(t, err)
	require.Empty(t, s.ProgressToken)
	require.Nil(t, s.PlanResult)
	require.Nil(t, s.ChatHistory)
	require.Nil(t, s.CreatedBy, "NULL created_by must be nil, not an all-empty Actor")
	require.Nil(t, s.UpdatedBy)
}

// TestScanWizardSession_CorruptActorFails pins that bad actor JSON surfaces rather than
// yielding a silent nil, which is indistinguishable from "not recorded".
func TestScanWizardSession_CorruptActorFails(t *testing.T) {
	for _, col := range []string{"created_by", "updated_by"} {
		t.Run(col, func(t *testing.T) {
			vals := wizardRowVals()
			vals[slices.Index(wizardSessionColumnOrder, col)] = []byte(`{"name":`)
			_, err := scanWizardSession(wizardFakeRow{vals: vals})
			require.ErrorContains(t, err, "unmarshal "+col)
		})
	}
}

// TestWizardSessionCols_ColumnOrderMatchesScan pins the select list against the order above.
func TestWizardSessionCols_ColumnOrderMatchesScan(t *testing.T) {
	got := make([]string, 0, len(wizardSessionColumnOrder))
	for _, c := range strings.Split(wizardSessionCols, ",") {
		got = append(got, strings.TrimSuffix(strings.TrimSpace(c), "::text"))
	}
	require.Equal(t, wizardSessionColumnOrder, got,
		"wizardSessionCols changed. scanWizardSession scans by POSITION, so update its "+
			"destination list in the same order before updating this test — a same-typed shift "+
			"across the five JSONB payload columns will not error, it will swap the A and B "+
			"variants of a generated email.")
}

// TestWizardSessionWrites_StampActorsInTheWriteStatement pins that attribution happens in the
// SAME statement as the write.
//
// A follow-up UPDATE to set the actor compiles and passes every other test, and leaves a
// committed window where the row had changed and the audit trail had not. These statements
// are the only place that can be checked without a live database.
//
// The placeholder INDEX is asserted, not merely "some placeholder": the create statement
// binds $12 to the actor for BOTH created_by and updated_by (a fresh row answers "who touched
// this last" without a second argument), and the update statement binds $10. Binding a
// neighbouring index persists the wrong value with every positional-only assertion green.
func TestWizardSessionWrites_StampActorsInTheWriteStatement(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		cols        map[string]string
	}{
		{"create", createWizardSessionQuery, map[string]string{"created_by": "$12", "updated_by": "$12"}},
		{"update", updateWizardSessionQuery, map[string]string{"updated_by": "$10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for col, placeholder := range tc.cols {
				require.Contains(t, tc.query, col,
					"%s does not mention %s, so the write leaves the audit trail behind", tc.name, col)
				require.Contains(t, tc.query, placeholder,
					"%s does not bind %s, the argument the repo method passes for %s",
					tc.name, placeholder, col)
			}
		})
	}

	// created_by is NOT in the update statement's SET list. Identity and provenance are fixed
	// at creation; a column that cannot change must not appear in an UPDATE, or a later caller
	// assumes it can.
	setClause, _, _ := strings.Cut(updateWizardSessionQuery, "WHERE")
	require.NotContains(t, setClause, "created_by",
		"the update statement rewrites created_by, which would let an edit reassign authorship")
	require.NotContains(t, setClause, "project_id=",
		"the update statement rewrites project_id, which would let an edit move a session between tenants")

	// The version gate and the increment are both required: the gate without the increment is
	// an optimistic-concurrency check that never rejects anything.
	require.Contains(t, updateWizardSessionQuery, "version=version+1")
	require.Contains(t, updateWizardSessionQuery, "version=$14")
}

// TestWizardSessionUpdate_IsTenantScoped pins that the version gate is not the only predicate.
//
// A session id is a UUID and is not guessable, but "not guessable" is not an authorization
// model. Every other write in this service proves tenancy in the WHERE clause; a wizard
// session holds generated marketing copy for an unannounced event.
func TestWizardSessionUpdate_IsTenantScoped(t *testing.T) {
	_, where, ok := strings.Cut(updateWizardSessionQuery, "WHERE")
	require.True(t, ok)
	for _, pred := range []string{"id=$11", "project_id=$12", "brief_id=$13"} {
		require.Contains(t, where, pred)
	}
}

// TestWizardChatHistoryRoundTrip pins model.WizardSession's turn accessors, which are what the
// chat endpoint appends through.
//
// An empty slice must serialise as `[]` and not `null`: the column is NOT NULL DEFAULT '[]',
// so writing null would violate the constraint at the end of a turn that had already called
// the model and cloned the draft.
func TestWizardChatHistoryRoundTrip(t *testing.T) {
	var s model.WizardSession

	turns, err := s.Turns()
	require.NoError(t, err)
	require.Nil(t, turns, "an unset history is no turns, not an error")

	require.NoError(t, s.SetTurns(nil))
	require.Equal(t, json.RawMessage("[]"), s.ChatHistory)

	want := []model.WizardChatTurn{{Role: "user", Content: "shorter", At: time.Unix(0, 0).UTC()}}
	require.NoError(t, s.SetTurns(want))
	got, err := s.Turns()
	require.NoError(t, err)
	require.Equal(t, want, got)

	s.ChatHistory = json.RawMessage(`{"not":"an array"}`)
	_, err = s.Turns()
	require.Error(t, err, "corrupt history must surface rather than reading as no turns")
}
