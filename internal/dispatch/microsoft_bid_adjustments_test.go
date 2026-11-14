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

const msBidAdjExtra = `,"deviceBidAdjustments":{"tablets":-100}`

// Off (the default): any of the three is a synchronous 400 naming the flag, and Dispatch refuses
// it again before any Microsoft call. An empty object and null are absent.
func TestMicrosoftBidAdjustments_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftBidAdjustmentsEnabled, "")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for _, extra := range []string{msBidAdjExtra, `,"ageBidAdjustments":{"Nope":1}`, `,"budget":"x","genderBidAdjustments":{"Male":5}`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, constants.EnvMicrosoftBidAdjustmentsEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msBidAdjExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftBidAdjustmentsEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"deviceBidAdjustments":{}`, `,"ageBidAdjustments":null,"genderBidAdjustments":{}`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type or a rule breach is the synchronous 400 naming the field, and Dispatch refuses
// it with no Microsoft call.
func TestMicrosoftBidAdjustments_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftBidAdjustmentsEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for extra, want := range map[string]string{
		`,"deviceBidAdjustments":[-100]`:                              "microsoftConfig.deviceBidAdjustments: must be an object of segment names to numbers",
		`,"AGEBIDADJUSTMENTS":{"EighteenToTwentyFour":"5"}`:           "microsoftConfig.ageBidAdjustments: must be an object",
		`,"genderBidAdjustments":"Male"`:                              "microsoftConfig.genderBidAdjustments: must be an object",
		`,"deviceBidAdjustments":{"TV":5}`:                            "microsoftConfig.deviceBidAdjustments: unknown key",
		`,"genderBidAdjustments":{"Male":5,"MALE":6}`:                 "microsoftConfig.genderBidAdjustments: Male is named twice",
		`,"budget":"x","ageBidAdjustments":{"FiftyToSixtyFour":-100}`: "microsoftConfig.ageBidAdjustments: FiftyToSixtyFour must be a whole percent from -90 to 900",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
		_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(extra))
		requirePreCreate(t, derr)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls for a refused adjustment; want 0", n)
	}
}

// On: the adjustment reaches the client and is attached before the ad group.
func TestMicrosoftBidAdjustments_DispatchSendsThem(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftBidAdjustmentsEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msBidAdjExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	calls := api.snapshot()
	ci, ag := slices.Index(calls, "CampaignCriterions"), slices.Index(calls, "AdGroups")
	if ci < 0 || ag < 0 || ci > ag {
		t.Fatalf("calls = %v, want the adjustment before the ad group", calls)
	}
	api.mu.Lock()
	body := api.bodies[ci]
	api.mu.Unlock()
	if !strings.Contains(body, `"Criterion":{"Type":"DeviceCriterion","DeviceName":"Tablets"}`) || !strings.Contains(body, `"Multiplier":-100`) {
		t.Errorf("criterion body = %s", body)
	}
	if !strings.Contains(string(camp.Result), `"bidAdjustmentCriterionIds":["8001"]`) {
		t.Errorf("result = %s", camp.Result)
	}
}
