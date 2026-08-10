# 2026-08-09 — LinkedIn ad-account discovery: two health axes, and what not to restate

**Update** — `internal/platform/linkedin/accounts.go` (LFXV2-3063), the client half of
LinkedIn ad-account discovery. It walks `GET /adAccounts?q=search` and returns every account
the access token can reach. The dispatcher adapter and the endpoint that expose it land
separately; this entry is about the walk itself. It is the LinkedIn counterpart to the Meta
walk (LFXV2-3060) and follows the same fail-not-truncate discipline.

## The contract was read, not remembered

Every field name here comes from LinkedIn's published "Create and Manage Ad Accounts" page
(`li-lms-2026-07`), not from recall: `q=search` with the criteria omitted returns all accounts
the caller can access; pagination is cursor-based (`pageSize` up to 1000, `pageToken`, next
cursor in `metadata.nextPageToken`) and replaced the index-based form in version 202401; and
`id` comes back as a bare JSON **number**, which is why the existing `flexibleID` decoder
matters. Reddit's metrics client (LFXV2-2995) is the standing reminder of what an unverified
contract costs — it shipped default-off because its shape was inferred rather than read.

## Two things already existed, and restating either would have been the bug

`doRequest` already sets `LinkedIn-Version` and `X-RestLi-Protocol-Version: 2.0.0` on every
call, and already fails any GET whose `elements` field is absent or null. Both were on the
list of guards to add here before reading the transport. Adding them locally would have been
precisely the duplicated-contract defect Copilot had just flagged on the Meta PR — two copies
of one rule, free to drift.

Same reasoning drove the id check: it reuses `accountIDRE` from `targeting.go`, the very
regexp a configured account id is validated against, rather than restating `^[0-9]+$`. An
account this walk offers has to be one the client will later accept.

## Lifecycle and serving are separate questions

An ad account carries two independent health signals, and the tempting simplification is to
fold them into one "is this account OK" verdict. That would be wrong in a way a user would
feel: an account whose `status` is ACTIVE but whose `servingStatuses` is `["BILLING_HOLD"]` is
perfectly bindable and will not spend a cent. Collapsing the axes either hides it from the
picker or promises it can serve. So `Active()`/`StatusLabel()` speak to the lifecycle and
`Servable()`/`ServingHolds()` speak to serving, and the picker can say "usable, but it will
not deliver until billing is resolved".

`Servable()` is an ALLOW-LIST (exactly `["RUNNABLE"]`), not an exclusion. An omitted or
unrecognized serving status is not evidence that an account can spend, and the honest answer
is "not confirmed servable" — while `ServingHolds()` stays empty, which is how a caller
distinguishes *held* from *unknown*. An absent lifecycle `status` is treated the same way:
not `Active()`, and no label. Absence is not a claim in either direction.

## Known-bad accounts are RETURNED, with their reason

Canceled, draft, held and `test` accounts all come back, each carrying why it is unusable.
Test accounts are the interesting case: one never serves and never bills, so binding a real
campaign to it produces a campaign that silently does nothing — but a developer wiring up an
integration is looking for exactly that account. Filtering it would answer "your token reaches
no ad accounts" about an account sitting right there, sending someone to hunt a permissions
problem that does not exist.

## Fail, do not truncate

A repeated page cursor and the page cap both return `nil, error` rather than the accounts
collected so far. A short account list is indistinguishable from a complete one at the
boundary, and the caller acts on the ABSENCE.

The cap is `adAccountMaxPages = 20`, not the package's existing `maxListPages = 1000`. That
constant is sized so a find-by-name survives a server-side filter the API may ignore; discovery
sends no filter, so there is nothing to be ignored, and a 20-page walk that has not terminated
means something is wrong rather than that the collection is large.

## Test notes

`newAccountsClient` builds a client with a deliberately ZERO `RuntimeConfig` — no default
account, no `Accounts` — so a future edit that starts consulting a configured account fails
here rather than in production, where a credentials-only connection has nothing to consult.
The request-URI recorder is mutex-guarded and hands the page index back from `add()` under the
same lock: the handler runs on the server's goroutine while assertions run on the test
goroutine, and a TCP socket is not a happens-before edge the race detector can see.

## Round 2: the comment about the decoder was wrong about the decoder

Copilot, on the `Elements *[]responseElement` comment: `encoding/json` does not leave a present
`null` slice untouched — it explicitly SETS the slice (or pointer) to nil. The comment's whole
job is to document that distinction, so being wrong about the mechanism there is worse than
being wrong about it anywhere else in the file.

Verified rather than assumed, because the two cases genuinely differ in the general case:
decoding `{}` over a slice preset to `[]string{"x"}` leaves `["x"]` in place, while decoding
`{"elements":null}` over the same value produces nil. Absent and null agree HERE only because
the struct is declared fresh per response, so "untouched" is already nil — which is a property
of the call sites, not of the decoder.

The conclusion the comment reaches is unaffected: the pointer still is not what makes `{}`
distinguishable from `[]`, and it is still there to stop a future `len(x) == 0` from merging
them. Only the reason given for the absent/null pair changed.

Worth stating generally: **"the outcome is the same" is not the same claim as "the decoder does
the same thing", and a comment that exists to explain a decoder has to make the second one.**

## Round N+1: the page size was a correctness constant, and absence had no representation

Two findings, and the interesting thing is that both are cases where a value that LOOKS like a
tuning choice is actually part of the contract.

`adAccountPageSize` was 100 against a documented maximum of 1000. That is not "more round
trips": the runaway guard bounds the walk in PAGES, so the page size sets the account ceiling
with it, and at 100 the walk refused any token reaching 2,001 accounts — a legitimate result
turned into an error by a number chosen for politeness. The neighbouring `findByName` already
requested 1000 for exactly this reason, which is the tell: the same file disagreed with itself.
The regression test asserts the constant rather than the behaviour, because the failure it
guards against only shows up on a token nobody has in a test.

The second is subtler. `metadata` was a VALUE-typed struct, so a response with `elements` and no
cursor envelope decoded to a zero-value `nextPageToken` — indistinguishable from the server
saying "that was the last page". Every other guard in this walk exists to stop a truncated
enumeration being reported as a complete one, and this path walked straight past all of them.
Absence cannot be rejected until it is representable, so the fix is the pointer, and the
rejection follows from it.

That pointer is shared with two older walks in `client.go`, which the same reasoning condemns.
They are deliberately left as they were, with the nil case now written out explicitly and a
comment naming the exposure: closing them means asserting a `metadata` block in roughly fifty
existing fixtures, which is a change with its own review surface and does not belong in a review
round on ad-account discovery. Tracked as LFXV2-3066.

The generalisable part: **a zero value that is also a meaningful value erases the difference
between "the server said nothing" and "the server said none".** Any decoded field whose zero
value terminates a loop should be a pointer, and the question to ask of a new response struct is
which of its fields the server is allowed to omit.
