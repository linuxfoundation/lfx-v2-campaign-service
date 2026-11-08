// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Community validation and interest resolution before create (#549, gaps 4 and 8).
//
// Reddit Ads API v3 (public OpenAPI document, https://ads-api.reddit.com/api/v3/openapi.json,
// read 2026-11-07):
//   - GET /targeting/communities (list_communities) takes `names`, "Filter by exact names.
//     Separate each community with a comma.", plus page.size (max 1000) and page.token, and
//     answers data[] of {id, name, subscriber_count, categories, ...} with pagination.next_url.
//   - GET /targeting/interests (list_interests) answers data[] of {id, name, category}; the
//     ad group's targeting.interests takes "the interests" from that list, i.e. their ids.
//
// Before this, subreddits went to the ad group as typed and interests as the brief's human
// labels ("Machine Learning"), and Reddit refused them with a 400 that the create answered by
// retrying without communities and interests. Resolving them first refuses a bad value before
// anything is created, and sends interest ids rather than labels.
//
// VERIFICATION LEVEL: neither read has been exercised against a live account (the issue asks for
// a probe of the interests shape), which is why the dispatcher keeps this behind
// REDDIT_TARGETING_RESOLUTION_ENABLED.
// ---------------------------------------------------------------------------

const (
	// communityNamesPerLookup bounds one names= filter, keeping the URL short.
	communityNamesPerLookup = 50
	// targetingLookupMaxPages caps each lookup walk; a list still paging at the cap cannot be
	// read to its end, which is an error, not "not found".
	targetingLookupMaxPages = 10

	// MaxResolvedSubreddits and MaxResolvedInterests bound the lists the lookups resolve, so one
	// create cannot fan out into dozens of sequential Reddit calls inside the synchronous
	// pre-check. 100 matches MaxExcludedSubreddits; 200 is targeting.interests' maxItems in
	// Reddit's schema.
	MaxResolvedSubreddits = 100
	MaxResolvedInterests  = 200
)

// errTargetingLookupPageCap marks a lookup walk that still had a next page at the cap.
var errTargetingLookupPageCap = errors.New("reddit targeting lookup still had more pages")

type communityElement struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type interestElement struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SubredditEntry is one normalized subreddit and its 1-based position in the caller's list, so
// a refusal names the entry the caller sent, not its position after blanks and duplicates were
// dropped.
type SubredditEntry struct {
	Name  string
	Entry int
}

// NormalizeSubredditEntries applies CreateCampaign's subreddit rules: trim, strip an optional
// r/, drop blanks and case-insensitive duplicates, keep the first spelling. Each kept name
// carries its original position.
func NormalizeSubredditEntries(in []string) []SubredditEntry {
	out := make([]SubredditEntry, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, s := range in {
		name := strings.TrimSpace(stripSubredditPrefix(strings.TrimSpace(s)))
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, SubredditEntry{Name: name, Entry: i + 1})
	}
	return out
}

// NormalizeSubreddits is NormalizeSubredditEntries' names, in order: exactly the names a create
// sends.
func NormalizeSubreddits(in []string) []string {
	entries := NormalizeSubredditEntries(in)
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// lookupPage decodes one targeting lookup page into out, refusing a body identityjson rejects
// (a duplicated key resolved last-wins could hide or invent an entry) and a missing data list.
func lookupPage(resp *apiResponse, out any, what string, page int) error {
	if resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return fmt.Errorf("reddit %s lookup page %d: the response cannot be trusted (duplicated key or malformed text)", what, page)
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" || json.Unmarshal(resp.Data, out) != nil {
		return fmt.Errorf("reddit %s lookup page %d: the response carries no list", what, page)
	}
	return nil
}

// ResolveCommunities confirms every entry (from NormalizeSubredditEntries) is a targetable
// community, and returns the names in Reddit's own spelling, in order. A name Reddit does not
// list is a domain.PreflightRefusal naming the entry's position in the caller's list; a lookup
// that fails or cannot be believed is a plain error. Either way the caller creates nothing.
//
// The names go in the filter as the caller spelled them and are matched ignoring case. Whether
// Reddit's "exact names" filter itself ignores case is one of the things the live probe must
// confirm before REDDIT_TARGETING_RESOLUTION_ENABLED is turned on.
func (c *Client) ResolveCommunities(ctx context.Context, entries []SubredditEntry) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxResolvedSubreddits {
		return nil, domain.PreflightRefusalf("invalid redditConfig.subreddits: at most %d distinct communities are accepted, got %d", MaxResolvedSubreddits, len(entries))
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	canonical := map[string]string{}
	for start := 0; start < len(names); start += communityNamesPerLookup {
		end := min(start+communityNamesPerLookup, len(names))
		q := url.Values{}
		q.Set("names", strings.Join(names[start:end], ","))
		q.Set("page.size", strconv.Itoa(1000))
		err := c.walkPagesCapped(ctx, http.MethodGet, "/targeting/communities?"+q.Encode(), nil, targetingLookupMaxPages, errTargetingLookupPageCap,
			func(page int, resp *apiResponse) error {
				var els []communityElement
				if err := lookupPage(resp, &els, "community", page); err != nil {
					return err
				}
				for _, e := range els {
					name := strings.TrimSpace(e.Name)
					if name == "" {
						return fmt.Errorf("reddit community lookup page %d: a community has no name", page)
					}
					canonical[strings.ToLower(name)] = name
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("reddit community lookup failed: %w", err)
		}
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		got, ok := canonical[strings.ToLower(e.Name)]
		if !ok {
			return nil, domain.PreflightRefusalf("invalid redditConfig.subreddits: entry %d (%s) is not a targetable Reddit community", e.Entry, domain.CallerValue(e.Name))
		}
		out[i] = got
	}
	return out, nil
}

// ResolveInterests maps each entry to an interest id from GET /targeting/interests: an entry
// equal to an id (ignoring case) is that id; otherwise it must equal exactly one interest name
// (trimmed, ignoring case). Duplicates after resolution are dropped, first kept. A blank entry, an
// entry matching nothing, or a name two interests share is a domain.PreflightRefusal naming its
// 1-based position; a lookup that fails or cannot be believed is a plain error.
func (c *Client) ResolveInterests(ctx context.Context, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxResolvedInterests {
		return nil, domain.PreflightRefusalf("invalid redditConfig.interests: at most %d entries are accepted, got %d", MaxResolvedInterests, len(entries))
	}
	byID := map[string]string{}
	byName := map[string][]string{}
	err := c.walkPagesCapped(ctx, http.MethodGet, "/targeting/interests", nil, targetingLookupMaxPages, errTargetingLookupPageCap,
		func(page int, resp *apiResponse) error {
			var els []interestElement
			if err := lookupPage(resp, &els, "interest", page); err != nil {
				return err
			}
			for _, e := range els {
				id := strings.TrimSpace(e.ID)
				if id == "" {
					return fmt.Errorf("reddit interest lookup page %d: an interest has no id", page)
				}
				// The catalogue is validated like the community answer: a repeated id or a row
				// with no name makes the lookup unreadable (a plain error, so the pre-check fails
				// open), rather than turning Reddit's malformed answer into a refusal that blames
				// the caller ("names 2 interests", "not an interest").
				key := strings.ToLower(id)
				if _, dup := byID[key]; dup {
					return fmt.Errorf("reddit interest lookup page %d: the same interest id was listed twice", page)
				}
				name := strings.ToLower(strings.TrimSpace(e.Name))
				if name == "" {
					return fmt.Errorf("reddit interest lookup page %d: an interest has no name", page)
				}
				byID[key] = id
				byName[name] = append(byName[name], id)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("reddit interest lookup failed: %w", err)
	}
	out := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for i, raw := range entries {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			return nil, domain.PreflightRefusalf("invalid redditConfig.interests: entry %d is blank", i+1)
		}
		id, ok := byID[key]
		if !ok {
			ids := byName[key]
			switch len(ids) {
			case 0:
				return nil, domain.PreflightRefusalf("invalid redditConfig.interests: entry %d (%s) is not a Reddit interest id or name", i+1, domain.CallerValue(raw))
			case 1:
				id = ids[0]
			default:
				return nil, domain.PreflightRefusalf("invalid redditConfig.interests: entry %d (%s) names %d Reddit interests; send the interest id instead", i+1, domain.CallerValue(raw), len(ids))
			}
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
