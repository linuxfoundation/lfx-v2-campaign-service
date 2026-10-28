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
// accountID names the customer to read; empty means the customer the project's connection is
// bound to. Like the account monitor (ListAccountCampaignMetrics), it resolves the project's
// OWN connection only (resolveOwnedGoogleAdsDiscovery) and never the LF system fallback. A user
// list is account data — its name, type and size — and Google Ads is ONE customer shared by
// every foundation (docs/architecture.md, "Account Tenancy"), so through the fallback any
// project with no connection of its own would enumerate every foundation's audiences. That is
// the reason the BFF made shared-account reads root-only (lfx-self-serve-ops#54), and the
// reason this service refuses the fallback for every caller rather than trusting a gate one
// layer up. A project with no connection of its own gets domain.ErrNotFound (404).
//
// Lists cannot be project-scoped the way the monitor's campaigns are: this service records no
// user list, so there is no project-owned set to filter to. That is why
// Orchestrator.ReadUserLists refuses this read on a shared-tenancy platform before it reaches
// here — Google Ads today — and only a per-foundation customer is ever listed.
func (d *GoogleAdsDispatcher) ListUserLists(ctx context.Context, projectID string, platform model.Provider, accountID string) (*model.AudienceUserLists, error) {
	accountID = strings.TrimSpace(accountID)
	// Validated before any credential is resolved, as the monitor does: a malformed id is a
	// permanent request fault whatever the connection's state.
	if accountID != "" {
		if err := googleads.ValidateCustomerID(accountID); err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
		}
	}
	client, res, err := d.resolveOwnedGoogleAdsDiscovery(ctx, projectID, platform, googleads.WithCallerScopedTokenRefresh())
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		stored := strings.TrimSpace(res.accountID)
		if stored == "" {
			// The connection was created credentials-first and no account was chosen yet.
			// Both sentinels, as Microsoft's dispatch does: ErrConnectionNotUsable decides the
			// status (400), ErrAccountNotSelected names the missing choice.
			return nil, fmt.Errorf("%w: %w: project %s names no google ads account and none was requested",
				domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected, projectID)
		}
		if verr := googleads.ValidateCustomerID(stored); verr != nil {
			return nil, fmt.Errorf("%w: %w: stored google ads account id: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, verr)
		}
		accountID = stored
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
