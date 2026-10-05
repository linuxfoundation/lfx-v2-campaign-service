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

	// demandGenImageFetchTimeout bounds one image fetch. The cascade may fetch up
	// to 25 images (20 marketing + 5 logos), so this is per-image and the caller's
	// context still bounds the whole operation.
	demandGenImageFetchTimeout = 20 * time.Second

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
var demandGenImageSlots = []imageSlot{
	{label: "marketing image", jsonKey: "marketingImages", ratioW: 191, ratioH: 100, minW: 600, minH: 314},
	{label: "square marketing image", jsonKey: "squareMarketingImages", ratioW: 1, ratioH: 1, minW: 300, minH: 300},
	{label: "portrait marketing image", jsonKey: "portraitMarketingImages", ratioW: 4, ratioH: 5, minW: 480, minH: 600},
	{label: "tall portrait marketing image", jsonKey: "tallPortraitMarketingImages", ratioW: 9, ratioH: 16, minW: 600, minH: 1067},
	{label: "logo image", jsonKey: "logoImages", ratioW: 1, ratioH: 1, minW: 128, minH: 128},
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

	headlines, err := validateDemandGenText("headline", d.Headlines, minDemandGenHeadlines, maxDemandGenHeadlines, maxHeadlineWeight)
	if err != nil {
		return demandGenCreativePlan{}, err
	}
	plan.headlines = headlines

	descriptions, err := validateDemandGenText("description", d.Descriptions, minDemandGenDescriptions, maxDemandGenDescriptions, maxDescriptionWeight)
	if err != nil {
		return demandGenCreativePlan{}, err
	}
	plan.descriptions = descriptions

	name := strings.TrimSpace(d.BusinessName)
	if name == "" {
		return demandGenCreativePlan{}, errors.New("google-ads Demand Gen ad requires a business name (Google marks the field required)")
	}
	if w := textWeight(name); w > maxDemandGenBusinessNameWeight {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen business name %q has a display width of %d, exceeding the %d limit", name, w, maxDemandGenBusinessNameWeight)
	}
	plan.businessName = name

	cta := strings.TrimSpace(d.CallToActionText)
	if n := utf8.RuneCountInString(cta); n > maxDemandGenCallToActionRunes {
		return demandGenCreativePlan{}, fmt.Errorf("google-ads Demand Gen call to action %q is %d characters, exceeding the %d limit", cta, n, maxDemandGenCallToActionRunes)
	}
	plan.callToActionText = cta

	return plan, nil
}

// validateDemandGenText checks one text list against its count bounds and display
// weight, and de-duplicates case-insensitively.
//
// It REFUSES over-long text rather than truncating it, which is the opposite of
// what boundedUniqueCopy does in ad_copy.go — deliberately, and for the reason
// assets.go already states: that path may truncate because it GENERATES its own
// copy and pads from defaults, so a cut line is a cut line of its own making.
// Demand Gen copy is written by a human for a reason, and silently shipping a
// headline cut mid-word is worse than refusing it while nothing has been paid for.
func validateDemandGenText(label string, in []string, min, max, maxWeight int) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, rawText := range in {
		text := strings.TrimSpace(rawText)
		if text == "" {
			return nil, fmt.Errorf("google-ads Demand Gen %s %d is empty", label, i)
		}
		if w := textWeight(text); w > maxWeight {
			return nil, fmt.Errorf("google-ads Demand Gen %s %q has a display width of %d, exceeding the %d limit", label, text, w, maxWeight)
		}
		// Google refuses a duplicate asset within one ad, and a caller who wrote
		// the same headline twice meant one — the same judgement validateCallouts
		// makes. Refused rather than collapsed because dropping one silently
		// changes how many assets the ad has, and the count carries a minimum.
		key := strings.ToLower(text)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads Demand Gen %s %q is listed more than once", label, text)
		}
		seen[key] = struct{}{}
		out = append(out, text)
	}
	if len(out) < min {
		return nil, fmt.Errorf("google-ads Demand Gen ad needs at least %d %s, got %d", min, label, len(out))
	}
	if len(out) > max {
		return nil, fmt.Errorf("google-ads Demand Gen ad accepts at most %d %ss, got %d", max, label, len(out))
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
			return nil, fmt.Errorf("google-ads Demand Gen %s %d has no URL", slot.label, i)
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("google-ads Demand Gen %s %d has an unparseable URL %q: %w", slot.label, i, raw, err)
		}
		// HTTPS only, and not as a style preference: the bytes are fetched by this
		// service from a caller-supplied address, and a plaintext fetch is one an
		// on-path attacker can replace with an image of their choosing that then
		// becomes a real ad creative under the Foundation's account.
		if !strings.EqualFold(u.Scheme, "https") {
			return nil, fmt.Errorf("google-ads Demand Gen %s %d must be an https URL, got scheme %q", slot.label, i, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("google-ads Demand Gen %s %d has no host: %q", slot.label, i, raw)
		}
		// The same image uploaded twice is two assets on one ad, which Google
		// refuses, and the duplicate consumes one of the slot's few places.
		if _, dup := seen[raw]; dup {
			return nil, fmt.Errorf("google-ads Demand Gen %s %q is listed more than once", slot.label, raw)
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
	out := make([]fetchedImage, 0, plan.imageCount())
	for slotIdx, urls := range plan.urls {
		slot := demandGenImageSlots[slotIdx]
		for _, u := range urls {
			data, err := c.fetchOneImage(ctx, slot, u)
			if err != nil {
				return nil, err
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
		return nil, fmt.Errorf("google-ads Demand Gen %s %q could not be requested: %w", slot.label, rawURL, err)
	}
	// No Authorization header, no developer token, nothing from the Google client:
	// this request goes to an address the caller chose.
	req.Header.Set("Accept", "image/png, image/jpeg, image/gif")

	resp, err := c.imageFetchClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q could not be downloaded: %w", slot.label, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q returned HTTP %d", slot.label, rawURL, resp.StatusCode)
	}

	// LimitReader with one byte of headroom: reading exactly the cap cannot
	// distinguish "exactly at the limit" from "truncated here", and silently
	// uploading a truncated image is the failure this bound exists to prevent.
	// Content-Length is deliberately not trusted — it is a claim by the same host
	// that is serving the bytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDemandGenImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q could not be read: %w", slot.label, rawURL, err)
	}
	if len(data) > maxDemandGenImageBytes {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q is larger than the %d byte limit", slot.label, rawURL, maxDemandGenImageBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q returned an empty body", slot.label, rawURL)
	}

	// Decoded rather than sniffed by Content-Type: the header is the serving host's
	// claim, image.DecodeConfig is the bytes themselves, and only the registered
	// decoders — GIF, JPEG, PNG — are the formats Google accepts here.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("google-ads Demand Gen %s %q is not a usable GIF, JPEG or PNG: %w", slot.label, rawURL, err)
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
		return fmt.Errorf("google-ads Demand Gen %s %q reports a %dx%d image, which is not usable", slot.label, rawURL, w, h)
	}
	if w < slot.minW || h < slot.minH {
		return fmt.Errorf("google-ads Demand Gen %s %q is %dx%d (%s), below the %dx%d minimum", slot.label, rawURL, w, h, format, slot.minW, slot.minH)
	}
	// Compared as a ratio of float64s against Google's documented +-1%. The target
	// is built from the two integers so the documented value stays the value in the
	// code; the division happens once, here.
	want := float64(slot.ratioW) / float64(slot.ratioH)
	got := float64(w) / float64(h)
	if diff := (got - want) / want; diff > demandGenAspectTolerance || diff < -demandGenAspectTolerance {
		return fmt.Errorf("google-ads Demand Gen %s %q is %dx%d, an aspect ratio of %.4f — Google requires %d:%d (%.4f) within %.0f%%", slot.label, rawURL, w, h, got, slot.ratioW, slot.ratioH, want, demandGenAspectTolerance*100)
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
			return fmt.Errorf("refusing to follow a redirect to %s (creative image URLs must point directly at the image)", req.URL.Redacted())
		},
	}
}

// checkPublicIP refuses any address that is not routable on the public internet.
//
// Written as an allowlist of "is this one of the known-bad classes" rather than
// IsGlobalUnicast alone, because IsGlobalUnicast is true for RFC1918, CGNAT and
// IPv6 ULA space — the ranges that matter most here.
func checkPublicIP(ip net.IP) error {
	if ip == nil {
		return errors.New("refusing an unparseable address")
	}
	// An IPv4-mapped IPv6 address is unwrapped first, so ::ffff:127.0.0.1 is
	// judged as 127.0.0.1 rather than slipping past the IPv4 checks below.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("refusing the unspecified address %s", ip)
	case ip.IsLoopback():
		return fmt.Errorf("refusing the loopback address %s", ip)
	case ip.IsPrivate():
		return fmt.Errorf("refusing the private address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 is also where cloud instance metadata lives.
		return fmt.Errorf("refusing the link-local address %s", ip)
	case ip.IsMulticast():
		return fmt.Errorf("refusing the multicast address %s", ip)
	case ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("refusing the interface-local address %s", ip)
	}
	// 100.64.0.0/10, carrier-grade NAT. net has no predicate for it and
	// IsPrivate does not cover it, but it is as unroutable as RFC1918 and is
	// used for internal addressing in several clusters.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return fmt.Errorf("refusing the carrier-grade NAT address %s", ip)
	}
	return nil
}

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
	if len(images) == 0 {
		return nil, "", nil
	}

	assetOps := make([]mutateOperation, 0, len(images))
	for _, img := range images {
		assetOps = append(assetOps, mutateOperation{Create: demandGenAssetCreate{
			ImageAsset: imageAssetCreate{Data: base64Image(img.data)},
		}})
	}
	assetResp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), mutateRequest{Operations: assetOps}, false)
	if err != nil {
		return nil, "", fmt.Errorf("google-ads demand gen image asset creation failed (%d image(s); ad group %s created): %w", len(assetOps), adGroupID, err)
	}
	var assetResults mutateResponse
	// EXACT equality, like every other create-path mutate in this package: one
	// operation per image means a short response leaves images unaccounted for, and
	// an extra result is a response that does not describe what was sent. Both are
	// unconfirmed rather than failed — the assets may exist.
	if uErr := json.Unmarshal(assetResp, &assetResults); uErr != nil || len(assetResults.Results) != len(assetOps) {
		return nil, "", fmt.Errorf("google-ads demand gen image asset creation UNCONFIRMED (ad group %s created; 2xx with a malformed/short mutate response for %d image(s) — assets may exist — verify in Google Ads before retrying)", adGroupID, len(assetOps))
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
