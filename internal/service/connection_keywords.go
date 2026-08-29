// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

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
			CriterionID:  r.CriterionID,
			AdGroupID:    r.AdGroupID,
			CampaignID:   r.CampaignID,
			AdGroupName:  r.AdGroupName,
			CampaignName: r.CampaignName,
			Text:         r.Text,
			MatchType:    r.MatchType,
			Status:       r.Status,
			// Optional on the wire, so nil serialises as an ABSENT key rather than 0 — the
			// distinction the whole pointer chain exists to preserve. A caller renders the
			// absent case as unknown, never as a low score.
			QualityScore: r.QualityScore,
			Impressions:  r.Impressions,
			Clicks:       r.Clicks,
			CostMicros:   r.CostMicros,
			Ctr:          r.Ctr,
			Conversions:  r.Conversions,
		})
	}
	return &conn.GoogleAdsKeywords{
		Window:    string(kp.Window),
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: kp.Truncated,
	}, nil
}

// ResolveGoogleAdsCampaign maps one Google Ads campaign id to this service's own campaign
// and brief, so a caller holding keyword rows can address the brief-scoped mutation routes.
//
// The keyword read publishes `campaign_id` as GOOGLE's numeric id, because that is what the
// GAQL rows carry; every mutation route here is keyed by this service's own campaign UUID under
// its brief. Nothing else bridges the two, so without this a keyword table cannot act on the
// rows it just displayed.
//
// NOT a dispatcher call: the answer is entirely in this service's tables, so no connection is
// resolved and Google is never contacted. That is why it declares no 409 — there is no
// connection to be unusable and no ad account to mismatch.
//
// It DOES declare a 503: resolveBackendWithOrch refuses until storage and the orchestrator are
// wired. That is USUALLY cold start, and retrying is then the right answer — but it is not the
// only case. In the supported no-database mode NewContainer leaves the repository and
// orchestrator nil deliberately, so its routes stay mounted and answer this typed 503 rather
// than a bare 404 — and there the same status persists for the life of the process, however
// long a caller retries. A client must treat 503 as "not available yet", never as a promise
// that waiting will change it.
//
// A storage FAULT is a 500 instead — a failure in a service already up, where retrying does not
// help. Both reach the caller from this one method, so both are declared and kept
// distinguishable.
//
// An unowned id is an empty `matches` with a 200, NOT a 404. "This project owns no campaign
// with that upstream id" is an answer the caller acts on by refusing the action, and a 404
// would say something different — that the route or the project is wrong. Distinguishing them
// is the difference between a caller reporting "not your campaign" and one retrying a request
// that will never work.

// validateGoogleAdsCampaignID mirrors the DSL constraint on `platform_campaign_id`.
//
// Kept as a named helper rather than inlined so the two cannot drift silently: if the design
// bound changes, this is the one other place that has to move, and it says so.
//
// 19 is len(math.MaxInt64), the widest a Google Ads numeric id can be. Digits-only matters
// beyond tidiness: the id is compared as a STRING against stored platform ids, so a value that
// is not a canonical decimal integer can never match a real row — it can only produce a
// confident "no such campaign".
func validateGoogleAdsCampaignID(id string) error {
	const maxGoogleAdsCampaignIDLen = 19
	const badID = "the campaign id must be 1-19 digits, without a leading zero, and within int64"

	if id == "" || len(id) > maxGoogleAdsCampaignIDLen {
		return &conn.BadRequestError{Code: "400", Message: badID}
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return &conn.BadRequestError{Code: "400", Message: badID}
		}
	}
	// Canonical decimal only. The doc comment above is the reason: the id is compared as a
	// STRING against stored platform ids, so "007" is a different row from "7" and simply
	// matches nothing — the caller gets a confident 200 "not your campaign" for an id that is
	// really just misspelled. "0" is not a Google Ads id at all.
	if id[0] == '0' {
		return &conn.BadRequestError{Code: "400", Message: badID}
	}
	// 19 digits is len(math.MaxInt64) but not every 19-digit string fits in one, and the values
	// above it are unrepresentable rather than merely absent. Rejecting here keeps "declared 400"
	// and "what the caller actually gets" the same answer.
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return &conn.BadRequestError{Code: "400", Message: badID}
	}
	return nil
}

func (s *ConnectionService) ResolveGoogleAdsCampaign(ctx context.Context, p *conn.ResolveGoogleAdsCampaignPayload) (*conn.PlatformCampaignResolution, error) {
	// Same reserved-scope refusal as the reads above: left open, this would report whether the
	// Linux Foundation's own scope holds a given campaign to any caller.
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	// The DSL's `^[0-9]+$` / MaxLength(19) is enforced by the generated HTTP DECODER only, so a
	// direct service or endpoint caller bypasses it entirely. Unchecked, "abc" or a 20-digit id
	// reaches the query and comes back as a 200 with no matches — which this route documents as
	// "this project owns no campaign with that id", a claim the input never justified. The
	// caller then refuses an action for the wrong reason and cannot tell a typo from an
	// unowned campaign. Mirrored here so the answer is the declared 400 whichever door the
	// request came in by.
	if err := validateGoogleAdsCampaignID(p.PlatformCampaignID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch("resolve campaign reference")
	if err != nil {
		return nil, err
	}
	refs, rerr := orch.ResolvePlatformCampaign(ctx, p.ProjectID, model.ProviderGoogleAds, p.PlatformCampaignID)
	if rerr != nil {
		// NOT classifyInsightsError. Every arm of that classifier describes a PLATFORM failure —
		// an unsupported window, an ad-account mismatch, a connection that cannot be used — and
		// its default reports an upstream Google Ads outage. This lookup contacts no platform at
		// all: the only thing that can fail is this service's own database.
		//
		// Routing it there would advertise a local table fault as a retryable Google Ads problem,
		// in a message naming "keyword insights", and would return a 503 this method does not
		// declare — so the generated server would encode an undeclared error as a 500 anyway.
		// A storage fault is this service's fault and is reported as one.
		slog.ErrorContext(ctx, "campaign reference lookup failed",
			"project_id", p.ProjectID, "error", safeErrSummary(rerr))
		return nil, &conn.InternalServerError{Code: "500", Message: "the campaign reference could not be read"}
	}

	// Preallocated with make so an empty result serializes as `[]`, never `null` — the empty
	// case is the one a caller must be able to read reliably.
	matches := make([]*conn.CampaignRef, 0, len(refs))
	for _, r := range refs {
		matches = append(matches, &conn.CampaignRef{CampaignID: r.CampaignID, BriefID: r.BriefID})
	}
	return &conn.PlatformCampaignResolution{
		// Echoed from the REQUEST, which is safe here precisely because it is not evidence of
		// anything: the caller already knows what it asked, and the matches are what answer it.
		PlatformCampaignID: p.PlatformCampaignID,
		Matches:            matches,
		MatchCount:         len(matches),
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
			Conversions: b.Conversions,
		})
	}
	return &conn.GoogleAdsAudience{
		Window:      string(ai.Window),
		Buckets:     buckets,
		BucketCount: len(buckets),
	}, nil
}
