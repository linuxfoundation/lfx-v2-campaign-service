// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Create-time ad schedule (#563, gap 10): microsoftConfig.adSchedules.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com):
//   - A DayTimeCriterion (Day Monday..Sunday, FromHour 0-23, FromMinute, ToHour 0-24, ToMinute
//     Zero | Fifteen | Thirty | FortyFive) in a BiddableCampaignCriterion schedules when the
//     campaign's ads are eligible, in the campaign's TimeZone. Once a campaign has any day-time
//     criterion its ads serve only inside the scheduled windows. It is added with
//     AddCampaignCriterions, CriterionType "Targets", like the location and radius criteria.
//   - Its CriterionBid is a BidMultiplier, the percent adjustment applied inside the window.
//
// This service takes whole hours only (both minutes are sent as Zero): the issue's shape, and
// enough for an event's dayparting. Windows on the same day may touch (9-12 and 12-17) but not
// overlap.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_AD_SCHEDULE_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldAdSchedules is the microsoftConfig key, named in refusals.
	FieldAdSchedules = "adSchedules"

	// MaxAdSchedules bounds the list (seven windows for each of the seven days). It is this
	// service's bound, keeping the one criterion call small; it is not a documented Microsoft
	// limit.
	MaxAdSchedules = 49

	// MinScheduleBidAdjustment and MaxScheduleBidAdjustment are Microsoft's BidMultiplier range
	// for a day-time criterion, in whole percent.
	MinScheduleBidAdjustment = -90
	MaxScheduleBidAdjustment = 900

	criterionTypeDayTime = "DayTimeCriterion"
	minuteZero           = "Zero"
)

// scheduleDays is Microsoft's Day enum in week order, keyed by lower case so a caller's casing
// is accepted and Microsoft's spelling is sent.
var scheduleDays = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// AdSchedule is one requested window: Day, a whole-hour [StartHour, EndHour) span and a
// whole-percent BidAdjustment applied inside it (0 schedules without adjusting).
type AdSchedule struct {
	Day           string
	StartHour     int
	EndHour       int
	BidAdjustment float64
}

// AdScheduleError is a refusal of the adSchedules input; Msg names a 1-based position.
type AdScheduleError struct {
	Msg string
}

func (e *AdScheduleError) Error() string { return FieldAdSchedules + ": " + e.Msg }

// ValidateAdSchedules applies the rules without contacting Microsoft, so the dispatcher's
// synchronous preflight and CreateCampaign agree, and returns the windows with Day in
// Microsoft's spelling, sorted by day and start hour (the order they are sent). A refusal is an
// *AdScheduleError.
func ValidateAdSchedules(in []AdSchedule) ([]AdSchedule, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAdSchedules {
		return nil, &AdScheduleError{Msg: fmt.Sprintf("at most %d windows are accepted, got %d", MaxAdSchedules, len(in))}
	}
	type indexed struct {
		AdSchedule
		day, pos int
	}
	out := make([]indexed, 0, len(in))
	for i, s := range in {
		day := -1
		for d, name := range scheduleDays {
			if strings.EqualFold(strings.TrimSpace(s.Day), name) {
				day = d
				s.Day = name
			}
		}
		switch {
		case day < 0:
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entry %d day must be Monday, Tuesday, Wednesday, Thursday, Friday, Saturday or Sunday", i+1)}
		case s.StartHour < 0 || s.StartHour > 23:
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entry %d startHour must be a whole hour from 0 to 23", i+1)}
		case s.EndHour < 1 || s.EndHour > 24:
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entry %d endHour must be a whole hour from 1 to 24", i+1)}
		case s.EndHour <= s.StartHour:
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entry %d endHour must be after startHour", i+1)}
		case math.IsNaN(s.BidAdjustment) || s.BidAdjustment != math.Trunc(s.BidAdjustment) ||
			s.BidAdjustment < MinScheduleBidAdjustment || s.BidAdjustment > MaxScheduleBidAdjustment:
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entry %d bidAdjustment must be a whole percent from %d to %d", i+1, MinScheduleBidAdjustment, MaxScheduleBidAdjustment)}
		}
		out = append(out, indexed{AdSchedule: s, day: day, pos: i + 1})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].day != out[b].day {
			return out[a].day < out[b].day
		}
		return out[a].StartHour < out[b].StartHour
	})
	res := make([]AdSchedule, len(out))
	for i, s := range out {
		if i > 0 && out[i-1].day == s.day && s.StartHour < out[i-1].EndHour {
			return nil, &AdScheduleError{Msg: fmt.Sprintf("entries %d and %d overlap on %s", min(out[i-1].pos, s.pos), max(out[i-1].pos, s.pos), s.Day)}
		}
		res[i] = s.AdSchedule
	}
	return res, nil
}

// msDayTimeCriterion is a DayTimeCriterion on the ADD path.
type msDayTimeCriterion struct {
	Type       string `json:"Type"`
	Day        string `json:"Day"`
	FromHour   int    `json:"FromHour"`
	FromMinute string `json:"FromMinute"`
	ToHour     int    `json:"ToHour"`
	ToMinute   string `json:"ToMinute"`
}

// createAdScheduleCriterions attaches each window as a BiddableCampaignCriterion with a
// DayTimeCriterion and its BidMultiplier, in ONE AddCampaignCriterions call with the outcome
// rules of postCampaignCriterions.
func (c *Client) createAdScheduleCriterions(ctx context.Context, campaignID string, schedules []AdSchedule) ([]string, error) {
	crits := make([]msTargetCampaignCriterion, 0, len(schedules))
	for _, s := range schedules {
		crits = append(crits, msTargetCampaignCriterion{
			CampaignId: json.Number(campaignID),
			Criterion: msDayTimeCriterion{
				Type: criterionTypeDayTime, Day: s.Day, FromHour: s.StartHour, FromMinute: minuteZero,
				ToHour: s.EndHour, ToMinute: minuteZero,
			},
			Type:         campaignCriterionTypeBiddable,
			CriterionBid: &msBidMultiplier{Type: criterionBidMultiplier, Multiplier: s.BidAdjustment},
		})
	}
	return c.postCampaignCriterions(ctx, campaignID, "ad schedule", "day-time criteria",
		addTargetCriterionsRequest{CampaignCriterions: crits, CriterionType: criterionTypeTargets}, len(crits))
}

// adScheduleLabel renders the windows for a step: "Monday 9-17 (+20%); Tuesday 0-24".
func adScheduleLabel(schedules []AdSchedule) string {
	parts := make([]string, 0, len(schedules))
	for _, s := range schedules {
		p := fmt.Sprintf("%s %d-%d", s.Day, s.StartHour, s.EndHour)
		if s.BidAdjustment != 0 {
			p += fmt.Sprintf(" (%+d%%)", int64(s.BidAdjustment))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}
