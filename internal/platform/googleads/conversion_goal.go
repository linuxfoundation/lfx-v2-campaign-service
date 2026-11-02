// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Campaign-level conversion goals (#436).
//
// A campaign the operator gave conversion actions bids toward EXACTLY those actions, not the
// account's primary goal set. Google expresses that as two resources, both written after the
// campaign exists:
//
//  1. a CustomConversionGoal holding the chosen actions. It is an ACCOUNT-level resource, so
//     it is shared: its name is derived from the action set, and every LFX campaign naming the
//     same set reuses the one goal rather than each creating its own.
//  2. the campaign's ConversionGoalCampaignConfig, updated to goal_config_level CAMPAIGN with
//     that goal. The config exists for every campaign and is update-only, addressed as
//     customers/<cid>/conversionGoalCampaignConfigs/<campaignId>.
//
// This replaces campaign.selective_optimization, which this client used to send at create
// time on Search, Video and Display. Google documents that field as App-campaign only, and it
// never ran against the live account; this path works on every channel, Demand Gen and
// Performance Max included.
//
// Both writes happen after the campaign create, so a failure is a PARTIAL result like the geo
// and extension steps: the campaign exists, PAUSED, bidding toward the account's goals, and
// the error says the goal was not applied rather than reporting the operator's choice as done.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const (
	// customConversionGoalNamePrefix marks a goal this client created, so a reader of the
	// account can tell an LFX goal from one made by hand.
	customConversionGoalNamePrefix = "LFX | conversion goal | "

	// errCodeDuplicateCustomGoalName is Google's CustomConversionGoalError code when a goal
	// name already exists: another create for the same action set won the race.
	errCodeDuplicateCustomGoalName = "DUPLICATE_NAME"

	goalConfigLevelCampaign = "CAMPAIGN"
	customGoalStatusEnabled = "ENABLED"
)

// customConversionGoalName is the deterministic name for one action SET. Sorted first, so
// the order the operator listed them in does not create a second goal for the same set.
func customConversionGoalName(actions []string) string {
	sorted := slices.Clone(actions)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return customConversionGoalNamePrefix + hex.EncodeToString(sum[:])[:16]
}

// customConversionGoalWrite is the create payload, and the update payload when an existing
// goal has to be re-enabled or re-pointed at the set its name stands for.
type customConversionGoalWrite struct {
	ResourceName      string   `json:"resourceName,omitempty"`
	Name              string   `json:"name,omitempty"`
	ConversionActions []string `json:"conversionActions"`
	Status            string   `json:"status"`
}

// conversionGoalCampaignConfigUpdate points one campaign at a custom goal.
type conversionGoalCampaignConfigUpdate struct {
	ResourceName         string `json:"resourceName"`
	GoalConfigLevel      string `json:"goalConfigLevel"`
	CustomConversionGoal string `json:"customConversionGoal"`
}

type gaqlCustomConversionGoalRow struct {
	CustomConversionGoal struct {
		ResourceName      string   `json:"resourceName"`
		Status            string   `json:"status"`
		ConversionActions []string `json:"conversionActions"`
	} `json:"customConversionGoal"`
}

// applyCampaignConversionGoal makes campaignID bid toward plan.conversionActions and returns
// the step line that says so. It does nothing, and returns "", when no actions were named:
// the campaign then inherits the account's goals, as every campaign before #436 did.
func (c *Client) applyCampaignConversionGoal(ctx context.Context, campaignID string, plan biddingPlan) (string, error) {
	actions := plan.conversionActions
	if len(actions) == 0 {
		return "", nil
	}
	goalResource, reused, err := c.ensureCustomConversionGoal(ctx, actions)
	if err != nil {
		return "", fmt.Errorf("google-ads conversion goal NOT applied to campaign %s (it bids toward the account's conversion goals until one is set in Google Ads): %w", campaignID, err)
	}

	configResource := "customers/" + c.account.CustomerID + "/conversionGoalCampaignConfigs/" + campaignID
	req := mutateRequest{Operations: []mutateOperation{{
		Update: conversionGoalCampaignConfigUpdate{
			ResourceName:         configResource,
			GoalConfigLevel:      goalConfigLevelCampaign,
			CustomConversionGoal: goalResource,
		},
		UpdateMask: "goal_config_level,custom_conversion_goal",
	}}}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("conversionGoalCampaignConfigs:mutate"), req, true)
	if err != nil {
		return "", fmt.Errorf("google-ads conversion goal NOT applied to campaign %s (custom goal %s exists; the campaign bids toward the account's conversion goals until it is pointed at that goal in Google Ads): %w", campaignID, goalResource, err)
	}
	// The result must name THIS campaign's config. A 2xx naming another one would report a
	// goal as applied to a campaign it never reached.
	if got, _, rErr := firstResourceName(resp); rErr != nil || got != configResource {
		return "", fmt.Errorf("google-ads conversion goal UNCONFIRMED for campaign %s (the config update answered 2xx without naming %s; verify the campaign's conversion goals in Google Ads)", campaignID, configResource)
	}

	how := "created"
	if reused {
		how = "reused"
	}
	goalID := resourceID(goalResource)
	if !plan.biddingOptimizesConversions() {
		return fmt.Sprintf("Conversion goal applied: %d conversion action(s) are this campaign's conversions (custom goal %s, %s); its %s bidding does not optimize toward conversions", len(actions), goalID, how, plan.describe()), nil
	}
	return fmt.Sprintf("Conversion goal applied: campaign optimizes toward %d conversion action(s) instead of the account goals (custom goal %s, %s)", len(actions), goalID, how), nil
}

// ensureCustomConversionGoal returns the enabled custom goal holding exactly actions, reusing
// the one named for this set when it exists and creating it otherwise. reused reports which.
func (c *Client) ensureCustomConversionGoal(ctx context.Context, actions []string) (resource string, reused bool, err error) {
	name := customConversionGoalName(actions)
	resource, err = c.reconcileCustomConversionGoal(ctx, name, actions)
	if err != nil || resource != "" {
		return resource, resource != "", err
	}

	req := mutateRequest{Operations: []mutateOperation{{Create: customConversionGoalWrite{
		Name:              name,
		ConversionActions: actions,
		Status:            customGoalStatusEnabled,
	}}}}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("customConversionGoals:mutate"), req, false)
	if err != nil {
		// Another create for the same set got there first. Its goal is the one to use.
		var ae *apiError
		if errors.As(err, &ae) && isDefiniteClientError(ae) && ae.hasErrorCode(errCodeDuplicateCustomGoalName) {
			resource, rErr := c.reconcileCustomConversionGoal(ctx, name, actions)
			if rErr != nil {
				return "", false, rErr
			}
			if resource != "" {
				return resource, true, nil
			}
		}
		return "", false, fmt.Errorf("create custom conversion goal %q: %w", name, err)
	}
	resource, _, err = firstResourceName(resp)
	if err != nil {
		return "", false, fmt.Errorf("create custom conversion goal %q answered 2xx without a usable resource name: %w", name, err)
	}
	if err := c.validateResourceKind("customConversionGoals", resource, true); err != nil {
		return "", false, fmt.Errorf("create custom conversion goal %q: %w", name, err)
	}
	return resource, false, nil
}

// reconcileCustomConversionGoal looks the goal up by name and returns its resource, or ""
// when no goal has that name. A goal found removed, or holding a different set (edited by
// hand), is put back to the set its name stands for before it is used.
func (c *Client) reconcileCustomConversionGoal(ctx context.Context, name string, actions []string) (string, error) {
	literal, err := gaqlNameLiteral("custom conversion goal", name)
	if err != nil {
		return "", err
	}
	rows, err := c.gaqlSearch(ctx, "SELECT custom_conversion_goal.resource_name, custom_conversion_goal.status, custom_conversion_goal.conversion_actions FROM custom_conversion_goal WHERE custom_conversion_goal.name = "+literal)
	if err != nil {
		return "", fmt.Errorf("look up custom conversion goal %q: %w", name, err)
	}
	switch len(rows) {
	case 0:
		return "", nil
	case 1:
	default:
		return "", fmt.Errorf("look up custom conversion goal %q: %d goals share the name, which Google keeps unique", name, len(rows))
	}
	var row gaqlCustomConversionGoalRow
	if err := json.Unmarshal(rows[0], &row); err != nil {
		return "", fmt.Errorf("decode custom conversion goal %q: %w", name, err)
	}
	goal := row.CustomConversionGoal
	if err := c.validateResourceKind("customConversionGoals", goal.ResourceName, true); err != nil {
		return "", fmt.Errorf("look up custom conversion goal %q: %w", name, err)
	}
	if goal.Status == customGoalStatusEnabled && sameActionSet(goal.ConversionActions, actions) {
		return goal.ResourceName, nil
	}

	req := mutateRequest{Operations: []mutateOperation{{
		Update: customConversionGoalWrite{
			ResourceName:      goal.ResourceName,
			ConversionActions: actions,
			Status:            customGoalStatusEnabled,
		},
		UpdateMask: "conversion_actions,status",
	}}}
	if _, err := c.doRequest(ctx, http.MethodPost, c.customerPath("customConversionGoals:mutate"), req, true); err != nil {
		return "", fmt.Errorf("restore custom conversion goal %q (status %s, %d action(s)): %w", name, goal.Status, len(goal.ConversionActions), err)
	}
	return goal.ResourceName, nil
}

func sameActionSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
