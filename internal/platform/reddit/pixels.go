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

// Pixel list (#549 gap 10): the pixels on the ad account, for a conversionPixelId picker.
//
// VERIFICATION LEVEL — read before changing anything here. The operation is
// `GET /ad_accounts/{ad_account_id}/pixels` (operationId list_pixels_by_ad_account) from Reddit's
// public v3 OpenAPI document (https://ads-api.reddit.com/api/v3/openapi.json, fetched 2026-11-14).
// Its 200 body is ListPixelsByAdAccountResponse: a required `data` array of
// components_schema_pixel ({id, name, business_id, created_at, modified_at, ...}; id is a string
// with the example "t2_123456", name 1-100 characters) and a required `pagination` whose
// `next_url` is followed directly, as for every other Reddit list here. NONE of it has been
// exercised against a live account, which is why the read sits behind REDDIT_PIXEL_LIST_ENABLED.
// Only id, name and business_id are decoded; every other field is ignored.

const (
	// pixelMaxPages caps the list walk. Reaching it is not an error: the read reports truncated.
	pixelMaxPages = 10
	// maxPixels bounds the items held, independently of Reddit's page size.
	maxPixels = 5000
	// maxPixelIDLen bounds one id, as maxCustomAudienceIDLen bounds an audience id.
	maxPixelIDLen = 64
)

// errPixelPageCap marks a walk that still had a next page at pixelMaxPages.
var errPixelPageCap = errors.New("reddit pixel list still had more pages")

// ErrPixelsMalformed marks a 2xx pixel list this client cannot believe: a missing, null or
// non-array data field, an unusable or duplicate id, or an item bound overrun. Upstream text
// never appears in it.
var ErrPixelsMalformed = errors.New("reddit: pixel list could not be confirmed")

// Pixel is one conversion pixel on the client's ad account.
type Pixel struct {
	// ID is the pixel id, in the form redditConfig.conversionPixelId and the connection's
	// conversion_pixel_id take.
	ID string
	// Name is the pixel's display name; may be empty. Not an identifier.
	Name string
	// BusinessID is the Reddit business that owns the pixel; empty when Reddit sent none.
	BusinessID string
}

type pixelElement struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BusinessID string `json:"business_id"`
}

// ValidPixelID reports whether id can be offered as a pixel id: non-empty, at most 64
// characters, letters, digits and underscores only, which also keeps it from altering a path
// anywhere it is later used.
func ValidPixelID(id string) bool {
	return id != "" && len(id) <= maxPixelIDLen && accountIDRe.MatchString(id)
}

// ListPixels lists the pixels on the client's ad account, following pagination.next_url (same
// origin and resource only) for at most 10 pages. A walk that still has a next page at the cap
// returns what it read with truncated true. Any other failure — a non-2xx, a transport error, an
// unreadable or malformed body, an unusable or duplicate id, more than maxPixels items — returns
// nil and an error, never a partial list. An account with no pixel is an empty non-nil slice.
func (c *Client) ListPixels(ctx context.Context) (pixels []Pixel, truncated bool, err error) {
	accountID := c.account.AccountID
	if accountID == "" || !accountIDRe.MatchString(accountID) {
		return nil, false, fmt.Errorf("list reddit pixels: the connection's ad account id is not usable")
	}
	out := make([]Pixel, 0)
	seen := map[string]struct{}{}
	werr := c.walkPagesCapped(ctx, http.MethodGet, "/ad_accounts/"+accountID+"/pixels", nil, pixelMaxPages, errPixelPageCap,
		func(page int, resp *apiResponse) error {
			var els []pixelElement
			if len(resp.Data) > 0 {
				if json.Unmarshal(resp.Data, &els) != nil {
					return fmt.Errorf("%w: page %d: data is not a list of pixels", ErrPixelsMalformed, page)
				}
			}
			if els == nil {
				return fmt.Errorf("%w: page %d: no data list", ErrPixelsMalformed, page)
			}
			for _, e := range els {
				// Validated RAW: an id is offered to a picker to be sent back verbatim.
				if !ValidPixelID(e.ID) {
					return fmt.Errorf("%w: page %d: a pixel has an unusable id", ErrPixelsMalformed, page)
				}
				if _, dup := seen[e.ID]; dup {
					return fmt.Errorf("%w: page %d: the same pixel was listed twice", ErrPixelsMalformed, page)
				}
				if len(out) >= maxPixels {
					return fmt.Errorf("%w: more than %d pixels", ErrPixelsMalformed, maxPixels)
				}
				seen[e.ID] = struct{}{}
				out = append(out, Pixel{ID: e.ID, Name: strings.TrimSpace(e.Name), BusinessID: strings.TrimSpace(e.BusinessID)})
			}
			return nil
		})
	if errors.Is(werr, errPixelPageCap) {
		return out, true, nil
	}
	if werr != nil {
		return nil, false, fmt.Errorf("list reddit pixels: %w", werr)
	}
	return out, false, nil
}
