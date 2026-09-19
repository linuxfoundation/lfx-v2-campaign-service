// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

func newWizardSessionRepo(pool *pgxpool.Pool) *postgres.WizardSessionRepo {
	return postgres.NewWizardSessionRepo(&postgres.Pool{Pool: pool})
}

// TestLiveScrubSessionsForBriefClearsOnlyThatBriefsPersonalData exercises the retention scrub
// against the real migrated schema. A service-level test with an in-memory fake cannot reach
// any of what decides whether this statement is safe:
//
//   - `chat_history` is JSONB NOT NULL DEFAULT '[]', so the scrubbed value has to be '[]' and
//     not NULL; a fake accepts whatever Go zero value the author picked.
//   - the `chat_history <> '[]'::jsonb` guard is a JSONB comparison, which is a real operator
//     with real semantics, not a Go `!=`.
//   - the WHERE is the tenancy boundary. This is a DESTRUCTIVE UPDATE with no undo: if the two
//     bind arguments landed on the wrong columns, the first production run would clear another
//     project's sessions and nothing would report it.
//
// The seed covers both survivor shapes: a same-project/different-brief row and a
// different-project row must both come through untouched.
//
// Only the brief_id predicate is load-bearing against them, and that is a property of the
// SCHEMA rather than of this statement: campaign_briefs.id is the primary key, so a brief id
// is globally unique, and wizard_sessions carries a COMPOSITE FK (brief_id, project_id) that
// forces a session's project to match its brief's. A cross-tenant row sharing the target's
// brief id therefore cannot be inserted -- verified against the live schema, which refuses
// the duplicate id outright.
//
// Stated plainly because the opposite reading is the dangerous one: removing `project_id = $1`
// does NOT fail this test, and that is correct, not a gap. The predicate is defence in depth
// against a future schema where brief ids are only unique per project -- the shape 000007's
// UNIQUE (id, project_id) already anticipates -- and against a caller that passes a brief id
// from one tenant with another tenant's project id. Keep it: this is a destructive UPDATE with
// no undo, and the cost of the extra predicate is nothing.
func TestLiveScrubSessionsForBriefClearsOnlyThatBriefsPersonalData(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	briefs := newBriefRepo(pool)
	sessions := newWizardSessionRepo(pool)

	projectID := dbtest.UniqueID(t, "proj-scrub")
	otherProject := dbtest.UniqueID(t, "proj-other")

	target, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "target")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (target): %v", err)
	}
	sibling, err := briefs.CreateBrief(ctx, draftBrief(projectID, dbtest.UniqueID(t, "sibling")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (sibling): %v", err)
	}
	// Same project id is not available across tenants, so the cross-project row gets its own
	// brief -- which is exactly the real shape: brief ids are unique per project.
	foreign, err := briefs.CreateBrief(ctx, draftBrief(otherProject, dbtest.UniqueID(t, "foreign")), nil)
	if err != nil {
		t.Fatalf("CreateBrief (foreign): %v", err)
	}

	actor := &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"}
	seed := func(projectID, briefID string) *model.WizardSession {
		t.Helper()
		s, serr := sessions.CreateSession(ctx, &model.WizardSession{
			ProjectID:   projectID,
			BriefID:     briefID,
			ChatHistory: json.RawMessage(`[{"role":"user","content":"my unpublished keynote"}]`),
			CreatedBy:   actor,
			UpdatedBy:   actor,
		})
		if serr != nil {
			t.Fatalf("CreateSession(%s/%s): %v", projectID, briefID, serr)
		}
		return s
	}

	victim := seed(projectID, target.ID)
	siblingSession := seed(projectID, sibling.ID)
	foreignSession := seed(otherProject, foreign.ID)

	n, err := sessions.ScrubSessionsForBrief(ctx, projectID, target.ID)
	if err != nil {
		t.Fatalf("ScrubSessionsForBrief: %v", err)
	}
	if n != 1 {
		t.Errorf("scrubbed %d rows, want exactly 1 -- a different count means the WHERE reached rows it should not, or missed the one it should", n)
	}

	got, err := sessions.GetSession(ctx, projectID, target.ID, victim.ID)
	if err != nil {
		t.Fatalf("GetSession (victim): %v", err)
	}
	// '[]' rather than NULL: the column is NOT NULL, so a NULL scrub would have errored --
	// but an empty Go slice would ALSO read as "cleared" here, which is why the raw JSON is
	// compared rather than just its length.
	if string(got.ChatHistory) != "[]" {
		t.Errorf("chat_history = %q, want %q -- the user's typed content is retained", got.ChatHistory, "[]")
	}
	if got.CreatedBy != nil || got.UpdatedBy != nil {
		t.Errorf("actor blobs survived (created_by=%v updated_by=%v)", got.CreatedBy, got.UpdatedBy)
	}

	for _, keep := range []struct {
		label     string
		projectID string
		briefID   string
		id        string
	}{
		{"same project, different brief", projectID, sibling.ID, siblingSession.ID},
		{"different project", otherProject, foreign.ID, foreignSession.ID},
	} {
		survivor, gerr := sessions.GetSession(ctx, keep.projectID, keep.briefID, keep.id)
		if gerr != nil {
			t.Fatalf("GetSession (%s): %v", keep.label, gerr)
		}
		if string(survivor.ChatHistory) == "[]" || survivor.CreatedBy == nil {
			t.Errorf("the scrub reached a session it must not touch (%s): chat_history=%q created_by=%v",
				keep.label, survivor.ChatHistory, survivor.CreatedBy)
		}
	}

	// Idempotent: a second run matches nothing, because the guard excludes already-scrubbed
	// rows. Without it every repeat delete would rewrite rows and bump updated_at forever.
	again, err := sessions.ScrubSessionsForBrief(ctx, projectID, target.ID)
	if err != nil {
		t.Fatalf("ScrubSessionsForBrief (second run): %v", err)
	}
	if again != 0 {
		t.Errorf("second scrub touched %d rows, want 0 -- the already-scrubbed guard does not bind", again)
	}
}

// TestLiveScrubSessionsForBriefRejectsEmptyIdentifiers pins the guard against the shape that
// makes a destructive UPDATE dangerous: an empty bind argument. Postgres would happily run
// `WHERE project_id = ” AND brief_id = ”` and report zero rows, so the failure is silent --
// the caller believes it scrubbed.
func TestLiveScrubSessionsForBriefRejectsEmptyIdentifiers(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	sessions := newWizardSessionRepo(pool)

	for _, tc := range []struct{ name, projectID, briefID string }{
		{"empty project", "", "b1"},
		{"empty brief", "p1", ""},
		{"both empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sessions.ScrubSessionsForBrief(ctx, tc.projectID, tc.briefID); err == nil {
				t.Error("an empty identifier must be refused, not run as a zero-row UPDATE")
			} else if err != domain.ErrNotFound {
				t.Errorf("err = %v, want domain.ErrNotFound", err)
			}
		})
	}
}
