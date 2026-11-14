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

const msScheduleExtra = `,"adSchedules":[{"day":"monday","startHour":9,"endHour":17,"bidAdjustment":20}]`

// Off (the default): adSchedules is a synchronous 400 naming the flag, and Dispatch refuses it
// again before any Microsoft call. An empty list and null are absent.
func TestMicrosoftAdSchedule_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdScheduleEnabled, "")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for _, extra := range []string{msScheduleExtra, `,"budget":"x"` + msScheduleExtra, `,"adSchedules":[{"day":"Funday"}]`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, constants.EnvMicrosoftAdScheduleEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msScheduleExtra))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftAdScheduleEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"adSchedules":[]`, `,"adSchedules":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// On: a wrong type, a missing or fractional hour, or a rule breach is the synchronous 400 naming
// the field, and Dispatch refuses it with no Microsoft call.
func TestMicrosoftAdSchedule_PreflightRefusesBadInput(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdScheduleEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for extra, want := range map[string]string{
		`,"adSchedules":{"day":"Monday"}`:                                                                           "microsoftConfig.adSchedules: must be an array of objects",
		`,"ADSCHEDULES":[{"day":1}]`:                                                                                "microsoftConfig.adSchedules: must be an array of objects",
		`,"adSchedules":[{"day":"Monday","startHour":"9","endHour":17}]`:                                            "microsoftConfig.adSchedules: must be an array of objects",
		`,"adSchedules":[{"day":"Monday","endHour":17}]`:                                                            "microsoftConfig.adSchedules: entry 1 startHour is required",
		`,"adSchedules":[{"day":"Monday","startHour":0}]`:                                                           "microsoftConfig.adSchedules: entry 1 endHour is required",
		`,"adSchedules":[{"day":"Monday","startHour":9.5,"endHour":17}]`:                                            "entry 1 startHour must be a whole hour",
		`,"adSchedules":[{"day":"Monday","startHour":9,"endHour":17},{"day":"Monday","startHour":16,"endHour":18}]`: "microsoftConfig.adSchedules: entries 1 and 2 overlap on Monday",
		`,"budget":"x","adSchedules":[{"day":"Funday","startHour":1,"endHour":2}]`:                                  "microsoftConfig.adSchedules: entry 1 day must be",
		`,"adSchedules":[{"day":"Monday","startHour":1,"endHour":2,"bidAdjustment":1000}]`:                          "entry 1 bidAdjustment must be a whole percent from -90 to 900",
	} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, want)
		_, derr := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(extra))
		requirePreCreate(t, derr)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls for a refused schedule; want 0", n)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(`,"adSchedules":[{"day":"Sunday","startHour":0,"endHour":24}]`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Errorf("valid: PreflightCreate = %v, want not applicable", err)
	}
}

// On: the window reaches the client and is attached before the ad group.
func TestMicrosoftAdSchedule_DispatchSendsIt(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftAdScheduleEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msScheduleExtra))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want created", camp.Status)
	}
	calls := api.snapshot()
	ci, ag := slices.Index(calls, "CampaignCriterions"), slices.Index(calls, "AdGroups")
	if ci < 0 || ag < 0 || ci > ag {
		t.Fatalf("calls = %v, want the schedule before the ad group", calls)
	}
	api.mu.Lock()
	body := api.bodies[ci]
	api.mu.Unlock()
	if !strings.Contains(body, `"Criterion":{"Type":"DayTimeCriterion","Day":"Monday","FromHour":9,"FromMinute":"Zero","ToHour":17,"ToMinute":"Zero"}`) || !strings.Contains(body, `"Multiplier":20`) {
		t.Errorf("criterion body = %s", body)
	}
	if !strings.Contains(string(camp.Result), `"adScheduleCriterionIds":["8001"]`) {
		t.Errorf("result = %s", camp.Result)
	}
}
