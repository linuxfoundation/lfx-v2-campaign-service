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
// A successful call MUST return a NON-NIL slice, for the reason AccountLister gives. called
// reports whether the platform was actually contacted: false for a cache hit and for a local
// refusal before any request (no connection, an unusable one), so only real upstream calls
// are recorded.
type GeoTargetSuggester interface {
	SuggestGeoTargets(ctx context.Context, projectID string, platform model.Provider, q model.GeoTargetQuery) (rows []model.GeoTargetSuggestion, called bool, err error)
}

// UserListReader is an OPTIONAL dispatcher capability: list the user lists (audiences) on one
// ad account. accountID empty means the account the project's connection is bound to.
//
// A successful call MUST return a non-nil result with a non-nil Lists slice. called reports
// whether the platform was actually contacted: false for a local refusal before the search
// (a malformed account_id, no connection, an unusable one), so only real upstream calls are
// recorded.
type UserListReader interface {
	ListUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (res *model.AudienceUserLists, called bool, err error)
}

// ConversionActionReader is an OPTIONAL dispatcher capability: list the enabled conversion
// actions on the ad account a campaign create for the project would use.
//
// A successful call MUST return a non-nil result with a non-nil Actions slice. called reports
// whether the platform was actually contacted, as GeoTargetSuggester's does: false for a local
// refusal before any request (no connection, an unusable one).
type ConversionActionReader interface {
	ListConversionActions(ctx context.Context, projectID string, platform model.Provider) (res *model.ConversionActions, called bool, err error)
}

// AudienceReader is an OPTIONAL dispatcher capability: list every usable audience entry of one
// kind (#496) on the ad account a campaign create for the project would use.
//
// A successful call MUST return a non-nil result with a non-nil Audiences slice. called reports
// whether the platform was actually contacted, as ConversionActionReader's does.
type AudienceReader interface {
	ListAudiences(ctx context.Context, projectID string, platform model.Provider, kind string) (res *model.AudienceResources, called bool, err error)
}

// MetaCustomAudienceReader is an OPTIONAL dispatcher capability: list every custom audience of the
// Meta ad account a campaign create for the project would use (#550).
//
// A successful call MUST return a non-nil result with a non-nil Audiences slice. called reports
// whether Meta was actually contacted, as ConversionActionReader's does.
type MetaCustomAudienceReader interface {
	ListMetaCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (res *model.MetaCustomAudiences, called bool, err error)
}

// ErrTargetingLookupUnsupported re-exports the domain sentinel, as the siblings in
// orchestrator.go do.
var ErrTargetingLookupUnsupported = domain.ErrTargetingLookupUnsupported

const (
	opSuggestGeoTargets       = "suggest_geo_targets"
	opListUserLists           = "list_user_lists"
	opListConversionActions   = "list_conversion_actions"
	opListAudiences           = "list_audiences"
	opListMetaCustomAudiences = "list_meta_custom_audiences"
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
	rows, called, err := s.SuggestGeoTargets(callCtx, projectID, platform, q)
	// Recorded only when the platform was reached. A cache hit or a local refusal (no or an
	// unusable connection) sent nothing: recording it would overstate upstream traffic, skew
	// the latency histogram toward zero and count local faults as platform errors.
	if called {
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
	// NOT refused on a shared-tenancy platform. It was, with 409, while the route admitted every
	// campaign_manager: the read is account-wide, nothing scopes user lists per project, and a
	// foundation executive director would have seen every foundation's lists. The gateway now
	// admits only global_marketing_ops of the project (the LF marketing team) to this route, the
	// same population that may read the conversion actions and audiences on the shared customer,
	// so the refusal no longer protects anyone and only hid the LF account's lists from the
	// people who build its campaigns.
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	lists, called, err := r.ListUserLists(callCtx, projectID, platform, accountID)
	// Recorded only when Google was reached, as ReadConversionActions does: a local refusal
	// sent nothing, and recording it would count a connection fault as a platform error.
	if called {
		o.recordUpstream(ctx, platform, opListUserLists, start, err)
	}
	if err != nil {
		return nil, err
	}
	if lists == nil || lists.Lists == nil {
		return nil, fmt.Errorf("%s user list reader returned a nil result with no error", platform)
	}
	return lists, nil
}

// ReadConversionActions lists the enabled conversion actions on the account a create for the
// project uses, through the platform's dispatcher. A pure read; nothing is persisted.
//
// Like ReadUserLists, this is NOT refused on a shared-tenancy platform, by decision (#458):
// the read exists for cross-foundation (LF admin) callers choosing the conversion actions a
// Google create attaches, and on Google Ads those actions live on the one customer every
// foundation shares, so the answer holds EVERY foundation's conversion action names. That is
// why the gateway admits only global_marketing_ops of the project (the LF marketing team) to
// this route, not campaign_manager, which also admits a foundation's executive_director (#486).
func (o *Orchestrator) ReadConversionActions(ctx context.Context, projectID string, platform model.Provider) (*model.ConversionActions, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(ConversionActionReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListConversionActions(callCtx, projectID, platform)
	// Recorded only when Google was reached, as ReadGeoTargetSuggestions does: a local refusal
	// sent nothing, and recording it would count a connection fault as a platform error.
	if called {
		o.recordUpstream(ctx, platform, opListConversionActions, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Actions == nil {
		return nil, fmt.Errorf("%s conversion action reader returned a nil result with no error", platform)
	}
	return res, nil
}

// ReadAudiences lists every usable audience entry of one kind on the account a create for the
// project uses, through the platform's dispatcher. A pure read; nothing is persisted.
//
// Like ReadConversionActions, and for the same reason, it is NOT refused on a shared-tenancy
// platform: on Google Ads the answer holds every foundation's audiences, so the gateway admits
// only global_marketing_ops of the project to this route (#496).
func (o *Orchestrator) ReadAudiences(ctx context.Context, projectID string, platform model.Provider, kind string) (*model.AudienceResources, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(AudienceReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListAudiences(callCtx, projectID, platform, kind)
	// Recorded only when the platform was reached, as ReadConversionActions does.
	if called {
		o.recordUpstream(ctx, platform, opListAudiences, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Audiences == nil {
		return nil, fmt.Errorf("%s audience reader returned a nil result with no error", platform)
	}
	return res, nil
}

// metaCustomAudiencesCallTimeout bounds the custom-audience read. It is longer than
// accountsCallTimeout because the walk may make up to 20 sequential Graph calls (the client's page
// bound) and each row asks for Meta's approximate audience size, which Meta computes per audience;
// 20 seconds would leave about a second per page on the largest accounts this read admits.
const metaCustomAudiencesCallTimeout = 45 * time.Second

// ReadMetaCustomAudiences lists every custom audience of the Meta ad account a create for the
// project uses (#550), through the platform's dispatcher. A pure read; nothing is persisted.
//
// Like ReadAudiences, it is NOT refused on a shared-tenancy platform: the LF Meta ad account is
// shared by every foundation, so the answer holds EVERY foundation's audiences, which is why the
// gateway admits only global_marketing_ops of the project to this route.
func (o *Orchestrator) ReadMetaCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (*model.MetaCustomAudiences, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrTargetingLookupUnsupported, platform)
	}
	r, ok := d.(MetaCustomAudienceReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTargetingLookupUnsupported, platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, metaCustomAudiencesCallTimeout)
	defer cancel()
	start := time.Now()
	res, called, err := r.ListMetaCustomAudiences(callCtx, projectID, platform)
	// Recorded only when Meta was reached: a local refusal sent nothing.
	if called {
		o.recordUpstream(ctx, platform, opListMetaCustomAudiences, start, err)
	}
	if err != nil {
		return nil, err
	}
	if res == nil || res.Audiences == nil {
		return nil, fmt.Errorf("%s custom audience reader returned a nil result with no error", platform)
	}
	return res, nil
}

// googleAdsGeoTargetLookup, googleAdsUserListLookup, googleAdsConversionActionLookup and
// googleAdsAudienceLookup reuse
// classifyDiscoveryError's connection-state arms under their own operation nouns, as the keyword
// read does.
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
			"every field set, that login_customer_id is digits only, and that an account_id of " +
			"digits only is selected on the connection",
		operation: "user list lookup",
	}
	googleAdsConversionActionLookup = accountDiscovery{
		provider:    model.ProviderGoogleAds,
		displayName: "google ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
			"every field set, that login_customer_id is digits only, and that an account_id of " +
			"digits only is selected on the connection",
		operation: "conversion action lookup",
	}
	googleAdsAudienceLookup = accountDiscovery{
		provider:    model.ProviderGoogleAds,
		displayName: "google ads",
		notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
			"every field set, that login_customer_id is digits only, and that an account_id of " +
			"digits only is selected on the connection",
		operation: "audience lookup",
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
	// googleAdsAudienceMax is the cap the audiences description publishes, applied after the
	// q filter; googleAdsAudienceQueryMaxLen mirrors the q MaxLength.
	googleAdsAudienceMax         = 500
	googleAdsAudienceQueryMaxLen = 100
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
// It resolves as a create does, the LF system fallback included — see
// GoogleAdsDispatcher.ListUserLists. The answer is every foundation's lists on the shared
// customer, which is why the gateway admits only global_marketing_ops of the project.
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

// ListGoogleAdsConversionActions lists the enabled conversion actions on the Google Ads customer
// a create for this project uses, for the googleAdsConfig.conversionActions picker.
//
// The answer is account-wide on a customer every foundation shares and is not isolated per
// project, which is why only the LF marketing team may call it; see Orchestrator.ReadConversionActions. The reserved LF scope stays unaddressable, as it is for
// every connection read.
func (s *ConnectionService) ListGoogleAdsConversionActions(ctx context.Context, p *conn.ListGoogleAdsConversionActionsPayload) (*conn.GoogleAdsConversionActions, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(googleAdsConversionActionLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadConversionActions(ctx, p.ProjectID, model.ProviderGoogleAds)
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, googleAdsConversionActionLookup, rerr)
	}
	out := make([]*conn.GoogleAdsConversionAction, 0, len(res.Actions))
	for _, a := range res.Actions {
		out = append(out, &conn.GoogleAdsConversionAction{
			ID:             a.ID,
			ResourceName:   a.ResourceName,
			Name:           a.Name,
			Status:         a.Status,
			Type:           a.Type,
			Category:       a.Category,
			PrimaryForGoal: a.PrimaryForGoal,
		})
	}
	return &conn.GoogleAdsConversionActions{
		AccountID:             res.AccountID,
		ConversionActions:     out,
		ConversionActionCount: len(out),
	}, nil
}

// ListGoogleAdsAudiences lists the custom audiences, combined audiences, Audience resources,
// user interests or custom interests on the Google Ads customer a create for this project uses, for the audience
// pickers (#496).
//
// Resolution and tenancy are ListGoogleAdsConversionActions': the answer is account-wide on a
// customer every foundation shares, which is why only the LF marketing team may call it (see
// Orchestrator.ReadAudiences). type and q are checked before any credential is resolved.
func (s *ConnectionService) ListGoogleAdsAudiences(ctx context.Context, p *conn.ListGoogleAdsAudiencesPayload) (*conn.GoogleAdsAudienceResources, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	// Mirrors the design Enum, Pattern and MaxLength for a caller that bypasses Goa.
	kind, ok := googleads.ParseAudienceKind(p.Type)
	if !ok {
		return nil, &conn.BadRequestError{Code: "400", Message: "type must be one of custom_audience, combined_audience, audience, user_interest, custom_interest"}
	}
	needle := ""
	if p.Q != nil {
		needle = strings.ToLower(strings.TrimSpace(*p.Q))
		if needle == "" || utf8.RuneCountInString(*p.Q) > googleAdsAudienceQueryMaxLen {
			return nil, &conn.BadRequestError{Code: "400", Message: "q must be 1-100 characters and not blank"}
		}
	}
	_, _, orch, err := s.resolveBackendWithOrch(googleAdsAudienceLookup.label())
	if err != nil {
		return nil, err
	}
	res, rerr := orch.ReadAudiences(ctx, p.ProjectID, model.ProviderGoogleAds, string(kind))
	if rerr != nil {
		return nil, s.classifyTargetingLookupError(ctx, p.ProjectID, googleAdsAudienceLookup, rerr)
	}
	out := make([]*conn.GoogleAdsAudienceResource, 0, min(len(res.Audiences), googleAdsAudienceMax))
	truncated := false
	for _, a := range res.Audiences {
		if needle != "" && !strings.Contains(strings.ToLower(a.Name), needle) {
			continue
		}
		if len(out) == googleAdsAudienceMax {
			truncated = true
			break
		}
		out = append(out, &conn.GoogleAdsAudienceResource{
			ResourceName: a.ResourceName,
			ID:           a.ID,
			Name:         a.Name,
			Type:         string(kind),
			Subtype:      optionalString(a.Subtype),
			Status:       optionalString(a.Status),
			Description:  optionalString(a.Description),
		})
	}
	return &conn.GoogleAdsAudienceResources{
		AccountID:     res.AccountID,
		Type:          string(kind),
		Audiences:     out,
		AudienceCount: len(out),
		Truncated:     truncated,
	}, nil
}
