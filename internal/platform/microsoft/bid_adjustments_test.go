// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestValidateBidAdjustments(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   BidAdjustments
		want string // "" = accepted
		got  string // accepted: the label of the normalized list
	}{
		{"nothing", BidAdjustments{}, "", ""},
		{"ordered and normalized", BidAdjustments{
			Device: map[string]float64{"tablets": -100, " SMARTPHONES ": -20},
			Age:    map[string]float64{"sixtyfiveandabove": -90, "EighteenToTwentyFour": 900},
			Gender: map[string]float64{"female": 10},
		}, "", "Smartphones -20%, Tablets -100%, EighteenToTwentyFour +900%, SixtyFiveAndAbove -90%, Female +10%"},
		{"unknown device", BidAdjustments{Device: map[string]float64{"TV": 5}}, "deviceBidAdjustments: unknown key; use Computers, Smartphones, Tablets", ""},
		{"unknown age", BidAdjustments{Age: map[string]float64{"Unknown": 5}}, "ageBidAdjustments: unknown key", ""},
		{"duplicate by case", BidAdjustments{Gender: map[string]float64{"Male": 5, "male": 6}}, "genderBidAdjustments: Male is named twice", ""},
		{"device below -100", BidAdjustments{Device: map[string]float64{"Computers": -101}}, "deviceBidAdjustments: Computers must be a whole percent from -100 to 900", ""},
		{"age -100 refused", BidAdjustments{Age: map[string]float64{"FiftyToSixtyFour": -100}}, "ageBidAdjustments: FiftyToSixtyFour must be a whole percent from -90 to 900", ""},
		{"gender above 900", BidAdjustments{Gender: map[string]float64{"Female": 901}}, "from -90 to 900", ""},
		{"fraction", BidAdjustments{Device: map[string]float64{"Tablets": 1.5}}, "Tablets must be a whole percent", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateBidAdjustments(tc.in)
			if tc.want != "" {
				var bErr *BidAdjustmentError
				if !errors.As(err, &bErr) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a BidAdjustmentError containing %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if l := bidAdjustmentLabel(got); l != tc.got {
				t.Errorf("label = %q, want %q", l, tc.got)
			}
		})
	}
}

func bidAdjustmentCreateInput() CampaignInput {
	in := validInput()
	in.GeoTargets = []string{"US"}
	in.BidAdjustments = BidAdjustments{
		Device: map[string]float64{"Tablets": -100},
		Age:    map[string]float64{"EighteenToTwentyFour": -50},
		Gender: map[string]float64{"Female": 15},
	}
	return in
}

// The adjustments follow the positive geo in one Targets call, each a biddable criterion of its
// own type with its BidMultiplier, before the ad group.
func TestCreateCampaign_BidAdjustmentsAttachedBeforeTheAdGroup(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), bidAdjustmentCreateInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	calls, _ := a.snapshot()
	var muts []string
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" || p == "AdGroups" || p == "Ads" {
			muts = append(muts, p)
		}
	}
	if got := strings.Join(muts, ","); got != "Campaigns,CampaignCriterions,CampaignCriterions,AdGroups,Ads" {
		t.Fatalf("mutations = %s", got)
	}
	want := `{"CampaignCriterions":[` +
		`{"CampaignId":321,"Criterion":{"Type":"DeviceCriterion","DeviceName":"Tablets"},"Type":"BiddableCampaignCriterion","CriterionBid":{"Type":"BidMultiplier","Multiplier":-100}},` +
		`{"CampaignId":321,"Criterion":{"Type":"AgeCriterion","AgeRange":"EighteenToTwentyFour"},"Type":"BiddableCampaignCriterion","CriterionBid":{"Type":"BidMultiplier","Multiplier":-50}},` +
		`{"CampaignId":321,"Criterion":{"Type":"GenderCriterion","GenderType":"Female"},"Type":"BiddableCampaignCriterion","CriterionBid":{"Type":"BidMultiplier","Multiplier":15}}` +
		`],"CriterionType":"Targets"}`
	if b := a.bodyOf("CampaignCriterions", 2); b != want {
		t.Errorf("adjustment body =\n%s\nwant\n%s", b, want)
	}
	if strings.Join(res.BidAdjustmentCriterionIDs, ",") != "9002,9102,9202" {
		t.Errorf("ids = %v", res.BidAdjustmentCriterionIDs)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Bid adjustments attached: 3 criteria (Tablets -100%, EighteenToTwentyFour -50%, Female +15%)") {
		t.Errorf("steps = %v", res.Steps)
	}
}

// A refused adjustment creates nothing.
func TestCreateCampaign_BidAdjustmentRefusalCreatesNothing(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	in := bidAdjustmentCreateInput()
	in.BidAdjustments.Gender = map[string]float64{"Other": 5}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || err == nil || !strings.Contains(err.Error(), "invalid microsoftConfig.genderBidAdjustments: unknown key") {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	calls, _ := a.snapshot()
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" {
			t.Fatalf("a mutation was sent: %v", calls)
		}
	}
}

// A failed attach stops the create before the ad group; a campaign found by name stops there too.
func TestCreateCampaign_BidAdjustmentFailureAndReuseStopBeforeTheAdGroup(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		a := &targetingAPI{failCriterion: 2, criterionStatus: http.StatusOK, criterionBody: `{"CampaignCriterionIds":[9002,null,9202],"NestedPartialErrors":[{"Index":1,"Code":1032,"ErrorCode":"X"}]}`}
		c := newTargetingClient(t, a)
		res, err := c.CreateCampaign(context.Background(), bidAdjustmentCreateInput())
		if res == nil || err == nil || !strings.Contains(err.Error(), "bid adjustments PARTIALLY attached (campaign 321 carries 2 of 3") {
			t.Fatalf("CreateCampaign = (%+v, %v)", res, err)
		}
		if strings.Join(res.BidAdjustmentCriterionIDs, ",") != "9002,9202" {
			t.Errorf("partial ids = %v", res.BidAdjustmentCriterionIDs)
		}
		calls, _ := a.snapshot()
		for _, p := range calls {
			if p == "AdGroups" {
				t.Fatalf("the ad group was created after a failed attach: %v", calls)
			}
		}
	})
	t.Run("reused", func(t *testing.T) {
		in := bidAdjustmentCreateInput()
		a := &targetingAPI{campaignQueryBody: `{"Campaigns":[{"Id":321,"Name":` + jsonString(composeName(in)) + `}]}`}
		c := newTargetingClient(t, a)
		res, err := c.CreateCampaign(context.Background(), in)
		if res == nil || err == nil || !strings.Contains(err.Error(), "NOT applied to reused campaign 321 (deviceBidAdjustments, ageBidAdjustments, genderBidAdjustments are set only") {
			t.Fatalf("CreateCampaign = (%+v, %v)", res, err)
		}
		calls, _ := a.snapshot()
		for _, p := range calls {
			if p == "Campaigns" || p == "CampaignCriterions" || p == "AdGroups" {
				t.Fatalf("a mutation was sent to a reused campaign: %v", calls)
			}
		}
	})
}
