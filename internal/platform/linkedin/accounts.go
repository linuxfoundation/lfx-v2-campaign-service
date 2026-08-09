// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	// adAccountPageSize is the per-page limit for the adAccounts search. LinkedIn caps
	// pageSize at 1000 here; a smaller page only means more round trips, and this walk
	// feeds a picker rather than a hot path.
	adAccountPageSize = 100
	// adAccountMaxPages bounds the walk at adAccountPageSize*adAccountMaxPages accounts.
	// Exceeding it is an ERROR, never a truncated list — see ListAdAccounts. This is a
	// far tighter cap than maxListPages because that one exists to survive a server-side
	// name filter the API may ignore; discovery has no filter to be ignored, so a walk
	// this long means something is wrong rather than that the collection is large.
	adAccountMaxPages = 20
)

// lifecycleStatusLabels maps a KNOWN-BAD LinkedIn ad-account `status` to a short reason.
// ACTIVE is absent deliberately: the map answers "why can this account not be used", and
// an active account has no answer. An unrecognized or absent status is also absent from
// the map and yields "" — see AdAccount.StatusLabel.
var lifecycleStatusLabels = map[string]string{
	"CANCELED":         "canceled",
	"DRAFT":            "not finished being set up",
	"PENDING_DELETION": "pending deletion",
	"REMOVED":          "deleted",
}

// servingHoldLabels maps a LinkedIn `servingStatuses` entry to a short reason the account
// cannot currently serve. RUNNABLE is absent: it is the one value that means "no hold".
var servingHoldLabels = map[string]string{
	"STOPPED":                   "stopped",
	"BILLING_HOLD":              "on billing hold",
	"ACCOUNT_TOTAL_BUDGET_HOLD": "over its total budget",
	"ACCOUNT_END_DATE_HOLD":     "past its end date",
	"RESTRICTED_HOLD":           "restricted",
	"INTERNAL_HOLD":             "on hold at LinkedIn",
}

// AdAccount is one ad account reachable with this client's access token.
//
// Lifecycle status and serving status are kept as SEPARATE fields because they answer
// different questions and can disagree: an ACTIVE account on BILLING_HOLD is perfectly
// bindable but will not spend, and a picker that collapsed the two would either hide a
// usable account or promise one that cannot serve.
type AdAccount struct {
	// ID is the bare numeric ad-account id, e.g. "507404993" — the same form
	// Account.AccountID and the connection config's account_id take (both constrained
	// to ^[0-9]+$), so it can be stored on the connection verbatim.
	ID string
	// Name is the account's label in Campaign Manager. May be empty.
	Name string
	// Status is LinkedIn's lifecycle status: ACTIVE, CANCELED, DRAFT, PENDING_DELETION
	// or REMOVED. Empty means the field was absent, which is NOT a claim either way.
	Status string
	// Type is BUSINESS or ENTERPRISE.
	Type string
	// Currency is the 3-character account currency, e.g. "USD". Budgets and bids on this
	// account are denominated in it, so a picker showing two accounts needs to show it.
	Currency string
	// Test reports LinkedIn's immutable test-account flag. Test accounts never serve and
	// never bill, and their creatives are auto-rejected — so binding a real campaign to
	// one produces a campaign that silently does nothing. Surfaced rather than filtered:
	// a developer wiring up an integration is looking for exactly this account.
	Test bool
	// ServingStatuses is LinkedIn's raw servingStatuses array: ["RUNNABLE"] when the
	// account can serve, otherwise one or more hold reasons.
	ServingStatuses []string
}

// Active reports whether the account's LIFECYCLE status is ACTIVE. It says nothing about
// whether the account can currently serve — see Servable. It is deliberately NOT used to
// filter ListAdAccounts.
func (a AdAccount) Active() bool { return a.Status == "ACTIVE" }

// StatusLabel returns a human-readable reason for a KNOWN-BAD lifecycle status, and "" for
// ACTIVE, absent, or unrecognized ones. An empty label is not a claim that the account is
// fine — only that this package has nothing to say about its status.
func (a AdAccount) StatusLabel() string { return lifecycleStatusLabels[a.Status] }

// Servable reports whether LinkedIn says the account can serve: servingStatuses is exactly
// the single element RUNNABLE. An ABSENT or empty servingStatuses returns false, because
// this is an allow-list rather than an exclusion — an unrecognized or omitted value is not
// evidence that the account can spend, and the honest answer is "not confirmed servable".
// Callers that need to distinguish "held" from "unknown" read ServingHolds, which is empty
// in the unknown case.
func (a AdAccount) Servable() bool {
	return len(a.ServingStatuses) == 1 && a.ServingStatuses[0] == "RUNNABLE"
}

// ServingHolds returns a human-readable reason for each RECOGNIZED serving hold on the
// account, in the order LinkedIn reported them. It is empty both for a servable account
// and for one whose holds are all unrecognized — Servable is what distinguishes those.
func (a AdAccount) ServingHolds() []string {
	holds := make([]string, 0, len(a.ServingStatuses))
	for _, s := range a.ServingStatuses {
		if label, ok := servingHoldLabels[s]; ok {
			holds = append(holds, label)
		}
	}
	return holds
}

// ListAdAccounts enumerates every ad account the client's access token can reach.
//
// It asks about the TOKEN, not about any one account: the request is `GET /adAccounts?
// q=search` with the `search` criteria OMITTED, which LinkedIn documents as returning
// every account the caller has access to, and no account id appears anywhere in the path
// or parameters. That is what lets a connection holding only credentials — or one being
// re-pointed at a different account — ask which accounts are available.
//
// Accounts that are canceled, draft, on billing hold, or flagged as test accounts are all
// RETURNED, each carrying the reason it is unusable. This is a picker: a user whose only
// account is on billing hold needs to see it and see why, and dropping it would answer
// "your token reaches no ad accounts" about an account sitting right there — sending them
// to look for a permissions problem that does not exist.
//
// A walk that cannot be completed is an ERROR, never a short list. A truncated account
// list is indistinguishable from a complete one at the boundary, and the caller acts on
// the absence: the account they wanted is simply not offered, and they conclude their
// token cannot reach it. The `elements`-absent case is caught one layer down, in
// doRequest's search-presence guard, which fails any GET whose body cannot prove a result
// set; the modes handled here are a repeated cursor and the page cap.
func (c *Client) ListAdAccounts(ctx context.Context) ([]AdAccount, error) {
	// make(..., 0, n) rather than a nil slice: a token that legitimately reaches zero ad
	// accounts is an ANSWER, and everything above this needs empty to stay distinguishable
	// from "no answer" — including on the wire, where nil would serialize as null.
	accounts := make([]AdAccount, 0, adAccountPageSize)
	pageToken := ""
	seen := make(map[string]struct{})
	for page := 0; page < adAccountMaxPages; page++ {
		params := map[string]string{
			// `q=search` with no `search` parameter is the documented way to ask for every
			// account the caller can access. Adding a criteria here would silently narrow
			// the picker to whatever this code happened to guess the user wanted.
			"q":        "search",
			"pageSize": strconv.Itoa(adAccountPageSize),
		}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		resp, err := c.doRequest(ctx, http.MethodGet, "adAccounts", nil, params, nil)
		if err != nil {
			return nil, fmt.Errorf("list linkedin ad accounts: %w", err)
		}
		// resp.Elements is guaranteed non-nil for a GET: doRequest rejects a 2xx search
		// body whose `elements` field is absent or null, precisely because such a body
		// cannot prove a result set. The nil check is retained so a future change to that
		// guard cannot turn this loop into a silent zero-account answer.
		if resp.Elements == nil {
			return nil, fmt.Errorf("linkedin ad-account discovery returned a 2xx response with no elements field; cannot confirm the token's accounts were enumerated")
		}
		for _, el := range *resp.Elements {
			id := strings.TrimSpace(el.ID.String())
			// accountIDRE is the SAME regexp targeting.go validates a configured account
			// id against. Reused rather than restated: an account this walk offers must
			// be one the client will later accept, and two copies of that contract can
			// drift into offering ids that fail at bind time.
			if !accountIDRE.MatchString(id) {
				// A response shape this far from the documented one means it is not the
				// response we think it is, so the rest of it is not trustworthy either —
				// fail the whole walk rather than skipping the row.
				return nil, fmt.Errorf("linkedin ad-account discovery returned an account with an unusable id")
			}
			accounts = append(accounts, AdAccount{
				ID:              id,
				Name:            strings.TrimSpace(el.Name),
				Status:          el.Status,
				Type:            el.Type,
				Currency:        el.Currency,
				Test:            el.Test,
				ServingStatuses: el.ServingStatuses,
			})
		}
		// NOT trimmed. A page cursor is an opaque server token echoed back verbatim, so
		// trimming can request a DIFFERENT page than the one offered — and a token that is
		// only whitespace would trim to "" and read as exhaustion, returning a partial
		// account list as a complete one. That is the false-absence shape this file's other
		// guards exist to prevent, arriving through the pagination door. The two existing
		// cursor walks (client.go findCreatives, findByName) preserve the exact value;
		// trimming belongs on human-entered fields, not on server-minted ones.
		next := resp.Metadata.NextPageToken
		if next == "" {
			return accounts, nil // fully enumerated
		}
		if _, dup := seen[next]; dup {
			return nil, fmt.Errorf("linkedin ad-account discovery did not terminate (repeated page cursor)")
		}
		seen[next] = struct{}{}
		pageToken = next
	}
	return nil, fmt.Errorf("linkedin ad-account discovery exceeded %d pages; too many accounts to enumerate", adAccountMaxPages)
}
