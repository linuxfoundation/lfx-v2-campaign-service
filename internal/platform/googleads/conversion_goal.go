// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Campaign-level conversion goals (#436).
//
// A campaign the operator gave conversion actions bids toward EXACTLY those actions, not the
// account's primary goal set. Google expresses that in three parts, all written after the
// campaign exists:
//
//  1. a CustomConversionGoal holding the chosen actions. It is an ACCOUNT-level resource, so
//     it is shared: its name is derived from the action set, and every LFX campaign naming the
//     same set reuses the one goal rather than each creating its own. Google also refuses a
//     second goal holding the same action list under any name, so a goal someone made by hand
//     for exactly that set is reused as it stands, never edited.
//  2. the campaign's ConversionGoalCampaignConfig, updated to goal_config_level CAMPAIGN with
//     that goal. The config exists for every campaign and is update-only, addressed as
//     customers/<cid>/conversionGoalCampaignConfigs/<campaignId>.
//  3. every CampaignConversionGoal the campaign inherited from the account set biddable=false.
//     The custom goal is ADDED to what the campaign bids toward; without this the account's
//     goals stay biddable beside it, which is Google's documented requirement for bidding only
//     on the custom goal's actions.
//
// Parts 2 and 3 go in ONE googleAds:mutate, so they land together or not at all. Google refuses
// part 3 on its own (EMPTY_CONVERSION_GOALS: a campaign switched to campaign goals with none
// biddable), and either half alone would leave the campaign bidding on something the operator
// did not choose.
//
// This replaces campaign.selective_optimization, which this client used to send at create
// time on Search, Video and Display. Google documents that field as App-campaign only, and it
// never ran against the live account; this path works on every channel, Demand Gen and
// Performance Max included.
//
// The goal step is the FIRST after the campaign create, and a failure returns there, so the
// error says the create stopped with nothing built past the campaign. It is a PARTIAL result:
// the campaign exists, PAUSED, and the error says the goal was NOT applied (a definite
// refusal, or any failure before the atomic write, which alone changes what the campaign bids
// toward) or is UNCONFIRMED (an atomic write whose outcome is unknown), rather than reporting
// the operator's choice as done.

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
	errCodeDuplicateCustomGoalName = "CUSTOM_GOAL_DUPLICATE_NAME"

	// errCodeDuplicateCustomGoalActions is Google's CustomConversionGoalError code when a goal
	// holding the same action list already exists, whatever its name.
	errCodeDuplicateCustomGoalActions = "DUPLICATE_CONVERSION_ACTION_LIST"

	goalConfigLevelCampaign = "CAMPAIGN"
	customGoalStatusEnabled = "ENABLED"

	// goalStoppedHere is what a goal-step failure adds about the rest of the create: the step
	// runs first after the campaign, and every channel returns on its failure.
	goalStoppedHere = "the create stopped here, so nothing past the campaign was built: no location targeting, ad group, asset group or ads"
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

// campaignConversionGoalUpdate takes one inherited account goal out of the campaign's bidding.
type campaignConversionGoalUpdate struct {
	ResourceName string `json:"resourceName"`
	Biddable     bool   `json:"biddable"`
}

// goalMutateOperation is one googleAds:mutate operation of the goal step's atomic write:
// exactly one arm is set.
type goalMutateOperation struct {
	ConversionGoalCampaignConfigOperation *mutateOperation `json:"conversionGoalCampaignConfigOperation,omitempty"`
	CampaignConversionGoalOperation       *mutateOperation `json:"campaignConversionGoalOperation,omitempty"`
}

type goalMutateRequest struct {
	MutateOperations []goalMutateOperation `json:"mutateOperations"`
}

// goalMutateResponse is index-aligned with the request's mutateOperations.
type goalMutateResponse struct {
	MutateOperationResponses []struct {
		ConversionGoalCampaignConfigResult *resourceNameResult `json:"conversionGoalCampaignConfigResult"`
		CampaignConversionGoalResult       *resourceNameResult `json:"campaignConversionGoalResult"`
	} `json:"mutateOperationResponses"`
}

type gaqlCustomConversionGoalRow struct {
	CustomConversionGoal struct {
		ResourceName      string   `json:"resourceName"`
		Status            string   `json:"status"`
		ConversionActions []string `json:"conversionActions"`
	} `json:"customConversionGoal"`
}

type gaqlCampaignConversionGoalRow struct {
	CampaignConversionGoal struct {
		ResourceName string `json:"resourceName"`
		Biddable     bool   `json:"biddable"`
	} `json:"campaignConversionGoal"`
}

// applyCampaignConversionGoal makes campaignID bid toward plan.conversionActions and returns
// the step line that says so. It does nothing, and returns "", when no actions were named:
// the campaign then inherits the account's goals, as every campaign before #436 did.
func (c *Client) applyCampaignConversionGoal(ctx context.Context, campaignID string, plan biddingPlan) (string, error) {
	actions := plan.conversionActions
	if len(actions) == 0 {
		return "", nil
	}
	notApplied := func(err error) error {
		return fmt.Errorf("google-ads conversion goal NOT applied to campaign %s (it bids toward the account's conversion goals until one is set in Google Ads; %s): %w", campaignID, goalStoppedHere, err)
	}
	goalResource, how, err := c.ensureCustomConversionGoal(ctx, actions)
	if err != nil {
		return "", notApplied(err)
	}
	competing, err := c.biddableCampaignConversionGoals(ctx, campaignID)
	if err != nil {
		return "", notApplied(err)
	}

	configResource := "customers/" + c.account.CustomerID + "/conversionGoalCampaignConfigs/" + campaignID
	ops := []goalMutateOperation{{ConversionGoalCampaignConfigOperation: &mutateOperation{
		Update: conversionGoalCampaignConfigUpdate{
			ResourceName:         configResource,
			GoalConfigLevel:      goalConfigLevelCampaign,
			CustomConversionGoal: goalResource,
		},
		UpdateMask: "goal_config_level,custom_conversion_goal",
	}}}
	for _, rn := range competing {
		ops = append(ops, goalMutateOperation{CampaignConversionGoalOperation: &mutateOperation{
			Update:     campaignConversionGoalUpdate{ResourceName: rn, Biddable: false},
			UpdateMask: "biddable",
		}})
	}
	resp, retries, err := c.doRequestCounted(ctx, http.MethodPost, c.customerPath("googleAds:mutate"), goalMutateRequest{MutateOperations: ops}, true)
	if err != nil {
		// Only a definite refusal proves the campaign still bids toward the account's goals. A
		// 5xx, a transport failure, an exhausted 429 or a refusal after a retried 429 may have
		// been applied, so it is reported UNCONFIRMED, as the geo step reports an unknown outcome.
		err = retriedUnconfirmed("campaign conversion goal update", retries, err)
		if IsOutcomeUnconfirmed(err) {
			return "", fmt.Errorf("google-ads conversion goal UNCONFIRMED for campaign %s (the campaign may already bid only toward custom goal %s; verify its conversion goals in Google Ads before retrying; %s): %w", campaignID, goalResource, goalStoppedHere, err)
		}
		return "", fmt.Errorf("google-ads conversion goal NOT applied to campaign %s (custom goal %s exists; the campaign bids toward the account's conversion goals until it is pointed at that goal in Google Ads; %s): %w", campaignID, goalResource, goalStoppedHere, err)
	}
	// The write is atomic, so a 2xx applied all of it. The results must still name THIS
	// campaign's config and each goal taken out of bidding, in order: a 2xx naming anything
	// else would report a goal as applied to a campaign it never reached.
	if !goalMutateConfirmed(resp, configResource, competing) {
		return "", fmt.Errorf("google-ads conversion goal UNCONFIRMED for campaign %s (the update answered 2xx without naming %s and each of its %d account goal(s); verify the campaign's conversion goals in Google Ads; %s)", campaignID, configResource, len(competing), goalStoppedHere)
	}

	goalID := resourceID(goalResource)
	if !plan.biddingOptimizesConversions() {
		return fmt.Sprintf("Conversion goal applied: %d conversion action(s) are this campaign's conversions (custom goal %s, %s; %d account goal(s) set not biddable); its %s bidding does not optimize toward conversions", len(actions), goalID, how, len(competing), plan.describe()), nil
	}
	return fmt.Sprintf("Conversion goal applied: campaign optimizes toward %d conversion action(s) instead of the account goals (custom goal %s, %s; %d account goal(s) set not biddable)", len(actions), goalID, how, len(competing)), nil
}

// goalMutateConfirmed reports whether a googleAds:mutate response names, in order, the config
// and then each campaign goal the request updated, and nothing else.
func goalMutateConfirmed(raw []byte, configResource string, competing []string) bool {
	var resp goalMutateResponse
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.MutateOperationResponses) != 1+len(competing) {
		return false
	}
	first := resp.MutateOperationResponses[0].ConversionGoalCampaignConfigResult
	if first == nil || first.ResourceName != configResource {
		return false
	}
	for i, rn := range competing {
		got := resp.MutateOperationResponses[i+1].CampaignConversionGoalResult
		if got == nil || got.ResourceName != rn {
			return false
		}
	}
	return true
}

// biddableCampaignConversionGoals lists the campaign's inherited account goals that are still
// biddable, which the atomic write takes out of bidding. Each is checked to belong to this
// account and this campaign before it is written to.
func (c *Client) biddableCampaignConversionGoals(ctx context.Context, campaignID string) ([]string, error) {
	if !numericID(campaignID) {
		return nil, fmt.Errorf("campaign id %q is not numeric", campaignID)
	}
	rows, err := c.gaqlSearch(ctx, "SELECT campaign_conversion_goal.resource_name, campaign_conversion_goal.biddable FROM campaign_conversion_goal WHERE campaign.id = "+campaignID)
	if err != nil {
		return nil, fmt.Errorf("look up campaign %s conversion goals: %w", campaignID, err)
	}
	var out []string
	for _, raw := range rows {
		var row gaqlCampaignConversionGoalRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("decode campaign %s conversion goal: %w", campaignID, err)
		}
		g := row.CampaignConversionGoal
		if err := c.validateResourceKind("campaignConversionGoals", g.ResourceName, false); err != nil {
			return nil, fmt.Errorf("look up campaign %s conversion goals: %w", campaignID, err)
		}
		if !strings.HasPrefix(resourceID(g.ResourceName), campaignID+"~") {
			return nil, fmt.Errorf("look up campaign %s conversion goals: %q belongs to another campaign", campaignID, g.ResourceName)
		}
		if g.Biddable {
			out = append(out, g.ResourceName)
		}
	}
	return out, nil
}

// ensureCustomConversionGoal returns the enabled custom goal holding exactly actions, and says
// how it came by it: the LFX goal named for this set, created or reused, or a goal made by
// hand for exactly this set, reused as it stands.
func (c *Client) ensureCustomConversionGoal(ctx context.Context, actions []string) (resource, how string, err error) {
	name := customConversionGoalName(actions)
	resource, err = c.reconcileCustomConversionGoal(ctx, name, actions)
	if err != nil || resource != "" {
		return resource, "reused", err
	}

	req := mutateRequest{Operations: []mutateOperation{{Create: customConversionGoalWrite{
		Name:              name,
		ConversionActions: actions,
		Status:            customGoalStatusEnabled,
	}}}}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("customConversionGoals:mutate"), req, false)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && isDefiniteClientError(ae) {
			switch {
			case ae.hasErrorCode(errCodeDuplicateCustomGoalName):
				// Another create for the same set got there first. Its goal is the one to use.
				resource, rErr := c.reconcileCustomConversionGoal(ctx, name, actions)
				if rErr != nil {
					return "", "", rErr
				}
				if resource != "" {
					return resource, "reused", nil
				}
			case ae.hasErrorCode(errCodeDuplicateCustomGoalActions):
				if resource, how, ok, fErr := c.goalHoldingActionSet(ctx, actions); fErr != nil {
					return "", "", fErr
				} else if ok {
					return resource, how, nil
				}
			}
		}
		return "", "", fmt.Errorf("create custom conversion goal %q: %w", name, err)
	}
	resource, _, err = firstResourceName(resp)
	if err != nil {
		return "", "", fmt.Errorf("create custom conversion goal %q answered 2xx without a usable resource name: %w", name, err)
	}
	if err := c.validateResourceKind("customConversionGoals", resource, true); err != nil {
		return "", "", fmt.Errorf("create custom conversion goal %q: %w", name, err)
	}
	return resource, "created", nil
}

// goalHoldingActionSet finds the enabled goal Google says already holds exactly actions, for
// the DUPLICATE_CONVERSION_ACTION_LIST refusal. It is reused as it stands and never edited: it
// may be a goal someone made by hand, which other campaigns already bid on. ok is false when no
// enabled goal holds the set (a removed one can still hold it), and the refusal then stands.
func (c *Client) goalHoldingActionSet(ctx context.Context, actions []string) (resource, how string, ok bool, err error) {
	rows, err := c.gaqlSearch(ctx, "SELECT custom_conversion_goal.resource_name, custom_conversion_goal.status, custom_conversion_goal.conversion_actions FROM custom_conversion_goal WHERE custom_conversion_goal.status = 'ENABLED'")
	if err != nil {
		return "", "", false, fmt.Errorf("look up the custom conversion goal holding the same actions: %w", err)
	}
	for _, raw := range rows {
		var row gaqlCustomConversionGoalRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return "", "", false, fmt.Errorf("decode custom conversion goal: %w", err)
		}
		goal := row.CustomConversionGoal
		if goal.Status != customGoalStatusEnabled || !sameActionSet(goal.ConversionActions, actions) {
			continue
		}
		if err := c.validateResourceKind("customConversionGoals", goal.ResourceName, true); err != nil {
			return "", "", false, fmt.Errorf("look up the custom conversion goal holding the same actions: %w", err)
		}
		return goal.ResourceName, "reused: an existing goal already holds these actions", true, nil
	}
	return "", "", false, nil
}

// reconcileCustomConversionGoal looks the goal up by name and returns its resource, or ""
// when no goal has that name. A goal found removed, or holding a different set (edited by
// hand), is put back to the set its name stands for before it is used. When Google refuses that
// because another goal now holds the set, that goal is used instead and the LFX one is left.
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
	resp, retries, err := c.doRequestCounted(ctx, http.MethodPost, c.customerPath("customConversionGoals:mutate"), req, true)
	if err != nil {
		var ae *apiError
		if retries == 0 && errors.As(err, &ae) && isDefiniteClientError(ae) && ae.hasErrorCode(errCodeDuplicateCustomGoalActions) {
			if resource, _, ok, fErr := c.goalHoldingActionSet(ctx, actions); fErr != nil {
				return "", fErr
			} else if ok {
				return resource, nil
			}
		}
		return "", fmt.Errorf("restore custom conversion goal %q (status %s, %d action(s)): %w", name, goal.Status, len(goal.ConversionActions), retriedUnconfirmed("custom conversion goal restore", retries, err))
	}
	// The restore must be confirmed for THIS goal before the campaign is pointed at it.
	if got, _, rErr := firstResourceName(resp); rErr != nil || got != goal.ResourceName {
		return "", &unconfirmedGoalRestoreError{err: fmt.Errorf("restore custom conversion goal %q answered 2xx without naming %s; verify it in Google Ads", name, goal.ResourceName)}
	}
	return goal.ResourceName, nil
}

// unconfirmedGoalRestoreError marks a restore Google ACCEPTED but did not acknowledge usably.
// The goal may hold the restored set or not, so IsOutcomeUnconfirmed reports it as unknown.
type unconfirmedGoalRestoreError struct{ err error }

func (e *unconfirmedGoalRestoreError) Error() string     { return e.err.Error() }
func (e *unconfirmedGoalRestoreError) Unwrap() error     { return e.err }
func (e *unconfirmedGoalRestoreError) Unconfirmed() bool { return true }

func sameActionSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
