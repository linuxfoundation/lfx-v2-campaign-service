// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// CreativeAssetRepo is a pgx-backed implementation of domain.CreativeAssetRepository.
type CreativeAssetRepo struct {
	db *Pool
}

// NewCreativeAssetRepo returns a CreativeAssetRepo backed by pool.
func NewCreativeAssetRepo(pool *Pool) *CreativeAssetRepo { return &CreativeAssetRepo{db: pool} }

var _ domain.CreativeAssetRepository = (*CreativeAssetRepo)(nil)

// creativeAssetCols is the column list every creative-asset read scans, in scanCreativeAsset
// order. It deliberately OMITS bytes: no caller of this repo needs the image back yet (the
// upload endpoint returns metadata only, and the byte-loading read used at dispatch lands with
// the Meta image step), so shipping a multi-megabyte column out of every write would be pure
// waste. A reader that needs the bytes selects them explicitly.
const creativeAssetCols = `id::text, project_id::text, brief_id::text,
	mime_type, byte_size, checksum, created_by, created_at`

// createCreativeAssetQuery inserts an uploaded image under an ACTIVE parent brief and returns
// the stored row, resolving a duplicate upload to the existing row.
//
// The parent-brief gate is INSIDE the write (INSERT ... SELECT ... WHERE EXISTS), not a
// preceding read, for the same reason CreateAudience gates its insert this way: a bare brief_id
// FK check would accept an archived brief and would let a caller scoped to project A attach an
// asset to a brief owned by project B (the FK only proves the brief exists, not that it is this
// tenant's or still active). When the active, same-project parent is absent the SELECT yields no
// candidate row, nothing is inserted, and RETURNING comes back empty — mapped to ErrNotFound.
//
// ON CONFLICT (brief_id, checksum) DO UPDATE — not DO NOTHING — is what makes a repeat upload
// return the EXISTING asset. DO NOTHING suppresses the RETURNING clause on a conflict (Postgres
// emits no row for a row it did not touch), so the caller could not tell "stored" from "already
// present" and would see ErrNoRows for a duplicate. The SET is a deliberate no-op
// (byte_size = the stored value): it changes nothing, and MUST NOT copy the incoming values over
// the stored ones, because a matching checksum already means the bytes are identical and the
// FIRST uploader's created_by must stay — a later re-upload by a different actor does not
// re-author the asset it merely re-sent. The no-op exists only to make the conflicting row
// eligible for RETURNING.
//
// No explicit transaction wraps this, unlike CreateAudience. That insert holds a (brief,platform)
// build lease whose row a lost commit-ack would strand, so it reconciles inside a tx; a creative
// asset carries no lease and no external side effect, and its idempotency key makes a
// lost-ack retry harmless — the retry returns the row the first attempt committed rather than
// duplicating it. A single autocommit statement is therefore both sufficient and safer (no
// connection held across a reconcile loop).
const createCreativeAssetQuery = `INSERT INTO creative_assets
		(project_id, brief_id, mime_type, byte_size, checksum, bytes, created_by)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE EXISTS (
			SELECT 1 FROM campaign_briefs
			WHERE id = $2 AND project_id = $1 AND status <> 'archived'
		)
		ON CONFLICT (brief_id, checksum) DO UPDATE SET byte_size = creative_assets.byte_size
		RETURNING ` + creativeAssetCols

// CreateAsset stores an uploaded image and returns it, or ErrNotFound when the parent brief is
// absent, archived, or owned by another project. Idempotent on (brief_id, checksum).
func (r *CreativeAssetRepo) CreateAsset(ctx context.Context, a *model.CreativeAsset) (*model.CreativeAsset, error) {
	stored, err := scanCreativeAsset(r.db.QueryRow(ctx, createCreativeAssetQuery,
		a.ProjectID, a.BriefID, a.MimeType, a.ByteSize, a.Checksum, a.Bytes, nullJSON(a.CreatedBy)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No active parent brief for (project, brief): missing, archived, or another
			// project's. All three are ErrNotFound — none may accrue an asset, and telling
			// them apart would leak whether a brief the caller cannot see exists.
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("create creative asset: %w", err)
	}
	return stored, nil
}

// scanCreativeAsset reads one creative_assets row in creativeAssetCols order. It does not scan
// bytes (creativeAssetCols omits them); the returned model's Bytes stays nil.
func scanCreativeAsset(row pgx.Row) (*model.CreativeAsset, error) {
	var (
		a         model.CreativeAsset
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &a.MimeType, &a.ByteSize, &a.Checksum,
		&createdBy, &a.CreatedAt,
	); err != nil {
		return nil, err
	}
	a.CreatedBy = createdBy
	return &a, nil
}
