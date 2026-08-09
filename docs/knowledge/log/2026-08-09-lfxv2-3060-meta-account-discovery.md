# 2026-08-09 — Enumerating Meta ad accounts: fail rather than truncate

**Update** — `internal/platform/meta/accounts.go` (LFXV2-3060), the client half of Meta
ad-account discovery. It walks `/me/adaccounts` and returns every account the access token
can reach. The dispatcher adapter and the endpoint that expose it land separately
(LFXV2-3062); this entry is about the walk itself.

## It asks about the TOKEN, not about an account

The path is `/me/adaccounts` and the client's `AccountConfig` is not consulted at all. That
is the whole point: a connection being re-pointed at a different account has to be able to
ask which accounts exist, and a request path that required the account id could only ever be
called after the question it answers had been answered. `newAccountsClient` in the test
builds a client with a deliberately ZERO `AccountConfig` so a future edit that starts reading
it fails here rather than in production.

## Fail, do not truncate

Every mode in which the walk cannot be completed returns `nil, error` rather than what was
collected so far:

- a 2xx body with no `data` field
- a `paging.next` link with no `paging.cursors.after`
- a repeated cursor (a non-terminating walk)
- an entry whose id is not `act_<digits>`
- the page cap (`adAccountPageSize` × `adAccountMaxPages`)

A short account list is indistinguishable from a complete one at the boundary, and the caller
acts on the ABSENCE — the account they wanted is simply not offered, and they conclude their
token cannot reach it. An error is the better outcome, because it is the only one that can be
told apart from the truth.

The id check reuses `accountIDRE` — the very regexp `AccountConfig.AccountID` is validated
against — rather than restating the `act_<digits>` pattern. An account this walk offers has to
be one the client will later accept, and a second copy of that contract could drift into
offering ids that fail at bind time.

The unusable-id case fails the WHOLE walk rather than skipping the row. A response shape that
far from the documented one means it is not the response we think it is, and the rest of it
is not trustworthy either.

## Nil vs. empty, and `make(..., 0, n)`

The `data` guard leans on a property of `encoding/json` worth stating precisely, because it
is easy to get backwards: a PRESENT empty array (`{"data":[]}`) is decoded into a **non-nil**
empty slice, while an absent or null field leaves the slice **nil**. A plain slice therefore
already separates the two, provided it starts nil — which is why the response struct is
declared inside the page loop rather than reused across iterations. `Data == nil` then means
"this body did not contain a result set", and a malformed `{}` cannot read as "fully
enumerated, zero accounts".

(An earlier version of this code used a `*[]struct` for that distinction and said a plain
slice could not make it. That was simply wrong about the decoder, and the pointer bought
nothing over the nil check.)

The returned slice is `make([]AdAccount, 0, adAccountPageSize)` rather than a nil slice, for
the same reason one layer up: a token that legitimately reaches zero ad accounts is an EMPTY
list, and everything above this needs empty to stay distinguishable from "no answer". It also
means the value serializes as `[]` rather than `null` once it reaches the wire.

## `paging.next` is never followed

Meta's absolute `next` URL carries `access_token` — and `appsecret_proof` — as query
parameters. Following it would put both into the request URL, which `apiError` and
`transportError` copy into their messages, which the discovery handler logs. The path is
rebuilt from the opaque `after` cursor instead. Same reasoning as `listAdIDs`, and the test
`_NeverPutsTheTokenInAURL` captures every request path and asserts it.

## Known-bad accounts are RETURNED, with their reason

Disabled, unsettled and closed accounts are not filtered out. This feeds a picker: a user
whose only account is unsettled needs to see it and see why, and dropping it answers "your
token reaches no ad accounts" about an account sitting right there — sending them to look for
a permissions problem that does not exist.

`AdAccount.StatusLabel` reads the same `inactiveAccountStatusLabels` map `CreateCampaign`'s
preflight uses, so the picker and the create path cannot disagree about which accounts are
known-bad. The decision to REFUSE a campaign on such an account stays where it already is, in
that preflight. `Status == 0` means the field was absent — Meta omits it for accounts it will
not report on — and is explicitly not a claim that the account is disabled.
