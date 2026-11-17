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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
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
// records (#563 gap 12), in order. A failed entry is never returned, even when it recorded an
// adGroupId, so the status toggle never activates a partly built group (activation needs every
// group's ad and keywords). The bid and keyword levers use microsoftConfirmedExtraAdGroupIDs
// instead. Empty for a single-group row.
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

// microsoftConfirmedExtraAdGroupIDs returns the distinct ad group ids after the first that a
// multi-ad-group row records as CONFIRMED (#563 gap 12), in order: every entry with an adGroupId,
// whatever its status. An entry records an adGroupId only once Microsoft confirmed that ad group,
// so one whose ad or keyword step failed later still exists upstream, carrying the campaign's
// bid and its confirmed keywords. The bid lever must re-bid it (or the campaign's recorded bid
// would not match it) and the keyword lever must own it. firstAdGroupID, the row's top-level ad
// group, is never repeated, nor is an id an earlier entry already named. Empty for a
// single-group row.
func microsoftConfirmedExtraAdGroupIDs(raw json.RawMessage, firstAdGroupID string) []string {
	if len(raw) == 0 {
		return nil
	}
	var blob struct {
		AdGroups []microsoft.AdGroupResult `json:"adGroups"`
	}
	if json.Unmarshal(raw, &blob) != nil {
		return nil
	}
	seen := map[string]bool{strings.TrimSpace(firstAdGroupID): true}
	var ids []string
	for i, g := range blob.AdGroups {
		id := strings.TrimSpace(g.AdGroupID)
		if i == 0 || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// microsoftUnaddressedAdGroups names, as "entry N (suffix)", every further ad group a
// multi-ad-group row records as failed with an UNCONFIRMED outcome and no confirmed ad group id
// (#563 gap 12): an ad group may exist upstream, carrying the campaign's bid, that no lever can
// address. Empty for a single-group row.
func microsoftUnaddressedAdGroups(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var blob struct {
		AdGroups []microsoft.AdGroupResult `json:"adGroups"`
	}
	if json.Unmarshal(raw, &blob) != nil {
		return nil
	}
	var out []string
	for i, g := range blob.AdGroups {
		if i > 0 && g.Unconfirmed && strings.TrimSpace(g.AdGroupID) == "" {
			out = append(out, fmt.Sprintf("entry %d (%s)", i+1, clipAdGroupSuffix(g.NameSuffix)))
		}
	}
	return out
}

// clipAdGroupSuffix bounds a recorded suffix for an error message.
func clipAdGroupSuffix(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// microsoftAdGroupFinder is the one client read reconcileMicrosoftUnaddressedAdGroups needs.
type microsoftAdGroupFinder interface {
	FindAdGroupByName(ctx context.Context, campaignID, name string) (string, bool, error)
}

// reconcileMicrosoftUnaddressedAdGroups resolves every further ad group the row records as
// failed with an UNCONFIRMED create and no id (#563 gap 12) by looking it up by its recorded,
// composed ad group name under the campaign:
//   - found: its id is recorded (adGroupId set, unconfirmed cleared; status stays failed, since
//     its ad and keywords are still unknown), so the caller re-bids it with the rest;
//   - definitely absent (a complete, readable list without it, every entry of which has a
//     readable name — FindAdGroupByName refuses to call it absent otherwise) AND the create old
//     enough to have settled (absentClears): unconfirmed is cleared, so it
//     no longer blocks — there is no ad group to re-bid;
//   - otherwise (the lookup failed, the list was unreadable, the name matched an entry with no
//     usable id, the id is one another entry already records, or no name was recorded) it stays
//     unconfirmed and is returned, named "entry N (suffix)", for the caller to refuse.
//
// Changes are written back into campaign.Result, so the caller that persists the row on success
// (update-campaign-bid's ReplaceCampaign, which writes the result column) records them; on any
// failure nothing is persisted and the next write reconciles again. Fields of the result this
// function does not know are preserved.
//
// absentClears says whether a definitely-absent group may be cleared: only once the create has
// had microsoftUnconfirmedSettleWindow to settle, since a timed-out create can commit after the
// lookup. Inside the window an absent group is returned in unsettled instead (unresolved stays
// for lookups that could not answer); a found group is recorded either way.
func reconcileMicrosoftUnaddressedAdGroups(ctx context.Context, client microsoftAdGroupFinder, campaign *model.Campaign, campaignID, firstAdGroupID string, absentClears bool) (unresolved, unsettled []string) {
	if len(microsoftUnaddressedAdGroups(campaign.Result)) == 0 {
		return nil, nil
	}
	var top map[string]json.RawMessage
	var entries []map[string]json.RawMessage
	if json.Unmarshal(campaign.Result, &top) != nil || json.Unmarshal(top["adGroups"], &entries) != nil {
		return microsoftUnaddressedAdGroups(campaign.Result), nil
	}
	var typed []microsoft.AdGroupResult
	_ = json.Unmarshal(top["adGroups"], &typed)
	known := map[string]bool{strings.TrimSpace(firstAdGroupID): true}
	for _, g := range typed {
		if id := strings.TrimSpace(g.AdGroupID); id != "" {
			known[id] = true
		}
	}
	changed := false
	for i, g := range typed {
		if i == 0 || i >= len(entries) || !g.Unconfirmed || strings.TrimSpace(g.AdGroupID) != "" {
			continue
		}
		label := fmt.Sprintf("entry %d (%s)", i+1, clipAdGroupSuffix(g.NameSuffix))
		id, found, err := client.FindAdGroupByName(ctx, campaignID, g.AdGroupName)
		switch {
		case err != nil:
			unresolved = append(unresolved, label)
		case !found && !absentClears:
			unsettled = append(unsettled, label)
		case !found:
			delete(entries[i], "unconfirmed")
			changed = true
		case known[id] || !microsoftCampaignIDRE.MatchString(id):
			// The name resolved to an ad group another entry already records (or to an unusable
			// id): not a group of its own, and not safely addressable.
			unresolved = append(unresolved, label)
		default:
			raw, _ := json.Marshal(id)
			entries[i]["adGroupId"] = raw
			delete(entries[i], "unconfirmed")
			known[id] = true
			changed = true
		}
	}
	if changed {
		if raw, err := json.Marshal(entries); err == nil {
			top["adGroups"] = raw
			if out, err := json.Marshal(top); err == nil {
				campaign.Result = out
			}
		}
	}
	return unresolved, unsettled
}

var _ service.BriefCreatePreflighter = (*MicrosoftDispatcher)(nil)

// PreflightCreateBrief implements service.BriefCreatePreflighter: the brief-dependent half of
// the create preflight. Each requested ad group's display URL — the brief's registration host
// plus that group's path1/path2 — is measured with microsoft.ValidateAdGroupDisplayURLs, the
// check CreateCampaign makes, so an overflow is the synchronous 400 rather than a job failure
// after the 202. Local only; ErrPreflightNotApplicable when no ad group is requested or the
// gate is off (PreflightCreate has already refused adGroups then).
func (d *MicrosoftDispatcher) PreflightCreateBrief(_ context.Context, _ string, _ model.Provider, brief *model.CampaignBrief, config json.RawMessage) error {
	var cfg microsoftConfig
	_ = unmarshalPlatformConfig(config, "microsoftConfig", &cfg)
	if brief == nil || len(cfg.AdGroups) == 0 || !microsoftAdGroupsEnabled() {
		return domain.ErrPreflightNotApplicable
	}
	bf, err := decodeBriefFields(brief)
	if err != nil {
		return domain.ErrPreflightNotApplicable
	}
	in := microsoft.CampaignInput{
		EventName: bf.EventName, EventSlug: brief.EventSlug, Project: brief.ProjectID,
		RegistrationURL: bf.RegistrationURL, AdGroups: microsoftAdGroupInputs(cfg.AdGroups),
	}
	if err := microsoft.ValidateAdGroupDisplayURLs(in); err != nil {
		var gErr *microsoft.AdGroupError
		if errors.As(err, &gErr) {
			return microsoftAdGroupInvalid("%s entry %d: %s: %s", microsoft.FieldAdGroups, gErr.Entry, gErr.Field, gErr.Msg)
		}
		return microsoftAdGroupInvalid("%s: invalid", microsoft.FieldAdGroups)
	}
	return nil
}
