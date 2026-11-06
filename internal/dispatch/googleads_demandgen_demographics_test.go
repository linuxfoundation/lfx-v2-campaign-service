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

// #498: googleAdsConfig.demandGenDemographics must reach the Audience create, and the Audience
// must reach the Demand Gen ad group. The client's tests pin the payloads from a
// CampaignInput; this pins that every wire field is mapped onto it.
func TestGoogleAds_DemandGenDemographicsReachTheAudienceAndTheAdGroup(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, "true")
	setGateEnv(t, constants.EnvGoogleAdsDemandGenAudienceEnabled, "true")
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","demandGenDemographics":{` +
		`"ageRanges":["25-34"],"genders":["FEMALE"],"incomeRanges":["INCOME_RANGE_90_UP"],` +
		`"parentalStatuses":["PARENT"],"audienceSegments":["customers/1234567890/userLists/55"]}}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	audiences, criteria := cap.audiences, cap.adGroupCriteria
	cap.mu.Unlock()

	if len(audiences) != 1 {
		t.Fatalf("got %d audiences:mutate, want 1", len(audiences))
	}
	for _, want := range []string{
		`{"age":{"ageRanges":[{"minAge":25,"maxAge":34}],"includeUndetermined":false}}`,
		`{"gender":{"genders":["FEMALE"],"includeUndetermined":false}}`,
		`{"householdIncome":{"incomeRanges":["INCOME_RANGE_90_UP"],"includeUndetermined":false}}`,
		`{"parentalStatus":{"parentalStatuses":["PARENT"],"includeUndetermined":false}}`,
		`{"audienceSegments":{"segments":[{"userList":{"userList":"customers/1234567890/userLists/55"}}]}}`,
	} {
		if !strings.Contains(string(audiences[0]), want) {
			t.Errorf("audience create = %s, want it to carry %s", audiences[0], want)
		}
	}
	attached := false
	for _, body := range criteria {
		if strings.Contains(string(body), `"audience":{"audience":"customers/1234567890/audiences/777"}`) {
			attached = true
		}
	}
	if !attached {
		t.Errorf("adGroupCriteria = %s, want the created Audience attached", criteria)
	}
}

// The field is behind GOOGLE_ADS_DEMOGRAPHIC_TARGETING_ENABLED: anything but exactly "true"
// refuses it, `{}` included, before the Audience or the budget.
func TestGoogleAds_DemandGenDemographicsGateRefusesBeforeAnyCreate(t *testing.T) {
	for _, env := range []string{unsetEnv, "", "false", "TRUE", " true"} {
		for _, demographics := range []string{`{"ageRanges":["25-34"]}`, `{}`} {
			t.Run(env+"/"+demographics, func(t *testing.T) {
				setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, env)
				setGateEnv(t, constants.EnvGoogleAdsDemandGenAudienceEnabled, "true")
				opts, cap := targetingServers(t)
				d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
				cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","demandGenDemographics":` + demographics + `}}`)
				camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
				want := "demandGenDemographics is disabled on this deployment (" + constants.EnvGoogleAdsDemographicTargetingEnabled
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want the gate refusal containing %q", err, want)
				}
				assertNothingCreated(t, cap, camp)
			})
		}
	}
}

// Its user lists are existing account resources, so they also need
// GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED, as demandGenAudience does. Demographics alone do not.
func TestGoogleAds_DemandGenDemographicSegmentsNeedTheAudienceGate(t *testing.T) {
	for _, env := range []string{unsetEnv, "false", "TRUE"} {
		t.Run(env, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, "true")
			setGateEnv(t, constants.EnvGoogleAdsDemandGenAudienceEnabled, env)
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","demandGenDemographics":` +
				`{"ageRanges":["25-34"],"audienceSegments":["customers/1234567890/userLists/55"]}}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			want := "demandGenDemographics.audienceSegments is disabled on this deployment (" + constants.EnvGoogleAdsDemandGenAudienceEnabled
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want the gate refusal containing %q", err, want)
			}
			assertNothingCreated(t, cap, camp)

			// The same create without the user lists is not gated by it.
			opts, _ = targetingServers(t)
			d = NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg = json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","demandGenDemographics":{"ageRanges":["25-34"]}}}`)
			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
				t.Fatalf("demographics without user lists must not need %s: %v", constants.EnvGoogleAdsDemandGenAudienceEnabled, err)
			}
		})
	}
}

// Off Demand Gen the field is the client's pre-flight refusal, before the Audience or budget.
func TestGoogleAds_DemandGenDemographicsAreRefusedOffDemandGen(t *testing.T) {
	for _, channel := range []string{"search", "display", "video", "performance-max"} {
		t.Run(channel, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsDemographicTargetingEnabled, "true")
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"` + channel + `","demandGenDemographics":{"ageRanges":["25-34"]}}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil || !strings.Contains(err.Error(), "Demand Gen demographics are not supported") {
				t.Fatalf("err = %v, want the Demand Gen demographics refusal", err)
			}
			assertNothingCreated(t, cap, camp)
		})
	}
}

func assertNothingCreated(t *testing.T, cap *targetingCapture, camp *model.Campaign) {
	t.Helper()
	if camp != nil {
		t.Errorf("nothing may be created for a refused input, got %+v", camp)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("the refusal must happen BEFORE the budget mutate")
	}
	if len(cap.audiences) > 0 {
		t.Error("the refusal must happen BEFORE the Audience create")
	}
}
