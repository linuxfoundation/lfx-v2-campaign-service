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

// Microsoft shared negative keyword lists at create (#563, gap 9):
// microsoftConfig.sharedNegativeKeywordListIds, existing account-level negative keyword lists
// verified before anything is created and associated with the confirmed campaign. The rules live
// once, in microsoft.ValidateSharedNegativeKeywordLists; this file adds the deployment gate and
// the JSON-type check, turns a refusal into the synchronous 400 shape ("invalid campaign config:
// microsoftConfig.sharedNegativeKeywordListIds: ..."), and associates the lists after the
// campaign is confirmed.
//
// Two gates, independent of each other, as for the remarketing lists (microsoft_audiences.go).
// MICROSOFT_SHARED_NEGATIVE_LISTS_ENABLED (here) keeps a shape no live call has exercised off the
// shared account. The global_marketing_ops check (service.MicrosoftMarketingOpsInputs) keeps a
// foundation's executive director, who is a campaign_manager, from attaching lists built on the
// LF account for another foundation.

// microsoftSharedNegativeListsEnabled reports whether this deployment opted in. Read per call,
// like microsoftAudiencesEnabled, so it flips without a rebuild.
func microsoftSharedNegativeListsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftSharedNegativeListsEnabled) == "true"
}

// microsoftSharedListInvalid is the refusal for a bad shared-list input: a 400 from
// PreflightCreate (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies
// verbatim (domain.PreflightRefusal). Service-authored text naming the field and positions only.
func microsoftSharedListInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkMicrosoftSharedNegativeListsShape refuses a value that is not a JSON array of strings
// before any job exists. Keys are matched case-insensitively, as encoding/json matches them, and
// visited in sorted order so the refusal is deterministic. An explicit null is absent.
func checkMicrosoftSharedNegativeListsShape(config json.RawMessage) error {
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
		if !strings.EqualFold(k, microsoft.FieldSharedNegativeKeywordListIDs) || isJSONNull(platformCfg[k]) {
			continue
		}
		var ids []string
		if json.Unmarshal(platformCfg[k], &ids) != nil {
			return microsoftSharedListInvalid("%s: must be an array of strings", microsoft.FieldSharedNegativeKeywordListIDs)
		}
	}
	return nil
}

// checkMicrosoftSharedNegativeLists applies the deployment gate and
// microsoft.ValidateSharedNegativeKeywordLists to a decoded config. Shared by PreflightCreate (a
// 400 before any job) and Dispatch (the same rules again, before any Microsoft call, for a job
// enqueued before the flag was turned off). Any non-empty list counts as set, so the gate never
// depends on the values' own validity.
func checkMicrosoftSharedNegativeLists(cfg microsoftConfig) error {
	if len(cfg.SharedNegativeKeywordListIDs) == 0 {
		return nil
	}
	field := microsoft.FieldSharedNegativeKeywordListIDs
	if !microsoftSharedNegativeListsEnabled() {
		env := constants.EnvMicrosoftSharedNegativeListsEnabled
		return microsoftSharedListInvalid("%s: Microsoft shared negative keyword lists are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			field, env, field, env)
	}
	if err := microsoft.ValidateSharedNegativeKeywordLists(cfg.SharedNegativeKeywordListIDs); err != nil {
		var sErr *microsoft.SharedListError
		if errors.As(err, &sErr) {
			return microsoftSharedListInvalid("%s: %s", sErr.Field, sErr.Msg)
		}
		return microsoftSharedListInvalid("%s: invalid", field)
	}
	return nil
}

// applyMicrosoftSharedNegativeLists associates the requested lists with the CONFIRMED campaign in
// result, recording the positional outcomes and Steps on result. It reports whether the campaign
// is DEGRADED: a requested list that is not, or may not be, associated. Nothing requested is a
// no-op. Like applyMicrosoftNegativeKeywords it never returns an error: the campaign exists, so a
// dispatch error would retain the claim for a reconcile that never re-runs this step.
//
// It runs on a campaign reused by name too, as the create-time negatives do: a negative list only
// narrows which searches serve, so associating it with a possibly live campaign cannot widen
// spend. Microsoft's answer for a list already associated is not documented; it is reported as
// whatever outcome the call yields (a refusal is FAILED with Microsoft's code).
func applyMicrosoftSharedNegativeLists(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, cfg microsoftConfig) bool {
	if len(cfg.SharedNegativeKeywordListIDs) == 0 {
		return false
	}
	outcomes, err := client.AttachCampaignSharedNegativeKeywordLists(ctx, result.CampaignID, cfg.SharedNegativeKeywordListIDs)
	if len(outcomes) == 0 && err != nil {
		// Refused before any request (a campaign id this client cannot address).
		result.Steps = append(result.Steps, "Shared negative keyword lists NOT associated; associate them with the campaign in Microsoft Advertising before enabling it: "+err.Error())
		slog.WarnContext(ctx, "microsoft create-time shared negative keyword lists not associated", "campaign_id", result.CampaignID, "error", err)
		return true
	}
	result.SharedNegativeKeywordLists = outcomes
	var applied, failed, unconfirmed int
	for _, o := range outcomes {
		switch o.Outcome {
		case microsoft.OutcomeApplied:
			applied++
		case microsoft.OutcomeFailed:
			failed++
		default:
			unconfirmed++
		}
	}
	result.Steps = append(result.Steps, fmt.Sprintf("Shared negative keyword lists on campaign %s: %d associated, %d failed, %d unconfirmed", result.CampaignID, applied, failed, unconfirmed))
	if err != nil {
		result.Steps = append(result.Steps, "Shared negative keyword list call failed: "+err.Error())
	}
	if failed == 0 && unconfirmed == 0 {
		return false
	}
	result.Steps = append(result.Steps, "Shared negative keyword lists not all associated: associate the FAILED ones (and any UNCONFIRMED ones missing in Microsoft Advertising) with the campaign before enabling it; repeating the create does not retry them")
	slog.WarnContext(ctx, "microsoft create-time shared negative keyword lists partly associated", "campaign_id", result.CampaignID,
		"failed", failed, "unconfirmed", unconfirmed)
	return true
}
