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
)

// Google creative assets, resolved from the library rather than fetched (LFXV2-2665).
//
// This is MetaDispatcher.resolveVariantAssets applied to Google's fourteen image slots, and it is
// deliberately the same shape rather than a second scheme: dedupe by CANONICALIZED uuid, price
// each distinct asset with GetAssetSize (one BIGINT, no BYTEA), apply the per-dispatch ceiling to
// the SIZE, reserve against the process-wide AssetReserver BEFORE the read, then GetAsset — and
// unwind every reservation structurally on any failure, panics included.
//
// What is NOT shared with Meta is validation. The bytes an asset ref resolves to are handed to
// the platform client and run through exactly the checks a FETCHED image runs through — the
// per-image size cap, image.DecodeConfig rather than a Content-Type sniff, and the slot's minimum
// size and aspect ratio. Nothing about those checks is a property of HTTP, and an asset that
// skipped them would be the one way to get a 601x314 image past the preflight and into the ad
// mutate, which runs after the budget and campaign exist. See fetchSlotImages.

// SetCreativeAssetRepo binds the creative-asset read path, mirroring
// MetaDispatcher.SetCreativeAssetRepo: called once by registerDispatchers before the dispatcher
// is shared, and a nil argument is ignored rather than stored as a typed nil.
func (d *GoogleAdsDispatcher) SetCreativeAssetRepo(r creativeAssetReader) {
	if r == nil {
		return
	}
	d.creatives = r
}

// SetAssetReserver binds the PROCESS-WIDE creative-asset memory budget — the same instance the
// Meta, Microsoft, Twitter and LinkedIn dispatchers hold, so the bound stays one bound across
// all of them.
func (d *GoogleAdsDispatcher) SetAssetReserver(a *AssetReserver) {
	if a == nil {
		return
	}
	d.assets = a
}

// CreativeAssetRepoIsSet reports whether the creative-asset read path was bound. Exported for
// the container wiring test, for the reason MetaDispatcher.CreativeAssetRepoIsSet gives.
func (d *GoogleAdsDispatcher) CreativeAssetRepoIsSet() bool { return d.creatives != nil }

// AssetReserverIsSet reports whether the aggregate asset budget was bound.
func (d *GoogleAdsDispatcher) AssetReserverIsSet() bool { return d.assets != nil }

// SharesAssetReserver is MetaDispatcher.SharesAssetReserver for the Google dispatcher.
func (d *GoogleAdsDispatcher) SharesAssetReserver(a *AssetReserver) bool {
	return a != nil && d.assets == a
}

// googleAdsCreativeSlot is one named image slot of one creative, addressed by pointer so the
// resolver can write the canonical uuid back into the config the mappers then read.
type googleAdsCreativeSlot struct {
	label string
	refs  *googleAdsImageRefs
}

// googleAdsCreativeSlots enumerates every image slot the config carries, across all three
// creative channels.
//
// One list rather than three loops: the budget, the dedupe and the per-dispatch ceiling are
// properties of the DISPATCH, not of a channel, and a config may name the same asset from two
// channels. Enumerating in one place is also what keeps a slot added later from quietly missing
// the resolution — a new slot that is not in this list resolves nothing and would fail at the
// fetch, which is a far later and much worse place to find out.
func googleAdsCreativeSlots(cfg *googleAdsConfig) []googleAdsCreativeSlot {
	var out []googleAdsCreativeSlot
	if c := cfg.DemandGenCreative; c != nil {
		out = append(out,
			googleAdsCreativeSlot{"demand gen marketingImages", &c.MarketingImages},
			googleAdsCreativeSlot{"demand gen squareMarketingImages", &c.SquareMarketingImages},
			googleAdsCreativeSlot{"demand gen portraitImages", &c.PortraitImages},
			googleAdsCreativeSlot{"demand gen tallPortraitImages", &c.TallPortraitImages},
			googleAdsCreativeSlot{"demand gen logoImages", &c.LogoImages},
		)
	}
	if c := cfg.PerformanceMaxCreative; c != nil {
		out = append(out,
			googleAdsCreativeSlot{"performance max marketingImages", &c.MarketingImages},
			googleAdsCreativeSlot{"performance max squareMarketingImages", &c.SquareMarketingImages},
			googleAdsCreativeSlot{"performance max portraitImages", &c.PortraitImages},
			googleAdsCreativeSlot{"performance max logoImages", &c.LogoImages},
			googleAdsCreativeSlot{"performance max landscapeLogoImages", &c.LandscapeLogoImages},
		)
	}
	if c := cfg.DisplayCreative; c != nil {
		out = append(out,
			googleAdsCreativeSlot{"display marketingImages", &c.MarketingImages},
			googleAdsCreativeSlot{"display squareMarketingImages", &c.SquareMarketingImages},
			googleAdsCreativeSlot{"display logoImages", &c.LogoImages},
			googleAdsCreativeSlot{"display squareLogoImages", &c.SquareLogoImages},
		)
	}
	return out
}

// resolveGoogleCreativeAssets loads every asset-backed image entry in cfg and returns the bytes
// keyed by the slot string the platform client will see (googleAdsImageRefScheme + canonical id),
// together with the releaser for the reservations it took.
//
// It CANONICALIZES each id in place, so the entries the mappers go on to render carry the same
// spelling as the map keys. A config naming one asset through two valid uuid spellings —
// canonical and braced, say — therefore resolves to one key, is read once, charged once, and is
// fetched, size-budgeted and reported once on the platform side.
//
// The releaser is the caller's to hold for the whole dispatch, exactly as Meta's is: the bytes
// stay resident until the client has POSTed them. On ANY failure here it has already run.
func (d *GoogleAdsDispatcher) resolveGoogleCreativeAssets(ctx context.Context, brief *model.CampaignBrief, cfg *googleAdsConfig) (map[string][]byte, func(), error) {
	slots := googleAdsCreativeSlots(cfg)
	if len(slots) == 0 {
		return nil, func() {}, nil
	}

	byID := make(map[string][]byte)
	var totalBytes int64

	// Per-asset reservations, released together, and released STRUCTURALLY rather than by every
	// exit remembering to: the succeeded flag asks "did this hand the reservation to the caller",
	// not "did it fail", so the unwind also covers a panic — which an `if err != nil` defer
	// would not, since err is nil there too. See AssetReserver's doc comment.
	var releases []func()
	releaseAll := func() {
		for _, rel := range releases {
			rel()
		}
		releases = nil
	}
	succeeded := false
	defer func() {
		if !succeeded {
			releaseAll()
		}
	}()

	for _, slot := range slots {
		refs := *slot.refs
		for i := range refs {
			ref := refs[i]
			raw := strings.TrimSpace(ref.ImageAssetID)
			url := strings.TrimSpace(ref.URL)
			// Exactly one source per entry, refused here rather than at decode time so the
			// message can name the slot and the position the caller actually wrote.
			if raw == "" && url == "" {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d names neither an image URL nor a creative asset id", slot.label, i+1)
			}
			if raw != "" && url != "" {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d names both an image URL and creative asset %s; use one or the other", slot.label, i+1, domain.CallerValue(raw))
			}
			// The resolved-entry scheme is RESERVED here, at the trust boundary, because this
			// is the only place that can tell a caller's string from one this resolver wrote.
			// Downstream it is too late: slotEntries renders a URL-backed ref unchanged, and
			// the platform client tests the prefix BEFORE the https rule, so such a string
			// would be taken for a resolved entry and its bytes looked up rather than fetched.
			//
			// The value is NOT echoed. All that has been established about it is that it
			// starts with our scheme, and a caller's creative URL can carry a signed query
			// string that is itself the credential for the image.
			if strings.HasPrefix(url, googleAdsImageRefScheme) {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d has an image URL beginning with %q, which is reserved for this service's own stored-asset references; name the asset with imageAssetId instead", slot.label, i+1, googleAdsImageRefScheme)
			}
			if raw == "" {
				continue
			}
			// CANONICALIZE before the id is used as a key: uuid.Parse accepts four spellings
			// of the same uuid, so the caller's raw spelling is not a stable identity. Keying
			// on it would let one asset be read, buffered and charged once per alias, and
			// would render two platform-side entries for one image.
			parsed, perr := uuid.Parse(raw)
			if perr != nil {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d references creative asset %s, which is not a valid asset id", slot.label, i+1, domain.CallerValue(raw))
			}
			assetID := parsed.String()
			(*slot.refs)[i].ImageAssetID = assetID

			if _, seen := byID[googleAdsImageRefScheme+assetID]; seen {
				// Already resolved for an earlier entry: no second read, no second buffer,
				// and no second charge against the aggregate budget.
				continue
			}
			if d.creatives == nil {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d references creative asset %s but the creative-asset store is not configured", slot.label, i+1, assetID)
			}
			// PRICE THE ASSET BEFORE LOADING IT — one BIGINT, no BYTEA. The aggregate
			// reservation has to be taken before the blob is resident; charging afterwards
			// bounds nothing, because every concurrent dispatch would already hold its full
			// allowance by the time it blocked on the semaphore.
			size, serr := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
			if serr != nil {
				if errors.Is(serr, domain.ErrNotFound) {
					return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d references creative asset %s, which does not exist for this brief", slot.label, i+1, assetID)
				}
				return nil, func() {}, fmt.Errorf("google-ads %s entry %d: size creative asset %s: %w", slot.label, i+1, assetID, serr)
			}
			// The PER-DISPATCH ceiling, applied to the size rather than to loaded bytes, so
			// the asset that trips it is never materialised.
			totalBytes += size
			if totalBytes > maxVariantAssetBytes {
				return nil, func() {}, domain.PreflightRefusalf("google-ads creative references more than %d bytes of distinct creative assets (%d bytes at %s entry %d); split the campaign or reuse fewer images",
					maxVariantAssetBytes, totalBytes, slot.label, i+1)
			}
			relOne, ok := d.assets.reserve(ctx, size)
			if !ok {
				return nil, func() {}, domain.PreflightRefusalf("google-ads creative references %d bytes of creative assets, which exceeds the memory concurrently available for dispatch; retry when other dispatches finish", totalBytes)
			}
			releases = append(releases, relOne)

			asset, gerr := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
			if gerr != nil {
				if errors.Is(gerr, domain.ErrNotFound) {
					return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d references creative asset %s, which does not exist for this brief", slot.label, i+1, assetID)
				}
				return nil, func() {}, fmt.Errorf("google-ads %s entry %d: load creative asset %s: %w", slot.label, i+1, assetID, gerr)
			}
			// A resolved asset that carries no bytes cannot become an ImageAsset. Refuse
			// rather than proceed: an empty entry would reach the platform client as an image
			// it must fetch and cannot, and the message there would blame a URL that does not
			// exist. Refusing here names the asset.
			if len(asset.Bytes) == 0 {
				return nil, func() {}, domain.PreflightRefusalf("google-ads %s entry %d references creative asset %s, which has no stored image bytes", slot.label, i+1, assetID)
			}
			byID[googleAdsImageRefScheme+assetID] = asset.Bytes
		}
	}
	if len(byID) == 0 {
		// No asset-backed entry anywhere in the config: nothing was reserved, so the releaser
		// is a no-op over an empty slice and such a dispatch never queues behind one holding
		// assets. Returned as nil rather than an empty map so the platform side's "was any
		// image pre-resolved" question has one answer.
		succeeded = true
		return nil, releaseAll, nil
	}
	succeeded = true
	return byID, releaseAll, nil
}
