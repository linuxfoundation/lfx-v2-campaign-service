// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Custom audiences (#549, gap 1): retargeting and suppression on the ad group.
//
// VERIFICATION LEVEL — read before changing anything here. The create fields are
// `targeting.custom_audience_ids` and `targeting.excluded_custom_audience_ids` and the list is
// `GET /ad_accounts/{ad_account_id}/custom_audiences`, all from Reddit's public v3 OpenAPI
// document (https://ads-api.reddit.com/api/v3/openapi.json, components_schema_targeting and
// GetCustomAudienceResponse / components_schema_custom_audience_base, fetched 2026-11-06). NONE
// of it has been exercised against a live Reddit account, which is why both sit behind the
// REDDIT_CUSTOM_AUDIENCES_ENABLED deployment flag. The document gives an audience id only as a
// string, with the example "67547345732543"; the id rule below (the account-id charset) is wider
// than that example on purpose, so a non-numeric id Reddit really issues is not refused.

const (
	// MaxCustomAudienceIDs caps each of the two create lists. Reddit's schema publishes no
	// maxItems for either; 100 is this service's bound so a request cannot carry an unbounded list.
	MaxCustomAudienceIDs = 100
	// maxCustomAudienceIDLen bounds one id, as maxDiscoveredAccountIDLen bounds an account id.
	maxCustomAudienceIDLen = 64
	// customAudienceMaxPages caps the list walk. Unlike account discovery, reaching it is not an
	// error: the read reports Truncated, so a picker can say the list is incomplete.
	customAudienceMaxPages = 10
	// maxCustomAudiences bounds the items held, independently of Reddit's page size.
	maxCustomAudiences = 5000
)

// errCustomAudiencePageCap marks a walk that still had a next page at customAudienceMaxPages.
var errCustomAudiencePageCap = errors.New("reddit custom audience list still had more pages")

// ErrCustomAudiencesMalformed marks a 2xx custom-audience list this client cannot believe: a
// missing, null or non-array data field, an unusable or duplicate id, or an item bound overrun.
// Upstream text never appears in it.
var ErrCustomAudiencesMalformed = errors.New("reddit: custom audience list could not be confirmed")

// CustomAudience is one custom audience on the client's ad account.
type CustomAudience struct {
	// ID is the audience id, in the form customAudienceIds / excludedCustomAudienceIds accept.
	ID string
	// Name is the display name; may be empty. Not an identifier.
	Name string
	// Type is Reddit's type verbatim (CUSTOMER_LIST, LOOKALIKE, ENGAGEMENT_RETARGETING,
	// PIXEL_RETARGETING, THIRD_PARTY, IN_MARKET); may be empty.
	Type string
	// Status is Reddit's processing status verbatim (VALID, PENDING, …); may be empty.
	Status string
	// SizeLower and SizeUpper are Reddit's estimated matched-user bounds; nil when Reddit sent
	// none, which is not a size of 0.
	SizeLower *int64
	SizeUpper *int64
}

type customAudienceElement struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	SizeRangeLower *int64 `json:"size_range_lower"`
	SizeRangeUpper *int64 `json:"size_range_upper"`
}

// ValidCustomAudienceID reports whether id can be sent as a custom audience id: non-empty, at
// most 64 characters, letters, digits and underscores only. It is also what keeps an id from
// altering a path or a filter anywhere it is later used.
func ValidCustomAudienceID(id string) bool {
	return id != "" && len(id) <= maxCustomAudienceIDLen && accountIDRe.MatchString(id)
}

// ValidateCustomAudienceIDs checks the two create lists together. Ids are compared exactly, not
// trimmed: trimming would send an id the caller did not write. It returns a caller-safe sentence
// naming the list and the 1-based position at fault, or "" when both lists are acceptable.
func ValidateCustomAudienceIDs(include, exclude []string) string {
	check := func(field string, ids []string) (map[string]struct{}, string) {
		if len(ids) > MaxCustomAudienceIDs {
			return nil, fmt.Sprintf("%s holds %d ids; at most %d are accepted", field, len(ids), MaxCustomAudienceIDs)
		}
		seen := make(map[string]struct{}, len(ids))
		for i, id := range ids {
			if !ValidCustomAudienceID(id) {
				return nil, fmt.Sprintf("%s entry %d is not a Reddit custom audience id (1-%d letters, digits or underscores)", field, i+1, maxCustomAudienceIDLen)
			}
			if _, dup := seen[id]; dup {
				return nil, fmt.Sprintf("%s entry %d repeats an earlier id", field, i+1)
			}
			seen[id] = struct{}{}
		}
		return seen, ""
	}
	in, msg := check("customAudienceIds", include)
	if msg != "" {
		return msg
	}
	_, msg = check("excludedCustomAudienceIds", exclude)
	if msg != "" {
		return msg
	}
	for i, id := range exclude {
		if _, both := in[id]; both {
			return fmt.Sprintf("excludedCustomAudienceIds entry %d is also in customAudienceIds; an audience cannot be both targeted and excluded", i+1)
		}
	}
	return ""
}

// ListCustomAudiences lists the custom audiences on the client's ad account, following
// pagination.next_url (same origin and resource only, nextPagePath) for at most 10 pages. A walk
// that still has a next page at the cap returns what it read with truncated true. Reads retry a
// throttle with backoff (request), as every Reddit read here does.
//
// Any other failure — a non-2xx, a transport error, an unreadable or malformed body, an unusable
// or duplicate id, more than maxCustomAudiences items — returns nil and an error, never a partial
// list. An account with no audiences is an empty non-nil slice.
func (c *Client) ListCustomAudiences(ctx context.Context) (audiences []CustomAudience, truncated bool, err error) {
	accountID := c.account.AccountID
	if accountID == "" || !accountIDRe.MatchString(accountID) {
		return nil, false, fmt.Errorf("list reddit custom audiences: the connection's ad account id is not usable")
	}
	out := make([]CustomAudience, 0)
	seen := map[string]struct{}{}
	werr := c.walkPagesCapped(ctx, http.MethodGet, "/ad_accounts/"+accountID+"/custom_audiences", nil, customAudienceMaxPages, errCustomAudiencePageCap,
		func(page int, resp *apiResponse) error {
			var els []customAudienceElement
			if len(resp.Data) > 0 {
				if json.Unmarshal(resp.Data, &els) != nil {
					return fmt.Errorf("%w: page %d: data is not a list", ErrCustomAudiencesMalformed, page)
				}
			}
			if els == nil {
				return fmt.Errorf("%w: page %d: no data list", ErrCustomAudiencesMalformed, page)
			}
			for _, e := range els {
				// Validated RAW: an id is offered to a picker to be sent back verbatim.
				if !ValidCustomAudienceID(e.ID) {
					return fmt.Errorf("%w: page %d: an audience has an unusable id", ErrCustomAudiencesMalformed, page)
				}
				if _, dup := seen[e.ID]; dup {
					return fmt.Errorf("%w: page %d: the same audience was listed twice", ErrCustomAudiencesMalformed, page)
				}
				if len(out) >= maxCustomAudiences {
					return fmt.Errorf("%w: more than %d audiences", ErrCustomAudiencesMalformed, maxCustomAudiences)
				}
				seen[e.ID] = struct{}{}
				out = append(out, CustomAudience{
					ID:        e.ID,
					Name:      strings.TrimSpace(e.Name),
					Type:      strings.TrimSpace(e.Type),
					Status:    strings.TrimSpace(e.Status),
					SizeLower: e.SizeRangeLower,
					SizeUpper: e.SizeRangeUpper,
				})
			}
			return nil
		})
	if errors.Is(werr, errCustomAudiencePageCap) {
		return out, true, nil
	}
	if werr != nil {
		return nil, false, fmt.Errorf("list reddit custom audiences: %w", werr)
	}
	return out, false, nil
}
