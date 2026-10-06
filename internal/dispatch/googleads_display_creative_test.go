// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

func fullDisplayCreativeConfig() *googleAdsDisplayCreativeConfig {
	return &googleAdsDisplayCreativeConfig{
		MarketingImages:       []string{signedCreativeURL},
		SquareMarketingImages: []string{"https://cdn.example.org/s.png?sig=square-secret"},
		LogoImages:            []string{"https://cdn.example.org/l.png?sig=logo-secret"},
		SquareLogoImages:      []string{"https://cdn.example.org/sl.png?sig=square-logo-secret"},
		Headlines:             []string{"Join us at KubeCon"},
		LongHeadline:          "KubeCon + CloudNativeCon Europe 2026",
		Descriptions:          []string{"Talks, workshops and hallway track."},
		BusinessName:          "Linux Foundation",
		CallToActionText:      "Register",
	}
}

// The responsive display ad has FOUR URL lists of its own, and they are neither Demand
// Gen's five nor Performance Max's five — there is no portrait slot here, and
// squareLogoImages exists on neither sibling. Listed individually for the reason the
// Demand Gen version gives: a helper applied to three of four passes any aggregate "no
// secrets anywhere" check whenever the fourth is empty in the fixture.
func TestGoogleAdsSnapshotConfig_SanitizesEveryDisplayURLList(t *testing.T) {
	cfg := googleAdsConfig{DisplayCreative: fullDisplayCreativeConfig()}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.DisplayCreative == nil {
		t.Fatal("the snapshot dropped the display creative entirely")
	}
	lists := map[string][]string{
		"marketingImages":       snapshot.DisplayCreative.MarketingImages,
		"squareMarketingImages": snapshot.DisplayCreative.SquareMarketingImages,
		"logoImages":            snapshot.DisplayCreative.LogoImages,
		"squareLogoImages":      snapshot.DisplayCreative.SquareLogoImages,
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

// The deep copy, again: the pointer and all four slices are shared with the caller's
// config, and the FULL url has to stay fetchable for the download that follows.
func TestGoogleAdsSnapshotConfig_DoesNotMutateTheCallersDisplayCreative(t *testing.T) {
	creative := fullDisplayCreativeConfig()
	cfg := googleAdsConfig{DisplayCreative: creative}

	snapshot := googleAdsSnapshotConfig(cfg)
	if creative.MarketingImages[0] != signedCreativeURL {
		t.Errorf("the caller's creative was mutated to %q; the signed url must still be fetchable", creative.MarketingImages[0])
	}
	if snapshot.DisplayCreative == creative {
		t.Error("the snapshot points at the caller's creative; a later edit to either would change both")
	}
	if strings.Contains(snapshot.DisplayCreative.MarketingImages[0], "s3cr3t") {
		t.Error("the snapshot creative was not sanitized")
	}
}

// The early return in googleAdsSnapshotConfig names SIX fields. A config carrying ONLY
// the display creative must not take it — the same bug the Performance Max condition was
// added to prevent, one channel later.
func TestGoogleAdsSnapshotConfig_SanitizesTheDisplayCreativeAlone(t *testing.T) {
	cfg := googleAdsConfig{DisplayCreative: &googleAdsDisplayCreativeConfig{
		MarketingImages: []string{signedCreativeURL},
	}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.DisplayCreative == nil {
		t.Fatal("a display creative with no sitelinks and no sibling creative was dropped")
	}
	if strings.Contains(snapshot.DisplayCreative.MarketingImages[0], "s3cr3t") {
		t.Errorf("not sanitized: %q", snapshot.DisplayCreative.MarketingImages[0])
	}
}

func TestGoogleAdsSnapshotConfig_NoDisplayCreativeStaysAbsent(t *testing.T) {
	if got := googleAdsSnapshotConfig(googleAdsConfig{}); got.DisplayCreative != nil {
		t.Errorf("DisplayCreative = %+v, want nil", got.DisplayCreative)
	}
}

// Each wire field must land in its own platform field. The text fields are the trap
// here: headlines, the long headline and descriptions are three DISTINCT asset field
// types, and the long headline is a SCALAR on this channel alone — a mapper that
// borrowed a sibling's list would still compile if it took the first element.
func TestGoogleAdsDisplayCreative_MapsEachFieldToItsOwnSlot(t *testing.T) {
	in := &googleAdsDisplayCreativeConfig{
		MarketingImages:       []string{"m"},
		SquareMarketingImages: []string{"s"},
		LogoImages:            []string{"l"},
		SquareLogoImages:      []string{"sl"},
		Headlines:             []string{"h"},
		LongHeadline:          "lh",
		Descriptions:          []string{"d"},
		BusinessName:          "LF",
		CallToActionText:      "Register",
	}
	got := googleAdsDisplayCreative(in)
	pairs := []struct {
		name      string
		got, want []string
	}{
		{"MarketingImages", got.MarketingImages, []string{"m"}},
		{"SquareMarketingImages", got.SquareMarketingImages, []string{"s"}},
		{"LogoImages", got.LogoImages, []string{"l"}},
		{"SquareLogoImages", got.SquareLogoImages, []string{"sl"}},
		{"Headlines", got.Headlines, []string{"h"}},
		{"Descriptions", got.Descriptions, []string{"d"}},
	}
	for _, p := range pairs {
		if len(p.got) != 1 || p.got[0] != p.want[0] {
			t.Errorf("%s = %v, want %v", p.name, p.got, p.want)
		}
	}
	if got.LongHeadline != "lh" || got.BusinessName != "LF" || got.CallToActionText != "Register" {
		t.Errorf("scalars did not map: %+v", got)
	}
}

// An absent creative maps to the zero value, which the platform layer reads as "no ad
// asked for" — the adoption case, where the ad was built by hand in the Google Ads UI
// and must not be refused.
func TestGoogleAdsDisplayCreative_NilIsTheZeroValue(t *testing.T) {
	var zero googleads.DisplayCreative
	if got := googleAdsDisplayCreative(nil); !reflect.DeepEqual(got, zero) {
		t.Errorf("nil config produced a non-zero creative: %+v", got)
	}
}
