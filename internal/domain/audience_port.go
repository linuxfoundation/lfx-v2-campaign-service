// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// AudienceRepository persists built campaign audiences (the "B2" resource: a
// pointer + provenance to a platform-side audience, not its contents). All reads are
// tenant-scoped by projectID.
type AudienceRepository interface {
	// CreateAudience inserts a new audience row and returns it (with its generated
	// id/version/timestamps).
	CreateAudience(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error)
	// CreateAudienceForApprovedBrief inserts the row ONLY if the parent brief is approved,
	// returning ErrStaleApproval otherwise, and reports the brief VERSION it observed under
	// the lock.
	//
	// It takes no expected version, and that is the point: this is the FIRST blocking call
	// the build makes, so there is no earlier read for a caller to have pinned. Reading the
	// brief before claiming would put a database round-trip ahead of the lease, and every
	// step ahead of the lease is a window in which a second request for the same brief finds
	// nothing to conflict with. The version comes back instead, and the caller re-checks it
	// immediately before the first upstream call — which is where the guard has to be, since
	// the build reads the brief and resolves past editions (a warehouse round-trip) after
	// claiming and before creating any HubSpot list. A plain CreateAudience only checks
	// `status <> 'archived'`, so without the approval gate the build could create REAL
	// HubSpot lists from a brief that is no longer approved. Mirrors
	// JobRepo.CreateJobForApprovedBrief in intent, not in signature.
	CreateAudienceForApprovedBrief(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, int64, error)
	// GetAudience returns one audience by id, scoped to (project, brief), or
	// ErrNotFound.
	GetAudience(ctx context.Context, projectID, briefID, id string) (*model.CampaignAudience, error)
	// ListAudiences returns a brief's audiences (newest first), scoped to the project.
	ListAudiences(ctx context.Context, projectID, briefID string) ([]*model.CampaignAudience, error)
	// UpdateAudience replaces the mutable fields of an audience, gated on
	// expectedVersion for optimistic concurrency: it returns ErrPreconditionFailed
	// when the stored version differs and ErrNotFound when the row is absent. On
	// success it bumps the version and returns the updated row.
	UpdateAudience(ctx context.Context, a *model.CampaignAudience, expectedVersion int64) (*model.CampaignAudience, error)
}
