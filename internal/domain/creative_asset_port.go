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
	CreateAsset(ctx context.Context, a *model.CreativeAsset) (*model.CreativeAsset, error)

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
}
