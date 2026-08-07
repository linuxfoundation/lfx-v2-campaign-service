# 2026-08-07 — LFXV2-2023: two account-discovery tests passed against nothing

**Fix** — `listManagerClients` filters server-side with
`WHERE customer_client.status = 'ENABLED'`, and that predicate is the ONLY thing keeping
cancelled or closed accounts out of the picker — no later stage re-checks
`customerClient.status`. Every manager fixture in `accounts_test.go` returns pre-filtered ENABLED
rows and none decoded the request body, so deleting the WHERE clause broke no test.
`TestListAccessibleCustomers_ExpandsManagerHierarchy` now captures the GAQL the client actually
sent and asserts the predicate. The revert confirms it: with the clause removed the test fails
naming the exact query.

**Fix** — `TestListAccessibleCustomers_APIErrorCarriesStatusAndCodes` promised parsed Google Ads
error codes in its name and its doc comment, and asserted none. Worse, its fixture could not have
produced any: `parseErrorCodes` skips any detail whose `@type` does not end in `GoogleAdsFailure`,
and the fixture carried no `@type` at all, so `ErrorCodes` was reliably empty and the test was
green. The fixture now carries the real type URL and the test asserts the code through
`hasErrorCode` — the accessor every classification site actually uses, so a slice carrying the
wrong value cannot satisfy it either.

Both are the same shape: a fixture that answers identically regardless of what the code under test
asked for. The rule that catches it is mechanical — revert the fix and confirm the test fails with
the right diagnostic — and it was skipped for these two because they were written alongside code
that was already correct.
