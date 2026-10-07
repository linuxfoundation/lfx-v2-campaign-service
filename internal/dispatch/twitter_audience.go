// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

var _ service.TwitterAudienceReader = (*TwitterDispatcher)(nil)

// twitterAudienceEnabled gates the X audience read on TWITTER_METRICS_ENABLED, the flag the X
// account monitor already uses, because the read runs on the same asynchronous stats-jobs contract
// (job creation, job-status read, results file) — X serves segmented stats nowhere else — and that
// contract follows X's public documentation without having been exercised against a live account.
// The synchronous campaign metrics read (ReadMetrics) is not gated and is unaffected. Only the
// exact value "true" enables it, read per call; disabled, the read answers the same 400 as a
// platform with no audience read, before any credential is resolved or request made.
func twitterAudienceEnabled() error {
	if os.Getenv(constants.EnvTwitterMetricsEnabled) != "true" {
		return fmt.Errorf("x audience insights are disabled (%s is not \"true\") while the stats-jobs contract is unverified: %w",
			constants.EnvTwitterMetricsEnabled, domain.ErrKeywordInsightsUnsupported)
	}
	return nil
}

// AudienceEnabled implements service.TwitterAudienceReader: the gate the orchestrator checks
// BEFORE the scope lookup, so a project with no X campaigns gets the same 400 as any other while
// the read is off. ReadTwitterAudienceInsights re-checks it for a caller that skips the
// orchestrator.
func (d *TwitterDispatcher) AudienceEnabled() error { return twitterAudienceEnabled() }

// ReadTwitterAudienceInsights implements service.TwitterAudienceReader: X's AGE, GENDER and
// PLATFORMS segmentations over window, confined to the campaigns in scope.
//
// It mirrors MetaDispatcher.ReadMetaAudienceInsights step for step:
//
//  1. The flag, then the window, are checked BEFORE credentials are resolved, so both refusals
//     are the same 400 whatever the connection's state. The window is the metrics read's
//     (twitterMetricsWindow): yesterday, today, last_7_days.
//  2. Credentials resolve through resolveExisting with no recorded account — the ordinary
//     project-then-system resolution, so a project running on the LF fallback reads only its
//     OWN campaigns there (the job entity_ids are the scope, not the connection). No connection
//     at all surfaces domain.ErrNotFound (404).
//  3. The connection's account id must be one the stats paths can address
//     (twitter.ValidateMonitorAccountID), held before X is contacted.
//  4. Every scope entry's recorded creation account must be the one the connection resolves to.
//     ANY mismatch refuses the whole read (ErrCampaignAccountMismatch, 409) rather than dropping
//     the entry; X campaign ids are unique only within an account.
//
// The CACHED client is used, as by the monitor: job creation goes through the client's write
// pacer, which bounds the account's write rate only if every caller for the connection shares it.
// The call itself goes through d.audience (twitterAudienceGuard): a cached result, a shared
// in-flight read, or one read per ad account at a time — each read holds stats-job slots on an
// account shared across foundations and with the X account monitor.
func (d *TwitterDispatcher) ReadTwitterAudienceInsights(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.TwitterAudienceInsights, error) {
	if err := twitterAudienceEnabled(); err != nil {
		return nil, err
	}
	xWindow, err := twitterMetricsWindow(window)
	if err != nil {
		return nil, fmt.Errorf("read x audience insights: %w", err)
	}
	res, err := d.creds.resolveExisting(ctx, projectID, platform, "")
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	if verr := twitter.ValidateMonitorAccountID(accountID); verr != nil {
		return nil, res.systemScoped(fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr))
	}
	campaignIDs, err := twitterScopeForAccount(scope, accountID, "read x audience insights")
	if err != nil {
		return nil, err
	}
	client := d.cachedTwitterClient(projectID, platform, res, creds, accountID,
		strings.TrimSpace(res.providerConfig["funding_instrument_id"]))
	now := d.audienceNow
	if now == nil {
		now = time.Now
	}
	ai, err := d.audience.read(ctx, accountID, twitterAudienceKey(accountID, xWindow, campaignIDs),
		twitter.AudienceJobCount(campaignIDs), now, client.RunningStatsJobs,
		func(callCtx context.Context) (*twitter.AudienceInsights, error) {
			return client.GetAudienceInsights(callCtx, xWindow, campaignIDs)
		})
	if err != nil {
		switch {
		case errors.Is(err, twitter.ErrAudienceScopeTooLarge):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeTooLarge, err)
		case errors.Is(err, twitter.ErrAudienceScopeInvalid):
			return nil, fmt.Errorf("%w: %w", domain.ErrAudienceScopeInvalid, err)
		case errors.Is(err, twitter.ErrReportWindowNotWholeHours):
			return nil, fmt.Errorf("read x audience insights: %w: %w", domain.ErrAccountTimezoneUnsupported, err)
		}
		return nil, fmt.Errorf("read x audience insights: %w", err)
	}
	buckets := make([]model.TwitterAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, model.TwitterAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	// The REQUEST window, not the client's literal: the API contract is the platform-agnostic
	// vocabulary.
	return &model.TwitterAudienceInsights{Window: window, Currency: ai.Currency, AllCountersNull: ai.AllCountersNull, Buckets: buckets}, nil
}

// twitterScopeForAccount is metaScopeForAccount for X: the campaign ids in scope, provided every
// entry's recorded creation account (twitterCreationAccountID, "" = unknown, proceed) is the
// account the connection resolves to. A stored id that is not a usable X campaign id is refused as
// domain.ErrAudienceScopeInvalid (409) before any request, never dropped.
func twitterScopeForAccount(scope []model.ProjectCampaignScope, accountID, op string) ([]string, error) {
	current := strings.TrimSpace(accountID)
	ids := make([]string, 0, len(scope))
	skipped := 0
	for _, s := range scope {
		if verr := twitter.ValidateCampaignID(s.PlatformCampaignID); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed x campaign id (%d bytes): %w",
				op, len(s.PlatformCampaignID), domain.ErrAudienceScopeInvalid)
		}
		created := twitterCreationAccountID(&model.Campaign{Result: s.Result})
		if created != "" && created != current {
			skipped++
			continue
		}
		ids = append(ids, s.PlatformCampaignID)
	}
	if skipped > 0 {
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns were created under a different x ads account than the one its connection now resolves to (%s); returning only the rest would report a partial result as complete: %w",
			op, skipped, len(scope), current, domain.ErrCampaignAccountMismatch)
	}
	if len(ids) == 0 {
		// Unreachable from the orchestrator, which answers an empty scope without calling the
		// adapter — refused here anyway so the guarantee does not rest on one caller.
		return nil, fmt.Errorf("%s: no campaigns in scope: %w", op, domain.ErrAudienceScopeInvalid)
	}
	return ids, nil
}
