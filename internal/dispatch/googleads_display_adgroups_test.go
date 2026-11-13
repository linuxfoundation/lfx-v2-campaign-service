// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
			"headlines":["H"],"longHeadline":"LH","descriptions":["D"],"businessName":"LF","callToActionText":"Learn more"},
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
				BusinessName: "LF", CallToActionText: "Learn more",
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
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
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
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
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
	if err == nil || !strings.Contains(err.Error(), `| Prospecting": google-ads: ExcludedAudienceSegments entry "customers/9999999999/userLists/70" is on customer "9999999999"`) {
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

// Off Display both new fields are refused before anything is created. The Display audiences
// gate is open here so the client's own refusal is what is tested; with it shut, the gate
// refuses excludedAudienceSegments first (TestGoogleAds_DisplayAudienceExclusionsGate).
func TestGoogleAds_DisplayAdGroupFieldsRefusedOnSearch(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
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

// TestGoogleAdsSnapshotConfig_EachEarlyReturnFieldAlone sets each of the twelve fields the
// snapshot's early return names, ALONE, with a signed URL, and asserts the snapshot loses the
// signature. A config carrying only one of them taking the early return is exactly the leak
// each condition was added to prevent, and the table is the list docs/knowledge cites.
func TestGoogleAdsSnapshotConfig_EachEarlyReturnFieldAlone(t *testing.T) {
	const signed = "https://cdn.example.org/a.png?X-Amz-Signature=s3cr3t"
	refs := googleAdsImageRefs{{URL: signed}}
	for name, cfg := range map[string]googleAdsConfig{
		"sitelinks":  {Sitelinks: []googleAdsSitelinkConfig{{FinalURL: signed}}},
		"promotions": {Promotions: []googleAdsPromotionConfig{{FinalURL: signed}}},
		"prices": {Prices: []googleAdsPriceConfig{{
			Offerings: []googleAdsPriceOfferingConfig{{FinalURL: signed}},
		}}},
		"leadForms": {LeadForms: []googleAdsLeadFormConfig{{PrivacyPolicyURL: signed}}},
		"demandGenCreative": {
			DemandGenCreative: &googleAdsDemandGenCreativeConfig{MarketingImages: refs},
		},
		"demandGenVideoCreative": {
			DemandGenVideoCreative: &googleAdsDemandGenVideoCreativeConfig{LogoImages: refs},
		},
		"demandGenAds": {DemandGenAds: []googleAdsDemandGenAdConfig{{
			Creative: &googleAdsDemandGenCreativeConfig{MarketingImages: refs},
		}}},
		"demandGenAdGroups": {DemandGenAdGroups: []googleAdsDemandGenAdGroupConfig{{
			Creative: &googleAdsDemandGenCreativeConfig{MarketingImages: refs},
		}}},
		"performanceMaxCreative": {
			PerformanceMaxCreative: &googleAdsPerformanceMaxCreativeConfig{MarketingImages: refs},
		},
		"displayCreative": {
			DisplayCreative: &googleAdsDisplayCreativeConfig{MarketingImages: refs},
		},
		"displayAdGroups": {DisplayAdGroups: []googleAdsDisplayAdGroupConfig{{
			Creative: &googleAdsDisplayCreativeConfig{MarketingImages: refs},
		}}},
		"searchAssets": {SearchAssets: &googleAdsSearchAssetsConfig{AdImages: refs}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(googleAdsSnapshotConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "s3cr3t") {
				t.Errorf("snapshot keeps the signature: %s", raw)
			}
			if !strings.Contains(string(raw), "cdn.example.org") {
				t.Errorf("snapshot lost the URL entirely, so this case proves nothing: %s", raw)
			}
		})
	}
}

// displayGateConfig is a Display create naming two groups, each with a creative; extra is
// merged into the config and group2 into the second group.
func displayGateConfig(t *testing.T, extra, group2 map[string]any) json.RawMessage {
	t.Helper()
	second := map[string]any{"name": "Prospecting", "creative": displayWireCreative()}
	for k, v := range group2 {
		second[k] = v
	}
	cfg := map[string]any{
		"budget": 50, "channel": "display",
		"displayAdGroups": []any{
			map[string]any{"name": "Retargeting", "creative": displayWireCreative()}, second,
		},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": cfg})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The #475 inputs that name an account-owned user list or custom segment sit behind the
// Display audiences gate, as the campaign-level lists do: the campaign-level
// excludedAudienceSegments and each displayAdGroups entry's audienceSegments,
// customAudiences, customAffinities and excludedAudienceSegments. With
// GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED anything but exactly "true", each is refused in
// Dispatch before the budget, as a not-created PreflightRefusal naming the field.
func TestGoogleAds_DisplayAudienceExclusionsGate(t *testing.T) {
	const (
		userList = "customers/1234567890/userLists/11"
		custom   = "customers/1234567890/customAudiences/22"
		affinity = "customers/1234567890/customInterests/33"
		excluded = "customers/1234567890/userLists/70"
	)
	inputs := []struct {
		name          string
		extra, group2 map[string]any
		field         string
	}{
		{"campaign exclusions", map[string]any{"excludedAudienceSegments": []string{excluded}}, nil,
			"excludedAudienceSegments"},
		{"group user lists", nil, map[string]any{"audienceSegments": []string{userList}},
			"displayAdGroups entry 2 audienceSegments"},
		{"group custom audiences", nil, map[string]any{"customAudiences": []string{custom}},
			"displayAdGroups entry 2 customAudiences"},
		{"group custom affinities", nil, map[string]any{"customAffinities": []string{affinity}},
			"displayAdGroups entry 2 customAffinities"},
		{"group exclusions", nil, map[string]any{"excludedAudienceSegments": []string{excluded}},
			"displayAdGroups entry 2 excludedAudienceSegments"},
		// Any non-empty list counts, whatever it holds.
		{"blank group exclusion", nil, map[string]any{"excludedAudienceSegments": []string{" "}},
			"displayAdGroups entry 2 excludedAudienceSegments"},
	}
	for _, in := range inputs {
		for _, env := range []struct{ name, value string }{
			{"unset", unsetEnv}, {"empty", ""}, {"false", "false"}, {"wrong case", "TRUE"},
			{"leading space", " true"},
		} {
			t.Run(in.name+"/"+env.name, func(t *testing.T) {
				setGateEnv(t, constants.EnvGoogleAdsDisplayAudiencesEnabled, env.value)
				opts, cap := targetingServers(t)
				conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
				d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
				camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds,
					displayGateConfig(t, in.extra, in.group2))
				if err == nil {
					t.Fatal("expected the dispatch to be refused while the gate is off")
				}
				for _, want := range []string{
					"Display audience targeting (" + in.field + ") is disabled on this deployment",
					"omit " + in.field,
					"; or ask an operator to set " + constants.EnvGoogleAdsDisplayAudiencesEnabled + ` to "true"`,
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				var pce *preCreateError
				if !errors.As(err, &pce) {
					t.Errorf("error = %v, want a not-created error so the claim is released", err)
				}
				got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
				if got != err.Error() {
					t.Errorf("job message = %q, want the gate's own sentence %q", got, err.Error())
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
}

// Every gated field a config sets is named, campaign-level first, then each group in order.
func TestGoogleAdsDisplayAudienceFieldsSet_NamesEveryGroupField(t *testing.T) {
	cfg := googleAdsConfig{
		Channel:                  "display",
		ExcludedAudienceSegments: []string{"x"},
		DisplayAdGroups: []googleAdsDisplayAdGroupConfig{
			{
				AudienceSegments: []string{"x"}, CustomAudiences: []string{"x"},
				UserInterests: []string{"x"},
			},
			{},
			{CustomAffinities: []string{"x"}, ExcludedAudienceSegments: []string{"x"}},
		},
	}
	want := []string{
		"excludedAudienceSegments",
		"displayAdGroups entry 1 audienceSegments", "displayAdGroups entry 1 customAudiences",
		"displayAdGroups entry 3 customAffinities",
		"displayAdGroups entry 3 excludedAudienceSegments",
	}
	if got := googleAdsDisplayAudienceFieldsSet(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %q, want %q", got, want)
	}
}

// With the gate exactly "true", every gated #475 input passes Dispatch and reaches the
// client's preflight: a foreign exclusion on the LAST group is refused there by that group's
// name, which it can only be if the gate let every earlier list through.
func TestGoogleAds_DisplayAudienceExclusionsGateOpensOnExactlyTrue(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	raw := displayGateConfig(t,
		map[string]any{"excludedAudienceSegments": []string{"customers/1234567890/userLists/71"}},
		map[string]any{
			"audienceSegments":         []string{"customers/1234567890/userLists/11"},
			"customAudiences":          []string{"customers/1234567890/customAudiences/22"},
			"customAffinities":         []string{"customers/1234567890/customInterests/33"},
			"excludedAudienceSegments": []string{"customers/9999999999/userLists/70"},
		})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, raw)
	if err == nil || strings.Contains(err.Error(), "disabled on this deployment") ||
		!strings.Contains(err.Error(), `| Prospecting": google-ads: ExcludedAudienceSegments `+
			`entry "customers/9999999999/userLists/70" is on customer "9999999999"`) {
		t.Fatalf("err = %v, want the client's refusal of group 2's foreign exclusion, "+
			"not the gate", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("a budget mutate was sent for a config the preflight must refuse")
	}
}

// With the gate off, a displayAdGroups config naming none of the gated lists is untouched:
// creatives, names and userInterests (Google's own categories, ungated at both levels) pass
// Dispatch and reach the client's preflight, which here refuses group 2's foreign user
// interest by name.
func TestGoogleAds_DisplayAudienceExclusionsGateLeavesUngatedGroupInputsAlone(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "")
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	raw := displayGateConfig(t, map[string]any{"excludedPlacements": []string{"example.com"}},
		map[string]any{"userInterests": []string{"customers/9999999999/userInterests/44"}})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, raw)
	if err == nil || strings.Contains(err.Error(), "disabled on this deployment") ||
		!strings.Contains(err.Error(), `| Prospecting": google-ads: UserInterests entry `+
			`"customers/9999999999/userInterests/44" is on customer "9999999999"`) {
		t.Fatalf("err = %v, want the client's refusal of group 2's foreign user interest, "+
			"not the gate", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("a budget mutate was sent for a config the preflight must refuse")
	}
}
