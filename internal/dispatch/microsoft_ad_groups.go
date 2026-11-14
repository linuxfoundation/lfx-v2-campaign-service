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

// Multiple ad groups at create (#563, gap 12): microsoftConfig.adGroups, up to
// microsoft.MaxAdGroups ad groups under the campaign, each with its own nameSuffix, keywords,
// negativeKeywords, remarketing lists and ad copy. Absent, the create builds today's single ad
// group exactly as before.
//
// Each per-group field keeps the rules AND the deployment gate of its single-group twin, checked
// by the same functions on a per-group view of the config (microsoftAdGroupView): copy behind
// MICROSOFT_AD_COPY_ENABLED, negatives behind MICROSOFT_CREATE_NEGATIVE_KEYWORDS_ENABLED (sent at
// the AD GROUP level), lists behind MICROSOFT_AUDIENCES_ENABLED and global_marketing_ops. The list
// itself is behind MICROSOFT_AD_GROUPS_ENABLED. A refusal names the entry.
//
// The single-group fields (keywords, headlines, descriptions, path1, path2, audienceIds,
// excludedAudienceIds, audienceBidAdjustment) are refused alongside adGroups: each group carries
// its own, and a value at both levels would leave which one applies to a guess. The campaign-level
// fields (budget, bidding, cpcBid, dates, searchPartners, the campaign negativeKeywords and the
// rest) apply to every group.

// adGroupKeywordConfig is the {text, matchType} shape of a group's keywords and negatives, the
// single-group microsoftKeywordConfig under a local name.
type adGroupKeywordConfig = microsoftKeywordConfig

// microsoftAdGroupConfig is one microsoftConfig.adGroups entry.
type microsoftAdGroupConfig struct {
	NameSuffix            string                 `json:"nameSuffix"`
	Keywords              []adGroupKeywordConfig `json:"keywords,omitempty"`
	NegativeKeywords      []adGroupKeywordConfig `json:"negativeKeywords,omitempty"`
	AudienceIDs           []string               `json:"audienceIds,omitempty"`
	ExcludedAudienceIDs   []string               `json:"excludedAudienceIds,omitempty"`
	AudienceBidAdjustment *float64               `json:"audienceBidAdjustment,omitempty"`
	Headlines             []string               `json:"headlines,omitempty"`
	Descriptions          []string               `json:"descriptions,omitempty"`
	Path1                 string                 `json:"path1,omitempty"`
	Path2                 string                 `json:"path2,omitempty"`
}

// microsoftAdGroupsEnabled reports whether this deployment opted in. Read per call.
func microsoftAdGroupsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftAdGroupsEnabled) == "true"
}

// microsoftAdGroupInvalid is the refusal for a bad adGroups input: a 400 from PreflightCreate and,
// from Dispatch, a refusal the job copies verbatim.
func microsoftAdGroupInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// microsoftAdGroupEntryInvalid re-addresses a single-group refusal (err, from checkMicrosoftAdCopy
// and the like run on a per-group view) to its adGroups entry.
func microsoftAdGroupEntryInvalid(entry int, err error) error {
	msg := strings.TrimPrefix(err.Error(), domain.ErrCreateConfigInvalid.Error()+": ")
	msg = strings.TrimPrefix(msg, "microsoftConfig.")
	msg = strings.TrimPrefix(msg, "microsoftConfig: ")
	return microsoftAdGroupInvalid("%s entry %d: %s", microsoft.FieldAdGroups, entry, msg)
}

// microsoftAdGroupView is the config one group's fields would be on the single-group path, so the
// single-group checks judge them unchanged.
func microsoftAdGroupView(g microsoftAdGroupConfig) microsoftConfig {
	return microsoftConfig{
		Keywords: g.Keywords, NegativeKeywords: g.NegativeKeywords,
		AudienceIDs: g.AudienceIDs, ExcludedAudienceIDs: g.ExcludedAudienceIDs, AudienceBidAdjustment: g.AudienceBidAdjustment,
		Headlines: g.Headlines, Descriptions: g.Descriptions, Path1: g.Path1, Path2: g.Path2,
	}
}

// microsoftSingleGroupFieldsSet names the single-group fields cfg sets, which adGroups replaces.
func microsoftSingleGroupFieldsSet(cfg microsoftConfig) []string {
	var set []string
	if len(cfg.Keywords) > 0 {
		set = append(set, "keywords")
	}
	set = append(set, microsoftAdCopySet(cfg)...)
	if len(cfg.AudienceIDs) > 0 {
		set = append(set, microsoft.FieldAudienceIDs)
	}
	if len(cfg.ExcludedAudienceIDs) > 0 {
		set = append(set, microsoft.FieldExcludedAudienceIDs)
	}
	if cfg.AudienceBidAdjustment != nil {
		set = append(set, microsoft.FieldAudienceBidAdjustment)
	}
	return set
}

// checkMicrosoftAdGroups applies the gate and every rule to a decoded config, returning each
// group's validated negatives (nil when it has none). Shared by PreflightCreate (a 400 before any
// job) and Dispatch (the same rules again, before any Microsoft call).
func checkMicrosoftAdGroups(cfg microsoftConfig) ([][]microsoft.NegativeKeyword, error) {
	if len(cfg.AdGroups) == 0 {
		return nil, nil
	}
	field := microsoft.FieldAdGroups
	if !microsoftAdGroupsEnabled() {
		env := constants.EnvMicrosoftAdGroupsEnabled
		return nil, microsoftAdGroupInvalid("%s: multiple Microsoft ad groups are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			field, env, field, env)
	}
	if len(cfg.AdGroups) > microsoft.MaxAdGroups {
		return nil, microsoftAdGroupInvalid("%s: holds %d ad groups; at most %d", field, len(cfg.AdGroups), microsoft.MaxAdGroups)
	}
	if set := microsoftSingleGroupFieldsSet(cfg); len(set) > 0 {
		return nil, microsoftAdGroupInvalid("%s: %s cannot be combined with adGroups; set them on each adGroups entry instead", field, strings.Join(set, ", "))
	}
	suffixes := make([]string, len(cfg.AdGroups))
	for i, g := range cfg.AdGroups {
		suffixes[i] = g.NameSuffix
	}
	if err := microsoft.ValidateAdGroupNameSuffixes(suffixes); err != nil {
		var gErr *microsoft.AdGroupError
		if errors.As(err, &gErr) {
			return nil, microsoftAdGroupInvalid("%s entry %d: %s: %s", field, gErr.Entry, gErr.Field, gErr.Msg)
		}
		return nil, microsoftAdGroupInvalid("%s: invalid", field)
	}
	negatives := make([][]microsoft.NegativeKeyword, len(cfg.AdGroups))
	for i, g := range cfg.AdGroups {
		entry := i + 1
		if len(g.Keywords) == 0 {
			return nil, microsoftAdGroupInvalid("%s entry %d: keywords: at least one keyword is required; an ad group with none can never serve", field, entry)
		}
		if err := microsoft.ValidateKeywords(microsoftKeywords(g.Keywords)); err != nil {
			return nil, microsoftAdGroupInvalid("%s entry %d: keywords: %s", field, entry, strings.TrimPrefix(err.Error(), "microsoft-ads: "))
		}
		view := microsoftAdGroupView(g)
		if err := checkMicrosoftAdCopy(view); err != nil {
			return nil, microsoftAdGroupEntryInvalid(entry, err)
		}
		if err := checkMicrosoftAudiences(view); err != nil {
			return nil, microsoftAdGroupEntryInvalid(entry, err)
		}
		n, err := checkMicrosoftNegativeKeywords(view)
		if err != nil {
			return nil, microsoftAdGroupEntryInvalid(entry, err)
		}
		negatives[i] = n
	}
	return negatives, nil
}

// checkMicrosoftAdGroupsShape refuses an adGroups value that is not an array of objects of the
// documented field types, so a wrong type is the synchronous 400. null counts as absent. Keys are
// matched case-insensitively, as encoding/json matches them, and visited in sorted order.
func checkMicrosoftAdGroupsShape(config json.RawMessage) error {
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
		if !strings.EqualFold(k, microsoft.FieldAdGroups) || isJSONNull(platformCfg[k]) {
			continue
		}
		var list []microsoftAdGroupConfig
		if json.Unmarshal(platformCfg[k], &list) != nil {
			return microsoftAdGroupInvalid("%s: must be an array of objects: nameSuffix a string; keywords and negativeKeywords arrays of {text, matchType}; audienceIds, excludedAudienceIds, headlines and descriptions arrays of strings; audienceBidAdjustment a number; path1 and path2 strings", microsoft.FieldAdGroups)
		}
	}
	return nil
}

// microsoftAdGroupInputs maps the decoded entries to the client's input.
func microsoftAdGroupInputs(in []microsoftAdGroupConfig) []microsoft.AdGroupInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.AdGroupInput, len(in))
	for i, g := range in {
		out[i] = microsoft.AdGroupInput{
			NameSuffix: g.NameSuffix, Keywords: microsoftKeywords(g.Keywords),
			Headlines: g.Headlines, Descriptions: g.Descriptions, Path1: g.Path1, Path2: g.Path2,
		}
	}
	return out
}

// checkMicrosoftAdGroupLists confirms, before anything is created, that every group's
// remarketing lists are lists on this account: one GetAudiencesByIds read per group that names
// any (each read stays inside the call's 100-id limit).
func checkMicrosoftAdGroupLists(ctx context.Context, client *microsoft.Client, cfg microsoftConfig) error {
	for i, g := range cfg.AdGroups {
		if len(g.AudienceIDs) == 0 && len(g.ExcludedAudienceIDs) == 0 {
			continue
		}
		if err := client.CheckRemarketingLists(ctx, g.AudienceIDs, g.ExcludedAudienceIDs); err != nil {
			return fmt.Errorf("microsoftConfig.%s entry %d: %w", microsoft.FieldAdGroups, i+1, err)
		}
	}
	return nil
}

// applyMicrosoftAdGroupExtras finishes a multi-ad-group create on the CONFIRMED campaign: it
// records every group that was not created (degrading the campaign with one step naming them),
// then attaches each CREATED group's negatives (ad-group level) and remarketing lists, under the
// single-group rules (a list is never written to a reused ad group). Outcomes are recorded on the
// group's AdGroupResult. It reports whether the campaign is DEGRADED and never returns an error:
// the campaign exists.
func applyMicrosoftAdGroupExtras(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, cfg microsoftConfig, negatives [][]microsoft.NegativeKeyword) bool {
	if len(result.AdGroups) == 0 {
		return false
	}
	degraded := false
	if failed := result.FailedAdGroups(); len(failed) > 0 {
		degraded = true
		result.Steps = append(result.Steps, fmt.Sprintf("Ad groups NOT created: %s. The campaign stands with the other ad groups (nothing was rolled back); create the missing ones in Microsoft Advertising, and verify any reported UNCONFIRMED first — repeating the create does not retry them", strings.Join(failed, ", ")))
		slog.WarnContext(ctx, "microsoft create: some ad groups not created", "campaign_id", result.CampaignID, "failed", failed)
	}
	for i := range result.AdGroups {
		g := &result.AdGroups[i]
		if g.Status != microsoft.AdGroupStatusCreated || i >= len(cfg.AdGroups) {
			continue
		}
		label := fmt.Sprintf("Ad group %d (%s): ", i+1, g.NameSuffix)
		if i < len(negatives) && len(negatives[i]) > 0 {
			if applyMicrosoftAdGroupNegatives(ctx, client, result, g, label, negatives[i]) {
				degraded = true
			}
		}
		gc := cfg.AdGroups[i]
		if len(gc.AudienceIDs) > 0 || len(gc.ExcludedAudienceIDs) > 0 {
			view := &microsoft.CampaignResult{CampaignID: result.CampaignID, AdGroupID: g.AdGroupID, AdGroupReused: g.AdGroupReused}
			if applyMicrosoftAudiences(ctx, client, view, microsoftAdGroupView(gc)) {
				degraded = true
			}
			g.Audiences = view.Audiences
			for _, s := range view.Steps {
				result.Steps = append(result.Steps, label+s)
			}
		}
	}
	return degraded
}

// applyMicrosoftAdGroupNegatives adds one group's negatives at the ad group level, recording the
// positional outcomes on g. It reports whether any is not, or may not be, attached.
func applyMicrosoftAdGroupNegatives(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, g *microsoft.AdGroupResult, label string, negatives []microsoft.NegativeKeyword) bool {
	outcomes, err := client.AddAdGroupNegativeKeywords(ctx, g.AdGroupID, negatives)
	if err != nil {
		state := "NOT attached"
		if microsoft.IsOutcomeUnconfirmed(err) {
			state = "UNCONFIRMED (they may be attached — verify in Microsoft Advertising)"
		}
		result.Steps = append(result.Steps, fmt.Sprintf("%snegative keywords %s; add any missing to ad group %s in Microsoft Advertising: %v", label, state, g.AdGroupID, err))
		slog.WarnContext(ctx, "microsoft create-time ad group negative keywords not attached", "campaign_id", result.CampaignID, "ad_group_id", g.AdGroupID, "error", err)
		return true
	}
	g.NegativeKeywords = outcomes
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
	result.Steps = append(result.Steps, fmt.Sprintf("%snegative keywords on ad group %s: %d attached, %d already present, %d failed, %d unconfirmed", label, g.AdGroupID, applied, present, failed, unconfirmed))
	if failed == 0 && unconfirmed == 0 {
		return false
	}
	result.Steps = append(result.Steps, label+"negative keywords not all attached: add the FAILED ones (and any UNCONFIRMED ones missing) to the ad group in Microsoft Advertising; repeating the create does not retry them")
	return true
}

// microsoftAdGroupRef is one ad group a row records: the first (top-level) one, or a created
// further one from adGroups.
type microsoftAdGroupRef struct {
	adGroupID, adID string
	keywordIDs      []string
}

// microsoftExtraAdGroups returns the CREATED ad groups after the first that a multi-ad-group row
// records (#563 gap 12), in order, and the entries that name an ad group but were not created.
// Empty for a single-group row.
func microsoftExtraAdGroups(raw json.RawMessage) (created []microsoftAdGroupRef) {
	if len(raw) == 0 {
		return nil
	}
	var blob struct {
		AdGroups []microsoft.AdGroupResult `json:"adGroups"`
	}
	if json.Unmarshal(raw, &blob) != nil {
		return nil
	}
	for i, g := range blob.AdGroups {
		if i == 0 || g.Status != microsoft.AdGroupStatusCreated {
			continue
		}
		created = append(created, microsoftAdGroupRef{adGroupID: strings.TrimSpace(g.AdGroupID), adID: strings.TrimSpace(g.AdID), keywordIDs: g.KeywordIDs})
	}
	return created
}
