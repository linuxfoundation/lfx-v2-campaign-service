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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

var _ service.MetaTargetingSearcher = (*MetaDispatcher)(nil)

// SearchMetaTargeting implements service.MetaTargetingSearcher (#550): a search of Meta's
// detailed-targeting taxonomy (interests, behaviors, work positions) for the metaConfig.targeting
// pickers.
//
// It resolves with creds.resolve, the CREATE path's resolution (the project's own connection, the
// LF system fallback when it has none, the forced-system redirect), without requiring an ad
// account, which Targeting Search does not take. Reading through the LF system credential is safe
// HERE for the reason Google's geo suggester and LinkedIn's typeahead give: the answer is Meta's own
// taxonomy, the same for every token, and names nothing any ad account holds.
//
// Answers are cached AFTER resolution, so a project that can reach no credential still gets its
// refusal. Interests and work positions are cached by kind and folded query: the key is the whole
// query, so the cache serves a repeated identical search, not the next keystroke. Behaviors are a
// browse of the whole class (Meta ignores q), so the class is read once per TTL and every query is
// filtered from it locally. What bounds the spend otherwise is the 2-character floor, the caller's
// debounce, and the client sending each request once, so a throttle fails fast (503) instead of
// being retried. called is false for a cache hit and for every local refusal.
//
// The query is the operator's free text and is never logged.
func (d *MetaDispatcher) SearchMetaTargeting(ctx context.Context, projectID string, platform model.Provider, q model.MetaTargetingQuery) (_ []model.MetaTargetingOption, called bool, _ error) {
	query, err := meta.NormalizeTargetingQuery(q.Query)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", domain.ErrMetaTargetingSearchInvalid, err)
	}
	if !slices.Contains(meta.TargetingSearchKinds(), q.Kind) {
		return nil, false, fmt.Errorf("%w: meta targeting search type %q is not served", domain.ErrMetaTargetingSearchInvalid, q.Kind)
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, false, err
	}
	key := q.Kind + "\x00" + strings.ToLower(query)
	if q.Kind == meta.TargetingSearchBehavior {
		key = q.Kind
	}
	if rows, ok := d.searchCache().get(key); ok {
		return finishMetaTargeting(q.Kind, rows, query), false, nil
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	var (
		rows    []meta.TargetingOption
		dropped int
	)
	if q.Kind == meta.TargetingSearchBehavior {
		rows, dropped, err = client.SearchTargetingAll(ctx)
	} else {
		rows, dropped, err = client.SearchTargeting(ctx, q.Kind, query)
	}
	if err != nil {
		return nil, true, res.systemScoped(fmt.Errorf("search meta targeting: %w", err))
	}
	if dropped > 0 {
		// Counts only: the query is never logged.
		slog.WarnContext(ctx, "meta targeting search dropped rows without a numeric id or a name",
			"type", q.Kind, "dropped", dropped, "kept", len(rows))
	}
	d.searchCache().put(key, rows)
	return finishMetaTargeting(q.Kind, rows, query), true, nil
}

// finishMetaTargeting maps the client's options to the model, filtering a behavior browse by query.
// It always returns a NON-NIL slice.
func finishMetaTargeting(kind string, rows []meta.TargetingOption, query string) []model.MetaTargetingOption {
	if kind == meta.TargetingSearchBehavior {
		rows = meta.FilterTargetingOptions(rows, query)
	}
	out := make([]model.MetaTargetingOption, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.MetaTargetingOption{ID: r.ID, Name: r.Name, Path: slices.Clone(r.Path), Description: r.Description,
			AudienceLower: r.AudienceLower, AudienceUpper: r.AudienceUpper, Coverage: r.Coverage})
	}
	return out
}

// searchCache returns the dispatcher's Targeting Search cache, building it on first use so a
// dispatcher built by NewMetaDispatcher or directly gets one either way.
func (d *MetaDispatcher) searchCache() *metaTargetingCache {
	d.targetingSearchOnce.Do(func() {
		if d.targetingSearch == nil {
			d.targetingSearch = newMetaTargetingCache()
		}
	})
	return d.targetingSearch
}

// metaTargetingTTL and metaTargetingMaxEntries bound the search cache. Meta's taxonomy changes
// slowly, so an hour of staleness is harmless (a retired id is still refused by the create's own
// validation read); the cap bounds memory however many distinct searches arrive.
const (
	metaTargetingTTL        = time.Hour
	metaTargetingMaxEntries = 2048
)

type metaTargetingEntry struct {
	rows    []meta.TargetingOption
	expires time.Time
}

// metaTargetingCache is a bounded TTL cache of successful search answers, keyed by kind and query
// only, not the project or credential, because the answer is Meta's taxonomy and the same for every
// caller. A nil cache caches nothing.
type metaTargetingCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]metaTargetingEntry
}

func newMetaTargetingCache() *metaTargetingCache {
	return &metaTargetingCache{now: time.Now, entries: map[string]metaTargetingEntry{}}
}

func (c *metaTargetingCache) get(key string) ([]meta.TargetingOption, bool) {
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
	return slices.Clone(e.rows), true
}

func (c *metaTargetingCache) put(key string, rows []meta.TargetingOption) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= metaTargetingMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop arbitrary entries; a miss costs one read.
		for k := range c.entries {
			if len(c.entries) < metaTargetingMaxEntries {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = metaTargetingEntry{rows: slices.Clone(rows), expires: now.Add(metaTargetingTTL)}
}
