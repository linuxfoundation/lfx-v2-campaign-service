// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

func fullPMaxCreativeConfig() *googleAdsPerformanceMaxCreativeConfig {
	return &googleAdsPerformanceMaxCreativeConfig{
		MarketingImages:       []string{signedCreativeURL},
		SquareMarketingImages: []string{"https://cdn.example.org/s.png?sig=square-secret"},
		PortraitImages:        []string{"https://cdn.example.org/p.png?sig=portrait-secret"},
		LogoImages:            []string{"https://cdn.example.org/l.png?sig=logo-secret"},
		LandscapeLogoImages:   []string{"https://cdn.example.org/ll.png?sig=landscape-secret"},
		Headlines:             []string{"Join us at KubeCon", "Three days in Amsterdam", "Meet the maintainers"},
		LongHeadlines:         []string{"KubeCon + CloudNativeCon Europe 2026"},
		Descriptions:          []string{"Talks, workshops and hallway track.", "Register now and save."},
		BusinessName:          "Linux Foundation",
	}
}

// The Performance Max asset group has FIVE URL lists of its own, and they are not
// Demand Gen's five — portraitImages is shared, tallPortraitImages does not exist
// here, and landscapeLogoImages does not exist there. Listed individually for the
// reason the Demand Gen version gives: a helper applied to four of five passes any
// aggregate "no secrets anywhere" check whenever the fifth is empty in the fixture.
func TestGoogleAdsSnapshotConfig_SanitizesEveryPerformanceMaxURLList(t *testing.T) {
	cfg := googleAdsConfig{PerformanceMaxCreative: fullPMaxCreativeConfig()}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.PerformanceMaxCreative == nil {
		t.Fatal("the snapshot dropped the asset group entirely")
	}
	lists := map[string][]string{
		"marketingImages":       snapshot.PerformanceMaxCreative.MarketingImages,
		"squareMarketingImages": snapshot.PerformanceMaxCreative.SquareMarketingImages,
		"portraitImages":        snapshot.PerformanceMaxCreative.PortraitImages,
		"logoImages":            snapshot.PerformanceMaxCreative.LogoImages,
		"landscapeLogoImages":   snapshot.PerformanceMaxCreative.LandscapeLogoImages,
	}
	for name, got := range lists {
		if len(got) != 1 {
			t.Errorf("%s: snapshot has %d entries, want 1", name, len(got))
			continue
		}
		if strings.Contains(got[0], "secret") || strings.Contains(got[0], "s3cr3t") {
			t.Errorf("%s: snapshot still carries the signing query: %q", name, got[0])
		}
		if !strings.HasPrefix(got[0], "https://cdn.example.org") {
			t.Errorf("%s: snapshot lost the host: %q", name, got[0])
		}
	}
}

// The deep copy, again: the pointer and all five slices are shared with the caller's
// config, and the FULL url has to stay fetchable for the download that follows.
func TestGoogleAdsSnapshotConfig_DoesNotMutateTheCallersAssetGroup(t *testing.T) {
	creative := fullPMaxCreativeConfig()
	cfg := googleAdsConfig{PerformanceMaxCreative: creative}

	snapshot := googleAdsSnapshotConfig(cfg)
	if creative.MarketingImages[0] != signedCreativeURL {
		t.Errorf("the caller's asset group was mutated to %q; the signed url must still be fetchable", creative.MarketingImages[0])
	}
	if snapshot.PerformanceMaxCreative == creative {
		t.Error("the snapshot points at the caller's asset group; a later edit to either would change both")
	}
	if strings.Contains(snapshot.PerformanceMaxCreative.MarketingImages[0], "s3cr3t") {
		t.Error("the snapshot asset group was not sanitized")
	}
}

// The early return in googleAdsSnapshotConfig names three fields. A config carrying
// ONLY the asset group must not take it — that is precisely the bug the third
// condition was added to prevent.
func TestGoogleAdsSnapshotConfig_SanitizesTheAssetGroupAlone(t *testing.T) {
	cfg := googleAdsConfig{PerformanceMaxCreative: &googleAdsPerformanceMaxCreativeConfig{
		MarketingImages: []string{signedCreativeURL},
	}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.PerformanceMaxCreative == nil {
		t.Fatal("an asset group with no sitelinks and no Demand Gen creative was dropped")
	}
	if strings.Contains(snapshot.PerformanceMaxCreative.MarketingImages[0], "s3cr3t") {
		t.Errorf("not sanitized: %q", snapshot.PerformanceMaxCreative.MarketingImages[0])
	}
}

func TestGoogleAdsSnapshotConfig_NoAssetGroupStaysAbsent(t *testing.T) {
	if got := googleAdsSnapshotConfig(googleAdsConfig{}); got.PerformanceMaxCreative != nil {
		t.Errorf("PerformanceMaxCreative = %+v, want nil", got.PerformanceMaxCreative)
	}
}

// Each wire list must land in its own platform field. The three text lists are the
// trap here: headlines, longHeadlines and descriptions are three DISTINCT asset
// field types, so a mapper that merged any two would still produce a campaign that
// creates — with the wrong field type on assets Google renders differently.
func TestGoogleAdsPerformanceMaxCreative_MapsEachListToItsOwnSlot(t *testing.T) {
	in := &googleAdsPerformanceMaxCreativeConfig{
		MarketingImages:       []string{"m"},
		SquareMarketingImages: []string{"s"},
		PortraitImages:        []string{"p"},
		LogoImages:            []string{"l"},
		LandscapeLogoImages:   []string{"ll"},
		Headlines:             []string{"h"},
		LongHeadlines:         []string{"lh"},
		Descriptions:          []string{"d"},
		YouTubeVideoIDs:       []string{"v"},
		BusinessName:          "LF",
		AssetGroupName:        "Group",
		Path1:                 "events",
		Path2:                 "kubecon",
	}
	got := googleAdsPerformanceMaxCreative(in)
	pairs := []struct {
		name      string
		got, want []string
	}{
		{"MarketingImages", got.MarketingImages, []string{"m"}},
		{"SquareMarketingImages", got.SquareMarketingImages, []string{"s"}},
		{"PortraitImages", got.PortraitImages, []string{"p"}},
		{"LogoImages", got.LogoImages, []string{"l"}},
		{"LandscapeLogoImages", got.LandscapeLogoImages, []string{"ll"}},
		{"Headlines", got.Headlines, []string{"h"}},
		{"LongHeadlines", got.LongHeadlines, []string{"lh"}},
		{"Descriptions", got.Descriptions, []string{"d"}},
		{"YouTubeVideoIDs", got.YouTubeVideoIDs, []string{"v"}},
	}
	for _, p := range pairs {
		if len(p.got) != 1 || p.got[0] != p.want[0] {
			t.Errorf("%s = %v, want %v", p.name, p.got, p.want)
		}
	}
	if got.BusinessName != "LF" || got.AssetGroupName != "Group" || got.Path1 != "events" || got.Path2 != "kubecon" {
		t.Errorf("scalars did not map: %+v", got)
	}
}

// An absent asset group maps to the zero value, which the platform layer reads as
// "no asset group asked for" — the adoption case, where the group was built by hand
// in the Google Ads UI and must not be refused.
func TestGoogleAdsPerformanceMaxCreative_NilIsTheZeroValue(t *testing.T) {
	var zero googleads.PerformanceMaxCreative
	if got := googleAdsPerformanceMaxCreative(nil); !reflect.DeepEqual(got, zero) {
		t.Errorf("nil config produced a non-zero asset group: %+v", got)
	}
}
