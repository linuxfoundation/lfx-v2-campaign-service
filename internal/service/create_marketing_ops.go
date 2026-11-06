// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// MarketingOpsChecker answers whether a user holds global_marketing_ops on a project. A false
// result is a definite denial; an error means no answer could be obtained. Implemented by
// internal/infrastructure/accesscheck.Checker.
type MarketingOpsChecker interface {
	IsGlobalMarketingOps(ctx context.Context, projectSlug, username string) (bool, error)
}

// SetMarketingOpsChecker injects the checker; the container calls it. Nil (no NATS configured)
// leaves the gated inputs (GoogleAdsMarketingOpsInputs) refused with 503, never admitted.
func (s *BriefService) SetMarketingOpsChecker(c MarketingOpsChecker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marketingOps = c
}

func (s *BriefService) marketingOpsChecker() MarketingOpsChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.marketingOps
}

// GoogleAdsMarketingOpsInputs names, in config order, the googleAdsConfig inputs this config
// sets that name an EXISTING audience, user list, custom segment, shared negative keyword list or
// conversion action on the connected Google Ads account, which every foundation shares, so that
// only global_marketing_ops may send them:
//
//   - demandGenAudience and each demandGenAdGroups entry's audience (#459, #460);
//   - the Display audience lists (#462, #475): audienceSegments, customAudiences,
//     customAffinities and excludedAudienceSegments, on the campaign and on each displayAdGroups
//     entry;
//   - performanceMaxAudienceSignal (#461);
//   - sharedNegativeKeywordLists (#477);
//   - demandGenDemographics.audienceSegments (#498) and demandGenDemographics.customAudiences
//     (#516), existing user lists and custom audiences put in the demographic Audience
//     (demographics without them are not gated), and the same two lists in each
//     demandGenAdGroups entry's own demographics (#518);
//   - conversionActions (#436), on every channel: existing conversion actions, and the
//     account-level custom conversion goal the client creates or reuses for them;
//   - audienceSegments on Search, at the campaign level and on each adGroups entry. Search
//     attaches them as observation-only user-list criteria and they predate every deployment
//     flag, but they still name the shared account's user lists;
//   - campaignAudienceSegments (#513), the same user lists observed at the Search campaign level.
//
// dispatch's TestEveryGoogleAdsConfigFieldIsClassifiedForTheMarketingOpsGate requires every
// googleAdsConfig field to be classified as gated or not.
//
// Group entries are named by 1-based position ("displayAdGroups entry 2 customAudiences").
// Every name is service-derived, so a message built from them carries no caller value.
//
// Any non-empty value or list counts, whitespace included and whatever its entries hold, on any
// channel, exactly as the dispatcher's deployment gates count them (googleAdsDisplayAudiencesEnabled
// and its siblings), so the two cannot disagree about whether a request carries one; the
// campaign-level audienceSegments count on every channel, a superset of the Display gate. It
// decodes the envelope the way the dispatcher's unmarshalPlatformConfig does (exact envelope key,
// then encoding/json into the same field names and types), and dispatch's tests pin the
// agreement with the dispatcher's gates.
//
// A config that fails to decode still reports any input the decoder populated: encoding/json
// skips a mistyped field and decodes the rest. When a gated field itself is mistyped the
// dispatcher's own decode fails on the same type, so the job fails before anything is created
// and nothing is admitted.
func GoogleAdsMarketingOpsInputs(config json.RawMessage) []string {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(config, &envelope); err != nil {
		return nil
	}
	raw, ok := envelope["googleAdsConfig"]
	if !ok || len(raw) == 0 {
		return nil
	}
	type audienceLists struct {
		AudienceSegments         []string `json:"audienceSegments"`
		CustomAudiences          []string `json:"customAudiences"`
		CustomAffinities         []string `json:"customAffinities"`
		ExcludedAudienceSegments []string `json:"excludedAudienceSegments"`
	}
	type demographicSegments struct {
		AudienceSegments []string `json:"audienceSegments"`
		CustomAudiences  []string `json:"customAudiences"`
	}
	var cfg struct {
		audienceLists
		DemandGenAudience string `json:"demandGenAudience"`
		DemandGenAdGroups []struct {
			Audience     string               `json:"audience"`
			Demographics *demographicSegments `json:"demographics"`
		} `json:"demandGenAdGroups"`
		AdGroups []struct {
			AudienceSegments []string `json:"audienceSegments"`
		} `json:"adGroups"`
		CampaignAudienceSegments     []string             `json:"campaignAudienceSegments"`
		DisplayAdGroups              []audienceLists      `json:"displayAdGroups"`
		PerformanceMaxAudienceSignal string               `json:"performanceMaxAudienceSignal"`
		SharedNegativeKeywordLists   []string             `json:"sharedNegativeKeywordLists"`
		ConversionActions            []string             `json:"conversionActions"`
		DemandGenDemographics        *demographicSegments `json:"demandGenDemographics"`
	}
	_ = json.Unmarshal(raw, &cfg) // see above: a partial decode is still inspected

	var set []string
	lists := func(prefix string, l audienceLists) {
		for _, f := range []struct {
			name string
			list []string
		}{
			{"audienceSegments", l.AudienceSegments},
			{"customAudiences", l.CustomAudiences},
			{"customAffinities", l.CustomAffinities},
			{"excludedAudienceSegments", l.ExcludedAudienceSegments},
		} {
			if len(f.list) > 0 {
				set = append(set, prefix+f.name)
			}
		}
	}
	if cfg.DemandGenAudience != "" {
		set = append(set, "demandGenAudience")
	}
	for i, g := range cfg.DemandGenAdGroups {
		if g.Audience != "" {
			set = append(set, "demandGenAdGroups entry "+strconv.Itoa(i+1)+" audience")
		}
	}
	lists("", cfg.audienceLists)
	for i, g := range cfg.AdGroups {
		if len(g.AudienceSegments) > 0 {
			set = append(set, "adGroups entry "+strconv.Itoa(i+1)+" audienceSegments")
		}
	}
	if len(cfg.CampaignAudienceSegments) > 0 {
		set = append(set, "campaignAudienceSegments")
	}
	for i, g := range cfg.DisplayAdGroups {
		lists("displayAdGroups entry "+strconv.Itoa(i+1)+" ", g)
	}
	if cfg.PerformanceMaxAudienceSignal != "" {
		set = append(set, "performanceMaxAudienceSignal")
	}
	if len(cfg.SharedNegativeKeywordLists) > 0 {
		set = append(set, "sharedNegativeKeywordLists")
	}
	if len(cfg.ConversionActions) > 0 {
		set = append(set, "conversionActions")
	}
	// #498 and #516: the user lists and custom audiences put in the demographic Audience.
	// demandGenDemographics without them is not gated: that Audience is created fresh and holds
	// only Google's demographic brackets, so it names no existing account resource.
	if d := cfg.DemandGenDemographics; d != nil {
		if len(d.AudienceSegments) > 0 {
			set = append(set, "demandGenDemographics.audienceSegments")
		}
		if len(d.CustomAudiences) > 0 {
			set = append(set, "demandGenDemographics.customAudiences")
		}
	}
	// #518: the same lists in each demandGenAdGroups entry's own demographic Audience.
	for i, g := range cfg.DemandGenAdGroups {
		if d := g.Demographics; d != nil {
			prefix := "demandGenAdGroups entry " + strconv.Itoa(i+1) + " demographics."
			if len(d.AudienceSegments) > 0 {
				set = append(set, prefix+"audienceSegments")
			}
			if len(d.CustomAudiences) > 0 {
				set = append(set, prefix+"customAudiences")
			}
		}
	}
	return set
}

// marketingOpsForbiddenMessage is the 403 sentence for the gated fields. Service-authored: it
// names the inputs (service-derived, see GoogleAdsMarketingOpsInputs) and the relation, never a
// caller value.
func marketingOpsForbiddenMessage(fields []string) string {
	return "googleAdsConfig inputs that name a resource on the shared Google Ads account require " +
		"the global_marketing_ops role on this project: " + strings.Join(fields, ", ")
}

// MetaMarketingOpsInputs names, in config order, the metaConfig inputs this config sets that name
// an EXISTING custom audience of the connected Meta ad account (#550): targeting.customAudiences
// and targeting.excludedCustomAudiences. That ad account is shared by every foundation, as the
// Google Ads account is, so only global_marketing_ops may send them. Age, gender and
// advantageAudience name no account resource and are not gated.
//
// Any non-empty list counts, whatever its entries hold, exactly as GoogleAdsMarketingOpsInputs
// counts, and the envelope is decoded the way the dispatcher's unmarshalPlatformConfig decodes it.
// dispatch's TestEveryMetaTargetingFieldIsClassifiedForTheMarketingOpsGate requires every
// metaConfig.targeting field to be classified as gated or not.
func MetaMarketingOpsInputs(config json.RawMessage) []string {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(config, &envelope); err != nil {
		return nil
	}
	raw, ok := envelope["metaConfig"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var cfg struct {
		Targeting *struct {
			CustomAudiences         []string `json:"customAudiences"`
			ExcludedCustomAudiences []string `json:"excludedCustomAudiences"`
		} `json:"targeting"`
	}
	_ = json.Unmarshal(raw, &cfg) // a partial decode is still inspected, as for Google
	if cfg.Targeting == nil {
		return nil
	}
	var set []string
	if len(cfg.Targeting.CustomAudiences) > 0 {
		set = append(set, "targeting.customAudiences")
	}
	if len(cfg.Targeting.ExcludedCustomAudiences) > 0 {
		set = append(set, "targeting.excludedCustomAudiences")
	}
	return set
}

// metaMarketingOpsForbiddenMessage is the 403 sentence for the gated metaConfig fields, worded as
// marketingOpsForbiddenMessage is for Google. Service-authored: it names service-derived inputs only.
func metaMarketingOpsForbiddenMessage(fields []string) string {
	return "metaConfig inputs that name a resource on the shared Meta ad account require " +
		"the global_marketing_ops role on this project: " + strings.Join(fields, ", ")
}

// requireMarketingOpsForSharedAccountInputs refuses a google-ads or meta-ads create that sets any
// GoogleAdsMarketingOpsInputs or MetaMarketingOpsInputs field (each counted only when its platform
// is selected, since only that platform's dispatcher reads it) unless the caller holds
// global_marketing_ops on the path project. One check per request, however many fields are set.
//
// The gateway admits every campaign_manager to create-campaigns, and campaign_manager is
// `executive_director or global_marketing_ops` (lfx-v2-helm model.fga): it includes each
// foundation's executive directors. Audiences, user lists, custom segments, shared sets and conversion
// actions are resources of the Google Ads account every foundation shares — and custom audiences
// of the Meta ad account every foundation shares — so a campaign_manager check alone
// would let one foundation's ED target or reuse resources built for another. The gateway cannot
// narrow this itself because the inputs are in the body, not the route, so the check is made
// here, on the same relation the marketing-ops-api gateway rule uses for the conversion-actions
// read (#486).
//
// Fail closed: no checker, no actor, or a check that could not be completed answers 503, and
// only a definite grant admits the request.
func (s *BriefService) requireMarketingOpsForSharedAccountInputs(ctx context.Context, projectSlug string, platforms []model.Provider, config json.RawMessage) error {
	var googleFields, metaFields []string
	if slices.Contains(platforms, model.ProviderGoogleAds) {
		googleFields = GoogleAdsMarketingOpsInputs(config)
	}
	if slices.Contains(platforms, model.ProviderMetaAds) {
		metaFields = MetaMarketingOpsInputs(config)
	}
	if len(googleFields) == 0 && len(metaFields) == 0 {
		return nil
	}
	var configKeys, sentences []string
	var all []string
	if len(googleFields) > 0 {
		configKeys = append(configKeys, "googleAdsConfig")
		sentences = append(sentences, marketingOpsForbiddenMessage(googleFields))
		all = append(all, googleFields...)
	}
	if len(metaFields) > 0 {
		configKeys = append(configKeys, "metaConfig")
		sentences = append(sentences, metaMarketingOpsForbiddenMessage(metaFields))
		all = append(all, metaFields...)
	}
	named := strings.Join(all, ", ")
	unavailable := &briefs.ConnServiceUnavailableError{Code: "503",
		Message: "could not verify the global_marketing_ops role that these " + strings.Join(configKeys, " and ") +
			" inputs require (" + named + "); retry, or omit them"}
	checker := s.marketingOpsChecker()
	if checker == nil {
		slog.ErrorContext(ctx, "refusing shared-account inputs: no marketing-ops checker is configured (NATS_URL empty?)",
			"project_id", projectSlug, "fields", named)
		return unavailable
	}
	actor := actorFromCtx(ctx)
	if actor == nil || actor.Username == "" {
		slog.ErrorContext(ctx, "refusing shared-account inputs: the request carries no authenticated username",
			"project_id", projectSlug, "fields", named)
		return unavailable
	}
	ok, err := checker.IsGlobalMarketingOps(ctx, projectSlug, actor.Username)
	if err != nil {
		slog.ErrorContext(ctx, "refusing shared-account inputs: the global_marketing_ops check failed",
			"project_id", projectSlug, "fields", named, "error", err)
		return unavailable
	}
	if !ok {
		slog.WarnContext(ctx, "refused shared-account inputs from a caller without global_marketing_ops",
			"project_id", projectSlug, "fields", named)
		return &briefs.ForbiddenError{Code: "403", Message: strings.Join(sentences, "; ")}
	}
	return nil
}
