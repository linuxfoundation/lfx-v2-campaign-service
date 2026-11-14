// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditDailyBudgetEnabled reports whether this deployment opted in to redditConfig.budgetType
// (#549 gap 9). Read per call, like REDDIT_CONVERSION_EVENT_ENABLED, so it flips without a
// rebuild. See constants.EnvRedditDailyBudgetEnabled for why it is off.
func redditDailyBudgetEnabled() bool {
	return os.Getenv(constants.EnvRedditDailyBudgetEnabled) == "true"
}

// redditBudgetTypeInvalid is the refusal for a budgetType the service will not send: an
// ErrCreateConfigInvalid (PreflightCreate answers 400 before any job exists) and a
// domain.PreflightRefusal (a Dispatch that reaches it copies the sentence into the job verbatim).
func redditBudgetTypeInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, reddit.FieldBudgetType, fmt.Sprintf(format, args...)))
}

// checkRedditBudgetType applies every budgetType rule, shared by PreflightCreate and Dispatch. It
// reads the RAW redditConfig, as checkRedditConversionEvent does, because the decoded struct
// cannot tell an absent field from `null` or `""` and encoding/json matches keys
// case-insensitively: only the exact key is accepted, and present it must be a non-blank string.
// A present field then needs the deployment gate — "lifetime" included, so the gate never depends
// on whether the value happens to be the default — and must be "lifetime" or "daily"
// (reddit.ResolveBudgetGoalType). An absent field is never refused, whatever the gate.
func checkRedditBudgetType(config json.RawMessage) error {
	raw, present, err := redditConfigField(config, reddit.FieldBudgetType)
	if err != nil || !present {
		return err
	}
	var budgetType string
	if isJSONNull(raw) || json.Unmarshal(raw, &budgetType) != nil {
		return redditBudgetTypeInvalid("must be a string, %q or %q; omit it for the default, %q", reddit.BudgetTypeLifetime, reddit.BudgetTypeDaily, reddit.BudgetTypeLifetime)
	}
	if strings.TrimSpace(budgetType) == "" {
		return redditBudgetTypeInvalid("an empty value is refused; omit the field for the default, %q", reddit.BudgetTypeLifetime)
	}
	if !redditDailyBudgetEnabled() {
		return redditBudgetTypeInvalid("disabled on this deployment (%s is not \"true\"); omit budgetType, or ask an operator to set %s to \"true\"",
			constants.EnvRedditDailyBudgetEnabled, constants.EnvRedditDailyBudgetEnabled)
	}
	if _, err := reddit.ResolveBudgetGoalType(budgetType); err != nil {
		return redditBudgetTypeInvalid("%s", err.Error())
	}
	return nil
}
