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
