// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Reddit creatives from STORED assets (LFXV2-2665).
//
// Reddit Ads API v3 takes a promoted-post image only as a public media_url it fetches and
// re-hosts while the post is created (createPromotedPost). To use an image stored through
// upload-creative-asset, this service serves that one asset at a short-lived HMAC-signed URL
// (internal/assetsign, internal/service/signed_asset.go). The dispatch only CHECKS the asset; it
// hands the client an ImageURLSource (lateAssetURL) that mints the URL immediately before the
// promoted-post request, so nothing earlier in CreateCampaign can spend its lifetime. The URL then
// takes the author-a-post path imageUrl takes. Everything downstream — the
// failed/UNCONFIRMED/degraded classification and claim retention — is the path imageUrl has
// always taken, untouched. The URL is never returned to an API client, never persisted (the
// config snapshot is built from cfg, which keeps imageAssetId and never the minted URL), and
// never logged: logs carry the asset id and expiry only.
//
// The dispatch takes NO asset-memory reservation. It never holds the bytes — it holds a URL —
// and AssetReserver's own contract is that budget is charged for memory actually held. The
// bytes are materialised by the serving route when Reddit fetches them, and THAT is where the
// shared AssetReserver is charged.

// errRedditStoredAssetsDisabled is the refusal when imageAssetId is used with the feature off.
const errRedditStoredAssetsDisabled = "stored creative assets are not enabled for Reddit; supply imageUrl"

// redditAssetSizer is the one creative-asset read the Reddit dispatch needs: prove the asset
// exists for THIS brief (and that the brief is still active), without loading its bytes.
type redditAssetSizer interface {
	GetAssetSize(ctx context.Context, projectID, briefID, assetID string) (int64, error)
}

// SetCreativeAssetURLs binds the stored-asset path: the store used to prove ownership and the
// signer that mints the URL. A nil signer leaves the feature OFF (imageAssetId refused). Guard
// the concrete repo pointer at the call site, as registerDispatchers does for Meta.
func (d *RedditDispatcher) SetCreativeAssetURLs(assets redditAssetSizer, signer *assetsign.Signer) {
	d.assets = assets
	d.assetURLs = signer
}

// CreativeAssetURLsEnabled reports whether a signer is bound. For the container wiring test.
func (d *RedditDispatcher) CreativeAssetURLsEnabled() bool { return d.assetURLs != nil }

// checkRedditAssetChoice applies the config-only rules, shared by PreflightCreate (synchronous
// 400) and Dispatch (the same rules again, pre-create, for any path that skipped preflight).
// It returns the canonical asset id, or "" when no stored asset is referenced.
func (d *RedditDispatcher) checkRedditAssetChoice(cfg redditConfig) (string, error) {
	raw := strings.TrimSpace(cfg.ImageAssetID)
	if raw == "" {
		return "", nil
	}
	if strings.TrimSpace(cfg.ImageURL) != "" {
		return "", fmt.Errorf("%w: redditConfig: supply exactly one of imageUrl or imageAssetId", domain.ErrCreateConfigInvalid)
	}
	if strings.TrimSpace(cfg.PostURL) != "" {
		// postUrl takes precedence and would silently ignore the asset; a caller who named an
		// image must not get an ad without it.
		return "", fmt.Errorf("%w: redditConfig: imageAssetId cannot be combined with postUrl", domain.ErrCreateConfigInvalid)
	}
	if d.assetURLs == nil {
		return "", fmt.Errorf("%w: %s", domain.ErrCreateConfigInvalid, errRedditStoredAssetsDisabled)
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: redditConfig: imageAssetId %s is not a valid asset id", domain.ErrCreateConfigInvalid, safeAssetIDForError(raw))
	}
	return parsed.String(), nil
}

// PreflightCreate refuses, before the create job exists, a Reddit config that names a stored
// asset the service cannot serve: the feature off, imageAssetId together with imageUrl or
// postUrl, or a malformed id. It never calls Reddit, so every other outcome is
// ErrPreflightNotApplicable. Whether the asset belongs to the brief cannot be checked here (the
// pre-check is not given the brief); Dispatch checks it before any upstream call.
func (d *RedditDispatcher) PreflightCreate(_ context.Context, _ string, _ model.Provider, config json.RawMessage) error {
	var cfg redditConfig
	if err := unmarshalPlatformConfig(config, "redditConfig", &cfg); err != nil {
		return domain.ErrPreflightNotApplicable // Dispatch reports a malformed config as the job failure
	}
	if _, err := d.checkRedditAssetChoice(cfg); err != nil {
		return err
	}
	return domain.ErrPreflightNotApplicable
}

// checkRedditAsset proves, before any Reddit call, everything about a referenced stored asset
// that minting its URL later depends on: the store is bound, the asset exists FOR THIS BRIEF
// under an active brief and has bytes, and the ids can be carried in a signed URL. Every
// failure is pre-create: the caller wraps it in notCreated so the claim is released.
func (d *RedditDispatcher) checkRedditAsset(ctx context.Context, brief *model.CampaignBrief, assetID string) error {
	if d.assets == nil {
		return fmt.Errorf("reddit config references creative asset %s but the creative-asset store is not configured", assetID)
	}
	size, err := d.assets.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("reddit config references creative asset %s, which does not exist for this brief", assetID)
		}
		return fmt.Errorf("reddit: size creative asset %s: %w", assetID, err)
	}
	if size <= 0 {
		return fmt.Errorf("reddit config references creative asset %s, which has no stored image bytes", assetID)
	}
	if err := assetsign.ValidateIDs(brief.ProjectID, brief.ID, assetID); err != nil {
		return fmt.Errorf("reddit: creative asset %s cannot be served at a signed URL: %w", assetID, err)
	}
	return nil
}

// lateAssetURL returns the reddit.CampaignInput.ImageURLSource that mints the signed URL. The
// client calls it immediately before the promoted-post request that hands the URL to Reddit —
// NOT here in Dispatch — because everything CreateCampaign does first (account verification,
// a Retry-After wait of up to a minute) would otherwise spend the URL's lifetime before Reddit
// fetches it. The post request itself may still refresh the token after minting, which adds
// at most one more request timeout; assetsign.MinTTL covers it. The minted URL is also written to *minted so the caller
// can scrub it from anything that comes back.
func (d *RedditDispatcher) lateAssetURL(brief *model.CampaignBrief, assetID string, minted *string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		signed, exp, err := d.assetURLs.Mint(brief.ProjectID, brief.ID, assetID)
		if err != nil {
			return "", fmt.Errorf("reddit: cannot mint a signed URL for creative asset %s: %w", assetID, err)
		}
		*minted = signed
		// The URL is a bearer credential for the image until exp: log the id and expiry only.
		slog.InfoContext(ctx, "minted signed creative-asset URL for a Reddit promoted post",
			"asset_id", assetID, "brief_id", brief.ID, "exp", exp.Unix())
		return signed, nil
	}
}

// scrubSignedURL removes the signature from any text that might carry it back out — an
// upstream error Reddit echoed, or the ad warning the result persists — keeping the rest of the
// message for diagnosis.
func scrubSignedURL(s, signed string) string {
	if signed == "" || s == "" {
		return s
	}
	i := strings.Index(signed, "?")
	if i < 0 {
		return s
	}
	return strings.ReplaceAll(s, signed[i:], "?[signature redacted]")
}

// scrubbedError wraps err so its message carries no signature, while errors.Is/As still see
// through to the original (classification of UNCONFIRMED vs failed is unaffected).
type scrubbedError struct {
	err    error
	signed string
}

func (e *scrubbedError) Error() string { return scrubSignedURL(e.err.Error(), e.signed) }
func (e *scrubbedError) Unwrap() error { return e.err }

func scrubErr(err error, signed string) error {
	if err == nil || signed == "" {
		return err
	}
	return &scrubbedError{err: err, signed: signed}
}
