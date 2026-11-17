// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X video creative (#564 gap 7): twitterConfig.videoAssetId, a stored MP4 creative asset
// (upload-creative-asset, content_type video/mp4) uploaded as amplify_video and attached to the
// authored tweet(s), or used as the website card's media. This file adds the deployment gate, the
// config-only rules (a synchronous 400, "invalid campaign config: twitterConfig.videoAssetId: ...")
// and the asset read; the upload itself is twitter.Client's (video.go).

// twitterVideoEnabled reports whether this deployment opted in. Read per call.
func twitterVideoEnabled() bool {
	return os.Getenv(constants.EnvTwitterVideoEnabled) == "true"
}

// checkTwitterVideo refuses a videoAssetId while the gate is off, one that is not an asset id, one
// beside an imageAssetId, one on a create that authors no tweet, and one on a create naming more
// targeting criteria than a video leaves room for (nCriteria, the normalized count). It returns
// the canonical asset id ("" when unset). Every refusal is a domain.ErrCreateConfigInvalid wrapped
// as a PreflightRefusal.
func checkTwitterVideo(cfg twitterConfig, nCriteria int) (string, error) {
	raw := strings.TrimSpace(cfg.VideoAssetID)
	if raw == "" {
		return "", nil
	}
	f := twitter.FieldVideoAssetID
	refuse := func(msg string) error {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, f, msg))
	}
	if !twitterVideoEnabled() {
		env := constants.EnvTwitterVideoEnabled
		return "", domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, f, env, f, env))
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", refuse(fmt.Sprintf("%s is not a valid asset id", domain.CallerValue(raw)))
	}
	if strings.TrimSpace(cfg.ImageAssetID) != "" {
		return "", refuse("a tweet carries an image or a video, not both; omit imageAssetId or videoAssetId")
	}
	if strings.TrimSpace(cfg.TweetID) != "" || (strings.TrimSpace(cfg.TweetText) == "" && len(cfg.Variants) == 0) {
		return "", refuse("a video is attached only to an authored tweet, so it requires tweetText (or variants) and no tweetId")
	}
	if cerr := twitter.CheckCreateCriteriaCountWithVideo(nCriteria); cerr != nil {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, cerr))
	}
	return parsed.String(), nil
}

// checkTwitterVideoShape refuses a videoAssetId that is not a JSON string, so a wrong type is the
// synchronous 400 rather than a job failure. null counts as absent.
func checkTwitterVideoShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["twitterConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, twitter.FieldVideoAssetID) || isJSONNull(raw) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be a string", domain.ErrCreateConfigInvalid, twitter.FieldVideoAssetID))
		}
	}
	return nil
}

// resolveTwitterVideo loads the MP4 a checked videoAssetId names, before the client is built or X
// contacted, so every refusal is pre-create: an unbound store, an asset that does not exist FOR
// THIS BRIEF, one over MaxVideoBytes (refused on its stored size, before the blob is loaded), no
// memory budget, an empty blob, or an asset that is not video/mp4 (an image in the video slot).
// The release func is always non-nil and owns the reservation on success, as for the image.
func (d *TwitterDispatcher) resolveTwitterVideo(ctx context.Context, brief *model.CampaignBrief, assetID string) (*twitter.VideoInput, func(), error) {
	noop := func() {}
	if assetID == "" {
		return nil, noop, nil
	}
	if d.creatives == nil {
		return nil, noop, domain.PreflightRefusalf("twitter config references creative asset %s but the creative-asset store is not configured", assetID)
	}
	size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("twitter config references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
	}
	if size > twitter.MaxVideoBytes {
		return nil, noop, domain.PreflightRefusalf("creative asset %s is %d bytes, over the %d-byte video limit", assetID, size, twitter.MaxVideoBytes)
	}
	release, ok := d.assets.reserve(ctx, size)
	if !ok {
		return nil, noop, domain.PreflightRefusalf("creative asset %s (%d bytes) exceeds the memory concurrently available for dispatch; retry when other dispatches finish", assetID, size)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("twitter config references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
	}
	if asset == nil || len(asset.Bytes) == 0 {
		return nil, noop, domain.PreflightRefusalf("creative asset %s has no stored video bytes", assetID)
	}
	if asset.MimeType != model.MimeTypeMP4 {
		return nil, noop, domain.PreflightRefusalf("creative asset %s is not a video; twitterConfig.videoAssetId accepts an MP4 (upload it with content_type video/mp4)", assetID)
	}
	handedOff = true
	return &twitter.VideoInput{Bytes: asset.Bytes, MIME: asset.MimeType}, release, nil
}
