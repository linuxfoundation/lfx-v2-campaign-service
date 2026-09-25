# 2026-09-26 — LFXV2-2665: the status gates the body, and the Microsoft probe tests dispatch's own scope

**Fix** — the second local-review round on `19159945`. Four findings, all of them the same class
the A1 connection-test work exists to remove: a connection reported as something it was not. A
fifth finding was examined and declined; it is recorded at the bottom because the reasoning is
worth keeping.

## 1. `classifyTokenRefusal` read the body before the status

The classifier landed in round 1 parsed the OAuth envelope first and consulted the status only as
a fallback. So a `302`, `404` or `405` carrying `{"error":"invalid_grant"}` returned
`ErrCredentialRejected` — flatly contradicting the godoc written alongside it, which promises that
a status no token endpoint answers a well-formed refresh with is reclassified on the status alone.

An OAuth-shaped body genuinely can arrive with such a status: from a proxy, a gateway error page,
or whatever now answers at a moved address. Reading it first reported a credential the platform
never evaluated as refused, which is exactly the misclassification the function exists to remove.

The **status now gates the body**. Only `400`, `401` and `403` — the three statuses a token
endpoint actually uses to refuse — reach the envelope parse; everything else returns
`ErrTokenRequestRejected` without looking. The conservative fallback inside those three statuses
is unchanged: an unrecognised body still stays a credential verdict.

Three rows added to each of the three `TestClassifyTokenRefusal` tables, one per misleading
status.

## 2. An unusable body erased the status in `googleads` and `microsoft`

Both `fetchToken`s returned a bare read-or-size error ahead of the classification. That error
matches neither probe predicate, so `ProbeInconclusive`'s unrecognised-error default answered
`true` and a plain `401` with a truncated or oversized body reported the connection as `OK: true` —
the false positive this ticket exists to remove, arriving through the one door left open. Reddit
already did this correctly.

The status is now kept and classified with a **nil** body, which carries no allowlisted code and
so takes the conservative fallback. The read error is still returned for a `2xx`, where the body IS
the answer and there is no status to fall back on. `TestFetchToken_UnusableBodyKeepsTheStatusVerdict`
in both packages serves `maxResponseBytes+1` bytes on a `401` and asserts the credential verdict.

## 3. The Microsoft probe enumerated every customer, not the configured one

`cachedMicrosoftClient` builds the dispatch client with the stored `customer_id` and sends it as
the `CustomerId` header on every request. The probe passed a zero `AccountConfig`, which makes
`discoveryCustomerIDs` walk every `CustomerRole` the credential holds — so a connection whose
`customer_id` was stale or wrong passed its test whenever the account was reachable under some
OTHER customer, then failed at campaign creation under the customer actually stored. `customer_id`
is operator-settable through the connection config API, so that is reachable, not theoretical.

This is **not** the narrowing the Google Ads probe refuses, and the difference is whose filter it
is. There the filtered walk was the account PICKER's and had nothing to do with dispatch, so
absence from it proved nothing. Here the narrowing IS dispatch's, so absence is the true statement
"not reachable as this connection is configured". With no customer configured the zero config is
still right, for the reason `ListAccounts` documents at length.

`internal/dispatch/microsoft_probe_customer_scope_test.go` pins both halves against a server whose
credential reaches two customers with the configured account under only one of them. Proved
non-vacuous: reverted to `AccountConfig{}` and the first test failed with the production defect
stated verbatim. `TestMicrosoftListAccountsDoesNotScopeToTheStoredCustomer` confirms the picker
was not dragged along.

## 4. The Goa contract still described the old meaning of `ok`

`ok` now answers "is this connection usable as configured", which is broader than "did the
credential authenticate" and deliberately so: an authenticated credential still fails the test
when the connection names no ad account, names one the platform cannot reach as configured, or
names a Google Ads manager or not-enabled account. The generated contract still said `ok` meant
authentication alone, so a client could read an account-selection failure as a reason to rotate a
working credential. `design/connection.go` now states both that and the inconclusive case — `ok` is
also `true` when nothing was learned — and the Goa artifacts were regenerated; the diff is
description-only.

## Declined: inconclusive → `OK: true` at `internal/service/connection.go:414`

Reported as a Critical. It is the documented product design, stated in `docs/api-catalog.md`: a
failure that proves nothing about the credential is reported as `OK: true` with an advisory rather
than as a failed test. Changing it is a product decision about the endpoint's contract, not a
regression introduced here, and it predates this change. Recorded, not changed — and item 4 above
makes the behaviour explicit in the public contract instead of leaving it implied.

Refs: LFXV2-2665
