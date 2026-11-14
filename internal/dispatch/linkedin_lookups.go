// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// LinkedInAdSegmentKind is the one AudienceReader kind the LinkedIn dispatcher serves: the ad
// segments (matched and retargeting audiences) linkedInConfig.targeting names (#552).
const LinkedInAdSegmentKind = "ad_segment"

// createLookupClient resolves the client a LinkedIn create for the project would use and its
// account: creds.resolve, the CREATE path's resolution (the project's own connection, the LF
// system fallback when it has none, and the forced-system redirect), so the ids a picker offers
// are on the account the create sends them to. Nothing is sent to LinkedIn.
func (d *LinkedInDispatcher) createLookupClient(ctx context.Context, projectID string, platform model.Provider) (*resolved, *linkedin.Client, string, error) {
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, nil, "", err
	}
	accountID := strings.TrimSpace(res.accountID)
	// A malformed bound account is stored connection configuration the operator must repair, and
	// it is refused here, before any request, so the read is not recorded as an upstream call.
	// ValidAccountID, not ValidateAccountID: the id must be a canonical positive int64 (no
	// leading zero, no overflow), as ReadSettings requires, or LinkedIn would refuse the URN and
	// the refusal would read as an upstream failure.
	if !linkedin.ValidAccountID(accountID) {
		return nil, nil, "", linkedinAccountIDUnusable(res, projectID, "linkedin picker lookup")
	}
	return res, d.cachedLinkedInClient(projectID, platform, res, creds, accountID), accountID, nil
}

// linkedInLookupError classifies a picker lookup failure the way the other LinkedIn reads do: a
// credential defect is the connection's, everything else is the platform's (503 upstream). A
// malformed bound account never gets here: createLookupClient refuses it first.
func linkedInLookupError(op string, res *resolved, err error) error {
	if linkedinConnectionDefect(err) {
		return res.systemScoped(linkedinExpiry(err))
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ListAudiences implements service.AudienceReader for LinkedIn: the ad segments on the account a
// create uses (#552). Only LinkedInAdSegmentKind is served.
func (d *LinkedInDispatcher) ListAudiences(ctx context.Context, projectID string, platform model.Provider, kind string) (_ *model.AudienceResources, called bool, _ error) {
	if kind != LinkedInAdSegmentKind {
		return nil, false, fmt.Errorf("%w: linkedin-ads audience kind %q", domain.ErrTargetingLookupUnsupported, kind)
	}
	res, client, accountID, err := d.createLookupClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	segments, err := client.ListAdSegments(ctx)
	if err != nil {
		return nil, true, linkedInLookupError("list linkedin ad segments", res, err)
	}
	out := make([]model.AudienceResource, 0, len(segments))
	for _, s := range segments {
		out = append(out, model.AudienceResource{
			ResourceName: s.URN(), ID: s.ID, Name: s.Name, Subtype: s.Type, Status: s.Status,
			Facet: s.Facet, MemberCount: s.ApproximateMemberCount,
		})
	}
	return &model.AudienceResources{AccountID: accountID, Kind: kind, Audiences: out}, true, nil
}

// ListConversionActions implements service.ConversionActionReader for LinkedIn: the conversion
// rules on the account a create uses (#552), enabled or not. Status is ENABLED or DISABLED.
func (d *LinkedInDispatcher) ListConversionActions(ctx context.Context, projectID string, platform model.Provider) (_ *model.ConversionActions, called bool, _ error) {
	res, client, accountID, err := d.createLookupClient(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	rules, err := client.ListConversionRules(ctx)
	if err != nil {
		return nil, true, linkedInLookupError("list linkedin conversion rules", res, err)
	}
	out := make([]model.ConversionAction, 0, len(rules))
	for _, r := range rules {
		status := "DISABLED"
		if r.Enabled {
			status = "ENABLED"
		}
		out = append(out, model.ConversionAction{ID: r.ID, ResourceName: r.URN(), Name: r.Name, Status: status, Type: r.Type})
	}
	return &model.ConversionActions{AccountID: accountID, Actions: out}, true, nil
}

// SearchTargetingEntities implements service.TargetingEntitySearcher for LinkedIn (#585): a
// typeahead over LinkedIn's targeting taxonomy (skills, titles, locations and the rest), for
// the linkedInConfig.targeting and geoTargets pickers.
//
// It resolves with resolveLinkedInDiscoveryCredentials: creds.resolve, the CREATE path's
// resolution (the project's own connection, the LF system fallback when it has none, the
// forced-system redirect), without requiring an ad account, which the finder does not take.
// Reading through the LF system credential is safe HERE for the reason Google's geo suggester
// gives: the answer is LinkedIn's own taxonomy, the same for every token, and names nothing any
// ad account holds.
//
// Answers are cached by facet and folded query (targetingEntityCache) AFTER resolution, so a
// project that can reach no credential still gets its refusal. The key is the whole query, so
// the cache serves a repeated identical search (another operator, a re-opened picker), not the
// next keystroke: every new prefix is a new key and a new adTargetingEntities call on the shared
// LF token. What bounds that spend is the 2-character floor (linkedin.MinTypeaheadQueryLen,
// enforced by the design and by NormalizeTypeaheadQuery), the caller's debounce, which the API
// asks for, and the client sending the request once, so a 429 is not retried into more spend
// but fails fast (503). called is false for a cache hit and for every local refusal.
func (d *LinkedInDispatcher) SearchTargetingEntities(ctx context.Context, projectID string, platform model.Provider, q model.TargetingEntityQuery) (_ []model.TargetingEntity, called bool, _ error) {
	query, err := linkedin.NormalizeTypeaheadQuery(q.Query)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", domain.ErrTargetingQueryInvalid, err)
	}
	if !slices.Contains(linkedin.TypeaheadFacetKeys(), q.Facet) {
		return nil, false, fmt.Errorf("%w: linkedin-ads targeting facet %q is not searchable", domain.ErrTargetingQueryInvalid, q.Facet)
	}
	res, creds, err := d.resolveLinkedInDiscoveryCredentials(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	key := q.Facet + "\x00" + strings.ToLower(query)
	if rows, ok := d.entities.get(key); ok {
		return rows, false, nil
	}
	// The cached client, keyed on the connection, so a refresh-capable credential mints one
	// token per connection rather than one per keystroke. Built with the connection's own
	// account (empty when none is selected), the same config every other cached caller builds.
	client := d.cachedLinkedInClient(projectID, platform, res, creds, strings.TrimSpace(res.accountID))
	rows, dropped, serr := client.SearchTargetingEntities(ctx, q.Facet, query)
	if serr != nil {
		return nil, true, linkedInLookupError("search linkedin targeting entities", res, serr)
	}
	if dropped > 0 {
		// Counts only: the query is the operator's free text and is never logged.
		slog.WarnContext(ctx, "linkedin targeting typeahead dropped entries whose urn does not have the facet's shape",
			"facet", q.Facet, "dropped", dropped, "kept", len(rows))
	}
	out := make([]model.TargetingEntity, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.TargetingEntity{URN: r.URN, Name: r.Name})
	}
	d.entities.put(key, out)
	return out, true, nil
}

// targetingEntityTTL and targetingEntityMaxEntries bound the typeahead cache. LinkedIn's
// taxonomy changes rarely, so hours of staleness are harmless; the cap bounds memory however
// many distinct searches arrive.
const (
	targetingEntityTTL        = 6 * time.Hour
	targetingEntityMaxEntries = 2048
)

type targetingEntityEntry struct {
	rows    []model.TargetingEntity
	expires time.Time
}

// targetingEntityCache is a bounded TTL cache of successful typeahead answers, keyed only by
// facet and query, not the project or credential, because the answer is LinkedIn's taxonomy and
// the same for every caller. A nil cache caches nothing.
type targetingEntityCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]targetingEntityEntry
}

func newTargetingEntityCache() *targetingEntityCache {
	return &targetingEntityCache{now: time.Now, entries: map[string]targetingEntityEntry{}}
}

func (c *targetingEntityCache) get(key string) ([]model.TargetingEntity, bool) {
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
	return copyEntityRows(e.rows), true
}

func (c *targetingEntityCache) put(key string, rows []model.TargetingEntity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= targetingEntityMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop arbitrary entries, as geoSuggestCache does; a miss costs one read.
		for k := range c.entries {
			if len(c.entries) < targetingEntityMaxEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = targetingEntityEntry{rows: copyEntityRows(rows), expires: now.Add(targetingEntityTTL)}
}

// copyEntityRows copies rows into a NON-NIL slice, empty included: nil is the contract
// violation the orchestrator refuses.
func copyEntityRows(rows []model.TargetingEntity) []model.TargetingEntity {
	out := make([]model.TargetingEntity, len(rows))
	copy(out, rows)
	return out
}
