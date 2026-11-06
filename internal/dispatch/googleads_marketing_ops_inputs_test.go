// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// dispatcherSharedAccountInputs is what this dispatcher treats as an input naming a resource on
// the shared Google Ads account, built from its own deployment gates: the Demand Gen audience
// gate, googleAdsDisplayAudienceFieldsSet, the Performance Max audience signal gate and the
// shared negative list gate. Plus Search's observation-only audienceSegments (campaign level off
// Display, and each adGroups entry), which no flag gates but which name the shared account's
// user lists.
func dispatcherSharedAccountInputs(cfg googleAdsConfig) []string {
	var set []string
	if cfg.DemandGenAudience != "" {
		set = append(set, "demandGenAudience")
	}
	if googleAdsDemandGenAdGroupAudiencesSet(cfg) != "" {
		for i, g := range cfg.DemandGenAdGroups {
			if g.Audience != "" {
				set = append(set, "demandGenAdGroups entry "+strconv.Itoa(i+1)+" audience")
			}
		}
	}
	set = append(set, googleAdsDisplayAudienceFieldsSet(cfg)...)
	if len(cfg.AudienceSegments) > 0 &&
		strings.ToLower(strings.TrimSpace(cfg.Channel)) != googleAdsChannelDisplay {
		set = append(set, "audienceSegments")
	}
	for i, g := range cfg.AdGroups {
		if len(g.AudienceSegments) > 0 {
			set = append(set, "adGroups entry "+strconv.Itoa(i+1)+" audienceSegments")
		}
	}
	if len(cfg.CampaignAudienceSegments) > 0 {
		set = append(set, "campaignAudienceSegments")
	}
	if cfg.PerformanceMaxAudienceSignal != "" {
		set = append(set, "performanceMaxAudienceSignal")
	}
	if len(cfg.SharedNegativeKeywordLists) > 0 {
		set = append(set, "sharedNegativeKeywordLists")
	}
	if len(cfg.ConversionActions) > 0 {
		set = append(set, "conversionActions")
	}
	set = append(set, googleAdsDemandGenDemographicSegmentsSet(cfg)...)
	return set
}

// TestSharedAccountInputDetectionAgreesWithTheCreateGate pins the create path's
// global_marketing_ops gate (service.GoogleAdsMarketingOpsInputs) to what this dispatcher's own
// gates detect, field for field. If the two disagreed, a config the dispatcher attaches a
// shared-account resource for could pass the create path without the role check.
func TestSharedAccountInputDetectionAgreesWithTheCreateGate(t *testing.T) {
	for _, raw := range []string{
		`{"googleAdsConfig":{"channel":"demand-gen"}}`,
		`{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`,
		`{"googleAdsConfig":{"demandGenAudience":" "}}`,
		`{"googleAdsConfig":{"DemandGenAudience":"customers/1/audiences/2"}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"}]}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"},{"name":"b","audience":"customers/1/audiences/3"}]}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a","Audience":"\t"}]}}`,
		`{"googleAdsConfig":{"channel":"display","audienceSegments":["a"],"customAudiences":["b"],"customAffinities":["c"],"excludedAudienceSegments":["d"],"userInterests":["e"],"excludedPlacements":["example.com"]}}`,
		`{"googleAdsConfig":{"channel":" Display ","audienceSegments":["a"]}}`,
		`{"googleAdsConfig":{"channel":"search","customAudiences":["b"]}}`,
		`{"googleAdsConfig":{"customAudiences":[]}}`,
		`{"googleAdsConfig":{"customAffinities":[""]}}`,
		`{"googleAdsConfig":{"displayAdGroups":[{"name":"a","userInterests":["u"]},{"name":"b","audienceSegments":["s"],"customAudiences":["c"],"customAffinities":["f"],"excludedAudienceSegments":["e"]}]}}`,
		`{"googleAdsConfig":{"channel":"performance-max","performanceMaxAudienceSignal":"123","performanceMaxSearchThemes":["k8s"]}}`,
		`{"googleAdsConfig":{"performanceMaxAudienceSignal":" "}}`,
		`{"googleAdsConfig":{"channel":"search","sharedNegativeKeywordLists":["customers/1/sharedSets/2"]}}`,
		`{"googleAdsConfig":{"sharedNegativeKeywordLists":[]}}`,
		`{"googleAdsConfig":{"channel":"search","audienceSegments":["customers/1/userLists/2"]}}`,
		`{"googleAdsConfig":{"audienceSegments":["customers/1/userLists/2"]}}`,
		`{"googleAdsConfig":{"channel":"search","adGroups":[{"name":"a"},{"name":"b","audienceSegments":["customers/1/userLists/2"]}]}}`,
		`{"googleAdsConfig":{"sharedNegativeKeywordLists":["s"],"performanceMaxAudienceSignal":"1","demandGenAudience":"a","customAudiences":["c"],"displayAdGroups":[{"excludedAudienceSegments":["x"]}]}}`,
		// A displayAdGroups entry on a non-Display channel, beside Demand Gen campaign segments.
		`{"googleAdsConfig":{"channel":"demand-gen","audienceSegments":["a"],"displayAdGroups":[{"customAffinities":["f"]}]}}`,
		`{"googleAdsConfig":{"channel":"video","audienceSegments":["a"]}}`,
		// encoding/json matches nested keys case-insensitively too.
		`{"googleAdsConfig":{"DisplayAdGroups":[{"CustomAudiences":["c"]}],"AdGroups":[{"AudienceSegments":["s"]}],"PerformanceMaxAudienceSignal":"1"}}`,
		`{"googleAdsConfig":{"channel":"search","conversionActions":["customers/1/conversionActions/2"]}}`,
		`{"googleAdsConfig":{"channel":"demand-gen","conversionActions":["123"],"demandGenAudience":"a"}}`,
		`{"googleAdsConfig":{"conversionActions":[]}}`,
		`{"googleAdsConfig":{"observedDemographics":["AGE_RANGE"]}}`,
		// #498: the demographic Audience counts only when it names existing user lists.
		`{"googleAdsConfig":{"demandGenDemographics":{"ageRanges":["25-34"]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":null}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":[]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":["customers/1/userLists/2"]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":[""]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"AudienceSegments":["customers/1/userLists/2"]}}}`,
		// #516: its custom audiences count on the same terms, alone or beside user lists.
		`{"googleAdsConfig":{"demandGenDemographics":{"customAudiences":["customers/1/customAudiences/2"]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"customAudiences":[]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"customAudiences":[" "]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"CustomAudiences":["x"]}}}`,
		`{"googleAdsConfig":{"demandGenDemographics":{"ageRanges":["25-34"],"audienceSegments":["a"],"customAudiences":["b"]}}}`,
		`{"microsoftConfig":{"budget":5}}`,
		`{"GoogleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`,
	} {
		var cfg googleAdsConfig
		if err := unmarshalPlatformConfig([]byte(raw), "googleAdsConfig", &cfg); err != nil {
			t.Fatalf("%s: decode: %v", raw, err)
		}
		dispatcher := dispatcherSharedAccountInputs(cfg)
		gate := service.GoogleAdsMarketingOpsInputs(json.RawMessage(raw))
		slices.Sort(dispatcher)
		slices.Sort(gate)
		if !slices.Equal(gate, dispatcher) {
			t.Errorf("%s: create gate names %q, dispatcher detects %q", raw, gate, dispatcher)
		}
	}
}

// sharedAccountGatedFields are the googleAdsConfig JSON paths that name an existing audience,
// user list, custom segment, shared negative list or conversion action on the shared Google Ads
// account, and so
// require global_marketing_ops at create. "group[]" marks a field of each entry of that list.
var sharedAccountGatedFields = map[string]bool{
	"demandGenAudience":                          true,
	"demandGenAdGroups[].audience":               true,
	"audienceSegments":                           true,
	"customAudiences":                            true,
	"customAffinities":                           true,
	"excludedAudienceSegments":                   true,
	"adGroups[].audienceSegments":                true,
	"displayAdGroups[].audienceSegments":         true,
	"displayAdGroups[].customAudiences":          true,
	"displayAdGroups[].customAffinities":         true,
	"displayAdGroups[].excludedAudienceSegments": true,
	"performanceMaxAudienceSignal":               true,
	"sharedNegativeKeywordLists":                 true,
	"conversionActions":                          true,
	"demandGenDemographics.audienceSegments":     true,
	"demandGenDemographics.customAudiences":      true,
	"campaignAudienceSegments":                   true,
}

// sharedAccountUngatedFields are every other googleAdsConfig JSON path, each deliberately
// outside the check. Most name no existing account resource: the caller's own text, URLs,
// images, bids and dates, Google's own reference data (geo targets, languages, userInterests,
// the fixed demographic brackets of observedDemographics and displayDemographics), or public domains
// (excludedPlacements).
var sharedAccountUngatedFields = map[string]bool{
	"budget": true, "channel": true, "headlines": true, "descriptions": true, "path1": true,
	"path2": true, "keywords": true, "userInterests": true, "excludedPlacements": true,
	"negativeKeywords": true, "searchPartners": true, "cpcBid": true, "biddingStrategy": true,
	"targetCpa": true, "targetRoas": true, "startDate": true,
	"endDate": true, "geoTargets": true, "excludedGeoTargets": true, "proximityTargets": true,
	"languages": true, "adSchedules": true, "deviceBidModifiers": true, "excludedAgeRanges": true,
	"excludedGenders": true, "excludedIncomeRanges": true, "excludedParentalStatuses": true,
	"sitelinks": true, "callouts": true, "structuredSnippets": true, "callExtensions": true,
	"promotions": true, "prices": true, "leadForms": true, "demandGenCreative": true,
	"demandGenVideoCreative": true, "demandGenAds": true, "performanceMaxCreative": true,
	"performanceMaxSearchThemes": true, "videoCreative": true, "displayCreative": true,
	"searchAssets": true, "adoptExisting": true, "observedDemographics": true,
	// Google's fixed demographic brackets (#499, #498); demandGenDemographics' user lists are
	// gated above.
	"displayDemographics":                    true,
	"demandGenDemographics.ageRanges":        true,
	"demandGenDemographics.genders":          true,
	"demandGenDemographics.incomeRanges":     true,
	"demandGenDemographics.parentalStatuses": true,
	"adGroups[].name":                        true, "adGroups[].cpcBid": true, "adGroups[].keywords": true,
	"adGroups[].ads": true, "adGroups[].negativeKeywords": true,
	"displayAdGroups[].name": true, "displayAdGroups[].creative": true,
	"displayAdGroups[].userInterests": true,
	"demandGenAdGroups[].name":        true, "demandGenAdGroups[].creative": true,
	"demandGenAdGroups[].videoCreative": true, "demandGenAdGroups[].ads": true,
}

// googleAdsConfigFieldPaths lists every JSON path of googleAdsConfig, descending into the three
// ad-group lists, with each field's type.
func googleAdsConfigFieldPaths() map[string]reflect.Type {
	paths := map[string]reflect.Type{}
	groups := map[string]bool{"adGroups": true, "displayAdGroups": true, "demandGenAdGroups": true}
	var walk func(prefix string, t reflect.Type)
	walk = func(prefix string, t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			if prefix == "" && groups[name] {
				walk(name+"[].", f.Type.Elem())
				continue
			}
			// demandGenDemographics (#498) mixes gated and ungated lists, so its fields are
			// classified one by one.
			if prefix == "" && name == "demandGenDemographics" {
				walk(name+".", f.Type.Elem())
				continue
			}
			paths[prefix+name] = f.Type
		}
	}
	walk("", reflect.TypeOf(googleAdsConfig{}))
	return paths
}

// TestEveryGoogleAdsConfigFieldIsClassifiedForTheMarketingOpsGate fails on any googleAdsConfig
// field (including a new one, or one on an ad-group entry) that is neither gated nor
// deliberately ungated, so a new shared-account input cannot slip past the create gate
// unclassified. Every gated field is then sent on its own, and both the create gate and the
// dispatcher's detection must name it.
func TestEveryGoogleAdsConfigFieldIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	paths := googleAdsConfigFieldPaths()
	for p := range paths {
		if sharedAccountGatedFields[p] == sharedAccountUngatedFields[p] {
			t.Errorf("googleAdsConfig field %q must be in exactly one of sharedAccountGatedFields "+
				"and sharedAccountUngatedFields", p)
		}
	}
	for p := range sharedAccountGatedFields {
		if _, ok := paths[p]; !ok {
			t.Errorf("sharedAccountGatedFields names %q, which googleAdsConfig no longer has", p)
		}
	}
	for p := range sharedAccountUngatedFields {
		if _, ok := paths[p]; !ok {
			t.Errorf("sharedAccountUngatedFields names %q, which googleAdsConfig no longer has", p)
		}
	}
	for p := range sharedAccountGatedFields {
		typ, ok := paths[p]
		if !ok {
			continue
		}
		var value any = " "
		if typ.Kind() == reflect.Slice {
			value = []string{"x"}
		}
		field := p
		cfg := map[string]any{"channel": "display"}
		if group, sub, ok := strings.Cut(p, "[]."); ok {
			field = group
			value = []any{map[string]any{sub: value}}
		} else if group, sub, ok := strings.Cut(p, "."); ok {
			field = group
			value = map[string]any{sub: value}
		}
		cfg[field] = value
		raw, err := json.Marshal(map[string]any{"googleAdsConfig": cfg})
		if err != nil {
			t.Fatal(err)
		}
		var decoded googleAdsConfig
		if err := unmarshalPlatformConfig(raw, "googleAdsConfig", &decoded); err != nil {
			t.Fatalf("%s: decode: %v", raw, err)
		}
		if got := dispatcherSharedAccountInputs(decoded); len(got) != 1 {
			t.Errorf("%s: dispatcher detects %q, want exactly one field", raw, got)
		}
		if got := service.GoogleAdsMarketingOpsInputs(raw); len(got) != 1 {
			t.Errorf("%s: create gate names %q, want exactly one field", raw, got)
		}
	}
}
