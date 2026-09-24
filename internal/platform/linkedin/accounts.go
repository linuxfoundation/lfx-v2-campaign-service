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
	err := c.walkAdAccountPages(ctx, func(page []AdAccount) (bool, error) {
		accounts = append(accounts, page...)
		return false, nil // never stop early: this walk's contract is "every account or an error"
	})
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

// walkAdAccountPages issues GET adAccounts pages in cursor order, calling visit with each
// page's accounts as they arrive. visit returns done=true to stop the walk before requesting
// the next page — the caller already has what it needs (VerifyAccountOrgReference uses this to
// stop as soon as it finds the account it is looking for, rather than paying for every
// remaining page, or discarding what it already found because a LATER page fails). It never
// stops early on its own: every completeness guard below (elements/metadata presence, cursor
// repetition, the page cap) still runs for every page visit does not cut short, because "found
// nothing yet" and "walk failed" must stay distinguishable from "walk didn't run long enough to
// tell".
func (c *Client) walkAdAccountPages(ctx context.Context, visit func(page []AdAccount) (done bool, err error)) error {
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
			return fmt.Errorf("list linkedin ad accounts: %w", err)
		}
		// resp.Elements is guaranteed non-nil for a GET: doRequest rejects a 2xx search
		// body whose `elements` field is absent or null, precisely because such a body
		// cannot prove a result set. The nil check is retained so a future change to that
		// guard cannot turn this loop into a silent zero-account answer.
		if resp.Elements == nil {
			return fmt.Errorf("linkedin ad-account discovery returned a 2xx response with no elements field; cannot confirm the token's accounts were enumerated")
		}
		pageAccounts := make([]AdAccount, 0, len(*resp.Elements))
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
				return fmt.Errorf("linkedin ad-account discovery returned an account with an unusable id")
			}
			pageAccounts = append(pageAccounts, AdAccount{
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
		done, err := visit(pageAccounts)
		if err != nil {
			return err
		}
		if done {
			return nil
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
			return fmt.Errorf("linkedin ad-account discovery returned a response with no metadata; cannot confirm the token's accounts were enumerated")
		}
		next := resp.Metadata.NextPageToken
		if next == "" {
			return nil // fully enumerated
		}
		if _, dup := seen[next]; dup {
			return fmt.Errorf("linkedin ad-account discovery did not terminate (repeated page cursor)")
		}
		seen[next] = struct{}{}
		pageToken = next
	}
	return fmt.Errorf("linkedin ad-account discovery exceeded %d pages; too many accounts to enumerate", adAccountMaxPages)
}

// ErrOrgVerificationInconclusive wraps a failure of the underlying ListAdAccounts walk itself
// that left the walk genuinely unresolved: a pre-send connection failure, a mid-flight transport
// failure, the walk's own runaway/truncation guards, HTTP 429, and any 5xx. Deliberately NOT any
// OTHER 4xx — a credential or application-authorization failure, a 403, and equally a 400 or 404
// — because LinkedIn RECEIVED those and refused them on the merits; see
// VerifyAccountOrgReference, whose guard is the authority on this split.
// It is deliberately
// distinct from a CONFIRMED contradiction (VerifyAccountOrgReference's other error returns):
// the walk failing to complete proves nothing about the account/org pairing either way, so a
// caller must not treat it as evidence of a broken connection. Callers that want to keep those
// two outcomes apart should check errors.Is(err, ErrOrgVerificationInconclusive).
var ErrOrgVerificationInconclusive = errors.New("linkedin ad-account enumeration did not complete; org reference could not be checked")

// ErrOrgVerificationFailed marks the OPPOSITE outcome: a CONFIRMED verdict about the stored
// pairing — a reference naming a different organization, an account absent from a complete walk,
// a stored id of the wrong shape, or a 403 refusal LinkedIn reached on the merits against this
// token. The other 4xx refusals are NOT confirmed verdicts and carry ErrAccountDiscoveryRejected
// instead — they say nothing about the pairing.
//
// It exists so a caller can allowlist the errors whose text is safe to render, instead of
// echoing by default and relying on every unsafe class having been given an arm first. That
// inversion is the same one ErrOrgVerificationInconclusive's conversion makes: safety as a
// property rather than an obligation each future caller must remember. The message on every
// error carrying this sentinel is built from this package's own vocabulary plus LinkedIn status
// codes (apiError.Error() deliberately omits the response body), never from a response body, a
// request URL, or credential material.
//
// Callers check errors.Is(err, ErrOrgVerificationFailed).
var ErrOrgVerificationFailed = errors.New("linkedin account/organization verification returned a confirmed verdict")

// ErrAccountDiscoveryRejected marks the THIRD outcome, which is a verdict about neither the
// pairing nor the token: LinkedIn refused the ad-account discovery request itself with a non-429,
// non-403 4xx. The walk embeds no account id and no org id, so such a refusal proves nothing
// about the stored connection — a 400 means this service built a malformed request, a 404 that
// the path it targets has moved. Both are defects in THIS service, and callers map them onto
// whatever they use for that (the dispatcher converts to domain.ErrServiceDefect) rather than
// failing the operator's connection over them.
//
// It is wrapped, not attached as a no-text tag: unlike the confirmed marker, nothing renders this
// error to an operator, so a sentence naming the class is useful in a log and duplicated nowhere.
//
// Callers check errors.Is(err, ErrAccountDiscoveryRejected).
var ErrAccountDiscoveryRejected = errors.New("linkedin refused the ad account discovery request")

// verdictError attaches ErrOrgVerificationFailed to an error WITHOUT adding any text of its own.
// A plain fmt.Errorf("%w: %w", ...) wrap would prepend the sentinel's sentence to a message the
// service layer already introduces with one of its own, so a caller would read the same thing
// twice. Unwrap keeps the chain intact, so errors.As(*apiError) and every inner sentinel check
// still works through it.
type verdictError struct{ err error }

func (e *verdictError) Error() string        { return e.err.Error() }
func (e *verdictError) Unwrap() error        { return e.err }
func (e *verdictError) Is(target error) bool { return target == ErrOrgVerificationFailed }

// confirmedVerdict tags err as a confirmed verdict. Applied at every return site in
// VerifyAccountOrgReference that states a FACT about the pairing or the stored fields — and at
// none that means the check could not run.
func confirmedVerdict(err error) error { return &verdictError{err: err} }

// SafeInconclusiveDetail classifies an error wrapped by ErrOrgVerificationInconclusive into a
// fixed, caller-safe string for logging. The underlying error can be a *transportError, or the
// plain error doRequest returns for a pre-send dial failure; either renders the raw round-trip
// failure, and a *url.Error inside one includes the full request URL, query parameters included
// (e.g. a pagination cursor). So callers must not log err.Error() verbatim. This reports only
// which failure class was hit, never any request- or response-derived text.
func SafeInconclusiveDetail(err error) string {
	// Checked FIRST, and separately from *transportError. doRequest deliberately does not
	// wrap a pre-send dial failure (DNS failure, connection refused, no route) as a
	// *transportError — that type means "may have been sent" — so a plain network outage,
	// the most common real cause of an inconclusive walk, matched neither branch below and
	// fell through to the completeness-guard string, telling an operator to go inspect a
	// LinkedIn response that was never received.
	// Checked BEFORE the dial branch, because a token exchange that could not dial is a
	// token-endpoint failure and naming discovery for it points an operator at the wrong
	// host. Either way no discovery request was ever made.
	var tre *tokenRefreshError
	if errors.As(err, &tre) || errors.Is(err, errTokenExchangeFailed) {
		return "linkedin token exchange failed, so ad-account discovery never ran"
	}
	if isPreSendDialError(err) {
		return "could not reach linkedin ad-account discovery (connection failure)"
	}
	var te *transportError
	if errors.As(err, &te) {
		return "transport failure contacting linkedin ad-account discovery"
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return fmt.Sprintf("linkedin ad-account discovery returned HTTP %d", ae.StatusCode)
	}
	return "linkedin ad-account discovery response failed a completeness guard"
}

// VerifyAccountOrgReference cross-checks a connection's configured org id against LinkedIn's
// own record — the `reference` field on accountID — of which organization sponsors that
// account. It is a connection-test-time signal, not a create-time gate: nothing in this
// package calls it, and CreateCampaign's own org resolution (resolveOrgID, targeting.go)
// is untouched by it.
//
// It returns a CONFIRMED error in two cases: accountID's reference names a DIFFERENT
// organization than configuredOrgID, and accountID is absent from a walk that reached the end
// of every page without ever finding it — walkAdAccountPages returns every account or an error
// (see its own doc comment), so exhausting it without a match means this token genuinely cannot
// reach the configured account, not that the walk merely missed it. The walk stops as soon as
// accountID itself is found, on WHATEVER page carries it — a confirmed match or mismatch on an
// earlier page is not retroactively undone by a later page this verification never needed to
// reach.
//
// It returns an ErrOrgVerificationInconclusive-wrapped error when the page walk itself fails,
// before finding accountID, for a reason that proves nothing about the pairing: a pre-send
// connection failure, a mid-flight transport failure, the page cap on a very large token or the
// other runaway/truncation guards, HTTP 429, and any 5xx. Those must not be confused with a
// confirmed contradiction. A credential failure (ErrCredentialsExpired,
// ErrApplicationCredentialsInvalid, ErrTokenRequestRejected) and EVERY other 4xx — 403, but 400
// and 404 just as much — are kept OUT of that sentinel instead: LinkedIn received the request and
// refused it on the merits, so it will not start succeeding on its own, and wrapping any of them
// in the inconclusive sentinel let TestLinkedinAds's errors.Is(err,
// ErrOrgVerificationInconclusive) check — which runs before any other classification — report a
// broken connection as OK: true. 429 is the one exempt status because a rate limit really does
// say nothing about the pairing. Those refusals do not all mean the same thing: a credential
// failure carries its own sentinel, a 403 is a confirmed verdict on this connection, and every
// remaining 4xx carries ErrAccountDiscoveryRejected because the walk names neither id and so
// refuses nothing about the pairing.
//
// An accountID or configuredOrgID that is not a numeric platform id — the empty string
// included — is likewise a CONFIRMED error, decided before the walk even starts; see the
// comment at the guards below. Every confirmed outcome carries ErrOrgVerificationFailed and
// never ErrOrgVerificationInconclusive; the marker adds no text of its own.
//
// Every other outcome returns nil, which covers a confirmed match and the ONE inconclusive
// case nil cannot distinguish from it: a reference that is empty or person-scoped, where
// LinkedIn simply has nothing on the account to compare against.
//
// This mirrors resolveOrgID's own philosophy in targeting.go — fail closed on an actual
// contradiction, and only on one this package can actually confirm.
func (c *Client) VerifyAccountOrgReference(ctx context.Context, accountID, configuredOrgID string) error {
	accountID = strings.TrimSpace(accountID)
	configuredOrgID = strings.TrimSpace(configuredOrgID)
	// Both ids are checked for SHAPE, not merely for presence, and both before contacting
	// LinkedIn — the two halves of the same stored pairing, failing the same way.
	//
	// accountIDRE is what targeting.go validates a configured account id against, so an
	// account id this fails is one CreateCampaign will refuse too: the connection is already
	// guaranteed to be unusable. Accepting it here instead spent a full enumeration walk to
	// reach one of two wrong answers — "not found among this token's own ad accounts", which
	// misdescribes a malformed stored field as a permissions problem, or, if the walk failed
	// for any other reason, ErrOrgVerificationInconclusive, which TestLinkedinAds reports as
	// OK: true. Mirrors ValidateAccountID's use in LinkedInDispatcher.ListAccountCampaignMetrics.
	//
	// The empty string falls into these guards rather than returning nil. Returning nil for a
	// caller with nothing to compare looked permissive-but-harmless for a client package, but
	// VerifyAccountOrgReference is exported and nil is read by every caller as "no mismatch
	// found"; the only thing keeping that out of a connection test was the dispatcher happening
	// to reject empty ids one layer up. A stored pairing that is half-absent is not a pairing.
	if err := ValidateAccountID(accountID); err != nil {
		return confirmedVerdict(fmt.Errorf("the configured linkedin ad account id is not usable, so campaign creation on this connection cannot succeed: %w", err))
	}
	// A non-numeric configuredOrgID is a CONFIRMED defect in the stored connection, decidable
	// without contacting LinkedIn at all. orgIDRE is this client's configuration invariant:
	// resolveOrgID (targeting.go) refuses the same value because it cannot build a valid
	// "urn:li:organization:<id>" URN, so a campaign creation on this connection is already
	// guaranteed to fail. Reporting it as inconclusive (nil) made TestLinkedinAds answer
	// OK: true for a connection known in advance to be unusable — the exact "broken
	// connection reported healthy" outcome this verification exists to prevent.
	//
	// It is deliberately NOT reported as a mismatch. LinkedIn's own reference is always
	// numeric, so this value never was a comparable org id; calling it a "different
	// organization" would misdescribe the fault and send an operator hunting a tenant mixup
	// instead of fixing a malformed field.
	if !orgIDRE.MatchString(configuredOrgID) {
		return confirmedVerdict(fmt.Errorf("the configured organization id %q is not a valid linkedin organization id (expected digits only), so campaign creation on this connection cannot build a valid organization urn", configuredOrgID))
	}
	// found/matchErr are set from inside visit and read after the walk returns; walk only
	// ever calls visit synchronously from the same goroutine, so this is not a data race.
	var found bool
	var matchErr error
	err := c.walkAdAccountPages(ctx, func(page []AdAccount) (bool, error) {
		for _, a := range page {
			if a.ID != accountID {
				continue
			}
			found = true
			// Stop the walk HERE, on the page that carries the target account, rather than
			// paying for every remaining page — and, more importantly, rather than letting
			// a later page's failure discard a confirmed match or mismatch this page already
			// proved. A confirmed contradiction found on page one is exactly as confirmed
			// whether or not LinkedIn can enumerate page two.
			if a.OrgID != "" && a.OrgID != configuredOrgID {
				matchErr = fmt.Errorf("linkedin ad account %s advertises on behalf of organization %s, not the configured organization %s", accountID, a.OrgID, configuredOrgID)
			}
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		// A credential or application-authorization failure is a CONFIRMED, actionable
		// reason this token cannot reach LinkedIn at all — not an inconclusive enumeration
		// outcome — so it must propagate as a real error rather than fold into the
		// inconclusive sentinel below, which callers treat as "proves nothing, do not fail
		// the connection over it."
		if errors.Is(err, ErrCredentialsExpired) || errors.Is(err, ErrApplicationCredentialsInvalid) || errors.Is(err, ErrTokenRequestRejected) {
			return err
		}
		// A 4xx reaches here as a plain *apiError (LinkedIn has no dedicated sentinel for
		// these, unlike the 401/token-exchange failures above), but it proves the same
		// thing they do: LinkedIn RECEIVED this request, evaluated it, and refused it.
		// Unlike a transport failure or the page-cap/runaway guards, that is not "the walk
		// could not complete": none of these improve on their own, and folding a
		// permanently-failing discovery path into the inconclusive sentinel would report
		// OK: true for it forever. Only 429 is exempt — rate limiting genuinely is an
		// interrupted walk a later attempt can complete — and so is 5xx, which is LinkedIn
		// failing to answer rather than answering with a refusal.
		//
		// The refusals then split by WHO can act on them, because the two halves carry
		// different verdicts and different remedies:
		//
		//   - 403 is a definite authorization failure. LinkedIn evaluated THIS token
		//     against THIS resource and refused it, so it is a confirmed verdict on the
		//     stored connection and the operator re-authorizes to fix it.
		//   - Everything else is a statement about the REQUEST, not the connection. The
		//     discovery walk sends q=search with a page size and cursor and embeds neither
		//     the account id nor the org id (see the request built above), so a 400 says
		//     this service built a malformed request and a 404 that the path it targets is
		//     gone. Neither is evidence about the pairing, and neither is repairable by
		//     editing connection fields — reporting them as a failed verification would
		//     send an operator to audit a configuration that was never at fault.
		var aerr *apiError
		if errors.As(err, &aerr) && aerr.StatusCode >= 400 && aerr.StatusCode < 500 && aerr.StatusCode != http.StatusTooManyRequests {
			if aerr.StatusCode == http.StatusForbidden {
				return confirmedVerdict(err)
			}
			return fmt.Errorf("%w: %w", ErrAccountDiscoveryRejected, err)
		}
		return fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, err)
	}
	if !found {
		return confirmedVerdict(fmt.Errorf("linkedin ad account %s was not found among this token's own ad accounts", accountID))
	}
	if matchErr != nil {
		return confirmedVerdict(matchErr)
	}
	return nil
}
