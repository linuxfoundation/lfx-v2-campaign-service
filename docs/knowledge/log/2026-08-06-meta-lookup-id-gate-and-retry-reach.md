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

Letting Meta's now-idempotent create re-dispatch a retained partial is a change to the
SHARED retry path — it must not re-dispatch a platform whose create is not idempotent — so it
is its own piece of work under LFXV2-2665, not part of this lookup. The reachability boundary
is now recorded next to `errLookupAmbiguous` so the next reader does not have to re-derive it.
