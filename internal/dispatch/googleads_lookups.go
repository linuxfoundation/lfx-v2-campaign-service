// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// SuggestGeoTargets implements service.GeoTargetSuggester for Google Ads: it searches Google's
// geo target constants by name or postal code.
//
// It resolves the way ListAccounts does (resolveGoogleAdsDiscoveryClient), so a project with no
// connection of its own reads through the LF system credential. That is safe HERE and nowhere
// it is refused: the answer is Google's public location table, the same for every credential,
// and it reveals nothing any ad account holds. The shared-account rule (monitor, probe,
// adoption, user lists) exists because those reads return one foundation's data to another;
// this one returns no foundation's data at all.
func (d *GoogleAdsDispatcher) SuggestGeoTargets(ctx context.Context, projectID string, platform model.Provider, q model.GeoTargetQuery) ([]model.GeoTargetSuggestion, error) {
	client, err := d.resolveGoogleAdsDiscoveryClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	rows, serr := client.SuggestGeoTargetConstants(ctx, q.Query, q.CountryCode)
	if serr != nil {
		return nil, serr
	}
	// make, not var: an empty answer is a real answer and must stay distinguishable from none.
	out := make([]model.GeoTargetSuggestion, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.GeoTargetSuggestion{
			ID:            r.ID,
			Name:          r.Name,
			CanonicalName: r.CanonicalName,
			CountryCode:   r.CountryCode,
			TargetType:    r.TargetType,
			Status:        r.Status,
			Reach:         r.Reach,
		})
	}
	return out, nil
}

// ListUserLists implements service.UserListReader for Google Ads: every user list on one
// customer, read with GAQL on user_list.
//
// accountID names the customer to read; empty means the customer the project's connection is
// bound to. Like the account monitor (ListAccountCampaignMetrics), it resolves the project's
// OWN connection only (resolveOwnedGoogleAdsDiscovery) and never the LF system fallback. A user
// list is account data — its name, type and size — and Google Ads is ONE customer shared by
// every foundation (docs/architecture.md, "Account Tenancy"), so through the fallback any
// project with no connection of its own would enumerate every foundation's audiences. That is
// the reason the BFF made shared-account reads root-only (lfx-self-serve-ops#54), and the
// reason this service refuses the fallback for every caller rather than trusting a gate one
// layer up. A project with no connection of its own gets domain.ErrNotFound (404).
//
// Lists cannot be project-scoped the way the monitor's campaigns are: this service records no
// user list, so there is no project-owned set to filter to. What the credential reaches is
// what is returned, the same boundary adoption's resolver documents.
func (d *GoogleAdsDispatcher) ListUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (*model.AudienceUserLists, error) {
	accountID = strings.TrimSpace(accountID)
	// Validated before any credential is resolved, as the monitor does: a malformed id is a
	// permanent request fault whatever the connection's state.
	if accountID != "" {
		if err := googleads.ValidateCustomerID(accountID); err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
		}
	}
	client, res, err := d.resolveOwnedGoogleAdsDiscovery(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		stored := strings.TrimSpace(res.accountID)
		if stored == "" {
			// The connection was created credentials-first and no account was chosen yet.
			// Both sentinels, as Microsoft's dispatch does: ErrConnectionNotUsable decides the
			// status (400), ErrAccountNotSelected names the missing choice.
			return nil, fmt.Errorf("%w: %w: project %s names no google ads account and none was requested",
				domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
		}
		if verr := googleads.ValidateCustomerID(stored); verr != nil {
			return nil, fmt.Errorf("%w: %w: stored google ads account id: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, verr)
		}
		accountID = stored
	}
	rows, lerr := client.ListUserLists(ctx, accountID)
	if lerr != nil {
		return nil, lerr
	}
	lists := make([]model.AudienceUserList, 0, len(rows))
	for _, r := range rows {
		lists = append(lists, model.AudienceUserList{
			ResourceName:       r.ResourceName,
			ID:                 r.ID,
			Name:               r.Name,
			Type:               r.Type,
			MembershipStatus:   r.MembershipStatus,
			AccountStatus:      r.AccountStatus,
			SizeForSearch:      r.SizeForSearch,
			SizeRangeForSearch: r.SizeRangeForSearch,
			EligibleForSearch:  r.EligibleForSearch,
		})
	}
	return &model.AudienceUserLists{AccountID: accountID, Lists: lists}, nil
}
