// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit VIDEO posts from stored assets (#549 gap 5).
//
// redditConfig.videoAssetId names a stored video/mp4 and redditConfig.thumbnailAssetId a stored
// PNG/JPEG of the same brief. Reddit fetches both from URLs while the post is created, exactly as
// it fetches an image post's media_url, so both are served by the signed-asset route
// (reddit_asset.go): the video at a kind=video URL, which serves video/mp4 and nothing else, and
// the thumbnail at an ordinary image URL, which serves PNG/JPEG and never a video. Both URLs are
// minted late, by the client, immediately before the one post request; both are scrubbed from
// anything that comes back. The thumbnail is REQUIRED rather than derived: the schema makes
// thumbnail_url "required for video posts", and this service has no video decoder to cut a frame.
//
// Everything after the post is the image path, unchanged: the post is authored before the paid
// campaign, so a refused post degrades to a campaign and ad group with no ad (created_degraded,
// claim retained), and an in-flight cancellation is an UNCONFIRMED partial.

// Field names, for refusal texts.
const (
	redditVideoAssetField     = "videoAssetId"
	redditThumbnailAssetField = "thumbnailAssetId"
)

// redditVideoEnabled reports whether this deployment opted in to Reddit video posts. Read per
// call, like the other REDDIT_* gates, so it flips without a rebuild.
func redditVideoEnabled() bool {
	return os.Getenv(constants.EnvRedditVideoEnabled) == "true"
}

// redditVideoInvalid is the refusal shape for a video config: an ErrCreateConfigInvalid (a 400
// from PreflightCreate) wrapped as a PreflightRefusal (a job that reached Dispatch copies it).
func redditVideoInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkRedditVideoChoice applies the config-only video rules, shared by PreflightCreate
// (synchronous 400) and Dispatch (again, pre-create, for any path that skipped preflight). It
// returns the canonical video and thumbnail asset ids, or two empty strings when neither field
// is set. The deployment gate is checked first, so it never depends on the values' validity.
func (d *RedditDispatcher) checkRedditVideoChoice(cfg redditConfig) (videoID, thumbID string, err error) {
	rawVideo, rawThumb := strings.TrimSpace(cfg.VideoAssetID), strings.TrimSpace(cfg.ThumbnailAssetID)
	if rawVideo == "" && rawThumb == "" {
		return "", "", nil
	}
	if !redditVideoEnabled() {
		env := constants.EnvRedditVideoEnabled
		return "", "", redditVideoInvalid("video posts are disabled on this deployment (%s is not \"true\"); omit %s and %s, or ask an operator to set %s to \"true\"",
			env, redditVideoAssetField, redditThumbnailAssetField, env)
	}
	if rawVideo == "" {
		return "", "", redditVideoInvalid("%s needs %s: it is the thumbnail of a video post", redditThumbnailAssetField, redditVideoAssetField)
	}
	if rawThumb == "" {
		return "", "", redditVideoInvalid("%s needs %s: Reddit requires a thumbnail for a video post (a stored PNG or JPEG)", redditVideoAssetField, redditThumbnailAssetField)
	}
	if strings.TrimSpace(cfg.PostURL) != "" || strings.TrimSpace(cfg.ImageURL) != "" || strings.TrimSpace(cfg.ImageAssetID) != "" {
		// One creative per ad: a video next to an image or an existing post leaves it ambiguous
		// which one the caller meant, and the client would refuse the mix anyway.
		return "", "", redditVideoInvalid("%s cannot be combined with postUrl, imageUrl or imageAssetId", redditVideoAssetField)
	}
	if d.assetURLs == nil {
		return "", "", redditVideoInvalid("stored creative assets are not enabled for Reddit on this deployment, so %s cannot be served", redditVideoAssetField)
	}
	v, err := uuid.Parse(rawVideo)
	if err != nil {
		return "", "", redditVideoInvalid("%s %s is not a valid asset id", redditVideoAssetField, domain.CallerValue(rawVideo))
	}
	th, err := uuid.Parse(rawThumb)
	if err != nil {
		return "", "", redditVideoInvalid("%s %s is not a valid asset id", redditThumbnailAssetField, domain.CallerValue(rawThumb))
	}
	if v == th {
		return "", "", redditVideoInvalid("%s and %s name the same asset; the thumbnail must be a separate PNG or JPEG", redditVideoAssetField, redditThumbnailAssetField)
	}
	return v.String(), th.String(), nil
}

// checkRedditVideoAssets proves, before any Reddit call, everything about the two stored assets
// that serving them later depends on: the store is bound, each exists FOR THIS BRIEF under an
// active brief and has bytes, the video is a stored video/mp4 and the thumbnail a stored PNG or
// JPEG (the signed route would otherwise 404 Reddit's fetch after nothing could be undone), and
// the ids can be carried in a signed URL. Every failure is pre-create.
func (d *RedditDispatcher) checkRedditVideoAssets(ctx context.Context, brief *model.CampaignBrief, videoID, thumbID string) error {
	if d.assets == nil {
		return domain.PreflightRefusalf("redditConfig.%s references creative asset %s but the creative-asset store is not configured", redditVideoAssetField, videoID)
	}
	slots := []struct {
		field, id string
		video     bool
	}{
		{redditVideoAssetField, videoID, true},
		{redditThumbnailAssetField, thumbID, false},
	}
	for _, s := range slots {
		meta, err := d.assets.GetAssetMetadata(ctx, brief.ProjectID, brief.ID, s.id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.PreflightRefusalf("redditConfig.%s references creative asset %s, which does not exist for this brief", s.field, s.id)
			}
			return fmt.Errorf("reddit: read creative asset %s: %w", s.id, err)
		}
		if meta == nil || meta.ByteSize <= 0 {
			return domain.PreflightRefusalf("redditConfig.%s references creative asset %s, which has no stored bytes", s.field, s.id)
		}
		isVideo := meta.MimeType == model.MimeTypeMP4
		if s.video && !isVideo {
			return domain.PreflightRefusalf("creative asset %s is not a video; redditConfig.%s accepts a stored video/mp4", s.id, s.field)
		}
		if !s.video && meta.MimeType != model.MimeTypePNG && meta.MimeType != model.MimeTypeJPEG {
			return domain.PreflightRefusalf("creative asset %s is not an image; redditConfig.%s accepts PNG or JPEG", s.id, s.field)
		}
		if err := assetsign.ValidateIDs(brief.ProjectID, brief.ID, s.id); err != nil {
			return fmt.Errorf("reddit: creative asset %s cannot be served at a signed URL: %w", s.id, err)
		}
	}
	return nil
}

// lateAssetURLKind is lateAssetURL for a given slot kind: the video of a VIDEO post is minted
// as assetsign.KindVideo, so the route serves it only as video/mp4.
func (d *RedditDispatcher) lateAssetURLKind(kind assetsign.Kind, brief *model.CampaignBrief, assetID string, minted *string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		signed, exp, err := d.assetURLs.MintKind(kind, brief.ProjectID, brief.ID, assetID)
		if err != nil {
			return "", fmt.Errorf("reddit: cannot mint a signed URL for creative asset %s: %w", assetID, err)
		}
		*minted = signed
		// The URL is a bearer credential for the asset until exp: log the id and expiry only.
		slog.InfoContext(ctx, "minted signed creative-asset URL for a Reddit promoted post",
			"asset_id", assetID, "brief_id", brief.ID, "exp", exp.Unix(), "video", kind == assetsign.KindVideo)
		return signed, nil
	}
}
