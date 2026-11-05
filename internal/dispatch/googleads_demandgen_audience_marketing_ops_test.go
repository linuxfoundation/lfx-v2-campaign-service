// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// TestDemandGenAudienceDetectionAgreesWithTheCreateGate pins the create path's
// global_marketing_ops gate (service.GoogleAdsDemandGenAudienceRequested) to what this
// dispatcher treats as an audience. If the two disagreed, a config the dispatcher attaches an
// Audience for could pass the create path without the role check.
func TestDemandGenAudienceDetectionAgreesWithTheCreateGate(t *testing.T) {
	for _, raw := range []string{
		`{"googleAdsConfig":{"channel":"demand-gen"}}`,
		`{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`,
		`{"googleAdsConfig":{"demandGenAudience":" "}}`,
		`{"googleAdsConfig":{"DemandGenAudience":"customers/1/audiences/2"}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"}]}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"},{"name":"b","audience":"customers/1/audiences/3"}]}}`,
		`{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a","Audience":"\t"}]}}`,
		`{"microsoftConfig":{"budget":5}}`,
		`{"GoogleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`,
	} {
		var cfg googleAdsConfig
		if err := unmarshalPlatformConfig([]byte(raw), "googleAdsConfig", &cfg); err != nil {
			t.Fatalf("%s: decode: %v", raw, err)
		}
		dispatcher := cfg.DemandGenAudience != "" || googleAdsDemandGenAdGroupAudiencesSet(cfg) != ""
		if gate := service.GoogleAdsDemandGenAudienceRequested(json.RawMessage(raw)); gate != dispatcher {
			t.Errorf("%s: create gate sees an audience = %v, dispatcher = %v", raw, gate, dispatcher)
		}
	}
}
