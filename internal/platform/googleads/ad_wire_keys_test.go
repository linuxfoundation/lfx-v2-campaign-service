// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"encoding/json"
	"testing"
)

// TestAdTypeWireKeysMatchTheAdOneof pins the key each channel's ad-type payload is sent
// under. Google's REST layer refuses an unknown key with a 400, and on every channel here
// that refusal arrives at adGroupAds:mutate, after the budget, the campaign and the ad
// group already exist — so a misspelled key is a campaign that can never be activated,
// and the fake servers in this package accept any key.
//
// Each want is the JSON spelling of the Ad oneof FIELD in v23 resources/ad.proto, not the
// name of the message it carries. That distinction is the defect #437 fixed: Demand Gen
// was sent as `demandGenMultiAssetAdInfo` (the message) where the field is
// `demand_gen_multi_asset_ad` (= 62).
func TestAdTypeWireKeysMatchTheAdOneof(t *testing.T) {
	cases := []struct {
		name string
		ad   any
		want string // v23 ad.proto oneof field, JSON spelling
	}{
		{"search", adCreate{ResponsiveSearchAd: &responsiveSearchAd{}}, "responsiveSearchAd"},                           // responsive_search_ad = 25
		{"demand gen", demandGenAdCreate{DemandGenMultiAssetAd: &demandGenMultiAssetAdInfo{}}, "demandGenMultiAssetAd"}, // demand_gen_multi_asset_ad = 62
		{"display", displayAdCreate{ResponsiveDisplayAd: &responsiveDisplayAdInfo{}}, "responsiveDisplayAd"},            // responsive_display_ad = 31
		{"video", videoAdCreate{VideoResponsiveAd: &videoResponsiveAdInfo{}}, "videoResponsiveAd"},                      // video_responsive_ad = 39
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.ad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, ok := keys[tc.want]; !ok {
				t.Errorf("ad payload %s carries no %q key; Google refuses any other spelling", raw, tc.want)
			}
			// Exactly one ad-type arm beside finalUrls: a second key is either a misspelling
			// sent alongside the right one or a second oneof arm, and Google refuses both.
			for k := range keys {
				if k != tc.want && k != "finalUrls" {
					t.Errorf("ad payload %s carries unexpected key %q", raw, k)
				}
			}
		})
	}
}
