// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ReconciliationRepository reads the stuck states the service leaves for a human and
// performs the ONE mutation that can be made safe.
//
// Note there is no "adopt" method. Recording an operator-supplied upstream campaign id
// is already expressible through the existing campaign update endpoint (which is
// If-Match gated and writes the same row), so adding a second write path here would
// duplicate that surface with weaker guarantees. See the PR description.
type ReconciliationRepository interface {
	// ListReconciliationItems returns the project's stuck campaign rows and partial
	// audiences, oldest first, capped at limit. The returned total is the true count
	// so a truncated page is honest about what it omitted.
	//
	// minClaimAge excludes rows too young to be considered stuck, so healthy
	// in-flight dispatches are never reported.
	ListReconciliationItems(ctx context.Context, projectID string, minClaimAge time.Duration, limit int) (items []model.ReconciliationItem, total int64, err error)

	// ReleaseDispatchClaimByID deletes a stranded bare claim, freeing the
	// (brief, platform) pair for a future dispatch.
	//
	// It must be safe against a concurrent in-flight dispatch. The implementation
	// therefore re-checks EVERY precondition inside one transaction under FOR UPDATE
	// — the row is still 'pending', still carries no upstream id and no result blob,
	// still matches expectedVersion, and is still older than minAge — because a plain
	// guarded DELETE does not close the window between the operator's read and their
	// write under READ COMMITTED.
	//
	// Returns ErrNotFound when no such campaign exists in the project, ErrConflict
	// when the row exists but is not a releasable bare claim (it has evidence of an
	// upstream create, or is too young), and ErrPreconditionFailed on a version
	// mismatch.
	ReleaseDispatchClaimByID(ctx context.Context, projectID, briefID, campaignID string, expectedVersion int64, minAge time.Duration) (*model.ReconciliationItem, error)
}
