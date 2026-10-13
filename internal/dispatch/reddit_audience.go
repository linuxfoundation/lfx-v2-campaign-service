// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

var _ service.RedditAudienceReader = (*RedditDispatcher)(nil)

// redditAudienceEnabled gates the Reddit audience read on REDDIT_METRICS_ENABLED, the flag the
// Reddit campaign metrics read already uses, because the audience read runs on the same reporting
// operation (POST /ad_accounts/{id}/reports) — matched to Reddit's published OpenAPI document but
// never exercised against a live ad account — and adds breakdowns to it, which is more of that
// contract, not less. Only the exact value "true" enables it, read per call; disabled, the read
// answers the same 400 as a platform with no audience read, before any credential is resolved or
// request made.
func redditAudienceEnabled() error {
	if os.Getenv(constants.EnvRedditMetricsEnabled) != "true" {
		return fmt.Errorf("reddit audience insights are disabled (%s is not \"true\") until the reporting contract is exercised against a live ad account: %w",
			constants.EnvRedditMetricsEnabled, domain.ErrKeywordInsightsUnsupported)
	}
	return nil
}

// AudienceEnabled implements service.RedditAudienceReader: the gate the orchestrator checks BEFORE
// the scope lookup. ReadRedditAudienceInsights re-checks it for a caller that skips the orchestrator.
func (d *RedditDispatcher) AudienceEnabled() error { return redditAudienceEnabled() }

// ReadRedditAudienceInsights implements service.RedditAudienceReader: Reddit's GENDER, COUNTRY and
// COMMUNITY report breakdowns over window, confined to the campaigns in scope.
//
// It mirrors TwitterDispatcher.ReadTwitterAudienceInsights step for step, with ONE deliberate
// difference — the credential:
//
//  1. The flag, then the window, are checked BEFORE credentials are resolved, so both refusals are
//     the same 400 whatever the connection's state. The window set is the campaign metrics read's
//     (reddit.ValidateMetricsWindow).
//  2. Credentials resolve through resolveOwned: the project's OWN connection only, never the LF
//     system fallback — the account monitor's rule (resolveMonitorClient), not the Meta/X audience
//     reads' project-then-system resolution. A project with no Reddit connection of its own is
//     domain.ErrNotFound (404). A connection with no account id selected is ErrConnectionNotUsable
//     + ErrAccountNotSelected (400), refused by resolveRedditClientWithCreds.
//  3. The connection's bound account must be addressable (reddit.ValidateAccountID) before Reddit
//     is contacted; the client reads ONLY that account.
//  4. Every scope entry's recorded creation account (redditCreationAccountID, "" = unknown,
//     proceed) must be the one the connection resolves to. ANY mismatch refuses the whole read
//     (ErrCampaignAccountMismatch, 409) rather than dropping the entry: Reddit campaign ids are
//     unique only within an account, so a stored id read under another account is another
//     campaign's data or none.
//
// The CACHED client is used, as by the metrics read, so the read reuses the connection's OAuth
// access token rather than minting one per call.
func (d *RedditDispatcher) ReadRedditAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.RedditAudienceInsights, error) {
	if err := redditAudienceEnabled(); err != nil {
		return nil, err
	}
	if werr := reddit.ValidateMetricsWindow(window); werr != nil {
		return nil, fmt.Errorf("read reddit audience insights: %w", errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}
	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.resolveOwned)
	if err != nil {
		return nil, err
	}
	accountID := strings.TrimSpace(res.accountID)
	if verr := reddit.ValidateAccountID(accountID); verr != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr)
	}
	campaignIDs, err := redditScopeForAccount(scope, accountID, "read reddit audience insights")
	if err != nil {
		return nil, err
	}
	ai, err := client.GetAudienceInsights(ctx, window, campaignIDs)
	if err != nil {
		switch {
		case errors.Is(err, reddit.ErrAudienceScopeTooLarge):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeTooLarge, err)
		case errors.Is(err, reddit.ErrAudienceScopeInvalid):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeInvalid, err)
		case errors.Is(err, reddit.ErrUnsupportedWindow):
			return nil, fmt.Errorf("read reddit audience insights: %w: %w", domain.ErrMetricsWindowUnsupported, err)
		}
		return nil, fmt.Errorf("read reddit audience insights: %w", err)
	}
	buckets := make([]model.RedditAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, model.RedditAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	// The REQUEST window, not the client's echo: the API contract is the platform-agnostic
	// vocabulary.
	return &model.RedditAudienceInsights{Window: window, Buckets: buckets}, nil
}

// redditScopeForAccount is twitterScopeForAccount for Reddit: the campaign ids in scope, provided
// every entry's recorded creation account (redditCreationAccountID, "" = unknown, proceed) is the
// account the connection resolves to. A stored id that is not a usable Reddit campaign id is
// refused as domain.ErrAudienceScopeInvalid (409) before any request, never dropped.
func redditScopeForAccount(scope []model.ProjectCampaignScope, accountID, op string) ([]string, error) {
	current := strings.TrimSpace(accountID)
	ids := make([]string, 0, len(scope))
	skipped := 0
	for _, s := range scope {
		if verr := reddit.ValidateCampaignID(s.PlatformCampaignID); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed reddit campaign id (%d bytes): %w",
				op, len(s.PlatformCampaignID), domain.ErrAudienceScopeInvalid)
		}
		created := redditCreationAccountID(&model.Campaign{Result: s.Result})
		if created != "" && created != current {
			skipped++
			continue
		}
		ids = append(ids, s.PlatformCampaignID)
	}
	if skipped > 0 {
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns were created under a different reddit ad account than the one its connection now resolves to; returning only the rest would report a partial result as complete: %w",
			op, skipped, len(scope), domain.ErrCampaignAccountMismatch)
	}
	if len(ids) == 0 {
		// Unreachable from the orchestrator, which answers an empty scope without calling the
		// adapter — refused here anyway so the guarantee does not rest on one caller.
		return nil, fmt.Errorf("%s: no campaigns in scope: %w", op, domain.ErrAudienceScopeInvalid)
	}
	return ids, nil
}
