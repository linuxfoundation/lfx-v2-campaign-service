// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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

	// Restore it whatever happens: every later test in this database depends on the lease.
	t.Cleanup(func() {
		if err := postgres.Migrate(dbtest.DSN()); err == nil {
			return // the forced re-run below already put it back
		}
		if _, err := pool.Exec(context.Background(),
			"CREATE UNIQUE INDEX IF NOT EXISTS "+idx+" ON campaign_audiences (brief_id, platform) WHERE status = 'building'"); err != nil {
			t.Errorf("restore %s: %v — later lease tests would silently pass without it", idx, err)
		}
	})

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
