// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Gap 4c (#564): followerLookalikeUserIds sits behind its own default-off flag (a synchronous 400
// naming it, and refused again at dispatch before any X call), is refused synchronously when
// malformed or over the shared criteria budget, and reaches the line item as
// SIMILAR_TO_FOLLOWERS_OF_USER criteria — without the targeting flag.
func TestTwitterFollowerLookalikes(t *testing.T) {
	t.Setenv(constants.EnvTwitterFollowerLookalikesEnabled, "")
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "")
	d, criteria, calls := twitterTargetingServer(t)
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"followerLookalikeUserIds":["14230524"]`)),
		constants.EnvTwitterFollowerLookalikesEnabled)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"followerLookalikeUserIds":["14230524"]`))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvTwitterFollowerLookalikesEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", n)
	}
	for _, extra := range []string{`,"followerLookalikeUserIds":[]`, `,"followerLookalikeUserIds":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%s with the flag off: %v, want not applicable", extra, err)
		}
	}

	t.Setenv(constants.EnvTwitterFollowerLookalikesEnabled, "true")
	for extra, want := range map[string]string{
		`,"followerLookalikeUserIds":"14230524"`:                 "twitterConfig.followerLookalikeUserIds: must be an array of strings",
		`,"FOLLOWERLOOKALIKEUSERIDS":[14230524]`:                 "twitterConfig.followerLookalikeUserIds: must be an array of strings",
		`,"followerLookalikeUserIds":["@lf"]`:                    "twitterConfig.followerLookalikeUserIds: entry 0",
		`,"followerLookalikeUserIds":[" "]`:                      "twitterConfig.followerLookalikeUserIds: entry 0 is blank",
		`,"budgetAmount":"bad","followerLookalikeUserIds":["0"]`: "twitterConfig.followerLookalikeUserIds:",
		`,"followerLookalikeUserIds":["9999999999999999999"]`:    "out of range",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)), want)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"followerLookalikeUserIds":["14230524"," 783214","14230524"]`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := strings.Join(criteria(), ","); got != "SIMILAR_TO_FOLLOWERS_OF_USER=14230524,SIMILAR_TO_FOLLOWERS_OF_USER=783214" {
		t.Fatalf("criteria = %q", got)
	}
}

// The look-alikes count toward the per-create criteria budget the other lists share.
func TestTwitterFollowerLookalikes_CountTowardTheCriteriaBudget(t *testing.T) {
	t.Setenv(constants.EnvTwitterFollowerLookalikesEnabled, "true")
	t.Setenv(constants.EnvTwitterCreateTargetingEnabled, "true")
	d, _, calls := twitterTargetingServer(t)
	ids := make([]string, 60)
	for i := range ids {
		ids[i] = `"` + strconv.Itoa(1000+i) + `"`
	}
	extra := `,"languages":["en"],"followerLookalikeUserIds":[` + strings.Join(ids, ",") + `]`
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)), "together name 61 targeting criteria")
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(extra))
	requirePreCreate(t, err)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls; want 0", n)
	}
}
