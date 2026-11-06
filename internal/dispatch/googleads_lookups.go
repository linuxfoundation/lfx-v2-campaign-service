// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// SuggestGeoTargets implements service.GeoTargetSuggester for Google Ads: it searches Google's
// geo target constants by name or postal code.
//
// It resolves the way ListAccounts does (resolveGoogleAdsDiscoveryClient), so a project with no
// connection of its own reads through the LF system credential. That is safe HERE and nowhere
// it is refused: the answer is Google's public location table, the same for every credential,
// and it reveals nothing any ad account holds. The shared-account rule (monitor, probe,
// adoption, user lists) exists because those reads return one foundation's data to another;
// this one returns no foundation's data at all.
//
// called reports whether Google was actually contacted. It is false for a cache hit AND for a
// local refusal before any request (no connection, an unusable one), so the caller records an
// upstream call only for a request that really went out — success or failure.
func (d *GoogleAdsDispatcher) SuggestGeoTargets(ctx context.Context, projectID string, platform model.Provider, q model.GeoTargetQuery) (_ []model.GeoTargetSuggestion, called bool, _ error) {
	// One-shot client: its token refresh must end with this request, not outlive it.
	client, err := d.resolveGoogleAdsDiscoveryClient(ctx, projectID, platform, googleads.WithCallerScopedTokenRefresh())
	if err != nil {
		return nil, false, err
	}
	// The cache is consulted AFTER resolution, so a project that can reach no credential still
	// gets its 404 rather than another project's cached answer; resolution sends nothing to
	// Google (the token is fetched on first request), so only the upstream call is saved.
	key := geoSuggestKey(q)
	if rows, ok := d.geo.get(key); ok {
		return rows, false, nil
	}
	rows, serr := client.SuggestGeoTargetConstants(ctx, q.Query, q.CountryCode)
	if serr != nil {
		return nil, true, serr
	}
	// make, not var: an empty answer is a real answer and must stay distinguishable from none.
	out := make([]model.GeoTargetSuggestion, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.GeoTargetSuggestion{
			ID:            r.ID,
			Name:          r.Name,
			CanonicalName: r.CanonicalName,
			CountryCode:   r.CountryCode,
			TargetType:    r.TargetType,
			Status:        r.Status,
			Reach:         r.Reach,
		})
	}
	d.geo.put(key, out)
	return out, true, nil
}

// ListUserLists implements service.UserListReader for Google Ads: every user list on one
// customer, read with GAQL on user_list.
//
// accountID names the customer to read; empty means the customer a campaign create for the
// project uses. It resolves exactly as ListConversionActions does (createAccountClient: the
// project's own connection, the LF system fallback when it has none, and the forced-system
// redirect), because the resource names it returns are only useful if the create accepts them,
// and audienceSegments refuses a list owned by another customer.
//
// TENANCY: Google Ads is ONE customer shared by every foundation (docs/architecture.md,
// "Account Tenancy"), and this service records no user list per project, so the answer is every
// foundation's user lists. It used to refuse the LF fallback, and Orchestrator.ReadUserLists
// refused the shared customer outright, because the route admitted every campaign_manager,
// executive directors included. The gateway now admits only global_marketing_ops of the
// project (the LF marketing team), as for the conversion-action and audience reads, so both
// refusals are gone.
func (d *GoogleAdsDispatcher) ListUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (*model.AudienceUserLists, error) {
	accountID = strings.TrimSpace(accountID)
	// Validated before any credential is resolved, as the monitor does: a malformed id is a
	// permanent request fault whatever the connection's state.
	if accountID != "" {
		if err := googleads.ValidateCustomerID(accountID); err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
		}
	}
	// The create's client and account. A connection with no account selected, or a stored id
	// that is not digits only, is refused here as a connection to fix (400, or 500 on the LF
	// system row), whether or not accountID names another customer: the credential is the
	// create's, and a create on that connection would fail the same way.
	client, createAccount, err := d.createAccountClient(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		accountID = createAccount
	}
	rows, lerr := client.ListUserLists(ctx, accountID, googleads.MaxUserListRows+1)
	if lerr != nil {
		return nil, lerr
	}
	lists := make([]model.AudienceUserList, 0, len(rows))
	for _, r := range rows {
		lists = append(lists, model.AudienceUserList{
			ResourceName:       r.ResourceName,
			ID:                 r.ID,
			Name:               r.Name,
			Type:               r.Type,
			MembershipStatus:   r.MembershipStatus,
			AccountStatus:      r.AccountStatus,
			SizeForSearch:      r.SizeForSearch,
			SizeRangeForSearch: r.SizeRangeForSearch,
			EligibleForSearch:  r.EligibleForSearch,
		})
	}
	return &model.AudienceUserLists{AccountID: accountID, Lists: lists}, nil
}

// ListConversionActions implements service.ConversionActionReader for Google Ads: the ENABLED
// conversion actions on the customer a campaign create for this project uses, read with one
// GAQL search (googleads.ListConversionActions). Nothing is written.
//
// It resolves with creds.resolve, the CREATE path's resolution (the project's own connection,
// the LF system fallback when it has none, and the forced-system redirect), because the ids it
// returns are only useful if the create accepts them: a conversion action belongs to one
// customer, and validateConversionActions refuses a resource name owned by another.
//
// TENANCY: this deliberately does NOT follow ListUserLists. Google Ads is ONE customer shared
// by every foundation (docs/architecture.md, "Account Tenancy"), so this read returns every
// foundation's conversion action names. The gateway therefore admits only global_marketing_ops
// of the project (the LF marketing team) to this route, not campaign_manager, which also admits
// a foundation's executive_director (#486). Orchestrator.ReadConversionActions says the same.
//
// called reports whether Google was contacted: false for every local refusal before the search
// (no connection, an unusable one, a stored account id that is not digits only).
func (d *GoogleAdsDispatcher) ListConversionActions(ctx context.Context, projectID string, platform model.Provider) (_ *model.ConversionActions, called bool, _ error) {
	client, accountID, err := d.createAccountClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	rows, lerr := client.ListConversionActions(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no enabled action is a real, empty answer.
	actions := make([]model.ConversionAction, 0, len(rows))
	for _, r := range rows {
		actions = append(actions, model.ConversionAction{
			ID:             r.ID,
			ResourceName:   r.ResourceName,
			Name:           r.Name,
			Status:         r.Status,
			Type:           r.Type,
			Category:       r.Category,
			PrimaryForGoal: r.PrimaryForGoal,
		})
	}
	return &model.ConversionActions{AccountID: accountID, Actions: actions}, true, nil
}

// createAccountClient resolves the client a campaign create for the project would use, and its
// account: creds.resolve, the CREATE path's resolution (the project's own connection, the LF
// system fallback when it has none, and the forced-system redirect). Nothing is sent to Google.
func (d *GoogleAdsDispatcher) createAccountClient(ctx context.Context, projectID string, platform model.Provider) (*googleads.Client, string, error) {
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, "", err
	}
	// The cached client, as every other read on an account-bound client takes it; its
	// CustomerID is the account validateGoogleAdsConnection returned, which it requires.
	client, err := d.cachedGoogleAdsClient(projectID, platform, res)
	if err != nil {
		return nil, "", err
	}
	accountID := client.CustomerID()
	// A stored id that is not digits only is a connection to edit, not an upstream failure:
	// checked here, before gaqlSearch would refuse it with an error that maps to 503. On the
	// project's own row that is a 400; systemScoped makes it the LF system row's defect (500)
	// when the project fell back to that row.
	if verr := googleads.ValidateCustomerID(accountID); verr != nil {
		return nil, "", res.systemScoped(fmt.Errorf("%w: %w: stored google ads account id: %w",
			domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, verr))
	}
	return client, accountID, nil
}

// ListAudiences implements service.AudienceReader for Google Ads (#496): every usable custom
// audience, combined audience, Audience resource or user interest (one kind per call) on the
// customer a campaign create for this project uses, read with one GAQL search
// (googleads.ListAudiences). Nothing is written.
//
// It resolves exactly as ListConversionActions does (createAccountClient), for the same
// reason: the resource names it returns are only useful if the create accepts them, and every
// audience input refuses a resource name owned by another customer.
//
// TENANCY: like ListConversionActions, and unlike ListUserLists, it is not refused on the
// shared customer, so it returns every foundation's custom audiences, combined audiences and
// Audiences (user_interest is Google's own taxonomy and holds no foundation's data). The
// gateway therefore admits only global_marketing_ops of the project to this route.
//
// An unknown kind is refused before any credential is resolved. called reports whether Google
// was contacted, as ListConversionActions' does: false for a local refusal and for a cache hit.
//
// user_interest answers are cached per account (userInterestCache). That taxonomy is Google's
// own, about a thousand rows that change rarely, and a picker searches it with q as the operator
// types; the service filters the cached rows by q, so each keystroke would otherwise be a full
// googleAds:search on the shared developer token. The cache is consulted after resolution, as
// the geo cache is, so a project that can reach no credential still gets its refusal. The
// account kinds are not cached: an audience created a minute ago must be offered.
func (d *GoogleAdsDispatcher) ListAudiences(ctx context.Context, projectID string, platform model.Provider, kind string) (_ *model.AudienceResources, called bool, _ error) {
	k, ok := googleads.ParseAudienceKind(kind)
	if !ok {
		// The service refuses an unknown type with its own 400 first; this guards a caller
		// that bypasses it, with the sentinel the service already answers 400.
		return nil, false, fmt.Errorf("%w: unknown google ads audience type %q", domain.ErrTargetingLookupUnsupported, kind)
	}
	client, accountID, err := d.createAccountClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	if k == googleads.AudienceKindUserInterest {
		if rows, hit := d.interests.get(accountID); hit {
			return &model.AudienceResources{AccountID: accountID, Kind: string(k), Audiences: rows}, false, nil
		}
	}
	rows, lerr := client.ListAudiences(ctx, k)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no entry of this kind is a real, empty answer.
	out := make([]model.AudienceResource, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.AudienceResource{
			ResourceName: r.ResourceName,
			ID:           r.ID,
			Name:         r.Name,
			Subtype:      r.Subtype,
			Status:       r.Status,
			Description:  r.Description,
		})
	}
	if k == googleads.AudienceKindUserInterest {
		d.interests.put(accountID, out)
	}
	return &model.AudienceResources{AccountID: accountID, Kind: string(k), Audiences: out}, true, nil
}

// userInterestTTL and userInterestMaxEntries bound the user_interest cache. One entry per
// account; the LF deployment reads one or two, so the cap only bounds a pathological spread.
const (
	userInterestTTL        = 6 * time.Hour
	userInterestMaxEntries = 64
)

type userInterestEntry struct {
	rows    []model.AudienceResource
	expires time.Time
}

// userInterestCache is a bounded TTL cache of user_interest reads keyed by account id. A nil
// cache caches nothing.
type userInterestCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]userInterestEntry
}

func newUserInterestCache() *userInterestCache {
	return &userInterestCache{now: time.Now, entries: map[string]userInterestEntry{}}
}

func (c *userInterestCache) get(accountID string) ([]model.AudienceResource, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[accountID]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, accountID)
		return nil, false
	}
	// A non-nil copy, so a caller cannot corrupt the cached answer and an empty one stays [].
	return copyAudienceRows(e.rows), true
}

func (c *userInterestCache) put(accountID string, rows []model.AudienceResource) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.entries[accountID]; !exists && len(c.entries) >= userInterestMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		for k := range c.entries {
			if len(c.entries) < userInterestMaxEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[accountID] = userInterestEntry{rows: copyAudienceRows(rows), expires: now.Add(userInterestTTL)}
}

func copyAudienceRows(rows []model.AudienceResource) []model.AudienceResource {
	out := make([]model.AudienceResource, len(rows))
	copy(out, rows)
	return out
}

// geoSuggestTTL and geoSuggestMaxEntries bound the geo suggestion cache. Google's geo target
// table changes a few times a year, so hours of staleness are harmless; the entry cap bounds
// memory however many distinct searches arrive.
const (
	geoSuggestTTL        = 6 * time.Hour
	geoSuggestMaxEntries = 2048
)

type geoSuggestEntry struct {
	rows    []model.GeoTargetSuggestion
	expires time.Time
}

// geoSuggestCache is a bounded TTL cache of successful geo searches. Keyed only by the query,
// not the project or credential, because the answer is Google's public table and is the same
// for every caller. A nil cache caches nothing.
type geoSuggestCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]geoSuggestEntry
}

func newGeoSuggestCache() *geoSuggestCache {
	return &geoSuggestCache{now: time.Now, entries: map[string]geoSuggestEntry{}}
}

// geoSuggestKey folds case and surrounding space so "Austin" and " austin" share an entry.
func geoSuggestKey(q model.GeoTargetQuery) string {
	return strings.ToUpper(strings.TrimSpace(q.CountryCode)) + "\x00" + strings.ToLower(strings.TrimSpace(q.Query))
}

func (c *geoSuggestCache) get(key string) ([]model.GeoTargetSuggestion, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	// A copy, so a caller that edits its slice cannot corrupt the cached answer. Built with
	// make so an empty answer stays a non-nil empty slice: nil is the contract violation
	// ReadGeoTargetSuggestions refuses, and would turn a repeated no-match search into a 503.
	return copyGeoRows(e.rows), true
}

func (c *geoSuggestCache) put(key string, rows []model.GeoTargetSuggestion) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= geoSuggestMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop arbitrary entries until there is room. Map order is random, which is
		// an adequate eviction policy for a cache whose misses only cost one upstream read.
		for k := range c.entries {
			if len(c.entries) < geoSuggestMaxEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = geoSuggestEntry{rows: copyGeoRows(rows), expires: now.Add(geoSuggestTTL)}
}

// copyGeoRows copies rows into a NON-NIL slice, empty included.
func copyGeoRows(rows []model.GeoTargetSuggestion) []model.GeoTargetSuggestion {
	out := make([]model.GeoTargetSuggestion, len(rows))
	copy(out, rows)
	return out
}
