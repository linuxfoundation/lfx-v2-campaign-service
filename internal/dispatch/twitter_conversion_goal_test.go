// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"net/url"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const twitterConversionExtra = `,"goal":"WEBSITE_CONVERSIONS","primaryWebEventTag":"o3bk1"`

// Gap 5 (#564): goal / primaryWebEventTag sit behind their own default-off flag (a synchronous 400
// naming it, and refused again at dispatch before any X call).
func TestTwitterConversionGoal_FlagOff(t *testing.T) {
	for _, v := range []string{"", "false", "TRUE", "1", " true"} {
		t.Setenv(constants.EnvTwitterWebsiteConversionsEnabled, v)
		d, _, calls := twitterLineItemServer(t)
		for _, extra := range []string{twitterConversionExtra, `,"primaryWebEventTag":"o3bk1"`, `,"goal":"WEBSITE_CONVERSIONS"`} {
			requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)),
				constants.EnvTwitterWebsiteConversionsEnabled)
		}
		_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(twitterConversionExtra))
		requirePreCreate(t, err)
		if n := calls.Load(); n != 0 {
			t.Fatalf("flag %q: %d X calls; want 0", v, n)
		}
		// Blank values are unset and need no flag.
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"goal":" ","primaryWebEventTag":""`)); err != domain.ErrPreflightNotApplicable {
			t.Fatalf("flag %q, blank goal: %v, want not applicable", v, err)
		}
	}
}

// With the flag on, a bad pair or a wrong type is a synchronous 400, and a good pair reaches the new
// line item as goal and primary_web_event_tag.
func TestTwitterConversionGoal_FlagOn(t *testing.T) {
	t.Setenv(constants.EnvTwitterWebsiteConversionsEnabled, "true")
	d, lineItemQuery, _ := twitterLineItemServer(t)
	for extra, want := range map[string]string{
		`,"goal":["WEBSITE_CONVERSIONS"],"primaryWebEventTag":"o3bk1"`:        "twitterConfig.goal: must be a string",
		`,"GOAL":"WEBSITE_CONVERSIONS","PrimaryWebEventTag":1`:                "twitterConfig.primaryWebEventTag: must be a string",
		`,"goal":"LINK_CLICKS","primaryWebEventTag":"o3bk1"`:                  "twitterConfig.goal: \"LINK_CLICKS\" must be WEBSITE_CONVERSIONS",
		`,"goal":"WEBSITE_CONVERSIONS"`:                                       "twitterConfig.primaryWebEventTag: is required with goal WEBSITE_CONVERSIONS",
		`,"primaryWebEventTag":"o3bk1"`:                                       "twitterConfig.primaryWebEventTag: needs goal WEBSITE_CONVERSIONS",
		`,"goal":"WEBSITE_CONVERSIONS","primaryWebEventTag":"tw-o8z6j-o3bk1"`: "twitterConfig.primaryWebEventTag:",
		`,"budgetAmount":"bad","goal":"x","primaryWebEventTag":"o3bk1"`:       "twitterConfig.goal:",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)), want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"goal":null,"primaryWebEventTag":null`)); err != domain.ErrPreflightNotApplicable {
		t.Fatalf("null goal: %v, want not applicable", err)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(twitterConversionExtra)); err != domain.ErrPreflightNotApplicable {
		t.Fatalf("valid goal: %v, want not applicable", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"goal":"website_conversions","primaryWebEventTag":"O3BK1"`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	q, _ := url.ParseQuery(lineItemQuery())
	if q.Get("goal") != "WEBSITE_CONVERSIONS" || q.Get("primary_web_event_tag") != "o3bk1" || q.Get("objective") != "WEBSITE_CLICKS" {
		t.Fatalf("line-item query %q, want goal=WEBSITE_CONVERSIONS primary_web_event_tag=o3bk1 objective=WEBSITE_CLICKS", lineItemQuery())
	}
}
