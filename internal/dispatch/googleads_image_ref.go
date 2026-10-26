// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// Google creative images by stored asset id (LFXV2-2665).
//
// Every other platform in this package names its creative image with an asset id and lets
// the service load the bytes; Google alone took a URL, because Google's ImageAsset wants raw
// bytes and the service was fetching them from wherever the caller hosted them. That left the
// one platform that spends the most money as the only one whose creative could not come from
// the asset library the UI uploads into.
//
// # Why an entry is a REF and not a second list
//
// The obvious shape — keep `marketingImages []string` for URLs and add `marketingImageAssetIds
// []string` beside it — breaks the dangling-reference guard, silently. ReplaceCampaign refuses a
// config_snapshot that newly names an asset which no longer exists, and
// postgres.collectSnapshotAssetIDs finds those references BY KEY: a value counts only when it is
// a STRING directly under a key equal, ignoring case, to `imageAssetId`. A flat list of ids under
// any other key carries no key on its elements at all, so every id in it would be skipped and the
// guard would read as passing — the exact failure TestPlatformConfigsNameTheirAssetUnderOneKey
// exists to make loud, and Google is the sixth platform that test anticipates.
//
// So an asset-backed entry has to be an OBJECT carrying the id under that key, which is also what
// meta.AdVariant and linkedin.CreativeVariant already do. A slot entry is therefore a ref: either
// a URL or an asset id, never a bare id.
//
// # Why both shapes decode, and why a URL still ENCODES as a bare string
//
// Existing briefs hold `"marketingImages": ["https://..."]`, and CampaignCreateInput.config is
// declared Any, so nothing upstream rewrites them. UnmarshalJSON therefore accepts a bare string
// as a URL ref, which is what every stored config is today.
//
// MarshalJSON is the mirror image and matters for a different reason: googleAdsSnapshotConfig
// RE-MARSHALS the config to persist it. Encoding a URL-only ref as {"url":"..."} would silently
// rewrite the shape of every existing snapshot on its next write, so a URL-only ref encodes back
// to the bare string it arrived as. Only a ref that actually names an asset encodes as an object,
// which is precisely when the snapshot needs the key the guard looks for.

// googleAdsImageRef is one entry in a Google creative image slot: either a URL this service
// fetches, or the id of a creative asset already stored for the brief.
//
// Exactly one of the two is set; a ref carrying both, or neither, is refused at resolve time
// rather than at decode time, so the message can name the slot and position the caller wrote.
type googleAdsImageRef struct {
	URL          string `json:"url,omitempty"`
	ImageAssetID string `json:"imageAssetId,omitempty"`
}

// googleAdsImageRefScheme prefixes an asset-backed entry once it crosses into the platform
// client, which works in flat per-slot string lists.
//
// It is a URI scheme rather than an invented sentinel because that is what the string IS at that
// point: the identity of an image that is not at a URL. It can never collide with a caller's
// entry, since a URL ref is refused unless it parses as http(s) with a host, and it keeps the
// platform-side budget, dedupe and error messages keyed on one string per image exactly as they
// already were.
//
// Taken from the platform package rather than spelled again here, because it is a CONTRACT
// between the two halves — this side writes the entry and the map key, that side matches them —
// and a contract written down twice is a contract that can drift silently.
const googleAdsImageRefScheme = googleads.ResolvedImageRefPrefix

// UnmarshalJSON accepts a bare string (today's URL shape) or an object.
func (r *googleAdsImageRef) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*r = googleAdsImageRef{URL: s}
		return nil
	}
	// A distinct type, so the object arm does not recurse into this method.
	type wire googleAdsImageRef
	var w wire
	if err := json.Unmarshal(trimmed, &w); err != nil {
		return err
	}
	*r = googleAdsImageRef(w)
	return nil
}

// MarshalJSON writes a URL-only ref as the bare string it came in as, and anything else as an
// object — see the file comment for why the asymmetry is the point.
func (r googleAdsImageRef) MarshalJSON() ([]byte, error) {
	if r.ImageAssetID == "" {
		return json.Marshal(r.URL)
	}
	type wire googleAdsImageRef
	return json.Marshal(wire(r))
}

// namesAsset reports whether this entry is asset-backed.
func (r googleAdsImageRef) namesAsset() bool { return strings.TrimSpace(r.ImageAssetID) != "" }

// googleAdsImageRefs is one slot's list of entries.
type googleAdsImageRefs []googleAdsImageRef

// sanitizeSnapshotImageRefs is sanitizeSnapshotURL over a ref list, returning a NEW slice so the
// caller's backing array is never written through. It reduces the URL of every URL-backed entry
// for the reason googleAdsSnapshotConfig gives — config_snapshot is persisted UNENCRYPTED and a
// creative URL can carry a signed query string that is itself the credential for the asset.
//
// An asset-backed entry is left ALONE, and must be: the id is this service's own primary key,
// not a credential, and it is the very value checkNewSnapshotAssetRefs reads back out of the
// snapshot. Reducing it would make the stored reference unresolvable and the guard blind.
func sanitizeSnapshotImageRefs(in googleAdsImageRefs) googleAdsImageRefs {
	if len(in) == 0 {
		return nil
	}
	out := make(googleAdsImageRefs, len(in))
	for i, ref := range in {
		out[i] = ref
		if !ref.namesAsset() {
			out[i].URL = sanitizeSnapshotURL(ref.URL)
		}
	}
	return out
}

// slotEntries renders a ref list into the flat per-slot strings the platform client takes: a URL
// stays a URL, an asset becomes googleAdsImageRefScheme + its id.
//
// The ids are the CANONICAL spellings resolveGoogleCreativeAssets produced, not the caller's, so
// a config naming one asset through two valid uuid spellings renders one string and is fetched,
// budgeted and reported once.
func (in googleAdsImageRefs) slotEntries() []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, ref := range in {
		if ref.namesAsset() {
			out[i] = googleAdsImageRefScheme + strings.TrimSpace(ref.ImageAssetID)
			continue
		}
		out[i] = ref.URL
	}
	return out
}

// googleAdsImageRefsFromURLs builds a ref list from plain URLs. Only tests and callers that
// genuinely have URLs use it; it exists so those sites do not each spell the literal struct.
func googleAdsImageRefsFromURLs(urls []string) googleAdsImageRefs {
	if len(urls) == 0 {
		return nil
	}
	out := make(googleAdsImageRefs, len(urls))
	for i, u := range urls {
		out[i] = googleAdsImageRef{URL: u}
	}
	return out
}

// describeGoogleAdsImageRef renders an entry for a refusal message: the asset id, which is not a
// secret, or the reduced URL, which may be.
func describeGoogleAdsImageRef(ref googleAdsImageRef) string {
	if ref.namesAsset() {
		return fmt.Sprintf("creative asset %s", strings.TrimSpace(ref.ImageAssetID))
	}
	return fmt.Sprintf("image URL %s", sanitizeSnapshotURL(ref.URL))
}
