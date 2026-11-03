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

// The dispatch half of #475: displayAdGroups and excludedAudienceSegments on the wire must
// reach the client's input field for field. The cascade itself is pinned in
// internal/platform/googleads/display_adgroups_test.go; a named group's ad cannot be reached
// from here for the reason googleads_display_wiring_test.go gives (real image bytes behind
// the fetch's dial guard).

// Every field of every group lands in its own client field, the creative through the same
// mapper as the campaign-level one.
func TestGoogleAdsDisplayAdGroups_MapsEveryField(t *testing.T) {
	var cfg googleAdsConfig
	raw := `{"displayAdGroups":[{"name":"Retargeting",
		"creative":{"marketingImages":["https://cdn.example.org/m.png"],"squareMarketingImages":[{"imageAssetId":"11111111-1111-4111-8111-111111111111"}],
			"headlines":["H"],"longHeadline":"LH","descriptions":["D"],"businessName":"LF","callToActionText":"Go"},
		"audienceSegments":["customers/1/userLists/11"],"customAudiences":["customers/1/customAudiences/22"],
		"customAffinities":["customers/1/customInterests/33"],"userInterests":["customers/1/userInterests/44"],
		"excludedAudienceSegments":["customers/1/userLists/70"]},{"name":"Prospecting"}],
		"excludedAudienceSegments":["customers/1/userLists/71"]}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := googleAdsDisplayAdGroups(cfg.DisplayAdGroups)
	want := []googleads.DisplayAdGroup{
		{
			Name: "Retargeting",
			Creative: googleads.DisplayCreative{
				MarketingImages:       []string{"https://cdn.example.org/m.png"},
				SquareMarketingImages: []string{googleAdsImageRefScheme + "11111111-1111-4111-8111-111111111111"},
				Headlines:             []string{"H"}, LongHeadline: "LH", Descriptions: []string{"D"},
				BusinessName: "LF", CallToActionText: "Go",
			},
			AudienceSegments:         []string{"customers/1/userLists/11"},
			CustomAudiences:          []string{"customers/1/customAudiences/22"},
			CustomAffinities:         []string{"customers/1/customInterests/33"},
			UserInterests:            []string{"customers/1/userInterests/44"},
			ExcludedAudienceSegments: []string{"customers/1/userLists/70"},
		},
		{Name: "Prospecting"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapped groups =\n  %+v\nwant\n  %+v", got, want)
	}
	if googleAdsDisplayAdGroups(nil) != nil {
		t.Error("an absent list must map to nil, the single-group shape")
	}
	if strings.Join(cfg.ExcludedAudienceSegments, ",") != "customers/1/userLists/71" {
		t.Errorf("excludedAudienceSegments = %v", cfg.ExcludedAudienceSegments)
	}
}

// The campaign-level exclusion reaches the campaign criteria mutate through Dispatch, as a
// negative user list.
func TestGoogleAds_DisplayAudienceExclusionReachesTheCampaignCriteria(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display",
		"excludedAudienceSegments":["customers/1234567890/userLists/70"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) != 1 {
		t.Fatalf("got %d campaignCriteria mutates, want 1 (the audience exclusion)", len(cap.campaignCriteria))
	}
	if body := string(cap.campaignCriteria[0]); !strings.Contains(body, `"negative":true,"userList":{"userList":"customers/1234567890/userLists/70"}`) {
		t.Errorf("campaign criteria mutate = %s, want a negative user list", body)
	}
}

// A named group reaches the client's preflight: a foreign exclusion on the SECOND group is
// refused by that group's name, before the budget — so the list, its order and the field are
// all carried.
func TestGoogleAds_DisplayAdGroupsReachThePreflight(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	group := func(name, excluded string) map[string]any {
		g := map[string]any{"name": name, "creative": displayWireCreative()}
		if excluded != "" {
			g["excludedAudienceSegments"] = []string{excluded}
		}
		return g
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": map[string]any{
		"budget": 50, "channel": "display",
		"displayAdGroups": []any{group("Retargeting", ""), group("Prospecting", "customers/9999999999/userLists/70")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, raw)
	if err == nil || !strings.Contains(err.Error(), `| Prospecting": google-ads: ExcludedAudienceSegments entry "customers/9999999999/userLists/70" is on customer 9999999999`) {
		t.Fatalf("err = %v, want the second group's foreign exclusion refused by name", err)
	}
	if camp != nil {
		t.Errorf("camp = %+v, want nil", camp)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("a budget mutate was sent for a config the preflight must refuse")
	}
}

// Off Display both new fields are refused before anything is created.
func TestGoogleAds_DisplayAdGroupFieldsRefusedOnSearch(t *testing.T) {
	for name, tc := range map[string]struct{ cfg, want string }{
		"excludedAudienceSegments": {`{"googleAdsConfig":{"budget":50,"excludedAudienceSegments":["customers/1234567890/userLists/70"]}}`,
			"audience exclusions are not supported on Search Campaign"},
		"displayAdGroups": {`{"googleAdsConfig":{"budget":50,"displayAdGroups":[{"name":"A"}]}}`,
			"Display ad groups are not supported on Search Campaign"},
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(tc.cfg))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if camp != nil {
				t.Errorf("camp = %+v, want nil", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("a budget mutate was sent for a config the preflight must refuse")
			}
		})
	}
}

// Every group's creative URLs are sanitized in the snapshot, on a deep copy: the caller's
// groups keep the signed URL the fetch still needs.
func TestGoogleAdsSnapshotConfig_SanitizesEveryDisplayAdGroup(t *testing.T) {
	first, second := fullDisplayCreativeConfig(), fullDisplayCreativeConfig()
	cfg := googleAdsConfig{DisplayAdGroups: []googleAdsDisplayAdGroupConfig{
		{Name: "A", Creative: first},
		{Name: "B", Creative: second, ExcludedAudienceSegments: []string{"customers/1/userLists/70"}},
		{Name: "C"},
	}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if len(snapshot.DisplayAdGroups) != 3 || snapshot.DisplayAdGroups[2].Creative != nil {
		t.Fatalf("snapshot groups = %+v", snapshot.DisplayAdGroups)
	}
	for i, g := range snapshot.DisplayAdGroups[:2] {
		for name, refs := range map[string]googleAdsImageRefs{
			"marketingImages": g.Creative.MarketingImages, "squareMarketingImages": g.Creative.SquareMarketingImages,
			"logoImages": g.Creative.LogoImages, "squareLogoImages": g.Creative.SquareLogoImages,
		} {
			if len(refs) != 1 || strings.Contains(refs[0].URL, "secret") || strings.Contains(refs[0].URL, "s3cr3t") {
				t.Errorf("group %d %s not sanitized: %+v", i+1, name, refs)
			}
		}
		if g.Creative == cfg.DisplayAdGroups[i].Creative {
			t.Errorf("group %d: the snapshot shares the caller's creative", i+1)
		}
	}
	if first.MarketingImages[0].URL != signedCreativeURL || second.MarketingImages[0].URL != signedCreativeURL {
		t.Error("the caller's group creatives were mutated; the signed url must still be fetchable")
	}
	if strings.Join(snapshot.DisplayAdGroups[1].ExcludedAudienceSegments, ",") != "customers/1/userLists/70" {
		t.Errorf("non-URL group fields must survive the snapshot: %+v", snapshot.DisplayAdGroups[1])
	}
}

// Each group's four slots are enumerated for stored-asset resolution, labelled by position.
func TestGoogleAdsCreativeSlots_EveryDisplayAdGroup(t *testing.T) {
	cfg := googleAdsConfig{DisplayAdGroups: []googleAdsDisplayAdGroupConfig{
		{Creative: &googleAdsDisplayCreativeConfig{}}, {}, {Creative: &googleAdsDisplayCreativeConfig{}},
	}}
	var labels []string
	for _, s := range googleAdsCreativeSlots(&cfg) {
		labels = append(labels, s.label)
	}
	want := []string{
		"display ad group 1 marketingImages", "display ad group 1 squareMarketingImages",
		"display ad group 1 logoImages", "display ad group 1 squareLogoImages",
		"display ad group 3 marketingImages", "display ad group 3 squareMarketingImages",
		"display ad group 3 logoImages", "display ad group 3 squareLogoImages",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("slots = %v, want %v", labels, want)
	}
}

// A status toggle on a two-group Display campaign reaches EVERY group's ad group and ad, in
// both directions. The blob is a CampaignResult in the shape CreateDisplayCampaign writes
// with DisplayAdGroups set: one AdGroups entry per group, the scalars a copy of the first,
// and no keyword criteria, which Display does not take.
func TestGoogleAds_ToggleStatus_TwoGroupDisplayCoversEveryGroupsAd(t *testing.T) {
	raw, err := json.Marshal(googleads.CampaignResult{
		CustomerID: "1234567890", CampaignID: "777", AdGroupID: "333", AdID: "444",
		AdGroups: []googleads.AdGroupResult{
			{Name: "KubeCon - Display Network | Retargeting", ID: "333", AdIDs: []string{"444"}},
			{Name: "KubeCon - Display Network | Prospecting", ID: "334", AdIDs: []string{"445"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	camp := &model.Campaign{
		Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Variant: googleAdsChannelDisplay, Result: raw,
	}
	wantGroups := "customers/1234567890/adGroups/333,customers/1234567890/adGroups/334"
	wantAds := "customers/1234567890/adGroupAds/333~444,customers/1234567890/adGroupAds/334~445"
	for _, status := range []string{model.CampaignRunPaused, model.CampaignRunActive} {
		t.Run(status, func(t *testing.T) {
			opts, cap := toggleServers(t)
			conns := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conns, identityEncryptor{}, opts...)
			ctx := context.Background()
			err := d.ToggleStatus(ctx, "proj", model.ProviderGoogleAds, camp, status)
			if err != nil {
				t.Fatalf("ToggleStatus: %v", err)
			}
			_, groups, ads := cap.snapshot()
			if got := strings.Join(groups, ","); got != wantGroups {
				t.Errorf("ad group updates = %s, want both groups %s", got, wantGroups)
			}
			if got := strings.Join(ads, ","); got != wantAds {
				t.Errorf("ad updates = %s, want each group's own ad %s", got, wantAds)
			}
		})
	}
}
