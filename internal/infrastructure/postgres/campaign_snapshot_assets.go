// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// snapshotAssetIDKey is the object key, LOWERCASED, under which a campaign's config snapshot
// names a stored creative asset. Matching is case-insensitive, and that is a property of the
// data rather than a convenience:
//
//   - config_snapshot is the CALLER'S OWN JSON, not a marshalled Go struct. UpdateCampaign
//     stores redactedConfigSnapshot(p.Campaign.Config) (service/brief.go), which re-marshals a
//     map[string]any — so whatever spelling the caller sent is the spelling that is stored.
//   - Every dispatcher binds that JSON into its config struct with encoding/json, whose field
//     matching IS case-insensitive. `imageAssetId`, `ImageAssetID` and `IMAGEASSETID` therefore
//     all reach the same field and all name a real asset to the dispatcher that resolves it.
//     An extractor that pinned one spelling would silently skip the others, which is the same
//     class of silent miss as a lock predicate that matches no row.
//
// Both spellings are live IN THIS REPO today, so this is not a hypothetical: the Reddit, X and
// Microsoft configs give ImageAssetID the json tag "imageAssetId" (dispatch/reddit.go,
// dispatch/twitter.go, dispatch/microsoft.go), while meta.AdVariant and linkedin.CreativeVariant
// carry NO json tag (platform/meta/client.go, platform/linkedin/client.go) and so round-trip as
// PascalCase `ImageAssetID` under `variants`.
const snapshotAssetIDKey = "imageassetid"

// snapshotAssetIDs returns the creative-asset ids a config snapshot names, in their canonical
// uuid spelling. An absent, empty or null snapshot names none, which is not an error.
//
// Extraction is driven by the KEY, not by the shape of the values: a snapshot legitimately
// carries uuids that are not creative assets — the brief, the project, an audience, a platform's
// own campaign id — and collecting every uuid-shaped token would refuse perfectly good configs
// for assets that were never referenced.
//
// Values that are not valid uuids are skipped rather than reported. They are a real error, but
// not THIS function's: the dispatchers already refuse an unparseable asset id with a message
// that names the offending value ("… which is not a valid asset id"), and creative_assets.id is
// a uuid column, so carrying one into the lookup below would raise a cast error instead of a
// refusal.
//
// The walk recurses without a depth bound. That adds no new reachability: redactSnapshotValue
// (service/campaign_config_redact.go) already walks the same decoded value the same way on the
// way IN, so any input deep enough to trouble this one never reaches storage.
func snapshotAssetIDs(snapshot []byte) (map[string]struct{}, error) {
	trimmed := bytes.TrimSpace(snapshot)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil, fmt.Errorf("decode config snapshot: %w", err)
	}
	out := map[string]struct{}{}
	collectSnapshotAssetIDs(decoded, out)
	return out, nil
}

// collectSnapshotAssetIDs adds every canonicalized asset id reachable from v to out.
//
// Canonicalizing through uuid.Parse().String() is what makes the set comparable at all:
// uuid.Parse accepts four spellings of the same id — canonical, braced, URN and unhyphenated —
// so a raw string is not a stable identity. Without it a caller could re-send an id it already
// had in a different spelling and have it counted as newly introduced.
func collectSnapshotAssetIDs(v any, out map[string]struct{}) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			s, isString := val.(string)
			if !isString {
				collectSnapshotAssetIDs(val, out)
				continue
			}
			if !strings.EqualFold(k, snapshotAssetIDKey) {
				continue
			}
			if id, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
				out[id.String()] = struct{}{}
			}
		}
	case []any:
		for _, val := range t {
			collectSnapshotAssetIDs(val, out)
		}
	}
}

// storedCampaignSnapshotQuery reads the snapshot currently on the row, so a replace can tell
// which of the asset ids it is about to store are NEW. It runs on the replace's own transaction,
// after the brief lock, and takes no lock of its own: the guarded UPDATE that follows is what
// decides the row's fate, and a FOR UPDATE here would only widen what the replace holds.
//
// status <> 'deleted' mirrors replaceCampaignQuery's guard for the same reason
// replaceCampaignExistsQuery does: a soft-deleted row is treated as absent everywhere else, and
// a predicate that disagreed with the UPDATE's would make this read answer about a row the
// UPDATE will not touch.
const storedCampaignSnapshotQuery = `SELECT config_snapshot FROM campaigns
	WHERE id = $1 AND brief_id = $2 AND project_id = $3 AND status <> 'deleted'`

// presentCreativeAssetsQuery reports which of the given ids exist for a (project, brief).
//
// It is scoped to the tenant pair ALONE — deliberately NOT to the non-archived-parent EXISTS
// that getCreativeAssetQuery and getCreativeAssetSizeQuery carry. Those scope a READ, where
// hiding an archived brief's children is the point. This decides whether a row is THERE, and the
// hazard being closed is a concurrent DELETE removing it. Borrowing the archival predicate would
// make every config-bearing edit on an archived brief that names any asset start failing with a
// 409 — a refusal this change has no business introducing, since a replace under an archived
// brief is allowed today and ReplaceCampaign's brief lock deliberately ignores the status it
// reads.
const presentCreativeAssetsQuery = `SELECT id::text FROM creative_assets
	WHERE project_id = $1 AND brief_id = $2 AND id = ANY($3::uuid[])`

// checkNewSnapshotAssetRefs refuses a replace whose incoming config_snapshot NEWLY names a
// creative asset that does not exist for the campaign's (project, brief).
//
// WHY IT HAS TO BE HERE, INSIDE THE TRANSACTION. DeleteAsset decides whether an asset is still
// referenced by reading config_snapshot under the brief row lock. ReplaceCampaign takes that
// same lock as its opening statement — so exactly one of the two runs first, and the loser sees
// what the winner committed. The delete winning first is the direction the lock alone does not
// fix: it finds no reference, removes the row, commits, and the replace then writes a snapshot
// naming an id nothing resolves. This check closes that direction by asking the question on the
// losing side, under the lock the winner had to release.
//
// It cannot be hoisted to the service layer for the same reason: a check outside this
// transaction is a check the delete can invalidate between the answer and the UPDATE.
//
// WHY ONLY NEWLY INTRODUCED IDS. The scope is the difference between closing a hazard and
// breaking four callers. ReplaceCampaign writes every mutable column, so brief_bid.go,
// brief_budget.go, brief.go's status persist and meta_ad_sets.go's persistMetaAdSetRecord all
// re-write the snapshot they read without changing it. Validating the whole snapshot would make
// any of them start failing the moment the brief held one already-dangling reference — a bid
// edit refused because of an asset nobody in that request mentioned. Diffing against the stored
// snapshot confines the refusal to the one caller that can actually introduce a reference.
//
// The cost of that scope, stated rather than glossed: a reference that is ALREADY dangling stays
// dangling, and this returns nil for it. That is pre-existing state, and it is not left
// unhandled — every asset-consuming dispatcher resolves the id before its first upstream call
// and refuses pre-create on ErrNotFound (dispatch/reddit_asset.go, meta.go, linkedin_creative.go,
// twitter_creative.go, microsoft_creative.go), so the claim is released and no paid resource is
// created.
//
// WHY UpsertCampaign IS NOT GUARDED TOO, though it is the only other statement that writes
// config_snapshot (upsertCampaignQuery). The two writes mean opposite things. ReplaceCampaign
// records an INTENT a caller may still revise, so refusing it costs that caller a retry.
// UpsertCampaign records a campaign the dispatcher has ALREADY created upstream, from the
// sanitized snapshot of the config it just dispatched; refusing there would reject the local
// record of a live, spending ad and leave exactly the orphan this repo treats as its most
// serious defect class. The asset is also not in question on that path — the dispatcher resolved
// it before its first upstream call, which is the same pre-create refusal named above. So the
// asymmetry is the point: the write that can still be taken back is checked, the write that is
// reporting an accomplished fact is not.
func (r *CampaignRepo) checkNewSnapshotAssetRefs(ctx context.Context, tx pgx.Tx, c *model.Campaign) error {
	incoming, err := snapshotAssetIDs(c.ConfigSnapshot)
	if err != nil {
		return fmt.Errorf("replace campaign: incoming snapshot: %w", err)
	}
	if len(incoming) == 0 {
		return nil
	}

	var stored []byte
	if serr := tx.QueryRow(ctx, storedCampaignSnapshotQuery, c.ID, c.BriefID, c.ProjectID).Scan(&stored); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			// No row to compare against, so nothing here is "newly" introduced. Say nothing and
			// let the guarded UPDATE below answer: it is the one place that tells missing
			// (404) from stale version (412), and a second verdict invented here would only
			// differ from it.
			return nil
		}
		return fmt.Errorf("replace campaign: read stored snapshot: %w", serr)
	}
	// The stored value came out of a jsonb column, so it decodes or the column was never valid
	// JSON in the first place; the error is surfaced rather than swallowed so that a snapshot
	// nothing can read is not silently treated as naming no assets.
	existing, err := snapshotAssetIDs(stored)
	if err != nil {
		return fmt.Errorf("replace campaign: stored snapshot: %w", err)
	}

	added := make([]string, 0, len(incoming))
	for id := range incoming {
		if _, carried := existing[id]; !carried {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil
	}
	sort.Strings(added)

	rows, qerr := tx.Query(ctx, presentCreativeAssetsQuery, c.ProjectID, c.BriefID, added)
	if qerr != nil {
		return fmt.Errorf("replace campaign: resolve creative assets: %w", qerr)
	}
	present := map[string]struct{}{}
	for rows.Next() {
		var id string
		if scerr := rows.Scan(&id); scerr != nil {
			rows.Close()
			return fmt.Errorf("replace campaign: resolve creative assets: %w", scerr)
		}
		present[id] = struct{}{}
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("replace campaign: resolve creative assets: %w", rerr)
	}

	missing := make([]string, 0, len(added))
	for _, id := range added {
		if _, ok := present[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// The ids are safe to carry to the caller, and naming them is what makes the 409
	// actionable when a config references several assets: each one was supplied by this very
	// request and canonicalized by uuid.Parse. Nothing a platform returned, and nothing read
	// from the stored row, is in this message.
	return fmt.Errorf("%w: %s", domain.ErrCreativeAssetReferenceMissing, strings.Join(missing, ", "))
}
