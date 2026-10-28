// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// GeoTargetSuggester is an OPTIONAL dispatcher capability: search the platform's geo target
// table by place name or postal code. Type-asserted like AccountLister; a dispatcher without
// it yields ErrTargetingLookupUnsupported (400) and the platform is never contacted.
//
// A successful call MUST return a NON-NIL slice, for the reason AccountLister gives. cached
// reports that the platform was not contacted (the answer came from a cache), so no upstream
// call is recorded for it.
type GeoTargetSuggester interface {
	SuggestGeoTargets(ctx context.Context, projectID string, platform model.Provider, q model.GeoTargetQuery) (rows []model.GeoTargetSuggestion, cached bool, err error)
}

// UserListReader is an OPTIONAL dispatcher capability: list the user lists (audiences) on one
// ad account. accountID empty means the account the project's connection is bound to.
//
// A successful call MUST return a non-nil result with a non-nil Lists slice.
type UserListReader interface {
	ListUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (*model.AudienceUserLists, error)
}

// ErrTargetingLookupUnsupported re-exports the domain sentinel, as the siblings in
// orchestrator.go do.
var ErrTargetingLookupUnsupported = domain.ErrTargetingLookupUnsupported

const (
	opSuggestGeoTargets = "suggest_geo_targets"
	opListUserLists     = "list_user_lists"
)

// ReadGeoTargetSuggestions runs a geo target search through the platform's dispatcher. A pure
// read; nothing is persisted.
func (o *Orchestrator) ReadGeoTargetSuggestions(ctx context.Context, projectID string, platform model.Provider, q model.GeoTargetQuery) ([]model.GeoTargetSuggestion, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	s, ok := d.(GeoTargetSuggester)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	rows, cached, err := s.SuggestGeoTargets(callCtx, projectID, platform, q)
	// A cache hit never reached the platform: recording it would overstate upstream traffic
	// and drag the latency histogram toward zero, hiding whether the cache saves quota.
	if !cached {
		o.recordUpstream(ctx, platform, opSuggestGeoTargets, start, err)
	}
	if err != nil {
		return nil, err
	}
	if rows == nil {
		// Same contract violation ReadAccounts refuses: nil cannot be told from "no matches".
		return nil, fmt.Errorf("%s geo target suggester returned a nil result with no error", platform)
	}
	return rows, nil
}

// ReadUserLists lists the user lists on an ad account through the platform's dispatcher. A
// pure read; nothing is persisted.
func (o *Orchestrator) ReadUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (*model.AudienceUserLists, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(UserListReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	// Refused on a shared-tenancy platform, before any credential is resolved or Google is
	// contacted. AccountMonitorScoper is this codebase's marker for "one upstream account shared
	// by every foundation" (docs/architecture.md, "Account Tenancy"). Refusing the LF system
	// fallback is not enough there: a project's OWN connection row points at the same shared
	// customer, so an account-wide user_list read returns every foundation's audiences, and the
	// monitor's remedy (filter to recorded ids) has nothing to filter by — no user list is
	// recorded per project. The BFF's ROOT-only gate (lfx-self-serve-ops#54) is not a boundary
	// this service can rely on, since a project campaign_manager can call it directly.
	if _, shared := d.(AccountMonitorScoper); shared {
		return nil, fmt.Errorf("%w: %s", domain.ErrUserListsSharedTenancy, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	lists, err := r.ListUserLists(callCtx, projectID, platform, accountID)
	o.recordUpstream(ctx, platform, opListUserLists, start, err)
	if err != nil {
		return nil, err
	}
	if lists == nil || lists.Lists == nil {
		return nil, fmt.Errorf("%s user list reader returned a nil result with no error", platform)
	}
	return lists, nil
}

// googleAdsGeoTargetLookup and googleAdsUserListLookup reuse classifyDiscoveryError's
// connection-state arms under their own operation nouns, as the keyword read does.
var (
	googleAdsGeoTargetLookup = accountDiscovery{
		provider:    model.ProviderGoogleAds,
		displayName: "google ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
			"every field set, and that login_customer_id is digits only",
		operation: "geo target search",
	}
	googleAdsUserListLookup = accountDiscovery{
		provider:    model.ProviderGoogleAds,
		displayName: "google ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
			"every field set, that login_customer_id is digits only, and that an account_id is " +
			"selected on the connection or passed as account_id",
		operation: "user list lookup",
	}
)

const (
	// googleAdsGeoSuggestMax mirrors the design constant of the same name and
	// googleAdsUserListMax the cap the user_lists description publishes
	// (design/google_ads_lookups.go); the HTTP decoder enforces the first, and this layer
	// re-applies it for a caller that bypasses Goa.
	googleAdsGeoSuggestMax     = 50
	googleAdsGeoSuggestDefault = 20
	googleAdsUserListMax       = googleads.MaxUserListRows
	// googleAdsGeoQueryMaxLen / googleAdsGeoTargetTypesMax mirror the payload's MaxLength.
	googleAdsGeoQueryMaxLen    = 100
	googleAdsGeoTargetTypesMax = 10
)

// classifyTargetingLookupError adds the one sentinel discovery has no notion of, then
// delegates every connection-state arm to classifyDiscoveryError.
func (s *ConnectionService) classifyTargetingLookupError(ctx context.Context, projectID string, d accountDiscovery, err error) error {
	if errors.Is(err, ErrTargetingLookupUnsupported) {
		return &conn.BadRequestError{Code: "400", Message: d.label() + " is not supported for this platform"}
	}
	if errors.Is(err, domain.ErrUserListsSharedTenancy) {
		return &conn.ConflictError{Code: "409", Message: domain.ErrUserListsSharedTenancy.Error() + "; enter the user-list resource name directly"}
	}
	return s.classifyDiscoveryError(ctx, projectID, d, err)
}

// SuggestGoogleAdsGeoTargets searches Google's geo target constants for the geoTargets picker.
//
// Resolution follows account discovery, fallback included: the answer is Google's public
// location table, so reading it through the LF system credential exposes no foundation's data
// (see GoogleAdsDispatcher.SuggestGeoTargets). The reserved LF scope stays unaddressable,
// exactly as it is for discovery.
func (s *ConnectionService) SuggestGoogleAdsGeoTargets(ctx context.Context, p *conn.SuggestGoogleAdsGeoTargetsPayload) (*conn.GoogleAdsGeoTargetSuggestions, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	q, limit, types, err := validateGeoTargetSuggestPayload(p)
	if err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(googleAdsGeoTargetLookup.label())
	if err != nil {
		return nil, err
	}
	rows, rerr := orch.ReadGeoTargetSuggestions(ctx, p.ProjectID, model.ProviderGoogleAds, q)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, googleAdsGeoTargetLookup, rerr)
	}
	out := make([]*conn.GoogleAdsGeoTargetSuggestion, 0, min(len(rows), limit))
	truncated := false
	for _, r := range rows {
		if len(types) > 0 {
			if _, keep := types[googleads.NormalizeGeoTargetType(r.TargetType)]; !keep {
				continue
			}
		}
		if len(out) == limit {
			truncated = true
			break
		}
		out = append(out, &conn.GoogleAdsGeoTargetSuggestion{
			ID:            r.ID,
			Name:          r.Name,
			CanonicalName: r.CanonicalName,
			CountryCode:   r.CountryCode,
			TargetType:    r.TargetType,
			Status:        r.Status,
			Reach:         r.Reach,
		})
	}
	return &conn.GoogleAdsGeoTargetSuggestions{
		Suggestions:     out,
		SuggestionCount: len(out),
		Truncated:       truncated,
	}, nil
}

// validateGeoTargetSuggestPayload mirrors the design constraints for a caller that bypasses
// the generated HTTP decoder, and normalizes the inputs.
func validateGeoTargetSuggestPayload(p *conn.SuggestGoogleAdsGeoTargetsPayload) (model.GeoTargetQuery, int, map[string]struct{}, error) {
	query := strings.TrimSpace(p.Q)
	if query == "" || utf8.RuneCountInString(p.Q) > googleAdsGeoQueryMaxLen {
		return model.GeoTargetQuery{}, 0, nil, &conn.BadRequestError{Code: "400", Message: "q must be 1-100 characters and not blank"}
	}
	country := ""
	if p.CountryCode != nil {
		country = strings.ToUpper(strings.TrimSpace(*p.CountryCode))
		if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' {
			return model.GeoTargetQuery{}, 0, nil, &conn.BadRequestError{Code: "400", Message: "country_code must be an ISO 3166-1 alpha-2 code"}
		}
	}
	limit := googleAdsGeoSuggestDefault
	if p.Limit != nil {
		limit = *p.Limit
	}
	if limit < 1 || limit > googleAdsGeoSuggestMax {
		return model.GeoTargetQuery{}, 0, nil, &conn.BadRequestError{Code: "400", Message: "limit must be between 1 and 50"}
	}
	if len(p.TargetTypes) > googleAdsGeoTargetTypesMax {
		return model.GeoTargetQuery{}, 0, nil, &conn.BadRequestError{Code: "400", Message: "at most 10 target_types are accepted"}
	}
	var types map[string]struct{}
	for _, t := range p.TargetTypes {
		n := googleads.NormalizeGeoTargetType(t)
		if n == "" || utf8.RuneCountInString(t) > 40 {
			return model.GeoTargetQuery{}, 0, nil, &conn.BadRequestError{Code: "400", Message: "each target_types entry must be 1-40 characters and not blank"}
		}
		if types == nil {
			types = map[string]struct{}{}
		}
		types[n] = struct{}{}
	}
	return model.GeoTargetQuery{Query: query, CountryCode: country}, limit, types, nil
}

// ListGoogleAdsUserLists lists the user lists on a Google Ads customer for the
// audienceSegments picker.
//
// OWN connection only, never the LF system fallback — see GoogleAdsDispatcher.ListUserLists
// for why (user lists are account data on a customer every foundation shares). A project with
// no connection of its own therefore gets the 404 classifyDiscoveryError gives ErrNotFound.
func (s *ConnectionService) ListGoogleAdsUserLists(ctx context.Context, p *conn.ListGoogleAdsUserListsPayload) (*conn.GoogleAdsUserLists, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	accountID := ""
	if p.AccountID != nil {
		accountID = strings.TrimSpace(*p.AccountID)
		// Mirrors the design Pattern/MaxLength for a caller that bypasses Goa; the dispatcher
		// re-checks the shape too.
		if accountID == "" || len(accountID) > 64 || googleads.ValidateCustomerID(accountID) != nil {
			return nil, &conn.BadRequestError{Code: "400", Message: "account_id must be digits only (no dashes)"}
		}
	}
	_, _, orch, err := s.resolveBackendWithOrch(googleAdsUserListLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadUserLists(ctx, p.ProjectID, model.ProviderGoogleAds, accountID)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, googleAdsUserListLookup, rerr)
	}
	n := min(len(res.Lists), googleAdsUserListMax)
	out := make([]*conn.GoogleAdsUserList, 0, n)
	for _, l := range res.Lists[:n] {
		out = append(out, &conn.GoogleAdsUserList{
			ResourceName:       l.ResourceName,
			ID:                 l.ID,
			Name:               l.Name,
			Type:               l.Type,
			MembershipStatus:   l.MembershipStatus,
			AccountStatus:      optionalString(l.AccountStatus),
			SizeForSearch:      l.SizeForSearch,
			SizeRangeForSearch: optionalString(l.SizeRangeForSearch),
			EligibleForSearch:  l.EligibleForSearch,
		})
	}
	return &conn.GoogleAdsUserLists{
		AccountID:     res.AccountID,
		UserLists:     out,
		UserListCount: len(out),
		Truncated:     len(res.Lists) > n,
	}, nil
}
