// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// archivedAssetSweepInterval is how often the archived-asset retention sweeper runs. Hourly, like
// jobRetentionSweepInterval and for the same reason: the window is measured in months, so a
// faster cadence would only re-scan for rows that are not there.
const archivedAssetSweepInterval = time.Hour

// archivedAssetPassTimeout bounds one pass (every batch in it), so a slow database cannot wedge
// the goroutine.
const archivedAssetPassTimeout = 2 * time.Minute

// archivedAssetMaxBatchesPerPass caps how many batches one pass deletes. With the repository's
// batch bound that is a few thousand assets an hour, which drains any realistic first-run
// backlog within a day while keeping each pass short; the rest waits for the next tick.
const archivedAssetMaxBatchesPerPass = 50

// SetArchivedAssetPruner binds the repository the archived-asset retention sweeper deletes
// through. Call during construction, before StartArchivedAssetRetentionSweeper. A nil pruner
// leaves the sweeper inert.
func (o *Orchestrator) SetArchivedAssetPruner(p domain.ArchivedCreativeAssetPruner) {
	o.assetPruner = p
}

// StartArchivedAssetRetentionSweeper launches the background goroutine that deletes creative
// assets of briefs archived longer ago than domain.ArchivedBriefAssetRetention (#171). Without it
// creative_assets only ever grows: briefs are never hard-deleted, and an archived brief's images
// can no longer be read by anything.
//
// It mirrors StartJobRetentionSweeper's lifetime: tracked by wg so Shutdown waits for it before
// the pool closes, owned by sweeperCtx, which Shutdown cancels first, so a pass blocked in the
// database is interrupted promptly. It runs on every replica; the repository's cluster-wide
// try-lock lets one replica delete a batch at a time and the others skip it.
func (o *Orchestrator) StartArchivedAssetRetentionSweeper() {
	o.StartArchivedAssetRetentionSweeperWithInterval(archivedAssetSweepInterval)
}

// StartArchivedAssetRetentionSweeperWithInterval is StartArchivedAssetRetentionSweeper with the
// tick injected, so a test can drive the LOOP rather than only the pass.
func (o *Orchestrator) StartArchivedAssetRetentionSweeperWithInterval(interval time.Duration) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-o.sweeperCtx.Done():
				return
			case <-ticker.C:
				o.runArchivedAssetPrune()
			}
		}
	}()
}

// runArchivedAssetPrune performs ONE pass: bounded batches until a batch comes back short (the
// backlog is drained), another replica holds the prune lock, the batch cap is reached, or the
// pass fails. It returns how many assets the pass deleted.
func (o *Orchestrator) runArchivedAssetPrune() int64 {
	if o.assetPruner == nil {
		return 0
	}
	// Derived from sweeperCtx (NOT detached), so Shutdown aborts a pass mid-statement.
	ctx, cancel := context.WithTimeout(o.sweeperCtx, archivedAssetPassTimeout)
	defer cancel()

	var total int64
	batches := 0
	for batches < archivedAssetMaxBatchesPerPass {
		n, ran, err := o.assetPruner.PruneArchivedBriefAssets(ctx, domain.ArchivedBriefAssetRetention, archivedAssetBatchSize)
		if err != nil {
			// Cancellation at Shutdown is expected, not a failure. Otherwise a failed prune costs
			// disk, never correctness: log it and try again next tick.
			if o.sweeperCtx.Err() == nil {
				slog.ErrorContext(o.sweeperCtx, "archived creative asset retention sweep failed",
					"error", err, "deleted", total, "batches", batches)
			}
			return total
		}
		if !ran {
			slog.DebugContext(o.sweeperCtx, "archived creative asset retention sweep skipped: another replica holds the prune lock")
			break
		}
		batches++
		total += n
		if n < int64(archivedAssetBatchSize) {
			break
		}
	}
	if total > 0 {
		slog.InfoContext(o.sweeperCtx, "archived creative asset retention sweep deleted assets",
			"count", total, "batches", batches, "retention", domain.ArchivedBriefAssetRetention.String())
	}
	return total
}

// archivedAssetBatchSize is the batch the sweeper asks for. It must equal what the repository
// actually deletes per batch, because a batch shorter than it is how the pass knows the backlog is
// drained; the sweeper passes it explicitly rather than 0 for that reason.
const archivedAssetBatchSize = 100
