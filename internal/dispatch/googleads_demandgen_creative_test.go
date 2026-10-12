// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// signedURL is the realistic case for a creative image: the query string is itself
// the credential that fetches the asset, and config_snapshot is persisted
// UNENCRYPTED, so it must not reach the row.
const signedCreativeURL = "https://cdn.example.org/m.png?X-Amz-Signature=s3cr3t-asset-key"

func fullCreativeConfig() *googleAdsDemandGenCreativeConfig {
	return &googleAdsDemandGenCreativeConfig{
		MarketingImages:       []string{signedCreativeURL},
		SquareMarketingImages: []string{"https://cdn.example.org/s.png?sig=square-secret"},
		PortraitImages:        []string{"https://cdn.example.org/p.png?sig=portrait-secret"},
		TallPortraitImages:    []string{"https://cdn.example.org/t.png?sig=tall-secret"},
		LogoImages:            []string{"https://cdn.example.org/l.png?sig=logo-secret"},
		Headlines:             []string{"Join us at KubeCon"},
		Descriptions:          []string{"Three days of talks."},
		BusinessName:          "Linux Foundation",
		CallToActionText:      "Register",
	}
}

// Every one of the five URL lists is sanitized. Listed individually rather than
// asserted in aggregate: a helper applied to four of five lists passes any check
// that only looks for "no secrets anywhere" as long as the fifth list happens to be
// empty in the fixture, which is exactly how the fifth list gets missed.
func TestGoogleAdsSnapshotConfig_SanitizesEveryCreativeURLList(t *testing.T) {
	cfg := googleAdsConfig{DemandGenCreative: fullCreativeConfig()}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.DemandGenCreative == nil {
		t.Fatal("the snapshot dropped the creative entirely")
	}
	lists := map[string][]string{
		"marketingImages":       snapshot.DemandGenCreative.MarketingImages,
		"squareMarketingImages": snapshot.DemandGenCreative.SquareMarketingImages,
		"portraitImages":        snapshot.DemandGenCreative.PortraitImages,
		"tallPortraitImages":    snapshot.DemandGenCreative.TallPortraitImages,
		"logoImages":            snapshot.DemandGenCreative.LogoImages,
	}
	for name, got := range lists {
		if len(got) != 1 {
			t.Errorf("%s: snapshot has %d entries, want 1", name, len(got))
			continue
		}
		if strings.Contains(got[0], "secret") || strings.Contains(got[0], "s3cr3t") {
			t.Errorf("%s: snapshot still carries the signing query: %q", name, got[0])
		}
		// Scheme and host survive, or the persisted row stops being readable at all
		// — the same balance sanitizeSnapshotURL strikes for every sibling.
		if !strings.HasPrefix(got[0], "https://cdn.example.org") {
			t.Errorf("%s: snapshot lost the host: %q", name, got[0])
		}
	}
}

// The deep copy is the point: cfg is passed by value but its slices share backing
// arrays with the caller's, so sanitizing in place would redact the URLs the fetch
// is about to download from. Same failure the sitelink copy guards against.
func TestGoogleAdsSnapshotConfig_DoesNotMutateTheCallersCreative(t *testing.T) {
	creative := fullCreativeConfig()
	cfg := googleAdsConfig{DemandGenCreative: creative}

	snapshot := googleAdsSnapshotConfig(cfg)
	if creative.MarketingImages[0] != signedCreativeURL {
		t.Errorf("the caller's creative was mutated to %q; the signed url must still be fetchable", creative.MarketingImages[0])
	}
	if snapshot.DemandGenCreative == creative {
		t.Error("the snapshot points at the caller's creative; a later edit to either would change both")
	}
	if strings.Contains(snapshot.DemandGenCreative.MarketingImages[0], "s3cr3t") {
		t.Error("the snapshot creative was not sanitized")
	}
}

// Sitelinks and the creative are sanitized independently, so a config carrying only
// one of them must still be sanitized — and the guard that skips the whole copy when
// both are absent must not skip it when only one is present.
func TestGoogleAdsSnapshotConfig_SanitizesTheCreativeWithNoSitelinks(t *testing.T) {
	cfg := googleAdsConfig{DemandGenCreative: &googleAdsDemandGenCreativeConfig{
		MarketingImages: []string{signedCreativeURL},
	}}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.DemandGenCreative == nil {
		t.Fatal("a creative with no sitelinks was dropped from the snapshot")
	}
	if strings.Contains(snapshot.DemandGenCreative.MarketingImages[0], "s3cr3t") {
		t.Errorf("not sanitized: %q", snapshot.DemandGenCreative.MarketingImages[0])
	}
}

// No creative means no creative: the snapshot must not invent an empty one, because
// a persisted empty object and an absent one say different things about what the
// operator asked for.
func TestGoogleAdsSnapshotConfig_NoCreativeStaysAbsent(t *testing.T) {
	if got := googleAdsSnapshotConfig(googleAdsConfig{}); got.DemandGenCreative != nil {
		t.Errorf("DemandGenCreative = %+v, want nil", got.DemandGenCreative)
	}
}

// The mapper must land each wire list in its own platform field. A mapper that
// crossed portrait and tall-portrait would pass any test that only counted images:
// both lists are non-empty, the totals match, and the campaign would be built with
// 4:5 images where Google wants 9:16.
func TestGoogleAdsDemandGenCreative_MapsEachListToItsOwnSlot(t *testing.T) {
	in := &googleAdsDemandGenCreativeConfig{
		MarketingImages:       []string{"m"},
		SquareMarketingImages: []string{"s"},
		PortraitImages:        []string{"p"},
		TallPortraitImages:    []string{"t"},
		LogoImages:            []string{"l"},
		Headlines:             []string{"h"},
		Descriptions:          []string{"d"},
		BusinessName:          "LF",
		CallToActionText:      "Register",
	}
	got := googleAdsDemandGenCreative(in)
	pairs := []struct {
		name      string
		got, want []string
	}{
		{"MarketingImages", got.MarketingImages, []string{"m"}},
		{"SquareMarketingImages", got.SquareMarketingImages, []string{"s"}},
		{"PortraitImages", got.PortraitImages, []string{"p"}},
		{"TallPortraitImages", got.TallPortraitImages, []string{"t"}},
		{"LogoImages", got.LogoImages, []string{"l"}},
		{"Headlines", got.Headlines, []string{"h"}},
		{"Descriptions", got.Descriptions, []string{"d"}},
	}
	for _, p := range pairs {
		if len(p.got) != 1 || p.got[0] != p.want[0] {
			t.Errorf("%s = %v, want %v", p.name, p.got, p.want)
		}
	}
	if got.BusinessName != "LF" || got.CallToActionText != "Register" {
		t.Errorf("scalars did not map: %+v", got)
	}
}

// An absent creative maps to the zero value, which is what the platform layer reads
// as "no creative asked for" — the pre-existing no-ad behaviour every Demand Gen
// campaign created before this feature relies on.
func TestGoogleAdsDemandGenCreative_NilIsTheZeroValue(t *testing.T) {
	var zero googleads.DemandGenCreative
	if got := googleAdsDemandGenCreative(nil); !reflect.DeepEqual(got, zero) {
		t.Errorf("nil config produced a non-zero creative: %+v", got)
	}
}
