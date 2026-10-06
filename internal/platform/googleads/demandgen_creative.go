// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	// Registered for their DecodeConfig side effect only. Google accepts GIF, JPEG
	// and PNG for Demand Gen image assets and nothing else, so these three are both
	// what we can decode and what we are allowed to send — the decoder set IS the
	// format allowlist, and a WebP that slips past the Content-Type check fails to
	// decode here rather than being uploaded and rejected after the campaign exists.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
)

// ---------------------------------------------------------------------------
// Demand Gen creative (LFXV2-2665)
// ---------------------------------------------------------------------------
//
// A Demand Gen campaign created by this service carried NO AD. The cascade built
// budget → campaign → ad group → geo and then told the operator to "upload images
// and publish in the Google Ads UI". A campaign with no ad cannot serve, so every
// Demand Gen campaign this service created needed a human to finish it by hand.
//
// This file closes that. It is the Demand Gen counterpart of ad_copy.go + assets.go
// and it differs from both in one structural way worth stating up front:
//
// GOOGLE WILL NOT FETCH AN IMAGE FOR US. Verified against the v23 proto
// (google/ads/googleads/v23/common/asset_types.proto):
//
//	message ImageAsset {
//	  // The raw bytes data of an image. This field is mutate only.
//	  optional bytes data = 5;
//
// There is no URL field on the input side. ImageDimension.url exists but is a URL
// that RETURNS an already-created image — output only. Every sibling platform in
// this repo takes an image URL in config and hands that URL to the platform
// (reddit.go's ImageURL, meta.go's per-variant ImageURL); Google is the one that
// cannot, so the bytes have to come from here.
//
// That makes this the only validator in the package that performs network I/O, and
// it is why the work is split in two:
//
//   - validateDemandGenCreative is PURE. Counts, text weights, business name, image
//     URL shape. It runs inside preflightCampaignKind with every other validator, so
//     ValidateCampaignInputKind refuses exactly what CreateDemandGenCampaign refuses —
//     the adoption-parity property the whole preflight exists to keep.
//   - fetchDemandGenImages does the network. It runs in the cascade BEFORE the budget
//     mutate, so an unreachable image, a 404, an over-sized file or a wrong aspect
//     ratio still costs nothing. The orphan guarantee holds; only the place it is
//     enforced moves, because "does this URL serve a 600x314 JPEG" is not locally
//     decidable in the way an over-long headline is.
//
// What is left to Google is therefore genuinely only what Google alone knows.

const (
	// Headline and description counts are Demand Gen's own, and they are NOT the
	// RSA numbers. ad_copy.go allows 15 headlines and 4 descriptions; Demand Gen
	// allows at least 1 and at most 5 of each. The WEIGHTS coincide exactly —
	// maxHeadlineWeight 30, maxDescriptionWeight 90 — which is precisely the trap:
	// the weight helpers are shared, the counts must not be.
	minDemandGenHeadlines    = 1
	maxDemandGenHeadlines    = 5
	minDemandGenDescriptions = 1
	maxDemandGenDescriptions = 5

	// maxDemandGenBusinessNameWeight is Google's documented "maximum display width
	// is 25" for business_name, measured in the same double-width units the
	// headlines and descriptions use.
	maxDemandGenBusinessNameWeight = 25

	// maxDemandGenCallToActionRunes bounds the optional call-to-action. Google
	// documents no display width for this field, so this is a payload bound rather
	// than an upstream limit being mirrored — deliberately generous, because
	// refusing text Google would have accepted is the costlier mistake.
	maxDemandGenCallToActionRunes = 30

	// minDemandGenLogos/maxDemandGenLogos are Google's own: "At least 1 and max 5
	// logo images can be specified." A Demand Gen ad without a logo is refused
	// upstream, which is why the minimum is enforced here rather than left to the
	// ad mutate — that mutate runs after the budget, campaign and ad group exist.
	minDemandGenLogos = 1
	maxDemandGenLogos = 5

	// maxDemandGenMarketingImages is the COMBINED ceiling across the four marketing
	// arrays, which is how Google states it: each array's doc says "Combined with
	// <the other three> the maximum is 20". Logos are counted separately and are
	// not part of this total.
	maxDemandGenMarketingImages = 20

	// maxDemandGenImageBytes caps a single downloaded image. Google's own image
	// asset limit is larger; this is the bound on what this service is willing to
	// pull over the network and hold in memory per image, and it is enforced with a
	// LimitReader rather than by trusting Content-Length.
	maxDemandGenImageBytes = 5 << 20 // 5 MiB

	// maxCreativeTotalImageBytes caps the SUM of every image in one creative.
	//
	// The per-image cap bounds one file; nothing bounded the set. A Performance Max
	// asset group takes up to 30 images across its slots and a Demand Gen ad up to 25,
	// every one of them read fully into memory, base64-expanded (which adds a third
	// again) and marshalled into a SINGLE assets:mutate body — so the per-image cap
	// alone permits a request of well over a hundred megabytes, held in one process.
	//
	// Positioned here rather than discovered at the mutate for the usual reason: the
	// fetch loop runs before the budget mutate, so a refusal costs the caller an error,
	// while the same request refused by Google lands after the campaign exists and
	// leaves it orphaned. The bound is set generously on purpose — 64 MiB is an average
	// of better than 2 MiB across a full 30-image group — because refusing a create
	// Google would have accepted is the worse failure of the two.
	maxCreativeTotalImageBytes = 64 << 20 // 64 MiB

	// demandGenImageFetchTimeout bounds one image fetch. The cascade may fetch up
	// to 25 images (20 marketing + 5 logos), so this is per-image and the caller's
	// context still bounds the whole operation.
	demandGenImageFetchTimeout = 20 * time.Second

	// creativeImageFetchDeadlineShare is the divisor applied to the caller's REMAINING
	// deadline to bound the whole fetch phase. Half: the fetch is the only pre-create
	// network step, and the mutate cascade that follows it is the half that must not
	// run out of time, because that is the half that creates things.
	creativeImageFetchDeadlineShare = 2

	// maxCreativeImageFetchWall is the ABSOLUTE ceiling on the fetch phase, and it exists
	// because the share above is conditional on the caller having a deadline at all. With
	// no deadline — a background reconcile, a test harness, any call site that passes
	// context.Background() — the share does nothing and the bound reverts to the one the
	// walk implies: 25 images times demandGenImageFetchTimeout, which is over eight
	// minutes of wall time chosen by whoever supplied the URLs. A host that accepts the
	// connection and then trickles bytes just under the per-image timeout spends all of
	// it. The share is still applied when there is a deadline and the SMALLER of the two
	// wins, so this never widens the caller's budget — it only puts a floor under the
	// case where there was no budget to divide.
	maxCreativeImageFetchWall = 90 * time.Second

	// demandGenAspectTolerance is Google's own "+-1%" on every documented ratio.
	demandGenAspectTolerance = 0.01
)

// DemandGenCreative is the image and text creative for the Demand Gen ad this
// client now creates. It is DEMAND GEN ONLY — refused at preflight on Search,
// which takes its creative as responsive search ad copy plus campaign-level
// extension assets instead (see ad_copy.go and assets.go).
//
// Images are given as URLs, matching every sibling platform's config shape, and
// are downloaded by this service rather than handed to Google — see the file
// comment for why that asymmetry is forced rather than chosen.
type DemandGenCreative struct {
	// The four marketing image slots, each with its own required aspect ratio and
	// minimum size. At least one of MarketingImages or SquareMarketingImages must
	// be present — Google requires each "if the other is not present" — and the
	// four together are capped at maxDemandGenMarketingImages.
	MarketingImages       []string
	SquareMarketingImages []string
	PortraitImages        []string
	TallPortraitImages    []string
	// LogoImages is required: at least one, at most five, square.
	LogoImages []string
	// Headlines and Descriptions are 1-5 each, bounded by display WEIGHT rather
	// than rune count, exactly as the RSA copy in ad_copy.go is.
	Headlines    []string
	Descriptions []string
	// BusinessName is the advertiser/brand name. Required by Google.
	BusinessName string
	// CallToActionText is optional; Google supplies its own default when absent.
	CallToActionText string
}

// empty reports whether the caller asked for no Demand Gen creative at all. A
// campaign with no creative is still created — the pre-existing behaviour, with
// its "upload images in the Google Ads UI" closing step — rather than refused,
// because every Demand Gen campaign created before this feature existed was that
// shape and refusing it now would break adoption of rows already in the database.
func (d DemandGenCreative) empty() bool {
	return len(d.MarketingImages) == 0 &&
		len(d.SquareMarketingImages) == 0 &&
		len(d.PortraitImages) == 0 &&
		len(d.TallPortraitImages) == 0 &&
		len(d.LogoImages) == 0 &&
		len(d.Headlines) == 0 &&
		len(d.Descriptions) == 0 &&
		strings.TrimSpace(d.BusinessName) == "" &&
		strings.TrimSpace(d.CallToActionText) == ""
}

// imageSlot describes one of the five image arrays: where it goes on the ad, what
// shape Google demands of it, and what to call it in an error.
//
// The ratio is held as two integers rather than a float so the documented value
// is the value in the code — 1.91:1 written as 191/100 cannot drift the way a
// hand-rounded 1.91 can, and the comparison below divides once at the point of
// use.
type imageSlot struct {
	label   string
	jsonKey string
	ratioW  int
	ratioH  int
	minW    int
	minH    int
}

// demandGenImageSlots is every image slot, in the order the ad payload lists them.
// Minimums and ratios are Google's own, from DemandGenMultiAssetAdInfo in
// google/ads/googleads/v23/common/ad_type_infos.proto.
// Each label NAMES ITS CHANNEL. The slot validators below are shared with Performance
// Max, whose slots carry different ratios and minimums under three of the same names, so
// an error reading only "marketing image" would not tell an operator which creative they
// got wrong. Carrying the channel in the label rather than threading it through every
// validator signature keeps one definition of each rule and renders exactly the message
// this channel has always produced.
var demandGenImageSlots = []imageSlot{
	{label: "Demand Gen marketing image", jsonKey: "marketingImages", ratioW: 191, ratioH: 100, minW: 600, minH: 314},
	{label: "Demand Gen square marketing image", jsonKey: "squareMarketingImages", ratioW: 1, ratioH: 1, minW: 300, minH: 300},
	{label: "Demand Gen portrait marketing image", jsonKey: "portraitMarketingImages", ratioW: 4, ratioH: 5, minW: 480, minH: 600},
	{label: "Demand Gen tall portrait marketing image", jsonKey: "tallPortraitMarketingImages", ratioW: 9, ratioH: 16, minW: 600, minH: 1067},
	{label: "Demand Gen logo image", jsonKey: "logoImages", ratioW: 1, ratioH: 1, minW: 128, minH: 128},
}

// logoSlotIndex is the position of the logo slot in demandGenImageSlots. Logos are
// the one slot with a MINIMUM count and are excluded from the combined marketing
// cap, so both rules need to name it without matching on its label.
const logoSlotIndex = 4

// demandGenCreativePlan is the validated creative: the per-slot image URLs in the
// order they will be fetched and attached, and the text already trimmed.
//
// urls is POSITIONALLY parallel to demandGenImageSlots — index i of urls holds the
// URLs for slot i — for the same reason assetPlan pairs its two slices: the fetch
// needs the slot's shape rules and the ad payload needs the slot's json key, and
// neither is recoverable from a bare URL.
type demandGenCreativePlan struct {
	urls             [][]string
	headlines        []string
	descriptions     []string
	businessName     string
	callToActionText string
	// present is false when the caller asked for no creative, which leaves the
	// pre-existing "no ad" behaviour intact.
	present bool
}

func (p demandGenCreativePlan) imageCount() int {
	n := 0
	for _, s := range p.urls {
		n += len(s)
	}
	return n
}

// validateDemandGenCreative resolves the creative input WITHOUT sending or
// fetching anything. Called from preflightCampaignKind alongside every other
// validator, so the adoption path refuses what the create path refuses.
//
// What it cannot check is whether a URL actually serves a decodable image of the
// right shape. That is fetchDemandGenImages' job and it still runs before the
// budget mutate — see the file comment.
func validateDemandGenCreative(kind string, in CampaignInput) (demandGenCreativePlan, error) {
	d := in.DemandGenCreative
	if d.empty() {
		return demandGenCreativePlan{}, nil
	}
	// Refused as a channel capability, exactly as validateAssetPlan refuses
	// extensions on Demand Gen: a Search campaign takes its creative as RSA copy
	// and campaign-level extension assets, and DemandGenMultiAssetAdInfo is not a
	// shape a Search ad group accepts.
	if kind != campaignKindDemandGen {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen creative (marketing images, logos, business name) is supported on Demand Gen campaigns only, not on %s", kind)
	}

	plan := demandGenCreativePlan{present: true, urls: make([][]string, len(demandGenImageSlots))}

	raw := [][]string{d.MarketingImages, d.SquareMarketingImages, d.PortraitImages, d.TallPortraitImages, d.LogoImages}
	marketing := 0
	for i, slot := range demandGenImageSlots {
		urls, err := validateImageURLs(slot, raw[i])
		if err != nil {
			return demandGenCreativePlan{}, err
		}
		plan.urls[i] = urls
		if i != logoSlotIndex {
			marketing += len(urls)
		}
	}

	// Google states the marketing ceiling as a combined total across the four
	// arrays, so it is checked once across them rather than four times per array —
	// four arrays of 19 is 76 images and satisfies every per-array reading.
	if marketing > maxDemandGenMarketingImages {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen ad accepts at most %d marketing images across all four shapes, got %d", maxDemandGenMarketingImages, marketing)
	}
	// At least one of the two REQUIRED shapes. Google's wording is reciprocal —
	// each is "required if the other is not present" — so neither alone is
	// mandatory and the pair is.
	if len(plan.urls[0]) == 0 && len(plan.urls[1]) == 0 {
		return demandGenCreativePlan{}, errors.New("google-ads Demand Gen ad needs at least one marketing image or one square marketing image (Google requires each when the other is absent)")
	}
	if n := len(plan.urls[logoSlotIndex]); n < minDemandGenLogos {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen ad needs at least %d logo image, got %d", minDemandGenLogos, n)
	} else if n > maxDemandGenLogos {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen ad accepts at most %d logo images, got %d", maxDemandGenLogos, n)
	}

	headlines, err := validateCreativeText("Demand Gen ad", "headline", d.Headlines, minDemandGenHeadlines, maxDemandGenHeadlines, maxHeadlineWeight)
	if err != nil {
		return demandGenCreativePlan{}, err
	}
	plan.headlines = headlines

	descriptions, err := validateCreativeText("Demand Gen ad", "description", d.Descriptions, minDemandGenDescriptions, maxDemandGenDescriptions, maxDescriptionWeight)
	if err != nil {
		return demandGenCreativePlan{}, err
	}
	plan.descriptions = descriptions

	name := strings.TrimSpace(d.BusinessName)
	if name == "" {
		return demandGenCreativePlan{}, errors.New("google-ads Demand Gen ad requires a business name (Google marks the field required)")
	}
	if w := textWeight(name); w > maxDemandGenBusinessNameWeight {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen business name %q has a display width of %d, exceeding the %d limit", capForError(name), w, maxDemandGenBusinessNameWeight)
	}
	plan.businessName = name

	cta := strings.TrimSpace(d.CallToActionText)
	if n := utf8.RuneCountInString(cta); n > maxDemandGenCallToActionRunes {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen call to action %q is %d characters, exceeding the %d limit", capForError(cta), n, maxDemandGenCallToActionRunes)
	}
	plan.callToActionText = cta

	return plan, nil
}

// validateCreativeText checks one text list against its count bounds and display
// weight, and de-duplicates case-insensitively.
//
// It REFUSES over-long text rather than truncating it, which is the opposite of
// what boundedUniqueCopy does in ad_copy.go — deliberately, and for the reason
// assets.go already states: that path may truncate because it GENERATES its own
// copy and pads from defaults, so a cut line is a cut line of its own making.
// Demand Gen copy is written by a human for a reason, and silently shipping a
// headline cut mid-word is worse than refusing it while nothing has been paid for.
func validateCreativeText(channel, label string, in []string, min, max, maxWeight int) ([]string, error) {
	// The count bound is checked BEFORE the loop as well as after it. Checking it only
	// after meant a caller could send a million strings and have every one of them
	// trimmed, display-width-measured and hashed into `seen` — per-element work, and a
	// map sized from the caller's own length — before being told the list was too long
	// by two. The post-loop check stays because it is the one that reports the SURVIVING
	// count, which de-duplication could in principle lower; this one just stops the walk
	// from being the expensive part of a refusal that was certain from the length alone.
	if len(in) > max {
		return nil, fmt.Errorf("google-ads %s accepts at most %d %ss, got %d", channel, max, label, len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, rawText := range in {
		text := strings.TrimSpace(rawText)
		if text == "" {
			return nil, fmt.Errorf("google-ads %s %s %d is empty", channel, label, i)
		}
		if w := textWeight(text); w > maxWeight {
			return nil, fmt.Errorf("google-ads %s %s %q has a display width of %d, exceeding the %d limit", channel, label, capForError(text), w, maxWeight)
		}
		// Google refuses a duplicate asset within one ad, and a caller who wrote
		// the same headline twice meant one — the same judgement validateCallouts
		// makes. Refused rather than collapsed because dropping one silently
		// changes how many assets the ad has, and the count carries a minimum.
		key := strings.ToLower(text)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads %s %s %q is listed more than once", channel, label, capForError(text))
		}
		seen[key] = struct{}{}
		out = append(out, text)
	}
	if len(out) < min {
		return nil, fmt.Errorf("google-ads %s needs at least %d %s, got %d", channel, min, label, len(out))
	}
	if len(out) > max {
		return nil, fmt.Errorf("google-ads %s accepts at most %d %ss, got %d", channel, max, label, len(out))
	}
	return out, nil
}

// textWeight is the display width Google measures ad text in — the same
// double-width accounting ad_copy.go applies to RSA headlines and descriptions.
//
// Demand Gen states its limits as "maximum display width", the identical phrase,
// so the helper is shared. The COUNT limits are not: see the constants above.
func textWeight(s string) int {
	w := 0
	for _, r := range s {
		w += googleAdsCharWeight(r)
	}
	return w
}

// validateImageURLs checks the SHAPE of each URL in one slot. Everything about the
// image itself — that it exists, decodes, and has the right dimensions — is
// checked in fetchDemandGenImages, because none of it is knowable from the string.
func validateImageURLs(slot imageSlot, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, rawURL := range in {
		raw := strings.TrimSpace(rawURL)
		if raw == "" {
			return nil, fmt.Errorf("google-ads %s %d has no URL", slot.label, i)
		}
		u, err := url.Parse(raw)
		if err != nil {
			// The unparseable value is NOT echoed. url.Parse failing tells us nothing
			// about what the string holds, so echoing it to help the caller is exactly
			// the case where it could be anything — including a signed URL whose query
			// is the credential. redactURLForError fails closed to a placeholder.
			return nil, fmt.Errorf("google-ads %s %d has an unparseable URL %q: %w", slot.label, i, redactURLForError(raw), redactedCause{err})
		}
		// HTTPS only, and not as a style preference: the bytes are fetched by this
		// service from a caller-supplied address, and a plaintext fetch is one an
		// on-path attacker can replace with an image of their choosing that then
		// becomes a real ad creative under the Foundation's account.
		if !strings.EqualFold(u.Scheme, "https") {
			// The caller's scheme is NOT echoed. At this point the code has established
			// only that it is not https — nothing about what it actually is — and a bare
			// token parses as a scheme all on its own ("sk-secret:foo" has scheme
			// "sk-secret"). Naming the required scheme from our own constant loses nothing:
			// the caller already knows what they sent.
			return nil, fmt.Errorf("google-ads %s %d must be an https URL", slot.label, i)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("google-ads %s %d has no host: %q", slot.label, i, redactURLForError(raw))
		}
		// The same image uploaded twice is two assets on one ad, which Google
		// refuses, and the duplicate consumes one of the slot's few places.
		if _, dup := seen[raw]; dup {
			return nil, fmt.Errorf("google-ads %s %q is listed more than once", slot.label, redactURLForError(raw))
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fetching the bytes
// ---------------------------------------------------------------------------

// fetchedImage is one downloaded, decoded and shape-checked image, ready to be
// sent as an asset. slot is its index into demandGenImageSlots.
type fetchedImage struct {
	slot int
	url  string
	data []byte
}

// fetchDemandGenImages downloads every image in the plan and checks each one's
// format and geometry locally.
//
// It runs in the cascade BEFORE the budget mutate. That placement is the whole
// point: a 404, a redirect, an over-sized file or a 601x314 image that misses the
// 1.91:1 ratio all fail while nothing has been created, which is the same
// guarantee the pure validators give and the reason they exist.
//
// Images are fetched SEQUENTIALLY. A Demand Gen ad tops out at 25 images, the
// per-image timeout bounds each one, and fetching them in parallel would turn one
// caller's creative into a 25-way fan-out against a host this service does not
// control.
func (c *Client) fetchDemandGenImages(ctx context.Context, plan demandGenCreativePlan) ([]fetchedImage, error) {
	if !plan.present {
		return nil, nil
	}
	return c.fetchSlotImages(ctx, demandGenImageSlots, plan.urls)
}

// fetchSlotImages is the channel-independent half of the fetch: given a slot table and a
// positionally parallel set of URL lists, it downloads and shape-checks every image.
//
// It is shared with Performance Max rather than copied, because every property that makes
// the fetch safe — the hardened client, the size cap, the decode-don't-sniff rule, the
// sequential walk — is a property of fetching a caller-supplied URL and not of a channel.
// A second copy would be a second place for one of those to lapse. What IS channel
// specific is the slot table, which carries the ratios, the minimums and the labels, and
// that is the parameter.
// creativeFetchBudget is how long the fetch phase may run: the SMALLER of the caller's
// share and the absolute ceiling.
//
// It is a function rather than four lines inline because the property worth testing is
// arithmetic, and the alternative — proving the ceiling by letting a test actually wait
// out a slow host — is a ninety-second test nobody will keep. Starting from the ceiling
// rather than from the caller's deadline is what makes the no-deadline case bounded: there
// is always a number, and a caller who has a deadline only ever narrows it.
//
// A non-positive result means the caller's deadline has already passed; the caller leaves
// the context alone in that case, so the expiry is reported as the CALLER's rather than as
// the fetch phase overrunning its share.
func creativeFetchBudget(ctx context.Context) time.Duration {
	budget := maxCreativeImageFetchWall
	if deadline, ok := ctx.Deadline(); ok {
		if share := time.Until(deadline) / creativeImageFetchDeadlineShare; share < budget {
			budget = share
		}
	}
	return budget
}

func (c *Client) fetchSlotImages(ctx context.Context, slots []imageSlot, urls [][]string) ([]fetchedImage, error) {
	total := 0
	for _, u := range urls {
		total += len(u)
	}
	out := make([]fetchedImage, 0, total)

	// The fetch phase gets a SHARE of the caller's remaining deadline, not all of it.
	// demandGenImageFetchTimeout bounds one image and the walk is sequential, so a full
	// 30-image group against slow hosts is ten minutes of worst case — far past the
	// dispatcher's per-provider budget. Without this the deadline would expire somewhere
	// in the MUTATE cascade, which is the orphaned-campaign state the whole preflight
	// design exists to avoid; with it, exhaustion lands here, before anything is created,
	// where it costs the caller an error and nothing else.
	//
	// Derived from what the caller actually has left rather than set as a constant: a
	// fixed ceiling would refuse a creative that comfortably fit the caller's budget,
	// and refusing a create Google would have accepted is the worse failure. A caller
	// with no deadline at all keeps the behaviour it had.
	fetchCtx := ctx
	if budget := creativeFetchBudget(ctx); budget > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	// Checked as a RUNNING total, not after the loop: the point is to stop reading
	// before the whole set is resident, so an oversized creative never allocates the
	// hundreds of megabytes the check exists to prevent.
	fetched := 0
	for slotIdx, slotURLs := range urls {
		slot := slots[slotIdx]
		for _, u := range slotURLs {
			data, err := c.fetchOneImage(fetchCtx, slot, u)
			if err != nil {
				// Said plainly, because "context deadline exceeded" on its own would
				// read as the caller's whole budget being gone when in fact the
				// mutate half of it is still intact and untouched.
				if fetchCtx.Err() != nil && ctx.Err() == nil {
					return nil, fmt.Errorf("google-ads creative image fetching used its share of the deadline after %d of %d image(s) — nothing was created; use faster image hosts or fewer images", len(out), total)
				}
				return nil, err
			}
			fetched += len(data)
			if fetched > maxCreativeTotalImageBytes {
				return nil, fmt.Errorf("google-ads creative images total more than the %d byte limit across all slots", maxCreativeTotalImageBytes)
			}
			out = append(out, fetchedImage{slot: slotIdx, url: u, data: data})
		}
	}
	return out, nil
}

// fetchOneImage downloads a single image and validates it.
//
// The client is built per call rather than shared on Client because it carries a
// redirect policy and a dialer that exist only for this use; reusing the API
// client here would point a hardened fetch at Google's own transport and, worse,
// could carry the account's auth headers to a caller-supplied host.
func (c *Client) fetchOneImage(ctx context.Context, slot imageSlot, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, demandGenImageFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("google-ads %s %q could not be requested: %w", slot.label, redactURLForError(rawURL), redactedCause{err})
	}
	// No Authorization header, no developer token, nothing from the Google client:
	// this request goes to an address the caller chose.
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif")

	resp, err := c.imageFetchClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("google-ads %s %q could not be downloaded: %w", slot.label, redactURLForError(rawURL), redactedCause{err})
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google-ads %s %q returned HTTP %d", slot.label, redactURLForError(rawURL), resp.StatusCode)
	}

	// LimitReader with one byte of headroom: reading exactly the cap cannot
	// distinguish "exactly at the limit" from "truncated here", and silently
	// uploading a truncated image is the failure this bound exists to prevent.
	// Content-Length is deliberately not trusted — it is a claim by the same host
	// that is serving the bytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDemandGenImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("google-ads %s %q could not be read: %w", slot.label, redactURLForError(rawURL), redactedCause{err})
	}
	if len(data) > maxDemandGenImageBytes {
		return nil, fmt.Errorf("google-ads %s %q is larger than the %d byte limit", slot.label, redactURLForError(rawURL), maxDemandGenImageBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("google-ads %s %q returned an empty body", slot.label, redactURLForError(rawURL))
	}

	// Decoded rather than sniffed by Content-Type: the header is the serving host's
	// claim, image.DecodeConfig is the bytes themselves, and only the registered
	// decoders — GIF, JPEG, PNG — are the formats Google accepts here.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("google-ads %s %q is not a usable GIF, JPEG or PNG: %w", slot.label, redactURLForError(rawURL), err)
	}
	if err := checkImageGeometry(slot, rawURL, format, cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	return data, nil
}

// checkImageGeometry applies the slot's minimum size and aspect ratio.
//
// Both are Google's own rules, so enforcing them refuses nothing Google would have
// accepted — it only moves the refusal from the ad mutate, which runs after the
// budget, campaign and ad group are committed, to before any of them exist.
func checkImageGeometry(slot imageSlot, rawURL, format string, w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("google-ads %s %q reports a %dx%d image, which is not usable", slot.label, redactURLForError(rawURL), w, h)
	}
	if w < slot.minW || h < slot.minH {
		return fmt.Errorf("google-ads %s %q is %dx%d (%s), below the %dx%d minimum", slot.label, redactURLForError(rawURL), w, h, format, slot.minW, slot.minH)
	}
	// Compared as a ratio of float64s against Google's documented +-1%. The target
	// is built from the two integers so the documented value stays the value in the
	// code; the division happens once, here.
	want := float64(slot.ratioW) / float64(slot.ratioH)
	got := float64(w) / float64(h)
	if diff := (got - want) / want; diff > demandGenAspectTolerance || diff < -demandGenAspectTolerance {
		return fmt.Errorf("google-ads %s %q is %dx%d, an aspect ratio of %.4f — Google requires %d:%d (%.4f) within %.0f%%", slot.label, redactURLForError(rawURL), w, h, got, slot.ratioW, slot.ratioH, want, demandGenAspectTolerance*100)
	}
	return nil
}

// imageFetchClient builds the hardened client used for creative downloads.
//
// Three things it does that the default client does not, each closing a hole that
// exists because the URL comes from a caller rather than from this code:
//
//   - REDIRECTS ARE REFUSED outright. Following one would re-open every check
//     below against an address the original URL never named, and a creative image
//     is not a resource that needs redirect support to be fetchable.
//   - THE DESTINATION IP IS CHECKED, and the connection is then made to the
//     checked IP rather than re-resolving the name. Re-resolving is the DNS
//     rebinding window: a name that answers with a public address for the check
//     and a loopback address for the dial.
//   - PRIVATE, LOOPBACK, LINK-LOCAL, CGNAT, ULA, MULTICAST AND UNSPECIFIED
//     ADDRESSES ARE REFUSED. This service runs inside a cluster with reachable
//     internal endpoints and a metadata service; without this a caller could use
//     the creative fetch as a read primitive against them.
func (c *Client) imageFetchClient() *http.Client {
	guard := c.imageDialGuard
	if guard == nil {
		// A Client built as a zero value rather than through NewClient still gets the
		// real policy. Defaulting to "allow" here would make the one path that must
		// never silently open up do exactly that.
		guard = checkPublicIP
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("malformed address %q: %w", addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("could not resolve %q: %w", host, err)
			}
			var lastErr error
			for _, ip := range ips {
				if err := guard(ip.IP); err != nil {
					lastErr = err
					continue
				}
				// Dial the ADDRESS, not the name: this is what closes the
				// rebinding window the check above would otherwise leave open.
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err != nil {
					lastErr = err
					continue
				}
				return conn, nil
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no addresses for %q", host)
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
		TLSClientConfig:       c.imageTLSConfig,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   demandGenImageFetchTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			// redactURLForError, not url.URL.Redacted(): Redacted() masks ONLY a password in
			// userinfo and keeps the query verbatim, so a redirect to a signed CDN asset put
			// its signature straight into this message — which net/http wraps into the
			// *url.Error that becomes a persisted Steps entry. The caller's own URL is
			// redacted at every one of the sites that render it; the redirect TARGET is the
			// same class of secret, reached by one hop, and reads as already-redacted
			// precisely because Redacted() is in its name.
			return fmt.Errorf("refusing to follow a redirect to %s (creative image URLs must point directly at the image)", redactURLForError(req.URL.String()))
		},
	}
}

// checkPublicIP refuses any address that is not routable on the public internet.
//
// It is eventurl's judgement, not a second one. This used to be a local enumeration of
// the known-bad classes, and the enumeration was WRONG in a way no list of predicates
// catches: none of them decode an RFC 6052 address, so 64:ff9b::a9fe:a9fe — the
// well-known NAT64 prefix naming 169.254.169.254, the instance metadata service — matched
// no predicate and was allowed. That is the exact failure eventurl's package doc calls out
// ("a second fetcher that builds its own http.Client is not a smaller version of this one
// — it is an unguarded one"), reached here through a second GUARD rather than a second
// client. Delegating means a range added to forbiddenNets protects this path too.
//
// It judges the well-known /96 alone. A deployment that configures operator-specific
// translation prefixes must pass them in via WithNAT64Prefixes, exactly as the HubSpot
// client requires — see that option.
var checkPublicIP = eventurl.NewAddressGuard()

// base64Image renders one image for the wire. Google's ImageAsset.data is a proto
// bytes field, which the REST surface carries as standard base64.
func base64Image(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// ---------------------------------------------------------------------------
// Creating the assets and the ad
// ---------------------------------------------------------------------------

// imageAssetCreate is ImageAsset. Only `data` is sent: `mime_type` and `full_size`
// are derived by Google from the bytes, and sending a mime type we inferred would
// let a wrong guess override what the bytes actually are.
type imageAssetCreate struct {
	Data string `json:"data"`
}

// demandGenAssetCreate is an Asset create carrying an image.
//
// Deliberately NOT assets.go's assetCreate with an image arm bolted on. That struct
// is the Search extension oneof — sitelink, callout, structured snippet — and its
// FinalURLs field means something there (the extension's destination) that it does
// not mean here. One struct spanning both would make every reader check which half
// applies.
type demandGenAssetCreate struct {
	Name       string           `json:"name,omitempty"`
	ImageAsset imageAssetCreate `json:"imageAsset"`
}

// adImageAsset is AdImageAsset: a reference to an already-created image asset by
// its resource name.
type adImageAsset struct {
	Asset string `json:"asset"`
}

// demandGenMultiAssetAdInfo is the ad-type payload, mirroring
// DemandGenMultiAssetAdInfo. Every image array is omitempty because Google
// distinguishes an absent optional array from an empty one, and the two required
// arrays are guaranteed non-empty by validateDemandGenCreative.
type demandGenMultiAssetAdInfo struct {
	MarketingImages             []adImageAsset `json:"marketingImages,omitempty"`
	SquareMarketingImages       []adImageAsset `json:"squareMarketingImages,omitempty"`
	PortraitMarketingImages     []adImageAsset `json:"portraitMarketingImages,omitempty"`
	TallPortraitMarketingImages []adImageAsset `json:"tallPortraitMarketingImages,omitempty"`
	LogoImages                  []adImageAsset `json:"logoImages,omitempty"`
	Headlines                   []adTextAsset  `json:"headlines"`
	Descriptions                []adTextAsset  `json:"descriptions"`
	BusinessName                string         `json:"businessName"`
	CallToActionText            string         `json:"callToActionText,omitempty"`
}

// demandGenAdCreate is the "ad" object nested in an adGroupAd create. It is
// adCreate's Demand Gen sibling, separate for the same reason
// demandGenAssetCreate is: adCreate's ResponsiveSearchAd arm has no meaning here.
type demandGenAdCreate struct {
	FinalUrls             []string                   `json:"finalUrls"`
	DemandGenMultiAssetAd *demandGenMultiAssetAdInfo `json:"demandGenMultiAssetAdInfo,omitempty"`
}

// demandGenAdGroupAdCreate is the create payload for adGroupAds:mutate on this
// channel.
type demandGenAdGroupAdCreate struct {
	AdGroup string            `json:"adGroup"`
	Status  string            `json:"status"`
	Ad      demandGenAdCreate `json:"ad"`
}

// createDemandGenAd uploads the fetched images as account-level assets and then
// creates the ad that references them.
//
// Two mutates, the same shape createCampaignAssets uses for Search extensions, and
// for the same reason: Google has no single call that creates an asset and attaches
// it. The second mutate uses THE RESOURCE NAMES GOOGLE RETURNED, never names rebuilt
// from the parsed ids — a rebuilt name is this client asserting what Google said
// rather than repeating it.
//
// The ad is created PAUSED, matching the Search path. The campaign is paused too,
// and the existing status cascade (UpdateAdGroupsAndAdsStatus, driven by the
// scalar AdGroupID/AdID pair this sets) flips the ad along with the campaign when
// the operator launches it — so a paused ad here is not an ad that stays dark.
//
// Returns the created asset ids and the ad id. Both are reported even on failure of
// the later stage, because assets created without an ad to reference them are
// account-level litter the operator can find and remove.
func (c *Client) createDemandGenAd(ctx context.Context, adGroupResource, adGroupID, finalURL string, plan demandGenCreativePlan, images []fetchedImage) (assetIDs []string, adID string, err error) {
	// Reached only when the preflight said a creative is PRESENT, so an empty image set
	// here is an internal inconsistency rather than a caller's choice — and returning
	// (nil, "", nil) for it was reporting that inconsistency as success. The caller then
	// appended `"Demand Gen ad created: %s"` with an empty id, writing an audit step that
	// says an ad was created and names no ad. A contradictory audit trail is worse than a
	// refusal, because the refusal is the only one of the two anybody acts on.
	//
	// (nil, "", err) is contract-correct: nothing has been sent at this point, so the
	// orchestrator may release its claim. The campaign above it still exists and is still
	// reported by the cascade's own partial result.
	if len(images) == 0 {
		return nil, "", fmt.Errorf("google-ads: demand gen creative is present but no images were fetched for it; refusing to create an ad with no assets")
	}

	assetOps := make([]mutateOperation, 0, len(images))
	for _, img := range images {
		assetOps = append(assetOps, mutateOperation{Create: demandGenAssetCreate{
			ImageAsset: imageAssetCreate{Data: base64Image(img.data)},
		}})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		// A 5xx or a timeout on a mutating POST is NOT a failure, it is an unknown
		// outcome: the assets may well have been created. The malformed-response arm
		// below and the adGroupAds:mutate that follows both already say so, as does the
		// Performance Max sibling; this was the one assets:mutate on a create path that
		// asserted "failed" over an outcome nobody knows, and an operator who believed
		// it would retry into a second set of account-level image assets.
		if createOutcomeAmbiguous(err) {
			return nil, "", fmt.Errorf("google-ads demand gen image asset creation UNCONFIRMED (%d image(s) may exist; ad group %s created — verify in Google Ads before retrying): %w", len(assetOps), adGroupID, err)
		}
		return nil, "", fmt.Errorf("google-ads demand gen image asset creation failed (%d image(s); ad group %s created): %w", len(assetOps), adGroupID, err)
	}
	var assetResults mutateResponse
	// EXACT equality, like every other create-path mutate in this package: one
	// operation per image means a short response leaves images unaccounted for, and
	// an extra result is a response that does not describe what was sent. Both are
	// unconfirmed rather than failed — the assets may exist.
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil {
		return nil, "", fmt.Errorf("google-ads demand gen image asset creation UNCONFIRMED (ad group %s created; 2xx with a malformed mutate response for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetOps))
	}
	if len(assetResults.Results) != len(assetOps) {
		// The body PARSED, so whatever ids it did carry are real — and they are the only
		// handle an operator has on account-level assets that may already exist. They go
		// back WITH the error rather than being dropped alongside it, the same way the
		// malformed-resource-name arm below returns what it had got to.
		return c.parsedAssetIDs(assetResults), "", fmt.Errorf("google-ads demand gen image asset creation UNCONFIRMED (ad group %s created; 2xx returned %d result(s) for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetResults.Results), len(assetOps))
	}

	// Positional: result i is the asset for images[i], which carries its slot.
	perSlot := make([][]adImageAsset, len(demandGenImageSlots))
	assetIDs = make([]string, 0, len(assetOps))
	for i, r := range assetResults.Results {
		resource := r.ResourceName
		// assetID checks the kind, the account AND the numeric trailing id, and
		// returns "" for anything else. Without it a wrong-account assets resource
		// would be referenced by the ad and persisted as this campaign's.
		id := c.assetID(resource)
		if id == "" {
			return assetIDs, "", fmt.Errorf("google-ads demand gen image asset creation UNCONFIRMED (ad group %s created; malformed asset resource name %q at index %d — assets may exist — verify in Google Ads before retrying)", adGroupID, resource, i)
		}
		assetIDs = append(assetIDs, id)
		perSlot[images[i].slot] = append(perSlot[images[i].slot], adImageAsset{Asset: resource})
	}

	ad := &demandGenMultiAssetAdInfo{
		MarketingImages:             perSlot[0],
		SquareMarketingImages:       perSlot[1],
		PortraitMarketingImages:     perSlot[2],
		TallPortraitMarketingImages: perSlot[3],
		LogoImages:                  perSlot[logoSlotIndex],
		Headlines:                   textAssets(plan.headlines),
		Descriptions:                textAssets(plan.descriptions),
		BusinessName:                plan.businessName,
		CallToActionText:            plan.callToActionText,
	}
	adReq := mutateRequest{Operations: []mutateOperation{{Create: demandGenAdGroupAdCreate{
		AdGroup: adGroupResource,
		Status:  "PAUSED",
		Ad:      demandGenAdCreate{FinalUrls: []string{finalURL}, DemandGenMultiAssetAd: ad},
	}}}}
	adResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupAds:mutate"), adReq, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s and %d image asset(s) created; the ad may exist — verify in Google Ads before retrying): %w", adGroupID, len(assetIDs), err)
		}
		return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation failed (ad group %s and %d image asset(s) created): %w", adGroupID, len(assetIDs), err)
	}
	var adResults mutateResponse
	if uErr := json.Unmarshal(adResp, &adResults); uErr != nil || len(adResults.Results) != 1 {
		return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; 2xx with a malformed/short mutate response — the ad may exist — verify in Google Ads before retrying)", adGroupID)
	}
	adResource := adResults.Results[0].ResourceName
	// requireNumericID=false: the trailing segment is the composite
	// "{adGroupId}~{adId}", split and checked by adGroupAdID just below — exactly as
	// the Search ad path does it.
	if verr := c.validateResourceKind("adGroupAds", adResource, false); verr != nil {
		return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; %w — verify in Google Ads before retrying)", adGroupID, verr)
	}
	returnedAdGroupID, adID := adGroupAdID(adResource)
	if adID == "" || returnedAdGroupID == "" {
		return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; malformed adGroupAd resource name %q — verify in Google Ads before retrying)", adGroupID, adResource)
	}
	// The resource name must describe the ad group this ad was created under. A
	// mismatch means the response is not about this call, so the ad id is not
	// trustworthy enough to persist and later toggle.
	if returnedAdGroupID != adGroupID {
		return assetIDs, "", fmt.Errorf("google-ads demand gen ad creation UNCONFIRMED (ad group %s created; adGroupAd resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, adResource, returnedAdGroupID)
	}
	return assetIDs, adID, nil
}

// demandGenCreativeStep summarises what the ad was built from, for the operator-
// facing step list.
func demandGenCreativeStep(plan demandGenCreativePlan) string {
	parts := make([]string, 0, 2)
	marketing := plan.imageCount() - len(plan.urls[logoSlotIndex])
	parts = append(parts, fmt.Sprintf("%d marketing image(s), %d logo(s)", marketing, len(plan.urls[logoSlotIndex])))
	parts = append(parts, fmt.Sprintf("%d headline(s), %d description(s)", len(plan.headlines), len(plan.descriptions)))
	return strings.Join(parts, ", ")
}
