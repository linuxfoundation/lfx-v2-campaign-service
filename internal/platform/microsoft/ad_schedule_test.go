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

func TestValidateAdSchedules(t *testing.T) {
	tooMany := make([]AdSchedule, MaxAdSchedules+1)
	for i := range tooMany {
		tooMany[i] = AdSchedule{Day: "Monday", StartHour: 0, EndHour: 1}
	}
	for _, tc := range []struct {
		name string
		in   []AdSchedule
		want string // "" = accepted
	}{
		{"nothing", nil, ""},
		{"touching windows", []AdSchedule{{Day: "monday", StartHour: 12, EndHour: 17}, {Day: " MONDAY ", StartHour: 9, EndHour: 12}}, ""},
		{"whole day with bounds", []AdSchedule{{Day: "Sunday", StartHour: 0, EndHour: 24, BidAdjustment: 900}, {Day: "Saturday", StartHour: 23, EndHour: 24, BidAdjustment: -90}}, ""},
		{"too many", tooMany, "adSchedules: at most 49 windows"},
		{"bad day", []AdSchedule{{Day: "Funday", StartHour: 1, EndHour: 2}}, "adSchedules: entry 1 day must be Monday"},
		{"start high", []AdSchedule{{Day: "Monday", StartHour: 24, EndHour: 24}}, "entry 1 startHour must be a whole hour from 0 to 23"},
		{"start negative", []AdSchedule{{Day: "Monday", StartHour: -1, EndHour: 2}}, "entry 1 startHour"},
		{"end high", []AdSchedule{{Day: "Monday", StartHour: 1, EndHour: 25}}, "entry 1 endHour must be a whole hour from 1 to 24"},
		{"end before start", []AdSchedule{{Day: "Monday", StartHour: 9, EndHour: 9}}, "entry 1 endHour must be after startHour"},
		{"adjustment low", []AdSchedule{{Day: "Monday", StartHour: 1, EndHour: 2, BidAdjustment: -91}}, "entry 1 bidAdjustment must be a whole percent from -90 to 900"},
		{"adjustment fraction", []AdSchedule{{Day: "Monday", StartHour: 1, EndHour: 2, BidAdjustment: 2.5}}, "bidAdjustment must be a whole percent"},
		{"overlap", []AdSchedule{{Day: "Tuesday", StartHour: 1, EndHour: 2}, {Day: "Monday", StartHour: 9, EndHour: 13}, {Day: "monday", StartHour: 12, EndHour: 17}}, "entries 2 and 3 overlap on Monday"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAdSchedules(tc.in)
			if tc.want != "" {
				var sErr *AdScheduleError
				if !errors.As(err, &sErr) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want an AdScheduleError containing %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.name == "touching windows" && (got[0].StartHour != 9 || got[0].Day != "Monday" || got[1].StartHour != 12) {
				t.Errorf("normalized = %+v, want Monday 9-12 then 12-17", got)
			}
		})
	}
}

func scheduleCreateInput() CampaignInput {
	in := validInput()
	in.GeoTargets = []string{"US"}
	in.AdSchedules = []AdSchedule{{Day: "tuesday", StartHour: 0, EndHour: 24}, {Day: "Monday", StartHour: 9, EndHour: 17, BidAdjustment: 20}}
	return in
}

// The windows follow the positive geo in their own Targets call, sorted, each a biddable
// DayTimeCriterion with its BidMultiplier, before the ad group.
func TestCreateCampaign_AdScheduleAttachedBeforeTheAdGroup(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), scheduleCreateInput())
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
		`{"CampaignId":321,"Criterion":{"Type":"DayTimeCriterion","Day":"Monday","FromHour":9,"FromMinute":"Zero","ToHour":17,"ToMinute":"Zero"},"Type":"BiddableCampaignCriterion","CriterionBid":{"Type":"BidMultiplier","Multiplier":20}},` +
		`{"CampaignId":321,"Criterion":{"Type":"DayTimeCriterion","Day":"Tuesday","FromHour":0,"FromMinute":"Zero","ToHour":24,"ToMinute":"Zero"},"Type":"BiddableCampaignCriterion","CriterionBid":{"Type":"BidMultiplier","Multiplier":0}}` +
		`],"CriterionType":"Targets"}`
	if b := a.bodyOf("CampaignCriterions", 2); b != want {
		t.Errorf("schedule body =\n%s\nwant\n%s", b, want)
	}
	if strings.Join(res.AdScheduleCriterionIDs, ",") != "9002,9102" {
		t.Errorf("schedule ids = %v", res.AdScheduleCriterionIDs)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Ad schedule attached: 2 day-time criteria (Monday 9-17 (+20%); Tuesday 0-24, in the campaign's time zone)") {
		t.Errorf("steps = %v", res.Steps)
	}
}

// A refused schedule creates nothing.
func TestCreateCampaign_AdScheduleRefusalCreatesNothing(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	in := scheduleCreateInput()
	in.AdSchedules[0].EndHour = 0
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || err == nil || !strings.Contains(err.Error(), "invalid microsoftConfig.adSchedules: entry 1 endHour") {
		t.Fatalf("CreateCampaign = (%v, %v)", res, err)
	}
	calls, _ := a.snapshot()
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" {
			t.Fatalf("a mutation was sent: %v", calls)
		}
	}
}

// A failed schedule attach stops the create before the ad group, with the campaign as a partial.
func TestCreateCampaign_AdScheduleFailureStopsBeforeTheAdGroup(t *testing.T) {
	a := &targetingAPI{failCriterion: 2, criterionStatus: http.StatusBadRequest, criterionBody: `{"Code":1001,"ErrorCode":"InvalidRequest"}`}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), scheduleCreateInput())
	if res == nil || res.CampaignID != "321" || err == nil || !strings.Contains(err.Error(), "ad schedule rejected (campaign 321 exists but would serve outside the requested schedule") {
		t.Fatalf("CreateCampaign = (%+v, %v)", res, err)
	}
	calls, _ := a.snapshot()
	for _, p := range calls {
		if p == "AdGroups" {
			t.Fatalf("the ad group was created after a failed schedule: %v", calls)
		}
	}
}

// A campaign found by name gets no schedule: the create stops before the ad group.
func TestCreateCampaign_AdScheduleOnAReusedCampaignStops(t *testing.T) {
	in := scheduleCreateInput()
	a := &targetingAPI{campaignQueryBody: `{"Campaigns":[{"Id":321,"Name":` + jsonString(composeName(in)) + `}]}`}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), in)
	if res == nil || err == nil || !strings.Contains(err.Error(), "NOT applied to reused campaign 321 (adSchedules are set only") {
		t.Fatalf("CreateCampaign = (%+v, %v)", res, err)
	}
	calls, _ := a.snapshot()
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" || p == "AdGroups" {
			t.Fatalf("a mutation was sent to a reused campaign: %v", calls)
		}
	}
}
