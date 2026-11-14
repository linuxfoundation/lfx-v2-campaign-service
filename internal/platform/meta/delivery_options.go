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
