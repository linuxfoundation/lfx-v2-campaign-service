// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// insertBrief creates the parent row a campaign's FK requires and returns its id.
//
// project_id is TEXT, not UUID — migration 000003 retyped it, because a project is
// identified here by its SLUG. It is derived from the test name so two tests (and two
// runs of one test against the same database) cannot collide on the brief table's
// partial-unique (project_id, event_slug).
func insertBrief(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	id := dbtest.UniqueID(t, "brief")
	var briefID string
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug)
		VALUES ($1, 'events', $2)
		RETURNING id`, id, id).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert parent brief: %v", err)
	}
	return briefID
}

// insertCampaignSQL builds the statement both tests below run, with the ON CONFLICT
// predicate substituted in. The predicate is the ONLY thing that varies — everything
// around it stays byte-identical, so a difference in outcome can only come from it.
func insertCampaignSQL(predicate string) string {
	return fmt.Sprintf(`
		INSERT INTO campaigns (project_id, brief_id, platform, campaign_name, status)
		VALUES ($1, $2, 'google_ads', $3, $4)
		ON CONFLICT (brief_id, platform) %s DO NOTHING`, predicate)
}

// TestLiveOnConflictNeedsThePartialIndexPredicate is the reason this package exists.
//
// campaign_repo_test.go pins the same coupling by REGEX over the repo's SQL source: it
// asserts that every `ON CONFLICT (brief_id, platform)` repeats migration 000013's
// `WHERE status <> 'deleted'`. That assertion encodes a belief about PostgreSQL — that
// once migration 000014 dropped the full UNIQUE constraint, the bare form matches no
// arbiter index and fails at runtime. The regex cannot check the belief; it can only
// check that the string still says what someone believed.
//
// This test checks the belief, against the real migrated schema:
//
//   - the BARE form must raise 42P10, "there is no unique or exclusion constraint
//     matching the ON CONFLICT specification"
//   - the PREDICATED form must be accepted and behave as an upsert
//
// If a future migration restores a full unique constraint on (brief_id, platform), the
// first assertion fails — and it should, because at that moment the predicate the repo
// carries everywhere stops being load-bearing and the regex test starts guarding a rule
// that no longer has a reason. Neither fact is visible from source text.
func TestLiveOnConflictNeedsThePartialIndexPredicate(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	project := dbtest.UniqueID(t, "project")
	briefID := insertBrief(ctx, t, pool)

	t.Run("bare ON CONFLICT has no arbiter index", func(t *testing.T) {
		_, err := pool.Exec(ctx, insertCampaignSQL(""),
			project, briefID, dbtest.UniqueID(t, "bare"), "draft")

		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("err = %v, want a *pgconn.PgError — the bare form must be REJECTED, "+
				"because migration 000014 left only the partial index behind", err)
		}
		if pgErr.Code != "42P10" {
			t.Fatalf("SQLSTATE = %s (%s), want 42P10 (invalid_column_reference)", pgErr.Code, pgErr.Message)
		}
	})

	t.Run("predicated ON CONFLICT upserts", func(t *testing.T) {
		stmt := insertCampaignSQL("WHERE status <> 'deleted'")
		name := dbtest.UniqueID(t, "predicated")

		tag, err := pool.Exec(ctx, stmt, project, briefID, name, "draft")
		if err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("first insert affected %d rows, want 1", tag.RowsAffected())
		}

		// The second one must be absorbed rather than raise a duplicate-key error: that
		// is what "the partial index is the arbiter" MEANS, and it is the behaviour
		// every dispatch retry depends on.
		tag, err = pool.Exec(ctx, stmt, project, briefID, name, "draft")
		if err != nil {
			t.Fatalf("second insert: %v — the partial index is not acting as the arbiter", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("second insert affected %d rows, want 0 (DO NOTHING)", tag.RowsAffected())
		}
	})
}

// TestLiveSoftDeletedRowFreesThePlatformSlot pins the POINT of the partial index.
//
// The predicate exists so a deleted campaign stops occupying its (brief_id, platform)
// slot and the pair can be used again. Nothing in the SQL source says that — it is a
// property of how PostgreSQL indexes rows the predicate excludes — so only a live test
// can show that re-launching a platform after a delete inserts rather than conflicts.
func TestLiveSoftDeletedRowFreesThePlatformSlot(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	project := dbtest.UniqueID(t, "project")
	briefID := insertBrief(ctx, t, pool)
	stmt := insertCampaignSQL("WHERE status <> 'deleted'")

	if _, err := pool.Exec(ctx, stmt, project, briefID, dbtest.UniqueID(t, "first"), "draft"); err != nil {
		t.Fatalf("insert live campaign: %v", err)
	}

	// Sanity: while it is LIVE the slot really is held, so the re-insert below is
	// evidence about the delete and not about a constraint that never bit.
	tag, err := pool.Exec(ctx, stmt, project, briefID, dbtest.UniqueID(t, "dup"), "draft")
	if err != nil {
		t.Fatalf("duplicate insert against a live row: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("duplicate insert affected %d rows, want 0 — the live row is not holding "+
			"its (brief_id, platform) slot, so nothing below proves anything", tag.RowsAffected())
	}

	if _, err := pool.Exec(ctx, `UPDATE campaigns SET status = 'deleted' WHERE brief_id = $1`, briefID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	tag, err = pool.Exec(ctx, stmt, project, briefID, dbtest.UniqueID(t, "second"), "draft")
	if err != nil {
		t.Fatalf("re-insert after soft delete: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("re-insert affected %d rows, want 1 — a soft-deleted row must not keep "+
			"holding the (brief_id, platform) slot, or a project can never relaunch a "+
			"platform it once deleted", tag.RowsAffected())
	}
}
