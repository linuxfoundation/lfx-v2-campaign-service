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
// The gate alone does NOT serialize against archival, which is why CreateAsset wraps it in a
// transaction that LOCKS the parent brief first. A bare guarded insert takes no lock on the
// parent, so under READ COMMITTED the statement's snapshot can see an active brief while a
// concurrent ArchiveBrief commits, and the asset lands under a brief that is archived by the
// time the insert does. That is not theoretical: it reproduces deterministically against a live
// database (TestCreativeAssetRepo_CreateAsset_SerializesAgainstArchival) — an uncommitted
// ArchiveBrief holding the row does not block the unlocked insert.
//
// The single-statement atomicity of INSERT ... SELECT is not the property needed here, and it is
// worth being exact about why, because "it is one statement" is the intuition that makes the
// unlocked version look sufficient. Atomicity means the statement does not half-apply; it says
// nothing about what the statement's snapshot may miss. Under READ COMMITTED each statement
// takes a FRESH snapshot of the last committed state, and a concurrent non-key status update
// does not conflict with the FK's key-share lock — so the EXISTS subquery can read a brief that
// another transaction is in the act of archiving. campaign_repo.go's lockAdoptBriefQuery states
// the same rule from the other side: what is required is FOR UPDATE, "not a plain re-read, and
// not the single-statement atomicity of the INSERT".
//
// This matches the treatment the rest of this package already gives a brief-parented write --
// AudienceRepo.CreateAudienceForApprovedBrief, BriefRepo's guarded update, and
// campaign_repo.go's lockAdoptBriefQuery all take SELECT ... FOR UPDATE on campaign_briefs
// before writing a child. CreateAsset was the outlier, not the exception.
//
// The cost is narrow and worth naming: uploads to the SAME brief serialize on that brief's row
// for the duration of the insert. Uploads to different briefs never contend, because the lock is
// per-brief-row rather than table-wide.
//
// Why an unreachable blob is not an acceptable loss, which is what the earlier reasoning here
// assumed: creative_assets has no prune, and briefs are never hard-deleted (archive is a soft
// status flip), so a row committed under an archived brief is retained FOREVER with nothing that
// can read it or clean it up. Storage that only grows and is unreachable by every code path is a
// different category from a row a later operation would refuse.
//
// The second of those triggers is CHECKED rather than left to review. getCreativeAssetQuery's
// EXISTS on a non-archived parent is what makes "nothing can reach it" true, and
// TestCreativeAssetRepo_GetAsset_ReturnsBytesScopedToTenant's "an archived parent brief makes
// the asset unreadable" subtest fails if it is dropped. That subtest is therefore load-bearing
// for THIS decision as well as for its own lifecycle-consistency point, and says so, so that
// weakening it cannot quietly remove this insert's justification.
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
// "No-op" describes the VALUES, not the write: a duplicate upload still takes a row lock and
// writes a new row version, leaving a dead tuple for autovacuum. On a table of multi-megabyte
// rows that is worth knowing, though it is harmless at the expected volume -- re-uploading the
// same image to the same brief is a retry, not a routine path -- and the alternative
// (DO NOTHING plus a follow-up SELECT) costs a second round trip on every duplicate and
// reintroduces the read-after-write race this single statement avoids.
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
	// Lock the parent brief, then insert on the SAME transaction. The lock is what orders this
	// against a concurrent ArchiveBrief; the insert's own WHERE EXISTS gate is retained because
	// it still enforces tenancy and the active-status rule, and re-reads the row under the lock.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("create creative asset: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE, not a plain SELECT: a plain read returns the last committed row and would
	// straddle a concurrent archival exactly as the unlocked insert did. Whichever transaction
	// takes this lock first runs to completion before the other observes the row.
	var status string
	lockQ := `SELECT status FROM campaign_briefs WHERE id = $1 AND project_id = $2 FOR UPDATE`
	if lerr := tx.QueryRow(ctx, lockQ, a.BriefID, a.ProjectID).Scan(&status); lerr != nil {
		if errors.Is(lerr, pgx.ErrNoRows) {
			// Absent, or owned by another project. Both are ErrNotFound for the same reason the
			// insert's gate collapses them: telling them apart leaks whether a brief the caller
			// cannot see exists.
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("create creative asset: lock brief: %w", lerr)
	}
	if status == "archived" {
		return nil, domain.ErrNotFound
	}

	stored, err := scanCreativeAsset(tx.QueryRow(ctx, createCreativeAssetQuery,
		a.ProjectID, a.BriefID, a.MimeType, a.ByteSize, a.Checksum, a.Bytes, nullJSON(a.CreatedBy)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Reachable only if the brief stopped qualifying between the lock and this insert,
			// which the lock is there to prevent — kept because the gate also enforces tenancy
			// and the status rule, and a gate whose failure path is unhandled is a panic waiting
			// on a future change. Telling the cases apart would leak whether a brief the caller
			// cannot see exists.
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("create creative asset: %w", err)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, fmt.Errorf("create creative asset: commit: %w", cerr)
	}
	return stored, nil
}

// creativeAssetColsWithBytes is creativeAssetCols plus the bytes column, for the ONE read that
// needs the image itself — GetAsset, called at Meta dispatch to upload the creative. bytes is
// last so scanCreativeAssetWithBytes can reuse scanCreativeAsset's field order and append one
// scan target. Every OTHER read stays on the bytes-free creativeAssetCols so it never ships a
// multi-megabyte column it does not use.
const creativeAssetColsWithBytes = creativeAssetCols + `, bytes`

// getCreativeAssetQuery loads one asset by id, scoped to (project, brief) AND to an ACTIVE parent
// brief. The scope lives in the WHERE clause, not a post-read check: a row under another project
// or brief simply does not match, so it comes back as pgx.ErrNoRows → ErrNotFound, and a variant
// can never reference a creative owned by a different tenant or a different brief.
//
// The EXISTS on a non-archived parent mirrors GetAudience, and exists for the reason stated
// there: once a brief is archived its child resources leave the live lifecycle, so a read must
// 404 rather than leaving the nested resource readable while CreateAsset has already begun
// refusing it. Without it, archival would be half-applied — create 404s, get still serves the
// bytes — which is the inconsistency, not a safety margin. It stays a single round trip. The id is compared as the UUID
// primary key ($1 bound as the caller's validated asset id, exactly as GetBrief compares its id),
// so the PK index serves the lookup.
//
// Note what that shared convention does NOT give you: the column is uncast, so a MALFORMED id
// is a Postgres 22P02 (invalid input syntax for uuid), which falls through to the wrapped-error
// return below and surfaces as a 500 -- not ErrNotFound. Only a WELL-FORMED id matching no row
// is a 404. GetBrief and GetAudience behave identically, so this is the repo's convention
// rather than a local choice, and the port doc states the obligation it creates: the CALLER
// validates assetID and briefID before calling. The endpoint that will do so lands with the
// upload work; until then there is no untrusted path into this function.
const getCreativeAssetQuery = `SELECT ` + creativeAssetColsWithBytes + `
	FROM creative_assets ca
	WHERE ca.id = $1 AND ca.project_id = $2 AND ca.brief_id = $3
	AND EXISTS (
		SELECT 1 FROM campaign_briefs b
		WHERE b.id = ca.brief_id AND b.project_id = ca.project_id AND b.status <> 'archived'
	)`

// GetAsset loads a stored asset with its bytes, scoped to (projectID, briefID), or ErrNotFound
// when no such asset exists for that brief (absent, or owned by another project/brief).
func (r *CreativeAssetRepo) GetAsset(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	asset, err := scanCreativeAssetWithBytes(r.db.QueryRow(ctx, getCreativeAssetQuery, assetID, projectID, briefID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get creative asset: %w", err)
	}
	return asset, nil
}

// scanCreativeAssetWithBytes reads one creative_assets row in creativeAssetColsWithBytes order —
// scanCreativeAsset's fields plus the trailing bytes column into model.Bytes.
func scanCreativeAssetWithBytes(row pgx.Row) (*model.CreativeAsset, error) {
	var (
		a         model.CreativeAsset
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &a.MimeType, &a.ByteSize, &a.Checksum,
		&createdBy, &a.CreatedAt, &a.Bytes,
	); err != nil {
		return nil, err
	}
	a.CreatedBy = createdBy
	return &a, nil
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
