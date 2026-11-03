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
//     for exactly that set is reused. No goal in use is ever written: the account is shared by
//     every foundation, so an enabled goal may be counting conversions for live campaigns
//     elsewhere. The one write is re-enabling a REMOVED goal that holds exactly the set, even one
//     someone removed by hand, which is safe because Google refuses to remove a goal linked to a campaign
//     (CANNOT_REMOVE_LINKED_CUSTOM_CONVERSION_GOAL), so a removed goal counts for no campaign.
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

	// customConversionGoalSecondName is appended to a set's name when the goal holding that
	// name was edited to hold something else and no other goal holds the set. That goal is left
	// as it is, and the set gets a goal of its own under this second name, which later campaigns
	// find the same way.
	customConversionGoalSecondName = " | 2"

	// goalHeldElsewhere is the step wording for a set some other enabled goal already holds.
	goalHeldElsewhere = "reused: an existing goal already holds these actions"

	// goalReenabled is the step wording for a removed goal holding the set, re-enabled.
	goalReenabled = "re-enabled: a removed goal held these actions"

	customGoalStatusRemoved = "REMOVED"

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

// customConversionGoalWrite is the create payload.
type customConversionGoalWrite struct {
	Name              string   `json:"name"`
	ConversionActions []string `json:"conversionActions"`
	Status            string   `json:"status"`
}

// customConversionGoalStatusUpdate re-enables a removed goal. Only status is written, so the
// action list it held is untouched.
type customConversionGoalStatusUpdate struct {
	ResourceName string `json:"resourceName"`
	Status       string `json:"status"`
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

// goalMutateRequest is the body of the goal step's one googleAds:mutate.
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

// gaqlCustomConversionGoalRow is one custom_conversion_goal GAQL row, as Google returns it.
type gaqlCustomConversionGoalRow struct {
	CustomConversionGoal struct {
		ResourceName      string   `json:"resourceName"`
		Status            string   `json:"status"`
		ConversionActions []string `json:"conversionActions"`
	} `json:"customConversionGoal"`
}

// gaqlCampaignConversionGoalRow is one campaign_conversion_goal GAQL row: a goal the campaign
// inherited from the account, and whether it is still biddable.
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
		return "", &unconfirmedGoalError{err: fmt.Errorf("google-ads conversion goal UNCONFIRMED for campaign %s (the update answered 2xx without naming %s and each of its %d account goal(s); verify the campaign's conversion goals in Google Ads; %s)", campaignID, configResource, len(competing), goalStoppedHere)}
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

// ensureCustomConversionGoal returns an enabled custom goal holding exactly actions, and says
// how it came by it. It never writes to a goal in use (see the file comment):
//   - the LFX goal named for this set, when it is enabled and still holds the set;
//   - when that name is held by a goal that was removed or edited, the goal holding exactly the
//     set under any name: reused when enabled, re-enabled when removed (which covers the LFX
//     goal itself); with none, a new goal under the set's second name;
//   - when no goal has the name, a new goal under it. Google refuses one whose action list
//     another goal already holds, and that goal is used the same way.
func (c *Client) ensureCustomConversionGoal(ctx context.Context, actions []string) (resource, how string, err error) {
	name := customConversionGoalName(actions)
	existing, err := c.customConversionGoalNamed(ctx, name)
	if err != nil {
		return "", "", err
	}
	if existing == nil {
		return c.createCustomConversionGoal(ctx, name, "created", actions)
	}
	if goalHolds(existing, actions) {
		return existing.ResourceName, "reused", nil
	}
	holder, err := c.goalHoldingActionSet(ctx, actions)
	if err != nil {
		return "", "", err
	}
	how = "created under its second name: the goal named for this set was edited, and is left as it is"
	if holder != nil {
		resource, holderHow, err := c.useGoalHoldingActionSet(ctx, holder)
		if err == nil || !definitelyRefused(err) {
			return resource, holderHow, err
		}
		// Google definitely refused to re-enable the removed holder (whether a removed goal's
		// status can be updated is not verified live), so nothing changed: the set gets its own
		// goal under the second name instead, as when the named goal was edited.
		how = "created under its second name: Google refused to re-enable the removed goal holding these actions"
	}
	return c.createCustomConversionGoal(ctx, name+customConversionGoalSecondName, how, actions)
}

// createCustomConversionGoal creates the goal name for actions. When Google refuses it because
// another create for the same name won a race, or because another goal already holds the same
// action list, that goal is used instead when it holds exactly the set: as it is when enabled,
// re-enabled when removed (see useGoalHoldingActionSet).
func (c *Client) createCustomConversionGoal(ctx context.Context, name, how string, actions []string) (string, string, error) {
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
				winner, lErr := c.customConversionGoalNamed(ctx, name)
				if lErr != nil {
					return "", "", lErr
				}
				if winner != nil && goalHolds(winner, actions) {
					return winner.ResourceName, "reused", nil
				}
				// A removed goal under the name that holds the set is used as any removed holder is.
				if winner != nil && winner.Status == customGoalStatusRemoved && sameActionSet(winner.ConversionActions, actions) {
					return c.useGoalHoldingActionSet(ctx, winner)
				}
			case ae.hasErrorCode(errCodeDuplicateCustomGoalActions):
				holder, fErr := c.goalHoldingActionSet(ctx, actions)
				if fErr != nil {
					return "", "", fErr
				}
				if holder != nil {
					return c.useGoalHoldingActionSet(ctx, holder)
				}
			}
		}
		return "", "", fmt.Errorf("create custom conversion goal %q: %w", name, err)
	}
	// A 2xx without a usable name may still have created the goal, so it is UNCONFIRMED about
	// the goal. The campaign is not pointed at anything here, so the caller still reports the
	// goal NOT applied to it, and a retry finds the goal by this deterministic name and reuses it.
	resource, _, err := firstResourceName(resp)
	if err == nil {
		err = c.validateResourceKind("customConversionGoals", resource, true)
	}
	if err != nil {
		return "", "", &unconfirmedGoalError{err: fmt.Errorf("create custom conversion goal %q answered 2xx without a usable resource name, so the goal may exist; a retry finds it by name and reuses it: %w", name, err)}
	}
	return resource, how, nil
}

// customConversionGoalNamed returns the goal with this exact name, or nil when there is none.
func (c *Client) customConversionGoalNamed(ctx context.Context, name string) (*customConversionGoalRow, error) {
	literal, err := gaqlNameLiteral("custom conversion goal", name)
	if err != nil {
		return nil, err
	}
	rows, err := c.gaqlSearch(ctx, "SELECT custom_conversion_goal.resource_name, custom_conversion_goal.status, custom_conversion_goal.conversion_actions FROM custom_conversion_goal WHERE custom_conversion_goal.status IN ('ENABLED', 'REMOVED') AND custom_conversion_goal.name = "+literal)
	if err != nil {
		return nil, fmt.Errorf("look up custom conversion goal %q: %w", name, err)
	}
	switch len(rows) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, fmt.Errorf("look up custom conversion goal %q: %d goals share the name, which Google keeps unique", name, len(rows))
	}
	goal, err := c.decodeCustomConversionGoal(rows[0])
	if err != nil {
		return nil, fmt.Errorf("look up custom conversion goal %q: %w", name, err)
	}
	return goal, nil
}

// goalHoldingActionSet finds the goal holding exactly actions, whatever its name and status:
// an enabled one when there is one, else the first removed one, else nil. Google refuses a second
// goal with the same action list, so at most one enabled match is expected. Any other status
// (UNKNOWN, UNSPECIFIED) is not used.
func (c *Client) goalHoldingActionSet(ctx context.Context, actions []string) (*customConversionGoalRow, error) {
	// REMOVED is asked for explicitly, so the lookup never depends on whether Google returns
	// removed rows to an unfiltered query.
	rows, err := c.gaqlSearch(ctx, "SELECT custom_conversion_goal.resource_name, custom_conversion_goal.status, custom_conversion_goal.conversion_actions FROM custom_conversion_goal WHERE custom_conversion_goal.status IN ('ENABLED', 'REMOVED')")
	if err != nil {
		return nil, fmt.Errorf("look up the custom conversion goal holding the same actions: %w", err)
	}
	var removed *customConversionGoalRow
	for _, raw := range rows {
		goal, err := c.decodeCustomConversionGoal(raw)
		if err != nil {
			return nil, fmt.Errorf("look up the custom conversion goal holding the same actions: %w", err)
		}
		if !sameActionSet(goal.ConversionActions, actions) {
			continue
		}
		switch goal.Status {
		case customGoalStatusEnabled:
			return goal, nil
		case customGoalStatusRemoved:
			if removed == nil {
				removed = goal
			}
		}
	}
	return removed, nil
}

// useGoalHoldingActionSet returns holder for the campaign, re-enabling it first when it is
// removed. An enabled holder may be a goal someone made by hand that other campaigns bid on, so
// it is only read. A removed one is linked to no campaign, since Google refuses to remove a
// linked goal, so re-enabling it changes no live campaign's counting.
func (c *Client) useGoalHoldingActionSet(ctx context.Context, holder *customConversionGoalRow) (string, string, error) {
	if holder.Status == customGoalStatusEnabled {
		return holder.ResourceName, goalHeldElsewhere, nil
	}
	req := mutateRequest{Operations: []mutateOperation{{
		Update:     customConversionGoalStatusUpdate{ResourceName: holder.ResourceName, Status: customGoalStatusEnabled},
		UpdateMask: "status",
	}}}
	resp, retries, err := c.doRequestCounted(ctx, http.MethodPost, c.customerPath("customConversionGoals:mutate"), req, true)
	if err != nil {
		return "", "", fmt.Errorf("re-enable removed custom conversion goal %s: %w", holder.ResourceName, retriedUnconfirmed("custom conversion goal re-enable", retries, err))
	}
	// The re-enable must name this goal before the campaign is pointed at it.
	if got, _, rErr := firstResourceName(resp); rErr != nil || got != holder.ResourceName {
		return "", "", &unconfirmedGoalError{err: fmt.Errorf("re-enable removed custom conversion goal %s answered 2xx without naming it; verify the goal in Google Ads", holder.ResourceName)}
	}
	return holder.ResourceName, goalReenabled, nil
}

// customConversionGoalRow is one custom_conversion_goal GAQL row, decoded.
type customConversionGoalRow struct {
	ResourceName      string
	Status            string
	ConversionActions []string
}

// decodeCustomConversionGoal decodes one row and checks it names a goal in this account.
func (c *Client) decodeCustomConversionGoal(raw json.RawMessage) (*customConversionGoalRow, error) {
	var row gaqlCustomConversionGoalRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, fmt.Errorf("decode custom conversion goal: %w", err)
	}
	g := row.CustomConversionGoal
	if err := c.validateResourceKind("customConversionGoals", g.ResourceName, true); err != nil {
		return nil, err
	}
	return &customConversionGoalRow{ResourceName: g.ResourceName, Status: g.Status, ConversionActions: g.ConversionActions}, nil
}

// goalHolds reports whether goal is enabled and holds exactly actions, so it can be used as is.
func goalHolds(goal *customConversionGoalRow, actions []string) bool {
	return goal.Status == customGoalStatusEnabled && sameActionSet(goal.ConversionActions, actions)
}

// definitelyRefused reports whether err is a definite client refusal that changed nothing: a
// 4xx other than 429 that no earlier retried attempt could have applied.
func definitelyRefused(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && isDefiniteClientError(ae) && !IsOutcomeUnconfirmed(err)
}

// unconfirmedGoalError marks a goal write Google ACCEPTED but did not acknowledge usably: the
// atomic campaign write, a goal create, or a re-enable. It may have been applied, so it satisfies the
// Unconfirmed() interface IsOutcomeUnconfirmed honours, as the bid, budget and status updates'
// own markers do.
type unconfirmedGoalError struct{ err error }

func (e *unconfirmedGoalError) Error() string     { return e.err.Error() }
func (e *unconfirmedGoalError) Unwrap() error     { return e.err }
func (e *unconfirmedGoalError) Unconfirmed() bool { return true }

// sameActionSet reports whether a and b hold the same actions, in any order.
func sameActionSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
