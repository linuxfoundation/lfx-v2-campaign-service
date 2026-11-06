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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The wire names of the two #460 inputs, decoded the way Dispatch decodes them, and the
// mapping to the client's input — every field, including a stored-asset logo.
func TestGoogleAdsDemandGenVideoAndAdGroups_DecodeAndMap(t *testing.T) {
	const assetID = "11111111-1111-4111-8111-111111111111"
	raw := json.RawMessage(`{"googleAdsConfig":{"channel":"demand-gen",
		"demandGenVideoCreative":{"youtubeVideoIds":["dQw4w9WgXcQ"],"logoImages":[{"imageAssetId":"` + assetID + `"}],
			"headlines":["H"],"longHeadlines":["LH"],"descriptions":["D"],"businessName":"Linux Foundation","callToAction":"SIGN_UP"},
		"demandGenAdGroups":[{"name":"Retargeting","audience":"customers/1234567890/audiences/41",
			"creative":{"marketingImages":["https://cdn.example.org/m.png"],"logoImages":["https://cdn.example.org/l.png"],
				"headlines":["H"],"descriptions":["D"],"businessName":"LF"}},
			{"name":"Prospecting","videoCreative":{"youtubeVideoIds":["abcdefghijk"],"logoImages":["https://cdn.example.org/v.png"]}}]}}`)
	var cfg googleAdsConfig
	if err := unmarshalPlatformConfig(raw, "googleAdsConfig", &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	video := googleAdsDemandGenVideoCreative(cfg.DemandGenVideoCreative)
	want := googleads.DemandGenVideoCreative{
		YouTubeVideoIDs: []string{"dQw4w9WgXcQ"},
		LogoImages:      []string{googleads.ResolvedImageRefPrefix + assetID},
		Headlines:       []string{"H"},
		LongHeadlines:   []string{"LH"},
		Descriptions:    []string{"D"},
		BusinessName:    "Linux Foundation",
		CallToAction:    "SIGN_UP",
	}
	if !reflect.DeepEqual(video, want) {
		t.Errorf("video creative =\n  %+v\nwant\n  %+v", video, want)
	}
	groups := googleAdsDemandGenAdGroups(cfg.DemandGenAdGroups)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if g := groups[0]; g.Name != "Retargeting" || g.Audience != "customers/1234567890/audiences/41" ||
		!reflect.DeepEqual(g.Creative.MarketingImages, []string{"https://cdn.example.org/m.png"}) || g.Creative.BusinessName != "LF" ||
		len(g.VideoCreative.YouTubeVideoIDs) != 0 {
		t.Errorf("group 0 = %+v", g)
	}
	if g := groups[1]; g.Name != "Prospecting" || g.Audience != "" || !reflect.DeepEqual(g.VideoCreative.YouTubeVideoIDs, []string{"abcdefghijk"}) ||
		!reflect.DeepEqual(g.VideoCreative.LogoImages, []string{"https://cdn.example.org/v.png"}) || len(g.Creative.MarketingImages) != 0 {
		t.Errorf("group 1 = %+v", g)
	}
	if got := googleAdsDemandGenVideoCreative(nil); !reflect.DeepEqual(got, googleads.DemandGenVideoCreative{}) {
		t.Errorf("nil video creative maps to %+v, want the zero value", got)
	}
	if got := googleAdsDemandGenAdGroups(nil); got != nil {
		t.Errorf("no groups maps to %+v, want nil", got)
	}
}

// The video logo and every group's image URLs are reduced in the snapshot, on a deep copy
// that leaves the caller's config — which still has to reach Google — untouched.
func TestGoogleAdsSnapshotConfig_SanitizesDemandGenVideoAndAdGroups(t *testing.T) {
	cfg := googleAdsConfig{
		DemandGenVideoCreative: &googleAdsDemandGenVideoCreativeConfig{LogoImages: googleAdsImageRefsFromURLs([]string{"https://cdn.example.org/v.png?sig=video-secret"})},
		DemandGenAdGroups: []googleAdsDemandGenAdGroupConfig{
			{Name: "A", Creative: fullCreativeConfig()},
			{Name: "B", VideoCreative: &googleAdsDemandGenVideoCreativeConfig{LogoImages: googleAdsImageRefsFromURLs([]string{"https://cdn.example.org/g.png?sig=group-secret"})}},
		},
	}
	snapshot := googleAdsSnapshotConfig(cfg)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "s3cr3t") {
		t.Errorf("the snapshot still carries a signing query: %s", raw)
	}
	for _, url := range []string{
		snapshot.DemandGenVideoCreative.LogoImages[0].URL,
		snapshot.DemandGenAdGroups[0].Creative.MarketingImages[0].URL,
		snapshot.DemandGenAdGroups[0].Creative.LogoImages[0].URL,
		snapshot.DemandGenAdGroups[1].VideoCreative.LogoImages[0].URL,
	} {
		if !strings.HasPrefix(url, "https://cdn.example.org") {
			t.Errorf("sanitized URL %q lost its scheme and host", url)
		}
	}
	if snapshot.DemandGenAdGroups[1].Name != "B" {
		t.Errorf("the snapshot lost a group's name: %+v", snapshot.DemandGenAdGroups[1])
	}
	if cfg.DemandGenVideoCreative.LogoImages[0].URL != "https://cdn.example.org/v.png?sig=video-secret" ||
		cfg.DemandGenAdGroups[0].Creative.MarketingImages[0].URL != signedCreativeURL ||
		cfg.DemandGenAdGroups[1].VideoCreative.LogoImages[0].URL != "https://cdn.example.org/g.png?sig=group-secret" {
		t.Error("sanitizing the snapshot modified the caller's config, which must still reach Google in full")
	}
}

// Every #460 refusal reaches the caller through Dispatch BEFORE the budget mutate. The
// audience gate is ON here, so the foreign group audience is the client's own refusal; the
// gate is TestGoogleAds_DemandGenAdGroupAudienceGate.
func TestGoogleAds_DemandGenVideoAndAdGroupRefusalsArePreCreate(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDemandGenAudienceEnabled, "true")
	const video = `"demandGenVideoCreative":{"youtubeVideoIds":["dQw4w9WgXcQ"],"logoImages":["https://cdn.example.org/v.png"],` +
		`"headlines":["H"],"longHeadlines":["LH"],"descriptions":["D"],"businessName":"LF"}`
	const image = `"demandGenCreative":{"marketingImages":["https://cdn.example.org/m.png"],"logoImages":["https://cdn.example.org/l.png"],` +
		`"headlines":["H"],"descriptions":["D"],"businessName":"LF"}`
	cases := []struct {
		name, cfg, wantSub string
	}{
		{"video creative on search", video, "supported on Demand Gen campaigns only, not on Search"},
		{"video beside image", `"channel":"demand-gen",` + video + `,` + image, "both a Demand Gen image creative and a Demand Gen video creative"},
		{"unlisted call to action", `"channel":"demand-gen",` + strings.Replace(video, `"businessName":"LF"`, `"businessName":"LF","callToAction":"REGISTER"`, 1), "is not one this client accepts"},
		{"ad groups on display", `"channel":"display","demandGenAdGroups":[{"name":"A",` + video + `}]`, "Demand Gen ad groups are not supported on"},
		{"duplicate group names", `"channel":"demand-gen","demandGenAdGroups":[{"name":"A","videoCreative":` + video[len(`"demandGenVideoCreative":`):] + `},{"name":"a","videoCreative":` + video[len(`"demandGenVideoCreative":`):] + `}]`, "is used more than once"},
		{"foreign group audience", `"channel":"demand-gen","demandGenAdGroups":[{"name":"A","audience":"customers/9999999999/audiences/4","videoCreative":` + video[len(`"demandGenVideoCreative":`):] + `}]`, "belongs to customer \"9999999999\""},
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

// googleAdsCreativeSlots enumerates the video logo and every group's slots — the image
// lists and the video logo — so a stored-asset entry in any of them is resolved. The
// every-slot tests elsewhere fill slots THROUGH this enumeration, so they cannot notice a
// slot it misses; this one names them.
func TestGoogleAdsCreativeSlots_CoverDemandGenVideoAndAdGroups(t *testing.T) {
	cfg := googleAdsConfig{
		DemandGenVideoCreative: &googleAdsDemandGenVideoCreativeConfig{},
		DemandGenAdGroups: []googleAdsDemandGenAdGroupConfig{
			{Creative: &googleAdsDemandGenCreativeConfig{}},
			{VideoCreative: &googleAdsDemandGenVideoCreativeConfig{}},
		},
	}
	var labels []string
	for _, slot := range googleAdsCreativeSlots(&cfg) {
		labels = append(labels, slot.label)
	}
	want := []string{
		"demand gen video logoImages",
		"demand gen ad group 1 marketingImages", "demand gen ad group 1 squareMarketingImages",
		"demand gen ad group 1 portraitImages", "demand gen ad group 1 tallPortraitImages",
		"demand gen ad group 1 logoImages",
		"demand gen ad group 2 video logoImages",
	}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("slots =\n  %q\nwant\n  %q", labels, want)
	}
	// The pointers are the config's own, so a resolved id written through one lands in it.
	for _, slot := range googleAdsCreativeSlots(&cfg) {
		*slot.refs = googleAdsImageRefs{{ImageAssetID: "x"}}
	}
	if cfg.DemandGenAdGroups[1].VideoCreative.LogoImages[0].ImageAssetID != "x" || cfg.DemandGenVideoCreative.LogoImages[0].ImageAssetID != "x" {
		t.Error("the enumerated slots do not point into the config")
	}
}

// demandGenGroupGateConfig is a Demand Gen create naming one ad group per audience given ("" for
// a group without one), each with an image ad whose images are STORED assets, so the create
// reaches the ad mutate over the fake API with nothing fetched.
func demandGenGroupGateConfig(t *testing.T, audiences ...string) json.RawMessage {
	t.Helper()
	groups := make([]map[string]any, len(audiences))
	for i, audience := range audiences {
		groups[i] = map[string]any{
			"name":     "Group " + string(rune('A'+i)),
			"audience": audience,
			"creative": map[string]any{
				"marketingImages": []any{map[string]any{"imageAssetId": gaWireMarketingAssetID}},
				"logoImages":      []any{map[string]any{"imageAssetId": gaWireLogoAssetID}},
				"headlines":       []string{"Join us at KubeCon"},
				"descriptions":    []string{"Talks, workshops and hallway track."},
				"businessName":    "Linux Foundation",
			},
		}
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": map[string]any{
		"budget": 50, "channel": "demand-gen", "demandGenAdGroups": groups,
	}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func gateAssetBudget() *AssetReserver {
	return NewAssetReserver(MaxConcurrentVariantAssetBytes, time.Second)
}

// A demandGenAdGroups entry's audience is the same account-checked input as demandGenAudience,
// so GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED gates it too: anything but exactly "true"
// refuses it in Dispatch before the budget, even an Audience the client would accept, with the
// gate's own sentence on the job. Exactly "true" lets it through to the ad group, and a group
// without an audience is never gated.
func TestGoogleAds_DemandGenAdGroupAudienceGate(t *testing.T) {
	const own = "customers/1234567890/audiences/4242"
	cases := []struct {
		name, env string
		audiences []string
		wantWhich string
	}{
		{"unset", unsetEnv, []string{own}, "demandGenAdGroups entry 1 is"},
		{"empty", "", []string{own}, "demandGenAdGroups entry 1 is"},
		{"false", "false", []string{own}, "demandGenAdGroups entry 1 is"},
		{"wrong case", "TRUE", []string{own}, "demandGenAdGroups entry 1 is"},
		{"leading space", " true", []string{own}, "demandGenAdGroups entry 1 is"},
		{"blank audience", "", []string{"  "}, "demandGenAdGroups entry 1 is"},
		{"second group only", "", []string{"", own}, "demandGenAdGroups entry 2 is"},
		{"every group", "", []string{own, own}, "demandGenAdGroups entries 1, 2 are"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsDemandGenAudienceEnabled, tc.env)
			store := gaWireLibrary(t)
			d, cap := gaWireDispatcher(t, store, gateAssetBudget())
			cfg := demandGenGroupGateConfig(t, tc.audiences...)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused while the gate is off")
			}
			msg := err.Error()
			if !strings.Contains(msg, constants.EnvGoogleAdsDemandGenAudienceEnabled) ||
				!strings.Contains(msg, "; or ask an operator to set "+constants.EnvGoogleAdsDemandGenAudienceEnabled+` to "true"`) ||
				!strings.Contains(msg, tc.wantWhich) {
				t.Errorf("error = %v, want it to name the gate, how to enable it and %q", err, tc.wantWhich)
			}
			if strings.Contains(msg, "Group A") || strings.Contains(msg, own) {
				t.Errorf("error = %v, want no caller value in the sentence", err)
			}
			var pce *preCreateError
			if !errors.As(err, &pce) {
				t.Errorf("error = %v, want a not-created error so the claim is released", err)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			if got != msg {
				t.Errorf("job message = %q, want the gate's own sentence %q", got, msg)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			if store.loads() != 0 {
				t.Errorf("the gate must refuse before any asset is loaded, got %d loads",
					store.loads())
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate")
			}
		})
	}

	// Each group's criteria mutate, keyed by the ad group it names.
	audienceOps := func(t *testing.T, cap *targetingCapture) []string {
		t.Helper()
		cap.mu.Lock()
		defer cap.mu.Unlock()
		var audiences []string
		for _, body := range cap.adGroupCriteria {
			var req struct {
				Operations []struct {
					Create struct {
						Audience *struct {
							Audience string `json:"audience"`
						} `json:"audience"`
					} `json:"create"`
				} `json:"operations"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, op := range req.Operations {
				if op.Create.Audience != nil {
					audiences = append(audiences, op.Create.Audience.Audience)
				}
			}
		}
		return audiences
	}

	t.Run("exactly true allows", func(t *testing.T) {
		t.Setenv(constants.EnvGoogleAdsDemandGenAudienceEnabled, "true")
		d, cap := gaWireDispatcher(t, gaWireLibrary(t), gateAssetBudget())
		cfg := demandGenGroupGateConfig(t, own, "")
		ctx := context.Background()
		if _, err := d.Dispatch(ctx, testBrief(), model.ProviderGoogleAds, cfg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if got := audienceOps(t, cap); !reflect.DeepEqual(got, []string{own}) {
			t.Errorf("audience criteria = %q, want the first group's Audience only", got)
		}
	})

	t.Run("a group without an audience is not gated", func(t *testing.T) {
		t.Setenv(constants.EnvGoogleAdsDemandGenAudienceEnabled, "")
		d, cap := gaWireDispatcher(t, gaWireLibrary(t), gateAssetBudget())
		cfg := demandGenGroupGateConfig(t, "", "")
		ctx := context.Background()
		if _, err := d.Dispatch(ctx, testBrief(), model.ProviderGoogleAds, cfg); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		cap.mu.Lock()
		groups := len(cap.adGroups)
		cap.mu.Unlock()
		if groups != 2 {
			t.Errorf("got %d ad groups, want both created", groups)
		}
		if got := audienceOps(t, cap); len(got) != 0 {
			t.Errorf("audience criteria = %q, want none", got)
		}
	})
}
