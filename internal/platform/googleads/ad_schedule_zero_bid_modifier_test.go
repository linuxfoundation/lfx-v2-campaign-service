// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A 0 ad-schedule bid modifier is refused BEFORE the budget mutate, on both channels
// that send a schedule modifier at all. Google's CampaignCriterion contract admits only
// 0.1..10.0 for a schedule modifier — 0 is a DEVICE exclusion — so letting it through
// would fail the criteria mutate after the budget and campaign already exist. The
// server errors on ANY request, so a pass proves nothing was sent.
func TestCreate_ZeroAdScheduleBidModifierRefusedBeforeAnyRequest(t *testing.T) {
	zeroSchedule := []AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17},
		{DayOfWeek: "TUESDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(0)},
	}
	wants := []string{"ad schedule 1 bid modifier 0", "omit BidModifier to serve at the normal bid"}

	t.Run(campaignKindSearch, func(t *testing.T) {
		tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
		t.Cleanup(tokenSrv.Close)
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("request %s sent for an input the preflight must refuse", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(apiSrv.Close)
		c := NewClient(testCreds(), testAccount(),
			WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
			withRetryBaseDelay(time.Millisecond))

		in := sampleInput()
		in.AdSchedules = zeroSchedule
		res, err := c.CreateCampaign(context.Background(), in)
		assertZeroScheduleRefusal(t, res, err, wants)
		if vErr := c.ValidateCampaignInputKind(campaignKindSearch, in); vErr == nil {
			t.Error("ValidateCampaignInputKind must refuse the same input, or adoption accepts what create refuses")
		}
	})

	t.Run(campaignKindDisplay, func(t *testing.T) {
		srv := demandGenTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("request %s sent for an input the preflight must refuse", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		})
		c := demandGenClient(t, srv)
		in := demandGenInput()
		in.AdSchedules = zeroSchedule
		res, err := c.CreateDisplayCampaign(context.Background(), in)
		assertZeroScheduleRefusal(t, res, err, wants)
		if vErr := c.ValidateCampaignInputKind(campaignKindDisplay, in); vErr == nil {
			t.Error("ValidateCampaignInputKind must refuse the same input, or adoption accepts what create refuses")
		}
	})
}

func assertZeroScheduleRefusal(t *testing.T, res *CampaignResult, err error, wants []string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a preflight refusal for a 0 ad-schedule bid modifier")
	}
	if res != nil {
		t.Errorf("res = %+v, want nil — nothing was created", res)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// The schedule refusal must not leak onto the neighbours: a 0.1 schedule modifier is
// inside Google's range and passes, and a DEVICE 0 is still the exclusion Google
// honours — under the default automated bidding as well as manual CPC, because
// validateBiddingPlan refuses only a NON-ZERO device adjustment there. Video and
// Performance Max take a schedule modifier too, so they are covered for the 0.1 case;
// Performance Max takes no device criteria at all, so it gets no device entry.
func TestCriteriaPlan_ScheduleFloorAndDeviceExclusionStillAccepted(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDisplay, campaignKindVideo, campaignKindPerformanceMax} {
		t.Run(kind, func(t *testing.T) {
			in := demandGenInput()
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(minBidModifier)}}
			if kind != campaignKindPerformanceMax {
				in.DeviceBidModifiers = []DeviceBidModifier{{Device: "TABLET", BidModifier: 0}}
			}
			plan, err := validateCriteriaPlan(kind, in)
			if err != nil {
				t.Fatalf("validateCriteriaPlan(%s): %v", kind, err)
			}
			if len(plan.scheduleMod) != 1 || plan.scheduleMod[0] == nil || *plan.scheduleMod[0] != minBidModifier {
				t.Errorf("schedule modifier = %v, want 0.1 carried through", plan.scheduleMod)
			}
			if kind != campaignKindPerformanceMax && (len(plan.deviceMod) != 1 || plan.deviceMod[0] != 0) {
				t.Errorf("device modifier = %v, want the 0 exclusion carried through", plan.deviceMod)
			}
		})
	}
}

// The full Search preflight (criteria AND bidding) passes a 0.1 schedule modifier with
// a device exclusion under manual CPC and under smart bidding alike: the smart-bidding
// arm in validateBiddingPlan keys on a NON-ZERO device modifier and never on a schedule.
func TestValidateSearch_ScheduleFloorAndDeviceExclusionUnderEveryStrategy(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for _, strategy := range []string{biddingManualCPC, biddingMaximizeConversions} {
		t.Run(strategy, func(t *testing.T) {
			in := sampleInput()
			in.BiddingStrategy = strategy
			in.CPCBid = 0
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(minBidModifier)}}
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "TABLET", BidModifier: 0}}
			if err := c.ValidateCampaignInputKind(campaignKindSearch, in); err != nil {
				t.Fatalf("ValidateCampaignInputKind(search, %s): %v", strategy, err)
			}
		})
	}
}

// Demand Gen is unchanged: it refuses ANY schedule modifier (only an unmodified schedule
// was probed there), so a 0 there still gets the Demand Gen message rather than the
// range one, and an unmodified schedule with a device exclusion still passes.
func TestCriteriaPlan_DemandGenScheduleRulesUnchanged(t *testing.T) {
	in := demandGenInput()
	in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(0)}}
	_, err := validateCriteriaPlan(campaignKindDemandGen, in)
	if err == nil || !strings.Contains(err.Error(), "ad schedule 0 carries a bid modifier") {
		t.Fatalf("err = %v, want the Demand Gen schedule-modifier refusal", err)
	}

	in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17}}
	in.DeviceBidModifiers = []DeviceBidModifier{{Device: "CONNECTED_TV", BidModifier: 0}}
	if _, err := validateCriteriaPlan(campaignKindDemandGen, in); err != nil {
		t.Fatalf("an unmodified schedule and a device exclusion must still pass on Demand Gen: %v", err)
	}
}
