// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// TestMigrateRefusesAnInvalidIndex pins the guard that makes `CREATE INDEX CONCURRENTLY
// IF NOT EXISTS` safe in production, which no fresh-database test can reach.
//
// The failure it defends against is a sequence, not a statement: a CONCURRENTLY build
// fails, leaving the index PRESENT and marked invalid while golang-migrate marks the
// version dirty; an operator reconciles the data and forces the version back; the re-run
// finds the NAME, does nothing, and reports success. The version is now clean over an
// index that enforces nothing — and every assertion that looks the index up by name still
// passes, which is exactly why TestAudienceBuildLeaseIndexIsValid checks indisvalid and
// why that check also has to exist in the RUNNER, on the path production takes.
//
// The invalid index here is real, not simulated: a unique CONCURRENTLY build over
// duplicate rows is precisely how Postgres produces one.
func TestMigrateRefusesAnInvalidIndex(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	// Own table, own name: this test deliberately leaves the schema carrying an invalid
	// index for the duration, and Migrate's check is schema-wide.
	// Short, not UniqueID-derived: Postgres truncates identifiers at 63 bytes, and a name
	// built from this test's own (long) name loses its tail — which the error-message
	// assertion below would then never match. The random suffix keeps it unique.
	id := dbtest.UniqueID(t, "")
	table := "zz_invalid_idx_" + id[strings.LastIndexByte(id, '-')+1:]
	idx := "uq_" + table
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (k int)"); err != nil { //nolint:gosec // name is built here, not from input.
		t.Fatalf("create scratch table: %v", err)
	}
	t.Cleanup(func() {
		//nolint:gosec // same locally-built name.
		if _, err := pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table); err != nil {
			t.Errorf("drop scratch table: %v — it would fail every later Migrate in this database", err)
		}
	})
	if _, err := pool.Exec(ctx, "INSERT INTO "+table+" VALUES (1), (1)"); err != nil { //nolint:gosec // same.
		t.Fatalf("seed duplicates: %v", err)
	}

	// Expected to FAIL — that is what leaves the invalid index behind.
	//nolint:gosec // same.
	if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY "+idx+" ON "+table+" (k)"); err == nil {
		t.Fatalf("the duplicate-key build succeeded; this test cannot produce an invalid index")
	}
	var valid bool
	//nolint:gosec // same.
	if err := pool.QueryRow(ctx, "SELECT indisvalid FROM pg_index WHERE indexrelid = '"+idx+"'::regclass").Scan(&valid); err != nil {
		t.Fatalf("read pg_index: %v", err)
	}
	if valid {
		t.Fatalf("%s came out valid; the premise of this test does not hold on this server", idx)
	}

	err := postgres.Migrate(dbtest.DSN())
	if !errors.Is(err, postgres.ErrInvalidIndex) {
		t.Fatalf("Migrate with an invalid index present: got %v, want ErrInvalidIndex — "+
			"reporting success here is how a lost UNIQUE constraint goes unnoticed", err)
	}
	if !strings.Contains(err.Error(), idx) {
		t.Errorf("the error does not name %s: %v — an operator has to know which index to drop", idx, err)
	}
	// This index is hand-built — no migration creates it — and the message must say so.
	// The scan is schema-wide, so this is not a contrived case: sending an operator to
	// force a version would replay unrelated DDL to fix an index nothing recreates.
	if !strings.Contains(err.Error(), idx+" (no migration creates this") {
		t.Errorf("the error offers version advice for a hand-built index: %v", err)
	}
	if !postgres.IsPermanentMigrationErr(err) {
		t.Errorf("ErrInvalidIndex is not permanent; boot would 503-loop instead of failing fast")
	}
}

// TestMigrateRefusesADroppedRequiredIndex pins the step AFTER the one above: the state an
// operator reaches by doing half of the recovery.
//
// Dropping the invalid index clears the scan in TestMigrateRefusesAnInvalidIndex, but
// golang-migrate has already recorded migration 18 as CLEAN — so Up() returns ErrNoChange,
// nothing rebuilds the index, and boot succeeds against a schema with no uniqueness at all.
// That is the same silent loss the invalid-index scan exists to prevent, reached by
// following its own remedy. Detection cannot be "nothing is invalid"; it has to be "the
// index that enforces the invariant is present and valid".
//
// This test also keeps `requiredIndexes` honest: it drops each name and requires Migrate to
// notice, so an entry naming an index no migration creates fails here rather than sitting
// in the list as decoration.
func TestMigrateRefusesADroppedRequiredIndex(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	const idx = "uq_campaign_audiences_brief_platform_building"

	t.Cleanup(func() { restoreLeaseIndex(t, pool) })

	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+idx); err != nil {
		t.Fatalf("drop the required index: %v", err)
	}

	// Precisely the operator's next move: re-run the migration, as the ErrInvalidIndex
	// message used to advise on its own. The version is clean, so this rebuilds nothing.
	err := postgres.Migrate(dbtest.DSN())
	if !errors.Is(err, postgres.ErrMissingRequiredIndex) {
		t.Fatalf("Migrate after dropping %s: got %v, want ErrMissingRequiredIndex — "+
			"succeeding here starts the service with the audience-build race wide open "+
			"and nothing to report it", idx, err)
	}
	if !strings.Contains(err.Error(), idx) {
		t.Errorf("the error does not name %s: %v", idx, err)
	}
	if !postgres.IsPermanentMigrationErr(err) {
		t.Errorf("ErrMissingRequiredIndex is not permanent; boot would 503-loop rather than " +
			"telling the operator the version must be forced back")
	}
}

// TestMigrateRefusesADroppedDispatchIndex is the same state for the OTHER required index,
// and it is the one with money attached.
//
// Migration 000014 dropped campaigns_brief_id_platform_key, so uq_campaigns_brief_platform_live
// is the sole arbiter of (brief_id, platform) uniqueness and the thing ClaimCampaignDispatch
// rests on. 000014 guards its definition — but only once, while 000014 is running. An index
// lost afterwards (an operator clearing invalid-index debris, a rebuild from 000013 whose
// IF NOT EXISTS no-opped against a same-named leftover) left a schema that booted clean and
// double-created paid campaigns under concurrency. That is exactly the silent absence the
// runner check exists to refuse, so it has to cover this index and not only the lease one.
func TestMigrateRefusesADroppedDispatchIndex(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	const idx = "uq_campaigns_brief_platform_live"

	t.Cleanup(func() { restoreDispatchIndex(t, pool) })

	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+idx); err != nil {
		t.Fatalf("drop the required index: %v", err)
	}

	err := postgres.Migrate(dbtest.DSN())
	if !errors.Is(err, postgres.ErrMissingRequiredIndex) {
		t.Fatalf("Migrate after dropping %s: got %v, want ErrMissingRequiredIndex — "+
			"succeeding here starts the service with (brief_id, platform) uniqueness gone "+
			"and concurrent claims free to double-create paid campaigns", idx, err)
	}
	if !strings.Contains(err.Error(), idx) {
		t.Errorf("the error does not name %s: %v", idx, err)
	}
	// The remedy has to point at 000013, the migration that CREATES this index —
	// forcing back to 17 replays 000018 and rebuilds the wrong one.
	if !strings.Contains(err.Error(), "force 12") {
		t.Errorf("the error does not tell the operator to force back to 12: %v", err)
	}
	if !postgres.IsPermanentMigrationErr(err) {
		t.Errorf("ErrMissingRequiredIndex is not permanent; boot would 503-loop rather than " +
			"telling the operator the version must be forced back")
	}
}

// TestMigrateRefusesARequiredIndexWithTheWrongDefinition pins the third state in this
// family, and the one a NAME-only check cannot see at all.
//
// Migration 000018 creates the lease index with CREATE UNIQUE INDEX CONCURRENTLY *IF NOT
// EXISTS*, so any index already carrying that name makes it a silent no-op — the real
// constraint is never built. A runner check that asks only "is an index of this name
// present and valid" then reports the schema healthy, and every concurrent build proceeds
// unconstrained with nothing to report it. This is not a hypothetical shape: it is the
// same hole migration 000014's drop-guard was written to close, and
// TestMigration000014_GuardChecksIndexDefinition records the PostgreSQL 16.10 run where a
// same-named NON-unique index passed the name-only form of that guard.
//
// The impostor here is non-unique AND non-partial, so it is wrong in two independent ways
// and the message has to say which — an operator who is told only "wrong definition" has
// to go read the catalog themselves.
func TestMigrateRefusesARequiredIndexWithTheWrongDefinition(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()

	const idx = "uq_campaign_audiences_brief_platform_building"

	t.Cleanup(func() { restoreLeaseIndex(t, pool) })

	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+idx); err != nil {
		t.Fatalf("drop the real index: %v", err)
	}
	// Exactly what 000018 would find and skip over.
	if _, err := pool.Exec(ctx,
		"CREATE INDEX "+idx+" ON campaign_audiences (brief_id, platform)"); err != nil {
		t.Fatalf("create the impostor index: %v", err)
	}

	err := postgres.Migrate(dbtest.DSN())
	if !errors.Is(err, postgres.ErrRequiredIndexMismatch) {
		t.Fatalf("Migrate with a same-named non-unique index: got %v, want "+
			"ErrRequiredIndexMismatch — the migration's IF NOT EXISTS skipped, so this "+
			"schema arbitrates no lease at all while every name-based check passes", err)
	}
	// The two defects must BOTH be named. A message that stops at the first one sends the
	// operator to rebuild an index that would still be wrong.
	for _, want := range []string{idx, "indisunique=false", "predicate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// Dropping is a required step here and not in the missing-index case, so the message
	// has to say so: forcing the version alone re-runs a CREATE the impostor no-ops.
	if !strings.Contains(err.Error(), "DROPPING") {
		t.Errorf("the error does not tell the operator to DROP the impostor first: %v", err)
	}
	if !postgres.IsPermanentMigrationErr(err) {
		t.Errorf("ErrRequiredIndexMismatch is not permanent; boot would 503-loop instead of " +
			"naming the one thing an operator has to do")
	}
}

// restoreLeaseIndex puts the audience-build lease index back exactly as migration 000018
// defines it, for tests that deliberately remove or replace it.
//
// It does NOT ask Migrate whether the index is back, and that is the point. Migrate
// returning nil is precisely the regression these tests exist to catch: if the runner
// check is weakened or deleted, Migrate answers "clean" over a schema with no lease at
// all, a cleanup trusting that answer restores nothing, and every LATER lease test in this
// database then passes against an unconstrained table. The cleanup's success signal must
// not be the code under test. DROP-then-CREATE rather than IF NOT EXISTS for the same
// reason the guard exists: an impostor left by the mismatch test carries the right name,
// so IF NOT EXISTS would skip and leave it in place.
func restoreLeaseIndex(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
},
) {
	t.Helper()
	restoreRequiredIndex(t, pool, "uq_campaign_audiences_brief_platform_building",
		"CREATE UNIQUE INDEX uq_campaign_audiences_brief_platform_building"+
			" ON campaign_audiences (brief_id, platform) WHERE status = 'building'")
}

// restoreDispatchIndex rebuilds the campaigns partial unique index (migration 000013).
// Since 000014 dropped campaigns_brief_id_platform_key this is the ONLY thing standing
// between two concurrent claims and two paid campaigns for one (brief, platform), so a
// test that leaves it dropped does not merely dirty the database — it makes every later
// dispatch test pass against a table that cannot enforce what they are about.
func restoreDispatchIndex(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
},
) {
	t.Helper()
	restoreRequiredIndex(t, pool, "uq_campaigns_brief_platform_live",
		"CREATE UNIQUE INDEX uq_campaigns_brief_platform_live"+
			" ON campaigns (brief_id, platform) WHERE status <> 'deleted'")
}

func restoreRequiredIndex(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, idx, create string,
) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+idx); err != nil {
		t.Errorf("restore %s: drop: %v", idx, err)
		return
	}
	if _, err := pool.Exec(ctx, create); err != nil {
		t.Errorf("restore %s: create: %v", idx, err)
		return
	}
	// Verified against the catalog, not against the CREATE's exit status, so a restore
	// that silently built the wrong thing fails here rather than in an unrelated test.
	var unique, valid, partial bool
	if err := pool.QueryRow(ctx, `SELECT i.indisunique, i.indisvalid, i.indpred IS NOT NULL
		FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = $1`, idx).Scan(&unique, &valid, &partial); err != nil {
		t.Errorf("restore %s: verify: %v — later lease tests would silently pass without it", idx, err)
		return
	}
	if !unique || !valid || !partial {
		t.Errorf("restore %s: unique=%t valid=%t partial=%t, want all true", idx, unique, valid, partial)
	}
}
