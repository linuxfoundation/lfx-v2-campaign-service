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

var (
	_ service.RedditAudienceReader = (*RedditDispatcher)(nil)
	_ service.RedditKeywordReader  = (*RedditDispatcher)(nil)
)

// redditReportsEnabled gates the Reddit broken-down reads (audience and keyword performance) on
// REDDIT_METRICS_ENABLED, the flag the Reddit campaign metrics read already uses, because they run
// on the same reporting operation (POST /ad_accounts/{id}/reports) — matched to Reddit's published
// OpenAPI document but never exercised against a live ad account — and add breakdowns to it, which
// is more of that contract, not less. Only the exact value "true" enables them, read per call;
// disabled, a read answers the same 400 as a platform without it, before any credential is
// resolved or request made.
func redditReportsEnabled(read string) error {
	if os.Getenv(constants.EnvRedditMetricsEnabled) != "true" {
		return fmt.Errorf("reddit %s are disabled (%s is not \"true\") until the reporting contract is exercised against a live ad account: %w",
			read, constants.EnvRedditMetricsEnabled, domain.ErrKeywordInsightsUnsupported)
	}
	return nil
}

// AudienceEnabled implements service.RedditAudienceReader: the gate the orchestrator checks BEFORE
// the scope lookup. ReadRedditAudienceInsights re-checks it for a caller that skips the orchestrator.
func (d *RedditDispatcher) AudienceEnabled() error { return redditReportsEnabled("audience insights") }

// KeywordsEnabled implements service.RedditKeywordReader, AudienceEnabled's twin.
func (d *RedditDispatcher) KeywordsEnabled() error { return redditReportsEnabled("keyword insights") }

// redditReportSentinels are the domain sentinels one broken-down read reports its scope refusals
// under: the audience read's (ErrAudienceScope*) or the keyword read's (ErrKeywordReportScope*),
// each with its own fixed, client-safe 409 text.
type redditReportSentinels struct {
	tooLarge, invalid error
}

var (
	redditAudienceSentinels = redditReportSentinels{tooLarge: domain.ErrAudienceScopeTooLarge, invalid: domain.ErrAudienceScopeInvalid}
	redditKeywordSentinels  = redditReportSentinels{tooLarge: domain.ErrKeywordReportScopeTooLarge, invalid: domain.ErrKeywordReportScopeInvalid}
)

// redditReportClient is the shared prelude of the Reddit broken-down reads, mirroring
// TwitterDispatcher.ReadTwitterAudienceInsights step for step with ONE deliberate difference — the
// credential:
//
//  1. The window is checked BEFORE credentials are resolved (the caller has already checked the
//     flag), so the refusal is the same 400 whatever the connection's state. The window set is the
//     campaign metrics read's (reddit.ValidateMetricsWindow).
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
// The CACHED client is returned, as for the metrics read, so a read reuses the connection's OAuth
// access token rather than minting one per call.
func (d *RedditDispatcher) redditReportClient(ctx context.Context, op, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope, s redditReportSentinels) (*reddit.Client, []string, error) {
	if werr := reddit.ValidateMetricsWindow(window); werr != nil {
		return nil, nil, fmt.Errorf("%s: %w", op, errors.Join(domain.ErrMetricsWindowUnsupported, werr))
	}
	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.resolveOwned)
	if err != nil {
		return nil, nil, err
	}
	accountID := strings.TrimSpace(res.accountID)
	if verr := reddit.ValidateAccountID(accountID); verr != nil {
		return nil, nil, fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr)
	}
	campaignIDs, err := redditScopeForAccount(scope, accountID, op, s.invalid)
	if err != nil {
		return nil, nil, err
	}
	return client, campaignIDs, nil
}

// classifyRedditReportError re-tags a broken-down read's client refusals with the read's domain
// sentinels; every upstream failure is returned unclassified (→ 503).
func classifyRedditReportError(op string, err error, s redditReportSentinels) error {
	switch {
	case errors.Is(err, reddit.ErrReportScopeTooLarge):
		return fmt.Errorf("%w: %w", s.tooLarge, err)
	case errors.Is(err, reddit.ErrReportScopeInvalid):
		return fmt.Errorf("%w: %w", s.invalid, err)
	case errors.Is(err, reddit.ErrUnsupportedWindow):
		return fmt.Errorf("%s: %w: %w", op, domain.ErrMetricsWindowUnsupported, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ReadRedditAudienceInsights implements service.RedditAudienceReader: Reddit's GENDER, COUNTRY and
// COMMUNITY report breakdowns over window, confined to the campaigns in scope (redditReportClient).
func (d *RedditDispatcher) ReadRedditAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.RedditAudienceInsights, error) {
	const op = "read reddit audience insights"
	if err := d.AudienceEnabled(); err != nil {
		return nil, err
	}
	client, campaignIDs, err := d.redditReportClient(ctx, op, projectID, platform, window, scope, redditAudienceSentinels)
	if err != nil {
		return nil, err
	}
	ai, err := client.GetAudienceInsights(ctx, window, campaignIDs)
	if err != nil {
		return nil, classifyRedditReportError(op, err, redditAudienceSentinels)
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

// ReadRedditKeywordPerformance implements service.RedditKeywordReader: Reddit's KEYWORD report
// breakdown over window, one row per (campaign, keyword), confined to the campaigns in scope
// (redditReportClient) and capped at reddit.MaxKeywordRows by impressions.
func (d *RedditDispatcher) ReadRedditKeywordPerformance(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.RedditKeywordPerformance, error) {
	const op = "read reddit keyword performance"
	if err := d.KeywordsEnabled(); err != nil {
		return nil, err
	}
	client, campaignIDs, err := d.redditReportClient(ctx, op, projectID, platform, window, scope, redditKeywordSentinels)
	if err != nil {
		return nil, err
	}
	kp, err := client.GetKeywordPerformance(ctx, window, campaignIDs)
	if err != nil {
		return nil, classifyRedditReportError(op, err, redditKeywordSentinels)
	}
	rows := make([]model.RedditKeywordRow, 0, len(kp.Rows))
	for _, r := range kp.Rows {
		rows = append(rows, model.RedditKeywordRow{
			CampaignID:  r.CampaignID,
			Text:        r.Text,
			Impressions: r.Impressions,
			Clicks:      r.Clicks,
			CostMicros:  r.CostMicros,
			Ctr:         r.Ctr,
		})
	}
	return &model.RedditKeywordPerformance{Window: window, Rows: rows, Truncated: kp.Truncated}, nil
}

// redditScopeForAccount is twitterScopeForAccount for Reddit: the campaign ids in scope, provided
// every entry's recorded creation account (redditCreationAccountID, "" = unknown, proceed) is the
// account the connection resolves to. A stored id that is not a usable Reddit campaign id is
// refused as invalid (the read's own 409 sentinel) before any request, never dropped.
func redditScopeForAccount(scope []model.ProjectCampaignScope, accountID, op string, invalid error) ([]string, error) {
	current := strings.TrimSpace(accountID)
	ids := make([]string, 0, len(scope))
	skipped := 0
	for _, s := range scope {
		if verr := reddit.ValidateCampaignID(s.PlatformCampaignID); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed reddit campaign id (%d bytes): %w",
				op, len(s.PlatformCampaignID), invalid)
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
		return nil, fmt.Errorf("%s: no campaigns in scope: %w", op, invalid)
	}
	return ids, nil
}
