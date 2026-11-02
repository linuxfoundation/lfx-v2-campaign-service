// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Performance Max asset groups (LFXV2-2665)
// ---------------------------------------------------------------------------
//
// A Performance Max campaign has NO AD GROUPS AND NO ADS. Its creative lives in an
// ASSET GROUP: a named container holding a final URL and a set of assets, each linked
// by a FIELD TYPE that says what the asset is for — HEADLINE, DESCRIPTION, LOGO and so
// on. Google then assembles ads from those parts for whichever surface it serves on.
//
// That makes this file the Performance Max counterpart of demandgen_creative.go, and
// the two differ in exactly one structural way worth stating up front: Demand Gen
// builds ONE ad object that embeds every asset reference inline, while Performance Max
// creates the assets, then creates the asset group together with a link per asset
// carrying the field type, in one atomic googleAds:mutate. Two calls: Google refuses a
// non-retail asset group created apart from its links (#437), and a link must name an
// asset that already exists.
//
// Everything the two channels genuinely share is shared: the slot table shape, the URL
// checks, the hardened fetch, the geometry rules, the text-weight accounting and the
// base64 rendering all come from demandgen_creative.go. What is NOT shared is the
// numbers — Performance Max requires three headlines where Demand Gen requires one, and
// its portrait slot is 4:5 at a different minimum. Those are stated here, separately,
// because the one trap in reusing this machinery is assuming the limits travel with the
// helpers.
//
// The split between pure validation and network I/O is identical to Demand Gen's and is
// there for the same reason: validatePerformanceMaxCreative runs inside
// preflightCampaignKind so ValidateCampaignInputKind refuses exactly what the create
// cascade refuses, and the fetch runs in the cascade but BEFORE the budget mutate, so a
// 404 or a mis-shaped image still costs nothing.

const (
	// Headline and description counts are Performance Max's own and are NOT Demand
	// Gen's. Google requires THREE headlines here against Demand Gen's one, which is
	// the specific number a reader is most likely to assume travels with the shared
	// helpers. The display WEIGHTS coincide with the RSA ones — 30 and 90 — exactly as
	// they do on Demand Gen, which is what makes the counts the thing to state.
	minPerformanceMaxHeadlines = 3
	maxPerformanceMaxHeadlines = 15

	// A long headline is its own field type (LONG_HEADLINE), not a headline that
	// happens to be long, and Google requires at least one.
	minPerformanceMaxLongHeadlines = 1
	maxPerformanceMaxLongHeadlines = 5

	minPerformanceMaxDescriptions = 2
	maxPerformanceMaxDescriptions = 5

	// maxPerformanceMaxShortDescriptionWeight is the extra rule descriptions carry
	// here: of the two-to-five descriptions, at least ONE must fit the short slot
	// Google renders on constrained surfaces. The others may run to the full 90.
	maxPerformanceMaxShortDescriptionWeight = 60

	// maxPerformanceMaxBusinessNameWeight is Google's documented limit for the
	// BUSINESS_NAME field type, in the same double-width units.
	maxPerformanceMaxBusinessNameWeight = 25

	// minPerformanceMaxLogos/maxPerformanceMaxLogos cover the square LOGO field type,
	// which Google requires. LANDSCAPE_LOGO is optional and bounded separately.
	minPerformanceMaxLogos          = 1
	maxPerformanceMaxLogos          = 5
	maxPerformanceMaxLandscapeLogos = 5

	// maxPerformanceMaxMarketingImages is the COMBINED ceiling across the three
	// marketing shapes, stated the way Google states it. Logos are counted separately.
	maxPerformanceMaxMarketingImages = 20

	// maxPerformanceMaxVideos bounds the optional YOUTUBE_VIDEO assets. Unlike images
	// these are not fetched — a YouTube video asset carries an id, and Google resolves
	// it — so this is purely a count bound.
	maxPerformanceMaxVideos = 5

	// maxAssetGroupNameRunes mirrors the campaign-name bound. Google documents no
	// separate limit for an asset group name; this is the same payload-shape bound the
	// campaign name takes, applied so an over-long name is refused before the budget
	// rather than at the asset-group mutate, which runs after the campaign exists.
	maxAssetGroupNameRunes = maxCampaignNameRunes

	// maxDisplayPathRunes is Google's documented limit for each of the two display-path
	// segments that render after the domain in a Performance Max ad.
	maxDisplayPathRunes = 15
)

// Asset field types, as AssetFieldTypeEnum names them. Written out rather than derived
// from the slot label, because the enum is the wire contract and a derived name that
// drifted would be rejected at the asset group mutate, which carries the links — and
// which runs after the campaign, the budget and the assets all exist.
const (
	assetFieldHeadline             = "HEADLINE"
	assetFieldLongHeadline         = "LONG_HEADLINE"
	assetFieldDescription          = "DESCRIPTION"
	assetFieldBusinessName         = "BUSINESS_NAME"
	assetFieldMarketingImage       = "MARKETING_IMAGE"
	assetFieldSquareMarketingImage = "SQUARE_MARKETING_IMAGE"
	assetFieldPortraitMarketingImg = "PORTRAIT_MARKETING_IMAGE"
	assetFieldLogo                 = "LOGO"
	assetFieldLandscapeLogo        = "LANDSCAPE_LOGO"
	assetFieldYouTubeVideo         = "YOUTUBE_VIDEO"
	performanceMaxAssetGroupSuffix = " - Asset Group"
	performanceMaxAssetGroupPaused = "PAUSED"
)

// PerformanceMaxCreative is the asset-group creative for a Performance Max campaign. It
// is PERFORMANCE MAX ONLY — refused at preflight on every other channel, the mirror of
// the way DemandGenCreative is refused off Demand Gen and the extension assets off
// Search.
//
// Images are given as URLs and downloaded by this service, for the reason stated in
// demandgen_creative.go's header: Google's ImageAsset carries bytes and will not fetch a
// URL, and that is a property of the asset type rather than of a channel.
type PerformanceMaxCreative struct {
	// The three marketing image slots plus the two logo slots. MarketingImages and
	// SquareMarketingImages are each required by Google; PortraitImages and
	// LandscapeLogoImages are optional.
	MarketingImages       []string
	SquareMarketingImages []string
	PortraitImages        []string
	LogoImages            []string
	LandscapeLogoImages   []string

	// Headlines (3-15), LongHeadlines (1-5) and Descriptions (2-5, at least one of
	// which fits the short slot) are three DISTINCT field types, not one list bucketed
	// by length.
	Headlines     []string
	LongHeadlines []string
	Descriptions  []string

	// BusinessName is required by Google.
	BusinessName string

	// YouTubeVideoIDs are optional YouTube video ids (the 11-character watch id, not a
	// URL). A Performance Max campaign without one still serves; Google will generate
	// video from the other assets, and an operator who would rather it did not supplies
	// their own here.
	YouTubeVideoIDs []string

	// AssetGroupName is optional. Absent, the group is named from the event, which
	// keeps it findable next to the campaign it belongs to.
	AssetGroupName string

	// Path1/Path2 are the optional display-path segments rendered after the domain.
	Path1 string
	Path2 string
}

// empty reports whether the caller asked for no asset group at all. A Performance Max
// campaign with no asset group is still CREATED rather than refused — it is a real,
// reconcilable campaign that an operator can finish in the Google Ads UI, and the
// closing step says so. The judgement is the same one CreateDemandGenCampaign makes
// about a missing creative, and it matters more here: refusing would also refuse
// ADOPTION of a Performance Max campaign that already exists upstream with its asset
// group built by hand.
func (p PerformanceMaxCreative) empty() bool {
	return len(p.MarketingImages) == 0 &&
		len(p.SquareMarketingImages) == 0 &&
		len(p.PortraitImages) == 0 &&
		len(p.LogoImages) == 0 &&
		len(p.LandscapeLogoImages) == 0 &&
		len(p.Headlines) == 0 &&
		len(p.LongHeadlines) == 0 &&
		len(p.Descriptions) == 0 &&
		len(p.YouTubeVideoIDs) == 0 &&
		strings.TrimSpace(p.BusinessName) == "" &&
		strings.TrimSpace(p.AssetGroupName) == "" &&
		strings.TrimSpace(p.Path1) == "" &&
		strings.TrimSpace(p.Path2) == ""
}

// performanceMaxImageSlots is every image slot for this channel, in the order the asset
// group links them. Ratios and minimums are Google's own, and three of the five differ
// from the Demand Gen slot of the same name — which is why the two tables are separate
// rather than one table with a channel column.
//
// jsonKey holds the ASSET FIELD TYPE rather than a payload key, because a Performance
// Max image is linked by field type and never nested under a named array the way a
// Demand Gen image is.
var performanceMaxImageSlots = []imageSlot{
	{label: "Performance Max marketing image", jsonKey: assetFieldMarketingImage, ratioW: 191, ratioH: 100, minW: 600, minH: 314},
	{label: "Performance Max square marketing image", jsonKey: assetFieldSquareMarketingImage, ratioW: 1, ratioH: 1, minW: 300, minH: 300},
	{label: "Performance Max portrait marketing image", jsonKey: assetFieldPortraitMarketingImg, ratioW: 4, ratioH: 5, minW: 480, minH: 600},
	{label: "Performance Max logo", jsonKey: assetFieldLogo, ratioW: 1, ratioH: 1, minW: 128, minH: 128},
	{label: "Performance Max landscape logo", jsonKey: assetFieldLandscapeLogo, ratioW: 4, ratioH: 1, minW: 512, minH: 128},
}

// Indices into performanceMaxImageSlots. The three rules that name a specific slot —
// the combined marketing cap, the logo minimum and the landscape-logo ceiling — address
// it by index rather than by matching on a label, so a reworded label cannot silently
// detach a rule from the slot it governs.
const (
	pmaxSlotMarketing = 0
	pmaxSlotSquare    = 1
	pmaxSlotPortrait  = 2
	pmaxSlotLogo      = 3
	pmaxSlotLandscape = 4
)

// performanceMaxPlan is the validated asset group: per-slot image URLs positionally
// parallel to performanceMaxImageSlots, the text already trimmed and bucketed by field
// type, and the group's own name and display paths.
type performanceMaxPlan struct {
	urls           [][]string
	headlines      []string
	longHeadlines  []string
	descriptions   []string
	businessName   string
	videoIDs       []string
	assetGroupName string
	path1          string
	path2          string
	// resolved is CampaignInput.ResolvedImages, carried for the reason
	// demandGenCreativePlan.resolved gives.
	resolved map[string][]byte
	// present is false when the caller asked for no asset group, which leaves the
	// campaign-only behaviour intact.
	present bool
}

func (p performanceMaxPlan) imageCount() int {
	n := 0
	for _, s := range p.urls {
		n += len(s)
	}
	return n
}

// validatePerformanceMaxCreative resolves the asset-group input WITHOUT sending or
// fetching anything, alongside every other validator in preflightCampaignKind — so the
// adoption path refuses exactly what the create path refuses.
//
// Every bound it enforces is one GOOGLE enforces when an asset group is enabled. That is
// what makes enforcing them here safe rather than over-refusing: the refusal is not
// invented, only moved from a mutate that runs after a budget, a campaign and an asset
// group have committed to a point where nothing exists.
func validatePerformanceMaxCreative(kind string, in CampaignInput) (performanceMaxPlan, error) {
	p := in.PerformanceMaxCreative
	if p.empty() {
		return performanceMaxPlan{}, nil
	}
	if kind != campaignKindPerformanceMax {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max creative (asset group headlines, images, logos, business name) is supported on Performance Max campaigns only, not on %s", kind)
	}

	plan := performanceMaxPlan{present: true, urls: make([][]string, len(performanceMaxImageSlots)), resolved: in.ResolvedImages}

	raw := [][]string{p.MarketingImages, p.SquareMarketingImages, p.PortraitImages, p.LogoImages, p.LandscapeLogoImages}
	marketing := 0
	for i, slot := range performanceMaxImageSlots {
		urls, err := validateImageEntries(slot, raw[i], in.ResolvedImages)
		if err != nil {
			return performanceMaxPlan{}, err
		}
		plan.urls[i] = urls
		if i == pmaxSlotMarketing || i == pmaxSlotSquare || i == pmaxSlotPortrait {
			marketing += len(urls)
		}
	}
	if marketing > maxPerformanceMaxMarketingImages {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group accepts at most %d marketing images across all three shapes, got %d", maxPerformanceMaxMarketingImages, marketing)
	}
	// Unlike Demand Gen's reciprocal pair, Google requires EACH of these two shapes on
	// an asset group: there is no reading under which one substitutes for the other.
	if len(plan.urls[pmaxSlotMarketing]) == 0 {
		return performanceMaxPlan{}, errors.New("google-ads Performance Max asset group needs at least one marketing image (1.91:1); Google requires the landscape shape on every asset group")
	}
	if len(plan.urls[pmaxSlotSquare]) == 0 {
		return performanceMaxPlan{}, errors.New("google-ads Performance Max asset group needs at least one square marketing image (1:1); Google requires the square shape on every asset group")
	}
	if n := len(plan.urls[pmaxSlotLogo]); n < minPerformanceMaxLogos {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group needs at least %d square logo, got %d", minPerformanceMaxLogos, n)
	} else if n > maxPerformanceMaxLogos {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group accepts at most %d square logos, got %d", maxPerformanceMaxLogos, n)
	}
	if n := len(plan.urls[pmaxSlotLandscape]); n > maxPerformanceMaxLandscapeLogos {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group accepts at most %d landscape logos, got %d", maxPerformanceMaxLandscapeLogos, n)
	}

	headlines, err := validateCreativeText("Performance Max asset group", "headline", p.Headlines, minPerformanceMaxHeadlines, maxPerformanceMaxHeadlines, maxHeadlineWeight)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	plan.headlines = headlines

	longHeadlines, err := validateCreativeText("Performance Max asset group", "long headline", p.LongHeadlines, minPerformanceMaxLongHeadlines, maxPerformanceMaxLongHeadlines, maxDescriptionWeight)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	plan.longHeadlines = longHeadlines

	descriptions, err := validateCreativeText("Performance Max asset group", "description", p.Descriptions, minPerformanceMaxDescriptions, maxPerformanceMaxDescriptions, maxDescriptionWeight)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	// At least one description has to fit the SHORT slot. Checked as "any of them"
	// rather than "the first of them": the caller's ordering is not a contract this
	// package has ever stated, and reading a position as a field type would refuse a
	// perfectly valid list whose short description happens to be written second.
	hasShort := false
	for _, d := range descriptions {
		if textWeight(d) <= maxPerformanceMaxShortDescriptionWeight {
			hasShort = true
			break
		}
	}
	if !hasShort {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group needs at least one description with a display width of %d or less (Google renders a short description on constrained surfaces); every supplied description is longer", maxPerformanceMaxShortDescriptionWeight)
	}
	plan.descriptions = descriptions

	name := strings.TrimSpace(p.BusinessName)
	if name == "" {
		return performanceMaxPlan{}, errors.New("google-ads Performance Max asset group requires a business name (Google marks the field required)")
	}
	if w := textWeight(name); w > maxPerformanceMaxBusinessNameWeight {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max business name %q has a display width of %d, exceeding the %d limit", capForError(name), w, maxPerformanceMaxBusinessNameWeight)
	}
	plan.businessName = name

	videos, err := validateYouTubeVideoIDs("Performance Max", "Performance Max asset group", maxPerformanceMaxVideos, p.YouTubeVideoIDs)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	plan.videoIDs = videos

	groupName := strings.TrimSpace(p.AssetGroupName)
	if groupName == "" {
		groupName = sanitizeNamePart(in.EventName) + performanceMaxAssetGroupSuffix
	} else if strings.ContainsAny(groupName, "\x00\n\r") {
		// The caller-supplied branch REFUSES where the derived branch above SANITIZES, and
		// the asymmetry is the point: sanitizeNamePart also maps `|` to a space and collapses
		// whitespace runs, which Google accepts in an asset group name. Running an operator's
		// own typed name through it would silently return a different name than they asked
		// for; running a name THIS code derived through it rewrites nothing anyone chose.
		//
		// What is refused is exactly what the upstream field cannot hold — NUL, LF and CR,
		// the same three runes returnedCampaignName names — and nothing more. TAB,
		// U+2028/U+2029 and format characters are legal here, so refusing them would be
		// over-refusal: a create Google would have accepted, stopped locally.
		//
		// Refusing rather than passing it through matters because the asset group create
		// (googleAds:mutate) runs AFTER the budget and campaign are created and paid for. An
		// unusable name there strands a PAID campaign; the same name caught here costs one
		// corrected request.
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group name %q contains NUL, LF or CR, which Google cannot store; remove the control character or omit the name to have one composed from the event", capForError(groupName))
	}
	if n := utf8.RuneCountInString(groupName); n > maxAssetGroupNameRunes {
		return performanceMaxPlan{}, fmt.Errorf("google-ads Performance Max asset group name %q is %d characters, exceeding the %d limit", capForError(groupName), n, maxAssetGroupNameRunes)
	}
	plan.assetGroupName = groupName

	path1, err := validateDisplayPath("path1", p.Path1)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	path2, err := validateDisplayPath("path2", p.Path2)
	if err != nil {
		return performanceMaxPlan{}, err
	}
	// Google reads path2 as the segment AFTER path1, so a path2 with no path1 renders
	// nothing. Refused rather than silently dropped or promoted: promoting it would put
	// the caller's text in a position they did not choose, and dropping it would show an
	// ad with a display path the operator believes is there.
	if path2 != "" && path1 == "" {
		return performanceMaxPlan{}, errors.New("google-ads Performance Max display path2 was supplied without path1; Google renders path2 only after path1, so supply path1 as well or move the text into it")
	}
	plan.path1, plan.path2 = path1, path2

	return plan, nil
}

// validateDisplayPath bounds one display-path segment and refuses the characters Google
// refuses there. A path segment renders inside the visible URL, so a slash or a space in
// it is rejected by Google at the asset-group mutate — after the campaign exists.
func validateDisplayPath(label, raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil
	}
	if n := utf8.RuneCountInString(p); n > maxDisplayPathRunes {
		return "", fmt.Errorf("google-ads Performance Max display %s %q is %d characters, exceeding the %d limit", label, capForError(p), n, maxDisplayPathRunes)
	}
	if strings.ContainsAny(p, "/?#&= \t\n") {
		return "", fmt.Errorf("google-ads Performance Max display %s %q contains a character Google does not accept in a display path (no slashes, spaces or URL punctuation)", label, capForError(p))
	}
	return p, nil
}

// isYouTubeIDRune reports whether r is in YouTube's id alphabet: the unreserved
// base64url characters.
func isYouTubeIDRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_':
		return true
	}
	return false
}

// validateYouTubeVideoIDs checks the SHAPE of each id, for whichever channel is
// asking. Whether the video exists, is
// public, and belongs to a channel the account may advertise is Google's to judge — this
// client has no YouTube credentials and inventing a lookup here would make
// ValidateCampaignInput send a request, which its contract forbids.
//
// A URL is REFUSED rather than parsed into an id. Accepting one would mean guessing
// which of youtu.be, /watch?v=, /shorts/ and /embed/ forms the caller meant, and a guess
// that extracted the wrong substring would attach the wrong video to a real campaign.
//
// The refusal reports the value through redactURLForError, like every other URL this
// package puts in an error. That arm fires precisely BECAUSE the value is URL-shaped, and
// these errors persist unencrypted as Steps entries — a pasted share link carries its
// tracking or signing query with it, so echoing the raw string would write a credential to
// the database to tell the caller something scheme+host+path already tells them. Every
// other arm caps the value instead: those have established it is NOT a URL, but nothing
// bounds its length.
//
// The two labels and the ceiling are PARAMETERS because two channels now reference
// YouTube videos and they do not share a vocabulary: a Performance Max video hangs
// off an asset group, a Video campaign's hangs off a responsive video ad, and the
// ceilings are each channel's own. The labels were literals while Performance Max
// was the only caller, and reusing the helper unchanged would have told a Video
// caller their "Performance Max YouTube video" was malformed — a helper answering
// for a channel it was not called about. `channel` names the owner in the per-item
// errors, `owner` the container in the count error, exactly as validateCreativeText
// splits its own two labels.
func validateYouTubeVideoIDs(channel, owner string, maxVideos int, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("google-ads %s YouTube video %d is empty", channel, i)
		}
		if strings.Contains(id, "/") || strings.Contains(id, ":") || strings.Contains(id, "?") {
			return nil, fmt.Errorf("google-ads %s YouTube video %d looks like a URL (%q); supply the bare video id instead, because this client will not guess which part of a URL is the id", channel, i, redactURLForError(id))
		}
		// The id alphabet is YouTube's own: unreserved base64url characters. Length is
		// deliberately NOT pinned to 11 — that is the length every id has had, not a
		// documented guarantee, and refusing a longer one would refuse a video Google
		// would have accepted.
		for _, r := range id {
			if !isYouTubeIDRune(r) {
				return nil, fmt.Errorf("google-ads %s YouTube video %q contains %q, which is not a YouTube id character", channel, capForError(id), r)
			}
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("google-ads %s YouTube video %q is listed more than once", channel, capForError(id))
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) > maxVideos {
		return nil, fmt.Errorf("google-ads %s accepts at most %d YouTube videos, got %d", owner, maxVideos, len(out))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Creating the assets and the asset group
// ---------------------------------------------------------------------------

// textAssetCreate is TextAsset. Only `text` exists on the input side.
type textAssetCreate struct {
	Text string `json:"text"`
}

// youTubeVideoAssetCreate is YoutubeVideoAsset. Only the id is sent; the title is
// OUTPUT_ONLY and Google fills it from the video.
type youTubeVideoAssetCreate struct {
	YouTubeVideoID string `json:"youtubeVideoId"`
}

// performanceMaxAssetCreate is an Asset create carrying exactly one of the three asset
// types this channel uses. It is a oneof on the wire, so at most one pointer is ever
// non-nil — enforced by construction in the three builders below rather than by a check,
// because there is no code path that could set two.
//
// Kept separate from both assets.go's assetCreate (the Search extension oneof) and
// demandgen_creative.go's demandGenAssetCreate for the reason that file states: a struct
// spanning two channels makes every reader work out which half applies.
type performanceMaxAssetCreate struct {
	TextAsset         *textAssetCreate         `json:"textAsset,omitempty"`
	ImageAsset        *imageAssetCreate        `json:"imageAsset,omitempty"`
	YouTubeVideoAsset *youTubeVideoAssetCreate `json:"youtubeVideoAsset,omitempty"`
	// Name is an optional human-visible asset label. This client NEVER sets it — see
	// buildPerformanceMaxAssets for why an image's name would export the caller's URL
	// into the shared account.
	Name string `json:"name,omitempty"`
}

// assetGroupCreate is AssetGroup. Status is PAUSED to match the campaign: a campaign
// created paused whose asset group was ENABLED would be a campaign one toggle away from
// spending, which is not what "created paused" is understood to mean anywhere else in
// this package.
type assetGroupCreate struct {
	// ResourceName is set only to the TEMPORARY name the asset group carries inside the
	// googleAds:mutate that creates it with its links; Google assigns the real one.
	ResourceName string   `json:"resourceName,omitempty"`
	Name         string   `json:"name"`
	Campaign     string   `json:"campaign"`
	FinalUrls    []string `json:"finalUrls"`
	Status       string   `json:"status"`
	Path1        string   `json:"path1,omitempty"`
	Path2        string   `json:"path2,omitempty"`
}

// assetGroupStatusUpdate flips an existing asset group's status. Separate from
// assetGroupCreate because a :mutate operation carries exactly one of create/update/remove
// and an update must send ONLY the masked field — sending the create shape with its
// required name/campaign/finalUrls would either be rejected or, worse, rewrite them.
type assetGroupStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
}

// unconfirmedAssetGroupStatusError marks an asset group status mutate Google ACCEPTED but
// did not acknowledge usably, the Performance Max counterpart of
// unconfirmedCampaignStatusError and satisfying the same Unconfirmed() interface
// IsOutcomeUnconfirmed honors. The flip may well have been applied, so the dispatcher must
// carry it through as "verify upstream before retrying" rather than "nothing was modified".
type unconfirmedAssetGroupStatusError struct{ err error }

func (e *unconfirmedAssetGroupStatusError) Error() string     { return e.err.Error() }
func (e *unconfirmedAssetGroupStatusError) Unwrap() error     { return e.err }
func (e *unconfirmedAssetGroupStatusError) Unconfirmed() bool { return true }

// UpdateAssetGroupStatus flips one Performance Max asset group between ENABLED and PAUSED.
//
// It exists because Performance Max has no ad groups and no ads: its creative is an asset
// group, created PAUSED by buildPerformanceMaxCreative for the same reason every other
// create in this client is paused — nothing serves until a human enables it. Without this
// method the campaign resource could be flipped to ENABLED while the asset group stayed
// PAUSED, which reports a launch that cannot deliver. That is precisely the false success
// ErrCampaignNotProvisioned exists to prevent, so the one channel whose creative this
// client creates paused must also be the channel it can un-pause.
//
// Held to the same standard as UpdateCampaignStatus, and for the same reasons: the mutate
// is sent idempotent because re-applying the same ENABLED/PAUSED converges on identical
// state, so declining bounded 429 retries would turn ordinary throttling into an avoidable
// UNCONFIRMED; and a 2xx that does not acknowledge the one operation is UNCONFIRMED rather
// than assumed applied.
func (c *Client) UpdateAssetGroupStatus(ctx context.Context, assetGroupID, status string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	if status != StatusEnabled && status != StatusPaused {
		return fmt.Errorf("google-ads: unsupported asset group status %q (want %s or %s)", status, StatusEnabled, StatusPaused)
	}
	id := strings.TrimSpace(assetGroupID)
	if id == "" {
		return fmt.Errorf("google-ads: asset group status update needs an asset group id")
	}
	// Held to UpdateCampaignStatus's rule for the same reason: the id is interpolated into a
	// resourceName, Google asset group ids are digits, and anything else could alter the
	// resource path. `customerIDRE` is used for its SHAPE and is length-agnostic by design
	// — an asset group id is not a customer id and does not share its width; see the
	// matcher's own comment in client.go for why narrowing it would break this call site.
	if !customerIDRE.MatchString(id) {
		return fmt.Errorf("google-ads: asset group id %q is not numeric", assetGroupID)
	}

	groupResource := "customers/" + c.account.CustomerID + "/assetGroups/" + id
	req := mutateRequest{Operations: []mutateOperation{{
		Update:     assetGroupStatusUpdate{ResourceName: groupResource, Status: status},
		UpdateMask: "status",
	}}}
	resp, retries, err := c.doRequestCounted(ctx, http.MethodPost, c.customerPath("assetGroups:mutate"), req, true)
	if err != nil {
		return retriedUnconfirmed("asset group status update", retries,
			fmt.Errorf("google-ads asset group %s status update to %s failed: %w", id, status, err))
	}
	if cErr := c.checkStatusMutateResults(resp, "assetGroups", "asset group", []string{groupResource}, true); cErr != nil {
		return &unconfirmedAssetGroupStatusError{err: fmt.Errorf("google-ads asset group %s status update to %s: %w", id, status, cErr)}
	}
	return nil
}

// assetGroupAssetCreate links one already-created asset to the asset group under a field
// type. The link is a separate resource from both ends, and it is created in the same
// googleAds:mutate as the group, which it names by the group's temporary resource name.
type assetGroupAssetCreate struct {
	AssetGroup string `json:"assetGroup"`
	Asset      string `json:"asset"`
	FieldType  string `json:"fieldType"`
}

// pendingAsset pairs an asset create with the field type its link will carry. The two
// have to travel together because the assets:mutate response is positional — result i
// describes operation i — and nothing in the response says what the asset was for.
type pendingAsset struct {
	create    performanceMaxAssetCreate
	fieldType string
}

// buildPerformanceMaxAssets turns the validated plan plus the fetched image bytes into
// the full asset list, in a FIXED order: text first, then images in slot order, then
// videos. The order is not cosmetic — it is what makes the positional pairing with the
// mutate response readable, and a test pins it.
func buildPerformanceMaxAssets(plan performanceMaxPlan, images []fetchedImage) []pendingAsset {
	out := make([]pendingAsset, 0, len(plan.headlines)+len(plan.longHeadlines)+len(plan.descriptions)+1+len(images)+len(plan.videoIDs))
	addText := func(texts []string, field string) {
		for _, t := range texts {
			out = append(out, pendingAsset{create: performanceMaxAssetCreate{TextAsset: &textAssetCreate{Text: t}}, fieldType: field})
		}
	}
	addText(plan.headlines, assetFieldHeadline)
	addText(plan.longHeadlines, assetFieldLongHeadline)
	addText(plan.descriptions, assetFieldDescription)
	// Guarded rather than added unconditionally. The validator REQUIRES a business
	// name on every present plan, so this is never empty on the cascade's path — but
	// an unguarded append would turn a zero plan into one empty TEXT asset, which is
	// exactly the operation that makes createPerformanceMaxAssetGroup's len(pending)
	// == 0 early return unreachable and sends Google a text asset with no text.
	if plan.businessName != "" {
		addText([]string{plan.businessName}, assetFieldBusinessName)
	}
	for _, img := range images {
		out = append(out, pendingAsset{
			// NO Name. The obvious label — the slot plus the source URL — ships the
			// caller's creative URL, query string and all, to Google as a permanent
			// human-visible asset label in the shared Foundation account: the same
			// value sanitizeSnapshotURL reduces to scheme+host before it may be
			// written to this service's own database. Redacted going in, exported
			// verbatim going out. It was also an unbounded caller-controlled string
			// in a payload sent AFTER the campaign exists, which is the orphan that
			// maxAssetGroupNameRunes is positioned in preflight to prevent.
			//
			// Omitted rather than reduced to the bare slot label, which matches the
			// Demand Gen sibling (demandGenAssetCreate sets no name either) and avoids
			// handing Google twenty marketing images that share one name.
			create:    performanceMaxAssetCreate{ImageAsset: &imageAssetCreate{Data: base64Image(img.data)}},
			fieldType: performanceMaxImageSlots[img.slot].jsonKey,
		})
	}
	for _, v := range plan.videoIDs {
		out = append(out, pendingAsset{create: performanceMaxAssetCreate{YouTubeVideoAsset: &youTubeVideoAssetCreate{YouTubeVideoID: v}}, fieldType: assetFieldYouTubeVideo})
	}
	return out
}

// createPerformanceMaxAssetGroup creates the assets, then the asset group together with
// the links between them — two mutates, in that order.
//
// The split is forced from both sides. Google requires the asset group and all of its
// AssetGroupAsset links in ONE request (#437), so those two cannot be separated; and a link
// must name an asset that already exists, so the assets go first. Assets first also fails
// in the less confusing place: loose assets are merely account-level litter, while the
// group and its links are atomic, so no empty group can be left behind.
//
// Every mutate past the campaign returns its error ALONGSIDE whatever was created, for
// the reason the whole package does: the campaign exists either way, and an operator
// reconciling it needs the ids of what got as far as being created.
func (c *Client) createPerformanceMaxAssetGroup(ctx context.Context, campaignResource, campaignID, finalURL string, plan performanceMaxPlan, images []fetchedImage) (assetIDs []string, assetGroupID string, linkCount int, err error) {
	pending := buildPerformanceMaxAssets(plan, images)
	if len(pending) == 0 {
		return nil, "", 0, nil
	}

	// Step A: the assets.
	assetOps := make([]mutateOperation, 0, len(pending))
	for _, p := range pending {
		assetOps = append(assetOps, mutateOperation{Create: p.create})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, "", 0, fmt.Errorf("google-ads Performance Max asset creation UNCONFIRMED (%d asset(s) may exist; campaign %s created — verify in Google Ads before retrying): %w", len(assetOps), campaignID, err)
		}
		return nil, "", 0, fmt.Errorf("google-ads Performance Max asset creation failed (%d asset(s); campaign %s created): %w", len(assetOps), campaignID, err)
	}
	var assetResults mutateResponse
	// EXACT equality, like every other create-path mutate here: a short response leaves
	// assets unaccounted for and an extra result is a response that does not describe
	// what was sent. Both are UNCONFIRMED rather than failed — the assets may exist.
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil {
		return nil, "", 0, fmt.Errorf("google-ads Performance Max asset creation UNCONFIRMED (campaign %s created; 2xx with a malformed mutate response for %d asset(s) — assets may exist — verify in Google Ads before retrying)", campaignID, len(assetOps))
	}
	if len(assetResults.Results) != len(assetOps) {
		// The body PARSED, so whatever ids it did carry are real — and they are the only
		// handle an operator has on account-level assets that may already exist. They go
		// back WITH the error rather than being dropped alongside it, the same way the
		// malformed-resource-name arm below returns what it had got to.
		return c.parsedAssetIDs(assetResults), "", 0, fmt.Errorf("google-ads Performance Max asset creation UNCONFIRMED (campaign %s created; 2xx returned %d result(s) for %d asset(s) — assets may exist — verify in Google Ads before retrying)", campaignID, len(assetResults.Results), len(assetOps))
	}
	assetResources := make([]string, 0, len(assetOps))
	assetIDs = make([]string, 0, len(assetOps))
	for i, r := range assetResults.Results {
		// assetID checks the kind, the ACCOUNT and the numeric trailing id, and returns
		// "" for anything else — so a wrong-account assets resource can neither be
		// linked into this campaign's asset group nor persisted as one of its ids.
		id := c.assetID(r.ResourceName)
		if id == "" {
			return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset creation UNCONFIRMED (campaign %s created; malformed asset resource name %q at index %d — assets may exist — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		assetIDs = append(assetIDs, id)
		assetResources = append(assetResources, r.ResourceName)
	}

	// Step B: the asset group AND its links, in ONE GoogleAdsService.Mutate (#437).
	//
	// Google refuses the two-step shape for a non-retail Performance Max campaign: "AssetGroup
	// objects cannot be created in isolation using the AssetGroupService ... an AssetGroup and
	// all associated AssetGroupAsset objects must be created in the same bulk mutate request"
	// (Google Ads API, Performance Max asset groups guide). A standalone assetGroups:mutate
	// fails its minimum-asset check, so every create that sent creative ended at this step
	// with a campaign that could never be activated.
	//
	// The group is named by a TEMPORARY resource name (a negative id), which the links in the
	// same request reference and Google replaces with the real id. partialFailure is left
	// false, so the request is ATOMIC: either the group exists with every link, or nothing in
	// it was applied. That is why a definite failure reports NO group: the state the earlier
	// three-call cascade could leave, a group with nothing attached, cannot arise.
	tempGroup := "customers/" + c.account.CustomerID + "/assetGroups/" + performanceMaxTempAssetGroupID
	ops := make([]googleAdsMutateOperation, 0, 1+len(pending))
	ops = append(ops, googleAdsMutateOperation{AssetGroupOperation: &mutateOperation{Create: assetGroupCreate{
		ResourceName: tempGroup,
		Name:         plan.assetGroupName,
		Campaign:     campaignResource,
		FinalUrls:    []string{finalURL},
		Status:       performanceMaxAssetGroupPaused,
		Path1:        plan.path1,
		Path2:        plan.path2,
	}}})
	for i, p := range pending {
		ops = append(ops, googleAdsMutateOperation{AssetGroupAssetOperation: &mutateOperation{Create: assetGroupAssetCreate{
			AssetGroup: tempGroup,
			// THE RESOURCE NAME GOOGLE RETURNED, never one rebuilt from the parsed id.
			// A rebuilt name is this client asserting what Google said rather than
			// repeating it.
			Asset:     assetResources[i],
			FieldType: p.fieldType,
		}}})
	}
	wantLinks := len(ops) - 1
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("googleAds:mutate"), googleAdsMutateRequest{MutateOperations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset group %q creation UNCONFIRMED (campaign %s and %d asset(s) created; the group and its %d link(s) may exist — verify in Google Ads before retrying): %w", plan.assetGroupName, campaignID, len(assetIDs), wantLinks, err)
		}
		return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset group %q creation failed (campaign %s and %d asset(s) created; the group and its %d link(s) are created in one atomic request, so none of them exists): %w", plan.assetGroupName, campaignID, len(assetIDs), wantLinks, err)
	}

	var results googleAdsMutateResponse
	if uErr := json.Unmarshal(resp, &results); uErr != nil || len(results.MutateOperationResponses) == 0 {
		return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset group %q creation UNCONFIRMED (campaign %s created; 2xx with a malformed mutate response — the group and its links may exist — verify in Google Ads before retrying)", plan.assetGroupName, campaignID)
	}
	// Responses are index-aligned with the operations, so the group's is the first.
	first := results.MutateOperationResponses[0].AssetGroupResult
	if first == nil || first.ResourceName == "" {
		return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset group %q creation UNCONFIRMED (campaign %s created; 2xx with no asset group resource name — the group may exist — verify in Google Ads before retrying)", plan.assetGroupName, campaignID)
	}
	if verr := c.validateResourceKind("assetGroups", first.ResourceName, true); verr != nil {
		return assetIDs, "", 0, fmt.Errorf("google-ads Performance Max asset group %q creation UNCONFIRMED (campaign %s created; %w — verify in Google Ads before retrying)", plan.assetGroupName, campaignID, verr)
	}
	groupID := resourceID(first.ResourceName)

	// The group id is returned from here on even when the links cannot be confirmed: the
	// request was atomic and answered 2xx, so the group exists, and it is the handle an
	// operator needs. The COUNT stays zero unless every link is accounted for, because zero
	// is what keeps the activation gate from reading an unconfirmed group as ready to serve.
	//
	// Each link result is matched against the operation at its own index (#439): the
	// response is index-aligned, so result i must name exactly the link operation i asked for.
	// A truncated name, or one link name repeated for every result, then confirms nothing.
	confirmed := 0
	for i, r := range results.MutateOperationResponses[1:] {
		if i < len(pending) && r.AssetGroupAssetResult != nil &&
			c.isAssetGroupAssetLink(r.AssetGroupAssetResult.ResourceName, groupID, resourceID(assetResources[i]), pending[i].fieldType) {
			confirmed++
		}
	}
	if len(results.MutateOperationResponses) != len(ops) || confirmed != wantLinks {
		return assetIDs, groupID, 0, fmt.Errorf("google-ads Performance Max asset group link creation UNCONFIRMED (asset group %s created; 2xx described %d of %d link(s) — verify in Google Ads before retrying)", groupID, confirmed, wantLinks)
	}
	return assetIDs, groupID, wantLinks, nil
}

// isAssetGroupAssetLink reports whether name is exactly the assetGroupAssets resource, in
// THIS account, that links assetID into groupID under fieldType:
// `customers/<cid>/assetGroupAssets/<groupId>~<assetId>~<fieldType>` (#439). A link result
// naming anything else — another account, group, asset or field type, or a truncated
// composite — does not describe the link its operation created, so it is not counted as
// confirmed: the same reasoning the ad-group-ad results apply to their own composite names.
// requireNumericID is false because the trailing segment is composite.
//
// The ids are compared exactly and the field type case-insensitively, as the extension-link
// check in assets.go does: the field type is Google's own enum name echoed back, so a casing
// difference would be a change in how the API spells it rather than the wrong link, and
// recording zero links for a correct group would be an over-refusal at activation.
func (c *Client) isAssetGroupAssetLink(name, groupID, assetID, fieldType string) bool {
	if groupID == "" || assetID == "" || fieldType == "" {
		return false
	}
	if c.validateResourceKind("assetGroupAssets", name, false) != nil {
		return false
	}
	parts := strings.Split(resourceID(name), "~")
	return len(parts) == 3 && parts[0] == groupID && parts[1] == assetID && strings.EqualFold(parts[2], fieldType)
}

// performanceMaxTempAssetGroupID is the TEMPORARY id the asset group carries inside the one
// GoogleAdsService.Mutate that creates it with its links. Google treats a negative id as a
// placeholder, scoped to that single request, and returns the real resource name in the
// response; it is never persisted.
const performanceMaxTempAssetGroupID = "-1"

// googleAdsMutateRequest is the body of GoogleAdsService.Mutate (googleAds:mutate): a list of
// typed operations over different resources, applied together. Only the two operation kinds
// the Performance Max asset group needs are modelled.
type googleAdsMutateRequest struct {
	MutateOperations []googleAdsMutateOperation `json:"mutateOperations"`
}

// googleAdsMutateOperation is one MutateOperation: exactly one of its oneof arms is set. Each
// arm reuses mutateOperation, which is the same {create|update|remove} shape the resource
// services take.
type googleAdsMutateOperation struct {
	AssetGroupOperation      *mutateOperation `json:"assetGroupOperation,omitempty"`
	AssetGroupAssetOperation *mutateOperation `json:"assetGroupAssetOperation,omitempty"`
}

// googleAdsMutateResponse is the (subset of the) GoogleAdsService.Mutate response consumed
// here. mutateOperationResponses is index-aligned with the request's mutateOperations, and
// each carries the result arm matching its operation.
type googleAdsMutateResponse struct {
	MutateOperationResponses []struct {
		AssetGroupResult      *resourceNameResult `json:"assetGroupResult"`
		AssetGroupAssetResult *resourceNameResult `json:"assetGroupAssetResult"`
	} `json:"mutateOperationResponses"`
}

// resourceNameResult is a Mutate*Result under the default RESOURCE_NAME_ONLY response
// content type: the created resource's name and nothing else.
type resourceNameResult struct {
	ResourceName string `json:"resourceName"`
}

// performanceMaxCreativeStep summarises the asset group for the operator-facing step
// list.
func performanceMaxCreativeStep(plan performanceMaxPlan) string {
	marketing := plan.imageCount() - len(plan.urls[pmaxSlotLogo]) - len(plan.urls[pmaxSlotLandscape])
	parts := []string{
		fmt.Sprintf("%d marketing image(s), %d logo(s)", marketing, len(plan.urls[pmaxSlotLogo])+len(plan.urls[pmaxSlotLandscape])),
		fmt.Sprintf("%d headline(s), %d long headline(s), %d description(s)", len(plan.headlines), len(plan.longHeadlines), len(plan.descriptions)),
	}
	if len(plan.videoIDs) > 0 {
		parts = append(parts, fmt.Sprintf("%d YouTube video(s)", len(plan.videoIDs)))
	}
	return strings.Join(parts, ", ")
}
