// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// creativeAssetCols is the column list the METADATA-ONLY creative-asset reads scan, in the
// scanner order. It deliberately OMITS bytes so a read that does not need the image never ships
// a multi-megabyte column: the upload endpoint returns metadata only, and GetAssetSize reads
// byte_size precisely so a caller can size an allocation WITHOUT materialising the blob.
//
// One read does need the image — GetAsset, called at Meta dispatch to upload the creative — and
// it appends the column through creativeAssetColsWithBytes below rather than widening this list.
//
// width and height are NULLABLE in the table (migration 000047), so they are read through
// COALESCE(..., 0) rather than scanned into pointers: the model carries them as a plain int pair
// whose zero value already MEANS "unknown", so the two representations agree and no scanner has to
// invent a nil case. 000047's CHECK keeps the pair whole — both set or both NULL — so the
// coalesced zero can never be a half-known shape.
const creativeAssetCols = `id::text, project_id::text, brief_id::text,
	mime_type, byte_size, checksum, COALESCE(width, 0), COALESCE(height, 0),
	created_by, created_at`

// createCreativeAssetQuery inserts an uploaded image under an ACTIVE parent brief and returns
// the stored row, resolving a duplicate upload to the existing row.
//
// The parent-brief gate is INSIDE the write (INSERT ... SELECT ... WHERE EXISTS), not a
// preceding read, for the same reason CreateAudience gates its insert this way: a bare brief_id
// FK check would accept an archived brief and would prove only that the brief exists, not that
// it is this tenant's or still active. When the active, same-project parent is absent the SELECT
// yields no candidate row, nothing is inserted, and RETURNING comes back empty — mapped to
// ErrNotFound.
//
// Since CreateAsset began locking the parent, the tenant boundary and the status rule are
// enforced by the lock query and its status check, which run first and refuse before this
// statement executes. This gate re-states the same predicate under the held lock, so it is
// defense-in-depth that keeps the statement self-contained rather than the clause a
// cross-project attach has to get past.
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
// getCreativeAssetQuery carries the same EXISTS on a non-archived parent, pinned by
// TestCreativeAssetRepo_GetAsset_ReturnsBytesScopedToTenant's "an archived parent brief makes
// the asset unreadable" subtest. That is a lifecycle-visibility rule in its own right — an
// archived brief must refuse its children on every operation — and it is NO LONGER load-bearing
// for this insert's correctness: the insert is correct because it locks, not because a later
// read happens to hide what it stored. Weakening the read would be its own bug, not a
// reopening of the archival race.
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
// RETURNING carries `(xmax = 0) AS inserted`, which is how the caller tells a first upload from
// an idempotent re-upload. The returned ROW cannot distinguish them — the conflict path returns a
// fully-populated asset with a real id and the FIRST upload's created_at — so without this the
// transport could only answer 201 unconditionally, telling a retrying client it created something
// it did not. xmax is the row version's deleting/locking transaction id: an INSERT leaves it 0,
// while the ON CONFLICT DO UPDATE arm writes a new row version whose xmax is this transaction, so
// `xmax = 0` is true for exactly the rows this statement inserted. It is read on the same
// RETURNING as the row itself, so it cannot disagree with what was returned.
//
// This DOES run inside an explicit transaction, because the parent-brief lock and the insert
// have to be one atomic unit (see the ordering argument above). The idempotency key still makes
// a lost commit-ack harmless — the retry returns the row the first attempt committed rather than
// duplicating it — so the transaction is here for ORDERING, not for the reconcile reason
// CreateAudience has one (that insert holds a (brief,platform) build lease whose row a lost ack
// would strand).
//
// width/height are converted to NULL as a PAIR, not column by column. The model's zero pair means
// "dimensions unknown" (see model.CreativeAsset), and the column's meaning for that is NULL —
// writing a literal 0 would both lie and be refused by 000047's CHECK.
//
// Two independent NULLIF(..., 0) calls would be the obvious spelling and would be wrong, because
// they decide each column on its own: a caller holding (1200, 0) would store (1200, NULL), which
// is a half-known shape — a width with no height classifies against nothing, and a reader that
// tests only one column would then treat the row as having a known shape. The CASE pair makes the
// two columns answer one question, so such a row stores (NULL, NULL): honestly unknown, and
// classified as unclassified rather than as the wrong shape. 000047's CHECK refuses the half-known
// pair outright, so this is the writer agreeing with the constraint rather than tripping it.
//
// The predicate is `> 0` rather than `<> 0`, so a NEGATIVE dimension is also stored as unknown
// instead of reaching the CHECK. That is a deliberate widening of what the old NULLIF admitted:
// a negative is not a shape, 000047 refuses it, and the only question is whether the caller
// learns that as SQLSTATE 23514 — a 500 on an upload — or as an honestly unknown pair. No
// reachable caller produces one (image.DecodeConfig cannot), so the choice only decides how a
// repository caller or a backfill misbehaves, and storing unknown is the recoverable half: the
// classifier reports the row as unclassified rather than the write failing outright.
//
// Every upload through internal/service supplies a real pair, so this is for a repository caller
// or a backfill that legitimately has none.
//
// The ON CONFLICT arm deliberately does NOT fill in dimensions on a re-upload of an asset stored
// before 000047. That would make the table insert-only EXCEPT for these two columns, contradicting
// the immutability the model and 000028 both state, and it would backfill only the assets someone
// happened to re-upload — an arbitrary subset. The backfill is a separate step by design; see
// 000047's comment.
const createCreativeAssetQuery = `INSERT INTO creative_assets
		(project_id, brief_id, mime_type, byte_size, checksum, width, height, bytes, created_by)
		SELECT $1, $2, $3, $4, $5,
			CASE WHEN $6::int > 0 AND $7::int > 0 THEN $6::int END,
			CASE WHEN $6::int > 0 AND $7::int > 0 THEN $7::int END,
			$8, $9
		WHERE EXISTS (
			SELECT 1 FROM campaign_briefs
			WHERE id = $2 AND project_id = $1 AND status <> 'archived'
		)
		ON CONFLICT (brief_id, checksum) DO UPDATE SET byte_size = creative_assets.byte_size
		RETURNING ` + creativeAssetCols + `, (xmax = 0) AS inserted`

// creativeAssetQuotaQuery reads a brief's current asset count and total stored bytes, and
// whether THIS upload's checksum is already stored there, in one aggregate over one index.
//
// It never touches the BYTEA: count, byte_size and checksum are all stored outside the blob, so
// the aggregate reads no TOAST pages regardless of how large the brief's library is. It is
// index-served by the UNIQUE (brief_id, checksum) index from 000028, whose leftmost column is
// brief_id — the same index that makes the insert's ON CONFLICT arm cheap.
//
// It is scoped by brief_id ALONE, with no project_id predicate, and that is deliberate rather
// than an omission: the caller has already taken FOR UPDATE on (brief_id, project_id), so the
// brief is proven to be this tenant's before this runs, and an asset row's project_id is the
// brief's by construction. Adding the predicate would narrow nothing while taking the aggregate
// off the (brief_id, checksum) index.
//
// bool_or(...) over zero rows is NULL, so the COALESCE is load-bearing for an empty brief.
const creativeAssetQuotaQuery = `SELECT count(*), COALESCE(sum(byte_size), 0),
		COALESCE(bool_or(checksum = $2), false)
	FROM creative_assets WHERE brief_id = $1`

// lockBriefForAssetWriteQuery is the brief row lock every write that can affect a brief's asset
// library takes first. CreateAsset takes it to order itself against a concurrent ArchiveBrief;
// DeleteAsset takes it to order itself against a concurrent dispatch claim (see
// deleteCreativeAssetQuery); campaign_repo.go's ReplaceCampaign takes it because it rewrites
// config_snapshot, the column DeleteAsset reads to decide whether an asset is still referenced.
//
// FOR UPDATE is the strength that matters, because of what it conflicts with rather than what it
// excludes here: claimCampaignDispatchQuery's INSERT takes FOR KEY SHARE on this same brief row as
// its foreign-key check, and holds it until COMMIT. FOR UPDATE blocks FOR KEY SHARE, so a claim
// that has already inserted forces the delete to wait for that claim to commit — which is exactly
// the ordering the delete needs and cannot get from its own snapshot.
//
// Lock ORDER: a transaction holding this must not go on to take the per-slot advisory lock.
// campaign_repo.go's AdoptCampaign fixes the order as slot -> brief for every transaction that
// takes both, precisely because a claim holds the slot lock while waiting for the brief's.
// The two callers in THIS file take this lock and nothing else, so neither can close that cycle.
// ReplaceCampaign does take both, and takes them in that same slot -> brief order: its caller may
// already hold the per-campaign advisory lock from ClaimCampaignVersion, and the brief lock is the
// opening statement of the replace's own transaction. Nothing acquires a slot lock while holding
// this one, which is the direction that would close the cycle.
const lockBriefForAssetWriteQuery = `SELECT status FROM campaign_briefs
	WHERE id = $1 AND project_id = $2 FOR UPDATE`

// CreateAsset stores an uploaded image and returns it, or ErrNotFound when the parent brief is
// absent, archived, or owned by another project. Idempotent on (brief_id, checksum).
//
// The bool reports whether THIS call inserted the row (true) or resolved to an existing one
// (false), so the transport can answer 201 only for a genuine creation. It is meaningful only
// when err is nil.
//
// Returns ErrCreativeAssetQuotaExceeded when the brief is already at model.MaxCreativeAssetsPerBrief
// assets or this upload would take it past model.MaxCreativeAssetBytesPerBrief — unless the upload
// is a dedupe hit, which stores nothing and is therefore exempt.
func (r *CreativeAssetRepo) CreateAsset(ctx context.Context, a *model.CreativeAsset) (*model.CreativeAsset, bool, error) {
	// Lock the parent brief, then insert on the SAME transaction. The lock is what orders this
	// against a concurrent ArchiveBrief; the insert's own WHERE EXISTS gate is retained because
	// it still enforces tenancy and the active-status rule, and re-reads the row under the lock.
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("create creative asset: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE, not a plain SELECT: a plain read returns the last committed row and would
	// straddle a concurrent archival exactly as the unlocked insert did. Whichever transaction
	// takes this lock first runs to completion before the other observes the row.
	var status string
	if lerr := tx.QueryRow(ctx, lockBriefForAssetWriteQuery, a.BriefID, a.ProjectID).Scan(&status); lerr != nil {
		if errors.Is(lerr, pgx.ErrNoRows) {
			// Absent, or owned by another project. Both are ErrNotFound for the same reason the
			// insert's gate collapses them: telling them apart leaks whether a brief the caller
			// cannot see exists.
			return nil, false, domain.ErrNotFound
		}
		return nil, false, fmt.Errorf("create creative asset: lock brief: %w", lerr)
	}
	if status == "archived" {
		return nil, false, domain.ErrNotFound
	}

	// Quota, decided under the brief lock taken above. The lock is what makes this race-free:
	// two concurrent uploads to the same brief serialize on that row, so the second one counts
	// the first one's asset rather than both reading the same pre-insert total and both passing.
	//
	// The dedupe probe is part of the SAME aggregate, not a second query, because a dedupe hit
	// must be EXEMPT: identical bytes re-sent to the same brief resolve to the existing row and
	// store nothing, so refusing it would make a plain retry fail at exactly the moment the
	// (brief_id, checksum) uniqueness exists to make retrying safe. Reading it separately would
	// let the row appear between the probe and the count.
	var (
		assetCount  int
		bytesUsed   int64
		isDuplicate bool
	)
	if qerr := tx.QueryRow(ctx, creativeAssetQuotaQuery, a.BriefID, a.Checksum).
		Scan(&assetCount, &bytesUsed, &isDuplicate); qerr != nil {
		return nil, false, fmt.Errorf("create creative asset: read quota: %w", qerr)
	}
	if !isDuplicate {
		if assetCount >= model.MaxCreativeAssetsPerBrief ||
			bytesUsed+a.ByteSize > model.MaxCreativeAssetBytesPerBrief {
			return nil, false, domain.ErrCreativeAssetQuotaExceeded
		}
	}

	stored, inserted, err := scanCreativeAssetWithInserted(tx.QueryRow(ctx, createCreativeAssetQuery,
		a.ProjectID, a.BriefID, a.MimeType, a.ByteSize, a.Checksum,
		a.Width, a.Height, a.Bytes, nullJSON(a.CreatedBy)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Reachable only if the brief stopped qualifying between the lock and this insert,
			// which the lock is there to prevent — kept because the gate also enforces tenancy
			// and the status rule, and a gate whose failure path is unhandled is a panic waiting
			// on a future change. Telling the cases apart would leak whether a brief the caller
			// cannot see exists.
			return nil, false, domain.ErrNotFound
		}
		return nil, false, fmt.Errorf("create creative asset: %w", err)
	}

	if cerr := tx.Commit(ctx); cerr != nil {
		return nil, false, fmt.Errorf("create creative asset: commit: %w", cerr)
	}
	return stored, inserted, nil
}

// getCreativeAssetSizeQuery reads ONE column — byte_size — under exactly the same tenant and
// active-parent scope as getCreativeAssetQuery below.
//
// It exists so a caller can learn what an asset WOULD cost before paying for it. The Meta
// dispatcher reserves aggregate memory from this number and only then calls GetAsset; reading
// the size from the row it is about to materialise would be too late, because the blob is
// resident by the time the size is known.
//
// Cheap for the reason migration 000028 records about the byte_size CHECK: byte_size is a
// stored BIGINT, so this never touches the BYTEA and never detoasts. The scope predicate is
// duplicated rather than shared with getCreativeAssetQuery because the two must stay
// identical — a size read that is scoped more loosely than the byte read would let a caller
// probe the existence of another tenant's asset.
const getCreativeAssetSizeQuery = `SELECT byte_size
	FROM creative_assets ca
	WHERE ca.id = $1 AND ca.project_id = $2 AND ca.brief_id = $3
	AND EXISTS (
		SELECT 1 FROM campaign_briefs b
		WHERE b.id = ca.brief_id AND b.project_id = ca.project_id AND b.status <> 'archived'
	)`

// GetAssetSize returns a stored asset's byte_size WITHOUT loading its bytes, or ErrNotFound when
// no such asset exists for that brief. Same scoping and same malformed-id caveat as GetAsset.
func (r *CreativeAssetRepo) GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error) {
	var size int64
	if err := r.db.QueryRow(ctx, getCreativeAssetSizeQuery, assetID, projectID, briefID).Scan(&size); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrNotFound
		}
		return 0, fmt.Errorf("get creative asset size: %w", err)
	}
	return size, nil
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

// getCreativeAssetMetadataQuery is getCreativeAssetQuery with creativeAssetCols in place of
// creativeAssetColsWithBytes: the same row, under the same tenant and active-parent scope, minus
// the blob.
//
// It is a separate statement rather than a GetAsset call whose Bytes the caller drops, for the
// reason getCreativeAssetSizeQuery exists at all: the column holds up to 30 MiB, and shipping it
// across the wire only to discard it is the exact cost creativeAssetCols was introduced to avoid.
// The scope predicate is duplicated verbatim, as the size query's is, because the three must stay
// identical — a metadata read scoped more loosely than the byte read would let a caller probe
// another tenant's assets for existence.
//
// Same malformed-id convention as getCreativeAssetQuery: a well-formed id matching no row is
// ErrNotFound, a malformed one is a 22P02 that surfaces as a 500, and the TRANSPORT is what keeps
// malformed ids out — design/brief.go's asset_id attribute carries Format(FormatUUID).
const getCreativeAssetMetadataQuery = `SELECT ` + creativeAssetCols + `
	FROM creative_assets ca
	WHERE ca.id = $1 AND ca.project_id = $2 AND ca.brief_id = $3
	AND EXISTS (
		SELECT 1 FROM campaign_briefs b
		WHERE b.id = ca.brief_id AND b.project_id = ca.project_id AND b.status <> 'archived'
	)`

// GetAssetMetadata loads a stored asset WITHOUT its bytes, scoped to (projectID, briefID), or
// ErrNotFound when no such asset exists for that brief.
func (r *CreativeAssetRepo) GetAssetMetadata(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	asset, err := scanCreativeAsset(r.db.QueryRow(ctx, getCreativeAssetMetadataQuery, assetID, projectID, briefID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get creative asset metadata: %w", err)
	}
	return asset, nil
}

// listCreativeAssetsQuery returns a brief's assets, oldest first, without their bytes.
//
// It is driven FROM campaign_briefs with a LEFT JOIN rather than from creative_assets, so that
// one statement answers two different questions that a plain SELECT over the assets cannot tell
// apart: a brief with no assets yields exactly one row whose asset columns are NULL, while a
// brief that is absent, archived or another tenant's yields NO rows. Selecting from the assets
// alone returns the empty set for both, which would report a brief the caller cannot see as an
// empty library — the opposite of what every other read of a brief's children does.
//
// ORDER BY created_at, id: created_at alone is not a total order (two uploads can share a
// timestamp), and an unstable order would reshuffle a folder upload's review grid between reads
// for no reason the user could see. The id breaks the tie deterministically.
//
// No LIMIT, by design — see the port doc. The set is bounded by the quota, and a brief that
// predates the quota needs its WHOLE library visible in order to be deleted down to it.
//
// COALESCE(ca.width, 0) is doing two jobs here: the column's own NULL (dimensions unknown,
// migration 000047) and the LEFT JOIN's NULL (no asset row at all) both coalesce to 0, which is
// correct for the first and irrelevant for the second, since the nil id already marks that row
// as the no-assets sentinel.
const listCreativeAssetsQuery = `SELECT ca.id::text, ca.mime_type, ca.byte_size, ca.checksum,
		COALESCE(ca.width, 0), COALESCE(ca.height, 0), ca.created_by, ca.created_at
	FROM campaign_briefs b
	LEFT JOIN creative_assets ca
		ON ca.brief_id = b.id AND ca.project_id = b.project_id
	WHERE b.id = $1 AND b.project_id = $2 AND b.status <> 'archived'
	ORDER BY ca.created_at, ca.id`

// ListAssets returns every asset stored for an active brief (oldest first, without bytes) with
// the brief's count and total stored bytes, or ErrNotFound when the brief is absent, archived or
// another project's.
//
// The totals are summed over exactly the rows returned, in one pass, rather than read from a
// second aggregate query. That is not an optimisation: a separate aggregate takes its own
// snapshot under READ COMMITTED, so an upload committing between the two would publish a count
// that disagrees with the list beside it — and the quota a client reads off that list is what it
// uses to decide whether to keep sending files.
func (r *CreativeAssetRepo) ListAssets(ctx context.Context, projectID, briefID string) ([]*model.CreativeAsset, int, int64, error) {
	rows, err := r.db.Query(ctx, listCreativeAssetsQuery, briefID, projectID)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list creative assets: %w", err)
	}
	defer rows.Close()

	// Non-nil empty rather than nil: an empty library is a real answer that the result type
	// publishes as `assets: []`, and a nil slice would encode it as a missing field.
	assets := []*model.CreativeAsset{}
	var bytesUsed int64
	briefFound := false
	for rows.Next() {
		var (
			id        *string
			mimeType  *string
			byteSize  *int64
			checksum  *string
			width     int
			height    int
			createdBy []byte
			createdAt *time.Time
		)
		if serr := rows.Scan(&id, &mimeType, &byteSize, &checksum, &width, &height, &createdBy, &createdAt); serr != nil {
			return nil, 0, 0, fmt.Errorf("list creative assets: scan: %w", serr)
		}
		briefFound = true
		if id == nil {
			// The LEFT JOIN's no-assets row: the brief exists and holds nothing.
			continue
		}
		a := &model.CreativeAsset{
			ID:        *id,
			ProjectID: projectID,
			BriefID:   briefID,
			MimeType:  derefString(mimeType),
			ByteSize:  derefInt64(byteSize),
			Checksum:  derefString(checksum),
			Width:     width,
			Height:    height,
			CreatedBy: createdBy,
		}
		if createdAt != nil {
			a.CreatedAt = *createdAt
		}
		bytesUsed += a.ByteSize
		assets = append(assets, a)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, 0, 0, fmt.Errorf("list creative assets: %w", rerr)
	}
	if !briefFound {
		return nil, 0, 0, domain.ErrNotFound
	}
	return assets, len(assets), bytesUsed, nil
}

// deleteCreativeAssetQuery resolves the asset, decides whether any live campaign on the brief
// could still need it, and deletes it — in ONE statement against ONE snapshot.
//
// Splitting it would be a bug, not a style choice: between a separate reference check and the
// delete, claimCampaignDispatchQuery could insert a 'pending' campaign row and a dispatch could
// begin reading the very bytes being removed.
//
// ONE SNAPSHOT IS NECESSARY AND NOT SUFFICIENT, and the difference is why DeleteAsset wraps this
// in a transaction that locks the brief first. Under READ COMMITTED a statement's snapshot is
// taken when the statement BEGINS, so this statement closes the gap between its own check and its
// own delete but says nothing about a claim that commits after that instant: such a claim is
// invisible to `referenced`, the delete proceeds, and the dispatcher then resolves a campaign
// whose asset is gone — a failed dispatch rather than the clean 409 the caller should have had.
// lockBriefForAssetWriteQuery is what orders the two, by conflicting with the FOR KEY SHARE the
// claim's insert holds until commit; this statement then runs as the NEXT statement in that
// transaction, with a fresh snapshot that sees any claim which won the race.
//
// The `referenced` arm has three halves.
//
//   - status = 'pending'. The claim row's provisioning state, held from the claim's INSERT until
//     the dispatcher resolves the platform call. This is the arm that actually decides the
//     in-flight case, and it keys on the column the dispatch itself owns.
//   - A NULL config_snapshot. claimCampaignDispatchQuery (campaign_repo.go) inserts the claim row
//     with no snapshot, BEFORE the dispatcher resolves any asset. So a NULL snapshot is not "we
//     cannot tell" — it is the shape a claim is born with.
//   - The id appearing in the snapshot's text. config_snapshot is persisted UNENCRYPTED and its
//     sanitization strips URL query/fragment and credentials but keeps imageAssetId VERBATIM
//     (reddit_asset.go, linkedin.go), so a stored asset reference survives into it as its id.
//
// WHY 'pending' IS A SEPARATE ARM AND NOT REDUNDANT WITH THE NULL ONE. The NULL arm was once
// relied on alone to cover the in-flight window, on the reasoning that a claim inserts NULL and
// nothing else writes the column until the dispatch resolves. The second half of that is false.
// UpdateCampaign (internal/service/brief.go) writes config_snapshot too, it is gated on the
// campaign's VERSION rather than on the brief, and ReplaceCampaign's UPDATE touches `campaigns`
// only — so the brief lock this delete takes does not order the two at all. A config edit landing
// while the campaign is still pending replaces that NULL with the caller's object, and the row
// then matches no arm: the delete proceeds against an asset the in-flight dispatch is about to
// read. 'pending' survives that, because UpdateCampaign cannot write status (it refuses any
// status change) and now refuses a config edit on a pending campaign outright.
//
// The general lesson, recorded because it is cheap to re-break: an arm that keys on a column's
// VALUE is only as good as the full set of writers of that column. 'pending' is durable here
// because the delete and the dispatch are the only writers of status on this path; NULL was not,
// because a second writer existed outside the dispatch flow.
//
// A TEXT SEARCH RATHER THAN A JSON PATH, and the trade is accepted rather than overlooked. The
// snapshot carries an asset id under a different key per platform (redditConfig.imageAssetId,
// twitterConfig.imageAssetId, and so on), so one substring test covers every platform in one
// place where a structured check would need a path per platform and would silently stop matching
// the day a platform adds a sixth. The failure mode decides it: a coincidental match refuses a
// delete that would have been safe (a 409 the operator can act on), while a missed match deletes
// bytes a live campaign still points at. Only one of those is recoverable, so the fuzzier test is
// the safe one here. Revisit if config_snapshot's shape is ever restructured.
//
// Two forms are matched, lower-cased on both sides: the hyphenated form and the 32-character
// dashless form. That is exhaustive over the four shapes uuid.Parse accepts, because the
// `urn:uuid:…` and `{…}` forms each CONTAIN the hyphenated form as a substring.
//
// strpos(...) > 0 rather than LIKE: a UUID contains no LIKE metacharacter today, but a pattern
// built from an identifier is a class of bug worth not having, and strpos treats every character
// literally by construction.
//
// status <> 'deleted' is the live predicate. 'deleted' is the terminal soft-delete state in
// model.Campaign's vocabulary; every other status — including the partial-orphan states
// group_created and unconfirmed — is a row whose config a dispatch or a retry may still read.
//
// The two booleans separate the outcomes the caller must tell apart: no `found` is ErrNotFound,
// found without `removed` is ErrCreativeAssetInUse, and both is success. A single row count
// could not distinguish "no such asset" from "refused".
const deleteCreativeAssetQuery = `WITH target AS (
		SELECT ca.id
		FROM creative_assets ca
		WHERE ca.id = $1 AND ca.project_id = $2 AND ca.brief_id = $3
		AND EXISTS (
			SELECT 1 FROM campaign_briefs b
			WHERE b.id = ca.brief_id AND b.project_id = ca.project_id AND b.status <> 'archived'
		)
	),
	referenced AS (
		SELECT 1
		FROM campaigns c, target t
		WHERE c.brief_id = $3 AND c.project_id = $2
		  AND c.status <> 'deleted'
		  AND (
			c.status = 'pending'
			OR c.config_snapshot IS NULL
			OR strpos(lower(c.config_snapshot::text), lower(t.id::text)) > 0
			OR strpos(lower(c.config_snapshot::text), replace(lower(t.id::text), '-', '')) > 0
		  )
	),
	removed AS (
		DELETE FROM creative_assets
		WHERE id IN (SELECT id FROM target)
		  AND NOT EXISTS (SELECT 1 FROM referenced)
		RETURNING id
	)
	SELECT EXISTS (SELECT 1 FROM target)  AS found,
	       EXISTS (SELECT 1 FROM removed) AS deleted`

// DeleteAsset permanently removes a stored asset, or returns ErrNotFound when no such asset
// exists for that brief, or ErrCreativeAssetInUse when a live campaign on the brief could still
// reference it.
//
// It locks the parent brief and then runs the delete as a SECOND statement on that transaction.
// Both halves are load-bearing: the lock orders this against a dispatch claim that is mid-flight,
// and the second statement is what gives the reference check a snapshot taken AFTER the lock was
// granted. Running the delete as the locking statement itself would leave it on the snapshot the
// transaction already had and reintroduce exactly the race the lock was taken to close.
func (r *CreativeAssetRepo) DeleteAsset(ctx context.Context, projectID, briefID, assetID string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete creative asset: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Exec rather than QueryRow, unlike CreateAsset and the sibling repositories' brief locks:
	// this caller wants the LOCK and not the projection. The delete statement below re-reads the
	// brief under that lock and owns the archived-brief rule (its `target` CTE requires
	// status <> 'archived'), so interpreting the status here would duplicate that rule in a second
	// place, free to drift from it. A row count of zero means the brief is absent or another
	// tenant's — ErrNotFound, as every other access to a brief's children returns.
	tag, lerr := tx.Exec(ctx, lockBriefForAssetWriteQuery, briefID, projectID)
	if lerr != nil {
		return fmt.Errorf("delete creative asset: lock brief: %w", lerr)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	var found, deleted bool
	if err := tx.QueryRow(ctx, deleteCreativeAssetQuery, assetID, projectID, briefID).Scan(&found, &deleted); err != nil {
		return fmt.Errorf("delete creative asset: %w", err)
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return fmt.Errorf("delete creative asset: commit: %w", cerr)
	}
	switch {
	case !found:
		return domain.ErrNotFound
	case !deleted:
		return domain.ErrCreativeAssetInUse
	default:
		return nil
	}
}

// derefString and derefInt64 read a LEFT JOIN's nullable column into the model's plain field.
// They are only ever called for a row whose asset id was non-NULL, where the join matched and
// every NOT NULL column of creative_assets is therefore present; the nil arms exist so a future
// change to the query cannot turn a NULL into a panic.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

// scanCreativeAsset reads one creative_assets row in creativeAssetCols order — every field except
// bytes, which that column list omits. The returned model's Bytes stays nil.
func scanCreativeAsset(row pgx.Row) (*model.CreativeAsset, error) {
	var (
		a         model.CreativeAsset
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &a.MimeType, &a.ByteSize, &a.Checksum,
		&a.Width, &a.Height, &createdBy, &a.CreatedAt,
	); err != nil {
		return nil, err
	}
	a.CreatedBy = createdBy
	return &a, nil
}

// scanCreativeAssetWithBytes reads one creative_assets row in creativeAssetColsWithBytes order —
// the creativeAssetCols fields plus the trailing bytes column into model.Bytes.
func scanCreativeAssetWithBytes(row pgx.Row) (*model.CreativeAsset, error) {
	var (
		a         model.CreativeAsset
		createdBy []byte
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &a.MimeType, &a.ByteSize, &a.Checksum,
		&a.Width, &a.Height, &createdBy, &a.CreatedAt, &a.Bytes,
	); err != nil {
		return nil, err
	}
	a.CreatedBy = createdBy
	return &a, nil
}

// scanCreativeAssetWithInserted reads one creative_assets row in creativeAssetCols order, plus the
// `(xmax = 0) AS inserted` probe that createCreativeAssetQuery appends. It does not scan bytes
// (creativeAssetCols omits them); the returned model's Bytes stays nil.
//
// It is the ONLY row scanner for this table's create path, and the only one that reads the probe:
// no other statement selects that column, and a flag would be meaningless outside an
// INSERT ... ON CONFLICT. Reads that do not need it use scanCreativeAssetWithBytes or scan inline.
func scanCreativeAssetWithInserted(row pgx.Row) (*model.CreativeAsset, bool, error) {
	var (
		a         model.CreativeAsset
		createdBy []byte
		inserted  bool
	)
	if err := row.Scan(
		&a.ID, &a.ProjectID, &a.BriefID, &a.MimeType, &a.ByteSize, &a.Checksum,
		&a.Width, &a.Height, &createdBy, &a.CreatedAt, &inserted,
	); err != nil {
		return nil, false, err
	}
	a.CreatedBy = createdBy
	return &a, inserted, nil
}
