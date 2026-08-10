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

// Role ID constants for Microsoft Advertising roles.
const (
	// RoleNotDiscovered represents a pre-configured account where the role was not discovered.
	// This is distinct from 0 (unparseable role) to maintain the allow-through semantics
	// for pre-configured accounts while defaulting to deny for genuinely unknown roles.
	RoleNotDiscovered = -1
	// RoleViewer is the read-only Viewer role (value 100) which cannot create campaigns.
	RoleViewer = 100
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
	// RoleID is Microsoft's role id for the role used to discover this account.
	// Values: RoleViewer (100) = read-only; RoleNotDiscovered (-1) = pre-configured account,
	// role not discovered; 0 = role was present but unparseable/absent; other positive values
	// may have write permission. See Usable() for how roles are interpreted.
	RoleID int
}

// Usable reports whether the account is one a campaign can be bound to and expected to
// run: status exactly "Active" AND no pause reason AND reachable with write permission.
// It is a DENY-LIST for roles known to be read-only or invalid, not an allow-list. The
// full set of write-granting RoleId values from Microsoft's Customer Management API is
// not documented here; only roles known to deny write are explicitly handled. An absent
// or unrecognized status returns false because an unknown status is not evidence the
// account can spend. It is deliberately NOT used to filter ListAdAccounts.
//
// Role semantics (DENY-LIST):
// - RoleNotDiscovered (-1): Pre-configured account, role not discovered. Allowed (assume write).
// - RoleViewer (100): Read-only. Denied.
// - 0: Role unparseable/absent during discovery. Denied (fail closed).
// - Other positive values: Unknown write capability. Allowed (assume write, pending Microsoft documentation).
// - Other negative values: Invalid. Denied.
func (a AdAccount) Usable() bool {
	if a.Status != "Active" || a.PauseReason != 0 {
		return false
	}
	// DENY-LIST: deny only roles known to be read-only or invalid.
	// RoleViewer (100) and 0 (unparseable) are denied; all other positive and RoleNotDiscovered are allowed.
	switch a.RoleID {
	case RoleViewer:
		return false
	case 0:
		// Unparseable role discovered during account enumeration. Deny (fail closed).
		return false
	case RoleNotDiscovered:
		// Pre-configured account. Allow through (assume write permission).
		return true
	default:
		// Allow positive roles (unknown write capability) and deny other negatives (except -1).
		return a.RoleID > 0 || a.RoleID == RoleNotDiscovered
	}
}

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

// userQueryResponse is the User/Query envelope, decoded only for its CustomerRoles.
//
// CustomerRoles is a POINTER for the same reason accountsInfoResponse.AccountsInfo is:
// absent (the response did not answer) has to stay distinguishable from present-and-empty,
// and Microsoft documents that at minimum one item is returned — so an EMPTY list is
// already a contract violation, and an absent one says nothing at all. The User object
// itself is deliberately not decoded: it carries a password field, a secret answer and an
// authentication token, and this code needs none of them.
type userQueryResponse struct {
	CustomerRoles *[]customerRole `json:"CustomerRoles"`
}

// customerRole is one CustomerRole entry, decoded only for its CustomerId.
//
// AccountIds and LinkedAccountIds are deliberately NOT used even though they list account
// ids directly: they are ids without names, numbers or lifecycle status, so a picker built
// from them could not show a user which account is which or why one is unusable. They are
// the wrong source for this; the customer ids are what this call is for.
type customerRole struct {
	CustomerID json.Number `json:"CustomerId"`
	RoleID     json.Number `json:"RoleId"`
}

// discoveredCustomer is a customer id with its associated role id.
type discoveredCustomer struct {
	id     string
	roleID int64
}

// roleIsStrong reports whether a role is likely to grant write permission (is "strong").
// This is a DENY-LIST: a role is weak only if it is known to be read-only (RoleViewer 100)
// or invalid (0, unparseable). The full set of write-granting RoleIds from Microsoft's
// Customer Management API is not documented here, so all other values are assumed strong.
// RoleNotDiscovered (-1) is strong (pre-configured accounts assume write).
func roleIsStrong(roleID int64) bool {
	switch roleID {
	case RoleViewer:
		return false
	case 0:
		// Unparseable role. Weak (fail closed).
		return false
	case RoleNotDiscovered:
		// Pre-configured account. Strong (assume write).
		return true
	default:
		// Other positive and negative (except -1) roles: allow positive (unknown capability),
		// deny other negatives.
		return roleID > 0
	}
}

// discoveryCustomerIDs resolves which customers to enumerate accounts under.
//
// A configured CustomerID is taken as the answer: the connection has been scoped on
// purpose, and widening it here would offer accounts the operator deliberately excluded.
// It is still validated as an identity, not merely as header bytes — see below.
//
// With no configured customer the credentials are the whole question, and one
// AccountsInfo/Query cannot answer it. Microsoft documents that operation as returning
// accounts "accessible from the specified customer" and CustomerId as optional only in the
// sense that "if not set, the user's credentials are used to determine THE customer" —
// singular either way. A user whose credentials reach several customers gets one
// CustomerRole per customer from User/Query, and only enumerating each of them covers the
// set. OnlyParentAccounts=false does not close this: it adds accounts LINKED to the
// customer being queried, which is a different relationship from a second customer the
// same user administers.
//
// The list is returned in Microsoft's own order. When an account is reachable under
// multiple customers (a linked account), ListAdAccounts keeps the one with the stronger
// role (most likely to grant write permission), so the result is deterministic for a
// given response.
func (c *Client) discoveryCustomerIDs(ctx context.Context) ([]discoveredCustomer, error) {
	if c.account.CustomerID != "" {
		// The SAME identity check the discovered roles below get, and for the same
		// reason. doCustomerRequest already rejects a non-digit CustomerID, but that is
		// a transport check on a header value: `0` and anything past MaxInt64 pass it,
		// because as header bytes they are harmless. Here the value is not a header —
		// it is the answer to "which customer's accounts are these", returned to a
		// caller that enumerates under it and offers the results as a picker. A
		// configured id being trusted more than a discovered one is backwards: both are
		// identity claims, and the configured one has been sitting in a connection
		// record since whenever it was written.
		// A value conversion, not a pointer cast. `(*json.Number)(&c.account.CustomerID)`
		// compiles — json.Number's underlying type is string — but it reads as a
		// reinterpretation of the connection record's own field, and the two callers
		// below (`&role.CustomerID`, `&ai.ID`) take the address of a json.Number that
		// already is one. Converting first keeps all three sites saying the same thing.
		customerID := json.Number(c.account.CustomerID)
		id := numberID(&customerID)
		if id == "" {
			return nil, fmt.Errorf("invalid Microsoft Advertising customer id %q on this connection: must be a positive integer", clipID(c.account.CustomerID))
		}
		// Configured customers have no role information; use RoleNotDiscovered to indicate
		// that the role was not discovered (distinct from 0, which means unparseable).
		return []discoveredCustomer{{id: id, roleID: int64(RoleNotDiscovered)}}, nil
	}

	// idempotent: a read, so a 429 retry cannot create anything. UserId is omitted
	// entirely, which Microsoft documents as "details for the authenticated user" —
	// exactly the question being asked. An empty JSON object, not a null body: the
	// operation takes a request object.
	body, err := c.doCustomerRequest(ctx, "POST", "User/Query", map[string]any{}, true)
	if err != nil {
		return nil, fmt.Errorf("list microsoft customers: %w", err)
	}
	var resp userQueryResponse
	if uerr := json.Unmarshal(body, &resp); uerr != nil {
		// Not quoted, for the same reason as the AccountsInfo body below: this is a
		// response the code has just failed to understand, so nothing is known about
		// what is in it — and a User/Query body is the one most likely to carry
		// personal data.
		return nil, fmt.Errorf("microsoft customer discovery returned a 2xx body that could not be decoded")
	}
	// Absent AND empty both fail. Microsoft documents "at minimum one list item will be
	// returned", so zero roles is not the answer "no customers" — it is a response that
	// does not match the contract, and treating it as an empty account list would report
	// a protocol change as a permissions problem.
	if resp.CustomerRoles == nil || len(*resp.CustomerRoles) == 0 {
		return nil, fmt.Errorf("microsoft customer discovery returned a 2xx response with no CustomerRoles; cannot confirm which customers these credentials reach")
	}

	// Define a maximum customer count to prevent unbounded request quota consumption.
	// This preserves the fail-not-truncate contract without allowing response amplification.
	const maxCustomers = 1000
	if len(*resp.CustomerRoles) > maxCustomers {
		return nil, fmt.Errorf("microsoft customer discovery returned %d CustomerRoles, exceeding the maximum of %d; cannot confirm which customers these credentials reach", len(*resp.CustomerRoles), maxCustomers)
	}

	customers := make([]discoveredCustomer, 0, len(*resp.CustomerRoles))
	seen := make(map[string]struct{}, len(*resp.CustomerRoles))
	for _, role := range *resp.CustomerRoles {
		// numberID, not accountIDRE. The two differ on exactly the values that matter
		// here: accountIDRE is `^[0-9]+$`, which is a TRANSPORT check — it asks whether
		// the string is safe to place in a header — and so it admits "0" and a
		// forty-digit number. This is an IDENTITY check: the id names a customer we are
		// about to query, and Microsoft customer ids are positive int64s, so "0" and
		// anything past MaxInt64 cannot be one. numberID enforces both (campaign.go),
		// and everything it accepts accountIDRE accepts too, so the ids reaching the
		// request are still ones doCustomerRequest will pass.
		id := numberID(&role.CustomerID)
		if id == "" {
			// The whole call, not this role: a response this far from the documented
			// shape is not the response we think it is, so skipping the row would turn
			// a protocol mismatch into a silently short account list.
			return nil, fmt.Errorf("microsoft customer discovery returned a role with an unusable customer id")
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		// Extract role id from the role; default to 0 if absent or unparseable.
		// Parse with the bit size of the destination int type to avoid truncation on 32-bit systems.
		var roleID int64
		if role.RoleID != "" {
			// Try to parse with strconv.IntSize for the platform-native int size.
			if rid, err := strconv.ParseInt(string(role.RoleID), 10, strconv.IntSize); err == nil {
				roleID = int64(rid)
			}
		}
		customers = append(customers, discoveredCustomer{id: id, roleID: roleID})
	}
	return customers, nil
}

// ListAdAccounts enumerates every Microsoft Advertising account these credentials can
// reach.
//
// It asks about the CREDENTIALS, not about any one account: no ad-account id appears in
// the path, the body, or the headers. That is what lets a connection holding only
// credentials, or one being re-pointed at a different account, ask which accounts are
// available; a call that required an account id could never run in the state discovery
// exists to resolve.
//
// "Every" needs one AccountsInfo/Query PER CUSTOMER, not one call. That operation is
// scoped to a single customer whether or not CustomerId is sent — omitting it makes the
// credentials determine THE customer, singular — so a user administering several
// customers would have had the other customers' accounts silently missing from the
// picker, with no signal that anything was left out. See discoveryCustomerIDs.
//
// OnlyParentAccounts is sent false so accounts LINKED to the customer being queried are
// included. Sending true would narrow each query to that customer's own accounts and, for
// an agency-style setup, answer "no accounts" about accounts the credentials manage. It
// is not a substitute for the per-customer loop: a linked account is one attached to THIS
// customer, not one belonging to a second customer the same user administers.
//
// Duplicates are dropped by account id, first occurrence wins. The same account can be
// reachable under more than one customer — that is precisely what a link is — and a
// picker offering it twice invites a user to wonder which one is real.
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
	customers, err := c.discoveryCustomerIDs(ctx)
	if err != nil {
		return nil, err
	}

	// Non-nil for the same reason the per-customer slice below is: zero accounts is an
	// ANSWER and has to stay distinguishable from "no answer", including on the wire
	// where nil serializes as null.
	accounts := make([]AdAccount, 0)
	seen := make(map[string]int) // map from account ID to index in accounts slice
	for _, customer := range customers {
		batch, berr := c.accountsInfoForCustomer(ctx, customer.id, int(customer.roleID))
		if berr != nil {
			// One customer failing fails the whole call. A partial union is the failure
			// mode this function exists to remove: it is indistinguishable from a
			// complete one at the boundary, and the caller acts on the absence.
			return nil, berr
		}
		for _, a := range batch {
			if idx, dup := seen[a.ID]; dup {
				// Keep the account with the stronger role. If this account's role is stronger,
				// replace the existing one; otherwise keep the existing.
				if roleIsStrong(int64(a.RoleID)) && !roleIsStrong(int64(accounts[idx].RoleID)) {
					accounts[idx] = a
				}
				continue
			}
			seen[a.ID] = len(accounts)
			accounts = append(accounts, a)
		}
	}
	return accounts, nil
}

// accountsInfoForCustomer enumerates the accounts reachable under one customer.
// roleID is the Microsoft role id for the role used to discover this customer (0 for unknown).
func (c *Client) accountsInfoForCustomer(ctx context.Context, customerID string, roleID int) ([]AdAccount, error) {
	// CustomerId is always sent here: discoveryCustomerIDs resolved a concrete customer,
	// either the configured one or one of the credentials' own roles. It is kept as the
	// string it was stored or decoded as rather than parsed to a number, so there is one
	// representation and it cannot disagree with the header doCustomerRequest's caller
	// sets from the configured field.
	req := map[string]any{
		"OnlyParentAccounts": false,
		"CustomerId":         json.Number(customerID),
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
		// numberID, the same identity check the create path applies to a returned id, and
		// for the same reason: this id is the whole answer. An account offered to the
		// picker gets bound to a connection and spends money, so it has to be a
		// Microsoft id and not merely a digit string. accountIDRE (`^[0-9]+$`) is the
		// TRANSPORT check — is this safe in a header — and it admits "0" and values past
		// MaxInt64, neither of which can name an account. numberID enforces positivity
		// and int64 range on top, and everything it accepts accountIDRE accepts too, so
		// an offered account is still one the client will bind without complaint.
		id := numberID(&ai.ID)
		if id == "" {
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
			RoleID:      roleID,
		})
	}
	return accounts, nil
}
