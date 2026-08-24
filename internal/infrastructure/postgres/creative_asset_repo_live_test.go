// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// creative_asset_repo.go has no source-text test and cannot get a meaningful one: its two
// statements each carry behaviour no string assertion can reach — the INSERT ... SELECT ... WHERE
// EXISTS parent-brief gate, the ON CONFLICT ... DO UPDATE idempotency no-op, and the (project,
// brief) scoping on both reads. None of those are visible in the SQL text the way a bind-argument
// count is; they are only observable against a real database. This file is that database test.
//
// It is in-package (not the external dbtest_test package) for the same reason
// audience_reconcile_live_test.go is: NewCreativeAssetRepo takes the instrumented *Pool wrapper,
// which dbtest.Pool does not hand back — it exposes only the embedded *pgxpool.Pool. Building the
// repo therefore has to happen where *Pool is constructible.

var (
	creativeAssetPoolOnce sync.Once
	creativeAssetPool     *Pool
	creativeAssetPoolErr  error
)

// creativeAssetTestPool mirrors reconcileTestPool: it stands up a migrated *Pool against
// TEST_DATABASE_URL, skipping off CI and failing on CI when the variable is unset (a skipped live
// test reports success, which on a runner that promised a database is a green build for a suite
// that never ran). It cannot import dbtest — dbtest imports this package, so the reverse is an
// import cycle — so it repeats the minimal bootstrap.
func creativeAssetTestPool(t *testing.T) *Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is empty on a CI runner: this live-database test would skip entirely")
		}
		t.Skip("TEST_DATABASE_URL is not set; skipping the live-database test")
	}

	creativeAssetPoolOnce.Do(func() {
		if err := Migrate(dsn); err != nil {
			creativeAssetPoolErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		creativeAssetPool, creativeAssetPoolErr = NewPool(ctx, dsn)
	})
	if creativeAssetPoolErr != nil {
		t.Fatalf("live-database harness: %v", creativeAssetPoolErr)
	}
	return creativeAssetPool
}

// creativeAssetUniqueID returns a value no other row in this schema shares, following the same
// name-prefix + crypto/rand-suffix shape as dbtest.UniqueID (the schema is shared across runs and
// never dropped, so a name-only id collides with the previous run's row).
func creativeAssetUniqueID(t *testing.T, suffix string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("cannot generate a unique id: %v", err)
	}
	return "creative-asset-" + suffix + "-" + hex.EncodeToString(b[:])
}

// insertCreativeAssetTestBrief inserts a parent brief with the given status and returns its id and
// project. status is a parameter because the parent-brief gate is exactly what several cases turn
// on: 'approved' is an active parent an asset may attach to, 'archived' is one it may not.
func insertCreativeAssetTestBrief(ctx context.Context, t *testing.T, pool *Pool, status string) (briefID, projectID string) {
	t.Helper()
	projectID = creativeAssetUniqueID(t, "brief")
	err := pool.QueryRow(ctx, `
		INSERT INTO campaign_briefs (project_id, program_type, event_slug, status)
		VALUES ($1, 'events', $1, $2)
		RETURNING id`, projectID, status).Scan(&briefID)
	if err != nil {
		t.Fatalf("insert %q parent brief: %v", status, err)
	}
	return briefID, projectID
}

// newTestAsset builds an in-memory asset for a (project, brief) with a unique checksum, so two
// distinct assets never collide on the (brief_id, checksum) key by accident. A caller that needs
// a SECOND asset deliberately reusing the same key (the idempotency case) reads it back off the
// returned asset's Checksum field.
func newTestAsset(t *testing.T, projectID, briefID string) *model.CreativeAsset {
	t.Helper()
	imgBytes := []byte("\x89PNG\r\n\x1a\n" + creativeAssetUniqueID(t, "bytes"))
	return &model.CreativeAsset{
		ProjectID: projectID,
		BriefID:   briefID,
		MimeType:  model.MimeTypePNG,
		ByteSize:  int64(len(imgBytes)),
		Checksum:  creativeAssetUniqueID(t, "sum"),
		Bytes:     imgBytes,
		CreatedBy: json.RawMessage(`{"principal":"first-uploader"}`),
	}
}

// TestCreativeAssetRepo_CreateAsset_StoresAndReturnsMetadata covers the happy path: an asset under
// an active same-project brief is stored and its metadata comes back. It also pins the deliberate
// decision that the WRITE does not ship the bytes back (createCreativeAssetQuery scans
// creativeAssetCols, which omits bytes) — the upload endpoint returns metadata only.
func TestCreativeAssetRepo_CreateAsset_StoresAndReturnsMetadata(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	asset := newTestAsset(t, projectID, briefID)

	stored, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if stored.ID == "" {
		t.Error("stored.ID is empty — the row was not returned")
	}
	if stored.ProjectID != projectID || stored.BriefID != briefID {
		t.Errorf("scope = (%s, %s), want (%s, %s)", stored.ProjectID, stored.BriefID, projectID, briefID)
	}
	if stored.MimeType != asset.MimeType || stored.ByteSize != asset.ByteSize || stored.Checksum != asset.Checksum {
		t.Errorf("metadata mismatch: got mime=%q size=%d sum=%q", stored.MimeType, stored.ByteSize, stored.Checksum)
	}
	if stored.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — the DEFAULT now() was not returned")
	}
	if stored.Bytes != nil {
		t.Errorf("the write returned %d bytes, want none — creativeAssetCols omits bytes so a write never ships the image back", len(stored.Bytes))
	}
}

// TestCreativeAssetRepo_CreateAsset_StoresByteSizeMatchingPayload pins byte_size to the bytes
// that were actually PERSISTED, not to the value the caller happened to pass in.
//
// This is a different assertion from the metadata check in the happy-path test above, which
// compares stored.ByteSize against asset.ByteSize — both sides of that comparison trace back to
// one in-memory field, so it proves the value round-tripped, not that it describes the image. A
// binding that stored a constant, the wrong argument, or a stale size would still have to be
// read back as itself, and the column exists precisely so callers and metrics can read the size
// WITHOUT loading the multi-megabyte blob: nothing else would ever notice it disagreed with the
// image. So the comparison here has the database compute length(bytes) over the stored row and
// asserts byte_size equals THAT, which is the claim the column actually makes.
func TestCreativeAssetRepo_CreateAsset_StoresByteSizeMatchingPayload(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	asset := newTestAsset(t, projectID, briefID)
	// A payload whose length is not a value the code could produce by accident: not zero, and
	// not equal to any other bound argument's length.
	asset.Bytes = append(asset.Bytes, make([]byte, 512)...)
	asset.ByteSize = int64(len(asset.Bytes))

	stored, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	// The database compares the column against the stored image itself. storedLen is the length
	// of the BYTEA as persisted, so this fails whether byte_size was written as 0, as some other
	// argument, or as a size that simply does not describe these bytes.
	var storedSize, storedLen int64
	if err := pool.QueryRow(ctx,
		`SELECT byte_size, length(bytes) FROM creative_assets WHERE id=$1`, stored.ID).
		Scan(&storedSize, &storedLen); err != nil {
		t.Fatalf("read back byte_size and length(bytes): %v", err)
	}
	if storedSize != storedLen {
		t.Errorf("byte_size = %d but the stored image is %d bytes — the column callers read "+
			"INSTEAD of loading the blob does not describe the blob", storedSize, storedLen)
	}
	if storedLen != int64(len(asset.Bytes)) {
		t.Errorf("stored image is %d bytes, want the %d that were uploaded", storedLen, len(asset.Bytes))
	}
	// And the value handed back to the caller agrees with what is on disk, so a caller that
	// trusts the returned metadata is not reading a different number from the stored one.
	if stored.ByteSize != storedLen {
		t.Errorf("CreateAsset returned byte_size %d, but the stored row holds %d", stored.ByteSize, storedLen)
	}
}

// TestCreativeAssetRepo_CreateAsset_IsIdempotentOnChecksum covers the ON CONFLICT (brief_id,
// checksum) DO UPDATE path: re-uploading the SAME bytes to the SAME brief returns the EXISTING row
// rather than a second copy, and the FIRST uploader's created_by is preserved (the SET is a no-op
// on byte_size precisely so a later re-sender does not re-author the asset). This is the behaviour
// DO NOTHING could not provide — it would suppress RETURNING on the conflict and surface ErrNoRows
// for a duplicate.
func TestCreativeAssetRepo_CreateAsset_IsIdempotentOnChecksum(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	asset := newTestAsset(t, projectID, briefID)

	first, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		t.Fatalf("first CreateAsset: %v", err)
	}

	// A second upload of the same checksum by a DIFFERENT actor.
	reupload := newTestAsset(t, projectID, briefID)
	reupload.Checksum = asset.Checksum
	reupload.Bytes = asset.Bytes
	reupload.ByteSize = asset.ByteSize
	reupload.CreatedBy = json.RawMessage(`{"principal":"second-uploader"}`)

	second, err := repo.CreateAsset(ctx, reupload)
	if err != nil {
		t.Fatalf("re-upload CreateAsset: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-upload returned a new id %s, want the existing %s — the upload was not idempotent", second.ID, first.ID)
	}

	// Exactly one row exists for this (brief, checksum).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM creative_assets WHERE brief_id=$1 AND checksum=$2`, briefID, asset.Checksum).
		Scan(&n); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 — a duplicate was stored", n)
	}

	// The first uploader's authorship survives: a re-send does not re-author.
	var createdBy []byte
	if err := pool.QueryRow(ctx, `SELECT created_by FROM creative_assets WHERE id=$1`, first.ID).Scan(&createdBy); err != nil {
		t.Fatalf("read back created_by: %v", err)
	}
	if !bytes.Contains(createdBy, []byte("first-uploader")) {
		t.Errorf("created_by = %s, want the first uploader preserved — a re-send must not re-author the asset", createdBy)
	}
}

// TestCreativeAssetRepo_CreateAsset_RejectsInactiveOrForeignBrief covers the three ways the
// parent-brief gate must refuse an insert, all mapped to ErrNotFound so none reveals whether a
// brief the caller cannot see exists: no such brief, an archived brief, and a brief owned by a
// different project. Each asserts nothing was stored (via assertNothingStored), because returning
// ErrNotFound is not the same claim as storing no row — a gate could do the wrong one of the two.
func TestCreativeAssetRepo_CreateAsset_RejectsInactiveOrForeignBrief(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	// assertNothingStored proves the gate refused the write, not merely that it returned an
	// error: a bug that both stored a row AND returned ErrNotFound would pass the error check
	// alone. Every subtest below asserts it, so the comment's "nothing was stored" promise holds
	// for all three refusal paths, not just the archived one.
	assertNothingStored := func(t *testing.T, briefID string) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM creative_assets WHERE brief_id=$1`, briefID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("row count = %d, want 0 — the gate returned ErrNotFound but stored a row anyway", n)
		}
	}

	t.Run("absent brief", func(t *testing.T) {
		// A well-formed brief id that names nothing. The INSERT ... SELECT finds no active parent,
		// so nothing is inserted and RETURNING is empty → ErrNotFound (no FK violation, because
		// nothing was inserted).
		briefID := "00000000-0000-4000-8000-000000000000"
		asset := newTestAsset(t, creativeAssetUniqueID(t, "proj"), briefID)
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound", err)
		}
		assertNothingStored(t, briefID)
	})

	t.Run("archived brief", func(t *testing.T) {
		briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "archived")
		asset := newTestAsset(t, projectID, briefID)
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — an archived brief must not accrue assets", err)
		}
		assertNothingStored(t, briefID)
	})

	t.Run("brief owned by another project", func(t *testing.T) {
		briefID, _ := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
		// Same (existing, active) brief, but a caller scoped to a DIFFERENT project. The FK would
		// accept it — the brief exists — but the WHERE project_id gate must not. This is a tenant
		// boundary, so it is the most important of the three to prove stored NOTHING, not just to
		// prove it returned an error.
		asset := newTestAsset(t, creativeAssetUniqueID(t, "other-proj"), briefID)
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — a caller must not attach an asset to another project's brief", err)
		}
		assertNothingStored(t, briefID)
	})
}

// TestCreativeAssetRepo_CreateAsset_AcceptsDraftBrief pins the deliberate choice of ACTIVE
// (not APPROVED) as the parent predicate.
//
// Every other successful case in this file uses an 'approved' brief, so the predicate could be
// narrowed to status = 'approved' without failing any of them — and that narrowing would break
// the actual product behaviour: uploading a creative is part of COMPOSING a brief, which happens
// BEFORE approval. (Campaign creation gates on approval; asset upload deliberately does not.)
// A guard no test reaches is not a guard, so this drives the state that distinguishes the two.
func TestCreativeAssetRepo_CreateAsset_AcceptsDraftBrief(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "draft")
	asset := newTestAsset(t, projectID, briefID)

	stored, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		t.Fatalf("CreateAsset under a DRAFT brief: %v — an asset must be storable while the "+
			"brief is still being composed; the parent predicate is ACTIVE, not APPROVED", err)
	}
	// And it reads back, so the same predicate is proven on both statements rather than only
	// on the write.
	if _, err := repo.GetAsset(ctx, projectID, briefID, stored.ID); err != nil {
		t.Fatalf("GetAsset under a DRAFT brief: %v", err)
	}
}

// TestCreativeAssets_MimeTypeCheckRejectsUnsupported pins the database's MIME allow-list.
// Every other insert in this file uses image/png, so removing or widening the CHECK would leave
// the suite green. Both sides are asserted: an unsupported type is refused, and the OTHER
// permitted type is accepted — a CHECK narrowed to just 'image/png' is as wrong as one dropped.
func TestCreativeAssets_MimeTypeCheckRejectsUnsupported(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")

	insert := func(mime string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
			VALUES ($1, $2, $3, 4, $4, '\x89504e47'::bytea)`,
			projectID, briefID, mime, creativeAssetUniqueID(t, "sum"))
		return err
	}

	// A plausible image type that is NOT in the allow-list — the case that matters, since the
	// list is an allow-list rather than a rejection of obvious junk.
	err := insert("image/gif")
	if err == nil {
		t.Fatal("mime_type 'image/gif' was accepted — the CHECK allow-list is missing or widened")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("err = %v, want a check_violation (SQLSTATE 23514) on mime_type", err)
	}

	// The second PERMITTED type must still be accepted, so the test fails on a CHECK narrowed to
	// image/png alone — which model.MimeTypeJPEG would then contradict.
	if err := insert(model.MimeTypeJPEG); err != nil {
		t.Fatalf("mime_type %q must be accepted — it is one of the two supported formats and "+
			"model.MimeTypeJPEG names it: %v", model.MimeTypeJPEG, err)
	}
}

// TestCreativeAssets_ByteSizeChecksBindSizeToPayload pins the column-level CHECK on byte_size:
// that it EQUALS octet_length(bytes). That single constraint is what refuses a negative value
// too — octet_length() is never negative — which is why the separate CHECK (byte_size >= 0)
// that briefly sat beside it was removed.
//
// The equality is the important one. Callers and metrics read this column INSTEAD of loading the
// blob, so a value that does not describe the image is invisible to every reader that uses the
// column as intended. CreateAsset binds a.ByteSize and a.Bytes as INDEPENDENT parameters —
// nothing in the insert derives one from the other — so the database is the only place the
// relationship can actually be enforced.
//
// Like the FK test, this writes DIRECTLY, to isolate the DATABASE constraint from the
// repository: going through CreateAsset would test whichever values the caller happened to set,
// while a direct insert tests the column. Three cases, because the constraint has three edges:
// negative, mismatched, and the legal zero/empty pair. All three are enforced by the SINGLE
// equality CHECK: a separate CHECK (byte_size >= 0) was removed as redundant, because
// octet_length() is never negative so the equality already implies it — and deleting the >= 0
// clause broke none of these cases, which is what proved it was doing nothing.
func TestCreativeAssets_ByteSizeChecksBindSizeToPayload(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")

	_, err := pool.Exec(ctx, `
		INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
		VALUES ($1, $2, 'image/png', -1, $3, '\x89504e47'::bytea)`,
		projectID, briefID, creativeAssetUniqueID(t, "sum"))
	if err == nil {
		t.Fatal("a byte_size of -1 was accepted for a 4-byte image — " +
			"CHECK (byte_size = octet_length(bytes)) is missing")
	}
	// 23514 = check_violation. Asserting the code keeps this from passing on an unrelated error.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("err = %v, want a check_violation (SQLSTATE 23514) on byte_size", err)
	}

	// A MISMATCH is refused too, which is the case that matters: CreateAsset binds ByteSize and
	// Bytes independently, so a buggy caller or a direct writer can otherwise persist a size that
	// does not describe the blob — and every reader that uses this column as intended (instead of
	// loading the blob) would trust it.
	_, err = pool.Exec(ctx, `
		INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
		VALUES ($1, $2, 'image/png', 999, $3, '\x89504e47'::bytea)`,
		projectID, briefID, creativeAssetUniqueID(t, "sum"))
	if err == nil {
		t.Fatal("byte_size = 999 was accepted for a 4-byte image — " +
			"CHECK (byte_size = octet_length(bytes)) is missing")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("err = %v, want a check_violation (SQLSTATE 23514) on the size/blob equality", err)
	}

	// Zero is legal — an empty payload is a question for the upload endpoint's validation, not
	// something the column itself should refuse. Asserting this keeps the CHECK from being
	// silently tightened to > 0, which would reject a row the repository can legitimately write.
	if _, err := pool.Exec(ctx, `
		INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
		VALUES ($1, $2, 'image/png', 0, $3, ''::bytea)`,
		projectID, briefID, creativeAssetUniqueID(t, "sum")); err != nil {
		t.Fatalf("byte_size = 0 must be accepted by the column, got: %v", err)
	}
}

// TestCreativeAssets_CompositeTenantFKRejectsMismatchedProject pins the DATABASE-level
// parent-tenant constraint, independently of CreateAsset's WHERE EXISTS gate.
//
// The two are not redundant. CreateAsset's gate protects the API path; this FK protects the
// TABLE, from every writer — a worker, a backfill, a migration, a psql session. The row copies
// project_id and GetAsset TRUSTS that copy for tenant scoping, so a row whose project_id names
// a different project than its brief would read out under the wrong tenant. That is exactly the
// hole migration 000007 closed for campaign_audiences, and the reason this table takes a
// composite (brief_id, project_id) FK rather than the brief_id-only form.
//
// The write here deliberately BYPASSES the repository and inserts directly, because going
// through CreateAsset would be stopped by the WHERE EXISTS gate and would prove only that the
// gate works. Only a direct insert can reach the constraint.
func TestCreativeAssets_CompositeTenantFKRejectsMismatchedProject(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	foreignProject := creativeAssetUniqueID(t, "foreign-proj")

	_, err := pool.Exec(ctx, `
		INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
		VALUES ($1, $2, 'image/png', 4, $3, '\x89504e47'::bytea)`,
		foreignProject, briefID, creativeAssetUniqueID(t, "sum"))
	if err == nil {
		t.Fatalf("a direct insert pairing brief %s (project %s) with project_id %q succeeded — "+
			"the composite (brief_id, project_id) FK is missing, so a non-API writer can create "+
			"an asset that GetAsset would serve under the wrong tenant",
			briefID, projectID, foreignProject)
	}
	// 23503 = foreign_key_violation. Asserting the CODE (not merely "some error") keeps the test
	// from passing on an unrelated failure such as a bad column list or a CHECK violation.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("err = %v, want a foreign_key_violation (SQLSTATE 23503) from the composite parent FK", err)
	}

	// The SAME insert with the brief's own project must succeed, so the test proves the FK is
	// selective rather than that the insert is simply broken.
	if _, err := pool.Exec(ctx, `
		INSERT INTO creative_assets (project_id, brief_id, mime_type, byte_size, checksum, bytes)
		VALUES ($1, $2, 'image/png', 4, $3, '\x89504e47'::bytea)`,
		projectID, briefID, creativeAssetUniqueID(t, "sum")); err != nil {
		t.Fatalf("the matching-project insert must be accepted, got: %v", err)
	}
}

// TestCreativeAssetRepo_GetAsset_ReturnsBytesScopedToTenant covers the dispatch-time read: GetAsset
// returns the stored image WITH its bytes (creativeAssetColsWithBytes), and only to a caller whose
// (project, brief) matches. The scope lives in the WHERE clause, so a mismatch is ErrNotFound and a
// Meta variant can never load a creative owned by another tenant or brief.
func TestCreativeAssetRepo_GetAsset_ReturnsBytesScopedToTenant(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
	asset := newTestAsset(t, projectID, briefID)
	stored, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	t.Run("in-scope read returns the bytes", func(t *testing.T) {
		got, err := repo.GetAsset(ctx, projectID, briefID, stored.ID)
		if err != nil {
			t.Fatalf("GetAsset: %v", err)
		}
		if !bytes.Equal(got.Bytes, asset.Bytes) {
			t.Errorf("bytes mismatch: got %d bytes, want %d — the dispatch read must return the image itself", len(got.Bytes), len(asset.Bytes))
		}
		if got.Checksum != asset.Checksum || got.MimeType != asset.MimeType {
			t.Errorf("metadata mismatch: got sum=%q mime=%q", got.Checksum, got.MimeType)
		}
	})

	t.Run("wrong project is ErrNotFound", func(t *testing.T) {
		_, err := repo.GetAsset(ctx, creativeAssetUniqueID(t, "wrong-proj"), briefID, stored.ID)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — a read outside the caller's project must not resolve", err)
		}
	})

	t.Run("wrong brief is ErrNotFound", func(t *testing.T) {
		otherBriefID, _ := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
		_, err := repo.GetAsset(ctx, projectID, otherBriefID, stored.ID)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — an asset is scoped to its own brief", err)
		}
	})

	t.Run("an archived parent brief makes the asset unreadable", func(t *testing.T) {
		// Archival must apply to the WHOLE nested resource, not half of it. CreateAsset already
		// refuses an archived parent; if GetAsset kept serving the bytes, archival would leave
		// the asset readable while it could no longer be created — the inconsistency GetAudience
		// documents. The asset is created while the brief is active, then the brief is archived
		// underneath it, which is the only ordering that can produce this state.
		//
		// This subtest ALSO carries createCreativeAssetQuery's locking decision, which is not
		// obvious from here and is the reason it must not be weakened casually. That insert
		// gates its parent with an unlocked WHERE EXISTS and does NOT serialize against
		// archival, so a concurrent ArchiveBrief can strand an asset under a brief that is
		// archived by the time the row lands. The comment there justifies leaving it unlocked
		// SOLELY by the consequence — "the consequence here is only a stored blob nothing can
		// reach" — and names the change that would invalidate it: "a path that reads assets
		// without re-checking the parent [means] this insert needs the locking treatment".
		//
		// This subtest independently pins LIFECYCLE VISIBILITY: an archived brief must refuse
		// its children on every operation, so dropping the EXISTS from getCreativeAssetQuery
		// fails here. It is no longer load-bearing for the INSERT: CreateAsset takes
		// SELECT ... FOR UPDATE on the parent, and
		// TestCreativeAssetRepo_CreateAsset_SerializesAgainstArchival carries that decision.
		// Weakening this read would expose archived assets — its own bug — but would not
		// reopen the archival race.
		liveBriefID, liveProjectID := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
		a := newTestAsset(t, liveProjectID, liveBriefID)
		created, err := repo.CreateAsset(ctx, a)
		if err != nil {
			t.Fatalf("CreateAsset under the active brief: %v", err)
		}
		// Readable while the parent is active — so the assertion below pins the ARCHIVAL, not a
		// read that never worked.
		if _, err := repo.GetAsset(ctx, liveProjectID, liveBriefID, created.ID); err != nil {
			t.Fatalf("GetAsset before archival: %v", err)
		}

		if _, err := pool.Exec(ctx, `UPDATE campaign_briefs SET status='archived' WHERE id=$1`, liveBriefID); err != nil {
			t.Fatalf("archive the parent brief: %v", err)
		}

		if _, err := repo.GetAsset(ctx, liveProjectID, liveBriefID, created.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — an archived brief's assets must leave "+
				"the live lifecycle, not stay readable after create has begun refusing them", err)
		}
	})

	t.Run("absent asset is ErrNotFound", func(t *testing.T) {
		_, err := repo.GetAsset(ctx, projectID, briefID, "00000000-0000-4000-8000-000000000000")
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound", err)
		}
	})
}

// TestCreativeAssetRepo_CreateAsset_SerializesAgainstArchival pins the ordering between an upload
// and a concurrent archival of its parent brief.
//
// This is the one property `INSERT ... WHERE EXISTS` cannot provide on its own, and it is worth a
// deterministic test rather than a comment because the failure is SILENT and PERMANENT. Under
// READ COMMITTED each statement takes a fresh snapshot, so an unlocked EXISTS gate can observe the
// brief as active while `ArchiveBrief` commits, and the insert then lands under a parent that is
// archived by the time it commits. `creative_assets` has no prune and briefs are never hard
// deleted, so the blob is unreachable storage retained forever — nothing later reconciles it away.
//
// The interleaving is FORCED, not raced. An archival is opened in its own transaction and left
// uncommitted, which takes the row lock; the upload then runs on another connection and must
// block on that lock rather than reading past it. Racing two goroutines and hoping for the window
// would make this test pass for timing reasons on a fast machine, which is precisely the kind of
// green that let this defect survive several review rounds.
//
// The assertion is on the OUTCOME, not on the mechanism: whichever order the two take, the
// database must not end up holding an asset whose parent is archived. That bound is independent
// of how the repository chooses to serialize.
func TestCreativeAssetRepo_CreateAsset_SerializesAgainstArchival(t *testing.T) {
	ctx := context.Background()
	pool := creativeAssetTestPool(t)
	repo := NewCreativeAssetRepo(pool)

	briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "draft")
	asset := newTestAsset(t, projectID, briefID)

	// Hold the archival open so the upload cannot slip past it on timing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin archival tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx,
		`UPDATE campaign_briefs SET status = 'archived' WHERE id = $1`, briefID); err != nil {
		t.Fatalf("archive brief inside tx: %v", err)
	}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, cerr := repo.CreateAsset(ctx, asset)
		done <- result{err: cerr}
	}()

	// A correctly serialized CreateAsset takes the same brief row lock and therefore BLOCKS.
	//
	// Assert that POSITIVELY, by asking Postgres which backends are waiting on another
	// backend's lock — not by sleeping and inferring a block from the absence of a return. A
	// timeout proves only that nothing came back within the window, which a goroutine that was
	// never scheduled or was waiting on a pool connection satisfies just as well; the archival
	// would then commit and even an unlocked implementation would read the archived row and
	// return ErrNotFound, passing the test for the wrong reason. Same shape as
	// waitForBlockedBackend in dbtest/audience_lease_live_test.go, which exists for this reason.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case res := <-done:
			if res.err == nil {
				t.Fatal("CreateAsset returned a stored asset while an uncommitted archival held " +
					"the parent brief row: the parent gate read past the concurrent archival " +
					"instead of serializing against it, so this blob can be committed under an " +
					"archived brief. creative_assets has no prune, so that row is unreachable " +
					"storage retained forever")
			}
			t.Fatalf("CreateAsset returned %v while the archival held the row: it did not wait "+
				"on the parent row lock", res.err)
		default:
		}
		var blocked int
		if qerr := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity a
			WHERE a.datname = current_database()
			  AND a.pid <> pg_backend_pid()
			  AND cardinality(pg_blocking_pids(a.pid)) > 0`).Scan(&blocked); qerr != nil {
			t.Fatalf("inspect pg_stat_activity for a blocked backend: %v", qerr)
		}
		if blocked > 0 {
			break // CreateAsset is genuinely waiting on the archival's row lock.
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend ever blocked on the archival transaction's row lock; CreateAsset " +
				"is not taking FOR UPDATE against the parent brief row")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Let the archival win. This test forces the ARCHIVE-FIRST order only: because the archival
	// transaction already owns the lock, CreateAsset must wait, then observe 'archived' and store
	// nothing. The opposite order (upload first) is legitimate and commits — see the port's
	// ordering contract; it is deliberately not what this test exercises.
	if cerr := tx.Commit(ctx); cerr != nil {
		t.Fatalf("commit archival: %v", cerr)
	}

	select {
	case res := <-done:
		if !errors.Is(res.err, domain.ErrNotFound) {
			t.Fatalf("after the archival committed, CreateAsset returned %v, want ErrNotFound: an "+
				"archived brief may not accrue an asset", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CreateAsset never returned after the archival committed")
	}

	// Independent of the call's return value, the database must not hold the row.
	var n int
	if serr := pool.QueryRow(ctx,
		`SELECT count(*) FROM creative_assets WHERE brief_id = $1`, briefID).Scan(&n); serr != nil {
		t.Fatalf("count assets: %v", serr)
	}
	if n != 0 {
		t.Fatalf("found %d creative_assets rows under an archived brief, want 0", n)
	}
}
