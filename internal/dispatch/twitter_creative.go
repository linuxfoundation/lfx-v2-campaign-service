// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// SetCreativeAssetRepo binds the creative-asset read path so a twitterConfig.imageAssetId
// resolves to image bytes at dispatch. Same opt-in shape, and the same nil handling, as
// MetaDispatcher.SetCreativeAssetRepo: registerDispatchers calls it once before the dispatcher
// is shared, and a nil argument leaves it unbound rather than storing a typed nil.
func (d *TwitterDispatcher) SetCreativeAssetRepo(r creativeAssetReader) {
	if r == nil {
		return
	}
	d.creatives = r
}

// SetAssetReserver binds the PROCESS-WIDE creative-asset memory budget (see AssetReserver).
// registerDispatchers hands every asset-consuming dispatcher the SAME reserver, because the
// bound is across concurrent dispatches of any platform, not per platform.
func (d *TwitterDispatcher) SetAssetReserver(a *AssetReserver) {
	if a == nil {
		return
	}
	d.assets = a
}

// CreativeAssetRepoIsSet reports whether the creative-asset read path was bound. Exported for
// the container's wiring test, for the reason MetaDispatcher.CreativeAssetRepoIsSet gives.
func (d *TwitterDispatcher) CreativeAssetRepoIsSet() bool { return d.creatives != nil }

// AssetReserverIsSet reports whether the aggregate asset budget was bound.
func (d *TwitterDispatcher) AssetReserverIsSet() bool { return d.assets != nil }

// SharesAssetReserver is MetaDispatcher.SharesAssetReserver for the X dispatcher.
func (d *TwitterDispatcher) SharesAssetReserver(a *AssetReserver) bool {
	return a != nil && d.assets == a
}

// resolveTwitterImage loads the creative image an X config references by imageAssetId.
//
// Every refusal here precedes any X request, and the caller wraps it in notCreated so the claim
// is released: a malformed id, an id combined with a request that authors no tweet, an unbound
// store, an asset that does not exist FOR THIS BRIEF (GetAssetSize / GetAsset are scoped by
// project and brief, so another brief's or project's asset is not-found), an asset over X's
// image limit (refused on its stored SIZE, before the blob is loaded), no budget, or an empty
// blob. An image that cannot be attached must not degrade to an imageless tweet: the caller
// asked for this creative, and spending on another one is the outcome to avoid.
//
// The release func is always non-nil. On success it owns the aggregate reservation, held until
// the client has uploaded the bytes; on failure it is a no-op because nothing is held.
func (d *TwitterDispatcher) resolveTwitterImage(ctx context.Context, brief *model.CampaignBrief, cfg twitterConfig, tweetID string) (*twitter.ImageInput, func(), error) {
	noop := func() {}
	raw := strings.TrimSpace(cfg.ImageAssetID)
	if raw == "" {
		return nil, noop, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return nil, noop, fmt.Errorf("twitter config references creative asset %s, which is not a valid asset id", safeAssetIDForError(raw))
	}
	assetID := parsed.String()
	// Judged on the SAME normalized inputs the client's authoring condition reads (tweetID is
	// already trimmed by Dispatch), so the two cannot disagree about whether a tweet is authored.
	if tweetID != "" || strings.TrimSpace(cfg.TweetText) == "" {
		return nil, noop, fmt.Errorf("twitter imageAssetId %s is attached only to an authored tweet, so it requires tweetText and no tweetId", assetID)
	}
	if d.creatives == nil {
		return nil, noop, fmt.Errorf("twitter config references creative asset %s but the creative-asset store is not configured", assetID)
	}
	size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, fmt.Errorf("twitter config references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
	}
	if size > twitter.MaxImageBytes {
		return nil, noop, fmt.Errorf("creative asset %s is %d bytes, over X's %d-byte image limit; upload a smaller image", assetID, size, twitter.MaxImageBytes)
	}
	release, ok := d.assets.reserve(ctx, size)
	if !ok {
		return nil, noop, fmt.Errorf("creative asset %s (%d bytes) exceeds the memory concurrently available for dispatch; retry when other dispatches finish", assetID, size)
	}
	// Structural unwind, as in resolveVariantAssets: every exit but the one success return —
	// including a panic out of the repository — gives the reservation back.
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, fmt.Errorf("twitter config references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
	}
	if asset == nil || len(asset.Bytes) == 0 {
		return nil, noop, fmt.Errorf("creative asset %s has no stored image bytes", assetID)
	}
	handedOff = true
	return &twitter.ImageInput{Bytes: asset.Bytes, MIME: asset.MimeType}, release, nil
}
