# 2026-09-28 — LFXV2-2665: flat mode read a manager-only resource to check a direct account

**Fix** — round 19 of review, raised by the general reviewer against the whole branch. A
behaviour change on the leg added earlier in this same branch.

Flat mode — no `login_customer_id`, so no manager in the picture — confirmed a configured
account in `customers:listAccessibleCustomers` and then read the account's manager flag and
status from `customer_client`, narrowed by `WHERE customer_client.id = <id>`. Google documents
`CustomerClient` as a link resource that exists for MANAGER customers. The ordinary direct
account flat mode exists to serve is exactly the case that documentation does not cover, so the
read rested on behaviour the contract does not promise.

The failure is quiet and it points the wrong way. If that query returns no row, `selfReach`
answers `errSelfRowMissing`, which matches no probe predicate by name and so lands on
`ProbeInconclusive`'s default — the correct classification for "reached, properties unknown",
and precisely the wrong OUTCOME here, because nothing is unknown: the account is fine. A
working direct-mode connection tests amber every time, on the endpoint whose purpose is telling
an operator whether their connection works.

The repair is to read the resource whose existence is guaranteed. `SELECT customer.id,
customer.manager, customer.status FROM customer`, scoped to the configured customer, returns
that customer's own record for managers and non-managers alike and carries the same two
properties under the same names. Being a manager is still detected — `customer.manager` is on
the record whether or not a hierarchy hangs beneath it, which is the case the second leg was
added for. The query needs no `WHERE` and no interpolated id, since `FROM customer` is already
scoped to the customer the search runs under; the id is still matched on the way out, because a
row about some other customer answers a question nobody asked.

Narrowing the old query further, or treating its empty result as reachable, would both have been
worse. The first keeps the bet and only moves it; the second converts an unpromised read into a
manufactured success — and a false green on this endpoint is how a manager account configured as
`account_id` reaches the first create, which is the production failure the leg was added to
catch.

The decode is its own type, `selfCustomerRow`, not a widened `customerClientRow`. The two
resources nest under different JSON keys, and decoding a `customer` row through the
`customerClient` struct produces a zero value — id empty, manager false, status `""` — which
reads as a reached, non-manager, NOT-ENABLED account. That is a confirmed operator-facing
verdict conjured out of a field name that did not match, and it is why the test helpers
`clientRow` and `customerRow` are kept apart as well.

`TestProbeAccountReach_FlatMode` was confirmed to fail against the pre-fix read before the fix
was kept: all three of its cases returned `the account did not return its own customer record`,
reproducing the amber-on-a-working-connection symptom directly. Its query assertion now also
fails if the implementation drifts back to `customer_client`.
