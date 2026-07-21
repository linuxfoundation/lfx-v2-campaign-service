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
