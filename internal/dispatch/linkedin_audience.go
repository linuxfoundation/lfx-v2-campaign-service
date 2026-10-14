// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.LinkedInAudienceReader = (*LinkedInDispatcher)(nil)

// ReadLinkedInAudienceInsights implements service.LinkedInAudienceReader: LinkedIn's
// professional-demographic pivots over window, confined to the campaigns in scope.
//
// Steps, in this order:
//
//  1. The window and the scope's SHAPE (canonical ids, at most linkedin.MaxAudienceCampaigns) are
//     checked BEFORE credentials are resolved, so a permanent request or row fault answers the same
//     whatever the connection's state.
//  2. Credentials resolve through resolveOwned — the project's OWN connection only, never the LF
//     system fallback, exactly like the LinkedIn account monitor and the Microsoft audience read. A
//     project with no LinkedIn connection of its own is domain.ErrNotFound (404), not a read served
//     from a credential that reaches other foundations' accounts. resolveLinkedInCredentials also
//     refuses an inactive connection, an incomplete credential and an unselected account.
//  3. Every scope entry's recorded creation account must be the account the connection is bound
//     to. ANY mismatch refuses the whole read (ErrCampaignAccountMismatch, 409) rather than
//     dropping the entry — see googleAdsScopeForCustomer for why a silently reduced scope is worse
//     than a refusal. An unrecorded account ("") is unknown and proceeds.
//  4. The client issues one Analytics-finder request per (pivot, campaign), each naming that one
//     campaign URN and the bound account's URN as the `accounts` facet, and sums the answers.
func (d *LinkedInDispatcher) ReadLinkedInAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.LinkedInAudienceInsights, error) {
	const op = "read linkedin audience insights"
	if werr := linkedin.ValidateMetricsWindow(window); werr != nil {
		return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}
	ids, err := linkedInAudienceScopeIDs(op, scope)
	if err != nil {
		return nil, err
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.resolveOwned)
	if err != nil {
		return nil, err
	}
	accountID := strings.TrimSpace(res.accountID)
	if err := linkedInAudienceScopeForAccount(op, scope, accountID); err != nil {
		return nil, err
	}
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)
	ai, err := client.GetAudienceInsights(ctx, accountID, window, ids)
	if err != nil {
		switch {
		case errors.Is(err, linkedin.ErrAudienceScopeTooLarge):
			return nil, fmt.Errorf("%s: %w: %w", op, domain.ErrAudienceScopeTooLarge, err)
		case errors.Is(err, linkedin.ErrAudienceScopeInvalid):
			return nil, fmt.Errorf("%s: %w: %w", op, domain.ErrAudienceScopeInvalid, err)
		case errors.Is(err, linkedin.ErrInvalidAccountID):
			// The bound account is stored connection configuration, so a malformed one is a
			// connection the operator must repair, not a request fault.
			return nil, res.systemScoped(fmt.Errorf("%s: %w: %w", op, domain.ErrConnectionNotUsable, err))
		case errors.Is(err, linkedin.ErrUnsupportedWindow):
			return nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetricsWindowUnsupported, err))
		case linkedinConnectionDefect(err):
			return nil, res.systemScoped(linkedinExpiry(err))
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	buckets := make([]model.LinkedInAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, model.LinkedInAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	// The REQUEST window, not anything the client echoes: the API contract is the
	// platform-agnostic vocabulary.
	return &model.LinkedInAudienceInsights{Window: window, Buckets: buckets}, nil
}

// linkedInAudienceScopeIDs returns the de-duplicated campaign ids in scope, refusing an empty
// scope, a stored id that is not a canonical LinkedIn campaign id (ErrAudienceScopeInvalid, 409)
// or more than linkedin.MaxAudienceCampaigns distinct ids (ErrAudienceScopeTooLarge, 409) — all
// before any credential is decrypted. Nothing is dropped: a reduced scope would be presented as
// the project's whole picture.
func linkedInAudienceScopeIDs(op string, scope []model.ProjectCampaignScope) ([]string, error) {
	if len(scope) == 0 {
		// Unreachable from the orchestrator, which answers an empty scope without calling the
		// adapter — refused here anyway so the guarantee does not rest on one caller.
		return nil, fmt.Errorf("%s: no campaigns in scope: %w", op, domain.ErrAudienceScopeInvalid)
	}
	seen := make(map[string]struct{}, len(scope))
	ids := make([]string, 0, len(scope))
	for _, s := range scope {
		id := s.PlatformCampaignID
		if verr := linkedin.ValidateAudienceCampaignID(id); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed linkedin campaign id (%d bytes): %w",
				op, len(id), domain.ErrAudienceScopeInvalid)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) > linkedin.MaxAudienceCampaigns {
		return nil, fmt.Errorf("%s: %d campaigns (at most %d): %w", op, len(ids), linkedin.MaxAudienceCampaigns, domain.ErrAudienceScopeTooLarge)
	}
	return ids, nil
}

// linkedInAudienceScopeForAccount refuses the read when ANY scope entry was created under a
// different LinkedIn ad account than the one the connection is bound to (linkedInCreationAccountID;
// "" = unknown, proceed). LinkedIn genuinely runs several ad accounts across foundations, so a
// re-pointed connection is a reachable state, not a defensive one.
func linkedInAudienceScopeForAccount(op string, scope []model.ProjectCampaignScope, accountID string) error {
	mismatched := 0
	for _, s := range scope {
		created := linkedInCreationAccountID(&model.Campaign{PlatformCampaignID: s.PlatformCampaignID, Result: s.Result})
		if created != "" && created != accountID {
			mismatched++
		}
	}
	if mismatched > 0 {
		return fmt.Errorf("%s: %d of this project's %d campaigns were created under a different linkedin ad account than the one its connection is bound to (%s); returning only the rest would report a partial result as complete: %w",
			op, mismatched, len(scope), accountID, domain.ErrCampaignAccountMismatch)
	}
	return nil
}
