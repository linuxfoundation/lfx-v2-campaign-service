// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// metaAdsCustomAudienceLookup is the custom-audience read's descriptor for the shared
// connection-state arms (classifyTargetingLookupError); the remedy is metaAdsAudienceInsights'.
var metaAdsCustomAudienceLookup = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json " +
		"with access_token set, and that an act_<digits> account_id is selected",
	operation: "custom audience lookup",
}

const (
	// metaCustomAudienceMax is the cap the list-meta-custom-audiences description publishes,
	// applied after the q filter; metaCustomAudienceQueryMaxLen mirrors the q MaxLength.
	metaCustomAudienceMax         = 500
	metaCustomAudienceQueryMaxLen = 100
)

// ListMetaCustomAudiences lists the custom audiences of the Meta ad account a create for this
// project uses, for the metaConfig.targeting audience pickers (#550).
//
// Resolution and tenancy are ListGoogleAdsAudiences': the answer is account-wide on an ad account
// every foundation shares, which is why only the LF marketing team may call it (see
// Orchestrator.ReadMetaCustomAudiences). q is checked before any credential is resolved.
func (s *ConnectionService) ListMetaCustomAudiences(ctx context.Context, p *conn.ListMetaCustomAudiencesPayload) (*conn.MetaCustomAudiences, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	// Mirrors the design Pattern and MaxLength for a caller that bypasses Goa.
	needle := ""
	if p.Q != nil {
		needle = strings.ToLower(strings.TrimSpace(*p.Q))
		if needle == "" || utf8.RuneCountInString(*p.Q) > metaCustomAudienceQueryMaxLen {
			return nil, &conn.BadRequestError{Code: "400", Message: "q must be 1-100 characters and not blank"}
		}
	}
	_, _, orch, err := s.resolveBackendWithOrch(metaAdsCustomAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadMetaCustomAudiences(ctx, p.ProjectID, model.ProviderMetaAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, metaAdsCustomAudienceLookup, rerr)
	}
	matched := make([]model.MetaCustomAudience, 0, len(res.Audiences))
	for _, a := range res.Audiences {
		if needle == "" || strings.Contains(strings.ToLower(a.Name), needle) {
			matched = append(matched, a)
		}
	}
	// Sorted by name ignoring case, then id, so the cap keeps a stable prefix.
	sort.SliceStable(matched, func(i, j int) bool {
		li, lj := strings.ToLower(matched[i].Name), strings.ToLower(matched[j].Name)
		if li != lj {
			return li < lj
		}
		return matched[i].ID < matched[j].ID
	})
	truncated := len(matched) > metaCustomAudienceMax
	if truncated {
		matched = matched[:metaCustomAudienceMax]
	}
	out := make([]*conn.MetaCustomAudience, 0, len(matched))
	for _, a := range matched {
		out = append(out, &conn.MetaCustomAudience{
			ID:                         a.ID,
			Name:                       a.Name,
			Subtype:                    a.Subtype,
			Description:                optionalString(a.Description),
			ApproximateCountLowerBound: a.CountLower,
			ApproximateCountUpperBound: a.CountUpper,
		})
	}
	return &conn.MetaCustomAudiences{
		AccountID:     res.AccountID,
		Audiences:     out,
		AudienceCount: len(out),
		Truncated:     truncated,
	}, nil
}
