// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// linkedInAdSegmentKind is the AudienceReader kind the LinkedIn dispatcher serves
// (dispatch.LinkedInAdSegmentKind; restated because service cannot import dispatch).
const linkedInAdSegmentKind = "ad_segment"

var (
	linkedInAdsAudienceLookup = accountDiscovery{
		provider:    model.ProviderLinkedInAds,
		displayName: "linkedin ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json " +
			"with access_token set, and that an account_id is selected",
		operation: "ad segment lookup",
	}
	linkedInAdsConversionLookup = accountDiscovery{
		provider:    model.ProviderLinkedInAds,
		displayName: "linkedin ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json " +
			"with access_token set, and that an account_id is selected",
		operation: "conversion rule lookup",
	}
)

// ListLinkedinAdsAudiences lists the ad segments on the LinkedIn ad account a create for this
// project uses, for the linkedInConfig.targeting segment pickers (#552).
//
// The LF system account is the fallback of every project without its own LinkedIn connection,
// so the answer can name segments built for other foundations; the gateway therefore admits
// only global_marketing_ops of the project (marketing-ops-api), as for the Google pickers. The
// reserved LF scope stays unaddressable, as it is for every connection read.
func (s *ConnectionService) ListLinkedinAdsAudiences(ctx context.Context, p *conn.ListLinkedinAdsAudiencesPayload) (*conn.LinkedinAdsAudienceSegments, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadAudiences(ctx, p.ProjectID, model.ProviderLinkedInAds, linkedInAdSegmentKind)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsAudienceLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsAudienceSegment, 0, len(res.Audiences))
	for _, a := range res.Audiences {
		seg := &conn.LinkedinAdsAudienceSegment{
			ID: a.ID, Urn: a.ResourceName, Name: a.Name, Type: a.Subtype, Status: a.Status,
			ApproximateMemberCount: a.MemberCount,
		}
		if a.Facet != "" {
			facet := a.Facet
			seg.Facet = &facet
		}
		out = append(out, seg)
	}
	return &conn.LinkedinAdsAudienceSegments{AccountID: res.AccountID, Audiences: out, AudienceCount: len(out)}, nil
}

// ListLinkedinAdsConversions lists the conversion rules on the LinkedIn ad account a create for
// this project uses, for the linkedInConfig.conversions picker (#552). Gated and resolved as
// ListLinkedinAdsAudiences is.
func (s *ConnectionService) ListLinkedinAdsConversions(ctx context.Context, p *conn.ListLinkedinAdsConversionsPayload) (*conn.LinkedinAdsConversionRules, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsConversionLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadConversionActions(ctx, p.ProjectID, model.ProviderLinkedInAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsConversionLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsConversionRule, 0, len(res.Actions))
	for _, a := range res.Actions {
		out = append(out, &conn.LinkedinAdsConversionRule{
			ID: a.ID, Urn: a.ResourceName, Name: a.Name, Type: a.Type, Enabled: a.Status == "ENABLED",
		})
	}
	return &conn.LinkedinAdsConversionRules{AccountID: res.AccountID, Conversions: out, ConversionCount: len(out)}, nil
}
