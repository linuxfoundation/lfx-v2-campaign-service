// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	// customAudienceListFields is the custom-audience listing's field set (Ad Account
	// `customaudiences` edge, Custom Audience fields). account_id is not displayed: it is the
	// per-row proof that the edge answered about THIS ad account, which the caller checks.
	customAudienceListFields = "id,name,subtype,description,approximate_count_lower_bound,approximate_count_upper_bound,account_id"
	// customAudienceListPageSize / customAudienceListMaxPages bound the walk at 2000 audiences;
	// past that the read fails rather than answering a partial list (#550).
	customAudienceListPageSize = 100
	customAudienceListMaxPages = 20
	// maxCustomAudienceTextBytes bounds a listed name or description.
	maxCustomAudienceTextBytes = 4096
)

// CustomAudience is one custom audience of an ad account, as the picker shows it (#550). ID is
// what metaConfig.targeting.customAudiences / excludedCustomAudiences take. Subtype is Meta's own
// kind (CUSTOM, WEBSITE, LOOKALIKE, ENGAGEMENT, …), verbatim. The approximate counts are Meta's
// own range, nil when Meta did not report one; Description is "" when Meta sent none.
type CustomAudience struct {
	ID          string
	Name        string
	Subtype     string
	Description string
	CountLower  *int64
	CountUpper  *int64
}

// ErrCustomAudienceListTooLarge reports an ad account with more custom audiences than the read
// publishes (customAudienceListPageSize * customAudienceListMaxPages). It does not clear on retry.
var ErrCustomAudienceListTooLarge = errors.New("meta custom audience listing exceeds the readable bound")

type customAudienceWire struct {
	ID          *string `json:"id"`
	Name        *string `json:"name"`
	Subtype     *string `json:"subtype"`
	Description *string `json:"description"`
	CountLower  *int64  `json:"approximate_count_lower_bound"`
	CountUpper  *int64  `json:"approximate_count_upper_bound"`
	AccountID   *string `json:"account_id"`
}

// ListCustomAudiences lists every custom audience of accountID (act_<digits>), all or nothing:
// `GET /act_<id>/customaudiences?fields=…&limit=100`, following the opaque `after` cursor (never
// Meta's token-bearing paging.next URL) for at most customAudienceListMaxPages pages. Every page
// passes identityjson on its raw bytes; every row must carry a canonical numeric id, an audience
// subtype token and the requested account, and appear once. A missing data field, a next page
// without a cursor, a repeated cursor, or more pages than the bound fails the read rather than
// truncating it. Errors never echo an upstream value.
func (c *Client) ListCustomAudiences(ctx context.Context, accountID string) ([]CustomAudience, error) {
	if err := ValidateAccountID(accountID); err != nil {
		return nil, fmt.Errorf("list meta custom audiences: %w", err)
	}
	out := []CustomAudience{}
	ids := map[string]struct{}{}
	cursors := map[string]struct{}{}
	after := ""
	for page := 0; page < customAudienceListMaxPages; page++ {
		path := "/" + accountID + "/customaudiences?fields=" + customAudienceListFields + "&limit=" + strconv.Itoa(customAudienceListPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		raw, err := c.getChecked(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("list meta custom audiences: %w", err)
		}
		p, err := decodePage(raw, path)
		if err != nil {
			return nil, fmt.Errorf("list meta custom audiences: %w", err)
		}
		for i, rowRaw := range *p.Data {
			a, rerr := decodeCustomAudience(rowRaw, accountID)
			if rerr != nil {
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: %w", page, i, rerr)}
			}
			if _, dup := ids[a.ID]; dup {
				return nil, &transportError{Method: http.MethodGet, Path: path, Err: fmt.Errorf("page %d row %d: the same audience is listed twice", page, i)}
			}
			// The bound is on ROWS, not only pages: a page larger than the requested limit must not
			// carry the listing past what the read publishes.
			if len(out) == customAudienceListPageSize*customAudienceListMaxPages {
				return nil, fmt.Errorf("list meta custom audiences: more than %d audiences: %w", customAudienceListPageSize*customAudienceListMaxPages, ErrCustomAudienceListTooLarge)
			}
			ids[a.ID] = struct{}{}
			out = append(out, a)
		}
		next, done, cerr := nextCursor(p, cursors)
		if cerr != nil {
			return nil, fmt.Errorf("list meta custom audiences: %w", cerr)
		}
		if done {
			return out, nil
		}
		after = next
	}
	return nil, fmt.Errorf("list meta custom audiences: more than %d pages of %d: %w", customAudienceListMaxPages, customAudienceListPageSize, ErrCustomAudienceListTooLarge)
}

// sizeBound normalizes one approximate-count bound: Meta reports -1 when the size is unavailable
// (an inactive lookalike, say), which is "no estimate", not a malformed audience, so it reads as
// absent. Any other negative is left for decodeCustomAudience to refuse.
func sizeBound(n *int64) *int64 {
	if n != nil && *n == -1 {
		return nil
	}
	return n
}

// decodeCustomAudience validates one listed audience. Errors never echo an upstream value.
func decodeCustomAudience(raw json.RawMessage, accountID string) (CustomAudience, error) {
	var w customAudienceWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return CustomAudience{}, errors.New("row is not a custom audience object of the expected shape")
	}
	if w.ID == nil || !numericIDRE.MatchString(*w.ID) {
		return CustomAudience{}, errors.New("row has no canonical audience id")
	}
	if w.Subtype == nil || !statusTokenRE.MatchString(*w.Subtype) {
		return CustomAudience{}, errors.New("row has no audience subtype token")
	}
	if w.AccountID == nil || canonicalAccountID(*w.AccountID) == "" || canonicalAccountID(*w.AccountID) != accountID {
		return CustomAudience{}, errors.New("row is not reported under the requested ad account")
	}
	a := CustomAudience{ID: *w.ID, Subtype: *w.Subtype, CountLower: sizeBound(w.CountLower), CountUpper: sizeBound(w.CountUpper)}
	if w.Name != nil {
		a.Name = *w.Name
	}
	if w.Description != nil {
		a.Description = *w.Description
	}
	if len(a.Name) > maxCustomAudienceTextBytes || len(a.Description) > maxCustomAudienceTextBytes {
		return CustomAudience{}, fmt.Errorf("name or description exceeds %d bytes", maxCustomAudienceTextBytes)
	}
	for _, n := range []*int64{a.CountLower, a.CountUpper} {
		if n != nil && *n < 0 {
			return CustomAudience{}, errors.New("approximate count is negative")
		}
	}
	return a, nil
}
