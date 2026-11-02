// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package domain

import (
	"context"
	"time"
)

// ArchivedBriefAssetRetention is how long a creative asset is kept after its brief is archived
// before the retention sweeper deletes it (#171).
//
// Nothing serves or dispatches such an asset once its brief is archived: every read of its bytes
// or size, every dispatch path and the upload are scoped to an ACTIVE parent brief, and
// ArchiveBrief is one-way. The window is not
// there for the service; it is the grace an operator has to notice an archival made in error and
// recover the images out of band before the bytes are gone. Ninety days is the product decision
// recorded on #171. A constant, not an environment variable: it is a policy, not a per-deployment
// tuning knob.
const ArchivedBriefAssetRetention = 90 * 24 * time.Hour

// ArchivedCreativeAssetPruner deletes the creative assets of briefs archived longer ago than a
// retention window. It is separate from CreativeAssetRepository because only the retention
// sweeper uses it, and no request path may.
type ArchivedCreativeAssetPruner interface {
	// PruneArchivedBriefAssets deletes at most limit assets whose parent brief has been archived
	// for longer than olderThan, as ONE bounded batch, and returns how many it deleted.
	//
	// ran is false when another replica is pruning at that moment: the implementation takes a
	// cluster-wide try-lock for the batch and skips it rather than waiting, so concurrent sweeps on
	// several replicas never delete the same rows twice or queue behind each other. A batch that
	// did not run deleted nothing and is not an error.
	//
	// It is idempotent: an asset already deleted is simply not found again. A non-positive
	// olderThan falls back to ArchivedBriefAssetRetention, and a non-positive limit to the
	// implementation's own batch bound, which also caps a larger limit, so a zero value never
	// reads as "delete everything".
	PruneArchivedBriefAssets(ctx context.Context, olderThan time.Duration, limit int) (deleted int64, ran bool, err error)
}
