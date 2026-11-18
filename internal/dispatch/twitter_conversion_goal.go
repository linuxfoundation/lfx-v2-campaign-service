// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X conversion optimization at create (#564 gap 5): twitterConfig.goal (WEBSITE_CONVERSIONS) and
// twitterConfig.primaryWebEventTag, sent as the new line item's goal and primary_web_event_tag. The
// rule lives once, in twitter.NormalizeConversionGoal; this file adds the deployment gate and the
// JSON-type check, and turns a refusal into the synchronous 400 shape ("invalid campaign config:
// twitterConfig.goal: ..."), as twitter_audience_expansion.go does for gap 11.

// twitterWebsiteConversionsEnabled reports whether this deployment opted in. Read per call, like
// twitterAudienceExpansionEnabled, so it flips without a rebuild.
func twitterWebsiteConversionsEnabled() bool {
	return os.Getenv(constants.EnvTwitterWebsiteConversionsEnabled) == "true"
}

// twitterConversionGoal is the normalized pair; both empty when unset.
type twitterConversionGoal struct {
	goal string
	tag  string
}

// checkTwitterConversionGoal refuses a goal or primaryWebEventTag while the deployment gate is off,
// or a pair twitter.NormalizeConversionGoal refuses, and otherwise returns the normalized pair.
// Every refusal is a domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal. Blank values count
// as unset, so they never need the flag.
func checkTwitterConversionGoal(cfg twitterConfig) (twitterConversionGoal, error) {
	if strings.TrimSpace(cfg.Goal) == "" && strings.TrimSpace(cfg.PrimaryWebEventTag) == "" {
		return twitterConversionGoal{}, nil
	}
	if !twitterWebsiteConversionsEnabled() {
		env := constants.EnvTwitterWebsiteConversionsEnabled
		return twitterConversionGoal{}, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s and %s disabled on this deployment (%s is not \"true\"); omit them, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, twitter.FieldGoal, twitter.FieldPrimaryWebEventTag, env, env))
	}
	goal, tag, err := twitter.NormalizeConversionGoal(cfg.Goal, cfg.PrimaryWebEventTag)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return twitterConversionGoal{}, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return twitterConversionGoal{}, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return twitterConversionGoal{goal: goal, tag: tag}, nil
}

// checkTwitterConversionGoalShape refuses a goal or primaryWebEventTag that is not a JSON string, so
// a wrong type is the synchronous 400 rather than a job failure. null counts as absent, and the keys
// are matched case-insensitively, as encoding/json matches them.
func checkTwitterConversionGoalShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["twitterConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		var field string
		switch {
		case strings.EqualFold(k, twitter.FieldGoal):
			field = twitter.FieldGoal
		case strings.EqualFold(k, twitter.FieldPrimaryWebEventTag):
			field = twitter.FieldPrimaryWebEventTag
		default:
			continue
		}
		if isJSONNull(raw) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be a string", domain.ErrCreateConfigInvalid, field))
		}
	}
	return nil
}
