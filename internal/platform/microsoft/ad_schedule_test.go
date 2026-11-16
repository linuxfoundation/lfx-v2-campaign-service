// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestValidateAdSchedules(t *testing.T) {
	tooMany := make([]AdSchedule, MaxAdSchedules+1)
	for i := range tooMany {
		tooMany[i] = AdSchedule{Day: "Monday", StartHour: 0, EndHour: 1}
	}
	// Eight non-overlapping Monday windows: within the 49 total but over Microsoft's seven per
	// day; seven per day on every day (49) is accepted.
	eightMonday := make([]AdSchedule, MaxAdSchedulesPerDay+1)
	for i := range eightMonday {
		eightMonday[i] = AdSchedule{Day: "Monday", StartHour: i, EndHour: i + 1}
	}
	fullWeek := make([]AdSchedule, 0, MaxAdSchedules)
	for _, d := range scheduleDays {
		for h := 0; h < 7; h++ {
			fullWeek = append(fullWeek, AdSchedule{Day: d, StartHour: h, EndHour: h + 1})
		}
	}
	for _, tc := range []struct {
		name string
		in   []AdSchedule
		want string // "" = accepted
	}{
		{"eight windows on one day", eightMonday, "adSchedules: at most 7 windows are accepted for one day, got more on Monday"},
		{"seven windows on every day", fullWeek, ""},
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
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Ad schedule attached: 2 day-time criteria (Monday 9-17 (+20%); Tuesday 0-24, in the Microsoft Advertising account's time zone, not the campaign's)") {
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

// fullWeekSchedule is seven windows on each of the seven days: MaxAdSchedules (49).
func fullWeekSchedule() []AdSchedule {
	out := make([]AdSchedule, 0, MaxAdSchedules)
	for _, d := range scheduleDays {
		for h := 0; h < MaxAdSchedulesPerDay; h++ {
			out = append(out, AdSchedule{Day: d, StartHour: h, EndHour: h + 1})
		}
	}
	return out
}

// A 49-window schedule that Microsoft accepts in full is CONFIRMED: every id survives the decode
// (the criterion response bound covers MaxAdSchedules, not just the 30 geo targets), including
// when NestedPartialErrors is positional and null-padded to one slot per window.
func TestCreateCampaign_FullWeekAdScheduleIsConfirmed(t *testing.T) {
	ids := make([]string, MaxAdSchedules)
	nulls := make([]string, MaxAdSchedules)
	for i := range ids {
		ids[i] = strconv.Itoa(7000 + i)
		nulls[i] = "null"
	}
	for name, body := range map[string]string{
		"plain":           "",
		"positional null": `{"CampaignCriterionIds":[` + strings.Join(ids, ",") + `],"NestedPartialErrors":[` + strings.Join(nulls, ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := &targetingAPI{}
			if body != "" {
				a.failCriterion, a.criterionStatus, a.criterionBody = 2, http.StatusOK, body
			}
			c := newTargetingClient(t, a)
			in := scheduleCreateInput()
			in.AdSchedules = fullWeekSchedule()
			res, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if n := len(res.AdScheduleCriterionIDs); n != MaxAdSchedules {
				t.Fatalf("schedule ids = %d, want %d", n, MaxAdSchedules)
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), "Ad schedule attached: 49 day-time criteria") {
				t.Errorf("steps = %v", res.Steps)
			}
		})
	}
}

// The criterion response's error entries follow audienceErrorCode's rule, for the ad schedule
// and the bid adjustments alike (both go through postCampaignCriterions): zero-code padding
// ({"Code":0,"ErrorCode":null}, at either nesting level) is success; an unreadable code is a
// rejection, never success; codes that are both explicitly null are UNCONFIRMED.
func TestCreateCampaign_CriterionErrorEntriesUseTheDefaultCodeRule(t *testing.T) {
	for _, step := range []struct {
		name string
		in   func() CampaignInput
		n    int
	}{
		{"ad schedule", scheduleCreateInput, 2},
		{"bid adjustments", bidAdjustmentCreateInput, 5}, // Tablets expands to three devices
	} {
		ids := make([]string, step.n)
		for i := range ids {
			ids[i] = strconv.Itoa(7000 + i)
		}
		idList := strings.Join(ids, ",")
		for _, tc := range []struct {
			name, errs  string
			wantErr     bool
			unconfirmed bool
		}{
			{"zero-code collection padding", `[{"Code":0,"ErrorCode":null,"Index":0},{"Code":0,"ErrorCode":"","Index":1}]`, false, false},
			{"zero-code nested padding", `[{"Code":0,"ErrorCode":null,"BatchErrors":[{"Code":0,"ErrorCode":null,"Index":0}]}]`, false, false},
			{"unreadable collection code", `[{"Code":true,"ErrorCode":null,"Index":0}]`, true, false},
			{"unreadable nested code", `[{"BatchErrors":[{"Code":{"x":1},"Index":0}]}]`, true, false},
			{"both codes explicitly null", `[{"Code":null,"ErrorCode":null,"Index":0}]`, true, true},
			// One explicitly null code with the other absent says no more than both null.
			{"Code null, ErrorCode absent", `[{"Code":null,"Index":0}]`, true, true},
			{"ErrorCode null, Code absent", `[{"ErrorCode":null,"Index":0}]`, true, true},
			{"nested Code null only", `[{"BatchErrors":[{"Code":null,"Index":0}]}]`, true, true},
			// Neither key, or a null slot, is index-aligned padding here.
			{"key-less padding", `[{"Index":0},null,{"BatchErrors":[{},null]}]`, false, false},
		} {
			t.Run(step.name+"/"+tc.name, func(t *testing.T) {
				a := &targetingAPI{failCriterion: 2, criterionStatus: http.StatusOK,
					criterionBody: `{"CampaignCriterionIds":[` + idList + `],"NestedPartialErrors":` + tc.errs + `}`}
				c := newTargetingClient(t, a)
				_, err := c.CreateCampaign(context.Background(), step.in())
				if (err != nil) != tc.wantErr {
					t.Fatalf("err = %v, want error=%v", err, tc.wantErr)
				}
				// The step error says which: "UNCONFIRMED" or a definite "PARTIALLY attached".
				if err != nil && strings.Contains(err.Error(), "UNCONFIRMED") != tc.unconfirmed {
					t.Fatalf("err = %v, want unconfirmed=%v", err, tc.unconfirmed)
				}
			})
		}
	}
}

// An entry whose codes are both explicitly null makes the outcome UNCONFIRMED even beside a
// coded rejection (which alone would be a definite partial failure), and the ids that were
// returned are kept for reconciliation — for the ad schedule and the bid adjustments alike.
func TestCreateCampaign_CriterionBothNullBesideARejectionIsUnconfirmed(t *testing.T) {
	for _, step := range []struct {
		name string
		in   func() CampaignInput
		n    int
		ids  func(*CampaignResult) []string
	}{
		{"ad schedule", scheduleCreateInput, 2, func(r *CampaignResult) []string { return r.AdScheduleCriterionIDs }},
		{"bid adjustments", bidAdjustmentCreateInput, 5, func(r *CampaignResult) []string { return r.BidAdjustmentCriterionIDs }},
	} {
		t.Run(step.name, func(t *testing.T) {
			ids := make([]string, step.n)
			for i := range ids {
				ids[i] = strconv.Itoa(7000 + i)
			}
			ids[0] = "null" // the rejected one
			body := `{"CampaignCriterionIds":[` + strings.Join(ids, ",") + `],"NestedPartialErrors":[` +
				`{"Code":1032,"ErrorCode":"X","Index":0},{"Code":null,"ErrorCode":null,"Index":1}]}`
			a := &targetingAPI{failCriterion: 2, criterionStatus: http.StatusOK, criterionBody: body}
			c := newTargetingClient(t, a)
			res, err := c.CreateCampaign(context.Background(), step.in())
			if err == nil || !strings.Contains(err.Error(), "UNCONFIRMED") {
				t.Fatalf("err = %v, want UNCONFIRMED", err)
			}
			if res == nil || len(step.ids(res)) != step.n-1 {
				t.Fatalf("recorded ids = %v, want the %d returned", step.ids(res), step.n-1)
			}
		})
	}
}

// An entry with ONE explicitly null code and the other absent is as unreadable as both null:
// beside a coded rejection the outcome is UNCONFIRMED, not a definite partial failure. A pair
// with neither key beside the same rejection is padding, so that stays a definite partial.
func TestCreateCampaign_CriterionSingleNullBesideARejectionIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		unconfirmed bool
	}{
		{"Code null, ErrorCode absent", `{"Code":null,"Index":1}`, true},
		{"ErrorCode null, Code absent", `{"ErrorCode":null,"Index":1}`, true},
		{"nested Code null", `{"BatchErrors":[{"Code":null,"Index":1}]}`, true},
		{"key-less padding", `{"Index":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"CampaignCriterionIds":[null,7001],"NestedPartialErrors":[{"Code":1032,"ErrorCode":"X","Index":0},` + tc.entry + `]}`
			a := &targetingAPI{failCriterion: 2, criterionStatus: http.StatusOK, criterionBody: body}
			c := newTargetingClient(t, a)
			res, err := c.CreateCampaign(context.Background(), scheduleCreateInput())
			if err == nil || strings.Contains(err.Error(), "UNCONFIRMED") != tc.unconfirmed {
				t.Fatalf("err = %v, want unconfirmed=%v", err, tc.unconfirmed)
			}
			if res == nil || len(res.AdScheduleCriterionIDs) != 1 {
				t.Fatalf("recorded ids = %v, want the one returned", res.AdScheduleCriterionIDs)
			}
		})
	}
}
