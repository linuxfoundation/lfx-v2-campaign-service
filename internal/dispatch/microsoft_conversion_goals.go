// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft conversion goals and conversion bidding at create (#563, gap 7):
// microsoftConfig.conversionGoalIds names EXISTING conversion goals on the shared account (LFX
// creates no UET tag and no goal: marketing ops owns both), verified with one
// GetConversionGoalsByIds read before anything is created and set as the new campaign's GoalIds.
// microsoftConfig.bidding gains two strategies, "maxConversions" and "targetCpa" (with
// bidding.targetCpa), sent as MaxConversionsBiddingScheme, allowed only with at least one goal.
//
// Two gates, independent of each other, as for the remarketing lists (microsoft_audiences.go).
// MICROSOFT_CONVERSION_GOALS_ENABLED (here) keeps a shape no live call has exercised off the
// shared account. The global_marketing_ops check (service.MicrosoftMarketingOpsInputs) keeps a
// foundation's executive director, who is a campaign_manager, from pointing a campaign at goals
// built on the LF account for another foundation. The strategies name no resource and are not
// role-gated on their own; they cannot be sent without the goals, which are.

// microsoftConversionGoalsEnabled reports whether this deployment opted in. Read per call, like
// microsoftAudiencesEnabled, so it flips without a rebuild.
func microsoftConversionGoalsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftConversionGoalsEnabled) == "true"
}

// microsoftGoalInvalid is the refusal for a bad goal input: a 400 from PreflightCreate
// (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies verbatim
// (domain.PreflightRefusal). Service-authored text naming the field and positions only.
func microsoftGoalInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkMicrosoftConversionGoalsShape refuses a conversionGoalIds value that is not a JSON array
// of strings before any job exists. Keys are matched case-insensitively, as encoding/json matches
// them, and visited in sorted order so the refusal is deterministic. An explicit null is absent.
func checkMicrosoftConversionGoalsShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["microsoftConfig"], &platformCfg) != nil {
		return nil
	}
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.EqualFold(k, microsoft.FieldConversionGoalIDs) || isJSONNull(platformCfg[k]) {
			continue
		}
		var ids []string
		if json.Unmarshal(platformCfg[k], &ids) != nil {
			return microsoftGoalInvalid("%s: must be an array of strings", microsoft.FieldConversionGoalIDs)
		}
	}
	return nil
}

// checkMicrosoftConversionGoals applies the deployment gate and microsoft.ValidateConversionGoalIDs
// to a decoded config. Shared by PreflightCreate (a 400 before any job) and Dispatch (the same
// rules again, before any Microsoft call). Any non-empty list counts as set, so the gate never
// depends on the values' own validity.
func checkMicrosoftConversionGoals(cfg microsoftConfig) error {
	if len(cfg.ConversionGoalIDs) == 0 {
		return nil
	}
	field := microsoft.FieldConversionGoalIDs
	if !microsoftConversionGoalsEnabled() {
		env := constants.EnvMicrosoftConversionGoalsEnabled
		return microsoftGoalInvalid("%s: Microsoft conversion goals are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			field, env, field, env)
	}
	if err := microsoft.ValidateConversionGoalIDs(cfg.ConversionGoalIDs); err != nil {
		var gErr *microsoft.ConversionGoalError
		if errors.As(err, &gErr) {
			return microsoftGoalInvalid("%s: %s", gErr.Field, gErr.Msg)
		}
		return microsoftGoalInvalid("%s: invalid", field)
	}
	return nil
}

// microsoftConversionPlan resolves bidding.strategy "maxConversions" or "targetCpa" (#563 gap 7).
// Both are sent as MaxConversionsBiddingScheme (Microsoft converts a new Target CPA strategy to
// MaxConversions with the target anyway), so "targetCpa" is MaxConversions plus a required
// targetCpa. Refused: either strategy while MICROSOFT_CONVERSION_GOALS_ENABLED is off, without
// at least one conversionGoalIds entry (a conversion strategy with no goal has nothing to
// optimize toward), with bidAmount or a legacy cpcBid (the strategy sets every bid), a targetCpa
// on "maxConversions", and a targetCpa that is not a finite amount above zero, at most
// maxCreateBid, with at most two decimals (Microsoft amounts are account-currency decimals; it is
// refused, never rounded). Recorded as automatic: the platform sets every bid, so the bid lever
// answers 409 for it, as it does for MaxClicks.
func microsoftConversionPlan(cfg microsoftConfig) (bidPlan, error) {
	const key = "microsoftConfig"
	b := cfg.Bidding
	if !microsoftConversionGoalsEnabled() {
		env := constants.EnvMicrosoftConversionGoalsEnabled
		return bidPlan{}, biddingInvalid(key, "strategy %q is disabled on this deployment (%s is not \"true\"); choose \"automatic\" or \"manual\", or ask an operator to set %s to \"true\"", b.Strategy, env, env)
	}
	if b.bidAmountPresent {
		return bidPlan{}, biddingInvalid(key, "bidAmount is not accepted with strategy %q, which sets every bid; remove bidAmount", b.Strategy)
	}
	if cfg.CpcBid != 0 {
		return bidPlan{}, biddingInvalid(key, "cpcBid sets a manual ad group bid, which strategy %q ignores; remove cpcBid", b.Strategy)
	}
	if len(cfg.ConversionGoalIDs) == 0 {
		return bidPlan{}, biddingInvalid(key, "strategy %q needs at least one existing conversion goal in microsoftConfig.%s to optimize toward", b.Strategy, microsoft.FieldConversionGoalIDs)
	}
	if b.Strategy == biddingStrategyMaxConversions {
		if b.targetCpaPresent {
			return bidPlan{}, biddingInvalid(key, "targetCpa is only accepted with strategy \"targetCpa\"; remove it, or choose \"targetCpa\"")
		}
		return bidPlan{strategy: model.BiddingAutomatic, conversions: true}, nil
	}
	if b.TargetCpa == nil {
		return bidPlan{}, biddingInvalid(key, "strategy \"targetCpa\" requires targetCpa, the target cost per acquisition in the ad account's currency")
	}
	t := *b.TargetCpa
	switch {
	case math.IsNaN(t) || math.IsInf(t, 0):
		return bidPlan{}, biddingInvalid(key, "targetCpa must be a finite number")
	case t <= 0:
		return bidPlan{}, biddingInvalid(key, "targetCpa must be greater than zero")
	case t > maxCreateBid:
		return bidPlan{}, biddingInvalid(key, "targetCpa %s exceeds the maximum %s this service sets", formatBid(t), formatBid(maxCreateBid))
	}
	if _, ok := exactScaled(t, 100); !ok {
		return bidPlan{}, biddingInvalid(key, "targetCpa %s has more than two decimal places; Microsoft Advertising amounts are set in the account currency to at most two, so it is refused rather than rounded", formatBid(t))
	}
	return bidPlan{strategy: model.BiddingAutomatic, conversions: true, targetCPA: t}, nil
}

// ListMicrosoftConversionGoals lists the conversion goals on the account a Microsoft create for
// the project uses (#563 gap 7 picker), behind the create's own MICROSOFT_CONVERSION_GOALS_ENABLED.
// Credentials resolve and validate exactly as for a create, so every Active goal listed is one the
// create's pre-create check (CheckConversionGoals, the same call with ids) accepts. The gateway's
// marketing-ops-api rule admits only global_marketing_ops to the route: the shared LF account holds
// every foundation's goals.
//
// called reports whether Microsoft was contacted: false for the deployment gate and for a local
// refusal before the request (no connection, an unusable one).
func (d *MicrosoftDispatcher) ListMicrosoftConversionGoals(ctx context.Context, projectID string, platform model.Provider) (_ *model.MicrosoftConversionGoals, called bool, _ error) {
	if !microsoftConversionGoalsEnabled() {
		return nil, false, fmt.Errorf("microsoft conversion goals are disabled (%s is not \"true\") until the read is exercised against a live account: %w",
			constants.EnvMicrosoftConversionGoalsEnabled, domain.ErrTargetingLookupUnsupported)
	}
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	creds, accountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, false, err
	}
	client := d.cachedMicrosoftClient(projectID, platform, res, creds, accountID)
	rows, lerr := client.ListConversionGoals(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no goal is a real, empty answer.
	out := make([]model.MicrosoftConversionGoal, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.MicrosoftConversionGoal{ID: r.ID, Name: r.Name, Status: r.Status, Type: r.Type, Scope: r.Scope})
	}
	return &model.MicrosoftConversionGoals{AccountID: client.AccountID(), Goals: out}, true, nil
}
