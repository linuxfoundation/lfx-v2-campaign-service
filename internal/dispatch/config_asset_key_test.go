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
// to this package, which is why the test lives here rather than beside the extractor.
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
