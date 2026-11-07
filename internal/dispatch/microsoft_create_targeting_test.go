// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const msTargetingExtra = `,"proximityTargets":[{"latitude":34.05,"longitude":-118.24,"radius":25,"unit":"Miles","name":"LA"}],"languages":["japanese"]`

// Off (the default): any of the three is a synchronous 400 naming the flag, and Dispatch refuses
// it again before any Microsoft call. Empty lists and null are absent.
func TestMicrosoftCreateTargeting_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateTargetingEnabled, "")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for _, extra := range []string{`,"excludedGeoTargets":["PR"]`, `,"proximityTargets":[{"latitude":1,"longitude":1,"radius":1,"unit":"Miles"}]`, `,"languages":["English"]`, `,"budget":"x","languages":["English"]`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, constants.EnvMicrosoftCreateTargetingEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msTargetingExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftCreateTargetingEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"excludedGeoTargets":[]`, `,"proximityTargets":null,"languages":[]`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a rule breach is the synchronous 400 naming the field.
func TestMicrosoftCreateTargeting_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateTargetingEnabled, "true")
	d := newMSAudDispatcher(t, &msAudAPI{})
	for extra, want := range map[string]string{
		`,"excludedGeoTargets":"PR"`:                       "microsoftConfig.excludedGeoTargets: must be an array of strings",
		`,"LANGUAGES":[1]`:                                 "microsoftConfig.languages: must be an array of strings",
		`,"proximityTargets":[{"latitude":"1"}]`:           "microsoftConfig.proximityTargets: must be an array of objects",
		`,"geoTargets":["US"],"excludedGeoTargets":["us"]`: "microsoftConfig.excludedGeoTargets: entry 1 (US) is also in geoTargets",
		`,"excludedGeoTargets":["ZZ"]`:                     "microsoftConfig.excludedGeoTargets: geo target",
		`,"proximityTargets":[{"latitude":1,"longitude":1,"radius":501,"unit":"Miles"}]`: "microsoftConfig.proximityTargets: entry 1 radius must be a whole number from 1 to 500 miles",
		`,"languages":["English","All"]`:                                                 "microsoftConfig.languages: All targets every language",
		`,"budget":"x","languages":["Klingon"]`:                                          "microsoftConfig.languages: entry 1 is not",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msTargetingExtra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the values reach the client: the campaign carries the languages, the radius is attached
// before the ad group, and the ad group carries no Language.
func TestMicrosoftCreateTargeting_DispatchSendsThem(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftCreateTargetingEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msTargetingExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	calls := api.snapshot()
	ci, ag := slices.Index(calls, "CampaignCriterions"), slices.Index(calls, "AdGroups")
	if ci < 0 || ag < 0 || ci > ag {
		t.Fatalf("calls = %v, want the radius criterion before the ad group", calls)
	}
	api.mu.Lock()
	campaignBody, critBody, adGroupBody := api.bodies[slices.Index(calls, "Campaigns")], api.bodies[ci], api.bodies[ag]
	api.mu.Unlock()
	if !strings.Contains(campaignBody, `"Languages":["Japanese"]`) {
		t.Errorf("campaign body = %s", campaignBody)
	}
	if !strings.Contains(critBody, `"Type":"RadiusCriterion"`) || !strings.Contains(critBody, `"RadiusUnit":"Miles"`) {
		t.Errorf("criterion body = %s", critBody)
	}
	if strings.Contains(adGroupBody, `"Language"`) {
		t.Errorf("ad group body = %s", adGroupBody)
	}
}
