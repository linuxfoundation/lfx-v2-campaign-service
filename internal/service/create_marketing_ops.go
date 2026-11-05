// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

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
// leaves the gated inputs refused with 503, never admitted.
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

// GoogleAdsDemandGenAudienceRequested reports whether config's googleAdsConfig names an
// existing Google Ads Audience: demandGenAudience, or the audience of any demandGenAdGroups
// entry. Any non-empty value counts, whitespace included, exactly as the dispatcher's
// GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED gate counts it, so the two cannot disagree about
// whether a request carries an audience. It decodes the envelope the way the dispatcher's
// unmarshalPlatformConfig does (exact envelope key, then encoding/json into the same field
// names and types), and dispatch's tests pin the agreement.
//
// A config that fails to decode still reports any audience the decoder populated before the
// error. When none was populated the dispatcher's own decode fails on the same field types,
// so the job fails before anything is created and nothing is admitted.
func GoogleAdsDemandGenAudienceRequested(config json.RawMessage) bool {
	if len(config) == 0 {
		return false
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(config, &envelope); err != nil {
		return false
	}
	raw, ok := envelope["googleAdsConfig"]
	if !ok || len(raw) == 0 {
		return false
	}
	var cfg struct {
		DemandGenAudience string `json:"demandGenAudience"`
		DemandGenAdGroups []struct {
			Audience string `json:"audience"`
		} `json:"demandGenAdGroups"`
	}
	_ = json.Unmarshal(raw, &cfg) // see above: a partial decode is still inspected
	if cfg.DemandGenAudience != "" {
		return true
	}
	for _, g := range cfg.DemandGenAdGroups {
		if g.Audience != "" {
			return true
		}
	}
	return false
}

// demandGenAudienceForbiddenMessage is the 403 sentence. Service-authored: it names the input
// and the relation, never a caller value.
const demandGenAudienceForbiddenMessage = "Demand Gen audiences (googleAdsConfig.demandGenAudience and each " +
	"demandGenAdGroups audience) require the global_marketing_ops role on this project"

// requireMarketingOpsForAudiences refuses a google-ads create that names a Demand Gen Audience
// unless the caller holds global_marketing_ops on the path project.
//
// The gateway admits every campaign_manager to create-campaigns, and campaign_manager is
// `executive_director or global_marketing_ops` (lfx-v2-helm model.fga): it includes each
// foundation's executive directors. An Audience is a resource of the Google Ads account every
// foundation shares, so a campaign_manager check alone would let one foundation's ED target
// audiences built for another. The gateway cannot narrow this itself because the input is in
// the body, not the route, so the check is made here, on the same relation the
// marketing-ops-api gateway rule uses for the conversion-actions read (#486).
//
// Fail closed: no checker, no actor, or a check that could not be completed answers 503, and
// only a definite grant admits the request.
func (s *BriefService) requireMarketingOpsForAudiences(ctx context.Context, projectSlug string, platforms []model.Provider, config json.RawMessage) error {
	if !slices.Contains(platforms, model.ProviderGoogleAds) || !GoogleAdsDemandGenAudienceRequested(config) {
		return nil
	}
	unavailable := &briefs.ConnServiceUnavailableError{Code: "503",
		Message: "could not verify the global_marketing_ops role that Demand Gen audiences require; retry, or omit the audience"}
	checker := s.marketingOpsChecker()
	if checker == nil {
		slog.ErrorContext(ctx, "refusing a Demand Gen audience: no marketing-ops checker is configured (NATS_URL empty?)",
			"project_id", projectSlug)
		return unavailable
	}
	actor := actorFromCtx(ctx)
	if actor == nil || actor.Username == "" {
		slog.ErrorContext(ctx, "refusing a Demand Gen audience: the request carries no authenticated username",
			"project_id", projectSlug)
		return unavailable
	}
	ok, err := checker.IsGlobalMarketingOps(ctx, projectSlug, actor.Username)
	if err != nil {
		slog.ErrorContext(ctx, "refusing a Demand Gen audience: the global_marketing_ops check failed",
			"project_id", projectSlug, "error", err)
		return unavailable
	}
	if !ok {
		slog.WarnContext(ctx, "refused a Demand Gen audience from a caller without global_marketing_ops",
			"project_id", projectSlug)
		return &briefs.ForbiddenError{Code: "403", Message: demandGenAudienceForbiddenMessage}
	}
	return nil
}
