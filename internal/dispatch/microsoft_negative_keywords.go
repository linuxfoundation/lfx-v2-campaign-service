// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft Advertising negative keywords at create time (#563): microsoftConfig.negativeKeywords,
// attached to the campaign as campaign-level negatives right after CreateCampaign confirms it.
//
// The call is the one the add-negative-keywords lever already makes
// (microsoft.Client.AddCampaignNegativeKeywords, POST EntityNegativeKeywords with EntityType
// "Campaign"), so the rules are that lever's, held once in microsoft.ValidateNegativeKeywords.
// This file adds the deployment gate and the JSON-type check, turns a refusal into the
// synchronous 400 shape ("invalid campaign config: microsoftConfig.negativeKeywords: ..."), and
// applies the list after the create.
//
// Applying it never fails the job: the campaign exists by then, so an error would retain the
// claim for a reconcile that cannot re-run this step (the reasoning attachMicrosoftImage gives).
// A shortfall degrades the campaign instead, with a Steps entry naming it.

// microsoftNegativeKeywordsField is the gated microsoftConfig key.
const microsoftNegativeKeywordsField = "negativeKeywords"

// microsoftCreateNegativeKeywordsEnabled reports whether this deployment opted in. Read per call,
// like redditTargetingExclusionsEnabled, so it flips without a rebuild.
func microsoftCreateNegativeKeywordsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftCreateNegativeKeywordsEnabled) == "true"
}

// microsoftNegativeKeywords maps the wire-shaped config onto the client's type. nil for an empty
// list, so "none requested" never reads as an empty non-nil request.
func microsoftNegativeKeywords(in []microsoftKeywordConfig) []microsoft.NegativeKeyword {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.NegativeKeyword, len(in))
	for i, k := range in {
		out[i] = microsoft.NegativeKeyword{Text: k.Text, MatchType: k.MatchType}
	}
	return out
}

// microsoftNegativeKeywordConfigs is the inverse of microsoftNegativeKeywords, for the snapshot.
func microsoftNegativeKeywordConfigs(in []microsoft.NegativeKeyword) []microsoftKeywordConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoftKeywordConfig, len(in))
	for i, k := range in {
		out[i] = microsoftKeywordConfig{Text: k.Text, MatchType: k.MatchType}
	}
	return out
}

// checkMicrosoftNegativeKeywords refuses a config whose negativeKeywords list is non-empty while
// the deployment gate is off, or which breaks microsoft.ValidateNegativeKeywords. It returns the
// validated (normalized) list to send, nil when none was requested. Every refusal is a
// domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a PreflightRefusal, so a
// job that reached Dispatch copies the sentence verbatim. The gate is checked first, so it never
// depends on the values' own validity.
func checkMicrosoftNegativeKeywords(cfg microsoftConfig) ([]microsoft.NegativeKeyword, error) {
	if len(cfg.NegativeKeywords) == 0 {
		return nil, nil
	}
	if !microsoftCreateNegativeKeywordsEnabled() {
		env := constants.EnvMicrosoftCreateNegativeKeywordsEnabled
		return nil, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig.%s: disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, microsoftNegativeKeywordsField, env, microsoftNegativeKeywordsField, env))
	}
	validated, err := microsoft.ValidateNegativeKeywords(microsoftNegativeKeywords(cfg.NegativeKeywords))
	if err != nil {
		msg := strings.TrimPrefix(err.Error(), "microsoft-ads: ")
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, microsoftNegativeKeywordsField, msg))
	}
	return validated, nil
}

// checkMicrosoftNegativeKeywordsShape refuses a negativeKeywords value that is not a JSON array of
// {text, matchType} string objects, so a wrong type is the synchronous 400 rather than a job
// failure (encoding/json would otherwise fail the whole microsoftConfig decode, which
// PreflightCreate leaves to Dispatch). null counts as absent, as encoding/json reads it. Keys are
// matched case-insensitively, as encoding/json matches them, so a case variant cannot slip a
// wrong type past this check; they are visited in sorted order so the refusal is deterministic.
func checkMicrosoftNegativeKeywordsShape(config json.RawMessage) error {
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
		raw := platformCfg[k]
		if !strings.EqualFold(k, microsoftNegativeKeywordsField) || isJSONNull(raw) {
			continue
		}
		var list []microsoftKeywordConfig
		if json.Unmarshal(raw, &list) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of {\"text\": string, \"matchType\": string} objects",
				domain.ErrCreateConfigInvalid, microsoftNegativeKeywordsField))
		}
	}
	return nil
}

// applyMicrosoftNegativeKeywords attaches the validated negatives to the confirmed campaign in
// result, recording the positional outcomes and a Steps entry on result. It reports whether the
// campaign is DEGRADED: a requested negative that is not, or may not be, on the campaign. A nil
// list is a no-op. A retry that reuses the campaign re-sends the list; Microsoft answers each
// one already there with 4335, which the client reports as ALREADY_PRESENT.
func applyMicrosoftNegativeKeywords(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, negatives []microsoft.NegativeKeyword) bool {
	if len(negatives) == 0 {
		return false
	}
	outcomes, err := client.AddCampaignNegativeKeywords(ctx, result.CampaignID, negatives)
	if err != nil {
		if microsoft.IsOutcomeUnconfirmed(err) {
			result.Steps = append(result.Steps, "Negative keywords UNCONFIRMED (they may be attached — verify in Microsoft Advertising before re-adding): "+err.Error())
			slog.WarnContext(ctx, "microsoft create-time negative keywords unconfirmed", "campaign_id", result.CampaignID, "error", err)
		} else {
			result.Steps = append(result.Steps, "Negative keywords NOT attached: "+err.Error())
			slog.WarnContext(ctx, "microsoft create-time negative keywords not attached", "campaign_id", result.CampaignID, "error", err)
		}
		return true
	}
	result.NegativeKeywords = outcomes
	var applied, present, failed, unconfirmed int
	for _, o := range outcomes {
		switch o.Outcome {
		case microsoft.OutcomeApplied:
			applied++
		case microsoft.OutcomeAlreadyPresent:
			present++
		case microsoft.OutcomeFailed:
			failed++
		default:
			unconfirmed++
		}
	}
	result.Steps = append(result.Steps, fmt.Sprintf("Negative keywords: %d attached, %d already present, %d failed, %d unconfirmed", applied, present, failed, unconfirmed))
	if failed > 0 || unconfirmed > 0 {
		slog.WarnContext(ctx, "microsoft create-time negative keywords partly applied", "campaign_id", result.CampaignID,
			"failed", failed, "unconfirmed", unconfirmed)
		return true
	}
	return false
}
