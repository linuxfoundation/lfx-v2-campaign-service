// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit CAROUSEL posts from stored images (#661, split from #549 gap 5).
//
// redditConfig.carouselCards names 2 to 6 cards, each a stored PNG/JPEG of the same brief
// (imageAssetId), a headline (the card's caption) and a destination URL. They are authored as one
// CAROUSEL post exactly as reddit_video.go authors a VIDEO post: every card's image is served by
// the signed-asset route at an ordinary image URL, minted late by the client immediately before
// the one post request, and every minted URL is scrubbed from anything that comes back.
// Everything after the post is the image path, unchanged: a refused post degrades to a campaign
// and ad group with no ad (created_degraded, claim retained).

// redditCarouselCard is one card of redditConfig.carouselCards. The image is tagged
// imageAssetId so the snapshot's dangling-asset guard (postgres.snapshotAssetIDs) finds it.
type redditCarouselCard struct {
	ImageAssetID   string `json:"imageAssetId"`
	Headline       string `json:"headline"`
	DestinationURL string `json:"destinationUrl"`
}

// redditCarouselEnabled reports whether this deployment opted in to Reddit carousel posts. Read
// per call, like the other REDDIT_* gates, so it flips without a rebuild.
func redditCarouselEnabled() bool {
	return os.Getenv(constants.EnvRedditCarouselEnabled) == "true"
}

// redditCarouselInvalid is the refusal shape for a carousel config: an ErrCreateConfigInvalid
// (a 400 from PreflightCreate) wrapped as a PreflightRefusal (a job that reached Dispatch copies
// it).
func redditCarouselInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkRedditCarouselChoice applies the config-only carousel rules, shared by PreflightCreate
// (synchronous 400) and Dispatch (again, pre-create, for any path that skipped preflight). It
// returns each card's canonical asset id, or nil when carouselCards is absent or null. The
// deployment gate is checked first, so it never depends on the cards' validity.
//
// An explicit empty array is SUPPLIED, not absent: encoding/json decodes `[]` to a non-nil empty
// slice, and it must reach the gate (400 while the flag is off) and the count check (400 for
// fewer than 2 cards). Treating it as absent would let the create proceed with no creative at
// all — a campaign and ad group with no post, recorded as a clean created.
func (d *RedditDispatcher) checkRedditCarouselChoice(cfg redditConfig) ([]string, error) {
	if cfg.CarouselCards == nil {
		return nil, nil
	}
	if !redditCarouselEnabled() {
		env := constants.EnvRedditCarouselEnabled
		return nil, redditCarouselInvalid("carousel posts are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			env, reddit.FieldCarouselCards, env)
	}
	if strings.TrimSpace(cfg.PostURL) != "" || strings.TrimSpace(cfg.ImageURL) != "" || strings.TrimSpace(cfg.ImageAssetID) != "" ||
		strings.TrimSpace(cfg.VideoAssetID) != "" || strings.TrimSpace(cfg.ThumbnailAssetID) != "" {
		// One creative per ad, as for a video post.
		return nil, redditCarouselInvalid("%s cannot be combined with postUrl, imageUrl, imageAssetId, videoAssetId or thumbnailAssetId", reddit.FieldCarouselCards)
	}
	if redditAdPerVariantEnabled() && len(cfg.Variants) >= 2 {
		// One ad per variant (#549 gap 6) is image-only: a carousel create authors one post and
		// one ad, so the other variants must not be silently dropped.
		return nil, redditCarouselInvalid("%s authors one post and one ad; one ad per variant applies to image posts only, so supply at most one variant", reddit.FieldCarouselCards)
	}
	if err := reddit.ValidateCarouselCardCount(len(cfg.CarouselCards)); err != nil {
		return nil, redditCarouselInvalid("%s", err.Error())
	}
	if d.assetURLs == nil {
		return nil, redditCarouselInvalid("stored creative assets are not enabled for Reddit on this deployment, so %s cannot be served", reddit.FieldCarouselCards)
	}
	ids := make([]string, len(cfg.CarouselCards))
	for i, c := range cfg.CarouselCards {
		raw := strings.TrimSpace(c.ImageAssetID)
		if raw == "" {
			return nil, redditCarouselInvalid("%s[%d]: imageAssetId is required", reddit.FieldCarouselCards, i+1)
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, redditCarouselInvalid("%s[%d]: imageAssetId %s is not a valid asset id", reddit.FieldCarouselCards, i+1, domain.CallerValue(raw))
		}
		if _, _, err := reddit.ValidateCarouselCardText(i, c.Headline, c.DestinationURL); err != nil {
			return nil, redditCarouselInvalid("%s", err.Error())
		}
		ids[i] = id.String()
	}
	return ids, nil
}

// checkRedditCarouselShape refuses, on the raw config, a non-null carouselCards that is not an
// array of objects with string fields. The best-effort decode in PreflightCreate would otherwise
// read a mistyped value as no cards, skip the carousel gate and its 400, and leave Dispatch's
// strict decode to fail the job instead, so this runs first in both places, like
// checkRedditVideoShape.
func checkRedditCarouselShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["redditConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, reddit.FieldCarouselCards) || isJSONNull(raw) {
			continue
		}
		var cards []redditCarouselCard
		if json.Unmarshal(raw, &cards) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: must be an array of {imageAssetId, headline, destinationUrl} objects with string values",
				domain.ErrCreateConfigInvalid, reddit.FieldCarouselCards))
		}
	}
	return nil
}

// checkRedditCarouselAssets proves, before any Reddit call, everything about each card's stored
// image that serving it later depends on: the store is bound, the asset exists FOR THIS BRIEF
// under an active brief and has bytes, it is a stored PNG or JPEG (the image route would
// otherwise 404 Reddit's fetch after nothing could be undone), and its id can be carried in a
// signed URL. Every failure is pre-create.
func (d *RedditDispatcher) checkRedditCarouselAssets(ctx context.Context, brief *model.CampaignBrief, ids []string) error {
	if d.assets == nil {
		return domain.PreflightRefusalf("redditConfig.%s references stored creative assets but the creative-asset store is not configured", reddit.FieldCarouselCards)
	}
	for i, id := range ids {
		field := fmt.Sprintf("%s[%d].imageAssetId", reddit.FieldCarouselCards, i+1)
		if err := d.checkRedditStoredAsset(ctx, brief, field, id, false); err != nil {
			return err
		}
	}
	return nil
}

// redditCarouselInput builds the client's cards: each card's image URL is minted late, as an
// image URL, and appended to minted so the dispatcher scrubs it.
func (d *RedditDispatcher) redditCarouselInput(brief *model.CampaignBrief, cards []redditCarouselCard, ids []string, minted *[]string) []reddit.CarouselCard {
	out := make([]reddit.CarouselCard, len(cards))
	for i, c := range cards {
		out[i] = reddit.CarouselCard{
			ImageURLSource: d.lateAssetURLKind(assetsign.KindImage, brief, ids[i], minted),
			Headline:       c.Headline,
			DestinationURL: c.DestinationURL,
		}
	}
	return out
}
