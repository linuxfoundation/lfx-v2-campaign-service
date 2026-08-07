# 2026-08-06 — Meta by-name lookups: gate the matched id, and state which retries reach them

**Update** — Copilot found that `findCampaignByName` and `findAdSetByName` checked only that
the matched id was non-empty before returning it, while the caller splices it straight into
`/{campaignID}/adsets` and `/{adSetID}/ads`. Every other id-interpolating path in this client
(`UpdateCampaignStatus`, `createAdSet`, the ad-discovery loop) already gates on `numericIDRE`
for exactly that reason; these two were the gap.

Both now apply the same gate. The classification is the load-bearing part: a rejected id is
`errLookupAmbiguous`, not a clean failure — a resource with this name DOES exist upstream and
we simply cannot address it, so reporting "absent" would let the retry create a duplicate,
which is the defect this lookup exists to prevent.

The test fixtures made the point themselves: four of them used ids like `existing_camp_123`
and `camp_from_page_2`, which the code accepted and would have interpolated. Meta Graph
object ids are numeric, so those fixtures were modelling a response Meta does not produce;
they now carry realistic numeric ids. `TestFindByName_NonNumericIDIsAmbiguousNotUsable`
covers both lookups against `camp_123`, `123/../../me`, `123?fields=x` and `12 3`; verified
binding by neutering both gates, which returns each unusable id with a nil error.

**Update** — Copilot also read the PR's "retries reconcile correctly" claim as covering
retained partial orphans, which it does not. Worth being precise, because the boundary is a
real property of the design rather than a wording slip:

- **Reached** — a retry after a CLEAN dispatch failure. The claim is released, ProcessJob
  re-dispatches, `CreateCampaign` runs again, and the lookup finds whatever the failed
  attempt may nevertheless have created. This is the duplicate-paid-campaign path this work
  closes.
- **Not reached** — a retry that finds a retained partial orphan (a row carrying a
  `PlatformCampaignID` or a `Result` reconcile blob). `ProcessJob` reports those as
  "reconciliation required" and never calls the dispatcher, deliberately: a human may
  already be reconciling the row upstream.

**Update** — Copilot then found the same classification gap one level up: the two paths where
the lookup cannot FINISH enumerating — a `paging.next` link with no cursor, and the
`adDiscoveryMaxPages` cap reached with pages still pending — returned plain errors that
`createOutcomeAmbiguous` reads as clean failures. Both mean unexamined matches may remain, so
absence is unconfirmed, and a clean failure releases the claim and lets the retry re-POST the
same deterministic name — the duplicate-paid-campaign defect this lookup exists to prevent.

All four (both lookups, both paths) now wrap `errLookupAmbiguous`. The sentinel's doc comment
was widened to state the rule rather than enumerate one instance: **every** outcome that leaves
absence unconfirmed is ambiguous, and only a definite answer — "this name is absent", or "it
exists but is unusable for a stated reason" — may be a clean failure. The status/objective
mismatch paths stay deliberately clean under that rule: they are definite findings a retry
would reproduce identically.

`TestFindByName_UnfinishableEnumerationIsAmbiguous` covers both paths across both lookups and
asserts the CONSEQUENCE (`createOutcomeAmbiguous(err) == true`), not just the sentinel; verified
binding by un-wrapping all four, which fails on exactly those assertions.

Letting Meta's now-idempotent create re-dispatch a retained partial is a change to the
SHARED retry path — it must not re-dispatch a platform whose create is not idempotent — so it
is its own piece of work under LFXV2-2665, not part of this lookup. The reachability boundary
is now recorded next to `errLookupAmbiguous` so the next reader does not have to re-derive it.
