// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// adAccountPageSize is the per-page limit for GET /me/adaccounts. Meta caps the
	// `limit` parameter well above this; a smaller page just means more round trips.
	adAccountPageSize = 100
	// adAccountMaxPages bounds the walk at adAccountPageSize*adAccountMaxPages accounts.
	// Exceeding it is an ERROR, never a truncated list — see ListAdAccounts.
	adAccountMaxPages = 20
)

// AdAccount is one ad account reachable with this client's access token.
type AdAccount struct {
	// ID is the ad-account node id, e.g. "act_193556282970417" — the same form
	// AccountConfig.AccountID takes, so it can be stored on the connection verbatim.
	ID string
	// Name is the account's display name as set in Ads Manager. May be empty: `name`
	// is not guaranteed on every account, and an unnamed account is still selectable.
	Name string
	// Status is Meta's account_status code (1 = active). Zero means the field was
	// absent from the response, which Meta does for accounts it will not report on;
	// it is NOT a claim that the account is disabled.
	Status int
}

// Active reports whether Meta considers this account active. It is deliberately NOT used
// to filter ListAdAccounts — see that function.
func (a AdAccount) Active() bool { return a.Status == metaAccountStatusActive }

// StatusLabel returns a human-readable reason for a KNOWN-BAD account_status, and "" for
// active, absent, or unrecognized ones. It reads the same map CreateCampaign's preflight
// uses to refuse a campaign, so the picker and the create path cannot disagree about which
// accounts are known-bad.
func (a AdAccount) StatusLabel() string { return inactiveAccountStatusLabels[a.Status] }

// ListAdAccounts enumerates every ad account the client's access token can reach.
//
// It asks about the TOKEN, not about any one account: the path is `/me/adaccounts`, and
// the client's AccountConfig is not consulted at all. That is what lets a connection with
// no account id — or one being re-pointed at a different account — ask which accounts are
// available. Which of those lifecycles a caller can actually reach is decided above this
// package, by whether the connection's config requires an account id at create time; today
// only re-pointing is reachable for Meta (LFXV2-3061).
//
// Disabled, unsettled and closed accounts are RETURNED, not filtered. This is a picker:
// a user whose only account is unsettled needs to see it and its reason, and dropping it
// would answer "your token reaches no ad accounts" about an account sitting right there —
// sending them to look for a permissions problem that does not exist. The status travels
// with each account (StatusLabel) so the caller can present it; the decision to refuse a
// CAMPAIGN on a known-bad account stays where it already is, in CreateCampaign's preflight.
//
// A walk that cannot be completed is an ERROR, never a short list. Every failure mode
// below — a 2xx body with no `data` field, a `next` link with no cursor, a repeated
// cursor, an entry whose id is not `act_<digits>`, the page cap — returns nil rather
// than what was collected so far. The unusable-id case belongs on that list even though
// it is a per-ROW defect: a response shape that far from the documented one means the
// response is not what we think it is, so the walk fails whole rather than dropping the
// row and reporting the remainder as complete. A truncated
// account list is indistinguishable from a complete one at the boundary, and the caller
// acts on an absence: the account they wanted is simply not offered, and they conclude
// their token cannot reach it.
func (c *Client) ListAdAccounts(ctx context.Context) ([]AdAccount, error) {
	// make(..., 0, n) rather than a nil slice: a token that legitimately reaches zero ad
	// accounts is an empty list, and the dispatch layer above relies on empty staying
	// distinguishable from "no answer".
	accounts := make([]AdAccount, 0, adAccountPageSize)
	after := ""
	seen := make(map[string]struct{})
	for page := 0; page < adAccountMaxPages; page++ {
		// Build the path OURSELVES from the opaque cursor rather than following Meta's
		// absolute `paging.next` URL: that URL carries the access_token (and
		// appsecret_proof) as query parameters, which would then land in the request URL
		// and be copied into apiError/transportError — and the discovery handler logs
		// those errors. Same reasoning as listAdIDs.
		path := "/me/adaccounts?fields=id,name,account_status&limit=" + strconv.Itoa(adAccountPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var resp struct {
			// `resp` is declared INSIDE the loop, so Data starts nil on every page and its
			// nil-ness is decided by this page's body alone. encoding/json replaces the
			// slice with a non-nil empty one for a present `[]` and leaves it nil for an
			// absent or null field, which is exactly the distinction the guard below needs:
			// an intentionally empty page (`{"data":[]}`) proves the token reaches no
			// accounts, while a malformed `{}` proves nothing and must not read as
			// "fully enumerated, zero accounts".
			Data []struct {
				ID            string `json:"id"`
				Name          string `json:"name"`
				AccountStatus int    `json:"account_status"`
			} `json:"data"`
			Paging struct {
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := c.doRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		if resp.Data == nil {
			return nil, fmt.Errorf("meta ad-account discovery returned a 2xx response with no data field; cannot confirm the token's accounts were enumerated")
		}
		for _, a := range resp.Data {
			id := strings.TrimSpace(a.ID)
			// accountIDRE is the SAME regexp AccountConfig.AccountID is validated against
			// (client.go). Discovery deliberately reuses it rather than restating the
			// pattern: an account this walk offers must be one the client will later
			// accept, and two copies of the contract can drift apart.
			if !accountIDRE.MatchString(id) {
				// An entry whose id is not act_DIGITS cannot be stored as a connection's
				// account or used to build any account-scoped path, so offering it would
				// hand the user a choice that fails at bind time. Failing the whole walk
				// rather than skipping the row: a shape this far from the documented one
				// means the response is not what we think it is, and the rest of it is
				// not trustworthy either.
				return nil, fmt.Errorf("meta ad-account discovery returned an account with an unusable id")
			}
			accounts = append(accounts, AdAccount{
				ID:     id,
				Name:   strings.TrimSpace(a.Name),
				Status: a.AccountStatus,
			})
		}
		if resp.Paging.Next == "" {
			return accounts, nil // fully enumerated
		}
		after = strings.TrimSpace(resp.Paging.Cursors.After)
		if after == "" {
			return nil, fmt.Errorf("meta ad-account discovery has more pages but no cursor; cannot guarantee every account was enumerated")
		}
		if _, dup := seen[after]; dup {
			return nil, fmt.Errorf("meta ad-account discovery did not terminate (repeated paging cursor)")
		}
		seen[after] = struct{}{}
	}
	return nil, fmt.Errorf("meta ad-account discovery exceeded %d pages; too many accounts to enumerate", adAccountMaxPages)
}
