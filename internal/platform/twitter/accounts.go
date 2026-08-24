// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// adAccountPageSize is the per-page `count` for GET /{version}/accounts, set to the
	// documented X Ads maximum. X documents `count` as min 1, max 1000, default 200
	// ("For most endpoints, the maximum count value is 1,000, the minimum is 1, and the
	// default is 200"). Requesting the maximum is not merely fewer round trips: paired
	// with the hard page cap below it raises the number of accounts this walk can
	// enumerate at all, and the walk's contract is that it returns EVERY account or an
	// error. The other cursor walk in this package (findByName) requests 1000 for the
	// same reason.
	adAccountPageSize = 1000
	// adAccountMaxPages bounds the walk at adAccountPageSize*adAccountMaxPages accounts.
	// Exceeding it is an ERROR, never a truncated list — see ListAdAccounts. It is a
	// tighter cap than maxListPages because that one exists to survive a server-side
	// name filter X may ignore; discovery sends no `q`, so there is no filter to be
	// ignored and a walk this long means something is wrong rather than that the
	// collection is genuinely large. A single user reaching 20,000 ad accounts is not a
	// state this endpoint should silently truncate for.
	adAccountMaxPages = 20
	// maxAccountIDLen mirrors MaxLength(64) on twitter-ads-connection-config.account_id in
	// design/connection.go, which Goa enforces at bind time. It is duplicated here rather
	// than imported because design/ is the DSL, not a runtime package — so the two must be
	// changed together, and the comment at the discovery check names the design attribute
	// so a reader can find its counterpart. Real X account ids are far shorter (e.g.
	// "8r7gb"); the bound exists so discovery never offers an id the connection cannot
	// store, not because a legitimate id approaches it.
	maxAccountIDLen = 64
)

// approvalStatusLabels maps a KNOWN-BAD X ad-account `approval_status` to a short reason.
// ACCEPTED is absent deliberately: the map answers "why can this account not be used",
// and an accepted account has no answer.
//
// **The enum is UNVERIFIED.** X's published Accounts reference shows only "ACCEPTED" in
// its example response and enumerates no other value, so the two entries below are the
// values X's own tooling and error copy use, not values this codebase could confirm from
// primary documentation. That is exactly why the map is an ALLOW-LIST keyed on known-bad
// values rather than a deny-list: an unrecognized or absent status yields "" (see
// AdAccount.ApprovalLabel), which is NOT a claim the account is fine — only that this
// package has nothing to say about it. A deny-list would have had to guess the good
// values, and guessing wrong would either hide a usable account or label a healthy one as
// broken. Unknown values travel to the caller untouched in Status.
var approvalStatusLabels = map[string]string{
	"UNDER_REVIEW": "under review",
	"REJECTED":     "rejected",
}

// AdAccount is one X Ads account reachable with this client's OAuth 1.0a user context.
type AdAccount struct {
	// ID is the X Ads account id — an alphanumeric handle, e.g. "18ce54d4x5t". This is
	// the same form AccountConfig.AccountID takes and the connection's account_id
	// stores (both constrained to ^[A-Za-z0-9]+$), so it can be stored verbatim.
	ID string
	// Name is the account's label in X Ads Manager. May be empty: an unnamed account is
	// still selectable, so this is not treated as a defect.
	Name string
	// Status is X's raw `approval_status`, e.g. "ACCEPTED". Empty means the field was
	// absent, which is NOT a claim either way — see ApprovalLabel.
	Status string
	// Deleted reports X's `deleted` flag. The walk does not send `with_deleted`, whose
	// documented default is false, so this should be false on every row; it is carried
	// rather than assumed because a row X marks deleted must not look as selectable as
	// a live one if the default ever changes or a row arrives flagged anyway.
	Deleted bool
	// Timezone is the account's configured timezone, e.g. "America/Los_Angeles". It is
	// surfaced because X reports campaign schedules and daily budget resets against it,
	// so two accounts that differ only by timezone are genuinely different choices.
	Timezone string
}

// ApprovalLabel returns a human-readable reason for a KNOWN-BAD approval_status, and ""
// for ACCEPTED, absent, or unrecognized ones. An empty label is not a claim the account
// is fine — only that this package has nothing to say about its status. See
// approvalStatusLabels for why this is an allow-list of bad values.
func (a AdAccount) ApprovalLabel() string { return approvalStatusLabels[a.Status] }

// accountElement is one row of the `data` array returned by GET /{version}/accounts.
type accountElement struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ApprovalStatus string `json:"approval_status"`
	Deleted        bool   `json:"deleted"`
	Timezone       string `json:"timezone"`
}

// ListAdAccounts enumerates every X Ads account this client's OAuth 1.0a user context can
// reach.
//
// It asks about the CREDENTIAL, not about any one account: the request is
// `GET {base}/{version}/accounts`, the collection form of the `/accounts/{id}` resource
// every other call in this client is nested under, and no account id appears anywhere in
// the path or the query. X documents it as "a listing of advertising accounts that the
// current user has access to". That is what lets a connection holding only credentials —
// or one being re-pointed at a different account — ask which accounts are available, and
// it is why this is the one call in this package that must NOT go through doRequest,
// whose path is rooted at accountURL() and would scope the question to a single answer.
//
// The client's AccountConfig is not consulted at all, and deliberately is not validated:
// accountIDRe guards the stored id on every account-scoped path, but requiring a valid
// stored id HERE would make discovery reachable only by connections that have already
// chosen an account — the account-less connection this endpoint exists to rescue is
// exactly the one it would refuse.
//
// `q`, `account_ids` and `sort_by` are all deliberately UNSENT. Each is optional and each
// narrows the answer: `account_ids` scopes to a caller-supplied subset, and `q` prefix-
// matches on name. Sending any of them would silently narrow the picker to whatever this
// code guessed the user wanted, and the caller cannot tell a narrowed list from a
// complete one. `with_deleted` is likewise unsent, taking X's documented default of
// false — a deleted account is not a choice — but the `deleted` flag is still carried
// per row rather than assumed, so a flagged row cannot pass as live.
//
// Accounts that are under review or rejected are RETURNED, each carrying the reason it is
// unusable. A row X flags DELETED is returned too if one arrives, but that is defensive
// rather than a promise: with `with_deleted` unsent the default excludes deleted rows
// upstream, so this walk normally never sees one. This is a picker: a user whose only account is
// under review needs to see it and see why, and dropping it would answer "your credential
// reaches no ad accounts" about an account sitting right there — sending them to look for
// a permissions problem that does not exist.
//
// A walk that cannot be completed is an ERROR, never a short list. A truncated account
// list is indistinguishable from a complete one at the boundary, and the caller acts on
// the absence: the account they wanted is simply not offered, and they conclude their
// credential cannot reach it. Every failure mode below — a 2xx body with no `data` field,
// a `data` that is not an account list, an absent `next_cursor`, a repeated cursor, a row
// whose id is not alphanumeric, the page cap — returns nil rather than what was collected
// so far.
//
// **What X's documentation does NOT settle**, recorded because the choice made here is
// the fail-loud one rather than the plausible one: X does not document the zero-account
// case. If the credential reaches no accounts, this returns an empty, non-nil slice and a
// nil error ONLY when X sent `"data":[]` with an explicit `next_cursor` — a body that
// affirmatively says "here is the set, and it is empty". Anything less than that (an
// error status, a body with no `data`, a `"data":null`, a body with no `next_cursor`) is
// an error, so an empty answer here always means X said the set was empty, never that the
// call failed.
func (c *Client) ListAdAccounts(ctx context.Context) ([]AdAccount, error) {
	// make(..., 0, n) rather than a nil slice: a credential that legitimately reaches
	// zero ad accounts is an ANSWER, and everything above this needs empty to stay
	// distinguishable from "no answer" — including on the wire, where nil would
	// serialize as null.
	accounts := make([]AdAccount, 0, adAccountPageSize)
	cursor := ""
	seen := make(map[string]struct{})
	// The collection URL is built here rather than by doRequest, which roots every path
	// at accountURL() — /accounts/{id} — and would therefore ask about one account
	// instead of all of them. doRequestAbs is the documented escape hatch for exactly
	// this (the stats endpoint uses it for the same reason) and applies the identical
	// OAuth1 signing, redirect policy, bounded read and error classification.
	baseURL := fmt.Sprintf("%s/%s/accounts", c.baseURL, c.apiVersion)
	for page := 0; page < adAccountMaxPages; page++ {
		reqURL := baseURL + "?count=" + strconv.Itoa(adAccountPageSize)
		if cursor != "" {
			// Escaped, but NOT trimmed. A page cursor is an opaque server token echoed
			// back verbatim, so trimming can request a DIFFERENT page than the one
			// offered — and a token that is only whitespace would trim to "" and read as
			// exhaustion, returning a partial account list as a complete one. The other
			// cursor walk in this package (findByName) preserves the exact value for the
			// same reason; trimming belongs on human-entered fields, not server-minted
			// ones.
			reqURL += "&cursor=" + url.QueryEscape(cursor)
		}
		// logPath is the bare collection path, never reqURL: the URL carries the cursor
		// query, and apiError/transportError render their Path into strings that are
		// persisted into a campaign's Steps and logged by the discovery handler. A query
		// string has no place in either.
		resp, err := c.doRequestAbs(ctx, http.MethodGet, reqURL, "accounts", nil)
		if err != nil {
			return nil, fmt.Errorf("list x ad accounts: %w", err)
		}
		if resp == nil {
			return nil, fmt.Errorf("x ad-account discovery returned an empty response; cannot confirm the credential's accounts were enumerated")
		}
		// Declared INSIDE the loop so it starts nil on every iteration and its nil-ness
		// is decided by this page's body alone.
		var elements []accountElement
		// resp.Data is nil ONLY when the `data` key was absent entirely. An explicit
		// `"data":null` is NOT nil here — encoding/json stores the four bytes `null` in a
		// json.RawMessage — so the absent case cannot be detected by a nil check alone,
		// and `null` would then unmarshal into a nil `elements` and report a healthy zero
		// accounts. Both are checked, and the check that catches them is on `elements`
		// AFTER decoding: a present `[]` decodes to a non-nil empty slice, while both
		// absent and null leave it nil. That is the one distinction this guard needs, and
		// it is the difference between "the credential reaches no accounts" and "this
		// body cannot tell us anything".
		if len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, &elements); err != nil {
				return nil, fmt.Errorf("x ad-account discovery returned a data field that is not an account list")
			}
		}
		if elements == nil {
			return nil, fmt.Errorf("x ad-account discovery returned a 2xx response with no data field; cannot confirm the credential's accounts were enumerated")
		}
		for _, el := range elements {
			// Validated RAW, deliberately NOT trimmed. An account id is an opaque
			// upstream token: trimming " acct1 " does not clean up the row, it invents
			// the DIFFERENT id `acct1` and offers it as one X sent, so we would bind a
			// connection to an id we never actually saw. Padding is malformed data from
			// upstream and this walk's stated policy is that a row whose id is not
			// alphanumeric fails it — repairing the value silently would exempt the one
			// malformation that happens to be easy to repair. The page cursor a few
			// lines above is left untrimmed for the same reason.
			id := el.ID
			// accountIDRe is the SAME regexp this client validates a CONFIGURED account
			// id against before every account-scoped call (client.go). Reused rather
			// than restated: an account this walk offers must be one the client will
			// later accept, and two copies of that contract can drift into offering ids
			// that fail at bind time. It is also the charset the connection's account_id
			// is pattern-checked against in design/connection.go. It is anchored and
			// admits no whitespace, so it rejects a padded id on its own.
			// The LENGTH bound is checked alongside the charset because the charset is only
			// half of what the connection will accept. design/connection.go caps
			// twitter-ads-connection-config.account_id at MaxLength(64) as well as
			// Pattern(^[A-Za-z0-9]+$), and Goa enforces BOTH at bind time. Validating only
			// the charset here meant a 65+ character alphanumeric id was advertised by
			// discovery as ready to store and then rejected as a 422 every single time the
			// user selected it — a dead entry in the picker with no way to tell from the
			// live ones. Discovery must not offer what bind will always refuse; the two
			// checks are one contract and drift the moment only one of them is stated.
			//
			// The bound is applied HERE rather than by tightening accountIDRe, which is the
			// shared path-injection guard on the campaign-create, metrics and toggle paths
			// too. Those validate an id already stored on a connection, where a length the
			// design admitted is not theirs to re-litigate; this walk is the one deciding
			// what to OFFER.
			if !accountIDRe.MatchString(id) || len(id) > maxAccountIDLen {
				// A response shape this far from the documented one means it is not the
				// response we think it is, so the rest of it is not trustworthy either —
				// fail the whole walk rather than skipping the row. Skipping would hand
				// back a list that is short by an unknown amount and looks complete.
				return nil, fmt.Errorf("x ad-account discovery returned an account with an unusable id")
			}
			accounts = append(accounts, AdAccount{
				ID:       id,
				Name:     strings.TrimSpace(el.Name),
				Status:   el.ApprovalStatus,
				Deleted:  el.Deleted,
				Timezone: strings.TrimSpace(el.Timezone),
			})
		}
		// An ABSENT next_cursor is NOT an exhausted cursor. X documents termination as an
		// explicit null — "If less than count entities are returned in the current page
		// of the result set, the next_cursor value will be null" — so a body carrying no
		// such key is not a body that said the walk is finished. Without this guard the
		// zero value reads as "no more pages" and a malformed intermediate response
		// truncates the picker silently: the same false absence the data guard above
		// prevents, arriving through the pagination door.
		if !resp.NextCursorPresent {
			return nil, fmt.Errorf("x ad-account discovery returned a response with no next_cursor field; cannot confirm the credential's accounts were enumerated")
		}
		// A present but EMPTY next_cursor is not exhaustion either. X documents
		// termination as an explicit null, and `""` is a value its contract never gives a
		// meaning to — so accepting it repeats the absent-cursor defect above one step
		// further in: the walk stops on a body that never said it was finished, and the
		// accounts gathered so far are returned AS A COMPLETE LIST. The user then picks
		// from a truncated account picker that is indistinguishable from a full one. Only
		// the documented null terminates.
		if !resp.NextCursorNull && resp.NextCursor == "" {
			return nil, fmt.Errorf("x ad-account discovery returned an empty next_cursor; X signals exhaustion with an explicit null, so the credential's accounts cannot be confirmed as fully enumerated")
		}
		// Present and null is X's documented exhaustion signal: fully enumerated.
		if resp.NextCursorNull {
			return accounts, nil
		}
		if _, dup := seen[resp.NextCursor]; dup {
			return nil, fmt.Errorf("x ad-account discovery did not terminate (repeated page cursor)")
		}
		seen[resp.NextCursor] = struct{}{}
		cursor = resp.NextCursor
	}
	return nil, fmt.Errorf("x ad-account discovery exceeded %d pages; too many accounts to enumerate", adAccountMaxPages)
}
