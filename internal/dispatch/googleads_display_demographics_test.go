// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// #499: googleAdsConfig.displayDemographics must reach the Display ad group create (its
// targetingSetting) and the group's criteria mutate, with every one of the four lists mapped.
func TestGoogleAds_DisplayDemographicsReachTheDisplayAdGroup(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, "true")
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display","displayDemographics":{
		"ageRanges":["25-34"],"genders":["FEMALE"],"incomeRanges":["INCOME_RANGE_90_UP"],"parentalStatuses":["PARENT"]}}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	groups, criteria := cap.adGroups, cap.adGroupCriteria
	cap.mu.Unlock()

	// Google refuses a demographic dimension set bidOnly false, so no targetingSetting is sent.
	if len(groups) != 1 || strings.Contains(string(groups[0]), "targetingSetting") {
		t.Fatalf("ad group creates = %s, want no targetingSetting", groups)
	}
	if len(criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(criteria))
	}
	for _, c := range []string{
		`"status":"ENABLED","ageRange":{"type":"AGE_RANGE_25_34"}`,
		`"status":"ENABLED","gender":{"type":"FEMALE"}`,
		`"status":"ENABLED","incomeRange":{"type":"INCOME_RANGE_90_UP"}`,
		`"status":"ENABLED","parentalStatus":{"type":"PARENT"}`,
		// one complement negative per dimension, as a sample
		`"negative":true,"ageRange":{"type":"AGE_RANGE_18_24"}`,
		`"negative":true,"gender":{"type":"MALE"}`,
		`"negative":true,"incomeRange":{"type":"INCOME_RANGE_0_50"}`,
		`"negative":true,"parentalStatus":{"type":"NOT_A_PARENT"}`,
	} {
		if !strings.Contains(string(criteria[0]), c) {
			t.Errorf("criteria = %s, want %s", criteria[0], c)
		}
	}
}

// Off Display the field is refused before the budget, and so is a value both targeted and
// excluded on Display.
func TestGoogleAds_DisplayDemographicsRefusedBeforeAnyCreate(t *testing.T) {
	cases := []struct{ name, channel, extra, want string }{
		{"search", "search", "", "display demographic targeting is not supported"},
		{"demand gen", "demand-gen", "", "display demographic targeting is not supported"},
		{"video", "video", "", "display demographic targeting is not supported"},
		{"performance max", "performance-max", "", "display demographic targeting is not supported"},
		{"contradiction", "display", `,"excludedAgeRanges":["25-34"]`, "is both targeted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, "true")
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"` + tc.channel + `","displayDemographics":{"ageRanges":["25-34"]}` + tc.extra + `}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
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

// displayDemographics shares GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED with observedDemographics:
// anything but exactly "true" refuses any non-empty list, on any channel, before the budget.
// An object with every list empty is not a request for targeting and is not refused.
func TestGoogleAds_DisplayDemographicsGateRefusesBeforeAnyCreate(t *testing.T) {
	for _, env := range []string{unsetEnv, "", "false", "TRUE"} {
		for _, list := range []string{"ageRanges", "genders", "incomeRanges", "parentalStatuses"} {
			t.Run(list+"/"+env, func(t *testing.T) {
				setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, env)
				opts, cap := targetingServers(t)
				d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
				cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display","displayDemographics":{"` + list + `":["x"]}}}`)
				camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
				want := "displayDemographics is disabled on this deployment (" + constants.EnvGoogleAdsDemographicTargetingEnabled
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want the gate refusal containing %q", err, want)
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
	t.Run("empty object", func(t *testing.T) {
		setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, unsetEnv)
		opts, _ := targetingServers(t)
		d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
		cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display","displayDemographics":{}}}`)
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
			t.Fatalf("an empty displayDemographics must not be refused: %v", err)
		}
	})
}
