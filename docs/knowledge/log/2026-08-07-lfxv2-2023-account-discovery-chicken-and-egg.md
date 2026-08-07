# 2026-08-07 — Account discovery could not run before an account was chosen (LFXV2-2023)

**Update** — Google Ads account discovery no longer requires a customer id to discover
customer ids, and an MCC connection now returns its child ad accounts instead of only the
manager. Empty results serialize as `[]`, not `null`.

Three findings from Copilot's suppressed review bodies on #82. As on #73, none of them
existed as a review thread — the unresolved count read zero throughout.

**The endpoint was unreachable in the state it exists to serve.** `ListAccounts` resolved
its client through `resolveGoogleAdsClient` → `validateGoogleAdsConnection`, which errors on
an empty account id; and `Client.doRequest` independently requires a digits-only
`c.account.CustomerID`. A connection is created with credentials first and an account chosen
afterwards, from the list this very call produces, so both preconditions demanded that the
caller already know the answer to the question they were asking. Neither check was deleted.
`validateGoogleAdsCredentials` keeps active status, blob decoding, and all four OAuth fields,
so a discovery call against a stale connection still reads as a connection problem rather
than an opaque Google error; `doRequestValidated` is `doRequest` with the id precondition
discharged by the caller, so the account-agnostic paths still share one copy of the URL
construction, header set, body bounding, retry gating, and error classification, and
`login-customer-id` is still validated.

**A manager credential saw almost nothing.** `customers:listAccessibleCustomers` returns the
accounts the authenticated user can act on DIRECTLY. A `login-customer-id` header does not
make it walk a hierarchy — that is a property of the endpoint, not of the header — so on an
agency-managed MCC connection the flat list is typically the manager alone and every child ad
account is absent. `listManagerClients` now expands a configured manager with a
`customer_client` query scoped to it (`gaqlSearchForCustomer`, which takes an explicit
customer id because the client's own is empty here). Manager rows are dropped: they cannot
hold campaigns, so offering one would let a caller pick an account that fails at the first
create. The expansion is also the only source of `descriptive_name` — the flat endpoint does
not return labels at all.

**An empty result must be `[]`, not `null`.** The dispatcher deliberately builds its slice
with `make(..., 0, n)` so a credential that legitimately reaches zero accounts is an empty
list rather than a contract violation, but the service layer's conversion loop used a `var`
declaration and undid that one layer up. The test that was supposed to cover this asserted
only `len(...) != 0`, which nil satisfies; it now asserts non-nil and marshals to `[]`.

Each fix was checked by reverting it. Reverting the customer-id relaxation fails with
`discovery must not require a customer id, got: invalid Google Ads customer id ""`;
reverting the manager expansion fails with `customer_client search ran 0 times, want exactly
1`; reverting the slice fix fails with `Accounts is nil; an empty result must serialize as
[], not null`.
