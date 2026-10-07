// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Account discovery (LFXV2-2665): which Reddit ad accounts does this credential reach?
//
// VERIFICATION LEVEL — read before changing anything here. The two operations are Reddit Ads
// API v3's "List My Businesses" (GET /me/businesses) and "List Ad Accounts by Business"
// (GET /businesses/{business_id}/ad_accounts), taken from Reddit's published v3 reference as
// the ticket names them. NOT exercised against a live Reddit account: this repository holds no
// Reddit credentials. Third-party reports exist of /me/businesses answering 404 for some
// tokens; if that is real it surfaces here as an apiError and the endpoint answers 503, never
// an empty list. The element field names (id, name, currency) and the pagination envelope's
// next_url are the same unverified conventions paginationEnvelope documents.
const (
	// discoveryBusinessMaxPages and discoveryAccountMaxPages cap each walk. A walk that still
	// has a next page at its cap FAILS — a truncated picker looks exactly like a complete one,
	// and the caller acts on the absence by concluding the credential cannot reach an account
	// it can.
	discoveryBusinessMaxPages = 10
	discoveryAccountMaxPages  = 20
	// maxDiscoveredBusinesses and maxDiscoveredAccounts bound the ITEMS the walk will hold,
	// independently of the page caps (a page size is Reddit's choice, not ours, and none is
	// sent). Reaching one more than the bound is an error, never a truncation.
	maxDiscoveredBusinesses = 100
	maxDiscoveredAccounts   = 2000
	// maxDiscoveredAccountIDLen mirrors MaxLength(64) on reddit-ads-connection-config.account_id
	// in design/connection.go: discovery must not offer an id the connection will refuse to
	// store. The charset half of that contract is accountIDRe.
	maxDiscoveredAccountIDLen = 64
	// maxBusinessIDLen bounds a business id before it is interpolated into a path.
	maxBusinessIDLen = 128
)

// businessIDRe guards a business id before it becomes a path segment. Wider than accountIDRe by
// the hyphen, because Reddit does not publish a business-id grammar and a UUID-shaped one is
// plausible; it still admits nothing that can alter a path (no '/', '?', '#', '%' or '.').
var businessIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var (
	errBusinessPageCap = fmt.Errorf("reddit business list still had more pages after %d; refusing to return a truncated account list", discoveryBusinessMaxPages)
	errAccountPageCap  = fmt.Errorf("reddit ad-account list still had more pages after %d; refusing to return a truncated account list", discoveryAccountMaxPages)
)

// ErrDiscoveryMalformed marks a 2xx discovery response this client cannot believe: a missing,
// null or non-array data field, an unusable id, a duplicate, or an item bound overrun. Upstream
// text never appears in it.
var ErrDiscoveryMalformed = errors.New("reddit: ad-account discovery response could not be confirmed")

// AdAccount is one Reddit ad account reachable with this client's credential.
type AdAccount struct {
	// ID is the ad account id (e.g. "t2_gv9wtbfa"), validated against the charset and length
	// the connection's account_id accepts, so it can be stored verbatim.
	ID string
	// Name is the account's display name; may be empty.
	Name string
	// Currency is the account's currency code when Reddit reports one; may be empty.
	Currency string
	// BusinessName is the name of the business the account was listed under; may be empty.
	BusinessName string
}

type discoveryBusiness struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type discoveryAdAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

// decodeDiscoveryList decodes a page's data field as a list of T. Absent, null and non-array data are all errors: only an explicit `[]` is an empty answer.
func decodeDiscoveryList[T any](data json.RawMessage, page int, what string) ([]T, error) {
	var out []T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("%w: %s page %d: data is not a list", ErrDiscoveryMalformed, what, page)
		}
	}
	if out == nil {
		return nil, fmt.Errorf("%w: %s page %d: no data list", ErrDiscoveryMalformed, what, page)
	}
	return out, nil
}

// ListAdAccounts enumerates every Reddit ad account the credential reaches: every business the
// token can access, then every ad account of each business, following each list's pagination.
//
// It asks about the CREDENTIAL. The client's AccountConfig is neither read nor validated —
// discovery exists for the connection that has not chosen an account yet.
//
// The result is all or nothing. Any failure — a non-2xx (401, 403, 404, 5xx, a 429 still
// throttled after request()'s bounded retry), a transport error, an unreadable or malformed
// body, an id that could not be stored, a duplicate within one list, a page or item cap — returns
// nil and an error, never the accounts gathered so far. A credential reaching no businesses, or
// businesses with no ad accounts, is an empty non-nil slice.
//
// An account listed under two businesses is returned once (the first business's listing wins):
// it is one account, and offering it twice would make a picker show a duplicate choice.
func (c *Client) ListAdAccounts(ctx context.Context) ([]AdAccount, error) {
	businesses := make([]discoveryBusiness, 0)
	seenBusiness := map[string]struct{}{}
	err := c.walkPagesCapped(ctx, http.MethodGet, "/me/businesses", nil, discoveryBusinessMaxPages, errBusinessPageCap,
		func(page int, resp *apiResponse) error {
			els, derr := decodeDiscoveryList[discoveryBusiness](resp.Data, page, "business list")
			if derr != nil {
				return derr
			}
			for _, b := range els {
				if len(b.ID) > maxBusinessIDLen || !businessIDRe.MatchString(b.ID) {
					return fmt.Errorf("%w: business list page %d: a business has an unusable id", ErrDiscoveryMalformed, page)
				}
				if _, dup := seenBusiness[b.ID]; dup {
					return fmt.Errorf("%w: business list page %d: the same business was listed twice", ErrDiscoveryMalformed, page)
				}
				if len(businesses) >= maxDiscoveredBusinesses {
					return fmt.Errorf("%w: more than %d businesses", ErrDiscoveryMalformed, maxDiscoveredBusinesses)
				}
				seenBusiness[b.ID] = struct{}{}
				businesses = append(businesses, b)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("list reddit businesses: %w", err)
	}

	accounts := make([]AdAccount, 0)
	seenAccount := map[string]struct{}{}
	for _, b := range businesses {
		inThisBusiness := map[string]struct{}{}
		businessName := strings.TrimSpace(b.Name)
		werr := c.walkPagesCapped(ctx, http.MethodGet, "/businesses/"+b.ID+"/ad_accounts", nil, discoveryAccountMaxPages, errAccountPageCap,
			func(page int, resp *apiResponse) error {
				els, derr := decodeDiscoveryList[discoveryAdAccount](resp.Data, page, "ad-account list")
				if derr != nil {
					return derr
				}
				for _, a := range els {
					// Validated RAW, not trimmed: trimming would invent an id Reddit never sent.
					if len(a.ID) > maxDiscoveredAccountIDLen || !accountIDRe.MatchString(a.ID) {
						return fmt.Errorf("%w: ad-account list page %d: an account has an unusable id", ErrDiscoveryMalformed, page)
					}
					if _, dup := inThisBusiness[a.ID]; dup {
						return fmt.Errorf("%w: ad-account list page %d: the same account was listed twice", ErrDiscoveryMalformed, page)
					}
					inThisBusiness[a.ID] = struct{}{}
					if _, dup := seenAccount[a.ID]; dup {
						continue
					}
					if len(accounts) >= maxDiscoveredAccounts {
						return fmt.Errorf("%w: more than %d ad accounts", ErrDiscoveryMalformed, maxDiscoveredAccounts)
					}
					seenAccount[a.ID] = struct{}{}
					accounts = append(accounts, AdAccount{
						ID:           a.ID,
						Name:         strings.TrimSpace(a.Name),
						Currency:     strings.TrimSpace(a.Currency),
						BusinessName: businessName,
					})
				}
				return nil
			})
		if werr != nil {
			return nil, fmt.Errorf("list reddit ad accounts: %w", werr)
		}
	}
	return accounts, nil
}
