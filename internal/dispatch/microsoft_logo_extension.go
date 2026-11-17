// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft logo extension at create time (#563, gap 13): microsoftConfig.logoAssetId names a
// stored PNG/JPEG creative asset of THIS brief, and logoBusinessName the BusinessName Microsoft
// requires beside it. The asset is uploaded as an Image1x1 media and attached to the confirmed
// campaign as a LogoAdExtension (internal/platform/microsoft/logo_extension.go), after the
// sitelinks, callouts and structured snippets.
//
//   - SYNCHRONOUS 400 (checkMicrosoftLogo, PreflightCreate and again in Dispatch): a wrong JSON
//     type, either field while MICROSOFT_LOGO_EXTENSION_ENABLED is off, an id that is not a UUID,
//     a business name without an asset or an asset without a business name, and a business name
//     over 25 characters or carrying a control character.
//   - PRE-CREATE JOB FAILURE (resolveMicrosoftLogo, before any Microsoft call): an asset that does
//     not exist for this brief, or whose bytes, format, dimensions or aspect ratio break the logo
//     limits (microsoft.ValidateLogo). It needs the brief's asset store, which the synchronous
//     check has no access to, so it is the image extension's rule: nothing is created.
//   - DEGRADED, NEVER FAILED: a logo that is not, or may not be, attached marks the campaign
//     created_degraded with a step; a campaign that already has a logo (a retry) gets none.
//
// Not role-gated: the image is uploaded new for this campaign and names no shared account
// resource.

// microsoftLogoExtensionEnabled reports whether this deployment opted in, read per call.
func microsoftLogoExtensionEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftLogoExtensionEnabled) == "true"
}

// microsoftLogoInvalid is the refusal for a bad logo input: a 400 from PreflightCreate
// (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies verbatim.
func microsoftLogoInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkMicrosoftLogo refuses a config that sets either logo field while the gate is off, or whose
// fields break the rules decidable from the config alone. Shared by PreflightCreate and Dispatch.
// A value that is only whitespace counts as set, so the gate never depends on validity.
func checkMicrosoftLogo(cfg microsoftConfig) error {
	if cfg.LogoAssetID == "" && cfg.LogoBusinessName == "" {
		return nil
	}
	if !microsoftLogoExtensionEnabled() {
		field := microsoft.FieldLogoAssetID
		if cfg.LogoAssetID == "" {
			field = microsoft.FieldLogoBusinessName
		}
		env := constants.EnvMicrosoftLogoExtensionEnabled
		return microsoftLogoInvalid("%s: Microsoft logo extensions are disabled on this deployment (%s is not \"true\"); omit %s and %s, or ask an operator to set %s to \"true\"",
			field, env, microsoft.FieldLogoAssetID, microsoft.FieldLogoBusinessName, env)
	}
	raw := strings.TrimSpace(cfg.LogoAssetID)
	if raw == "" {
		return microsoftLogoInvalid("%s: is required with %s", microsoft.FieldLogoAssetID, microsoft.FieldLogoBusinessName)
	}
	if _, err := uuid.Parse(raw); err != nil {
		return microsoftLogoInvalid("%s: is not a valid asset id", microsoft.FieldLogoAssetID)
	}
	if _, err := microsoft.ValidateLogoBusinessName(cfg.LogoBusinessName); err != nil {
		// ErrLogoInvalid's text is service-authored; strip its prefix so the field leads.
		msg := strings.TrimPrefix(err.Error(), microsoft.ErrLogoInvalid.Error()+": ")
		if strings.TrimSpace(cfg.LogoBusinessName) == "" {
			msg = "is required with " + microsoft.FieldLogoAssetID + " (Microsoft requires a BusinessName beside the logo)"
		}
		return microsoftLogoInvalid("%s: %s", microsoft.FieldLogoBusinessName, msg)
	}
	return nil
}

// checkMicrosoftLogoShape refuses a logoAssetId or logoBusinessName that is not a JSON string, so
// a wrong type is the synchronous 400 rather than a job failure. null counts as absent. Keys are
// matched case-insensitively, as encoding/json matches them, in sorted order.
func checkMicrosoftLogoShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["microsoftConfig"], &platformCfg) != nil {
		return nil
	}
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, field := range []string{microsoft.FieldLogoAssetID, microsoft.FieldLogoBusinessName} {
			if !strings.EqualFold(k, field) || isJSONNull(platformCfg[k]) {
				continue
			}
			var s string
			if json.Unmarshal(platformCfg[k], &s) != nil {
				return microsoftLogoInvalid("%s: must be a string", field)
			}
		}
	}
	return nil
}

// resolveMicrosoftLogo loads and validates the logo cfg names, returning nil when none is
// requested. It is resolveMicrosoftImage for the logo: scoped to the brief, priced on byte_size
// before loading, charged against the shared AssetReserver, and checked against the logo limits
// before any Microsoft call. On success cfg.LogoAssetID is the canonical uuid and the returned
// release func owns the reservation; on any failure the reservation is released here.
func (d *MicrosoftDispatcher) resolveMicrosoftLogo(ctx context.Context, brief *model.CampaignBrief, cfg *microsoftConfig) (*microsoft.LogoInput, func(), error) {
	noop := func() {}
	raw := strings.TrimSpace(cfg.LogoAssetID)
	if raw == "" {
		return nil, noop, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		// checkMicrosoftLogo refused this already; kept so the function stands alone.
		return nil, noop, microsoftLogoInvalid("%s: is not a valid asset id", microsoft.FieldLogoAssetID)
	}
	assetID := parsed.String()
	if d.creatives == nil {
		return nil, noop, domain.PreflightRefusalf("microsoftConfig.logoAssetId references creative asset %s but the creative-asset store is not configured", assetID)
	}
	release := noop
	succeeded := false
	defer func() {
		if !succeeded {
			release()
		}
	}()
	size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("microsoftConfig.logoAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
	}
	if size > microsoft.MaxImageBytes {
		return nil, noop, domain.PreflightRefusalf("microsoftConfig.logoAssetId: creative asset %s is %d bytes; Microsoft Advertising logo uploads accept at most %d bytes", assetID, size, microsoft.MaxImageBytes)
	}
	rel, ok := d.assets.reserve(ctx, size)
	if !ok {
		return nil, noop, domain.PreflightRefusalf("creative asset %s (%d bytes) exceeds the memory concurrently available for dispatch; retry when other dispatches finish", assetID, size)
	}
	release = rel
	asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("microsoftConfig.logoAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
	}
	if asset == nil || len(asset.Bytes) == 0 {
		return nil, noop, domain.PreflightRefusalf("microsoftConfig.logoAssetId: creative asset %s has no stored image bytes", assetID)
	}
	in := &microsoft.LogoInput{Data: asset.Bytes, MimeType: asset.MimeType, BusinessName: cfg.LogoBusinessName}
	if err := microsoft.ValidateLogo(*in); err != nil {
		// Size, shape, format and name only: operator-safe, pre-send.
		return nil, noop, domain.WrapPreflightRefusal(fmt.Errorf("microsoftConfig.logoAssetId: creative asset %s: %w", assetID, err))
	}
	cfg.LogoAssetID = assetID
	cfg.LogoBusinessName = strings.TrimSpace(cfg.LogoBusinessName)
	succeeded = true
	return in, release, nil
}

// attachMicrosoftLogo attaches logo to the CONFIRMED campaign in result, recording the ids and
// Steps. It reports whether the campaign is DEGRADED (a logo was requested and is not, or may not
// be, attached). A nil logo is a no-op. It never returns an error, for attachMicrosoftImage's
// reason: the campaign exists.
func attachMicrosoftLogo(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, logo *microsoft.LogoInput) bool {
	if logo == nil {
		return false
	}
	lr, err := client.AttachCampaignLogo(ctx, result.CampaignID, *logo)
	if lr != nil {
		result.LogoMediaID = lr.MediaID
		result.LogoAdExtensionID = lr.AdExtensionID
		result.Steps = append(result.Steps, lr.Steps...)
	}
	if err == nil {
		return false
	}
	if microsoft.IsOutcomeUnconfirmed(err) {
		result.Steps = append(result.Steps, "Logo extension UNCONFIRMED (it may be attached — verify in Microsoft Advertising before re-attaching): "+err.Error())
		slog.WarnContext(ctx, "microsoft logo extension attach unconfirmed", "campaign_id", result.CampaignID, "error", err)
	} else {
		result.Steps = append(result.Steps, "Logo extension NOT attached: "+err.Error())
		slog.WarnContext(ctx, "microsoft logo extension not attached", "campaign_id", result.CampaignID, "error", err)
	}
	return true
}
