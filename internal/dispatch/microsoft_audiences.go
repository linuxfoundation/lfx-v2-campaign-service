// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft remarketing lists at create (#563, gap 4): microsoftConfig.audienceIds (observed,
// bid only, with an optional audienceBidAdjustment) and excludedAudienceIds, on the ad group a
// create makes or reuses. The rules live once, in microsoft.ValidateAudiences; this file adds the
// deployment gate and the JSON-type check, turns a refusal into the synchronous 400 shape
// ("invalid campaign config: microsoftConfig.<field>: ..."), and attaches the lists after the
// campaign is confirmed.
//
// Two gates, independent of each other, as for the Reddit and X audiences.
// MICROSOFT_AUDIENCES_ENABLED (here) keeps shapes no live call has exercised off the shared
// account. The global_marketing_ops check (service.MicrosoftMarketingOpsInputs) keeps a
// foundation's executive director, who is a campaign_manager, from using lists built on the LF
// account for another foundation.

// microsoftAudienceFields are the three create inputs, as the JSON keys a caller spells them.
var microsoftAudienceFields = []string{microsoft.FieldAudienceIDs, microsoft.FieldExcludedAudienceIDs, microsoft.FieldAudienceBidAdjustment}

// microsoftAudiencesEnabled reports whether this deployment opted in. Read per call, like
// microsoftAdExtensionsEnabled, so it flips without a rebuild.
func microsoftAudiencesEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftAudiencesEnabled) == "true"
}

// microsoftAudienceInvalid is the refusal for a bad audience input: a 400 from PreflightCreate
// (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies verbatim
// (domain.PreflightRefusal). Service-authored text naming fields and positions only.
func microsoftAudienceInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkMicrosoftAudiencesShape refuses a list that is not a JSON array of strings, or an
// adjustment that is not a JSON number, before any job exists. Keys are matched
// case-insensitively, as encoding/json matches them, and visited in sorted order so the refusal
// is deterministic. An explicit null is absent, as it decodes.
func checkMicrosoftAudiencesShape(config json.RawMessage) error {
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
		if isJSONNull(raw) {
			continue
		}
		switch {
		case strings.EqualFold(k, microsoft.FieldAudienceIDs), strings.EqualFold(k, microsoft.FieldExcludedAudienceIDs):
			field := microsoft.FieldAudienceIDs
			if strings.EqualFold(k, microsoft.FieldExcludedAudienceIDs) {
				field = microsoft.FieldExcludedAudienceIDs
			}
			var ids []string
			if json.Unmarshal(raw, &ids) != nil {
				return microsoftAudienceInvalid("%s: must be an array of strings", field)
			}
		case strings.EqualFold(k, microsoft.FieldAudienceBidAdjustment):
			var n float64
			if json.Unmarshal(raw, &n) != nil {
				return microsoftAudienceInvalid("%s: must be a number", microsoft.FieldAudienceBidAdjustment)
			}
		}
	}
	return nil
}

// checkMicrosoftAudiences applies the deployment gate and microsoft.ValidateAudiences to a
// decoded config. Shared by PreflightCreate (a 400 before any job) and Dispatch (the same rules
// again, before any Microsoft call, for a job enqueued before the flag was turned off). Any
// non-empty list, or any adjustment, counts as set, so the gate never depends on the values' own
// validity.
func checkMicrosoftAudiences(cfg microsoftConfig) error {
	var set []string
	if len(cfg.AudienceIDs) > 0 {
		set = append(set, microsoft.FieldAudienceIDs)
	}
	if len(cfg.ExcludedAudienceIDs) > 0 {
		set = append(set, microsoft.FieldExcludedAudienceIDs)
	}
	if cfg.AudienceBidAdjustment != nil {
		set = append(set, microsoft.FieldAudienceBidAdjustment)
	}
	if len(set) == 0 {
		return nil
	}
	if !microsoftAudiencesEnabled() {
		env := constants.EnvMicrosoftAudiencesEnabled
		return microsoftAudienceInvalid("%s: Microsoft remarketing lists are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			set[0], env, strings.Join(microsoftAudienceFields, ", "), env)
	}
	if err := microsoft.ValidateAudiences(cfg.AudienceIDs, cfg.ExcludedAudienceIDs, cfg.AudienceBidAdjustment); err != nil {
		var aErr *microsoft.AudienceError
		if errors.As(err, &aErr) {
			return microsoftAudienceInvalid("%s: %s", aErr.Field, aErr.Msg)
		}
		return microsoftAudienceInvalid("%s: invalid", microsoft.FieldAudienceIDs)
	}
	return nil
}

// microsoftAudienceAdjustment is the BidMultiplier sent with each observed list: the caller's
// audienceBidAdjustment, or 0 (observe only) when it is absent.
func microsoftAudienceAdjustment(cfg microsoftConfig) float64 {
	if cfg.AudienceBidAdjustment == nil {
		return 0
	}
	return *cfg.AudienceBidAdjustment
}

// applyMicrosoftAudiences attaches the requested lists to the CONFIRMED ad group in result,
// recording the positional outcomes and Steps on result. It reports whether the campaign is
// DEGRADED: a requested list that is not, or may not be, attached. Nothing requested is a no-op.
// Like applyMicrosoftNegativeKeywords it never returns an error: the campaign exists, so a
// dispatch error would retain the claim for a reconcile that never re-runs this step.
func applyMicrosoftAudiences(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, cfg microsoftConfig) bool {
	if len(cfg.AudienceIDs) == 0 && len(cfg.ExcludedAudienceIDs) == 0 {
		return false
	}
	outcomes, err := client.AttachAdGroupAudiences(ctx, result.AdGroupID, cfg.AudienceIDs, cfg.ExcludedAudienceIDs, microsoftAudienceAdjustment(cfg))
	result.Audiences = outcomes
	if len(outcomes) == 0 {
		// Refused before any request (an ad group id this client cannot address).
		msg := "Remarketing lists NOT attached; add them to the ad group in Microsoft Advertising before enabling the campaign"
		if err != nil {
			msg += ": " + err.Error()
		}
		result.Steps = append(result.Steps, msg)
		slog.WarnContext(ctx, "microsoft create-time remarketing lists not attached", "campaign_id", result.CampaignID, "error", err)
		return true
	}
	var applied, failed, unconfirmed int
	exclusionShort := false
	for _, o := range outcomes {
		switch o.Outcome {
		case microsoft.OutcomeApplied:
			applied++
			continue
		case microsoft.OutcomeFailed:
			failed++
		default:
			unconfirmed++
		}
		if o.Excluded {
			exclusionShort = true
		}
	}
	result.Steps = append(result.Steps, fmt.Sprintf("Remarketing lists on ad group %s: %d attached, %d failed, %d unconfirmed", result.AdGroupID, applied, failed, unconfirmed))
	if err != nil {
		result.Steps = append(result.Steps, "Remarketing list call failed: "+err.Error())
	}
	if failed == 0 && unconfirmed == 0 {
		return false
	}
	if exclusionShort {
		result.Steps = append(result.Steps, "An excluded remarketing list is not confirmed attached: do not enable the campaign until the exclusion is attached to the ad group in Microsoft Advertising")
	}
	result.Steps = append(result.Steps, "Remarketing lists not all attached: add the FAILED ones (and any UNCONFIRMED ones missing in Microsoft Advertising) to the ad group; repeating the create does not retry them")
	slog.WarnContext(ctx, "microsoft create-time remarketing lists partly applied", "campaign_id", result.CampaignID,
		"failed", failed, "unconfirmed", unconfirmed)
	return true
}
