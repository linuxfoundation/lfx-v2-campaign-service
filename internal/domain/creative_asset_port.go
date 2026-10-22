// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// CreativeAssetRepository persists uploaded creative images, each subordinate to a brief.
// All operations are tenant-scoped by projectID.
//
// An asset is INSERT-ONLY: its bytes are immutable once stored (see model.CreativeAsset), so
// there is no update method here and never will be — a changed image is a NEW asset with a new
// checksum, and re-uploading identical bytes is resolved by returning the existing row rather
// than mutating one.
type CreativeAssetRepository interface {
	// CreateAsset stores an uploaded image under an ACTIVE parent brief and returns the stored
	// row (with its generated id/created_at).
	//
	// The parent-brief gate is part of the write, not a preceding read: the insert takes effect
	// only if an active brief exists for (a.ProjectID, a.BriefID), and CreateAsset returns
	// ErrNotFound when none does — a missing, archived, or cross-project brief are one outcome
	// to the caller, none of which may accrue an asset. Requiring only ACTIVE (not approved) is
	// deliberate: uploading a creative is part of COMPOSING a brief, which happens before
	// approval, unlike campaign creation which gates on approval.
	//
	// The gate IS serialized against archival, and that is part of the contract rather than an
	// implementation detail. The guarantee is an ORDERING one, and it is worth stating exactly
	// because the stronger-sounding version is false: an implementation MUST make this call and
	// ArchiveBrief take effect in some serial order, so the two outcomes are
	//   - archival first  → this call observes the archived brief and returns ErrNotFound; and
	//   - this call first → the asset commits under a still-active brief, and the archival
	//     applies afterwards to a brief that now has one more asset.
	// What is EXCLUDED is the interleaving where both appear to win: an upload admitted against
	// a brief the archival has already committed. A guarded insert alone does not exclude it,
	// because under READ COMMITTED its snapshot can observe an active brief while the archival
	// commits. The Postgres implementation takes SELECT ... FOR UPDATE on the parent brief and
	// inserts on that transaction.
	//
	// It is required rather than optional because the consequence is permanent: creative_assets
	// has no prune and briefs are never hard-deleted, so an asset committed under an archived
	// brief is unreachable storage retained forever.
	//
	// It is idempotent on (BriefID, Checksum): re-uploading identical bytes to the same brief
	// returns the EXISTING asset rather than storing a second copy or raising a conflict. The
	// checksum is content-derived (a.Checksum is the SHA-256 the caller computed over a.Bytes),
	// so "same checksum" means "same image", and a second upload is a no-op that returns what
	// is already there — the same id every time, which is what lets the caller (and the Meta
	// image-upload step at dispatch) treat upload as safe to retry.
	//
	// The second return reports whether THIS call performed the insert: true when the row was
	// created, false when an existing row was returned by the idempotent path. It exists because
	// the two outcomes are indistinguishable in the returned row — the idempotent path returns a
	// fully-populated asset with a real id and the FIRST upload's created_at — and the HTTP layer
	// has to tell them apart to answer 201 (created) rather than 200 (already existed). Without
	// it the transport can only report creation unconditionally, which tells a retrying client it
	// created something it did not.
	CreateAsset(ctx context.Context, a *model.CreativeAsset) (asset *model.CreativeAsset, created bool, err error)

	// GetAsset loads a single stored asset — INCLUDING its Bytes — by id, scoped to
	// (projectID, briefID) AND to an ACTIVE parent brief. The scope is part of the lookup, not
	// a check after the read: an id that exists but belongs to another project, another brief,
	// or a brief that has since been ARCHIVED resolves to ErrNotFound, so a caller can never
	// dereference a creative it does not own or one that has left the live lifecycle. The four
	// cases collapse to one outcome for the same reason CreateAsset's parent-brief gate does —
	// telling them apart would leak whether an asset the caller cannot see exists.
	//
	// Requiring an ACTIVE parent here mirrors CreateAsset's gate and GetAudience's read: an
	// archived brief must refuse its children on EVERY operation, or archival is half-applied
	// — creates refused while reads still serve the bytes.
	//
	// Unlike the metadata-only rows CreateAsset returns (bytes are multi-megabyte and no write
	// caller needs them back), GetAsset DOES load Bytes: its one caller is the Meta dispatch
	// step, which resolves a variant's imageAssetId to the image it uploads to the ad account.
	// The id is expected to be a well-formed asset id — the caller validates untrusted input
	// before calling, as the sibling repositories' id lookups assume (see GetBrief/GetAudience).
	GetAsset(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error)

	// GetAssetSize returns byte_size WITHOUT loading the bytes, under the same (project, brief)
	// and active-parent scope as GetAsset.
	//
	// It exists so a caller can bound an allocation BEFORE making it. The Meta dispatcher
	// reserves aggregate asset memory from this figure and only then calls GetAsset: reading the
	// size off the row it is about to materialise would be too late, because the blob is already
	// resident by the time its size is known. That ordering is the whole point — an aggregate
	// budget charged after the memory is held bounds nothing.
	//
	// Same malformed-id obligation as GetAsset: the CALLER validates assetID and briefID.
	GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error)

	// GetAssetMetadata loads one stored asset WITHOUT its Bytes, under the same (project, brief)
	// and active-parent scope as GetAsset — the same four cases collapse to ErrNotFound.
	//
	// It is a fourth method rather than a GetAsset call whose Bytes the caller ignores, for
	// GetAssetSize's reason one step further: an asset row holds up to 30 MiB of BYTEA, and the
	// read that serves the metadata endpoint has no use for any of it. Shipping it from the
	// database and then discarding it is precisely what creativeAssetCols exists to prevent.
	// GetAssetSize is the same arrangement with a narrower projection still; this one returns the
	// whole row minus the blob because the endpoint publishes mime type, size, checksum and
	// dimensions together.
	//
	// Same malformed-id obligation as GetAsset: the CALLER validates assetID and briefID.
	GetAssetMetadata(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error)

	// ListAssets returns every asset stored for an ACTIVE brief, WITHOUT Bytes, oldest first,
	// alongside the brief's current totals — the count and the summed byte_size of exactly those
	// rows. A missing, archived or cross-project brief is ErrNotFound, as it is for every other
	// operation here.
	//
	// It takes NO limit and returns no cursor, deliberately. Rows written once the quota exists
	// are bounded by model.MaxCreativeAssetsPerBrief; a brief that predates it may hold more, and
	// that full set is exactly what an operator must see in order to delete down to the cap — so
	// truncating would hide the very rows the quota's 409 is pointing at.
	//
	// The totals are returned here rather than left for the caller to compute from the slice
	// because they are what the upload path's quota check compares against. Deriving them in the
	// caller would reimplement that check in a second place, free to drift from it.
	ListAssets(ctx context.Context, projectID, briefID string) (assets []*model.CreativeAsset, count int, bytesUsed int64, err error)

	// DeleteAsset permanently removes one stored asset, under the same (project, brief) and
	// active-parent scope as GetAsset.
	//
	// A HARD delete, unlike a brief or a campaign, which are retired by a status flip: there is no
	// soft-delete column on creative_assets, and reclaiming the BYTEA is the entire reason the
	// operation exists — a tombstoned row would still hold its blob and still count against the
	// brief's quota.
	//
	// It is REFUSED with ErrCreativeAssetInUse when any campaign on the brief with status other
	// than 'deleted' could still need the asset: one whose config_snapshot names the asset id, or
	// one whose config_snapshot is NULL. The NULL arm is exact rather than cautious — the dispatch
	// claim inserts its 'pending' campaign row before the dispatcher reads any asset, so that is
	// precisely the window in which a referencing snapshot does not exist yet.
	//
	// The found-but-refused and not-found outcomes are decided in ONE statement against ONE
	// snapshot. Checking references in a separate round trip would leave a window in which a
	// dispatch claims a campaign between the check and the delete.
	//
	// Same malformed-id obligation as GetAsset: the CALLER validates assetID and briefID.
	DeleteAsset(ctx context.Context, projectID, briefID, assetID string) error
}
