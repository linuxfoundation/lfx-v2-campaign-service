# 2026-08-07 — Reading the account list a credential can actually reach

**Update** — New capability on PR #83 (`internal/platform/googleads/client.go`,
`accounts_test.go`, and `docs/knowledge/code/internal-platform-googleads.md`).

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

The first cut MERGED the two sources and filtered managers out of each half separately: the
expansion has `customer_client.manager` and drops on it, while the flat list has no such
field — a manager and an ad account are indistinguishable there by resource name — so only
the configured `login_customer_id` could be dropped, by id. Offering a manager as a choice is
not cosmetic: it produces a connection that fails at the first campaign create, far from
where the choice was made.

**The merge did not survive review** — see the review pass below, which replaced it with two
modes. Read that section, not this paragraph, for the shipped behaviour; what carries over is
the reasoning about what each source can and cannot tell you. The expansion remains the only
source of `descriptive_name`, and a `customer_client` row with no id is still a hard error
rather than a silent drop, which would understate the list the operator picks from.

This PR is the platform client only. The dispatch adapter and the HTTP endpoint that expose it
land separately.

## Review pass — the flat list is not usable in manager mode

The first cut treated `listAccessibleCustomers` and the manager expansion as two sources
to merge, dropping only the configured manager from the flat half. Review pointed out that
the merge itself is wrong.

`listAccessibleCustomers` is unscoped: the `login-customer-id` header does not filter it.
Every other request this client makes does carry that header. So the flat list can contain
an account the user genuinely reaches — through some *other* manager, or directly — that
this client cannot address at all. Returning it puts an unusable option in the account
picker, and the failure does not surface at selection time. It surfaces at first dispatch,
as `PERMISSION_DENIED`, long after the connection was saved, where it reads as a broken
credential rather than a wrong account.

Manager mode now returns exactly the manager's ENABLED, non-manager children. That set is
also better shaped for every other reason: `customer_client` carries `descriptive_name`, so
the accounts are labelled, and the manager rows the flat list cannot identify are already
filtered out by the query. Nothing addressable is lost — an account under the configured
manager appears in the expansion too — so the merge was only ever adding options that could
not work.

The flat list is still fetched and still validated in manager mode. A resource name that is
not `customers/{digits}` means the 2xx did not match the documented contract, and that is
worth failing on wherever it is noticed, regardless of which branch consumes the rows.

`TestListAccessibleCustomers_ManagerModeExcludesAccountsOutsideTheHierarchy` puts three
things in the flat list — the configured manager, a child inside the hierarchy, and an
outsider — and asserts only the child survives. Restoring the merge fails it, along with
`ManagerModeDedupsRepeatedChildren`, which covers the one dedup that still matters:
`customer_client` reports a client once per path through the hierarchy, so a client of a
sub-manager that is itself a client of the root would otherwise appear in the picker twice.
