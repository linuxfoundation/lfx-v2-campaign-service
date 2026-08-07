# 2026-08-07 — Reading the account list a credential can actually reach

`Client.ListAccessibleCustomers` is the Google Ads client's first call that runs **without a
configured customer id**, because it is how a caller LEARNS one. `doRequest` validates
`c.account.CustomerID` as digits-only before it builds anything, which is right for every other
call and exactly wrong for this one, so the account-agnostic paths go through
`doRequestValidated` — the same function with that single precondition discharged by the caller.
Splitting it that way rather than adding a bypass flag keeps one copy of the URL construction,
header set, body bounding, retry gating, and `apiError`/`transportError` classification.
`validateLoginCustomerID` still runs: the `login-customer-id` header is still attached and still
has to be well-formed.

`customers:listAccessibleCustomers` returns only what the authenticated user can act on
DIRECTLY, and a `login-customer-id` header does not change that — it is a property of the
endpoint. **On an MCC credential the flat list is usually the manager itself and every child ad
account is missing**, so a configured manager id is expanded with a `customer_client` GAQL query
via `gaqlSearchForCustomer`, which takes an explicit customer id (validated where it is
interpolated into the path) instead of the client's configured one.

Both halves of the merged list need a manager filter, for different reasons. The expansion has
`customer_client.manager` and drops on it. The flat list has no such field — a manager and an ad
account are indistinguishable there by resource name — so the one manager identifiable without
extra metadata, the configured `login_customer_id`, is dropped by id. Any other manager in the
flat list survives; there is nothing to recognise it by, and a per-row round-trip would cost
more than it saves on a list this short. Offering a manager as a choice is not cosmetic: it
produces a connection that fails at the first campaign create, far from where the choice was
made.

The expansion is also the only source of `descriptive_name`, so dedup by resource name prefers
the labelled copy. A row with no id is a hard error, not a silent drop — dropping it would
understate the list the operator picks from.

This PR is the platform client only. The dispatch adapter and the HTTP endpoint that expose it
land separately.
