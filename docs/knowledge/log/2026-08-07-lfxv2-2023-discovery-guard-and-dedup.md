# 2026-08-07 — LFXV2-2023: the discovery guard had no test, and the dedup claimed an invariant GAQL does not provide

**Update** — Four suppressed Copilot findings closed on PR #83.
`TestGaqlSearchForCustomer_RejectsMalformedCustomerID` now covers the explicit-customer path's
own validation; the manager-hierarchy dedup no longer rests on a false ordering claim; the
`validateLoginCustomerID` comment names the right request layer; and account discovery is in the
concept description and `index.md`.

**Fix** — The customer-id guard in `gaqlSearchForCustomer` was untested.
`TestDoRequest_RejectsMalformedCustomerID` only exercises `doRequest`, and
`gaqlSearchForCustomer` deliberately bypasses it — it calls `doRequestValidated` so
`validateAccountIDs` does not reject the empty `c.account.CustomerID` that discovery depends on.
That bypass also skips the check that test covers, so deleting the guard would have left the
existing security test green while `123/456` was concatenated straight into
`customers/<id>/googleAds:search` and silently addressed a different resource. The new table test
starts no server — that is the assertion — and checks for the ABSENCE of the `gaql search` prefix
every post-validation failure carries, so "rejected before anything was sent" is asserted rather
than assumed. Removing the guard fails all six ids with exactly that diagnostic.

**Fix** — The hierarchy dedup kept the first occurrence of a duplicate client and explained it as
"the first, which is the shallowest". The query selects no `customer_client.level` and carries no
`ORDER BY`, so GAQL promises nothing about which path comes back first; the comment invented an
invariant and would have been a trap for anyone who later relied on depth. Keeping the first is
still correct, for a different reason: every duplicate describes the same customer, so id and
descriptive_name belong to the customer rather than to the path taken to reach it. The one
asymmetry is now handled instead of assumed away — a later duplicate carrying a
`descriptive_name` the first lacked fills it in, which makes the result independent of arrival
order rather than merely tolerant of it.

**Fix** — `validateLoginCustomerID`'s doc said the header "is still attached by doRequest".
It is attached in `doRequestValidated`, alongside developer-token, and `doRequest` is precisely
the layer discovery skips. The comment exists to document WHICH precondition is bypassed, so
naming the wrong layer defeated its only purpose.

**Fix** — The package gained account discovery, but the concept's frontmatter `description` and
its mirrored `index.md` entry still enumerated the package without it. `CLAUDE.md:19-23` requires
the description and the containing index to move together when a concept is re-described; both
now name `customers:listAccessibleCustomers`, the manager-hierarchy expansion via
`customer_client`, and the account-agnostic request path that validates only the manager id.
