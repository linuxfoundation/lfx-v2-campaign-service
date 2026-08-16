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
// distinct assets never collide on the (brief_id, checksum) key by accident. checksum is returned
// separately so a caller can build a SECOND asset that deliberately reuses it (the idempotency
// case).
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
// different project. Each asserts nothing was stored.
func TestCreativeAssetRepo_CreateAsset_RejectsInactiveOrForeignBrief(t *testing.T) {
	pool := creativeAssetTestPool(t)
	ctx := context.Background()
	repo := NewCreativeAssetRepo(pool)

	t.Run("absent brief", func(t *testing.T) {
		// A well-formed brief id that names nothing. The INSERT ... SELECT finds no active parent,
		// so nothing is inserted and RETURNING is empty → ErrNotFound (no FK violation, because
		// nothing was inserted).
		asset := newTestAsset(t, creativeAssetUniqueID(t, "proj"), "00000000-0000-4000-8000-000000000000")
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound", err)
		}
	})

	t.Run("archived brief", func(t *testing.T) {
		briefID, projectID := insertCreativeAssetTestBrief(ctx, t, pool, "archived")
		asset := newTestAsset(t, projectID, briefID)
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — an archived brief must not accrue assets", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM creative_assets WHERE brief_id=$1`, briefID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("row count = %d, want 0 — an asset was stored under an archived brief", n)
		}
	})

	t.Run("brief owned by another project", func(t *testing.T) {
		briefID, _ := insertCreativeAssetTestBrief(ctx, t, pool, "approved")
		// Same (existing, active) brief, but a caller scoped to a DIFFERENT project. The FK would
		// accept it — the brief exists — but the WHERE project_id gate must not.
		asset := newTestAsset(t, creativeAssetUniqueID(t, "other-proj"), briefID)
		_, err := repo.CreateAsset(ctx, asset)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound — a caller must not attach an asset to another project's brief", err)
		}
	})
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

	t.Run("absent asset is ErrNotFound", func(t *testing.T) {
		_, err := repo.GetAsset(ctx, projectID, briefID, "00000000-0000-4000-8000-000000000000")
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want domain.ErrNotFound", err)
		}
	})
}
