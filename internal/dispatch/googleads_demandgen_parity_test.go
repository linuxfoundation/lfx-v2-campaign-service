// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The dispatch half of #474: on "demand-gen" the existing biddingStrategy/targetCpa,
// adSchedules, deviceBidModifiers (CONNECTED_TV included) and sitelinks fields now reach
// the outbound requests instead of being refused, through the same mapping Search uses.
func TestGoogleAds_DemandGenParityFieldsReachTheRequests(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen",` +
		`"biddingStrategy":"target-cpa","targetCpa":25,` +
		`"adSchedules":[{"dayOfWeek":"MONDAY","startHour":9,"startMinute":0,"endHour":17,"endMinute":0}],` +
		`"deviceBidModifiers":[{"device":"CONNECTED_TV","bidModifier":0}],` +
		`"sitelinks":[{"text":"Register","finalUrl":"https://events.example/kc/register"}]}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("nil campaign on success")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaigns) != 1 || !strings.Contains(string(cap.campaigns[0]), `"targetCpa":{"targetCpaMicros":25000000}`) {
		t.Errorf("campaign body %s, want the standalone targetCpa", cap.campaigns)
	}
	if len(cap.campaignCriteria) != 1 {
		t.Fatalf("got %d campaignCriteria mutates, want 1", len(cap.campaignCriteria))
	}
	crit := string(cap.campaignCriteria[0])
	if !strings.Contains(crit, `"adSchedule":{"dayOfWeek":"MONDAY"`) || !strings.Contains(crit, `"bidModifier":0,"device":{"type":"CONNECTED_TV"}`) {
		t.Errorf("campaign criteria %s, want the schedule and the CONNECTED_TV exclusion", crit)
	}
	if !strings.Contains(string(cap.assets), `"linkText":"Register"`) || !strings.Contains(string(cap.campaignAssets), `"fieldType":"SITELINK"`) {
		t.Errorf("sitelink assets %s / links %s", cap.assets, cap.campaignAssets)
	}
}

// demandGenAds and a group's ads (#474) decode under their wire names and map onto the
// client's input, every field and a stored-asset logo included.
func TestGoogleAdsDemandGenAds_DecodeAndMap(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	raw := json.RawMessage(`{"googleAdsConfig":{"channel":"demand-gen",
		"demandGenAds":[{"name":"Cut A","videoCreative":{"youtubeVideoIds":["dQw4w9WgXcQ"],"logoImages":[{"imageAssetId":"` + assetID + `"}]}},
			{"name":"Static","creative":{"marketingImages":["https://cdn.example.org/m.png"],"businessName":"LF"}}],
		"demandGenAdGroups":[{"name":"G","ads":[{"name":"One","creative":{"logoImages":["https://cdn.example.org/l.png"]}}]}]}}`)
	var cfg googleAdsConfig
	if err := unmarshalPlatformConfig(raw, "googleAdsConfig", &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ads := googleAdsDemandGenAds(cfg.DemandGenAds)
	want := []googleads.DemandGenAd{
		{Name: "Cut A", VideoCreative: googleads.DemandGenVideoCreative{
			YouTubeVideoIDs: []string{"dQw4w9WgXcQ"},
			LogoImages:      []string{googleads.ResolvedImageRefPrefix + assetID},
		}},
		{Name: "Static", Creative: googleads.DemandGenCreative{MarketingImages: []string{"https://cdn.example.org/m.png"}, BusinessName: "LF"}},
	}
	if !reflect.DeepEqual(ads, want) {
		t.Errorf("ads =\n  %+v\nwant\n  %+v", ads, want)
	}
	groups := googleAdsDemandGenAdGroups(cfg.DemandGenAdGroups)
	if len(groups) != 1 || len(groups[0].Ads) != 1 || groups[0].Ads[0].Name != "One" ||
		!reflect.DeepEqual(groups[0].Ads[0].Creative.LogoImages, []string{"https://cdn.example.org/l.png"}) {
		t.Errorf("group ads = %+v", groups)
	}
	if got := googleAdsDemandGenAds(nil); got != nil {
		t.Errorf("no ads maps to %+v, want nil", got)
	}
}

// Every ad's image URLs are reduced in the snapshot, on a deep copy that leaves the
// caller's config — which still reaches Google — untouched.
func TestGoogleAdsSnapshotConfig_SanitizesDemandGenAds(t *testing.T) {
	cfg := googleAdsConfig{
		DemandGenAds: []googleAdsDemandGenAdConfig{{Name: "A", Creative: fullCreativeConfig()}},
		DemandGenAdGroups: []googleAdsDemandGenAdGroupConfig{{Name: "G", Ads: []googleAdsDemandGenAdConfig{
			{Name: "V", VideoCreative: &googleAdsDemandGenVideoCreativeConfig{LogoImages: googleAdsImageRefsFromURLs([]string{"https://cdn.example.org/g.png?sig=group-secret"})}},
		}}},
	}
	snapshot := googleAdsSnapshotConfig(cfg)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "s3cr3t") {
		t.Errorf("the snapshot still carries a signing query: %s", raw)
	}
	if snapshot.DemandGenAds[0].Name != "A" || snapshot.DemandGenAdGroups[0].Ads[0].Name != "V" {
		t.Errorf("the snapshot lost an ad's name: %s", raw)
	}
	if cfg.DemandGenAds[0].Creative.MarketingImages[0].URL != signedCreativeURL ||
		cfg.DemandGenAdGroups[0].Ads[0].VideoCreative.LogoImages[0].URL != "https://cdn.example.org/g.png?sig=group-secret" {
		t.Error("sanitizing the snapshot modified the caller's config, which must still reach Google in full")
	}
}

// googleAdsCreativeSlots names every ad's slots, on the campaign and inside a group, so a
// stored-asset entry in any of them is resolved.
func TestGoogleAdsCreativeSlots_CoverDemandGenAds(t *testing.T) {
	cfg := googleAdsConfig{
		DemandGenAds: []googleAdsDemandGenAdConfig{{VideoCreative: &googleAdsDemandGenVideoCreativeConfig{}}},
		DemandGenAdGroups: []googleAdsDemandGenAdGroupConfig{{Ads: []googleAdsDemandGenAdConfig{
			{VideoCreative: &googleAdsDemandGenVideoCreativeConfig{}},
			{Creative: &googleAdsDemandGenCreativeConfig{}},
		}}},
	}
	var labels []string
	for _, slot := range googleAdsCreativeSlots(&cfg) {
		labels = append(labels, slot.label)
	}
	want := []string{
		"demand gen ad 1 video logoImages",
		"demand gen ad group 1 ad 1 video logoImages",
		"demand gen ad group 1 ad 2 marketingImages", "demand gen ad group 1 ad 2 squareMarketingImages",
		"demand gen ad group 1 ad 2 portraitImages", "demand gen ad group 1 ad 2 tallPortraitImages",
		"demand gen ad group 1 ad 2 logoImages",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("slots =\n  %q\nwant\n  %q", labels, want)
	}
}

// The #474 ad-list refusals reach the caller through Dispatch BEFORE the budget mutate.
func TestGoogleAds_DemandGenAdsRefusalsArePreCreate(t *testing.T) {
	const ad = `{"name":"A","creative":{"marketingImages":["https://cdn.example.org/m.png"],"logoImages":["https://cdn.example.org/l.png"],"headlines":["H"],"descriptions":["D"],"businessName":"LF"}}`
	cases := []struct{ name, cfg, wantSub string }{
		{"ads on search", `"demandGenAds":[` + ad + `]`, "Demand Gen ads are not supported on"},
		{"duplicate ad names", `"channel":"demand-gen","demandGenAds":[` + ad + `,` + strings.Replace(ad, `"A"`, `"a"`, 1) + `]`, "is used more than once"},
		{"demographics on demand gen", `"channel":"demand-gen","excludedGenders":["MALE"]`, "inside the Audience that DemandGenAudience"},
		{"target ROAS on demand gen", `"channel":"demand-gen","biddingStrategy":"maximize-conversion-value","targetRoas":4`, "a target ROAS is not supported on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,` + tc.cfg + `}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate")
			}
		})
	}
}

// A single Demand Gen group with several ads is recorded in adGroups (#474), so the launch
// toggle reaches every ad rather than only the scalar adId; with every planned ad created it
// is complete.
func TestGoogleAdsToggleTargets_SeveralDemandGenAdsInOneGroup(t *testing.T) {
	c := &model.Campaign{Result: json.RawMessage(`{"adGroupId":"333","adId":"444","adGroups":[{"name":"E - Display","id":"333","adIds":["444","445","555"],"plannedAds":3}]}`)}
	targets, _, incomplete := googleAdsToggleTargets(c)
	if len(targets) != 1 || targets[0].AdGroupID != "333" || strings.Join(targets[0].AdIDs, ",") != "444,445,555" || incomplete != nil {
		t.Errorf("targets = %+v incomplete = %v, want every ad of the group", targets, incomplete)
	}
}

// A Demand Gen group whose ad 2 failed (or came back UNCONFIRMED) holds fewer ad ids than
// it planned (#474). It must not read as complete — that would launch ad 1 and say nothing
// about ad 2 — so it is reported in incomplete_ad_groups, while the ad it has is still
// toggled, as every group that CAN take the toggle is. A complete group beside it is not
// reported, and a row with no plannedAds (older rows, Search) keeps its old reading.
func TestGoogleAdsToggleTargets_PartialDemandGenGroupIsIncomplete(t *testing.T) {
	c := &model.Campaign{Result: json.RawMessage(`{"adGroupId":"333","adId":"444","adGroups":[` +
		`{"name":"E - Display | Prospecting","id":"333","adIds":["444"],"plannedAds":2,` +
		`"ads":[{"name":"Cut A","id":"444","creativeAssetIds":["900"]},{"name":"Cut B","creativeAssetIds":["901"]}]},` +
		`{"name":"E - Display | Retargeting","id":"334","adIds":["446","447"],"plannedAds":2},` +
		`{"name":"E - Search","id":"335","adIds":["448"]}]}`)}
	targets, _, incomplete := googleAdsToggleTargets(c)
	if strings.Join(incomplete, ",") != "E - Display | Prospecting" {
		t.Errorf("incomplete = %v, want the partial group alone", incomplete)
	}
	if len(targets) != 3 || targets[0].AdGroupID != "333" || strings.Join(targets[0].AdIDs, ",") != "444" ||
		strings.Join(targets[1].AdIDs, ",") != "446,447" || strings.Join(targets[2].AdIDs, ",") != "448" {
		t.Errorf("targets = %+v, want the partial group's created ad and both complete groups", targets)
	}
}

// A config whose only URL-bearing field is demandGenAds must not take the snapshot's early
// return, which would persist the signed URL unsanitized.
func TestGoogleAdsSnapshotConfig_SanitizesDemandGenAdsAlone(t *testing.T) {
	cfg := googleAdsConfig{DemandGenAds: []googleAdsDemandGenAdConfig{{Name: "A", Creative: fullCreativeConfig()}}}
	raw, err := json.Marshal(googleAdsSnapshotConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "s3cr3t") {
		t.Errorf("the snapshot still carries a signing query: %s", raw)
	}
}
