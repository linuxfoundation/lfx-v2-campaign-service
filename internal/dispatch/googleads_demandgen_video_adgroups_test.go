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

// Every #460 refusal reaches the caller through Dispatch BEFORE the budget mutate.
func TestGoogleAds_DemandGenVideoAndAdGroupRefusalsArePreCreate(t *testing.T) {
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
		{"foreign group audience", `"channel":"demand-gen","demandGenAdGroups":[{"name":"A","audience":"customers/9999999999/audiences/4","videoCreative":` + video[len(`"demandGenVideoCreative":`):] + `}]`, "belongs to customer 9999999999"},
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
