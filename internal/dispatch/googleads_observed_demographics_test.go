// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// #497: googleAdsConfig.observedDemographics must reach the Search ad group create (its
// targetingSetting) and the group's criteria mutate. The client's tests pin the payloads
// from a CampaignInput; this pins that the wire field is mapped onto it at all.
func TestGoogleAds_ObservedDemographicsReachTheSearchAdGroup(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"observedDemographics":["GENDER"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	groups, criteria := cap.adGroups, cap.adGroupCriteria
	cap.mu.Unlock()

	if len(groups) != 1 || !strings.Contains(string(groups[0]), `"targetRestrictions":[{"targetingDimension":"GENDER","bidOnly":true}]`) {
		t.Fatalf("ad group creates = %s, want GENDER declared bidOnly", groups)
	}
	if len(criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(criteria))
	}
	for _, g := range []string{"MALE", "FEMALE", "UNDETERMINED"} {
		if !strings.Contains(string(criteria[0]), `"status":"ENABLED","gender":{"type":"`+g+`"}`) {
			t.Errorf("criteria = %s, want a positive ENABLED gender %s", criteria[0], g)
		}
	}
}

// Off Search the field is refused before the budget.
func TestGoogleAds_ObservedDemographicsAreRefusedOffSearchBeforeAnyCreate(t *testing.T) {
	for _, channel := range []string{"display", "demand-gen", "video", "performance-max"} {
		t.Run(channel, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"` + channel + `","observedDemographics":["AGE_RANGE"]}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), "observed demographics are not supported") {
				t.Fatalf("err = %v, want the observed-demographics refusal", err)
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
