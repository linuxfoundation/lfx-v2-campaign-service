// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"os"
	"strings"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

var linkedInAdsAdoptableLookup = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "the LF system linkedin connection needs attention: check that it is active, that the " +
		"stored credential is valid json with access_token set, and that its account_id is set",
	operation: "adoptable campaign lookup",
}

// linkedinAdoptionFromSystemEnabled reports whether adoption may name an account the LF system
// LinkedIn row addresses (#581). It is LINKEDIN_ADS_ACCOUNT_SELECTION_ENABLED, the flag that makes
// those accounts nameable at create: one switch decides whether the system row's grants are
// addressable by name from the API. Only the exact value "true" turns it on.
func linkedinAdoptionFromSystemEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsAccountSelectionEnabled) == "true"
}

// errLinkedInAdoptionFromSystemDisabled is the 400 text for either adoption surface while the flag
// is off. Service-authored.
const errLinkedInAdoptionFromSystemDisabled = "adopting LinkedIn campaigns from the LF system account's ad accounts is not enabled on this deployment"

// ListLinkedinAdsAdoptableCampaigns lists the ACTIVE and PAUSED campaigns in one of the LF system
// LinkedIn row's addressable accounts, for adopt-campaign with account_id (#581).
//
// The read lists every foundation's campaigns in that account, so the gateway admits only
// global_marketing_ops of the project (marketing-ops-api), as for the pickers. The reserved LF
// scope stays unaddressable, as it is for every connection read.
func (s *ConnectionService) ListLinkedinAdsAdoptableCampaigns(ctx context.Context, p *conn.ListLinkedinAdsAdoptableCampaignsPayload) (*conn.LinkedinAdsAdoptableCampaigns, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if !linkedinAdoptionFromSystemEnabled() {
		return nil, &conn.BadRequestError{Code: "400", Message: errLinkedInAdoptionFromSystemDisabled}
	}
	accountID := strings.TrimSpace(p.AccountID)
	if accountID == "" {
		return nil, &conn.BadRequestError{Code: "400", Message: "account_id is required"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsAdoptableLookup.label())
	if err != nil {
		return nil, err
	}
	rows, rerr := orch.ListAdoptableCampaigns(ctx, p.ProjectID, model.ProviderLinkedInAds, accountID)
	if rerr != nil {
		if errors.Is(rerr, domain.ErrAccountNotManagedByConnection) {
			return nil, &conn.BadRequestError{Code: "400", Message: "account_id is neither the LF system LinkedIn account's default account nor one of its granted accounts"}
		}
		if errors.Is(rerr, ErrAdoptionUnsupported) {
			return nil, &conn.BadRequestError{Code: "400", Message: "campaign adoption is not supported for this platform"}
		}
		return nil, s.classifyDiscoveryError(ctx, p.ProjectID, linkedInAdsAdoptableLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsAdoptableCampaign, 0, len(rows))
	for _, r := range rows {
		out = append(out, &conn.LinkedinAdsAdoptableCampaign{ID: r.ID, Name: r.Name, Status: r.Status, RecordedInProject: r.RecordedInProject})
	}
	return &conn.LinkedinAdsAdoptableCampaigns{AccountID: accountID, Campaigns: out, AdoptableCount: len(out)}, nil
}
