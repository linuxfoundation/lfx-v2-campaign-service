// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
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
// exists for THIS brief (and that the brief is still active), has bytes, and is an image —
// without loading its bytes. GetAssetMetadata is the port's bytes-free row read (one query, the
// same scope as GetAsset), so the size AND the mime type come back together; GetAssetSize alone
// would leave a video/mp4 asset indistinguishable from an image until Reddit fetched the URL.
type redditAssetSizer interface {
	GetAssetMetadata(ctx context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error)
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
		return "", fmt.Errorf("%w: redditConfig: imageAssetId %s is not a valid asset id", domain.ErrCreateConfigInvalid, domain.CallerValue(raw))
	}
	return parsed.String(), nil
}

// PreflightCreate refuses, before the create job exists, a Reddit config that names a stored
// asset the service cannot serve (the feature off, imageAssetId together with imageUrl or
// postUrl, or a malformed id), whose bidding cannot be created as asked (redditBidPlan), whose
// custom audience lists are mistyped, malformed or off on this deployment (#549), whose
// conversionEvent is refused (checkRedditConversionEvent), or whose placements, platforms or
// expandTargeting are refused (checkRedditPlacementTargeting, #549 gap 7), or whose budgetType is
// refused (checkRedditBudgetType, #549 gap 9), or whose variants break the one-ad-per-variant
// rules (checkRedditAdPerVariant, #549 gap 6), or whose gender, languages or schedule are refused
// (checkRedditDemographicTargeting, #549 gap 13), or whose subCountryGeoTargets are refused
// (checkRedditSubCountryGeo, #549 gap 11), or whose video post is gated off or contradictory
// (checkRedditVideoChoice, #549 gap 5), or whose carousel post is gated off or breaks its card
// rules (checkRedditCarouselChoice, #661). It
// never calls Reddit, so every other outcome is ErrPreflightNotApplicable. Whether the asset belongs to the brief cannot be checked here (the
// pre-check is not given the brief); Dispatch checks it before any upstream call. A config that does
// not decode is left to Dispatch, except a wrong-typed `bidding` (checkBiddingShape).
func (d *RedditDispatcher) PreflightCreate(ctx context.Context, projectID string, platform model.Provider, config json.RawMessage) error {
	// A wrong-typed `bidding` is a synchronous 400 even though other type errors are not.
	if err := checkBiddingShape(config, "redditConfig"); err != nil {
		return err
	}
	// Likewise a wrong-typed custom audience list (#549), which encoding/json would otherwise
	// leave to the job.
	if err := checkRedditCustomAudienceShape(config); err != nil {
		return err
	}
	// A wrong-typed exclusion list is a synchronous 400 too (#549).
	if err := checkRedditExclusionsShape(config); err != nil {
		return err
	}
	// And a wrong-typed subCountryGeoTargets (#549 gap 11).
	if err := checkRedditSubCountryGeoShape(config); err != nil {
		return err
	}
	// And a wrong-typed placements, platforms or expandTargeting (#549 gap 7).
	if err := checkRedditPlacementShape(config); err != nil {
		return err
	}
	// And a wrong-typed gender, languages or schedule (#549 gap 13).
	if err := checkRedditDemographicShape(config); err != nil {
		return err
	}
	// And a wrong-typed videoAssetId or thumbnailAssetId (#549 gap 5).
	if err := checkRedditVideoShape(config); err != nil {
		return err
	}
	// And a wrong-typed carouselCards (#661).
	if err := checkRedditCarouselShape(config); err != nil {
		return err
	}
	// BEST-EFFORT decode (PR #332 review): encoding/json fills every field it can past a type
	// error, so the bid plan runs even when an unrelated field is broken — an invalid bidding is
	// always the synchronous 400. The asset checks keep their old scope: they run only on a config
	// that decoded cleanly, and a malformed one is still Dispatch's job failure.
	var cfg redditConfig
	decodeErr := unmarshalPlatformConfig(config, "redditConfig", &cfg)
	if _, err := redditBidPlan(cfg); err != nil {
		return err
	}
	// The conversion event is checked on the raw key, so a wrong type is the synchronous 400 even
	// when it made the decode fail (#549).
	if err := checkRedditConversionEvent(config, cfg); err != nil {
		return err
	}
	// The custom audiences' gate and id rules run on the best-effort decode too, like the bid
	// plan: an unrelated mistyped field must not turn a gated or invalid list into an accepted
	// job. checkRedditCustomAudienceShape above already refused a mistyped list, so both decoded.
	if err := checkRedditCustomAudiences(cfg); err != nil {
		return err
	}
	// The exclusions' gate and rules run on the best-effort decode too, like the bid plan: an
	// unrelated mistyped field must not turn a gated or invalid exclusion list into an accepted
	// job. checkRedditExclusionsShape above already refused a mistyped list, so the lists decoded.
	if err := checkRedditTargetingExclusions(cfg); err != nil {
		return err
	}
	// Sub-country locations (#549 gap 11): gate and rules on the best-effort decode, like the
	// exclusions; checkRedditSubCountryGeoShape above already refused a mistyped list.
	if err := checkRedditSubCountryGeo(cfg); err != nil {
		return err
	}
	// Placements, platforms and expansion (#549 gap 7): gate and rules on the best-effort decode,
	// like the exclusions; checkRedditPlacementShape above already refused a mistyped value.
	if err := checkRedditPlacementTargeting(cfg); err != nil {
		return err
	}
	// Gender, languages and dayparting (#549 gap 13): gate and rules on the best-effort decode,
	// like the placements; checkRedditDemographicShape above already refused a mistyped value.
	if err := checkRedditDemographicTargeting(cfg); err != nil {
		return err
	}
	// The budget's pacing (#549 gap 9), checked on the raw key like conversionEvent, so a wrong
	// type is the synchronous 400 even when it made the decode fail.
	if err := checkRedditBudgetType(config); err != nil {
		return err
	}
	// The video post's gate and config rules (#549) run on the best-effort decode too, so a gated
	// or contradictory video config is the synchronous 400 even beside an unrelated broken field
	// (checkRedditVideoShape above already refused a mistyped video field).
	if _, _, err := d.checkRedditVideoChoice(cfg); err != nil {
		return err
	}
	// The carousel post's gate and card rules (#661), likewise on the best-effort decode.
	if _, err := d.checkRedditCarouselChoice(cfg); err != nil {
		return err
	}
	if decodeErr != nil {
		return domain.ErrPreflightNotApplicable // Dispatch reports a malformed config as the job failure
	}
	if _, err := d.checkRedditAssetChoice(cfg); err != nil {
		return err
	}
	// One ad per variant (#549 gap 6): the cap and every variant headline, on a config that
	// decoded cleanly (variants is a plain list, so a mistyped one is the decode failure above).
	if err := checkRedditAdPerVariant(cfg, redditAdPerVariantEnabled()); err != nil {
		return err
	}
	// Community and interest resolution (#549 gaps 4 and 8) and sub-country geolocation
	// validation (#549 gap 11): the checks here that read Reddit, so they run last, after every
	// refusal decidable from the config alone. Behind REDDIT_TARGETING_RESOLUTION_ENABLED and
	// REDDIT_SUBCOUNTRY_GEO_ENABLED; see reddit_targeting_resolution.go.
	if needsRedditTargetingResolution(cfg) || needsRedditSubCountryGeoValidation(cfg) {
		return d.preflightRedditTargeting(ctx, projectID, platform, cfg)
	}
	return domain.ErrPreflightNotApplicable
}

// checkRedditAsset proves, before any Reddit call, everything about a referenced stored asset
// that minting its URL later depends on: the store is bound, the asset exists FOR THIS BRIEF
// under an active brief, has bytes and is not a video, and the ids can be carried in a signed URL. Every
// failure is pre-create: the caller wraps it in notCreated so the claim is released.
func (d *RedditDispatcher) checkRedditAsset(ctx context.Context, brief *model.CampaignBrief, assetID string) error {
	if d.assets == nil {
		return domain.PreflightRefusalf("reddit config references creative asset %s but the creative-asset store is not configured", assetID)
	}
	meta, err := d.assets.GetAssetMetadata(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.PreflightRefusalf("reddit config references creative asset %s, which does not exist for this brief", assetID)
		}
		return fmt.Errorf("reddit: read creative asset %s: %w", assetID, err)
	}
	if meta == nil || meta.ByteSize <= 0 {
		return domain.PreflightRefusalf("reddit config references creative asset %s, which has no stored image bytes", assetID)
	}
	// A video would pass every check above, and the signed route would then refuse to serve it
	// as an image only when Reddit fetched it at promoted-post time — after the campaign and ad
	// group already exist, leaving a partial campaign. Refuse it here, before any Reddit request.
	if meta.MimeType == model.MimeTypeMP4 {
		return domain.PreflightRefusalf("creative asset %s is a video, not an image; redditConfig.imageAssetId accepts PNG or JPEG", assetID)
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
func (d *RedditDispatcher) lateAssetURL(brief *model.CampaignBrief, assetID string, minted *[]string) func(context.Context) (string, error) {
	// One URL per post on a one-ad-per-variant create (#549 gap 6), each appended to minted.
	return d.lateAssetURLKind(assetsign.KindImage, brief, assetID, minted)
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

// scrubRedditOutcome removes every minted signed URL (an image post's, or a video post's video
// and thumbnail, #549) from what a create returns. Reddit may echo media_url or thumbnail_url in
// an error body, which would carry a signature into the job error and the persisted
// Steps/AdWarning. Classification is unaffected (scrubErr preserves errors.Is/As). Empty entries
// (URLs never minted) are skipped.
func scrubRedditOutcome(err error, result *reddit.CampaignResult, minted ...string) error {
	for _, m := range minted {
		if m == "" {
			continue
		}
		err = scrubErr(err, m)
		if result != nil {
			result.AdWarning = scrubSignedURL(result.AdWarning, m)
			for i := range result.Steps {
				result.Steps[i] = scrubSignedURL(result.Steps[i], m)
			}
		}
	}
	return err
}
