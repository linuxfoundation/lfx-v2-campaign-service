// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// Microsoft Advertising creative image (LFXV2-2665).
//
// A Microsoft dispatch may name ONE stored creative asset (microsoftConfig.imageAssetId),
// which is attached to the created Search campaign as a campaign-level Image ad extension —
// the only image surface the campaign type this service creates can carry (see
// internal/platform/microsoft/image_extension.go for the choice and the documentation it
// rests on). The safety properties are Meta's, applied to one asset:
//
//   - VALIDATE BEFORE MUTATING. The asset is resolved within the brief, sized, charged
//     against the shared AssetReserver, loaded and checked against Microsoft's image limits
//     before CreateCampaign runs. Every failure there is a pre-create error: the claim is
//     released and nothing exists upstream.
//   - UPLOAD AT MOST ONCE PER CAMPAIGN. The client looks for an image extension already
//     associated with the campaign before it uploads, so a retry that reuses the campaign by
//     name does not upload the image again; the media and extension ids it did create are
//     recorded in the persisted result (CampaignResult.ImageMediaID/ImageAdExtensionID), as
//     Meta records each ad's image_hash.
//   - DEFINITE VS UNCONFIRMED. A failed attach DEGRADES the campaign (created_degraded) and
//     records a Steps entry that says which: "NOT attached" for a definite rejection,
//     "UNCONFIRMED" when the image may have been attached and must be verified before anyone
//     re-attaches it. Steps carry ids and the client's status-only error text, never
//     upstream message text.

// SetCreativeAssetRepo binds the creative-asset read path, mirroring
// MetaDispatcher.SetCreativeAssetRepo: called once by registerDispatchers before the
// dispatcher is shared, and a nil argument is ignored rather than stored as a typed nil.
func (d *MicrosoftDispatcher) SetCreativeAssetRepo(r creativeAssetReader) {
	if r == nil {
		return
	}
	d.creatives = r
}

// SetAssetReserver binds the PROCESS-WIDE creative-asset memory budget — the same instance
// the Meta dispatcher holds, so the bound stays one bound across both.
func (d *MicrosoftDispatcher) SetAssetReserver(a *AssetReserver) {
	if a == nil {
		return
	}
	d.assets = a
}

// CreativeAssetRepoIsSet reports whether the creative-asset read path was bound. Exported for
// the container wiring test, for the reason MetaDispatcher.CreativeAssetRepoIsSet gives.
func (d *MicrosoftDispatcher) CreativeAssetRepoIsSet() bool { return d.creatives != nil }

// AssetReserverIsSet reports whether the aggregate asset budget was bound.
func (d *MicrosoftDispatcher) AssetReserverIsSet() bool { return d.assets != nil }

// resolveMicrosoftImage loads and validates the campaign image cfg names, returning nil when
// none is requested. On success cfg.ImageAssetID is replaced with the canonical uuid (so the
// snapshot never stores a caller spelling) and the returned release func owns the
// reservation, which the caller holds until the upload is done. On ANY failure — including a
// panic — the reservation is released here and the returned func is a no-op, exactly as
// resolveVariantAssets does.
//
// The asset is looked up SCOPED to the brief (project, brief, asset), so an asset belonging
// to another brief or project is "does not exist for this brief", never another tenant's image.
func (d *MicrosoftDispatcher) resolveMicrosoftImage(ctx context.Context, brief *model.CampaignBrief, cfg *microsoftConfig, eventName string) (*microsoft.ImageInput, func(), error) {
	noop := func() {}
	raw := strings.TrimSpace(cfg.ImageAssetID)
	if raw == "" {
		if strings.TrimSpace(cfg.ImageAltText) != "" {
			return nil, noop, errors.New("microsoftConfig.imageAltText was supplied without imageAssetId; supply the image it describes or omit it")
		}
		return nil, noop, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return nil, noop, fmt.Errorf("microsoftConfig.imageAssetId %s is not a valid asset id", safeAssetIDForError(raw))
	}
	assetID := parsed.String()
	if d.creatives == nil {
		return nil, noop, fmt.Errorf("microsoftConfig.imageAssetId references creative asset %s but the creative-asset store is not configured", assetID)
	}

	release := noop
	succeeded := false
	defer func() {
		if !succeeded {
			release()
		}
	}()

	// PRICE BEFORE LOADING: the size read is one BIGINT, and Microsoft's ceiling is far below
	// the 30 MiB a stored asset may be, so an oversized asset is refused on its byte_size and
	// never materialised.
	size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, fmt.Errorf("microsoftConfig.imageAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
	}
	if size > microsoft.MaxImageBytes {
		return nil, noop, fmt.Errorf("creative asset %s is %d bytes; Microsoft Advertising image extensions accept at most %d bytes", assetID, size, microsoft.MaxImageBytes)
	}
	rel, ok := d.assets.reserve(ctx, size)
	if !ok {
		return nil, noop, fmt.Errorf("creative asset %s (%d bytes) exceeds the memory concurrently available for dispatch; retry when other dispatches finish", assetID, size)
	}
	release = rel

	asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, fmt.Errorf("microsoftConfig.imageAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
	}
	if asset == nil || len(asset.Bytes) == 0 {
		// Refused rather than skipped: the caller asked for an image, and silently creating
		// an image-less campaign would spend on a creative nobody approved.
		return nil, noop, fmt.Errorf("creative asset %s has no stored image bytes", assetID)
	}
	alt := strings.TrimSpace(cfg.ImageAltText)
	if alt == "" {
		alt = microsoft.ImageAltTextFromName(eventName)
	}
	in := &microsoft.ImageInput{Data: asset.Bytes, MimeType: asset.MimeType, AltText: alt}
	if err := microsoft.ValidateImage(*in); err != nil {
		return nil, noop, fmt.Errorf("creative asset %s: %w", assetID, err)
	}
	cfg.ImageAssetID = assetID
	succeeded = true
	return in, release, nil
}

// attachMicrosoftImage attaches img to the confirmed campaign in result, recording the ids
// and a Steps entry on result. It reports whether the campaign is DEGRADED (an image was
// requested and is not, or may not be, attached). A nil img is a no-op.
//
// The failure is not returned as an error on purpose. The campaign exists, so a dispatch
// error would retain the claim and leave a reconcile that never re-runs this step — the
// same reasoning Meta applies to a per-variant ad failure. created_degraded makes the
// shortfall visible instead, and the Steps entry says whether to verify (UNCONFIRMED) or
// simply re-attach (NOT attached).
func (d *MicrosoftDispatcher) attachMicrosoftImage(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, img *microsoft.ImageInput) bool {
	if img == nil {
		return false
	}
	ar, err := client.AttachCampaignImage(ctx, result.CampaignID, *img)
	if ar != nil {
		result.ImageMediaID = ar.MediaID
		result.ImageAdExtensionID = ar.AdExtensionID
		result.Steps = append(result.Steps, ar.Steps...)
	}
	if err == nil {
		return false
	}
	if microsoft.IsOutcomeUnconfirmed(err) {
		result.Steps = append(result.Steps, "Image extension UNCONFIRMED (it may be attached — verify in Microsoft Advertising before re-attaching): "+err.Error())
		slog.WarnContext(ctx, "microsoft image extension attach unconfirmed", "campaign_id", result.CampaignID, "error", err)
	} else {
		result.Steps = append(result.Steps, "Image extension NOT attached: "+err.Error())
		slog.WarnContext(ctx, "microsoft image extension not attached", "campaign_id", result.CampaignID, "error", err)
	}
	return true
}
