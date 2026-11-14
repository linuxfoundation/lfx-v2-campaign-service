// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"reflect"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestMeta_PreflightCreate_OSTargetingFlag: targeting.userOs has its own flag; off, it is a 400
// naming it; on, an OS-only block needs no audience flag, and the documented combination rules, the
// desktop rule and the awareness rule are 400s before any job exists.
func TestMeta_PreflightCreate_OSTargetingFlag(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaOSTargetingEnabled, "on")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "userOs", config: `{"metaConfig":{"targeting":{"userOs":["iOS"]}}}`,
				want: "metaConfig.targeting.userOs is disabled on this deployment (META_OS_TARGETING_ENABLED is not \"true\")"},
		})
	})
	t.Run("on, audience flag off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaOSTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaDeviceTargetingEnabled, "true")
		t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "both families", config: `{"metaConfig":{"targeting":{"userOs":["iOS","Android"]}}}`},
			{name: "an OS and an age", config: `{"metaConfig":{"targeting":{"userOs":["iOS"],"ageMin":30}}}`,
				want: "META_AUDIENCE_TARGETING_ENABLED"},
			{name: "a version with the other family", config: `{"metaConfig":{"targeting":{"userOs":["Android","iOS_ver_8.0_and_above"]}}}`,
				want: "invalid campaign config: metaConfig.targeting: userOs iOS_ver_8.0_and_above names a version of iOS, so it cannot be combined with Android"},
			{name: "desktop only", config: `{"metaConfig":{"targeting":{"userOs":["iOS"],"devicePlatforms":["desktop"]}}}`,
				want: "cannot be combined with devicePlatforms [desktop]"},
			{name: "a version on awareness", config: `{"metaConfig":{"objective":"awareness","targeting":{"userOs":["iOS_ver_9.0_and_above"]}}}`,
				want: "which the awareness objective cannot target"},
			{name: "a number instead of a string", config: `{"metaConfig":{"targeting":{"userOs":[1]}}}`,
				want: `"targeting" must be an object of`},
		})
	})
}

// TestMeta_DispatchCarriesUserOSToTheAdSet: with only the OS flag on, the canonical values reach the
// ad set's user_os and no Advantage+ flag is sent.
func TestMeta_DispatchCarriesUserOSToTheAdSet(t *testing.T) {
	t.Setenv(constants.EnvMetaOSTargetingEnabled, "true")
	t.Setenv(constants.EnvMetaAudienceTargetingEnabled, "false")
	targeting := dispatchMetaAdSetTargeting(t, `"targeting":{"userOs":["android_ver_5.0_and_above"]}`)
	if !reflect.DeepEqual(targeting["user_os"], []any{"Android_ver_5.0_and_above"}) {
		t.Errorf("user_os = %#v, want [Android_ver_5.0_and_above]", targeting["user_os"])
	}
	if _, has := targeting["targeting_automation"]; has {
		t.Errorf("targeting_automation = %v, want it absent", targeting["targeting_automation"])
	}
}
