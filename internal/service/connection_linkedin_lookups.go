// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// linkedInAdSegmentKind is the AudienceReader kind the LinkedIn dispatcher serves
// (dispatch.LinkedInAdSegmentKind; restated because service cannot import dispatch).
const linkedInAdSegmentKind = "ad_segment"

var (
	linkedInAdsAudienceLookup = accountDiscovery{
		provider:    model.ProviderLinkedInAds,
		displayName: "linkedin ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json " +
			"with access_token set, and that an account_id is selected",
		operation: "ad segment lookup",
	}
	linkedInAdsConversionLookup = accountDiscovery{
		provider:    model.ProviderLinkedInAds,
		displayName: "linkedin ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json " +
			"with access_token set, and that an account_id is selected",
		operation: "conversion rule lookup",
	}
)

// ListLinkedinAdsAudiences lists the ad segments on the LinkedIn ad account a create for this
// project uses, for the linkedInConfig.targeting segment pickers (#552).
//
// The LF system account is the fallback of every project without its own LinkedIn connection,
// so the answer can name segments built for other foundations; the gateway therefore admits
// only global_marketing_ops of the project (marketing-ops-api), as for the Google pickers. The
// reserved LF scope stays unaddressable, as it is for every connection read.
func (s *ConnectionService) ListLinkedinAdsAudiences(ctx context.Context, p *conn.ListLinkedinAdsAudiencesPayload) (*conn.LinkedinAdsAudienceSegments, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadAudiences(ctx, p.ProjectID, model.ProviderLinkedInAds, linkedInAdSegmentKind)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsAudienceLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsAudienceSegment, 0, len(res.Audiences))
	for _, a := range res.Audiences {
		seg := &conn.LinkedinAdsAudienceSegment{
			ID: a.ID, Urn: a.ResourceName, Name: a.Name, Type: a.Subtype, Status: a.Status,
			ApproximateMemberCount: a.MemberCount,
		}
		if a.Facet != "" {
			facet := a.Facet
			seg.Facet = &facet
		}
		out = append(out, seg)
	}
	// Truncated is always false: offsetWalk fails a read rather than truncating it.
	return &conn.LinkedinAdsAudienceSegments{AccountID: res.AccountID, Audiences: out, AudienceCount: len(out), Truncated: false}, nil
}

// ListLinkedinAdsConversions lists the conversion rules on the LinkedIn ad account a create for
// this project uses, for the linkedInConfig.conversions picker (#552). Gated and resolved as
// ListLinkedinAdsAudiences is.
func (s *ConnectionService) ListLinkedinAdsConversions(ctx context.Context, p *conn.ListLinkedinAdsConversionsPayload) (*conn.LinkedinAdsConversionRules, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsConversionLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadConversionActions(ctx, p.ProjectID, model.ProviderLinkedInAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsConversionLookup, rerr)
	}
	out := make([]*conn.LinkedinAdsConversionRule, 0, len(res.Actions))
	for _, a := range res.Actions {
		out = append(out, &conn.LinkedinAdsConversionRule{
			ID: a.ID, Urn: a.ResourceName, Name: a.Name, Type: a.Type, Enabled: a.Status == "ENABLED",
		})
	}
	return &conn.LinkedinAdsConversionRules{AccountID: res.AccountID, Conversions: out, ConversionCount: len(out)}, nil
}

// TargetingEntitySearcher is an OPTIONAL dispatcher capability: search the platform's targeting
// taxonomy by facet and name (#585). Type-asserted like GeoTargetSuggester; a dispatcher without
// it yields ErrTargetingLookupUnsupported (400) and the platform is never contacted.
//
// A successful call MUST return a NON-NIL slice. called reports whether the platform was
// actually contacted: false for a cache hit and for a local refusal before any request.
type TargetingEntitySearcher interface {
	SearchTargetingEntities(ctx context.Context, projectID string, platform model.Provider, q model.TargetingEntityQuery) (rows []model.TargetingEntity, called bool, err error)
}

const opSearchTargetingEntities = "search_targeting_entities"

// targetingEntityCallTimeout bounds one typeahead read. Shorter than accountsCallTimeout: the
// read is one request a picker makes while the operator types, so a LinkedIn 429 whose
// Retry-After would hold it longer is better answered 503 now than after the operator has typed
// on.
const targetingEntityCallTimeout = 10 * time.Second

// ReadTargetingEntities runs a targeting taxonomy search through the platform's dispatcher. A
// pure read; nothing is persisted.
func (o *Orchestrator) ReadTargetingEntities(ctx context.Context, projectID string, platform model.Provider, q model.TargetingEntityQuery) ([]model.TargetingEntity, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	s, ok := d.(TargetingEntitySearcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, targetingEntityCallTimeout)
	defer cancel()
	start := time.Now()
	rows, called, err := s.SearchTargetingEntities(callCtx, projectID, platform, q)
	// Recorded only when the platform was reached, as ReadGeoTargetSuggestions does.
	if called {
		o.recordUpstream(ctx, platform, opSearchTargetingEntities, start, err)
	}
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("%s targeting entity searcher returned a nil result with no error", platform)
	}
	return rows, nil
}

var linkedInAdsTargetingEntitySearch = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
	operation: "targeting entity search",
}

// SearchLinkedinAdsTargetingEntities searches LinkedIn's targeting taxonomy by name for the
// linkedInConfig.targeting and geoTargets pickers (#585).
//
// Gated on campaign_manager (the project-api gateway rule), NOT global_marketing_ops like the
// sibling audience and conversion pickers: those list resources ON the ad account a create uses,
// which is the LF system account for most projects, so they can name other foundations' segments
// and rules. This read names only LinkedIn's own taxonomy (skills, titles, places), identical
// for every token, so a foundation's executive director learns nothing about any other
// foundation from it, and the resolution may fall back to the LF system credential, as
// SuggestGoogleAdsGeoTargets does. The reserved LF scope stays unaddressable.
//
// The query is the operator's free text and is never logged or echoed.
func (s *ConnectionService) SearchLinkedinAdsTargetingEntities(ctx context.Context, p *conn.SearchLinkedinAdsTargetingEntitiesPayload) (*conn.LinkedinAdsTargetingEntities, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	// Re-applies the design constraints for a caller that bypasses the generated decoder.
	if !slices.Contains(linkedin.TypeaheadFacetKeys(), p.Facet) {
		return nil, &conn.BadRequestError{Code: "400", Message: "facet must be one of " + strings.Join(linkedin.TypeaheadFacetKeys(), ", ")}
	}
	query, qerr := linkedin.NormalizeTypeaheadQuery(p.Query)
	if qerr != nil {
		return nil, &conn.BadRequestError{Code: "400", Message: "query must be 1-" + strconv.Itoa(linkedin.MaxTypeaheadQueryLen) +
			" characters, not blank, with no control characters"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(linkedInAdsTargetingEntitySearch.label())
	if err != nil {
		return nil, err
	}
	rows, rerr := orch.ReadTargetingEntities(ctx, p.ProjectID, model.ProviderLinkedInAds, model.TargetingEntityQuery{Facet: p.Facet, Query: query})
	if rerr != nil {
		if errors.Is(rerr, domain.ErrTargetingQueryInvalid) {
			return nil, &conn.BadRequestError{Code: "400", Message: "the targeting search is not valid: check facet and query"}
		}
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, linkedInAdsTargetingEntitySearch, rerr)
	}
	out := make([]*conn.LinkedinAdsTargetingEntity, 0, len(rows))
	for _, r := range rows {
		out = append(out, &conn.LinkedinAdsTargetingEntity{Urn: r.URN, Name: r.Name})
	}
	return &conn.LinkedinAdsTargetingEntities{Facet: p.Facet, Entities: out, EntityCount: len(out)}, nil
}
