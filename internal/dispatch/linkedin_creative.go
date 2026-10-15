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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
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

// resolveLinkedInImages loads and validates every stored image cfg's variants name, returning
// nil when none does. On success each variant's ImageAssetID is replaced with the canonical
// uuid (so the snapshot never stores a caller spelling), and the returned release func owns ONE
// reservation for all distinct assets, held until the dispatch returns. On any failure,
// including a panic, the reservation is released here and the returned func is a no-op.
//
// Every asset is looked up SCOPED to the brief (project, brief, asset), so an asset of another
// brief or project is "does not exist for this brief", never another tenant's image.
func (d *LinkedInDispatcher) resolveLinkedInImages(ctx context.Context, brief *model.CampaignBrief, cfg *linkedinConfig) (map[string]linkedin.ImageUpload, func(), error) {
	noop := func() {}
	var order []string
	seen := map[string]bool{}
	for i := range cfg.Variants {
		v := &cfg.Variants[i]
		raw := strings.TrimSpace(v.ImageAssetID)
		if raw == "" {
			v.ImageAssetID = ""
			continue
		}
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d].imageAssetId %s is not a valid asset id", i, domain.CallerValue(raw))
		}
		if strings.TrimSpace(v.ImageURN) != "" {
			return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants[%d] supplies both imageUrn and imageAssetId; supply one", i)
		}
		v.ImageAssetID = parsed.String()
		if !seen[v.ImageAssetID] {
			seen[v.ImageAssetID] = true
			order = append(order, v.ImageAssetID)
		}
	}
	if len(order) == 0 {
		return nil, noop, nil
	}
	if d.creatives == nil {
		return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s but the creative-asset store is not configured", order[0])
	}

	// PRICE BEFORE LOADING: one BIGINT per asset, so an oversized asset is refused on its
	// byte_size and never materialised, and the reservation is taken before any blob is read.
	var total int64
	for _, assetID := range order {
		size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s, which does not exist for this brief", assetID)
			}
			return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
		}
		if size > linkedin.MaxImageBytes {
			return nil, noop, domain.PreflightRefusalf("creative asset %s is %d bytes; LinkedIn single image ads accept at most %d bytes", assetID, size, linkedin.MaxImageBytes)
		}
		total += size
	}
	if total > maxVariantAssetBytes {
		return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference %d bytes of distinct creative assets; at most %d may be used in one dispatch", total, maxVariantAssetBytes)
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
		return nil, noop, domain.PreflightRefusalf("creative assets (%d bytes) exceed the memory concurrently available for dispatch; retry when other dispatches finish", total)
	}
	release = rel

	images := make(map[string]linkedin.ImageUpload, len(order))
	for _, assetID := range order {
		asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, noop, domain.PreflightRefusalf("linkedInConfig.variants reference creative asset %s, which does not exist for this brief", assetID)
			}
			return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
		}
		if asset == nil || len(asset.Bytes) == 0 {
			// Refused rather than skipped: the caller asked for an image, and silently creating
			// an image-less ad would spend on a creative nobody approved.
			return nil, noop, domain.PreflightRefusalf("creative asset %s has no stored image bytes", assetID)
		}
		img := linkedin.ImageUpload{Data: asset.Bytes, MimeType: asset.MimeType}
		if err := linkedin.ValidateImage(img); err != nil {
			// Size, shape and format only (ValidateImage renders the recorded MIME through
			// domain.CallerValue): operator-safe, and raised before any LinkedIn request.
			return nil, noop, domain.WrapPreflightRefusal(fmt.Errorf("creative asset %s: %w", assetID, err))
		}
		images[assetID] = img
	}
	succeeded = true
	return images, release, nil
}
