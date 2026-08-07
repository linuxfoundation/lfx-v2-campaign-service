# 2026-08-07 — LFXV2-2023: manager-mode discovery no longer fetches a list it discards

**Update** — `ListAccessibleCustomers` issued `customers:listAccessibleCustomers` on every
call, then, when a `login_customer_id` was configured, discarded the result entirely and
answered from `listManagerClients`. Two Google Ads round-trips where one was needed.

**Fix** — The mode branch moved ABOVE the flat request: with a manager configured, a new
`listManagerChildren` returns straight from the hierarchy query and the flat endpoint is never
called. The ordering is the whole fix.

**Why it was not merely wasteful** — The discarded call still spent request quota and the
caller's shared 20-second discovery budget, and its own timeout, 429, or 5xx propagated out of
`ListAccessibleCustomers` and failed discovery outright. A call whose success is not needed
was able to cause a failure. On an agency-managed connection — the case the expansion exists
for — that is the common path, not an edge one.

**How it hid** — Every existing manager-mode test served both endpoints, so all of them passed
whether the flat list was fetched-then-discarded or never fetched at all. The suite could not
see the extra request.
`TestListAccessibleCustomers_ManagerModeNeverCallsTheFlatList` closes that: it fails the flat
endpoint with a 500 and asserts both that the hit count is zero and that discovery still
succeeds. Verified binding by reverting the branch — both diagnostics fire.

**Note** — An earlier comment justified validating flat-list resource names "in BOTH modes
regardless of which branch consumes them". That reads as defensive rigour, but validating rows
nobody will consume adds no safety and one more way to fail; it was the same premise that made
the wasted call look acceptable. Manager-mode ids are validated in `listManagerClients`, where
they are actually used.
