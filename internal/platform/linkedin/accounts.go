// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	// adAccountPageSize is the per-page limit for the adAccounts search, set to the
	// documented LinkedIn maximum. A smaller page is not merely more round trips: paired
	// with the hard page cap below it lowers the number of accounts this walk can
	// enumerate at all, and the walk's contract is that it returns EVERY account or an
	// error. At 100 it refused a legitimate 2,001-account token; at the maximum the same
	// runaway guard covers 20,000. findByName already requests 1000 for this reason.
	adAccountPageSize = 1000
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
	// OrgID is the numeric organization id parsed from LinkedIn's own `reference` field for
	// this account — its record of which organization the account advertises on behalf of —
	// or "" when the account's reference is absent, person-scoped, or malformed. It is NOT
	// the same thing as a connection's configured org id; see referenceOrgID and
	// VerifyAccountOrgReference, which compare the two.
	OrgID string
}

// referenceOrgID extracts the numeric organization id from a LinkedIn adAccount `reference`
// URN. It returns "" — never an error — for every case that is not confidently "this
// account's reference names organization <id>": an absent reference, a person-scoped one
// ("urn:li:person:..."), or a value that does not parse as expected. An empty return is
// therefore inconclusive, not a claim that the account has no organization; callers that need
// to tell "no reference" apart from "not an organization reference" do not exist yet, and
// none of the current ones need to.
func referenceOrgID(reference string) string {
	const prefix = "urn:li:organization:"
	reference = strings.TrimSpace(reference)
	if !strings.HasPrefix(reference, prefix) {
		return ""
	}
	id := strings.TrimPrefix(reference, prefix)
	if !orgIDRE.MatchString(id) {
		return ""
	}
	return id
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
// the single element RUNNABLE, and the account is not a TEST account. An ABSENT or empty
// servingStatuses returns false, because this is an allow-list rather than an exclusion —
// an unrecognized or omitted value is not evidence that the account can spend, and the
// honest answer is "not confirmed servable". Callers that need to distinguish "held" from
// "unknown" read ServingHolds, which is empty in the unknown case.
//
// The test-account term is not redundant with servingStatuses. LinkedIn reports RUNNABLE
// on test accounts — they are runnable in the sense the field means — while a campaign
// bound to one never serves, never bills, and has its creatives auto-rejected. Without
// this term a picker built on Servable would present a test account as the one healthy
// choice, which is the single most misleading answer it could give. Test accounts are
// still RETURNED by ListAdAccounts and still carry their flag; what changes here is only
// the claim "this can serve", which for a test account is false.
func (a AdAccount) Servable() bool {
	return !a.Test && len(a.ServingStatuses) == 1 && a.ServingStatuses[0] == "RUNNABLE"
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
				OrgID:           referenceOrgID(el.Reference),
			})
		}
		// NOT trimmed. A page cursor is an opaque server token echoed back verbatim, so
		// trimming can request a DIFFERENT page than the one offered — and a token that is
		// only whitespace would trim to "" and read as exhaustion, returning a partial
		// account list as a complete one. That is the false-absence shape this file's other
		// guards exist to prevent, arriving through the pagination door. The other two
		// cursor walks (client.go listCreativeURNs, findMatch) preserve the exact value;
		// trimming belongs on human-entered fields, not on server-minted ones.
		// An ABSENT metadata block is not an exhausted cursor. Without this the zero value
		// reads as "no more pages" and a malformed intermediate response truncates the
		// picker silently — the same false absence the elements guard above prevents,
		// arriving through the pagination door instead.
		if resp.Metadata == nil {
			return nil, fmt.Errorf("linkedin ad-account discovery returned a response with no metadata; cannot confirm the token's accounts were enumerated")
		}
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

// ErrOrgVerificationInconclusive wraps a failure of the underlying ListAdAccounts walk itself
// (transport, or the walk's own runaway/truncation guards) — deliberately NOT a credential or
// application-authorization failure; see VerifyAccountOrgReference. It is deliberately
// distinct from a CONFIRMED contradiction (VerifyAccountOrgReference's other error returns):
// the walk failing to complete proves nothing about the account/org pairing either way, so a
// caller must not treat it as evidence of a broken connection. Callers that want to keep those
// two outcomes apart should check errors.Is(err, ErrOrgVerificationInconclusive).
var ErrOrgVerificationInconclusive = errors.New("linkedin ad-account enumeration did not complete; org reference could not be checked")

// VerifyAccountOrgReference cross-checks a connection's configured org id against LinkedIn's
// own record — the `reference` field on accountID — of which organization sponsors that
// account. It is a connection-test-time signal, not a create-time gate: nothing in this
// package calls it, and CreateCampaign's own org resolution (resolveOrgID, targeting.go)
// is untouched by it.
//
// It returns a CONFIRMED error in two cases: accountID's reference names a DIFFERENT
// organization than configuredOrgID, and accountID is absent from a walk this package has
// already verified was complete (ListAdAccounts returns every account or an error — see its
// own doc comment — so reaching the end of that list without a match means this token
// genuinely cannot reach the configured account, not that the walk merely missed it).
//
// It returns an ErrOrgVerificationInconclusive-wrapped error when the ListAdAccounts walk
// itself fails for a reason that proves nothing about the pairing (transport failure, page cap
// on a very large token), so it must not be confused with a confirmed contradiction. A
// credential or application-authorization failure (ErrCredentialsExpired,
// ErrApplicationCredentialsInvalid, ErrTokenRequestRejected) is returned UNWRAPPED instead: it
// is the reason a broken connection cannot reach LinkedIn at all, a real and actionable
// failure, and wrapping it in the inconclusive sentinel let TestLinkedinAds's
// errors.Is(err, ErrOrgVerificationInconclusive) check — which runs before any other
// classification — report an expired or revoked credential as OK: true.
//
// Every other outcome returns nil, and is inconclusive rather than a confirmed pass, but
// nil cannot say so: a reference that is empty or person-scoped (LinkedIn simply has nothing
// to compare against), or a malformed configuredOrgID. This mirrors resolveOrgID's own
// philosophy in targeting.go — fail closed on an actual contradiction, and only on one this
// package can actually confirm.
func (c *Client) VerifyAccountOrgReference(ctx context.Context, accountID, configuredOrgID string) error {
	accountID = strings.TrimSpace(accountID)
	configuredOrgID = strings.TrimSpace(configuredOrgID)
	if accountID == "" || configuredOrgID == "" {
		return nil
	}
	// A non-numeric configuredOrgID can never equal a.OrgID (LinkedIn's reference is always
	// numeric, per orgIDRE), so comparing it anyway would report a CONFIRMED disagreement for
	// a value that was never a comparable org id in the first place — exactly the "malformed
	// configuredOrgID" case the doc comment above already promises stays inconclusive.
	if !orgIDRE.MatchString(configuredOrgID) {
		return nil
	}
	accounts, err := c.ListAdAccounts(ctx)
	if err != nil {
		// A credential or application-authorization failure is a CONFIRMED, actionable
		// reason this token cannot reach LinkedIn at all — not an inconclusive enumeration
		// outcome — so it must propagate as a real error rather than fold into the
		// inconclusive sentinel below, which callers treat as "proves nothing, do not fail
		// the connection over it."
		if errors.Is(err, ErrCredentialsExpired) || errors.Is(err, ErrApplicationCredentialsInvalid) || errors.Is(err, ErrTokenRequestRejected) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, err)
	}
	for _, a := range accounts {
		if a.ID != accountID {
			continue
		}
		if a.OrgID == "" {
			return nil
		}
		if a.OrgID != configuredOrgID {
			return fmt.Errorf("linkedin ad account %s advertises on behalf of organization %s, not the configured organization %s", accountID, a.OrgID, configuredOrgID)
		}
		return nil
	}
	return fmt.Errorf("linkedin ad account %s was not found among this token's own ad accounts", accountID)
}
