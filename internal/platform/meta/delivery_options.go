// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// OptimizationLandingPageViews is the ad set optimization goal that delivers to people likely to
// load the linked page after the click, rather than to people likely to click (Ad Set
// `optimization_goal` enum, Marketing API v23). A traffic objective accepts it in place of
// LINK_CLICKS without a pixel or promoted object (Meta's ODAX objective / optimization goal matrix:
// OUTCOME_TRAFFIC with destination website pairs with LINK_CLICKS, LANDING_PAGE_VIEWS, REACH and
// IMPRESSIONS).
const OptimizationLandingPageViews = "LANDING_PAGE_VIEWS"

// trafficOptimizationGoals are the goals metaConfig.optimizationGoal accepts (#550, gap 11). Only
// the traffic objective takes a choice: every other objective keeps its one fixed goal, and leads,
// which runs as traffic only until instant forms exist, is kept on LINK_CLICKS so a later real
// leads objective does not inherit a goal it cannot use.
var trafficOptimizationGoals = []string{OptimizationLinkClicks, OptimizationLandingPageViews}

// NormalizeOptimizationGoal returns the ad set optimization_goal metaConfig.optimizationGoal asks
// for: "" for a blank value (the objective's own goal, unchanged), else the upper-cased goal when
// the objective (as CreateCampaign reads it, empty meaning traffic) is traffic and the goal is in
// trafficOptimizationGoals. A goal on any other objective, or outside the list, is a refusal raised
// before any request.
func NormalizeOptimizationGoal(objective, goal string) (string, error) {
	v := strings.ToUpper(strings.TrimSpace(goal))
	if v == "" {
		return "", nil
	}
	if obj := defaultObjective(strings.ToLower(strings.TrimSpace(objective))); obj != "traffic" {
		return "", creativeOptionInvalid("optimizationGoal", "applies only to the traffic objective (objective is %s); omit it", domain.CallerValue(obj))
	}
	for _, g := range trafficOptimizationGoals {
		if v == g {
			return v, nil
		}
	}
	return "", creativeOptionInvalid("optimizationGoal", "%s is not one of %s", domain.CallerValue(goal), strings.Join(trafficOptimizationGoals, ", "))
}

// FrequencyCap is metaConfig.frequencyCap (#550, gap 18): at most MaxImpressions impressions per
// person every IntervalDays days, sent as the ad set's `frequency_control_specs`
// (`[{"event":"IMPRESSIONS","interval_days":…,"max_frequency":…}]`, Ad Set reference). Meta takes
// it only on an ad set that optimizes for REACH (or THRUPLAY), which here is the awareness
// objective, so it is refused on every other one rather than sent and refused by Meta after the
// campaign exists.
type FrequencyCap struct {
	MaxImpressions int `json:"maxImpressions"`
	IntervalDays   int `json:"intervalDays"`
}

// MinFrequencyCapValue and MaxFrequencyCapValue bound both FrequencyCap fields: Meta documents
// interval_days and max_frequency as integers "between 1 and 90 (inclusive)".
const (
	MinFrequencyCapValue = 1
	MaxFrequencyCapValue = 90
)

// NormalizeFrequencyCap refuses a frequency cap the create cannot send: one on an objective (as
// CreateCampaign reads it, empty meaning traffic) other than awareness, or a field outside
// MinFrequencyCapValue..MaxFrequencyCapValue (a missing field reads as 0 and is refused too). A nil
// cap is no cap. Every refusal is a domain.PreflightRefusal wrapping domain.ErrCreateConfigInvalid.
func NormalizeFrequencyCap(objective string, f *FrequencyCap) error {
	if f == nil {
		return nil
	}
	if obj := defaultObjective(strings.ToLower(strings.TrimSpace(objective))); obj != "awareness" {
		return creativeOptionInvalid("frequencyCap", "applies only to the awareness objective, whose ad set optimizes for reach (objective is %s); omit it", domain.CallerValue(obj))
	}
	for _, v := range []struct {
		name string
		n    int
	}{{"maxImpressions", f.MaxImpressions}, {"intervalDays", f.IntervalDays}} {
		if v.n < MinFrequencyCapValue || v.n > MaxFrequencyCapValue {
			return creativeOptionInvalid("frequencyCap", "%s %d is out of range; Meta accepts %d to %d", v.name, v.n, MinFrequencyCapValue, MaxFrequencyCapValue)
		}
	}
	return nil
}

// frequencyControlSpecs is the ad set field for a cap NormalizeFrequencyCap accepted.
func frequencyControlSpecs(f *FrequencyCap) []map[string]any {
	return []map[string]any{{"event": "IMPRESSIONS", "interval_days": f.IntervalDays, "max_frequency": f.MaxImpressions}}
}

// AdSchedule is metaConfig.adSchedule (#550, gap 16): the ad set runs only on Days between
// StartMinute and EndMinute of the day, in the ad ACCOUNT's time zone. Sent as the ad set's
// `adset_schedule` (`[{"start_minute":…,"end_minute":…,"days":[…],"timezone_type":"ADVERTISER"}]`)
// with `pacing_type: ["day_parting"]` (Ad Set reference). Meta accepts a schedule only on a
// lifetime budget, so it is refused on a daily one rather than refused by Meta after the campaign
// exists.
type AdSchedule struct {
	// Days are the days of the week it runs, 0 (Sunday) to 6 (Saturday), Meta's numbering.
	Days []int `json:"days"`
	// StartMinute and EndMinute are minutes of the day, whole hours (multiples of 60), with
	// 0 <= StartMinute < EndMinute <= 1440.
	StartMinute int `json:"startMinute"`
	EndMinute   int `json:"endMinute"`
}

// Ad schedule bounds: Meta schedules in whole hours of a 1440-minute day; days are numbered 0
// (Sunday) to 6 (Saturday).
const (
	adScheduleMinutesPerDay = 1440
	adScheduleStep          = 60
	adScheduleMaxDay        = 6
	// AdScheduleTimezone is the timezone_type every schedule is sent with: the ad account's time
	// zone, so an operator reads the hours as the account (and Ads Manager) shows them.
	AdScheduleTimezone = "ADVERTISER"
	// PacingDayParting is the pacing_type a scheduled ad set needs.
	PacingDayParting = "day_parting"
)

// NormalizeAdSchedule refuses a schedule the create cannot send and returns the copy it sends
// (days de-duplicated and sorted): one on a daily budget, no day, a day outside 0..6, or minutes
// that are not whole hours with 0 <= start < end <= 1440. A nil schedule is no schedule. Every
// refusal is a domain.PreflightRefusal wrapping domain.ErrCreateConfigInvalid.
func NormalizeAdSchedule(lifetimeBudget bool, s *AdSchedule) (*AdSchedule, error) {
	if s == nil {
		return nil, nil
	}
	if !lifetimeBudget {
		return nil, creativeOptionInvalid("adSchedule", "needs a lifetime budget (Meta schedules only lifetime-budget ad sets); set lifetimeBudget true or omit adSchedule")
	}
	if len(s.Days) == 0 {
		return nil, creativeOptionInvalid("adSchedule", "days must name at least one day, 0 (Sunday) to 6 (Saturday)")
	}
	var seen [adScheduleMaxDay + 1]bool
	for _, d := range s.Days {
		if d < 0 || d > adScheduleMaxDay {
			return nil, creativeOptionInvalid("adSchedule", "day %d is out of range; days run from 0 (Sunday) to 6 (Saturday)", d)
		}
		seen[d] = true
	}
	out := &AdSchedule{StartMinute: s.StartMinute, EndMinute: s.EndMinute}
	for d, on := range seen {
		if on {
			out.Days = append(out.Days, d)
		}
	}
	if s.StartMinute < 0 || s.EndMinute > adScheduleMinutesPerDay || s.StartMinute >= s.EndMinute {
		return nil, creativeOptionInvalid("adSchedule", "startMinute %d and endMinute %d must satisfy 0 <= startMinute < endMinute <= %d", s.StartMinute, s.EndMinute, adScheduleMinutesPerDay)
	}
	if s.StartMinute%adScheduleStep != 0 || s.EndMinute%adScheduleStep != 0 {
		return nil, creativeOptionInvalid("adSchedule", "startMinute %d and endMinute %d must be whole hours (multiples of %d)", s.StartMinute, s.EndMinute, adScheduleStep)
	}
	return out, nil
}

// adSetSchedule is the ad set's adset_schedule for a schedule NormalizeAdSchedule returned.
func adSetSchedule(s *AdSchedule) []map[string]any {
	return []map[string]any{{"start_minute": s.StartMinute, "end_minute": s.EndMinute, "days": s.Days, "timezone_type": AdScheduleTimezone}}
}
