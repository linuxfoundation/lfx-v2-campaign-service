// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
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
	if cfg.PerformanceMaxAudienceSignal != "" {
		set = append(set, "performanceMaxAudienceSignal")
	}
	if len(cfg.SharedNegativeKeywordLists) > 0 {
		set = append(set, "sharedNegativeKeywordLists")
	}
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
