# 2026-09-26 — LFXV2-2665: flat mode tested a manager account green

**Fix** — Round 14 of the connection-probe work. One finding, taken.

## The endpoint reproduced the failure it exists to catch

`ProbeAccountReach` answers in two modes. Manager mode walks `customer_client` beneath the
configured `login_customer_id` unfiltered, so it can say whether the account is a manager, not
enabled, or fine. Flat mode — no `login_customer_id` — had only
`customers:listAccessibleCustomers`, which is unfiltered but carries neither the manager flag nor
the status, so it answered on MEMBERSHIP alone.

Membership alone is a false success. A manager (MCC) account appears in that list and cannot hold
a campaign. So a flat-mode connection naming a manager as its `account_id` tested `ok: true` and
then failed at the first create — which is, precisely, the production failure this endpoint was
built to catch, produced by the endpoint meant to catch it.

The code documented the narrowing ("the answer is narrower, not wrong") and that reading was
wrong: an answer that says a broken connection is healthy is not a narrower answer.

## A self-scoped read, not a hierarchy walk

Presence in the enumeration is now followed by `selfReach`: a `customer_client` read scoped to
the configured customer and narrowed to its own row by id. Two properties of GAQL make this work
without a manager, which is the point, since flat mode is the mode with no manager —
`customer_client` queried under a customer includes that customer's own row; and asking for the
single row by id rather than reading the table is what stops the call enumerating an entire
hierarchy in exactly the case it exists to detect, when the configured account IS a manager.

It reuses `queryCustomerClients`, so the id validation and the row-shape guard on the decode
come along rather than being written a second time, and the manager/status reading is the same
switch manager mode already applies.

## The failure direction was the part worth getting right

A failure on the second leg stays an error. Folding it into `AccountReachable` would be a success
nothing established; folding it into `AccountUnreachable` would be a confirmed operator-facing
verdict contradicting the enumeration that had just named the account. Both are the shapes this
whole branch exists to remove. The error reaches `probeClass` instead, where
`ProbeInconclusive`'s default for an unrecognised error of this package classifies it
inconclusive — "reached, properties unknown", which is what actually happened.

`errSelfRowMissing` is deliberately a plain package error for that reason: it needs to match
neither predicate by name and let that default do the work.

`TestProbeAccountReach_FlatMode` and `TestProbeAccountReach_ManagerMode` are the first direct
tests this method has had — it was covered only through the dispatch layer's fake, which is why
three rounds of review passed over a mode that answered from membership alone. Five of the flat
subtests were confirmed to fail against the previous implementation.
