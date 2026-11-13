// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ReindexAction names which index message a resource gets: "updated" for a live one and
// "deleted" for an archived brief or a soft-deleted campaign. The values mirror
// indexer.ActionUpdated and indexer.ActionDeleted, which this package does not import.
const (
	reindexActionUpdated = "updated"
	reindexActionDeleted = "deleted"
)

// ReindexBatch is the outcome of one batch: how many resources were read, how many of them
// were enqueued as live and as deleted, how many were skipped, and the id to continue after.
type ReindexBatch struct {
	Read    int
	Live    int
	Deleted int
	Skipped int
	// LastID is the id of the last row read, "" when the batch read nothing. The next batch
	// continues strictly after it.
	LastID string
}

// BriefPayloadFunc and CampaignPayloadFunc build the outbox payload for one resource and
// action. The reindex command passes service.BriefIndexPayload / CampaignIndexPayload, so a
// re-enqueued message is byte-for-byte the message a write would have produced.
type (
	BriefPayloadFunc    func(action string, b *model.CampaignBrief) ([]byte, error)
	CampaignPayloadFunc func(action string, c *model.Campaign) ([]byte, error)
)

// ReindexRepo re-enqueues index messages for existing briefs and campaigns (#590). It writes
// NOTHING but index_outbox rows; the running relay publishes them like any other write's.
//
// Each batch is one transaction that reads its rows FOR SHARE and enqueues their messages
// before committing. Every write path updates its row and enqueues its own message in one
// transaction while holding the row lock, so a write in flight blocks this read until it
// commits (and this batch then reads the newer row), and a write that starts after this read
// waits for this batch and enqueues after it. Either way the outbox's per-resource order ends
// on the latest state, and a re-run only re-sends the current document.
type ReindexRepo struct{ db *Pool }

// NewReindexRepo returns a ReindexRepo over db.
func NewReindexRepo(db *Pool) *ReindexRepo { return &ReindexRepo{db: db} }

const reindexBriefsQuery = `SELECT ` + briefCols + ` FROM campaign_briefs
	WHERE ($1 = '' OR project_id = $1) AND ($2 = '' OR id > $2::uuid)
	ORDER BY id LIMIT $3 FOR SHARE`

// reindexCampaignsQuery excludes nothing: a bare dispatch claim is filtered in Go
// (isBareClaim) so the decision is counted as a skip rather than silently absent.
const reindexCampaignsQuery = `SELECT ` + campaignCols + ` FROM campaigns
	WHERE ($1 = '' OR project_id = $1) AND ($2 = '' OR id > $2::uuid)
	ORDER BY id LIMIT $3 FOR SHARE`

// ReindexBriefs reads up to limit briefs after afterID (optionally only projectID's) and, when
// apply is set, enqueues one message each: "deleted" for an archived brief, "updated"
// otherwise. Without apply the transaction is rolled back, so nothing is written.
func (r *ReindexRepo) ReindexBriefs(ctx context.Context, projectID, afterID string, limit int, apply bool, build BriefPayloadFunc) (ReindexBatch, error) {
	return r.batch(ctx, reindexBriefsQuery, projectID, afterID, limit, apply, func(ctx context.Context, tx pgx.Tx, rows pgx.Rows, out *ReindexBatch) error {
		var briefs []*model.CampaignBrief
		for rows.Next() {
			b, err := scanBrief(rows)
			if err != nil {
				return fmt.Errorf("scan brief: %w", err)
			}
			briefs = append(briefs, b)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, b := range briefs {
			out.Read++
			out.LastID = b.ID
			action := reindexActionUpdated
			if b.Status == model.BriefArchived {
				action = reindexActionDeleted
			}
			if err := enqueueReindex(ctx, tx, apply, indexObjectTypeBrief, b.ID, func() ([]byte, error) { return build(action, b) }); err != nil {
				return err
			}
			if action == reindexActionDeleted {
				out.Deleted++
			} else {
				out.Live++
			}
		}
		return nil
	})
}

// ReindexCampaigns is ReindexBriefs for campaigns: "deleted" for a soft-deleted row,
// "updated" for any other, and a bare dispatch claim (see isBareClaim) is skipped because no
// write ever indexed it.
func (r *ReindexRepo) ReindexCampaigns(ctx context.Context, projectID, afterID string, limit int, apply bool, build CampaignPayloadFunc) (ReindexBatch, error) {
	return r.batch(ctx, reindexCampaignsQuery, projectID, afterID, limit, apply, func(ctx context.Context, tx pgx.Tx, rows pgx.Rows, out *ReindexBatch) error {
		var campaigns []*model.Campaign
		for rows.Next() {
			c, err := scanCampaign(rows)
			if err != nil {
				return fmt.Errorf("scan campaign: %w", err)
			}
			campaigns = append(campaigns, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, c := range campaigns {
			out.Read++
			out.LastID = c.ID
			if isBareClaim(c) {
				out.Skipped++
				continue
			}
			action := reindexActionUpdated
			if c.Status == model.CampaignStatusDeleted {
				action = reindexActionDeleted
			}
			if err := enqueueReindex(ctx, tx, apply, indexObjectTypeCampaign, c.ID, func() ([]byte, error) { return build(action, c) }); err != nil {
				return err
			}
			if action == reindexActionDeleted {
				out.Deleted++
			} else {
				out.Live++
			}
		}
		return nil
	})
}

// isBareClaim reports a dispatch claim that never became a campaign: still pending, with no
// upstream id and no result. ClaimCampaignDispatch does not enqueue an index message, and a
// write that does (UpsertCampaign) always gives the row an id or a result, so re-indexing a
// bare claim would publish a document no write ever published.
func isBareClaim(c *model.Campaign) bool {
	return c.Status == model.CampaignStatusPending && c.PlatformCampaignID == "" && len(c.Result) == 0
}

func enqueueReindex(ctx context.Context, tx pgx.Tx, apply bool, objectType, objectID string, build func() ([]byte, error)) error {
	payload, err := build()
	if err != nil {
		return fmt.Errorf("build index payload for %s %s: %w", objectType, objectID, err)
	}
	if !apply {
		return nil
	}
	return enqueueIndexMessage(ctx, tx, objectType, objectID, payload)
}

func (r *ReindexRepo) batch(ctx context.Context, query, projectID, afterID string, limit int, apply bool,
	each func(context.Context, pgx.Tx, pgx.Rows, *ReindexBatch) error) (out ReindexBatch, err error) {
	if limit <= 0 {
		return out, errors.New("reindex: batch limit must be positive")
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("reindex: begin: %w", err)
	}
	defer func() {
		if err != nil || !apply {
			_ = tx.Rollback(ctx)
		}
	}()
	rows, err := tx.Query(ctx, query, projectID, afterID, limit)
	if err != nil {
		return out, fmt.Errorf("reindex: query: %w", err)
	}
	defer rows.Close()
	if err = each(ctx, tx, rows, &out); err != nil {
		return ReindexBatch{}, err
	}
	if apply {
		if err = tx.Commit(ctx); err != nil {
			return ReindexBatch{}, fmt.Errorf("reindex: commit: %w", err)
		}
	}
	return out, nil
}
