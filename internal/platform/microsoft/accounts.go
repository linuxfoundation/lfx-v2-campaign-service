// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// lifecycleStatusLabels maps a KNOWN-BAD AccountLifeCycleStatus to a short reason.
// "Active" is absent deliberately: the map answers "why can this account not be used",
// and an active account has no answer. An unrecognized or absent status is also absent
// and yields "" — see AdAccount.StatusLabel.
//
// "Pause" and "Pending" are documented "For internal use only" and both say the account
// may still be updated, but neither says ads serve; they are listed as unusable rather
// than omitted, because an account a picker offers must be one a campaign can actually
// run on, and "internal use only" is not a promise of that.
var lifecycleStatusLabels = map[string]string{
	"Draft":     "not finished being set up",
	"Inactive":  "deactivated for inactivity",
	"Pause":     "paused",
	"Pending":   "pending",
	"Suspended": "suspended",
}

// pauseReasonLabels maps Microsoft's PauseReason FLAG value to who paused the account.
// It is a flag field, but Microsoft documents only these three combinations, so this is
// an allow-list rather than a bitmask decode: a value outside it is reported verbatim
// (see AdAccount.PauseLabel) rather than guessed at, since inventing a reading of an
// undocumented flag would put words in Microsoft's mouth about a paid account.
var pauseReasonLabels = map[int]string{
	1: "paused by a user",
	2: "paused by billing",
	4: "paused by a user and by billing",
}

// AdAccount is one Microsoft Advertising account the client's credentials can reach.
//
// Lifecycle status and pause reason are SEPARATE fields because they answer different
// questions and can disagree: Microsoft returns a PauseReason alongside a status that is
// not itself "Pause", so an account can read as bindable and still not spend. A picker
// that collapsed the two would either hide a usable account or promise one that is
// stopped.
type AdAccount struct {
	// ID is the account id rendered as digits, e.g. "1234567" — the same form
	// AccountConfig.AccountID and the connection config's account_id take (both
	// constrained to ^[0-9]+$), so it can be stored on the connection verbatim.
	ID string
	// Name is the account's label in Microsoft Advertising. May be empty: the field is
	// nillable in Microsoft's schema.
	Name string
	// Number is Microsoft's human-facing account number (e.g. "X1234567"). It is what
	// the Microsoft Advertising UI shows, so a picker needs it to let a user recognise
	// the account they are looking at; it is NOT the id to store. Also nillable.
	Number string
	// Status is Microsoft's AccountLifeCycleStatus: Active, Draft, Inactive, Pause,
	// Pending or Suspended. Empty means the field was absent, which is NOT a claim
	// either way.
	Status string
	// PauseReason is Microsoft's raw flag value, 0 when the field was absent or null.
	// Retained raw rather than reduced to a bool so an undocumented value survives to
	// the caller instead of being flattened into "paused, reason unknown".
	PauseReason int
}

// Usable reports whether the account is one a campaign can be bound to and expected to
// run: status exactly "Active" AND no pause reason. It is an ALLOW-LIST, not an
// exclusion — an absent or unrecognized status returns false, because an unknown status
// is not evidence the account can spend. It is deliberately NOT used to filter
// ListAdAccounts.
func (a AdAccount) Usable() bool { return a.Status == "Active" && a.PauseReason == 0 }

// StatusLabel returns a human-readable reason for a KNOWN-BAD lifecycle status, and ""
// for Active, absent, or unrecognized ones. An empty label is not a claim that the
// account is fine — only that this package has nothing to say about its status.
func (a AdAccount) StatusLabel() string { return lifecycleStatusLabels[a.Status] }

// PauseLabel returns who paused the account, "" when it is not paused, and a verbatim
// rendering of an undocumented flag value rather than a guess. Reporting the raw value
// keeps an unexpected flag visible to whoever has to explain why an "Active" account is
// not spending; silently mapping it to "paused" would lose the only detail that
// distinguishes a Microsoft-side change from a bug here.
func (a AdAccount) PauseLabel() string {
	if a.PauseReason == 0 {
		return ""
	}
	if label, ok := pauseReasonLabels[a.PauseReason]; ok {
		return label
	}
	return "paused (unrecognized reason " + strconv.Itoa(a.PauseReason) + ")"
}

// accountsInfoResponse is the AccountsInfo/Query envelope.
//
// The distinction that matters is `{}` (the response did not answer the question) versus
// `{"AccountsInfo": []}` (the credentials reach no accounts): collapsing them reports the
// first as the second, and a user goes looking for a permissions problem that does not
// exist. A plain slice would in fact preserve it — encoding/json leaves an ABSENT field
// untouched and SETS a present `null` to nil, so either way the field is nil here, while a
// present `[]` decodes to a non-nil empty slice. (Those two are different operations and
// agree only because this envelope is declared fresh per response.) AccountsInfo is nonetheless a POINTER, and the reason is legibility rather than
// mechanism: a plain slice invites the next reader to write `len(x) == 0`, which merges
// exactly the two cases this code must keep apart, whereas a nil pointer has to be
// dereferenced and the compiler makes that decision explicit at the call site.
type accountsInfoResponse struct {
	AccountsInfo *[]accountInfo `json:"AccountsInfo"`
}

// accountInfo is one AccountInfo entry.
//
// Id is a json.Number, not an int64 or a string: Microsoft types it as a `long` and
// serializes it unquoted, and decoding a long into a float64 (which `any` would do)
// silently loses precision above 2^53 — producing a WRONG account id that still looks
// like an account id. json.Number keeps the digits Microsoft actually sent.
type accountInfo struct {
	ID          json.Number `json:"Id"`
	Name        string      `json:"Name"`
	Number      string      `json:"Number"`
	Status      string      `json:"AccountLifeCycleStatus"`
	PauseReason *int        `json:"PauseReason"`
}

// ListAdAccounts enumerates every Microsoft Advertising account these credentials can
// reach.
//
// It asks about the CREDENTIALS, not about any one account. The request is
// `POST CustomerManagement/v13/AccountsInfo/Query` with CustomerId omitted unless the
// connection already carries one — Microsoft documents that as "the user's credentials
// are used to determine the customer" — and no ad-account id appears in the path, the
// body, or the headers. That is what lets a connection holding only credentials, or one
// being re-pointed at a different account, ask which accounts are available; a call that
// required an account id could never run in the state discovery exists to resolve.
//
// OnlyParentAccounts is sent false so accounts LINKED under other customers are included.
// Sending true would silently narrow the picker to one customer's own accounts and, for
// an agency-style setup, answer "no accounts" about accounts the credentials manage.
//
// Accounts that are draft, inactive, suspended or paused are all RETURNED, each carrying
// the reason it is unusable. This is a picker: a user whose only account is suspended
// needs to see it and see why, and dropping it would answer "your credentials reach no
// ad accounts" about an account sitting right there.
//
// An incomplete answer is an ERROR, never a short list. The endpoint is unpaginated, so
// there is no cursor walk to bound — the modes that remain are a missing AccountsInfo
// envelope and an id that will not round-trip, and both fail the whole call rather than
// dropping a row. A truncated account list is indistinguishable from a complete one at
// the boundary, and the caller acts on the absence.
func (c *Client) ListAdAccounts(ctx context.Context) ([]AdAccount, error) {
	// CustomerId is omitted (not sent as 0 or null) when the connection has none, which
	// is what makes the credentials themselves determine the customer. Sending 0 would
	// be a request for customer zero, not a request to infer one.
	req := map[string]any{"OnlyParentAccounts": false}
	if c.account.CustomerID != "" {
		// Kept as the string it is stored as. doCustomerRequest validates it
		// digits-only before building the request below, and parsing it to a number
		// here would introduce a second representation that can disagree with the
		// header that call sets from the same field.
		req["CustomerId"] = json.Number(c.account.CustomerID)
	}

	// idempotent: this is a read. It creates nothing, so a 429 retry cannot
	// double-create anything, and the retry is what keeps discovery from failing a
	// user's first connection attempt over a transient rate limit.
	body, err := c.doCustomerRequest(ctx, "POST", "AccountsInfo/Query", req, true)
	if err != nil {
		return nil, fmt.Errorf("list microsoft ad accounts: %w", err)
	}

	var resp accountsInfoResponse
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		// The body is NOT quoted. It is an upstream response this code has just failed
		// to understand, so nothing is known about what it contains — and these
		// credentials' accounts are exactly the context an error here travels with.
		//
		// "could not be decoded", not "is not valid JSON": Unmarshal also fails on
		// syntactically perfect JSON whose field TYPES do not match — a string where
		// AccountsInfo expects a list is the likelier upstream change of the two, and
		// naming it a syntax error sends the reader to look for a truncated body.
		return nil, fmt.Errorf("microsoft ad-account discovery returned a 2xx body that could not be decoded")
	}
	if resp.AccountsInfo == nil {
		return nil, fmt.Errorf("microsoft ad-account discovery returned a 2xx response with no AccountsInfo field; cannot confirm the credentials' accounts were enumerated")
	}

	// make(..., 0, n) rather than a nil slice: credentials that legitimately reach zero
	// accounts is an ANSWER, and everything above this needs empty to stay
	// distinguishable from "no answer" — including on the wire, where nil serializes
	// as null.
	accounts := make([]AdAccount, 0, len(*resp.AccountsInfo))
	for _, ai := range *resp.AccountsInfo {
		id := strings.TrimSpace(ai.ID.String())
		// accountIDRE is the SAME regexp validateAccountIDs checks a configured account
		// id against. Reused rather than restated: an account this call offers must be
		// one the client will later accept, and two copies of that contract can drift
		// into offering ids that fail at bind time. It also rejects the shapes a
		// json.Number can legally hold but an id cannot — "1.5e3", "-1", "" — so a
		// number that is not an integer id fails here rather than being rendered into
		// something that merely looks like one.
		if !accountIDRE.MatchString(id) {
			// A response shape this far from the documented one means it is not the
			// response we think it is, so the rest of it is not trustworthy either —
			// fail the whole call rather than skipping the row.
			return nil, fmt.Errorf("microsoft ad-account discovery returned an account with an unusable id")
		}
		pause := 0
		if ai.PauseReason != nil {
			pause = *ai.PauseReason
		}
		accounts = append(accounts, AdAccount{
			ID:          id,
			Name:        strings.TrimSpace(ai.Name),
			Number:      strings.TrimSpace(ai.Number),
			Status:      ai.Status,
			PauseReason: pause,
		})
	}
	return accounts, nil
}
