# 2026-08-07 — LFXV2-2023: the discovery reader trusted two upstream strings it should not have

**Update** — Closed five suppressed Copilot findings on PR #83
(`internal/platform/googleads/client.go` and `accounts_test.go`).

`AccessibleCustomer.ResourceName` is not a display string. A caller picks one entry, and
that value is persisted as the connection's `account_id` and interpolated into every later
request path. So the type's promise — `customers/{digits}` — is a security boundary, and
both of its sources were producing it on trust.

- **The flat list had no validation at all.** Every `resourceNames` entry from
  `customers:listAccessibleCustomers` was wrapped in an `AccessibleCustomer` verbatim. A
  malformed 2xx could therefore hand back `""`, a bare id, `customerClients/999`, or
  `customers/999/campaigns/1` as a selectable account. `accessibleResourceNameRE` now pins
  the exact two-segment shape, anchored on both ends, and a mismatch is a
  `transportError` — malformed response, not rejected request, which is the distinction the
  dispatcher maps to 503-with-retry.
- **The hierarchy expansion checked only for emptiness.** `listManagerClients` rejected a
  row with no id but accepted any non-empty one, then concatenated it into
  `"customers/" + id`. `1/other` therefore FORGES a resource name pointing at a different
  account than the row describes; `123-456-7890` (the dashed form Google's own UI shows) is
  the innocent version of the same defect. `customerIDRE` now gates it.

The emptiness check is the instructive part: it was written deliberately, with a comment
explaining why a missing id must fail rather than be dropped — and it was still the wrong
predicate. Guarding the case you thought of is not the same as guarding the field.

**Two comments asserted things the code does not do.** One said this call "goes through
`doRequest` like every other call" when it deliberately calls `doRequestValidated` to bypass
the digits-only `CustomerID` precondition — the very bypass that makes account discovery
reachable. The other said the flat list is scoped by `login-customer-id`, contradicting the
method's own doc ten lines below, which correctly states the header does not scope or expand
it. That contradiction matters: the whole reason `listManagerClients` exists is that the
header will NOT enumerate children, so a reader who believed the response comment would
conclude the expansion is redundant and delete it.

**The dedup merge was quadratic** — and then it was deleted. `seen` was a set, so relabelling
a duplicate meant a full linear scan of `accounts`; the fix at the time was a resource-name →
index map. A later round in the same branch removed the merge entirely: manager mode now
answers from the expansion alone (see the review pass in
[the accessible-customers-client entry](2026-08-07-lfxv2-2023-accessible-customers-client.md)),
so there is no cross-source relabelling left to be quadratic. What survives is a plain
`map[string]struct{}` deduplicating the expansion's own repeated children, which
`customer_client` can report more than once when the hierarchy has several paths to the same
account.

Both new guards revert-verified individually. Reverting the resource-name check fails all
five subtests; reverting the id check to the old emptiness-only form fails exactly the two
non-numeric subtests and leaves the absent-id one green, which is the point.
