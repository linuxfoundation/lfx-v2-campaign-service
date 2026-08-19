// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// googleAdsKeywordInsights reuses the account-discovery descriptor's status mapping under a
// different noun. The `operation` field exists for exactly this: the classification arms
// (404 for no connection, 500 that logs but never echoes a decryption failure, 400 rather
// than 503 for a connection no amount of waiting repairs) are reasoned about once in
// classifyDiscoveryError, and per this helper's contract a caller gets all of them or none.
// Only the operation noun changes, because the caller did not perform account discovery.
var googleAdsKeywordInsights = accountDiscovery{
	provider:    model.ProviderGoogleAds,
	displayName: "google ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"every field set, and that login_customer_id is digits only",
	operation: "keyword insights",
}

// resolveInsightsWindow maps the optional caller-supplied window onto the closed
// platform-agnostic vocabulary, defaulting to last_30_days.
//
// Unlike the campaign-scoped metrics read there is no platform-aware default to apply: this
// surface is Google Ads only, and Google serves every window in the vocabulary.
//
// The design layer already constrains this parameter with the same Enum, so an HTTP caller
// cannot reach the error arm. It is enforced here anyway for non-HTTP callers, and because a
// runtime rejection with no matching design constraint (or the reverse) is the drift this
// repo's rules exist to prevent.
func resolveInsightsWindow(window *string) (model.MetricsWindow, error) {
	w := model.MetricsWindowLast30Days
	if window != nil {
		w = model.MetricsWindow(*window)
		if !model.IsValidMetricsWindow(w) {
			return "", &conn.BadRequestError{
				Code:    "400",
				Message: "window must be one of: today, yesterday, last_7_days, last_14_days, last_30_days, this_month, last_month",
			}
		}
	}
	return w, nil
}

// classifyInsightsError maps an orchestrator failure onto this service's error set.
//
// It delegates everything it can to classifyDiscoveryError — the connection-state arms are
// identical, and a second copy is where one of those judgements quietly diverges. Only the
// two sentinels discovery has no notion of are handled here first.
func (s *ConnectionService) classifyInsightsError(ctx context.Context, projectID string, err error) error {
	switch {
	case errors.Is(err, domain.ErrKeywordInsightsUnsupported):
		return &conn.BadRequestError{Code: "400", Message: "keyword and audience insights are not supported for this platform"}
	case errors.Is(err, domain.ErrMetricsWindowUnsupported):
		// The adapter's wrapped detail is logged, never concatenated into the client-facing
		// message: it can carry an allow-list of internal GAQL literals.
		slog.WarnContext(ctx, "keyword insights window unsupported by platform",
			"project_id", projectID, "error", safeErrSummary(err))
		return &conn.BadRequestError{Code: "400", Message: "this reporting window is not supported"}
	case errors.Is(err, ErrCampaignAccountMismatch):
		// Raised when the scope filter refuses ANY campaign, not only when it refuses every
		// one — googleAdsScopeForCustomer fails closed on a PARTIAL mismatch too, rather than
		// returning the matching subset as though it were the project's whole picture. Either
		// way it is permanent until the rows and the connection are reconciled, so it must not
		// reach the 503 default, which invites retrying a read that will keep failing.
		//
		// The remedy must therefore cover the MIXED case, which is the common one: some
		// campaigns were created under the account the connection now resolves to and others
		// under an older one. "Reconnect the original account" is wrong there — it would only
		// swap which subset mismatches, breaking the campaigns that currently match. Naming
		// reconciliation of the mismatched ROWS first is the remedy that works in both cases;
		// reconnecting is offered only for the case where a single account really does own
		// every campaign in scope.
		//
		// The two customer ids stay server-side, as on every sibling arm — which ad account a
		// project is connected to is connection configuration, not something a keyword read
		// should disclose. That is also why the message cannot say WHICH campaigns mismatch.
		slog.WarnContext(ctx, "keyword insights blocked: at least one campaign in scope belongs to a different ad account than the current connection",
			"project_id", projectID, "error", safeErrSummary(err))
		return &conn.ConflictError{Code: "409", Message: "some of this project's campaigns were created under a different ad account than its current connection — re-dispatch or reconcile those campaigns onto the connected account to read their keywords, or reconnect the account that owns all of them"}
	default:
		return s.classifyDiscoveryError(ctx, projectID, googleAdsKeywordInsights, err)
	}
}

// GetGoogleAdsKeywords reads Google Ads keyword performance across the project's OWN campaigns.
//
// A pure read-through: nothing is persisted, and this service stores no keyword of its own.
// It enumerates nothing this service holds, which is why it does not breach the
// Query-Service rule in docs/api-catalog.md — the same reasoning the catalog already applies
// to GET /projects/{projectId}/connection-google-ads/accounts.
func (s *ConnectionService) GetGoogleAdsKeywords(ctx context.Context, p *conn.GetGoogleAdsKeywordsPayload) (*conn.GoogleAdsKeywords, error) {
	// The reserved LF scope is unaddressable, exactly as it is for account discovery: left
	// open, a GET here decrypts the LF credential and reports the Linux Foundation's own
	// keyword performance. A project with no connection of its own still reads through the
	// fallback under ITS OWN id, deliberately; naming the reserved scope directly is the
	// different thing, and it is rejected.
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveInsightsWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("keyword insights")
	if err != nil {
		return nil, err
	}
	kp, kerr := orch.ReadKeywordPerformance(ctx, p.ProjectID, model.ProviderGoogleAds, window)
	if kerr != nil {
		return nil, s.classifyInsightsError(ctx, p.ProjectID, kerr)
	}
	// Preallocated with make so an empty result serializes as `[]`, never `null`.
	rows := make([]*conn.GoogleAdsKeyword, 0, len(kp.Rows))
	for _, r := range kp.Rows {
		rows = append(rows, &conn.GoogleAdsKeyword{
			CriterionID: r.CriterionID,
			AdGroupID:   r.AdGroupID,
			CampaignID:  r.CampaignID,
			Text:        r.Text,
			MatchType:   r.MatchType,
			Status:      r.Status,
			Impressions: r.Impressions,
			Clicks:      r.Clicks,
			CostMicros:  r.CostMicros,
			Ctr:         r.Ctr,
		})
	}
	return &conn.GoogleAdsKeywords{
		Window:    string(kp.Window),
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: kp.Truncated,
	}, nil
}

// GetGoogleAdsAudience reads Google Ads demographics across the project's OWN campaigns.
//
// Age, gender and device arrive in one flat array discriminated by `dimension`. Each
// dimension independently covers the same traffic, so a consumer must total within a
// dimension and never across them — stated on the design type as well, because the flat
// array makes the mistake easy to reach.
func (s *ConnectionService) GetGoogleAdsAudience(ctx context.Context, p *conn.GetGoogleAdsAudiencePayload) (*conn.GoogleAdsAudience, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	window, err := resolveInsightsWindow(p.Window)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("audience insights")
	if err != nil {
		return nil, err
	}
	ai, aerr := orch.ReadAudienceInsights(ctx, p.ProjectID, model.ProviderGoogleAds, window)
	if aerr != nil {
		return nil, s.classifyInsightsError(ctx, p.ProjectID, aerr)
	}
	buckets := make([]*conn.GoogleAdsAudienceBucket, 0, len(ai.Buckets))
	for _, b := range ai.Buckets {
		buckets = append(buckets, &conn.GoogleAdsAudienceBucket{
			Dimension:   b.Dimension,
			Value:       b.Value,
			Impressions: b.Impressions,
			Clicks:      b.Clicks,
			CostMicros:  b.CostMicros,
			Ctr:         b.Ctr,
		})
	}
	return &conn.GoogleAdsAudience{
		Window:      string(ai.Window),
		Buckets:     buckets,
		BucketCount: len(buckets),
	}, nil
}
