# 2026-08-18 — LFXV2-2641 keyword/audience reads scoped to the project's own campaigns

**Fix** — the two Google Ads insight reads crossed the project authorization boundary. Raised in
review, confirmed by capturing the GAQL actually sent before anything was changed.

**Measured before:**

```
SELECT ad_group_criterion.criterion_id, ..., campaign.id, metrics.impressions, ...
FROM keyword_view WHERE segments.date DURING LAST_7_DAYS
AND ad_group_criterion.status IN ('ENABLED','PAUSED')
ORDER BY metrics.impressions DESC LIMIT 51
```

No campaign or project predicate. `docs/architecture.md` ("Account Tenancy") establishes that
Google Ads is ONE customer shared across every foundation, distinguished only by naming
convention — so any `campaign_manager` could read every other project's keyword text, campaign
ids and spend, and the three demographic queries aggregated every project's traffic the same way.
The code's own comment asserted "the connection IS the scope, exactly as it is for ListAccounts",
and that is precisely the reasoning that fails on a shared customer: it is true for
`ListAccounts`, which reports on the credential itself, and false for a query over campaigns.

**Measured after:**

```
... FROM keyword_view WHERE segments.date DURING LAST_7_DAYS
AND ad_group_criterion.status IN ('ENABLED','PAUSED')
AND campaign.id IN ('111', '222') ORDER BY ...
... FROM age_range_view WHERE segments.date DURING LAST_7_DAYS AND campaign.id IN ('111', '222')
... FROM gender_view    WHERE segments.date DURING LAST_7_DAYS AND campaign.id IN ('111', '222')
... FROM campaign       WHERE segments.date DURING LAST_7_DAYS AND campaign.id IN ('111', '222')
```

All four queries carry the predicate. Both endpoints needed it: fixing only the keyword read would
have left the whole exposure intact on the audience one.

**The scope comes from SQL, not from a Go-side filter.** `ListProjectPlatformCampaignIDs` selects
`platform_campaign_id` with `project_id` in the WHERE clause. Filtering an unscoped read in Go
would leave the same cross-tenant exposure one layer up, and the call site would look identical in
review — the failure would be invisible exactly where it is most expensive.

**The empty case is where this design silently fails, so it is handled twice.** If the id list is
empty and the predicate is rendered anyway, the query becomes `campaign.id IN ()` — or, if a
future author "helpfully" drops an empty predicate, the original account-wide read returns. Either
way the exposure comes back for the caller most likely to trigger it: a project that has
dispatched nothing and therefore owns none of the data it would receive. The orchestrator returns
an empty result BEFORE any upstream call, and `campaignScopePredicate` independently refuses an
empty list rather than returning an empty string.

`TestKeywordInsights_EmptyScopeIssuesNoUpstreamCall` asserts the CALL COUNT, not the result. That
distinction is the point: a dispatcher that is contacted and whose rows are then discarded would
satisfy an empty-result assertion while still having issued the exposing query, so the fake returns
non-empty rows to make any implementation that calls through fail loudly. Removing either early
return fails it with "contacted the platform 1 time(s) with an EMPTY scope".

**An empty result, not a 409.** A project with no campaigns has nothing to show, which is an
ordinary state. Reporting it as a conflict would make "you have not run any campaigns yet"
indistinguishable from "something is broken" — the failure-as-error mirror of the fabricated
measurement this epic has been removing elsewhere.

**A scope lookup FAILURE is an error, never a widening.** If the repository read fails, the request
fails; "we could not determine which campaigns you own" must not degrade into "so here is
everything". Pinned by `TestKeywordInsights_ScopeLookupFailureDoesNotFallBack`.

**Behaviour change, stated because consumers will see it.** A campaign created outside this
service, or claimed but not yet dispatched (no `platform_campaign_id`), has no dispatched row and
is not in scope, so its keywords and demographics no longer appear. `docs/api-catalog.md` said
these endpoints were account-wide and has been corrected; leaving the catalog alone would have
left the consumer-facing contract describing the vulnerable behaviour as intended.

**The LF system fallback is deliberately kept.** Dropping it was considered and rejected as
insufficient on its own — project-owned connection rows point at the same shared customer, so the
fallback was never the mechanism. Once the query is campaign-scoped, a fallback project reads only
its own campaigns, which is the right answer rather than a hole.
