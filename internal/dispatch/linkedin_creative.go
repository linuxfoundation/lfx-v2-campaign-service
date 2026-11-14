// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn creative images from stored assets (LFXV2-2665).
//
// A LinkedIn variant may name a stored creative asset (linkedInConfig.variants[].imageAssetId),
// which becomes that variant's dark-post thumbnail through LinkedIn's Images API (see
// internal/platform/linkedin/images.go). The safety properties are Meta's and Microsoft's:
//
//   - VALIDATE BEFORE MUTATING. Every named asset is resolved within the brief, sized, charged
//     against the shared AssetReserver, loaded and checked against LinkedIn's single image ad
//     limits before CreateCampaign runs. Every failure here is a pre-create error: the claim
//     is released and nothing exists upstream.
//   - UPLOAD AT MOST ONCE. Variants naming the same asset share one upload, and the client
//     uploads only after the campaign is ensured, so every exit after the first upload returns
//     a partial result, the claim is retained, and the orchestrator never re-dispatches it. The
//     minted URNs are recorded on the result (CampaignResult.ImageURNs).
//   - DEFINITE VS UNCONFIRMED. An image failure leaves the campaign created_degraded with zero
//     creatives and a Steps entry saying "NOT uploaded" or "UNCONFIRMED", built from the
//     client's status-only error text, never upstream message text.

// SetCreativeAssetRepo binds the creative-asset read path, mirroring
// MetaDispatcher.SetCreativeAssetRepo: called once by registerDispatchers before the dispatcher
// is shared, and a nil argument is ignored rather than stored as a typed nil.
func (d *LinkedInDispatcher) SetCreativeAssetRepo(r creativeAssetReader) {
	if r == nil {
		return
	}
	d.creatives = r
}

// SetAssetReserver binds the PROCESS-WIDE creative-asset memory budget, the same instance the
// Meta and Microsoft dispatchers hold, so the bound stays one bound across all three.
func (d *LinkedInDispatcher) SetAssetReserver(a *AssetReserver) {
	if a == nil {
		return
	}
	d.assets = a
}

// CreativeAssetRepoIsSet reports whether the creative-asset read path was bound. Exported for
// the container wiring test.
func (d *LinkedInDispatcher) CreativeAssetRepoIsSet() bool { return d.creatives != nil }

// AssetReserverIsSet reports whether the aggregate asset budget was bound.
func (d *LinkedInDispatcher) AssetReserverIsSet() bool { return d.assets != nil }

// SharesAssetReserver reports whether this dispatcher charges exactly a. Exported for the
// container test that proves LinkedIn is on the container's ONE budget rather than a second one.
func (d *LinkedInDispatcher) SharesAssetReserver(a *AssetReserver) bool {
	return a != nil && d.assets == a
}

// linkedinVideoEnabled reports whether this deployment opted in to LinkedIn video Sponsored
// Content (#586). Read per call so it flips without a rebuild. Off by default: the Videos API
// upload and the video dark post have not been exercised against a live LinkedIn account.
func linkedinVideoEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsVideoEnabled) == "true"
}

// checkLinkedInVideoVariants is the synchronous (400) check of the video request shape (#586),
// run by PreflightCreate before a job exists and again by Dispatch. With no variant naming a
// videoAssetId it returns nil, so a config without videos behaves exactly as before. Otherwise it
// refuses: the feature off; a videoAssetId that is not a uuid; a variant carrying a video and an
// image; and a mix of video and non-video variants, which a SINGLE_VIDEO campaign cannot hold.
// Whether the asset exists, and is an MP4 LinkedIn accepts, is decided at dispatch.
func checkLinkedInVideoVariants(cfg linkedinConfig) error {
	videos := 0
	for i, v := range cfg.Variants {
		raw := strings.TrimSpace(v.VideoAssetID)
		if raw == "" {
			continue
		}
		videos++
		if !linkedinVideoEnabled() {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.variants[].videoAssetId is disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
				domain.ErrCreateConfigInvalid, constants.EnvLinkedInAdsVideoEnabled, constants.EnvLinkedInAdsVideoEnabled))
		}
		if _, err := uuid.Parse(raw); err != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.variants[%d].videoAssetId %s is not a valid asset id", domain.ErrCreateConfigInvalid, i, domain.CallerValue(raw)))
		}
		if strings.TrimSpace(v.ImageURN) != "" || strings.TrimSpace(v.ImageAssetID) != "" {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.variants[%d] supplies a videoAssetId together with an image; a video variant carries no image", domain.ErrCreateConfigInvalid, i))
		}
	}
	if videos > 0 && videos != len(cfg.Variants) {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.variants mixes %d video variant(s) with %d without a videoAssetId; a LinkedIn video campaign (format SINGLE_VIDEO) takes only video creatives, so give every variant a videoAssetId or none", domain.ErrCreateConfigInvalid, videos, len(cfg.Variants)-videos))
	}
	return nil
}

// resolveLinkedInImages loads and validates every stored image AND video (#586) cfg's variants
// name, returning nil maps when none does. On success each variant's ImageAssetID/VideoAssetID
// is replaced with the canonical uuid (so the snapshot never stores a caller spelling), and the
// returned release func owns ONE reservation for all distinct assets of both kinds, held until
// the dispatch returns. One reservation rather than one per kind, so a dispatch never holds part
// of the shared budget while waiting for the rest. On any failure, including a panic, the
// reservation is released here and the returned func is a no-op.
//
// Every asset is looked up SCOPED to the brief (project, brief, asset), so an asset of another
// brief or project is "does not exist for this brief", never another tenant's image or video.
func (d *LinkedInDispatcher) resolveLinkedInImages(ctx context.Context, brief *model.CampaignBrief, cfg *linkedinConfig) (map[string]linkedin.ImageUpload, map[string]linkedin.VideoUpload, func(), error) {
	noop := func() {}
	var order, videoOrder []string
	seen := map[string]bool{}
	isVideo := map[string]bool{}
	for i := range cfg.Variants {
		v := &cfg.Variants[i]
		rawVideo := strings.TrimSpace(v.VideoAssetID)
		v.VideoAssetID = ""
		if rawVideo != "" {
			parsed, err := uuid.Parse(rawVideo)
			if err != nil {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d].videoAssetId %s is not a valid asset id", i, domain.CallerValue(rawVideo))
			}
			if strings.TrimSpace(v.ImageURN) != "" || strings.TrimSpace(v.ImageAssetID) != "" {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d] supplies a videoAssetId together with an image; a video variant carries no image", i)
			}
			v.VideoAssetID = parsed.String()
			if seen[v.VideoAssetID] && !isVideo[v.VideoAssetID] {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants name creative asset %s as both an image and a video", v.VideoAssetID)
			}
			if !seen[v.VideoAssetID] {
				seen[v.VideoAssetID], isVideo[v.VideoAssetID] = true, true
				videoOrder = append(videoOrder, v.VideoAssetID)
			}
			continue
		}
		raw := strings.TrimSpace(v.ImageAssetID)
		if raw == "" {
			v.ImageAssetID = ""
			continue
		}
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d].imageAssetId %s is not a valid asset id", i, domain.CallerValue(raw))
		}
		if strings.TrimSpace(v.ImageURN) != "" {
			return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d] supplies both imageUrn and imageAssetId; supply one", i)
		}
		v.ImageAssetID = parsed.String()
		if isVideo[v.ImageAssetID] {
			return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants name creative asset %s as both an image and a video", v.ImageAssetID)
		}
		if !seen[v.ImageAssetID] {
			seen[v.ImageAssetID] = true
			order = append(order, v.ImageAssetID)
		}
	}
	all := append(append([]string{}, order...), videoOrder...)
	if len(all) == 0 {
		return nil, nil, noop, nil
	}
	if d.creatives == nil {
		return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s but the creative-asset store is not configured", all[0])
	}

	// PRICE BEFORE LOADING: one BIGINT per asset, so an oversized asset is refused on its
	// byte_size and never materialised, and the reservation is taken before any blob is read.
	var total int64
	for _, assetID := range all {
		size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s, which does not exist for this brief", assetID)
			}
			return nil, nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
		}
		if isVideo[assetID] {
			if size < linkedin.MinVideoBytes || size > linkedin.MaxVideoBytes {
				return nil, nil, noop, domain.PreflightRefusalf("creative asset %s is %d bytes; LinkedIn video ads accept %d to %d bytes", assetID, size, linkedin.MinVideoBytes, linkedin.MaxVideoBytes)
			}
		} else if size > linkedin.MaxImageBytes {
			return nil, nil, noop, domain.PreflightRefusalf("creative asset %s is %d bytes; LinkedIn single image ads accept at most %d bytes", assetID, size, linkedin.MaxImageBytes)
		}
		total += size
	}
	if total > maxVariantAssetBytes {
		return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference %d bytes of distinct creative assets; at most %d may be used in one dispatch", total, maxVariantAssetBytes)
	}

	release := noop
	succeeded := false
	defer func() {
		if !succeeded {
			release()
		}
	}()
	rel, ok := d.assets.reserve(ctx, total)
	if !ok {
		return nil, nil, noop, domain.PreflightRefusalf("creative assets (%d bytes) exceed the memory concurrently available for dispatch; retry when other dispatches finish", total)
	}
	release = rel

	var images map[string]linkedin.ImageUpload
	var videos map[string]linkedin.VideoUpload
	for _, assetID := range all {
		asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s, which does not exist for this brief", assetID)
			}
			return nil, nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
		}
		if asset == nil || len(asset.Bytes) == 0 {
			// Refused rather than skipped: the caller asked for an image or a video, and silently
			// creating an ad without it would spend on a creative nobody approved.
			return nil, nil, noop, domain.PreflightRefusalf("creative asset %s has no stored bytes", assetID)
		}
		if isVideo[assetID] {
			if asset.MimeType != model.MimeTypeMP4 {
				return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants name creative asset %s as videoAssetId, but it is an image; name it as imageAssetId", assetID)
			}
			vid := linkedin.VideoUpload{Data: asset.Bytes, MimeType: asset.MimeType}
			if err := linkedin.ValidateVideo(vid); err != nil {
				return nil, nil, noop, domain.WrapPreflightRefusal(fmt.Errorf("creative asset %s: %w", assetID, err))
			}
			if videos == nil {
				videos = map[string]linkedin.VideoUpload{}
			}
			videos[assetID] = vid
			continue
		}
		if asset.MimeType == model.MimeTypeMP4 {
			return nil, nil, noop, domain.PreflightRefusalf("linkedInConfig.variants name creative asset %s as imageAssetId, but it is a video; name it as videoAssetId", assetID)
		}
		img := linkedin.ImageUpload{Data: asset.Bytes, MimeType: asset.MimeType}
		if err := linkedin.ValidateImage(img); err != nil {
			// Size, shape and format only (ValidateImage renders the recorded MIME through
			// domain.CallerValue): operator-safe, and raised before any LinkedIn request.
			return nil, nil, noop, domain.WrapPreflightRefusal(fmt.Errorf("creative asset %s: %w", assetID, err))
		}
		if images == nil {
			images = map[string]linkedin.ImageUpload{}
		}
		images[assetID] = img
	}
	succeeded = true
	return images, videos, release, nil
}
