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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.MetaAudienceReader = (*MetaDispatcher)(nil)

// ReadMetaAudienceInsights implements service.MetaAudienceReader: Meta's age+gender and
// placement breakdowns over window, confined to the campaigns in scope.
//
// It mirrors GoogleAdsDispatcher.ReadAudienceInsights step for step:
//
//  1. The window is translated BEFORE credentials are resolved, so an unsupported window is
//     the same 400 whatever the connection's state.
//  2. Credentials resolve through resolveExisting with no recorded account — the ordinary
//     project-then-system resolution, so a project running on the LF fallback reads only its
//     OWN campaigns there (the filter is the scope, not the connection). No connection at all
//     surfaces domain.ErrNotFound (404).
//  3. The connection must name an ad account (Insights is read on /act_<id>/insights), held to
//     act_<digits> before Meta is contacted.
//  4. Every scope entry's recorded creation account must be the one the connection resolves
//     to. ANY mismatch refuses the whole read (ErrCampaignAccountMismatch, 409) rather than
//     dropping the entry — see googleAdsScopeForCustomer for why a silently reduced scope is
//     worse than a refusal.
func (d *MetaDispatcher) ReadMetaAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.MetaAudienceInsights, error) {
	metaWindow, err := metaMetricsWindow(window)
	if err != nil {
		return nil, fmt.Errorf("read meta audience insights: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(""))
	if err != nil {
		return nil, err
	}
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return nil, err
	}
	if verr := meta.ValidateAccountID(accountID); verr != nil {
		return nil, res.systemScoped(fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr))
	}
	campaignIDs, err := metaScopeForAccount(scope, accountID, "read meta audience insights")
	if err != nil {
		return nil, err
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	ai, err := client.GetAudienceInsights(ctx, accountID, metaWindow, campaignIDs)
	if err != nil {
		switch {
		case errors.Is(err, meta.ErrAudienceScopeTooLarge):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeTooLarge, err)
		case errors.Is(err, meta.ErrAudienceScopeInvalid):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeInvalid, err)
		}
		return nil, err
	}
	buckets := make([]model.MetaAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, model.MetaAudienceBucket{
			Dimension:         b.Dimension,
			Age:               b.Age,
			Gender:            b.Gender,
			PublisherPlatform: b.PublisherPlatform,
			PlatformPosition:  b.PlatformPosition,
			Impressions:       b.Impressions,
			Clicks:            b.Clicks,
			CostMicros:        b.CostMicros,
			Ctr:               b.Ctr,
		})
	}
	// The REQUEST window, not the client's echoed literal: the API contract is the
	// platform-agnostic vocabulary.
	return &model.MetaAudienceInsights{Window: window, Currency: ai.Currency, Buckets: buckets}, nil
}

// metaScopeForAccount is googleAdsScopeForCustomer for Meta: the campaign ids in scope, provided
// every entry's recorded creation account (metaCreationAccountID, "" = unknown, proceed) is the
// account the connection resolves to. A stored id that is not a canonical Meta campaign id is
// refused as domain.ErrAudienceScopeInvalid (409) before any request, never dropped.
func metaScopeForAccount(scope []model.ProjectCampaignScope, accountID, op string) ([]string, error) {
	current := normalizeMetaAccountID(accountID)
	ids := make([]string, 0, len(scope))
	skipped := 0
	for _, s := range scope {
		if verr := meta.ValidateCampaignID(s.PlatformCampaignID); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed meta campaign id (%d bytes): %w",
				op, len(s.PlatformCampaignID), domain.ErrAudienceScopeInvalid)
		}
		created := metaCreationAccountID(&model.Campaign{Result: s.Result})
		if created != "" && created != current {
			skipped++
			continue
		}
		ids = append(ids, strings.TrimSpace(s.PlatformCampaignID))
	}
	if skipped > 0 {
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns were created under a different meta ad account than the one its connection now resolves to (%s); returning only the rest would report a partial result as complete: %w",
			op, skipped, len(scope), current, domain.ErrCampaignAccountMismatch)
	}
	if len(ids) == 0 {
		// Unreachable from the orchestrator, which answers an empty scope without calling the
		// adapter — refused here anyway so the guarantee does not rest on one caller.
		return nil, fmt.Errorf("%s: no campaigns in scope: %w", op, domain.ErrAudienceScopeInvalid)
	}
	return ids, nil
}
