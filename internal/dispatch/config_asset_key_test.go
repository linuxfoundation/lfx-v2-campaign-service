// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// snapshotAssetIDKey is postgres.snapshotAssetIDKey, repeated here because it is a CONTRACT
// between two packages that cannot import one another's unexported names, not a shared helper.
// See the test below for what keeps the two copies honest.
const snapshotAssetIDKey = "imageassetid"

// TestPlatformConfigsNameTheirAssetUnderOneKey pins the assumption the campaign repo's
// dangling-reference check is built on: every platform config that can reference a stored
// creative asset marshals that reference under a key equal, ignoring case, to `imageAssetId`.
//
// WHY A TEST AND NOT A COMMENT. ReplaceCampaign refuses a config_snapshot that newly names an
// asset which no longer exists (postgres.checkNewSnapshotAssetRefs), and it finds those
// references BY KEY. A platform whose config spelled the key differently would not be refused —
// it would be skipped, silently, and the guard would read as passing. That is the failure mode
// this file exists to make loud, and the moment it can appear is when a SIXTH platform is added
// to this package, which is why the test lives here rather than beside the extractor. Google is
// that sixth platform (LFXV2-2665), and it is the one the test was written for: its slot entries
// have to be OBJECTS carrying the key precisely because a flat list of ids would be skipped.
//
// The key is matched case-insensitively on purpose and both live spellings are represented
// below: the three configs in this package tag the field `json:"imageAssetId"`, while
// meta.AdVariant and linkedin.CreativeVariant carry no tag at all and so marshal as PascalCase
// `ImageAssetID`. encoding/json binds all of these on the way back in, so all of them are real
// references.
func TestPlatformConfigsNameTheirAssetUnderOneKey(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	for name, config := range map[string]any{
		"reddit":    redditConfig{ImageAssetID: assetID},
		"twitter":   twitterConfig{ImageAssetID: assetID},
		"microsoft": microsoftConfig{ImageAssetID: assetID},
		"meta":      metaConfig{Variants: []meta.AdVariant{{ImageAssetID: assetID}}},
		"linkedin":  linkedinConfig{Variants: []linkedin.CreativeVariant{{ImageAssetID: assetID}}},
		// EVERY Google slot, not one of them: the fourteen are separate fields on three structs,
		// and a fifteenth declared as a bare []string would carry no key on its elements at all.
		// Built through googleAdsCreativeSlots so a slot added later is covered by construction
		// rather than by someone remembering to extend this literal.
		"google": googleAdsConfigWithAssetInEverySlot(assetID),
	} {
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var decoded any
		if uerr := json.Unmarshal(raw, &decoded); uerr != nil {
			t.Fatalf("%s: unmarshal: %v", name, uerr)
		}
		keys := keysNaming(decoded, assetID)
		if len(keys) == 0 {
			t.Errorf("%s: marshalled config carries %s under no key at all:\n%s", name, assetID, raw)
			continue
		}
		for _, k := range keys {
			if !strings.EqualFold(k, snapshotAssetIDKey) {
				t.Errorf("%s: asset id is marshalled under %q, which postgres.snapshotAssetIDs will not extract; "+
					"either tag the field `json:\"imageAssetId\"` or widen snapshotAssetIDKey", name, k)
			}
		}
	}
}

// The loop above only proves that the keys it FOUND are the right key; it passes when a slot
// contributes no key at all, as long as one other slot does. For Google that is the whole risk —
// fourteen slots, one of which could be declared as a bare []string and silently contribute
// nothing — so count them: one key per enumerated slot, no more and no fewer.
func TestGoogleAdsConfigNamesItsAssetInEverySlot(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	cfg := googleAdsConfigWithAssetInEverySlot(assetID)
	want := len(googleAdsCreativeSlots(&cfg))
	if want == 0 {
		t.Fatal("googleAdsCreativeSlots enumerated no slots; the fixture is not exercising anything")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if uerr := json.Unmarshal(raw, &decoded); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if got := len(keysNaming(decoded, assetID)); got != want {
		t.Errorf("the marshalled config names the asset %d times, want once per slot (%d):\n%s", got, want, raw)
	}
}

// googleAdsConfigWithAssetInEverySlot returns a config naming assetID once in each of the image
// slots googleAdsCreativeSlots enumerates, with all three creative channels present so the
// enumeration returns all of them.
func googleAdsConfigWithAssetInEverySlot(assetID string) googleAdsConfig {
	cfg := googleAdsConfig{
		DemandGenCreative:      &googleAdsDemandGenCreativeConfig{},
		PerformanceMaxCreative: &googleAdsPerformanceMaxCreativeConfig{},
		DisplayCreative:        &googleAdsDisplayCreativeConfig{},
	}
	for _, slot := range googleAdsCreativeSlots(&cfg) {
		*slot.refs = googleAdsImageRefs{{ImageAssetID: assetID}}
	}
	return cfg
}

// keysNaming returns every object key anywhere in v whose value is exactly want.
func keysNaming(v any, want string) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if s == want {
					out = append(out, k)
				}
				continue
			}
			out = append(out, keysNaming(val, want)...)
		}
	case []any:
		for _, val := range t {
			out = append(out, keysNaming(val, want)...)
		}
	}
	return out
}
